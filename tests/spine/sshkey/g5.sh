# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's sshkey half (ParsedFile-types
# design §10.4). Uses that script's HERE, ROOT, IMAGE, VOXAGENT, EVIDENCE and
# WORK, simp/lib.sh's simp_modules, parsedfile/image.sh's parsedfile_image and
# g5-case.sh's g5_judge and g5_refusal.
#
# It runs on localhost/parsedfile-measure-el, as the host half does, whose
# openssh owns /etc/ssh, the default target's directory. Every container runs
# with --no-hosts, like the host half's, so both agents start from the image's
# own files.
#
# Every case is a directory holding site.pp, an optional seed.sh and an
# optional `files`, the paths its end state is read from. The cases in cases/
# run with no sshkeys_core in --libdir, so both agents use openvox's vendored
# 2.5.0; those in cases-3.1.0/ run with sshkeys_core 3.1.0's lib/ (from the
# SIMP gate's pinned module cache) in --libdir, for the `key` validation that
# release adds (§2.2, §4.2). Ruby loads the libdir copy over the vendored one
# (M3), and voxagent routes sshkey natively only on the digests of §2.2, so a
# changed pin shows up as a delegated run, which fails the half (below). A
# missing case stops the half with 2 before anything runs. Each case is
# compiled on its own by the image's openvox and applied in FRESH containers,
# one per agent and mode, seeded by its seed.sh under `sh -eu`, so a failing
# seed is a gate error (2). Per case: live, then a rerun in the same
# container; then --noop from a fresh seeded container. Each pair is judged by
# gatekit compare-reports and compare-order against the unchanged allowlist.
# After the live run each side's state is read and compared: every path
# `files` lists (mode, owner, group, size and `cat -A`), with only the
# header's timestamp normalised, and every clientbucket entry's `paths` file,
# since reports carry neither the file's text nor its backups.
#
# A case Ruby refuses at catalog conversion (SSHKEY_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name sshkey among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

SSHKEY_IMAGE="${SSHKEY_IMAGE:-localhost/parsedfile-measure-el:latest}"
SSHKEY_CASES="cases/create cases/refs cases/existing cases/aliases-reordered
  cases/aliases-empty cases/aliases-empty-create cases/alias-string
  cases/comment-key-in-sync cases/comment-key-dropped cases/modify-key
  cases/remove cases/absent-no-type cases/leading-whitespace cases/no-type
  cases/missing-target-dir cases/target cases/key-whitespace
  cases/line-break-key
  cases-3.1.0/create"
SSHKEY_REFUSED="cases/name-whitespace cases/name-comma cases/alias-whitespace
  cases/alias-comma cases/bad-type
  cases-3.1.0/key-whitespace cases-3.1.0/line-break-key"

# sshkey_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
sshkey_case_dir() {
  local dir="$HERE/sshkey/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: sshkey: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# sshkey_check_cases: 2 unless every case's directory and site.pp exist.
sshkey_check_cases() {
  local c rc=0
  for c in $SSHKEY_CASES $SSHKEY_REFUSED; do
    sshkey_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

sshkey_is_refused() {
  local c
  for c in $SSHKEY_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# sshkey_libdir CASE: the libdir CASE's set applies with.
sshkey_libdir() {
  case "$1" in
    cases-3.1.0/*) echo "$WORK/sshkey/libdir-3.1.0" ;;
    *) echo "$WORK/sshkey/libdir-2.5.0" ;;
  esac
}

# sshkey_compile: compiles every case into $WORK/sshkey/CASE/catalog.json, in one
# container with an empty modulepath.
sshkey_compile() {
  local name="sshkey-compile-$$" c out
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$SSHKEY_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: sshkey: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $SSHKEY_CASES $SSHKEY_REFUSED; do
    out="$WORK/sshkey/$c"
    mkdir -p "$out" || return 2
    podman cp "$(sshkey_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: sshkey: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# sshkey_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/sshkey/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
sshkey_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/sshkey/$2"
  local name="sshkey-$agent-$mode-$$" dir run flags cmd
  dir="$(sshkey_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$SSHKEY_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$(sshkey_libdir "$c")" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: sshkey $c: the seed failed in $name:" >&2
      sed -n '1,10p' "$out/$agent-$mode-seed.out" >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  fi
  if [ "$agent" = voxagent ]; then
    if ! podman cp "$VOXAGENT" "$name:/tmp/voxagent" >/dev/null 2>&1 \
      || ! podman exec "$name" chmod +x /tmp/voxagent >/dev/null 2>&1; then
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  fi
  flags=''
  [ "$mode" = noop ] && flags='--noop'
  for run in $([ "$mode" = live ] && echo live rerun || echo noop); do
    if [ "$agent" = ruby ]; then
      cmd="/opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --libdir /tmp/lib \
        --report --reports gatejson --reportdir /tmp/r-$run --color=false $flags"
    else
      cmd="/tmp/voxagent apply --catalog /tmp/catalog.json --libdir /tmp/lib --report-dir /tmp/r-$run $flags"
    fi
    podman exec "$name" sh -c "$cmd" >"$out/$agent-$run.out" 2>&1
    echo "$?" >"$out/$agent-$run.rc"
    podman exec "$name" sh -c "g=\"\$(find /tmp/r-$run -name '*.json' 2>/dev/null | head -1)\"
      [ -z \"\$g\" ] || cat \"\$g\"" >"$out/$agent-$run.json" 2>/dev/null
    if [ "$run" = live ] && [ -f "$dir/files" ]; then
      sshkey_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# sshkey_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists: its stat line and `cat -A`, the header's first
# line with its timestamp replaced by <time>, or ABSENT; then every
# clientbucket entry, sorted: its directory (the old content's sha256) and its
# `paths` file.
sshkey_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    # report-diff-container.sh sets pipefail, so a failed exec fails the pipe.
    podman exec "$1" sh -c 'if [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" |
      sed -E 's/^(# HEADER: This file was autogenerated at ).*$/\1<time>/' || return 2
  done <"$2"
  echo "== clientbucket"
  podman exec "$1" sh -c 'b=/opt/puppetlabs/puppet/cache/clientbucket
    [ ! -d "$b" ] || find "$b" -name paths -type f | sort | while read -r p; do
      echo "${p#"$b"/}"; cat -A "$p"
    done' || return 2
}

# sshkey_judge_state CASE: compares the two sides' live state.
sshkey_judge_state() {
  local out="$WORK/sshkey/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: sshkey $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: sshkey $1 live: end state agrees"
}

# sshkey_native CASE RUN: 1 when voxagent sent sshkey to the extension host.
sshkey_native() {
  local out="$WORK/sshkey/$1"
  if grep -Eq '^voxagent: [0-9]+ sshkey resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: sshkey $1 $2: voxagent delegated sshkey to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# sshkey_gate: the whole sshkey half. Returns the worst result.
sshkey_gate() {
  local pin_hash modules base c mode run rc=0 r pids p
  sshkey_check_cases || return 2
  mkdir -p "$WORK/sshkey" || return 2
  parsedfile_image report-diff-container "$IMAGE" "$SSHKEY_IMAGE" "$WORK/sshkey/image-build.log" || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2

  base="$WORK/sshkey/libdir-2.5.0"
  if ! mkdir -p "$base" || ! cp -R "$HERE/lib/." "$base/" \
    || ! cp -R "$base" "$WORK/sshkey/libdir-3.1.0" \
    || ! cp -R "$modules/sshkeys_core/lib/." "$WORK/sshkey/libdir-3.1.0/"; then
    echo "report-diff-container: sshkey: could not create the libdirs in $WORK/sshkey" >&2
    return 2
  fi
  sshkey_compile || return 2

  for c in $SSHKEY_CASES $SSHKEY_REFUSED; do
    pids=''
    for mode in live noop; do
      sshkey_is_refused "$c" && [ "$mode" != live ] && continue
      sshkey_side ruby "$c" "$mode" &
      pids="$pids $!"
      sshkey_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: sshkey $c: a container could not be prepared or run" >&2
      return 2
    fi
    if sshkey_is_refused "$c"; then
      g5_refusal "$WORK/sshkey/$c" "sshkey $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/sshkey/$c" "sshkey $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      sshkey_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(sshkey_case_dir "$c")/files" ]; then
      sshkey_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-sshkey" \
      && cp -R "$WORK/sshkey" "$EVIDENCE/container-sshkey" \
      && echo "report-diff-container: the sshkey runs are in $EVIDENCE/container-sshkey" >&2
  fi
  return "$rc"
}
