# Maintainer notes: what per-property attribution cost, and what it proved

For maintainers. Split out of the [apply spine](apply-spine.md) notes, which define the terms used here (G1 to G5, Tier 3). Back to the [README](../../README.md).

Closing the spine's first known defect — a failed resource reported one
`resource_error` that omits the property, where Puppet names the property that
failed — was run as an experiment in two parts: whether it could be done, and
whether it was worth doing. The implementation work answered the first. This section answers
the second, and it is written as evidence rather than as a recommendation. Every
number below carries the command that produced it; re-run them rather than
trusting them.

## What it cost

Measured with `git diff --numstat -- <pathspec>` over the attribution work
alone — from the spine's last commit to the commit that finished this section —
summed per row. Those commits have since been squashed into one, so the figures
are a record of that measurement rather than a command that reproduces it; a
diff to the present would fold later work into them.

| | Added | Removed | Files |
|---|---|---|---|
| Production Go | 872 | 169 | 5 |
| Test Go | 2041 | 181 | 5 |
| Gate scripts and fixtures | 189 | 5 | 4 |
| Docs (this file and the design notes) | 2904 | 12 | 3 |

A caution that cost time while writing this: `git diff A..HEAD` compares two
endpoints, it does not sum the intervening commits. Lines this section rewrote
in the paragraphs above had themselves been *added* by an earlier commit inside
the same window, so they never appear as deletions in the cumulative diff, and
the Docs row is smaller than adding each commit's own `--numstat` would predict.
Re-measure the range; do not add up the diffs of individual commits.

**552 of the 872 added production lines are comment** — 63% against numstat's
872, or 65% against the 848 non-blank added lines. The two denominators differ by
exactly the 24 added blank lines, which numstat counts and
`git diff -U0 … | grep '^+[^+]'` does not: that pipeline prints 848, not 872,
because a blank added line is a bare `+`. Comments are the lines of that output
matching `^\s*(//|\*|/\*)`.

Per file, over non-blank added lines: `ral.go` 90% (39 of 43), `exec.go` 73%,
`transaction.go` 72%, `file.go` 58%. The contract file is almost entirely prose
because the contract is almost entirely convention — which of three return values
a provider reaches is not visible in any signature.

The test diff is the more informative half, because **no new test file was
created** (no `*_test.go` file was added in that range). All 2041 lines landed in five files that already existed. Within
them:

- **42 test functions added**, **6 deleted**, **73 surviving functions modified**.
- Of those 73, **65 are mechanical**: every changed line in the body mentions
  `Apply(`, because `Apply` gained a third return value and the compiler
  demanded the edit. **8 changed an assertion** — six in `exec_test.go`, two in
  `file_test.go`.

So the honest cost of the contract change in existing tests is the rewritten
assertions plus 6 deletions, not 73. The 65 are the compiler doing its job, and
they are the argument *for* changing a return signature rather than smuggling
the new channel through a context or an optional interface: the edit is
unmissable and mechanical.

**The six deleted tests are the sharpest single piece of evidence**, because
each name is a belief this work falsified:

| Deleted test | The belief it encoded |
|---|---|
| `TestExecNonZeroExitIsAnError` | an exec's non-zero exit is a resource failure, not a `returns` property failure |
| `TestExecReturnsWritesActualExitCodeIntoChange` | superseded by the `Is`/`IsDisplay` split |
| `TestFileDirectoryMissingParentIsAnError` | a missing parent is a resource failure, not an `ensure` property failure |
| `TestFileOwnerChangeUnprivilegedFails` | unprivileged ownership fails rather than warns |
| `TestFilePartialApplyReturnsChangesAlongsideError` | a provider may return changes *and* a bare error |
| `TestFileUnwritableParentIsAnError` | unattributed |

Five of the six were not merely imprecise; they asserted the defect. A test
suite that is green against the wrong contract is the cost this kind of work
actually pays.

## Did the `Change.Err` contract hold, and would it survive `package` and `service`?

Across `file`, `exec`, and `notify`, yes — the contract expressed every shape
each provider needed. But the contract was widened **twice within the same body of
work**, not once: `Change.Err` on 2026-09-18, then a third `Apply` return for
run-level warnings on 2026-09-19, fourteen commits later,
because providers had no channel to the run log at all and the
unprivileged-ownership warning had nowhere to go. A contract that needed a
second widening before its first body of work was finished, for a requirement
nobody anticipated, is weak evidence that it is now complete.

D-11 is the surviving hole: on a created path the collapsed metadata failure is
wrapped unconditionally, because voxagent's collapse runs after the raise and
cannot tell which layer raised it, where Ruby distinguishes a `Puppet::Error`
from every other `StandardError` (`property.rb:490` versus `:492`). `Change.Err`
carries a Go `error`, and a Go error does not carry its originating layer the
way an exception class does.

Against `package` and `service` — the next two providers, read in Puppet 8.10.0
source before either was implemented — the interface **survives with additive changes**,
with one clear vindication and five named gaps.

The vindication is `type/package.rb:218-224` (comment at `:217`). `should_to_s`
substitutes `@latest`
— the concrete resolved version — into the *message*, while
`resource_harness.rb:188` has already put `property.should` — the literal symbol
`:latest` — into the event's `desired_value`. That is exactly the `Is` versus
`IsDisplay` split, in a type nobody had implemented, reached independently of
the `file` owner-uid measurement that motivated it. Had the contract shipped
with one value per side, `package { ensure => latest }` would have had to be
wrong in either the event or the message. This is the strongest argument that
changing the contract now was better than waiting.

The gaps, each of which is additive rather than a reshaping:

| Gap | Puppet's behaviour | Why the contract cannot express it |
|---|---|---|
| **No suitable provider** | `transaction.rb:132-135` sets `resource_status(resource).failed = true` with **zero events**, then logs one `Puppet.err` per type at teardown (`:146`) | voxagent's bare `err` always synthesizes a `resource_error` event. There is no "failed, no events". Puppet also decides suitability *before* applying, so `Apply` is the wrong place to ask |
| **`flush` failing after changes** | `resource_harness.rb:25-28` runs `flush` after all syncs, but only `if status.changed? && !resource.noop?`; a raise is caught by the rescue at `:29` and the status then carries both the recorded change events **and** a `resource_error` | the transaction drops `changes` when the error return is non-nil, as the contract promises, except for a `ral.FlushError`, whose changes take the success path and are followed by the `resource_error` (`transaction.go:569-570`) |
| **Warning dedupe key** | `maybe_log`'s dedupe is keyed on calling class *and* message (`util/warnings.rb:28-29`), and is shared by `warnonce`, `notice_once`, and `debug_once` (`:8`, `:14`, `:18`). Plain `Puppet.warning` — what package providers actually call, e.g. `provider/package/gem.rb:152` — does not dedupe at all | the transaction's `warned` map keys on `DedupeClass` and message, and only when `DedupeClass` is non-empty (`logWarnings`), so a warning with no class is never deduped, as in Ruby |
| **Warning source** | a warning raised on a provider instance is *not* sourced at the resource's path: `log_source` returns `path` only for a `Puppet::Type` or `Puppet::Parameter` (`util/logging.rb:298-302`), and a provider is neither, so it falls through to `to_s` — `"#{@resource}(provider=#{self.class.name})"` (`provider.rb:560`), e.g. `Package[foo](provider=gem)` | `ral.Warning` carries its own `Source`, which `logWarnings` logs |
| **No prefetch hook** | `prefetch_if_necessary` (`transaction.rb:243-260`) delegates at `:259` to `Transaction#prefetch` (`:374-389`), which calls `provider_class.prefetch(resources)` at `:381` — for packages, `provider/package.rb:7-14`, enumerating every installed package once per (type, provider class). A failure is recorded at `:386` and fails every resource of that type | `Provider` had no per-run bulk-read hook and no way to fail a whole type (it has `Prefetch` now, below). Also a performance point: without it, N packages means N `rpm -q` invocations |

None of those five is a divergence-register entry. The register records where
voxagent's *implemented* behaviour differs from Puppet's; these are shapes that
two unimplemented types would need and the contract cannot yet carry. They become
divergences the moment `package` or `service` ships, and the rule at the top of
this document applies to them then.

`package` has since shipped, bounded to dnf, and settled four of the five that
way. **No suitable provider** and **warning dedupe key** are now D-12 and D-16.
**No prefetch hook** and **warning source** were closed rather than registered:
`Provider` gained `Prefetch`, and `Warning` carries its own `Source`, so a
provider-class warning is sourced `Puppet::Type::Package::ProviderDnf` as Ruby's
is. **`flush` failing after changes** is untouched and stays a gap: dnf's
`flush` is `@property_hash.clear` (`provider/package.rb:17-19`), which runs no
command and cannot raise, so `package` does not reach the shape. `service` has
shipped too, and `internal/units/service` has no `Flush`.

One finding landed on this repository rather than on Puppet, and has been fixed.
`internal/ral/ral.go` commented that `Change.Name` need only be set for `ensure`
"because only ensure declares per-value events" — true of `file`, `exec`, and
`notify`, and false of Puppet.

`type/service.rb:68-92` carries five per-value
event declarations on `enable`, under four distinct names: `:service_enabled`
(`:68`), `:service_disabled` (`:72`, and again at `:82` for `mask`),
`:service_manual_start` (`:76`), and `:service_delayed_start` (`:90`). Behaviour
was never affected — `Name` is per-`Change` — so this was a comment that
generalised the three implemented types into a rule about Puppet, which is the
specific way a porting note goes wrong.

`service` also breaks the per-property isolation this document states as the
model. `resource_harness.rb:84-99` syncs `ensure` first and skips the properties
loop entirely unless `ensure` was a no-op, so `Service#ensure.sync`
(`type/service.rb:120-136`) syncs `logonaccount` and `enable` inline by calling
`Property#sync` directly, bypassing `sync_if_needed`.

Because `event = super()`
is computed at `:127` *before* the `enable` sync at `:132`, a failure to enable
is reported as **`ensure`'s** failure even though the service started. The
contract can represent that — one `Change` for `ensure` carrying `enable`'s
error — but only if the implementer knows to, and nothing in the interface hints
at it.

## How many of the must-measure predictions were wrong?

The predictions recorded when the apply spine was designed (made by reading
Ruby's source before running it) are the only tally with a denominator, because
there were six and they were written down *before* measurement: **three wrong, one partially wrong, one
correct and measured, one correct but never measured.** That last split matters
and the record does not make it: prediction #2 is recorded `✅ (read, not run)`, so on a
list of *must-measure* predictions it is a prediction that was re-read rather
than checked. Counting it as a clean success would be the same move this section
refuses two paragraphs below. None of the three outright failures was in the
part of the design that felt uncertain.

Four further source-derived claims were falsified after those predictions were recorded: the
`ensure => absent` unwrapping reason (right answer, wrong mechanism), the
`--noop` suppression not actually being noop-gated, the `" (noop)"` claim being
overstated, and D-8's security claim that a `Sensitive()` value reached an
all-readable file in clear — measured at **zero** occurrences, both content paths
reporting a `{sha256}` digest, since corrected.

**Those four are a count, not a rate.** Nobody enumerated the source-derived
claims that turned out correct after the predictions were recorded, so "seven of ten
wrong" would be a survivorship figure and is not claimed here. The defensible
statement is the narrow one: of six pre-registered predictions, three were wrong,
and four more source-derived claims were caught later by measurement.

Two further errors are deliberately **excluded** from that tally, because
including them would support the wrong remedy. One task brief cited
`TestResourceLevelFailureIsStillResourceError`, which has never existed as a
test:

```bash
git log -S'func TestResourceLevelFailureIsStillResourceError(' --all -- '*_test.go'   # empty
```

The pathspec is load-bearing. Without it the same command returns six commits,
among them `0350d62`, which added a planning document, because that document embeds the proposed test's source — which is precisely the
error being described, a name that exists in a document and not in the tree.

The
same brief cited `TestFilePartialApplyReturnsChangesAlongsideError`, which did
exist — added during the spine and deleted by this very work — so that
citation was stale rather than invented.

Separately, `report-diff.sh` was
diagnosed as blind to a sabotage when the actual cause was a four-minute-old
binary; rebuilt, the same sabotage yielded `1 divergent` (fixed since: the
gates now exit 2 if any `.go` file under `cmd/` or `internal/` is newer than the
binary). Neither is a failure to read Ruby correctly. They are
failures of unverified recall and of tooling assumptions, and the lesson —
re-check a citation against the tree you are describing — is a different lesson
from that of the design-time predictions.

Their own conclusion survives the extra evidence unchanged: reading Ruby reliably
answers *which code path runs* and unreliably answers *what is in the output*.
Every one of the four post-prediction errors is of the second kind.

## Did G1 catch anything the unit tests missed, or vice versa?

This was measured rather than recalled. **Ten** single-behaviour regressions were
introduced one at a time into a throwaway worktree; after each, `task build`,
then `go test ./... -count=1` and `bash tests/spine/report-diff.sh`. Baseline
before and after: unit green, G1 `exit=0, 181 leaves compared, 0 divergent`. The
build step is not optional — G1 exits 2 against a stale binary, and a 2 is
"could not run", not a pass.

| Sabotage | Unit | G1 |
|---|---|---|
| Attribution removed (pre-work behaviour) | fail (3 tests) | `exit=1`, 4 divergent |
| `Was`/`Is` vs `WasDisplay`/`IsDisplay` collapsed, owner | **green** | **`exit=0`** |
| Same, group | fail (1) | `exit=0` |
| Counter undercounts failures | fail (4) | `exit=1`, 3 divergent |
| `applyMetadata` returns at first failure | fail (1) | `exit=0` |
| `(file: F, line: N)` suffix dropped | fail (3) | `exit=1`, 5 divergent |
| `Could not set 'V' on P: ` wrapper dropped | fail (3) | `exit=1`, 5 divergent |
| exec failure attributed to `command`, not `returns` | fail (2) | `exit=0` |
| notify's run-log line deleted | **green** | **`exit=1`**, 1 divergent |
| mode pre-sync failure re-attributed to `mode` | **green** | **`exit=0`** |

Four conclusions, in descending order of how much they should change behaviour.

**Two sabotages were caught by neither instrument, and both are in the region
this work was about.**

The first is unconditional. Making the failures of the declared-mode parse
(`puppetpath.ParseDeclaredMode`) and of the current-mode read (`retrieved`) land
on a `mode` `Change.Err` instead of returning a bare resource-level error
inverts the rule `applyMetadata`'s own comment states — *"nothing was attempted, so
nothing can be blamed"* (`internal/units/file/file.go`, inside `applyMetadata`) — and produces a green
`task check`. Reproduced directly: unit suite `exit=0`, G1 `181 leaves compared,
0 divergent`. G1 is blind because its fixture declares only valid modes; the unit
suite is blind because those two error returns are never reached by a test that
asserts on them.

The second is conditional on the privilege the run has. Collapsing
`WasDisplay`/`IsDisplay` into `Was`/`Is` in the **owner** branch — the exact
mistake the `Is`/`IsDisplay` split was introduced to prevent, and the one
`package { ensure => latest }` independently vindicates — is green on both
instruments when run unprivileged.

The assertion exists
(`TestFileOwnershipRendersNamesButKeepsRawValues/owner`) but self-skips at
`os.Getuid() != 0`, and G1's fixture declares no `owner`. Its
**group** twin is caught, by an accident of which subtest skips. So this one is a
sampling gap rather than an absent assertion — but it is a live hole in every
unprivileged run, which is how `task check` runs by default, and whoever decides
whether the fleet harness runs its gates as root is deciding whether it closes.

Worse, for the first hole a test *appears* to cover it.
`TestFileMetadataChmodFailureIsAttributedToMode` runs, is not skipped
unprivileged, states in its comment that it documents exactly this boundary, and
**passes under the sabotage** — its lone `err != nil` assertion is satisfied by
an error raised earlier in `Apply`, so it never reaches `applyMetadata` at all.
An assertion that only checks for a non-nil error cannot tell you *which* error.
This is the first vacuous test found by a retrospective rather than by a gate or
a review; it is recorded as a trap below.

**The two instruments are complements, not substitutes, and the axis is
breadth versus depth of branch.** G1 caught the deletion of the transaction's
`rep.Log` call for a notify's run-log line — unit-green, G1 `exit=1`. No unit
test then asserted that the *transaction emits* that entry;
`internal/units/notify/notify_test.go:171` asserts only that the provider
populates `Change.Notice`, one layer below the deletion.
`internal/transaction/sensitive_test.go:TestMarkedPropertyLeavesNoticesAlone`
has since come to assert the emitted entry. That is G1's distinctive value: it compares the whole emitted document
against Ruby, so it covers fields nobody thought to assert.

G1 missed five sabotages, of which the unit suite caught **three** — the
display and value collapse in its `group` form, the lost per-property independence
in `applyMetadata`, and exec's misattribution to `command`. (The other two are
the pair caught by neither, above.) All five are missed for one structural
reason: **G1's fixture was five resources wide** and contained no `exec`, no
`owner`/`group`, no multi-property failure, and no `--noop` pass. Its 181 leaves
were deep, not broad. The fixture has since grown, but it still has no `exec`
and no `--noop` pass. G1's reach is exactly its fixture's reach.

**Where both fire, G1 is the more diagnostic.** For the attribution sabotage it
named the four exact event fields that lost their value; for the counter
regression it named both the per-resource counter and the run-level roll-up
metric, which is how it was established that attribution propagates into
`metrics`, not just into events. But it does not localize: dropping the location
suffix and dropping the `Could not set` wrapper produce byte-identical G1
output, because the comparator elides the innermost failure detail. It
reports *that* the message differs, not *which half* is gone. The unit
assertions distinguish them.

**The cheapest improvement is the fixture, not more unit tests.** Four of the
five G1 misses are one fixture edit away from being caught — `owner`/`group`
closes both display and value rows, an invalid `mode` closes the pre-sync row, and a
resource with two failing properties closes the independence row — at near-zero
authoring cost, whereas each unit test costs a function.

A `--noop` pass is
worth adding on the same grounds, though no sabotage here needed it. The fifth,
`exec`, is **not**: as recorded above, G1's fixture cannot
contain one at all on this host, and closing that gap needs a second reference
implementation in a container, not a wider catalog.

**That gap is now closed, and the diagnosis above was right about where.** G5 —
the container differential — carries many `exec` resources, and re-running this
exercise's eighth sabotage against it moves the row's verdict: attributing a
non-zero status to `command` instead of `returns` now yields `exit=1`, three
divergent leaves, naming
`resource_statuses.Exec[spine-exec-fail].events.0.property` and the two `err`
logs whose source changed with it.

The G1 column stands as measured — G1's
fixture still has no `exec` and cannot — so the table records what each
instrument saw, not what is reachable today. The first run of the extended
fixture also found two defects neither the unit suite nor any gate had seen, both
in the shape of an event that describes no property; see *What the gates do and
do not prove*.

What neither instrument reaches, and where both uncaught sabotages sit, is
**error paths that precede a sync attempt** — a value that could not be parsed,
a current state that could not be read, a rendering that no catalog exercises.
Those need unit tests written deliberately, because no catalog produces them and
no differential will ever see them.

## The judgement

The work is defensible on its own terms and the evidence does not fully settle
it either way. Recorded so the next reader can weigh it rather than inherit a
conclusion:

**For.** The defect was real and user-visible; Puppet names the property and
voxagent did not. The `Is`/`IsDisplay` split was independently required by
`package`'s `ensure => latest`, so the contract change bought a second type's
correctness before that type existed — the single best argument for doing it now
rather than after `package` and `service` forced it. G1 could see the defect
class only once a failing resource gave it one, so the differential's
reach genuinely grew: `149` leaves to `181`.

**Against.** The contract needed two widenings inside one body of work, which is weak
evidence it is now stable, and the `package`/`service` reading found five more
additive gaps plus one documented case (`service`'s inline `enable` sync) where
the per-property model this document states is simply not what Puppet does. The
docs-to-production ratio is roughly 3:1 by line. And the retrospective itself
found a live, uncaught regression path and a vacuous test — meaning the
instruments that certified this work green do not cover the region the work was
about.

The transferable finding, independent of the verdict, is the last one: **a green
`task check` did not mean the attribution contract was enforced.** Both new
sub-projects that depend on the spine should budget for pre-sync error-path tests
explicitly, because nothing in the existing instrument set will ask for them.
