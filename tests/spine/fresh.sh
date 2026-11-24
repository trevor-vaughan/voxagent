# shellcheck shell=bash
# Sourced by the spine gates, which run prebuilt binaries from bin/.
#
# A prebuilt binary cannot be told apart from a STALE one, and a stale binary is
# a FALSE GREEN: the gate compares Ruby against code that is no longer in the
# tree. Measured the hard way -- a deliberate sabotage of the attribution code
# reported "0 divergent" because bin/voxagent predated the edit by four minutes,
# and the sabotage was wrongly recorded as a hole in the gate. The same holds
# for bin/gatekit from the other side: a stale judge compares both
# implementations with rules that are no longer in the tree.
#
# state-diff.sh does not source this: it builds its own binaries from the tree.
#
# `task` cannot hit this, because each gate's task declares deps: [build]. It
# bites a DIRECT invocation, which is exactly how these scripts are run while
# iterating on a failure -- the moment the answer matters most.
#
# Every refusal is exit 2, could-not-run, rather than 1: nothing was compared,
# so it is not a result about the implementations. That includes a `find` that
# could not read the source tree: answering "nothing is newer" then would be
# the false green this exists to prevent.

# require_fresh LABEL ROOT BINARY...
#
# Exits 2, naming LABEL, unless every BINARY exists, is executable, and is newer
# than every Go source under ROOT/cmd and ROOT/internal.
require_fresh() {
  local label="$1" root="$2" bin newer
  shift 2
  for bin in "$@"; do
    [ -x "$bin" ] || {
      echo "$label: $bin is not built; run 'task build'" >&2
      exit 2
    }
    newer="$(find "$root/cmd" "$root/internal" -name '*.go' -newer "$bin" -print -quit)" || {
      echo "$label: could not compare $bin with the Go sources" >&2
      exit 2
    }
    [ -z "$newer" ] || {
      echo "$label: $bin is older than the Go sources; run 'task build'" >&2
      exit 2
    }
  done
}
