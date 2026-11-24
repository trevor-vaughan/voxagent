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
#   0  the reports agree outside the allowlist
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
WANT_PUPPET="${WANT_PUPPET:-8.29.0}"

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
  if ! /bin/true; then
    echo '/bin/true does not exit 0, so the successful exec is not successful'; exit 1
  fi
  if /bin/false; then
    echo '/bin/false exits 0, so the failing exec does not reach the failure path'; exit 1
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
  podman exec "$name" sh -c 'mkdir -p /tmp/target' >/dev/null 2>&1 || return 2
  podman exec "$name" sh -c "$SEED_LATEST" || return 2
  return 0
}

start_side "$RUBY_SIDE" || {
  echo "report-diff-container: could not start the Ruby side" >&2
  exit 2
}
podman exec "$RUBY_SIDE" sh -c '
  /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json \
    --report --reports store --reportdir /tmp/ruby
' >"$WORK/ruby.out" 2>&1

# Ruby's report is a YAML object graph; convert it to the same data hash a
# report processor or PuppetDB would receive, which is the surface that has to
# match. Converted INSIDE the container, with the image's vendored ruby and its
# openvox: the host's Ruby is the one that cannot fork, and loading a different
# Puppet to deserialise this image's report would be a third implementation in
# the comparison.
podman exec "$RUBY_SIDE" sh -c '
  y="$(find /tmp/ruby -name "*.yaml" | head -1)"
  [ -n "$y" ] || exit 3
  /opt/puppetlabs/puppet/bin/ruby -ryaml -rpuppet -rjson -e "
    puts YAML.unsafe_load_file(ARGV[0]).to_data_hash.to_json
  " "$y"
' >"$WORK/ruby.json" 2>"$WORK/convert.err"
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

"$GATEKIT" compare-reports \
  "$WORK/ruby.json" "$WORK/go.json" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0) echo "report-diff-container: reports agree outside the allowlist" ;;
  1)
    # Keep the evidence. The divergence list names the fields, but reading why
    # needs the two reports side by side, and the work tree is about to go.
    mkdir -p "$EVIDENCE" \
      && cp "$WORK/ruby.json" "$EVIDENCE/container-ruby.json" \
      && cp "$WORK/go.json" "$EVIDENCE/container-voxagent.json" \
      && cp "$WORK/catalog.json" "$EVIDENCE/container-catalog.json" \
      && echo "report-diff-container: the two reports and the catalog are in $EVIDENCE" >&2
    echo "report-diff-container: reports diverge" >&2
    ;;
  *) echo "report-diff-container: the comparison could not run" >&2 ;;
esac
exit "$rc"
