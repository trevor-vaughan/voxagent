# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's host half (ParsedFile-types design
# §10.4). Uses that script's HERE, IMAGE, VOXAGENT, EVIDENCE and WORK,
# parsedfile/image.sh's parsedfile_image and g5-case.sh's g5_judge and
# g5_refusal.
#
# It runs on localhost/parsedfile-measure-el, built from $IMAGE by
# images/Containerfile.parsedfile-types when missing, without openssh, or when
# its openvox differs from $IMAGE's (parsedfile/image.sh; a cold build needs
# the network). Every container runs with --no-hosts, so /etc/hosts is the
# image's own file, the same for both agents, and not podman's bind mount.
#
# Every case is a directory holding site.pp, an optional seed.sh and an
# optional `files`, the paths its end state is read from. Every case runs with
# no host_core in --libdir, so both agents use openvox's vendored 1.3.0, which
# is SIMP's 2.1.0 byte for byte (§2.2): there is no second release to run. A
# missing case stops the half with 2 before anything runs. Each case is
# compiled on its own by the image's openvox and applied in FRESH containers,
# one per agent and mode, seeded by its seed.sh under `sh -eu`, so a failing
# seed is a gate error (2). Per case: live, then a rerun in the same
# container; then --noop from a fresh seeded container. Each pair is judged by
# gatekit compare-reports and compare-order against the unchanged allowlist.
# After the live run each side's state is read and compared: every path
# `files` lists (mode, owner, group, size and `cat -A`), with only the header's
# timestamp normalised, and every clientbucket entry's `paths` file, since
# reports carry neither the file's text nor its backups.
#
# A case Ruby refuses at catalog conversion (HOST_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name host among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

HOST_IMAGE="${HOST_IMAGE:-localhost/parsedfile-measure-el:latest}"
HOST_CASES="cases/create cases/aliases-reordered cases/comment-added-removed
  cases/aliases-empty cases/aliases-after-empty cases/remove cases/preserve
  cases/alias-string-one-word cases/no-ip cases/noop cases/missing-target"
HOST_REFUSED="cases/invalid-ip cases/invalid-name cases/alias-string-whitespace
  cases/line-break-ip cases/line-break-name cases/line-break-alias
  cases/line-break-comment"

# host_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
host_case_dir() {
  local dir="$HERE/host/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: host: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# host_check_cases: 2 unless every case's directory and site.pp exist.
host_check_cases() {
  local c rc=0
  for c in $HOST_CASES $HOST_REFUSED; do
    host_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

host_is_refused() {
  local c
  for c in $HOST_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# host_compile: compiles every case into $WORK/host/CASE/catalog.json, in one
# container with an empty modulepath.
host_compile() {
  local name="host-compile-$$" c out
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$HOST_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: host: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $HOST_CASES $HOST_REFUSED; do
    out="$WORK/host/$c"
    mkdir -p "$out" || return 2
    podman cp "$(host_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: host: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# host_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/host/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
host_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/host/$2"
  local name="host-$agent-$mode-$$" dir run flags cmd
  dir="$(host_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$HOST_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$WORK/host/libdir" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: host $c: the seed failed in $name:" >&2
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
      host_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# host_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists: its stat line and `cat -A`, the header's first
# line with its timestamp replaced by <time>, or ABSENT; then every
# clientbucket entry, sorted: its directory (the old content's sha256) and its
# `paths` file.
host_state() {
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

# host_judge_state CASE: compares the two sides' live state.
host_judge_state() {
  local out="$WORK/host/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: host $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: host $1 live: end state agrees"
}

# host_native CASE RUN: 1 when voxagent sent host to the extension host.
host_native() {
  local out="$WORK/host/$1"
  if grep -Eq '^voxagent: [0-9]+ host resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: host $1 $2: voxagent delegated host to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# host_gate: the whole host half. Returns the worst result.
host_gate() {
  local c mode run rc=0 r pids p
  host_check_cases || return 2
  mkdir -p "$WORK/host" || return 2
  parsedfile_image report-diff-container "$IMAGE" "$HOST_IMAGE" "$WORK/host/image-build.log" || return 2
  if ! mkdir -p "$WORK/host/libdir" || ! cp -R "$HERE/lib/." "$WORK/host/libdir/"; then
    echo "report-diff-container: host: could not create the libdir in $WORK/host" >&2
    return 2
  fi
  host_compile || return 2

  for c in $HOST_CASES $HOST_REFUSED; do
    pids=''
    for mode in live noop; do
      host_is_refused "$c" && [ "$mode" != live ] && continue
      host_side ruby "$c" "$mode" &
      pids="$pids $!"
      host_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: host $c: a container could not be prepared or run" >&2
      return 2
    fi
    if host_is_refused "$c"; then
      g5_refusal "$WORK/host/$c" "host $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/host/$c" "host $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      host_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(host_case_dir "$c")/files" ]; then
      host_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-host" \
      && cp -R "$WORK/host" "$EVIDENCE/container-host" \
      && echo "report-diff-container: the host runs are in $EVIDENCE/container-host" >&2
  fi
  return "$rc"
}
