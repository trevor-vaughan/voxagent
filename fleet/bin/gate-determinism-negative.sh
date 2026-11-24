#!/usr/bin/env bash
# Proves gate check 1 can go red.
#
# This is a test OF a gate, not a gate. Checks 1, 2, 3 and 5 all report
# agreement, and a determinism gate that has never been observed red is
# indistinguishable from one that compares nothing at all -- which is the exact
# failure mode this harness exists to prevent. It was proved once by hand
# and reverted; nothing re-proved it, so this does, on every run.
#
# Injects a genuinely nondeterministic value into the fixture manifest, runs
# check 1, restores the manifest, and runs check 1 again. Both halves matter:
# the first shows the gate can detect, the second shows this script left
# nothing behind.
#
# Exit codes follow the fleet convention: 0 the gate went red under mutation
# and green again after restoration, 1 it did not (a result, and an alarming
# one -- the gate is not measuring what it claims), 2 the check could not run.
set -euo pipefail
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

OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-determinism-negative.sh <out-dir>" >&2 || exit 2
  exit 2
fi

# Resolved once and absolutely. gate-determinism-negative.sh invokes another gate
# by path, and gate-exports.sh resolves voxdiff and the two check-4 wrappers the
# same way, so a relative $0 breaks those callers depending on the caller's cwd.
# `|| exit 2` because a failing command substitution under `set -e` exits 1, and
# 1 is a result about the fleet in every gate here.
BIN_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 2

# Guarded, not bare. `. missing-file` under `set -e` exits 1, so a gate-common.sh
# that failed to load would be reported as a divergence, an instability finding
# or a collapsed hiera axis depending on which script hit it first -- the eighth
# channel of the same defect; more were found afterwards in redirect targets.
#
# Safe against a false positive: this reads the sourced file's own exit status,
# and gate-common.sh ends in an explicit `:` sentinel that pins that to 0.
# shellcheck source=gate-common.sh
. "$BIN_DIR/gate-common.sh" || {
  echo "gate: cannot load ${BIN_DIR}/gate-common.sh" >&2 || exit 2
  exit 2
}

fleet_require_writable_dir "$OUT_DIR"

# Checked before anything is backed up or mutated, with the other could-not-run
# checks: gatekit performs the mutation, and a missing or stale one must stop
# this control while the fixture is still intact.
fleet_require_gatekit

MANIFEST="$BIN_DIR/../code/environments/production/modules/fleet_profile/manifests/init.pp"
# "missing or not writable": -w is false in both cases, and naming only the
# second would send a reader hunting for a permission problem on a file that
# is not there at all.
if [ ! -w "$MANIFEST" ]; then
  echo "negative-control: ${MANIFEST} is missing or not writable" >&2 || exit 2
  exit 2
fi

# Matched in full rather than by pattern. A fuzzy match could silently rewrite
# a different line after a refactor, and a mutation that landed somewhere
# unintended would still make the gate go red -- passing this check while
# proving nothing about the line it names.
#
# The ${...} below are Puppet interpolations in the manifest's own source text,
# not shell expansions, so the single quotes are what make this work at all --
# SC2016's advice is inverted here. A shellcheck directive covers only the next
# command, hence one per assignment.
# shellcheck disable=SC2016
ORIGINAL='  message => "${message}/${is_me}/${is_not_me}",'
# shellcheck disable=SC2016
MUTATED='  message => "${message}/${is_me}/${is_not_me}/${Timestamp.new}",'

# Both guarded, matching every other `mktemp` in the gate scripts: bare under
# `set -e` these exit 1, and 1 is this script's code for "the gate is not
# measuring what it claims" -- a far stronger claim than "no temp file".
BACKUP="$(mktemp)" || {
  echo "negative-control: could not create a temporary backup file" >&2 || exit 2
  exit 2
}
cp "$MANIFEST" "$BACKUP" || {
  echo "negative-control: could not back up ${MANIFEST} to ${BACKUP}" >&2 || exit 2
  # `|| :`, because `set -e` is NOT suppressed inside this group. Only the left
  # operand of `||` is exempt; the group on the right runs under errexit as
  # normal, so a failing `rm` aborts one line above the `exit 2` below and the
  # shell exits 1 instead. Measured: with a directory at $BACKUP this reported 1
  # -- "check 1 is not measuring what it claims" -- for a backup that could not
  # be taken. Not fleet_clear_artefacts: that helper exits 2 itself, and its
  # message names a previous run's artefacts, which this is not.
  rm -f "$BACKUP" || :
  exit 2
}

# Belt and braces. The normal path restores explicitly and checks the result,
# but an interrupt between the mutation and that point would otherwise leave a
# nondeterministic manifest bind-mounted into all three containers, poisoning
# every later check in the suite.
#
# Set only after the backup succeeds: armed any earlier it would copy an empty
# or partial $BACKUP over the manifest on the way out.
#
# `|| :` on BOTH commands is load-bearing, not decoration. `set -e` applies
# inside an EXIT trap, so a failing cp aborts the trap there, `rm -f` never runs,
# and the shell exits 1 -- rewriting this script's exit 2 into its code for
# "check 1 failed to detect". The path that does it is the one that matters
# most: an explicit restore that fails exits 2, then the trap's cp fails for the
# same reason and the one run where the fixture really is poisoned reports an
# alarming finding instead of "could not run", leaking $BACKUP as it goes.
#
# The `rm` needs it for the same reason, one command later. It is the trap's
# LAST command, so under `set -e` its failure is simply what the shell exits
# with. Measured with an unremovable $BACKUP, before this guard existed:
#
#   requested exit 0 (green) -> 1     requested exit 2 -> 1
#
# The green case is the worst result this tree can produce. The run printed
# mutated_diverged=yes and restored_identical=yes -- a clean pass, the gate
# proven able to detect -- and exited 1, which every consumer reads as "the gate
# is not measuring what it claims", contradicted by the script's own stdout with
# nothing to arbitrate between them.
#
# `|| :` and not fleet_clear_artefacts, which every other gate now uses: that
# helper calls `exit` on failure, and an `exit` inside an EXIT trap overwrites
# the very status the trap exists to preserve -- swapping one shape of "an
# outage reported as a finding" for another. Its message would be wrong here
# too, since $BACKUP is this run's own temp file, not a previous run's artefact.
#
# The cp's `|| :` is the one place here that hides something material, so it
# announces the failure before swallowing it. Two paths reach this trap with the
# manifest still mutated: the explicit restore below already failed, which
# printed its own message and exited 2, or a signal or an `exit 2` landed between
# the mutation and that restore -- including the exit 2 from the $MUTATED_LOG
# probe added below. On that second path this cp is the ONLY restore attempt
# there will be, and a silent failure leaves a nondeterministic manifest
# bind-mounted into three containers, poisoning every later check in the suite:
# precisely what the paragraph above says this trap exists to prevent.
#
# Chained with `||`, deliberately not a `{ ... }` group. A group on the right of
# `||` runs under `set -e`, which is the trap-abort mechanism already corrected
# once here. The trailing `|| :` catches a failing echo so the status stays
# pinned; verified to preserve 0 -> 0, 2 -> 2 and 1 -> 1 with $BACKUP unremovable.
trap 'cp -f "$BACKUP" "$MANIFEST" >/dev/null 2>&1 \
        || echo "negative-control: EXIT restore of ${MANIFEST} FAILED -- the fixture may be poisoned" >&2 \
        || :; rm -f "$BACKUP" || :' EXIT

set +e
# gatekit refuses, changing nothing, unless the target line occurs exactly once,
# and rewrites the manifest in place -- the same inode -- because it is
# bind-mounted into the three node containers.
"$FLEET_GATEKIT" manifest-mutate "$MANIFEST" "$ORIGINAL" "$MUTATED"
status=$?
set -e

if [ "$status" -ne 0 ]; then
  echo "negative-control: the mutation did not apply (exit ${status})" >&2 || exit 2
  exit 2
fi

MUTATED_LOG="$OUT_DIR/negative-determinism-mutated.txt"
RESTORED_LOG="$OUT_DIR/negative-determinism-restored.txt"

# Cleared here AND probed next to each use below. The two do different jobs and
# neither subsumes the other; an earlier revision dropped this clear on the
# grounds that truncation replaced it, which was true of content but not of path
# type.
#
# The clear owns path type. `rm -f` unlinks whatever sits at the path, including
# a stale symlink, so the redirection always creates a fresh regular file.
# Truncation cannot do that job: `: >` FOLLOWS a symlink, so a link left at one
# of these paths would be written through rather than replaced -- and if it
# pointed outside $OUT_DIR, that is what would be truncated.
#
# The probe owns openability, which the clear cannot speak to at all. A
# redirection that cannot open its target fails BEFORE the command runs, so the
# shell reports status 1 for a gate that never executed -- and 1 is a legitimate
# result from gate-determinism.sh, so it satisfies the 0/1/2 contract check and
# the 2-outranks-1 check alike and arrives at the assertions as a finding.
# Clearing does not prevent that: `rm -f` on an absent path succeeds even in a
# directory it could not write, so a clear returning 0 says nothing about whether
# the next `>` can open anything.
#
# And each probe has to sit next to its own use. $RESTORED_LOG is not redirected
# until the mutated run has finished, seconds later, so a probe placed up here
# would already be stale by then. Measured with $OUT_DIR made read-only after
# entry: the mutated run wrote its log, the restored one could not, and the
# script exited 1 reporting that check 1 did not return to green after
# restoration -- an announcement that the fixture is poisoned and every later
# check is measuring a broken manifest, produced because a log could not be
# opened.
fleet_clear_artefacts "$MUTATED_LOG" "$RESTORED_LOG"

# The nested runs get their own directory. gate-determinism.sh clears
# "$OUT_DIR"/determinism-*.txt on entry and names those files in the pointers it
# prints, so pointing it at $OUT_DIR would have it delete the per-node logs the
# preceding testcase wrote and then overwrite them twice, ending on the restored
# green run -- a reader following a genuine DIVERGED pointer would find a clean
# log. No mkdir needed: gate-determinism.sh calls fleet_require_writable_dir.
#
# The two logs above stay in $OUT_DIR because their redirections are set up
# before that call has created the nested directory.
NESTED_DIR="$OUT_DIR/negative"

: > "$MUTATED_LOG" || {
  echo "negative-control: cannot open ${MUTATED_LOG}" >&2 || exit 2
  exit 2
}

set +e
bash "$BIN_DIR/gate-determinism.sh" "$NESTED_DIR" > "$MUTATED_LOG" 2>&1
mutated_status=$?
set -e

# Restored before the second run and before any assertion, so a red result
# cannot leave the fixture poisoned. Failure here is fatal and loud: every
# later check would be measuring a manifest this script broke.
if ! cp -f "$BACKUP" "$MANIFEST"; then
  echo "negative-control: FAILED to restore ${MANIFEST} from ${BACKUP}" >&2 || exit 2
  exit 2
fi

: > "$RESTORED_LOG" || {
  echo "negative-control: cannot open ${RESTORED_LOG}" >&2 || exit 2
  exit 2
}

set +e
bash "$BIN_DIR/gate-determinism.sh" "$NESTED_DIR" > "$RESTORED_LOG" 2>&1
restored_status=$?
set -e

# --- BEGIN adjudication -------------------------------------------------------
# fleet/tests/adjudication.sh slices from this marker to end of file and drives
# the result with injected statuses and log paths, which is how the ordering
# below is held in place without standing up the fleet. Moving or renaming the
# marker makes that test exit 2 -- could not run -- rather than silently slicing
# the wrong region and passing over nothing.
#
# Both nested statuses first, before anything below can exit. They are already in
# hand, they need no file to be readable, and everything after this point can
# leave early -- so printing them here is the difference between a transcript
# that records what check 1 did and one that records only why this script gave
# up. Measured on the version that printed them after the log reads: an
# unreadable $RESTORED_LOG suppressed all four lines, including the mutated_exit=0
# that is the whole reason to read the transcript at all.
#
# The two derived readings are NOT printed here. Each needs its log read, and a
# reading must not be printed for a run whose result is about to be thrown away.
# An earlier revision printed restored_identical=no next to restored_exit=2 -- a
# summary of a run that could not complete, which says nothing and reads as
# though it did. One before that was worse: it used a bare `grep -q`, so that
# same line was printed for a log it had never successfully opened.
#
# They print below, each one after its own half's *status* has been cleared --
# not after that half's verdict, which is rendered a few lines later still.
echo "mutated_exit=${mutated_status}" || exit 2
echo "restored_exit=${restored_status}" || exit 2

# From here the two halves are adjudicated ONE AT A TIME, and the ordering is the
# whole point rather than tidiness.
#
# 2 outranks 1, as everywhere else here. A gate run that could not complete tells
# us nothing about whether the gate can detect anything, and saying "the negative
# control failed" on that basis would be the same outage-as-finding conflation
# corrected four times already. But that rule is about a *half*, not about this
# script. Asked once about both, the restored half's outage outranked a
# conclusive mutated-half verdict -- mutated=0, the gate reporting IDENTICAL over
# a manifest carrying a live `Timestamp.new`, the most alarming result this
# control can produce -- reported as could-not-complete at exit 2. A finding
# announced as an outage, the same conflation running backwards.
#
# **The restored half's log read is part of that rule, not an exception to it.**
# fleet_log_contains exits 2 on a log it cannot open, so a read placed above this
# point can end the script before the mutated half has been judged at all -- and
# an earlier revision of this fix did exactly that, interleaving the status
# guards while leaving both reads grouped above them. Measured: mutated=0 with an
# unreadable $RESTORED_LOG exited 2, where the code this fix replaced exited 1.
# The reads are therefore interleaved with the guards, each half touching only
# its own log, and the restored log is not opened until the mutated half's
# verdict is already rendered.
#
# Ordering the pairs this way also keeps the `-ne 1` below reachable. Testing
# only `mutated_status -eq 0` up here and leaving a combined rule in place would
# have fixed the measured case and left `-ne 1` unable to fail, trading this
# defect for a vacuous assertion -- the other class this tree tracks.
#
# The gate's 0/1/2 contract is enforced per half for the same reason, and it was
# not always. Recognising 2 and treating everything else as 1 would fold 127
# (script missing), 130 (SIGINT), 137 (OOM-killed -- plausible with three
# containers compiling at once) and 143 (CI timeout) into "check 1 failed to
# detect", so the contract is checked; but it used to be checked over BOTH
# statuses before either verdict, justified as "the one question whose answer is
# not about a half at all". That justification did not survive measurement on the
# sibling control: with the first half conclusive and the second outside the
# contract, the conclusive verdict was reported as could-not-complete. The same
# finding-announced-as-an-outage shape the 2-outranks-1 rule had already been
# split to avoid, surviving one guard above where it was fixed.
case "$mutated_status" in
  0|1|2) ;;
  *) echo "negative-control: the gate run over the mutated manifest exited outside" \
          "its 0/1/2 contract (mutated=${mutated_status} restored=${restored_status})" >&2 || exit 2
     exit 2 ;;
esac
if [ "$mutated_status" -eq 2 ]; then
  echo "negative-control: the gate run over the mutated manifest could not" \
       "complete (mutated=${mutated_status} restored=${restored_status})" >&2 || exit 2
  exit 2
fi

# fleet_log_contains, not `grep -q`, and the difference is not stylistic. A bare
# `grep -q` in this position folds its status 2 -- could not read the file --
# into the `else` branch, so a $MUTATED_LOG that vanished or became unreadable
# between the run above and this line produced mutated_diverged=no and, a few
# lines below, "check 1 did NOT detect a nondeterministic manifest" at exit 1:
# an announcement that the gate is not measuring what it claims, from a log
# nothing ever read. Measured with a mode-000 log; see the helper's header.
if fleet_log_contains 'DIVERGED' "$MUTATED_LOG"; then
  mutated_diverged=yes
else
  mutated_diverged=no
fi
echo "mutated_diverged=${mutated_diverged}" || exit 2

if [ "$mutated_status" -ne 1 ] || [ "$mutated_diverged" != yes ]; then
  echo "negative-control: check 1 did NOT detect a nondeterministic manifest" >&2 || exit 2
  exit 1
fi

case "$restored_status" in
  0|1|2) ;;
  *) echo "negative-control: the gate run over the restored manifest exited outside" \
          "its 0/1/2 contract (mutated=${mutated_status} restored=${restored_status})" >&2 || exit 2
     exit 2 ;;
esac
if [ "$restored_status" -eq 2 ]; then
  echo "negative-control: the gate run over the restored manifest could not" \
       "complete (mutated=${mutated_status} restored=${restored_status})" >&2 || exit 2
  exit 2
fi

# The pairing below is where fleet_log_contains matters most. Two greps over one
# file, the second negated, gave an unreadable $RESTORED_LOG two ways to be
# misread at once -- absent IDENTICAL and absent DIVERGED -- landing on
# restored_identical=no, the same answer a genuinely still-red gate gives.
# fleet_log_contains exits 2 from inside the `&&` chain, in the negated position
# as well as the plain one, before either reading is formed. Both positions were
# measured; `!` does not swallow the exit.
if fleet_log_contains 'IDENTICAL' "$RESTORED_LOG" && ! fleet_log_contains 'DIVERGED' "$RESTORED_LOG"; then
  restored_identical=yes
else
  restored_identical=no
fi
echo "restored_identical=${restored_identical}" || exit 2

if [ "$restored_status" -ne 0 ] || [ "$restored_identical" != yes ]; then
  echo "negative-control: check 1 did not return to green after restoration" >&2 || exit 2
  exit 1
fi
exit 0
