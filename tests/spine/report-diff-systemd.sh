#!/usr/bin/env bash
# Compare the reports Ruby Puppet and voxagent produce for `service` from
# identical catalog bytes, each inside a container booted with systemd as PID 1.
#
# Neither the host nor report-diff-container.sh can be the oracle for service.
# Puppet selects the systemd provider only when systemd is the init system, and
# voxagent refuses a catalog holding a service unless PID 1 is systemd (D-22). A
# container running `sleep` as PID 1 would make Ruby fall back to another
# provider and make voxagent refuse the run, so the comparison would be about
# nothing. This gate boots /sbin/init in a container built from the G5 base
# (images/Containerfile.systemd adds nothing but the boot).
#
# TWO CONTAINERS, ONE PER AGENT, for the reason report-diff-container.sh gives
# for package: a service apply mutates the machine. Sharing one container would
# leave Ruby's starts and enables in place and turn voxagent's run into a no-op.
# Both are seeded identically before either agent runs, and the seeded premises
# are asserted, because a fixture whose starting state drifted makes both agents
# do nothing and agree perfectly. Named with this script's PID so the cleanup
# trap cannot destroy anyone else's container.
#
# Besides the reports, the resulting unit state is compared: `systemctl
# is-active` and `is-enabled` per fixture unit must match between the sides.
# Two reports can agree while both agents left the machine wrong.
#
# Inside a nested container rootless systemd needs a delegated cgroup to boot;
# see docs/maintainers/apply-spine.md, "Running the systemd gate".
#
#   0  the reports, the walk order and the unit state agree outside the
#      allowlist
#   1  they diverge -- a result about voxagent
#   2  the comparison could not run
set -uo pipefail
# Inherited noclobber disarms every `: > "$f"` writability probe in this tree,
# and makes the plain `>` redirects below fail on a reused path. A gate owns its
# work directory outright, so clobber-protection guards nothing of value here.
set +C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
EVIDENCE="${EVIDENCE:-$ROOT/.test-output/spine}"
IMAGE="${IMAGE:-localhost/voxagent-systemd:latest}"
BASE_IMAGE="${BASE_IMAGE:-localhost/fleet-node-el:latest}"

# The same pin, and the same tripwire, as report-diff-container.sh: the base
# installs openvox-agent unversioned, so a rebuild floats the oracle, and exit 2
# beats a green run measured against a reference nobody reviewed.
WANT_PUPPET="${WANT_PUPPET:-8.33.0}"

RUBY_SIDE="systemd-ruby-$$"
GO_SIDE="systemd-go-$$"

# Every unit the fixture declares that exists on disk. voxb and voxd start
# active; every other one starts inactive and disabled.
UNITS="voxa voxb voxc voxd voxe voxf voxg"
SEEDED_ACTIVE="voxb voxd"
MISSING_UNIT="voxagent-no-such-unit"

# The longest the gate waits for each container's systemd to finish booting.
BOOT_TIMEOUT=180

command -v podman >/dev/null 2>&1 || {
  echo "report-diff-systemd: podman is not available" >&2
  exit 2
}
podman image exists "$BASE_IMAGE" || {
  echo "report-diff-systemd: $BASE_IMAGE is not built; run 'task fleet:images'" >&2
  exit 2
}
# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh report-diff-systemd "$ROOT" "$VOXAGENT" "$GATEKIT"

# shellcheck source=build-image.sh
. "$HERE/build-image.sh" || exit 2

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"; podman rm -f "$RUBY_SIDE" "$GO_SIDE" >/dev/null 2>&1' EXIT

if ! podman image exists "$IMAGE"; then
  echo "report-diff-systemd: building $IMAGE"
  build_image report-diff-systemd "$IMAGE" "$HERE/images/Containerfile.systemd" \
    "$HERE/images" "$WORK/build.log" || exit 2
fi

got_puppet="$(podman run --rm "$IMAGE" /opt/puppetlabs/bin/puppet --version 2>/dev/null)" || {
  echo "report-diff-systemd: cannot read the image's puppet version" >&2
  exit 2
}
if [ "$got_puppet" != "$WANT_PUPPET" ]; then
  echo "report-diff-systemd: $IMAGE carries puppet $got_puppet but the oracle is" >&2
  echo "report-diff-systemd: pinned at $WANT_PUPPET; refusing to compare against an" >&2
  echo "report-diff-systemd: unpinned reference" >&2
  exit 2
fi

# The fixture units: simple services that run forever, so a start leaves them
# active.
mkdir -p "$WORK/units" || exit 2
for u in $UNITS; do
  printf '[Unit]\nDescription=voxagent fixture\n[Service]\nType=simple\nExecStart=/bin/sleep infinity\n[Install]\nWantedBy=multi-user.target\n' \
    >"$WORK/units/$u.service" || exit 2
done

# boot_side NAME: boot a systemd container, install the fixture units, seed and
# assert the starting state. Returns 2, naming the cause, on any failure.
#
# Both sides get the same hostname for the reason report-diff-container.sh
# gives: a report's `host` is compared, and podman would otherwise name each
# container after its own id.
boot_side() {
  local name="$1" state u want got
  podman run -d --systemd=always --hostname spine.example --name "$name" \
    "$IMAGE" >/dev/null 2>"$WORK/$name.run.err" || {
    echo "report-diff-systemd: could not start $name:" >&2
    sed -n '1,10p' "$WORK/$name.run.err" >&2
    return 2
  }
  # is-system-running --wait blocks until boot finishes, but only once the
  # system bus exists: asked straight after `podman run` it prints nothing and
  # fails with "Failed to connect to system scope bus" (measured). So it is
  # retried while it prints nothing. One wall-clock deadline bounds the whole
  # wait at BOOT_TIMEOUT seconds (plus at most one second of sleep): each
  # attempt's timeout is the time left, and a timed-out attempt (124) ends the
  # wait. `degraded` is accepted: it means some image unit failed, which is
  # nothing to do with the fixture.
  local deadline=$((SECONDS + BOOT_TIMEOUT)) left wait_rc
  state=''
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
      echo "report-diff-systemd: $name reached '${state:-nothing}' within ${BOOT_TIMEOUT}s, not running or degraded" >&2
      podman logs "$name" 2>&1 | tail -20 >&2
      echo "report-diff-systemd: systemd did not boot; inside a nested container rootless systemd needs a delegated cgroup — see docs/maintainers/apply-spine.md \"Running the systemd gate\"" >&2
      return 2
      ;;
  esac
  got="$(podman exec "$name" cat /proc/1/comm 2>/dev/null)"
  [ "$got" = systemd ] || {
    echo "report-diff-systemd: PID 1 in $name is '$got', not systemd" >&2
    return 2
  }
  if ! podman cp "$WORK/units/." "$name:/etc/systemd/system/" >/dev/null 2>&1 \
    || ! podman exec "$name" systemctl daemon-reload >/dev/null 2>&1; then
    echo "report-diff-systemd: could not install the fixture units in $name" >&2
    return 2
  fi
  # Units are installed stopped and disabled; only the active ones need work.
  # shellcheck disable=SC2086 # word-splitting the unit list is intended
  podman exec "$name" systemctl start $SEEDED_ACTIVE >/dev/null 2>&1 || {
    echo "report-diff-systemd: could not start $SEEDED_ACTIVE in $name" >&2
    return 2
  }
  # Each premise is one line of `unit active enabled`, read in one exec.
  podman exec "$name" sh -c '
    for u in '"$UNITS"'; do
      echo "$u $(systemctl is-active "$u") $(systemctl is-enabled "$u")"
    done
    echo "'"$MISSING_UNIT"' $(systemctl show -p LoadState --value '"$MISSING_UNIT"')"
  ' >"$WORK/$name.premises" 2>/dev/null
  for u in $UNITS; do
    want="$u inactive disabled"
    case " $SEEDED_ACTIVE " in *" $u "*) want="$u active disabled" ;; esac
    got="$(grep "^$u " "$WORK/$name.premises")"
    [ "$got" = "$want" ] || {
      echo "report-diff-systemd: premise failed in $name: want '$want', got '$got'" >&2
      return 2
    }
  done
  got="$(grep "^$MISSING_UNIT " "$WORK/$name.premises")"
  [ "$got" = "$MISSING_UNIT not-found" ] || {
    echo "report-diff-systemd: premise failed in $name: $MISSING_UNIT should be unknown, got '$got'" >&2
    return 2
  }
  podman exec "$name" mkdir -p /tmp/target >/dev/null 2>&1 || {
    echo "report-diff-systemd: could not create /tmp/target in $name" >&2
    return 2
  }
  return 0
}

# What each resource exercises:
#   voxa     stopped+disabled -> running+enabled: a start with enable synced
#            inside it, and the enable state read again after the start.
#   voxb     active -> stopped: a stop.
#   voxc     enable only: an enable change of its own.
#   voxd     running, subscribed to a change: a refresh restart.
#   voxe     no ensure, subscribed, not running: Puppet records the refresh
#            event and counts it as restarted, but restarts nothing (measured:
#            the unit stays inactive), so the state compare is what checks it.
#   voxf     stopped -> running, subscribed: the start invalidates the pending
#            refresh, so no restart follows.
#   voxg     a custom start command, run through /bin/sh -c.
#   voxagent-no-such-unit  enable => false on a unit that does not exist.
#
# A failing start is deliberately ABSENT. Measured: its message carries
# journalctl's lines, whose timestamps differ between any two containers --
# two Ruby runs differ there too -- and Puppet logs it once bare, which the
# comparator's failure-detail mask cannot reach. The unit test
# TestServiceStartFailureCarriesTheJournal pins that path instead; see
# docs/maintainers/apply-spine.md.
cat >"$WORK/site.pp" <<'EOF' || exit 2
node default {
  file { '/tmp/target/trigger': ensure => file, content => "changed\n" }
  service { 'voxa': ensure => running, enable => true }
  service { 'voxb': ensure => stopped }
  service { 'voxc': enable => true }
  service { 'voxd': ensure => running, subscribe => File['/tmp/target/trigger'] }
  service { 'voxe': subscribe => File['/tmp/target/trigger'] }
  service { 'voxf': ensure => running, subscribe => File['/tmp/target/trigger'] }
  service { 'voxg': ensure => running, start => '/usr/bin/systemctl start voxg.service' }
  service { 'voxagent-no-such-unit': enable => false }
}
EOF

boot_side "$RUBY_SIDE" || exit 2

# Compiled by the booted Ruby side's own Puppet, and the same bytes are fed to
# both agents; report-diff-container.sh records why a hand-written catalog is
# not acceptable.
podman cp "$WORK/site.pp" "$RUBY_SIDE:/tmp/site.pp" >/dev/null 2>&1 || {
  echo "report-diff-systemd: could not copy the fixture manifest into $RUBY_SIDE" >&2
  exit 2
}
podman exec "$RUBY_SIDE" sh -c '
  mkdir -p /tmp/e/production/manifests &&
  cp /tmp/site.pp /tmp/e/production/manifests/site.pp &&
  /opt/puppetlabs/bin/puppet catalog find spine.example --terminus compiler \
    --render-as json --node_name_value spine.example \
    --environmentpath /tmp/e --environment production 2>/dev/null |
  sed -n "/^{/,\$p"
' >"$WORK/catalog.json" 2>"$WORK/compile.err"
[ -s "$WORK/catalog.json" ] || {
  echo "report-diff-systemd: compiling the fixture catalog failed" >&2
  sed -n '1,20p' "$WORK/compile.err" >&2
  exit 2
}
if ! podman cp "$WORK/catalog.json" "$RUBY_SIDE:/tmp/catalog.json" >/dev/null 2>&1 \
  || ! podman cp "$HERE/lib" "$RUBY_SIDE:/tmp/gatelib" >/dev/null 2>&1; then
  echo "report-diff-systemd: could not copy the catalog and the report processor into $RUBY_SIDE" >&2
  exit 2
fi

# The apply's exit status is not consulted; report-diff-container.sh records
# why the stored report, not the process exit, is the oracle. Captured inside
# the container by the image's own openvox, through gatejson.rb, exactly as
# report-diff-container.sh does.
podman exec "$RUBY_SIDE" sh -c '
  /opt/puppetlabs/bin/puppet apply --catalog /tmp/catalog.json \
    --report --reports gatejson --libdir /tmp/gatelib --reportdir /tmp/ruby
' >"$WORK/ruby.out" 2>&1
podman exec "$RUBY_SIDE" cat /tmp/ruby/gate.json >"$WORK/ruby.json" 2>"$WORK/convert.err"
if [ ! -s "$WORK/ruby.json" ]; then
  echo "report-diff-systemd: Ruby wrote no usable report; there is nothing to" >&2
  echo "report-diff-systemd: compare against" >&2
  sed -n '1,20p' "$WORK/convert.err" >&2
  sed -n '1,20p' "$WORK/ruby.out" >&2
  exit 2
fi

# Prove the fixture reached the systemd provider before trusting the
# comparison. If Ruby had picked another provider, or every unit had failed to
# load, both agents could agree on a report about nothing.
for want in '"provider_used":"systemd"' '"service_started"' '"service_stopped"' \
  '"service_enabled"' '"refresh"'; do
  grep -qF "$want" "$WORK/ruby.json" || {
    echo "report-diff-systemd: the reference run did not exercise the service" >&2
    echo "report-diff-systemd: fixture: $want is missing from Ruby's report" >&2
    sed -n '1,20p' "$WORK/ruby.out" >&2
    exit 2
  }
done

boot_side "$GO_SIDE" || exit 2
if ! podman cp "$WORK/catalog.json" "$GO_SIDE:/tmp/catalog.json" >/dev/null 2>&1 \
  || ! podman cp "$VOXAGENT" "$GO_SIDE:/tmp/voxagent" >/dev/null 2>&1 \
  || ! podman exec "$GO_SIDE" chmod +x /tmp/voxagent >/dev/null 2>&1; then
  echo "report-diff-systemd: could not copy the catalog and $VOXAGENT into $GO_SIDE" >&2
  exit 2
fi
podman exec "$GO_SIDE" /tmp/voxagent apply --catalog /tmp/catalog.json \
  --report-dir /tmp/go >"$WORK/go.out" 2>&1
podman exec "$GO_SIDE" sh -c '
  g="$(find /tmp/go -name "*.json" | head -1)"
  [ -n "$g" ] || exit 3
  cat "$g"
' >"$WORK/go.json" 2>"$WORK/go.err"
if [ ! -s "$WORK/go.json" ]; then
  echo "report-diff-systemd: voxagent wrote no report" >&2
  sed -n '1,20p' "$WORK/go.err" >&2
  sed -n '1,20p' "$WORK/go.out" >&2
  exit 2
fi

"$GATEKIT" compare-reports \
  "$WORK/ruby.json" "$WORK/go.json" "$HERE/allowlist.json"
rc=$?
case "$rc" in
  0 | 1) ;;
  *)
    echo "report-diff-systemd: the comparison could not run" >&2
    exit 2
    ;;
esac
# The walk order: compare-reports matches statuses by ref and logs as a
# multiset, so it cannot see the order resources were evaluated in.
# compare-order reads it from each report's log order.
"$GATEKIT" compare-order "$WORK/ruby.json" "$WORK/go.json"
case $? in
  0) ;;
  1)
    echo "report-diff-systemd: the walk order diverges" >&2
    rc=1
    ;;
  *)
    echo "report-diff-systemd: the walk order could not be compared" >&2
    exit 2
    ;;
esac

# keep_evidence: copy whatever the run produced to $EVIDENCE, because reading
# a divergence needs both reports side by side and the work tree is about to
# go. A state file is missing when reading the state was what failed.
keep_evidence() {
  local f
  mkdir -p "$EVIDENCE" || return 1
  cp "$WORK/ruby.json" "$EVIDENCE/systemd-ruby.json" \
    && cp "$WORK/go.json" "$EVIDENCE/systemd-voxagent.json" \
    && cp "$WORK/catalog.json" "$EVIDENCE/systemd-catalog.json" || return 1
  for f in ruby:"$RUBY_SIDE" voxagent:"$GO_SIDE"; do
    if [ -f "$WORK/${f#*:}.state" ]; then
      cp "$WORK/${f#*:}.state" "$EVIDENCE/systemd-${f%%:*}.state" || return 1
    fi
  done
  echo "report-diff-systemd: the reports, the catalog and any unit state read are in $EVIDENCE" >&2
}

# The unit state each agent left behind: one `unit|active|enabled` line per
# unit. `|` cannot occur in a unit name or a systemctl state word, so an empty
# field stays visible as `||`, where a space would collapse. stderr is dropped
# because systemctl adds a complaint there for the missing unit; its stdout is
# `inactive not-found` (measured) and is compared as is. An empty is-active
# field means systemctl itself failed, and two such failures would otherwise
# agree, so it is could-not-run.
for side in "$RUBY_SIDE" "$GO_SIDE"; do
  podman exec "$side" sh -c '
    for u in '"$UNITS $MISSING_UNIT"'; do
      echo "$u|$(systemctl is-active "$u" 2>/dev/null)|$(systemctl is-enabled "$u" 2>/dev/null)"
    done
  ' >"$WORK/$side.state" 2>/dev/null
  for u in $UNITS $MISSING_UNIT; do
    if [ -z "$(awk -F'|' -v u="$u" '$1 == u { print $2 }' "$WORK/$side.state")" ]; then
      echo "report-diff-systemd: could not read the state of $u in $side" >&2
      [ "$rc" -ne 1 ] || keep_evidence
      exit 2
    fi
  done
done
# Ruby's end state must be the one the fixture asks for before the two sides
# are compared: a run that failed identically on both sides -- every start
# refused, say -- would otherwise agree perfectly. Each entry is
# `unit:is-active:is-enabled`, `-` where the fixture does not manage the field.
for expect in voxa:active:enabled voxb:inactive:- voxc:-:enabled voxd:active:- \
  voxe:inactive:- voxf:active:- voxg:active:-; do
  IFS=: read -r u want_active want_enabled <<<"$expect"
  IFS='|' read -r _ got_active got_enabled < <(grep "^$u|" "$WORK/$RUBY_SIDE.state")
  if { [ "$want_active" != - ] && [ "$got_active" != "$want_active" ]; } \
    || { [ "$want_enabled" != - ] && [ "$got_enabled" != "$want_enabled" ]; }; then
    echo "report-diff-systemd: the reference run left $u ${got_active:-?}/${got_enabled:-?}," >&2
    echo "report-diff-systemd: not the fixture's ${want_active}/${want_enabled}; refusing to compare" >&2
    keep_evidence
    exit 2
  fi
done
if ! diff "$WORK/$RUBY_SIDE.state" "$WORK/$GO_SIDE.state" >"$WORK/state.diff"; then
  echo "report-diff-systemd: unit state differs (< ruby, > voxagent):" >&2
  grep '^[<>]' "$WORK/state.diff" >&2
  rc=1
fi

case "$rc" in
  0) echo "report-diff-systemd: reports agree outside the allowlist" ;;
  1)
    keep_evidence
    echo "report-diff-systemd: reports diverge" >&2
    ;;
esac
exit "$rc"
