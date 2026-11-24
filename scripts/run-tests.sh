#!/usr/bin/env bash
# Run the Go test suite in one of two output modes.
#   human — full test log, for a person reading a terminal
#   llm   — failures only, for an agent with a limited context window
# The process exit code always reflects the real test result in both modes.
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

MODE="${1:-human}"
TEST_DIR="${2:-.test-output}"

mkdir -p "$TEST_DIR"
LOG="$TEST_DIR/go-test.log"

# The extension host's tests skip without a Ruby that can load Puppet. The
# suite must not pass by skipping them, so the runner makes their absence a
# failure (internal/extension/extensiontest).
export VOXAGENT_REQUIRE_RUBY=1

set +e
go test -count=1 ./... >"$LOG" 2>&1
rc=$?
set -e

if [ "$MODE" = "llm" ]; then
  if [ "$rc" -ne 0 ]; then
    # Extract just the failure lines. If nothing matches -- a build error or a
    # tooling failure whose output looks nothing like a test failure -- fall
    # back to the whole log rather than reporting a failure silently.
    if ! grep -E '^(--- FAIL|FAIL|panic:)|_test\.go:[0-9]+:' "$LOG"; then
      cat "$LOG"
    fi
  else
    echo "ok: all Go tests passed"
  fi
else
  cat "$LOG"
fi

exit "$rc"
