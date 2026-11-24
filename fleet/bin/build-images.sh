#!/usr/bin/env bash
# Build the fleet node images.
#
# Building is separated from `fleet:up` because the two have different failure
# modes here. Inside a nested container the kernel refuses to mount an overlay
# whose lower layer is itself an overlay with user xattrs, so `podman build`
# dies with "mounting an overlay over build context directory: ... userxattr:
# invalid argument" before any layer is created. fuse-overlayfs performs the
# same union in userspace, where that restriction does not apply.
#
# The fallback is DETECTED, never assumed. A normal host has working native
# overlay and may not have fuse-overlayfs installed at all, so the plain build
# is always tried first and the override is used only when the plain build
# fails with that specific error. The override is written to a temporary file
# and passed via CONTAINERS_STORAGE_CONF, so no storage.conf is ever committed
# and a normal host never sees one.
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

IMAGES_DIR="${1:?usage: build-images.sh <images-dir> [force]}"
FORCE="${2:-}"

OVERLAY_ERROR='mounting an overlay over build context directory'
FUSE_OVERLAYFS='/usr/bin/fuse-overlayfs'

# image-tag<TAB>dockerfile
IMAGES="fleet-node-el:latest	Dockerfile.el
fleet-node-deb:latest	Dockerfile.deb"

storage_conf=''
build_log="$(mktemp -t fleet-build-XXXXXX.log)"

cleanup() {
  rm -f "$build_log"
  [ -n "$storage_conf" ] && rm -f "$storage_conf"
  return 0
}
trap cleanup EXIT

# Write the storage override once, on first need. Only the mount program is
# overridden: graphroot and runroot are deliberately left at their defaults so
# the build lands in the same image store that `podman-compose` reads without
# the override, and so images pulled earlier stay visible during the build.
ensure_storage_conf() {
  [ -n "$storage_conf" ] && return 0

  if [ ! -x "$FUSE_OVERLAYFS" ]; then
    echo "build-images: native overlay build failed and $FUSE_OVERLAYFS is not installed" >&2
    echo "build-images: install fuse-overlayfs or build on a host with working overlay" >&2
    return 1
  fi

  storage_conf="$(mktemp -t fleet-storage-XXXXXX.conf)"
  cat >"$storage_conf" <<'CONF'
[storage]
driver = "overlay"

[storage.options.overlay]
mount_program = "/usr/bin/fuse-overlayfs"
CONF
}

while IFS=$'\t' read -r tag dockerfile; do
  [ -n "$tag" ] || continue

  if [ -z "$FORCE" ] && podman image exists "$tag"; then
    echo "build-images: $tag already present, skipping"
    continue
  fi

  echo "build-images: building $tag from $dockerfile"
  if podman build -t "$tag" -f "$IMAGES_DIR/$dockerfile" "$IMAGES_DIR" 2>&1 | tee "$build_log"; then
    echo "build-images: $tag built using native overlay"
    continue
  fi

  # Any other failure is a real build error and must not be retried: retrying
  # would bury the cause under a second, identical failure.
  if ! grep -qF "$OVERLAY_ERROR" "$build_log"; then
    echo "build-images: $tag failed to build; see the output above" >&2
    exit 1
  fi

  echo "build-images: native overlay unavailable here, retrying $tag with fuse-overlayfs"
  ensure_storage_conf
  CONTAINERS_STORAGE_CONF="$storage_conf" \
    podman build -t "$tag" -f "$IMAGES_DIR/$dockerfile" "$IMAGES_DIR"
  echo "build-images: $tag built using fuse-overlayfs"
done <<<"$IMAGES"

echo "build-images: done"
