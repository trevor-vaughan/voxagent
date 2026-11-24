#!/usr/bin/env bash
# G2: apply a catalog twice; the second run must change nothing.
#
# This needs no Ruby at all, and it catches a class the report differential
# cannot see: a provider that reports a change it did not make, or makes one it
# did not report. Two implementations can agree on a report while both flap.
#
# The fixture contains NO notify resource. Measured against Puppet 8.10.0: a
# notify has no state on the node to compare against and reports a change on
# every run, so a catalog containing one is never idempotent. Including one here
# would make this gate assert something no correct implementation can satisfy.
#
# EXIT CODES OF THE RUNS UNDER TEST, which are not this script's own. voxagent
# with --detailed-exitcodes returns Puppet's bitmask: 0 nothing, 2 changes,
# 4 failures, 6 both -- and 1, uniquely, for could-not-run. So 2 is the EXPECTED
# result of the first run against a fresh directory and must not be mistaken for
# a failure to start. Only 1 means the run could not happen.
#
#   0  the second run changed nothing
#   1  it changed something -- a result about voxagent
#   2  the check could not run
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || exit 2
ROOT="$(cd "$HERE/../.." && pwd)" || exit 2
VOXAGENT="${VOXAGENT:-$ROOT/bin/voxagent}"
ARTIFACTS="${ARTIFACTS:-$ROOT/.test-output/spine}"

# Refuses a missing or stale binary -- a stale one is a false green; fresh.sh
# records why.
# shellcheck source=fresh.sh
. "$HERE/fresh.sh" || exit 2
require_fresh idempotence "$ROOT" "$VOXAGENT"

WORK="$(mktemp -d)" || exit 2
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/target" || exit 2

# Sources live beside target, and the paths the catalog reconciles rather
# than creates are seeded before the first run: a purge with a stale file and
# an explicitly managed child whose content is wrong, a permissions-only
# recursion, and a mode-only file. No unmanaged directory under the purge and
# no missing source: Puppet reports the first "removed" every run and the
# second fails its first run, so neither can converge (both are in G1).
mkdir -p "$WORK/src/tree/sub" "$WORK/src/pur" "$WORK/target/pur" "$WORK/target/perms/sub" || exit 2
printf 'one\n' > "$WORK/src/one.txt" && printf 'a\n' > "$WORK/src/tree/a.txt" \
  && printf 'b\n' > "$WORK/src/tree/sub/b.txt" && printf 'new\n' > "$WORK/src/pur/new.txt" \
  && printf 'stale\n' > "$WORK/target/pur/stale.txt" && printf 'old\n' > "$WORK/target/pur/keep.txt" \
  && printf 'f\n' > "$WORK/target/perms/f" && printf 'g\n' > "$WORK/target/perms/sub/g" \
  && printf 'seed\n' > "$WORK/target/perm.txt" \
  && printf 'keep\n' > "$WORK/target/norepl.txt" && mkdir "$WORK/target/fdir" \
  && printf 'plain\n' > "$WORK/target/lnf" && ln -s old "$WORK/target/rt" \
  && printf 'real\n' > "$WORK/src/real.txt" && ln -s real.txt "$WORK/src/lnk" \
  && ln -s "$WORK/src/real.txt" "$WORK/target/flw" \
  && chmod 0755 "$WORK/target/pur" "$WORK/target/perms" "$WORK/target/perms/sub" \
  && chmod 0644 "$WORK/target/perms/f" "$WORK/target/perms/sub/g" "$WORK/target/perm.txt" || exit 2

# The links, force and replace cases below each start from a seeded state that
# the first run changes and the second must leave alone. Left out because they
# never converge: keepdir, `ensure => absent` over a directory without `force`
# (I-1, the `removed` event repeats); `ensure => file` with a `target` over an
# existing path (I-6, it alternates between a file and a link); `ensure => link`
# with `links => follow` over an existing link (the link is re-created every
# run, measured); and any resource that fails.
#
# The exec is `creates`-guarded on purpose. An unguarded exec runs on every
# apply, so a catalog holding one is never idempotent -- the same reason notify
# is excluded. Guarded, it is the only resource here that can prove voxagent
# honours a guard at all, which is what makes the second run's silence mean
# something.
cat > "$WORK/catalog.json" <<EOF || exit 2
{"catalog_format":2,"name":"spine.example","environment":"production",
 "version":1,"catalog_uuid":"00000000-0000-4000-8000-000000000000",
 "tags":[],"classes":[],"edges":[],
 "resources":[
  {"type":"File","title":"$WORK/target/a.txt","tags":[],
   "parameters":{"ensure":"file","content":"alpha\n","mode":"0644"}},
  {"type":"File","title":"$WORK/target/d","tags":[],
   "parameters":{"ensure":"directory","mode":"0755"}},
  {"type":"Exec","title":"guarded","tags":[],
   "parameters":{"command":"/usr/bin/touch $WORK/target/made",
                 "creates":"$WORK/target/made"}},
  {"type":"File","title":"$WORK/target/from-src.txt","tags":[],
   "parameters":{"source":"$WORK/src/one.txt"}},
  {"type":"File","title":"$WORK/target/tree","tags":[],
   "parameters":{"source":"file://$WORK/src/tree","recurse":true}},
  {"type":"File","title":"$WORK/target/pur","tags":[],
   "parameters":{"ensure":"directory","source":"$WORK/src/pur","recurse":true,"purge":true}},
  {"type":"File","title":"$WORK/target/pur/keep.txt","tags":[],
   "parameters":{"ensure":"file","content":"kept\n"}},
  {"type":"File","title":"$WORK/target/perms","tags":[],
   "parameters":{"recurse":true,"mode":"0640"}},
  {"type":"File","title":"$WORK/target/perm.txt","tags":[],
   "parameters":{"mode":"0600"}},
  {"type":"File","title":"$WORK/target/perm-absent","tags":[],
   "parameters":{"mode":"0600"}},
  {"type":"File","title":"nsx","tags":[],
   "parameters":{"path":"$WORK/target/ns.txt","ensure":"file","content":"ns\n"}},
  {"type":"File","title":"$WORK/target/norepl.txt","tags":[],
   "parameters":{"ensure":"file","content":"other\n","replace":false}},
  {"type":"File","title":"$WORK/target/fdir","tags":[],
   "parameters":{"ensure":"file","content":"over a directory\n","force":true}},
  {"type":"File","title":"$WORK/target/lnf","tags":[],
   "parameters":{"ensure":"link","target":"$WORK/src/real.txt"}},
  {"type":"File","title":"$WORK/target/rt","tags":[],
   "parameters":{"ensure":"link","target":"new"}},
  {"type":"File","title":"$WORK/target/sl","tags":[],
   "parameters":{"ensure":"symlink","target":"$WORK/src/real.txt"}},
  {"type":"File","title":"$WORK/target/flw","tags":[],
   "parameters":{"ensure":"file","content":"followed\n","links":"follow"}},
  {"type":"File","title":"$WORK/target/from-link","tags":[],
   "parameters":{"source":"$WORK/src/lnk"}}
 ]}
EOF

"$VOXAGENT" apply --catalog "$WORK/catalog.json" --detailed-exitcodes \
  >"$WORK/first.out" 2>&1
first=$?
if [ "$first" -eq 1 ]; then
  echo "idempotence: the first run could not complete (exit 1)" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi
if [ "$((first & 4))" -ne 0 ]; then
  echo "idempotence: the first run had resource failures (exit $first)" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi
# A first run that changed NOTHING means the fixture never reached the state it
# is supposed to converge from, so the second run's silence would prove nothing.
if [ "$((first & 2))" -eq 0 ]; then
  echo "idempotence: the first run reported no changes (exit $first), so the" >&2
  echo "idempotence: second run cannot demonstrate convergence" >&2
  sed -n '1,20p' "$WORK/first.out" >&2
  exit 2
fi

"$VOXAGENT" apply --catalog "$WORK/catalog.json" --detailed-exitcodes \
  >"$WORK/second.out" 2>&1
second=$?
case "$second" in
  0)
    echo "idempotence: the second run changed nothing"
    exit 0
    ;;
  1)
    echo "idempotence: the second run could not run" >&2
    sed -n '1,20p' "$WORK/second.out" >&2
    exit 2
    ;;
  *)
    echo "idempotence: the second run reported changes or failures (exit $second)" >&2
    mkdir -p "$ARTIFACTS" \
      && cp "$WORK/first.out" "$ARTIFACTS/idempotence-first.out" \
      && cp "$WORK/second.out" "$ARTIFACTS/idempotence-second.out" \
      && cp "$WORK/catalog.json" "$ARTIFACTS/idempotence-catalog.json" \
      && echo "idempotence: evidence in $ARTIFACTS" >&2
    sed -n '1,20p' "$WORK/second.out" >&2
    exit 1
    ;;
esac
