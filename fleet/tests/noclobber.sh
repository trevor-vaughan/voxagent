#!/usr/bin/env bash
# Prove this tree's writability probes still work under an inherited noclobber.
#
# Every gate opens its log with a `: > "$f"` probe before writing to it. Those
# probes are not decoration: gate-determinism.sh records the measurement that
# put them there -- with the output directory made read-only after entry, two
# nodes were reported DIVERGED for a comparison that never executed, so the
# suite would have gone red naming the fleet for an outage on the machine
# running it.
#
# Under `set -C` that guard inverts. A `: > "$f"` on a path that does not exist
# SUCCEEDS, creating the file, and the redirect it was written to predict then
# fails with "cannot overwrite existing file". The probe reports the write is
# safe at exactly the moment it stops being safe, and `set -euo pipefail` turns
# the failed redirect into exit 1 -- a finding about the fleet, for a condition
# that is entirely local to the operator's shell.
#
# `env SHELLOPTS=noclobber` reaches a shebang script through two process hops;
# measured, not assumed. So this is a setting a caller can really arrive with.
#
# Two halves, because either alone would be a test that cannot fail:
#
#   1. The mechanism. Build the probe-then-write pattern in a temp script and
#      run it under noclobber with and without `set +C`. Without the fix it must
#      FAIL; with it, succeed. A structural check alone would pass just as well
#      against a `set +C` that did nothing.
#   2. The coverage. Every executable shell script in the tree carries `set +C`.
#      The behavioural check alone would pass while a new gate quietly omitted
#      it, which is precisely how this class of guard rots.
#
#   0  the mechanism holds and every script is covered
#   1  a result about this tree -- the fix is absent, ineffective, or incomplete
#   2  the check could not run
set -uo pipefail
set +C

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT

failures=0
cases=0

# --- 1. The mechanism ---------------------------------------------------------
#
# `env` rather than a bare assignment: SHELLOPTS is readonly inside bash, so
# `SHELLOPTS=noclobber bash -c ...` fails to set it in the parent and silently
# tests nothing. Measured -- the first version of this file did exactly that and
# reported the child as unaffected.

write_probe_script() {
  # $1 destination, $2 either "guarded" or "bare"
  #
  # Single quotes throughout the body, and that is the point: this is writing a
  # script, so $f and $1 must reach the generated file unexpanded. Expanding
  # them here would bake this script's own variables in and the generated probe
  # would test nothing.
  # shellcheck disable=SC2016
  {
    echo '#!/usr/bin/env bash'
    echo 'set -euo pipefail'
    [ "$2" = "guarded" ] && echo 'set +C'
    echo 'f="$1"'
    echo ': > "$f" || { echo "probe-failed" >&2; exit 2; }'
    echo 'echo payload > "$f"'
    echo 'echo wrote'
  } > "$1" || return 2
  chmod +x "$1" || return 2
}

run_case() {
  # $1 label, $2 guarded|bare, $3 expected exit status
  cases=$((cases + 1))
  local script="$WORK/probe-$2.sh" target="$WORK/target-$2-$cases"
  if ! write_probe_script "$script" "$2"; then
    echo "noclobber: could not write the $2 probe script" >&2
    exit 2
  fi
  env SHELLOPTS=noclobber "$script" "$target" >/dev/null 2>&1
  local got=$?
  if [ "$got" -ne "$3" ]; then
    echo "  FAIL $1: exit $got, expected $3" >&2
    failures=$((failures + 1))
  fi
}

# Without `set +C` the redirect after a passing probe must fail. If this case
# stops failing, noclobber no longer behaves as described and the rest of this
# file is asserting something that cannot happen.
run_case "bare script under noclobber must fail its redirect" bare 1

# With it, the same script completes.
run_case "guarded script under noclobber must succeed" guarded 0

# --- 2. The coverage ----------------------------------------------------------
#
# Every executable shell script in the tree must carry `set +C`. A new gate that
# forgets it reopens the hole with every existing test still green, because no
# behavioural test exercises a script that does not exist yet.

while IFS= read -r script; do
  cases=$((cases + 1))
  if ! head -1 "$script" | grep -q 'bash'; then
    continue
  fi
  if ! grep -q '^set +C' "$script"; then
    echo "  FAIL ${script#"$ROOT"/}: no 'set +C'; an inherited noclobber would" \
         "disarm its writability probes" >&2
    failures=$((failures + 1))
  fi
done < <(find "$ROOT/fleet/bin" "$ROOT/fleet/tests" "$ROOT/scripts" \
              -type f -perm -u+x -print 2>/dev/null | sort)

if [ "$cases" -lt 3 ]; then
  echo "noclobber: only $cases cases ran; the script list came back empty" >&2
  exit 2
fi

if [ "$failures" -ne 0 ]; then
  echo "noclobber: $cases cases, $failures FAILED" >&2
  exit 1
fi
echo "noclobber: $cases cases, all as expected"
