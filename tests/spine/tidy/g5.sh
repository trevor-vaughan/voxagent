# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's tidy half (tidy design §9.2). Uses
# that script's HERE, IMAGE, VOXAGENT, EVIDENCE and WORK and g5-case.sh's
# g5_judge and g5_refusal.
#
# It runs on $IMAGE itself: tidy is lstat, readdir and file's deletion, so it
# needs no package the plain openvox image lacks.
#
# Every case is a directory holding site.pp, an optional seed.sh and an
# optional `files`, the roots its end state is read from. A missing case stops
# the half with 2 before anything runs. Each case is compiled on its own by
# the image's openvox and applied in FRESH containers, one per agent and mode,
# seeded by its seed.sh under `sh -eu`, so a failing seed is a gate error (2).
# Seeds date every path they mean to be old with
# `touch -h -d '2001-01-01 00:00:00'` (atime and mtime), and leave a path
# they mean to be fresh at the seed's own time, so the two agents' different
# start times cannot change what an age selects; no case depends on ctime,
# which cannot be set. Per case: live, then a rerun in the same container;
# then --noop from a fresh seeded container. Each pair is judged by gatekit
# compare-reports and compare-order against the unchanged allowlist, and by
# the two exit codes. After the live run each side's tree is read and
# compared: `find -printf` of every root `files` lists (type, mode, owner,
# group, size and path), sorted, or ABSENT.
# Tidied files are never backed up (TY:240-245), so there is no clientbucket
# to read.
#
# A case Ruby refuses at catalog conversion (TIDY_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name tidy among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

TIDY_CASES="cases/simple cases/recurse-all cases/recurse-1 cases/age-mtime
  cases/age-atime-default cases/size cases/rmdirs cases/managed-excluded
  cases/metaparams cases/missing-path cases/max-files-exceeded
  cases/max-files-string cases/non-utf8 cases/two-tidies-overlap"
TIDY_REFUSED="cases/age-10 cases/age-integer cases/recurse-08
  cases/matches-no-recurse cases/type-bogus"

# tidy_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
tidy_case_dir() {
  local dir="$HERE/tidy/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: tidy: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# tidy_check_cases: 2 unless every case's directory and site.pp exist.
tidy_check_cases() {
  local c rc=0
  for c in $TIDY_CASES $TIDY_REFUSED; do
    tidy_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

tidy_is_refused() {
  local c
  for c in $TIDY_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# tidy_compile: compiles every case into $WORK/tidy/CASE/catalog.json, in one
# container with an empty modulepath.
tidy_compile() {
  local name="tidy-compile-$$" c out
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: tidy: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $TIDY_CASES $TIDY_REFUSED; do
    out="$WORK/tidy/$c"
    mkdir -p "$out" || return 2
    podman cp "$(tidy_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: tidy: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# tidy_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/tidy/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
tidy_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/tidy/$2"
  local name="tidy-$agent-$mode-$$" dir run flags cmd
  dir="$(tidy_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$WORK/tidy/libdir" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: tidy $c: the seed failed in $name:" >&2
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
      tidy_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# tidy_state NAME FILES: the tree a live run left in container NAME, one block
# per root FILES lists: every path under it, the root included, as type, mode,
# owner, group, size and path, sorted bytewise; or ABSENT.
tidy_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    # report-diff-container.sh sets pipefail, so a failed exec fails the pipe.
    podman exec "$1" sh -c 'if [ -e "$1" ] || [ -L "$1" ]; then
        find "$1" -printf "%y %m %u %g %s %p\n" | LC_ALL=C sort
      else echo ABSENT; fi' sh "$f" || return 2
  done <"$2"
}

# tidy_judge_state CASE: compares the two sides' live state.
tidy_judge_state() {
  local out="$WORK/tidy/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: tidy $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: tidy $1 live: end state agrees"
}

# tidy_exit CASE RUN: 1 when the two agents' exit codes for RUN differ. A
# failed generation is logged and the run carries on (ARG:21-29), so the
# report alone does not show that both agents ended the run the same way.
tidy_exit() {
  local out="$WORK/tidy/$1" want got
  want="$(cat "$out/ruby-$2.rc")" got="$(cat "$out/voxagent-$2.rc")"
  if [ "$want" != "$got" ]; then
    echo "report-diff-container: tidy $1 $2: DIVERGE: Ruby exits $want, voxagent $got" >&2
    return 1
  fi
}

# tidy_native CASE RUN: 1 when voxagent sent tidy to the extension host.
tidy_native() {
  local out="$WORK/tidy/$1"
  if grep -Eq '^voxagent: [0-9]+ tidy resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: tidy $1 $2: voxagent delegated tidy to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# tidy_gate: the whole tidy half. Returns the worst result.
tidy_gate() {
  local c mode run rc=0 r pids p
  tidy_check_cases || return 2
  mkdir -p "$WORK/tidy" || return 2
  if ! mkdir -p "$WORK/tidy/libdir" || ! cp -R "$HERE/lib/." "$WORK/tidy/libdir/"; then
    echo "report-diff-container: tidy: could not create the libdir in $WORK/tidy" >&2
    return 2
  fi
  tidy_compile || return 2

  for c in $TIDY_CASES $TIDY_REFUSED; do
    pids=''
    for mode in live noop; do
      tidy_is_refused "$c" && [ "$mode" != live ] && continue
      tidy_side ruby "$c" "$mode" &
      pids="$pids $!"
      tidy_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: tidy $c: a container could not be prepared or run" >&2
      return 2
    fi
    if tidy_is_refused "$c"; then
      g5_refusal "$WORK/tidy/$c" "tidy $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/tidy/$c" "tidy $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      tidy_exit "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
      tidy_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(tidy_case_dir "$c")/files" ]; then
      tidy_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-tidy" \
      && cp -R "$WORK/tidy" "$EVIDENCE/container-tidy" \
      && echo "report-diff-container: the tidy runs are in $EVIDENCE/container-tidy" >&2
  fi
  return "$rc"
}
