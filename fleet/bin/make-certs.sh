#!/usr/bin/env bash
# Mint the fleet's own CA and the TLS material storeconfigs needs.
#
# The PuppetDB terminus refuses plain HTTP -- config.rb:179 raises
# "PuppetDB 'server_urls' must be https" -- and stage 1 is masterless, so there
# is no Puppet Server CA to issue from. openvox-ca fills that gap offline: a
# CA, one server certificate for OpenVoxDB, and one client certificate per node.
#
# EVERYTHING HAPPENS IN ONE THROWAWAY CONTAINER. openvox-ca needs Go >= 1.27 and
# this host has Go 1.26.5 with GOTOOLCHAIN=local, so it cannot be built here;
# the host also has no openssl, so there is no second way to do the cert work
# either. The container supplies both, plus the wget used for the bootstrap
# probe, and its module and build caches die with it.
#
# The generated material is gitignored. Private keys must never be committed.
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

CERTS_DIR="${1:?usage: make-certs.sh <certs-dir> <openvox-ca-source-dir>}"
SRC_DIR="${2:?usage: make-certs.sh <certs-dir> <openvox-ca-source-dir>}"

GO_IMAGE='docker.io/library/golang:1.27'

# The name on the server certificate, and the names on the client certificates.
# These are certnames, and each node's puppet.conf names the same one.
SERVER_NAME='openvoxdb'
NODE_NAMES=(node-el node-el-alt node-deb)

if [ ! -d "$SRC_DIR" ]; then
  echo "make-certs: $SRC_DIR is missing; run fleet:vendor first" >&2
  exit 1
fi

mkdir -p "$CERTS_DIR"

missing=''
for name in ca.crt ca.crl; do
  [ -f "$CERTS_DIR/$name" ] || missing="$missing $name"
done
for name in "$SERVER_NAME" "${NODE_NAMES[@]}"; do
  [ -f "$CERTS_DIR/$name.crt" ] || missing="$missing $name.crt"
  [ -f "$CERTS_DIR/$name.key" ] || missing="$missing $name.key"
done

if [ -z "$missing" ]; then
  echo "make-certs: certificates already present, skipping"
  exit 0
fi

# Whatever is here now is partial, and a partial cadir cannot be topped up:
# `openvox-ca generate` refuses a certname its inventory already holds, so a
# second pass would die on the certificate the first pass did manage to mint.
# Start from an empty cadir instead. Nothing here is worth preserving -- these
# are fixture certificates -- and the skip above keeps this path rare. Removal
# is by known name, never a blanket wipe of the caller's directory.
echo "make-certs: minting${missing}"
rm -rf "${CERTS_DIR:?}/cadir"
rm -f "$CERTS_DIR"/ca.crt "$CERTS_DIR"/ca.crl
for name in "$SERVER_NAME" "${NODE_NAMES[@]}"; do
  rm -f "$CERTS_DIR/$name.crt" "$CERTS_DIR/$name.key"
done

# The body below runs inside the container, fed on stdin so that it stays
# readable shell rather than a quoted one-liner. It is bash, not sh: the golang
# image's /bin/sh is dash, which has no `pipefail`. The quoted heredoc keeps the
# host shell out of it entirely -- every expansion here is the container's.
#
# Both mounts use :z, the SHARED SELinux label, because both trees are shared
# by design. This container writes the certificates; OpenVoxDB and all three
# nodes then read them, and the vendored source is also read by other tooling.
# :Z would relabel with a private category belonging to this throwaway
# container, so every later consumer would be locked out of the very files this
# script exists to produce -- and podman's own guidance is that :Z on a
# host directory shared with anything else is a mistake.
#
# The CA private key is not protected by the mount label and must not rely on
# it. It is protected by never being mounted into any long-lived container --
# compose.yml mounts individual leaf files, never cadir/ -- and by its 0600
# mode. Those are the real controls.
podman run --rm -i \
  --volume "$SRC_DIR:/src:ro,z" \
  --volume "$CERTS_DIR:/certs:z" \
  --workdir /src \
  --env CGO_ENABLED=0 \
  "$GO_IMAGE" bash -s -- "$SERVER_NAME" "${NODE_NAMES[@]}" <<'INNER'
set -euo pipefail

server="$1"
shift

cadir=/certs/cadir
bin=/tmp/openvox-ca

# CGO_ENABLED=0 comes from the environment: the binary only ever runs in this
# container, but a static build removes any question of glibc version skew.
go build -o "$bin" ./cmd/openvox-ca

# `openvox-ca generate` will not create a CA -- it errors with "no CA exists ...
# refusing to create one". Only the server bootstraps one, so it is started
# once, purely for that side effect, and killed as soon as the CA is on disk.
#
# While it is up its admin API is unauthenticated, because TLS is off and there
# is no client certificate to authenticate with yet. That is why it binds
# 127.0.0.1 inside a container that is about to be destroyed, and why it does
# not outlive the probe. --single-process keeps the signer in the same process
# so that one kill takes the whole CA down; the default splits the key into a
# child that would have to be reaped separately.
"$bin" --cadir "$cadir" \
  --host 127.0.0.1 --port 18140 \
  --no-tls-required --single-process \
  --hostname fleet-ca >/tmp/ca-bootstrap.log 2>&1 &
ca_pid=$!

# Probe with wget: the golang image has it, and it is the readiness signal for
# the CA specifically rather than for the process, since the route only answers
# once the CA certificate exists. It is ready in about three seconds.
ready=0
for _ in $(seq 1 60); do
  if wget --quiet --output-document=/dev/null \
       http://127.0.0.1:18140/puppet-ca/v1/certificate/ca; then
    ready=1
    break
  fi
  sleep 1
done

kill "$ca_pid" 2>/dev/null || true
wait "$ca_pid" 2>/dev/null || true

if [ "$ready" -ne 1 ]; then
  echo "make-certs: the CA did not bootstrap within 60s" >&2
  cat /tmp/ca-bootstrap.log >&2
  exit 1
fi

# --ttl is required, despite the help text reading as though it were optional.
# --dns gives the server certificate the name the nodes dial; localhost is there
# so the same certificate can be probed from inside the OpenVoxDB container.
"$bin" generate --cadir "$cadir" \
  --certname "$server" --dns "$server,localhost" --ttl 8760h \
  --cert-out "/certs/$server.crt" --key-out "/certs/$server.key"

for node in "$@"; do
  "$bin" generate --cadir "$cadir" \
    --certname "$node" --ttl 8760h \
    --cert-out "/certs/$node.crt" --key-out "/certs/$node.key"
done

# Node keys are read by container root, which maps to the file's owner, so 0600
# is enough. Only OpenVoxDB needs group-readable: it runs as uid 64604 gid 0,
# and under rootless podman these files appear to it as root:root, so 0640 is
# the narrowest mode that lets it read its own key.
chmod 0644 /certs/*.crt
chmod 0600 /certs/*.key
chmod 0640 "/certs/$server.key"

# ca.crt and ca.crl are what the skip guard looks for, so they are written LAST
# and with their final mode in one step. Written earlier, a kill in the window
# before the chmod would leave a set the guard calls complete but whose keys
# OpenVoxDB cannot read -- a confusing startup failure instead of a re-mint.
install -m 0644 "$cadir/ca_crl.pem" /certs/ca.crl
install -m 0644 "$cadir/ca_crt.pem" /certs/ca.crt
INNER

echo "make-certs: done"
