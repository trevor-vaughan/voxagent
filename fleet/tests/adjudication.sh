#!/usr/bin/env bash
# Hold the two negative controls' adjudication ordering in place.
#
# This exists because that ordering has already regressed once and nothing in
# the repository noticed. An earlier revision interleaved the controls' status
# guards per half but left both log reads above them; since fleet_log_contains
# exits 2, an unreadable restored log ended the script before the first half's
# verdict was rendered, turning a conclusive exit 1 into exit 2 -- the most
# alarming result these controls can emit, silently downgraded to "could not
# run". It was caught by review, not by a test, and the measurement that
# caught it lived only in a session transcript.
#
# The Venom suite cannot cover this. It runs each control end to end against the
# live fleet, so it only ever exercises the happy path: a green run, all four
# summary lines present. Every case below is a failure path, and most of them
# require a nested gate status the fleet will not produce on demand.
#
# The gap that let the regression through was narrower still. The original
# 22-case check varied ONE axis at a time -- every finding path, every outage
# path, statuses 137 and 130 -- and never crossed them. The defect needed a
# conclusive verdict on one half AND an unreadable log on the other, which no
# single-axis case reaches. The crossed cases are marked CROSS below and are the
# reason this file exists; do not drop them for being redundant-looking.
#
# Method: slice each control from its `BEGIN adjudication` marker to end of file,
# prepend a preamble that injects the four inputs the block reads, and run it.
# That keeps the assertions against the shipped text rather than a copy of it --
# a copy would drift and go on passing.
#
# Exit codes follow the fleet convention: 0 every case behaved, 1 at least one
# did not, 2 the test could not run.
set -uo pipefail
# Inherited noclobber disarms every `: > "$f"` writability probe in this tree.
# Measured: under `set -C`, the probe SUCCEEDS by creating $f, and the redirect
# it was written to predict then fails with "cannot overwrite existing file".
# `env SHELLOPTS=noclobber` reaches here -- confirmed through two process hops
# and a shebang script -- so this is a caller's shell setting, not a hypothetical.
#
# Those probes exist to stop an outage being reported as a finding about the
# fleet; see gate-determinism.sh, which records the DIVERGED-for-a-run-that-never-
# happened measurement that put them there. Leaving noclobber active would let
# that defect back in with the guard still apparently passing.
#
# A gate owns its output directory outright, so clobber-protection guards
# nothing of value here.
set +C

# Not `set -e`. Every case here runs a script that is SUPPOSED to exit non-zero,
# and the whole point is to read those statuses rather than abort on them.

# Resolved absolutely, as everywhere else in this tree: this script locates two
# gate scripts by path. `|| exit 2` because a failing command substitution under
# a later `set -e` exits 1, and 1 here means "a case failed".
TESTS_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 2
BIN_DIR="$(cd "$TESTS_DIR/../bin" && pwd)" || {
  echo "adjudication: cannot locate fleet/bin next to $TESTS_DIR" >&2
  exit 2
}

WORK="$(mktemp -d)" || {
  echo "adjudication: could not create a temporary working directory" >&2
  exit 2
}
# `|| :` on every command: `set -e` is off here, but this trap must not change
# the exit status the script chose, and an unremovable $WORK is not a test
# result.
trap 'rm -rf "$WORK" || :' EXIT

failures=0
cases=0

# slice <control-basename> <preamble-file> <output-file>
#
# Extract the adjudication block from a shipped control and make it runnable.
# The marker is required, not optional: without it sed emits nothing and every
# case below would "pass" against an empty script, which is the vacuous-assertion
# failure this tree tracks. So its absence is could-not-run, not a finding.
slice() {
  local control="$1" preamble="$2" out="$3" src="$BIN_DIR/$1"

  if [ ! -r "$src" ]; then
    echo "adjudication: cannot read ${src}" >&2
    exit 2
  fi
  if ! grep -q '^# --- BEGIN adjudication' "$src"; then
    echo "adjudication: ${control} has no 'BEGIN adjudication' marker;" \
         "the slice below would be empty and every case would pass vacuously" >&2
    exit 2
  fi

  cat "$preamble" > "$out" || exit 2
  sed -n '/^# --- BEGIN adjudication/,$p' "$src" >> "$out" || exit 2
}

# run_case <description> <expected-exit> <script> <args>...
#
# Exit status ONLY. The transcript is asserted separately, by assert_prints and
# by the explicit absence check further down, and both are needed: status alone
# would not have caught the transcript half of the regression, where that
# revision returned a defensible code on some paths while printing nothing at
# all.
run_case() {
  local desc="$1" want="$2"; shift 2
  local out got
  cases=$((cases + 1))

  out="$(bash "$@" 2>&1)"
  got=$?

  if [ "$got" -ne "$want" ]; then
    echo "FAIL ${desc}: expected exit ${want}, got ${got}" >&2
    printf '%s\n' "$out" | sed 's/^/       /' >&2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc} (exit ${got})"
}

# run_case_unwritable <description> <script> <args>...
#
# The same block with stdout on a device whose every write fails. A failed
# `echo` under `set -e` exits 1, and 1 is a finding about the fleet in every
# gate here, so an unwritable stdout used to turn a green run into an
# accusation. Measured on gate-determinism.sh before the `|| exit 2` guards
# existed: a run that was otherwise green exited 1.
#
# Not routed through run_case: that captures stdout in a command substitution,
# which is exactly the redirection this case needs to control.
run_case_unwritable() {
  local desc="$1"; shift
  local got
  cases=$((cases + 1))

  bash "$@" >/dev/full 2>/dev/null
  got=$?

  if [ "$got" -ne 2 ]; then
    echo "FAIL ${desc}: expected exit 2 with stdout unwritable, got ${got}" >&2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc} (exit 2)"
}

# run_case_unwritable_stderr <description> <script> <args>...
#
# The other half of the write-guard story, and the half the commit that added
# these guards called the worse one: `echo "... could not complete" >&2` followed
# by `exit 2` exits **1** when stderr fails, so the outage announcement destroys
# the status it was about to set. run_case_unwritable sends stderr to /dev/null
# and so proves nothing about those guards. Measured on the finding path with
# stderr on /dev/full: exit 2 as shipped, exit 1 with the guard removed.
run_case_unwritable_stderr() {
  local desc="$1"; shift
  local got
  cases=$((cases + 1))

  bash "$@" >/dev/null 2>/dev/full
  got=$?

  if [ "$got" -ne 2 ]; then
    echo "FAIL ${desc}: expected exit 2 with stderr unwritable, got ${got}" >&2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc} (exit 2)"
}

# assert_prints <description> <substring> <script> <args>...
assert_prints() {
  local desc="$1" needle="$2"; shift 2
  local out
  cases=$((cases + 1))

  out="$(bash "$@" 2>&1)"
  case "$out" in
    *"$needle"*) echo "ok   ${desc} (printed '${needle}')" ;;
    *) echo "FAIL ${desc}: '${needle}' absent from the transcript" >&2
       printf '%s\n' "$out" | sed 's/^/       /' >&2
       failures=$((failures + 1)) ;;
  esac
}

# assert_absent <description> <substring> <script> <args>...
#
# The mirror of assert_prints, and it needs its own shape rather than a flag: a
# `case` glob with an empty needle matches everything, so an absence assertion
# written as assert_prints with "" would pass over anything at all. That is the
# vacuous-assertion class this tree tracks, so the empty needle is rejected as
# could-not-run rather than quietly accepted.
assert_absent() {
  local desc="$1" needle="$2"; shift 2
  local out
  cases=$((cases + 1))

  if [ -z "$needle" ]; then
    echo "adjudication: assert_absent needs a non-empty needle;" \
         "an empty one matches everything and would pass vacuously" >&2
    exit 2
  fi

  out="$(bash "$@" 2>&1)"
  case "$out" in
    *"$needle"*) echo "FAIL ${desc}: '${needle}' present when it must not be" >&2
                 printf '%s\n' "$out" | sed 's/^/       /' >&2
                 failures=$((failures + 1)) ;;
    *) echo "ok   ${desc} (no '${needle}')" ;;
  esac
}

# assert_all_writes_guarded <description> <sliced-block>
#
# Structural, not behavioural, and deliberately so. The unwritable-stdout cases
# can only ever reach the FIRST write: /dev/full fails it, the guard exits 2, and
# every later guard in the block is untested. Measured — dropping the guard on
# the first echo fails two of the behavioural cases; dropping it on any of the
# other nine fails none of them except the stderr write that
# run_case_unwritable_stderr drives, which is the gap this helper covers. Nine,
# not three: this helper now examines ten writes per block, and the sentence said
# three for as long as it examined eight. Making a
# later write fail while earlier ones succeed is not cleanly
# constructible, so the remaining guards are asserted by reading the shipped text
# instead.
#
# Continuation-aware: several writes here span two lines with the guard on the
# second, and a per-line grep would report every one of them as unguarded.
#
# The write need not open the line, and anchoring on `^[[:space:]]*` is why this
# assertion was weaker than its name. That anchor skipped the two `*) echo`
# contract-violation writes in each block -- the writes the per-half contract
# check itself added -- so their guards could be removed with all cases still
# green. Measured: with them stripped, a nested status of 137 and an unwritable
# stderr exits 1 where the shipped block exits 2, an outage announced as a
# finding. The same anchor also missed `[ -r "$prep_err" ] && cat "$prep_err"
# >&2` -- quoted verbatim, because this tree's rule is to grep for the construct
# and a paraphrase cannot be grepped -- the shape
# review found by hand in gate-implementations.sh; measured against the file as
# it stood before that fix landed, the old anchor passes it and the widened one
# reports it. This
# helper is never pointed at that file -- the point is that the shape is now
# mechanically visible rather than only findable by reading.
#
# `|| :` is NOT a guard here. It swallows a failed write where `|| exit 2`
# reports could-not-run, and a block whose whole purpose is telling those two
# apart cannot accept them as equivalent. The controls' EXIT traps do use `|| :`,
# correctly, but they sit above the BEGIN marker and never reach this helper --
# measured: zero occurrences of `|| :` in either sliced block, so exempting it
# protected nothing and left every later stdout write downgradable for free.
assert_all_writes_guarded() {
  local desc="$1" file="$2" bad
  cases=$((cases + 1))

  bad="$(awk '
    /^[[:space:]]*#/ { next }
    { line = (buf == "" ? $0 : buf " " $0) }
    /\\[[:space:]]*$/ { buf = line; next }
    { buf = ""
      # Trailing comments are stripped before matching. Without this a line whose
      # comment merely mentions `; echo ` is reported as an unguarded write --
      # measured, a false positive, and a check that cries wolf gets widened
      # until it stops catching things.
      probe = line
      sub(/[[:space:]]#[^"]*$/, "", probe)
      # The guard must CLOSE the line, not merely appear on it. As a substring
      # test, one guarded write immunised every other write sharing its logical
      # line: measured, `echo "a" >&2; echo "b" >&2 || exit 2` was certified
      # clean with the first write bare.
      if (probe ~ /(^|[;&|)]|[[:space:]](then|do|else|\{))[[:space:]]*!?[[:space:]]*(echo|printf|sed|cat)([[:space:]]|>)/ &&
          probe !~ /\|\|[[:space:]]*exit[[:space:]]+2[[:space:]]*;?[[:space:]]*$/)
        printf "  %d: %s\n", NR, line
    }' "$file")"

  if [ -n "$bad" ]; then
    echo "FAIL ${desc}: unguarded write(s)" >&2
    printf '%s\n' "$bad" >&2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc}"
}

# /dev/full is how the unwritable-stdout cases fail every write. Its absence is
# could-not-run rather than a skip: a skipped case reads as a passing one in the
# summary line, and this file exists because a gap in coverage went unnoticed.
if [ ! -w /dev/full ]; then
  echo "adjudication: /dev/full is missing or not writable;" \
       "the unwritable-stdout cases cannot run" >&2
  exit 2
fi

# Every preamble generated below sources gate-common.sh, and the generated
# scripts run under `set -euo pipefail`, so a load failure exits them with 1 --
# which is this file's own code for "a case failed". Measured with it
# unreachable: 22 of the 46 cases pass vacuously, and the 24 that fail blame the
# status they were given rather than the load, so the run looks like a broken
# fixture rather than a missing file.
#
# Checked here as a precondition rather than guarded inside each preamble. A
# guard there would make the blocks exit 2, and EIGHTEEN of the cases below
# assert exit 2 -- twelve `run_case` invocations plus the six
# `run_case_unwritable*` ones, whose helpers hardcode it -- so the vacuous passes
# would move rather than go away. Measured under that counterfactual: 24 pass,
# two more than the 22 the precondition replaced.
#
# Run in a FRESH shell with the preambles' own options, not in a `( )` subshell
# of this one. Two separate faults made the subshell form blind, and either alone
# was enough:
#
#   - `if ! ( ... )` puts the subshell in a condition context, and bash suppresses
#     errexit inside it. Measured: `if ! ( false; echo REACHED )` prints REACHED
#     and the subshell succeeds. Adding `set -e` inside the parens does not help;
#     the suppression propagates in. This extends the rule already recorded for
#     the left operand of `||` -- condition contexts suppress it too, and through
#     a subshell boundary.
#   - gate-common.sh ends in a `:` sentinel that pins its status to 0 on purpose,
#     so a probe whose only criterion is that status reads a value engineered to
#     be constant.
#
# With both faults this check saw a missing file and a syntax error and nothing
# else. Measured against a gate-common.sh that loads but whose top level fails:
# 22 of 46 cases passed vacuously at exit 1 -- bit for bit the signature this
# precondition exists to eliminate.
#
# The sentinel immunises the nine guarded sourcers, whose `. x || { ...; }` puts
# the source on the left of `||`. This file emits a BARE source into its
# preambles, under `set -euo pipefail`, so it is the one sourcer the sentinel
# does not cover -- and the only one that has to check for itself. `bash -euo
# pipefail -c` reproduces that environment exactly rather than approximating it.
#
# stderr is captured rather than discarded: for the syntax-error case bash's
# parse diagnostic is the only thing the operator can act on.
if ! load_err="$(bash -euo pipefail -c '. "$1"' _ "$BIN_DIR/gate-common.sh" 2>&1)"; then
  echo "adjudication: ${BIN_DIR}/gate-common.sh cannot be loaded; every sliced" \
       "block sources it bare and would exit 1, which is this file's code for a" \
       "failed case${load_err:+ -- }${load_err}" >&2 || exit 2
  exit 2
fi

# An unreadable log is a path under a directory that does not exist, NOT a
# chmod 000 file. grep exits 2 for both, but root bypasses DAC and this tree has
# already been bitten by a fixture that proved nothing when run as root. A
# missing parent is unreadable for every user there is.
UNREADABLE="$WORK/no-such-dir/absent.log"

# Every fixture write below carries `|| exit 2`. This file runs `set -uo
# pipefail` and deliberately not `set -e`, so a failed redirect does NOT abort --
# it leaves a missing or empty fixture, the cases using it disagree with their
# expected status, and the script exits 1. Measured with one fixture redirected
# at an unwritable path: exit 1, seven cases "failing", every message blaming a
# status the control never produced. A harness that cannot build its inputs has
# not found anything about the controls, and 1 here means it has.

printf 'unenforced\n'                                 > "$WORK/broken-detected.log"    || exit 2
printf 'compliance_marker = enforced-by-disa_stig\n'  > "$WORK/restored-enforced.log"  || exit 2
printf 'DIVERGED\n'                                   > "$WORK/mutated-diverged.log"   || exit 2
printf 'IDENTICAL\n'                                  > "$WORK/restored-identical.log" || exit 2
printf 'IDENTICAL\nDIVERGED\n'                        > "$WORK/restored-both.log"      || exit 2
# Check 6's FAILURE message, which names the enforced value it did not find. The
# compliance control matches on `compliance_marker = <value>` precisely so this
# log reads as red; loosen the pattern to the bare value and the control reports
# green over a still-red gate. Measured: exit 1 as shipped, exit 0 loosened.
printf "compliance_marker: expected 'enforced-by-disa_stig', got 'unenforced'\n" \
                                                      > "$WORK/restored-failmsg.log" || exit 2
# Check 6's SUCCESS line -- what a check-6 run that detected NOTHING prints. The
# broken half matches on `unenforced` so this log reads as "did not detect";
# loosen it to the bare `enforced`, which this line contains, and the control
# reports green over a gate that saw nothing. Measured: exit 1 as shipped, exit 0
# loosened. This is the mirror of restored-failmsg.log above, and its absence is
# why that loosening survived: the broken half's only wrong-reason fixture was
# empty.log, which contains neither the pattern nor anything close to it and so
# cannot tell a tight pattern from a loose one.
printf 'fleet_profile::compliance_marker = enforced-by-disa_stig\n' \
                                                      > "$WORK/broken-successline.log" || exit 2
: > "$WORK/empty.log" || exit 2

cat > "$WORK/preamble-compliance.sh" <<PREAMBLE
#!/usr/bin/env bash
set -euo pipefail
. "$BIN_DIR/gate-common.sh"
BROKEN_LOG="\$1"
RESTORED_LOG="\$2"
broken_status="\$3"
restored_status="\$4"
PREAMBLE

cat > "$WORK/preamble-determinism.sh" <<PREAMBLE
#!/usr/bin/env bash
set -euo pipefail
. "$BIN_DIR/gate-common.sh"
MUTATED_LOG="\$1"
RESTORED_LOG="\$2"
mutated_status="\$3"
restored_status="\$4"
PREAMBLE

slice gate-compliance-negative.sh  "$WORK/preamble-compliance.sh"  "$WORK/compliance.sh"
slice gate-determinism-negative.sh "$WORK/preamble-determinism.sh" "$WORK/determinism.sh"

C="$WORK/compliance.sh"
D="$WORK/determinism.sh"
B="$WORK/broken-detected.log"
R="$WORK/restored-enforced.log"
M="$WORK/mutated-diverged.log"
I="$WORK/restored-identical.log"
E="$WORK/empty.log"

echo "== check 6's control (gate-compliance-negative.sh) =="
run_case "green: detected then recovered"              0 "$C" "$B" "$R" 1 0
run_case "check 6 went red for another reason"         1 "$C" "$E" "$R" 1 0
run_case "broken half could not run"                   2 "$C" "$B" "$R" 2 0
run_case "restored half could not run"                 2 "$C" "$B" "$R" 1 2
run_case "check 6 stayed red after restoration"        1 "$C" "$B" "$R" 1 1
run_case "restored log lacks the success line"         1 "$C" "$B" "$E" 1 0
run_case "broken run outside the 0/1/2 contract (137)" 2 "$C" "$B" "$R" 137 0
run_case "unreadable broken log is an outage"          2 "$C" "$UNREADABLE" "$R" 1 0
run_case "unreadable restored log is an outage"        2 "$C" "$B" "$UNREADABLE" 1 0
# The two that regressed. broken_exit=0 means check 6 stayed GREEN with the
# compliance data renamed away; that verdict is conclusive on its own and must
# not be downgraded by anything the restored half does or fails to do.
run_case "CROSS conclusive broken=0 + unreadable restored log" 1 "$C" "$B" "$UNREADABLE" 0 0
run_case "CROSS conclusive broken=0 + restored could not run"  1 "$C" "$B" "$UNREADABLE" 0 2
assert_prints "conclusive broken=0 still reaches the transcript" \
              "broken_exit=0" "$C" "$B" "$UNREADABLE" 0 0
assert_prints "green run prints restored_exit" \
              "restored_exit=0" "$C" "$B" "$R" 1 0
run_case "restored log carries only check 6's FAILURE message" \
                                                       1 "$C" "$B" "$WORK/restored-failmsg.log" 1 0
run_case "broken log carries only check 6's SUCCESS line" \
                                                       1 "$C" "$WORK/broken-successline.log" "$R" 1 0
run_case "restored run outside the 0/1/2 contract (143)" 2 "$C" "$B" "$R" 1 143
run_case "CROSS conclusive broken=0 + restored outside contract" 1 "$C" "$B" "$R" 0 137
# A reading must never be printed for a run whose result is about to be thrown
# away, nor for a log that was never read. The first of these is the case the
# original regression actually produced, and it needs READABLE logs to reach --
# with an unreadable one the helper exits before printing either way, so the
# unreadable variant alone does not pin it.
assert_absent "no restored_enforced reading when the restored run returned 2" \
              "restored_enforced=" "$C" "$B" "$R" 1 2
assert_absent "no broken_reported reading when the broken run returned 2" \
              "broken_reported=" "$C" "$B" "$R" 2 0
assert_absent "no restored_enforced reading over an unread log" \
              "restored_enforced=" "$C" "$B" "$UNREADABLE" 1 0
run_case_unwritable "green inputs, stdout unwritable" "$C" "$B" "$R" 1 0
run_case_unwritable "finding path, stdout unwritable" "$C" "$E" "$R" 1 0
run_case_unwritable_stderr "finding path, stderr unwritable" "$C" "$E" "$R" 1 0
assert_all_writes_guarded "every write in check 6's block is guarded" "$C"

echo "== check 1's control (gate-determinism-negative.sh) =="
run_case "green: detected then recovered"              0 "$D" "$M" "$I" 1 0
run_case "check 1 went red for another reason"         1 "$D" "$E" "$I" 1 0
run_case "mutated half could not run"                  2 "$D" "$M" "$I" 2 0
run_case "restored half could not run"                 2 "$D" "$M" "$I" 1 2
run_case "restored log lacks IDENTICAL"                1 "$D" "$M" "$E" 1 0
run_case "restored log carries BOTH markers"           1 "$D" "$M" "$WORK/restored-both.log" 1 0
run_case "mutated run outside the 0/1/2 contract (130)" 2 "$D" "$M" "$I" 130 0
run_case "unreadable mutated log is an outage"         2 "$D" "$UNREADABLE" "$I" 1 0
# The `&&`-with-`!` pairing: an unreadable restored log has two ways to be
# misread at once, and must be neither.
run_case "unreadable restored log is an outage"        2 "$D" "$M" "$UNREADABLE" 1 0
run_case "CROSS conclusive mutated=0 + unreadable restored log" 1 "$D" "$M" "$UNREADABLE" 0 0
run_case "CROSS conclusive mutated=0 + restored could not run"  1 "$D" "$M" "$UNREADABLE" 0 2
assert_prints "conclusive mutated=0 still reaches the transcript" \
              "mutated_exit=0" "$D" "$M" "$UNREADABLE" 0 0
assert_prints "green run prints restored_exit" \
              "restored_exit=0" "$D" "$M" "$I" 1 0
run_case "restored run outside the 0/1/2 contract (143)" 2 "$D" "$M" "$I" 1 143
run_case "CROSS conclusive mutated=0 + restored outside contract" 1 "$D" "$M" "$I" 0 137
assert_absent "no restored_identical reading when the restored run returned 2" \
              "restored_identical=" "$D" "$M" "$I" 1 2
assert_absent "no mutated_diverged reading when the mutated run returned 2" \
              "mutated_diverged=" "$D" "$M" "$I" 2 0
assert_absent "no restored_identical reading over an unread log" \
              "restored_identical=" "$D" "$M" "$UNREADABLE" 1 0
run_case_unwritable "green inputs, stdout unwritable" "$D" "$M" "$I" 1 0
run_case_unwritable "finding path, stdout unwritable" "$D" "$E" "$I" 1 0
run_case_unwritable_stderr "finding path, stderr unwritable" "$D" "$E" "$I" 1 0
assert_all_writes_guarded "every write in check 1's block is guarded" "$D"

echo
if [ "$failures" -ne 0 ]; then
  echo "adjudication: ${failures} of ${cases} cases failed" >&2
  exit 1
fi

# The count is asserted, not just reported. Deleting a case would otherwise still
# print "all as expected" with a smaller number -- the same "a skipped case reads
# as a passing one" hazard the /dev/full precondition above refuses to accept.
# Hardcoded on purpose: a count derived from the run it is checking agrees with
# whatever that run did.
# Adding or removing a case means editing this number.
if [ "$cases" -ne 46 ]; then
  echo "adjudication: expected 46 cases, ran ${cases};" \
       "update this number deliberately when adding or removing one" >&2
  exit 1
fi
echo "adjudication: ${cases} cases, all as expected"
exit 0
