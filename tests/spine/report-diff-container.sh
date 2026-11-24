#!/usr/bin/env bash
# G5: compare the reports Ruby Puppet and voxagent produce from identical
# catalog bytes, with both applies moved inside a container.
#
# G1 (tests/spine/report-diff.sh) already does this on the host, and for file
# and notify the host is a perfectly good oracle. For `package` it is not, and
# it fails SILENTLY, which is worse than failing loudly:
#
#   The host's Ruby cannot fork under Puppet -- util.rb walks /proc/self/fd
#   closing descriptors and closes the handle it is iterating, so every
#   shell-out raises Errno::EBADF. dnf.rb's load-time confine shells out to
#   `rpm --version`, so it goes suitable=false, and Puppet silently falls back
#   to `dnfmodule`, which manages dnf MODULES rather than packages. A host-side
#   package differential runs, produces a plausible report, and answers a
#   different question. See docs/maintainers/apply-spine.md, "The two oracles
#   are not the same code".
#
# So this gate runs both agents in the fleet-node-el image, where openvox runs
# on a vendored Ruby that can fork and `dnf` is the suitable provider.
#
# The same fork defect is why `exec` is here rather than in G1: on the host every
# exec raises Errno::EBADF before running anything, so a host-side exec
# differential compares two reports about nothing. Inside the image exec works,
# which makes this the only gate that can compare an exec EVENT -- the
# `executed_command` name, `returns` as the property a bad status is attributed
# to, `desired_value` as the accepted list, and the refresh callback event --
# against Ruby.
#
# TWO CONTAINERS, ONE PER AGENT, and that is not a stylistic choice. G1 resets
# its target/ directory between the two applies; a package has no such
# analogue, because it mutates the machine rather than a directory. Sharing one
# container would leave Ruby's install in place, turn voxagent's install into a
# no-op, and the gate would compare a report about work against a report about
# nothing. Named with this script's PID because the fleet stack may be up: a
# fixed name would let the cleanup trap here destroy someone else's container.
#
# The comparator and the allowlist are `gatekit compare-reports` and
# tests/spine/allowlist.json, used unchanged. A second comparator would be a
# second thing to keep in step with Ruby.
#
#   0  the reports agree outside the allowlist and the walk order agrees
#   1  they diverge -- a result about voxagent
#   2  the comparison could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine}"
IMAGE="${IMAGE:-localhost/fleet-node-el:latest}"

# The same pin, and the same tripwire, as tests/spine/state-diff.sh. Read that
# file's note before bumping this: fleet/images/Dockerfile.el installs
# openvox-agent UNVERSIONED, so a rebuild floats the oracle, and this constant
# is the only thing that notices. Exit 2 -- refuse to compare -- beats a green
# run measured against a reference nobody reviewed.
#
# Deliberately NOT a second pinning mechanism: one constant per gate, the same
# shape, so a rebuild trips both in the same recognisable way.
WANT_PUPPET="${WANT_PUPPET:-8.33.0}"

# The four package fixtures. Every one of them is measured against the image
# below before anything is applied, because a package fixture whose starting
# state has drifted asserts nothing while still passing -- both agents do
# nothing, agree perfectly, and the gate goes green having never entered the
# provider.
#
#   tree             absent from the image and installable from baseos.
#   gdb-gdbserver    installed, and `rpm -e --test` lets it go alone.
#   rust-toolset-    DOWNGRADED to $PKG_LATEST_FROM (1.97.1-4.el10) in every
#   srpm-macros      container before anything else runs, so 1.98.1-1.el10 in
#                    appstream is an available update. The image installs the
#                    newest, and a fresh build has NO package with an update
#                    available (measured 2026-10-02), so an image-state fixture
#                    stops fitting with every rebuild; seeding makes the
#                    starting state the gate's own. The downgrade and the
#                    upgrade are that one package alone (measured: `dnf
#                    --assumeno downgrade` lists nothing else; annobin-docs and
#                    vim-data each drag a second package in). `ensure => latest`
#                    on an ALREADY-NEWEST package is a no-op, and the whole
#                    latest path -- the Is/IsDisplay split this sub-project was
#                    ordered around -- would go untested.
#   voxagent-no-     does not exist in any enabled repository, so the resource
#   such-package     FAILS. A differential that cannot reach a failure cannot
#                    see attribution, and attribution is what this gate is for.
PKG_INSTALL="${PKG_INSTALL:-tree}"
PKG_REMOVE="${PKG_REMOVE:-gdb-gdbserver}"
PKG_LATEST="${PKG_LATEST:-rust-toolset-srpm-macros}"
PKG_LATEST_FROM="${PKG_LATEST_FROM:-1.97.1-4.el10}"

# Run in every container before it is used, so all of them start from the same
# seeded state. Its failure is could-not-run: the repository no longer carries
# the version, and nothing about either agent has been learned.
SEED_LATEST="dnf -y -q downgrade '$PKG_LATEST-$PKG_LATEST_FROM' >/dev/null 2>&1"
PKG_MISSING="${PKG_MISSING:-voxagent-no-such-package}"

# The validate_cmd fixture's starting state, also run in every container: sudo
# for SIMP's `visudo -q -c -f %` (the image does not carry it), and the two
# existing files whose content change a validator accepts or refuses.
SEED_VALIDATE="dnf -y -q install sudo >/dev/null 2>&1 &&
  printf 'old\\n' > /tmp/target/v-fail && printf 'old\\n' > /tmp/target/v-notoken &&
  chmod 0644 /tmp/target/v-fail /tmp/target/v-notoken"

# The Sensitive fixture's starting state: files whose content, mode or owner
# the catalog changes, and one it removes, so each marked property has a
# previous value for Ruby to redact.
SEED_SENSITIVE="printf 'old\\n' > /tmp/target/s-change &&
  printf 'plain\\n' > /tmp/target/s-mode && chmod 0644 /tmp/target/s-mode &&
  touch /tmp/target/s-owner && printf 'g\\n' > /tmp/target/s-absent"

# The exec identity fixture's starting state: a root-owned directory nobody
# cannot search, which a user => nobody exec can start in only because cwd is
# entered as root before the identity drop.
SEED_EXEC="mkdir -m 0700 /tmp/target/private"

RUBY_SIDE="report-ruby-$$"
GO_SIDE="report-go-$$"
COMPILER="report-compile-$$"

command -v podman >/dev/null 2>&1 || {
  echo "report-diff-container: podman is not available" >&2
  exit 2
}
podman image exists "$IMAGE" || {
  echo "report-diff-container: $IMAGE is not built; run 'task fleet:images'" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff-container "$ROOT" "$VOXAGENT" "$GATEKIT"

got_puppet="$(podman run --rm "$IMAGE" /opt/puppetlabs/bin/puppet --version 2>/dev/null)" || {
  echo "report-diff-container: cannot read the image's puppet version" >&2
  exit 2
}
if [ "$got_puppet" != "$WANT_PUPPET" ]; then
  echo "report-diff-container: $IMAGE carries puppet $got_puppet but the oracle is" >&2
  echo "report-diff-container: pinned at $WANT_PUPPET; refusing to compare against an" >&2
  echo "report-diff-container: unpinned reference" >&2
  exit 2
fi

# The package fixture's starting state, read from a pristine container.
#
# `dnf check-update <pkg>` exits 100 when an update is available and 0 when
# there is none, which is how the `latest` candidate is checked: without this,
# a repository that has caught up turns that resource into a no-op and the
# latest path stops being exercised without anything going red.
#
# The failing name is checked with repoquery, which prints nothing for a name no
# enabled repository carries. If the name ever starts resolving, the resource
# stops failing and the attribution half of this gate quietly evaporates.
#
# The exec fixture's three commands are checked the same way and for the same
# reason. `test -d /nonexistent-spine` is asserted to exit exactly 1, not merely
# non-zero: the resource declaring `returns => [0, 1]` exists to carry an
# ACCEPTED non-zero status, and a command that started exiting 0 -- because the
# path came into existence -- would leave that resource indistinguishable from
# the plain successful one.
WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"; podman rm -f "$RUBY_SIDE" "$GO_SIDE" "$COMPILER" >/dev/null 2>&1' EXIT

if ! podman run --rm "$IMAGE" sh -c "
  if ! $SEED_LATEST; then
    echo 'downgrading $PKG_LATEST to $PKG_LATEST_FROM failed, so there is no latest fixture'; exit 1
  fi
  if [ \"\$(rpm -q --qf '%{VERSION}-%{RELEASE}' '$PKG_LATEST')\" != '$PKG_LATEST_FROM' ]; then
    echo '$PKG_LATEST is not at $PKG_LATEST_FROM after seeding'; exit 1
  fi
  if rpm -q '$PKG_INSTALL' >/dev/null 2>&1; then
    echo '$PKG_INSTALL is already installed, so installing it is a no-op'; exit 1
  fi
  if ! rpm -q '$PKG_REMOVE' >/dev/null 2>&1; then
    echo '$PKG_REMOVE is not installed, so removing it is a no-op'; exit 1
  fi
  if ! rpm -q '$PKG_LATEST' >/dev/null 2>&1; then
    echo '$PKG_LATEST is not installed, so it is not a latest fixture'; exit 1
  fi
  dnf -q check-update '$PKG_LATEST' >/dev/null 2>&1
  if [ \$? -ne 100 ]; then
    echo '$PKG_LATEST has no update available, so ensure => latest is a no-op'; exit 1
  fi
  if [ -n \"\$(dnf -q repoquery '$PKG_MISSING' 2>/dev/null)\" ]; then
    echo '$PKG_MISSING now resolves, so it no longer reaches the failure path'; exit 1
  fi
  if ! (mkdir -p /tmp/target && $SEED_VALIDATE) || [ ! -x /usr/sbin/visudo ]; then
    echo 'installing sudo failed, so there is no visudo for the validate_cmd fixture'; exit 1
  fi
  if [ -e /nonexistent/validator ]; then
    echo '/nonexistent/validator exists, so the validate_cmd exec failure is not one'; exit 1
  fi
  if ! /bin/true; then
    echo '/bin/true does not exit 0, so the successful exec is not successful'; exit 1
  fi
  if /bin/false; then
    echo '/bin/false exits 0, so the failing exec does not reach the failure path'; exit 1
  fi
  if ! id nobody >/dev/null 2>&1; then
    echo 'there is no user nobody, so the Sensitive owner change and the user => nobody execs cannot apply'; exit 1
  fi
  if ! getent group nobody >/dev/null 2>&1; then
    echo 'there is no group nobody, so the group => nobody execs fail instead of running'; exit 1
  fi
  if id nosuchuser >/dev/null 2>&1; then
    echo 'the user nosuchuser exists, so the unknown-user exec does not reach the failure path'; exit 1
  fi
  if [ -e /usr/bin/nosuchcmd ]; then
    echo '/usr/bin/nosuchcmd exists, so the missing argv command is not missing'; exit 1
  fi
  /usr/bin/test -d /nonexistent-spine
  if [ \$? -ne 1 ]; then
    echo 'test -d /nonexistent-spine does not exit 1, so returns => [0, 1] accepts nothing it would not accept anyway'; exit 1
  fi
" >"$WORK/fixture.err" 2>&1; then
  echo "report-diff-container: the package or exec fixture does not fit $IMAGE:" >&2
  sed -n '1,10p' "$WORK/fixture.err" >&2
  exit 2
fi

# The file and notify half, in a QUOTED heredoc so "alpha\n" reaches the
# manifest as written. Kept from G1's fixture rather than dropped, because it
# costs nothing here and a package-only gate would stop noticing the day a
# package change broke the file path.
#
# missing/bad.txt fails on purpose: its parent directory does not exist, so the
# create raises an Errno on both sides, which is what makes both of them WRAP
# it -- the only shape that exercises file's failure attribution.
#
# LINE NUMBERS BELOW ARE COMPARED. A wrapped failure's message ends
# "(file: .../site.pp, line: N)". Both implementations read N from the same
# compiled catalog, so inserting a line above simply moves N on both sides --
# expected, not a regression.
#
# The node block is left OPEN and closed by the second heredoc, which must be
# unquoted so the shell substitutes the four package names. One heredoc per
# quoting rule is cheaper than one heredoc that has to satisfy both.
cat > "$WORK/site.pp" <<'EOF' || exit 2
node default {
  notify { 'spine': message => 'constant text', notify => Exec['spine-exec-refresh'] }
  file { '/tmp/target/a.txt': ensure => file, content => "alpha\n", mode => '0644' }
  file { '/tmp/target/d':     ensure => directory, mode => '0755' }
  file { '/tmp/target/gone':  ensure => absent }
  file { '/tmp/target/missing/bad.txt': ensure => file, content => "x\n" }

  # The exec half. Until these resources existed, NO gate compared an exec event
  # against Ruby: G1's host Ruby cannot fork under Puppet, so every host exec
  # dies with Errno::EBADF, and G3 compares filesystem state rather than
  # reports. This is the only instrument that can see the `executed_command`
  # event name, `returns` as the property a non-zero status is attributed to, or
  # `desired_value` as the accepted list.
  #
  # Every command is deterministic and writes nothing. That is a hard
  # requirement, not tidiness: this gate compares two runs of two
  # implementations, so a command whose output or exit status varied between
  # runs would surface as a divergence with no cause in either agent -- and the
  # tempting fix for that is an allowlist entry, which would stop comparing the
  # leaf here and in G1 too.
  exec { 'spine-exec-ok':
    command => '/bin/true',
    cwd     => '/tmp',
  }
  # Exits 1 with [0] accepted, so the RETURNS PROPERTY fails -- not the
  # resource. The distinction is the whole point: type/exec.rb:86 declares
  # `newproperty(:returns, :array_matching => :all, :event =>
  # :executed_command)`, so a non-zero status is a property event carrying
  # desired_value, where a resource-level failure would carry neither a property
  # nor a desired_value.
  exec { 'spine-exec-fail':
    command => '/bin/false',
  }
  # An ACCEPTED non-zero status, and a desired_value with more than one entry.
  # `/usr/bin/test -d` on a missing path exits 1, which `returns` accepts, so
  # the event's status is success while the command returned 1 -- the shape that
  # catches an implementation reporting the observed status as desired_value
  # instead of the declared list. A single-element list cannot catch it: with
  # returns => [0] and a command that exits 0, the two are the same string.
  exec { 'spine-exec-returns':
    command => '/usr/bin/test -d /nonexistent-spine',
    returns => [0, 1],
  }
  # refreshonly, driven by the notify above. This is how an exec takes part in
  # refresh propagation, and G3 already covers two refresh hops for STATE; here
  # it is the report that matters -- the callback event, which carries name
  # "refresh", the message "Triggered 'refresh' from 1 event", and no property
  # at all. `restarted` itself is not comparable: to_data_hash never writes it,
  # and it reaches a report only as the resources.restarted metric.
  exec { 'spine-exec-refresh':
    command     => '/bin/true',
    refreshonly => true,
  }
  # A guard that SKIPS: onlyif runs the command only if every entry exits 0, and
  # /bin/false does not. The resource must record no event -- a status that is
  # neither changed nor failed and holds an empty event list. An implementation
  # that reported the skip as a change, or as a no-op event, diverges here.
  exec { 'spine-exec-guarded':
    command => '/bin/true',
    onlyif  => '/bin/false',
  }

  # argv commands. An argv runs with no shell, after Puppet's checkexe, and a
  # failure names it by Array#inspect.
  # Both agents log a failing command's output at the returns property
  # (logoutput's on_failure default); the Exec output cases below compare
  # that. 'exit 3' stands in for 'echo out; exit 3' only for brevity.
  #
  # Shell metacharacters and a space arrive as literal arguments.
  exec { 'spine-argv-ok':
    command => ['/bin/echo', 'hello world', '$HOME;ls', '*'],
  }
  # The failure detail is '["/bin/sh", "-c", "exit 3"]' returned 3 ...
  exec { 'spine-argv-fail':
    command => ['/bin/sh', '-c', 'exit 3'],
  }
  # A slash-less argv[0] is found through path.
  exec { 'spine-argv-rel':
    command => ['echo', 'rel'],
    path    => ['/usr/bin', '/bin'],
  }
  # checkexe fails the returns property with "Could not find command".
  exec { 'spine-argv-missing':
    command => ['nosuchcmd', 'x'],
    path    => '/usr/bin',
  }
  # Each name is one argument no shell sees, so touch creates every one
  # literally, and nothing is redirected. The one exec that writes, and only
  # into /tmp/target, which each side's own container starts empty.
  exec { 'spine-argv-meta':
    command => ['/usr/bin/touch', '/tmp/target/a b;c', '/tmp/target/`id`',
      '/tmp/target/$(id)', '/tmp/target/"q"', "/tmp/target/it's", '/tmp/target/>x'],
  }
  # An argv check that exits 0 makes unless skip, with no event.
  exec { 'spine-argv-unless':
    command => ['/bin/echo', 'x'],
    unless  => [['/usr/bin/test', '-d', '/tmp']],
  }

  # validate_cmd. Every failure message carries the
  # substituted temp path, whose date, pid and random token the comparator
  # masks (maskValidateTempNames); the output after "returned N: " is masked
  # as every ExecutionFailure's is (failure_detail).
  #
  # Accepted on a new file: file_created, "defined content as ...".
  file { '/tmp/target/v-ok':
    content      => "new\n",
    validate_cmd => '/usr/bin/test -s %',
  }
  # Refused, with output on both streams, over an existing file: content
  # fails, the file keeps its content, and mode still syncs (case 2, c02b).
  # The quotes and ; send the line through /bin/sh, as in Ruby.
  file { '/tmp/target/v-fail':
    content      => "new\n",
    mode         => '0600',
    validate_cmd => '/bin/sh -c "echo stdout line; echo stderr line >&2; exit 1" %',
  }
  # No token: the command runs verbatim and the file is replaced (case 3).
  file { '/tmp/target/v-notoken':
    content      => "new\n",
    validate_cmd => '/usr/bin/true',
  }
  # The validator cannot be executed: status 1, and Ruby's output is its log
  # destination's rendering of the exec error (case 6, c06a).
  file { '/tmp/target/v-noexec':
    content      => "new\n",
    validate_cmd => '/nonexistent/validator %',
  }
  # SIMP's sudoers check (case 7): valid content is created at 0440, and
  # invalid content fails ensure and creates nothing. -q prints nothing, so
  # Ruby's message ends "returned 1: ".
  file { '/tmp/target/sudoers-ok':
    content      => "Defaults env_reset\nroot ALL=(ALL) ALL\n",
    mode         => '0440',
    validate_cmd => '/usr/sbin/visudo -q -c -f %',
  }
  file { '/tmp/target/sudoers-bad':
    content      => "root ALL=(ALL ALL\nthis is not sudoers\n",
    mode         => '0440',
    validate_cmd => '/usr/sbin/visudo -q -c -f %',
  }

  # Sensitive. A marked property's event and log read "[redacted]"; Notify logs "Sensitive [value redacted]"; a marked Exec
  # command or guard reads "[command redacted]" and its output
  # "[output redacted]". A marked cwd only warns on the console.
  file { '/tmp/target/s-create': ensure => file, content => Sensitive("topsecret-create\n") }
  file { '/tmp/target/s-change': ensure => file, content => Sensitive("topsecret-change\n") }
  file { '/tmp/target/s-absent': ensure => absent, content => Sensitive("gone\n") }
  file { '/tmp/target/s-mode':   ensure => file, content => "plain\n", mode => Sensitive('0600') }
  file { '/tmp/target/s-owner':  ensure => file, owner => Sensitive('nobody') }
  notify { 'n1': message => Sensitive('notify-secret') }
  exec { 'e-unless':  command => '/bin/echo ran-unless', unless => Sensitive('/bin/false'), logoutput => true }
  exec { 'e-cwd':     command => '/bin/echo plain', onlyif => '/bin/true', cwd => Sensitive('/tmp'), logoutput => true }

  # Exec output (logoutput). The container sets no LANG, so output is decoded
  # as US-ASCII and binary-fail's \377 fails the exec in both agents.
  exec { 'default-ok':      command => '/bin/sh -c "echo quiet"' }
  exec { 'default-fail':    command => '/bin/sh -c "printf \"a\\n\\n  b\\n\\n\"; exit 2"' }
  exec { 'true-ok':         command => '/bin/sh -c "printf \"x\\r\\ny\\n\""', logoutput => true }
  exec { 'false-fail':      command => '/bin/sh -c "echo hidden; exit 2"', logoutput => false }
  exec { 'stderr-fail':     command => '/bin/sh -c "echo to-out; echo to-err >&2; exit 1"' }
  exec { 'binary-fail':     command => '/bin/sh -c "printf \"\\377\\376bad\\n\"; exit 1"' }
  exec { 'empty-fail':      command => '/bin/sh -c "exit 1"' }
  exec { 'noeol-true':      command => '/bin/sh -c "printf last-no-eol"', logoutput => true }
  exec { 'lo-string':       command => '/bin/sh -c "echo s"', logoutput => 'true' }
  exec { 'sens-true':       command => Sensitive('/bin/sh -c "echo s-true"'), logoutput => true }
  exec { 'sens-empty':      command => Sensitive('/bin/sh -c "exit 5"') }
  exec { 'sens-unless':     command => '/bin/sh -c "echo s-unless; exit 6"', unless => Sensitive('/bin/false') }
  exec { 'sens-ok-default': command => Sensitive('/bin/sh -c "echo s-ok"') }
  notify { 'kick': message => 'kick' }
  exec { 'refresh-true': command => '/bin/sh -c "echo refreshed"', refreshonly => true, logoutput => true, subscribe => Notify['kick'] }
  exec { 'refresh-fail': command => '/bin/sh -c "echo refresh-broke; exit 4"', refreshonly => true, subscribe => Notify['kick'] }
  exec { 'rs-true': command => Sensitive('/bin/sh -c "echo rs"'), refreshonly => true, logoutput => true, subscribe => Notify['kick'] }
  exec { 'rs-fail': command => Sensitive('/bin/sh -c "echo rsf; exit 3"'), refreshonly => true, subscribe => Notify['kick'] }

  # exec's run parameters. Timeouts are sub-second to keep the gate fast, and
  # no timed-out command backgrounds a grandchild: voxagent TERMs the whole
  # process group where Ruby TERMs only the child (D-83), so a grandchild's
  # survival is a state difference this report gate cannot see. These commands
  # write only into /tmp/target, which each side's container starts empty.
  exec { 'to-main':    command => '/bin/sleep 3', timeout => 0.3 }
  exec { 'to-guard':   command => '/bin/true', unless => '/bin/sleep 3', timeout => 0.3 }
  exec { 'to-refresh': command => '/bin/true', refresh => '/bin/sleep 3', timeout => 0.3,
    refreshonly => true, subscribe => Notify['kick'] }
  # Fails twice, then succeeds on the third try, counted in a file.
  exec { 'tries-third':
    command   => '/bin/sh -c "echo t >> /tmp/target/tries.count; test $(wc -l < /tmp/target/tries.count) -ge 3"',
    tries     => 3,
    try_sleep => 0.2,
  }
  # Every environment warning: unparsable, overriding, overriding PATH while
  # path is set, and empty (whose message carries this file's line). The guard
  # sees the same environment, and the empty warning is still logged once.
  exec { 'env-warn':
    command     => '/bin/sh -c "echo FOO=$FOO EMPTY=$EMPTY"',
    path        => ['/usr/bin', '/bin'],
    environment => ['FOO=bar', 'A-B=c', 'FOO=again', 'PATH=/usr/bin:/bin', 'EMPTY='],
    onlyif      => '/bin/true',
    logoutput   => true,
  }
  # The file's mode reaches the report only through the printed output.
  exec { 'umask-0027':
    command   => '/bin/sh -c "umask; touch /tmp/target/umask-file; stat -c %a /tmp/target/umask-file"',
    umask     => '0027',
    logoutput => true,
  }
  # A refresh command's exit 1 is a successful refresh; 127 fails it (I-31).
  exec { 'refresh-1':   command => '/bin/true', refresh => '/bin/sh -c "exit 1"', refreshonly => true, subscribe => Notify['kick'] }
  exec { 'refresh-127': command => '/bin/true', refresh => '/bin/sh -c "exit 127"', refreshonly => true, subscribe => Notify['kick'] }
  # Identity. group alone keeps root's supplementary groups; the unknown user
  # fails before the command starts; private-cwd is the seed's root-owned 0700
  # directory, entered as root before the drop to nobody.
  exec { 'as-user':     command => '/usr/bin/id', user => 'nobody', logoutput => true }
  exec { 'as-group':    command => '/usr/bin/id', group => 'nobody', logoutput => true }
  exec { 'as-both':     command => '/usr/bin/id', user => 'nobody', group => 'nobody', logoutput => true }
  exec { 'as-unknown':  command => '/usr/bin/id', user => 'nosuchuser', logoutput => true }
  exec { 'private-cwd': command => '/bin/pwd', cwd => '/tmp/target/private', user => 'nobody', logoutput => true }
  # Exit 127: the command's output is the failure detail, and a guard's makes
  # the resource fail to evaluate.
  exec { 'main-127':   command => '/bin/sh -c "echo gone >&2; exit 127"' }
  exec { 'unless-127': command => '/bin/true', unless => '/bin/sh -c "echo u; exit 127"' }
EOF

cat >> "$WORK/site.pp" <<EOF || exit 2
  package { '$PKG_INSTALL': ensure => present }
  package { '$PKG_REMOVE':  ensure => absent }
  package { '$PKG_LATEST':  ensure => latest }
  package { '$PKG_MISSING': ensure => present }
}
EOF

# The catalog is COMPILED by the image's own Puppet and the same bytes are fed
# to both sides. A hand-written catalog is a second, undocumented implementation
# of the catalog format, and G3 measured it being wrong: omitting `code_id` and
# the per-resource `exported` made Ruby refuse the catalog with "Could not run:
# Got nil value for name", a message that names neither field.
podman run --name "$COMPILER" --detach "$IMAGE" sleep 300 >/dev/null 2>&1 || {
  echo "report-diff-container: could not start a container to compile the fixture" >&2
  exit 2
}
podman cp "$WORK/site.pp" "$COMPILER:/tmp/site.pp" >/dev/null 2>&1 || exit 2
podman exec "$COMPILER" sh -c '
  mkdir -p /tmp/e/production/manifests &&
  cp /tmp/site.pp /tmp/e/production/manifests/site.pp &&
  /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
    --render-as json --node_name_value spine.example \
    --environmentpath /tmp/e --environment production 2>/dev/null |
  sed -n "/^{/,\$p"
' > "$WORK/catalog.json" 2>"$WORK/compile.err"
podman rm -f "$COMPILER" >/dev/null 2>&1
[ -s "$WORK/catalog.json" ] || {
  echo "report-diff-container: compiling the fixture catalog failed" >&2
  sed -n '1,20p' "$WORK/compile.err" >&2
  exit 2
}

# One container per agent, both from the same image, both starting identical.
# The apply's own exit status is not consulted: measured against Puppet 8,
# `puppet apply --catalog` ignores --detailed-exitcodes entirely, because
# Application::Apply#run_command dispatches --catalog to `apply`, which calls
# apply_catalog and discards its return value. The stored report is correct
# where the process exit is wrong, so the report is the oracle.
#
# Both containers are given the SAME hostname, and that is part of the
# identical starting state rather than a fudge. A report's `host` is
# Puppet[:node_name_value], which falls back to the certname and thence to the
# fqdn fact; voxagent derives its certname from os.Hostname() the same way.
# Left to podman, each container is named after its own id, so the two agents
# correctly report two different machines and `host` diverges for a reason that
# is nothing to do with either implementation. Pinning it keeps the leaf
# COMPARED -- an agent that read the hostname wrongly would still be caught --
# where an allowlist entry would have stopped comparing it here and in G1 too.
start_side() {
  local name="$1"
  podman run --name "$name" --hostname spine.example --detach "$IMAGE" \
    sleep 900 >/dev/null 2>&1 || return 2
  podman cp "$WORK/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1 || return 2
  podman cp "$HERE/lib" "$name:/tmp/gatelib" >/dev/null 2>&1 || return 2
  podman exec "$name" sh -c 'mkdir -p /tmp/target' >/dev/null 2>&1 || return 2
  podman exec "$name" sh -c "$SEED_LATEST" || return 2
  podman exec "$name" sh -c "$SEED_VALIDATE" || return 2
  podman exec "$name" sh -c "$SEED_SENSITIVE" || return 2
  podman exec "$name" sh -c "$SEED_EXEC" || return 2
  return 0
}

start_side "$RUBY_SIDE" || {
  echo "report-diff-container: could not start the Ruby side" >&2
  exit 2
}
# --color=false: a forked child's error is logged through Ruby's console
# destination into the captured output, so a coloured console puts ANSI codes
# into the report message. voxagent never colours (D-85).
podman exec "$RUBY_SIDE" sh -c '
  /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json \
    --report --reports gatejson --libdir /tmp/gatelib --reportdir /tmp/ruby \
    --color=false
' >"$WORK/ruby.out" 2>&1

# Ruby's report is captured by tests/spine/lib/puppet/reports/gatejson.rb as
# the data hash a report processor or PuppetDB would receive, which is the
# surface that has to match; that file records why it is captured live rather
# than converted from the stored YAML. Captured INSIDE the container, by the
# image's own openvox: the host's Ruby is the one that cannot fork, and loading
# a different Puppet to read this image's report would be a third
# implementation in the comparison.
podman exec "$RUBY_SIDE" cat /tmp/ruby/gate.json >"$WORK/ruby.json" 2>"$WORK/convert.err"
if [ ! -s "$WORK/ruby.json" ]; then
  echo "report-diff-container: Ruby wrote no usable report; there is nothing to" >&2
  echo "report-diff-container: compare against" >&2
  sed -n '1,20p' "$WORK/convert.err" >&2
  sed -n '1,20p' "$WORK/ruby.out" >&2
  exit 2
fi

start_side "$GO_SIDE" || {
  echo "report-diff-container: could not start the voxagent side" >&2
  exit 2
}
podman cp "$VOXAGENT" "$GO_SIDE:/tmp/voxagent" >/dev/null 2>&1 || {
  echo "report-diff-container: could not copy $VOXAGENT into the container" >&2
  exit 2
}
podman exec "$GO_SIDE" sh -c '
  chmod +x /tmp/voxagent &&
  /tmp/voxagent apply --catalog /tmp/catalog.json --report-dir /tmp/go
' >"$WORK/go.out" 2>&1
podman exec "$GO_SIDE" sh -c '
  g="$(find /tmp/go -name "*.json" | head -1)"
  [ -n "$g" ] || exit 3
  cat "$g"
' >"$WORK/go.json" 2>"$WORK/go.err"
if [ ! -s "$WORK/go.json" ]; then
  echo "report-diff-container: voxagent wrote no report" >&2
  sed -n '1,20p' "$WORK/go.err" >&2
  sed -n '1,20p' "$WORK/go.out" >&2
  exit 2
fi

# Prove the fixture REACHED the provider before trusting the comparison.
#
# Comparing two reports answers "do the agents agree?", which is a different
# question from "was any package or exec work attempted?". If the container
# cannot reach a repository, dnf fails identically on both sides, the reports match
# to the byte, and the gate goes green having exercised nothing -- the
# contaminated-control shape recorded in docs/maintainers/apply-spine.md.
#
# Asserted against RUBY alone, deliberately. Ruby is the reference, so Ruby
# getting this wrong means the fixture did not run and nothing was compared
# (exit 2). If only voxagent got it wrong, that is a real divergence and the
# comparator below is where it belongs (exit 1). Asserting it on both sides
# would convert every voxagent package defect into "could not run".
# gatekit prints one complaint per line; the prefix keeps them in this gate's
# voice. Under pipefail the pipeline's status is its rightmost non-zero one,
# which is gatekit's whenever sed succeeds, so a refusal (2) is not hidden
# behind sed's 0. The assertions themselves, and why each exists,
# are in internal/gatekit/g5ref.
if ! "$GATEKIT" g5-reference "$WORK/ruby.json" "$WORK/catalog.json" \
  "$PKG_INSTALL" "$PKG_REMOVE" "$PKG_LATEST" "$PKG_MISSING" |
  sed 's/^/report-diff-container: /' >&2
then
  echo "report-diff-container: the reference run did not exercise the package or" >&2
  echo "report-diff-container: exec fixture, so nothing meaningful was compared --" >&2
  echo "report-diff-container: check that the container can reach its repositories" >&2
  sed -n '1,20p' "$WORK/ruby.out" >&2
  exit 2
fi

# keep_evidence: the divergence list names the fields, but reading why needs
# the two reports side by side, and the work tree is about to go.
keep_evidence() {
  mkdir -p "$EVIDENCE" \
    && cp "$WORK/ruby.json" "$EVIDENCE/container-ruby.json" \
    && cp "$WORK/go.json" "$EVIDENCE/container-voxagent.json" \
    && cp "$WORK/catalog.json" "$EVIDENCE/container-catalog.json" \
    && echo "report-diff-container: the two reports and the catalog are in $EVIDENCE" >&2
}

"$GATEKIT" compare-reports \
  "$WORK/ruby.json" "$WORK/go.json" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0) echo "report-diff-container: reports agree outside the allowlist" ;;
  1)
    keep_evidence
    echo "report-diff-container: reports diverge" >&2
    ;;
  *) echo "report-diff-container: the comparison could not run" >&2 ;;
esac
# The walk order: compare-reports matches statuses by ref and logs as a
# multiset, so it cannot see the order resources were evaluated in.
# compare-order reads it from each report's log order. Could-not-run (2)
# outranks a divergence (1), which outranks agreement.
"$GATEKIT" compare-order "$WORK/ruby.json" "$WORK/go.json"
order_rc=$?
case "$order_rc" in
  0) ;;
  1)
    [ "$rc" -eq 1 ] || keep_evidence
    echo "report-diff-container: the walk order diverges" >&2
    ;;
  *)
    echo "report-diff-container: the walk order could not be compared" >&2
    order_rc=2
    ;;
esac
[ "$order_rc" -gt "$rc" ] && rc=$order_rc
exit "$rc"
