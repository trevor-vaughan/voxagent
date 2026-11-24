#!/usr/bin/env bash
# Gate check 4: prove that exported resources break the two-field allowlist.
#
# Sequence:
#   1. node-el exports its base resource into OpenVoxDB.
#   2. voxdiff compiles node-deb via the left wrapper  -- collects the base set.
#   3. The right wrapper re-exports from node-el as "mutated", then compiles
#      node-deb again -- now collecting a different set.
#   4. voxdiff reports the difference.
#
# voxdiff runs on the host here, because only the host can drive step 3: a
# container cannot podman exec a sibling.
#
# EXIT CODES, inverted from the other gates and deliberately so. This is the
# only check that asserts a divergence, so 0 means the divergence happened and
# the negative control worked; 1 means it did not, which is a finding about the
# fleet; 2 means the comparison could not run at all. Checks 1-3 all report
# agreement, so this is the only evidence the harness can detect anything --
# which is why its assertion is the tightest in the tree rather than the
# loosest.
set -euo pipefail
# Inherited noclobber disarms every `: > "$f"` writability probe in this tree.
# Measured: under `set -C`, the probe SUCCEEDS by creating $f, and the redirect
# it was written to predict then fails with "cannot overwrite existing file".
# `env SHELLOPTS=noclobber` reaches here -- confirmed through two process hops
# and a shebang script -- so this is a caller's shell setting, not a hypothetical.
#
# Those probes exist to stop an outage being reported as a finding about the
# fleet; see gate-determinism.sh, which records the DIVERGED-for-a-run-that-never-
# happened measurement that put them there. Leaving noclobber active would let
# that defect back in with the guard still apparently passing.
#
# A gate owns its output directory outright, so clobber-protection guards
# nothing of value here.
set +C

# Usage errors exit 2, like every other could-not-run condition here.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-exports.sh <out-dir>" >&2 || exit 2
  exit 2
fi

# Resolved once and absolutely. gate-determinism-negative.sh invokes another gate
# by path, and gate-exports.sh resolves voxdiff and the two check-4 wrappers the
# same way, so a relative $0 breaks those callers depending on the caller's cwd.
# `|| exit 2` because a failing command substitution under `set -e` exits 1, and
# 1 is a result about the fleet in every gate here.
BIN_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 2

# Guarded, not bare. `. missing-file` under `set -e` exits 1, so a gate-common.sh
# that failed to load would be reported as a divergence, an instability finding
# or a collapsed hiera axis depending on which script hit it first -- the eighth
# channel of the same defect; more were found afterwards in redirect targets.
#
# Safe against a false positive: this reads the sourced file's own exit status,
# and gate-common.sh ends in an explicit `:` sentinel that pins that to 0.
# shellcheck source=gate-common.sh
. "$BIN_DIR/gate-common.sh" || {
  echo "gate: cannot load ${BIN_DIR}/gate-common.sh" >&2 || exit 2
  exit 2
}

# Resolved from the script's own location, not the caller's cwd: this gate is
# run directly by the venom suite and by hand, from more than one directory.
VOXDIFF="${VOXDIFF:-$BIN_DIR/../../bin/voxdiff}"
if [ ! -x "$VOXDIFF" ]; then
  echo "gate-exports: $VOXDIFF is missing or not executable; run 'task build'" >&2 || exit 2
  exit 2
fi

# Up front, before the export below: gatekit answers the export poll, and a
# missing or stale one must stop the gate before it changes OpenVoxDB's state.
fleet_require_gatekit
# The export talks TLS to OpenVoxDB, and an expired CRL or certificate fails it
# with nothing but a compile error. See fleet_require_certs.
fleet_require_certs

fleet_require_writable_dir "$OUT_DIR"
fleet_clear_artefacts "$OUT_DIR"/exports.txt
log="$OUT_DIR/exports.txt"

echo "step 1: node-el exports its base resource" || exit 2
fleet_export_as base
fleet_wait_for_export "fleet-exported-node-el-base"

# Probe-then-use, deliberately here and not beside the clear at the top. The gap
# between those two points is the largest in this tree: fleet_export_as compiles
# a catalog inside a container and fleet_wait_for_export polls OpenVoxDB for up
# to 60 seconds, so a probe taken before them would be long stale by the time the
# redirection below runs.
#
# A failed open gives code=1, and 1 is this gate's code for "the fleet did not
# diverge as asserted" -- on the one check that proves this harness can detect
# anything at all. Both greps below then fail on the absent log, and the script
# ends up printing the self-refuting "expected exit 1 ...; got exit 1".
# Measured: exit 1, for a comparison that never ran.
: > "$log" || {
  echo "gate-exports: cannot open ${log}" >&2 || exit 2
  exit 2
}

echo "step 2-4: comparing node-deb across a change in exported state" || exit 2
set +e
"$VOXDIFF" compare \
  -node node-deb \
  -left  "$BIN_DIR/puppet-node-deb" \
  -right "$BIN_DIR/puppet-node-deb-after-export" \
  -environmentpath "$FLEET_ENVIRONMENTPATH" \
  -environment "$FLEET_ENVIRONMENT" \
  -codedir "$FLEET_CODEDIR" \
  > "$log" 2>&1
code=$?
set -e

# voxdiff's contract is enforced here, not assumed, exactly as both negative
# controls enforce it on their nested gate runs.
#
# Without this, only three statuses were classified: `code -eq 1` plus both
# markers meant diverged-as-asserted, `code -eq 2` meant could-not-run, and
# EVERYTHING else fell through to the closing `exit 1`. This check is INVERTED,
# so that 1 asserts the catalogs did NOT diverge -- meaning a voxdiff that was
# never found (127), interrupted (130), OOM-killed (137, plausible with three
# containers compiling) or timed out by CI (143) was reported as "the harness
# detects nothing", on the one check that proves it detects anything.
#
# Measured before this loop existed, with a readable log and both markers
# present:
#
#   voxdiff 0   -> 1   correct: no divergence is a real finding here
#   voxdiff 1   -> 0   correct
#   voxdiff 2   -> 2   correct
#   voxdiff 127 -> 1   WRONG                 130 -> 1   WRONG
#   voxdiff 137 -> 1   WRONG                 143 -> 1   WRONG
#
# A status outside the contract means the thing measured is not the thing this
# gate believes it invoked, which is could-not-run and never a result about the
# fleet. It also makes the closing message honest: "got exit ${code}" is now
# reachable only for 0 and 1, the two statuses it was written to describe.
case "$code" in
  0|1|2) ;;
  *) echo "gate-exports: voxdiff exited outside its 0/1/2 contract (exit ${code});" \
          "the comparison cannot be believed" >&2 || exit 2
     exit 2 ;;
esac

# Assert the specific mutation on the correct sides, not the family name.
# `grep -q fleet-exported` would also pass if the right-hand collection simply
# returned nothing: left would hold the base resource, right nothing, voxdiff
# would exit 1, and the gate would report success while proving nothing about
# the mutation. Requiring base-on-left and mutated-on-right is the difference
# between observing the negative control and merely observing a difference.
#
# fleet_log_contains, not `grep -q`, and on this gate the difference is at its
# sharpest. `grep -q` returns 2 when it cannot read the file, which falls out of
# this `&&` chain exactly as a genuine no-match does, past the `code -eq 2` test
# below -- code is 1 here, not 2 -- and onto the exit 1 at the bottom. This gate
# is INVERTED, so that 1 asserts the catalogs did NOT diverge: on the one check
# that proves this harness can detect anything at all, an unreadable log was
# reported as the harness detecting nothing. Measured, with the same voxdiff
# divergence in both runs:
#
#   log readable    -> "DIVERGED as expected", exit 0
#   log unreadable  -> "expected exit 1 ...; got exit 1", exit 1
#
# The second line is self-refuting on its face, which is what the comment at the
# `: > "$log"` probe above already predicts for a redirect that fails. That probe
# secures the open; it cannot secure a read fifteen lines later, and the three
# `[ -r "$log" ]` guards elsewhere in this file are the same acknowledgement
# applied only where the log is printed, never where the verdict is decided.
if [ "$code" -eq 1 ] \
   && fleet_log_contains 'only_in_left.*fleet-exported-node-el-base' "$log" \
   && fleet_log_contains 'only_in_right.*fleet-exported-node-el-mutated' "$log"; then
  echo "exports: DIVERGED as expected -- external state broke the allowlist" || exit 2
  # Kept, but its rationale has changed and the old one no longer applies. It
  # was written when the two tests above were bare `grep -q`, which tolerate an
  # unreadable log; reaching this line now means both fleet_log_contains calls
  # returned 0, so the log was opened and read moments ago and `-r` is true
  # except in a race. What the guard still buys is that race: unguarded under
  # `set -e`, a `sed` that lost the file between the read above and this line
  # would abort here -- after the divergence was correctly detected -- and turn
  # a pass into exit 1, a false claim that the fleet did not diverge.
  [ -r "$log" ] && { sed 's/^/  /' "$log" || exit 2; }
  exit 0
fi

# Distinguish the two ways this check fails, for the same reason the other
# gates do. voxdiff exit 2 means a wrapper never produced a catalog, so the
# test never ran; anything else means it ran and the fleet did not behave as
# the check asserts, which is a finding about the fleet.
if [ "$code" -eq 2 ]; then
  echo "exports: FAILED to run -- a wrapper produced no catalog (voxdiff exit 2)" >&2 || exit 2
  [ -r "$log" ] && { cat "$log" >&2 || exit 2; }
  exit 2
fi

echo "exports: expected exit 1 with fleet-exported-node-el-base only_in_left and" >&2 || exit 2
echo "         fleet-exported-node-el-mutated only_in_right; got exit ${code}" >&2 || exit 2
[ -r "$log" ] && { cat "$log" >&2 || exit 2; }
exit 1
