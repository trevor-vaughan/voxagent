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
#   0  the reports agree outside the allowlist, the walk order agrees and the
#      backups agree
#   1  the reports diverge, the walk order diverges, or the backups differ or
#      voxagent left no clientbucket or no bk-suffix.bak -- a result about
#      voxagent
#   2  the comparison could not run (this outranks a divergence)
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

# Source trees live outside target, which reset_target recreates between the
# two agents, and exist before the catalog is compiled, as a real source
# exists before its catalog is applied. Neither agent writes under src.
mkdir -p "$WORK/src/tree/sub" "$WORK/src/pur" || exit 2
printf 'one\n' > "$WORK/src/one.txt" && printf 'a\n' > "$WORK/src/tree/a.txt" \
  && printf 'b\n' > "$WORK/src/tree/sub/b.txt" && printf 'new\n' > "$WORK/src/pur/new.txt" \
  && printf 'z\n' > "$WORK/src/zreal" && ln -s zreal "$WORK/src/zlnk" || exit 2

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
#
# The wo* resources are measured walk-order cases, for `gatekit compare-order`. Their titles are
# out of byte order on purpose, and each one logs, so each is placed in the
# walk: a resource that logs nothing has no position to compare.
cat > "$WORK/code/environments/production/manifests/site.pp" <<EOF || exit 2
define spine::wrap(String \$m) {
  notify { "wrapped-\${title}": message => \$m }
}
define wo3b::def() {
  notify { "wo3b_\${title}_x": }
  notify { "wo3b_\${title}_y": }
}
class wo3f_outer {
  include wo3f_inner1
  contain wo3f_inner2
  notify { 'wo3f_outer_n': }
}
class wo3f_inner1 {
  notify { 'wo3f_i1': require => Notify['wo3f_late'] }
}
class wo3f_inner2 {
  notify { 'wo3f_i2': }
}
class wo3g_x {
  notify { 'wo3g_xm': }
}
node default {
  notify { 'spine': message => 'constant text' }
  spine::wrap { 'one': m => 'inside a define' }
  notify { 'after-wrap': message => 'after the define', require => Spine::Wrap['one'] }
  file { '$WORK/target/a.txt': ensure => file, content => "alpha\n", mode => '0644' }
  file { '$WORK/target/d':     ensure => directory, mode => '0755' }
  file { '$WORK/target/gone':  ensure => absent }
  file { '$WORK/target/missing/bad.txt': ensure => file, content => "x\n" }
  file { '$WORK/target/perm-absent': mode => '0600' }
  file { '$WORK/target/perm.txt': mode => '0600' }
  file { '$WORK/target/dmx': ensure => directory, mode => '0640' }
  file { '$WORK/target/after-bad.txt': ensure => file, content => "never\n", require => File['$WORK/target/missing/bad.txt'] }
  notify { 'after-after-bad': message => 'never', require => File['$WORK/target/after-bad.txt'] }
  file { '$WORK/target/from-src.txt': source => '$WORK/src/one.txt' }
  file { '$WORK/target/tree': source => 'file://$WORK/src/tree', recurse => true }
  file { '$WORK/target/pur': ensure => directory, source => '$WORK/src/pur', recurse => true, purge => true }
  file { '$WORK/target/pur/keep.txt': ensure => file, content => "kept\n" }
  file { '$WORK/target/nosrc': source => ['$WORK/src/absent-1', 'file://$WORK/src/absent-2/'] }
  notify { 'after-nosrc': message => 'never', require => File['$WORK/target/nosrc'] }
  file { '$WORK/target/perms': recurse => true, mode => '0640' }
  file { 'aliased': path => '$WORK/target/aliased.txt', ensure => file, content => "a\n" }
  file { '$WORK/target/after-alias.txt': ensure => file, require => File['$WORK/target/aliased.txt'] }
  file { '$WORK/target/keep-old.txt': ensure => file, content => "new\n", replace => false }
  file { '$WORK/target/diffed.txt': ensure => file, content => "d\n", show_diff => true }
  file { '$WORK/target/fdir': ensure => file, content => "c\n", force => true }
  file { '$WORK/target/nofdir': ensure => file, content => "c\n" }
  file { '$WORK/target/rmdir': ensure => absent, force => true }
  file { '$WORK/target/keepdir': ensure => absent }
  file { '$WORK/target/wasfile': ensure => directory }
  file { '$WORK/target/lnf': ensure => file, content => "c\n" }
  file { '$WORK/target/lnfollow': ensure => file, content => "a\n", links => follow }
  file { '$WORK/target/lnfmode': links => follow, mode => '0600' }
  file { '$WORK/target/lnmode': mode => '0600' }
  file { '$WORK/target/src-ln': source => '$WORK/src/zlnk' }
  file { '$WORK/target/src-follow': source => 'file://$WORK/src/zlnk', links => follow, mode => '0644' }
  file { '$WORK/target/lnd': ensure => directory }
  file { '$WORK/target/ln-new': ensure => link, target => 'x' }
  file { '$WORK/target/lnfile': ensure => link, target => 'x' }
  file { '$WORK/target/lnold': ensure => link, target => 'new' }
  file { '$WORK/target/lndir': ensure => link, target => 'x', force => true }
  file { '$WORK/target/lndir-nf': ensure => link, target => 'x' }
  file { '$WORK/target/ln-symlink': ensure => symlink, target => '$WORK/target/lnfile' }
  file { '$WORK/target/ln-word': ensure => symlink }
  file { '$WORK/target/ln-none': ensure => link }
  file { '$WORK/target/tgtfile': ensure => file, target => 'x' }
  file { '$WORK/target/se-type': ensure => file, seltype => 'bin_t' }
  file { '$WORK/target/se-all': ensure => file, seluser => 'system_u', selrole => 'object_r', seltype => 'shadow_t', selrange => 's0' }
  file { '$WORK/target/se-ign': ensure => directory, selinux_ignore_defaults => true }
  file { '$WORK/target/se-diff': ensure => file, selinux_ignore_defaults => false, show_diff => true }
  file { '$WORK/target/lnwalk': recurse => true, group => '$(id -gn)', mode => '0600' }
  file { '$WORK/target/rf-absent': ensure => absent, replace => false, mode => '0600' }
  file { '$WORK/target/rf-fdir': ensure => file, content => "c\n", replace => false, force => true, mode => '0700' }
  file { '$WORK/target/rf-ln': ensure => file, content => "c\n", replace => false }
  file { '$WORK/target/rf-dir': ensure => directory, replace => false, mode => '0600' }
  file { '$WORK/target/rf-present': ensure => present, replace => false, mode => '0700' }
  file { '$WORK/target/rf-link': ensure => link, target => 'x', replace => false, force => true }
  file { '$WORK/target/rf-srcdir': source => '$WORK/src/tree', replace => false }
  file { '$WORK/target/rf-pur': ensure => directory, recurse => true, purge => true, force => true, replace => false, mode => '0700' }
  file { '$WORK/target/fifo-c': ensure => file, content => "c\n" }
  file { '$WORK/target/fifo-e': ensure => file }
  file { '$WORK/target/bk-suffix': ensure => file, content => "new\n", backup => '.bak' }
  file { '$WORK/target/bk-bucket': ensure => file, content => "new\n", backup => 'puppet' }
  file { '$WORK/target/sym-exist': mode => 'u+rwX,g-rwx,o-rwx' }
  file { '$WORK/target/sym-new': ensure => file, content => 'x', mode => 'g-w,o-rw' }
  file { '$WORK/target/sym-dir': ensure => directory, mode => 'u+rwx,g+rwx,o+rwxt' }
  notify { 'wo1_zeta': }
  notify { 'wo1_alpha': }
  notify { 'wo1_mid': }
  file { '$WORK/target/wo1': ensure => directory }
  notify { 'wo1_beta': }
  notify { 'wo2b_g': }
  notify { 'wo2b_f': }
  notify { 'wo2b_e': }
  notify { 'wo2b_free': }
  Notify['wo2b_e'] -> Notify['wo2b_f'] -> Notify['wo2b_g']
  notify { 'wo2c_p': require => Notify['wo2c_h'] }
  notify { 'wo2c_q': require => Notify['wo2c_h'] }
  notify { 'wo2c_r': }
  notify { 'wo2c_h': }
  notify { 'wo2c_s': require => Notify['wo2c_h'] }
  notify { 'wo2c_t': }
  notify { 'wo3b_before': }
  wo3b::def { 'd1': }
  notify { 'wo3b_after': }
  wo3b::def { 'd0': }
  notify { 'wo3b_last': }
  include wo3f_outer
  notify { 'wo3f_after': require => Class['wo3f_outer'] }
  notify { 'wo3f_late': }
  notify { 'wo3g_d': require => Class['wo3g_x'] }
  notify { 'wo3g_r': require => Notify['wo3g_xm'] }
  include wo3g_x
  notify { 'wo4a_first': }
  file { '$WORK/target/wo4a': recurse => true, mode => '0700' }
  notify { 'wo4a_after_d': }
  notify { 'wo4a_dep_on_d': require => File['$WORK/target/wo4a'] }
  file { '$WORK/target/wo4a/c': mode => '0700' }
  notify { 'wo4a_before_d': before => File['$WORK/target/wo4a'] }
}
EOF

# publicdir is not derived from vardir: as root it defaults to
# /opt/puppetlabs/puppet/public, which a gem-installed Puppet cannot create.
C=(--confdir "$WORK/conf" --vardir "$WORK/var" --codedir "$WORK/code" --publicdir "$WORK/public")

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
# Every path an agent reconciles rather than creates is seeded here, so both
# agents start from identical state: perm.txt and dmx (a mode-only resource
# and a directory mode lacking search bits), pur (a purge with a stale file,
# an unmanaged directory holding a file, and an explicitly managed child),
# perms (a permissions-only recursion), the ln* links to lnreal and
# lndreal, and what ensure => link and target replace: lnfile and tgtfile
# (files), lnold (a link) and lndir and lndir-nf (directories). ln-none and
# lndir-nf fail on both sides, like bad.txt. lnwalk is a recursion whose walk
# finds links into the tree, out of it (to lnreal) and dangling, with mode
# and the run's own group declared: it detects a mode event on a walked link,
# or any group event where the group is already in sync, while real and the
# directory are chmod'ed. It cannot see an lchown: an unprivileged run reaches
# no other group, and a failed group change on the parent would skip every
# child. The SELinux gate's root run compares that (D-28, retired).
# keep.txt is seeded with other content, so the run compares a content
# change: its desired_value is the ASCII-8BIT String gatejson.rb coerces.
# lnfollow's target already holds its catalog content. The rf-* paths hold
# a type their ensure would replace, so replace => false must keep every one
# (a file under absent and directory, a directory with force under file and
# link, a link under file, a file under a source directory) and purge
# nothing below rf-pur, while each declared mode still changes. fifo-c and
# fifo-e are fifos that ensure => file replaces, with and without content;
# reading one would block the agent. bk-suffix and bk-bucket hold content
# their content change replaces, so each is backed up first: to bk-suffix.bak
# and into the clientbucket under each agent's vardir. sym-exist is a 0750
# file a symbolic mode recomputes from its own bits. wo4a is a
# recursed tree, every entry off the 0700 its recursion declares, so each
# generated child logs a mode change and is placed in the walk. Explicit modes
# make the starting state independent of the caller's umask.
reset_target() {
  rm -rf "${WORK:?}/target" && mkdir -p "$WORK/target" || return 1
  printf 'seed\n' > "$WORK/target/perm.txt" && chmod 0644 "$WORK/target/perm.txt" \
    && mkdir -m 0700 "$WORK/target/dmx" || return 1
  printf 'old\n' > "$WORK/target/keep-old.txt" && chmod 0644 "$WORK/target/keep-old.txt" || return 1
  mkdir -p "$WORK/target/pur/unm" "$WORK/target/perms/sub" \
    "$WORK/target/fdir" "$WORK/target/nofdir" "$WORK/target/rmdir" "$WORK/target/keepdir" || return 1
  printf 'x\n' > "$WORK/target/fdir/x" && printf 'x\n' > "$WORK/target/nofdir/x" \
    && printf 'x\n' > "$WORK/target/rmdir/x" && printf 'x\n' > "$WORK/target/wasfile" \
    && chmod 0644 "$WORK/target/fdir/x" "$WORK/target/nofdir/x" "$WORK/target/rmdir/x" "$WORK/target/wasfile" \
    && chmod 0755 "$WORK/target/fdir" "$WORK/target/nofdir" "$WORK/target/rmdir" "$WORK/target/keepdir" || return 1
  printf 'stale\n' > "$WORK/target/pur/stale.txt" \
    && printf 'old\n' > "$WORK/target/pur/keep.txt" \
    && printf 'inner\n' > "$WORK/target/pur/unm/inner.txt" \
    && printf 'f\n' > "$WORK/target/perms/f" \
    && printf 'g\n' > "$WORK/target/perms/sub/g" \
    && chmod 0755 "$WORK/target/pur" "$WORK/target/pur/unm" "$WORK/target/perms" "$WORK/target/perms/sub" \
    && chmod 0644 "$WORK/target/pur/stale.txt" "$WORK/target/pur/keep.txt" \
      "$WORK/target/pur/unm/inner.txt" "$WORK/target/perms/f" "$WORK/target/perms/sub/g" || return 1
  printf 'a\n' > "$WORK/target/lnreal" && chmod 0644 "$WORK/target/lnreal" \
    && ln -s lnreal "$WORK/target/lnf" && ln -s lnreal "$WORK/target/lnfollow" \
    && ln -s lnreal "$WORK/target/lnmode" && ln -s lnreal "$WORK/target/lnfmode" \
    && mkdir -m 0755 "$WORK/target/lndreal" && ln -s lndreal "$WORK/target/lnd" || return 1
  printf 'x\n' > "$WORK/target/lnfile" && printf 'x\n' > "$WORK/target/tgtfile" \
    && chmod 0644 "$WORK/target/lnfile" "$WORK/target/tgtfile" \
    && ln -s old "$WORK/target/lnold" \
    && mkdir -m 0755 "$WORK/target/lndir" "$WORK/target/lndir-nf" \
    && printf 'x\n' > "$WORK/target/lndir/x" && chmod 0644 "$WORK/target/lndir/x" || return 1
  mkdir -m 0755 "$WORK/target/lnwalk" && printf 'w\n' > "$WORK/target/lnwalk/real" \
    && chmod 0644 "$WORK/target/lnwalk/real" && ln -s real "$WORK/target/lnwalk/ln" \
    && ln -s ../lnreal "$WORK/target/lnwalk/lnout" && ln -s nowhere "$WORK/target/lnwalk/dead" || return 1
  printf 'x\n' > "$WORK/target/rf-absent" && printf 'x\n' > "$WORK/target/rf-dir" \
    && printf 'x\n' > "$WORK/target/rf-srcdir" \
    && mkdir -m 0755 "$WORK/target/rf-fdir" "$WORK/target/rf-present" "$WORK/target/rf-link" \
      "$WORK/target/rf-pur" "$WORK/target/rf-pur/sub" \
    && printf 'x\n' > "$WORK/target/rf-fdir/x" && printf 'x\n' > "$WORK/target/rf-link/x" \
    && printf 'x\n' > "$WORK/target/rf-pur/stale" && printf 'x\n' > "$WORK/target/rf-pur/sub/y" \
    && chmod 0644 "$WORK/target/rf-absent" "$WORK/target/rf-dir" "$WORK/target/rf-srcdir" \
      "$WORK/target/rf-fdir/x" "$WORK/target/rf-link/x" "$WORK/target/rf-pur/stale" "$WORK/target/rf-pur/sub/y" \
    && ln -s lnreal "$WORK/target/rf-ln" || return 1
  mkfifo -m 0644 "$WORK/target/fifo-c" "$WORK/target/fifo-e" || return 1
  printf 'old\n' > "$WORK/target/bk-suffix" && printf 'bucketed\n' > "$WORK/target/bk-bucket" \
    && chmod 0640 "$WORK/target/bk-suffix" && chmod 0644 "$WORK/target/bk-bucket" \
    && touch -d '2020-01-01 00:00:00 UTC' "$WORK/target/bk-suffix" || return 1
  printf 'x\n' > "$WORK/target/sym-exist" && chmod 0750 "$WORK/target/sym-exist" || return 1
  mkdir -m 0755 "$WORK/target/wo4a" "$WORK/target/wo4a/a" "$WORK/target/wo4a/zdir" || return 1
  local f
  for f in 0 a/x a-b b c zdir/q; do
    printf '%s\n' "$f" > "$WORK/target/wo4a/$f" && chmod 0644 "$WORK/target/wo4a/$f" || return 1
  done
}

# backups FILE: what each agent's backups left -- the suffix copy's content,
# mode and mtime, and every entry of its clientbucket (by the vardir it was
# given) with its type, mode and content -- so the two can be compared
# byte for byte. The bucket's paths files name $WORK/target on both sides,
# because both agents reconcile the same tree.
backups() {
  { stat -c 'bk-suffix.bak %a %Y' "$WORK/target/bk-suffix.bak" && cat "$WORK/target/bk-suffix.bak" \
    && (cd "$2/clientbucket" && find . -printf '%P %y %m\n' | sort) \
    && (cd "$2/clientbucket" && find . -type f -print | sort | while IFS= read -r f; do printf '%s:\n' "$f" && cat "$f" || exit 1; done)
  } > "$1"
}

reset_target || exit 2
"$PUPPET" apply --catalog "$WORK/catalog.json" "${C[@]}" \
  --report --reports gatejson --libdir "$HERE/lib" --reportdir "$WORK/ruby" >"$WORK/ruby.out" 2>&1
# Ruby's report is captured by tests/spine/lib/puppet/reports/gatejson.rb as
# the data hash a report processor or PuppetDB would receive, which is the
# surface that has to match; that file records why it is captured live rather
# than converted from the stored YAML.
backups "$WORK/ruby-backups" "$WORK/var" || {
  echo "report-diff: Ruby's backups could not be read" >&2
  exit 2
}
# A run that bucketed nothing would agree with a voxagent that bucketed
# nothing, so Ruby's entry for bk-bucket's old content must be there.
bk_sum="$(printf 'bucketed\n' | sha256sum | cut -d' ' -f1)"
bk_entry="$WORK/var/clientbucket/$(printf '%s' "${bk_sum:0:8}" | sed 's|.|&/|g')$bk_sum"
if ! grep -qxF "$WORK/target/bk-bucket" "$bk_entry/paths" 2>/dev/null \
  || [ "$(cat "$bk_entry/contents" 2>/dev/null)" != "bucketed" ]; then
  echo "report-diff: Ruby's clientbucket holds no backup of bk-bucket ($bk_entry); nothing to compare against" >&2
  exit 2
fi
RUBY_REPORT="$WORK/ruby/gate.json"
[ -s "$RUBY_REPORT" ] || {
  # A report processor that fails to load says why only in the apply's
  # output, so its tail is the diagnosis.
  echo "report-diff: Ruby wrote no report; there is nothing to compare against" >&2
  tail -n 20 "$WORK/ruby.out" >&2
  exit 2
}

reset_target || exit 2
"$VOXAGENT" apply --catalog "$WORK/catalog.json" --report-dir "$WORK/go" \
  --vardir "$WORK/vox-var" >/dev/null 2>&1
GO_REPORT="$(find "$WORK/go" -name '*.json' | head -1)"
[ -n "$GO_REPORT" ] || {
  echo "report-diff: voxagent wrote no report" >&2
  exit 2
}

# keep_evidence: the divergence list names the fields, but reading why needs
# the two reports side by side, and the work tree is about to go.
keep_evidence() {
  mkdir -p "$EVIDENCE" && cp "$RUBY_REPORT" "$EVIDENCE/ruby.json" \
    && cp "$GO_REPORT" "$EVIDENCE/voxagent.json" \
    && cp "$WORK/catalog.json" "$EVIDENCE/catalog.json" \
    && echo "report-diff: the two reports and the catalog are in $EVIDENCE" >&2
}

"$GATEKIT" compare-reports \
  "$RUBY_REPORT" "$GO_REPORT" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0) echo "report-diff: reports agree outside the allowlist" ;;
  1)
    keep_evidence
    echo "report-diff: reports diverge" >&2
    ;;
  *) echo "report-diff: the comparison could not run" >&2 ;;
esac
# The walk order: compare-reports matches statuses by ref and logs as a
# multiset, so it cannot see the order resources were evaluated in.
# compare-order reads it from each report's log order. It is folded into rc
# as the backups are, below.
"$GATEKIT" compare-order "$RUBY_REPORT" "$GO_REPORT"
order_rc=$?
case "$order_rc" in
  0) ;;
  1)
    # A report divergence has already kept them.
    [ "$rc" -eq 1 ] || keep_evidence
    echo "report-diff: the walk order diverges" >&2
    ;;
  *)
    echo "report-diff: the walk order could not be compared" >&2
    order_rc=2
    ;;
esac
[ "$order_rc" -gt "$rc" ] && rc=$order_rc
# The backups are state, not report: the suffix copy and the clientbucket
# must be the same bytes, modes and layout on both sides. They are compared
# after the reports, so a report divergence is never hidden behind a backup
# one. Ruby made both (checked above), so a voxagent that left either missing
# backed up nothing it should have: a divergence, not a gate that could not
# run. backups_rc is folded into rc so that could-not-run (2) outranks a
# divergence (1), which outranks agreement.
backups_rc=0
if [ ! -d "$WORK/vox-var/clientbucket" ]; then
  echo "report-diff: voxagent made no clientbucket under its vardir; Ruby's holds bk-bucket's backup" >&2
  backups_rc=1
elif [ ! -e "$WORK/target/bk-suffix.bak" ] && [ ! -L "$WORK/target/bk-suffix.bak" ]; then
  echo "report-diff: voxagent left no bk-suffix.bak; Ruby made one" >&2
  backups_rc=1
elif ! backups "$WORK/go-backups" "$WORK/vox-var"; then
  echo "report-diff: voxagent's backups could not be read" >&2
  backups_rc=2
else
  diff -u "$WORK/ruby-backups" "$WORK/go-backups" >&2
  case $? in
    0) echo "report-diff: backups agree" ;;
    1)
      echo "report-diff: the backups differ (- Ruby, + voxagent)" >&2
      backups_rc=1
      ;;
    *)
      echo "report-diff: the backups could not be compared" >&2
      backups_rc=2
      ;;
  esac
fi
[ "$backups_rc" -gt "$rc" ] && rc=$backups_rc
exit "$rc"
