#!/usr/bin/env bash
# Gate check 1: compile each node twice with the same implementation and
# require the catalogs to be identical.
#
# This is P0's claim, now tested against layered hiera and real per-node
# facts rather than a single flat fixture.
#
# The two kinds of red are reported separately and exit differently, mirroring
# voxdiff's own contract: 1 means the catalogs diverged, which is a result and
# the most important one this fleet can produce; 2 means a comparison could not
# be run at all, which is a broken stack and not a finding. Collapsing them
# would let a stopped container be reported as evidence against P0's allowlist.
#
# Every write to stdout or stderr carries `|| exit 2`. See the "Guarding writes"
# section of gate-common.sh's header for why; the short version is that a failed
# `echo` under `set -e` exits 1, and 1 is a finding about the fleet here.
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

# Usage errors exit 2, not 1: under this script's own contract 1 means the
# catalogs diverged, so a caller inspecting only the exit code would read a
# missing argument as a finding.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-determinism.sh <out-dir>" >&2 || exit 2
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

# Drop logs from a previous run, so a node later removed from NODES cannot
# leave a stale file behind that still reads as current.
fleet_clear_artefacts "$OUT_DIR"/determinism-*.txt

NODES="fleet_node_el:node-el fleet_node_el_alt:node-el-alt fleet_node_deb:node-deb"
diverged=0
failed=0

for pair in $NODES; do
  container="${pair%%:*}"
  node="${pair##*:}"
  log="$OUT_DIR/determinism-${node}.txt"

  echo "== ${node} ==" || exit 2

  # Probe-then-use, the idiom fleet_require_writable_dir uses at entry. Clearing
  # the previous run's logs is not enough on its own: a redirection also fails at
  # open(2) on a path that does NOT exist -- ENOSPC, EDQUOT, a read-only remount,
  # or $OUT_DIR losing write permission after the entry probe passed -- and
  # `rm -f` on an absent path succeeds even in a directory it could not write, so
  # nothing upstream notices. The redirection then fails BEFORE podman runs, and
  # under the `set +e` below that is status 1, which this gate reports as
  # DIVERGED.
  #
  # Measured with $OUT_DIR made read-only after entry: two nodes were reported
  # DIVERGED for a comparison that never executed, pointing at logs that do not
  # exist. The suite asserts ShouldNotContainSubstring DIVERGED, so check 2 would
  # have gone red naming the fleet for an outage on the machine running it.
  #
  # `failed=1` and `continue`, not `exit`: an operational failure already
  # outranks divergence at the end of this script, and aborting here would leave
  # the remaining nodes unmeasured with nothing in the output to say so.
  if ! : > "$log"; then
    echo "  FAILED to run (cannot open $log)" || exit 2
    failed=1
    continue
  fi

  # voxdiff hands both compiles the same confdir and vardir, so the second
  # reads whatever state the first wrote. That is sound here because
  # `puppet catalog find` caches no catalog and environment_timeout is left at
  # its default, so each invocation re-reads and re-evaluates the environment.
  # Demonstrated: with a timestamp in the manifest the second
  # compile produces a different value rather than replaying the first.
  #
  # The directories live under one temp root that is removed afterwards, so
  # repeated gate runs do not grow a long-lived container's writable layer.
  set +e
  podman exec "$container" sh -c "
        d=\$(mktemp -d) || exit 2
        mkdir -p \"\$d/conf\" \"\$d/var\" || exit 2
        voxdiff compare $(fleet_compile_args "$node") \
          -confdir \"\$d/conf\" \
          -vardir  \"\$d/var\"
        s=\$?
        rm -rf \"\$d\"
        exit \$s
      " > "$log" 2>&1
  status=$?
  set -e

  case "$status" in
    0) echo "  IDENTICAL" || exit 2 ;;
    1) echo "  DIVERGED (see $log)" || exit 2
       diverged=1 ;;
    *) echo "  FAILED to run (exit ${status}; see $log)" || exit 2
       failed=1 ;;
  esac

  # Guarded rather than bare: under `set -e` an unreadable log would abort the
  # loop and silently skip the remaining nodes, turning a reporting problem
  # into missing coverage. The `|| exit 2` covers the other half of the same
  # hazard -- sed writing to a stdout that cannot take it -- which would
  # otherwise abort with 1 and report a divergence.
  if [ "$status" -ne 0 ] && [ -r "$log" ]; then
    sed 's/^/    /' "$log" || exit 2
  fi
done

# Operational failure outranks divergence. If any comparison could not run, the
# gate did not measure what it claims to measure, and saying "diverged" would
# be a stronger claim than the evidence supports.
if [ "$failed" -eq 1 ]; then
  exit 2
fi
if [ "$diverged" -eq 1 ]; then
  exit 1
fi
exit 0
