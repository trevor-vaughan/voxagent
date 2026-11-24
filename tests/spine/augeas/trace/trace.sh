#!/usr/bin/env bash
# Records the libaugeas calls Ruby makes for an augeas case (augeas design
# §10). For each case directory (tests/spine/augeas/cases/<case>/: site.pp,
# optionally seed.sh and lenses), it compiles site.pp into catalog.json as G5's
# augeas half does (augeas_compile in ../g5.sh, in a container of its own),
# then applies that catalog once, live, with aug-trace.so preloaded, in a
# fresh container set up as augeas_side in g5.sh sets up Ruby's side, so:
#   - the libdir /tmp/lib is built as g5.sh builds it: tests/spine/lib, the
#     lib/puppet trees of augeasproviders_core, _sysctl and _ssh and augeas_core's
#     lib from the SIMP module cache, checked against the routing
#     fingerprints, plus augeas/lenses/ holding the lenses the case's `lenses`
#     file lists (or the files of its `lenses` directory), when it has one;
#   - the apply names no --environmentpath or --environment, so the types
#     load from that libdir alone and ruby-augeas's aug_init gets "" as
#     its loadpath, as under the gate (the compile step keeps its environment);
#   - /tmp/sysctl-shim.sh and /tmp/papply are staged as g5.sh stages them, so a
#     seed that installs the shim or puts its target in sync runs as it does
#     under the gate;
#   - seed.sh, when the case has one, has run;
#   - the case's optional `podman-args` file, whitespace-split into extra
#     `podman run` arguments (e.g. a --cap-add). It is word-split unquoted on
#     purpose, so it must be a repo-controlled file, never untrusted input.
# puppet's wrapper script unsets LD_PRELOAD, so the traced apply runs the real
# binary; the seed's papply runs the wrapper and is not traced. Writes
# $ROOT/.test-output/augeas-trace/<case>/catalog.json and aug-trace.txt, and
# exits 2 when the trace holds no `aug_init "/" "" 64|32` for the type's handle
# (a seed that failed, or an apply that loaded a lens path).
#
#   trace.sh <aug-trace.so> <case-dir>...
#
# Build the interposer on a glibc-compatible host first:
#   gcc -shared -fPIC -O1 -o aug-trace.so aug-trace.c -ldl
set -uo pipefail
SO="$(cd "$(dirname "${1:?usage: trace.sh <aug-trace.so> <case-dir>...}")" && pwd)/$(basename "$1")" || exit 2
shift
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)" || exit 2
IMAGE="${IMAGE:-localhost/augeas-measure-el:latest}"
CACHE="$(echo "$ROOT"/.test-output/simp/modules-*)"
MODS="augeas_core augeasproviders_core augeasproviders_sysctl augeasproviders_ssh augeasproviders_grub"
[ -d "$CACHE" ] || {
  echo "trace.sh: no SIMP module cache at $CACHE" >&2
  exit 2
}
# shellcheck source=../../simp/lib.sh
. "$ROOT/tests/spine/simp/lib.sh" || exit 2
# modules NAME: an empty production environment in container NAME holding $MODS.
modules() {
  local m
  podman exec "$1" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules || return 2
  for m in $MODS; do podman cp "$CACHE/$m" "$1:/tmp/e/production/modules/$m" || return 2; done
}
for c in "$@"; do
  CASE="$(cd "$c" && pwd)" || exit 2
  OUT="$ROOT/.test-output/augeas-trace/$(basename "$CASE")"
  CN="augtrace-$$"
  mkdir -p "$OUT" || exit 2
  podman run --name "$CN-compile" --hostname spine.example --detach "$IMAGE" sleep 600 >/dev/null || exit 2
  modules "$CN-compile" || exit 2
  podman cp "$CASE/site.pp" "$CN-compile:/tmp/e/production/manifests/site.pp" || exit 2
  podman exec "$CN-compile" sh -c '
    /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
      --render-as json --node_name_value spine.example \
      --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
    sed -n "/^{/,\$p"' >"$OUT/catalog.json"
  if [ ! -s "$OUT/catalog.json" ]; then
    echo "trace.sh: compiling $CASE/site.pp failed:" >&2
    podman exec "$CN-compile" cat /tmp/compile.err >&2
    podman rm -f -t 0 "$CN-compile" >/dev/null
    exit 2
  fi
  podman rm -f -t 0 "$CN-compile" >/dev/null
  LIB="$(mktemp -d)" || exit 2
  mkdir -p "$LIB/puppet" && cp -R "$ROOT/tests/spine/lib/." "$LIB/" || exit 2
  for m in augeasproviders_core augeasproviders_sysctl augeasproviders_ssh; do
    cp -R "$CACHE/$m/lib/puppet/." "$LIB/puppet/" || exit 2
  done
  cp -R "$CACHE/augeas_core/lib/." "$LIB/" || exit 2
  augeasproviders_fingerprints trace.sh "$LIB" || exit 2
  if [ -d "$CASE/lenses" ]; then
    mkdir -p "$LIB/augeas/lenses" && cp "$CASE/lenses"/* "$LIB/augeas/lenses/" || exit 2
  elif [ -f "$CASE/lenses" ]; then
    mkdir -p "$LIB/augeas/lenses" || exit 2
    while read -r l; do
      [ -z "$l" ] || cp "$CACHE/$l" "$LIB/augeas/lenses/" || exit 2
    done <"$CASE/lenses"
  fi
  PARGS=""
  [ -f "$CASE/podman-args" ] && PARGS="$(cat "$CASE/podman-args")"
  # shellcheck disable=SC2086
  podman run --name "$CN" --hostname spine.example $PARGS --detach "$IMAGE" sleep 600 >/dev/null || exit 2
  podman cp "$LIB" "$CN:/tmp/lib" || exit 2
  rm -rf "$LIB"
  podman cp "$ROOT/tests/spine/augeas/sysctl-shim.sh" "$CN:/tmp/sysctl-shim.sh" || exit 2
  podman exec "$CN" sh -c "printf '%s\n' '#!/bin/sh' \
    'exec /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --libdir /tmp/lib --color=false \"\$@\"' \
    >/tmp/papply && chmod +x /tmp/papply" || exit 2
  if [ -f "$CASE/seed.sh" ]; then podman cp "$CASE/seed.sh" "$CN:/tmp/seed.sh" || exit 2; fi
  podman cp "$OUT/catalog.json" "$CN:/tmp/catalog.json" || exit 2
  podman cp "$SO" "$CN:/tmp/aug-trace.so" || exit 2
  podman exec "$CN" sh -c '{ [ ! -f /tmp/seed.sh ] || sh /tmp/seed.sh >/dev/null 2>&1; } &&
    LD_PRELOAD=/tmp/aug-trace.so AUGTRACE=/tmp/aug-trace.txt /opt/puppetlabs/puppet/bin/puppet apply \
      --catalog /tmp/catalog.json --libdir /tmp/lib --color=false >/dev/null 2>&1; cat /tmp/aug-trace.txt' >"$OUT/aug-trace.txt"
  podman rm -f -t 0 "$CN" >/dev/null
  if ! grep -Eq '^h[0-9]+ aug_init "/" "" (64|32)$' "$OUT/aug-trace.txt"; then
    echo "trace.sh: $(basename "$CASE"): no aug_init \"/\" \"\" 64|32 in $OUT/aug-trace.txt" >&2
    exit 2
  fi
  echo "$(basename "$CASE"): $(wc -l <"$OUT/aug-trace.txt") calls in $OUT/aug-trace.txt"
done
