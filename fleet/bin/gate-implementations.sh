#!/usr/bin/env bash
# Gate check 2: compare OpenVox against the puppet gem on the same node.
#
# This deliberately does NOT assert the catalogs match. Whether two
# independent implementations agree on this data is unknown, and that is
# precisely the measurement. Asserting equality would turn an open question
# into a red build the moment reality disagreed.
#
# Each node is compared TWICE, because the two sides differ by more than the
# compiler:
#
#   native      each side runs its own fact engine -- openfact 5.6.1 on the
#               RPM side, facter 4.10.0 on the gem side. They disagree on real
#               values (processors.isa resolves to x86_64 under openfact and
#               unknown under facter 4), so this run varies implementation,
#               version AND fact engine, and attributes none of them.
#   normalised  both sides compile from one pre-captured fact set through
#               Puppet's json facts terminus, so the fact engine stops being a
#               variable and what remains is implementation and version.
#
# The delta between the two counts is the fact engine's contribution.
#
# EXIT CODES. voxdiff exits 0 identical, 1 diverged, 2 could-not-run. THIS
# SCRIPT deliberately collapses voxdiff's 0 and 1 into its own 0, because the
# check does not assert equality -- a divergence is the measurement, not a
# failure. It exits 2, and only 2, when a comparison could not run, matching
# gate-determinism.sh. It never exits 1. A gate that reported an outage with
# the code it uses for a finding would make the finding unbelievable.
#
# The normalised set is captured from the RPM side because OpenVox is the
# target platform. Capturing from the gem side would answer a different
# question, so if that is ever wanted it belongs as a third run, not a swap.
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

# Usage errors exit 2, like every other could-not-run condition here.
OUT_DIR="${1:-}"
if [ -z "$OUT_DIR" ]; then
  echo "usage: gate-implementations.sh <out-dir>" >&2 || exit 2
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

# Checked once, up front: the fact-capture check below runs gatekit, and a
# missing or stale one is could-not-run for every node, not a capture failure.
fleet_require_gatekit
fleet_clear_artefacts "$OUT_DIR"/implementations-*.json "$OUT_DIR"/implementations-*.err

# Captured facts and shims live at a fixed path and are rebuilt per node, so
# they do not accumulate; the last node's set is left in place deliberately, as
# the evidence behind whatever the normalised run reported.

RPM_PUPPET=/opt/puppetlabs/bin/puppet
GEM_PUPPET=/usr/local/bin/puppet
NORM_DIR=/var/tmp/fleet-normalised

# Only the RedHat nodes: node-deb carries a single Puppet implementation by
# design, so there is nothing on it to compare. gate-determinism.sh covers all
# three because determinism is a per-node property.
NODES="fleet_node_el:node-el fleet_node_el_alt:node-el-alt"
rc=0

# compare_one <container> <node> <mode> <left-binary> <right-binary>
#
# Everything here is `local` except rc, which must stay global so a failure
# propagates to the exit status. Without that, these names would shadow the
# driver loop's own container/node -- harmless while the loop re-derives both
# each iteration, but a trap for anyone who restructures it later.
compare_one() {
  local container="$1" node="$2" mode="$3" left="$4" right="$5"
  local log="$OUT_DIR/implementations-${node}-${mode}.json"
  local err="$OUT_DIR/implementations-${node}-${mode}.err"
  local code

  # Probe-then-use; the reasoning is spelled out in gate-determinism.sh. A
  # redirection that cannot open its target fails before podman runs, and that is
  # status 1, which the case below reports as DIVERGED.
  #
  # Measured with a dangling symlink at $log, planted after the entry clear had
  # already run: this printed
  #
  #   node-el-alt [native]: DIVERGED across implementations ( differences)
  #
  # -- the count empty because `grep -c` could not read the file either -- and
  # the gate still exited 0, since rc is raised only by the could-not-run branch.
  # The venom suite asserts nothing about this line, so a fabricated
  # cross-implementation divergence passed the gate silently. That is the most
  # serious claim this check can make.
  #
  # rc=2 and return, not exit: this script deliberately keeps measuring the
  # remaining nodes rather than aborting the loop.
  if ! : > "$log" || ! : > "$err"; then
    echo "${node} [${mode}]: cannot open ${log} or ${err} -- comparison skipped" >&2 || exit 2
    rc=2
    return 0
  fi

  # One temp root, removed afterwards, matching gate-determinism.sh. These
  # containers are long-lived, so a per-run leak would grow the writable layer
  # on every gate invocation.
  set +e
  podman exec "$container" sh -c "
      d=\$(mktemp -d) || exit 2
      mkdir -p \"\$d/conf\" \"\$d/var\" || exit 2
      voxdiff compare -json \
        -left  '${left}' \
        -right '${right}' \
        $(fleet_compile_args "$node") \
        -confdir \"\$d/conf\" \
        -vardir  \"\$d/var\"
      s=\$?
      rm -rf \"\$d\"
      exit \$s
    " > "$log" 2>"$err"
  code=$?
  set -e

  case "$code" in
    0) echo "${node} [${mode}]: IDENTICAL across implementations" || exit 2 ;;
    # Count `"path":` keys, not lines containing `path`: an only-in-left or
    # only-in-right difference embeds the whole resource, and Puppet resources
    # routinely carry a `path` parameter that would inflate the total.
    1) echo "${node} [${mode}]: DIVERGED across implementations ($(grep -c '"path":' "$log") differences) -- see $log" || exit 2 ;;
    *) echo "${node} [${mode}]: comparison FAILED to run (exit ${code})" >&2 || exit 2
       [ -r "$err" ] && { cat "$err" >&2 || exit 2; }
       rc=2 ;;
  esac
  return 0
}

# Capture one fact set and generate wrappers that make each implementation
# read it instead of running its own engine. The wrappers are generated in the
# container rather than committed because fleet/bin is not mounted into the
# nodes -- only the voxdiff binary is. For the same reason the capture is
# validated on the host, by gatekit, after being copied out: the wrappers it
# leaves behind are harmless if validation then fails, because a failed
# preparation skips the node.
prepare_normalised() {
  podman exec -i "$1" sh -s -- "$2" "$RPM_PUPPET" "$GEM_PUPPET" "$NORM_DIR" <<'REMOTE' || return
set -eu
node="$1"; rpm="$2"; gem="$3"; d="$4"
rm -rf "$d"
mkdir -p "$d/data/facts" "$d/bin" "$d/var" "$d/conf"
"$rpm" facts find "$node" --render-as json --vardir "$d/var" --confdir "$d/conf" \
  > "$d/data/facts/${node}.json"

for pair in "rpm ${rpm}" "gem ${gem}"; do
  name="${pair%% *}"; impl="${pair#* }"
  printf '#!/bin/sh\nexec %s "$@" --facts_terminus json --client_datadir %s/data\n' \
    "$impl" "$d" > "$d/bin/${name}-puppet"
  chmod +x "$d/bin/${name}-puppet"
done
REMOTE

  # Validate the capture rather than trusting its exit status. `puppet facts
  # find` exits 0 even when individual resolvers fail -- on this host the lscpu
  # resolver errors on every run and the affected facts are simply absent. A
  # truncated capture would normalise BOTH sides onto the same wrong facts, the
  # comparison would still report IDENTICAL, and the delta this check exists to
  # produce would be attributed to the compiler. That is the one remaining way
  # this gate could be believed and wrong, so it fails loudly instead.
  local facts="$OUT_DIR/implementations-$2-facts.json"
  podman exec "$1" cat "$NORM_DIR/data/facts/$2.json" >"$facts" || return
  "$FLEET_GATEKIT" fleet-facts "$facts" "$2" >&2
}

for pair in $NODES; do
  container="${pair%%:*}"
  node="${pair##*:}"

  compare_one "$container" "$node" native "$RPM_PUPPET" "$GEM_PUPPET"

  # Classified like a comparison, not left bare. As a plain command under
  # `set -e` this would abort the script with the REMOTE command's status --
  # frequently 1, which under this harness's contract reads as "the catalogs
  # diverged". It would also kill the loop, silently leaving later nodes
  # unmeasured with nothing in the output to say so.
  prep_err="$OUT_DIR/implementations-${node}-prepare.err"

  # Probed like the comparison logs above. An unopenable $prep_err makes the
  # redirection fail before prepare_normalised ever runs, and the branch below
  # then reports "fact capture FAILED" -- the right classification bolted to the
  # wrong cause, which is exactly the confusion gate-common.sh says these guards
  # exist to prevent.
  if ! : > "$prep_err"; then
    echo "${node} [normalised]: cannot open ${prep_err} -- fact capture skipped" >&2 || exit 2
    rc=2
    continue
  fi

  set +e
  prepare_normalised "$container" "$node" 2>"$prep_err"
  prep_code=$?
  set -e

  if [ "$prep_code" -ne 0 ]; then
    echo "${node} [normalised]: fact capture FAILED (exit ${prep_code}) -- skipping" >&2 || exit 2
    # Guarded like its twin above. Unguarded, a `cat` that failed here exited 1 --
    # which this script's header says it never does -- and, because `set -e` kills
    # the script rather than the iteration, `rc=2` and `continue` never ran and
    # the remaining node went unmeasured with nothing in the output to say so.
    # Reachable only when the echo above succeeds and this write then fails, so a
    # wholly broken stderr is shadowed by that echo and does not expose it.
    [ -r "$prep_err" ] && { cat "$prep_err" >&2 || exit 2; }
    rc=2
    continue
  fi

  compare_one "$container" "$node" normalised \
    "$NORM_DIR/bin/rpm-puppet" "$NORM_DIR/bin/gem-puppet"
done

exit "$rc"
