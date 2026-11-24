#!/usr/bin/env bash
# Gate: `voxagent agent` against a real OpenVox server, with Ruby `puppet agent`
# on the same server as the oracle.
#
# Stack (tests/agent/compose.yml): one OpenVox server (CA, autosign, reports
# stored as YAML on disk) and two EL nodes. `ruby.example.test` runs
# `puppet agent -t`; `vox.example.test` runs the voxagent binary copied in.
#
# What it asserts, in order:
#   1. Ruby half (always): two `puppet agent -t` runs against the fixture. This
#      proves the stack and the fixture, and yields the oracle's reports.
#   2. voxagent half (only when `bin/voxagent agent --help` works): a wrong
#      --ca-fingerprint is refused and applies nothing; two real runs; run 2
#      sends no certificate request; both agents' run-2 reports are compared
#      with `gatekit compare-reports` through the spine allowlist merged with
#      this gate's own (tests/agent/allowlist.json), and the resource totals
#      are checked against D-75; with the server stopped the agent exits 1.
#
# Why run 2 exits 2 and not 0: the fixture's `notify` resources report a change
# on EVERY run (a notify's event is its message being logged), and Ruby exits 2
# for them too. The check that run 2 is idempotent is therefore "every changed
# resource is a Notify", read from the stored report, not the exit code.
#
# Reports: the server's `store` processor writes Puppet YAML, named by minute,
# so two runs inside one minute overwrite each other. The stored directory is
# emptied before each run and the one file is copied out right after it.
# gatekit reads JSON, so each YAML is converted on the ruby node with
# `YAML.unsafe_load_file(...).to_data_hash` (the same method gatejson.rb
# serialises). The converted JSON holds what the server stored: a binary-encoded
# event value is already Base64 text in the YAML, identically for both agents.
#
#   0  every assertion held, voxagent half included
#   1  an assertion failed -- a result about the agent or the server
#   2  could not run (no podman, no image, stack never ready, or the voxagent
#      `agent` subcommand does not exist yet)
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
GATEKIT="${GATEKIT:-$ROOT/bin/gatekit}"
SPINE_ALLOWLIST="${SPINE_ALLOWLIST:-$ROOT/tests/spine/allowlist.json}"
AGENT_ALLOWLIST="${AGENT_ALLOWLIST:-$HERE/allowlist.json}"
OUT="${OUT:-$ROOT/.test-output/agent}"
READY_TIMEOUT="${READY_TIMEOUT:-180}"

# Named with this script's PID so a concurrent run, or another gate's stack,
# cannot be torn down by this trap.
PROJECT="agentgate$$"
export AGENTGATE_PROJECT="$PROJECT"
SERVER="${PROJECT}_puppet"
RUBY_NODE="${PROJECT}_ruby"
VOX_NODE="${PROJECT}_vox"

SERVER_REPORTS=/opt/puppetlabs/server/data/puppetserver/reports
ACCESS_LOG=/var/log/puppetlabs/puppetserver/puppetserver-access.log
PUPPET_RUBY=/opt/puppetlabs/puppet/bin/ruby
# `puppet agent -t` implies --verbose and --detailed-exitcodes; the report
# holds info-level logs only at that level.
VOX_ARGS=(agent --server puppet --certname vox.example.test --detailed-exitcodes --verbose)
ZERO_FP="$(printf '0%.0s' {1..64})"

FAILS=0
ok() { echo "ok   $*"; }
bad() {
  echo "FAIL $*" >&2
  FAILS=$((FAILS + 1))
}
# holds LABEL CMD... passes when CMD succeeds; lacks LABEL CMD... when it fails.
holds() {
  local label="$1"
  shift
  if "$@" >/dev/null 2>&1; then ok "$label"; else bad "$label"; fi
}
lacks() {
  local label="$1"
  shift
  if "$@" >/dev/null 2>&1; then bad "$label"; else ok "$label"; fi
}
# expect LABEL WANT GOT
expect() { if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1: want $2, got $3"; fi; }

cleanup() {
  podman-compose -p "$PROJECT" -f "$HERE/compose.yml" down -t 1 >/dev/null 2>&1
  podman rm -f "$SERVER" "$RUBY_NODE" "$VOX_NODE" >/dev/null 2>&1
}
trap cleanup EXIT

command -v podman >/dev/null || {
  echo "agent-gate: podman not found" >&2
  exit 2
}
command -v podman-compose >/dev/null || {
  echo "agent-gate: podman-compose not found" >&2
  exit 2
}
command -v jq >/dev/null || {
  echo "agent-gate: jq not found" >&2
  exit 2
}
podman image exists localhost/fleet-node-el:latest ||
  {
    echo "agent-gate: localhost/fleet-node-el:latest missing; run 'task fleet:images'" >&2
    exit 2
  }
[ -x "$GATEKIT" ] || {
  echo "agent-gate: $GATEKIT missing; run 'task build'" >&2
  exit 2
}

rm -rf "$OUT"
mkdir -p "$OUT/ruby" "$OUT/vox"

# gatekit reads one allowlist: the spine's, extended by this gate's. A
# cross_implementation key both name is refused rather than overridden, so
# this gate cannot silently replace a spine reason.
ALLOWLIST="$OUT/allowlist.json"
jq -s 'if ([.[0].cross_implementation | keys[]] - ([.[0].cross_implementation | keys[]] - [.[1].cross_implementation | keys[]]) | length) > 0
       then error("cross_implementation keys in both allowlists") else . end
       | {_comment: (.[0]._comment + .[1]._comment),
          run_to_run: (.[0].run_to_run + .[1].run_to_run),
          cross_implementation: (.[0].cross_implementation + .[1].cross_implementation),
          normalisation: .[0].normalisation}' \
  "$SPINE_ALLOWLIST" "$AGENT_ALLOWLIST" >"$ALLOWLIST" ||
  {
    echo "agent-gate: cannot merge $SPINE_ALLOWLIST and $AGENT_ALLOWLIST" >&2
    exit 2
  }

podman-compose -p "$PROJECT" -f "$HERE/compose.yml" up -d >"$OUT/compose-up.log" 2>&1 ||
  {
    echo "agent-gate: compose up failed:" >&2
    tail -20 "$OUT/compose-up.log" >&2
    exit 2
  }

# Podman healthchecks do not run in this environment, so poll from a node.
ready=""
for ((waited = 0; waited < READY_TIMEOUT; waited += 5)); do
  if podman exec "$RUBY_NODE" curl -ksf https://puppet:8140/status/v1/simple >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 5
done
if [ -z "$ready" ]; then
  echo "agent-gate: server not ready after ${READY_TIMEOUT}s" >&2
  podman logs --tail 20 "$SERVER" >&2
  exit 2
fi
echo "server ready after ~${waited}s"

# access_lines prints the access log length; access_since FROM prints the lines after it.
access_lines() { podman exec "$SERVER" sh -c "wc -l < $ACCESS_LOG"; }
access_since() { podman exec "$SERVER" tail -n "+$(($1 + 1))" "$ACCESS_LOG"; }

# stored_report CERTNAME DEST_PREFIX: copy the one stored report out and convert it.
stored_report() {
  local certname="$1" dest="$2" files
  files="$(podman exec "$SERVER" sh -c "ls $SERVER_REPORTS/$certname/*.yaml 2>/dev/null")"
  if [ "$(wc -w <<<"$files")" != 1 ]; then
    bad "$dest: want exactly one stored report for $certname, found: ${files:-none}"
    return 1
  fi
  podman cp "$SERVER:$files" "$dest.yaml" >/dev/null || {
    bad "$dest: could not copy $files"
    return 1
  }
  podman cp "$dest.yaml" "$RUBY_NODE:/tmp/convert.yaml" >/dev/null || return 1
  podman exec "$RUBY_NODE" "$PUPPET_RUBY" -e \
    'require "puppet"; require "json"; require "yaml"; puts JSON.generate(YAML.unsafe_load_file(ARGV[0]).to_data_hash)' \
    /tmp/convert.yaml >"$dest.json" 2>"$dest.convert.err" || {
    bad "$dest: YAML->JSON conversion failed"
    return 1
  }
}

# check_applied NODE: the fixture's resources are on disk.
check_applied() {
  local node="$1" want got
  holds "$node: content file present" podman exec "$node" test -f /tmp/vox_probe_content
  holds "$node: vox_marker present" podman exec "$node" test -f /tmp/vox_probe_marker
  holds "$node: recursed tree present" podman exec "$node" test -f /tmp/vox_probe_tree/sub/b
  want="$(cat "$HERE/fixture/environments/production/modules/vox_probe/files/x")"
  got="$(podman exec "$node" cat /tmp/vox_probe_source 2>/dev/null)"
  expect "$node: puppet:/// source content" "$want" "$got"
  got="$(podman exec "$node" cat /tmp/vox_probe_content 2>/dev/null)"
  expect "$node: inline content" "inline content from the catalog" "$got"
}

# check_reports LABEL DIR: run 1 shows the plugin fact; run 2 changed only Notify resources.
check_reports() {
  local label="$1" dir="$2" not_notify
  holds "$label: run 1 report has the plugin-fact notify" \
    jq -e '.resource_statuses | has("Notify[fact=from-plugin]")' "$dir/run1.json"
  expect "$label: run 1 report status" changed "$(jq -r .status "$dir/run1.json")"
  not_notify="$(jq -r '.resource_statuses | to_entries[] | select(.value.changed and (.key | startswith("Notify[") | not)) | .key' "$dir/run2.json")"
  expect "$label: run 2 changed only Notify resources" "" "$not_notify"
}

# reset_reports CERTNAME: empty the stored-report dir so the next run leaves exactly one file.
reset_reports() { podman exec "$SERVER" rm -rf "$SERVER_REPORTS/$1"; }

# ---- Ruby half ---------------------------------------------------------------
for run in 1 2; do
  reset_reports ruby.example.test
  podman exec "$RUBY_NODE" /opt/puppetlabs/bin/puppet agent -t --server puppet >"$OUT/ruby/run$run.log" 2>&1
  expect "ruby run $run exit code" 2 "$?"
  stored_report ruby.example.test "$OUT/ruby/run$run"
  [ "$run" = 1 ] && check_applied "$RUBY_NODE"
done
check_reports ruby "$OUT/ruby"

# ---- voxagent half -----------------------------------------------------------
if ! "$VOXAGENT" agent --help >/dev/null 2>&1; then
  echo "voxagent half skipped: agent subcommand missing"
  if [ "$FAILS" -ne 0 ]; then
    echo "agent-gate: $FAILS assertion(s) failed in the Ruby half" >&2
    exit 1
  fi
  exit 2
fi

podman cp "$VOXAGENT" "$VOX_NODE:/usr/local/bin/voxagent" >/dev/null || {
  echo "agent-gate: could not copy voxagent in" >&2
  exit 2
}
vox() { podman exec "$VOX_NODE" voxagent "${VOX_ARGS[@]}" "$@"; }

# Negative: a wrong CA fingerprint on a fresh ssldir is refused before anything
# is trusted, requested or applied.
vox --ssldir /tmp/ssl-fp --ca-fingerprint "$ZERO_FP" >"$OUT/vox/negative-fingerprint.log" 2>&1
expect "vox wrong --ca-fingerprint exit code" 1 "$?"
lacks "vox applied nothing on a wrong fingerprint" podman exec "$VOX_NODE" sh -c 'compgen -G "/tmp/vox_probe_*"'
lacks "vox kept no unverified CA" podman exec "$VOX_NODE" test -e /tmp/ssl-fp/certs/ca.pem
lacks "vox sent no CSR after a wrong fingerprint" podman exec "$SERVER" grep -q 'certificate_request/vox.example.test' "$ACCESS_LOG"

# Positive: two real runs.
for run in 1 2; do
  reset_reports vox.example.test
  mark="$(access_lines)"
  vox >"$OUT/vox/run$run.log" 2>&1
  expect "vox run $run exit code" 2 "$?"
  access_since "$mark" >"$OUT/vox/access-run$run.log"
  stored_report vox.example.test "$OUT/vox/run$run"
  if [ "$run" = 1 ]; then
    check_applied "$VOX_NODE"
    holds "vox run 1 requested a certificate (control for the run 2 check)" \
      grep -q 'PUT /puppet-ca/v1/certificate_request/vox.example.test' "$OUT/vox/access-run1.log"
  else
    lacks "vox run 2 sent no certificate_request" grep -q 'certificate_request' "$OUT/vox/access-run2.log"
  fi
done
check_reports vox "$OUT/vox"
holds "vox run 1 log shows the notify" grep -q 'vox_probe says hello' "$OUT/vox/run1.log"
holds "vox run 1 log shows the plugin fact" grep -q 'fact=from-plugin' "$OUT/vox/run1.log"

# Parity: the second-run reports, through the merged allowlist.
"$GATEKIT" compare-reports "$OUT/ruby/run2.json" "$OUT/vox/run2.json" "$ALLOWLIST" >"$OUT/compare-run2.txt" 2>&1
expect "compare-reports ruby vs vox, run 2" 0 "$?"
tail -3 "$OUT/compare-run2.txt"
# The allowlist drops resources.total (D-75); what remains is asserted here:
# Ruby counts the catalog's resources plus Puppet's six default schedules and
# its default filebucket, voxagent the catalog's alone.
total() { jq '.metrics.resources.values[] | select(.[0] == "total") | .[2]' "$1"; }
ruby_total="$(total "$OUT/ruby/run2.json")"
expect "resources total: voxagent's is Ruby's minus its 7 default resources (D-75)" \
  "$((ruby_total - 7))" "$(total "$OUT/vox/run2.json")"

# The server parsed every report it was sent.
podman logs "$SERVER" >"$OUT/server.log" 2>&1
lacks "server log has no 'Could not' (see $OUT/server.log)" grep -q 'Could not' "$OUT/server.log"

# Negative: server gone.
podman stop -t 1 "$SERVER" >/dev/null 2>&1
vox >"$OUT/vox/negative-server-down.log" 2>&1
expect "vox with the server stopped exit code" 1 "$?"

if [ "$FAILS" -ne 0 ]; then
  echo "agent-gate: $FAILS assertion(s) failed; artifacts in $OUT" >&2
  exit 1
fi
echo "agent-gate: all assertions held"
