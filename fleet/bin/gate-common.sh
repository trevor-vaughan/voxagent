#!/usr/bin/env bash
# Shared definitions for the fleet gate checks: the code tree they all work
# against, the voxdiff arguments every comparison uses, and the output-directory
# guard every gate needs.
#
# Kept in one place because the gates must agree on what they are measuring. If
# the comparison gates compiled one environment while the scan read another,
# their results would describe different trees while reading as if they
# described the same one -- and a harness whose checks measure subtly different
# things is worse than one check fewer.
#
# This file is SOURCED, not executed. The arguments are interpolated into a
# `podman exec sh -c "..."` string inside each gate script, and a subprocess
# that merely printed them would need another layer of quoting to compose into
# that string. A shell function interpolates cleanly.
#
# Ten files source this one. Nine do it guarded, as
# `. gate-common.sh || { ...; exit 2; }`, so that a load failure is reported as
# could-not-run rather than as a finding: the eight gate scripts, and
# fleet/bin/puppet-node-deb-after-export, check 4's voxdiff `-right` wrapper,
# which was the last unguarded one.
#
# The tenth is fleet/tests/adjudication.sh, which emits a bare source into the
# preamble of each block it generates and covers it with a precondition instead
# -- it proves this file loads once, before any case runs. A guard in the
# generated scripts would be worse there, not better: it would make them exit 2,
# and twelve of that file's cases expect 2, so the vacuous passes would move
# rather than go away. Measured unguarded and unchecked: 22 of 46 cases passed
# over a missing gate-common.sh.
#
# Count them before repeating either number. This comment has been wrong twice.
# That guard has one consequence which is invisible from the call sites and so is
# recorded here: because the source is the left operand of `||`, `set -e` is
# suppressed for the commands inside THIS file. A failing top-level command here
# would not abort the gate and would not be reported -- execution would simply
# carry on to the sentinel at the bottom, which returns 0. The guard traded a
# loud failure for a silent one in exchange for catching a missing file.
#
# This concerns top-level statements only: function BODIES still run under the
# caller's `set -e` when they are called later. Anything added at top level must
# check its own status rather than relying on `set -e` to catch it.
#
# Guarding writes
# ---------------
# Every `echo`, `printf`, `sed` and `cat` that writes to stdout or stderr in this
# file and in the gate scripts carries `|| exit 2`. This is not defensive noise.
#
# A failed write exits 1 under `set -e`, and 1 is "a result about the fleet" in
# every gate here, so an unwritable stdout turns a green run into a finding.
# Measured on gate-determinism.sh with stdout on a full device: exit **1** before
# this convention, exit 2 after. The worse half is on stderr, where the failing
# command is the outage announcement itself -- `echo "... could not run" >&2`
# followed by `exit 2` exits **1** instead, so the line whose only job is to
# report an outage is what converts it into a finding.
#
# Writes were the last unguarded class here. Redirect targets, command
# substitutions, `source`, `grep`, `podman exec`, `cp` and `mv` were all routed
# to 2 in earlier rounds; `echo` was overlooked because it is not usually thought
# of as a command that can fail.
#
# Two deliberate exceptions:
#
#   Inside an EXIT trap, writes keep `|| :` and must NOT gain `|| exit 2`. An
#   `exit` in a trap overwrites the status the trap exists to preserve.
#
#   fleet_compile_args' `printf` is unguarded. It is called only inside `$( )`,
#   where an `exit` would kill the subshell rather than the gate -- the same trap
#   fleet_log_contains' header documents -- and its output is a value the caller
#   consumes, not a transcript. A failure there surfaces as an empty argument
#   string and a failed compile, which the caller already classifies.
#
# Paths here are CONTAINER paths: the gates run on the host but every command
# they compose executes inside a node container.
# Sourced, not executed, so this also clears noclobber in the sourcing shell --
# deliberate. Every gate sets it too; this is the defence in depth for the three
# writability probes below, which would otherwise depend on each of ten callers
# having remembered.
#
# Under `set -C` a `: > "$f"` probe succeeds by CREATING $f and the redirect it
# predicts then fails. That would re-arm the defect the probes exist to prevent:
# an outage reported as a finding about the fleet.
set +C

FLEET_ENVIRONMENTPATH=/etc/puppetlabs/code/environments
FLEET_ENVIRONMENT=production
FLEET_CODEDIR=/etc/puppetlabs/code

# OpenVoxDB's plaintext query API, as seen from the host. Published on loopback
# only: this port is unauthenticated and accepts write commands as well as
# queries, which is the surface the TLS and client certificates on 8081 exist to
# keep off the network.
FLEET_PDB_URL=http://127.0.0.1:18080

# gatekit does the parsing and judging these gates need -- reading catalogs and
# fact captures, querying OpenVoxDB, mutating the manifest -- so that the harness
# needs no second language. Resolved from this file's own location, the way
# gate-exports.sh resolves voxdiff, and overridable for a test. `|| exit 2`
# because a failing command substitution here would otherwise leave the root
# empty and every gate would look for /bin/gatekit.
FLEET_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)" || exit 2
FLEET_GATEKIT="${GATEKIT:-$FLEET_ROOT/bin/gatekit}"

# fleet_require_gatekit
#
# Checked once, up front, by every gate that calls gatekit. A missing binary
# would return 127 at the first call and a stale one would judge with rules no
# longer in the tree -- the four-minute-old-binary trap recorded in
# docs/maintainers/apply-spine.md. Both are could-not-run, never a result about
# the fleet, so both exit 2.
fleet_require_gatekit() {
  [ -x "$FLEET_GATEKIT" ] || {
    echo "gate: $FLEET_GATEKIT is not built; run 'task build'" >&2 || exit 2
    exit 2
  }
  # A find that cannot read the sources fails closed: "nothing is newer" would
  # be exactly the false green this check exists to prevent.
  local newer
  newer="$(find "$FLEET_ROOT/cmd" "$FLEET_ROOT/internal" -name '*.go' -newer "$FLEET_GATEKIT" -print -quit)" || {
    echo "gate: could not compare $FLEET_GATEKIT with the Go sources" >&2 || exit 2
    exit 2
  }
  if [ -n "$newer" ]; then
    echo "gate: $FLEET_GATEKIT is older than the Go sources; run 'task build'" >&2 || exit 2
    exit 2
  fi
}

# fleet_require_certs
#
# The TLS material make-certs.sh mints is not permanent: the CA's CRL expires
# 30 days after minting, the node and server certificates after a year.
# Measured on 2026-10-03: an expired CRL made gate-exports fail with nothing
# more than "node-el's base export failed to compile (exit 1)", the reason
# visible only by re-running puppet by hand. A gate that talks TLS checks first,
# so an expiry is named, and stays quiet unless something is expired or due
# within gatekit's warning window. Expired is could-not-run (2): the fleet was
# never measured.
fleet_require_certs() {
  fleet_require_gatekit
  local report status=0
  report="$("$FLEET_GATEKIT" fleet-certs "$FLEET_ROOT/fleet/.certs" 2>&1)" || status=$?
  # The report is shown once: whenever the check failed, or passed with a warning.
  if [ "$status" -ne 0 ] || [[ "$report" == *WARNING* ]]; then
    printf '%s\n' "$report" >&2 || exit 2
  fi
  case "$status" in
    0) ;;
    1) echo "gate: a fleet certificate or the CRL has expired; re-mint as shown above" >&2 || exit 2
       exit 2 ;;
    *) echo "gate: could not check the fleet certificates" >&2 || exit 2
       exit 2 ;;
  esac
}

# fleet_compile_args <node-name>
fleet_compile_args() {
  printf -- "-node '%s' -environmentpath %s -environment %s -codedir %s" \
    "$1" "$FLEET_ENVIRONMENTPATH" "$FLEET_ENVIRONMENT" "$FLEET_CODEDIR"
}

# fleet_catalog_json <container> <node> <stdout-file> <stderr-file>
#
# Compile <node>'s catalog inside <container>, leaving raw output in
# <stdout-file>. RETURNS podman's status, or 2 if either redirect target cannot
# be opened, and deliberately does not exit on either: 1 means something
# different in every gate that calls this, so only the caller can classify it.
#
# Both redirect targets are probed before podman runs, so a target that cannot
# be opened returns 2 rather than the 1 a failed redirection reports. Neither
# current caller misclassified that -- both map every non-zero to could-not-run,
# so 2 lands exactly where 1 landed. This is a guarantee for callers that read 1
# as a finding, not a fix for a live misclassification, and it follows
# fleet_clear_artefacts' header: a successful clear is no substitute for
# probing a path you are about to redirect into.
#
# The message a caller prints for this case is still wrong and is the caller's
# to fix: both say "FAILED to compile", naming a compile that never ran, and
# point at an .err the shell never got as far as creating.
#
# <node> is interpolated UNQUOTED into the `sh -c` string below, so it is a
# command-injection sink. Both callers pass hardcoded names -- gate-axes.sh from
# its expectation table, gate-compliance.sh from a literal -- so nothing here is
# reachable by an attacker today. This is a shared entry point now, though, and
# that property no longer sits next to the values: quote or validate before
# passing anything derived from a fact, an argument, or a file.
#
# The caller MUST bracket the call in `set +e` / `set -e`. This function does not
# touch that setting itself -- `set -e` is global shell state, so toggling it here
# would re-enable it underneath a caller that had deliberately turned it off:
#
#   set +e
#   fleet_catalog_json "$CONTAINER" "$NODE" "$RAW" "$ERR"
#   status=$?
#   set -e
#
# --confdir/--vardir under a throwaway directory is load-bearing isolation, not
# hygiene. The default confdir holds the puppet.conf that turns storeconfigs on,
# and checks 1, 2, 3, 5 and 6 must observe the environment's own catalog rather
# than one built against OpenVoxDB. Check 4 is the deliberate exception: it wants
# storeconfigs, and does not call this.
#
# --node_name_value matches what voxdiff passes (internal/compile/runner.go), so
# the gates and voxdiff see the same node identity. Kept here because checks 5
# and 6 each assert in a comment that they observe the same catalog check 1
# measures: two copies of this block could drift a flag apart while both comments
# went on claiming equivalence.
fleet_catalog_json() {
  local container="$1" node="$2" out="$3" err="$4"
  # Probed, not documented. `rm -f` succeeding in fleet_clear_artefacts says
  # nothing about whether the redirect below can open its target, and a failed
  # redirection reports 1 without running podman at all. 2 is likewise a status
  # podman never produces, but it is the honest one under this tree's contract.
  : > "$out" || return 2
  : > "$err" || return 2
  podman exec "$container" sh -c "
        d=\$(mktemp -d) || exit 2
        mkdir -p \"\$d/conf\" \"\$d/var\" || exit 2
        /opt/puppetlabs/bin/puppet catalog find ${node} \
          --terminus compiler --render-as json --node_name_value ${node} \
          --environmentpath ${FLEET_ENVIRONMENTPATH} \
          --environment ${FLEET_ENVIRONMENT} \
          --codedir ${FLEET_CODEDIR} \
          --confdir \"\$d/conf\" --vardir \"\$d/var\"
        s=\$?
        rm -rf \"\$d\"
        exit \$s
      " > "$out" 2> "$err"
}

# fleet_clear_artefacts <path>...
#
# Drop a previous run's artefacts, so a run that fails to write cannot leave a
# stale file behind that still reads as current.
#
# Exits 2 on failure; never returns non-zero. Do not call it from inside a trap
# -- an `exit` there overwrites the status the trap exists to preserve, which is
# why gate-determinism-negative.sh guards its own cleanup with `|| :` instead.
# For the same reason, callers inside a loop that must keep measuring the
# remaining nodes need their own guard rather than this.
#
# It is also not a substitute for probing a path you are about to redirect into.
# `rm -f` on an absent path succeeds even in a directory it could not write, so a
# clear that returns 0 says nothing about whether the next `>` can open its
# target. gate-determinism.sh and gate-implementations.sh probe with `:` at the
# point of use for that reason.
#
# `rm -f` ignores a path that does not exist, but it still fails on one it cannot
# unlink: a directory sitting where an artefact belongs, or a parent left
# unwritable by a rootful container run. Bare under `set -e` that failure exits
# 1, and 1 is a result about the fleet in every gate here, so a check that never
# ran gets reported as one that ran and found something.
#
# Measured before this helper existed: a directory at
# .test-output/fleet/compliance-node-el.json made gate-compliance.sh exit 1 with
# empty stdout, which the Venom suite reads as "enforcement did not reach the
# catalog" -- and its ShouldNotContainSubstring assertion passes vacuously on
# empty output, so the transcript names the wrong cause with nothing to correct
# it.
#
# Unquoted globs are expected at the call sites: they expand before the call, and
# an unmatched pattern arrives as a literal that `rm -f` ignores.
fleet_clear_artefacts() {
  rm -f "$@" || {
    echo "gate: cannot clear previous artefacts: $*" >&2 || exit 2
    exit 2
  }
}

# fleet_require_writable_dir <dir>
#
# Every gate redirects a comparison's output to a per-node log and then reads
# the command's exit status. If the log cannot be opened, the redirection fails
# before the command ever runs and the shell reports status 1 -- which under
# this project's convention means "the catalogs diverged". An unwritable
# .test-output would therefore be reported as evidence against P0's allowlist,
# with no comparison having taken place at all.
#
# `mkdir -p` alone does not catch it: it succeeds on an existing directory
# whatever its mode, which is exactly the state a rootful container run can
# leave behind. Probe for writability and exit 2 -- could not run -- instead.
fleet_require_writable_dir() {
  mkdir -p "$1" || {
    echo "gate: cannot create output directory $1" >&2 || exit 2
    exit 2
  }
  if ! : > "$1/.writable"; then
    echo "gate: output directory $1 is not writable" >&2 || exit 2
    exit 2
  fi
  # The probe is the one file this function is certain it created, so a failure
  # to remove it means the directory changed underneath the check -- could not
  # run, not a finding.
  fleet_clear_artefacts "$1/.writable"
}

# fleet_log_contains <pattern> <file>
#
# Returns 0 when <file> contains <pattern> and 1 when it does not. Exits 2 --
# never returns -- when grep could not read <file> at all.
#
# That third case is the whole reason this exists. `grep -q` is three-valued: 0
# match, 1 no match, 2 could not read. Written as `if grep -q P "$f"; then`, the
# 2 lands in the `else` branch, so a log that vanished, or sat under a directory
# that lost its permissions mid-run, became indistinguishable from one whose
# check found nothing -- and both negative controls then announced a finding
# about the fleet over a log they never read. Measured with a mode-000 log,
# before this helper existed:
#
#   broken_exit=1  broken_reported=no   -> exit 1, "check 6 did NOT detect ..."
#   mutated_exit=1 mutated_diverged=no  -> exit 1, "check 1 did NOT detect ..."
#
# The eleventh channel of the same defect, and the first one found in a status
# that is not the shell's own. grep's "Permission denied" does reach stderr, so
# the outage is not silent -- but the exit status and the summary line both state
# the opposite of the truth, and those two are what the Venom suite asserts on.
#
# Exits rather than returning 2, unlike fleet_catalog_json. That function returns
# because 1 means something different in every gate that calls it, so only a
# caller can classify it; "could not run" needs no classifying, which is why
# fleet_clear_artefacts and fleet_require_writable_dir exit too. Exiting also
# keeps this a drop-in for the `grep -q` it replaces, so every call site keeps
# the shape a reader already knows -- including the `&& !` pairing in
# gate-determinism-negative.sh, where an unreadable log now exits from inside the
# `if` condition itself.
#
# It therefore carries fleet_clear_artefacts' caveat for the same reason: do not
# call it from inside a trap, where an `exit` overwrites the status the trap
# exists to preserve.
#
# And a second caveat that is specific to exiting: **call it directly, never in a
# pipeline, a subshell or a command substitution.** An `exit` there terminates
# only the subshell, so the outage comes back as a status the caller is free to
# ignore -- and in the one shape a reader is most likely to reach for it does
# exactly that. Measured:
#
#   if fleet_log_contains X "$unreadable" | cat; then ...; else ...; fi
#   -> takes the `else` branch and the script carries on
#
# which is precisely the folding of 2 into "no match" this function exists to
# prevent, reintroduced one pipe at a time. `fleet_catalog_json` pushes its own
# `set +e` constraint onto callers the same way; this is that, for `exit`.
#
# The example above folds correctly only because every caller sets `pipefail`
# before sourcing this file. Without it the pipeline reports `cat`'s status, 0,
# and the `if` takes the *`then`* branch -- an unreadable log read as a match,
# which is worse again.
#
# Seven invocations today, in two shapes, and both are safe because neither puts
# the call in a subshell:
#
#   bare in an `if` condition -- five of them, including the `&&`-with-`!`
#   pairing in gate-determinism-negative.sh, where the exit propagates out of
#   the negated operand;
#
#   the 2nd and 3rd operands of a three-term `&&` chain whose first operand is a
#   `[ ]` test -- gate-exports.sh's verdict, added later.
#
# Count and shapes are stated here because this header is what the next author
# consults before adding an eighth. Keep them current.
#
# `|| status=$?` and not a `set +e` bracket. This runs under the caller's `set
# -e`, which is global shell state, so toggling it here would re-enable it
# underneath a caller that had deliberately turned it off -- the constraint
# fleet_catalog_json's header states and pushes onto its callers instead. The
# left operand of `||` is exempt from errexit, so nothing needs toggling and the
# call sites need no bracket of their own.
#
# `--` because a pattern beginning with `-` would otherwise be read as an option.
# Every caller today passes a literal that cannot; this is for the next one.
fleet_log_contains() {
  local status=0
  grep -q -- "$1" "$2" || status=$?
  # `-gt 1`, not `-eq 2`, for the reason both controls enforce the 0/1/2 contract
  # on their nested runs. 2 is the status grep documents, but it is not the only
  # one above 1: a grep killed by a signal reports 128+n, and 137 -- an OOM kill,
  # plausible with three containers compiling -- was measured arriving here
  # intact, the parent shell surviving to classify it. Anything above 1 says just
  # as little about what the log contains as 2 does.
  #
  # A Ctrl-C is deliberately NOT the example. SIGINT reaches the whole foreground
  # process group, as gate-compliance-negative.sh's header sets out at length, so
  # the calling shell dies alongside grep and nothing reaches this test at all.
  # 130 does arrive here from a signal aimed at grep alone -- it is reachable,
  # just not by the route a reader would assume -- which is why the reachable,
  # measured example is the one quoted above.
  if [ "$status" -gt 1 ]; then
    echo "gate: cannot search ${2} for '${1}' (grep exited ${status})" >&2 || exit 2
    exit 2
  fi
  return "$status"
}

# fleet_export_as <variant>
#
# Make node-el export `fleet-exported-node-el-<variant>` and store it in
# OpenVoxDB. Shared by gate-exports.sh (the base export) and the right-hand
# wrapper (the mutated one) because the two must be produced under identical
# conditions -- if one gained a flag the other lacked, the gate would compare
# catalogs built from different inputs and report the difference as a finding.
#
# No --confdir override, deliberately: the default confdir holds the puppet.conf
# that turns storeconfigs on, and voxdiff cannot pass --storeconfigs itself.
#
# Writes mutator.yaml, never fleet.yaml -- clobbering the latter would remove
# fleet_tier and every node-el compile would then fail fleet_profile's guard.
fleet_export_as() {
  local variant="$1" status
  set +e
  podman exec fleet_node_el sh -c "
      printf 'fleet_export_variant: ${variant}\n' > /opt/puppetlabs/facter/facts.d/mutator.yaml
      /opt/puppetlabs/bin/puppet catalog find node-el \
        --terminus compiler --render-as json \
        --environmentpath ${FLEET_ENVIRONMENTPATH} --environment ${FLEET_ENVIRONMENT} \
        --codedir ${FLEET_CODEDIR}
    " >/dev/null 2>&1
  status=$?
  set -e

  # Classified, not bare. Left under `set -e` this would exit with podman's own
  # status -- 125 for a stopped container, 1 for a compile error -- and 1 is
  # this gate's code for "the fleet did not diverge as asserted", turning an
  # outage into a finding.
  if [ "$status" -ne 0 ]; then
    echo "gate: node-el's ${variant} export failed to compile (exit ${status})" >&2 || exit 2
    exit 2
  fi
}

# fleet_wait_for_export <title>
#
# OpenVoxDB accepts a catalog submission and processes it asynchronously, so a
# resource is not queryable the instant a compile returns. Measured: usually
# under 100ms, but on a freshly created database the first submission took long
# enough that an immediate query came back empty. Compiling the collector before
# the export lands would silently produce the wrong catalog and make the check
# timing-dependent -- exactly what the check rules out by mutating a fixed
# string rather than a timestamp, because a flaky test in a trustworthiness
# harness is worse than no test.
#
# Polls for the state rather than sleeping, following wait-for-openvoxdb.sh: the
# only reliable readiness signal is the thing you need being true. Exhaustion
# exits 2 (could not run), never 1 (a result about the fleet).
fleet_wait_for_export() {
  local title="$1"

  # Checked once, up front: without this a missing gatekit returns 127 on every
  # iteration and the loop reports a 60-second OpenVoxDB timeout for what is
  # actually a missing binary.
  fleet_require_gatekit

  # A shell counter, not `for _ in $(seq 1 120)`. `seq` is not POSIX, it was the
  # one host binary on this path with no probe, and its absence failed in the
  # worst available way: a command substitution in a `for` word list that fails
  # expands to nothing and does NOT trip `set -e`, so the loop ran **zero** times
  # and fell straight through to the timeout message below -- announcing a
  # 60-second OpenVoxDB timeout, zero seconds after starting, and naming
  # OpenVoxDB for a missing coreutils binary. Measured: 0 iterations, no error.
  #
  # That is the exact symptom the fleet_require_gatekit check at the top of this
  # function exists to prevent -- grep for the construct, the line moves --
  # and it was worse here because there was nothing to probe *for*:
  # the loop simply did not run. Removing the dependency beats probing it: the
  # counter needs no external binary and cannot fail.
  local i=0
  while [ "$i" -lt 120 ]; do
    i=$((i + 1))
    # 0 exported, 1 not yet, 2 the query failed; both non-zero answers mean
    # poll again, so stderr is discarded rather than repeated 120 times.
    if "$FLEET_GATEKIT" pdb-exported "$FLEET_PDB_URL" "$title" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "gate: ${title} never became queryable at ${FLEET_PDB_URL}" >&2 || exit 2
  exit 2
}

# Explicit sentinel, and the last line on purpose. The guarded source in every
# gate reads THIS file's exit status, which is the status of its last command.
# That is a function definition today, which returns 0, but nothing enforced it:
# appending any top-level command that returned non-zero would have made all
# eight gates exit 2 reporting "cannot load gate-common.sh" for a file that had
# loaded perfectly. `:` pins the status to 0 whatever is added above it.
:
