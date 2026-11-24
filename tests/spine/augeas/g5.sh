# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's augeas half (augeas design §10).
# Uses that script's HERE, ROOT, IMAGE, VOXAGENT, GATEKIT, EVIDENCE and WORK,
# simp/lib.sh's simp_modules and augeasproviders_fingerprints, augeas/image.sh's
# augeas_image and g5-case.sh's g5_judge and g5_refusal.
#
# It runs on localhost/augeas-measure-el, built from $IMAGE by
# images/Containerfile.augeas when missing or when its openvox or libaugeas
# differs from $IMAGE's (augeas/image.sh; a cold build needs the network). When
# podman is rootful the half returns 2 and runs no case: a rootful `sysctl -w`
# would change the host kernel. Rootless, the real `sysctl -w` is refused on
# both sides alike; the success paths run through the measurements' shim
# (sysctl-shim.sh), on both sides.
#
# Every case is a directory under cases/ beside this file: the measured ones
# (augeas measurements M1-M34, M61-M69 and 06a-06r, 2026-11-21: their site.pp,
# seed.sh, files, podman-args and lenses, verbatim, but for 06d's and 06e's
# seeds, which stop when the unmount fails) and the ones this gate adds. A missing case
# stops the half with 2 before anything runs. Each is compiled on its own
# by the image's openvox and applied in FRESH containers, one per agent and
# mode, as it was measured: the cases share /etc/sysctl.conf and
# /etc/ssh/*_config. Per case: live, then a rerun in the same container; then
# --noop, without --show_diff, from a fresh seeded container. Each pair is
# judged by gatekit compare-reports and compare-order against the unchanged
# allowlist. A case's optional `podman-args` file holds extra `podman run`
# arguments, whitespace-split (06d, 06e: --cap-add SYS_ADMIN, to unmount
# /etc/resolv.conf). After the live run each side's state is read and compared: every
# path the case's `files` lists (mode, owner, group, size and `cat -A`), then
# the shim's log of the sysctl commands run, which no report shows.
#
# A case Ruby refuses at catalog conversion (refuse-*) is judged by
# g5_refusal: the same detail, and no report from either side. A case Ruby
# applies but voxagent must send to the extension host (refuse-augeas-*, the
# augeas type's routing conditions) is judged like any case, and its output
# must name augeas among the types `applied via extension host`.
#
# Both agents read one libdir per case: tests/spine/lib's gatejson, the
# lib/puppet/ trees of augeasproviders_core, _sysctl and _ssh and augeas_core's
# lib/puppet and lib/puppet_x from the SIMP gate's pinned module cache (never
# the augeasproviders' lib/augeas/), and augeas/lenses/ only when the case has
# a `lenses` file, holding exactly the cache lenses it lists, or a `lenses`
# directory, holding the files it holds.
# The modulepath is empty, so Ruby loads the types from that libdir too. Cases
# without `lenses` gate libaugeas's NULL-loadpath branch; the ones with it
# (M62-M64, M66-M69, lens-dir-known-hosts-only) the $libdir/augeas/lenses
# branch. The libdir is checked against the fifteen routing fingerprints, and
# voxagent's output must not name a native type among those `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native; sysctl-delegated, whose comment the native sysctl does not
# implement, must name sysctl there, and the refuse-augeas-* cases must name
# augeas.

AUGEAS_IMAGE="${AUGEAS_IMAGE:-localhost/augeas-measure-el:latest}"
AUGEAS_CASES="01a-sysctl-new-file-absent 01b-sysctl-new-stock-file 01c-sysctl-existing-differs
  01d-sysctl-rerun 01e-sysctl-silent-real-sysctl 01f-sysctl-d-default-target
  01g-sysctl-d-explicit-target 01h-sysctl-shim-failure 01i-sysctl-65-resources-slices
  01j-sysctl-duplicates 02a-ssh_config-add 02b-ssh_config-change 02c-ssh_config-rerun
  02d-ssh_config-array-add 02e-ssh_config-array-change 02f-ssh_config-absent
  02g-ssh_config-absent-rerun 03a-sshd_config-plain-add-change 03b-sshd_config-rerun
  03c-sshd_config-array-new-stock 03d-sshd_config-array-new-one-array-only
  03e-sshd_config-array-existing 03f-sshd_config-array-append 03g-sshd_config-match-new
  03h-sshd_config-match-existing 03i-sshd_config-plain-with-match-present
  03j-sshd_config-match-rerun 03k-sshd_config-absent 03l-sshd_config-notify-only
  04a-subsystem-add 04b-subsystem-change 04c-subsystem-rerun 04d-subsystem-with-match
  04e-subsystem-absent 07a-no-simp-lens 07b-simp-ssh-lens 07c-all-simp-lenses
  07d-simp-ssh-lens-match-file 07e-no-simp-lens-match-file 07f-simp-ssh-lens-stock-file-add
  07g-simp-ssh-lens-simple-change 07h-simp-ssh-lens-array-change 07i-simp-ssh-lens-absent
  sysctl-existing-w-fails sysctl-save-fails sysctl-prefetch-load-error
  sysctl-stale-invalid-key sysctl-file-before sshd-two-handles sshd-mode-preserved
  lens-dir-known-hosts-only sysctl-delegated
  06a-augeas-kmod-new 06b-augeas-kmod-rerun 06c-augeas-kmod-option-and-rm
  06d-augeas-resolv-simp-shape 06e-augeas-resolv-rerun
  06i-augeas-changes-string-vs-array 06k-augeas-kmod-option-and-rm-correct
  06r-augeas-resolv-save-fails-on-mount augeas-runtime-errors augeas-notified
  augeas-apply-ignores-libdir-lens"
AUGEAS_HOSTED="refuse-augeas-onlyif refuse-augeas-force refuse-augeas-parse-error
  refuse-augeas-setm"
AUGEAS_REFUSED="refuse-sysctl-apply-yes refuse-ssh-config-relative-target
  refuse-sshd-array-append-maybe refuse-ensure-bogus"
AUGEAS_TYPES_RE='(sysctl|ssh_config|sshd_config|sshd_config_subsystem|augeas)'

# augeas_case_dir CASE: the directory holding CASE's site.pp and seed.sh;
# 2, naming CASE, when it has no site.pp.
augeas_case_dir() {
  local dir="$HERE/augeas/cases/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: augeas: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# augeas_check_cases: 2 unless every case's directory and site.pp exist.
augeas_check_cases() {
  local c rc=0
  for c in $AUGEAS_CASES $AUGEAS_HOSTED $AUGEAS_REFUSED; do
    augeas_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

augeas_is_refused() {
  local c
  for c in $AUGEAS_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# augeas_compile MODULES: compiles every case into $WORK/augeas/CASE/
# catalog.json, in one container whose environment holds the three
# augeasproviders modules of MODULES.
augeas_compile() {
  local name="augeas-compile-$$" c m out
  podman run --name "$name" --hostname spine.example --detach "$AUGEAS_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: augeas: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for m in augeasproviders_core augeasproviders_sysctl augeasproviders_ssh; do
    podman cp "$1/$m" "$name:/tmp/e/production/modules/$m" >/dev/null 2>&1 || {
      echo "report-diff-container: augeas: could not copy $m into the compiler" >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    }
  done
  for c in $AUGEAS_CASES $AUGEAS_HOSTED $AUGEAS_REFUSED; do
    out="$WORK/augeas/$c"
    mkdir -p "$out" || return 2
    podman cp "$(augeas_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: augeas: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# augeas_libdir MODULES BASE CASE: CASE's libdir, $WORK/augeas/CASE/libdir:
# BASE plus augeas/lenses/ with the lenses CASE's `lenses` file lists (names
# relative to MODULES) or the files its `lenses` directory holds.
augeas_libdir() {
  local lib="$WORK/augeas/$3/libdir" lenses l
  lenses="$(augeas_case_dir "$3")/lenses"
  cp -R "$2" "$lib" || return 2
  if [ -d "$lenses" ]; then
    mkdir -p "$lib/augeas/lenses" && cp "$lenses"/* "$lib/augeas/lenses/"
    return
  fi
  [ -f "$lenses" ] || return 0
  mkdir -p "$lib/augeas/lenses" || return 2
  while read -r l; do
    [ -n "$l" ] || continue
    cp "$1/$l" "$lib/augeas/lenses/" || return 2
  done <"$lenses"
}

# augeas_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop), in a container started with CASE's podman-args. Writes $WORK/augeas/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
augeas_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/augeas/$2"
  local name="augeas-$agent-$mode-$$" dir run flags cmd pargs=''
  dir="$(augeas_case_dir "$c")"
  [ ! -f "$dir/podman-args" ] || pargs="$(cat "$dir/podman-args")"
  # pargs is repo-controlled and word-split on purpose.
  # shellcheck disable=SC2086
  podman run --name "$name" --hostname spine.example $pargs --detach "$AUGEAS_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  # /tmp/papply is the measurement's: the rerun cases' seeds put their target
  # in sync with Ruby's apply of the case's own catalog first.
  if ! podman cp "$out/libdir" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1 \
    || ! podman cp "$HERE/augeas/sysctl-shim.sh" "$name:/tmp/sysctl-shim.sh" >/dev/null 2>&1 \
    || ! podman exec "$name" sh -c "printf '%s\n' '#!/bin/sh' \
      'exec /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --libdir /tmp/lib --color=false \"\$@\"' \
      >/tmp/papply && chmod +x /tmp/papply" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: augeas $c: the seed failed in $name:" >&2
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
      augeas_state "$name" "$dir/files" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# augeas_state NAME FILES: the state a live run left in container NAME, one
# block per path FILES lists, as the measurements' probe.sh dumps it, then the
# shim's command log (NONE without the shim).
augeas_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    podman exec "$1" sh -c 'if [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" || return 2
  done <"$2"
  echo "== /tmp/sysctl.calls"
  podman exec "$1" sh -c 'if [ -e /tmp/sysctl.calls ]; then cat /tmp/sysctl.calls; else echo NONE; fi' || return 2
}

# augeas_judge_state CASE: compares the two sides' live state.
augeas_judge_state() {
  local out="$WORK/augeas/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: augeas $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: augeas $1 live: end state agrees"
}

# augeas_native CASE RUN: 1 when voxagent's routing contradicts the case:
# sysctl-delegated must send sysctl to the extension host, the refuse-augeas-*
# cases augeas, every other case none of the five types.
augeas_native() {
  local out="$WORK/augeas/$1" re
  re="^voxagent: [0-9]+ $AUGEAS_TYPES_RE resource\\(s\\) applied via extension host"
  # The list wraps; a newline must separate names as a space does.
  case " ${AUGEAS_HOSTED//$'\n'/ } " in *" $1 "*)
    grep -Eq '^voxagent: [0-9]+ augeas resource\(s\) applied via extension host' "$out/voxagent-$2.out" && return 0
    echo "report-diff-container: augeas $1 $2: voxagent did not send augeas to the extension host:" >&2
    sed -n '1,10p' "$out/voxagent-$2.out" >&2
    return 1
    ;;
  esac
  if [ "$1" = sysctl-delegated ]; then
    grep -Eq '^voxagent: [0-9]+ sysctl resource\(s\) applied via extension host' "$out/voxagent-$2.out" && return 0
    echo "report-diff-container: augeas $1 $2: voxagent did not send sysctl to the extension host:" >&2
    sed -n '1,10p' "$out/voxagent-$2.out" >&2
    return 1
  fi
  if grep -Eq "$re" "$out/voxagent-$2.out"; then
    echo "report-diff-container: augeas $1 $2: voxagent delegated a native augeas type to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# augeas_gate: the whole augeas half. Returns the worst result.
augeas_gate() {
  local pin_hash modules base c mode run rc=0 r pids p
  augeas_check_cases || return 2
  if [ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" != true ]; then
    echo "report-diff-container: augeas: podman is not rootless, and a rootful sysctl -w would change" >&2
    echo "report-diff-container: augeas: the host kernel; no augeas case runs" >&2
    return 2
  fi
  mkdir -p "$WORK/augeas" || return 2
  augeas_image report-diff-container "$IMAGE" "$AUGEAS_IMAGE" "$WORK/augeas/image-build.log" || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2

  base="$WORK/augeas/libdir"
  if ! mkdir -p "$base/puppet" || ! cp -R "$HERE/lib/." "$base/"; then
    echo "report-diff-container: augeas: could not create $base" >&2
    return 2
  fi
  for c in augeasproviders_core augeasproviders_sysctl augeasproviders_ssh; do
    cp -R "$modules/$c/lib/puppet/." "$base/puppet/" || {
      echo "report-diff-container: augeas: could not merge $c's lib/puppet into $base" >&2
      return 2
    }
  done
  if ! cp -R "$modules/augeas_core/lib/." "$base/"; then
    echo "report-diff-container: augeas: could not merge augeas_core's lib into $base" >&2
    return 2
  fi
  augeasproviders_fingerprints report-diff-container "$base" || return 2
  augeas_compile "$modules" || return 2

  for c in $AUGEAS_CASES $AUGEAS_HOSTED $AUGEAS_REFUSED; do
    augeas_libdir "$modules" "$base" "$c" || {
      echo "report-diff-container: augeas $c: could not assemble the libdir" >&2
      return 2
    }
    pids=''
    for mode in live noop; do
      augeas_is_refused "$c" && [ "$mode" != live ] && continue
      augeas_side ruby "$c" "$mode" &
      pids="$pids $!"
      augeas_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: augeas $c: a container could not be prepared or run" >&2
      return 2
    fi
    if augeas_is_refused "$c"; then
      g5_refusal "$WORK/augeas/$c" "augeas $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/augeas/$c" "augeas $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      augeas_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(augeas_case_dir "$c")/files" ]; then
      augeas_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-augeas" \
      && cp -R "$WORK/augeas" "$EVIDENCE/container-augeas" \
      && echo "report-diff-container: the augeas runs are in $EVIDENCE/container-augeas" >&2
  fi
  return "$rc"
}
