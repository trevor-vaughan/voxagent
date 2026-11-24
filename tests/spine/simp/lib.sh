# shellcheck shell=bash
# Sourced by report-diff-simp.sh: the steps of the SIMP acceptance gate that
# touch podman, one function per step. Every function returns 2 on any failure,
# with a message naming the cause, because each failure means the comparison
# could not run, never that the agents disagree.

# The longest each container's systemd is given to finish booting.
SIMP_BOOT_TIMEOUT=180

# simp_modules CACHE IMAGE PIN
#
# Installs every `name version` line of PIN into CACHE and verifies it, unless
# CACHE already verifies, so a warm run needs no network. The install runs in a
# throwaway container of IMAGE with its own openvox, because the host Ruby
# cannot fork under Puppet's tar extraction.
# --ignore-dependencies because PIN is the complete, resolved set: the forge's
# resolver would float the releases PIN fixes. The tree is built beside CACHE
# and moved into place only once it verifies, so an interrupted install is never
# mistaken for a cached one.
simp_modules() {
  local cache="$1" image="$2" pin="$3" partial
  simp_verify_modules "$cache" "$image" "$pin" 2>/dev/null && return 0
  partial="$cache.partial"
  if ! rm -rf "$cache" "$partial" || ! mkdir -p "$partial"; then
    echo "report-diff-simp: could not prepare $partial" >&2
    return 2
  fi
  podman run --rm -v "$partial:/mods:Z" -v "$pin:/pin:ro,Z" "$image" sh -ec '
    while read -r name ver; do
      /opt/puppetlabs/bin/puppet module install "$name" --version "$ver" \
        --ignore-dependencies --modulepath /mods >/dev/null
    done </pin' >"$partial.log" 2>&1 || {
    echo "report-diff-simp: installing the pinned modules failed (the forge needs network):" >&2
    tail -10 "$partial.log" >&2
    return 2
  }
  simp_verify_modules "$partial" "$image" "$pin" || return 2
  rm -f "$partial.log"
  mv "$partial" "$cache" || {
    echo "report-diff-simp: could not move $partial to $cache" >&2
    return 2
  }
}

# simp_verify_modules DIR IMAGE PIN
#
# Returns 0 when the container's `puppet module list` of DIR names every pin of
# PIN at its version and nothing else. Each module line is `── name (vX.Y.Z)`
# under one `/mods` header; a line of any other shape is a failure rather than
# being skipped, so a warning about a broken module cannot pass unseen.
simp_verify_modules() {
  local dir="$1" image="$2" pin="$3" listed
  [ -d "$dir" ] || {
    echo "report-diff-simp: $dir does not exist" >&2
    return 2
  }
  listed="$(podman run --rm -v "$dir:/mods:ro,Z" "$image" \
    /opt/puppetlabs/bin/puppet module list --color=false --modulepath /mods 2>&1)" || {
    echo "report-diff-simp: puppet module list failed on $dir:" >&2
    printf '%s\n' "$listed" | tail -10 >&2
    return 2
  }
  listed="$(printf '%s\n' "$listed" | awk '
    NR == 1 { if ($0 != "/mods") bad = bad "\n" $0; next }
    NF == 3 && $3 ~ /^\(v[^)]+\)$/ { v = $3; gsub(/^\(v|\)$/, "", v); print $2, v; next }
    { bad = bad "\n" $0 }
    END { if (bad != "") { print "unparsed:" bad > "/dev/stderr"; exit 1 } }
  ' | LC_ALL=C sort)" || {
    echo "report-diff-simp: puppet module list of $dir printed lines that are not modules" >&2
    return 2
  }
  if ! diff <(LC_ALL=C sort "$pin") <(printf '%s\n' "$listed") >&2; then
    echo "report-diff-simp: $dir does not hold exactly the pins of $pin (< pinned, > installed)" >&2
    return 2
  fi
}

# simp_libdir MODULES OUT
#
# Merges every module's lib/ under MODULES into OUT, as pluginsync's plugins
# mount does: modules in sorted order, and on a path collision the first module
# wins (cp -n). The collisions: augeas_core
# and augeasproviders_core share puppet/feature/augeas.rb byte for byte, and
# selinux_core's selinux_login type and provider win over vox_selinux's.
simp_libdir() {
  local modules="$1" out="$2" m
  local -a mods
  if ! rm -rf "$out" || ! mkdir -p "$out"; then
    echo "report-diff-simp: could not create $out" >&2
    return 2
  fi
  mapfile -t mods < <(find "$modules" -mindepth 1 -maxdepth 1 -type d | LC_ALL=C sort)
  [ "${#mods[@]}" -gt 0 ] || {
    echo "report-diff-simp: $modules holds no modules" >&2
    return 2
  }
  for m in "${mods[@]}"; do
    [ -d "$m/lib" ] || continue
    cp -Rn "$m/lib/." "$out/" || {
      echo "report-diff-simp: could not merge $m/lib into $out" >&2
      return 2
    }
  done
}

# simp_boot NAME IMAGE
#
# Boots a container of IMAGE named NAME with systemd as PID 1 and waits until
# systemd reports running or degraded. The hostname is simp.example.com on every
# container: the catalog is compiled for that certname, and a report's `host`
# is compared. The wait is report-diff-systemd.sh's, for the reason it records:
# is-system-running --wait prints nothing until the system bus exists.
simp_boot() {
  local name="$1" image="$2" state='' deadline left wait_rc got err
  err="$(podman run -d --systemd=always --hostname simp.example.com --name "$name" \
    "$image" 2>&1 >/dev/null)" || {
    echo "report-diff-simp: could not start $name:" >&2
    printf '%s\n' "$err" | sed -n '1,10p' >&2
    return 2
  }
  deadline=$((SECONDS + SIMP_BOOT_TIMEOUT))
  while [ -z "$state" ]; do
    left=$((deadline - SECONDS))
    [ "$left" -gt 0 ] || break
    state="$(timeout "$left" podman exec "$name" systemctl is-system-running --wait 2>/dev/null)"
    wait_rc=$?
    [ "$wait_rc" -ne 124 ] || break
    [ -n "$state" ] || sleep 1
  done
  case "$state" in
    running | degraded) ;;
    *)
      echo "report-diff-simp: $name reached '${state:-nothing}' within ${SIMP_BOOT_TIMEOUT}s, not running or degraded" >&2
      podman logs "$name" 2>&1 | tail -20 >&2
      echo "report-diff-simp: systemd did not boot; inside a nested container rootless systemd needs a delegated cgroup — see docs/maintainers/apply-spine.md \"Running the systemd gate\"" >&2
      return 2
      ;;
  esac
  got="$(podman exec "$name" cat /proc/1/comm 2>/dev/null)"
  [ "$got" = systemd ] || {
    echo "report-diff-simp: PID 1 in $name is '$got', not systemd" >&2
    return 2
  }
}

# simp_compile NAME ENV OUT
#
# Compiles simp.example.com's catalog in the booted container NAME, with its own
# openvox and its own facts, from ENV (an environmentpath holding production/),
# into OUT/catalog.json; the compiler's stderr goes to OUT/compile.err. The node
# that applies the catalog is the node it is compiled for: a host compile leaks
# the compiling host's hostname, addresses, vardir and packages into the catalog.
simp_compile() {
  local name="$1" env="$2" out="$3"
  podman cp "$env" "$name:/tmp/env" >/dev/null 2>&1 || {
    echo "report-diff-simp: could not copy $env into $name" >&2
    return 2
  }
  podman exec "$name" sh -c '
    /opt/puppetlabs/bin/puppet catalog find simp.example.com --terminus compiler \
      --render-as json --node_name_value simp.example.com \
      --environmentpath /tmp/env --environment production 2>/tmp/compile.err |
    sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
  podman exec "$name" cat /tmp/compile.err >"$out/compile.err" 2>/dev/null
  [ -s "$out/catalog.json" ] || {
    echo "report-diff-simp: compiling the SIMP catalog failed:" >&2
    tail -10 "$out/compile.err" >&2
    return 2
  }
}

# simp_apply NAME AGENT CATALOG LIBDIR OUTDIR
#
# Runs one real apply of CATALOG in the booted container NAME. AGENT is `ruby`
# for the image's openvox, reporting through tests/spine/lib's gatejson, or the
# path of a voxagent binary to copy in and run. LIBDIR -- the merged module
# lib/ plus tests/spine/lib -- goes to /opt/puppetlabs/puppet/cache/lib, the
# default libdir of both agents, and both use their default vardir, so each
# agent starts from the same files a pluginsynced agent would. Writes
# OUTDIR/report.json, apply.out (the agent's output), rc (its exit status) and
# wall (seconds). The exit status is recorded, not judged: the stored report is
# the oracle, as report-diff-container.sh records.
simp_apply() {
  local name="$1" agent="$2" catalog="$3" libdir="$4" outdir="$5" cmd f
  mkdir -p "$outdir" || {
    echo "report-diff-simp: could not create $outdir" >&2
    return 2
  }
  if ! podman exec "$name" mkdir -p /opt/puppetlabs/puppet/cache/lib >/dev/null 2>&1 \
    || ! podman cp "$libdir/." "$name:/opt/puppetlabs/puppet/cache/lib/" >/dev/null 2>&1 \
    || ! podman cp "$catalog" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    echo "report-diff-simp: could not copy the libdir and the catalog into $name" >&2
    return 2
  fi
  if [ "$agent" = ruby ]; then
    cmd='/opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --report --reports gatejson --reportdir /tmp/rep'
  else
    if ! podman cp "$agent" "$name:/usr/local/bin/voxagent" >/dev/null 2>&1 \
      || ! podman exec "$name" chmod +x /usr/local/bin/voxagent >/dev/null 2>&1; then
      echo "report-diff-simp: could not copy $agent into $name" >&2
      return 2
    fi
    cmd='/usr/local/bin/voxagent apply --catalog /tmp/catalog.json --report-dir /tmp/rep'
  fi
  podman exec "$name" bash -c '
    s=$EPOCHREALTIME; '"$cmd"' >/tmp/apply.out 2>&1; rc=$?; e=$EPOCHREALTIME
    echo "$rc" >/tmp/rc
    awk -v a="$s" -v b="$e" "BEGIN { printf \"%.2f\n\", b - a }" >/tmp/wall' >/dev/null 2>&1 || {
    echo "report-diff-simp: could not run the $agent apply in $name" >&2
    return 2
  }
  for f in apply.out rc wall; do
    podman exec "$name" cat "/tmp/$f" >"$outdir/$f" 2>/dev/null || {
      echo "report-diff-simp: could not read /tmp/$f from $name" >&2
      return 2
    }
  done
  podman exec "$name" sh -c '
    f="$(find /tmp/rep -name "*.json" 2>/dev/null | head -1)"
    [ -n "$f" ] && cat "$f"' >"$outdir/report.json" 2>/dev/null
  [ -s "$outdir/report.json" ] || {
    rm -f "$outdir/report.json"
    echo "report-diff-simp: the $agent apply in $name wrote no report:" >&2
    tail -10 "$outdir/apply.out" >&2
    return 2
  }
}
