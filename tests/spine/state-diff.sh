#!/usr/bin/env bash
# G3: apply the same catalog in two identical containers, one per agent, and
# compare the resulting state on disk.
#
# This is the only gate that checks the disk rather than the report, and the
# only place two things can be tested at all:
#
#   * exec. Measured: the host runs Ruby 3.4.9 against the Puppet 8.10.0 gem,
#     and util.rb walks /proc/self/fd closing descriptors, closing the directory
#     handle it is iterating. Every exec fails with Errno::EBADF and produces no
#     side effect. The fleet-node-el image runs openvox on a vendored Ruby
#     3.2.11, where the same exec succeeds. The exact version is asserted below
#     rather than stated here, so one place can go stale instead of two.
#   * file's owner and group, which need root.
#
# It also exercises the refresh path end to end, which the report differential
# cannot: G1's fixture has no exec, for the Errno::EBADF reason above.
#
# Containers are named with this script's PID. The fleet stack may be up and
# other agents may be running, and a fixed name would make the cleanup trap here
# capable of destroying someone else's container.
#
#   0  the two agents produced the same state
#   1  they diverged -- a result about voxagent
#   2  the comparison could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
ARTIFACTS="${ARTIFACTS:-$ROOT/.test-output/spine}"
IMAGE="${IMAGE:-localhost/fleet-node-el:latest}"
# Pinned -- but read what this pin is, because it is a tripwire, not a guarantee.
#
# fleet/images/Dockerfile.el installs openvox-agent UNVERSIONED, which its own
# comment lists as tracked debt, so a rebuild floats the oracle. This constant is
# what notices. It did: a rebuild moved the image from 8.28.1 to 8.29.0 and this
# gate exited 2 -- refusing to compare -- rather than quietly measuring against a
# reference nobody reviewed.
#
# Bumped to 8.29.0 deliberately, after re-measuring: at 8.29.0 the two agents
# still produce identical state across all four compared paths. Bumping without
# that measurement would have turned the tripwire into a rubber stamp. Bumped
# again to 8.33.0 on 2026-11-17 the same way, when a rebuild moved the image:
# this gate, report-diff-container, -systemd and -simp all agreed at 8.33.0
# before any pin moved.
#
# The float itself is untouched, which means the NEXT rebuild will trip this
# again. That is the intended failure mode: exit 2 and a demand to re-measure
# beats a green run whose meaning silently changed. Pinning the Dockerfile would
# fix it properly and belongs to the fleet harness, not the apply spine.
WANT_PUPPET="${WANT_PUPPET:-8.33.0}"

# The two package fixtures, chosen by measuring the image rather than by taste.
#
#   tree           absent from the image, available from baseos, and its install
#                  transaction is exactly one package -- `dnf --assumeno install
#                  tree` lists tree and nothing else, so adding it cannot make
#                  the installed-set comparison noisy.
#   gdb-gdbserver  present in the image, and the only package rpm will let go of
#                  cleanly that nothing here uses: `rpm -e --test` succeeds, and
#                  `dnf --assumeno remove` takes it alone with no "removing
#                  unused dependencies" tail. libxkbcommon, the other obvious
#                  candidate, drags xkeyboard-config out with it.
#
# Deliberately NOT `zsh` for the removal: zsh is not installed in this
# image, so `ensure => absent` on it is a no-op that exercises nothing.
#
# Both are checked against the live image below, before anything is applied. A
# fixture whose starting state has drifted -- tree preinstalled, gdb-gdbserver
# already gone -- asserts nothing while still passing, which is the trap this
# gate exists on the other side of.
PKG_INSTALL="${PKG_INSTALL:-tree}"
PKG_REMOVE="${PKG_REMOVE:-gdb-gdbserver}"

RUBY_SIDE="spine-ruby-$$"
GO_SIDE="spine-go-$$"
COMPILER="spine-compile-$$"

command -v podman >/dev/null 2>&1 || {
  echo "state-diff: podman is not available" >&2
  exit 2
}
podman image exists "$IMAGE" || {
  echo "state-diff: $IMAGE is not built; run 'task fleet:images'" >&2
  exit 2
}

got_puppet="$(podman run --rm "$IMAGE" /opt/puppetlabs/bin/puppet --version 2>/dev/null)" || {
  echo "state-diff: cannot read the image's puppet version" >&2
  exit 2
}
if [ "$got_puppet" != "$WANT_PUPPET" ]; then
  echo "state-diff: $IMAGE carries puppet $got_puppet but the oracle is pinned at $WANT_PUPPET" >&2
  echo "state-diff: refusing to compare against an unpinned reference" >&2
  exit 2
fi

# The package fixture's starting state, read from a pristine container.
#
# `package { tree: ensure => present }` proves nothing if tree is already
# installed, and `ensure => absent` proves nothing if the package is already
# gone: both agents would do nothing, agree perfectly, and this gate would go
# green having never entered the dnf provider. Same shape as a fixture declaring
# `owner => root` on an already-root-owned file.
if ! podman run --rm "$IMAGE" sh -c \
  "! rpm -q '$PKG_INSTALL' >/dev/null 2>&1 && rpm -q '$PKG_REMOVE' >/dev/null 2>&1"; then
  echo "state-diff: the package fixture does not fit $IMAGE -- it needs" >&2
  echo "state-diff: $PKG_INSTALL absent and $PKG_REMOVE installed, and the image" >&2
  echo "state-diff: no longer starts that way, so applying it would exercise no" >&2
  echo "state-diff: package work at all" >&2
  exit 2
fi

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"; podman rm -f "$RUBY_SIDE" "$GO_SIDE" "$COMPILER" >/dev/null 2>&1' EXIT

# The catalog is COMPILED by the image's own Puppet rather than hand-written
# here, and then the same bytes are fed to both sides.
#
# A hand-written catalog is a second, undocumented implementation of the catalog
# format, and it was wrong: the first version of this fixture omitted `code_id`
# and the per-resource `exported`, and Ruby refused it with "Could not run: Got
# nil value for name" -- a message that names neither field. Compiling removes
# that whole class of failure and, as a bonus, produces the real containment
# edges (Stage[main] -> Class[main] -> Node[default] -> ...) so this gate
# exercises container handling too.
#
# /etc/hosts is deliberately NOT in this fixture, and the reason is measured.
# podman writes each container's OWN id and name into it, so two containers
# differ at that path before either agent touches it -- there is no identical
# starting state to compare from. It is also unmanageable by Puppet in a
# container at all: /etc/hosts is a bind mount, and replace_file's rename cannot
# cross one, so Ruby fails with EBUSY.
#
# Proving the snapshot sees what `podman diff` cannot is a separate job from
# comparing the two agents, and it is done by the self-check below rather than
# by smuggling an uncomparable path into the fixture.
cat > "$WORK/site.pp" <<'EOF' || exit 2
node default {
  file { '/opt/spine':
    ensure => directory, mode => '0750', owner => 'daemon', group => 'daemon',
  }
  file { '/opt/spine/conf':
    ensure  => file, content => "alpha\n", mode => '0600', owner => 'daemon',
    notify  => Exec['reload'],
  }
  exec { 'reload':
    command => '/usr/bin/touch /opt/spine/reloaded', refreshonly => true,
    notify  => Exec['cascade'],
  }
  # The second hop. A refresh propagates: event_manager.rb's process_events
  # queues a :restarted event FROM a resource that was successfully refreshed,
  # and queue_events dispatches it along that resource's own edges. Exec[cascade]
  # must fire even though nothing about it changed and File[conf] does not name
  # it -- being refreshed is itself the event that propagates.
  #
  # Without this hop the fixture proved only that the FIRST link fires, which a
  # one-hop implementation satisfies exactly as well as a correct one.
  exec { 'cascade':
    command => '/usr/bin/touch /opt/spine/cascaded', refreshonly => true,
  }
EOF

# The node block is left OPEN above and closed here, because this half needs the
# shell to substitute the two package names while the half above must not be
# substituted at all -- it contains "alpha\n", which an unquoted heredoc would be
# free to mangle. One heredoc per quoting rule is cheaper than one heredoc that
# has to satisfy both.
cat >> "$WORK/site.pp" <<EOF || exit 2
  package { '$PKG_INSTALL': ensure => present }
  package { '$PKG_REMOVE': ensure => absent }
}
EOF

podman run --name "$COMPILER" --detach "$IMAGE" sleep 120 >/dev/null 2>&1 || {
  echo "state-diff: could not start a container to compile the fixture" >&2
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
  echo "state-diff: compiling the fixture catalog failed" >&2
  sed -n '1,20p' "$WORK/compile.err" >&2
  exit 2
}

cat > "$WORK/paths.txt" <<'EOF' || exit 2
/opt/spine
/opt/spine/conf
/opt/spine/reloaded
/opt/spine/cascaded
EOF

# Built from inside $ROOT, not merely pointed at it. `go build` resolves the
# module from the WORKING directory, so invoking this script from anywhere
# outside the repository failed with "building voxagent failed" -- an outage
# reported as if the code were broken.
(cd "$ROOT" && go build -o "$WORK/voxagent" ./cmd/voxagent) || {
  echo "state-diff: building voxagent failed" >&2
  exit 2
}

# One container per agent, both from the same image, both starting identical.
#
# The snapshot is invoked with bash rather than sh: it declares `set -o
# pipefail`, which dash does not implement, and this repository has already been
# bitten once by /bin/sh being a different shell inside a container than out.
run_side() {
  local name="$1" cmd="$2"
  podman run --name "$name" --detach "$IMAGE" sleep 300 >/dev/null 2>&1 || return 2
  podman cp "$WORK/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1 || return 2
  podman cp "$HERE/snapshot.sh" "$name:/tmp/snapshot.sh" >/dev/null 2>&1 || return 2
  podman cp "$WORK/paths.txt" "$name:/tmp/paths.txt" >/dev/null 2>&1 || return 2
  if [ "$name" = "$GO_SIDE" ]; then
    podman cp "$WORK/voxagent" "$name:/tmp/voxagent" >/dev/null 2>&1 || return 2
  fi
  podman exec "$name" sh -c "$cmd" >"$WORK/$name.out" 2>&1
  echo "$?" > "$WORK/$name.rc"
  podman exec "$name" sh -c 'bash /tmp/snapshot.sh < /tmp/paths.txt' \
    >"$WORK/$name.state" 2>&1 || return 2
  # The WHOLE installed set, not just the fixture's two names. Restricting it to
  # the fixture would make the comparison blind to exactly the failure a package
  # provider is most likely to produce -- a removal that took a dependency with
  # it. That was a real risk here and it is why libxkbcommon was rejected as the
  # removal fixture: dnf drops xkeyboard-config alongside it. Measured, both
  # fixture transactions are single-package on this image, so the whole set is
  # not noisy; if a future fixture makes it noisy, narrow the fixture rather than
  # the comparison.
  #
  # Versions are deliberately excluded: %{NAME} only. A version column would turn
  # any repository update landing between the two containers' dnf runs into a
  # reported divergence about voxagent, which it would not be.
  podman exec "$name" sh -c 'rpm -qa --qf "%{NAME}\n" | sort' \
    >"$WORK/$name.packages" 2>&1 || return 2
  podman diff --format json "$name" >"$WORK/$name.layer" 2>/dev/null || return 2
  return 0
}

run_side "$RUBY_SIDE" '/opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json' || {
  echo "state-diff: the Ruby side could not run" >&2
  sed -n '1,20p' "$WORK/$RUBY_SIDE.out" >&2
  exit 2
}
run_side "$GO_SIDE" 'chmod +x /tmp/voxagent && /tmp/voxagent apply --catalog /tmp/catalog.json' || {
  echo "state-diff: the voxagent side could not run" >&2
  sed -n '1,20p' "$WORK/$GO_SIDE.out" >&2
  exit 2
}

# Prove the instrument before trusting it.
#
# The whole reason this gate snapshots named paths instead of reading
# `podman diff` is that podman bind-mounts /etc/hosts, /etc/resolv.conf and
# /etc/hostname, so changes to them never reach the image layer. That is a claim
# about podman, not about this repository, and a claim nobody re-checks is one
# that quietly stops being true. So check it on every run: change a bind-mounted
# path, and require that the layer diff misses it and the snapshot catches it.
probe_before="$(podman exec "$GO_SIDE" sh -c 'echo /etc/hosts | bash /tmp/snapshot.sh')" || exit 2
podman exec "$GO_SIDE" sh -c 'echo "1.2.3.4 instrument-proof" >> /etc/hosts' >/dev/null 2>&1 || {
  echo "state-diff: could not modify a bind-mounted path to test the instrument" >&2
  exit 2
}
probe_after="$(podman exec "$GO_SIDE" sh -c 'echo /etc/hosts | bash /tmp/snapshot.sh')" || exit 2

if [ "$probe_before" = "$probe_after" ]; then
  echo "state-diff: the snapshot did not notice a change to /etc/hosts, so it" >&2
  echo "state-diff: cannot be trusted to notice one in the compared paths" >&2
  exit 2
fi
if podman diff --format json "$GO_SIDE" | grep -q '/etc/hosts'; then
  echo "state-diff: podman diff now reports /etc/hosts, so the premise this gate" >&2
  echo "state-diff: was built on no longer holds -- re-derive which paths the" >&2
  echo "state-diff: layer diff is blind to before trusting this comparison" >&2
  exit 2
fi

# A voxagent that REFUSED the catalog exits 2 in plain mode and changes nothing.
# Its snapshot would then differ from Ruby's at every path, and reporting that as
# a divergence would be an outage dressed up as a finding -- the defect class
# this repository tracks most closely. Could-not-run is 2, not 1, in plain mode
# (divergence D-3).
go_rc="$(cat "$WORK/$GO_SIDE.rc")"
if [ "$go_rc" = "2" ]; then
  echo "state-diff: voxagent could not run this catalog; nothing was compared" >&2
  sed -n '1,20p' "$WORK/$GO_SIDE.out" >&2
  exit 2
fi

for side in "$RUBY_SIDE" "$GO_SIDE"; do
  [ -s "$WORK/$side.state" ] || {
    echo "state-diff: $side produced no snapshot" >&2
    exit 2
  }
  [ -s "$WORK/$side.packages" ] || {
    echo "state-diff: $side produced no installed-package list" >&2
    exit 2
  }
  if grep -q 'UNREADABLE' "$WORK/$side.state"; then
    echo "state-diff: $side could not read part of the state it was asked to record" >&2
    grep 'UNREADABLE' "$WORK/$side.state" >&2
    exit 2
  fi
done

# The refresh must actually have fired on the reference side.
#
# Without this, a fixture whose notify never forms an edge -- for instance
# because the metaparameter was written outside `parameters`, where nothing
# reads it -- leaves /opt/spine/reloaded absent on BOTH sides. The two snapshots
# would then agree perfectly and this gate would exit 0 having tested no refresh
# at all. That is an assertion that cannot fail.
#
# Checked against Ruby rather than voxagent on purpose: Ruby is the reference, so
# if IT did not refresh then the fixture is wrong and the comparison could not
# run. If only voxagent failed to refresh, that is a real divergence and the diff
# below reports it as one.
#
# Matched on "regular" rather than "regular file", because `stat -c %F` returns
# "regular EMPTY file" for a zero-byte path and the marker is made with touch.
# The stricter spelling made this guard fire on a run where the refresh HAD
# worked -- a gate reporting an outage as a fixture defect.
for marker in /opt/spine/reloaded /opt/spine/cascaded; do
  if ! grep -q "^$marker|regular" "$WORK/$RUBY_SIDE.state"; then
    echo "state-diff: the reference run did not create $marker, so this fixture" >&2
    echo "state-diff: proves nothing about refresh propagation -- check that notify" >&2
    echo "state-diff: sits inside the resource's \"parameters\" object" >&2
    sed -n '1,20p' "$WORK/$RUBY_SIDE.out" >&2
    exit 2
  fi
done

# The package work must ABSOLUTELY have happened, not merely have happened the
# same way twice.
#
# Comparing the two installed sets answers "do the agents agree?", which is a
# different question from "was the provider entered at all?". If the container
# cannot reach a repository, or the package name stops existing, dnf fails
# identically on both sides: the two sets still match to the byte and the gate
# goes green having exercised nothing. That is the contaminated-control shape
# recorded in docs/maintainers/apply-spine.md -- a comparison whose two halves
# fail together cannot see the failure.
#
# So assert the post-state outright. Checked against Ruby alone, for the same
# reason the refresh markers above are: Ruby is the reference, so Ruby getting it
# wrong means the fixture did not run and nothing was compared (exit 2). If only
# voxagent got it wrong, that is a real divergence and the set diff below is
# where it belongs (exit 1). Asserting it on both sides would convert every
# voxagent package defect into "could not run".
if ! grep -qx "$PKG_INSTALL" "$WORK/$RUBY_SIDE.packages"; then
  echo "state-diff: the reference run did not install $PKG_INSTALL, so the" >&2
  echo "state-diff: package fixture exercised nothing -- check that the container" >&2
  echo "state-diff: can reach its repositories and that $PKG_INSTALL still exists" >&2
  sed -n '1,20p' "$WORK/$RUBY_SIDE.out" >&2
  exit 2
fi
if grep -qx "$PKG_REMOVE" "$WORK/$RUBY_SIDE.packages"; then
  echo "state-diff: the reference run did not remove $PKG_REMOVE, so the package" >&2
  echo "state-diff: fixture exercised nothing on the removal path" >&2
  sed -n '1,20p' "$WORK/$RUBY_SIDE.out" >&2
  exit 2
fi

diverged=0
diff -u "$WORK/$RUBY_SIDE.state" "$WORK/$GO_SIDE.state" >"$WORK/state.diff" || diverged=1
diff -u "$WORK/$RUBY_SIDE.packages" "$WORK/$GO_SIDE.packages" >"$WORK/packages.diff" || diverged=1

if [ "$diverged" = "0" ]; then
  echo "state-diff: both agents produced the same state across $(wc -l < "$WORK/paths.txt") paths" \
    "and the same $(wc -l < "$WORK/$RUBY_SIDE.packages") installed packages"
  exit 0
fi

echo "state-diff: the two agents produced different state" >&2
mkdir -p "$ARTIFACTS" \
  && cp "$WORK/state.diff" "$ARTIFACTS/state.diff" \
  && cp "$WORK/packages.diff" "$ARTIFACTS/packages.diff" \
  && cp "$WORK/$RUBY_SIDE.state" "$ARTIFACTS/state-ruby.txt" \
  && cp "$WORK/$GO_SIDE.state" "$ARTIFACTS/state-voxagent.txt" \
  && cp "$WORK/$RUBY_SIDE.packages" "$ARTIFACTS/packages-ruby.txt" \
  && cp "$WORK/$GO_SIDE.packages" "$ARTIFACTS/packages-voxagent.txt" \
  && cp "$WORK/$RUBY_SIDE.layer" "$ARTIFACTS/layer-ruby.json" \
  && cp "$WORK/$GO_SIDE.layer" "$ARTIFACTS/layer-voxagent.json" \
  && cp "$WORK/catalog.json" "$ARTIFACTS/state-diff-catalog.json" \
  && echo "state-diff: evidence in $ARTIFACTS" >&2
cat "$WORK/state.diff" >&2
cat "$WORK/packages.diff" >&2
exit 1
