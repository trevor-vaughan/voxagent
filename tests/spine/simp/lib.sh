# shellcheck shell=bash
# Sourced by report-diff-simp.sh (and report-diff-container.sh, for
# simp_modules, concat_fingerprints, augeasproviders_fingerprints and
# cron_core_fingerprints): the
# steps of the SIMP acceptance gate that touch podman, one function per step.
# Every function returns 2 on any failure, with a message naming the cause,
# because each failure means the comparison could not run, never that the
# agents disagree.

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

# concat_fingerprints GATE LIBDIR VERSION
#
# Returns 0 when LIBDIR's puppet/type/concat_file.rb and concat_fragment.rb
# carry the sha256 of puppetlabs-concat VERSION (9.1.0 or 10.0.1), the
# fingerprints voxagent routes concat on (internal/units/concat/manifest.go; concat
# design §0). Otherwise it names the file and returns 2: a wrong module cache
# would send both types to the extension host, and the gate would compare Ruby
# with Ruby while looking green. GATE prefixes the messages.
concat_fingerprints() {
  local gate="$1" libdir="$2" version="$3" file want got
  local -A sums
  case "$version" in
    9.1.0) sums=([concat_file.rb]=7872760598bf35c8e3b285d89daecc4fa29c727a456e47118626a2c58f9c0d6f
      [concat_fragment.rb]=ee003b5d10550f6104fde62ad46f5296ce9361a6a4c1e5ef6d973333f30585ef) ;;
    10.0.1) sums=([concat_file.rb]=e38f130ed5ccb6bc0bb3c7a047ead0ace40ebcbb8d2f408acf46f10b8b9c59c6
      [concat_fragment.rb]=1a53e85c6a873be95eb4b410153e3632658d01c0c65b5f2e0f2886a6f0cb067d) ;;
    *)
      echo "$gate: no concat fingerprints for version '$version'" >&2
      return 2
      ;;
  esac
  for file in concat_file.rb concat_fragment.rb; do
    want="${sums[$file]}"
    got="$(sha256sum "$libdir/puppet/type/$file" 2>/dev/null)"
    got="${got%% *}"
    if [ "$got" != "$want" ]; then
      echo "$gate: $libdir/puppet/type/$file is not puppetlabs-concat $version's (sha256 ${got:-unreadable}, want $want)" >&2
      return 2
    fi
  done
}

# check_sha256_pins GATE LIBDIR LABEL ARRAY
#
# Returns 0 when every file named by a key of the associative array ARRAY (a
# variable name, read by nameref) exists under LIBDIR with the sha256 in its
# value. Otherwise it prints "GATE: LIBDIR/FILE is not LABEL (sha256 GOT, want
# WANT)" to stderr and returns 2.
check_sha256_pins() {
  local gate="$1" libdir="$2" label="$3" file got
  local -n pins="$4"
  for file in "${!pins[@]}"; do
    got="$(sha256sum "$libdir/$file" 2>/dev/null)"
    got="${got%% *}"
    if [ "$got" != "${pins[$file]}" ]; then
      echo "$gate: $libdir/$file is not $label (sha256 ${got:-unreadable}, want ${pins[$file]})" >&2
      return 2
    fi
  done
}

# augeasproviders_fingerprints GATE LIBDIR
#
# Returns 0 when LIBDIR carries the fifteen files voxagent routes sysctl,
# ssh_config, sshd_config, sshd_config_subsystem and augeas on, with their
# sha256 (the manifests of internal/units/augeasproviders_{core,sysctl,ssh} and
# internal/units/augeas_core/manifest.go; augeas design §0):
# augeasproviders_core 4.2.0's three, then each type's type and provider file
# from augeasproviders_sysctl 4.1.0 and augeasproviders_ssh 7.0.0, _ssh's
# sshd_config_match type, which sshd_config's autorequire depends on, and
# augeas_core 2.1.0's type, provider and parser files (its feature file is the
# byte-identical puppet/feature/augeas.rb above), which route the generic augeas
# type. Otherwise it names the file and returns 2: a wrong module cache would
# send the five types to the extension host, and the gate would compare Ruby
# with Ruby while looking green. GATE prefixes the messages.
augeasproviders_fingerprints() {
  local gate="$1" libdir="$2"
  local -A sums=(
    [puppet/provider/augeasprovider/default.rb]=3900c7e789d651b9f5271087ef3a3b3a9b08cfef2b725d30b559bfff280af42a
    [puppet/type/augeasprovider.rb]=0cd37a04489790a03adcae071cf3ee62a81ba238c4ce4959314092d1ab69e795
    [puppet/feature/augeas.rb]=d98e8ef39433632b9e835bbb3e85a75f90a8d17a33c2540f2e14f851ca2791b3
    [puppet/type/sysctl.rb]=e3e75816ce52ca14acbf78252e2dc0ae98867e5d3b81637a5d4e29fe2803d863
    [puppet/provider/sysctl/augeas.rb]=25ca6b921df6408d69107cba0dcc807c0ff996f8d2e6f2d7b381fa0ef285031d
    [puppet/type/ssh_config.rb]=d1486d319d5b8f036d5d0f308503f2e5cb46ae45527371a7637585a58b582884
    [puppet/provider/ssh_config/augeas.rb]=9df8e00affdfe326090333b578eb91235e4a011c72d4cd3547601fd9d15c7221
    [puppet/type/sshd_config.rb]=5b7cf51863c22995039d6db35e67f43c7e273c886a668ec1365428ed5baa1350
    [puppet/provider/sshd_config/augeas.rb]=322463ac709f044adcbfd6ff5b20216490c928e31d26a8622fc9266bafd4374f
    [puppet/type/sshd_config_subsystem.rb]=e5d4c56466d580bdf5943ae89d312916b8d8de3fc473a8a3012bb8240e5fc58b
    [puppet/provider/sshd_config_subsystem/augeas.rb]=237148d020f7c3b53d8414863afc7c76dc85f4e3241b685d04af1cf791f13c9c
    [puppet/type/sshd_config_match.rb]=dedfb3f7e0cf6250fe1454c247c5b3d7968c80b673f6e704b5314da34a33ccd7
    [puppet/type/augeas.rb]=1b8fe183607c21efb9a3a6431a8644d1dbb7e061a83b03bdd6744c1d9549b071
    [puppet/provider/augeas/augeas.rb]=4eedaf2d530bc89909b97f36f552cf820bfbe9808040e6c9caa748a10df31c93
    [puppet_x/augeas/util/parser.rb]=586daa7ddf5a741286f805c90c5e9fc464bb91f184c0c746231bbc5ca0374a7a
  )
  check_sha256_pins "$gate" "$libdir" "the measured augeasproviders file" sums
}

# cron_core_fingerprints GATE LIBDIR
#
# Returns 0 when LIBDIR carries puppetlabs-cron_core 2.1.0's type, provider
# and filetype files with their sha256, the release voxagent routes cron on
# (internal/units/cron_core/manifest.go; cron design §0, §2). Otherwise it
# names the file and returns 2: a changed pin would send cron to the extension
# host, and the gate would compare Ruby with Ruby while looking green. GATE
# prefixes the messages.
cron_core_fingerprints() {
  local gate="$1" libdir="$2"
  local -A sums=(
    [puppet/type/cron.rb]=a5f7712895d0370077b24e8cfcd47ab16f57e4df9016257258fa7b01168a903c
    [puppet/provider/cron/crontab.rb]=e823f00e5155c87d2f2acaedf109be12c0b13b3781e6bc05d7285f1cdc4dba4a
    [puppet/provider/cron/filetype.rb]=f82a87886c2ff0b72a0e43ebd0bb6edecda0362ddcbc672bc51245f2b88266ce
  )
  check_sha256_pins "$gate" "$libdir" "cron_core 2.1.0's" sums
}

# host_core_fingerprints GATE LIBDIR
#
# Returns 0 when LIBDIR carries puppetlabs-host_core 2.1.0's type and provider
# files with their sha256, which equal the vendored 1.3.0's voxagent routes
# host on (internal/units/host_core/manifest.go; parsedfile-types design §2.2).
# Otherwise it names the file and returns 2: a changed pin would send host to
# the extension host, and the gate would compare Ruby with Ruby while looking
# green. GATE prefixes the messages.
host_core_fingerprints() {
  local gate="$1" libdir="$2"
  local -A sums=(
    [puppet/type/host.rb]=e0d448a5835a12a2565d309b348ed071c0f849aeec87372777c948acc6bf1e35
    [puppet/provider/host/parsed.rb]=a396f906038d718f6c69cb7a836f13eed16de5e9b2cf9d39a020eed9e9685f7d
  )
  check_sha256_pins "$gate" "$libdir" "host_core 2.1.0's" sums
}

# mount_core_fingerprints GATE LIBDIR
#
# Returns 0 when LIBDIR carries puppetlabs-mount_core 2.1.0's type and two
# provider files with their sha256, a release voxagent routes mount on
# (internal/units/mount_core/manifest.go; parsedfile-types design §2.2).
# Otherwise it names the file and returns 2: a changed pin would send mount to
# the extension host, and the gate would compare Ruby with Ruby while looking
# green. GATE prefixes the messages.
mount_core_fingerprints() {
  local gate="$1" libdir="$2"
  local -A sums=(
    [puppet/type/mount.rb]=ad59cd27fb9611a38407ece68e1d128c05fe6dba6f5be5af979c4c50eda43d7e
    [puppet/provider/mount/parsed.rb]=9ce69a02f80ad9e04c014bec2d555240dca26e5226e77f2ea0695bc96c293230
    [puppet/provider/mount.rb]=9c2ce747e83d5398ac51cd5f5f9abe80860c75c61f7650ddc3fe39bf3c07bea1
  )
  check_sha256_pins "$gate" "$libdir" "mount_core 2.1.0's" sums
}

# sshkeys_core_fingerprints GATE LIBDIR
#
# Returns 0 when LIBDIR carries puppetlabs-sshkeys_core 3.1.0's sshkey and
# ssh_authorized_key type and provider files with their sha256, the release
# voxagent routes both types on (internal/units/sshkeys_core/manifest.go;
# parsedfile-types design §2.2). Otherwise it names the file and returns 2: a
# changed pin would send both types to the extension host, and the gate would
# compare Ruby with Ruby while looking green. GATE prefixes the messages.
sshkeys_core_fingerprints() {
  local gate="$1" libdir="$2"
  local -A sums=(
    [puppet/type/sshkey.rb]=a16318375affd95b2e368d69b5ede6cae8d8639fcad85277bdabca0d0e2ed8da
    [puppet/provider/sshkey/parsed.rb]=2f8d4fe1dc69c2c5f362cfbe044e5bd477c4700d079a817557ca7e08d660e1cf
    [puppet/type/ssh_authorized_key.rb]=48dd2f61019e1e163e7d354c4618e7466dfd93784c38b9b9afe2180002ea1a06
    [puppet/provider/ssh_authorized_key/parsed.rb]=86f01f5ed68cb89e193b70291712bd2417df7b6a7a9e382cfeecfb4ea1810334
  )
  check_sha256_pins "$gate" "$libdir" "sshkeys_core 3.1.0's" sums
}
