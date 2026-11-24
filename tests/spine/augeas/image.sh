# shellcheck shell=bash
# Sourced by tests/spine/augeas/g5.sh and scripts/augeas-lib-test.sh, after
# tests/spine/build-image.sh: keeps localhost/augeas-measure-el in step with
# the G5 base it is built from (augeas design §10).
#
# The image adds packages only, so its openvox and libaugeas must be the
# base's. A base rebuilt since (openvox-agent is installed unversioned) would
# leave the derived image applying with an older oracle than G5's other
# halves, and the native routing measures that very libaugeas: so the image is
# rebuilt whenever its `puppet --version` or libaugeas sha256 differs from the
# base's, and checked again after the build. A cold build needs the network,
# for dnf.

AUGEAS_CONTAINERFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../images" && pwd)/Containerfile.augeas"

# augeas_image_identity IMAGE: prints IMAGE's puppet version and libaugeas
# sha256, one per line.
augeas_image_identity() {
  podman run --rm "$1" sh -c '/opt/puppetlabs/bin/puppet --version &&
    sha256sum /opt/puppetlabs/puppet/lib/libaugeas.so.0.25.0' 2>/dev/null
}

# augeas_image GATE BASE IMAGE LOG
#
# Builds IMAGE from Containerfile.augeas with build_image, writing podman's
# output to LOG, when IMAGE is missing or its identity differs from BASE's.
# Returns 2, the message prefixed with GATE, when BASE cannot be read, the
# build fails, or the built image still differs.
augeas_image() {
  local gate="$1" base="$2" image="$3" log="$4" want got
  want="$(augeas_image_identity "$base")" || {
    echo "$gate: cannot read openvox's version and libaugeas from $base" >&2
    return 2
  }
  if podman image exists "$image"; then
    got="$(augeas_image_identity "$image")"
    [ "$got" = "$want" ] && return 0
    echo "$gate: $image does not carry $base's openvox and libaugeas; rebuilding"
  else
    echo "$gate: building $image"
  fi
  build_image "$gate" "$image" "$AUGEAS_CONTAINERFILE" "${AUGEAS_CONTAINERFILE%/*}" "$log" || return 2
  got="$(augeas_image_identity "$image")"
  if [ "$got" != "$want" ]; then
    echo "$gate: the rebuilt $image still differs from $base:" >&2
    printf '%s\n' "$base:" "$want" "$image:" "${got:-unreadable}" >&2
    return 2
  fi
}
