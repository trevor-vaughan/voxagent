#!/usr/bin/env bash
# Poll OpenVoxDB until it serves its status endpoint.
#
# This exists because podman drives container healthchecks from systemd
# transient timers, and PID 1 in this environment is bash. The checks never
# run, so `depends_on: condition: service_healthy` would wait forever. Polling
# the endpoint is the only reliable readiness signal here.
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

URL="${1:-http://localhost:18080/status/v1/services}"
TIMEOUT="${2:-180}"
CONTAINER="${3:-fleet_openvoxdb}"

deadline=$(( SECONDS + TIMEOUT ))
while [ "$SECONDS" -lt "$deadline" ]; do
  if curl -sf -m 5 "$URL" >/dev/null 2>&1; then
    echo "openvoxdb: ready after ${SECONDS}s"
    exit 0
  fi
  sleep 3
done

echo "openvoxdb: NOT ready after ${TIMEOUT}s" >&2
echo "--- container logs ---" >&2
podman logs --tail 40 "$CONTAINER" >&2 2>&1 || true
exit 1
