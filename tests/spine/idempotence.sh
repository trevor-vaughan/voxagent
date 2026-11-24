#!/usr/bin/env bash
# G2: apply a catalog twice; the second run must change nothing.
#
# This needs no Ruby at all, and it catches a class the report differential
# cannot see: a provider that reports a change it did not make, or makes one it
# did not report. Two implementations can agree on a report while both flap.
#
# The fixture contains NO notify resource. Measured against Puppet 8.10.0: a
# notify has no state on the node to compare against and reports a change on
# every run, so a catalog containing one is never idempotent. Including one here
# would make this gate assert something no correct implementation can satisfy.
#
# EXIT CODES OF THE RUNS UNDER TEST, which are not this script's own. voxagent
# with --detailed-exitcodes returns Puppet's bitmask: 0 nothing, 2 changes,
# 4 failures, 6 both -- and 1, uniquely, for could-not-run. So 2 is the EXPECTED
# result of the first run against a fresh directory and must not be mistaken for
# a failure to start. Only 1 means the run could not happen.
#
#   0  the second run changed nothing
#   1  it changed something -- a result about voxagent
#   2  the check could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
ARTIFACTS="${ARTIFACTS:-$ROOT/.test-output/spine}"

# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh idempotence "$ROOT" "$VOXAGENT"

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/target" || exit 2

# The exec is `creates`-guarded on purpose. An unguarded exec runs on every
# apply, so a catalog holding one is never idempotent -- the same reason notify
# is excluded. Guarded, it is the only resource here that can prove voxagent
# honours a guard at all, which is what makes the second run's silence mean
# something.
cat > "$WORK/catalog.json" <<EOF || exit 2
{"catalog_format":2,"name":"spine.example","environment":"production",
 "version":1,"catalog_uuid":"00000000-0000-4000-8000-000000000000",
 "tags":[],"classes":[],"edges":[],
 "resources":[
  {"type":"File","title":"$WORK/target/a.txt","tags":[],
   "parameters":{"ensure":"file","content":"alpha\n","mode":"0644"}},
  {"type":"File","title":"$WORK/target/d","tags":[],
   "parameters":{"ensure":"directory","mode":"0755"}},
  {"type":"Exec","title":"guarded","tags":[],
   "parameters":{"command":"/usr/bin/touch $WORK/target/made",
                 "creates":"$WORK/target/made"}}
 ]}
EOF

"$VOXAGENT" apply --catalog "$WORK/catalog.json" --detailed-exitcodes \
  >"$WORK/first.out" 2>&1
first=$?
if [ "$first" -eq 1 ]; then
  echo "idempotence: the first run could not complete (exit 1)" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi
if [ "$((first & 4))" -ne 0 ]; then
  echo "idempotence: the first run had resource failures (exit $first)" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi
# A first run that changed NOTHING means the fixture never reached the state it
# is supposed to converge from, so the second run's silence would prove nothing.
if [ "$((first & 2))" -eq 0 ]; then
  echo "idempotence: the first run reported no changes (exit $first), so the" >&2
  echo "idempotence: second run cannot demonstrate convergence" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi

"$VOXAGENT" apply --catalog "$WORK/catalog.json" --detailed-exitcodes \
  >"$WORK/second.out" 2>&1
second=$?
case "$second" in
  0)
    echo "idempotence: the second run changed nothing"
    exit 0
    ;;
  1)
    echo "idempotence: the second run could not run" >&2
    sed -n '1,20p' "$WORK/second.out" >&2
    exit 2
    ;;
  *)
    echo "idempotence: the second run reported changes or failures (exit $second)" >&2
    mkdir -p "$ARTIFACTS" \
      && cp "$WORK/first.out" "$ARTIFACTS/idempotence-first.out" \
      && cp "$WORK/second.out" "$ARTIFACTS/idempotence-second.out" \
      && cp "$WORK/catalog.json" "$ARTIFACTS/idempotence-catalog.json" \
      && echo "idempotence: evidence in $ARTIFACTS" >&2
    sed -n '1,20p' "$WORK/second.out" >&2
    exit 1
    ;;
esac
