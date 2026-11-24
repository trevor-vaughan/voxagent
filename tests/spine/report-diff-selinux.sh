#!/usr/bin/env bash
# Compare the reports Ruby Puppet and voxagent produce for `file`'s SELinux
# contexts from identical catalog bytes, on a VM whose SELinux is enforcing.
#
# No other gate can be the oracle for the enabled path. The host and every
# container here see SELinux as disabled (no read-write selinuxfs), and with
# SELinux disabled both agents make every sel* property a no-op, so G1 compares
# nothing about labels. This gate boots a CentOS Stream 10 cloud image under
# QEMU, from a Fedora container (tests/spine/selinux-vm/Containerfile): there is
# no /dev/kvm and RHEL's qemu-kvm has no TCG, so the VM is emulated. cloud-init
# (selinux-vm/user-data) installs openvox 8.29.0, libselinux-utils and
# dosfstools; the image itself ships SELinux enforcing.
#
# ONE VM, BOTH AGENTS, like G1 and unlike the container gates: the VM costs
# minutes to boot, and a label is reset as exactly as a file is. reset.sh below
# rebuilds every fixture path and sets every seeded label with chcon before
# each side, and the labels it left are read and compared between the sides:
# a seed that drifted would let both agents agree about different questions.
#
# The catalog is compiled on the host by Puppet 8.10.0, as G1 compiles its
# own, and the same bytes are applied by openvox inside the VM and by
# voxagent. Besides the reports, the label every fixture path ends with is
# compared between the sides: two reports can agree while both agents left
# the wrong labels.
#
# The base image is pinned by file name and the SHA256 CentOS publishes for
# it, and is cached outside the tree (it is 1 GiB); every run verifies it.
#
#   0  the reports, the walk order and the resulting labels agree outside the
#      allowlist
#   1  they diverge -- a result about voxagent
#   2  the comparison could not run
set -uo pipefail
# A gate owns its work directory outright; see report-diff-systemd.sh.
set +C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
PUPPET="${PUPPET:-puppet}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine}"
IMAGE="${IMAGE:-localhost/voxagent-selinux-vm:latest}"
CACHE="${SELINUX_VM_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/voxagent/selinux-vm}"

VM_IMAGE=CentOS-Stream-GenericCloud-10-20260930.0.x86_64.qcow2
VM_IMAGE_URL="https://cloud.centos.org/centos/10-stream/x86_64/images/$VM_IMAGE"
VM_IMAGE_SHA256=42bd0c8f58574c25a549f134887936347d3e38e3401a20388fad532db1a243c0

# user-data installs exactly this version; reading it back is the tripwire for
# a repository that served something else.
WANT_PUPPET="${WANT_PUPPET:-8.29.0}"

# Measured 2026-10-06 on an AMD Ryzen 9 5900X under TCG: SSH answered after
# 85-101 s and cloud-init finished at 238-283 s. The deadline bounds both waits.
BOOT_TIMEOUT=1800

NAME="selinux-vm-$$"

command -v podman >/dev/null 2>&1 || {
  echo "report-diff-selinux: podman is not available" >&2
  exit 2
}
command -v "$PUPPET" >/dev/null 2>&1 || {
  echo "report-diff-selinux: $PUPPET is not on PATH; the catalog cannot be compiled" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff-selinux "$ROOT" "$VOXAGENT" "$GATEKIT"
# shellcheck source=build-image.sh
. "$HERE/build-image.sh" || exit 2

WORK="$(mktemp -d)" || exit 2
trap 'podman rm -f "$NAME" >/dev/null 2>&1; rm -rf "$WORK"' EXIT

if ! podman image exists "$IMAGE"; then
  echo "report-diff-selinux: building $IMAGE"
  build_image report-diff-selinux "$IMAGE" "$HERE/selinux-vm/Containerfile" \
    "$HERE/selinux-vm" "$WORK/build.log" || exit 2
fi

mkdir -p "$CACHE" || exit 2
if [ ! -f "$CACHE/$VM_IMAGE" ]; then
  echo "report-diff-selinux: downloading $VM_IMAGE"
  curl -fsSL -o "$CACHE/$VM_IMAGE.part" "$VM_IMAGE_URL" || {
    echo "report-diff-selinux: could not download $VM_IMAGE_URL" >&2
    rm -f "$CACHE/$VM_IMAGE.part"
    exit 2
  }
  mv "$CACHE/$VM_IMAGE.part" "$CACHE/$VM_IMAGE" || exit 2
fi
echo "$VM_IMAGE_SHA256  $CACHE/$VM_IMAGE" | sha256sum -c --quiet - || {
  echo "report-diff-selinux: $CACHE/$VM_IMAGE does not match its pinned SHA256;" >&2
  echo "report-diff-selinux: delete it to download it again" >&2
  exit 2
}

# The fixture. What each resource exercises (seeded labels are reset.sh's):
#   mis        file seeded unconfined_u:...:tmp_t: relabelled to its default,
#              one event for seluser and one for seltype.
#   typed      declared seltype over the default type.
#   ign        selinux_ignore_defaults: seeded tmp_t, left alone.
#   ign-typed  selinux_ignore_defaults with a declared seltype: only seltype.
#   new        a created file: labelled with its default, silently.
#   suid       a created 4755 file owned by nobody: the chown clears S_ISUID
#              and property_fix's mode chmods it back, silently, so the
#              listing shows 4755 (type/file.rb:1112-1121).
#   rewrite    seeded tmp_t and rewritten by content: the content write's
#              relabel against the seltype property's own event.
#   dir        a mislabelled directory; newdir a created one.
#   lnm        an existing link seeded tmp_t, pointing at lnreal (also tmp_t):
#              the link is relabelled and its target is not.
#   lntyped    a declared seltype on an existing link, links => manage: the
#              link takes it, lnreal does not.
#   ln-new     a created link.
#   badtype    a type the loaded policy does not have: the kernel refuses the
#              relabel with EINVAL (measured with chcon), which is I-4's
#              warning with a successful event.
#   tree       recursion: its children, seeded tmp_t, take batched defaults.
#              tree/ln, a symlink child also seeded tmp_t, is relabelled
#              itself, never through to tree/a, which is relabelled as its
#              own child (D-28, retired; Puppet reports `seltype changed
#              'tmp_t' to 'etc_t'` on the link). owner, group and mode are
#              declared over a tree seeded root:root: every link, tree/ln and
#              tree/lnout (to lnreal, outside the tree), is lchowned to nobody
#              with no mode event, and lnreal stays root:root, which the
#              listing compares. The files and directories are seeded at a
#              mode out of sync (0600, 0700), so each gets owner, group and
#              mode events, in Puppet's order (type/file.rb:1133-1135).
#   /opt/voxgate/etc       absent, content only (I-11): looked up with mode 0,
#              which matches `/opt/(.*/)?etc -d` (etc_t), where a regular file
#              there is usr_t.
#   /opt/voxgate/file/etc  the same path shape with ensure => file: usr_t.
#   /mnt/voxlnk  ensure => file over a symlink (I-10): looked up as a link
#              (mnt_t), where a regular file there is default_t.
#   /mnt/voxvfat/*  vfat has no label support: declared seltype is in sync,
#              on a created file and an existing one.
cat >"$WORK/site.pp" <<'EOF' || exit 2
node default {
  file { '/etc/voxgate/mis': ensure => file }
  file { '/etc/voxgate/typed': ensure => file, seltype => 'httpd_sys_content_t' }
  file { '/etc/voxgate/ign': ensure => file, selinux_ignore_defaults => true }
  file { '/etc/voxgate/ign-typed': ensure => file, selinux_ignore_defaults => true, seltype => 'httpd_sys_content_t' }
  file { '/etc/voxgate/new': ensure => file, content => "new\n" }
  file { '/etc/voxgate/suid': ensure => file, content => "suid\n", owner => 'nobody', mode => '4755' }
  file { '/etc/voxgate/rewrite': ensure => file, content => "new\n" }
  file { '/etc/voxgate/dir': ensure => directory }
  file { '/etc/voxgate/newdir': ensure => directory }
  file { '/etc/voxgate/lnm': ensure => link, target => 'lnreal' }
  file { '/etc/voxgate/lntyped': seltype => 'httpd_sys_content_t' }
  file { '/etc/voxgate/ln-new': ensure => link, target => 'lnreal' }
  file { '/etc/voxgate/badtype': ensure => file, seltype => 'voxagent_no_such_t' }
  file { '/etc/voxgate/tree': ensure => directory, recurse => true, owner => 'nobody', group => 'nobody', mode => '0640' }
  file { '/opt/voxgate/etc': content => "i11\n" }
  file { '/opt/voxgate/file/etc': ensure => file, content => "i11\n" }
  file { '/mnt/voxlnk': ensure => file, content => "i10\n" }
  file { '/mnt/voxvfat/new': ensure => file, content => "v\n", seltype => 'etc_t' }
  file { '/mnt/voxvfat/old': ensure => file, seltype => 'etc_t' }
}
EOF

# Compiled on the host, as G1 compiles: no path in it needs to exist here.
mkdir -p "$WORK"/{code/environments/production/manifests,conf,var} || exit 2
cp "$WORK/site.pp" "$WORK/code/environments/production/manifests/site.pp" || exit 2
"$PUPPET" catalog find spine.example --terminus compiler --render-as json \
  --node_name_value spine.example \
  --environmentpath "$WORK/code/environments" --environment production \
  --confdir "$WORK/conf" --vardir "$WORK/var" --codedir "$WORK/code" \
  2>"$WORK/compile.err" | sed -n '/^{/,$p' >"$WORK/catalog.json"
[ -s "$WORK/catalog.json" ] || {
  echo "report-diff-selinux: compiling the fixture catalog failed" >&2
  sed -n '1,10p' "$WORK/compile.err" >&2
  exit 2
}

# reset.sh: the starting state, rebuilt from nothing before each side. Every
# seeded path gets an explicit mode and a full context, because what root
# creates over SSH is labelled unconfined_u and inherits its parent's type --
# neither of which the fixture should depend on. set -e makes any refused
# seed a failed reset.
cat >"$WORK/reset.sh" <<'EOF' || exit 2
set -eu
umask 022
if mountpoint -q /mnt/voxvfat; then umount /mnt/voxvfat; fi
rm -rf /etc/voxgate /opt/voxgate /mnt/voxlnk /mnt/voxvfat /var/tmp/voxvfat.img
mkdir -p /etc/voxgate/tree/sub /etc/voxgate/dir /opt/voxgate/file /mnt/voxvfat
for f in mis typed ign ign-typed badtype lnreal; do echo seed > "/etc/voxgate/$f"; done
echo old > /etc/voxgate/rewrite
echo a > /etc/voxgate/tree/a
echo b > /etc/voxgate/tree/sub/b
ln -s a /etc/voxgate/tree/ln
ln -s ../lnreal /etc/voxgate/tree/lnout
chown -hR 0:0 /etc/voxgate
chmod 0700 /etc/voxgate/tree /etc/voxgate/tree/sub
chmod 0600 /etc/voxgate/tree/a /etc/voxgate/tree/sub/b
ln -s lnreal /etc/voxgate/lnm
ln -s lnreal /etc/voxgate/lntyped
ln -s voxlnk-target /mnt/voxlnk
chcon -h system_u:object_r:etc_t:s0 /etc/voxgate /etc/voxgate/typed /etc/voxgate/badtype \
  /etc/voxgate/lntyped /etc/voxgate/tree /etc/voxgate/tree/lnout
chcon -h unconfined_u:object_r:tmp_t:s0 /etc/voxgate/mis
chcon -h system_u:object_r:tmp_t:s0 /etc/voxgate/ign /etc/voxgate/ign-typed \
  /etc/voxgate/rewrite /etc/voxgate/dir /etc/voxgate/lnreal /etc/voxgate/lnm \
  /etc/voxgate/tree/a /etc/voxgate/tree/ln /etc/voxgate/tree/sub /etc/voxgate/tree/sub/b
chcon -h system_u:object_r:usr_t:s0 /opt/voxgate /opt/voxgate/file
chcon -h system_u:object_r:mnt_t:s0 /mnt/voxvfat /mnt/voxlnk
truncate -s 8M /var/tmp/voxvfat.img
mkfs.vfat /var/tmp/voxvfat.img >/dev/null
mount -o loop /var/tmp/voxvfat.img /mnt/voxvfat
echo seed > /mnt/voxvfat/old
EOF

# report.sh SIDE: print SIDE's report as JSON. Ruby's was captured by the VM's
# openvox through tests/spine/lib/puppet/reports/gatejson.rb, as the container
# gates capture it. These are files rather than `sh -c` arguments because ssh
# joins its arguments into one string for the remote shell, which would
# re-split them.
cat >"$WORK/report.sh" <<'EOF' || exit 2
set -eu
case "$1" in
  ruby)
    cat /tmp/gate/ruby/gate.json
    ;;
  go)
    g="$(find /tmp/gate/go -name '*.json' | head -1)"
    [ -n "$g" ] || exit 3
    cat "$g"
    ;;
  *) exit 3 ;;
esac
EOF

# labels.sh: one `path type mode owner:group context` line per fixture path,
# sorted, never following a link.
cat >"$WORK/labels.sh" <<'EOF' || exit 2
set -eu
find /etc/voxgate /opt/voxgate /mnt/voxlnk /mnt/voxvfat -printf '%p %y %m %u:%g %Z\n' | LC_ALL=C sort
EOF

# A key for this run only. Generated in the image, where ssh-keygen is
# installed, and substituted into the seed.
podman run --rm -v "$WORK:/w" "$IMAGE" \
  ssh-keygen -q -t ed25519 -N '' -C voxagent-selinux-gate -f /w/id_gate \
  >"$WORK/keygen.err" 2>&1 || {
  echo "report-diff-selinux: could not generate the gate's SSH key:" >&2
  sed -n '1,10p' "$WORK/keygen.err" >&2
  exit 2
}
mkdir -p "$WORK/seed" || exit 2
key="$(cat "$WORK/id_gate.pub")" || exit 2
sed "s|@GATE_SSH_KEY@|$key|" "$HERE/selinux-vm/user-data" >"$WORK/seed/user-data" \
  && cp "$HERE/selinux-vm/meta-data" "$WORK/seed/meta-data" || exit 2
podman run --rm -v "$CACHE:/c:ro" -v "$WORK:/w" "$IMAGE" sh -c '
  genisoimage -quiet -output /w/seed.iso -volid cidata -joliet -rock \
    /w/seed/user-data /w/seed/meta-data &&
  qemu-img create -q -f qcow2 -F qcow2 -b /c/'"$VM_IMAGE"' /w/disk.qcow2 20G
' >"$WORK/prepare.err" 2>&1 || {
  echo "report-diff-selinux: could not build the seed ISO and the VM's disk:" >&2
  sed -n '1,10p' "$WORK/prepare.err" >&2
  exit 2
}

# The base image is mounted read-only; QEMU writes only the overlay in $WORK.
# SSH is forwarded to the container's own loopback, and only the gate's
# `podman exec` reaches it.
boot_start=$SECONDS
podman run -d --name "$NAME" -v "$CACHE:/c:ro" -v "$WORK:/w" "$IMAGE" \
  qemu-system-x86_64 -accel tcg -cpu max -m 2048 -smp 2 \
  -display none -monitor none -serial file:/w/console.log \
  -drive file=/w/disk.qcow2,if=virtio,format=qcow2 \
  -drive file=/w/seed.iso,if=virtio,format=raw,readonly=on \
  -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:22 \
  -device virtio-net-pci,netdev=n0 \
  >/dev/null 2>"$WORK/run.err" || {
  echo "report-diff-selinux: could not start QEMU:" >&2
  sed -n '1,10p' "$WORK/run.err" >&2
  exit 2
}

SSH_OPTS=(-i /w/id_gate -o BatchMode=yes -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10)

# vm CMD...: run CMD in the VM as the gate user.
vm() {
  podman exec -i "$NAME" ssh "${SSH_OPTS[@]}" -p 2222 gate@127.0.0.1 "$@"
}

# vm_put FILE...: copy files from $WORK to the VM's /tmp/gate.
vm_put() {
  local f args=()
  for f in "$@"; do args+=("/w/$f"); done
  podman exec "$NAME" scp "${SSH_OPTS[@]}" -P 2222 "${args[@]}" gate@127.0.0.1:/tmp/gate/
}

# console_tail: what the VM printed last, for a boot that did not finish.
console_tail() {
  tail -c 2000 "$WORK/console.log" 2>/dev/null | tr -d '\r' >&2
}

deadline=$((SECONDS + BOOT_TIMEOUT))
until vm true >/dev/null 2>&1; do
  if [ "$(podman inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)" != true ]; then
    echo "report-diff-selinux: QEMU exited before the VM answered SSH:" >&2
    podman logs "$NAME" 2>&1 | tail -10 >&2
    console_tail
    exit 2
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "report-diff-selinux: the VM did not answer SSH within ${BOOT_TIMEOUT}s" >&2
    console_tail
    exit 2
  fi
  sleep 10
done
ssh_after=$((SECONDS - boot_start))
left=$((deadline - SECONDS))
[ "$left" -gt 0 ] || left=1
if ! timeout "$left" podman exec -i "$NAME" ssh "${SSH_OPTS[@]}" -p 2222 gate@127.0.0.1 \
  sudo cloud-init status --wait >"$WORK/cloud-init.out" 2>&1; then
  echo "report-diff-selinux: cloud-init did not finish cleanly:" >&2
  vm sudo cloud-init status --long 2>&1 | sed -n '1,20p' >&2
  vm sudo tail -20 /var/log/cloud-init-output.log 2>&1 >&2
  exit 2
fi
echo "report-diff-selinux: VM answered SSH after ${ssh_after}s, provisioned after $((SECONDS - boot_start))s"

got="$(vm getenforce 2>/dev/null)"
[ "$got" = Enforcing ] || {
  echo "report-diff-selinux: the VM's SELinux is '${got:-unreadable}', not Enforcing;" >&2
  echo "report-diff-selinux: the gate would compare the disabled path G1 already covers" >&2
  exit 2
}
got="$(vm /opt/puppetlabs/bin/puppet --version 2>/dev/null)"
[ "$got" = "$WANT_PUPPET" ] || {
  echo "report-diff-selinux: the VM carries puppet '${got:-none}' but the oracle is" >&2
  echo "report-diff-selinux: pinned at $WANT_PUPPET; refusing to compare against an" >&2
  echo "report-diff-selinux: unpinned reference" >&2
  exit 2
}

cp "$VOXAGENT" "$WORK/voxagent" && cp "$HERE/lib/puppet/reports/gatejson.rb" "$WORK/" || exit 2
if ! vm mkdir -p /tmp/gate/lib/puppet/reports >/dev/null 2>&1 \
  || ! vm_put voxagent catalog.json reset.sh labels.sh report.sh gatejson.rb >/dev/null 2>"$WORK/put.err" \
  || ! vm mv /tmp/gate/gatejson.rb /tmp/gate/lib/puppet/reports/ >/dev/null 2>&1 \
  || ! vm chmod +x /tmp/gate/voxagent >/dev/null 2>&1; then
  echo "report-diff-selinux: could not copy the fixture and $VOXAGENT into the VM:" >&2
  sed -n '1,10p' "$WORK/put.err" >&2
  exit 2
fi

# seed SIDE: reset the fixture and record the labels it starts from.
seed() {
  if ! vm sudo sh /tmp/gate/reset.sh >"$WORK/reset-$1.err" 2>&1 \
    || ! vm sudo sh /tmp/gate/labels.sh >"$WORK/seed-$1.labels" 2>>"$WORK/reset-$1.err" \
    || [ ! -s "$WORK/seed-$1.labels" ]; then
    echo "report-diff-selinux: could not reset the fixture before the $1 side:" >&2
    sed -n '1,20p' "$WORK/reset-$1.err" >&2
    return 2
  fi
}

seed ruby || exit 2
# The apply's exit status is not consulted; report-diff.sh records why the
# stored report, not the process exit, is the oracle.
vm sudo /opt/puppetlabs/bin/puppet apply --catalog /tmp/gate/catalog.json \
  --report --reports gatejson --libdir /tmp/gate/lib --reportdir /tmp/gate/ruby >"$WORK/ruby.out" 2>&1
vm sudo sh /tmp/gate/report.sh ruby >"$WORK/ruby.json" 2>"$WORK/convert.err"
if [ ! -s "$WORK/ruby.json" ]; then
  echo "report-diff-selinux: Ruby wrote no usable report; there is nothing to" >&2
  echo "report-diff-selinux: compare against" >&2
  sed -n '1,20p' "$WORK/convert.err" >&2
  sed -n '1,20p' "$WORK/ruby.out" >&2
  exit 2
fi
vm sudo sh /tmp/gate/labels.sh >"$WORK/ruby.labels" 2>/dev/null

seed go || exit 2
if ! diff "$WORK/seed-ruby.labels" "$WORK/seed-go.labels" >"$WORK/seed.diff"; then
  echo "report-diff-selinux: the two sides did not start from the same labels:" >&2
  grep '^[<>]' "$WORK/seed.diff" >&2
  exit 2
fi
vm sudo /tmp/gate/voxagent apply --catalog /tmp/gate/catalog.json \
  --report-dir /tmp/gate/go >"$WORK/go.out" 2>&1
vm sudo sh /tmp/gate/report.sh go >"$WORK/go.json" 2>"$WORK/go.err"
if [ ! -s "$WORK/go.json" ]; then
  echo "report-diff-selinux: voxagent wrote no report" >&2
  sed -n '1,20p' "$WORK/go.err" >&2
  sed -n '1,20p' "$WORK/go.out" >&2
  exit 2
fi
vm sudo sh /tmp/gate/labels.sh >"$WORK/go.labels" 2>/dev/null

# keep_evidence: the reports, the catalog and both label listings. Kept on
# every run that got this far, not only on a divergence: a run costs minutes,
# and the reference labels are what docs/maintainers/apply-spine.md cites.
keep_evidence() {
  local f
  mkdir -p "$EVIDENCE" || return 1
  for f in ruby.json go.json catalog.json seed-ruby.labels ruby.labels go.labels ruby.out go.out; do
    if [ -f "$WORK/$f" ]; then
      cp "$WORK/$f" "$EVIDENCE/selinux-$f" || return 1
    fi
  done
  echo "report-diff-selinux: the reports, the catalog and the labels are in $EVIDENCE (selinux-*)"
}
keep_evidence || exit 2

# Prove the fixture reached the enabled path before trusting the comparison:
# with SELinux seen as disabled both agents would produce no sel* event and
# agree perfectly.
for want in '"seltype_changed"' '"seluser_changed"' 'Failed to set SELinux context' '"owner_changed"' '"mode_changed"'; do
  grep -qF "$want" "$WORK/ruby.json" || {
    echo "report-diff-selinux: the reference run did not exercise the SELinux" >&2
    echo "report-diff-selinux: fixture: $want is missing from Ruby's report" >&2
    sed -n '1,20p' "$WORK/ruby.out" >&2
    exit 2
  }
done
for side in ruby go; do
  [ -s "$WORK/$side.labels" ] || {
    echo "report-diff-selinux: could not read the labels the $side side left" >&2
    exit 2
  }
done

"$GATEKIT" compare-reports \
  "$WORK/ruby.json" "$WORK/go.json" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0 | 1) ;;
  *)
    echo "report-diff-selinux: the comparison could not run" >&2
    exit 2
    ;;
esac
# The walk order: compare-reports matches statuses by ref and logs as a
# multiset, so it cannot see the order resources were evaluated in.
# compare-order reads it from each report's log order.
"$GATEKIT" compare-order "$WORK/ruby.json" "$WORK/go.json"
case $? in
  0) ;;
  1)
    echo "report-diff-selinux: the walk order diverges" >&2
    rc=1
    ;;
  *)
    echo "report-diff-selinux: the walk order could not be compared" >&2
    exit 2
    ;;
esac
if ! diff "$WORK/ruby.labels" "$WORK/go.labels" >"$WORK/labels.diff"; then
  echo "report-diff-selinux: the resulting labels differ (< ruby, > voxagent):" >&2
  grep '^[<>]' "$WORK/labels.diff" >&2
  rc=1
fi

case "$rc" in
  0) echo "report-diff-selinux: reports agree outside the allowlist" ;;
  1) echo "report-diff-selinux: reports diverge" >&2 ;;
esac
exit "$rc"
