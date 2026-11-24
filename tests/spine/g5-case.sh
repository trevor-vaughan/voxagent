# shellcheck shell=bash
# Sourced by report-diff-container.sh: the per-case judging its concat half
# (concat/g5.sh) and augeas half (augeas/g5.sh) share. Uses that script's HERE
# and GATEKIT. OUT is a case's directory, holding AGENT-RUN.{out,json,rc} for
# AGENT ruby and voxagent; an empty .json is a run that wrote no report. LABEL
# prefixes every message.

# g5_judge OUT LABEL RUN: compares the two reports of RUN. Prints one line;
# returns 0 agree, 1 diverge, 2 could not run.
g5_judge() {
  local out="$1" label="$2" run="$3" rc order_rc
  if [ ! -s "$out/ruby-$run.json" ]; then
    echo "report-diff-container: $label $run: Ruby wrote no report:" >&2
    sed -n '1,10p' "$out/ruby-$run.out" >&2
    return 2
  fi
  if [ ! -s "$out/voxagent-$run.json" ]; then
    echo "report-diff-container: $label $run: voxagent wrote no report:" >&2
    sed -n '1,10p' "$out/voxagent-$run.out" >&2
    return 1
  fi
  "$GATEKIT" compare-reports "$out/ruby-$run.json" "$out/voxagent-$run.json" \
    "$HERE/allowlist.json" >"$out/compare-$run.txt" 2>&1
  rc=$?
  "$GATEKIT" compare-order "$out/ruby-$run.json" "$out/voxagent-$run.json" >"$out/order-$run.txt" 2>&1
  order_rc=$?
  # A clean rerun logs at no resource on either side: no order to compare.
  if [ "$order_rc" -eq 2 ] && grep -q '^gatekit compare-order: compared 0 of 0 logging resources; nothing to compare$' \
    "$out/order-$run.txt"; then
    order_rc=0
  fi
  [ "$rc" -le 2 ] || rc=2
  [ "$order_rc" -le 2 ] || order_rc=2
  [ "$order_rc" -gt "$rc" ] && rc=$order_rc
  case "$rc" in
    0) echo "report-diff-container: $label $run: agree" ;;
    1)
      echo "report-diff-container: $label $run: DIVERGE" >&2
      sed -n '1,30p' "$out/compare-$run.txt" "$out/order-$run.txt" >&2
      ;;
    *)
      echo "report-diff-container: $label $run: could not compare" >&2
      sed -n '1,10p' "$out/compare-$run.txt" "$out/order-$run.txt" >&2
      ;;
  esac
  return "$rc"
}

# g5_refusal OUT LABEL: compares a catalog Ruby refuses at conversion: Ruby's
# `Error: Could not run: <detail> (file: ..., line: N)` against voxagent's
# first `voxagent: <detail>`, exit 2, and no report from either.
g5_refusal() {
  local out="$1" label="$2" want got
  want="$(sed -n 's/^Error: Could not run: \(.*\) (file: [^)]*, line: [0-9]*)$/\1/p' "$out/ruby-live.out")"
  if [ -z "$want" ] || [ -s "$out/ruby-live.json" ]; then
    echo "report-diff-container: $label: Ruby did not refuse the catalog:" >&2
    sed -n '1,10p' "$out/ruby-live.out" >&2
    return 2
  fi
  got="$(grep '^voxagent: ' "$out/voxagent-live.out" |
    grep -Ev '^voxagent: ([0-9]+ invalid resource\(s\); nothing applied|outcome=.*)$' |
    sed -n '1s/^voxagent: //p')"
  if [ "$got" != "$want" ] || [ -s "$out/voxagent-live.json" ] || [ "$(cat "$out/voxagent-live.rc")" != 2 ]; then
    echo "report-diff-container: $label: DIVERGE: Ruby refuses with" >&2
    echo "  $want" >&2
    echo "report-diff-container: voxagent (exit $(cat "$out/voxagent-live.rc")) printed:" >&2
    sed -n '1,10p' "$out/voxagent-live.out" >&2
    return 1
  fi
  echo "report-diff-container: $label: both refuse: $want"
}
