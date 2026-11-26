#!/usr/bin/env bash
# Regenerates munge.json from openvox's own Ruby. Needs podman and the
# localhost/fleet-node-el:latest image (`task fleet:images`, built from
# fleet/images/Dockerfile.el), whose openvox must be 8.34.0, the oracle.
set -euo pipefail
dir="$(cd "$(dirname "$0")" && pwd)"
image=localhost/fleet-node-el:latest
want=8.34.0
got="$(podman run --rm "$image" /opt/puppetlabs/bin/puppet --version)" || {
  echo "$0: cannot read openvox's version from $image; build it with task fleet:images" >&2
  exit 1
}
if [ "$got" != "$want" ]; then
  echo "$0: $image carries openvox $got, not $want; rebuild it with task fleet:images FORCE=1" >&2
  exit 1
fi
podman run --rm -v "$dir":/g:Z "$image" \
  /opt/puppetlabs/puppet/bin/ruby /g/munge.rb > "$dir/munge.json.tmp"
mv "$dir/munge.json.tmp" "$dir/munge.json"
