#!/usr/bin/env bash
# Clone the external sources the fleet depends on, at pinned refs.
#
# Everything lands under fleet/.vendor/, which is gitignored.
#
# Every ref below is a COMMIT, never a branch name. That is the whole point of
# this file: the gate's results are claims about a specific set of inputs, and a
# branch pin lets those inputs move underneath it, so a green run next week could
# be measuring different code than the green run that was reviewed. None of these
# repositories publishes tags this fleet can rely on, which is why the pins are
# raw SHAs rather than versions.
#
# The four module pins are the commits that were checked out when the eight-check
# gate was last green, read back out of the working checkouts rather than picked
# from upstream, so the pinned tree is the tested tree and not merely a
# contemporaneous one.
#
# Updating a pin is a deliberate act with a cost: re-vendor from scratch, run
# `task fleet:gate`, and record the new commit here only once it is green. Do not
# reach for a branch name to skip that -- an unreproducible gate is worth less
# than a stale one, because a stale one at least says what it measured.
#
# Full 40-character SHAs, not the 7-character prefixes this file carried first.
# Ambiguity was never a silent hazard -- `git checkout` refuses an ambiguous
# prefix rather than choosing a candidate, measured as "error: short object ID
# ... is ambiguous" followed by exit 1 -- but a prefix is grindable: producing an
# object with a chosen 7-hex prefix is roughly 2^28 work, so an upstream with
# push access could make a pin unresolvable at will. Full SHAs remove that for
# the price of line length, and each one can be checked against `git rev-parse`
# output without any abbreviation rules in the way. git's own heuristic already
# wanted 8 characters for stdlib, so the old pins were a character shorter than
# git itself would have printed.
#
# A pin that stops resolving cannot degrade into a branch checkout: the guarded
# `git checkout` below exits 1 before the `mv`, and the destination is never
# created, so the fleet has no module tree to compile against rather than a wrong
# one. Measured with a full-length bogus SHA -- "fatal: unable to read tree", and
# and nothing left behind at all. An earlier version left a `.partial` for the
# next run to clear, which understated the hazard twice over: that leftover is a
# COMPLETE clone at the remote's default branch, and the next run cleared it only
# to recreate it, so the tree was never actually observed without it. Every
# failure path now removes its own temporary tree before exiting, and
# fleet/tests/vendor-pins.sh asserts that neither the destination nor its
# `.partial` survives a failed vendoring.
#
# The guard is what makes the status 1; unguarded, `set -e` carried git's own
# 128 out of the script, which named neither the repository nor the reason.
#
# A pin can fail to bind in five further ways that have nothing to do with
# resolving, and every one of them is likelier than a ref going missing. All five
# are checked below. Four were measured to pass silently before they were; the
# fifth, a destination that is not a checkout, was closed earlier. Counting these
# is not decoration -- the count said four over five paragraphs for one commit,
# and docs/maintainers/fleet.md says six because it also counts the resolving
# failure above.
#
# An existing checkout at the wrong commit: this script skips any destination
# that already has a .git. Until the HEAD comparison existed, editing a ref here
# was a no-op against a populated fleet/.vendor/ -- the gate would go on
# measuring the old trees while this file claimed otherwise. Changing four refs
# at once, as the commit that introduced these SHAs did, is exactly the situation
# that invites it.
#
# An existing checkout from a DIFFERENT origin: the same no-op, one column over.
# Repointing an entry at a fork or a mirror and re-running changed nothing,
# because nothing read remote.origin.url.
#
# An existing checkout that is at the pin but has been EDITED: a commit id names
# a tree, and until the worktree check existed nothing confirmed the tree on disk
# was still that one. This is the sharpest of the four, because it is the only
# one where the gate compiles code that exists in no repository at all.
#
# A fresh clone whose ref resolves to something else: `git checkout` accepts an
# annotated tag's object SHA and a branch name, landing on a commit that is not
# the pin. Nothing compared them on the creating run, which is the run every
# clean checkout and every CI runner performs.
#
# A destination that exists but is NOT a checkout: that falls past the .git test
# to the clone path, where `mv` into an existing directory nests instead of
# failing. Measured: exit 0, the clone parked one level down, "vendor: done"
# printed over a tree nothing had replaced. Guarded below.
#
# fleet/tests/vendor-pins.sh holds all of this in place, offline, against local
# origins -- `task dev:vendor-pins`.
#
# This is a setup script, not a gate: it has no 0/1/2 contract, and a non-zero
# exit here means only that vendoring did not complete.
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

VENDOR_DIR="${1:?usage: vendor.sh <vendor-dir>}"
mkdir -p "$VENDOR_DIR"

# repo-url<TAB>destination<TAB>pinned-ref
REPOS=$(cat <<'EOF'
https://github.com/trevor-vaughan/simp-webrick	simp-webrick	5491f424a370bd0e18639426f2ac8b14aa1738f6
https://github.com/puppetlabs/puppetlabs-stdlib	modules/stdlib	829af9ab4685c1fc6498e0cddd6d6aa7669843b2
https://github.com/puppetlabs/puppetlabs-concat	modules/concat	8cbb369535b3c376e7ef9f9ea732c6a600b8b0bc
https://github.com/simp/pupmod-simp-simplib	modules/simplib	a5122e9c0094e5c629f7585228f3f7c35b050970
https://github.com/simp/pupmod-simp-compliance_markup	modules/compliance_markup	2897944b94bdaa62cbb64c2f0fa464ff5e56ea95
https://github.com/voxpupuli/openvox-ca	openvox-ca	43c8dd407655fceffb3c187d69413e0fa8eb119c
EOF
)

while IFS=$'\t' read -r url dest ref; do
  [ -n "$url" ] || continue
  target="$VENDOR_DIR/$dest"
  if [ -d "$target/.git" ]; then
    # Three things must hold for an existing checkout to be the tree this table
    # names: it came from that origin, it sits at that commit, and nothing has
    # edited it since. Each was a way for a pin to not bind until it was checked.
    # Tested outermost first, so the message names the largest thing that is
    # wrong rather than a symptom of it.
    #
    # Every one of them is guarded, because a bare `git` failing under `set -e`
    # would exit with git's own status for a checkout this script cannot
    # describe, and "present but unreadable" must never read as "present and
    # correct".
    have_url=$(git -C "$target" config --get remote.origin.url) || {
      echo "vendor: $dest is present but names no origin;" \
           "remove ${target} and re-run" >&2
      exit 1
    }
    if [ "$have_url" != "$url" ]; then
      echo "vendor: $dest was cloned from ${have_url} but this table names" \
           "${url}; remove ${target} and re-run" >&2
      exit 1
    fi

    have=$(git -C "$target" rev-parse HEAD) || {
      echo "vendor: $dest is present but its HEAD cannot be read;" \
           "remove ${target} and re-run" >&2
      exit 1
    }
    # Exact, not a prefix match. A prefix would let a pin truncated to fewer
    # than 40 characters bind silently to whatever happened to be checked out,
    # which is the one remaining way a pin could fail to mean what it says --
    # and the header above mandates full SHAs precisely so it cannot. The
    # message prints both values so the cause is obvious.
    if [ "$have" != "$ref" ]; then
      echo "vendor: $dest is at ${have} but is pinned at ${ref};" \
           "remove ${target} and re-run (pins must be full 40-character SHAs)" >&2
      exit 1
    fi

    # A commit id names a tree; it does not prove the tree on disk still matches
    # it. fleet/.vendor/modules is bind-mounted as the modulepath into all three
    # nodes, so a file edited or added here reaches every catalog the gate
    # compiles while this script goes on reporting the tree as pinned-correct.
    # Measured before this check existed: a checkout at the pin carrying a
    # modified file and an added .pp printed "already present at <pin>" and
    # exited 0. --porcelain lists untracked files too, which is the half that
    # matters most -- a stray .pp is precisely what a modulepath picks up.
    #
    # It does NOT list IGNORED files, and Puppet's modulepath does not consult
    # .gitignore. Measured: a checkout at its pin carrying a .pp covered by that
    # tree's own committed .gitignore produces empty --porcelain output and passes
    # here. No live instance -- none of the six vendored trees ignores a Puppet
    # autoload path today -- but a pin bump could introduce one, so this check is
    # "tracked modifications and non-ignored untracked files", not "nothing has
    # changed". --ignored=matching would close it at the cost of failing on build
    # artefacts.
    dirty=$(git -C "$target" status --porcelain) || {
      echo "vendor: $dest is present but its worktree cannot be read;" \
           "remove ${target} and re-run" >&2
      exit 1
    }
    if [ -n "$dirty" ]; then
      echo "vendor: $dest is at ${ref} but its worktree has been modified;" \
           "remove ${target} and re-run" >&2
      exit 1
    fi

    echo "vendor: $dest already present at $ref"
    continue
  fi

  # A destination that exists but has no .git falls through the branch above, and
  # `mv` into an existing directory NESTS rather than failing: measured, `mv src
  # target` with target present produces target/src and exits 0. The clone would
  # land at modules/stdlib/stdlib.partial/, this script would print "done" and
  # exit 0, and the fleet would go on compiling whatever was already there --
  # the header's paragraph beginning "A destination that exists but is NOT a
  # checkout". Named rather than numbered: this citation said "its third form"
  # until three paragraphs were inserted above it and the ordinal silently began
  # pointing at the origin-URL case instead.
  #
  # Checked rather than solved with `mv -T`, which is GNU-only: this script has
  # no other non-POSIX dependency and should not acquire one.
  if [ -e "$target" ]; then
    echo "vendor: ${target} exists but is not a git checkout;" \
         "remove it and re-run" >&2
    exit 1
  fi

  echo "vendor: cloning $dest at $ref"
  mkdir -p "$(dirname "$target")"
  tmp="${target}.partial"
  rm -rf "$tmp"
  # Guarded rather than left to `set -e`, which would abort carrying git's own
  # status: 128 for a ref that does not resolve, which says nothing about which
  # of six repositories failed or why.
  git clone --quiet "$url" "$tmp" || {
    rm -rf "$tmp"
    echo "vendor: cloning ${url} failed; ${dest} was not vendored" >&2
    exit 1
  }
  git -C "$tmp" checkout --quiet "$ref" || {
    # Removed, not left for the next run to clear. A clone whose checkout failed
    # is a COMPLETE tree at the remote's default branch -- a wrong-commit module
    # sitting inside the bind-mounted modulepath under a `.partial` suffix. The
    # ref-resolved-elsewhere path below already removed it; leaving it here was
    # the same hazard through a different door, and nothing asserted either way
    # until fleet/tests/vendor-pins.sh did.
    rm -rf "$tmp"
    echo "vendor: ${ref} does not resolve in ${url};" \
         "${dest} was not vendored" >&2
    exit 1
  }
  # The comparison in the branch above guards a destination that already exists.
  # This one guards the run that CREATES it -- the path every clean checkout and
  # every CI runner takes, because fleet/.vendor/ is gitignored, and so the path
  # that matters most. `git checkout` succeeding does not mean HEAD equals $ref:
  # an annotated tag's own object SHA checks out the commit it names, and a
  # branch name checks out whatever its tip is today. Measured before this check
  # existed: a fresh run with such a ref exited 0 having printed "done", with
  # HEAD != pin, and the mismatch surfaced only on the NEXT run -- as an error
  # that could not be cleared without deleting the tree by hand.
  fresh=$(git -C "$tmp" rev-parse HEAD) || {
    echo "vendor: ${dest} was cloned but its HEAD cannot be read" >&2
    exit 1
  }
  if [ "$fresh" != "$ref" ]; then
    echo "vendor: ${ref} resolved to ${fresh} in ${url};" \
         "a pin must name a commit exactly" >&2
    rm -rf "$tmp"
    exit 1
  fi
  mv "$tmp" "$target"
done <<< "$REPOS"

echo "vendor: done"
