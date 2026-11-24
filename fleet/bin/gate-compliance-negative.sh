#!/usr/bin/env bash
# Negative control for gate check 6: prove it can go red, then prove it goes
# green again.
#
# Check 6 asserts that an enforced compliance value reaches the catalog. Its
# failure mode is quiet by construction: fleet_profile::compliance_marker
# defaults to 'unenforced', so a modulepath that stopped covering fleet_profile,
# or a renamed SIMP/compliance_profiles, produces a catalog that compiles
# cleanly and simply carries the default. Nothing fails. The check is the only
# thing standing between that and a green gate, so the check itself needs
# evidence it still works.
#
# The mutation renames the compliance data away rather than removing the
# enforcement key. Both turn check 6 red, but only this one exercises discovery
# -- compliance_mapper.rb:166-208 splitting env.modulepath, globbing each module
# directory and reading SIMP/compliance_profiles from every hit. That path works
# for a non-obvious reason: environment.conf's first modulepath entry is
# relative, and only Puppet::Settings::EnvironmentConf#modulepath expanding it
# makes the glob resolve. A future change that bypasses that accessor breaks
# discovery silently, and this is the path this control keeps check 6 able to
# see. Check 6 is what catches such a change, on its own, next run; this control
# is the evidence that it still can.
#
# Ctrl-C does not stop it promptly, and that is deliberate rather than a hang.
# SIGINT reaches the whole foreground process group, so the podman exec dies and
# check 6 reports could-not-run -- but this script itself keeps going, restores
# the compliance data explicitly, and runs the second full catalog compile before
# exiting 2. An operator therefore waits out one more compile after pressing
# Ctrl-C. Letting it finish is the point: the explicit restore checks its own
# result and says so, where the EXIT trap can only announce a failure it cannot
# fix. Kill it outright only if you are prepared to check `git status
# fleet/code` yourself; SIGKILL runs no trap at all, and the next run then exits
# 2 at the $HIDDEN_DIR guard below rather than proceeding.
#
# Exit codes: 0 check 6 went red and recovered, 1 it did not, 2 could not run.
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
  echo "usage: gate-compliance-negative.sh <out-dir>" >&2 || exit 2
  exit 2
fi

# Resolved once and absolutely, as in every other gate here: this script invokes
# another gate by path, so a relative $0 breaks it depending on the caller's cwd.
# `|| exit 2` because a failing command substitution under `set -e` exits 1, and
# 1 is a result about the fleet in every gate here.
BIN_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 2

# Guarded, not bare. `. missing-file` under `set -e` exits 1, which this script
# would report as "check 6 cannot detect a missing compliance tree" -- an outage
# announced as an alarming finding, the defect class this tree has now closed in
# fourteen separate channels.
#
# Safe against a false positive: this reads the sourced file's own exit status,
# and gate-common.sh ends in an explicit `:` sentinel that pins that to 0.
# shellcheck source=gate-common.sh
. "$BIN_DIR/gate-common.sh" || {
  echo "gate: cannot load ${BIN_DIR}/gate-common.sh" >&2 || exit 2
  exit 2
}

fleet_require_writable_dir "$OUT_DIR"

# `mv -T` is GNU-only, and it is probed because losing it would leave the fixture
# broken, not because it is the harness's only non-POSIX host dependency. An
# earlier version of this comment claimed it was the only one on a gate path and
# the only one using a non-POSIX *option* rather than a non-POSIX tool. Both
# halves were false, the second falsified by the two lines directly beneath it.
#
# What is actually true, measured by grep over fleet/bin, scripts/ and
# .taskfiles: `sleep 0.5` in gate-common.sh's poll loop takes a fractional
# operand POSIX does not require, and that is on check 4's path; `mktemp` is not
# a POSIX utility and is called on the host by check 1's control and by
# build-images.sh -- NOT by this file, which names it only in this sentence, as
# an earlier draft of the sentence wrongly claimed; the `mv --help` and
# `mv --version` probe further down uses GNU long options itself; and
# make-certs.sh still calls `seq`, off the gate path.
#
# What singles `mv -T` out is consequence, not rarity, and the consequences
# differ. `mktemp` here and the `mv --help` probe fail to 2, honestly. `seq` is
# off the gate path. `sleep 0.5` is worse than either: measured against a
# sleep that rejects a fractional operand, gate-common.sh's poll exits 1 --
# a finding about the fleet -- where its own designed timeout exits 2. It stays
# unprobed as a judgement call about likelihood, recorded rather than claimed
# away. `mv -T` alone fails by silently NESTING the restore and leaving the
# fixture broken, which is why it alone is probed here.
#
# Nothing documents a coreutils requirement, so a BusyBox or BSD host is a real
# possibility rather than a theoretical one.
#
# Probed HERE, before the rename, and that placement is the whole point. The
# rename below is plain POSIX `mv` and would succeed on such a host, so without
# this the fixture breaks first and only the restore fails on the unknown
# option: exit 2 and loud, honest classification, but the tree left dirty with
# three containers compiling without compliance data and a human needed to
# repair it. That is the one outcome this script is built end to end to prevent.
# Checked with the other could-not-run gates, all of which fire before anything
# is mutated, and sharing the inline `cmd || { echo ... >&2 || exit 2; exit 2; }`
# guard shape of gate-common.sh's checks.
#
# A pipeline under `pipefail`, which would report 141 if `grep -q` matched and
# closed the pipe before `mv` finished writing. It cannot here: `mv --help` is
# 2956 bytes, far inside the 64KiB pipe buffer, so mv has written everything and
# exited before grep reads. Measured at 500 iterations, 0 SIGPIPE. Worth knowing
# before reusing this shape on a command with real output.
mv --help 2>/dev/null | grep -q -- '--no-target-directory' || {
  echo "negative-control: this control needs GNU mv (-T); found $(mv --version 2>&1 | head -1)" >&2 || exit 2
  exit 2
}

# The child gate-compliance.sh calls gatekit; a missing or stale one must stop
# this control now, while the fixture is intact, not after the rename below.
fleet_require_gatekit

SIMP_DIR="$BIN_DIR/../code/environments/production/modules/fleet_profile/SIMP"
HIDDEN_DIR="${SIMP_DIR}-disabled"

if [ ! -d "$SIMP_DIR" ]; then
  echo "negative-control: ${SIMP_DIR} is missing" >&2 || exit 2
  exit 2
fi

# A leftover from an interrupted run would make the rename below fail, and
# restoring the wrong direction would delete the fleet's compliance data.
if [ -e "$HIDDEN_DIR" ]; then
  echo "negative-control: ${HIDDEN_DIR} already exists; a previous run did not" \
       "restore. Move it back to ${SIMP_DIR} before rerunning." >&2 || exit 2
  exit 2
fi

if ! mv "$SIMP_DIR" "$HIDDEN_DIR"; then
  echo "negative-control: could not rename ${SIMP_DIR} away" >&2 || exit 2
  exit 2
fi

# Belt and braces. The normal path restores explicitly and checks the result,
# but an interrupt between the rename above and that point would otherwise leave
# three containers bind-mounting a fleet_profile with no compliance data, and
# every later check in the suite would be measuring a tree this script broke.
#
# Armed only after the rename succeeds: armed any earlier it would try to move a
# $HIDDEN_DIR that belongs to nobody.
#
# Chained with `||` and never a `{ ... }` group. `set -e` applies inside an EXIT
# trap, and only the LEFT operand of `||` is exempt from it, so a group on the
# right runs under errexit and a failure inside it aborts the trap -- which
# overwrites this script's exit status with 1. Measured elsewhere in this tree
# with an unguarded cleanup command: requested 0, 1 and 2 all became 1.
#
# The leading `test ! -e` is not tidiness either. Unlike
# gate-determinism-negative.sh, whose trap re-copies a backup file that outlives
# the explicit restore, this restore CONSUMES $HIDDEN_DIR: after the explicit
# `mv -fT` below succeeds there is nothing left to move, so an unguarded trap
# would fail and print its "the fleet is left without its compliance data"
# alarm on every healthy run. The guard makes the announcement mean something.
#
# The announcement is why the trap is not simply `|| :`. Two paths reach here
# with the data still hidden: the explicit restore already failed, which printed
# its own message and exited 2, or a signal or an `exit 2` landed between the
# rename and that restore -- including the exit 2 from either log probe below.
# On that second path this is the ONLY restore attempt there will be, and a
# silent failure is exactly what this trap exists to prevent. The trailing
# `|| :` catches a failing echo so the status stays pinned.
#
# `-T` is the difference between a loud failure and a silent one, and this is
# the site where it matters most. Without it, a $SIMP_DIR recreated mid-run --
# by anything outside this script that rewrites the tree, `git checkout`, `git
# restore`, `git reset --hard`, a rebase or a worktree operation, an editor, a
# bare `mkdir`, though NOT by a second copy of this control, which exits 2 at
# the $HIDDEN_DIR guard above before it can arm its own trap -- makes `mv` move
# the source INSIDE the target and return
# 0. Measured: SIMP/SIMP-disabled/, exit 0, no message, and this trap would then
# report a clean run over a dirty fleet/code. `-T` refuses instead: "cannot
# overwrite 'SIMP': File exists", exit 1, which the echo below announces. An
# EMPTY directory at the target is still replaced, which is the wanted
# behaviour; `-T` fails only on a non-empty one.
trap 'test ! -e "$HIDDEN_DIR" \
        || mv -fT "$HIDDEN_DIR" "$SIMP_DIR" >/dev/null 2>&1 \
        || echo "negative-control: EXIT restore of ${SIMP_DIR} FAILED -- the fleet is left without its compliance data, at ${HIDDEN_DIR}" >&2 \
        || :' EXIT

BROKEN_LOG="$OUT_DIR/negative-compliance-broken.txt"
RESTORED_LOG="$OUT_DIR/negative-compliance-restored.txt"

# Its own directory, and not $OUT_DIR: gate-compliance.sh clears
# compliance-node-el.json on entry and names it in the pointer it prints, so
# sharing $OUT_DIR would delete the artefact the preceding testcase wrote and
# leave a reader following a genuine pointer at a file from this control's run
# instead. No mkdir needed: gate-compliance.sh calls fleet_require_writable_dir.
#
# The two logs above stay in $OUT_DIR because their redirections are set up
# before that call has created the nested directory.
NESTED_DIR="$OUT_DIR/negative"

# Cleared here AND probed next to each use below. The two do different jobs and
# neither subsumes the other.
#
# The clear owns path type. `rm -f` unlinks whatever sits at the path, including
# a stale symlink, so each redirection creates a fresh regular file. Truncation
# cannot do that job: `: >` FOLLOWS a symlink, so a link left at one of these
# paths would be written through rather than replaced -- and if it pointed
# outside $OUT_DIR, that is what would be truncated.
#
# The probe owns openability, which the clear cannot speak to at all. A
# redirection that cannot open its target fails BEFORE the command runs, so the
# shell reports status 1 for a gate that never executed -- and 1 is a legitimate
# result from gate-compliance.sh, so it would satisfy the 0/1/2 contract check
# and the 2-outranks-1 check alike and arrive at the assertions as a finding.
# `rm -f` on an absent path succeeds even in a directory it could not write, so
# a clear returning 0 says nothing about whether the next `>` can open anything.
#
# And each probe has to sit next to its own use: $RESTORED_LOG is not redirected
# until the broken run has finished, tens of seconds later, so a probe placed up
# here would already be stale by then.
#
# Called here rather than from the trap on purpose: fleet_clear_artefacts exits
# 2 itself, and an `exit` inside an EXIT trap overwrites the status the trap
# exists to preserve. Called from the main body its exit 2 is correct, and the
# trap above still restores the compliance data on the way out.
fleet_clear_artefacts "$BROKEN_LOG" "$RESTORED_LOG"

: > "$BROKEN_LOG" || {
  echo "negative-control: cannot open ${BROKEN_LOG}" >&2 || exit 2
  exit 2
}

set +e
bash "$BIN_DIR/gate-compliance.sh" "$NESTED_DIR" > "$BROKEN_LOG" 2>&1
broken_status=$?
set -e

# Restored before the second run and before any assertion, so a red result
# cannot leave the fleet without its compliance data. Failure here is fatal and
# loud: every later check would be measuring a tree this script broke.
#
# `-T` for the reason given at the trap: plain `mv -f` onto a $SIMP_DIR that
# something outside this script recreated nests the source inside it and returns
# 0, so the restore this script reports as successful would have left the tree
# dirty. Here the damage would at least surface as a red run; in the trap it
# would not, which is why both sites carry the flag rather than only that one.
if ! mv -fT "$HIDDEN_DIR" "$SIMP_DIR"; then
  echo "negative-control: FAILED to restore ${SIMP_DIR} from ${HIDDEN_DIR}" >&2 || exit 2
  exit 2
fi

: > "$RESTORED_LOG" || {
  echo "negative-control: cannot open ${RESTORED_LOG}" >&2 || exit 2
  exit 2
}

set +e
bash "$BIN_DIR/gate-compliance.sh" "$NESTED_DIR" > "$RESTORED_LOG" 2>&1
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
# that records what check 6 did and one that records only why this script gave
# up. Measured on the version that printed them after the log reads: an
# unreadable $RESTORED_LOG suppressed all four lines, including the broken_exit=0
# that is the whole reason to read the transcript at all.
#
# The two derived readings are NOT printed here. Each needs its log read, and a
# reading must not be printed for a run whose result is about to be thrown away.
# An earlier revision printed restored_enforced=no next to restored_exit=2 -- a
# summary of a run that could not complete, which says nothing and reads as
# though it did. One before that was worse: it used a bare `grep -q`, so that
# same line was printed for a log it had never successfully opened.
#
# They print below, each one after its own half's *status* has been cleared --
# not after that half's verdict, which is rendered a few lines later still.
echo "broken_exit=${broken_status}" || exit 2
echo "restored_exit=${restored_status}" || exit 2

# From here the two halves are adjudicated ONE AT A TIME, and the ordering is the
# whole point rather than tidiness.
#
# 2 outranks 1, as everywhere else here: a run that could not complete says
# nothing about what the check can detect, and calling the negative control
# failed on that basis is the same outage-as-finding conflation again. But that
# rule is about a *half*, not about this script. Asked once about both, the
# restored half's outage outranked a conclusive broken-half verdict: measured
# with broken=0 restored=2 -- check 6 staying green with the compliance data
# renamed away, the most alarming result this control can produce -- reported as
# "a check-6 run could not complete", exit 2. A finding announced as an outage,
# the same conflation running backwards.
#
# **The restored half's log read is part of that rule, not an exception to it.**
# fleet_log_contains exits 2 on a log it cannot open, so a read placed above this
# point can end the script before the broken half has been judged at all -- and
# an earlier revision of this fix did exactly that, interleaving the status
# guards while leaving both reads grouped above them. Measured: broken=0 with an
# unreadable $RESTORED_LOG exited 2, where the code this fix replaced exited 1.
# The reads are therefore interleaved with the guards, each half touching only
# its own log, and the restored log is not opened until the broken half's verdict
# is already rendered.
#
# Ordering the pairs this way also keeps the `-ne 1` below reachable. Testing
# only `broken_status -eq 0` up here and leaving a combined rule in place would
# have fixed the measured case and left `-ne 1` unable to fail, trading this
# defect for a vacuous assertion -- the other class this tree tracks.
#
# The callee's 0/1/2 contract is enforced per half for the same reason, and it
# was not always. Recognising 2 and treating everything else as 1 would fold 127
# (script missing), 130 (SIGINT), 137 (OOM-killed -- plausible with three
# containers compiling) and 143 (CI timeout) into "check 6 failed to detect", so
# the contract is checked; but it used to be checked over BOTH statuses before
# either verdict, justified as "the one question whose answer is not about a half
# at all". That justification did not survive measurement: with broken=0
# restored=137, a conclusive broken-half verdict -- check 6 green while its
# compliance data was renamed away -- was reported as could-not-complete, exit 2,
# because the *other* half's status was outside the contract. The same
# finding-announced-as-an-outage shape the 2-outranks-1 rule had already been
# split to avoid, surviving one guard above where it was fixed.
case "$broken_status" in
  0|1|2) ;;
  *) echo "negative-control: the check-6 run over the broken fixture exited outside" \
          "its 0/1/2 contract (broken=${broken_status} restored=${restored_status})" >&2 || exit 2
     exit 2 ;;
esac
if [ "$broken_status" -eq 2 ]; then
  echo "negative-control: the check-6 run over the broken fixture could not" \
       "complete (broken=${broken_status} restored=${restored_status})" >&2 || exit 2
  exit 2
fi

# fleet_log_contains, not `grep -q`, and the difference is not stylistic. A bare
# `grep -q` in this position folds its status 2 -- could not read the file -- into
# the `else` branch, so a $BROKEN_LOG that vanished or became unreadable between
# the run above and this line produced broken_reported=no and, a few lines below,
# "check 6 did NOT detect missing compliance data" at exit 1. Measured with a
# mode-000 log; see the helper's header for the full reading.
if fleet_log_contains 'unenforced' "$BROKEN_LOG"; then
  broken_reported=yes
else
  broken_reported=no
fi
echo "broken_reported=${broken_reported}" || exit 2

if [ "$broken_status" -ne 1 ] || [ "$broken_reported" != yes ]; then
  echo "negative-control: check 6 did NOT detect missing compliance data" >&2 || exit 2
  exit 1
fi

case "$restored_status" in
  0|1|2) ;;
  *) echo "negative-control: the check-6 run over the restored fixture exited outside" \
          "its 0/1/2 contract (broken=${broken_status} restored=${restored_status})" >&2 || exit 2
     exit 2 ;;
esac
if [ "$restored_status" -eq 2 ]; then
  echo "negative-control: the check-6 run over the restored fixture could not" \
       "complete (broken=${broken_status} restored=${restored_status})" >&2 || exit 2
  exit 2
fi

# Matched on check 6's success line, not on the bare expected value. Its FAILURE
# message names the same string -- "expected 'enforced-by-disa_stig', got
# 'unenforced'" -- so a loose grep would print restored_enforced=yes over a log
# that says the opposite. The exit-status test below would still fail the run,
# but the transcript exists to record what check 6 did, and a line contradicting
# the log it summarises is worse than no line. Same reason
# gate-determinism-negative.sh pairs its IDENTICAL grep with a DIVERGED one.
if fleet_log_contains 'compliance_marker = enforced-by-disa_stig' "$RESTORED_LOG"; then
  restored_enforced=yes
else
  restored_enforced=no
fi
echo "restored_enforced=${restored_enforced}" || exit 2

if [ "$restored_status" -ne 0 ] || [ "$restored_enforced" != yes ]; then
  echo "negative-control: check 6 did not return to green after restoration" >&2 || exit 2
  exit 1
fi
exit 0
