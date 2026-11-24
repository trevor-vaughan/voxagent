#!/usr/bin/env bash
# G1 and G2 for Tier 3 resources: the types voxagent runs through its Ruby
# extension host rather than natively.
#
# This follows report-diff.sh step for step, and its header holds for this
# script too: both sides get identical catalog bytes, the report rather than
# `puppet apply --catalog`'s exit status is the oracle, and the allowlist is
# the same measured one. What differs is the fixture, which mixes native
# resources with stdlib's file_line and anchor, concat, and the extension
# host's own test types, and the libdir both sides load those types from.
#
# G2 is a second apply by each side on the target its own first apply
# converged, compared the same way; the comment above that comparison says why
# it is held to Ruby's second apply rather than to "changed nothing", and the
# resources that do converge are then asserted to have changed nothing.
#
# Last, the clientbucket each side's `backup => 'puppet'` wrote is compared
# byte for byte.
#
#   0  both pairs of reports agree outside the allowlist and in walk order,
#      the converging resources changed nothing, and the clientbuckets agree
#   1  a pair diverges in report or walk order, a resource did not converge,
#      or the clientbuckets differ or voxagent's is missing -- a result about
#      voxagent
#   2  the comparison could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
PUPPET="${PUPPET:-puppet}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine-extension}"
STDLIB="$ROOT/fleet/.vendor/modules/stdlib"
CONCAT="$ROOT/fleet/.vendor/modules/concat"

command -v "$PUPPET" >/dev/null 2>&1 || {
  echo "report-diff-extension: $PUPPET is not on PATH; the oracle is unavailable" >&2
  exit 2
}
# The extension host is a Ruby process voxagent starts by absolute path.
RUBY="$(command -v ruby)"
[ -n "$RUBY" ] || {
  echo "report-diff-extension: ruby is not on PATH; voxagent's extension host cannot start" >&2
  exit 2
}
if [ ! -f "$STDLIB/lib/puppet/type/file_line.rb" ] || [ ! -f "$CONCAT/lib/puppet/type/concat_file.rb" ]; then
  echo "report-diff-extension: the vendored stdlib or concat is missing; run 'task fleet:vendor'" >&2
  exit 2
fi
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff-extension "$ROOT" "$VOXAGENT" "$GATEKIT"

# The mixed-routing case's starting state, which this gate cannot seed: it is
# the host's, and the case is only meaningful if it runs no command.
if ! getent group nobody >/dev/null || [ "$(getent passwd nobody | cut -d: -f6)" != / ] \
  || [ -e /.ssh/authorized_keys ]; then
  echo "report-diff-extension: the host's accounts do not fit the mixed-routing case (a group nobody," >&2
  echo "report-diff-extension: a user nobody with home /, and no /.ssh/authorized_keys)" >&2
  exit 2
fi

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK"/{code/environments/production/manifests,code/environments/production/modules,conf,var,target,ruby,go,go2} || exit 2
ln -s "$STDLIB" "$WORK/code/environments/production/modules/stdlib" \
  && ln -s "$CONCAT" "$WORK/code/environments/production/modules/concat" || exit 2

# One pluginsync-style libdir serves both sides, as an agent's pluginsync would
# lay it out: the modules' libs, the extension host's test types, and the
# gatejson report processor that captures Ruby's report.
mkdir -p "$WORK/libdir" \
  && cp -R "$STDLIB/lib/." "$WORK/libdir/" \
  && cp -R "$CONCAT/lib/." "$WORK/libdir/" \
  && cp -R "$ROOT/internal/extension/testdata/libdir/." "$WORK/libdir/" \
  && cp -R "$HERE/lib/." "$WORK/libdir/" || exit 2

T="$WORK/target"

# Every G1 case the extension host can deliver, generation and concat
# included. The fixture, like report-diff.sh's, avoids exec and anything else that forks:
# the host's Ruby cannot fork under Puppet's own execute.
#
# vox_value 'broken' fails on every run (its provider refuses to write 'fail'),
# and so do vox_prefetch 'no1' (the bad provider's prefetch raises) and both
# vox_conf resources (no suitable provider). All block their dependents on
# every run too. So does vox_evalgen 'gb', whose eval_generate raises.
#
# concat keeps its default backup => 'puppet' and no validate_cmd: validate_cmd
# forks, and its G1 is the G5 gate's.
#
# LINE NUMBERS BELOW ARE COMPARED, as in report-diff.sh: both sides read them
# from the same compiled catalog, so moving a line moves it on both sides.
cat > "$WORK/code/environments/production/manifests/site.pp" <<EOF || exit 2
node default {
  file { '$T': ensure => directory, mode => '0755' }
  file { '$T/lines': ensure => file, mode => '0644', require => File['$T'] }
  # file_line on a seeded file (stdlib, Tier 3).
  file_line { 'add': path => '$T/lines', line => 'added', require => File['$T/lines'] }
  # anchor between native resources.
  anchor { 'mid': require => File['$T/lines'] }
  file { '$T/after-anchor': ensure => file, content => "a\n", require => Anchor['mid'] }
  # notify/subscribe between a native File and a Tier 3 resource, both ways.
  file { '$T/conf': ensure => file, content => "c\n", notify => Vox_value['refreshed'] }
  vox_value { 'refreshed': path => '$T/rv', value => 'r' }
  vox_value { 'notifier': path => '$T/nv', value => 'n', notify => File['$T/notified'] }
  file { '$T/notified': ensure => file, content => "x\n" }
  # A Tier 3 refresh callback that logs and then raises: the callback's line,
  # "Failed to call refresh" and log_exception's line, in Puppet's order.
  file { '$T/refuser-conf': ensure => file, content => "r\n", notify => Vox_value['refuser'] }
  vox_value { 'refuser': path => '$T/refv', value => 'refusing-refresh' }
  # A Tier 3 failure that blocks a native dependent.
  vox_value { 'broken': path => '$T/bv', value => 'fail' }
  file { '$T/blocked': ensure => file, content => "b\n", require => Vox_value['broken'] }
  # No suitable provider (vox_conf's only provider is confined away): Puppet
  # defers both and marks them failed unevaluated; the chosen one is named at
  # once, the other in teardown's "Could not find a suitable provider".
  vox_conf { 'none': value => 'v' }
  vox_conf { 'chosen': value => 'v', provider => never }
  file { '$T/after-unsuitable': ensure => file, content => "u\n", require => Vox_conf['none'] }
  # Per-provider-class prefetch failure, and post_resource_eval.
  vox_prefetch { 'ok': ensure => present, provider => good }
  vox_prefetch { 'no1': ensure => present, provider => bad }
  vox_prefetch { 'no2': ensure => present, provider => bad, require => Vox_value['broken'] }
  # concat: concat_file generates its File, and its eval_generate fills that
  # File's content from two fragments declared out of order. c.conf is seeded
  # with other content, so the replacement is bucketed. One fragment is
  # Sensitive, so the generated File's content event is redacted.
  concat { '$T/c.conf': ensure => present, mode => '0640', notify => Notify['after-concat'] }
  concat::fragment { 'c-second': target => '$T/c.conf', content => Sensitive("second\n"), order => '20' }
  concat::fragment { 'c-first': target => '$T/c.conf', content => "first\n", order => '10' }
  notify { 'after-concat': }
  # No fragments and create_empty_file => false: eval_generate turns the
  # generated File's ensure to absent, and the seeded file is removed.
  concat { '$T/empty.conf': create_empty_file => false }
  # eval_generate making two resources, and one that raises.
  vox_evalgen { 'g': make => '2' }
  vox_evalgen { 'gb': make => 'raise' }
  # generate making a resource.
  vox_gen { 'loud': make => true }
  # A reference through an alias: vt's namevar is vn.
  vox_value { 'vt': name => 'vn', path => '$T/vn', value => 'v' }
  notify { 'needs-alias': require => Vox_value['vn'] }
  # Generate runs before the graph is built and puts
  # what it makes beside its parent in the catalog, before it for tidy, which
  # is depth-first. The recursing file's children come from eval_generate.
  notify { 'first': }
  tidy { '$T/td': recurse => true, matches => '*.old' }
  notify { 'after_tidy': }
  file { '$T/d': recurse => true }
  notify { 'last': }
  # Mixed routing: Group stays native while purge_ssh_keys sends User to the
  # host, and the edges between them cross the boundary both ways. Nothing
  # here may run a command (the host's Ruby cannot fork, and this gate is not
  # root), so the existing nobody accounts are held in sync, and nobody's
  # keyfile, /.ssh/authorized_keys, does not exist, so no key is purged. No
  # Group property beyond ensure is compared: this oracle is the host's
  # Puppet, not openvox 8.33.0, and its groupadd provider lacks the
  # manages_members feature the native Group implements (spec section 6).
  group { 'nobody': ensure => present, notify => Vox_value['after-group'] }
  vox_value { 'after-group': path => '$T/ag', value => 'g' }
  user { 'nobody': ensure => present, purge_ssh_keys => true, require => Group['nobody'] }
}
EOF

C=(--confdir "$WORK/conf" --vardir "$WORK/var" --codedir "$WORK/code")

# The compiler needs the libdir as well: vox_value and vox_prefetch live in no
# module, so it is the only place it can find them.
"$PUPPET" catalog find spine.example --terminus compiler --render-as json \
  --node_name_value spine.example \
  --environmentpath "$WORK/code/environments" --environment production \
  --libdir "$WORK/libdir" "${C[@]}" 2>"$WORK/compile.err" | sed -n '/^{/,$p' > "$WORK/catalog.json"
[ -s "$WORK/catalog.json" ] || {
  echo "report-diff-extension: compiling the fixture catalog failed" >&2
  sed -n '1,10p' "$WORK/compile.err" >&2
  exit 2
}

# Each side starts from the same state. lines is seeded so file_line has an
# existing file to edit, c.conf so concat replaces (and buckets) other
# content, empty.conf so concat's ensure => absent removes something, td so
# tidy has files to remove and one to keep, and d so the recursing file has
# children; every other path is created by the run.
reset_target() {
  rm -rf "${WORK:?}/target" && mkdir -m 0755 "$T" || return 1
  printf 'seed\n' > "$T/lines" && chmod 0644 "$T/lines" \
    && printf 'old concat\n' > "$T/c.conf" && chmod 0640 "$T/c.conf" \
    && printf 'stale\n' > "$T/empty.conf" && chmod 0644 "$T/empty.conf" \
    && mkdir -p "$T/td" "$T/d/sub" && touch "$T"/td/{a,b,c}.old "$T/td/keep" "$T/d/z" "$T/d/sub/y"
}

# ruby_apply DIR: one Ruby apply, its report captured in DIR/gate.json.
ruby_apply() {
  "$PUPPET" apply --catalog "$WORK/catalog.json" "${C[@]}" \
    --report --reports gatejson --libdir "$WORK/libdir" --reportdir "$WORK/$1" >"$WORK/$1.out" 2>&1
  [ -s "$WORK/$1/gate.json" ] || {
    echo "report-diff-extension: Ruby wrote no report; there is nothing to compare against" >&2
    tail -n 20 "$WORK/$1.out" >&2
    exit 2
  }
}

# voxagent_apply DIR: one voxagent apply, its report written under DIR.
# --detailed-exitcodes only so that could-not-run is exit 1 on its own: the
# verdict is the report, as for Ruby.
voxagent_apply() {
  "$VOXAGENT" apply --catalog "$WORK/catalog.json" --report-dir "$WORK/$1" \
    --ruby "$RUBY" --libdir "$WORK/libdir" --vardir "$WORK/vox-var" \
    --detailed-exitcodes >"$WORK/$1.out" 2>&1
  local rc=$?
  if [ "$rc" -eq 1 ] || [ -z "$(find "$WORK/$1" -name '*.json' -print -quit)" ]; then
    echo "report-diff-extension: voxagent could not run" >&2
    tail -n 20 "$WORK/$1.out" >&2
    exit 2
  fi
}

# keep_evidence LABEL RUBYDIR GODIR GOREPORT: the work tree is about to go,
# and reading a divergence needs the two reports, both runs' output and the
# catalog side by side.
keep_evidence() {
  local label="$1" ruby="$WORK/$2/gate.json" go="$4"
  mkdir -p "$EVIDENCE" && cp "$ruby" "$EVIDENCE/ruby.json" \
    && cp "$go" "$EVIDENCE/voxagent.json" \
    && cp "$WORK/catalog.json" "$EVIDENCE/catalog.json" \
    && cp "$WORK/$2.out" "$EVIDENCE/ruby.out" \
    && cp "$WORK/$3.out" "$EVIDENCE/voxagent.out" \
    && echo "report-diff-extension: the $label reports, their output and the catalog are in $EVIDENCE" >&2
}

# compare LABEL RUBYDIR GODIR: the report differential, then the walk order,
# keeping the evidence for a divergence in either. compare-reports matches
# statuses by ref and logs as a multiset, so it cannot see the order resources
# were evaluated in; compare-order reads that from each report's log order.
compare() {
  local label="$1" ruby="$WORK/$2/gate.json" go
  go="$(find "$WORK/$3" -name '*.json' -print -quit)"
  "$GATEKIT" compare-reports "$ruby" "$go" "$HERE/allowlist.json"
  case $? in
    0) echo "report-diff-extension: $label: reports agree outside the allowlist" ;;
    1)
      keep_evidence "$@" "$go"
      echo "report-diff-extension: $label: reports diverge" >&2
      exit 1
      ;;
    *)
      echo "report-diff-extension: $label: the comparison could not run" >&2
      exit 2
      ;;
  esac
  "$GATEKIT" compare-order "$ruby" "$go"
  case $? in
    0) ;;
    1)
      keep_evidence "$@" "$go"
      echo "report-diff-extension: $label: the walk order diverges" >&2
      exit 1
      ;;
    *)
      echo "report-diff-extension: $label: the walk order could not be compared" >&2
      exit 2
      ;;
  esac
}

# Each side applies twice: once from the seeded state, and once more on the
# target its own first run converged.
# clientbucket FILE VARDIR: every entry of the clientbucket under VARDIR, with
# its type, mode and content, so the two sides' can be compared byte for
# byte. The paths files name $T on both sides, as both agents reconcile it.
clientbucket() {
  { (cd "$2/clientbucket" && find . -printf '%P %y %m\n' | sort) \
    && (cd "$2/clientbucket" && find . -type f -print | sort | while IFS= read -r f; do printf '%s:\n' "$f" && cat "$f" || exit 1; done)
  } > "$1"
}

reset_target || exit 2
ruby_apply ruby && ruby_apply ruby2
clientbucket "$WORK/ruby-bucket" "$WORK/var" || {
  echo "report-diff-extension: Ruby's clientbucket could not be read" >&2
  exit 2
}
# A run that bucketed nothing would agree with a voxagent that bucketed
# nothing, so Ruby's entry for c.conf's seeded content must be there.
c_sum="$(printf 'old concat\n' | sha256sum | cut -d' ' -f1)"
c_entry="$WORK/var/clientbucket/$(printf '%s' "${c_sum:0:8}" | sed 's|.|&/|g')$c_sum"
if ! grep -qxF "$T/c.conf" "$c_entry/paths" 2>/dev/null \
  || [ "$(cat "$c_entry/contents" 2>/dev/null)" != "old concat" ]; then
  echo "report-diff-extension: Ruby's clientbucket holds no backup of c.conf ($c_entry); nothing to compare against" >&2
  exit 2
fi
reset_target || exit 2
voxagent_apply go && voxagent_apply go2

compare "first apply" ruby go
# G2. A converging Tier 3 resource must change nothing on the second apply, but
# "changed nothing" is not the claim this fixture can make: Vox_prefetch[ok]
# changes on every run on either side, because its good provider's prefetch
# always reports it absent and create records present only in memory
# (internal/extension/testdata/libdir/puppet/provider/vox_prefetch/good.rb).
# So the second apply is held to Ruby's second apply instead: every resource
# that converges under Ruby must converge under voxagent, and the rest must
# behave as Ruby's do -- broken and no1 fail again, no2 and blocked are skipped
# again.
compare "second apply" ruby2 go2

# Agreeing with Ruby's second apply would also pass if both sides failed to
# converge, so the resources that do converge are asserted to change nothing in
# voxagent's second report. Both sides' reports drop a resource nothing
# happened to, as PuppetDB does, so "changed nothing" is absence, or presence
# with no change, failure, skip or event. Each ref must have been in the first
# report, so a stale or misspelt ref cannot pass by being absent from both --
# except the refs after "--", which never change: an anchor, and the target
# directory and seeded file reset_target creates. Those are held to no change
# in both reports.
# The fixture already requires Ruby on PATH; its JSON library is the
# extraction, so the gate needs nothing new.
converged() {
  local first second
  first="$(find "$WORK/go" -name '*.json' -print -quit)"
  second="$(find "$WORK/go2" -name '*.json' -print -quit)"
  "$RUBY" -rjson -e '
    first, second = ARGV.shift(2).map { |f| JSON.parse(File.read(f)).fetch("resource_statuses") }
    split = ARGV.index("--") || ARGV.size
    changing, steady = ARGV[0...split], ARGV[split + 1..] || []
    quiet = ->(s) { s.nil? || (!s["failed"] && !s["skipped"] && !s["changed"] && !s["out_of_sync"] && s["events"].empty?) }
    bad = changing.reject { |ref| first.key?(ref) && quiet.(second[ref]) } +
          steady.reject { |ref| quiet.(first[ref]) && quiet.(second[ref]) }
    brief = ->(s) { s.nil? ? "absent" : s.slice("changed", "failed", "skipped", "out_of_sync").merge("events" => s["events"].size).inspect }
    bad.each do |ref|
      warn "report-diff-extension: second apply: #{ref} did not converge " \
           "(first: #{brief.(first[ref])}, second: #{brief.(second[ref])})"
    end
    exit(bad.empty? ? 0 : 1)
  ' "$first" "$second" "$@"
}
# concat's generated Files converge like any File; the concat_file,
# concat_fragment, vox_evalgen and vox_gen resources have no property that
# can change. The Notifys are left out: a notify changes on every run, under
# Ruby as under voxagent, whether declared or generated.
if converged "File_line[add]" "File[$T/after-anchor]" "File[$T/conf]" \
  "Vox_value[refreshed]" "Vox_value[notifier]" "File[$T/notified]" "File[$T/refuser-conf]" \
  "Vox_value[refuser]" "File[$T/c.conf]" "File[$T/empty.conf]" "Vox_value[vt]" "Vox_value[after-group]" \
  -- "Anchor[mid]" "File[$T]" "File[$T/lines]" "Concat_file[$T/c.conf]" "Concat_file[$T/empty.conf]" \
  "Concat_fragment[c-first]" "Concat_fragment[c-second]" "Vox_evalgen[g]" "Vox_gen[loud]" \
  "Group[nobody]" "User[nobody]"; then
  echo "report-diff-extension: second apply: every converging resource changed nothing"
else
  echo "report-diff-extension: second apply: a converging resource changed again" >&2
  exit 1
fi
# The clientbuckets are state, not report: both agents must have bucketed the
# same content, under the same layout and modes. They are compared after the
# reports, so a report divergence is never hidden behind a bucket one. Ruby's
# holds c.conf's backup (checked above), so a voxagent that made no clientbucket
# bucketed nothing it should have: a divergence, not a gate that could not run.
if [ ! -d "$WORK/vox-var/clientbucket" ]; then
  echo "report-diff-extension: voxagent made no clientbucket under its vardir; Ruby's holds c.conf's backup" >&2
  exit 1
fi
clientbucket "$WORK/go-bucket" "$WORK/vox-var" || {
  echo "report-diff-extension: voxagent's clientbucket could not be read" >&2
  exit 2
}
diff -u "$WORK/ruby-bucket" "$WORK/go-bucket" >&2
case $? in
  0) echo "report-diff-extension: clientbuckets agree" ;;
  1)
    echo "report-diff-extension: the clientbuckets differ (- Ruby, + voxagent)" >&2
    exit 1
    ;;
  *)
    echo "report-diff-extension: the clientbuckets could not be compared" >&2
    exit 2
    ;;
esac
