# shellcheck shell=bash
# Sourced by report-diff-container.sh: G5's mount half (ParsedFile-types
# design §10.4). Uses that script's HERE, ROOT, IMAGE, VOXAGENT, EVIDENCE and
# WORK, simp/lib.sh's simp_modules, parsedfile/image.sh's parsedfile_image and
# g5-case.sh's g5_judge and g5_refusal.
#
# It runs on localhost/parsedfile-measure-el, as the host half does. Every
# container runs with --no-hosts, like the host half's, so both agents start
# from the image's own files (it has no /etc/fstab), and, in this half only,
# with --cap-add SYS_ADMIN: M10 measured it as the one flag that lets the
# container's root run mount, mount -o remount and umount on a tmpfs, so every
# ensure transition and refresh runs live. `mount` lists the container's own
# mounts, the same for both agents; no case manages Mount['/'], which the
# container shows as a ghost (§10.4).
#
# Every case is a directory holding site.pp, an optional seed.sh, an optional
# `files`, the paths its end state is read from, and an optional `mounts`, the
# mount points it reads findmnt for. The cases in cases/ run with no
# mount_core in --libdir, so both agents use openvox's vendored 1.3.0;
# cases-2.1.0/create runs with mount_core 2.1.0's lib/ (from the SIMP gate's
# pinned module cache) in --libdir. The two releases' contracts do not differ
# (§2.2), so one case proves voxagent routes the SIMP release natively. Ruby
# loads the libdir copy over the vendored one (M3), and voxagent routes mount
# natively only on the digests of §2.2, so a changed pin shows up as a
# delegated run, which fails the half (below). A missing case stops the half
# with 2 before anything runs. Each case is compiled on its own by the image's
# openvox and applied in FRESH containers, one per agent and mode, seeded by
# its seed.sh under `sh -eu`, so a failing seed is a gate error (2). Per case:
# live, then a rerun in the same container; then --noop from a fresh seeded
# container. Each pair is judged by gatekit compare-reports and compare-order
# against the unchanged allowlist. After the live run each side's state is
# read and compared: every path `files` lists (mode, owner, group, size and
# `cat -A`), with only the header's timestamp normalised, every clientbucket
# entry's `paths` file, since reports carry neither the file's text nor its
# backups, and `findmnt -rn -o TARGET,FSTYPE,OPTIONS` of every `mounts` path,
# with the host-dependent options `seclabel`, `uid=<n>` and `gid=<n>` removed
# (M10: a rootless tmpfs gets the host user's ids).
#
# Both agents run without --debug, as in every other half, so the
# `Executing:`, `Flushing` and `Adding auto...` lines are compared through
# their effects: the end state and the walk order.
#
# A case Ruby refuses at catalog conversion (MOUNT_REFUSED) is judged by
# g5_refusal: the same detail, and no report from either side. In every other
# case voxagent's output must not name mount among the types `applied via
# extension host`, so a delegated run, Ruby against Ruby, cannot pass as
# native.

MOUNT_IMAGE="${MOUNT_IMAGE:-localhost/parsedfile-measure-el:latest}"
MOUNT_CASES="cases/defined-absent cases/name-040 cases/trailing-slash
  cases/missing-fstype cases/leading-whitespace cases/fields
  cases/dump-pass-reset cases/remove cases/atboot cases/dash-defined
  cases/line-break-name cases/line-break-device cases/line-break-dump
  cases/line-break-pass
  cases/mounted-from-absent cases/unmounted-from-mounted cases/ghost-to-mounted
  cases/ghost-to-absent cases/ghost-no-ensure cases/options-remount
  cases/options-unmounted-to-mounted cases/remounts-false cases/swap
  cases/notify-refresh cases/mount-fails cases/dash-mounted cases/umount-busy
  cases/refresh-fails cases/relations
  cases-2.1.0/create"
MOUNT_REFUSED="cases/dump-bad"

# mount_case_dir CASE: the directory holding CASE's site.pp; 2, naming CASE,
# when it has no site.pp.
mount_case_dir() {
  local dir="$HERE/mount/$1"
  if [ ! -f "$dir/site.pp" ]; then
    echo "report-diff-container: mount: case $1 is missing: no $dir/site.pp" >&2
    return 2
  fi
  echo "$dir"
}

# mount_check_cases: 2 unless every case's directory and site.pp exist.
mount_check_cases() {
  local c rc=0
  for c in $MOUNT_CASES $MOUNT_REFUSED; do
    mount_case_dir "$c" >/dev/null || rc=2
  done
  return "$rc"
}

mount_is_refused() {
  local c
  for c in $MOUNT_REFUSED; do
    [ "$c" != "$1" ] || return 0
  done
  return 1
}

# mount_libdir CASE: the libdir CASE's set applies with.
mount_libdir() {
  case "$1" in
    cases-2.1.0/*) echo "$WORK/mount/libdir-2.1.0" ;;
    *) echo "$WORK/mount/libdir-1.3.0" ;;
  esac
}

# mount_compile: compiles every case into $WORK/mount/CASE/catalog.json, in one
# container with an empty modulepath.
mount_compile() {
  local name="mount-compile-$$" c out
  podman run --name "$name" --hostname spine.example --no-hosts --detach "$MOUNT_IMAGE" sleep 900 >/dev/null 2>&1 || {
    echo "report-diff-container: mount: could not start a compiler" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/e/production/manifests /tmp/e/production/modules >/dev/null 2>&1 || return 2
  for c in $MOUNT_CASES $MOUNT_REFUSED; do
    out="$WORK/mount/$c"
    mkdir -p "$out" || return 2
    podman cp "$(mount_case_dir "$c")/site.pp" "$name:/tmp/e/production/manifests/site.pp" >/dev/null 2>&1 || return 2
    podman exec "$name" sh -c '
      /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
        --render-as json --node_name_value spine.example \
        --environmentpath /tmp/e --environment production 2>/tmp/compile.err |
      sed -n "/^{/,\$p"' >"$out/catalog.json" 2>/dev/null
    if [ ! -s "$out/catalog.json" ]; then
      echo "report-diff-container: mount: compiling $c failed:" >&2
      podman exec "$name" cat /tmp/compile.err 2>/dev/null | sed -n '1,10p' >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# mount_side AGENT CASE MODE: one fresh container, seeded as CASE's seed.sh
# seeds it, applied by AGENT (ruby or voxagent) in MODE (live, which also
# reruns, or noop). Writes $WORK/mount/CASE/AGENT-RUN.{out,rc,json} for each
# run, and AGENT-live.state after the live run.
mount_side() {
  local agent="$1" c="$2" mode="$3" out="$WORK/mount/$2"
  local name="mount-$agent-$mode-$$" dir run flags cmd
  dir="$(mount_case_dir "$c")"
  podman run --name "$name" --hostname spine.example --no-hosts --cap-add SYS_ADMIN --detach "$MOUNT_IMAGE" sleep 900 >/dev/null 2>&1 || return 2
  if ! podman cp "$(mount_libdir "$c")" "$name:/tmp/lib" >/dev/null 2>&1 \
    || ! podman cp "$out/catalog.json" "$name:/tmp/catalog.json" >/dev/null 2>&1; then
    podman rm -f -t 0 "$name" >/dev/null 2>&1
    return 2
  fi
  if [ -f "$dir/seed.sh" ]; then
    if ! podman cp "$dir/seed.sh" "$name:/tmp/seed.sh" >/dev/null 2>&1 \
      || ! podman exec "$name" sh -eu /tmp/seed.sh >"$out/$agent-$mode-seed.out" 2>&1; then
      echo "report-diff-container: mount $c: the seed failed in $name:" >&2
      sed -n '1,10p' "$out/$agent-$mode-seed.out" >&2
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  fi
  if [ "$agent" = voxagent ]; then
    if ! podman cp "$VOXAGENT" "$name:/tmp/voxagent" >/dev/null 2>&1 \
      || ! podman exec "$name" chmod +x /tmp/voxagent >/dev/null 2>&1; then
      podman rm -f -t 0 "$name" >/dev/null 2>&1
      return 2
    fi
  fi
  flags=''
  [ "$mode" = noop ] && flags='--noop'
  for run in $([ "$mode" = live ] && echo live rerun || echo noop); do
    if [ "$agent" = ruby ]; then
      cmd="/opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json --libdir /tmp/lib \
        --report --reports gatejson --reportdir /tmp/r-$run --color=false $flags"
    else
      cmd="/tmp/voxagent apply --catalog /tmp/catalog.json --libdir /tmp/lib --report-dir /tmp/r-$run $flags"
    fi
    podman exec "$name" sh -c "$cmd" >"$out/$agent-$run.out" 2>&1
    echo "$?" >"$out/$agent-$run.rc"
    podman exec "$name" sh -c "g=\"\$(find /tmp/r-$run -name '*.json' 2>/dev/null | head -1)\"
      [ -z \"\$g\" ] || cat \"\$g\"" >"$out/$agent-$run.json" 2>/dev/null
    if [ "$run" = live ] && [ -f "$dir/files" ]; then
      mount_state "$name" "$dir" >"$out/$agent-live.state" 2>&1 || {
        podman rm -f -t 0 "$name" >/dev/null 2>&1
        return 2
      }
    fi
  done
  podman rm -f -t 0 "$name" >/dev/null 2>&1
}

# mount_state NAME DIR: the state a live run left in container NAME, one
# block per path DIR/files lists: its stat line and `cat -A`, the header's
# first line with its timestamp replaced by <time>, or ABSENT; then every
# clientbucket entry, sorted: its directory (the old content's sha256) and its
# `paths` file; then, per path DIR/mounts lists, findmnt's line for each mount
# on it, in findmnt's order, without the options seclabel, uid=<n> and
# gid=<n>, or NOT MOUNTED.
mount_state() {
  local f
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== $f"
    # report-diff-container.sh sets pipefail, so a failed exec fails the pipe.
    podman exec "$1" sh -c 'if [ -e "$1" ]; then stat -c "mode=%a owner=%U group=%G size=%s" "$1"; cat -A "$1"; else echo ABSENT; fi' sh "$f" |
      sed -E 's/^(# HEADER: This file was autogenerated at ).*$/\1<time>/' || return 2
  done <"$2/files"
  echo "== clientbucket"
  podman exec "$1" sh -c 'b=/opt/puppetlabs/puppet/cache/clientbucket
    [ ! -d "$b" ] || find "$b" -name paths -type f | sort | while read -r p; do
      echo "${p#"$b"/}"; cat -A "$p"
    done' || return 2
  [ -f "$2/mounts" ] || return 0
  while read -r f; do
    [ -n "$f" ] || continue
    echo "== findmnt $f"
    # findmnt exits 1 when nothing is mounted on the path.
    podman exec "$1" sh -c 'findmnt -rn -o TARGET,FSTYPE,OPTIONS --mountpoint "$1"; r=$?
      [ "$r" -ne 1 ] || { echo NOT MOUNTED; r=0; }; exit "$r"' sh "$f" |
      awk '{ n = split($3, o, ","); s = ""
        for (i = 1; i <= n; i++) if (o[i] != "seclabel" && o[i] !~ /^(uid|gid)=[0-9]+$/) s = s (s == "" ? "" : ",") o[i]
        if (NF == 3) $3 = s; print }' || return 2
  done <"$2/mounts"
}

# mount_judge_state CASE: compares the two sides' live state.
mount_judge_state() {
  local out="$WORK/mount/$1"
  if ! diff -u "$out/ruby-live.state" "$out/voxagent-live.state" >"$out/state.diff"; then
    echo "report-diff-container: mount $1 live: the end state DIVERGES (- Ruby, + voxagent):" >&2
    sed -n '1,30p' "$out/state.diff" >&2
    return 1
  fi
  echo "report-diff-container: mount $1 live: end state agrees"
}

# mount_native CASE RUN: 1 when voxagent sent mount to the extension host.
mount_native() {
  local out="$WORK/mount/$1"
  if grep -Eq '^voxagent: [0-9]+ mount resource\(s\) applied via extension host' "$out/voxagent-$2.out"; then
    echo "report-diff-container: mount $1 $2: voxagent delegated mount to the extension host:" >&2
    grep 'applied via extension host' "$out/voxagent-$2.out" >&2
    return 1
  fi
}

# mount_gate: the whole mount half. Returns the worst result.
mount_gate() {
  local pin_hash modules base c mode run rc=0 r pids p
  mount_check_cases || return 2
  mkdir -p "$WORK/mount" || return 2
  parsedfile_image report-diff-container "$IMAGE" "$MOUNT_IMAGE" "$WORK/mount/image-build.log" || return 2
  pin_hash="$(sha256sum "$HERE/simp/modules.pin")" || return 2
  modules="$ROOT/.test-output/simp/modules-${pin_hash:0:12}"
  mkdir -p "${modules%/*}" || return 2
  simp_modules "$modules" "$IMAGE" "$HERE/simp/modules.pin" || return 2

  base="$WORK/mount/libdir-1.3.0"
  if ! mkdir -p "$base" || ! cp -R "$HERE/lib/." "$base/" \
    || ! cp -R "$base" "$WORK/mount/libdir-2.1.0" \
    || ! cp -R "$modules/mount_core/lib/." "$WORK/mount/libdir-2.1.0/"; then
    echo "report-diff-container: mount: could not create the libdirs in $WORK/mount" >&2
    return 2
  fi
  mount_compile || return 2

  for c in $MOUNT_CASES $MOUNT_REFUSED; do
    pids=''
    for mode in live noop; do
      mount_is_refused "$c" && [ "$mode" != live ] && continue
      mount_side ruby "$c" "$mode" &
      pids="$pids $!"
      mount_side voxagent "$c" "$mode" &
      pids="$pids $!"
    done
    r=0
    for p in $pids; do
      wait "$p" || r=2
    done
    if [ "$r" -ne 0 ]; then
      echo "report-diff-container: mount $c: a container could not be prepared or run" >&2
      return 2
    fi
    if mount_is_refused "$c"; then
      g5_refusal "$WORK/mount/$c" "mount $c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      continue
    fi
    for run in live rerun noop; do
      g5_judge "$WORK/mount/$c" "mount $c" "$run"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
      mount_native "$c" "$run" || { [ "$rc" -ge 1 ] || rc=1; }
    done
    if [ -f "$(mount_case_dir "$c")/files" ]; then
      mount_judge_state "$c"
      r=$?
      [ "$r" -gt "$rc" ] && rc=$r
    fi
  done
  if [ "$rc" -ne 0 ]; then
    mkdir -p "$EVIDENCE" && rm -rf "$EVIDENCE/container-mount" \
      && cp -R "$WORK/mount" "$EVIDENCE/container-mount" \
      && echo "report-diff-container: the mount runs are in $EVIDENCE/container-mount" >&2
  fi
  return "$rc"
}
