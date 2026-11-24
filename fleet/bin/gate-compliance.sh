#!/usr/bin/env bash
# Gate check 6: assert compliance_markup enforcement actually reaches a catalog.
#
# Check 1 already proves the allowlist survives the backend being CONSULTED --
# compliance_markup is the first hierarchy entry, and enforcement.rb:51 re-reads
# and instance_evals the mapper's Ruby on every single lookup. That result is
# real but narrow. compliance_mapper.rb:47-55 looks up
# compliance_markup::enforcement with a default of [] and gates everything below
# on `unless profile_list == []`, so with no profile enforced the profile
# compiler never runs and no compliance YAML is ever read off disk.
#
# So this check asserts the OUTPUT of that data-loading path, not that it ran.
# fleet_profile::compliance_marker is set by nothing but the enforcement
# backend, and its default -- 'unenforced' -- names its own absence. Asserting
# the enforced value is what makes the difference visible: a check that only
# required the catalog to compile would stay green in exactly the state this
# check exists to detect.
#
# `unenforced` is the diagnostic outcome. It means the backend was consulted and
# the catalog compiled, but the profile compiler found no check for this
# parameter -- a modulepath that stopped covering fleet_profile, a renamed
# SIMP/compliance_profiles directory, or a glob in compliance_mapper.rb:166-208
# that stopped matching. None of those fail at compile time.
#
# One node, not three, deliberately: compliance_markup::enforcement lives in the
# common hiera layer, so it is environment-wide rather than per-node and
# compiling all three would triple the runtime to re-measure one fact. Gate
# check 5 already proves per-node differentiation works.
#
# Exit codes follow the fleet convention: 0 the enforced value is present, 1 it
# is not (a result about the fleet), 2 the check could not run.
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

# Usage errors exit 2, not 1: under this script's contract 1 means enforcement
# did not reach the catalog, so a caller reading only the exit code would take a
# missing argument as a finding about the fleet.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-compliance.sh <out-dir>" >&2 || exit 2
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

CONTAINER=fleet_node_el
# Quoted where its neighbours are not, and not decoration: `node-el` reads as
# `node - el` to shellcheck, which raises SC2100 and fails dev:lint as soon as a
# variable named `node` is in scope. One is, since `-x` follows the source above
# into fleet_catalog_json's locals. Quoting says string, not arithmetic.
NODE='node-el'
EXPECTED=enforced-by-disa_stig

RAW="$OUT_DIR/compliance-${NODE}.json"
ERR="${RAW%.json}.err"

# Drop the previous run's artefacts so a compile that fails to write cannot
# leave a stale catalog behind that still reads as current.
fleet_clear_artefacts "$RAW" "$ERR"

echo "== ${NODE} ==" || exit 2

# Shared with gate check 5 rather than inlined, so the two cannot drift a flag
# apart while both go on claiming they observe the same catalog check 1
# measures. gate-common.sh documents why the compile is isolated the way it is.
set +e
fleet_catalog_json "$CONTAINER" "$NODE" "$RAW" "$ERR"
status=$?
set -e

# Classified, not bare. Left under `set -e` this would exit with podman's own
# status -- 125 for a stopped container, 1 for a compile error -- and 1 is this
# gate's code for "enforcement did not reach the catalog", turning an outage
# into a finding.
if [ "$status" -ne 0 ]; then
  echo "  FAILED to compile (exit ${status}; see ${ERR})" || exit 2
  # Inlined as well as pointed at, following gate-axes.sh: a CI reader usually
  # sees only the Venom transcript and cannot open the .err file. The `-r` guard
  # matters under `set -e` -- an unreadable .err would otherwise abort the
  # script before it could classify the failure as "could not run".
  if [ -r "$ERR" ]; then
    sed 's/^/    /' "$ERR" || exit 2
  fi
  exit 2
fi

set +e
# gatekit reads the catalog the way voxdiff's own compile path does and prints
# one line per mismatch; see internal/gatekit/fleet. Its exit status follows
# this gate's contract directly, so the case below classifies it unchanged.
"$FLEET_GATEKIT" fleet-compliance "$RAW" "$EXPECTED"
status=$?
set -e

case "$status" in
  0) echo "  OK" || exit 2
     exit 0 ;;
  1) echo "  NOT ENFORCED (compliance data never reached the catalog)" || exit 2
     exit 1 ;;
  *) echo "  FAILED to read catalog (exit ${status})" || exit 2
     exit 2 ;;
esac
