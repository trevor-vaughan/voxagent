# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's concat half (concat design §10).
# Uses that script's HERE, ROOT, IMAGE, VOXAGENT, GATEKIT, EVIDENCE and WORK,
# simp/lib.sh's simp_modules and concat_fingerprints, and g5-case.sh's
# g5_judge and g5_refusal.
#
# Every case is a directory under cases/ beside this file: a measured one
# (concat measurements M1-M23 and v10-*, 2026-11-20: its site.pp, seed.sh and,
# for a state case, files, verbatim) or one of the three this gate adds. A
# missing case stops the half with 2 before anything runs. Each is compiled
# on its own, by the image's openvox, and applied in FRESH containers, one per agent and mode, as
# it was measured: the cases share paths (/etc/c01.conf, /etc/c13.conf), and a
# case seeded on top of another's result would compare a different start. Per
# case: live, then a rerun in the same container; then --noop, without
# --show_diff (a diff prints the content, which neither report carries), from a
# fresh seeded container. Each pair is judged by gatekit compare-reports and
# compare-order against the unchanged allowlist.
#
# A case Ruby refuses at catalog conversion (M17, M23) writes no report on
# either side, so its comparison is the refusal itself: Ruby's
# `Error: Could not run: <detail> (file: ..., line: N)` against voxagent's
# first `voxagent: <detail>`, and no report from either. Ruby stops at the
# first invalid resource; voxagent names every one, in catalog order.
#
# Both agents read one libdir: tests/spine/lib's gatejson, the module's lib/,
# and for 9.1.0 stdlib's lib/ (the concat-with-host case's file_line). 9.1.0
# and stdlib come from the SIMP gate's pinned module cache, 10.0.1 from
# fleet/.vendor (`task fleet:vendor`). The libdir's two concat type files are
# checked against the version's fingerprints, and voxagent's output against
# `applied via extension host` naming concat_file or concat_fragment, so a
# delegated run, Ruby against Ruby, cannot pass as native.

CONCAT_V9_CASES="01-basic 02-rerun 03a-order-numeric 03b-order-alpha 03c-order-mixed-error
  03d-order-default-and-number-str 04-ensure-newline 05-existing-differs 06-replace-false
  07-create-empty-file 08-fragment-targeting 09-owner-group-mode 10-validate-cmd 12-sensitive
  13-refresh-order 13b-refresh-insync 14-declared-file 15a-simp-limits 15b-simp-sudoers
  15c-simp-access 18-ensure-absent 21-file-visited-first 22-tag-truthiness
  eval-failure-existing declared-file-by-path concat-with-host"
CONCAT_V9_REFUSED="17a-frag-no-target-tag 17b-frag-order-slash 17d-frag-neither
  17e-file-relative-path 17f-bad-format 17g-frag-order-colon-newline 17h-validate-cmd-int
  17i-backup-int 17j-concat-order-enum 17k-frag-target-int 17l-frag-content-int
  17m-frag-order-float 17o-concat-replace-str 23-group-false"
# The cases whose outcome is also on disk, where no report shows it: M5's
# backups (`.bak` beside the file, the clientbucket under the default vardir,
# nothing for the default and for false). After the live run each side's
# state is read and compared: every path the case's `files` lists (stat and
# content) and every clientbucket entry with its content, the extension
# gate's clientbucket listing.
CONCAT_STATE_CASES="05-existing-differs"
CONCAT_V10_CASES="v10-07-create-empty-file v10-08-tag-list v10-12-sensitive
  v10-12b-sensitive-tag-list v10-12c-sensitive-newfile v10-14-declared-file
  v10-18-ensure-absent"

# concat_case_dir CASE: the directory holding CASE's site.pp and seed.sh;
# 2, naming CASE, when it has no site.pp.
concat_case_dir() {
  local dir="$HERE/concat/cases/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: concat: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# concat_check_cases: 2 unless every case's directory and site.pp exist, and
# every state case's files.
concat_check_cases() {
  local c dir rc=0
  for c in $CONCAT_V9_CASES $CONCAT_V9_REFUSED $CONCAT_V10_CASES; do
    dir="$(concat_case_dir "$c")" || { rc=2; continue; }
    if concat_has_state "$c" && [ ! -f "$dir/files" ]; then
      echo "report-diff-container: concat: case $c is missing: no $dir/files" >&2
      rc=2
    fi
  done
  return "$rc"
}

# concat_compile VERSION MODULES...: compiles every case of VERSION into
# $WORK/concat/VERSION/CASE/catalog.json, in one container whose environment
# holds the MODULES directories.
concat_compile() {
  local version="$1" name="concat-compile-$$" c m out
  shift
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: concat $version: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for m in "$@"; do
    podman cp "$m" "$name:/tmp/e/production/modules/${m##*/}" >/dev/null 2>&1 || {
      echo "report-diff-container: concat $version: could not copy $m into the compiler" >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    }
  done
  for c in $(concat_cases "$version") $(concat_refused "$version"); do
    out="$WORK/concat/$version/$c"
    mkdir -p "$out" || return 2
    podman cp "$(concat_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: concat $version: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

concat_cases() {
  case "$1" in
    9.1.0) echo "$CONCAT_V9_CASES" ;;
    10.0.1) echo "$CONCAT_V10_CASES" ;;
  esac
}

concat_refused() {
  [ "$1" != 9.1.0 ] || echo "$CONCAT_V9_REFUSED"
}

# concat_is_refused VERSION CASE: whether Ruby refuses CASE's catalog.
concat_is_refused() {
  local c
  for c in $(concat_refused "$1"); do
    [ "$c" != "$2" ] || return 0
  done
  return 1
}

# concat_side AGENT VERSION CASE MODE: one fresh container, seeded as CASE's
# seed.sh seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which
# also reruns, or noop). Writes $WORK/concat/VERSION/CASE/AGENT-RUN.{out,json}
# for each run; an empty .json is a run that wrote no report.
concat_side() {
  local agent="$1" version="$2" c="$3" mode="$4" out="$WORK/concat/$2/$3"
  local name="concat-$agent-$mode-$$" dir run flags cmd
  dir="$(concat_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  # /tmp/papply is the measurement's: two seeds put their target in sync with
  # Ruby's apply of the case's own catalog before the run under test.
  if ! podman cp "$WORK/concat/$version/libdir" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1 \
    || ! podman exec "$name" sh -c "printf '%s\n' '#!/bin/sh' \
      'exec /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --libdir /tmp/lib --color=false \"\$@\"' \
      >/tmp/papply && chmod +x /tmp/papply" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: concat $version $c: the seed failed in $name:" >&2
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
    if [ "$run" = live ] && concat_has_state "$c"; then
      concat_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

concat_has_state() {
  case " $CONCAT_STATE_CASES " in *" $1 "*) return 0 ;; esac
  return 1
}

# concat_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists, then the clientbucket under the default vardir
# (both agents' default), listed and printed as report-diff-extension.sh's
# clientbucket does.
concat_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    podman exec "$1" sh -c 'if [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" || return 2
  done <"$2"
  echo "== clientbucket"
  podman exec "$1" sh -c '
    b=/opt/puppetlabs/puppet/cache/clientbucket
    [ -d "$b" ] || { echo NONE; exit 0; }
    (cd "$b" && find . -printf "%P %y %m\n" | sort) &&
    (cd "$b" && find . -type f -print | sort | while IFS= read -r f; do printf "%s:\n" "$f" && cat "$f" || exit 1; done)'
}

# concat_judge_state VERSION CASE: compares the two sides' live state.
concat_judge_state() {
  local out="$WORK/concat/$1/$2"
  # A Ruby side that backed up nothing would agree with a voxagent that
  # backed up nothing: the measured .bak and bucket entry must be there.
  if ! grep -q '^== /etc/c05c.conf.bak$' "$out/ruby-live.state" \
    || grep -A1 '^== /etc/c05c.conf.bak$' "$out/ruby-live.state" | grep -q '^ABSENT$' \
    || ! grep -q '/contents:$' "$out/ruby-live.state"; then
    echo "report-diff-container: concat $1 $2: Ruby left no .bak or clientbucket entry; nothing to compare against" >&2
    sed -n '1,20p' "$out/ruby-live.state" >&2
    return 2
  fi
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: concat $1 $2 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: concat $1 $2 live: end state agrees"
}

# concat_native VERSION CASE RUN: 1 when voxagent sent a concat type to the
# extension host.
concat_native() {
  local out="$WORK/concat/$1/$2"
  if grep -Eq '^voxagent: [0-9]+ concat_(file|fragment) resource\(s\) applied via extension host' \
    "$out/voxagent-$3.out"; then
    echo "report-diff-container: concat $1 $2 $3: voxagent delegated concat to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$3.out" >&2
    return 1
  fi
}

# concat_gate: the whole concat half. Returns the worst result.
concat_gate() {
  local pin_hash modules version c mode run rc=0 r pa pb
  concat_check_cases || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2
  [ -d "$ROOT/fleet/.vendor/modules/concat/lib" ] || {
    echo "report-diff-container: fleet/.vendor/modules/concat is missing; run 'task fleet:vendor'" >&2
    return 2
  }
  for version in 9.1.0 10.0.1; do
    mkdir -p "$WORK/concat/$version/libdir" || return 2
    cp -R "$HERE/lib/." "$WORK/concat/$version/libdir/" || return 2
    if [ "$version" = 9.1.0 ]; then
      cp -R "$modules/concat/lib/." "$modules/stdlib/lib/." "$WORK/concat/$version/libdir/" || return 2
      concat_compile "$version" "$modules/concat" "$modules/stdlib" || return 2
    else
      cp -R "$ROOT/fleet/.vendor/modules/concat/lib/." "$WORK/concat/$version/libdir/" || return 2
      concat_compile "$version" "$ROOT/fleet/.vendor/modules/concat" || return 2
    fi
    concat_fingerprints report-diff-container "$WORK/concat/$version/libdir" "$version" || return 2
  done

  for version in 9.1.0 10.0.1; do
    for c in $(concat_cases "$version") $(concat_refused "$version"); do
      for mode in live noop; do
        if concat_is_refused "$version" "$c" && [ "$mode" != live ]; then
          continue
        fi
        concat_side ruby "$version" "$c" "$mode" &
        pa=$!
        concat_side voxagent "$version" "$c" "$mode" &
        pb=$!
        if ! wait "$pa" || ! wait "$pb"; then
          echo "report-diff-container: concat $version $c $mode: a container could not be prepared or run" >&2
          return 2
        fi
      done
      if concat_is_refused "$version" "$c"; then
        g5_refusal "$WORK/concat/$version/$c" "concat $version $c"
        r=$?
        [ "$r" -gt "$rc" ] && rc=$r
        continue
      fi
      for run in live rerun noop; do
        g5_judge "$WORK/concat/$version/$c" "concat $version $c" "$run"
        r=$?
        [ "$r" -gt "$rc" ] && rc=$r
        concat_native "$version" "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
      done
      if concat_has_state "$c"; then
        concat_judge_state "$version" "$c"
        r=$?
        [ "$r" -gt "$rc" ] && rc=$r
      fi
    done
  done
  # concat-with-host exists to load the host beside native concat.
  if ! grep -q '^voxagent: [0-9]* file_line resource(s) applied via extension host' \
    "$WORK/concat/9.1.0/concat-with-host/voxagent-live.out"; then
    echo "report-diff-container: concat 9.1.0 concat-with-host: the extension host did not apply file_line:" >&2
    sed -n '1,10p' "$WORK/concat/9.1.0/concat-with-host/voxagent-live.out" >&2
    [ "$rc" -ge 1 ] || rc=1
  fi
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-concat" \
      && cp -R "$WORK/concat" "$EVIDENCE/container-concat" \
      && echo "report-diff-container: the concat runs are in $EVIDENCE/container-concat" >&2
  fi
  return "$rc"
}
