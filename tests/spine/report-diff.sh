#!/usr/bin/env bash
# G1: compare the reports Ruby Puppet and voxagent produce from identical
# catalog bytes.
#
# Feeding both sides the same bytes removes compile variance, hiera, facts and
# catalog_uuid from the comparison, leaving a clean function: bytes in, report
# out. Whatever differs after that is one of the two implementations.
#
# The exit status of `puppet apply --catalog` is NOT consulted, and that is not
# an oversight. Measured against Puppet 8.10.0: that path ignores
# --detailed-exitcodes entirely, because Puppet::Application::Apply#run_command
# dispatches --catalog to `apply`, which calls apply_catalog and discards its
# return value; the exit call exists only in the manifest path. The stored
# report is correct where the process exit is wrong, so the report is the
# oracle.
#
# The allowlist this compares against is itself measured, by
# tests/spine/derive-run-to-run.sh. Run that first if a timestamp-shaped field
# ever appears in the divergence list: it answers whether the measurement was
# incomplete or voxagent is wrong, which is a question you cannot resolve by
# staring at the divergence.
#
#   0  the reports agree outside the allowlist
#   1  they diverge -- a result about voxagent
#   2  the comparison could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
PUPPET="${PUPPET:-puppet}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine}"

command -v "$PUPPET" >/dev/null 2>&1 || {
  echo "report-diff: $PUPPET is not on PATH; the oracle is unavailable" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff "$ROOT" "$VOXAGENT" "$GATEKIT"

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK"/{code/environments/production/manifests,conf,var,target,ruby,go} || exit 2

# The fixture deliberately excludes exec: measured, the host's Ruby 3.4.9 cannot
# fork under Puppet 8.10.0 at all -- util.rb walks /proc/self/fd closing
# descriptors and closes the handle it is iterating, so every exec fails with
# Errno::EBADF and produces no side effect. exec's differential is the container
# gate's job.
#
# bad.txt fails on purpose: its parent directory does not exist, so the create
# raises an Errno on both sides, which is what makes both of them WRAP it -- the
# only shape that exercises the failure attribution. `ensure => directory` would
# not wrap and is a different test; it is not a substitute.
#
# LINE NUMBERS BELOW ARE COMPARED. A wrapped failure's message ends
# "(file: .../site.pp, line: N)", where N is the resource's line inside this
# heredoc. Both implementations read N from the same compiled catalog, so they
# agree automatically and inserting a line above bad.txt simply moves N on both
# sides -- expected, not a regression.
cat > "$WORK/code/environments/production/manifests/site.pp" <<EOF || exit 2
node default {
  notify { 'spine': message => 'constant text' }
  file { '$WORK/target/a.txt': ensure => file, content => "alpha\n", mode => '0644' }
  file { '$WORK/target/d':     ensure => directory, mode => '0755' }
  file { '$WORK/target/gone':  ensure => absent }
  file { '$WORK/target/missing/bad.txt': ensure => file, content => "x\n" }
}
EOF

C=(--confdir "$WORK/conf" --vardir "$WORK/var" --codedir "$WORK/code")

"$PUPPET" catalog find spine.example --terminus compiler --render-as json \
  --node_name_value spine.example \
  --environmentpath "$WORK/code/environments" --environment production \
  "${C[@]}" 2>"$WORK/compile.err" | sed -n '/^{/,$p' > "$WORK/catalog.json"
[ -s "$WORK/catalog.json" ] || {
  echo "report-diff: compiling the fixture catalog failed" >&2
  sed -n '1,10p' "$WORK/compile.err" >&2
  exit 2
}

# Each side starts from the same state, so any difference is the implementation
# and not the starting point.
reset_target() { rm -rf "${WORK:?}/target" && mkdir -p "$WORK/target"; }

reset_target || exit 2
"$PUPPET" apply --catalog "$WORK/catalog.json" "${C[@]}" \
  --report --reports store --reportdir "$WORK/ruby" >/dev/null 2>&1
RUBY_REPORT="$(find "$WORK/ruby" -name '*.yaml' | head -1)"
[ -n "$RUBY_REPORT" ] || {
  echo "report-diff: Ruby wrote no report; there is nothing to compare against" >&2
  exit 2
}

reset_target || exit 2
"$VOXAGENT" apply --catalog "$WORK/catalog.json" --report-dir "$WORK/go" >/dev/null 2>&1
GO_REPORT="$(find "$WORK/go" -name '*.json' | head -1)"
[ -n "$GO_REPORT" ] || {
  echo "report-diff: voxagent wrote no report" >&2
  exit 2
}

# Ruby's report is a YAML object graph; convert it to the same data hash a
# report processor or PuppetDB would receive, which is the surface that has to
# match.
ruby -ryaml -rpuppet -rjson -e '
  puts YAML.unsafe_load_file(ARGV[0]).to_data_hash.to_json
' "$RUBY_REPORT" > "$WORK/ruby.json" 2>"$WORK/convert.err" || {
  echo "report-diff: converting Ruby's report failed" >&2
  sed -n '1,10p' "$WORK/convert.err" >&2
  exit 2
}

"$GATEKIT" compare-reports \
  "$WORK/ruby.json" "$GO_REPORT" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0) echo "report-diff: reports agree outside the allowlist" ;;
  1)
    # Keep the evidence. The divergence list names the fields, but reading why
    # needs the two reports side by side, and the work tree is about to go.
    mkdir -p "$EVIDENCE" && cp "$WORK/ruby.json" "$EVIDENCE/ruby.json" \
      && cp "$GO_REPORT" "$EVIDENCE/voxagent.json" \
      && cp "$WORK/catalog.json" "$EVIDENCE/catalog.json" \
      && echo "report-diff: the two reports and the catalog are in $EVIDENCE" >&2
    echo "report-diff: reports diverge" >&2
    ;;
  *) echo "report-diff: the comparison could not run" >&2 ;;
esac
exit "$rc"
