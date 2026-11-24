# shellcheck shell=bash
# Sourced by the gates that run host, sshkey, ssh_authorized_key and mount on
# localhost/parsedfile-measure-el, after tests/spine/build-image.sh: keeps that
# image in step with the G5 base it is built from (ParsedFile-types design
# §10.4).
#
# The image adds openssh only, so its openvox must be the base's. A base
# rebuilt since (openvox-agent is installed unversioned) would leave the
# derived image applying with an older oracle than G5's other halves: so the
# image is rebuilt whenever its `puppet --version` differs from the base's or
# it has no openssh, and checked again after the build. A cold build needs the
# network, for dnf.

PARSEDFILE_CONTAINERFILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../images" && pwd)/Containerfile.parsedfile-types"

# parsedfile_image_identity IMAGE: prints IMAGE's puppet version and `rpm -q
# openssh`'s output, one per line. Fails when either command fails, so an
# image without openssh has no identity.
parsedfile_image_identity() {
  podman run --rm "$1" sh -c '/opt/puppetlabs/bin/puppet --version &&
    rpm -q openssh' 2>/dev/null
}

# parsedfile_image GATE BASE IMAGE LOG
#
# Builds IMAGE from Containerfile.parsedfile-types with build_image, writing
# podman's output to LOG, when IMAGE is missing, has no openssh, or its puppet
# version differs from BASE's. Returns 2, the message prefixed with GATE, when
# BASE's version cannot be read, the build fails, or the built image still
# differs.
parsedfile_image() {
  local gate="$1" base="$2" image="$3" log="$4" want got
  want="$(podman run --rm "$base" /opt/puppetlabs/bin/puppet --version 2>/dev/null)" || {
    echo "$gate: cannot read openvox's version from $base" >&2
    return 2
  }
  if podman image exists "$image"; then
    got="$(parsedfile_image_identity "$image")" && [ "${got%%$'\n'*}" = "$want" ] && return 0
    echo "$gate: $image does not carry $base's openvox and openssh; rebuilding"
  else
    echo "$gate: building $image"
  fi
  build_image "$gate" "$image" "$PARSEDFILE_CONTAINERFILE" "${PARSEDFILE_CONTAINERFILE%/*}" "$log" || return 2
  if ! got="$(parsedfile_image_identity "$image")" || [ "${got%%$'\n'*}" != "$want" ]; then
    echo "$gate: the rebuilt $image still differs from $base:" >&2
    printf '%s\n' "$base:" "$want" "$image:" "${got:-unreadable}" >&2
    return 2
  fi
}
