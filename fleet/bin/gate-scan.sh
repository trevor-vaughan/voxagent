#!/usr/bin/env bash
# Gate check 3: scan the fleet's module tree for constructs that break
# compile determinism.
#
# EXIT CODES. voxdiff scan exits 0 clean, 1 findings-produced, 2 incomplete.
# THIS SCRIPT deliberately collapses voxdiff's 0 and 1 into its own 0, because
# findings are reviewed, not treated as errors -- the vendored tree contains
# constructs the scanner legitimately flags, and that false-positive rate is an
# accepted design decision. It exits 2, and only 2, when the scan was
# incomplete, matching the other gates. It never exits 1.
#
# An incomplete scan must never present as a clean one: its findings cannot
# support the claim that everything was reviewed.
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

# Usage errors exit 2, not 1: 1 is a result (findings were produced), so a
# caller reading only the exit code must not confuse it with a broken run.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-scan.sh <out-dir>" >&2 || exit 2
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

fleet_require_writable_dir "$OUT_DIR"
fleet_clear_artefacts "$OUT_DIR"/scan.txt "$OUT_DIR"/scan.err

log="$OUT_DIR/scan.txt"

# Probe-then-use. The clear above removes a stale scan.txt, but a redirection
# also fails at open(2) on a path that does NOT exist -- ENOSPC, EDQUOT, a
# read-only remount, or $OUT_DIR losing write permission after
# fleet_require_writable_dir passed -- and `rm -f` on an absent path succeeds
# even in a directory it could not write, so nothing upstream notices.
#
# This gate is the worst place in the tree for that. The redirection fails
# before podman runs, giving code=1, which the case below treats as "findings
# produced"; `tail -1` on the missing log then prints nothing, and the script
# announces "findings written to ..." and exits 0. Measured: a scan that never
# ran, reported as a clean pass, with no INCOMPLETE marker reaching stderr or
# the log -- so both of check 3's assertions pass green. That is a direct
# contradiction of this file's own header.
#
# Routed to INCOMPLETE rather than a bare `exit 2` because exit 0 is what this
# path produced, and the header forbids an incomplete scan presenting as a clean
# one. The marker cannot be appended to $log the way the case below does it --
# $log is the file that could not be opened -- so it goes to stderr alone.
if ! : > "$log" || ! : > "$OUT_DIR/scan.err"; then
  echo "scan: INCOMPLETE -- cannot open ${log} or ${OUT_DIR}/scan.err;" \
       "the scan did not run" >&2 || exit 2
  exit 2
fi

# Scanned on node-el only, and that is sufficient rather than a shortcut: all
# three nodes bind-mount the identical ./code and ./.vendor/modules, and a scan
# is a filesystem parse with no node-dependent input. The environment path
# comes from gate-common.sh so the tree scanned here cannot drift from the tree
# the comparison gates compile.
set +e
podman exec fleet_node_el voxdiff scan /fleet-modules "$FLEET_ENVIRONMENTPATH" \
  > "$log" 2>"$OUT_DIR/scan.err"
code=$?
set -e

case "$code" in
  0) echo "scan: clean, 0 findings" || exit 2 ;;
  1) echo "scan: $(tail -1 "$log")" || exit 2 ;;
  # `cat` is guarded: unguarded under `set -e` a missing scan.err would abort
  # here with status 1, turning the intended could-not-run 2 into a result code.
  # The `|| exit 2` on the writes is the same hazard from the other side -- this
  # gate never exits 1, so a failed write must not be what makes it.
  *) echo "scan: INCOMPLETE (exit ${code}) -- findings cannot be trusted" >&2 || exit 2
     [ -r "$OUT_DIR/scan.err" ] && { cat "$OUT_DIR/scan.err" >&2 || exit 2; }
     echo "scan: INCOMPLETE -- this file is partial and must not be read as a full scan" >> "$log" || exit 2
     exit 2 ;;
esac

echo "scan: findings written to $log" || exit 2
