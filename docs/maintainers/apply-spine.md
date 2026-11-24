# Maintainer notes: the apply spine

`voxagent apply --catalog <file>` reads a Puppet catalog, refuses anything it
cannot fully implement, applies the rest, writes a `report_format 12` report,
and exits on a contract that matches measured Puppet.

## The divergence register

The design of the apply spine rules out bug-for-bug parity with Ruby. A divergence is
defensible only if it is enumerated rather than accidental, so every place
voxagent *knowingly* differs is listed here with its reason and with whatever
pins it — and where nothing pins it, the row says so, because a row claiming a
test it does not have is worse than one admitting it has none.

**A divergence not in this table is a defect.** That is what makes the table
falsifiable rather than decorative. The standing rule is to match the original
implementation completely first and optimise afterwards, so where voxagent could
have been "more informative" than Ruby it matches Ruby instead. The G1, G2 and G3
differentials closed every difference they found rather than adding rows here.

The property-attribution round closed six more the same way. Not one of them had
been *chosen* — each was found by reading the Ruby that governs it, and each now
does what Ruby does:

- **A created path's metadata is applied silently.** `type/file.rb:1119`'s
  `property_fix` calls `thing.sync` directly rather than going through
  `sync_if_needed`, so nothing records what it did, and a failure inside it
  surfaces as an `ensure` failure. Silent is not unapplied: the metadata really
  is set. voxagent used to emit `owner`/`group` events Puppet does not.
  Pinned by `internal/ral/file_test.go:TestFileCreatedMetadataIsAppliedButNotReported`
- **`owner` and `group` render as names in the message while the event keeps the
  raw ids.** `type/file/owner.rb:44` and `:48`, and `group.rb:42` and `:46`,
  override `is_to_s`/`should_to_s` to print names; `previous_value` stays the
  integer. Measured: an `owner` event carrying `previous_value` `1000` with a
  message reading `'llm'`. Pinned by
  `internal/ral/file_test.go:TestFileOwnershipRendersNamesButKeepsRawValues`
- **`ensure => absent` reports the path's actual type**, `file` or `link`, not
  the constant `"present"`. Ruby reads the `ensure` property's `retrieve`, which
  answers what the path currently *is*. The type comes from `Lstat`, so a symlink
  answers for itself rather than for its target. Pinned by
  `internal/ral/file_test.go:TestFileAbsentRemovesDanglingSymlink`
- **A failed resource is still refreshed.** `transaction.rb:288` calls
  `process_events` unconditionally — only `skip?` guards it, and
  `event_manager.rb`'s `process_callback?` tests noop and never failure. The one
  place failure does gate is `transaction.rb:269`, on the *outgoing* queue.
  Measured in a container, because the host Ruby cannot fork under Puppet:
  `exec`'s `refresh` parameter decoupled the two commands, and the apply command
  failed while the refresh command ran anyway. Pinned by
  `internal/transaction/transaction_test.go:TestFailedResourceIsStillRefreshed`
- **`exec`'s failure is attributed to `returns`.** `type/exec.rb:86` declares
  `newproperty(:returns, :array_matching => :all, :event => :executed_command)`,
  so the event name is `executed_command`, `desired_value` is the accepted list
  as strings — it is the property's should value and owes nothing to whether the
  command ran — and a success message reads `executed successfully`. Pinned by
  `internal/ral/exec_test.go:TestExecNonZeroExitFailsTheReturnsProperty` and
  `TestExecSuccessEventMatchesPuppetsExecutedCommand`
- **An unprivileged run warns instead of failing on ownership.**
  `type/file/owner.rb:35` reports the property in sync with a `warnonce` when the
  process is not root, so `owner` never syncs and no event is recorded.
  `group.rb` has no such guard and still attempts the `chown`, so the asymmetry
  is Puppet's, not a transcription error. Pinned by
  `internal/ral/file_test.go:TestFileOwnerChangeUnprivilegedWarnsAndReportsNoChange`
  and `TestFileGroupChangeUnprivilegedFails`

Tier 1 `package` closed one more after it shipped. It was registered as D-14
and the number is retired rather than reused:

- **A versioned install is verified, and fails when the result is not in
  sync.** openvox's `yum.rb:312-321` re-runs `query` after any install that
  carried a declared version and raises `Failed to update to version X, got
  version Y instead` unless the *provider's* `insync?` accepts the result,
  which `newvalue(/./)` re-raises as `Could not update: …`. The query falls
  back to `--whatprovides` (`allow_virtual` defaults to true) and raises `Could
  not find package` when both queries fail. Measured in the container with
  `kernel-core`, which is installonly: 266 installed, `ensure =>
  '6.12.0-267.el10'`, and dnf adds 267 *beside* 266. Ruby fails the resource
  on the first run and on every run after, because the re-query reads
  `266; 267` and `yum.rb:46` refuses any joined value. voxagent reported a
  successful change both times. After the fix, all three cases (upgrade,
  install from absent, re-run) compare 81 leaves with 0 divergent, and the
  failure and log texts match byte for byte. Pinned by
  `internal/ral/package_test.go:TestPackageVersionInstallLeavingTwoVersionsFails`,
  `TestPackageMultiversionWithADeclaredVersionAlwaysSyncs` and the
  `TestPackageVersionCheck*` tests. **No gate reaches the failure path**: G5's
  versioned resources take the check's success path, but its fixture has no
  installonly package, and the measurement script is under
  `.test-output/package-oracle/d14/`, which is gitignored

| # | Divergence | Reason | Pinned by |
|---|---|---|---|
| D-1 | `voxagent apply --catalog` honours `--detailed-exitcodes`; Ruby's ignores it | `Puppet::Application::Apply#run_command` dispatches `--catalog` to `apply`, which calls `apply_catalog` and discards its return value; the `exit` call exists only in the manifest path. Measured: a catalog with a failing resource exits 0 while the stored report says `failed=1` | `cmd/voxagent/main_test.go:TestApplyExitCodes` |
| D-2 | An unfinalised report is could-not-run; Ruby exits 255 | `report.rb` returns -1 when metrics are absent, and Ruby's `exit(-1)` surfaces as 255. That is an artifact of the return value, not a contract anything consumes | `internal/report/report_test.go:TestUnfinalizedReportIsCouldNotRun` |
| D-3 | Could-not-run is `2` in plain mode; Ruby exits `1` in both | Plain mode's `0` is already spoken for by every completed outcome, so `2` is the value it leaves free. Matches `voxdiff` and the fleet gates, which a wrapper reads alongside this | `cmd/voxagent/main_test.go:TestPreflightRefusesAndChangesNothing` |
| D-4 | A symlinked path is refused; Puppet replaces the symlink | `links` is outside `file`'s bounded attribute set. Following a symlink while running as root is a traversal primitive, and doing it *silently* — without implementing the attribute that governs it — is worse than refusing | `internal/ral/file_test.go:TestFileRefusesToFollowSymlink` |
| D-5 | `ensure => absent` on a directory fails; Puppet no-ops with a notice | `force` is outside the bounded set, and it is the attribute that decides whether a directory may be removed. Puppet also logs a misleading `ensure: removed` event in the same run; do not copy that | `internal/ral/file_test.go:TestFileAbsentRefusesToRemoveDirectory` |
| D-6 | `corrective_change` is always `false`, and the ` (corrective)` message suffix is never produced | Both come from `@persistence`, Puppet's `state.yml`: `resource_harness.rb:169` compares the current value against the value the *last run recorded*, not against the catalog, and `:170` appends the suffix when they differ. voxagent has no counterpart to that store, so it has nothing to derive the distinction from | Nothing pins the difference. `dev:report-diff` compares the leaf and sees `false` on both sides — but only because the fixture's `vardir` is a fresh `mktemp -d`, so Ruby has no persisted value there either. See the trap below |
| D-7 | `audited` is always `false` and `historical_value` is never carried, so `sync_if_needed`'s audit branch has no analogue | `audit` is in no provider's attribute set and not in `engineAttrs`, so a catalog declaring it is a **pre-flight refusal** rather than a run that silently drops the auditing requirement. The report fields exist only so the two implementations agree on the keys | `internal/ral/ral_test.go:TestEngineAttrsCoverRelationshipMetaparameters`, which asserts `audit` is not engine-handled |
| D-8 | A value Ruby writes as `[redacted]` is written in full, and `redacted` is always the zero value | `sensitive` is unimplemented, so `create_change_event`'s redaction branch — `resource_harness.rb:182`, which masks `previous_value`, `desired_value` and `historical_value` — is not reproduced. Measured: a catalog compiled from `content => Sensitive("…")` carries the cleartext in `parameters.content` and the flag in a sibling `sensitive_parameters` array, which `resource.rb:70` reads and `catalog.Resource` has no field for, so it never reaches a provider — voxagent therefore *cannot* redact, rather than choosing not to. The exposure is **latent, not live**: measured 2026-09-21, a `content => Sensitive("topsecret\n")` catalog applied by voxagent leaves **zero** occurrences of the cleartext in the report, because both the created and the changed content paths report a `{sha256}` digest rather than the bytes. It would become live the moment any sensitive-capable property reported a raw value, since nothing downstream knows to mask it. An earlier draft of this row claimed the secret already reaches an all-readable report; that was wrong and is corrected here | Nothing. Declaring an attribute is a promise to read it, and this one is not read |
| D-9 | Go and Ruby render the same errno differently — `chown <p>: operation not permitted` against `Operation not permitted @ apply2files - <p>` | **Unclosable.** It is each runtime's own error text, not voxagent's; matching it would mean hand-writing errno strings that then drift from the syscall producing them. `internal/gatekit/reportcmp`'s `elideFailureDetail` masks exactly that span and keeps the `change from X to Y failed: ` prefix, the `Could not set 'V' on P: ` wrapper and the `(file: F, line: N)` suffix compared — those three *are* the attribution the gate exists to check | `dev:report-diff`, whose fixture now contains a failing resource, so the elision runs on every green rather than being dead code |
| D-10 | A noop message never carries a trailing ` (noop)`, where `resource_harness.rb:243` appends one | A report-shape decision for whichever round measures noop end to end, not a bug in any single branch. **The evidence here is weaker than the rest of this table and must not be read as equal to it:** no gate measures it — G1 and G5 both compare a non-noop run and G3 compares filesystem state, not reports — so the claim rests on reading `:243` plus one `exec` noop event measured by hand. No `file` property's noop message has been measured against Ruby at all | `internal/ral/exec_test.go:TestExecNoopChangeDoesNotFabricateExitCode`, which records the measured Ruby event and asserts only the part voxagent reproduces |
| D-11 | On a created path the collapsed metadata failure is wrapped as `Could not set 'V' on ensure: …` unconditionally; Ruby wraps only some | `property.rb:490` re-raises a `Puppet::Error` untouched and `:492` wraps every other `StandardError`. A `chown` failing inside `property_fix` raises `Puppet::Error` (`provider/file/posix.rb:110` for owner, `:142` for group) and is **not** wrapped, while an unresolvable owner *name* is a bare `RuntimeError` from `type/file/owner.rb:29` and **is**. voxagent's collapse runs after the raise and cannot tell the two layers apart, so it wraps both. Reachable only when a created path's metadata fails, which the spine fixture does not do; `task check` is unaffected | `internal/ral/file_test.go:TestFileCreatedMetadataFailureCollapsesOntoEnsure` |
| D-12 | Provider selection is not implemented. voxagent is dnf and only dnf, so a catalog naming another provider is a **pre-flight refusal** where Ruby selects at runtime among the forty-three files under `provider/package/` | Selection is suitability and specificity evaluated against the host, and reproducing it means reproducing the confines, features and `defaultfor` of providers none of which exists here. Bounding it is also what keeps the retrospective's "no suitable provider" gap unforced: Puppet's failed-with-zero-events shape — `transaction.rb:132-135`, then one `Puppet.err` per type at teardown (`:146`) — is unrepresentable in a contract whose bare `err` always synthesizes a `resource_error`, and a refusal that happens before the walk means nothing ever needs it. **One half of the design is not implemented as written:** a host with no usable `dnf` is *not* refused at pre-flight. `Prefetch` resolves both commands with `exec.LookPath` (`internal/ral/package.go:254` and `:258`) and a failure there is the type-wide prefetch failure instead — one `err` log, one `resource_error` per package resource, `failed` and `skipped` together — while every other type in the catalog still applies, which is the half-converged node the value-level refusal exists to prevent | `internal/ral/package_test.go:TestPackageSupportsValue` pins the provider refusal in both directions (`dnf` accepted, `gem` and `yum` refused) and `internal/ral/preflight_test.go:TestPreflightRefusesAnUnimplementedAttributeValue` pins the mechanism. **Nothing pins the absent-`dnf` path.** No test drives `LookPath` into failure, and no gate can: every container G3 and G5 start has dnf |
| D-13 | Four `ensure` values are pre-flight refusals: `purged`, `disabled`, `held`, and every version **range** (`>=8.1`, `>8.1 <9.0`) | Four refusals for four different reasons, and the differences are the point. `purged` (`type/package.rb:107`) needs `:purgeable`, which is method-derived (`:47-51`, `:methods => [:purge]`) and which `provider/package/yum.rb:355` satisfies — so Ruby really does purge, and this is voxagent declining a value Puppet implements, for the feature-confinement machinery nothing else here needs. `disabled` (`:111`) needs `:disableable` (`:67-69`), whose `:methods => [:disable]` only `dnfmodule.rb:128` defines, so no yum-family provider has it and Ruby refuses the value too; this row is about *when* the refusal lands, not whether, and where Ruby's lands is unmeasured. `held` is not an `ensure` value at all — holding is the `mark` property (`:664`, values at `:676`) — so Ruby's catch-all `newvalue(/./)` (`:136`) would read it as a version and hand dnf `nano-held`; refusing it by name is what keeps voxagent from reproducing that. Ranges are implemented and real: `dnf.rb:14` declares `:version_ranges` and `best_version` resolves them by running `dnf list <name> --showduplicates` and taking the greatest available version inside the range (openvox `yum.rb:199-228`, called at `:283`; the gem's are `:201` and `:288` — the line numbers differ between the trees, the behaviour does not). All four are refused at pre-flight rather than at apply because `ensure` carries an open-ended version string: an apply-time refusal applies the rest of the catalog first and leaves the node half-converged | `TestPackageSupportsValue` asserts all four refused **and** each implemented value accepted, so it cannot pass against a provider that supports nothing; `internal/ral/package_test.go:TestPackageRefusesAVersionRange` pins the leading-operator test and the multiversion string (`6.12.0-1.el10; 6.12.0-2.el10`) it must *not* catch; `TestPreflightRefusesAnUnimplementedAttributeValue` pins the `Package[foo]: ensure => "purged" is not implemented` form |
| D-15 | Non-adjacent multiversion grouping: where one name appears twice in `rpm -qa` with another package between, voxagent joins every version into one `previous_value` and Ruby reports only the last adjacent group's | One of the predictions recorded when the apply spine was designed (made by reading Ruby's source before running it), still unmeasured, registered here rather than decided in code. `rpm.rb:238-242` flushes its accumulator whenever the name changes, so it groups **adjacent** lines only and a non-adjacent repeat becomes two entries in the instance list; `provider/package.rb:7-14`'s prefetch then does `packages[prov.name] = prov` once per instance, so the **last** entry wins. voxagent's `parseInstalled` (`internal/ral/dnf.go:96`) keys a `map[string]string` and groups globally. Keeping the global map because it is the more useful answer is exactly the improvement the standing directive forbids making before parity. Read further, the case may not be reachable at all: `rpm.rb:70` pipes the bulk query through `| sort`, and sorting makes every line sharing a `name ` prefix contiguous, so Ruby's adjacency requirement is satisfied by Ruby's own command line. That second reading is a stronger kind than the first: whether `| sort` is in the command line is a question about **which code path runs**, which reading source answers reliably, where the first reading is about **what ends up in the output**, which is the form this project has been wrong about repeatedly. So the likeliest state of the world is that the two implementations cannot disagree here. It stays a register row rather than being closed because nobody has produced the input and watched, and an unreachable divergence that is later made reachable -- by a query that drops the sort, say -- would otherwise become a silent defect | **Nothing pins the difference.** `internal/ral/dnf_test.go:TestParseInstalledJoinsMultipleVersions` and `TestParseInstalledSkipsRepeatedVersions` pin voxagent's own side only, and the captured `internal/ral/testdata/rpm-qa.txt` is sorted, so it cannot exhibit the input that would separate the two |
| D-16 | One `Could not check for updates` per run is reached by a different mechanism than Ruby's, while the output matches | Ruby's `latest_package_version` (openvox `yum.rb:91-101`) memoises `check_updates` into a class-level `@latest_versions` keyed `[disablerepo, enablerepo, disableexcludes]`, so the first `latest` resource to reach `insync?` pays for the call and every later one reads the cache. The warning itself (openvox `yum.rb:128`, the gem's `:129`) is plain `warning`, which does not dedupe at all — the single entry is an artifact of the memo, not of logging. voxagent resolves the updates map once in `Prefetch`, the only place that can see "some `latest` resource is already installed" before any `Apply`, and returns the stored warning from every `latest` resource with `Warning.DedupeClass` set to `Puppet::Type::Package::ProviderDnf`, which the transaction collapses on class-plus-message (`internal/transaction/transaction.go:205-212`). Both halves are needed: dedupe alone would still spawn N subprocesses, which `dev:state-diff` can see and no report differential can; eager resolution alone would log N times. The observable counts agree — one warning and one `dnf check-update` per run, zero of each when no installed resource asks (`type/package.rb:165` returns before `provider.latest` for an absent or purged package). What differs and is not observable in any report leaf is *when*: Ruby's call happens lazily at the first `latest` resource, voxagent's before any resource is applied | Ruby's side is measured — three installed `latest` resources against an unreachable repo emit exactly one warning (another of the predictions recorded when the apply spine was designed). voxagent's side is pinned by unit test rather than by a differential against that same catalog: `internal/ral/package_test.go:TestPackageResolveUpdatesRunsOnceForTheWholeRun`, `TestPackageResolveUpdatesSkipsWhenNoInstalledResourceAsks` and `TestPackageLatestOnAPurgedPackageRaisesNoCheckUpdateWarning` for the call count and its guard, `TestPackageCheckUpdateFailureWarnsAndLeavesTheResourceInSync` for `DedupeClass` and the no-change outcome, and `internal/transaction/transaction_test.go:TestWarningCarriesItsOwnSource` for the collapse. **No gate reaches it**: G5's containers have healthy repos, so the branch never fires there |

Two differences are not numbered divergences but `cross_implementation` entries
in `tests/spine/allowlist.json`, because each is dropped from the **Ruby side
only** — the same entry appearing on voxagent's side would still surface as a
divergence:

- `puppet_version`. Ruby reports its own version and Go cannot match it without
  lying.
- Ruby's `Wrapped exception:` log. `resource_harness.rb:149` logs the
  `ResourceError` itself, so `util/logging.rb`'s `format_exception` appends a
  second line carrying the original errno's message — an embedded newline in a log
  message. voxagent reproduces the *other* `Puppet`-sourced log,
  `property.rb:496`'s bare wrapper, but not the chain: Ruby exception-chain
  rendering has no Go analogue worth inventing, and fabricating the text would
  assert nothing.

## Known defects

Kept deliberately separate from the register above. A divergence is a decision;
these are differences nobody chose, and filing them as divergences would launder
bugs into design. Each is measured.

Four entries left this table rather than being fixed around: refresh chaining,
`restarted` under `--noop`, the refresh log wording and callback event, and a
failed apply carrying no event at all. All four came from one reading of
`transaction/event_manager.rb`, and the chain is now proven against Ruby by G3
rather than by that reading — its fixture carries a two-hop
`File ~> Exec ~> Exec`, and removing the propagation leaves `/opt/spine/cascaded`
absent on voxagent's side alone.

A fifth left it the same way: a failed apply reported one `resource_error` event
naming no property, where Puppet attributes the failure to the property that
failed. Closing it took a whole round rather than a patch — `Apply` had to be
able to carry per-property failures at all, which is the contract change below —
and it is now proven against Ruby by G1 rather than by a reading. The fixture
carries a file whose parent directory does not exist; that resource fails on both
sides and both wrap the underlying errno, so `events.*.property`, the event name
and the failure message are compared rather than asserted. `resource_error`
itself did not go away: it is still what a resource-level failure produces, and
it is now reached only when no property can be blamed.

A sixth was fixed the day it was recorded: a package built with an explicit
`Epoch: 0` read as `0:1.0-1` where Ruby reads `1.0-1`, because voxagent's
shorter query format inlined the epoch whenever the tag *existed*, while
`rpm.rb:214` drops an epoch *equal to* `'0'`. `rpmQueryFormat` is now rpm.rb's
`NEVRA_FORMAT` verbatim, and `parseInstalled` rebuilds the version as
`nevra_to_hash` does. Measured in the container with a locally built `Epoch: 0`
rpm and `ensure => absent`: before, `previous_value` was the one divergent leaf
of 81; after, 0. Pinned by `internal/ral/dnf_test.go:TestParseInstalledDropsAZeroEpoch`,
`TestParseInstalledMatchesNevraRegexExactly` and
`internal/ral/package_test.go:TestRpmQueryFormatIsRubysNevraFormat`. No gate
reaches it: no package in `fleet-node-el` carries the tag.

A seventh followed the same day: a **virtual** package name read as absent.
provider/package.rb's prefetch hands a provider only to resources whose name an
installed package carries; any other fills its state at its own turn in the walk
from rpm.rb's `query` — `rpm -q NAME`, then `--whatprovides` — and is `purged`
only when both find nothing. voxagent keyed on the prefetched set alone, so
`package { 'vi': ensure => present }`, which `vim-minimal` provides, reported a
`created` change on every run where Ruby reports none. `Apply` now runs the same
query for any name prefetch did not carry, under noop too. Measured in the
container (`.test-output/package-oracle/unmeasured/measure.sh`, gitignored):
`vi` with `ensure => present`, 51 leaves and 0 divergent; `vi` with an older
declared version, where both now report `2:9.1.083-23.el10` and run `downgrade`,
equal except for dnf's own download speeds inside the failure log; and
`vim-enhanced -> vim`, where the query at `vim`'s turn sees the provider the
earlier resource installed, 81 leaves and 0 divergent. Pinned by the
`internal/ral/package_test.go:TestPackageVirtualName*`,
`TestPackageAbsentNameIsQueriedThenPurged`, `TestPackagePrefetchedNameIsNotQueried`
and `TestPackageQueryFailureIsAResourceFailure` tests. The several-providers
`TypeError` that test pins is unmeasured: no name in the image has two installed
providers.

| Difference from Puppet | Where |
|---|---|
| `apply --help` exits 2 with `outcome=could-not-run`; `flag.ErrHelp` takes the parse-error path. `puppet apply --help` exits 0 | `cmd/voxagent/main.go` |
| The could-not-run code for a bad flag depends on argument order — `apply --bogus --detailed-exitcodes` exits 2, the reverse order exits 1 — because `*detailed` is read after a partial parse | `cmd/voxagent/main.go` |
| Global flags before the subcommand are rejected: `voxagent --detailed-exitcodes apply ...` is could-not-run | `cmd/voxagent/main.go` |
| Reports are written `0o644` and non-atomically. Event values embed managed file content, and Puppet writes `last_run_report.yaml` at `0640` | `cmd/voxagent/main.go` |
| No signal handling. `tx.Run` gets `context.Background()`, so SIGINT mid-run kills the process with no report and no `outcome=` line, and a hung `exec` has no upper bound | `cmd/voxagent/main.go` |

## The exit contract

| Outcome | plain | `--detailed-exitcodes` |
|---|---|---|
| completed, no changes | 0 | 0 |
| completed, changes | 0 | 2 |
| completed, failures | 0 | 4 / 6 |
| could not run | 2 | 1 |

Every result row matches measured Puppet 8.10.0. Plain `puppet apply` exits 0
even when resources fail, so its exit status carries no verdict at all —
`--detailed-exitcodes` is the verdict channel. Could-not-run takes the value
each mode leaves free.

Every run also prints `outcome=` to stderr — `applied`, `changed`, `failed`, or
`could-not-run` — because the meaning of the bare integer depends on the flag,
and exit statuses are read out of band from the invocation that produced them.

## Ordering the catalog does not state

Two kinds, both measured against Puppet 8.10.0 and both invisible in the
catalog's `edges` array, which carries containment and nothing else.

**Relationship metaparameters** arrive as resource parameters: `notify`,
`before`, `require`, `subscribe`. A single-target metaparameter compiles to a
bare string and multiple targets to an array; a chaining arrow (`->`) compiles
to an array even with one target.

**Autorelations** are declared by the type and appear nowhere in the catalog at
all. A `file` autorequires its nearest managed ancestor directory; an `exec`
autorequires `cwd` and the absolute paths its `command`, `onlyif` and `unless`
name. They are *soft* — naming an unmanaged path is no edge, where an explicit
relationship to an absent resource is could-not-run — and an explicit
relationship always wins, so the two can never form a cycle between one pair.

Without autorequire the most ordinary pattern there is fails: a directory and
the files inside it, where `File[/opt/spine/conf]` sorts before
`File[/opt/spine]` under any plain string order because `/` is `0x2F` and `]` is
`0x5D`.

**Containers are spliced out**, as `replace_containers_with_anchors` does with
whits. `Class['a'] -> Class['b']` means every resource in `a` precedes every
resource in `b`; ordering the two inert container vertices against each other
instead discards the relationship silently, because the walk still has a valid
topological order.

## Refresh propagation

A refresh **chains**. Measured against `transaction/event_manager.rb`
`process_events`: a resource that was successfully refreshed queues a
`:restarted` event *of its own*, which `queue_events` dispatches along that
resource's edges. So `File[conf] ~> Exec[mid] ~> Exec[last]` refreshes both
execs, even though `Exec[mid]` reported no change — being refreshed is itself
the event that propagates. Stopping at one hop meant a service whose config was
rewritten reloaded while anything downstream of that reload never did.

The notice and the report event share one string, so a consumer reads this
exact wording: `Triggered 'refresh' from 1 event`, pluralised on the count by
`n_()`. The event carries `name: "refresh"` and **no property** —
`add_callback_status_event` builds it from `{message:, status:, name:
callback.to_s}`, because a callback event describes the callback rather than a
property that moved.

Under `--noop` none of that happens. `process_callback?` is
`!(events.all? noop || resource.noop?)`, so a global noop run always takes the
`process_noop_events` path: it logs `Would have triggered 'refresh' from N
event(s)`, queues a `noop_restart` event to *other* resources — so the chain
still propagates — and adds nothing to this resource's own status. It returns
false, so `restarted` is never set. Reporting `restarted: true` under noop also
left the resource out of sync, which meant it survived report pruning and every
noop run over a catalog with a notify emitted a resource status Ruby does not.

A failed refresh sets `failed_to_restart` **and** `failed`, and blocks
dependents — see the note under the exit contract.

G3's fixture carries the two-hop chain deliberately. A one-hop implementation
satisfies a single-hop fixture exactly as well as a correct one.

## The provider boundary

`ral.Provider` is the only interface the transaction sees, and
`Apply(ctx, r, noop) (changes []Change, warnings []string, err error)` has three
return values carrying three different kinds of answer. Which one a provider
reaches is not a style choice; each maps to something different that Puppet's
transaction does with it.

**A `Change` is one property that moved, or one property that was attempted and
failed.** `Change.Err`, when non-nil, means the latter: `Was` and `Is` then
describe the transition that was *tried*, and the transaction builds Puppet's
`change from X to Y failed: …` wording from them. `resource_harness.rb:123`
`sync_if_needed` wraps each property's sync individually and its rescue at `:147`
records a failure event for *that* property and lets the next one proceed — so a
provider must keep going after one property fails rather than returning early.

`Change.WasDisplay` and `IsDisplay` belong to the same value: they change how
`Was` and `Is` are rendered **into the message** without changing the values the
event carries. This is Ruby's `is_to_s`/`should_to_s`, which `previous_value` and
`desired_value` do not go through. Measured: an `owner` event carries
`previous_value` as the integer `1000` while its message reads `'llm'`. Without
the split there is no way to be right about both at once — and getting it wrong
produces a report that looks correct to a human reading the message and is wrong
to anything reading the fields.

**A bare `error` return means only that no property can be blamed.** It is
Ruby's `status.failed_because` (`resource/status.rb:140`), which synthesizes a
`resource_error` event for the resource as a whole. It is a result about the resource, not about the run:
the transaction records it, skips the resource's dependents, and carries on.
Returning a non-nil error **discards the accompanying change list**, so a
provider must never rely on reporting both — an attributable failure belongs in a
`Change.Err` instead.

**`warnings` exist because a provider had no channel to the run log at all.**
Ruby's `warnonce` (`util/warnings.rb:17`) goes through `Puppet.warning`, the
global logger, so the entry belongs to no resource: it is logged at level
`warning` with source `Puppet` rather than at the resource's path. `package`
made both halves of that sentence too narrow, so `Warning` now carries its own
`Source` and its own `DedupeClass`, independently:

- **Source**, because a warning raised on a *provider instance* is not sourced
  at the resource's path either. `log_source` returns `path` only for a
  `Puppet::Type` or `Puppet::Parameter` (`util/logging.rb:298-302`), and a
  provider is neither, so it falls through to `to_s` — measured verbatim as
  `Puppet::Type::Package::ProviderDnf`.
- **Dedupe**, keyed on the class *and* the message together, which is what
  `maybe_log`'s `@stampwarnings` does (`util/warnings.rb:26-29`: the hash is
  keyed by class at `:28` and tested against the message at `:29`). An empty
  `DedupeClass` means the warning is logged every time it is
  raised, because plain `Puppet.warning` — what package providers actually call
  — does not dedupe at all.

So a condition every `file` in the catalog hits is still reported once rather
than once per resource, and two providers raising different warnings no longer
collide. A provider returns its warning every time it applies and never tries to
remember whether it already has; it is stateless and could not. Reaching Ruby's
*count* is not always the same job as reaching its dedupe: for `package`'s
check-update warning it took resolving once in `Prefetch` as well, because a log
key cannot unspawn a subprocess (D-16).

## Writing files

Content replacement is atomic: a temp file in the same directory, `fsync`,
`chown` to the target's existing owner, `chmod`, then `rename`. This is
`file_system/file_impl.rb replace_file`, and it is not a refinement — a
truncate-and-write lets a reader observe a half-written config, and a failure
part way through destroys the file rather than leaving it unchanged.

The existing uid and gid are carried across deliberately. The temp file belongs
to whoever is running, so renaming it into place without the `chown` would
silently reassign every managed file whose owner the catalog does not declare.

## `gatekit`: the gates' parsing and judging

The gates are shell; anything they need parsed or judged is done by
`bin/gatekit`, a development binary that `task build` builds beside voxagent.
It replaced the Python the repository used to carry, and it reaches
the verdicts that Python did, on the inputs that Python saw.

| Subcommand | Used by | Package |
|---|---|---|
| `compare-reports RUBY VOXAGENT ALLOWLIST` | G1, G5 | `internal/gatekit/reportcmp` |
| `run-to-run RUN1 RUN2 ALLOWLIST` | `dev:derive-run-to-run` | `internal/gatekit/reportcmp` |
| `g5-reference REPORT CATALOG INSTALL REMOVE LATEST MISSING` | G5 | `internal/gatekit/g5ref` |
| `fleet-axes`, `fleet-compliance`, `fleet-facts`, `pdb-exported`, `manifest-mutate`, `fleet-certs` | the fleet gates | `internal/gatekit/fleet`; see `docs/maintainers/fleet.md` |

**Exit contract.** Every subcommand exits 0 for the expected outcome, 1 for a
finding, and 2 when it could not run: bad arguments, an unreadable file, or
input that is not the shape it judges. A 2 is never a statement about what was
judged. Every shell gate that calls `bin/gatekit` refuses a missing or stale
one with exit 2, for the reason recorded under *A stale binary is a false
green* below -- a stale judge is the same trap from the other side.

**The judge does not share code with the judged.** `reportcmp` reads reports
as decoded JSON and must not import `internal/report`, `internal/transaction`
or `internal/ral`, directly or through another package of this module. A
struct shared with the code under test would let a bug in that struct agree
with itself. `TestJudgeImportsNothingItJudges` enforces it.

**Python's semantics, kept on purpose.** The allowlist and the comparator were
written for Python, and three of Go's nearest equivalents differ:

- allowlist patterns are CPython 3.12 `fnmatch`, whose `*` crosses `.` and
  `/` -- not `path.Match`. `pycompat.CompileFnmatch` is a port.
- regexes keep Python `re`'s meaning: `\d` is `\p{Nd}`, not ASCII, and `$`
  also matches before a final newline.
- JSON numbers compare as Python's do: integers exactly, and `1 == 1.0`. The
  comparator departs from Python in one respect, recorded below: a bool equals
  only a bool, where Python has `true == 1`.

Each port is held to a table CPython measured while it was still in the
repository: `internal/gatekit/*/testdata/*.json`. The tables are frozen. A new
case is measured with a CPython 3.12 elsewhere and recorded; the scripts
that generated the tables are not kept in the repository.

**Inputs it refuses.** Where Python crashed on an input -- a traceback, which
exited 1 and read as a finding -- `gatekit` exits 2. It also exits 2 on inputs
whose meaning differs between the two languages and that no report contains:
invalid UTF-8 in a report, `NaN`, an unpaired surrogate escape, an integer of
more than 4300 digits, a non-string metric name or log field, a metric name
repeated within a group, and two leaves that flatten to one path.

## What the gates do and do not prove

| Gate | Proves | Does not prove |
|---|---|---|
| `dev:report-diff` | voxagent's report matches Ruby's from identical catalog bytes, outside a measured allowlist | Anything about the disk. Two implementations can agree on a report while both are wrong about the filesystem |
| `dev:idempotence` | A second apply changes nothing | Anything about agreement with Ruby. A consistently wrong provider is perfectly idempotent |
| `dev:state-diff` | Both agents leave the same filesystem state, including `exec` and `owner`/`group`, and — since `package` shipped — the same installed package set, compared with `rpm -qa` | Anything outside the paths the fixture names, and anything about the report: `rpm -qa` cannot see an event name, a `desired_value`, or which property a failure was attributed to |
| `dev:derive-run-to-run` | `report-diff`'s allowlist still covers exactly what varies between two Ruby runs | Anything about voxagent. It measures Ruby against itself |
| `dev:report-diff-container` | voxagent's report matches Ruby's from identical catalog bytes for the two types the host cannot measure at all — `package` and `exec` — with both agents inside `fleet-node-el`, against a catalog the image's own Puppet compiled. The comparison reports `448 leaves compared, 0 divergent`; it was `314` before the five `exec` resources were added | Anything about the disk, exactly as G1 does not. Nothing under `--noop`: it applies a real run, so D-10 stays unmeasured. Nothing about the *text inside* a failure detail, which the comparator masks. And nothing about the *host*: the oracle inside the image is openvox, so a leaf where openvox and the gem differ — `package`'s install command line is one — is settled in openvox's favour by this gate and would diverge against the gem by design |

Each has been seen to go red on a deliberate break and back to green. A gate
that has never failed is not known to be able to. G5's `package` break was
dropping `-y` from the install argv: `18` divergent leaves naming the command
line, `Execution of '/usr/bin/dnf install tree' returned 1: …` against Ruby's
`-y install`. Its `exec` break was attributing a non-zero status to `command`
instead of `returns` — one line in `internal/ral/exec.go` — and it went red
naming three leaves:

```
resource_statuses.Exec[spine-exec-fail].events.0.property
  ruby:     'returns'
  voxagent: 'command'
logs (only in ruby)
  ('/Stage[main]/…/Exec[spine-exec-fail]/returns', "change from 'notrun' to ['0'] failed: <detail>", 'err')
logs (only in voxagent)
  ('/Stage[main]/…/Exec[spine-exec-fail]/command', "change from 'notrun' to ['0'] failed: <detail>", 'err')
```

`dev:report-diff`'s fixture now contains a **failing** resource — a file whose
parent directory does not exist — so the differential reaches the failure
attribution rather than only the success path. It reports `181 leaves compared,
0 divergent`; it was `149` before the failing resource was added. A gate that
cannot reach the code it was written for is not a gate, and until that resource
existed this one could not.

**`exec`'s report surface is compared against Ruby by G5, and by G5 alone.** G1
cannot do it: the host Ruby cannot fork under Puppet, so every `exec` there dies
with `Errno::EBADF` before running anything, and G3 compares filesystem *state*
rather than reports. G5 runs inside the image where `exec` works, so closing this
was a fixture edit rather than a new instrument. Five resources carry it, and each
one exists for a shape the others cannot reach:

| Resource | Reaches |
|---|---|
| `spine-exec-ok` (`/bin/true`, `cwd => '/tmp'`) | the success event — `executed_command`, `previous_value` `notrun`, message `executed successfully` |
| `spine-exec-fail` (`/bin/false`) | a non-zero status attributed to the **`returns` property**, not to the resource, and the `err` log at `…/Exec[spine-exec-fail]/returns` |
| `spine-exec-returns` (`test -d` on a missing path, `returns => [0, 1]`) | `desired_value` as the **declared list** — `["0", "1"]` with status `success` while the command exited 1. A one-element list cannot tell the list apart from the status |
| `spine-exec-refresh` (`refreshonly`, notified by `Notify[spine]`) | the refresh **callback** event: name `refresh`, no property, `Triggered 'refresh' from 1 event` |
| `spine-exec-guarded` (`onlyif => '/bin/false'`) | a guard that skips, which must record **no event at all** |

It went red on the first run, which is the whole reason the exercise was worth
doing, and both findings were in the event shape rather than in `exec`:

- **`property` was `""` where Ruby writes `null`.** `event.rb:58` is
  `self.property = property if property`, so an event built without one leaves
  the field nil. Go's `string` had no way to say that.
- **`redacted` was `null` where Ruby writes `false`.** `event.rb:38` defaults the
  `redacted:` keyword to `false`, and only a *property* event overrides it —
  `property.rb:260` passes `attrs[:redacted] = @sensitive`, which is nil for
  every property in the spine's bounded sets. So one Ruby report holds
  `{"property":"returns","redacted":null}` and `{"property":null,"redacted":false}`
  side by side, and voxagent had both backwards.

Neither is confined to `exec`. The same two fields are wrong on every event with
no property — the `resource_error` a prefetch failure writes, the one an apply
error writes, and the one a *failed* refresh writes — and two existing unit tests
asserted `property == ""` while their own comments said Ruby writes null.
`internal/report/report.go` now types both fields `any`, and
`internal/transaction/transaction_test.go`'s `TestRefreshReportingMatchesPuppet`,
`TestFailedRefreshFailsTheResourceAndSkipsDependents` and
`TestPrefetchFailureFailsEveryResourceOfTheTypeAndAppliesNone` pin all four
combinations, so the gate is not the only thing holding it.

One measurement worth keeping: the guarded `exec` is absent from *both* reports,
not present with an empty event list. `Puppet[:exclude_unchanged_resources]`
defaults true and `report.rb:326` drops every status that is none of `failed`,
`failed_to_restart`, `out_of_sync` or `skipped`. Its absence is therefore the
assertion — which is evidence only because the gate also checks the compiled
catalog *declares* it, so a misspelled title cannot pass as a skipped guard.
`restarted` is asserted the same indirect way, through the callback event:
`resource/status.rb` lists it in `STATES` and the transaction sets it, but
`to_data_hash` never writes it, and it reaches a report only as the
`resources.restarted` metric.

**What G5 still does not compare about `exec`:**

- **Anything under `--noop`.** G5 applies a real run, so D-10 — the missing
  ` (noop)` suffix, and voxagent's noop event omitting `desired_value` — is still
  pinned by one hand measurement and one unit test, and by no gate.
- **The text inside a failure detail.** `internal/gatekit/reportcmp`'s
  `elideFailureDetail` masks everything after `change from 'notrun' to ['0']
  failed: `, so the wording `'/bin/false' returned 1 instead of one of [0]` is not
  a compared leaf — it was measured byte-identical on 2026-09-26, and it is
  `internal/ral/exec_test.go:TestExecFailureRendersTheReturnsListBothWays` that
  holds it. The masked prefix still carries the property and the transition,
  which is the attribution the gate exists to check.
- **`path`, and the `creates` and `unless` guard shapes.** The fixture exercises
  `cwd` and `onlyif`; the rest of the bounded attribute set is unit tests only.
- **A refresh that FAILS.** `spine-exec-refresh`'s command succeeds, so
  `failed_to_restart` with its `resource_error` event is reached by
  `TestFailedRefreshFailsTheResourceAndSkipsDependents` and by no gate.

**`package` is the mirror image, and is invisible to `dev:report-diff` for a
worse reason.** The host's Ruby cannot fork, so `dnf.rb:27-34`'s load-time
confine fails, `dnf` is unsuitable, and Puppet falls back to the most specific
provider that *is* suitable — `dnfmodule`, which manages dnf **modules**. A
host-side differential would therefore run, produce a plausible report, exit 0,
and answer a different question than the one asked. `exec` on the host fails
loudly with `Errno::EBADF`; `package` succeeds against the wrong subsystem,
which is why G5 exists rather than a package resource in G1's fixture. It is the
lesson of *"The two oracles are not the same code"* below, one step further
along: there the risk is reading the wrong tree, here it is *measuring* the
wrong one.

### What the comparator got wrong, and the fix

`internal/gatekit/reportcmp` is a port of the Python comparator it replaced. The
port reproduced that comparator's verdicts exactly, including three that were
wrong, because its proof of correctness was identical verdicts on identical
inputs. Once Python was gone, all three were fixed:

| Defect, as ported | Now | Golden case |
|---|---|---|
| Log entries compared by membership: a log one side emits twice and the other once was not a divergence | Compared by count; each surplus copy is one divergence | `edge/log-count-ignored` |
| Python's `true == 1`: a `true` leaf matched `1` or `1.0` | A bool equals only a bool; numbers still compare by value, so `1` matches `1.0` | `edge/bool-equals-one` |
| Absence spelled as the string `"<absent>"`: an absent leaf matched a present leaf holding that string | Absence is compared as absence, in `compare-reports` and `run-to-run` alike | `edge/absent-equals-absent-string` |

Each golden case keeps Python's measured verdict as history beside a
`corrected` field holding the verdict the comparator must now reach, so the
departure is visible case by case. The `corrected` field is parsed strictly,
so a malformed entry fails the test instead of falling back to Python's verdict.
No corpus pair changed verdict. Ruby does emit duplicate log lines, so the count
rule is reachable. Two different rpm debug lines are duplicated in the
`unmeasured-W1-ruby` and `-W2-ruby` reports: the plain `Executing: '/usr/bin/rpm
-q nano ...'` query (twice; `-W3-ruby` carries it once) and a `--whatprovides`
query (twice; `-W3-ruby` lacks it entirely). The count rule fires only on the
plain query line, adding one divergent log line to each of the four ordered
W1/W2-versus-W3 Ruby pairs; those pairs already exited 1 before the fix. The
`--whatprovides` line is reported as before, because W3 lacks it. Voxagent emits
neither line, so no natural Ruby/voxagent pair changes at all.

G5's reference check, `internal/gatekit/g5ref`, carries a fourth inherited
quirk of the same kind. Its "multi-entry accepted list" test is Python's
`len()`, so a `desired_value` that is a two-character *string* passes as if it
were a two-entry list (`exec-returns-two-char-string` in that package's
golden). Ruby writes `returns` as a list, so no real run reaches it.

### The allowlist is measured, not asserted

Every entry in `run_to_run` is a field `report-diff` can **never** report, so the
list's provenance is part of the gate's evidence. `dev:derive-run-to-run`
re-measures it by applying one catalog twice and fails on two conditions:

- **incomplete** — a leaf varies that no pattern covers, so the differential
  will report noise above any real finding;
- **over-broad** — a pattern also suppresses a leaf that does *not* vary, so the
  gate goes green while comparing less than it appears to.

The second is the dangerous one, and it caught a real defect immediately:
`metrics.time.values.*.*` also hid the metric *names* and *labels*, because
`fnmatch`'s `*` crosses dots and Puppet stores metrics as positional
`[name, label, value]` triples. The comparator now re-keys every metric
group by name before flattening, which makes the values addressable and stops a
differing metric *set* from misaligning every index into a landslide of false
pairs. `config_retrieval` is deliberately compared rather than allowlisted: on
the `--catalog` path neither implementation retrieves a catalog, so both must
report a literal `0`.

One entry breaks that provenance rule and says so in the file:
`metrics.time.values.package.value`. `dev:derive-run-to-run` reports it
**UNUSED** rather than validating it, because the host fixture has no `package`
resource and cannot have one — Puppet on the host resolves `dnfmodule`. Its
provenance is the container instead: G5's own catalog applied twice by Ruby in
two fresh containers measured `7.21` against `14.98` seconds for that leaf. The
gate tolerates UNUSED and refuses OVER-BROAD, so what stands in for the
derivation here is that recorded measurement, and the entry becomes derivable
the day the host fixture can carry a package.

### Two reference implementations, and why it matters

| Where | Ruby | Puppet | `exec` |
|---|---|---|---|
| host | 3.4.9 | puppet gem 8.10.0 | **fails** — `Errno::EBADF` |
| `fleet-node-el` | 3.2.11 vendored | openvox, version asserted by `state-diff.sh` | works |

The host's Ruby is newer than Puppet 8.10 supports. `util.rb` walks
`/proc/self/fd` closing descriptors and closes the directory handle it is
iterating, so **every** `exec` fails on the host with no side effect. That is
why `report-diff.sh`'s fixture contains no `exec` and `state-diff.sh` does.

`state-diff.sh` asserts the image's puppet version against a pin and exits 2 if
it moved. Without that check, rebuilding the image would silently change the
oracle and every later green run would mean something different from the one
that was reviewed.

That is not hypothetical. `fleet/images/Dockerfile.el` installs `openvox-agent`
UNVERSIONED -- tracked debt its own comment names -- so a rebuild floats the
oracle, and one did: the image moved from 8.28.1 to 8.29.0 and the gate exited
**2**, refusing to compare, rather than measuring against a reference nobody had
reviewed. The pin was then bumped deliberately, after re-measuring that parity
still holds at the new version across all four compared paths.

So the pin is a **tripwire, not a guarantee**: it catches the float rather than
preventing it, and the next rebuild will trip it again. Exit 2 and a demand to
re-measure is the intended outcome. Pinning the Dockerfile would fix it at the
source and belongs to the fleet harness rather than the spine.

#### The two oracles are not the same code, and every citation here is the gem

The table above reads as two versions of one implementation. They are two
**codebases**: the host runs the `puppet` gem 8.10.0, the image runs openvox,
which is a fork on a separate release line. Every `path:line` in this document
and in the Go comments is against the gem at
`/home/llm/.rvm/gems/ruby-3.4.9/gems/puppet-8.10.0/lib/puppet/`, because that is
what is readable from the host — but the behaviour a container gate compares
against is openvox's.

Spot-checked 2026-09-22 by comparing the two trees directly:
`provider/file/posix.rb` and `type/exec.rb` are byte-identical, and
`type/file/owner.rb` differs only in a `rubocop:disable` directive
(`Naming/PredicateName` against `Naming/PredicatePrefix` at `:44`) — no
behaviour. That is a spot check of three files, not a proof about the rest.

For `package` the difference is real, and it lands in the report rather than
staying internal:

| | builds the install command as |
|---|---|
| openvox 8.29.0 | `provider/package/yum.rb:305` — `[command(:cmd)] + ["-y", install_options, operation, wanted].compact` |
| puppet gem 8.10.0 | `provider/package/yum.rb:250` — `[command(:cmd), '-d', '0', '-e', error_level, '-y', …]` |

A failed install reports `Execution of '<the command line>' returned 1: …`, so
the difference is a compared leaf, not a detail. voxagent matches **openvox**,
because that is what the fleet runs and what the container differential
compares. A differential against the host gem would diverge on that leaf by
design.

The transferable rule: a citation against the gem answers *which code path
runs* and is reliable for that. Where the question is *what is in the output* —
which is where this project has been wrong most often — read the openvox source
inside the image (`/opt/puppetlabs/puppet/lib/ruby/vendor_ruby/puppet/`) or
measure there. Do not assume the fork agrees.

### The bind-mount blind spot, and why `/etc/hosts` is not in the fixture

Podman bind-mounts `/etc/hosts`, `/etc/resolv.conf` and `/etc/hostname` into
every container, so those three are not in the image layer and changes to them
never appear in `podman diff`. A gate resting on the layer diff alone would
report no divergence for a catalog managing them while asserting nothing. That
is why `snapshot.sh` records named paths directly and is the primary
instrument rather than a supplement.

An earlier design managed `/etc/hosts` in the state-diff fixture to prove that
point. **It cannot serve as a compared path, and the reason is measured:**

- podman writes each container's *own* id and name into `/etc/hosts`, so two
  containers differ there before either agent runs. There is no identical
  starting state to compare from.
- Puppet cannot manage it inside a container at all. `replace_file` renames its
  temp file over the target, and rename cannot cross a bind mount, so Ruby fails
  with `EBUSY` and leaves the file untouched.

Proving the instrument and comparing the agents are two different jobs, and the
fixture conflated them. `state-diff.sh` now does the proof as a **self-check on
every run**: it changes a bind-mounted path, requires that the snapshot notices
and that `podman diff` does not, and exits 2 if either stops being true. A claim
about podman that nobody re-checks is one that quietly stops holding.

Do not put a bind-mounted path back into the compared set. Extend the self-check
instead.

## Traps this project keeps hitting

Each of these cost real time and none of them looked like a trap at the time.
They are recorded because the *reasoning* error is reusable even where the code
error has been fixed.

**A stale binary is a false green, and it lies about which leaves are compared.**
`report-diff.sh` and `idempotence.sh` take a prebuilt `bin/voxagent` and used to
check only that it *existed*. A deliberate sabotage of the attribution code
reported `181 leaves compared, 0 divergent` — because `bin/voxagent` predated the
edit by four minutes — and the gate was briefly and wrongly recorded as blind to
`events.*.property`. Rebuilt, the same sabotage yields `1 divergent` naming that
exact leaf. `task check` was never affected, because `dev:report-diff` and
`dev:idempotence` both declare `deps: [build]`; it bit a *direct* invocation,
which is how these scripts get run while iterating on a failure — the moment a
false green is most expensive. Both scripts now exit **2** if any `.go` file under
`cmd/` or `internal/` is newer than the binary: nothing was compared, so the
result is not a statement about the two implementations. The fix is in, and the
trap stays recorded anyway, because the lesson is the conclusion that was drawn,
not the missing check: a green gate was taken as evidence about the *comparator*
when it was evidence about a four-minute-old binary. Suspect the inputs before
the instrument.

**The failure-detail elision has a latent fragility, and it is one line away.**
`internal/gatekit/reportcmp`'s `errorLocation` is `$`-anchored, so the elision
keeps the `(file: F, line: N)` suffix compared and masks only the detail before
it. But
`resource_harness.rb:170` appends `' (corrective)'` **after** that suffix. If it
ever fired, the anchored match would miss, and the elision would widen to swallow
the location — quietly comparing less than it appears to, which is the same
failure mode `dev:derive-run-to-run` exists to catch in the allowlist. It cannot
fire today: the fixture's `vardir` is a fresh `mktemp -d`, so Ruby has no
persisted system value and `corrective_change` is `false` on both sides (D-6). It
would fire the moment the fixture kept its `vardir` between runs. A gate whose
correctness rests on a *property of the fixture* rather than on the comparison
logic is one edit away from being wrong, and the edit will look unrelated.

**`if err == nil { t.Error(…) }` does not test attribution.** A test that
asserts only that *an* error came back cannot tell you which error, which layer
raised it, or whether the code under test was reached at all.
`TestFileMetadataChmodFailureIsAttributedToMode` states in its own comment that
it documents the boundary between a resource-level error and a property
attribution, and it passes unchanged when that boundary is inverted — because an
earlier failure in `Apply` satisfies the assertion before `applyMetadata` runs.
The fixture makes the assertion true for a reason unrelated to the behaviour
under test, which is the recurring shape of every vacuous test this project has
hit. Assert the error's *identity* — the property it is attributed to, or the
message — and sabotage the code to prove the assertion can fail.

**A test whose control is a second run of the code under test cannot see a
change that affects both runs.** This is a different shape from the three above,
and it survived a sabotage. `TestSuccessfulPrefetchLeavesTheRunUnchanged` was
written the obvious way — run a catalog with prefetching, run it again without,
assert the two reports match — and it is unfalsifiable for any change that lands
on *both* sides. A deliberate mutation that logged a notice on every successful
prefetch added the same line to both reports, so the diff stayed empty and the
test passed. Rewritten to pin the report literally, against an exact log set and
exact status fields, the same mutation fails immediately.

The general form: a differential test is only as good as the independence of its
two sides. Where the baseline comes from the same binary, the test can only see
changes that are *conditional* — never ones that are unconditional. That is
exactly backwards from what a reader assumes it proves, and it is why
`dev:report-diff` compares against Ruby rather than against a second voxagent
run.

**`defaultfor` is not provider selection.** On this host `dnf` is
`default?=true` and `suitable?=false` *at the same time*, because `dnf.rb:27-34`
runs a confine at load time that shells out to `rpm --version` and the host
cannot fork under Puppet. Puppet picks the most specific **suitable** provider,
not the default one, so the host resolves `dnfmodule` (specificity 8, suitable)
over `dnf` (specificity 107, unsuitable) and manages dnf *modules*. Reading
`defaultfor 'os.family' => :redhat` (`dnf.rb:40`) and concluding "so the
provider is `dnf`" gives the wrong answer on the very machine doing the reading.
The reusable error is trusting a *declaration* where the runtime asks a
*question* — `defaultfor` declares a preference, `suitable?` answers whether it
can be honoured, and only the second is a fact about this host. This is exactly
why the retrospective was right to list provider resolution as unmeasured rather
than infer it from the type's source: the inference was available, cheap, and
wrong.

**A gate fixture's starting state is part of the gate, and has to be measured
before the gate is trusted.** Three fixtures here have assumed a starting state
they did not have, and each would have proved nothing: a file already owned by
`root` with `owner => root` declared; `package { 'zsh': ensure => absent }`
against an image where `zsh` was never installed; and `ensure => latest` on
`which`, already the newest build available. The last two were written into a
task brief and caught only by measuring the image before trusting them. All
three are green, fast, and never enter the path they were written for. The same
shape reaches further than fixtures: with no network in the containers **both**
agents fail `dnf install` identically, the two installed sets match to the byte,
and a comparison-only state gate exits 0 — measured, with the assertion
disabled. A differential cannot see a failure its two halves share, whether the
shared cause is one binary (the trap above) or one broken environment. So both
package gates check their fixture against a pristine container before applying
anything and exit **2** — could-not-run, not a pass — when it no longer fits:
`$PKG_INSTALL` absent, `$PKG_REMOVE` installed, `$PKG_LATEST` installed with
`dnf check-update` exiting 100 rather than 0, `$PKG_MISSING` still unresolvable.
`$PKG_LATEST`'s state is the one G5 makes rather than finds: the image installs
the newest of everything, and a rebuild on 2026-10-02 left no package in it
with an update available, so G5 downgrades `$PKG_LATEST` to `$PKG_LATEST_FROM`
in every container before using it, and a downgrade the repositories can no
longer serve is could-not-run.
Each then asserts an absolute outcome on the **reference side alone**, not a
comparison: `state-diff.sh` requires `$PKG_INSTALL` present and `$PKG_REMOVE`
gone from Ruby's `rpm -qa`, and `report-diff-container.sh` requires Ruby's own
report to show all three changed with events and the fourth failed. Asserting it
on Ruby alone is deliberate — the reference getting it wrong means nothing ran
(exit 2), where voxagent getting it wrong is a real divergence and belongs to
the comparator (exit 1). A gate that only compares is asking "do they agree",
and an environment where nothing happened answers yes.

## What the spine does not implement

`file` is bounded to `ensure`, `content`, `mode`, `owner`, `group`. `exec` is
bounded to `command`, `path`, `cwd`, `returns`, `refreshonly`, `creates`,
`onlyif`, `unless`. `notify` is bounded to `message`. `package` is bounded to
`ensure`, `name` and `provider` — three of the twenty-three attributes the type
declares, with `provider` recognised **only** so that a value other than `dnf`
can be refused. Everything else — and every resource type other than `notify`,
`file`, `exec` and `package` — is a pre-flight refusal, not a silent omission,
and the refusal covers the **whole catalog**: a partially applied node produces
state-diff divergences that are artifacts of coverage rather than defects.

`package` also bounds an attribute's **values**, which no other type here does.
`ensure` is `present`, its alias `installed`, `absent`, `latest`, or a concrete
version string; `purged`, `disabled`, `held` and every version range are
refusals (D-13). This is why `Gap` carries a `Value` and pre-flight grew a third
message form, `Package[foo]: ensure => "purged" is not implemented`: `file`'s
`ensure` has four values and catching a fifth at apply time is nearly harmless,
where `package`'s carries an open-ended version string and an apply-time refusal
would apply the rest of the catalog before noticing. `file`'s apply-time
`default:` branch is deliberately unchanged — changing a shipped provider with
no consumer asking for it is a behaviour change nobody requested.

Declaring an attribute is a promise to read it. `withpath` was removed from
`notify` for exactly this reason rather than implemented: pre-flight accepted a
catalog setting it, the run reported success, and the attribute had no effect.

The refusal list doubles as the porting roadmap: run `voxagent apply` against a
fleet's catalogs and the gaps it prints, ranked by frequency, are the measured
answer to "what should Tier 1 cover next".

## Refusing what Puppet refuses

`ral.Validate` runs after pre-flight and refuses what Puppet refuses while
converting the catalog, so the whole run fails before anything is applied. It
is a separate pass because its findings are not porting gaps: counting a
relative path as an unimplemented attribute would put invalid catalogs into the
roadmap that pre-flight's gap list feeds. A provider opts in by implementing
`ral.Validating`, and returns the first failure for a resource in Puppet's own
order and words. Two do today:

- `file` refuses a path that is not absolute (Puppet: `Parameter path failed on
  File[x]: File paths must be fully qualified, not 'x'`). This message was
  measured against Puppet 8.10.0. Before the check voxagent applied a relative
  path against its working directory.
- `exec` refuses a relative or array `cwd`, a relative `creates` (scalar or any
  array element), and a command whose executable does not start with `/` when
  no `path` is set. These messages were **read from source, not measured**:
  `cwd` and `creates` are `Parameter::Path` (`type/exec.rb:266`, `:420`), whose
  `validate_path` (`parameter/path.rb`) failures `type.rb:692` wraps as
  `Parameter cwd failed on Exec[x]: cwd must be a fully qualified path`; the
  command goes through `provider/exec.rb` `validatecmd` from the type-level
  `validate` (`type/exec.rb:589`), which `type.rb:2360` wraps as `Validation
  of Exec[x] failed: 'touch' is not qualified and no path was specified.
  Please qualify the command or specify a path.` The executable is
  `extractexe`'s: a leading single- or double-quoted string if one opens any
  line, else everything before the first space. The order is `cwd`, `creates`,
  then the command, because Puppet sets parameters in declaration order and
  validates the command after all of them. Before the check a relative `cwd`
  ran the command via `/bin/sh` in voxagent's own working directory.
  `onlyif` and `unless` also go through `validatecmd` in Puppet
  (`type/exec.rb:497`, `:560`); voxagent does not validate them, so an
  unqualified guard with no `path` runs where Puppet would refuse.

## What per-property attribution cost, and what it proved

Closing the spine's first known defect — a failed resource reported one
`resource_error` naming no property, where Puppet names the property that
failed — was run as an experiment in two parts: whether it could be done, and
whether it was worth doing. The implementation work answered the first. This section answers
the second, and it is written as evidence rather than as a recommendation. Every
number below carries the command that produced it; re-run them rather than
trusting them.

### What it cost

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
Re-measure the range; do not do arithmetic over commit diffs.

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

So the honest cost of the contract change in existing tests is 8 rewritten
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

### Did the `Change.Err` contract hold, and would it survive `package` and `service`?

Across `file`, `exec`, and `notify`, yes — no provider needed a shape it could
not express. But the contract was widened **twice within the same body of
work**, not once: `Change.Err` on 2026-09-18, then a third `Apply` return for
run-level warnings on 2026-09-19, eleven commits later,
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
source and not implemented — the interface **survives with additive changes**,
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
| **`flush` failing after changes** | `resource_harness.rb:25-28` runs `flush` after all syncs, but only `if status.changed? && !resource.noop?`; a raise is caught by the rescue at `:29` and the status then carries both the recorded change events **and** a `resource_error` | the transaction drops `changes` entirely when the error return is non-nil, exactly as the contract promises |
| **Warning dedupe key** | `maybe_log`'s dedupe is keyed on calling class *and* message (`util/warnings.rb:28-29`), and is shared by `warnonce`, `notice_once`, and `debug_once` (`:8`, `:14`, `:18`). Plain `Puppet.warning` — what package providers actually call, e.g. `provider/package/gem.rb:152` — does not dedupe at all | the transaction dedupes every warning on the message alone (`transaction.go`'s `warned` map), which is broader than Ruby on both counts |
| **Warning source** | a warning raised on a provider instance is *not* sourced at the resource's path: `log_source` returns `path` only for a `Puppet::Type` or `Puppet::Parameter` (`util/logging.rb:298-302`), and a provider is neither, so it falls through to `to_s` — `"#{@resource}(provider=#{self.class.name})"` (`provider.rb:559`), e.g. `Package[foo](provider=gem)` | the transaction hardcodes source `Puppet` for every warning |
| **No prefetch hook** | `prefetch_if_necessary` (`transaction.rb:243-260`) delegates at `:259` to `Transaction#prefetch` (`:374-389`), which calls `provider_class.prefetch(resources)` at `:381` — for packages, `provider/package.rb:7-14`, enumerating every installed package once per (type, provider class). A failure is recorded at `:387` and fails every resource of that type | `Provider` has no per-run bulk-read hook and no way to fail a whole type. Also a performance point: without it, N packages means N `rpm -q` invocations |

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
command and cannot raise, so `package` does not reach the shape. `service` still
will.

One finding landed on this repository rather than on Puppet, and has been fixed.
`internal/ral/ral.go` commented that `Change.Name` need only be set for `ensure`
"because only ensure declares per-value events" — true of `file`, `exec`, and
`notify`, and false of Puppet. `type/service.rb:68-92` carries five per-value
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
`Property#sync` directly, bypassing `sync_if_needed`. Because `event = super()`
is computed at `:127` *before* the `enable` sync at `:132`, a failure to enable
is reported as **`ensure`'s** failure even though the service started. The
contract can represent that — one `Change` for `ensure` carrying `enable`'s
error — but only if the implementer knows to, and nothing in the interface hints
at it.

### How many of the must-measure predictions were wrong?

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

The pathspec is load-bearing. Without it the same command returns the commit
that added a planning document, because that document embeds the proposed test's source — which is precisely the
error being described, a name that exists in a document and not in the tree. The
same brief cited `TestFilePartialApplyReturnsChangesAlongsideError`, which did
exist — added during the spine and deleted by this very work — so that
citation was stale rather than invented. Separately, `report-diff.sh` was
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

### Did G1 catch anything the unit tests missed, or vice versa?

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

The first is unconditional. Making `parseMode` and `currentMode` failures land
on a `mode` `Change.Err` instead of returning a bare resource-level error
inverts the rule `applyMetadata`'s own comment states — *"Nothing moved, so
nothing can be blamed"* (`internal/ral/file.go:500`) — and produces a green
`task check`. Reproduced directly: unit suite `exit=0`, G1 `181 leaves compared,
0 divergent`. G1 is blind because its fixture declares no invalid mode; the unit
suite is blind because those two error returns are never reached by a test that
asserts on them.

The second is conditional on the privilege the run has. Collapsing
`WasDisplay`/`IsDisplay` into `Was`/`Is` in the **owner** branch — the exact
mistake the `Is`/`IsDisplay` split was introduced to prevent, and the one
`package { ensure => latest }` independently vindicates — is green on both
instruments when run unprivileged. The assertion exists
(`TestFileOwnershipRendersNamesButKeepsRawValues/owner`) but self-skips at
`os.Getuid() != 0`, and G1's fixture declares no `owner` or `group`. Its
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
test asserts that the *transaction emits* that entry; `notify_test.go:168`
asserts only that the provider populates `Change.Notice`, one layer below the
deletion. That is G1's distinctive value: it compares the whole emitted document
against Ruby, so it covers fields nobody thought to assert.

G1 missed five sabotages, of which the unit suite caught **three** — the
display/value collapse in its `group` form, the lost per-property independence
in `applyMetadata`, and exec's misattribution to `command`. (The other two are
the pair caught by neither, above.) All five are missed for one structural
reason: **G1's fixture is five resources wide** and contains no `exec`, no
`owner`/`group`, no multi-property failure, and no `--noop` pass. Its 181 leaves
are deep, not broad. G1's reach is exactly its fixture's reach.

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
closes both display/value rows, an invalid `mode` closes the pre-sync row, and a
resource with two failing properties closes the independence row — at near-zero
authoring cost, whereas each unit test costs a function. A `--noop` pass is
worth adding on the same grounds, though no sabotage here needed it. The fifth,
`exec`, is **not**: as recorded above, G1's fixture cannot
contain one at all on this host, and closing that gap needs a second reference
implementation in a container, not a wider catalog.

**That gap is now closed, and the diagnosis above was right about where.** G5 —
the container differential — carries five `exec` resources, and re-running this
exercise's eighth sabotage against it moves the row's verdict: attributing a
non-zero status to `command` instead of `returns` now yields `exit=1`, three
divergent leaves, naming
`resource_statuses.Exec[spine-exec-fail].events.0.property` and the two `err`
logs whose source changed with it. The G1 column stands as measured — G1's
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

### The judgement

The work is defensible on its own terms and the evidence does not fully settle
it either way. Recorded so the next reader can weigh it rather than inherit a
conclusion:

**For.** The defect was real and user-visible; Puppet names the property and
voxagent did not. The `Is`/`IsDisplay` split was independently required by
`package`'s `ensure => latest`, so the contract change bought a second type's
correctness before that type existed — the single best argument for doing it now
rather than after `package` and `service` forced it. G1 could not see the defect
class at all until a failing resource gave it one, so the differential's
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
