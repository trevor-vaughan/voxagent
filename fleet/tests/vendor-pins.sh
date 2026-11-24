#!/usr/bin/env bash
# Hold vendor.sh's destination guards in place.
#
# fleet/.vendor/modules is bind-mounted as the modulepath into all three nodes,
# so whatever vendor.sh leaves on disk is an input to every catalog the gate
# compiles. A pin that does not bind is therefore not a vendoring inconvenience:
# it means the gate measured a tree nobody named.
#
# Every guard here was found by review rather than by a test, which is the
# pattern this repository keeps paying for. Four of them -- a modified worktree
# at the pinned commit, a fresh clone whose ref resolves to something other than
# the pin, a checkout from an origin the table no longer names, and a ref that
# does not resolve at all leaving git's own 128 -- were measured to pass or to
# misreport before the commit that added this file.
#
# Method: copy the shipped vendor.sh and replace ONLY its REPOS table with local
# origins built under a temporary directory. The guard logic under test is the
# shipped text, so it cannot drift away from what runs in anger; the table is the
# one thing that must change, because no case here may reach the network or
# fleet/.vendor/. The substitution is verified rather than assumed -- a silently
# failed rewrite would leave every case exercising the real table, and "the test
# ran against something else entirely" is the vacuous-assertion failure this tree
# tracks.
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

# Not `set -e`. Most cases run a script that is SUPPOSED to fail, and the point
# is to read its status rather than abort on it.

# git is driven directly below, so the caller's identity and config must not
# reach it. Set here rather than only in .taskfiles/dev.yml because this script
# is also run as `bash fleet/tests/vendor-pins.sh`, the way the Venom suite
# invokes everything, and isolation that depends on the entry point is not
# isolation. /dev/null for both config scopes, .invalid addresses for both
# identities: nothing here should ever be attributable to a person.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=voxagent-vendor-test
export GIT_AUTHOR_EMAIL=voxagent-vendor-test@invalid
export GIT_COMMITTER_NAME=voxagent-vendor-test
export GIT_COMMITTER_EMAIL=voxagent-vendor-test@invalid

TESTS_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 2
BIN_DIR="$(cd "$TESTS_DIR/../bin" && pwd)" || {
  echo "vendor-pins: cannot locate fleet/bin next to $TESTS_DIR" >&2
  exit 2
}
VENDOR_SRC="$BIN_DIR/vendor.sh"
[ -r "$VENDOR_SRC" ] || {
  echo "vendor-pins: cannot read ${VENDOR_SRC}" >&2
  exit 2
}
command -v git >/dev/null || {
  echo "vendor-pins: git is not on PATH; every case below drives it directly" >&2
  exit 2
}

WORK="$(mktemp -d)" || {
  echo "vendor-pins: could not create a temporary working directory" >&2
  exit 2
}
trap 'rm -rf "$WORK" || :' EXIT

failures=0
cases=0

# origin <name> -- build a throwaway upstream and echo its path.
#
# Two commits and an annotated tag: the second commit gives "pinned at a commit
# that is not the tip" something to be, and the tag object gives the
# resolves-elsewhere case a ref whose own SHA is not the commit it names.
origin() {
  local path="$WORK/origins/$1"
  mkdir -p "$path" || exit 2
  git init --quiet "$path" || exit 2
  (
    cd "$path" || exit 2
    printf 'one\n' > f.txt || exit 2
    git add f.txt || exit 2
    git commit --quiet -m 'one' || exit 2
    printf 'two\n' >> f.txt || exit 2
    git commit --quiet -am 'two' || exit 2
    git tag -a v1 -m 'v1' || exit 2
  ) || exit 2
  printf '%s\n' "$path"
}

ORIGIN="$(origin upstream)"  || exit 2
OTHER="$(origin elsewhere)"  || exit 2

PIN="$(git -C "$ORIGIN" rev-parse HEAD)"             || exit 2
FIRST="$(git -C "$ORIGIN" rev-parse HEAD~1)"         || exit 2
TAGOBJ="$(git -C "$ORIGIN" rev-parse v1)"            || exit 2
TAGGED="$(git -C "$ORIGIN" rev-parse 'v1^{commit}')" || exit 2

# An annotated tag's own object SHA is not the commit it names. Pinning to it is
# a plausible mis-pin -- `git rev-parse v1` prints exactly this -- and `git
# checkout` of that SHA lands on the commit, so the resulting HEAD can never
# equal the pin. If git ever stopped distinguishing them the case below would
# assert nothing, so the distinction is checked rather than assumed.
[ "$TAGOBJ" != "$TAGGED" ] || {
  echo "vendor-pins: the annotated tag resolves to its own commit; the" \
       "resolves-elsewhere case would be vacuous" >&2
  exit 2
}

TABLE="${ORIGIN}	mod	${PIN}"
TABLE_TAGOBJ="${ORIGIN}	mod	${TAGOBJ}"
TABLE_BOGUS="${ORIGIN}	mod	deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
# Names OTHER while the prepared checkout below came from ORIGIN: the shape of
# repointing an entry at a fork or a mirror against a populated .vendor/.
TABLE_REPOINTED="${OTHER}	mod	${PIN}"
# A pin truncated below 40 characters. vendor.sh's header mandates full SHAs and
# both comparisons say they are exact "so a pin truncated below 40 characters
# fails loudly rather than binding to whatever happens to be checked out" -- but
# nothing tested it, and making either comparison a prefix match survived the
# whole suite. This is the fixture that claim needed.
TABLE_SHORT="${ORIGIN}	mod	${PIN:0:12}"
# Every other table uses the flat destination `mod`, but all four real module
# entries are `modules/<name>`. Without one nested table, `mkdir -p "$(dirname
# "$target")"` is never exercised: measured, deleting it broke all four real
# module destinations and the suite stayed green.
TABLE_NESTED="${ORIGIN}	modules/mod	${PIN}"
# An origin that is not there. Reaches the clone guard, which is the only one of
# vendor.sh's "cannot be read" arms that fires before a destination exists.
TABLE_GONE="${WORK}/origins/definitely-absent	mod	${PIN}"

# build_vendor <table> <out> -- shipped vendor.sh with its REPOS table replaced.
build_vendor() {
  local table="$1" out="$2"

  awk -v table="$table" '
    /^REPOS=\$\(cat <<.EOF.$/ { print; inblock = 1; next }
    inblock && /^EOF$/        { print table; print; inblock = 0; next }
    inblock                   { next }
                              { print }
  ' "$VENDOR_SRC" > "$out" || exit 2

  # The rewrite is load-bearing: had it silently done nothing, every case below
  # would clone the real modules over the network and prove nothing about the
  # guards. Both directions are checked -- the table went in, and no shipped
  # entry survived.
  if ! grep -qF "$WORK/origins" "$out"; then
    echo "vendor-pins: the REPOS rewrite did not take; cases would run against" \
         "the shipped table" >&2 || exit 2
    exit 2
  fi
  # Status captured rather than branched on directly. `grep -q` exits 2 when it
  # cannot read the file, and a bare `if grep -q` folds that into the else --
  # reading an unreadable script as "no shipped entry survived", the safe
  # branch. The paired check above uses `! grep -q` and fails safe by accident;
  # this one did not. Same shape as the eleventh and twelfth channels.
  shipped=0
  grep -q 'https://github.com/' "$out" || shipped=$?
  if [ "$shipped" -gt 1 ]; then
    echo "vendor-pins: cannot read the generated ${out}" >&2 || exit 2
    exit 2
  fi
  if [ "$shipped" -eq 0 ]; then
    echo "vendor-pins: a shipped REPOS entry survived the rewrite; cases would" \
         "reach the network" >&2 || exit 2
    exit 2
  fi
}

# prepare <shape> <vendor-dir> -- build the "destination already exists" state.
#
# A case dispatch rather than one function per shape invoked by name: a function
# called indirectly is unreachable as far as shellcheck can see (SC2317), and
# dev:lint reports that. An unknown shape is could-not-run, never a silent skip.
prepare() {
  local shape="$1" dir="$2"
  case "$shape" in
    none) : ;;
    vendored)
      git clone --quiet "$ORIGIN" "$dir/mod"      || return 1
      git -C "$dir/mod" checkout --quiet "$PIN"   || return 1
      ;;
    wrong-commit)
      git clone --quiet "$ORIGIN" "$dir/mod"      || return 1
      git -C "$dir/mod" checkout --quiet "$FIRST" || return 1
      ;;
    # Split deliberately. One shape carrying BOTH a tracked edit and an
    # untracked addition cannot fail on either half alone: measured, a
    # `--porcelain --untracked-files=no` vendor.sh passed the combined case at
    # exit 0 while the untracked-only shape caught it. The untracked half is the
    # one that matters most -- a stray .pp is what a modulepath picks up -- so it
    # needs a case that goes red when only it is lost.
    dirty-tracked)
      git clone --quiet "$ORIGIN" "$dir/mod"      || return 1
      git -C "$dir/mod" checkout --quiet "$PIN"   || return 1
      printf 'local edit\n' >> "$dir/mod/f.txt"   || return 1
      ;;
    dirty-untracked)
      git clone --quiet "$ORIGIN" "$dir/mod"      || return 1
      git -C "$dir/mod" checkout --quiet "$PIN"   || return 1
      printf 'class evil {}\n' > "$dir/mod/evil.pp" || return 1
      ;;
    not-a-checkout)
      mkdir -p "$dir"              || return 1
      printf 'stale\n' > "$dir/mod" || return 1
      ;;
    empty-dir)
      mkdir -p "$dir/mod" || return 1
      ;;
    # A real checkout at the pin whose origin has been removed. vendor.sh reads
    # remote.origin.url first, and `config --get` exits 1 when the key is absent,
    # so this reaches the "present but names no origin" arm -- one of five
    # "present but unreadable" arms that had no coverage at all. All five exist
    # to stop unreadable reading as correct, and all five could be flipped to
    # exit 0 without any case noticing.
    no-origin)
      git clone --quiet "$ORIGIN" "$dir/mod"       || return 1
      git -C "$dir/mod" checkout --quiet "$PIN"    || return 1
      git -C "$dir/mod" remote remove origin       || return 1
      ;;
    # Origin readable, HEAD not: an initialised repository with no commits, so
    # `rev-parse HEAD` fails while `config --get remote.origin.url` succeeds.
    # This is the shape a killed clone leaves behind.
    no-commits)
      mkdir -p "$dir/mod"                           || return 1
      git init --quiet "$dir/mod"                   || return 1
      git -C "$dir/mod" remote add origin "$ORIGIN" || return 1
      ;;
    # Origin and HEAD both readable, worktree not: a corrupt index breaks
    # `status --porcelain` and nothing else, which is the only one of these three
    # that reaches the third guard rather than short-circuiting at an earlier one.
    corrupt-index)
      git clone --quiet "$ORIGIN" "$dir/mod"        || return 1
      git -C "$dir/mod" checkout --quiet "$PIN"     || return 1
      printf 'garbage' > "$dir/mod/.git/index"      || return 1
      ;;
    *)
      echo "vendor-pins: unknown setup shape ${shape}" >&2 || exit 2
      exit 2
      ;;
  esac
}

# run_case <description> <expected-exit> <table> <shape> [expected-message]
#
# Each case gets its own vendor directory, so no case can observe another's
# leftovers.
#
# The message argument is not decoration. vendor.sh maps every failure to 1, so
# status alone cannot distinguish "the guard this case names fired" from
# "something else failed first". Measured instance: with `[ -e "$target" ]`
# weakened to `[ -d "$target" ]`, the not-a-checkout case still reported ok --
# the guard never ran, `mv` failed instead, and a full clone was left behind.
# Every expected-1 case below names the message its guard prints.
run_case() {
  local desc="$1" want="$2" table="$3" shape="$4" needle="${5:-}"
  local dir script out got
  cases=$((cases + 1))

  dir="$WORK/run/$cases"
  mkdir -p "$dir" || exit 2
  script="$WORK/run/$cases.sh"
  build_vendor "$table" "$script"

  # Could-not-run, NOT a failed case. Every `prepare` shape fails by `return 1`
  # from a git call or a write, and none of those failing says anything about
  # vendor.sh -- it says this harness could not build the fixture. Counting it as
  # a failure drove the script to exit 1, and 1 here is a result about vendor.sh:
  # measured with a shim failing only one setup clone, the suite reported
  # "1 of 11 cases failed" and exited 1, accusing a guard that never ran. That is
  # the outage-as-a-finding class this tree has closed fifteen times, and this
  # file was the fifteenth. adjudication.sh's `slice` already had it right.
  if ! prepare "$shape" "$dir"; then
    echo "vendor-pins: could not build the ${shape} destination for" \
         "\"${desc}\"; this is a harness failure, not a result about" \
         "vendor.sh" >&2 || exit 2
    exit 2
  fi

  out="$(bash "$script" "$dir" 2>&1)"
  got=$?

  if [ "$got" -ne "$want" ]; then
    echo "FAIL ${desc}: expected exit ${want}, got ${got}" >&2 || exit 2
    printf '%s\n' "$out" | sed 's/^/       /' >&2
    failures=$((failures + 1))
    return
  fi
  if [ -n "$needle" ]; then
    case "$out" in
      *"$needle"*) ;;
      *) echo "FAIL ${desc}: exited ${got} but without '${needle}';" \
              "a different guard fired" >&2 || exit 2
         printf '%s\n' "$out" | sed 's/^/       /' >&2
         failures=$((failures + 1))
         return ;;
    esac
  fi
  echo "ok   ${desc} (exit ${got})" || exit 2
}

# assert_head <description> <table> <dest> <expected-sha>
#
# Status alone would not catch a clone that succeeded at the wrong commit, which
# is exactly what the fresh-clone path did before it verified anything. The
# .partial check rides along because nothing else asserts it: measured, `mv` ->
# `cp -r` leaves a duplicate tree beside every successful vendoring and no case
# noticed.
assert_head() {
  local desc="$1" table="$2" dest="$3" want="$4"
  local dir script got have
  cases=$((cases + 1))

  dir="$WORK/run/$cases"
  mkdir -p "$dir" || exit 2
  script="$WORK/run/$cases.sh"
  build_vendor "$table" "$script"

  bash "$script" "$dir" >/dev/null 2>&1
  got=$?
  if [ "$got" -ne 0 ]; then
    echo "FAIL ${desc}: vendor.sh exited ${got}, expected 0" >&2 || exit 2
    failures=$((failures + 1))
    return
  fi

  have="$(git -C "$dir/$dest" rev-parse HEAD 2>/dev/null)"
  if [ "$have" != "$want" ]; then
    echo "FAIL ${desc}: HEAD is ${have:-unreadable}, expected ${want}" >&2 || exit 2
    failures=$((failures + 1))
    return
  fi
  if [ -e "$dir/${dest}.partial" ]; then
    echo "FAIL ${desc}: ${dir}/${dest}.partial survived a successful vendoring" >&2 || exit 2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc} (HEAD at the pin, no .partial)" || exit 2
}

# assert_absent <description> <table> -- neither the destination nor its
# .partial may survive a failed vendoring.
#
# A pin that stops resolving must leave no module tree rather than a wrong one;
# that is vendor.sh's own claim about why a bogus ref is safe, and it is the
# property that makes an unresolvable pin a hard stop instead of a silent drift.
#
# .partial matters most on the ref-resolved-elsewhere path, where the leftover
# would be a COMPLETE checkout at the wrong commit sitting inside the
# bind-mounted modulepath. Measured: deleting vendor.sh's `rm -rf "$tmp"` on that
# path left exactly that, and nothing noticed.
assert_absent() {
  local desc="$1" table="$2"
  local dir script
  cases=$((cases + 1))

  dir="$WORK/run/$cases"
  mkdir -p "$dir" || exit 2
  script="$WORK/run/$cases.sh"
  build_vendor "$table" "$script"

  bash "$script" "$dir" >/dev/null 2>&1
  if [ -e "$dir/mod" ]; then
    echo "FAIL ${desc}: ${dir}/mod exists; a failed vendoring left a tree" >&2 || exit 2
    failures=$((failures + 1))
    return
  fi
  if [ -e "$dir/mod.partial" ]; then
    echo "FAIL ${desc}: ${dir}/mod.partial survived; a failed vendoring left a" \
         "checkout at the wrong commit inside the modulepath" >&2 || exit 2
    failures=$((failures + 1))
    return
  fi
  echo "ok   ${desc} (no tree left behind)" || exit 2
}

echo "== vendor.sh destination guards ==" || exit 2
run_case      "fresh clone at the pin"                   0 "$TABLE"           none
assert_head   "fresh clone lands exactly at the pin"       "$TABLE"           mod         "$PIN"
run_case      "re-run over a correct checkout is a no-op" 0 "$TABLE"           vendored
run_case      "a checkout at another commit is refused"   1 "$TABLE"           wrong-commit \
              "but is pinned at"
run_case      "a destination that is not a checkout"      1 "$TABLE"           not-a-checkout \
              "exists but is not a git checkout"
run_case      "an empty destination directory"            1 "$TABLE"           empty-dir \
              "exists but is not a git checkout"
run_case      "an unresolvable pin fails as 1, not 128"   1 "$TABLE_BOGUS"     none \
              "does not resolve in"
assert_absent "an unresolvable pin leaves no tree"          "$TABLE_BOGUS"
# The three that passed silently before this file existed.
run_case      "a tracked edit at the pin is refused"      1 "$TABLE"           dirty-tracked \
              "its worktree has been modified"
run_case      "an untracked .pp at the pin is refused"    1 "$TABLE"           dirty-untracked \
              "its worktree has been modified"
run_case      "a ref that resolves away from the pin"     1 "$TABLE_TAGOBJ"    none \
              "a pin must name a commit exactly"
run_case      "a checkout from an origin no longer named" 1 "$TABLE_REPOINTED" vendored \
              "but this table names"
# Added after round 5 measured each of these mutations surviving the suite.
assert_absent "a ref resolving elsewhere leaves no tree"    "$TABLE_TAGOBJ"
run_case      "a truncated pin against a checkout"        1 "$TABLE_SHORT"     vendored \
              "pins must be full 40-character SHAs"
run_case      "a truncated pin on the creating run"       1 "$TABLE_SHORT"     none \
              "a pin must name a commit exactly"
run_case      "a checkout with no origin is refused"      1 "$TABLE"           no-origin \
              "names no origin"
run_case      "a nested destination vendors"              0 "$TABLE_NESTED"    none
assert_head   "a nested destination lands at the pin"       "$TABLE_NESTED"    modules/mod "$PIN"
# The remaining "present but unreadable" arms. Each exists to stop unreadable
# reading as correct, and each could be flipped to exit 0 unnoticed until now.
# Ordered so that each reaches its own guard rather than short-circuiting at an
# earlier one: no origin, then origin-but-no-HEAD, then origin-and-HEAD-but-no-
# worktree.
run_case      "a checkout with no commits is refused"     1 "$TABLE"           no-commits \
              "its HEAD cannot be read"
run_case      "a checkout with an unreadable worktree"    1 "$TABLE"           corrupt-index \
              "its worktree cannot be read"
run_case      "an origin that is not there"               1 "$TABLE_GONE"      none \
              "failed; mod was not vendored"

echo || exit 2
if [ "$failures" -ne 0 ]; then
  echo "vendor-pins: ${failures} of ${cases} cases failed" >&2 || exit 2
  exit 1
fi

# Hardcoded, as adjudication.sh hardcodes its own: a count derived from the
# run it checks agrees with whatever that run did. Adding or removing a case
# means editing it.
if [ "$cases" -ne 21 ]; then
  echo "vendor-pins: expected 21 cases, ran ${cases};" \
       "update this number deliberately when adding or removing one" >&2 || exit 2
  exit 1
fi
echo "vendor-pins: ${cases} cases, all as expected" || exit 2
exit 0
