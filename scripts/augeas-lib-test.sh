#!/usr/bin/env bash
# Run internal/ral's `augeaslib`-tagged tests against openvox's real libaugeas
# 1.15.0 (spec §10, `task dev:augeas-lib`).
#
# The host has no openvox and the image has no Go, so the test binary is built
# here with `go test -c` and copied into a fresh container of the measurement
# image, where it runs as root: the save test chowns a file to uid 65534. The
# container is rootless podman's, so "root" is the caller's uid.
#
# Usage: augeas-lib-test.sh MODE TEST_DIR
#   MODE human -- every test named (-test.v); llm -- failures and the verdict.
# The image is built first, or rebuilt when it no longer carries the base's
# openvox and libaugeas (tests/spine/augeas/image.sh; a cold build needs the
# network). Exits 2, not 1, when it cannot run (no podman, no base image, a
# failed build), so a missing library is never mistaken for a result.
set -euo pipefail
set +C

MODE="${1:-human}"
TEST_DIR="${2:-.test-output}"
IMAGE="${AUGEAS_IMAGE:-localhost/augeas-measure-el}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$TEST_DIR/augeas-lib"
BIN="$OUT/ral.test"
LOG="$OUT/test.log"

command -v podman >/dev/null || { echo "augeas-lib: podman not found" >&2; exit 2; }

mkdir -p "$OUT"
# shellcheck source=../tests/spine/build-image.sh
. "$ROOT_DIR/tests/spine/build-image.sh"
# shellcheck source=../tests/spine/augeas/image.sh
. "$ROOT_DIR/tests/spine/augeas/image.sh"
augeas_image augeas-lib "${AUGEAS_BASE_IMAGE:-localhost/fleet-node-el:latest}" "$IMAGE" \
  "$OUT/image-build.log" || exit 2
(cd "$ROOT_DIR" && CGO_ENABLED=1 go test -c -tags augeaslib -o "$BIN" ./internal/ral)

# Only the tests that need the library: the rest of the package expects its
# testdata/ beside the binary and runs on the host anyway.
RUN='^Test(ProbeRealLibrary|Real)'
# Named with this script's PID so the trap cannot remove another run's container.
NAME="voxagent-augeas-lib-$$"
VERBOSE=(-test.v)
[ "$MODE" = llm ] && VERBOSE=()
trap 'podman rm -f "$NAME" >/dev/null 2>&1 || :' EXIT
podman create --name "$NAME" --user 0 --env GODEBUG=cgocheck=1 "$IMAGE" \
  /usr/local/bin/ral.test -test.count=1 -test.run "$RUN" "${VERBOSE[@]}" >/dev/null
podman cp "$BIN" "$NAME:/usr/local/bin/ral.test"

set +e
podman start --attach "$NAME" >"$LOG" 2>&1
rc=$?
set -e

if [ "$MODE" = llm ]; then
  if [ "$rc" -ne 0 ]; then
    grep -E -- '^(--- FAIL|FAIL|panic:|fatal error:)|_test\.go:[0-9]+:' "$LOG" || tail -n 40 "$LOG"
  fi
else
  cat "$LOG"
fi
echo "augeas-lib: log at $LOG (exit $rc)"
exit "$rc"
