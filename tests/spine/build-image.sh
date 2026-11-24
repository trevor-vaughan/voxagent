# shellcheck shell=bash
# Sourced by the spine gates that build their own image.
#
# Inside a nested container the kernel refuses an overlay over an overlay with
# user xattrs, so `podman build` fails with "mounting an overlay over build
# context directory" (measured, rootless podman 6.1 in this project's
# development container). fuse-overlayfs does the union in userspace and is
# used only for that failure. The storage override text is
# fleet/bin/build-images.sh's, repeated rather than sourced.

# build_image LABEL IMAGE CONTAINERFILE CONTEXT LOG
#
# Builds IMAGE, writing podman's output to LOG. Returns 2, naming LABEL and
# showing the log's tail, when the build fails natively and through
# fuse-overlayfs, or when the native build hit the nested-overlay refusal and
# fuse-overlayfs is not installed.
build_image() {
  local label="$1" image="$2" containerfile="$3" context="$4" log="$5" conf rc
  podman build -t "$image" -f "$containerfile" "$context" >"$log" 2>&1 && return 0
  if ! grep -qF 'mounting an overlay over build context directory' "$log"; then
    echo "$label: building $image failed:" >&2
    tail -20 "$log" >&2
    return 2
  fi
  if [ ! -x /usr/bin/fuse-overlayfs ]; then
    echo "$label: native overlay build failed and /usr/bin/fuse-overlayfs is not installed" >&2
    echo "$label: install fuse-overlayfs or build on a host with working overlay" >&2
    return 2
  fi
  conf="$(mktemp -t spine-storage-XXXXXX.conf)" || return 2
  cat >"$conf" <<'CONF'
[storage]
driver = "overlay"

[storage.options.overlay]
mount_program = "/usr/bin/fuse-overlayfs"
CONF
  CONTAINERS_STORAGE_CONF="$conf" podman build -t "$image" \
    -f "$containerfile" "$context" >"$log" 2>&1
  rc=$?
  rm -f "$conf"
  [ "$rc" -eq 0 ] && return 0
  echo "$label: building $image failed, also with fuse-overlayfs:" >&2
  tail -20 "$log" >&2
  return 2
}
