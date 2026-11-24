#!/usr/bin/env bash
# Record the state of every path named on stdin, one per line.
#
# This is the PRIMARY instrument for the state-diff, not a supplement to
# `podman diff`. Measured: podman bind-mounts /etc/hosts, /etc/resolv.conf and
# /etc/hostname into every container, so those three are not in the image layer
# and changes to them NEVER appear in a layer diff. A catalog managing
# /etc/hosts is entirely ordinary, and a gate resting on the layer diff alone
# would report no divergence for it while asserting nothing.
#
# Output is one line per path:
#
#   <path>|<type>|<mode>|<uid>|<gid>|<identity>
#
# Pipe-delimited rather than space-delimited, because `stat -c %F` returns
# "regular file" -- two words -- for a file and "directory" for a directory.
# Splitting that on spaces would give a different field count per path type and
# silently shift the mode into the type column for every regular file.
#
# The identity column is what makes two same-shaped paths distinguishable:
# the SHA-256 for a regular file, "-> target" for a symlink, "-" for anything
# with no content of its own. Recording only the symlink's own mode would let
# two links pointing at different targets compare equal, which is a divergence
# this gate exists to catch.
#
# Measured, and the reason for the -e/-L pairing below: GNU stat does NOT
# dereference by default, so a symlink reports "symbolic link"; and a DANGLING
# symlink is `-e` false but `-L` true, so testing -e alone would report it as
# absent when it is present and wrong.
set -uo pipefail

while IFS= read -r path; do
  [ -n "$path" ] || continue

  if [ ! -e "$path" ] && [ ! -L "$path" ]; then
    printf '%s|absent|-|-|-|-\n' "$path"
    continue
  fi

  meta="$(stat -c '%F|%a|%u|%g' "$path" 2>/dev/null)" || {
    printf '%s|UNREADABLE|-|-|-|-\n' "$path"
    continue
  }

  if [ -L "$path" ]; then
    target="$(readlink "$path" 2>/dev/null)"
    if [ -n "$target" ]; then
      identity="-> $target"
    else
      identity="UNREADABLE"
    fi
  elif [ -f "$path" ]; then
    identity="$(sha256sum "$path" 2>/dev/null | cut -d' ' -f1)"
    [ -n "$identity" ] || identity="UNREADABLE"
  else
    identity="-"
  fi

  printf '%s|%s|%s\n' "$path" "$meta" "$identity"
done
