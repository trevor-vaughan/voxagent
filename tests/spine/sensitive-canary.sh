#!/usr/bin/env bash
# The container half of the Sensitive canary sweep: cmd/voxagent's
# TestSensitiveCanarySweep, run inside the openvox image.
#
# On the dev host that test runs --noop everywhere and real applies only of the
# types whose effects stay under its temp dir, and it skips the augeas types,
# whose native route needs openvox's ruby-augeas and libaugeas. A Sensitive
# mark sends an augeas type's resource to the extension host
# (augeas.Prelude), so their canaries take the host route in both runs. In
# the image it runs twice:
#
#   native     VOXAGENT_CANARY_REAL=1: real applies of every type, user, group,
#              package and service included, in a container nobody keeps.
#   delegated  also VOXAGENT_CANARY_LIBDIR: the module units' type files, each
#              with one comment line appended (still valid Ruby, but no longer
#              their fingerprints), so every module type goes to the extension
#              host on openvox's Ruby; plus the host's fixture types.
#
# Service refuses every resource unless PID 1 is systemd, and a systemd
# container needs a delegated cgroup (docs/maintainers/apply-spine.md,
# "Running the systemd gate"), so here service sweeps its refusal text only.
#
# Every type the gate covers must run: any skipped subtest fails it. user and
# group are native here (the image is the measured platform), so their route
# delegated off it is not reached in either run.
#
#   0  no canary reached stdout, stderr or a report in either run
#   1  a canary leaked, or a type was skipped or its base refused -- a result
#      about voxagent
#   2  the sweep could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/sensitive-canary}"
IMAGE="${IMAGE:-localhost/fleet-node-el:latest}"
# The sweep runs on the augeas measurement image: the base plus sysctl and
# the ssh packages, without which sysctl is not native (augeas/image.sh).
AUGEAS_IMAGE="${AUGEAS_IMAGE:-localhost/augeas-measure-el:latest}"
CONCAT="$ROOT/fleet/.vendor/modules/concat"
# The augeas module units are not vendored: their measured files are the
# units' testdata, byte for byte what the routing fingerprints.
AUGEAS_LIBS="augeas_core augeasproviders_core augeasproviders_sysctl augeasproviders_ssh"

command -v podman >/dev/null 2>&1 || {
  echo "sensitive-canary: podman is not available" >&2
  exit 2
}
podman image exists "$IMAGE" || {
  echo "sensitive-canary: $IMAGE is not built; run 'task fleet:images'" >&2
  exit 2
}
[ -f "$CONCAT/lib/puppet/type/concat_file.rb" ] || {
  echo "sensitive-canary: the vendored concat is missing; run 'task fleet:vendor'" >&2
  exit 2
}
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh sensitive-canary "$ROOT" "$VOXAGENT"
# shellcheck source=build-image.sh
. "$HERE/build-image.sh" || exit 2
# shellcheck source=augeas/image.sh
. "$HERE/augeas/image.sh" || exit 2

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$EVIDENCE" || exit 2
augeas_image sensitive-canary "$IMAGE" "$AUGEAS_IMAGE" "$EVIDENCE/image-build.log" || exit 2

(cd "$ROOT" && go test -c -o "$WORK/canary.test" ./cmd/voxagent) >"$WORK/build.log" 2>&1 || {
  echo "sensitive-canary: could not build the test binary:" >&2
  tail -20 "$WORK/build.log" >&2
  exit 2
}

mkdir -p "$WORK/libdir" && cp -R "$CONCAT/lib/." "$WORK/libdir/" || exit 2
for u in $AUGEAS_LIBS; do
  cp -R "$ROOT/internal/units/$u/testdata/lib/." "$WORK/libdir/" || exit 2
done
for f in "$WORK"/libdir/puppet/type/*.rb; do
  echo '# voxagent sensitive-canary: fingerprint broken' >>"$f" || exit 2
done
cp -R "$ROOT/internal/extension/testdata/libdir/." "$WORK/libdir/" || exit 2

# sweep NAME ENV...: runs the test in a fresh container with ENV, its -test.v
# output in $EVIDENCE/NAME.log. The test locates its testdata through the
# source path compiled into it, so the tree is mounted at its own path.
# Returns 2 when the test did not run, else its exit status.
sweep() {
  local name="$1" log="$EVIDENCE/$1.log" rc
  shift
  podman run --rm --hostname spine.example \
    -v "$ROOT:$ROOT:ro" -v "$WORK:$WORK:ro" \
    "$AUGEAS_IMAGE" env VOXAGENT_BIN="$VOXAGENT" VOXAGENT_CANARY_REAL=1 "$@" \
    "$WORK/canary.test" -test.run '^TestSensitiveCanarySweep$' -test.v >"$log" 2>&1
  rc=$?
  if ! grep -q '^=== RUN   TestSensitiveCanarySweep$' "$log"; then
    echo "sensitive-canary: $name: the test did not run (podman exit $rc):" >&2
    tail -20 "$log" >&2
    return 2
  fi
  return "$rc"
}

# verdict NAME RC: prints NAME's leaks, skips and failures; returns 0 when
# there are none.
verdict() {
  local name="$1" rc="$2" log="$EVIDENCE/$1.log" leaks skips
  leaks="$(grep -F 'leaks a canary' "$log")"
  skips="$(grep -E -- '--- SKIP: ' "$log")"
  if [ -n "$leaks" ]; then
    echo "sensitive-canary: $name: canaries leaked:"
    printf '%s\n' "$leaks"
  fi
  if [ -n "$skips" ]; then
    echo "sensitive-canary: $name: types the gate covers were skipped:"
    printf '%s\n' "$skips"
    grep -E 'is not native on this host' "$log"
  fi
  if [ "$rc" -ne 0 ] && [ -z "$leaks" ]; then
    echo "sensitive-canary: $name: the sweep failed:"
    grep -E -- '--- FAIL: |canary_test.go:[0-9]+: ' "$log" | head -40
  fi
  echo "sensitive-canary: $name: log in $EVIDENCE/$name.log"
  [ "$rc" -eq 0 ] && [ -z "$skips" ]
}

status=0
for run in native delegated; do
  if [ "$run" = native ]; then
    sweep native
  else
    sweep delegated VOXAGENT_CANARY_LIBDIR="$WORK/libdir"
  fi
  rc=$?
  [ "$rc" -eq 2 ] && exit 2
  verdict "$run" "$rc" || status=1
done
echo "sensitive-canary: user and group are delegated only off the measured platform; not covered here."
[ "$status" -eq 0 ] && echo "sensitive-canary: no canary leaked"
exit "$status"
