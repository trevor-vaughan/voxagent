# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's inifile half (inifile design §8.3).
# Uses that script's HERE, ROOT, IMAGE, VOXAGENT, GATEKIT, EVIDENCE and WORK,
# simp/lib.sh's simp_modules and inifile_fingerprints, and g5-case.sh's
# g5_judge.
#
# Every case is a directory under cases/ beside this file: Task 1's Ruby-only
# measurements M1-M8 (2026-11-26) verbatim, plus subsetting-650-create-change-
# remove, which runs that measurement's subsetting case against 6.5.0 (design
# §8.3 wants both types there). A case holds site.pp, seed.sh (optional),
# files (the paths whose end state is compared), and optionally:
#   module      650 | 661 | 661p; default 661. Which tree Ruby loads (below).
#   podmanargs  extra `podman run` arguments, eval'd with CASE_DIR set.
#   rodir/      a directory the case's podmanargs mounts read-only.
# Each case is compiled on its own, by the image's openvox, and applied in
# FRESH containers, one per agent and mode: the cases share /etc/m.ini, and a
# case seeded on top of another's result would compare a different start. Per
# case: live, then a rerun in the same container; then --noop from a fresh
# seeded container. Each run's two reports are judged by gatekit
# compare-reports and compare-order against the unchanged allowlist, and each
# run's end state (every path in `files`: type, mode, size, cat -A) is
# compared, so a noop that wrote, or a write that differs by a byte, fails.
# Both agents compile and apply the same catalog, so the compile-time manifest
# path in a failure message is the same on both sides.
#
# Three trees, all with tests/spine/lib's gatejson added:
#   650   puppetlabs-inifile 6.5.0 from the SIMP gate's module cache (no
#         ini_section).
#   661   6.6.1 from fleet/.vendor (`task fleet:vendor`).
#   661p  661 with PR #581's lib hunk (581-lib.diff, beside this file) applied.
# voxagent always reads 650 or 661, whose files carry the fingerprints it
# routes on (checked per tree). Ruby reads the case's `module`: 661p for the
# ini_section cases, so Ruby applies ini_section as PR #581 intends and the two
# agents can be compared (design §8.3, D-105). voxagent's output is checked
# against `applied via extension host` naming an inifile type, so a delegated
# run, Ruby against Ruby, cannot pass as native.
#
# The two D-105 cases run Ruby on the UNPATCHED 661, where every ini_section
# fails with `Invalid parameter refreshonly(:refreshonly)` (measurement M4); no
# report comparison can agree there, so ini_d105_judge asserts the difference.

INIFILE_D105_CASES="d105-unpatched-insync d105-unpatched-outofsync"
# The tag of the patched tree's ini_section type: S3's sha256 prefix.
INIFILE_PATCHED_TYPE_SHA_PREFIX=8b766e66

# ini_cases: every case directory's name, one per line; 2, naming the
# directory, when one has no site.pp or no files.
ini_cases() {
  local d rc=0
  for d in "$HERE"/inifile/cases/*/; do
    d="${d%/}"
    if [ ! -f "$d/site.pp" ] || [ ! -f "$d/files" ]; then
      echo "report-diff-container: inifile: case ${d##*/} is missing its site.pp or files" >&2
      rc=2
    fi
    echo "${d##*/}"
  done
  return "$rc"
}

# ini_module CASE: the tree Ruby loads for CASE.
ini_module() {
  local f="$HERE/inifile/cases/$1/module"
  if [ -f "$f" ]; then cat "$f"; else echo 661; fi
}

# ini_native_tree MODULE: the tree voxagent loads where Ruby loads MODULE.
ini_native_tree() {
  case "$1" in
    661p) echo 661 ;;
    *) echo "$1" ;;
  esac
}

ini_is_d105() {
  case " $INIFILE_D105_CASES " in *" $1 "*) return 0 ;; esac
  return 1
}

# ini_prepare_trees MODULES_CACHE: fills $WORK/inifile/mod-{650,661,661p} with
# whole module trees and $WORK/inifile/libdir-{650,661,661p} with their lib/
# plus gatejson, and checks each tree's fingerprints. 2 on any failure.
ini_prepare_trees() {
  local cache="$1" w="$WORK/inifile" t
  [ -d "$ROOT/fleet/.vendor/modules/inifile/lib" ] || {
    echo "report-diff-container: fleet/.vendor/modules/inifile is missing; run 'task fleet:vendor'" >&2
    return 2
  }
  mkdir -p "$w/mod-650" "$w/mod-661" "$w/mod-661p" || return 2
  cp -R "$cache/inifile/." "$w/mod-650/" || return 2
  cp -R "$ROOT/fleet/.vendor/modules/inifile/." "$w/mod-661/" || return 2
  rm -rf "$w/mod-661/.git"
  cp -R "$w/mod-661/." "$w/mod-661p/" || return 2
  patch -p1 -s -d "$w/mod-661p" <"$HERE/inifile/581-lib.diff" || {
    echo "report-diff-container: inifile: 581-lib.diff does not apply to the vendored 6.6.1" >&2
    return 2
  }
  for t in 650 661 661p; do
    mkdir -p "$w/libdir-$t" || return 2
    cp -R "$HERE/lib/." "$w/mod-$t/lib/." "$w/libdir-$t/" || return 2
  done
  inifile_fingerprints report-diff-container "$w/libdir-650" 6.5.0 || return 2
  inifile_fingerprints report-diff-container "$w/libdir-661" 6.6.1 || return 2
  local sha
  sha="$(sha256sum "$w/libdir-661p/puppet/type/ini_section.rb")"
  case "$sha" in
    "$INIFILE_PATCHED_TYPE_SHA_PREFIX"*) ;;
    *)
      echo "report-diff-container: inifile: the patched ini_section type is not PR #581's (sha256 ${sha%% *})" >&2
      return 2
      ;;
  esac
}

# ini_compile MODULE CASES...: compiles each of CASES into
# $WORK/inifile/CASE/catalog.json in one container whose environment holds the
# MODULE tree as modules/inifile. The catalog does not depend on the tree's
# provider code, so one catalog serves both agents.
ini_compile() {
  local module="$1" name="inifile-compile-$$" c out
  shift
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: inifile $module: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  podman cp "$WORK/inifile/mod-$module" "$name:/tmp/e/production/modules/inifile" >/dev/null 2>&1 || {
    echo "report-diff-container: inifile $module: could not copy the module into the compiler" >&2
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  }
  for c in "$@"; do
    out="$WORK/inifile/$c"
    mkdir -p "$out" || return 2
    podman cp "$HERE/inifile/cases/$c/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: inifile $module: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# ini_state NAME FILES: the state in container NAME of every path FILES lists,
# as Task 1's measurement dumped it.
ini_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    podman exec "$1" sh -c 'find "$1" -maxdepth 0 -printf "%y %m %s %p\n" 2>&1; if [ -f "$1" ]; then cat -A "$1"; fi' sh "$f" || return 2
  done <"$2"
}

# ini_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh seeds
# it, applied by AGENT (ruby or voxagent) in MODE (live, which also reruns, or
# noop). Writes $WORK/inifile/CASE/AGENT-RUN.{out,json,rc,state} for each run;
# an empty .json is a run that wrote no report.
ini_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/inifile/$2"
  local name="inifile-$agent-$mode-$$" dir="$HERE/inifile/cases/$2" run flags cmd tree
  local -a extra=()
  tree="$(ini_module "$c")"
  [ "$agent" = ruby ] || tree="$(ini_native_tree "$tree")"
  # Only podmanargs is eval'd: it is this repository's own file.
  # shellcheck disable=SC2034
  local CASE_DIR="$dir"
  [ ! -f "$dir/podmanargs" ] || eval "extra=($(cat "$dir/podmanargs"))"
  podman run --name "$name" --hostname spine.example --detach "${extra[@]}" "$IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$WORK/inifile/libdir-$tree" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: inifile $c: the seed failed in $name:" >&2
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
    ini_state "$name" "$dir/files" >"$out/$agent-$run.state" 2>&1 || {
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    }
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# ini_native CASE RUN: 1 when voxagent sent an inifile type to the extension
# host.
ini_native() {
  local out="$WORK/inifile/$1"
  if grep -Eq '^voxagent: [0-9]+ ini_(setting|subsetting|section) resource\(s\) applied via extension host' \
    "$out/voxagent-$2.out"; then
    echo "report-diff-container: inifile $1 $2: voxagent delegated an inifile type to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# ini_judge_state CASE RUN: compares the two sides' end state after RUN.
ini_judge_state() {
  local out="$WORK/inifile/$1"
  if ! diff -u "$out/ruby-$2.state" "$out/voxagent-$2.state" >"$out/state-$2.diff"; then
    echo "report-diff-container: inifile $1 $2: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state-$2.diff" >&2
    return 1
  fi
  echo "report-diff-container: inifile $1 $2: end state agrees"
}

# ini_d105_judge CASE: Ruby, unpatched, fails every ini_section in every run
# (M4) and leaves the file as seeded; voxagent never fails, applies the
# out-of-sync section live (and not under --noop), and changes nothing in the
# in-sync case. 1 on any other outcome.
ini_d105_judge() {
  local c="$1" out="$WORK/inifile/$1" run rc=0 seeded
  for run in live rerun noop; do
    if ! grep -q 'Invalid parameter refreshonly(:refreshonly)' "$out/ruby-$run.out"; then
      echo "report-diff-container: inifile $c $run: Ruby did not fail with D-105's refreshonly error:" >&2
      sed -n '1,10p' "$out/ruby-$run.out" >&2
      rc=1
    fi
    if grep -q 'Invalid parameter refreshonly' "$out/voxagent-$run.out" || grep -q '^Error:' "$out/voxagent-$run.out"; then
      echo "report-diff-container: inifile $c $run: voxagent failed where D-105 says it applies:" >&2
      sed -n '1,10p' "$out/voxagent-$run.out" >&2
      rc=1
    fi
    ini_native "$c" "$run" || rc=1
  done
  # Ruby leaves the seed alone in every run: that is the unpatched baseline.
  seeded="$out/ruby-live.state"
  case "$c" in
    d105-unpatched-insync)
      for run in live rerun noop; do
        diff -q "$seeded" "$out/voxagent-$run.state" >/dev/null || {
          echo "report-diff-container: inifile $c $run: voxagent changed an in-sync file:" >&2
          diff -u "$seeded" "$out/voxagent-$run.state" | sed -n '1,20p' >&2
          rc=1
        }
      done
      ;;
    d105-unpatched-outofsync)
      grep -qx '\[b\]\$' "$out/voxagent-live.state" || {
        echo "report-diff-container: inifile $c live: voxagent did not create section [b]:" >&2
        sed -n '1,10p' "$out/voxagent-live.state" >&2
        rc=1
      }
      diff -q "$out/voxagent-live.state" "$out/voxagent-rerun.state" >/dev/null || {
        echo "report-diff-container: inifile $c rerun: voxagent changed the file again" >&2
        rc=1
      }
      diff -q "$seeded" "$out/voxagent-noop.state" >/dev/null || {
        echo "report-diff-container: inifile $c noop: voxagent wrote under --noop" >&2
        rc=1
      }
      ;;
  esac
  for run in live rerun noop; do
    diff -q "$seeded" "$out/ruby-$run.state" >/dev/null || {
      echo "report-diff-container: inifile $c $run: Ruby's unpatched run changed the file; the baseline moved" >&2
      rc=2
    }
  done
  [ "$rc" -ne 0 ] || echo "report-diff-container: inifile $c: D-105 holds (Ruby fails, voxagent applies)"
  return "$rc"
}

# ini_gate: the whole inifile half. Returns the worst result.
ini_gate() {
  local pin_hash modules c mode run rc=0 r pa pb m cases
  local -a all
  ini_cases >/dev/null || return 2
  mapfile -t all < <(ini_cases)
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2
  ini_prepare_trees "$modules" || return 2
  for m in 650 661 661p; do
    cases=()
    for c in "${all[@]}"; do
      [ "$(ini_module "$c")" != "$m" ] || cases+=("$c")
    done
    [ "${#cases[@]}" -eq 0 ] || ini_compile "$m" "${cases[@]}" || return 2
  done

  for c in "${all[@]}"; do
    for mode in live noop; do
      ini_side ruby "$c" "$mode" &
      pa=$!
      ini_side voxagent "$c" "$mode" &
      pb=$!
      if ! wait "$pa" || ! wait "$pb"; then
        echo "report-diff-container: inifile $c $mode: a container could not be prepared or run" >&2
        return 2
      fi
    done
    if ini_is_d105 "$c"; then
      ini_d105_judge "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/inifile/$c" "inifile $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      ini_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
      ini_judge_state "$c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    done
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-inifile" \
      && cp -R "$WORK/inifile" "$EVIDENCE/container-inifile" \
      && echo "report-diff-container: the inifile runs are in $EVIDENCE/container-inifile" >&2
  fi
  return "$rc"
}
