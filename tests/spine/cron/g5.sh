# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's cron half (cron design §10.4).
# Uses that script's HERE, ROOT, IMAGE, VOXAGENT, EVIDENCE and WORK,
# simp/lib.sh's simp_modules and cron_core_fingerprints, cron/image.sh's
# cron_image and g5-case.sh's g5_judge and g5_refusal.
#
# It runs on localhost/cron-measure-el, built from $IMAGE by
# images/Containerfile.cron when missing, without cronie, or when its openvox
# differs from $IMAGE's (cron/image.sh; a cold build needs the network):
# fleet-node-el has no crontab, so there cron is not native (§10.1 M4).
#
# Every case is a directory holding site.pp, an optional seed.sh and an
# optional `files`, the paths its end state is read from. The cases in cases/
# run with no cron_core in --libdir, so both agents use openvox's vendored
# 1.3.0; those in cases-2.1.0/ run with cron_core 2.1.0's lib/ (from the SIMP
# gate's pinned module cache, checked against its three routing fingerprints)
# in --libdir, for the outcomes §4.3 changes. A missing case stops the half
# with 2 before anything runs. Each case is compiled on its own by the image's
# openvox and applied in FRESH containers, one per agent and mode, seeded by
# its seed.sh under `sh -eu`, so a failing seed is a gate error (2). Per case:
# live, then a rerun in the same container; then --noop from a fresh seeded
# container. Each pair is judged by gatekit compare-reports and compare-order
# against the unchanged allowlist. After the live run each side's state is
# read and compared: every path `files` lists (mode, owner, group, size and
# `cat -A`), with only the first header line's timestamp normalised, since
# reports carry no crontab text. Some cases also list cronie's backup of a
# replaced crontab (/root/.cache/crontab/), which it saves only when the
# write's environment holds HOME.
#
# A case Ruby refuses at catalog conversion (CRON_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name cron among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

CRON_IMAGE="${CRON_IMAGE:-localhost/cron-measure-el:latest}"
CRON_CASES="cases/create cases/modify cases/remove cases/special-to-numeric
  cases/numeric-to-special cases/environment cases/simp-absent cases/simp-absent-empty
  cases/simp-yum-update cases/adoption cases/read-error cases/parse-error
  cases/parse-error-unrelated-file cases/two-in-one-crontab cases/no-command
  cases/env-empty cases/line-breaks cases/same-name-two-targets
  cases/user-created-in-run
  cases-2.1.0/env-empty cases-2.1.0/minute-0x1f"
CRON_REFUSED="cases-2.1.0/weekday-true"

# cron_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
cron_case_dir() {
  local dir="$HERE/cron/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: cron: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# cron_check_cases: 2 unless every case's directory and site.pp exist.
cron_check_cases() {
  local c rc=0
  for c in $CRON_CASES $CRON_REFUSED; do
    cron_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

cron_is_refused() {
  local c
  for c in $CRON_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# cron_libdir CASE: the libdir CASE's set applies with.
cron_libdir() {
  case "$1" in
    cases-2.1.0/*) echo "$WORK/cron/libdir-2.1.0" ;;
    *) echo "$WORK/cron/libdir-1.3.0" ;;
  esac
}

# cron_compile: compiles every case into $WORK/cron/CASE/catalog.json, in one
# container with an empty modulepath.
cron_compile() {
  local name="cron-compile-$$" c out
  podman run --name "$name" --hostname spine.example --detach "$CRON_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: cron: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $CRON_CASES $CRON_REFUSED; do
    out="$WORK/cron/$c"
    mkdir -p "$out" || return 2
    podman cp "$(cron_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: cron: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# cron_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/cron/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
cron_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/cron/$2"
  local name="cron-$agent-$mode-$$" dir run flags cmd
  dir="$(cron_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --detach "$CRON_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$(cron_libdir "$c")" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: cron $c: the seed failed in $name:" >&2
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
      cron_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# cron_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists: its stat line and `cat -A`, the header's first
# line with its timestamp replaced by <time>, or ABSENT.
cron_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    # report-diff-container.sh sets pipefail, so a failed exec fails the pipe.
    podman exec "$1" sh -c 'if [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" |
      sed -E 's/^(# HEADER: This file was autogenerated at ).*( by puppet\.\$)$/\1<time>\2/' || return 2
  done <"$2"
}

# cron_judge_state CASE: compares the two sides' live state.
cron_judge_state() {
  local out="$WORK/cron/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: cron $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: cron $1 live: end state agrees"
}

# cron_native CASE RUN: 1 when voxagent sent cron to the extension host.
cron_native() {
  local out="$WORK/cron/$1"
  if grep -Eq '^voxagent: [0-9]+ cron resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: cron $1 $2: voxagent delegated cron to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# cron_gate: the whole cron half. Returns the worst result.
cron_gate() {
  local pin_hash modules base c mode run rc=0 r pids p
  cron_check_cases || return 2
  mkdir -p "$WORK/cron" || return 2
  cron_image report-diff-container "$IMAGE" "$CRON_IMAGE" "$WORK/cron/image-build.log" || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2

  base="$WORK/cron/libdir-1.3.0"
  if ! mkdir -p "$base" || ! cp -R "$HERE/lib/." "$base/" \
    || ! cp -R "$base" "$WORK/cron/libdir-2.1.0" \
    || ! cp -R "$modules/cron_core/lib/." "$WORK/cron/libdir-2.1.0/"; then
    echo "report-diff-container: cron: could not create the libdirs in $WORK/cron" >&2
    return 2
  fi
  cron_core_fingerprints report-diff-container "$WORK/cron/libdir-2.1.0" || return 2
  cron_compile || return 2

  for c in $CRON_CASES $CRON_REFUSED; do
    pids=''
    for mode in live noop; do
      cron_is_refused "$c" && [ "$mode" != live ] && continue
      cron_side ruby "$c" "$mode" &
      pids="$pids $!"
      cron_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: cron $c: a container could not be prepared or run" >&2
      return 2
    fi
    if cron_is_refused "$c"; then
      g5_refusal "$WORK/cron/$c" "cron $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/cron/$c" "cron $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      cron_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(cron_case_dir "$c")/files" ]; then
      cron_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-cron" \
      && cp -R "$WORK/cron" "$EVIDENCE/container-cron" \
      && echo "report-diff-container: the cron runs are in $EVIDENCE/container-cron" >&2
  fi
  return "$rc"
}
