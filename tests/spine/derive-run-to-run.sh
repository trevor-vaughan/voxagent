#!/usr/bin/env bash
# Re-derive the run_to_run allowlist by running Ruby Puppet against itself.
#
# The report differential can only report a field it compares, so every entry in
# run_to_run is a field the gate is blind to by construction. That makes the
# list's provenance part of the gate's evidence: if the differential later
# surfaces a timestamp-shaped field the list does not contain, you need to know
# whether the measurement was incomplete or the Go side is wrong, and an
# allowlist nobody can re-run cannot tell you.
#
# So this measures it. One catalog, applied twice from an identical starting
# state by the same implementation, with the two to_data_hash reports deep-diffed
# against each other. Anything that differs varies run to run and nothing else
# does.
#
# Two ways the list can be wrong, and this fails on both:
#
#   INCOMPLETE  a leaf differed that no pattern covers. The differential will
#               report it as a divergence on every run -- noise above any real
#               finding.
#   OVER-BROAD  a pattern also matched a leaf that did NOT differ. That leaf is
#               silently excluded from every future comparison. This is the
#               dangerous direction: the gate goes green while asserting less
#               than it appears to, which this repository treats as a defect
#               equal to a crash.
#
# The matching rule is the comparator's own -- `gatekit run-to-run` and
# `gatekit compare-reports` share internal/gatekit/reportcmp -- so the list is
# validated against the same semantics that will consume it. Two copies of an
# fnmatch rule would drift, and the drift would be invisible.
#
#   0  the allowlist covers exactly what varies
#   1  it is incomplete or over-broad -- a result about the allowlist
#   2  the derivation could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
PUPPET="${PUPPET:-puppet}"

command -v "$PUPPET" >/dev/null 2>&1 || {
  echo "derive-run-to-run: $PUPPET is not on PATH; there is nothing to measure" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh derive-run-to-run "$ROOT" "$GATEKIT"

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK"/{code/environments/production/manifests,conf,var,target,r1,r2} || exit 2

# The same fixture shape as report-diff.sh, and deliberately without exec:
# measured, the host's Ruby 3.4.9 cannot fork under Puppet 8.10.0 at all --
# util.rb walks /proc/self/fd closing descriptors and closes the handle it is
# iterating, so every exec fails with Errno::EBADF and produces no side effect.
cat > "$WORK/code/environments/production/manifests/site.pp" <<EOF || exit 2
node default {
  notify { 'spine': message => 'constant text' }
  file { '$WORK/target/a.txt': ensure => file, content => "alpha\n", mode => '0644' }
  file { '$WORK/target/d':     ensure => directory, mode => '0755' }
  file { '$WORK/target/gone':  ensure => absent }
}
EOF

C=(--confdir "$WORK/conf" --vardir "$WORK/var" --codedir "$WORK/code")

"$PUPPET" catalog find spine.example --terminus compiler --render-as json \
  --node_name_value spine.example \
  --environmentpath "$WORK/code/environments" --environment production \
  "${C[@]}" 2>"$WORK/compile.err" | sed -n '/^{/,$p' > "$WORK/catalog.json"
[ -s "$WORK/catalog.json" ] || {
  echo "derive-run-to-run: compiling the fixture catalog failed" >&2
  sed -n '1,10p' "$WORK/compile.err" >&2
  exit 2
}

# Both runs must start from the same state, or a difference between them is the
# starting point rather than the implementation.
for n in 1 2; do
  rm -rf "${WORK:?}/target" && mkdir -p "$WORK/target" || exit 2
  "$PUPPET" apply --catalog "$WORK/catalog.json" "${C[@]}" \
    --report --reports gatejson --libdir "$HERE/lib" --reportdir "$WORK/r$n" \
    >/dev/null 2>"$WORK/apply$n.err"
  # Captured as the gates capture it, by lib/puppet/reports/gatejson.rb.
  [ -s "$WORK/r$n/gate.json" ] || {
    echo "derive-run-to-run: run $n wrote no report" >&2
    sed -n '1,10p' "$WORK/apply$n.err" >&2
    exit 2
  }
done

"$GATEKIT" run-to-run "$WORK/r1/gate.json" "$WORK/r2/gate.json" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0) echo "derive-run-to-run: the allowlist covers exactly what varies" ;;
  1) echo "derive-run-to-run: the allowlist no longer matches what Ruby varies" >&2 ;;
  *) echo "derive-run-to-run: the derivation could not run" >&2 ;;
esac
exit "$rc"
