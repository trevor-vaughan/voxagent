#!/usr/bin/env bash
# The extension host's acceptance gate: the public SIMP catalog, applied
# for real by Ruby Puppet and by voxagent in identical systemd-booted containers,
# compared by report, walk order and end state.
#
# The catalog is built from pinned forge releases (simp/modules.pin) and the
# fixture beside it (simp/site.pp, hiera.yaml, data/common.yaml). The modules
# are installed once, by the image's own openvox, into
# .test-output/simp/modules-<pin hash>/, so a warm run fetches nothing from the
# forge. The catalog is compiled in a booted container of the same image, with
# that container's openvox and facts, because the node that applies a catalog
# is the node it is compiled for; a host compile leaks the host into it. Three
# premises guard against an environment that compiles to almost nothing: at
# least 700 resources, at least 60 Sysctl, and at least one rich Binary value,
# which proves the rich-data path is exercised. The merged libdir's concat type
# files must carry puppetlabs-concat 9.1.0's fingerprints, and voxagent must
# not name concat_file or concat_fragment among the types it applies via the
# extension host: both are native on this catalog (concat design §1). The
# same holds for sysctl, ssh_config, sshd_config, sshd_config_subsystem and
# augeas, against the fifteen augeas fingerprints (augeas design §1 item 5;
# augeas generic design §1 item 4), and for cron, against cron_core 2.1.0's
# three fingerprints (cron design §1 item 6).
# The merged libdir holds SIMP's ssh.aug and resolv.aug in augeas/lenses, so
# the 49 Ssh_config resources take the custom-lens path. Its Sysctl coverage
# is mostly the failure path: rootless, the real `sysctl -w` is refused for 62
# of the 63, and the one `silent => true` resource ignores the refusal; G5's
# shim cases gate the success paths. Rootful podman exits 2: a rootful
# `sysctl -w` would change the host kernel.
#
# TWO FRESH CONTAINERS, ONE PER AGENT, booted and applied in parallel, for the
# reason report-diff-systemd.sh gives: an apply mutates the machine. Each gets
# every module's lib/, merged pluginsync-style, plus tests/spine/lib's gatejson
# report processor, at /opt/puppetlabs/puppet/cache/lib, the default libdir of
# both agents. Named with this script's PID so the cleanup trap cannot destroy
# anyone else's container.
#
# Besides the reports, end states are compared, because two reports can
# agree while the machines differ: `systemctl
# is-enabled tmp.mount`, the mode of /etc/audit, the reboot notifications
# simplib's reboot_notify records, and /etc/sysctl.conf, /etc/ssh/ssh_config,
# /etc/ssh/sshd_config and /etc/modprobe.d/blacklist.conf (mode, owner and
# content). /etc/resolv.conf is not among them: it is a mount in the
# container, so the Augeas resource that edits it fails its save on both
# sides and the reports carry the comparison.
#
# NETWORK: needed only on a cold run, to fetch the 77 module releases from the
# forge and to build the image from the CentOS Stream mirrors. The catalog
# holds Package[haveged], so every apply, warm or cold, runs `dnf install
# haveged`, which asks the mirrors for metadata; haveged is in EPEL, which the
# image does not enable, so it fails on both sides either way. Measured
# 2026-10-08 with both apply containers on `--network=none`: haveged failed on
# both sides, each report held the same 73 failed statuses, and the comparison
# found the same 28 divergent leaves as the networked run. The compiling
# container keeps podman's default network all the same: the catalog derives
# from its networking facts, and without an interface the compile fails
# (Class[Pupmod::Agent::Cron]: minute_base got Undef).
#
# Inside a nested container rootless systemd needs a delegated cgroup to boot,
# and every podman call inherits the cgroup of the shell that runs this
# script; see docs/maintainers/apply-spine.md, "Running the systemd gate".
#
# DERIVE=1 applies the catalog with Ruby on both sides instead and checks
# tests/spine/allowlist.json's run_to_run list against the pair with `gatekit
# run-to-run`, which is how the leaves this catalog varies run to run are
# measured rather than asserted.
#
# On exit 1 or 2 whatever the run produced is kept in .test-output/spine/simp-*.
#
#   0  the reports, the walk order and the end states agree outside the
#      allowlist (DERIVE=1: the allowlist covers exactly what Ruby varies)
#   1  they diverge -- a result about voxagent (DERIVE=1: about the allowlist)
#   2  the comparison could not run
set -uo pipefail
# Inherited noclobber disarms every `: > "$f"` writability probe in this tree,
# and makes the plain `>` redirects below fail on a reused path. A gate owns its
# work directory outright, so clobber-protection guards nothing of value here.
set +C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine}"
IMAGE="${IMAGE:-localhost/voxagent-simp:latest}"
SYSTEMD_IMAGE="${SYSTEMD_IMAGE:-localhost/voxagent-systemd:latest}"
BASE_IMAGE="${BASE_IMAGE:-localhost/fleet-node-el:latest}"
DERIVE="${DERIVE:-0}"
FIXTURE="$HERE/simp"

# The same pin, and the same tripwire, as report-diff-container.sh: the base
# installs openvox-agent unversioned, so a rebuild floats the oracle, and exit 2
# beats a green run measured against a reference nobody reviewed.
WANT_PUPPET="${WANT_PUPPET:-8.34.0}"

COMPILER="simp-compile-$$"
if [ "$DERIVE" = 1 ]; then
  LABEL_A=ruby1 LABEL_B=ruby2 AGENT_B=ruby
else
  LABEL_A=ruby LABEL_B=voxagent AGENT_B="$VOXAGENT"
fi
SIDE_A="simp-$LABEL_A-$$"
SIDE_B="simp-$LABEL_B-$$"

command -v podman >/dev/null 2>&1 || {
  echo "report-diff-simp: podman is not available" >&2
  exit 2
}
# The catalog's Sysctl resources run `sysctl -w`, which rootless is refused on
# both sides alike and rootful would change the host kernel.
if [ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" != true ]; then
  echo "report-diff-simp: podman is not rootless, and a rootful sysctl -w would change the host kernel" >&2
  exit 2
fi
podman image exists "$BASE_IMAGE" || {
  echo "report-diff-simp: $BASE_IMAGE is not built; run 'task fleet:images'" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff-simp "$ROOT" "$VOXAGENT" "$GATEKIT"

# shellcheck source=build-image.sh
. "$HERE/build-image.sh" || exit 2
# shellcheck source=simp/lib.sh
. "$FIXTURE/lib.sh" || exit 2

WORK="$(mktemp -d)" || exit 2

# keep_evidence: copy whatever the run produced to $EVIDENCE/simp-*, because
# reading a divergence needs both reports side by side and the work tree is
# about to go. Called by on_exit, the EXIT trap. The previous run's simp-* evidence is removed first, so files
# from two runs never mix. The environment and the merged libdir are left out:
# both are rebuilt from the module cache.
# shellcheck disable=SC2317 # reached only through the EXIT trap
keep_evidence() {
  local f
  if ! rm -rf "${EVIDENCE:?}"/simp-* || ! mkdir -p "$EVIDENCE"; then
    return 1
  fi
  for f in "$WORK"/*; do
    [ -e "$f" ] || continue
    case "${f##*/}" in env | libdir) continue ;; esac
    cp -R "$f" "$EVIDENCE/simp-${f##*/}" || return 1
  done
  echo "report-diff-simp: the evidence is in $EVIDENCE/simp-*" >&2
}
# shellcheck disable=SC2317 # the EXIT trap
on_exit() {
  local rc=$?
  [ "$rc" -eq 0 ] || keep_evidence
  rm -rf "$WORK"
  podman rm -f "$COMPILER" "$SIDE_A" "$SIDE_B" >/dev/null 2>&1
  exit "$rc"
}
trap on_exit EXIT

# Containerfile.simp builds FROM the systemd image, so that is built first.
if ! podman image exists "$SYSTEMD_IMAGE"; then
  echo "report-diff-simp: building $SYSTEMD_IMAGE"
  build_image report-diff-simp "$SYSTEMD_IMAGE" "$HERE/images/Containerfile.systemd" \
    "$HERE/images" "$WORK/systemd-build.log" || exit 2
fi
if ! podman image exists "$IMAGE"; then
  echo "report-diff-simp: building $IMAGE"
  build_image report-diff-simp "$IMAGE" "$HERE/images/Containerfile.simp" \
    "$HERE/images" "$WORK/simp-build.log" || exit 2
fi

got_puppet="$(podman run --rm "$IMAGE" /opt/puppetlabs/bin/puppet --version 2>/dev/null)" || {
  echo "report-diff-simp: cannot read the image's puppet version" >&2
  exit 2
}
if [ "$got_puppet" != "$WANT_PUPPET" ]; then
  echo "report-diff-simp: $IMAGE carries puppet $got_puppet but the oracle is" >&2
  echo "report-diff-simp: pinned at $WANT_PUPPET; refusing to compare against an" >&2
  echo "report-diff-simp: unpinned reference" >&2
  exit 2
fi

# The cache is keyed on the pin file's content, so editing a pin installs a
# fresh tree instead of verifying the old one against the new pins.
pin_hash="$(sha256sum "$FIXTURE/modules.pin")" || exit 2
MODULES="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
mkdir -p "${MODULES%/*}" || exit 2
simp_modules "$MODULES" "$IMAGE" "$FIXTURE/modules.pin" || exit 2

ENVDIR="$WORK/env/production"
if ! mkdir -p "$ENVDIR/manifests" "$ENVDIR/data" \
  || ! cp "$FIXTURE/site.pp" "$ENVDIR/manifests/site.pp" \
  || ! cp "$FIXTURE/hiera.yaml" "$ENVDIR/hiera.yaml" \
  || ! cp "$FIXTURE/data/common.yaml" "$ENVDIR/data/common.yaml" \
  || ! cp -R "$MODULES" "$ENVDIR/modules"; then
  echo "report-diff-simp: could not assemble the environment in $ENVDIR" >&2
  exit 2
fi
# The module lib/ trees, then the gatejson report processor.
simp_libdir "$MODULES" "$WORK/libdir" || exit 2
cp -R "$HERE/lib/." "$WORK/libdir/" || {
  echo "report-diff-simp: could not add $HERE/lib to the libdir" >&2
  exit 2
}
# The pin holds puppetlabs-concat 9.1.0, whose two type files route
# concat_file and concat_fragment natively (concat design §1 item 5). A cache
# holding anything else would send both to the extension host, unnoticed.
concat_fingerprints report-diff-simp "$WORK/libdir" 9.1.0 || exit 2
# Likewise augeasproviders_core 4.2.0, _sysctl 4.1.0, _ssh 7.0.0 and
# augeas_core 2.1.0, whose fifteen files route sysctl, ssh_config, sshd_config,
# sshd_config_subsystem and augeas natively (augeas design §1 item 5).
augeasproviders_fingerprints report-diff-simp "$WORK/libdir" || exit 2
# Likewise puppetlabs-cron_core 2.1.0, whose three files route cron natively
# (cron design §1 item 6).
cron_core_fingerprints report-diff-simp "$WORK/libdir" || exit 2

mkdir -p "$WORK/compile" || exit 2
simp_boot "$COMPILER" "$IMAGE" || exit 2
simp_compile "$COMPILER" "$WORK/env" "$WORK/compile" || exit 2
podman rm -f "$COMPILER" >/dev/null 2>&1
CATALOG="$WORK/compile/catalog.json"

# The premises, counted on the compiler's compact JSON. A resource is the one
# object with a "type" immediately followed by a "title".
resources="$(grep -o '"type":"[^"]*","title":' "$CATALOG" | wc -l)"
sysctls="$(grep -o '"type":"Sysctl"' "$CATALOG" | wc -l)"
binaries="$(grep -o '"__ptype":"Binary"' "$CATALOG" | wc -l)"
echo "report-diff-simp: catalog: $resources resources, $sysctls Sysctl, $binaries Binary values"
if [ "$resources" -lt 700 ] || [ "$sysctls" -lt 60 ] || [ "$binaries" -lt 1 ]; then
  echo "report-diff-simp: the catalog is not the SIMP catalog the gate measures:" >&2
  echo "report-diff-simp: it needs at least 700 resources, 60 Sysctl and one Binary value" >&2
  exit 2
fi

(simp_boot "$SIDE_A" "$IMAGE" \
  && simp_apply "$SIDE_A" ruby "$CATALOG" "$WORK/libdir" "$WORK/$LABEL_A") &
pid_a=$!
(simp_boot "$SIDE_B" "$IMAGE" \
  && simp_apply "$SIDE_B" "$AGENT_B" "$CATALOG" "$WORK/libdir" "$WORK/$LABEL_B") &
pid_b=$!
wait "$pid_a"
rc_a=$?
wait "$pid_b"
rc_b=$?
[ "$rc_a" -eq 0 ] && [ "$rc_b" -eq 0 ] || exit 2
REPORT_A="$WORK/$LABEL_A/report.json"
REPORT_B="$WORK/$LABEL_B/report.json"
echo "report-diff-simp: applied: $LABEL_A rc=$(cat "$WORK/$LABEL_A/rc") $(cat "$WORK/$LABEL_A/wall")s," \
  "$LABEL_B rc=$(cat "$WORK/$LABEL_B/rc") $(cat "$WORK/$LABEL_B/wall")s"

if [ "$DERIVE" = 1 ]; then
  "$GATEKIT" run-to-run "$REPORT_A" "$REPORT_B" "$HERE/allowlist.json" >"$WORK/run-to-run.txt"
  rc=$?
  cat "$WORK/run-to-run.txt"
  case "$rc" in
    0) echo "report-diff-simp: the allowlist covers exactly what Ruby varies" ;;
    1) echo "report-diff-simp: the allowlist no longer matches what Ruby varies" >&2 ;;
    *)
      echo "report-diff-simp: the derivation could not run" >&2
      rc=2
      ;;
  esac
  exit "$rc"
fi

# The full comparison runs to thousands of lines on this catalog, so only its
# summary line is printed; the whole of it is kept as evidence.
"$GATEKIT" compare-reports "$REPORT_A" "$REPORT_B" "$HERE/allowlist.json" >"$WORK/compare.txt"
rc=$?
case "$rc" in
  0 | 1) sed -n '1p' "$WORK/compare.txt" ;;
  *)
    echo "report-diff-simp: the comparison could not run" >&2
    exit 2
    ;;
esac
# The walk order: compare-reports matches statuses by ref and logs as a
# multiset, so it cannot see the order resources were evaluated in.
# compare-order reads it from each report's log order.
"$GATEKIT" compare-order "$REPORT_A" "$REPORT_B" >"$WORK/order.txt"
case $? in
  0) sed -n '1p' "$WORK/order.txt" ;;
  1)
    echo "report-diff-simp: the walk order diverges" >&2
    sed -n '1,5p' "$WORK/order.txt" >&2
    rc=1
    ;;
  *)
    echo "report-diff-simp: the walk order could not be compared" >&2
    exit 2
    ;;
esac

# The end state each agent left, one `what|value` line each. `|` occurs in
# none of the values. An empty value means the reading itself failed. On Ruby's
# side that is could-not-run, since there is no reference to compare against;
# on voxagent's it is a result about voxagent, and the empty value is left in
# place to diverge from Ruby's.
for label in "$LABEL_A" "$LABEL_B"; do
  side="simp-$label-$$"
  podman exec "$side" sh -c '
    echo "tmp.mount is-enabled|$(systemctl is-enabled tmp.mount 2>/dev/null)"
    echo "/etc/audit mode|$(stat -c %a /etc/audit 2>/dev/null)"
  ' >"$WORK/$label.state" 2>/dev/null
  for what in 'tmp.mount is-enabled' '/etc/audit mode'; do
    if [ "$label" = "$LABEL_A" ] \
      && [ -z "$(awk -F'|' -v w="$what" '$1 == w { print $2 }' "$WORK/$label.state")" ]; then
      echo "report-diff-simp: could not read $what in $side" >&2
      exit 2
    fi
  done
  # The reboot notifications, compared by record key
  # and each record's reason only: `updated` is a timestamp, and
  # reboot_control_metadata is not a record (simplib
  # provider/reboot_notify/notify.rb:111-122). Parsed by the image's own
  # openvox Ruby inside the container under test -- the oracle's interpreter,
  # not a language of this repository.
  if ! podman exec "$side" /opt/puppetlabs/puppet/bin/ruby -rjson -e \
    'r = JSON.parse(File.read(ARGV[0])); r.delete("reboot_control_metadata"); r.keys.sort.each { |k| puts "#{k}\t#{r[k]["reason"]}" }' \
    /opt/puppetlabs/puppet/cache/reboot_notifications.json >"$WORK/$label.reboot" 2>/dev/null; then
    # Ruby's side is the reference: without its notifications there is
    # nothing to compare against. voxagent's missing or unreadable file is a
    # result about voxagent.
    if [ "$label" = "$LABEL_A" ]; then
      echo "report-diff-simp: could not read the reboot notifications Ruby left in $side" >&2
      exit 2
    fi
    echo "unreadable" >"$WORK/$label.reboot"
  fi
  if [ "$label" = "$LABEL_A" ] && [ ! -s "$WORK/$label.reboot" ]; then
    echo "report-diff-simp: Ruby recorded no reboot notification; refusing to compare nothing" >&2
    exit 2
  fi
  sed 's/^/reboot notification|/' "$WORK/$label.reboot" >>"$WORK/$label.state" || exit 2
  # The three files the augeasproviders types edit and the one the Augeas
  # resources save: mode, owner and every byte (cat -A), each line prefixed
  # with the path.
  for f in /etc/sysctl.conf /etc/ssh/ssh_config /etc/ssh/sshd_config /etc/modprobe.d/blacklist.conf; do
    if ! podman exec "$side" sh -c 'stat -c "mode=%a owner=%U group=%G" "$1" && cat -A "$1"' sh "$f" \
      >"$WORK/$label.file" 2>/dev/null; then
      if [ "$label" = "$LABEL_A" ]; then
        echo "report-diff-simp: could not read $f in $side" >&2
        exit 2
      fi
      echo "unreadable" >"$WORK/$label.file"
    fi
    sed "s#^#$f|#" "$WORK/$label.file" >>"$WORK/$label.state" || exit 2
  done
done
diff "$WORK/$LABEL_A.state" "$WORK/$LABEL_B.state" >"$WORK/state.diff"
case $? in
  0) ;;
  1)
    echo "report-diff-simp: the end state differs (< ruby, > voxagent):" >&2
    grep '^[<>]' "$WORK/state.diff" >&2
    rc=1
    ;;
  *)
    echo "report-diff-simp: the end states could not be compared" >&2
    exit 2
    ;;
esac

# Both checks below read apply.out; grep exits 2 for a file it cannot read,
# which an `if` would take for "no match" and pass.
if [ ! -r "$WORK/$LABEL_B/apply.out" ]; then
  echo "report-diff-simp: $WORK/$LABEL_B/apply.out is missing or unreadable, cannot check for delegation to the extension host" >&2
  exit 2
fi

# Native concat: voxagent names every type it delegates, and concat must not
# be one of them.
if grep -E '^voxagent: [0-9]+ concat_(file|fragment) resource\(s\) applied via extension host' \
  "$WORK/$LABEL_B/apply.out" >&2; then
  echo "report-diff-simp: voxagent applied concat through the extension host, not natively" >&2
  rc=1
fi

# Native augeasproviders types, augeas and cron (the four Cron resources, with
# cron_core 2.1.0 in the libdir): none of the six may be among them either.
# The catalog's Sysctl coverage is mostly of the failure path: rootless, the
# real `sysctl -w` is refused, so the success paths (`-w` succeeding, then the
# save) are gated only by G5's shim cases.
if grep -E '^voxagent: [0-9]+ (sysctl|ssh_config|sshd_config|sshd_config_subsystem|augeas|cron) resource\(s\) applied via extension host' \
  "$WORK/$LABEL_B/apply.out" >&2; then
  echo "report-diff-simp: voxagent applied an augeasproviders type, augeas or cron through the extension host, not natively" >&2
  rc=1
fi

case "$rc" in
  0) echo "report-diff-simp: reports agree outside the allowlist" ;;
  1) echo "report-diff-simp: reports diverge" >&2 ;;
esac
exit "$rc"
