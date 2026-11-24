# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's ssh_authorized_key (sak) half
# (ParsedFile-types design §10.4). Uses that script's HERE, ROOT, IMAGE,
# VOXAGENT, EVIDENCE and WORK, simp/lib.sh's simp_modules,
# parsedfile/image.sh's parsedfile_image and g5-case.sh's g5_judge and
# g5_refusal.
#
# It runs on localhost/parsedfile-measure-el, as the host and sshkey halves
# do, with their --no-hosts containers. Seeds create the users and files a
# case needs; agents run as root, so every drop_privileges true flush writes
# through voxagent's asuser helper as the key's user, and Ruby's asuser.
#
# Every case is a directory holding site.pp, an optional seed.sh and an
# optional `files`, the paths its end state is read from. The cases in cases/
# run with no sshkeys_core in --libdir, so both agents use openvox's vendored
# 2.5.0; cases-3.1.0/create runs with sshkeys_core 3.1.0's lib/ (from the
# SIMP gate's pinned module cache) in --libdir. The two releases'
# ssh_authorized_key files differ in digest but not in contract (§2.2), so
# that case holds only that voxagent routes 3.1.0 natively: Ruby loads the
# libdir copy over the vendored one (M3), and voxagent routes natively only
# on the digests of §2.2, so a changed pin shows up as a delegated run, which
# fails the half (below). A missing case stops the half with 2 before anything
# runs. Each case is compiled on its own by the image's openvox and applied in
# FRESH containers, one per agent and mode, seeded by its seed.sh under
# `sh -eu`, so a failing seed is a gate error (2). Per case: live, then a
# rerun in the same container; then --noop from a fresh seeded container. Each
# pair is judged by gatekit compare-reports and compare-order against the
# unchanged allowlist. After the live run each side's state is read and
# compared: every path `files` lists (mode, owner, group, size and `cat -A`
# for a file, the stat line alone for a directory), with only the header's
# timestamp normalised, and every clientbucket entry's `paths` file, since
# reports carry neither the file's text nor its backups.
#
# Both agents run without --debug, as in every other half, so no report holds
# a debug line: the count of `The required user is not yet present on the
# system`, which Ruby logs a varying number of times (§6.3), is never
# compared. With debug off Ruby's trusted_path is never true (§8 item 9), so
# every drop_privileges false case here ends `drop_privileges is false but
# the target path is not trusted` or, for a directory target, in the prefetch
# failure; the trusted write is unit-tested only. So are the D-102 leak cases,
# which would need an allowlist entry (§10.4). D-100 is kept out the same way:
# in move-target alice's own key changes first, so her file, the moved key's
# old target, is backed up by that flush in both agents, and the move's write
# of it as bob finds it already backed up in the run (pf:91-99). Moved first,
# voxagent's root-parent backup would add a bucket entry Ruby's skipped
# backup as bob lacks.
#
# A case Ruby refuses at catalog conversion (SAK_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name ssh_authorized_key, nor sshkey (which
# shared-target-across-types also applies), among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

SAK_IMAGE="${SAK_IMAGE:-localhost/parsedfile-measure-el:latest}"
SAK_CASES="cases/create cases/existing cases/options-change cases/remove
  cases/key-v1 cases/key-v1-unnamed cases/user-in-run cases/user-after-key
  cases/nonexistent-user cases/no-home cases/no-user-target
  cases/drop-false-file cases/drop-false-untrusted cases/drop-false-dir
  cases/write-refused cases/write-symlink cases/mkdir-refused
  cases/chmod-refused cases/move-target cases/move-not-target
  cases/sensitive cases/line-break-name cases/line-break-options
  cases/shared-target-across-types
  cases-3.1.0/create"
SAK_REFUSED="cases/no-user cases/key-whitespace cases/drop-privileges-maybe
  cases/options-bad cases/type-bad cases/title-vs-name"

# sak_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
sak_case_dir() {
  local dir="$HERE/sak/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: sak: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# sak_check_cases: 2 unless every case's directory and site.pp exist.
sak_check_cases() {
  local c rc=0
  for c in $SAK_CASES $SAK_REFUSED; do
    sak_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

sak_is_refused() {
  local c
  for c in $SAK_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# sak_libdir CASE: the libdir CASE's set applies with.
sak_libdir() {
  case "$1" in
    cases-3.1.0/*) echo "$WORK/sak/libdir-3.1.0" ;;
    *) echo "$WORK/sak/libdir-2.5.0" ;;
  esac
}

# sak_compile: compiles every case into $WORK/sak/CASE/catalog.json, in one
# container with an empty modulepath.
sak_compile() {
  local name="sak-compile-$$" c out
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$SAK_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: sak: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $SAK_CASES $SAK_REFUSED; do
    out="$WORK/sak/$c"
    mkdir -p "$out" || return 2
    podman cp "$(sak_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: sak: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# sak_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/sak/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
sak_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/sak/$2"
  local name="sak-$agent-$mode-$$" dir run flags cmd
  dir="$(sak_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$SAK_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$(sak_libdir "$c")" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: sak $c: the seed failed in $name:" >&2
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
      sak_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# sak_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists: its stat line and, for a file, `cat -A`, the
# header's first line with its timestamp replaced by <time>, or ABSENT; then
# every clientbucket entry, sorted: the sha256 of its `contents` with the
# header's timestamp normalised as above, and its `paths` file. Not the
# entry's directory, the raw content's sha256: a target backed up after an
# earlier flush of the run wrote it holds that flush's timestamp, so the name
# differs run to run (shared-target-across-types).
sak_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    # report-diff-container.sh sets pipefail, so a failed exec fails the pipe.
    podman exec "$1" sh -c 'if [ -d "$1" ]; then stat -c "mode=%a owner=%U group=%G directory" "$1"
      elif [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" |
      sed -E 's/^(# HEADER: This file was autogenerated at ).*$/\1<time>/' || return 2
  done <"$2"
  echo "== clientbucket"
  podman exec "$1" sh -c 'b=/opt/puppetlabs/puppet/cache/clientbucket
    [ ! -d "$b" ] || find "$b" -name paths -type f | while read -r p; do
      c="${p%/paths}/contents" h=NO-CONTENTS
      [ ! -f "$c" ] || h="$(sed -E "s/^(# HEADER: This file was autogenerated at ).*\$/\1<time>/" "$c" | sha256sum)"
      echo "${h%% *} $(cat -A "$p" | tr "\n" " ")"
    done | sort' || return 2
}

# sak_judge_state CASE: compares the two sides' live state.
sak_judge_state() {
  local out="$WORK/sak/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: sak $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: sak $1 live: end state agrees"
}

# sak_native CASE RUN: 1 when voxagent sent ssh_authorized_key or sshkey to the
# extension host.
sak_native() {
  local out="$WORK/sak/$1"
  if grep -Eq '^voxagent: [0-9]+ (ssh_authorized_key|sshkey) resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: sak $1 $2: voxagent delegated ssh_authorized_key or sshkey to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# sak_gate: the whole sak half. Returns the worst result.
sak_gate() {
  local pin_hash modules base c mode run rc=0 r pids p
  sak_check_cases || return 2
  mkdir -p "$WORK/sak" || return 2
  parsedfile_image report-diff-container "$IMAGE" "$SAK_IMAGE" "$WORK/sak/image-build.log" || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2

  base="$WORK/sak/libdir-2.5.0"
  if ! mkdir -p "$base" || ! cp -R "$HERE/lib/." "$base/" \
    || ! cp -R "$base" "$WORK/sak/libdir-3.1.0" \
    || ! cp -R "$modules/sshkeys_core/lib/." "$WORK/sak/libdir-3.1.0/"; then
    echo "report-diff-container: sak: could not create the libdirs in $WORK/sak" >&2
    return 2
  fi
  sak_compile || return 2

  for c in $SAK_CASES $SAK_REFUSED; do
    pids=''
    for mode in live noop; do
      sak_is_refused "$c" && [ "$mode" != live ] && continue
      sak_side ruby "$c" "$mode" &
      pids="$pids $!"
      sak_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: sak $c: a container could not be prepared or run" >&2
      return 2
    fi
    if sak_is_refused "$c"; then
      g5_refusal "$WORK/sak/$c" "sak $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/sak/$c" "sak $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      sak_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(sak_case_dir "$c")/files" ]; then
      sak_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-sak" \
      && cp -R "$WORK/sak" "$EVIDENCE/container-sak" \
      && echo "report-diff-container: the sak runs are in $EVIDENCE/container-sak" >&2
  fi
  return "$rc"
}
