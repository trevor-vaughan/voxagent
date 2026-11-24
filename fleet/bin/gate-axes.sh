#!/usr/bin/env bash
# Gate check 5: assert the fleet's two axes are actually contributing.
#
# Every other check compares a node against ITSELF -- check 1 compiles one node
# twice, check 2 compares two implementations on one node, check 3 is static,
# check 4 compares node-deb before and after an export. Nothing compares node-el
# to node-el-alt. So if the per-node or per-OS-family hiera layer silently
# stopped contributing -- a misnamed hieradata/nodes/*.yaml, a container
# hostname that stops matching %{facts.networking.hostname} -- all three nodes
# would converge on the same catalog shape and every other check would still
# pass green.
#
# fleet_profile's fail() covers the fleet_tier external fact. It cannot cover
# the hiera layers: a missing layer degrades to the common one, which is
# perfectly valid data. Only an out-of-band expectation catches that.
#
# The expected values below duplicate what hieradata/ already says, and that
# duplication is the entire point. This is the one place in the harness where
# DRY is wrong: an expectation derived from the same YAML the catalog was built
# from would agree with whatever that YAML produced, including the degenerate
# case it exists to detect. They were measured against the running fleet.
#
# So when hieradata/ legitimately changes and this check goes red, hand-edit the
# table below to the value you INTEND and confirm the check goes green. Never
# copy the value out of a compiled catalog to make the red go away: that derives
# the expectation from the same YAML again, which is the same mistake as
# generating this table and quietly restores the blind spot it exists to cover.
#
# Exit codes follow the fleet convention: 0 every node carries its expected
# values, 1 an axis has collapsed (a result about the fleet), 2 the check could
# not run.
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

# Usage errors exit 2, not 1: under this script's contract 1 means an axis
# collapsed, so a caller reading only the exit code would take a missing
# argument as a finding about the fleet.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-axes.sh <out-dir>" >&2 || exit 2
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

# Checked once, up front: without this a missing gatekit returns 127 and the
# gate reports a catalog-read failure for what is actually a missing binary,
# and a stale one would read catalogs with rules no longer in the tree.
fleet_require_gatekit

# Drop catalogs from a previous run so a node later removed from the table
# cannot leave a stale file behind that still reads as current.
fleet_clear_artefacts "$OUT_DIR"/axes-*.json "$OUT_DIR"/axes-*.err

# container|node|Notify[fleet-message] message|/var/tmp/fleet-tier content|sorted fleet-pkg-* titles
#
# Read the columns for what each one isolates. `message` and the base package
# separate the OS-family layer (RedHat vs Debian, openssl vs gnupg). The tier
# and the extra package separate node-el from node-el-alt, which share an
# image, an OS family and every hiera layer but one -- tmux vs screen is the
# only evidence anywhere in this harness that the per-node layer contributes at
# all, so dropping that column would leave the harder axis unguarded.
EXPECTED="fleet_node_el|node-el|from RedHat/true/false|primary|fleet-pkg-base-curl,fleet-pkg-base-openssl,fleet-pkg-extra-tmux,fleet-pkg-extra-vim
fleet_node_el_alt|node-el-alt|from RedHat/true/false|secondary|fleet-pkg-base-curl,fleet-pkg-base-openssl,fleet-pkg-extra-screen,fleet-pkg-extra-vim
fleet_node_deb|node-deb|from Debian/true/false|primary|fleet-pkg-base-curl,fleet-pkg-base-gnupg,fleet-pkg-extra-vim"

mismatched=0
failed=0

while IFS='|' read -r container node exp_message exp_tier exp_packages; do
  raw="$OUT_DIR/axes-${node}.json"

  echo "== ${node} ==" || exit 2

  # Shared with gate check 6 rather than inlined, so the two cannot drift a flag
  # apart while both go on claiming they observe the same catalog check 1
  # measures. gate-common.sh documents why the compile is isolated the way it is.
  set +e
  fleet_catalog_json "$container" "$node" "$raw" "${raw%.json}.err"
  status=$?
  set -e

  # Classified, not bare. Left under `set -e` this would exit with podman's own
  # status -- 125 for a stopped container, 1 for a compile error -- and 1 is
  # this gate's code for "an axis collapsed", turning an outage into a finding.
  if [ "$status" -ne 0 ]; then
    echo "  FAILED to compile (exit ${status}; see ${raw%.json}.err)" || exit 2
    # Inlined as well as pointed at, following gate-determinism.sh: a CI reader
    # usually sees only the Venom transcript and cannot open the .err file. The
    # `-r` guard matters under `set -e` -- an unreadable .err would otherwise
    # abort the loop and silently skip every remaining node.
    if [ -r "${raw%.json}.err" ]; then
      sed 's/^/    /' "${raw%.json}.err" || exit 2
    fi
    failed=1
    continue
  fi

  set +e
  # gatekit reads the catalog the way voxdiff's own compile path does and prints
  # one line per mismatch; see internal/gatekit/fleet. Its exit status follows
  # this gate's contract directly, so the case below classifies it unchanged.
  "$FLEET_GATEKIT" fleet-axes "$raw" "$exp_message" "$exp_tier" "$exp_packages"
  status=$?
  set -e

  case "$status" in
    0) echo "  OK" || exit 2 ;;
    1) echo "  MISMATCH (an axis has collapsed)" || exit 2
       mismatched=1 ;;
    *) echo "  FAILED to read catalog (exit ${status})" || exit 2
       failed=1 ;;
  esac
done <<EOF
$EXPECTED
EOF

# Operational failure outranks a result, as in gate-determinism.sh. If any node
# could not be compiled or read, the check did not measure what it claims to,
# and "an axis collapsed" would be a stronger claim than the evidence supports.
if [ "$failed" -eq 1 ]; then
  exit 2
fi
if [ "$mismatched" -eq 1 ]; then
  exit 1
fi
exit 0
