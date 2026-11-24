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
Puppet behaviour that is matched but should eventually change goes in
[the improvements log](improvements.md), not in this table.

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
  answers what the path currently *is*. Under `links => manage`, the default,
  the type comes from `Lstat`, so a symlink answers for itself rather than for
  its target; under `follow` it comes from `Stat` (`type/file.rb:895-903`).
  Pinned by `internal/ral/file_test.go:TestFileAbsentRemovesDanglingSymlink`
  and `TestFileFollowAbsentReportsTheTargetsType`
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
| D-4 | Retired. A symlinked path used to be refused; it now reproduces Puppet's `links` | The file-attribute work added `links => manage \| follow` (`type/file.rb:272-281, 895-903`). Under `manage` a link is replaced by `ensure => file` or `directory` through `removeExisting`, satisfies `present`, keeps its mode (`type/file/mode.rb:133`) and is `lchown`ed (`provider/file/posix.rb:99-144`). Under `follow` the target decides type and mode, `chmod` and `chown` go through the link, and a content write replaces the link by renaming a temp file over it (I-5). Nothing else goes through a link. The title is still cleaned first (`type/file.rb:66-73`), so `File['/d/link/']` acts on the link exactly as `File['/d/link']` does | `internal/ral/file_test.go:TestFileManageReplacesALinkWithAFile`, `TestFileEnsureFileWithoutContentReplacesALink`, `TestFileDirectoryReplacesALink`, `TestFileManageLeavesALinksModeAlone`, `TestFilePresentIsSatisfiedByALink`, `TestFileFollowContentReplacesTheLinkNotItsTarget`, `TestFileFollowModeChangesTheTarget`, `TestFileNonCleanTitleOverSymlinkActsAsTheCleanTitle`; `internal/ral/file_fs_test.go:TestFileOwnershipOnALinkLchownsUnderManageAndChownsUnderFollow`; `cmd/voxagent/main_test.go:TestTrailingSlashTitleOverASymlinkReplacesTheLink`; G1's `lnf`, `lnfollow`, `lnmode`, `lnfmode` and `lnd` fixtures |
| D-5 | Retired. An explicit `ensure => absent` on a directory used to fail; it now reproduces Puppet | `force` was outside the bounded set and decides whether a directory may be removed. The file-attribute work added `force` and `removeExisting` (`type/file.rb:1022-1049`): with `force` the tree is removed (`RemoveAll`, which never follows a link); without it Puppet's notice is logged once and a successful `ensure: removed` event is reported while the directory stays, every run (I-1). A purged child behaves the same and inherits the parent's `force` (`type/file.rb:595-600`). A purge over an unmanaged directory is therefore not idempotent, under either agent | `internal/ral/file_test.go:TestFileAbsentWithoutForceNoticesAndReportsRemoved`, `TestFileAbsentForceRemovesTheTree`; `internal/ral/file_recurse_test.go:TestFilePurgedDirectoryReportsRemovedAndStays`; G1's `rmdir`, `keepdir` and `pur` fixtures |
| D-6 | `corrective_change` is always `false`, and the ` (corrective)` message suffix is never produced; and voxagent never writes `state.yaml` | Both come from `@persistence`, Puppet's `state.yml`: `resource_harness.rb:169` compares the current value against the value the *last run recorded*, not against the catalog, and `:170` appends the suffix when they differ. voxagent has no counterpart to that store, so it has nothing to derive the distinction from. Nor does it write Puppet's other run state under `--vardir`. Natively it has no store at all. The extension host's harness records each Tier 3 resource's `checked` and `synced` times in `Puppet::Util::Storage` (`resource_harness.rb:66-71`), but only in memory: `Catalog#apply` is what loads and stores `state.yaml` (`resource/catalog.rb:234`, `:248`), and `Transaction#evaluate` is what saves the transaction store (`transaction.rb:155`), and the host calls neither (read, not measured). A Ruby run later on the same vardir therefore finds what the last *Ruby* run stored, not this run's times | Nothing pins the difference. `dev:report-diff` compares the leaf and sees `false` on both sides — but only because the fixture's `vardir` is a fresh `mktemp -d`, so Ruby has no persisted value there either. See the trap below |
| D-7 | `audited` is always `false` and `historical_value` is never carried, so `sync_if_needed`'s audit branch has no analogue | `audit` is in no provider's attribute set and not in `engineAttrs`, so a catalog declaring it is a **pre-flight refusal** rather than a run that silently drops the auditing requirement. The report fields exist only so the two implementations agree on the keys | `internal/ral/ral_test.go:TestEngineAttrsCoverRelationshipMetaparameters`, which asserts `audit` is not engine-handled |
| D-8 | Retired. A `Sensitive` value used to be written in full where Ruby writes `[redacted]`, with `redacted` always the zero value. It is now redacted as Ruby does, outside the text D-78 redacts beyond Ruby | Measured 2026-11-17 on openvox 8.33.0 (puppet 8.10.0 source for the rules). A top-level `Sensitive` reaches the catalog as the plain value plus a sibling `sensitive_parameters` array, which `catalog.Resource` now reads. `ral.MarkSensitive` applies `set_sensitive_parameters` (`type.rb:2389`): only a *property* is marked; `File` `content` also marks `ensure`, and `source` marks `content` and `ensure`, while a link `target` marks only itself; any of `Exec`'s `command`, `unless` and `onlyif` marks the three present ones. A marked property's event has `previous_value` and `desired_value` `[redacted]`, `historical_value` null, `redacted: true` and the message and log `changed [redacted] to [redacted]` (`resource_harness.rb:182`, `property.rb:260`), under `--noop` `current_value [redacted], should be [redacted] (noop)`, and on failure `change from [redacted] to [redacted] failed: <error>`. A name that cannot be marked (a parameter such as `cwd`, an unknown or unassigned one) is warned on the console only, `Warning: /Exec[x]: Unable to mark 'cwd' as sensitive: …`, printed by `applyCatalog` (`voxagent: ` prefixed under `apply`) and never put in the report. A host override's `sensitive` list is kept and concatenated with the catalog's (`Resource.MarkSensitive`), so `concat`'s generated `File` is redacted like Ruby's. Children a native `File` recursion generates carry no marks, as in Ruby. Notify's and Exec's log text is `Sensitive [value redacted]`, `[command redacted]`, `[output redacted]`. voxagent shows no diffs, so `show_diff`'s `[diff redacted]` has nothing to redact. The two gates that held this back are now compared in full: `concat` with a `Sensitive` fragment is in `report-diff-extension.sh`, and `File[/etc/chrony.keys]`'s six registered leaves are gone from `tests/spine/allowlist.json` (the SIMP gate compares them). Before the allowlist entries were removed the SIMP gate was red on exactly those six leaves. Ruby leaks a marked value in error text; voxagent does not, D-78 | `internal/ral/sensitive_test.go:TestMarkSensitive`; `internal/transaction/sensitive_test.go:TestMarkedPropertyEventIsRedacted`, `TestMarkedPropertyNoopEventIsRedacted`, `TestMarkedPropertyFailureIsRedacted`; `internal/transaction/evalgen_test.go:TestOverrideSensitiveMarksTheTarget`; `cmd/voxagent/generation_test.go:TestMergeGenerationKeepsSensitive`; `internal/ral/file_recurse_test.go:TestFileGenerateChildrenCarryNoSensitiveMarks`; `internal/ral/exec_test.go:TestExecOnlyUnlessMarkedRedactsCommand`; `cmd/voxagent/main_test.go:TestApplyWarnsOnAnUnmarkableSensitiveParameter`, `TestApplyDoesNotWarnOnAMarkableSensitiveProperty`; `cmd/voxagent/agent_test.go:TestAgentWarnsOnAnUnmarkableSensitiveParameter`; `cmd/voxagent/sensitive_test.go:TestNoSensitiveMarkerLeaks` (no marker value appears in the report, the logs or the console of a run). Gates: `dev:report-diff-container` (`Sensitive` and `Exec` cases), `dev:report-diff-extension` (a `Sensitive` `concat` fragment), `dev:report-diff-simp` (no D-8 allowlist entry) |
| D-9 | Go and Ruby render the same errno differently — `chown <p>: operation not permitted` against `Operation not permitted @ apply2files - <p>` | **Unclosable.** It is each runtime's own error text, not voxagent's; matching it would mean hand-writing errno strings that then drift from the syscall producing them. `internal/gatekit/reportcmp`'s `elideFailureDetail` masks exactly that span and keeps the `change from X to Y failed: ` prefix, the `Could not set 'V' on P: ` wrapper and the `(file: F, line: N)` suffix compared — those three *are* the attribution the gate exists to check | `dev:report-diff`, whose fixture now contains a failing resource, so the elision runs on every green rather than being dead code |
| D-10 | A noop message never carries a trailing ` (noop)`, where `resource_harness.rb:243` appends one — for `file`, `exec` and `service` alike | A report-shape decision for whichever round measures noop end to end, not a bug in any single branch. **The evidence here is weaker than the rest of this table and must not be read as equal to it:** no gate measures it — G1 and G5 both compare a non-noop run and G3 compares filesystem state, not reports — so the claim rests on reading `:243` plus one `exec` noop event measured by hand. No `file` property's noop message has been measured against Ruby at all | `internal/ral/exec_test.go:TestExecNoopChangeDoesNotFabricateExitCode`, which records the measured Ruby event and asserts only the part voxagent reproduces |
| D-11 | On a created path the collapsed metadata failure is wrapped as `Could not set 'V' on ensure: …` unconditionally; Ruby wraps only some | `property.rb:490` re-raises a `Puppet::Error` untouched and `:492` wraps every other `StandardError`. A `chown` failing inside `property_fix` raises `Puppet::Error` (`provider/file/posix.rb:110` for owner, `:142` for group) and is **not** wrapped, while an unresolvable owner *name* is a bare `RuntimeError` from `type/file/owner.rb:29` and **is**. voxagent's collapse runs after the raise and cannot tell the two layers apart, so it wraps both. Reachable only when a created path's metadata fails, which the spine fixture does not do; `task check` is unaffected | `internal/ral/file_test.go:TestFileCreatedMetadataFailureCollapsesOntoEnsure` |
| D-12 | Provider selection is not implemented. voxagent is dnf and only dnf, so a catalog naming another provider is a **pre-flight refusal** where Ruby selects at runtime among the forty-three files under `provider/package/` | Selection is suitability and specificity evaluated against the host, and reproducing it means reproducing the confines, features and `defaultfor` of providers none of which exists here. Bounding it is also what keeps the retrospective's "no suitable provider" gap unforced: Puppet's failed-with-zero-events shape — `transaction.rb:132-135`, then one `Puppet.err` per type at teardown (`:146`) — is unrepresentable in a contract whose bare `err` always synthesizes a `resource_error`, and a refusal that happens before the walk means nothing ever needs it. **One half of the design is not implemented as written:** a host with no usable `dnf` is *not* refused at pre-flight. `Prefetch` resolves both commands with `exec.LookPath` (`internal/ral/package.go:254` and `:258`) and a failure there is the type-wide prefetch failure instead — one `err` log, one `resource_error` per package resource, `failed` and `skipped` together — while every other type in the catalog still applies, which is the half-converged node the value-level refusal exists to prevent | `internal/ral/package_test.go:TestPackageSupportsValue` pins the provider refusal in both directions (`dnf` accepted, `gem` and `yum` refused) and `internal/ral/preflight_test.go:TestPreflightRefusesAnUnimplementedAttributeValue` pins the mechanism. **Nothing pins the absent-`dnf` path.** No test drives `LookPath` into failure, and no gate can: every container G3 and G5 start has dnf |
| D-13 | Four `ensure` values are pre-flight refusals: `purged`, `disabled`, `held`, and every version **range** (`>=8.1`, `>8.1 <9.0`) | Four refusals for four different reasons, and the differences are the point. `purged` (`type/package.rb:107`) needs `:purgeable`, which is method-derived (`:47-51`, `:methods => [:purge]`) and which `provider/package/yum.rb:355` satisfies — so Ruby really does purge, and this is voxagent declining a value Puppet implements, for the feature-confinement machinery nothing else here needs. `disabled` (`:111`) needs `:disableable` (`:67-69`), whose `:methods => [:disable]` only `dnfmodule.rb:128` defines, so no yum-family provider has it and Ruby refuses the value too; this row is about *when* the refusal lands, not whether, and where Ruby's lands is unmeasured. `held` is not an `ensure` value at all — holding is the `mark` property (`:664`, values at `:676`) — so Ruby's catch-all `newvalue(/./)` (`:136`) would read it as a version and hand dnf `nano-held`; refusing it by name is what keeps voxagent from reproducing that. Ranges are implemented and real: `dnf.rb:14` declares `:version_ranges` and `best_version` resolves them by running `dnf list <name> --showduplicates` and taking the greatest available version inside the range (openvox `yum.rb:199-228`, called at `:283`; the gem's are `:201` and `:288` — the line numbers differ between the trees, the behaviour does not). All four are refused at pre-flight rather than at apply because `ensure` carries an open-ended version string: an apply-time refusal applies the rest of the catalog first and leaves the node half-converged | `TestPackageSupportsValue` asserts all four refused **and** each implemented value accepted, so it cannot pass against a provider that supports nothing; `internal/ral/package_test.go:TestPackageRefusesAVersionRange` pins the leading-operator test and the multiversion string (`6.12.0-1.el10; 6.12.0-2.el10`) it must *not* catch; `TestPreflightRefusesAnUnimplementedAttributeValue` pins the `Package[foo]: ensure => "purged" is not implemented` form |
| D-15 | Non-adjacent multiversion grouping: where one name appears twice in `rpm -qa` with another package between, voxagent joins every version into one `previous_value` and Ruby reports only the last adjacent group's | One of the predictions recorded when the apply spine was designed (made by reading Ruby's source before running it), still unmeasured, registered here rather than decided in code. `rpm.rb:238-242` flushes its accumulator whenever the name changes, so it groups **adjacent** lines only and a non-adjacent repeat becomes two entries in the instance list; `provider/package.rb:7-14`'s prefetch then does `packages[prov.name] = prov` once per instance, so the **last** entry wins. voxagent's `parseInstalled` (`internal/ral/dnf.go:96`) keys a `map[string]string` and groups globally. Keeping the global map because it is the more useful answer is exactly the improvement the standing directive forbids making before parity. Read further, the case may not be reachable at all: `rpm.rb:70` pipes the bulk query through `| sort`, and sorting makes every line sharing a `name ` prefix contiguous, so Ruby's adjacency requirement is satisfied by Ruby's own command line. That second reading is a stronger kind than the first: whether `| sort` is in the command line is a question about **which code path runs**, which reading source answers reliably, where the first reading is about **what ends up in the output**, which is the form this project has been wrong about repeatedly. So the likeliest state of the world is that the two implementations cannot disagree here. It stays a register row rather than being closed because nobody has produced the input and watched, and an unreachable divergence that is later made reachable -- by a query that drops the sort, say -- would otherwise become a silent defect | **Nothing pins the difference.** `internal/ral/dnf_test.go:TestParseInstalledJoinsMultipleVersions` and `TestParseInstalledSkipsRepeatedVersions` pin voxagent's own side only, and the captured `internal/ral/testdata/rpm-qa.txt` is sorted, so it cannot exhibit the input that would separate the two |
| D-16 | One `Could not check for updates` per run is reached by a different mechanism than Ruby's, while the output matches | Ruby's `latest_package_version` (openvox `yum.rb:91-101`) memoises `check_updates` into a class-level `@latest_versions` keyed `[disablerepo, enablerepo, disableexcludes]`, so the first `latest` resource to reach `insync?` pays for the call and every later one reads the cache. The warning itself (openvox `yum.rb:128`, the gem's `:129`) is plain `warning`, which does not dedupe at all — the single entry is an artifact of the memo, not of logging. voxagent resolves the updates map once in `Prefetch`, the only place that can see "some `latest` resource is already installed" before any `Apply`, and returns the stored warning from every `latest` resource with `Warning.DedupeClass` set to `Puppet::Type::Package::ProviderDnf`, which the transaction collapses on class-plus-message (`internal/transaction/transaction.go:205-212`). Both halves are needed: dedupe alone would still spawn N subprocesses, which `dev:state-diff` can see and no report differential can; eager resolution alone would log N times. The observable counts agree — one warning and one `dnf check-update` per run, zero of each when no installed resource asks (`type/package.rb:165` returns before `provider.latest` for an absent or purged package). What differs is *when*: Ruby's call happens lazily at the first `latest` resource, voxagent's before any resource is applied. That is not observable in any compared report leaf; it shows in `evaluation_time`, which no gate compares (D-57) | Ruby's side is measured — three installed `latest` resources against an unreachable repo emit exactly one warning (another of the predictions recorded when the apply spine was designed). voxagent's side is pinned by unit test rather than by a differential against that same catalog: `internal/ral/package_test.go:TestPackageResolveUpdatesRunsOnceForTheWholeRun`, `TestPackageResolveUpdatesSkipsWhenNoInstalledResourceAsks` and `TestPackageLatestOnAPurgedPackageRaisesNoCheckUpdateWarning` for the call count and its guard, `TestPackageCheckUpdateFailureWarnsAndLeavesTheResourceInSync` for `DedupeClass` and the no-change outcome, and `internal/transaction/transaction_test.go:TestWarningCarriesItsOwnSource` for the collapse. **No gate reaches it**: G5's containers have healthy repos, so the branch never fires there |
| D-17 | A catalog_format 2 resource with no `kind` is applied by type; Puppet makes it an inert Component | `resource.rb:515-526` gives Component to anything not `compilable_type`, including a missing kind. Puppet 8 emits kind on every compiled resource, so only hand-written catalogs (test fixtures) omit it, and treating those as Components would make every fixture a no-op | `internal/ral/ral_test.go:TestIsContainerResource` |
| D-18 | `service` runs journalctl as argv, `env LANG=C LC_ALL=C journalctl -n 50 --since '5 minutes ago' -u <name> --no-pager`; Puppet builds an unescaped shell string | `systemd.rb:227` interpolates the unit name into a string that `execute` hands to a shell, so a name with shell metacharacters reaches it. voxagent never gives a unit name to a shell: every built-in command is argv with `--` before the name. The output is the same for any ordinary name | `internal/ral/service_test.go:TestServiceStartFailureCarriesTheJournal` pins the failure message and the journalctl argv shape; nothing pins a hostile name against Ruby |
| D-19 | `service`'s `start`/`stop`/`restart`/`status` keep the caller's locale and journalctl gets `C`, as Puppet does; but `is-enabled`, `show`, `enable`, `disable` and `unmask` also keep the caller's locale where Puppet forces `C` | The state tokens voxagent parses out of `systemctl` (`enabled`, `disabled`, `static`, `indirect`, `masked`, `yes`) are not localised, so the difference cannot change a decision. It could change the text of a failure from those commands, which is the part not measured. journalctl's text is copied into the failure an operator reads, which is why it alone is pinned to `C` | `internal/ral/service.go`'s type comment states the split. **Nothing pins the difference against Ruby** |
| D-20 | `service`'s `is-enabled` and `show` are read from combined stdout+stderr; Puppet reads stdout only | `runner().run` returns one combined stream. It matters only if systemctl writes a token to stderr: `static`, `indirect` and `masked` are the ones whose handling depends on the exact word, so stderr noise around them is the case that could mis-parse. Not observed. `show`'s output is scanned for its `NeedDaemonReload=` line rather than split on the last `=` as `systemd.rb:162` does, so noise around that line cannot flip the daemon-reload decision; on `show`'s clean one-line stdout the two readings agree | `internal/ral/service_test.go` pins `is-enabled`'s token handling on clean output only, and `TestServiceDaemonReloadFindsItsLineAmongOtherOutput` pins `show`'s line scan with extra output before and after. **Nothing pins the stderr case against Ruby** |
| D-21 | A journalctl that cannot start (not installed, not executable) fails with `Execution of '<line>' failed: <err>`; Puppet says `Could not execute posix command: <err>` | Puppet's wording comes from `util/execution.rb`'s spawn rescue and wraps a Ruby `SystemCallError`; voxagent's is its own text around Go's error, which would drift if copied. A journalctl that starts and exits non-zero yields Puppet's `ExecutionFailure` text, which is reproduced | `internal/ral/service_test.go:TestServiceJournalctlFailureIsExecutionFailure` covers the non-zero exit. **The cannot-start wording is not pinned against Ruby** |
| D-22 | A host whose PID 1 is not systemd refuses the **whole catalog** when it holds a `service`, as a could-not-run before anything is applied; Puppet selects another provider | Provider selection is not implemented (D-12). Checked in `Service.Validate`, not `Prefetch`, because a prefetch failure applies every other type first and leaves the node half-converged, the case the whole-catalog refusal exists to prevent. The test is PID 1's `comm` containing `systemd`, so a container that is not run under systemd is refused | `internal/ral/service_test.go:TestServiceValidateRequiresSystemdAsInit`; `cmd/voxagent/main_test.go:TestServiceIsRegistered` pins that the pre-flight gap is gone, and `TestServiceOnANonSystemdHostRefusesTheWholeCatalog` pins, at the binary, that a valid `file` beside a `service` is not written and the run exits 2 as could-not-run (skipped where PID 1 is systemd) |
| D-23 | Retired. `service` and `exec` commands used to inherit voxagent's `HOME`, `USER` and `LOGNAME`; no child voxagent starts gets them now, as under Puppet | `util/execution.rb:387` deletes each of `util/posix.rb:11`'s `USER_ENV_VARS` in the child before every command Puppet executes (measured, M10). `exec` builds every child's environment, guards and `refresh` included, from `withoutUserEnv` (`internal/ral/exec_env.go`), and `runArgv` (`internal/ral/fileoutput.go`), which starts `service`'s `systemctl` and `package`'s dnf, applies the same function; `validate_cmd`'s `rubyEnvDropped` holds the same `userEnvVars`. A `user` exec is given no `HOME` either: Puppet sets none. Their `PATH` still differs: D-49 | `internal/ral/exec_env_test.go:TestExecChildHasNoUserEnv` (a command, its guard and the `runArgv` runner), `TestWithoutUserEnv`; `task dev:report-diff-systemd` and G5's package cases after the change |
| D-24 | A daemon that a `service` or `exec` command backgrounds can keep writing to stdout after the command returns; under Puppet its next write gets `EPIPE`, and `SIGPIPE` unless it ignores the signal | Puppet captures a command's output through a pipe on POSIX (`util/execution.rb:200`) and closes the read end once the direct child has exited (`:325`), so a backgrounded descendant's later writes reach a pipe with no reader. voxagent's `runCapturingToFile` (`internal/ral/fileoutput.go`, used by `fileOutputRunner` and `Exec.execute`) gives the command a temporary file instead: writes after the command returns land in a file whose name is already removed. Reproducing the pipe would mean killing a service for logging to stdout, an artifact of Puppet's capture rather than a contract | **Nothing pins it.** `TestFileOutputRunnerReturnsWhenABackgroundedChildInheritsStdout` and `TestExecReturnsWhenABackgroundedChildInheritsStdout` pin only that the command returns when its shell exits; no test has the backgrounded child write afterwards, and the systemd gate's custom commands write nothing after they return |
| D-25 | Under `--noop`, a generating parent's unit logs no `Would have triggered 'refresh' from N events` notice of its own | Puppet's completed whit is a real vertex, so a noop run's `process_noop_events` logs that notice at the generating resource's path with N the events the whit collected (measured: source `/tmp/rx/tgt`, N = 3). voxagent reproduces the whit's two effects -- blocking the parent's dependents and sending one event -- from the unit's aggregate without a vertex, so there is no sentinel whit to log the notice; a container's whits are vertices and do log theirs. Subscribers still receive their one event and log their own "Would have triggered" line | **Nothing pins it.** G1 compares a live run only |
| D-26 | A `source` that is a FIFO, socket or device fails the resource; Puppet describes it with a `{none}` checksum. (The symlink part is retired: a link source now reproduces Puppet, below) | Source metadata is read with `lstat` under `links => manage` (`file_serving/base.rb:79`), and a FIFO or socket is a type Puppet describes rather than refuses. voxagent refuses anything but a regular file, a directory or (below) a link. **Retired part:** under `manage` a symlink source makes the resource a link to the source's raw `readlink` (`type/file/source.rb:170-176`; `file_serving/metadata.rb:118-124`), as `link_created ... created`; under `follow` the source is read with `stat` and its target's content is copied (`file_created`, `defined content as ...`). `links` is consulted when reading a top-level source; a symlink inside a recursed source tree is still refused, as before | `internal/ral/file_source_test.go:TestResolveSourceRefusesASpecialFile`, `TestFileSymlinkSourceUnderManageMakesALink`, `TestFileSymlinkSourceUnderFollowCopiesTheTarget`; G1's `src-ln` and `src-follow` fixtures; `internal/ral/file_recurse_test.go:TestFileSymlinkInASourceTreeFailsThatChild` (recursion) |
| D-27 | Retired. A recursion child whose source type differs from what exists used to fail; it now reproduces Puppet | A source directory over a file replaces the file (`directory_created`, `ensure changed 'file' to 'directory'`). A source file over a directory fails the child's ensure with the notice twice (I-3) unless the parent's `force`, inherited by every child, lets it replace the directory (`file_created`, `defined content as ...`; `type/file.rb:791-818`) | `internal/ral/file_recurse_test.go:TestFileChildTypeConflictReplacesOrFailsByForce`; G1's `fdir`, `nofdir` and `wasfile` fixtures |
| D-28 | Retired. A symlink found by a recursion walk used to get no permission changes and no SELinux relabel; it now gets Puppet's treatment of the link itself | Puppet's `newchild` makes a walked link a `File` like any other (`type/file.rb:583-602`), with the parent's `links`, which is `manage` because `Validate` refuses `follow` with `recurse` (D-32). Its mode is left alone (`type/file/mode.rb:133`), `owner` and `group` are `lchown`ed (`provider/file/posix.rb:99-144`; measured as root on Puppet 8.10.0: `owner changed 'llm' to 'nobody'` and `group changed 'llm' to 'nobody'` on links into the tree, out of it and dangling, no `mode` event, the outside target untouched, and `(noop)` events with nothing changed under noop) and it is relabelled itself (the SELinux gate: `seltype changed 'tmp_t' to 'etc_t'`). Every operation goes through the tree's `rootFS`: `Lchown`, and the `O_PATH\|O_NOFOLLOW` handle the label is read and set through, act on the link and never its target, and `purge` still unlinks it | `internal/ral/file_recurse_test.go:TestFileWalkedSymlinkIsLchownedAndKeepsItsMode`, `TestFileWalkedSymlinkUnderNoopChangesNothing`; `internal/ral/file_selinux_test.go:TestSELinuxRelabelsAWalkedLinkItself`, `TestXattrHelpersActOnALinkItselfThroughTheRoot`; the SELinux gate's `tree` fixture, run as root: `tree/ln` and `tree/lnout` (to `lnreal`, outside the tree) are compared with Ruby for their `owner` and `group` events and no `mode` event, `tree/ln` (seeded `tmp_t`) for its `seltype` event, and `tree/lnout`, seeded `etc_t` and so already in sync, for having none; and the ownership listing shows `lnreal` keeping `root:root`. G1's `lnwalk` sees only a mode or group event that should not be there |
| D-29 | Retired. On an SELinux-enabled host a managed `File` used to keep whatever context it had; it now gets the `selabel_lookup` default as Puppet does | Closed by the `sel*` properties, defaults and relabelling, and by reading and writing a recursion child's label through its `os.Root`. `type/file/selcontext.rb:42-48` gives every `File` the four `sel*` properties, defaulted unless `selinux_ignore_defaults`, whenever `Selinux.is_selinux_enabled == 1` (`util/selinux.rb:21-28`); see "`file` SELinux contexts" below. The number is retired rather than reused | `internal/ral/file_selinux_test.go` (the `TestSELinux*` tests, against a fake host); this container cannot mount `selinuxfs`, so Puppet's side is measured on an enforcing VM (`tests/spine/report-diff-selinux.sh`) |
| D-30 | `links => follow`, `ensure => file` and no `content` or `source` over a dangling link fails the create; Puppet creates the file the link points at | Puppet opens the path in place (`type/file.rb:964-966`), and `File.open` follows the link. voxagent creates such a file with `O_EXCL` and never writes through a link, so the link fails it with `file exists` and nothing is created at its target (measured on Puppet 8.10.0 2026-10-06) | `internal/ral/file_test.go:TestFileFollowEnsureFileOverADanglingLinkWritesNothingThroughIt` |
| D-31 | A generated recursion child under an inherited `links => follow` whose destination is a symlink is not chmod'ed or chown'ed through it: a declared `mode` fails (`refusing to chmod through it`) and `owner`/`group` are `lchown`ed. Puppet chmods and chowns through the link | Under `follow` Puppet's `mode=` is `File.chmod` and `owner=`/`group=` are `File.chown` (`provider/file/posix.rb:99-144, 155-156`), all following the link. A child is applied through `rootFS`, whose `Chmod` refuses a link and whose `Chown` is `Lchown`, because inside the `os.Root` a link swapped in after the walk must not carry a chmod or chown anywhere, even within the tree. Not reachable from a catalog since `Validate` refuses `links => follow` with `recurse` (D-32), so every generated child is under `manage`; `rootFS` keeps the guarantee for any child applied regardless | `internal/ral/file_fs_test.go:TestFileFollowChildLinkIsNeitherChmodedNorChownedThroughTheRoot` |
| D-32 | `links => follow` with `recurse => true` is refused before anything is applied (`Validate`, exit 2 could-not-run); Puppet follows a linked source directory and copies its tree | Measured on Puppet 8.10.0: `source => '<dir>/dl', recurse => true, links => follow`, where `dl` links to a directory holding `a`, creates the destination and copies the tree (`File[/d/a]` `file_created`, `defined content as '{sha256}...'`); under `manage` it makes the destination a link and yields no children. voxagent's walk (`walkTree`) is `os.Root`-confined and never follows a link, so following one is a security-sensitive change not made yet. It is a `Validate` refusal, not a pre-flight `Gap`, because `SupportsValue` sees one attribute at a time and cannot express a combination; it is therefore not in the porting histogram | `internal/ral/file_source_test.go:TestFileValidateSourceAndCreators` (case `follow and recurse`) |
| D-33 | voxagent reads and writes raw SELinux contexts; Puppet's non-raw calls translate through mcstrans | `selcontext.rb:35-36, 121-122` runs `selrange` through `selinux_category_to_label` and `lgetfilecon` is the translating call, so Puppet compares and writes translated ranges. The two differ only on a host with mcstrans translations configured (`setrans.conf`), which a default RHEL or SIMP install does not have. voxagent's `current` reads `security.selinux` raw and `defaultFor` takes `selabel_lookup`'s non-raw output, which is the same string Puppet's non-raw lookup returns. On a host with translations the effect is not small: voxagent compares a translated default range against a raw label, so every `File` that wants defaults reports a `selrange` change on every run, and writing the translated range, which the kernel does not accept as a raw label, fails as a warning (I-4) | **Nothing pins it**: no mcstrans host here |
| D-34 | `ensure => present` with `content` over a path that is not a regular file omits Puppet's warning | Puppet keeps the path, as voxagent does, and `checksum_insync?` (`type/file/data_sync.rb:31-33`) logs `Ensure set to :present but file type is <ftype> so no content will be synced` at warning level, sourced at the resource (measured on a fifo: `/Stage[main]/Main/File[<path>]`). voxagent's provider warnings carry a fixed source (`ral.Warning`), and a resource-sourced one is not implemented. The disk state and the events agree; only that log line is missing | **Nothing pins it.** `internal/ral/file_test.go:TestFilePresentWithContentLeavesAFifo` pins the kept fifo and the absence of a blocking read, not the warning |
| D-35 | When the SELinux file-contexts handle does not open, voxagent refuses only a catalog with a `File` that would look a default up; Puppet fails every run | **Measured** 2026-10-06 on the SELinux gate's VM (openvox 8.29.0, enforcing) with `file_contexts` and `file_contexts.bin` moved aside, so `selabel_lookup` prints `ERROR: selabel_open - Could not obtain handle`: `puppet apply --catalog` printed `Error: Could not prepare for execution: Could not create resources for managing OpenVox's files and directories in sections [:main, :agent, :ssl]: Cannot get default context with nil handle`, exited 1, applied nothing (the catalog's notifies and files did not run) and wrote no report. Puppet 8.10.0 has the same path with `Puppet's files` (`settings.rb:1145`, `application.rb:415`). The failure is in Puppet's own settings resources, at setup, so it does not depend on the catalog. voxagent has no settings resources: its `Validate` probes the handle once and refuses the catalog, nothing applied, for each `File` that wants defaults and syncs its SELinux properties on a filesystem with labels, as `File[<path>]: Cannot get default context with nil handle`. A catalog with no such `File` is applied, where Puppet would not run. Before this, voxagent failed each such `File` and applied the rest. Verified in a second run of the same broken VM: voxagent printed `File[/etc/voxgate/i3a]: Cannot get default context with nil handle` for the one `File` wanting defaults (not the one with `selinux_ignore_defaults`), applied nothing and wrote no report; it exits with its own could-not-run code, `2` (README), where Puppet exited `1` | `internal/ral/file_selinux_test.go:TestFileValidateRefusesTheCatalogWhenNoHandleOpens`, `TestHandleOpensProbesOnceAndReportsANilHandle`. No gate fixture: breaking `file_contexts` would fail every other fixture of the SELinux gate, so it was measured in a separate VM run |
| D-36 | A skipped resource's `evaluation_time` is `0` in voxagent's report and `null` in Ruby's, for a native resource skipped because its provider's prefetch failed and for a Tier 3 resource skipped the same way (the host's `evaluate` or `skip` answers it) | Ruby marks the resource skipped before `apply` runs (`transaction.rb:280-284`), so it is never timed and `evaluation_time` stays `null`. voxagent has no unset float in its report, and for a Tier 3 resource the host's round trip is not Puppet's span, so it writes `0`. `resource_statuses.*.evaluation_time` is in `tests/spine/allowlist.json`'s volatile set, and that pattern is what hides the difference: both spellings of "no time was spent" are dropped before the comparison, so no gate would catch a change in either | `internal/transaction/transaction_test.go:TestPrefetchFailureFailsEveryResourceOfTheTypeAndAppliesNone` (native), `internal/transaction/evaluator_test.go:TestEvaluatorPrefetchSkipBlocksAndIsNotRefreshed` (Tier 3) |
| D-37 | Retired. A Tier 3 resource with no suitable provider used to be answered where voxagent's walk reached it; it is now deferred and answered last, as Puppet does | `relationship_graph.rb:114-145` sets a resource that is not `suitable?` aside, offers it again once the ready queue drains if anything was placed since the last drain, and otherwise hands it to the overly-deferred handler (`transaction.rb:124-136`), which logs `Provider X is not functional on this host` (or records the type for teardown's once-per-type `Could not find a suitable provider for X`) and marks it failed. `graph.Walker` keeps the same deferred list and progress flag; the host answers `unsuitable` without logging and logs at `overly_deferred`. Inside an `eval_generate` unit the resource is still failed at its turn (D-56) | `internal/graph/graph_test.go:TestWalkerDefersToTheEndOfADrain`, `TestWalkerHandsBackEveryOverlyDeferredVertexFirst`; `internal/transaction/evaluator_test.go:TestRunDefersAnUnsuitableResourceToTheEnd`, `TestEvaluatorUnsuitableFailsWithoutEvaluation`; `internal/extension/ruby_test.go:TestRealHostUnsuitableProvider` |
| D-38 | The dependent of an unsuitable Tier 3 resource whose own dependency failed names both failures in its `Dependency ... has failures` lines; Puppet names only the unsuitable resource | Puppet's `propagate_failure` (`transaction.rb:333-349`) builds each resource's failed dependencies from its direct prerequisites, inheriting a prerequisite's own list only when that one was skipped for it. The unsuitable resource is failed rather than dependency-failed, so it stops the inheritance. voxagent blocks the transitive closure of every failure, so the original failure reaches past it. **Measured** 2026-10-07: `vox_value { 'broken': value => 'fail' }`, `vox_conf { 'late': require => Vox_value['broken'] }`, `file { 'after-late': require => Vox_conf['late'] }`: Ruby logs only `Dependency Vox_conf[late] has failures: true`, voxagent also `Dependency Vox_value[broken] has failures: true`. Both skip the file; only the extra notice differs | Nothing; the shape of the unsuitable resource itself is pinned by `internal/transaction/evaluator_test.go:TestEvaluatorUnsuitableWithFailedDependencyIsFailedNotSkipped` |
| D-39 | A type all of whose resources were skipped for failed dependencies has a `metrics.time` entry of `0` in voxagent's report and none in Ruby's | `report.rb:499-510`'s `calculate_time_metrics` adds only a status with an `evaluation_time`, and a skipped resource's is `null` (D-36). voxagent's `Finalize` sums every status not marked `Untimed`, and only the Tier 3 never-evaluated paths (prefetch-skipped and unsuitable) set it; a resource skipped for dependencies, native or Tier 3, is still summed as `0`. Left so to keep native behaviour unchanged in the extension-host work. **Measured** 2026-10-07 in the D-38 catalog: Ruby's `metrics.time` has no `file` entry, voxagent's has `file: 0` | Nothing |
| D-40 | Under `--noop` the `events` metric has no `noop` entry; Ruby's counts noop events there | `report.rb:467-478`'s `calculate_event_metrics` seeds `total`, `failure` and `success` and then adds one to `metrics[event.status]` for every event, so a noop run gains a `noop` entry. voxagent's `Finalize` counts only `failure` and `success`. **Measured** 2026-10-07 on `report-diff-extension.sh`'s catalog under `--noop`: Ruby `[["total","Total",13],["failure","Failure",2],["success","Success",0],["noop","Noop",11]]`, voxagent the same without the `noop` entry. A native report defect, left unfixed to keep native behaviour unchanged in the extension-host work. A G1 `--noop` pass for Tier 3 (Ruby `--noop` against voxagent `--noop` on `report-diff-extension.sh`'s catalog from the reset starting state, same allowlist) agreed on every Tier 3 leaf -- `noop` events and their ` (noop)` messages, `Would have triggered` on `Vox_value[refreshed]` and `Vox_value[refuser]`, the report's `noop` -- and diverged only on native resources (549 leaves, 18 divergent: this row, its sibling and D-10's missing ` (noop)` suffix on five `File` events). It is held back from the gate until these native defects are fixed | Nothing; no gate runs `--noop` |
| D-41 | Retired. Under `--noop` a native `File` that a changed resource notifies used to log `Would have triggered 'refresh' from N event(s)`; it now logs nothing, as Puppet does | `event_manager.rb:73` queues an event only for a target that responds to the callback, and neither `type/file.rb` nor `type/notify.rb` defines `refresh` (among the native types only `exec`, `service` and `package` do). `ral.Refreshing` carries that answer: `File` and `Notify` return false, an extension-host type answers with its type's `refreshable`, and a provider that does not implement it is refreshable. The walk drops a pending refresh silently for a target that is not, so the `File` and `Notify` members of a notified class are not scheduled either. **Measured** 2026-10-07, before the fix, on `report-diff-extension.sh`'s catalog under `--noop`: Ruby logged `Would have triggered` for `Vox_value[refreshed]` and `Vox_value[refuser]` only; voxagent also for `File[$T/notified]`, which `Vox_value[notifier]` notifies. No gate runs `--noop` yet: D-10 and D-40 still hold that pass back | `internal/transaction/transaction_test.go:TestNoopDoesNotScheduleANonRefreshableNative`, `TestRefreshIntoAContainedClassIsOneEvent`; `internal/ral/file_test.go:TestFileIsNotRefreshable`; `internal/ral/notify_test.go:TestNotifyIsNotRefreshable` |
| D-42 | A new resource an extension-host `eval_generate` makes is a resource error, where Puppet applies it, when (a) its type has no provider at all: `resource type <Type> is not implemented`; (b) pre-flight's judgement or the provider's `Validate` refuses it: the first gap, or the validation error; (c) its own generation yields resources: `<ref> generates resources, and resources generated during evaluation that generate further are not implemented` | The walk applies each new resource through its own type's provider (`visitHostUnit`). A type the catalog never named is prefetched at its first generated resource, as `prefetch_if_necessary` prefetches lazily (`transaction.rb:243-260`), so (a) is only a type voxagent's registry does not know. Pre-flight never saw a generated resource, so (b) judges it then; a resource Puppet evaluates in the host is judged only by the metaparameters the walk would have to honour. (c) runs the child's own generation at its turn and refuses it rather than applying it without what it made. Three consequences follow. A nested generator whose turn is skipped, because its parent or a generated ancestor failed, never runs its `eval_generate`, so its logs, `failed_to_generate` and overrides are lost where Puppet's `pre_process` would run it. An override a nested generation makes on a sibling in the same unit is lost, because the walk overrides only catalog resources. A skipped child of a type that was never primed reports no `provider_used` | `internal/transaction/evalgen_test.go:TestGeneratedChildWithNoProviderIsRefused`, `TestGeneratedChildIsJudgedLikeACatalogResource`, `TestGeneratedChildThatGeneratesIsRefused` |
| D-43 | An `eval_generate` that returns resources already in the catalog does not move its parent's other out-edges onto a `completed_<title>` sentinel, does not make those resources depend on the parent, and relationship values an `eval_generate` appends are not applied | `additional_resource_generator.rb:86-117` moves the parent's out-edges onto a sentinel that waits for every generated resource. `made` includes the existing resources, through `replace_duplicates_with_catalog_resources` (:80-84), so `connect_resources_to_ancestors` (:119-128) also makes each of them depend on the generator: when the generator fails, Puppet skips them, where voxagent applies them with the override. voxagent's walk order is fixed before the walk, so `ral.Extension.EvalGenerate` drops the host's `existing` and `before`; its overrides are still applied to a resource not yet visited. Unobservable for `concat`: its `generate` already orders `Concat_file` before the `File` (`add_generated_directed_dependency`, :47-50), and the `concat` define declares no relationship on `concat_file` (`fleet/.vendor/modules/concat/manifests/init.pp`) | `internal/ral/extension_test.go:TestExtensionEvalGenerateConvertsTheHostsAnswer` |
| D-44 | Retired. Resources no relationship orders used to be applied in byte order of their refs; they are now applied in catalog order, as Puppet applies them | Puppet keys every vertex by its index in the catalog array (`SequentialPrioritizer`, `graph/sequential_prioritizer.rb:17-24`) and always takes the smallest ready key (`relationship_graph.rb:97-99`); generated children slot under their parent (`[p,1]`, `[p,2]`, ...) and a container is walked as its admissible and completed whits, which take its key (`relationship_graph.rb:259-261`). The walk now does the same: catalog-order keys, the unsuitable-provider deferral, generated positions and whits, all measured. `gatekit compare-order` compares the order each resource first logs in, and every report gate runs it. **Measured** 2026-10-08: `walk order agrees over 112 resources` (`dev:report-diff`, whose `wo*` fixtures exercise these cases), `29` and `15` (`dev:report-diff-extension`, first and second apply), `23` (`dev:report-diff-container`), `8` (`dev:report-diff-systemd`) and `22` (`dev:report-diff-selinux`). The earlier byte-order walk fails G1 with `walk order diverges at 0: ruby Notify[spine], voxagent File[$T/a.txt]` | `internal/graph/graph_test.go:TestOrderTakesTheSmallestReadyKey`, `TestOrderFollowsKeysForIndependentResources`; `internal/transaction/build_test.go:TestBuildGraphKeysResourcesByCatalogIndex`, `TestBuildGraphOrdersContainerContentsThroughWhits`, `TestWhitIsAQueueEntryNotASplice`, `TestWhitOrderMatchesPuppet`; `cmd/voxagent/main_test.go:TestWalkOrderAroundARecursedDirectory`; `dev:report-diff` and its siblings through `gatekit compare-order` |
| D-45 | `backup`: a resource's backup warnings are logged before its notices, where Puppet interleaves them; a bucket failure logs one `Puppet` error line fewer; a `fifo`, socket or device inside a directory backed up to a suffix is refused; a link retargeted between its stat and its open can still have a device opened; a directory suffix backup fails closed, and stays refused, where the agent does not own the directories it makes | (a) `remove_existing` runs twice on one failed `ensure => file` over a directory without `force` (I-3), logging warning, notice, warning, notice (measured); `ral.Change` carries `PathWarnings` and `PathNotices` apart, so voxagent logs warning, warning, notice, notice. G1 compares logs as a multiset. (b) Dipper's rescue (`file_bucket/dipper.rb:55-59`) calls `Puppet.log_exception` with `Could not back up <file>: <detail>` before raising it, so a failed bucket write has one more `err` line sourced `Puppet` than voxagent's, which logs the failure as any property failure (read, not measured: no gate fails a bucket write). (c) `FileUtils.cp_r` recreates a fifo or device node (`mkfifo`, `mknod`); `copyTree` fails the backup with `Could not back <path> up: cannot copy <path> of type <ftype>`, so the replacement does not happen, and removes the partial copy (`RemoveAll`, which never follows a link) so the next run is not refused by `Will not remove directory backup` (read). Residual: that removal is by path, so a writer of the copy's parent, when the parent is not sticky, who renames a directory onto `<path><suffix>` between the failure and the removal makes the agent delete that directory's contents (`RemoveAll` never follows a link, so only a renamed-in directory is exposed); the robust form, for later, empties the copy through the descriptor `copyRoot` opened (an `os.Root` on it) and then removes the emptied directory. (d) A link below a directory backed up to the bucket is stat'ed through its directory's checked descriptor, and only a regular file is opened (`openLinkAt`). A writer of that directory who retargets the link at a device between the stat and the `openat` has the device opened, with `O_NONBLOCK|O_NOCTTY`, before the second check refuses it, and opening a device as root can act on it. Puppet has the same gap: `File.file?` stats the path (`util/backups.rb:29`), then Dipper reads it by path (`file_bucket/dipper.rb:39-44`). The robust form, for later, never opens anything but a regular file: `openat` the link with `O_PATH` (which follows it but opens no device), `fstat` that descriptor and refuse anything but a regular file, then reopen it through `/proc/self/fd/<pathfd>` with `O_RDONLY|O_NONBLOCK|O_NOCTTY`. (e) The top of a directory's suffix copy must be owned by the agent's effective user (`copyRoot`). Where the filesystem gives a directory the agent just made to another owner, NFS with `root_squash` being the usual case, the check fails closed: the copy is refused with `Could not back <path> up: <path><suffix> is not the private directory the backup made`, the change it guards does not happen, and the empty `0700` `<path><suffix>` directory is left, because the agent cannot tell it from one a writer of the parent swapped in. Every later run is then refused with `Will not remove directory backup <path><suffix>; use a filebucket` (`remove_backup`, `util/backups.rb:56-79`) until the directory is removed by hand (`rmdir '<path><suffix>'`, which removes it only while it is empty). On such a filesystem a suffix backup of a directory cannot succeed, so the resource needs `backup => false` or `'puppet'`. Puppet copies there without the check (read, not measured: no gate runs on a filesystem that squashes root). Info and debug lines (`Filebucketed …`, `Removing old backup …`, the lock lines) are not divergences: Puppet's report carries them only with `--debug` (measured), and voxagent has no debug level | `internal/ral/file_backup_test.go:TestFileBackupWarnsForADirectoryWithoutForce`, `internal/transaction/transaction_test.go:TestPathWarningsAreLoggedAtTheResourceBeforeItsNotices`; (c) `TestFileBackupSuffixRemovesAPartialTreeCopy`; (e) the refusal, `TestFileBackupSuffixRefusesADestinationSwappedAfterItsMkdir`; (b), (d) nothing |
| D-46 | `backup => <hash>` is refused with the hash in Go's rendering, `Invalid backup type map[k:v]`; Puppet prints Ruby's `Hash#inspect` | **Measured** 2026-10-07 (`puppet apply -e`, Ruby 3.4.9): `Parameter backup failed on File[/tmp/bkh]: Invalid backup type {"k" => "v", "n" => [1, "a"]}`. Ruby prints a hash in insertion order, and the catalog's JSON object reaches voxagent as a Go map, which has lost that order, so the rendering cannot be reproduced for more than one key; Ruby's format also changed in 3.4 (`=>` gained spaces). A whole number, a decimal such as `5.5` and `nil` are rendered as Ruby renders them, and a list is its first element, as munge's `shift` makes it (measured: `5.5`, `[]` gives `nil`, `[5]` gives `5`). Both refuse the catalog before applying anything; only the message differs | `internal/ral/file_backup_test.go:TestFileValidateBackupRefusesAMap` pins voxagent's wording; `TestFileValidateBackup` pins the measured scalar and list cases |
| D-47 | `backup => 'puppet'` on a generated child that is a directory buckets a link below it only when what the link reaches lies inside the recursed tree; Puppet buckets what it reaches wherever that is | `perform_backup_with_bucket` (`util/backups.rb:21-34`) walks the directory with `Find.find` and backs up every entry `File.file?` accepts, which follows a link, so Dipper reads the target's bytes from anywhere on the host (measured for a catalog resource by a probe while adding `backup`: an inner link to a file outside the directory is bucketed under its realpath, I-14). voxagent opens the link relative to its directory's checked descriptor (`openLinkAt`) and then asks the child's `rootFS` whether the opened file's real path, read from the kernel through `/proc/self/fd`, is below the tree's (`rootFS.Contains`); a target outside is silently not bucketed, as any link that reaches no regular file is not, and the removal goes ahead. Not reproduced because a generated child is confined to its tree: a writer of the recursed directory chooses the link, so following it would read any file on the host as root into the bucket on that writer's say. A catalog resource (`osFS`) follows the link as Puppet does. Read, not measured: no gate purges a directory holding a link out of its tree. An in-tree link given as an absolute path was not bucketed before `Contains` (`os.Root` refuses absolute links) and now is, as Puppet buckets it | `internal/ral/file_backup_test.go:TestFileBackupBucketOfAGeneratedDirectoryStaysInItsTree` |
| D-48 | `validate_cmd`: an unexecutable validator's output, the debug lines, the child's `PATH`, and `recurse` | (a) A validator that cannot be executed (`ENOENT`, `EACCES`) fails with Puppet's `Execution of '<cmd>' returned 1: ` prefix, but the text after it differs: Ruby's forked child logs the exec error through the agent's own log destinations into the captured pipe, so on the console it is `\e[1;31mError: Could not execute posix command: No such file or directory - <argv0>\e[0m` with the ANSI codes inside the report message, and with `--logdest <file>` it is a Ruby `EBADF` backtrace (measured, c06a and x06h). voxagent writes `Could not execute posix command: <strerror> - <argv0>`. The comparator's `failure_detail` normalisation masks it, as it masks every `ExecutionFailure`'s output. (b) No debug lines: Puppet logs `Executing: '<cmd>'` (source `Puppet`) and each output line at the resource only under `--debug` (measured, c01c), and voxagent has no debug level. (c) The validator inherits the agent's `PATH` as it is; Puppet's `path` setting first appends any missing `/usr/sbin` and `/sbin` to the agent's own `PATH` (`defaults.rb:354-373`, measured c06e). This is D-49, which covers every command voxagent runs. (d) `validate_cmd` with `recurse => true` is refused (`Validate`); Puppet hands it to every generated child (`type/file.rb:583-603`), substituting each name found on disk into a command line `/bin/sh` may parse. (e) `staging_location` is not implemented, so it is a pre-flight refusal like any undeclared attribute. The temp file's date, pid and random token differ between any two runs, Ruby against Ruby included; `maskValidateTempNames` masks only those, inside the command, so the directory, base name and naming shape stay compared | (a) G5's `v-noexec` fixture, through `failure_detail`; `internal/ral/file_validate_test.go:TestFileValidateCmdExecFailureReturnsOne` pins the prefix. (b) nothing; (c) see D-49. (d) `TestFileValidateRefusesValidateParameters`. The temp-name mask: `internal/gatekit/reportcmp/normalise_test.go:TestMaskValidateTempNames` |
| D-49 | Commands voxagent runs search the agent's `PATH` as it is; Puppet first appends whichever of `/usr/sbin` and `/sbin` it lacks | The `path` setting's hook (`defaults.rb:354-373`) runs when Puppet's settings are initialised and appends each of `Puppet::Util::Platform.default_paths` missing from `ENV['PATH']`, so every command Puppet runs, and every program that command starts, searches them. voxagent changes `PATH` only for an `exec` that declares `path`, which replaces it as Puppet's does. So these differ: an `exec` with no `path` (its command must then be fully qualified, but what it runs inherits the `PATH`), `onlyif` and `unless` likewise, `service`'s `systemctl` and its `start`, `stop`, `status` and `restart` commands, `package`'s `dnf` and `rpm`, which are found through `PATH` (`exec.LookPath`), and a `File`'s `validate_cmd` (D-48(c), measured c06e). Invisible while the agent's `PATH` already holds both directories, as the gate environments' do; visible when it lacks them, where a command or tool that lives only in `/usr/sbin` or `/sbin` is found by Puppet and not by voxagent. Tier 3 resources are not affected: the extension host initialises Puppet's settings, so its own `PATH` is amended | (c) of D-48 was measured (c06e); the rest is read. Nothing pins it |
| D-50 | A catalog that declares a `filebucket` resource is a **pre-flight refusal** (`Filebucket[<title>]: resource type Filebucket is not implemented: native File's backup does not implement named filebuckets`, exit 2 could-not-run, nothing applied); Puppet applies it and backs `File`s up into it | `filebucket` is a core type, so the extension host could apply the resource itself, but native `File`'s backup implements only the default `puppet` bucket under `--vardir` (`internal/ral/file_backup.go`). Applying the declaration would be worse than refusing it: a `File` with `backup => '<title>'` would fail with `Could not find filebucket <title> specified in backup` where Puppet backs up into it and replaces the file, and a declared `puppet` bucket with a `path` or a `server` would be bypassed without a word, its backups landing in `<vardir>/clientbucket`. So `ral.ResourceGaps` refuses every declared `filebucket` whatever provider claims the type, and `cmd/voxagent` never starts the host for one | `internal/ral/preflight_test.go:TestPreflightRefusesADeclaredFilebucket`, `cmd/voxagent/extension_test.go:TestDeclaredFilebucketIsAPreflightGap` |
| D-51 | voxagent refuses rich catalog values Ruby applies: (a) a `Deferred` value, at any depth (inside a `Sensitive`, a `Hash`, an Object's init hash, an array or a hash), on any resource that is not a container, one line per parameter holding one, refused straight after decode and before the extension host starts (`<Ref>: parameter <name> is Deferred; voxagent does not resolve Deferred values`, then `<n> Deferred value(s); nothing applied`, exit 2 could-not-run); on a native resource, at pre-flight, (b) a `Binary` that is not ASCII (`... holds a Binary value that is not ASCII, which voxagent does not support on a native resource`), (c) a `Sensitive` map, and (d) every other rich kind: `Default`, `Hash`, `Timestamp`, `Timespan`, `SemVer`, `SemVerRange`, `Regexp`, `URI`, `Error`, `Pcore::*` and Object (`... holds a rich <__ptype> value, which voxagent does not support`). An ASCII-only `Binary` is not a divergence: `catalog.Decode` turns it into its string, at any depth | (a) Ruby resolves a Deferred at apply (`resolve_and_replace`, `a-b` for `Deferred('join',[['a','b'],'-'])`), and the host would do the same on loading the catalog, wherever the Deferred sits, a secret looked up at apply time inside a `Sensitive` included; voxagent does not resolve Deferred values. Container parameters are never read, so a Deferred one is accepted. (b) Ruby renders a non-ASCII Binary byte-wise (`w6k=` logs as `Ã©`), which no native provider reproduces; SIMP catalogs carry none. (c) a top-level Sensitive arrives as a plain value with `sensitive_parameters`, which is honoured (D-8, retired); a Sensitive map is hand-crafted or nested, and has no Go representation. (d) Ruby hands each to the type as a Ruby object; no native provider takes one. A Tier 3 resource keeps every rich value except Deferred, because the host decodes the raw catalog bytes itself | `internal/ral/preflight_test.go:TestResourceGapsRefuseRichValuesOnNativeResources`, `TestResourceGapsLeaveRichValuesToTheHost`, `TestDeferredGapsSkipContainers`, `TestDeferredGapsFindNestedDeferred`; `cmd/voxagent/main_test.go:TestNonASCIIBinaryOnANativeResourceIsRefused`, `TestDeferredIsRefusedBeforeTheHost`, `TestSensitiveDeferredOnATier3ResourceIsRefusedBeforeTheHost` |
| D-52 | `catalog.Decode` accepts only the `__ptype` names measured: `Binary`, `Sensitive`, `Default`, `Hash`, `Symbol`, `LocalRef`, `Timestamp`, `Timespan`, `SemVer`, `SemVerRange`, `Regexp`, `URI`, `Error`, `Deferred`, and any name beginning `Pcore::`. Any other string name fails the whole catalog with Ruby's text, `Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: No implementation mapping found for Puppet Type <name>`; Ruby resolves any type its loaders know, module-defined Object types included | Go cannot load a module's Object types, and an unknown name anywhere fails Ruby's whole catalog too (`from_data_converter.rb:133-140`), so refusing at decode keeps the one message. Two `Binary` messages are read, not measured: a `__pvalue` that is not a string gives `Cannot create a Binary from a <class>` (`from_data_converter.rb:224-226`); a Hash `__pvalue` goes to Object initialisation (`:212-220`), which `Binary` does not support, and is given the same message. Invalid base64 (missing padding, a CR or LF, a non-alphabet byte) fails the catalog with `... invalid base64`, as `Base64.strict_decode64` does (`p_binary_type.rb:181-193`) | `internal/catalog/richdata_test.go:TestDecodeRefusesAnUnknownPType`, `TestDecodeRefusesInvalidBase64`; `TestDecodeRefusesANonStringBinary` pins the two read messages; `cmd/voxagent/main_test.go:TestInvalidBase64IsCouldNotRun` |
| D-53 | `rubyInspect` (`internal/ral/inspect.go`) escapes only Cc, Zl and Zp code points and keeps every other code point raw. Ruby also escapes the non-printable rest (unassigned, noncharacters, private-use beyond the BMP): `ruby -e 'puts "\u{FFFF}".inspect'` prints `"\uFFFF"` (six ASCII characters, escaped) where voxagent prints the character; `ruby -e 'puts "\u{378}".inspect'` prints `"\u0378"` (unassigned); `ruby -e 'puts "\u{10FFFF}".inspect'` prints `"\u{10FFFF}"`. Go's Unicode tables are a different version from Ruby's, so exact `isprint` parity is not cheap, and the inputs are mode and argv strings. `rubyInspectValue` also renders a Hash with Go's `%v` (`map[k:v]`) where Ruby 3.4 prints `{"k" => "v"}` (`ruby -e 'puts({"k"=>"v"}.inspect)'`); only the backup-type error can reach it. | Only the quoted text of an error message naming such a value differs; accepted. | `internal/ral/inspect_test.go:TestRubyInspect` pins U+FFFF raw |
| D-54 | argv-array `exec` commands: (a) the failure detail and the noop message render the argv as `Array#inspect` assuming a UTF-8 locale, so `"é"` stays raw where Ruby under the container's empty `LANG` prints `"\u00E9"`; (b) an argv element that is a Hash, at any depth, in `command` or in an `onlyif` or `unless` argv check, is refused in `Validate` (`Exec[x]: command => an argv element of class Hash is not implemented`), where Ruby passes `Hash#to_s` as the argument; (c) with a `path`, an empty argv or one whose first element is not a String is refused in `Validate` (`... => an empty argv is not implemented`, `... => an argv whose first element is of class Array is not implemented`), where Puppet fails the whole catalog in autorequire with `Failed to apply catalog: undefined method 'scan' for nil:NilClass` (or `for [...]:Array`) after warning `Cannot check if  is an absolute path ...`; without a path both are refused by `validatecmd` with Puppet's text, naming `''` or the element's `to_s`, and without that warning; (d) once checkexe has passed, an exec that still cannot start (no file found from `cwd`, a `cwd` that cannot be entered, `EACCES` on an interpreter, an empty argv the engine did not validate) is status 1, so `'<argv>' returned 1 instead of one of [0]`, as Puppet's child calls `exit!(1)` (`util/execution.rb:392-396`), and the `Could not execute posix command: ...` line that child logs is not written. The one exception is a file with no `#!` line (`ENOEXEC`), which is not a divergence: it is retried as Ruby's exec retries it, `/bin/sh <file> <args...>` with `sh` as argv[0], the script path and each argument a separate positional argument and no `-c`, so the shell reads the file and never parses the argv; (e) no `~` expansion of a `path` entry, on either side: checkexe's `which` gets it from Ruby's `File.expand_path`, and the child's own search from Ruby's `dln_find_exe_r`, both against `HOME`; voxagent's `checkExe` and `lookPathIn` take `~` literally; (f) native change to `exec` autorequires, made to follow `type/exec.rb:593-640`: an argv command's first element is scanned (with both patterns), a list of string guards is now scanned element by element (before, only a scalar guard was), an argv check's first element is scanned, and guards are scanned with `^(/\S+)` only, where voxagent also scanned them with the quoted pattern; edges are added in Puppet's order | (a) measured; the agent normally runs under a UTF-8 locale, and `rubyInspect` knows no other. (b) `rubyInspectValue` renders a Hash with Go's `%v` (D-53), so the argument would differ; no catalog seen passes one. (c) measured; refusing before anything applies is the nearest voxagent has to a catalog that fails whole. (d) read, not measured, except the `ENOEXEC` retry, measured (a 0755 file with no `#!` line ran as a script); no gate reaches another start failure past checkexe. (e) read, not measured: no catalog seen puts `~` in an exec `path`. (f) read, and the guard's quoted pattern measured (`onlyif => '"/tmp/d/run.sh" x'` added no edge to the managed `File[/tmp/d/run.sh]`). Unchanged for argv, and so still divergent: D-10 (noop message) and D-49 (`PATH`). Not reproduced, deliberately: a one-element argv that would reach Ruby's shell (I-25) | (a) `internal/ral/inspect_test.go:TestRubyInspect`; (b), (c) `internal/ral/exec_test.go:TestExecValidateCommandType`; (d) `TestExecArgvStartFailures`; (e) nothing; (f) `TestExecAutoRequiresFollowPuppet`, `TestExecAutoRequiresTheFilesItNames` ("a guard's quoted path is no edge") |
| D-55 | A `before`, `require`, `subscribe` or `notify` naming a Tier 3 resource by a catalog title Puppet cannot look up, because its type overrides `title` (`Vox_titled[fips]`, which Puppet names `Vox_titled[fips:all]`), is refused before anything applies: `voxagent: Failed to apply catalog: Could not retrieve dependency '<catalog ref>' of <declarer's Puppet ref>; nothing applied`, could-not-run (exit 2, or 1 under `--detailed-exitcodes`), no report. Ruby fails the run with the same text, writes a failed report with no resource statuses and that one `err` log, and exits 0, even under `--detailed-exitcodes`; not reproducing that exit 0 is I-28. A target named through an alias resolves in both and is not refused. Not a divergence: such a resource is reported under Puppet's ref (status key, `resource`, `title`, containment leaf, log sources, `Dependency <ref> has failures: true`), while `tags`, `file`, `line` and `resource_type` stay the catalog's and the walk, the graph and the host keep the catalog's ref | Ruby's `apply --catalog` exit status is not authoritative (`tests/spine/report-diff.sh` header), and a run that applied nothing because its catalog cannot be ordered is could-not-run under voxagent's exit contract. The host decides resolvability with Puppet's own lookup (`unresolvable` on `load`, `docs/maintainers/extension-host.md`); `transaction.BuildGraph` checks each relationship target as written, before alias canonicalisation, and returns `transaction.UnresolvableError` (`type.rb:1516-1524`, `configurer.rb:507`) | `internal/transaction/build_test.go:TestBuildGraphRefusesAnUnresolvableTitle`, `internal/transaction/evaluator_test.go:TestRetitledResourceIsReportedUnderPuppetsRef`, `cmd/voxagent/extension_test.go:TestUnresolvableTitleIsCouldNotRun` |
| D-56 | Inside an `eval_generate` unit, a resource with no suitable provider (the generating parent, or a resource it generated) is failed at the unit's turn, with Puppet's status and lines, where Puppet defers it to the end of a drain like any other | voxagent applies a unit's members together at the parent's turn rather than adding them to the graph (`applyUnit`), so a member has no vertex the walk could set aside. Puppet would also defer an unsuitable parent before `pre_process`, so it would not `eval_generate` at all; voxagent has already run `eval_generate` when the parent's answer arrives. No fixture has an unsuitable `eval_generate` type, and no gate reaches it | `internal/transaction/evalgen_test.go:TestUnsuitableMemberOfAUnitIsFailedAtItsTurn` |
| D-57 | A failing `package` install's `evaluation_time` leaves out dnf's mirror-metadata fetch, which Ruby's includes. On the SIMP gate's catalog, `Package[haveged]` (in EPEL, which the image does not enable) takes 4.8 to 7.1 s in Ruby and 0.27 to 0.35 s in voxagent, over 5 fresh-container applies each (measured 2026-10-08), and the failure messages differ: Ruby's output starts with dnf's per-repository download lines, voxagent's with `Last metadata expiration check: 0:00:01 ago on …` | Both run the same command, `/usr/bin/dnf -y install haveged`: openvox `yum.rb:305` builds `[command(:cmd), "-y", install_options, operation, wanted]`, and `allow_virtual` defaults to true, so the `list` probe at `yum.rb:244-246` is skipped. The difference is which `dnf` command fetches the metadata first, which is D-16's *when*: visible in no compared report leaf, but visible in `evaluation_time`, which no gate compares. Ruby's prefetch (`yum.rb:72-76`) reads only the rpm set, and `check_updates` runs lazily, when a `latest` resource is retrieved (`yum.rb:91-97`), so haveged's install, the first `dnf` call in the run, pays for the fetch. voxagent's `Package.Prefetch` runs `dnf -y check-update` up front (`internal/ral/package.go:691`) whenever an installed resource asks for `latest`, and the SIMP catalog's `crypto-policies` and `crypto-policies-scripts` qualify. That prefetch runs inside `transaction_evaluation` (`internal/transaction/transaction.go:159`, `:170`) but before any resource's timer (`:468`, `:506`). The applied-catalog time and the transaction total therefore include the fetch, and no `metrics.time` type entry does. Measured in a booted `localhost/voxagent-simp` container, with `dnf clean all` before each sequence, 3 rounds: the install alone took 8.56, 8.75 and 5.45 s; `dnf -y check-update` (rc 100) took 4.04, 11.09 and 3.98 s, and the install after it 0.34, 0.29 and 0.29 s. Both orders exit 1 with `No match for argument: haveged`. No gate sees this: `resource_statuses.*.evaluation_time` and `metrics.time.values.package.value` are `run_to_run` entries, and the `failure_detail` normalisation masks the message's output. Reproducing it means deferring check-update to the first `latest` retrieval, which is not implemented | Not pinned by a test. `internal/ral/package_test.go:TestPackageResolveUpdatesRunsOnceForTheWholeRun` pins the single up-front check-update this follows from, and `docs/maintainers/performance.md`, "Public SIMP catalog applied", records the measurement |
| D-58 | A Tier 3 type declared `self_refresh: true` that defines no `refresh` method: after a change, Puppet fails the resource with `Failed to call refresh: …`; voxagent drops the self-refresh silently and the resource keeps its status | `event_manager.rb:79` queues the self-event with no `respond_to?` check, unlike the edge events at `:73`, and `Puppet::Type` defines no `refresh`, so `process_callback` (`:144`) raises `NoMethodError` and its rescue (`:160-165`) logs `Failed to call refresh: <detail>` at `err`, sets `failed_to_restart`, fails the resource with that event and calls `log_exception`; under `--noop`, or when every event is noop, it logs `Would have triggered 'refresh' from N event(s)` instead (`:133-135`, `:174-179`). voxagent queues the self-events (`internal/transaction/transaction.go:709-736`), but the inbound refresh block runs only for a refreshable target (`:768-772`), and the host answers `refreshable` as `method_defined?(:refresh)` (`internal/extension/host.rb:274`), so the queue is deleted with nothing logged. Read, not measured: no fixture or catalog seen has such a type (`mount_core`'s `Mount` defines `refresh`), and the type cannot work under Puppet either | Nothing pins it |
| D-59 | `voxagent agent` always requests the catalog with `static_catalog=false`; Puppet sends `true` | A static catalog makes the server inline `file` metadata and content references (`code_id`-addressed `file_content`), which needs the static-catalog fetch path voxagent does not implement. With `false` the server returns an ordinary catalog and voxagent fetches `puppet:` sources itself. Measured against the stock server. | `internal/puppetapi/client_test.go:TestCatalogFormMatchesPuppet` |
| D-60 | Every run POSTs a node request; Puppet skips it when `last_run_summary.yaml` records the environment as unchanged | voxagent writes no `last_run_summary.yaml`, so it cannot know the previous environment. The node request costs one GET and the server's classification wins either way | nothing pins it |
| D-61 | `file_content` is fetched for every `puppet:` source, even when the local file already matches; Puppet's provider fetches content only after the metadata checksum differs | voxagent resolves sources at pre-flight, before any resource is evaluated, and rewrites each source to a local path. Whether the target differs is not known then. Costs bandwidth, not correctness | `internal/agent/sources_test.go:TestFetchSourcesFile` |
| D-62 | No report is submitted when the run fails before applying: TLS bootstrap, the node request, the trusted-location checks, pluginsync, facts, the catalog request (including a compile failure, HTTP 4xx/5xx), or the puppet:/// source fetch; nor when the catalog is refused before the transaction starts: decoding, Deferred values, the extension host's load, pre-flight gaps, validation, graph building, or an undeterminable certname. Puppet submits a failed report for a compile failure. Once the transaction has started a report is always submitted, even when the run ends could-not-run: a transaction error sends it with status `failed` and `transaction_completed: false`, and a `--report-dir` write failure sends the finalized report | Before the transaction there is nothing to report on, so voxagent exits could-not-run and the operator reads stderr. After it the machine may have changed, so the server gets what the run knows | `cmd/voxagent/agent_test.go:TestAgentCompileErrorAppliesNothing`, `cmd/voxagent/agent_test.go:TestAgentReportDirFailureStillSendsReport`, `internal/report/report_test.go:TestFinalizeIncompleteFailsTheReport` |
| D-63 | HTTP redirects are refused; Puppet follows up to 10 | A redirect on a facts-carrying POST, or on a request made as root, would send the node's identity somewhere the server certificate was not checked for. The stock server does not redirect, so a deployment behind a redirecting load balancer fails closed | `internal/puppetapi/client_test.go:TestRedirectNotFollowed` |
| D-64 | Pluginsync fails closed: any metadata or content failure is could-not-run and no facter run follows; Puppet logs the error and continues with what it has | The agent runs as root and loads pluginsynced Ruby; continuing on a partial libdir would run code the server never intended to be the whole set | `internal/agent/pluginsync_test.go:TestPluginsyncContentErrorStopsBeforePurge` |
| D-65 | A 404 on a pluginsync mount (`plugins`, `pluginfacts`) is an error and purges nothing; whether a server ever answers an empty mount with 404 was not measured when this was written (`task dev:agent-gate` pluginsyncs both mounts against the stock server); if one does, every run fails | A transient 404 read as "empty mount" would purge the whole libdir | `internal/agent/pluginsync_test.go:TestPluginsyncMissingMountFailsClosed` |
| D-66 | A catalog whose environment differs from the one pluginsync used is refused; Puppet re-runs pluginsync for the new environment and recompiles | The libdir was filled for the first environment, and Ruby loaded from it would be the wrong code for the second. A node classifier that moves a node between environments therefore makes `voxagent agent` fail until the node request and `--environment` agree | `internal/agent/run_test.go:TestPrepareRefusesCatalogFromAnotherEnvironment` |
| D-67 | `Caching catalog for <certname>` is not logged, and no catalog is cached under `vardir/client_data`; Puppet writes both | Cached catalogs are out of scope: voxagent does not fall back to one when the server is down. The report lacks that one info line; the agent gate drops Ruby's (reportcmp `dropCachingCatalogLog`, Ruby side only) | `internal/gatekit/reportcmp/cachingcatalog_test.go` pins the drop's exactness; `dev:agent-gate` |
| D-68 | `--ca-fingerprint` is the SHA-256 of the DER of the self-signed root CA certificate (hex, colons optional). Puppet's `ca_fingerprint` digests the PEM of the CA bundle it downloaded. The value is checked on first fetch, on every load of `ca.pem` and on every refresh | The DER digest identifies the trust anchor itself and survives bundle re-ordering or an added intermediate. An operator copying a `ca_fingerprint` from Puppet sees a mismatch, so the flag's help says which digest it is | `internal/agent/ssl_test.go:TestBootstrapFingerprintPin` |
| D-69 | The CSR carries only the `pp_auth_auto_renew` attribute Puppet always sends; `csr_attributes.yaml` (`custom_attributes`, `extension_requests`) is not read | `puppet.conf` and its sibling files are not read. A server whose autosign policy executable relies on a challenge password or a `pp_*` extension will not sign a voxagent CSR | `internal/agent/ssl_test.go:TestSubmitCSR` pins the attribute that is sent; nothing pins the absence of the rest |
| D-70 | A `File` whose `links` is neither `manage` nor `follow`, and two `File`s sharing one `puppet:` source with different `links`, refuse the whole run before anything is applied; Puppet evaluates each File on its own | Sources are fetched once per URI at pre-flight with the metadata `links` setting the server resolves; two settings for one URI would need two fetches into one local path. Only the two values the provider implements are accepted | `internal/agent/sources_test.go:TestFetchSourcesFileLinksFollow` pins `follow`; nothing pins the two refusals |
| D-71 | A `source` list of several `puppet:` URIs fetches every element; Puppet asks the server for each in turn and stops at the first that exists | Pre-flight does not know which element would exist without asking, and asking is the fetch. A server error on a later element fails a run Puppet would have completed with the first | nothing pins it |
| D-72 | `--waitforcert` defaults to `0` (exit instead of polling); Puppet's default is `120` (2m) | The flag takes Puppet's duration syntax exactly (`DurationSetting::FORMAT`: digits with an optional `y`/`d`/`h`/`m`/`s` suffix, a bare number is seconds), so only the default differs. voxagent's one-shot run should not block by default | `cmd/voxagent/duration_test.go:TestPuppetDurationRejects`, `internal/agent/ssl_test.go:TestBootstrapWaitforcertPolls` |
| D-73 | A refreshed CA bundle that contains a second self-signed root is refused, and so is any change of the root. CA rotation is not supported; Puppet accepts the new bundle | A refresh is accepted only if it still contains the trusted root and every certificate in it is a CA. Rotating the CA means deleting `ca.pem` and `crl.pem` on the node and re-bootstrapping, which is first contact again, so use `--ca-fingerprint` | `internal/agent/ssl_test.go:TestBootstrapRefreshesStaleCA` pins an accepted refresh; nothing pins the second-root refusal |
| D-74 | The node certificate is never renewed. Puppet renews it when it is within `renewal_grace_period` of expiry | Renewal needs the `pp_auth_auto_renew` flow against the CA and the CSR attributes of D-69; it is not in the slice. When the certificate expires the run fails closed; delete `certs/<certname>.pem` and the key to re-enroll | nothing pins it |
| D-75 | An agent run's catalog gets none of Puppet's default resources. Ruby's agent finalizes the catalog it receives (`catalog.rb` `make_default_resources`), adding `Schedule[puppet]`, `[hourly]`, `[daily]`, `[weekly]`, `[monthly]`, `[never]` and `Filebucket[puppet]`; they apply nothing, but its `resources.total` counts them (7 more than voxagent's) and its time metric has `schedule` and `filebucket` entries. `puppet apply --catalog` skips the finalize, so the apply gates never see them | They exist for the `schedule` metaparameter, which voxagent refuses, and for `backup => 'puppet'`, which File's backup implements itself (`<vardir>/clientbucket`). Adding them would mean inert native Schedule and Filebucket types beside the extension host's and the pre-flight Filebucket refusal, to change a count | `tests/agent/run.sh` asserts Ruby's total minus 7 equals voxagent's; `tests/agent/allowlist.json` drops the total and the two time entries |
| D-76 | `voxagent agent` requests the node (and so the server's environment) on every run, so its report always has a `node_retrieval` timing. Ruby skips the request once `lastrunfile` exists (`use_last_environment`, `configurer.rb` `last_server_specified_environment`), so its second and later reports have none | voxagent keeps no `lastrunfile`; the node answer is always fresh, at the cost of one request a run | `internal/agent/run_test.go:TestPreparePhasesArePuppetsTimings`; `tests/agent/allowlist.json` drops the entry |
| D-77 | The agent's phase timings measure voxagent's nearest step, not Ruby's: `startup_time` runs from the Go process's initialisation (Ruby: option parsing) to the end of the SSL bootstrap; `config_retrieval` is the catalog request plus decoding; `convert_catalog` is decoding through the extension host's load, pre-flight and validation (Ruby: `to_ral` and `finalize`); fetching `puppet:` sources happens before applying, so it is in `total` but in no phase, where Ruby's lazy fetch is in `catalog_application` | The keys, labels and order of work are Puppet's; the values are durations of voxagent's own code, which the gates mask as run-to-run variance anyway | `internal/report/report_test.go:TestTimeMetricCarriesAgentPhases`; `cmd/voxagent/agent_test.go:TestAgentFirstRunAppliesAndReports` |
| D-78 | voxagent redacts a `Sensitive` value from error and warning text where Ruby prints it. Every place a provider interpolates a marked attribute prints a fixed placeholder (`[redacted]` for a `File` value) instead: `Exec`'s `[command redacted]` in the noop, bad-return, guard, lookup, not-qualified and run errors (Ruby's own text for a bad return only); `File`'s owner, group, mode, `ensure`, `target` and `source` errors, a recursive `source` walk's errors and soft-limit warning, and symlink errors; `Package`'s `ensure` value errors, including the dnf command line and its output; and the pre-flight refusal text; `Exec`'s `returns` value error and its invalid `logoutput` value error are redacted too. A `File` child a native or `eval_generate` generation makes from a marked parent `source` redacts the parent's `source` in error text only: the child's own events carry no marks | Ruby leaks the value in these messages (`Could not find user nosuchuser-secret`, `Could not find command '/nonexistent/cmd-secret'`, `The file mode specification is invalid: "bogus-secret"`, `'relative-secret' is not qualified and no path was specified`), so a report that matched Ruby leaf for leaf would carry a secret the property's own events hide. The redaction is voxagent's deliberate choice, and each such leaf is a divergence from Ruby's text. Ruby's leak is the divergence; voxagent does not copy it. Ruby cannot mark the plain parameters `environment` and `cwd`, but the catalog still lists them as marked, and voxagent redacts a marked `environment` where it interpolates the value: `Overriding environment setting '<var>' with [redacted]`, `Cannot understand environment setting [redacted]` and `Invalid environment setting [redacted]`, where Ruby prints it. This follows the plan's decision (its Review Focus 4) over the exec-complete spec §10, which says Ruby's `Overriding` text prints the value; the variable name in `Empty environment setting '<var>'` is still printed. No message voxagent writes interpolates `cwd`, so a marked `cwd` has nothing to redact: a `cwd` that cannot be entered fails the command with status 1 and no text. The same rule covers the `--debug` lines (an `Executing` line): a marked command or package version prints as `Executing '[redacted]'`, `Executing check '[redacted]'` or `Executing: '[redacted]'`, as Ruby does. D-78 covers only an *interpolated* value, so Ruby's fixed-text `Package` lines `Ensuring latest, so using <op>` and `Ensuring latest, but package is absent, so using install` still print when `ensure` is marked: they interpolate no value, and the `<op>` is dnf's subcommand, never the version. Since `exec` gained `user`, `group` and `refresh`, three more: an exit-127 failure, whose text is the command's output, reads `[output redacted]` when the command or guard that exited 127 is marked; a marked `user` or `group` reads `[redacted]` in the identity errors (`Error: Could not execute posix command: Invalid user: [redacted]`, where Ruby prints the name); and a marked `refresh` alone redacts the refresh command, in its `Executing` debug lines and in its `Could not find command [command redacted]` failure, though `command` is not marked. The `--debug` line `Executing with uid=<user> gid=<group>: '<cmd>'` (`util/execution.rb:167-180`) prints `[redacted]` for a marked `user` or `group`, where Ruby prints the value, and for a marked command as before | `internal/ral/preflight_test.go:TestPreflightRedactsAMarkedValue`; `internal/ral/exec_test.go:TestExecOnlyUnlessMarkedRedactsCommand`; `internal/ral/exec_run_test.go:TestExecExit127RedactedWhenMarked`; `internal/ral/exec_stage_test.go:TestExecUnknownSensitiveUserIsRedacted`, `TestExecUnknownSensitiveIdentityIsRedacted`; `internal/ral/exec_test.go:TestExecRefreshMarkedOnlyIsRedacted`, `TestExecDebugLineNamesIdentity`; `internal/ral/exec_env_test.go:TestExecEnvironmentMarkedIsRedactedInWarnings`; `internal/ral/exec_params_test.go:TestExecParamsRedactsSensitiveValues`; `cmd/voxagent/sensitive_test.go:TestNoSensitiveMarkerLeaks`, which fails if any marker value reaches the report, a log line or the console. The gates do not compare this text: the fixtures' failures that hit it are masked failure detail |
| D-79 | `Exec`'s `logoutput` decodes the command's output as US-ASCII under any locale codeset that is neither UTF-8 nor absent. Ruby uses `Encoding.default_external`, so under `LANG=en_US.ISO-8859-1` it would decode as ISO-8859-1. voxagent takes the first non-empty of `LC_ALL`, `LC_CTYPE`, `LANG`; a codeset of `UTF-8` or `utf8` (any case) is UTF-8, no codeset (including `C` and `POSIX`) is US-ASCII (measured as Ruby's), and every other codeset is treated as US-ASCII, so a byte above 0x7F in output that is to be logged fails the exec with `invalid byte sequence in US-ASCII` where Ruby would log it | Supporting every Ruby encoding would mean a table of Ruby's encoding names and their decoders for a host a fleet runs under `C.UTF-8` or `en_US.UTF-8`; the difference needs a legacy single-byte locale and non-ASCII output that is logged (`logoutput => true`, or a failing command under the `on_failure` default). Output that is not logged is never decoded. Ruby takes the encoding from `nl_langinfo(CODESET)` after `setlocale`, so two more cases differ: (a) an installed locale with no codeset (`LANG=en_US`) is ISO-8859-1 in Ruby but US-ASCII in voxagent; (b) a UTF-8 locale that is set but not installed falls back to US-ASCII in Ruby, while voxagent treats it as UTF-8 | `internal/ral/exec_output_test.go:TestOutputEncoding`; `internal/ral/exec_test.go:TestExecLogoutputEncoding`. The locale measurement covered `C` and `C.UTF-8` only, so the legacy-codeset behaviour of Ruby is from its documented rule, not a probe |
| D-80 | voxagent keeps at most 64 MiB (`maxCommandOutput`) of any one command's combined stdout and stderr. The kept part ends at its last `\n`, or, when it holds none, at its last complete UTF-8 sequence, so the cut never makes the output undecodable. voxagent then logs one warning at source `voxagent`, `<ref>: output of <what> truncated at 64 MiB` (`<what>` is `[command redacted]` for a `Sensitive` command, as in D-78). Truncation never fails the run and keeps the command's exit status. The cap covers every command voxagent runs, because all of them go through `runLogged`/`runArgv` in `internal/ral/fileoutput.go`: `Exec` commands and their `onlyif`/`unless` guards, `validate_cmd`, `dnf` and `rpm`, the SELinux helpers and `systemctl` | Ruby reads a command's output whole, so one command that floods its output grows the agent without bound. The cap is a constant, not a flag, and a command that prints exactly 64 MiB is not truncated (the reader takes one byte more to detect overflow). Output past the cap is lost for good, not spooled elsewhere: a `logoutput` or a failure message shows only the kept part | `internal/ral/fileoutput_test.go:TestRunCapturingToFileCaps`, `TestRunCapturingToFileKeepsExitStatusWhenTruncated`, `TestFileOutputRunnerWarnsOnTruncation`, `TestExecLogoutputTruncatedStaysDecodable`, `TestExecTruncationWarningRedactsASensitiveCommand`, `TestValidateCmdTruncationWarns`, `TestExecRunnerTruncatesAndWarns` (dnf, rpm and the SELinux helpers). No gate: no fixture prints 64 MiB |
| D-81 | voxagent stores at most 16 MiB (`reportDebugBudget`, `internal/runlog`) of debug-level message text in a run's report. The first debug line that would pass it is not recorded: voxagent records one warning at source `voxagent`, `debug log truncated in the report after 16 MiB; the console has the rest`, and from then on sends debug lines to the console only. Lines at info and above always reach the report, before and after the truncation, and the console prints every debug line whatever the budget. A line that fits the remaining budget exactly is recorded; one byte more is not. The budget is one per run: it is spent across the pre-report phases of `agent` and the transaction, because `Redirect` keeps the count | Ruby's report holds every log line, with no bound, so a `--debug` run on a large catalog grows the report as far as the catalog's own volume of lines. The report is held in memory until the run ends; a bound keeps a debug run's memory finite however much the providers log. The console is a stream, so it needs none. A truncated report is therefore not Ruby's report: it lacks debug lines the console shows. A run without `--debug` is unaffected, and the report-diff gates run without it | `internal/runlog/runlog_test.go:TestReportDebugBudget`, `TestBudgetExactFitIsRecorded`, `TestRedirectKeepsBudget`, `TestSmallDebugAfterTruncationIsNotRecorded`; `cmd/voxagent/sensitive_test.go:TestNoSensitiveMarkerLeaks` runs every case under `--debug`. The measured cost on the SIMP catalog is under "Debug logging's memory cost" in "The SIMP gate" |
| D-82 | Under `--debug` voxagent does not emit these of Ruby's debug lines, because it has no counterpart or does not do the act. *Never emitted:* state-cache load and store; environment caching and eviction; text domains; missing-feature probes; `Applying settings catalog`; report processors; `Runtime environment: ...`; `Finishing transaction <object_id>`. *Dropped by the graph and transaction task:* `Prefetching <provider> resources for <type>` for `file`, `exec` and `service` (Ruby's posix and systemd providers define no prefetch, so only `Package` announces it); `Nothing to manage: no ensure and the resource doesn't exist` for `exec`, `package` and `service` (Ruby's `resource_present?` is true for them). *Dropped by the File, Package and Service task:* `remove_existing`'s current-equals-wanted early return (callers reach it only for a different type); the File SELinux lines for `ensure => absent` (voxagent plans no SELinux properties there) and `Not managing symlink mode` after a `target` sync that left a link (voxagent drops `mode` from the params); the `Package` `install_only` variants (outside the bounded attributes); `systemd.rb:61` `Service <n> is in 'indirect' state and cannot be enabled/disabled` (outside the approved row set); `yum.rb:290` `Detected Arch argument in package! - Moving arch to end of version string` (voxagent does no arch detection). *Emitted with a different source:* `<cmd> check-update exited with 0; no package updates available.` has source `Puppet::Type::Package::ProviderDnf`, not `Package[n](provider=dnf)`, because Ruby's `yum.rb:114` `self.check_updates` is a class method and its `Puppet.debug` names the provider class | A debug line is emitted only where voxagent performs the same act, so a line for an act voxagent never does would claim work that did not happen. Each row above was checked against the Ruby source and the code path voxagent takes. `Debug:` output is therefore a subset of Ruby's, not a copy; a comparison of the two logs must expect these gaps. The generated-unit `will propagate` line was dropped by the graph and transaction task and is emitted since the recurse and generate units log their propagation lines, so it is not in this set | `internal/ral/package_test.go`, `internal/ral/file_test.go`, `internal/ral/service_test.go` and `cmd/voxagent/debuglog_test.go` pin the lines that are emitted, with their text and source; nothing pins an absence. The gates do not compare debug lines: the report-diff fixtures run without `--debug` |
| D-83 | On a `timeout`, voxagent SIGTERMs the exec's whole process group; Puppet SIGTERMs only the direct child, so under Puppet the descendants of a timed-out command keep running | Measured (M3): Puppet's timeout handler TERMs `child_pid` alone. voxagent starts every exec child as a group leader, and signalling the negated pid is what reaches a forked grandchild (dash forks for `sh -c`; see `runExec`). Neither escalates: the group gets SIGTERM once, `execute` returns `Command exceeded timeout` at once without waiting, and a process that traps or ignores TERM keeps running until it exits, when `runExec`'s waiter goroutine, which only calls `Wait`, reaps it (Ruby's `Thread.new { Process.waitpid(child_pid) }`, `util/execution.rb:283`). SIGKILL is sent only when the run's context is cancelled while `execute` is waiting: `runExec` selects on `ctx.Done()` beside the waiter and the timer, and arms nothing that outlives the call (no `context.AfterFunc`, no `exec.CommandContext`, whose `WaitDelay` would SIGKILL the leader), so the agent's own exit, which cancels the context, never signals a command that already timed out. Such a command can still write to its capture file, which `runCapped` has unlinked and no longer reads, as Puppet's survivor writes to its tempfile. Reports agree; the system state can differ | `internal/ral/exec_run_test.go:TestExecTimeoutKillsGrandchild`, `TestExecTimeoutTermTrapperSurvivesAndIsReaped`, `TestExecTimeoutChildSurvivesLaterCancel`. G5's `to-main`, `to-guard` and `to-refresh` compare the reports; they background no grandchild, because the difference is in the process table, not the report |
| D-84 | A whole-number Float in the catalog (`2.0`, `1.0`) is read as the Integer of the same value. `exec`'s `tries => 2.0` runs the command where Puppet fails `returns` with ``undefined method `times' for 2.0:Float``, and `try_sleep => 1.0` logs the debug line `Sleeping for 1 seconds between tries` where Puppet logs `1.0` | The compiled catalog keeps the Float (measured: `"tries":2.0,"try_sleep":1.0`), and Ruby parses it back as one; openvox 8.33.0 then fails `tries => 2.0` and prints `1.0` (measured, `puppet apply --debug`). `catalog.Decode` reads every JSON number as a `float64`, and `catalog.NormaliseNumbers` does the same for the extension host, so `2.0` and `2` are one value before any provider sees it. `tries` and `try_sleep` are the only attributes known to behave differently for the two. A non-whole Float (`2.5`, `0.5`) is unaffected | `internal/ral/exec_test.go:TestExecTrySleepIntegerShown` pins `1.0` printing `1`. G5's `tries-third` uses `try_sleep => 0.2`, which both print the same |
| D-85 | An `exec` whose `user` or `group` is unknown logs `Error: Could not execute posix command: Invalid user: <u>` (or `Invalid group: <g>`) as its output without colour; under Puppet that text follows Puppet's console settings | Puppet raises the error in the forked child, whose rescue logs it with `Puppet.log_exception` (`util/execution.rb:393-395`) through the agent's own log destinations into the captured output. On a coloured console, Puppet's default `color=ansi`, the report message is therefore `\e[1;31mError: … \e[0m`. The same mechanism as D-48(a), where `failure_detail` masks it; here the text is a plain `logoutput` line, so nothing masks it. The exec-complete measurements (M6, M16) were taken with `--color=false`, and voxagent never colours (`runlog.md`, "Console format"), so voxagent writes the `--color=false` text. G5 therefore runs Ruby with `--color=false`, which changes only child-logged output in a report | `internal/ral/exec_stage_test.go:TestExecUnknownIdentity` pins the exact text; G5's `as-unknown` fixture compares it against Ruby under `--color=false` (`tests/spine/report-diff-container.sh`) |
| D-86 | A numeric `user` or `group` string is read as a decimal id, and an id with no passwd or group entry is refused. `exec`'s `user => '0010'` is uid 10 where Puppet runs uid 8, `'0x10'` is a name where Puppet runs uid 16, and `'09'` is uid 9 where Puppet looks up a user named `09`. An unknown id, a digit string or an Integer, fails with `Invalid user: <id>` (or `Invalid group: <id>`); under Puppet an unknown Integer group runs with that gid, and an unknown Integer user fails with `can't find user for <id>` | Unmeasured; read from source. `get_posix_value` parses the string with `Kernel#Integer` (`util/posix.rb:174`), which honours the `0`, `0x` and `0b` prefixes and rejects `09`, and `convert_xid` returns an Integer unchanged without a lookup (`util/suidmanager.rb:142-143`), so only the later `Etc.getpwuid` for the user's groups fails. voxagent's `identityKey` (`internal/ral/exec_stage.go`) reads a digit string with `strconv.ParseUint(v, 10, 32)` and resolves every id through `os/user`, refusing one with no entry. M16 measured a digit string with no entry (`'12345'`, `'4242'`), which both read the same way | Nothing pins the decimal reading of a prefixed string. `internal/ral/exec_stage_test.go:TestExecUnknownIdentity` (`UnknownIntegerUser`) pins the refusal of an unknown Integer user as `Invalid user: 12345` |
| D-87 | `user` and `group` are native only on the platform they were measured on (RHEL family, major 10 or later, not Fedora, no libuser), and each is delegated to the extension host when a resource of it names another provider, or, for `user`, when any `purge_ssh_keys` would purge. Everywhere else voxagent delegates them, so with `--no-ruby` such a catalog is refused where Ruby applies it | Ruby's attribute set and commands depend on the host: its provider features (libuser's `forcelocal`, `manages_members`, libshadow) decide which attributes are managed and which commands run, and only this combination was measured (M1–M45). Delegating elsewhere is never a regression, because the host is Ruby itself. Purging generates `Ssh_authorized_key` resources, a type only the host applies (M32) | `internal/ral/usergroup_route_test.go` pins each condition, os-release missing and garbled included; `cmd/voxagent/usergroup_test.go:TestUserGroupAreNativeOnlyOnTheMeasuredPlatform` pins what `nativeProviders` claims. The extension gate's mixed-routing case (`tests/spine/report-diff-extension.sh`) applies a native `Group` beside a delegated `User` |
| D-88 | Under `--debug`, voxagent does not emit Ruby's provider-suitability lines for `useradd` and `groupadd`, nor `Puppet::Util::POSIX`'s fallback lines `Falling back to Puppet::Etc.group: can't find user for <u>` and `Removing any duplicate group entries` (`util/posix.rb:20,30`). A member of the D-82 family | voxagent performs neither act: it chooses no provider, and it reads a user's groups with `getgrouplist` or, for a user that does not exist, one group scan, without Ruby's exception-driven fallback. The scan's result is the same (M2, M43) | Nothing pins an absence. The gates do not compare debug lines |

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

Three more left with the `file` source round, each found by reading the Ruby a
new fixture was about to exercise, and each now compared by G1: a `file` with no
`ensure`, `content` or `source` created its path, where Puppet only reconciles
the permissions of an existing one (`type/file.rb:561-567`); a numeric `mode` on
a directory was applied without Puppet's dirmask (`type/file/mode.rb:111-121`);
and a resource skipped for a failed dependency logged voxagent's own sentence,
where Puppet notices `Dependency <ref> has failures: true` once per failed
resource and warns `Skipping because of failed dependencies` for each skipped
one (`transaction.rb:305-322`, `:418-424`), and names the skipped resource's
provider.

One more left it once the report contract was decided: a `content` change's
`desired_value`, and every `sel*` value libselinux supplies, came out of Ruby's
report Base64-encoded (`e3NoYTI1Nn03ODA1MWZh…` for `{sha256}78051faa…`,
`dG1wX3Q=` for `tmp_t`) where voxagent wrote the plain string. They are
ASCII-8BIT Strings, which `Event#to_data_hash`'s `ToStringifiedConverter`
Base64-encodes (`transaction/event.rb:89-105`,
`pops/serialization/to_stringified_converter.rb:97-103`). OpenVoxDB stores the
plain string ("What OpenVoxDB stores for a binary-encoded value" below), and
the user decided that is the contract, so voxagent was right and the gates
were comparing an artefact. The gates now capture Ruby's report as the
puppetdb terminus would send it ("How the gates capture Ruby's report"). G1
compares a real content change (`pur/keep.txt` is seeded with other content)
and the SELinux VM gate exits 0.

| Difference from Puppet | Where |
|---|---|
| `apply --help` exits 2 with `outcome=could-not-run`; `flag.ErrHelp` takes the parse-error path. `puppet apply --help` exits 0 | `cmd/voxagent/main.go` |
| The could-not-run code for a bad flag depends on argument order — `apply --bogus --detailed-exitcodes` exits 2, the reverse order exits 1 — because `*detailed` is read after a partial parse | `cmd/voxagent/main.go` |
| Global flags before the subcommand are rejected: `voxagent --detailed-exitcodes apply ...` is could-not-run | `cmd/voxagent/main.go` |
| Reports are written `0o644` and non-atomically. Event values embed managed file content, and Puppet writes `last_run_report.yaml` at `0640` | `cmd/voxagent/main.go` |
| No signal handling in `apply` (`agent` cancels its pre-apply steps on SIGINT/SIGTERM and holds the signal during apply). `tx.Run` gets `context.Background()`, so SIGINT mid-run kills the process with no report and no `outcome=` line, and a hung `exec` has no upper bound | `cmd/voxagent/main.go` |

## What OpenVoxDB stores for a binary-encoded value

Reproduction of the 2026-10-06 measurement that closed the `content`/`sel*` known defect, and the
justification for the coercion in "How the gates capture Ruby's report". Line numbers
for the terminus are from openvox-agent 8.29.0 and openvoxdb-termini 8.16.0 (the `fleet-node-el` image,
under `/opt/puppetlabs/puppet/lib/ruby/vendor_ruby/puppet/`); the `to_data_hash` converter lines are
from the Puppet 8.10.0 gem G1 captures with. They are different packages, so the numbers do not align.

1. `task fleet:up`. Then, in `fleet_node_el`, copy `/etc/puppetlabs/puppet/{puppet.conf,puppetdb.conf}`
   and the `fleet-ssl` certificates to a writable directory and point `puppet.conf` at the copies. The
   default confdir cannot be used: Puppet tries to chmod the read-only bind-mounted key and fails.
2. Seed `/tmp/m/f.txt` with `old content`, then apply, submitting through the real report processor:

   ```
   file { "/tmp/m/f.txt": ensure => file, content => "new content\n" }
   puppet apply --confdir <copy> --vardir /tmp/m/var --reports puppetdb a.pp
   ```
3. Query the stored events, authenticating with the node certificate:

   ```
   curl --cacert ca.crt --cert node-el.crt --key node-el.key -G https://openvoxdb:8081/pdb/query/v4/events \
        --data-urlencode 'query=["=","certname","node-el"]'
   ```
4. Observed: `old_value` `{sha256}40eda80e…`, `new_value` `{sha256}1c3ef9a7…`, plain. The same holds when
   a throwaway report processor (in `--libdir`, listed before `puppetdb` in `--reports`) forces every
   event's `desired_value` and `previous_value` to `.b` (ASCII-8BIT) first. In the same run, a second
   throwaway processor shows the untouched `desired_value` is ASCII-8BIT and that `to_data_hash` turns
   it into `e3NoYTI1Nn0xYzNl…`; the stored value is not that.
5. `task fleet:down`. If it cannot remove the containers after `podman exec` sessions
   (`openByHandleAt failed`), `podman pod rm -f pod_fleet` finishes the job.

Not measured: a real libselinux string, because no fleet node sees SELinux as enabled; the forced
ASCII-8BIT run reproduces the mechanism.

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

**Priority is the catalog index.** Relationships, stated and automatic, decide
only which resources are ready. Among the ready ones the walk always takes the
one earliest in the catalog's `resources` array: Puppet's
`SequentialPrioritizer` keys each vertex by that index
(`graph/sequential_prioritizer.rb:17-24`) and `relationship_graph.rb:97-99`
takes the smallest ready key, and `graph.Walker` does the same. So resources no
relationship orders apply in catalog order, which is the order the compiler
evaluated the manifest in. A resource the load-time `generate` pass adds takes
the place Puppet's `add_resource` gives it (the host's `order`,
`extension-host.md`), and the resources an `eval_generate` makes are applied at
their parent's turn. A Tier 3 resource whose type has no suitable provider is
**deferred**, not applied, when the walk reaches it: once the ready queue
drains it is offered again if anything was placed since the last drain, and
otherwise handed back overly deferred and failed (`relationship_graph.rb:114-145`,
D-37, retired; inside an `eval_generate` unit, D-56). D-44, retired, records
the measurements, and `gatekit compare-order` holds every report gate to them.

**Containers become whits**, as `replace_containers_with_anchors` makes them.
Every class, stage, node and defined-type instance is replaced by two vertices,
`Whit[admissible_<ref>]` and `Whit[completed_<ref>]`, both keyed at the
container's catalog index. Each containment edge `X -> v` becomes
`admissible_X -> v` and `v -> completed_X`, both refreshing; a relationship
`s -> X` lands on `admissible_X` and `X -> t` leaves from `completed_X`. So
`Class['a'] -> Class['b']` still means every resource in `a` precedes every
resource in `b`, and a class that is both `include`d and `contain`ed sits under
both of its containers. The whits are walked but never applied or reported: a
whit collapses the refresh events it was sent into one for each of its targets
(see "Refresh propagation"), or, behind a failed dependency, drops them
without a log line. They take their own turn in the ready queue, which
is why rewriting each edge member-to-member is not equivalent: a measured case
gave `xm r d`, where the splice gives `xm d r`. A cycle message
names a whit as its container (`Class[A]`), though not in Puppet's format
(I-29), and a resource's
`containment_path` follows its first containment edge, as Puppet's
pathbuilder does.

One consequence was a named change in behaviour. Autorelations are found
before the containers are rewired, on the catalog's refs, because
`populate_from` (`graph/relationship_graph.rb:24-33`) runs
`build_autorelation_dependencies` before `replace_containers_with_anchors`. So
a relationship *to a class* does not stop an autorequire between one of the
class's members and the resource that stated it. When voxagent spliced, it
saw the spliced member-to-member edges as stated, and quietly dropped that
autorequire. Example: `class app { file { '/opt/app/conf': } }` with
`file { '/opt/app': require => Class['app'] }`. The directory now autorequires
the file inside it, which closes a loop through the class. **Measured** with
Puppet 8.10.0 `puppet apply`, which refuses the catalog with `Found 1
dependency cycle: (File[/tmp/t8bapp/conf] => Class[App] => File[/tmp/t8bapp]
=> File[/tmp/t8bapp/conf])`. voxagent now refuses it the same way:
`dependency cycle: File[/opt/app/conf] -> Class[App] -> File[/opt/app] ->
File[/opt/app/conf]` (`TestAutoRequireIsNotSuppressedByARelationshipToTheContainer`).

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

A refresh through a container is **one event per whit**. Measured:
a whit processes its queue as any
resource does, `Whit#refresh` is a no-op that returns true, and
`process_events` then queues a single `restarted`. So `Class[A] ~> Exec[x]`
triggers `x` `from 1 event` however many of `A`'s members changed, a target
subscribed to two classes and one resource gets one event per class plus every
event of the resource, and a refresh *into* a class reaches each member
`from 1 event`, including the members of a class it `contain`s but not those of
one it only `include`s. Under `--noop` every whit with a queue also logs
`Would have triggered 'refresh' from N event(s)` at its container's name
(`Class[Allnoop]`, `Class[Main]`, `Stage[main]`), N being the raw count it
collected, before passing one `noop_restart` on; the admissible and the
completed whit of one class can both log it, with the same text. Whits
have no status, so `restarted` counts real resources only.

Only a target whose type defines `refresh` is sent anything
(`event_manager.rb:73`): a `File` or `Notify` is not (`ral.Refreshing`, D-41
retired). An `exec` whose `creates`, `onlyif` or `unless` blocks the command is,
and reports `Triggered 'refresh' from N event(s)` with a `refresh` event and
`restarted` without running it: `type/exec.rb:690-699` returns normally when a
check fails (measured on direct edges and
through a class).

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

`Change.DisplayVerbatim` makes the transaction use `WasDisplay` and `IsDisplay`
exactly as given, empty strings included and with no quotes added, in the
failure line (`change from <was> to <is> failed: …`) and nowhere else; the
provider then owns Ruby's quoting. `user` sets it on every change but `ensure`,
and `group` on `members` only (not `ensure` or `gid`). Their `is_to_s`/`should_to_s` print some values bare and some quoted, and a
group with no members fails as `change from  to nosuchuser failed: …` (M39,
two spaces), which no quoting rule applied by the transaction can produce.

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

A copy from a `source` takes the same temp-file path, streamed rather than held
in memory, and is checksummed as it is written; a checksum that differs from the
one taken when the source was resolved discards the temp file
(`type/file.rb:1076-1081`). Creating a file takes that path too whenever it has
`content` or a `source`, as Puppet writes any such file through `replace_file`
even when it is new (`type/file.rb:1104-1107`), so a failed or mismatched write
leaves the path as it began. Only a file with neither is written in place, under
`O_EXCL` (`:964-966`). A generated child's writes go through `os.Root`: the temp
file is `O_CREATE|O_EXCL|O_WRONLY` at 0600 on a random name, renamed with
`Root.Rename`.

A symlink at the path is never written through. Under `links => manage` it is
removed and the file created in its place; under `follow` the content is
compared against the link's target, and the write renames the temp file over
the link, leaving the target untouched. That rename copies the uid, gid and
mode of the path's `lstat` -- the link's own -- as `util.rb:623-626` does (I-5),
and `write`'s `property_fix` then applies any declared `mode`, `owner` and
`group` to the new file, unreported (`type/file.rb:970, 1112-1121`). Those
properties are reported against the target as it was before the write, which
is what Puppet retrieved. The only operations that go through a link are
`follow`'s `chmod` and `chown`.

Nothing at the path is read unless it is a regular file. `ensure => file`
(stated or inferred from `content`) replaces anything else, so a fifo or
socket in the way is unlinked and the file created, and a device fails with
`Could not remove files of type <ftype>` (`type/file.rb:814-818`, measured);
`ensure => present` keeps it and syncs no content (D-34). The one read, of an
existing file's content for comparison, opens with `O_NONBLOCK` and checks the
type of what it opened (`openSource`), so a file swapped for a fifo after the
stat fails the resource instead of blocking the agent. `fileSystem` has no
plain `Open` for that reason.

`backup` (`internal/ral/file_backup.go`) runs where Puppet's
`remove_existing` does (`type/file.rb:791-805`): before `ensure` replaces or
removes what is at the path, before a content write over an existing file,
and before `mklink`, never on a mode-only change or a new file, and never
under `--noop`. `true` is the suffix `.puppet-bak`; a suffix copies the path
to `<path><suffix>` as `FileUtils.cp_r(preserve: true)` does (a link at the
path is copied as what it reaches, a link below a directory as a link, mode,
owner and times kept). A stale copy is unlinked first, never written through,
and the copy is created afresh (`O_EXCL|O_NOFOLLOW`, `Mkdir`, `Symlink`).
The path and its copy are reached through the provider's `fileSystem`, so a
generated child's copy stays inside its `os.Root`; everything below them is
reached through an `os.Root` made from the directory descriptor already
opened and checked, never by a joined path, and each entry opened is checked
against its `lstat`, so a directory swapped for a link during the copy is
refused rather than read. Bytes and metadata come from the descriptor
opened. `backup =>
'puppet'` writes the local clientbucket under `--vardir`:
`clientbucket/<d1>/…/<d8>/<sha256>/{contents,paths}`, contents `0440`, paths
`0640` holding each original path's `realpath`, directories `0770`, the
clientbucket itself `0750`, all byte-identical to Ruby's (G1 diffs the two
trees). A directory is walked as the suffix copy walks it, through the
descriptor of each directory already opened and checked, and each file is
hashed and stored from one descriptor, whose `/proc/self/fd` link (checked
against an `lstat` of it) is the path `paths` records: bytes are always
recorded under the file they came from, so a later restore cannot write a
swapped-in file's bytes to the managed path. A link below the directory is
followed, as `File.file?` follows it, and recorded under its target's path:
what it reaches is first stat'ed through its directory's checked descriptor
(`/proc/self/fd/<fd>/<name>`), and anything but a regular file is skipped
unopened, as `File.file?` only stats; a regular file is then opened with
`openat` (`O_NONBLOCK|O_NOCTTY`) on that descriptor, which follows only the
link itself, and what opened is checked again for a regular file, so nothing
in a directory swapped in during the walk is ever read, link entries
included. A link retargeted at a device between that stat and the `openat`
still has the device opened before the second check refuses it; Puppet's
`File.file?` then `open` has the same gap, and the robust form is D-45(d).
For a generated child the link's target must lie inside the recursed tree
(D-47).
The top of a suffix copy's directory is made with `Mkdir` at `0700`,
`lstat`ed and opened without following; the opened directory must be the
one `lstat`ed (`SameFile`), owned by the agent's effective user and with no
group or other permission bits. That guarantees the directory written into
is the agent's own and private, not that it is the one `Mkdir` made: a
directory that fails the check -- swapped in before the `lstat` or after --
is neither written into nor removed, and the copy is refused; one that
passes it, an existing private directory of the agent's renamed into place,
is not told apart. Where the agent does not own a directory it has just
made, the check fails closed (D-45(e)). A copy that fails later is removed
by path (D-45(c)). Everything below the vardir is opened through `os.Root`, one directory
at a time, and a link anywhere there -- the clientbucket, a shard directory,
`paths` or `contents` -- fails the resource rather than being followed (Puppet
follows it). A failed backup fails the change it guards, unwrapped, and the
path is left as it was.

`ensure => link` (and `ensure => <target>`, and a `target` with no `ensure`)
is `internal/ral/file_link.go`, following `type/file/ensure.rb:100-122` and
`type/file/target.rb`. `linkTarget` is the munged target: an explicit `target`
wins, else a non-keyword `ensure` is the target, so `ensure => symlink` links
to the relative path `symlink` (I-2). A path that is not a link is cleared by
`removeExisting` twice, once by `ensure`'s sync and once by `mklink`, which is
why a directory kept for want of `force` is noticed twice (I-3); the first
removal happens even when there is no target to link to. `mklink` refuses with
Puppet's `Could not remove existing file` when anything is left, creates the
link, then applies `mode`, `owner` and `group` unreported against a fresh read,
as `property_fix` does: under `manage` the link keeps its mode and is
`lchown`ed, under `follow` its target is `chmod`ed and `chown`ed through it.
`target` is an ordinary property on any path that existed with `ensure` in
sync, whatever `ensure` is, so `ensure => file` with a `target` over an
existing file replaces the file with a link (I-6). Under `follow` `ensure`
reads the link's target, never a link, so `ensure => link` re-creates the
link on every run, as Puppet does.

## Staging cwd, umask and identity

`exec`'s `cwd`, `umask`, `user` and `group` are applied by the agent around
the child's start, not by Go's child (`staging.start`,
`internal/ral/exec_stage.go`). Ruby enters `cwd` as root and only then drops
privileges (M17), so a `user` command can start in a directory that user
cannot reach. Go's child applies `SysProcAttr.Credential` before `Dir`
(`syscall/exec_linux.go`), so `Dir` cannot reproduce that order. The agent
therefore changes its own, process-wide cwd and umask for the length of
`cmd.Start`. It runs as root while it does, so these eight rules are binding
(agent spec §5.1):

1. **One lock.** `stageMu` serialises every staged start, held from the
   `chdir` through `cmd.Start` returning to the restore. Nothing else runs
   inside it.
2. **Saved origin.** The first staged `cwd` opens the agent's own cwd (`"."`)
   with `O_PATH|O_DIRECTORY|O_CLOEXEC` and keeps the fd (`stageOrigin`) for
   the life of the process. The restore is `fchdir` on that fd, never a path,
   so a renamed or removed origin cannot misdirect it. `O_PATH` needs no read
   permission, so a non-root agent in a search-only directory can still save
   it. Failing to open it is a staging failure (rule 5), before anything
   changed.
3. **Enter as root.** `cwd` is entered with `chdir(2)` on the path, as Ruby's
   `Dir.chdir`: search permission only, symlinks followed (I-30). An
   `O_RDONLY` open would also demand read permission and refuse a search-only
   directory Ruby enters. A failure here is the old unenterable-cwd outcome,
   status 1 with nothing started, and the cwd is unchanged.
4. **umask.** Set with `syscall.Umask` immediately before `Start` and restored
   to the returned value immediately after, and only when `umask` is set.
5. **Restore or stop.** `umask(2)` cannot fail, but `fchdir` back to the
   origin can (a non-root agent whose origin lost its search permission
   mid-run). Then `execute` returns `ErrStagingBroken`, wrapping the cause,
   and the transaction applies nothing more: it logs the error and the run's
   status is `could_not_run`. Continuing in the wrong cwd is never an option.
   `restoreCwd` is a variable so a test can make it fail.
6. **The child does syscalls only.** `Dir` is `""`. `Credential` is applied
   by Go's raw-syscall child (`setgroups`, `setgid`, `setuid`), which runs no
   NSS lookup, no allocation and no Go scheduler; names are resolved in the
   agent first (`resolveIdentity`). Every fd the agent holds is close-on-exec,
   so the child inherits only stdin (`/dev/null`) and the capture file.
7. **No staging without need.** A command with no `cwd`, `umask`, `user` or
   `group` starts without the lock, as before. A command with only a `cwd`
   still stages, so a cwd failure has one path.
8. **Serial apply is pinned.** See below.

The `ENOEXEC` retry, which runs a file with no `#!` line through `/bin/sh`,
starts with the same staging.

**Why serial apply is required.** A cwd and a umask belong to the process,
not to a goroutine. While one is staged, any other goroutine that resolved a
relative path, created a file or started a child would do it in the exec's
`cwd` with the exec's umask, as root, and the lock would not stop it, because
only staged starts take the lock. Staging is safe only because voxagent
applies one resource at a time and nothing else runs during a start. The one
goroutine `internal/ral` and `internal/transaction` start is `runExec`'s
waiter, which calls nothing but `cmd.Wait`.
`internal/ral/goroutines_test.go:TestOnlyTheExecWaiterStartsAGoroutine`
parses every non-test `.go` file of both packages with `go/ast` and fails on
any other `go` statement. Elsewhere, `internal/extension`'s host starts a
goroutine per frame round trip and one at shutdown. They do only pipe I/O on
descriptors that are already open, and `cmd.Wait`, and the pin does not cover
them. A change that adds a goroutine able to touch the filesystem during an
apply, such as parallel apply or a background prefetch, must first move
staging out of the agent's process. A re-exec launcher is the known
alternative, rejected so far for its cost: more code running as root, and
about 1.2 ms per command.

Tests: `internal/ral/exec_stage_test.go` (`TestExecCwdEnteredBeforeDrop`,
`TestExecStagingRestoresAgentState`, `TestExecSearchOnlyCwdNonRoot`,
`TestExecNoStagingWithoutNeed`, `TestExecRestoreFailureIsStagingBroken`;
the root-only ones skip as non-root), and
`internal/transaction/transaction_test.go:TestRunStopsOnStagingBroken`. G5's
`private-cwd` fixture runs a `user => nobody` command in a root-owned `0700`
directory against Ruby.

## `gatekit`: the gates' parsing and judging

The gates are shell; anything they need parsed or judged is done by
`bin/gatekit`, a development binary that `task build` builds beside voxagent.
It replaced the Python the repository used to carry, and it reaches
the verdicts that Python did, on the inputs that Python saw.

| Subcommand | Used by | Package |
|---|---|---|
| `compare-reports RUBY VOXAGENT ALLOWLIST` | every report gate (`dev:report-diff`, `-extension`, `-container`, `-systemd`, `-selinux`, `-simp`) | `internal/gatekit/reportcmp` |
| `compare-order RUBY VOXAGENT` | every report gate (as compare-reports). Exit 0 when the walks agree; 1 for a divergence: a status only one report has, a walk-order mismatch, or a status both report that only one logged for; 2 when it cannot run: attribution failed or nothing was compared | `internal/gatekit/reportcmp` |
| `run-to-run RUN1 RUN2 ALLOWLIST` | `dev:derive-run-to-run`, `dev:report-diff-simp DERIVE=1` | `internal/gatekit/reportcmp` |
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
| `dev:report-diff` | voxagent's report matches Ruby's from identical catalog bytes, outside a measured allowlist. It also compares the order resources were evaluated in (`gatekit compare-order`, read from each report's log order), over every resource that logs; its `wo*` fixtures cover the walk-order cases, and it reports `walk order agrees over 112 resources`. For `backup` alone it also compares state: the suffix copy's content, mode and mtime, and every entry of each agent's clientbucket, byte for byte | Anything else about the disk. Two implementations can agree on a report while both are wrong about the filesystem |
| `dev:report-diff-extension` | The same differential for Tier 3 types run through the extension host — stdlib's `file_line` and `anchor`, `concat`, and the host's own test types, mixed with native `File`s in refresh and failure chains — from one pluginsync-style `libdir` both sides load. Generation is covered: `concat_file`'s load-time `generate` of its `File`, `concat`'s `eval_generate` overriding that `File`'s content from two fragments declared out of order, one of them `Sensitive` so Ruby's redacted content event and voxagent's must agree (D-8) (and its `ensure` to `absent` for a `concat` with no fragments), `vox_evalgen` making two resources and raising, `vox_gen`'s `generate` making a `Notify`, and a relationship through a Tier 3 name alias. Then G2: each side applies again on the target its own first run converged, the two second reports must agree too, and the resources that converge must have changed nothing in voxagent's second report. Last, `concat`'s default `backup => 'puppet'` must have left both agents' clientbuckets byte-identical. The comparisons report `1037` and `553 leaves compared, 0 divergent` (measured 2026-10-08), and the walk order agrees over `29` and `15` resources; the first includes `tidy` children, which `generate` places before their parent | That a second apply changes *nothing* everywhere: `Vox_prefetch[ok]`'s test provider never persists, so it changes on every run under Ruby as well; the converging resources are asserted by name instead. Anything under `--noop`: no noop pass runs, because a measured one agreed on every Tier 3 leaf but diverges on the fixture's native resources (D-10, D-40; D-41 was a third, now retired). `validate_cmd`, which forks and is G5's. The refusals of D-42, which no fixture reaches, and D-43's sentinel, which `concat` reaches without being able to observe it. The `evaluation_time` of a skipped resource: Ruby writes `null` and voxagent `0`, and `resource_statuses.*.evaluation_time` hides both. The order of the recursing `File[$T/d]` children: they change nothing, so they log nothing and have no position in the walk; a G1 fixture holds an `eval_generate` recursion's order |
| `dev:idempotence` | A second apply changes nothing | Anything about agreement with Ruby. A consistently wrong provider is perfectly idempotent |
| `dev:state-diff` | Both agents leave the same filesystem state, including `exec` and `owner`/`group`, and — since `package` shipped — the same installed package set, compared with `rpm -qa` | Anything outside the paths the fixture names, and anything about the report: `rpm -qa` cannot see an event name, a `desired_value`, or which property a failure was attributed to |
| `dev:derive-run-to-run` | `report-diff`'s allowlist still covers exactly what varies between two Ruby runs | Anything about voxagent. It measures Ruby against itself |
| `dev:report-diff-container` | voxagent's report matches Ruby's from identical catalog bytes for the two types the host cannot measure at all — `package` and `exec` — with both agents inside `fleet-node-el`, against a catalog the image's own Puppet compiled. It also holds `File`'s `validate_cmd`, which forks: a passing and a failing validator, one with no `validate_replacement` token, one that cannot be executed, and `visudo` on a good and a bad `sudoers` file. It also holds the `Sensitive` cases (a marked `File` content, mode and `ensure`, a marked `Exec` command and `unless`, a marked `Notify`) and `Exec`'s `logoutput` (`true`, `false`, `on_failure`, a marked command's `[output redacted]`, a failing command's output, a refresh). The comparison reports `1668 leaves compared, 0 divergent` (measured 2026-11-17, `WANT_PUPPET=8.33.0`); it was `815`, `314` before the five `exec` resources were added, `448` before `validate_cmd`, and `650` before the argv-array `exec` fixtures. The walk order agrees over `49` resources | Anything about the disk, exactly as G1 does not. Nothing under `--noop`: it applies a real run, so D-10 stays unmeasured. Nothing about the *text inside* a failure detail, which the comparator masks. And nothing about the *host*: the oracle inside the image is openvox, so a leaf where openvox and the gem differ — `package`'s install command line is one — is settled in openvox's favour by this gate and would diverge against the gem by design |
| `dev:report-diff-systemd` | voxagent's report for `service` matches Ruby's from identical catalog bytes, with each agent in its own container booted with systemd as PID 1, and both leave every fixture unit in the same `is-active`/`is-enabled` state. The comparison reports `315 leaves compared, 0 divergent`, and the walk order agrees over `8` resources | Anything under `--noop` (D-10). A failing start: its journal text varies between any two containers, so the fixture has none and `TestServiceStartFailureCarriesTheJournal` pins it. The built-in commands' locale, which no report shows. And any init system other than systemd, which voxagent refuses (D-22) |
| `dev:report-diff-simp` | voxagent applies the public SIMP catalog (737 resources, 63 `Sysctl`, 16 `Binary` values, compiled in the container it is applied to) as Ruby does, each agent in its own systemd-booted container: the reports agree outside the allowlist (`12782 leaves compared, 0 divergent`), the walk order agrees over `340` resources, and both leave the same `tmp.mount` enablement, `/etc/audit` mode and reboot notifications | Anything about auditd's daemon or about polkit's start path: the image stubs the one and boots with the other running ("The SIMP gate"). `haveged`, which is in EPEL and fails to install on both sides, so only its failure attribution is compared. Any catalog but this one, and any SIMP release but the pinned one |
| `dev:agent-gate` | `voxagent agent` enrols against a real OpenVox server (CA, autosign), refuses a wrong `--ca-fingerprint`, pluginsyncs, applies, and PUTs a report whose run-2 form matches `puppet agent -t`'s from the same server under `gatekit compare-reports` (the spine allowlist merged with `tests/agent/allowlist.json`); run 2 sends no certificate request; with the server stopped the agent exits could-not-run. Not part of `task check` | Anything about catalogs beyond the fixture's; the first-contact TOFU window (it is accepted by design); CA rotation, renewal, or a server that answers differently from the pinned OpenVox image. See `agent-transport.md` |
| `gatekit compare-order`, inside every report gate (`dev:report-diff`, `-extension`, `-container`, `-systemd`, `-selinux`, `-simp`) | Each resource that logs is evaluated in the same order relative to the others as in Ruby's run: each report's resource statuses, ordered by their first log line (a log belongs to the status whose containment path is the longest prefix of its source), compared over the resources both logged for. A status only one report has diverges, and so does a status both report that only one logged for. A comparison that cannot attribute a log (the side without it has a log source no status owns that the other side does not also leave unowned, as both leave `Puppet`), or that compares nothing, cannot run (exit 2), so it never agrees vacuously. Counts are in each gate's row and in D-44 | The position of a resource that logs nothing: one that changes nothing has no place in either order, so most of an idempotent run is not compared. Whits, which have no status. Anything between two log lines of one resource |

Each has been seen to go red on a deliberate break and back to green, except
`dev:report-diff-simp`, which has gone red only on a real divergence, D-8's
six `File[/etc/chrony.keys]` leaves before voxagent redacted them ("The SIMP
gate"); no deliberate break has been run against it. A gate that has never
failed is not known to be able to. G5's `package` break was
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

The walk-order comparison, `gatekit compare-order`, went red the same way.
Run against a `bin/voxagent` that still took
ready resources in byte order of their refs, G1 exits 1 with `walk order
diverges at 0: ruby Notify[spine], voxagent File[$T/a.txt]`; restricted to
the `wo*` walk-order fixtures alone it is `walk order diverges at 0: ruby
Notify[wo1_zeta], voxagent File[$T/wo1]`.

`dev:report-diff`'s fixture now contains a **failing** resource — a file whose
parent directory does not exist — so the differential reaches the failure
attribution rather than only the success path. It reported `181 leaves compared,
0 divergent` once that resource was added, against `149` before (it reports
`3672` now, measured 2026-10-08). A gate that
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

### How the gates capture Ruby's report

Every gate that compares against Ruby (`dev:report-diff`, `dev:report-diff-extension`,
`dev:derive-run-to-run`, `dev:report-diff-container`, `dev:report-diff-systemd`,
`dev:report-diff-selinux`)
captures Ruby's report through one committed report processor,
`tests/spine/lib/puppet/reports/gatejson.rb`. Each runs `puppet apply ...
--libdir <lib> --reports gatejson --reportdir <dir>` and compares
`<dir>/gate.json`; the container and VM gates copy `lib` in as they copy the
catalog. `store` is no longer requested, because nothing read its YAML except
the conversion this replaces.

The JSON is the report's `to_data_hash`, as before, with one **normalisation**
in front of it: every String the puppetdb terminus sends is first coerced to
UTF-8 with the terminus's own `coerce_to_utf8`, copied verbatim
(openvoxdb-termini 8.16.0, `puppet/util/puppetdb/char_encoding.rb:13,66-82`).
The scope is what `reports/puppetdb.rb` puts in the payload (`:36-79` the
report, `:109-123` logs, `:128-138` metrics, `:165-177` events, `:191-217`
resource statuses), which `util/puppetdb/command.rb:20-57,76` runs through
`coerce_payload`; strings it never sends, such as a status's `tags` or an
event's `historical_value`, are untouched. A coerced String is UTF-8, so
`Event#to_data_hash` no longer Base64-encodes an ASCII-8BIT event value.

- **Why.** OpenVoxDB stores those values plain. That was measured
  ("What OpenVoxDB stores for a binary-encoded value"), and the user made what
  OpenVoxDB stores the report contract.
- **Why a processor, not a conversion of the stored YAML.** The coercion
  cannot be applied afterwards. `store` writes `to_yaml` (`reports/store.rb:38`),
  and the report's classes serialise through `to_data_hash`
  (`util/psych_support.rb:28-31`). The stored file therefore already holds a bare
  Base64 string, which reads back as UTF-8 with no type marker. Decoding it, in
  Ruby or in the comparator, could not tell an encoded value from one that
  really is that text, and would hide a real divergence. The terminus is itself
  a report processor, so `gatejson` sees the live report exactly where the
  terminus does.
- **Proven not to change anything else.** One G1 run applied with
  `--reports store,gatejson`. `store` ran first, so its YAML was written before
  the coercion. That YAML, converted the old way, was identical to `gate.json`
  (`jq -S` then `diff`, empty, over 50 events with no ASCII-8BIT value).
  `dev:derive-run-to-run` reports the same 14 patterns, 21 differing leaves,
  through the processor. Removing `:@desired_value` from the coercion turns G1
  red again on `pur/keep.txt`'s Base64 `desired_value`. Making voxagent emit a
  wrong checksum turns it red on that leaf with the plain value on both sides.
- **Limits.** A String that is not valid UTF-8 is changed by PuppetDB too: the
  terminus re-tags it US-ASCII and replaces every non-ASCII byte with U+FFFD.
  The gate then compares PuppetDB's form, not Ruby's bytes. An event value that
  is not a String (an Array, a Hash, a Symbol) is left as `to_data_hash` writes
  it. The terminus sends `value.to_s` instead, a different shape that the gates
  do not reproduce. The terminus drops unchanged resource statuses unless
  `include_unchanged_resources` is set, but `gatejson` coerces every status.
  That makes no difference to a status whose strings are already UTF-8.

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

`metrics.time.values.service.value` has the same standing. The host's PID 1 is
not systemd, so Puppet there would not select the systemd provider and the host
fixture cannot carry a `service`. Its provenance is `report-diff-systemd.sh`'s
catalog applied twice by Ruby in two fresh systemd-booted containers:
`1.044489995` against `1.144156349` seconds, with `gatekit run-to-run` naming it
the only uncovered differing leaf apart from journal text inside a failure.

The four Tier 3 timings — `anchor`, `file_line`, `vox_prefetch` and
`vox_value` — are UNUSED on the host fixture for the plainer reason that it
declares none of those types. Their provenance is `report-diff-extension.sh`'s
catalog applied twice by Ruby from an identical starting state, with
`gatekit run-to-run` reporting each `1 differing, 0 stable`.

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
still holds at the new version across all four compared paths. It happened again
on 2026-11-17 (8.29.0 to 8.33.0): `dev:state-diff`, `dev:report-diff-container`,
`dev:report-diff-systemd` and `dev:report-diff-simp` were run with
`WANT_PUPPET=8.33.0` and all agreed (the same state across 4 paths and 204
packages; 1668, 315 and 12782 leaves, 0 divergent) before the four pins moved
together. The SELinux gate's VM installs its own pinned RPM, not this image, and
stays at 8.29.0.

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

### The systemd gate

`dev:report-diff-systemd` (`tests/spine/report-diff-systemd.sh`) is G5 for
`service`. Neither the host nor `report-diff-container.sh` can be its oracle:
Puppet selects the systemd provider only when systemd is PID 1, and voxagent
refuses a catalog holding a `service` otherwise (D-22). So the gate builds
`localhost/voxagent-systemd:latest` from `tests/spine/images/Containerfile.systemd`.
That image is the G5 base with `/sbin/init` as its command and nothing added,
because systemd 257 is already installed. The base is never rebuilt, so the
openvox pin (`WANT_PUPPET`) is the same tripwire G5 has.

It uses two containers, one per agent, for the reason G5 does: a service
apply changes the machine, so a shared container would turn voxagent's run into
a no-op. Each side is booted, the seven fixture units are installed, `voxb` and
`voxd` are started, and every premise is asserted before anything is applied.
That means `is-active` and `is-enabled` for each unit, and `LoadState=not-found`
for `voxagent-no-such-unit`. A drifted premise exits 2, naming itself. The
Ruby side compiles the catalog and both agents apply the same bytes.

| Resource | Exercises |
|---|---|
| `voxa` | start with `enable` synced inside it: one `service_started` event, no `enable` event, unit left enabled |
| `voxb` | stop |
| `voxc` | `enable` alone: its own `service_enabled` event |
| `voxd` | refresh of a running unit from a changed `file`: a restart |
| `voxe` | refresh of a stopped unit with no `ensure`. Measured: Puppet still records `Triggered 'refresh' from 1 event` and counts it in `resources.restarted`, but restarts nothing, so the state comparison is what checks the skip |
| `voxf` | start plus a pending refresh in one run: the start invalidates the refresh, so there is one event and no restart |
| `voxg` | a custom `start`, run through `/bin/sh -c` |
| `voxagent-no-such-unit` | `enable => false` on a unit that does not exist: in sync, so it has no resource status (Puppet 8 excludes unchanged resources from the report) |

Before trusting the comparison, the gate checks that Ruby's report contains
`"provider_used":"systemd"`, `service_started`, `service_stopped`,
`service_enabled` and `refresh`. If any is missing, the gate exits 2: the fixture
did not reach the provider, and two agents agreeing about nothing would pass.
After `gatekit compare-reports` and `gatekit compare-order`, the gate compares
`systemctl is-active` and `is-enabled` for every fixture unit between the two
sides. A walk-order or state difference exits 1, as a report divergence does,
and keeps the same evidence in `.test-output/spine/systemd-*`.

**A failing start is deliberately not in the fixture.** The brief had one, as a
`voxfail` unit with `ExecStart=/bin/false`, and the first live run diverged only
in its journal text. Puppet logs the failure twice: once as
`change from 'stopped' to 'running' failed: <detail>`, which the comparator's
`failure_detail` mask reaches, and once bare under source `Puppet`, which it
does not. The bare log differed only in the journal's timestamps:
`Oct 05 19:25:33` on the Ruby side, `19:25:38` on voxagent's. Two Ruby runs in
two containers differ in the same way (`19:26:26` against `19:26:32`), so the
difference says nothing about either agent. `voxfail` was dropped rather than
allowlisted. `TestServiceStartFailureCarriesTheJournal` pins the message instead.

The gate has been seen to go red. With `InvalidateRefreshes` disabled in
`internal/ral/service.go` it reported `315 leaves compared, 17 divergent`, all
naming `Service[voxf]`'s extra `refresh` event and the metrics it moved.

#### Running the systemd gate

Rootless systemd needs a cgroup subtree it can write. Inside a nested container
(this project's development environment) it has none, and the boot fails with
`Failed to create /init.scope control group: Permission denied`. The gate then
exits 2 and points here. Delegate a subtree once per container boot, as root.
Kernel threads in the root cgroup cannot move, so they are expected to refuse,
and the scoped `|| true` exists for them.

**Only run this inside a container whose PID 1 is not systemd.** It moves every
process in the root cgroup into a new child and rewrites the root's
`cgroup.subtree_control`. On a host, or in a container, where systemd is PID 1,
that is systemd's tree to manage, and doing this behind its back breaks it. The
snippet refuses when `/proc/1/comm` is `systemd`:

```bash
sudo sh -c '
  set -e
  if [ "$(cat /proc/1/comm)" = systemd ]; then
    echo "PID 1 is systemd: it owns the cgroup tree; do not run this here" >&2
    exit 1
  fi
  cd /sys/fs/cgroup
  mkdir -p init
  for p in $(cat cgroup.procs); do echo "$p" > init/cgroup.procs 2>/dev/null || true; done
  echo "+cpu +memory +pids" > cgroup.subtree_control
  mkdir -p llm && chown -R llm:llm llm
  echo "+cpu +memory +pids" > llm/cgroup.subtree_control
'
```

Replace `llm` with your user. Then run the gate from a shell placed in the
delegated subtree. Every podman call inherits the cgroup of the shell that made
it, so the move has to happen in the same shell that runs `task`:

```bash
mkdir -p /sys/fs/cgroup/llm/gate
bash -c 'sudo sh -c "echo $$ > /sys/fs/cgroup/llm/gate/cgroup.procs" && task dev:report-diff-systemd'
```

The expected last line is `report-diff-systemd: reports agree outside the
allowlist`, exit 0. On a host where systemd already delegates the user's
cgroup subtree, as a normal systemd login session does, no setup should be
needed. That has not been measured here.

One more thing was measured while getting it to run. Asked straight after
`podman run`, `systemctl is-system-running --wait` fails with `Failed to connect
to system scope bus` and prints nothing, so the gate retries while it prints
nothing. One wall-clock deadline, `BOOT_TIMEOUT` (180 seconds), bounds the
whole wait for each container: every attempt is given only the time left, and
an attempt that times out ends the wait with exit 2.

### The SIMP gate

`dev:report-diff-simp` (`tests/spine/report-diff-simp.sh`) is the extension
host's acceptance gate: the public SIMP catalog, applied for real by both agents
and compared by report, walk order and end state. The script header covers how
it is built. That means the pinned forge releases in `tests/spine/simp/modules.pin`, the
fixture beside them, the compile in a booted container of the same image, the
three premises, and one fresh container per agent.
It needs the delegated cgroup the systemd gate needs ("Running the systemd
gate"), run the same way with `task dev:report-diff-simp`. It needs network
only on a cold run. The gate builds `localhost/voxagent-simp:latest` only when
it is absent, so after editing `tests/spine/images/Containerfile.simp`, remove
the image (`podman rmi localhost/voxagent-simp:latest`) or the next run measures
the old one.

The expected last lines are `12782 leaves compared, 0 divergent`, `walk order
agrees over 340 resources` and `report-diff-simp: reports agree outside the
allowlist`, exit 0. The gate run that first gave those lines, on
2026-10-08, gave Ruby 30.7 s and voxagent 27.7 s on its `applied:` line, each
the wall time around one agent's command inside its container; the whole warm run
takes about a minute. One run is not a benchmark: the five-run medians, 28.68 s
and 25.10 s, are in `docs/maintainers/performance.md`, "Public SIMP catalog
applied".

**Every exception is measured.**

- **Run to run.** The eighteen `metrics.time.values.<type>.value` entries for
  the SIMP types (augeas through user) come from `DERIVE=1 bash
  tests/spine/report-diff-simp.sh`: two Ruby runs, every measured pair in
  `tests/spine/allowlist.json`'s `_comment`. `tidy.value` was already there.
- **Registered, not compared.** None for `Sensitive` any more. `File[/etc/chrony.keys]`'s
  content is `Sensitive`; before D-8 was resolved Ruby wrote `[redacted]` and
  `redacted: true` where voxagent wrote the digest and `null`, and the gate
  reported `12782 leaves compared, 6 divergent`, exactly the four event leaves and
  the ensure log (a `logs (source: …)` key) that were then registered. Those
  entries are removed and the leaves are compared: `12782 leaves compared, 0
  divergent`. That earlier red was the gate's evidence that it can go red. It
  was a real divergence rather than a deliberate break.

**`DERIVE=1` exits 1, and that is expected.** Since run-to-run derives over the
same normalisations as compare-reports, no differing leaf is left uncovered.
Before that change, `haveged`'s dnf mirror speeds inside its failure detail
were three such leaves. But it reports the shared
`resource_statuses.*.evaluation_time` pattern OVER-BROAD, with 8 stable leaves.
Each is the `null` evaluation time of a resource both runs skipped:
`Exec[dracut_rebuild]`, `Exec[remount /var/tmp]`,
`Exec[systemd-haveged.service-systemctl-daemon-reload]`,
`File[/etc/systemd/system/haveged.service.d]`,
`File[/etc/systemd/system/haveged.service.d/haveged_settings.conf]`,
`Reboot_notify[audit:all]`, `Reboot_notify[fips]` and `Service[haveged]`.
Every other gate shares the pattern, so narrowing it is its own change: a
skipped resource's `null` would have to be told apart from a timed one's float. Until then, on this catalog, a
skipped resource's evaluation time is not compared.

**auditd and polkit are prepared in the image, identically for both agents.**
Unprepared, both fail to start in a rootless container, for each agent. Puppet's
failure log then carries a `journalctl` tail whose timestamps, PIDs and line
window differ between any two containers, Ruby against Ruby included, so no
mask of the timestamps alone would make them agree. `Containerfile.simp` adds
two drop-ins named `voxagent-gate.conf` and disables auditd. None of it changes
what the catalog asks:

| Unit | Why it fails | What the image changes | Starting state | What is compared |
|---|---|---|---|---|
| `auditd` | Kernel audit belongs to the initial user namespace: `4/NOPERMISSION` | `ExecStart=/usr/bin/true`, `Type=oneshot`, `RemainAfterExit=yes`, `PIDFile=` cleared; `systemctl disable auditd` | installed, disabled, stopped | The catalog's `ensure => running` start, `ensure changed 'stopped' to 'running'`, in both reports. No `enable` event: Puppet syncs only `ensure` when it is out of sync |
| `polkit` | `User=` plus any seccomp-backed setting needs `CAP_SYS_ADMIN`, which rootless podman does not grant: step USER, "Failed to keep CAP_SYS_ADMIN" | Fourteen settings reset (`SystemCallFilter`, `SystemCallArchitectures`, `RestrictAddressFamilies`, `RestrictNamespaces`, `RestrictRealtime`, `RestrictSUIDSGID`, `LockPersonality`, `MemoryDenyWriteExecute`, `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectClock`, `ProtectHostname`); each one alone still fails | installed, static, running | That both agents find it in sync: with Puppet 8's `exclude_unchanged_resources` neither report has a status for it |

polkit cannot start out stopped. It is a static unit, so `systemctl disable` does
nothing, and tuned asks for `org.freedesktop.PolicyKit1` at boot, so D-Bus
activation starts it before the agent runs. Measured: with polkit stopped,
restarting tuned starts it again, and restarting `sshd` or `chronyd` does not.
Stopping it would mean changing tuned, which is itself a resource of the catalog.
The catalog also refreshes `Service[tuned]` during the run, which would
re-activate a stopped polkit mid-run. So neither agent's start path for polkit
is exercised here.

### Debug logging's memory cost

Measured 2026-11-18, one run each, on the SIMP catalog `tests/spine/report-diff-simp.sh`
compiles (737 resources), applied live by `voxagent apply --catalog <catalog>
--report-dir <dir>` in a freshly booted systemd container of
`localhost/voxagent-simp` (one container per run, so neither run sees the
other's changes), under GNU `/usr/bin/time -v`, peak RSS from
`Maximum resident set size`. The catalog came from the gate's own compile step
(the script's code, stopped before its apply step), so it is the catalog the
gate measures. This is a record, not a gate: one sample each, on a shared host.

| Run | Peak RSS | Wall clock | Stdout | Report |
|---|---|---|---|---|
| default | 108344 kB (105.8 MiB) | 23.31 s | 50096 bytes | 677891 bytes |
| `--debug` | 113964 kB (111.3 MiB) | 21.50 s | 435239 bytes | 1461811 bytes |

`--debug` cost about 5.5 MiB (5.2%) more peak RSS on this catalog, and its
report is 2.2 times the default's. The debug text here is far below the 16 MiB
report budget (D-81), so the budget did not engage. Both runs exited 0. The wall
clocks differ the other way from what logging would cause, which shows the noise
of one sample: do not read a speed claim from them.

### The SELinux gate

`dev:report-diff-selinux` (`tests/spine/report-diff-selinux.sh`) compares the
two agents on `file`'s SELinux contexts with SELinux **enforcing**. No other gate
can: the host and every container here see SELinux as disabled, and then both
agents make every `sel*` property a no-op, so G1 compares nothing about labels.

There is no `/dev/kvm` here and RHEL's `qemu-kvm` is built without TCG, so the
gate emulates a VM with Fedora's QEMU (`-accel tcg`), from the image
`tests/spine/selinux-vm/Containerfile` builds (Fedora 44, pinned by digest). The
guest is the CentOS Stream 10 GenericCloud image
`CentOS-Stream-GenericCloud-10-20260930.0.x86_64.qcow2`, pinned by the SHA256
CentOS publishes and verified on every run. It is downloaded once to
`${XDG_CACHE_HOME:-~/.cache}/voxagent/selinux-vm` (override with
`SELINUX_VM_CACHE`), outside the tree because it is 1 GiB; each run writes only
a copy-on-write overlay in its own `mktemp -d`. cloud-init
(`selinux-vm/user-data`) installs openvox 8.29.0 (the release RPM pinned by
SHA256, the agent by version), `libselinux-utils` and `dosfstools`, and
authorizes an SSH key generated for that run alone. The gate reaches the guest
by `podman exec … ssh` into QEMU's user-mode network, so nothing is published on
the host. The image ships enforcing; the gate asserts `getenforce` is
`Enforcing` and the agent is 8.29.0, and exits 2 otherwise.

One VM serves both agents, as G1's one directory does. Before each side,
`reset.sh` rebuilds every fixture path and sets every seeded label with
`chcon`, and the labels it left are read and compared between the sides; a
seed that differs exits 2. The catalog is compiled on the host by Puppet
8.10.0, exactly as G1 compiles its own, and the same bytes go to `puppet apply
--catalog` (openvox) and `voxagent apply`. Ruby's report must carry
`seltype_changed`, `seluser_changed`, `Failed to set SELinux context`,
`owner_changed` and `mode_changed`, or the fixture did not reach the enabled
path and the gate exits 2. After `gatekit compare-reports` and `gatekit
compare-order`, the mode, owner, group and label every fixture path ends with
(`find -printf '%p %y %m %u:%g %Z'`, never following a link) are compared
between the sides; a walk-order or label difference exits 1. Every run that
gets that far keeps the reports, the catalog and the label listings in
`.test-output/spine/selinux-*`.

What the fixture measured on 2026-10-06 (openvox 8.29.0, libselinux 3.11-3.el10,
selinux-policy-targeted 42.1.27-2.el10), the same on both sides unless said:

| Resource | Seeded | Result |
|---|---|---|
| `/etc/voxgate/mis`, `ensure => file` | `unconfined_u:object_r:tmp_t:s0` | two events, `seluser changed 'unconfined_u' to 'system_u'` and `seltype changed 'tmp_t' to 'etc_t'` |
| `typed`, `seltype => httpd_sys_content_t` | `etc_t` | `seltype_changed` to the declared type |
| `ign`, `selinux_ignore_defaults => true` | `tmp_t` | nothing |
| `ign-typed`, the same with a declared `seltype` | `tmp_t` | only `seltype` |
| `new`, created by `content` | absent | `etc_t`, silently: the only event is `ensure` |
| `suid`, created with `owner => 'nobody'`, `mode => '4755'` | absent | the only event is `ensure`; the chown clears `S_ISUID` and `property_fix`'s mode puts it back, so it ends `4755 nobody:root` |
| `rewrite`, mislabelled and rewritten by `content` | `tmp_t` | a `content` event **and** a `seltype_changed` event, and `etc_t` |
| `dir`, `ensure => directory`; `newdir`, created | `tmp_t`; absent | one `seltype_changed`; `etc_t` silently |
| `lnm`, `ensure => link` over a link to `lnreal` | both `tmp_t` | the link is relabelled `etc_t`; `lnreal` keeps `tmp_t` |
| `lntyped`, `seltype` only, on a link (`links => manage`) | `etc_t`, target `tmp_t` | the link takes `httpd_sys_content_t`; the target is untouched |
| `ln-new`, a created link | absent | `etc_t`, silently |
| `badtype`, `seltype => voxagent_no_such_t` | `etc_t` | warning `Failed to set SELinux context system_u:object_r:voxagent_no_such_t:s0 on /etc/voxgate/badtype`, and the event's status is `success`; the label stays `etc_t` (I-4) |
| `tree`, `recurse => true`, `owner` and `group` `nobody`, `mode => '0640'` | children `tmp_t`, all `root:root`, files 0600, directories 0700 | `owner`, `group` and `mode` events, in that order (`type/file.rb:1133-1135`), on `tree` and each file and directory child, and one `seltype_changed` to `etc_t` on each child, through the batched lookup |
| `tree/ln`, a symlink child to `tree/a` | `tmp_t`, `root:root` | the link itself is relabelled `etc_t` and `lchown`ed to `nobody:nobody`, with no `mode` event (D-28, retired) |
| `tree/lnout`, a symlink child to `lnreal`, outside the tree | `etc_t`, `root:root` | `owner` and `group` events only; `lnreal` stays `root:root` |
| `/opt/voxgate/etc`, `content` only, absent | absent | `etc_t`: looked up with mode 0, which matches `/opt/(.*/)?etc -d` (I-11) |
| `/opt/voxgate/file/etc`, the same with `ensure => file` | absent | `usr_t` |
| `/mnt/voxlnk`, `ensure => file` over a symlink | link `mnt_t` | the new regular file is `mnt_t`, the link's default, where a file there is `default_t` (I-10) |
| `/mnt/voxvfat/new` and `old`, `seltype => etc_t` on a vfat loop mount | `dosfs_t` | no `sel*` event; both stay `dosfs_t` |

**The gate exits 0: 915 leaves compared, 0 divergent** (2026-10-06, after
the capture change). Before it, the gate exited 1 on one difference, Base64.
The first full run compared 647 leaves and found 21 divergent. With `tree/ln`
relabelled (D-28, retired) and ownership declared on `tree`, the run compared
915 leaves and found 23. Each was a value Ruby reported Base64-encoded where
voxagent reports it plain (`previous_value` `dG1wX3Q=` against `tmp_t`); every
`owner`, `group` and `mode` leaf already agreed, in Puppet's order. The encoded
values were every `previous_value` (the context read from the file) and every
`desired_value` that came from the default (`ZXRjX3Q=`); a declared value
(`httpd_sys_content_t`) was plain on both sides. The cause is Puppet's:
libselinux's Ruby binding returns ASCII-8BIT strings, and
`Event#to_data_hash` runs `ToStringifiedConverter`, which turns any ASCII-8BIT
string into Base64 (`transaction/event.rb:89-105`,
`pops/serialization/to_stringified_converter.rb:97-103`). OpenVoxDB stores the
plain string, so the gates now capture Ruby's report the way the terminus
sends it ("How the gates capture Ruby's report"). The gate was not made green
by an allowlist entry: the leaves are compared, and they agree.

What it does not prove: one policy (targeted, enforcing) and one kernel; no
MLS ranges beyond `s0`, so not `selrange` (I-7); no mcstrans (D-33). I-9 (an
unreadable context) is not reachable on this host: a file with no
`security.selinux` xattr reads back as `system_u:object_r:unlabeled_t:s0`
(measured on an ext4 loop mount, a file written by `debugfs`), removing the
xattr is refused with `Permission denied`, and an invalid context cannot be set
(`Invalid argument`), so `lgetfilecon` did not fail in any case tried. The
literal `unlabeled` of I-12 was not reached either.

`task check` does not run it: a run takes about five minutes.

#### Running the SELinux gate

```bash
task dev:report-diff-selinux
```

It needs `podman`, `puppet` 8.10.0 on `PATH` (to compile the catalog, as G1
does), network access for the first run's download and for cloud-init's
package installs, and about 2 GiB of memory for the guest. No cgroup
delegation is needed: the container runs QEMU, not systemd. The first run
builds `localhost/voxagent-selinux-vm:latest` (with the fuse-overlayfs fallback
`tests/spine/build-image.sh` shares with the systemd gate) and downloads the
image. Measured on an AMD Ryzen 9 5900X under TCG: SSH answered after 85-101 s,
cloud-init finished after 238-283 s, and a whole run took 4 m 54 s. One
deadline, 30 minutes, bounds the boot.

The expected last line is `report-diff-selinux: reports agree outside the
allowlist`, exit 0, after `915 leaves compared, 0 divergent` and `walk order
agrees over 22 resources`.

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

`file` is bounded to `ensure`, `content`, `mode`, `owner`, `group`, `source`
(local paths and `file:` URIs), `recurse` and `purge` (both `true`/`false`),
`path`, `replace`, `force`, `links` (`manage` or `follow`), `target` and
`show_diff` (accepted, no effect), the SELinux attributes `seluser`, `selrole`,
`seltype`, `selrange` and `selinux_ignore_defaults`, and `backup` (a suffix, or
`puppet`, the default bucket under `--vardir`; any other bucket name fails as
Puppet's missing bucket does, and a catalog that declares any `filebucket`
resource is a pre-flight refusal, D-50), and `validate_cmd` with
`validate_replacement` (strings only, and not with `recurse => true`;
`internal/ral/file_validate.go`, D-48).
Of the metaparameters, `loglevel` is accepted on any type only as `notice`,
Puppet's default, which `concat` copies onto every `File` it generates; any
other level is refused like any unimplemented attribute (`internal/ral/check.go`).
`exec` is bounded to `command`, `path`, `cwd`, `returns`, `refreshonly`, `creates`,
`onlyif`, `unless`, `logoutput`. `notify` is bounded to `message`. `package` is bounded to
`ensure`, `name` and `provider` — three of the twenty-three attributes the type
declares, with `provider` recognised **only** so that a value other than `dnf`
can be refused. `service` is bounded to `ensure`, `enable`, `hasstatus`,
`hasrestart`, `start`, `stop`, `status`, `restart`, `name` and `provider`.
Everything else — and every resource type other than `notify`, `file`, `exec`,
`package` and `service`, and `user` and `group` where they are native (below) —
is a pre-flight refusal or a resource for the extension host, not a silent omission,
and the refusal covers the **whole catalog**: a partially applied node produces
state-diff divergences that are artifacts of coverage rather than defects.

`file` and `package` bound an attribute's **values**: `file` bounds `source`,
`recurse` and `purge`, `package` bounds `ensure` and `provider`.
Pre-flight hands a non-string value to the bound as its string form, and splits
a list into elements only for an attribute the provider declares list-valued
(`ral.ListValued`) -- `file`'s `source` alone -- so a list anywhere else is
still judged whole and refused.

For `package`, `ensure` is `present`, its alias `installed`, `absent`, `latest`, or a concrete
version string; `purged`, `disabled`, `held` and every version range are
refusals (D-13). This is why `Gap` carries a `Value` and pre-flight grew a third
message form, `Package[foo]: ensure => "purged" is not implemented`: `file`'s
`ensure` has four values and catching a fifth at apply time is nearly harmless,
where `package`'s carries an open-ended version string and an apply-time refusal
would apply the rest of the catalog before noticing. `file`'s apply-time
`default:` branch is deliberately unchanged — changing a shipped provider with
no consumer asking for it is a behaviour change nobody requested.

### `service`

`service` is the systemd provider and nothing else. `provider` is recognised
only so another value can be refused; `enable => mask` is refused at pre-flight,
and `manual` and `delayed` fail Puppet's own feature check for systemd.
`hasstatus` and `hasrestart` are accepted and have no effect, because the
systemd provider always has a status and a restart command (`systemd.rb:188,201`).

- **The enable change rides inside ensure's.** When `ensure` changes and `enable`
  is out of sync, `enable` is synced inside the `ensure` change and gets no event
  of its own (`type/service.rb:120-136`, `resource_harness.rb:84-99`); under noop
  it is not reported at all. When `ensure` is in sync or unmanaged, `enable` is
  its own change.
- **`InvalidateRefreshes`.** A change that starts the service sets
  `Change.InvalidateRefreshes`, which drops the resource's own pending refresh:
  the start event carries `invalidate_refreshes` (`type/service.rb:109`), so a
  service started this run is not then restarted by its notifiers
  (`event_manager.rb:82`).
- **The init check is `Validate`'s, not `Prefetch`'s.** A host whose PID 1 is not
  systemd refuses the whole catalog (D-22), because a prefetch failure would let
  every other type apply first.
- **Custom commands.** A `start`, `stop`, `status` or `restart` given as a string,
  a single-element array or a scalar runs through `/bin/sh -c`, as Puppet's does
  (`util/execution.rb:360,391`): the catalog author's shell, as `exec`'s command
  is. A multi-element array runs as direct argv. Every built-in command is argv
  with `--` before the unit name, never a shell.
- **Failures carry the journal.** A failed start, stop or restart appends
  `journalctl`'s last lines, run as argv under `env LANG=C LC_ALL=C`. See D-18,
  D-19 and D-21 for where this differs from Puppet.

D-10, D-18, D-19, D-20, D-21 and D-22 are `service`'s; D-23, which it shared with `exec`, is retired. `task dev:report-diff-systemd`
compares voxagent's report with Ruby's in a container with systemd as PID 1.

Declaring an attribute is a promise to read it. `withpath` was removed from
`notify` for exactly this reason rather than implemented: pre-flight accepted a
catalog setting it, the run reported success, and the attribute had no effect.

The refusal list doubles as the porting roadmap: run `voxagent apply` against a
fleet's catalogs and the gaps it prints, ranked by frequency, are the measured
answer to "what should Tier 1 cover next". With the extension host, the same
ranking is the `N <type> resource(s) applied via extension host` lines. On the
SIMP gate's catalog (737 resources), measured 2026-11-19 before `user` and
`group` went native, 246 resources went to the host or would have been
refused without it: 210 of module types, which voxagent can only delegate, and
36 of core types applied through the host (`augeas` 18, `mount` 5, `cron` 4,
`group` 4, `user` 3, `host` 1, `tidy` 1). By type, all 246: `sysctl` 63,
`ssh_config` 49, `sshd_config` 36, `concat_fragment` 20, `ini_setting` 20,
`augeas` 18, `concat_file` 6, `mount` 5, `anchor`, `cron`, `group` and
`reboot_notify` 4 each, `user` 3, `kernel_parameter` 2, and one each of
`host`, `init_ulimit`, `runlevel`, `selinux_state`, `simp_file_line`,
`sshd_config_subsystem`, `svckill` and `tidy`. 169 of the 246 (69%) are
Augeas-backed: `sysctl`, `ssh_config`, `sshd_config`, `augeas`,
`kernel_parameter` and `sshd_config_subsystem`. With `user` and `group` native
there, 239 remain; the order after them is `concat`, then the Augeas family.

### `user` and `group`

`ral.User` (`internal/ral/user.go`, `user_params.go`, `user_apply.go`) and
`ral.Group` (`internal/ral/group.go`) reproduce openvox 8.33.0's `useradd` and
`groupadd` providers, measured on CentOS Stream 10 (M1–M45, 2026-11-19,
openvox 8.33.0).

- **Routing.** `ral.NativeUserGroup` (`internal/ral/usergroup_route.go`) is
  asked once per run by `nativeProviders` (`cmd/voxagent/apply.go`), the one
  place natives are listed for `apply` and `agent`. Both types need a
  RHEL-family `/etc/os-release` (`ID` `rhel`, or `rhel` among `ID_LIKE`'s
  words), not Fedora, with `VERSION_ID`'s major at least 10, and Puppet's
  libuser feature off (`/usr/sbin/luseradd` and `/usr/sbin/lgroupadd` not both
  executable). An unreadable or unparsable os-release delegates. Then each type
  is decided on its own: it is delegated when any resource of it names a
  `provider` other than `useradd`/`groupadd`, and `User` also when any `User`'s
  `purge_ssh_keys` is not `false`, `"false"` or `[]`, because purging generates
  `Ssh_authorized_key` resources only the host can apply. A type is also
  delegated when any resource of it has a value shape the native parser does
  not implement (`unimplementedValue`: a `gid` array, a non-string `comment`,
  a number among `members`); a value Ruby itself refuses stays native and is
  refused with Ruby's message. Ruby's attribute set
  depends on the host's provider features, so elsewhere the host is the oracle
  (D-87).
- **Attributes.** Every attribute the Ruby types declare is accepted, so
  pre-flight never refuses one as unknown. Those whose provider feature is
  missing (`user`'s `roles`, `auths`, `profiles`, `keys`, `project`,
  `attributes`, `salt`, `iterations`, `loginclass`, `ia_load_module` and
  `forcelocal`; `group`'s `attributes`, `ia_load_module` and `forcelocal`) are
  dropped as
  `type.rb:788-791` drops them, each with Ruby's `Provider <p> does not support
  features <f>; not managing attribute <a>` debug line, logged while the
  catalog is converted (`ral.ConversionDebugging`). Values are validated before
  anything is applied, with Ruby's texts, and numeric strings are read with
  Ruby's `Integer()` (`'0010'` is uid 8).
- **State.** Accounts are read through libc NSS with cgo
  (`internal/ral/nss_linux.go`, the only cgo in voxagent): `getpwnam_r`,
  `getgrnam_r`, `getgrgid_r`, `getspnam_r`, `getgrouplist`, and a group
  enumeration under a process-wide mutex. Buffers grow on `ERANGE` up to 1 MiB,
  then the property fails. As non-root `getspnam_r` is refused and the shadow
  fields read as absent, as Ruby's nil.
- **Commands.** shadow-utils, found on Ruby's default `PATH` order
  (`shadowPath`), run as argv with `LANG=C LC_ALL=C` through the same path as
  every other provider's commands. When any property is Sensitive (password
  always is), every create command is logged and reported as `[redacted]`; on
  modify only a Sensitive property's own command. The changed password is written to `chpasswd` on stdin; the create path
  sets it with `usermod -p`, as Ruby does (I-34). A name beginning with `-`
  reaches shadow-utils as an option, as under Ruby (I-33).
- **Messages.** Every `user` change but `ensure`, and `group`'s `members`
  change, sets `Change.DisplayVerbatim`, which governs the failure line only
  (the provider boundary, above).

Unit tests use a fake command runner and a fake NSS seam
(`internal/ral/user_apply_test.go`, `group_test.go`); the root-only
`nss_linux_test.go` reads real entries it creates. G5
(`tests/spine/report-diff-container.sh`) compares the measured cases against
Ruby, live and, from fresh containers, under `--noop`; the extension gate holds the mixed-routing case, and the SIMP gate
applies SIMP's three users and four groups natively. D-87 and D-88 are theirs.

### `file` source, recurse and purge

`source` is read locally: an absolute path or a `file:` URI, or a list of them,
the first that exists winning. `puppet:`, `http:` and `https:` sources are
pre-flight gaps, each list element judged on its own. Relative, opaque and
unparseable sources, a `file:` URI naming a host, two of `content`, `source`
and `target` (a target `ensure` supplies counts), `content` with
`recurse => true`, `ensure => link` or a link target with `recurse => true`
(Puppet's `recurse_link`), an `ensure` that is neither a string nor `false`, a
`target` that is not a string and a static catalog's inlined `metadata` are
validation refusals. A regular-file source is checksummed when it is resolved
and streamed through sha256 when it is copied; a copy whose checksum differs --
the source changed in between -- is discarded with Puppet's `File written to
disk did not match desired checksum` message, and the destination keeps what it
had. Neither side of a comparison is read whole, so a source may be larger than
memory. A missing source fails its resource even under `ensure => absent` and
`--noop`, as Puppet's does.

Recursion is an engine capability, `ral.Generator`. A resource whose provider
generates is applied as a **unit** at its turn in the walk: `Generate` reads the
filesystem as the run has left it, the parent applies, then each child applies
in bytewise path order through the same per-resource routine, each with its own
status. Children are not graph vertices. Puppet inserts them into its graph and
moves the parent's out-edges onto a "completed" whit; voxagent reproduces the
whit's two effects from the unit's result. A failure anywhere in the unit
blocks the parent's dependents, naming the member that failed, and otherwise the
parent's subscribers get one refresh event when any member produced one.
Generating up front was rejected because it reads the filesystem before earlier
resources run; a mutable graph because it rewrites the walk's core contract for
no observable gain. A child depends on its nearest generated ancestor; a failed
or skipped parent still generates and its children are reported skipped. A
generation failure is logged against the parent and fails the report but not
the exit code (Puppet's `compute_status` against `exit_status`). Under `--noop`
the whit's own "Would have triggered" notice is not reproduced (D-25).

`File.Generate` walks the directory and a directory source breadth first, never
descending a symlink, and drops every child at or below another catalog `File`.
Children inherit the parent's parameters except `ensure`, `recurse` and
`source`. A child that neither the source nor `purge` decides has no `ensure`
and manages only the permissions of what is there -- the rule a catalog `file`
with no `ensure`, `content` or `source` now follows too (`type/file.rb:561-567`).
Numeric modes are dirmasked on directories, so `mode => '0644'` leaves the
tree's directories 0755. More than 1,000 entries in one walk logs Puppet's
soft-limit warning and keeps every entry. Memory is linear in the number of
changed children, because each one's status and logs are held until the report
is written: measured at about 8 KB per changed child, a first run over a
100,000-entry tree peaked at about 822 MB and the converged rerun at 159 MB.

Every operation on a child goes through an `os.Root` opened on the recursed
directory (`rootFS`), after refusing a symlink at the directory itself, so a
symlink swapped into the tree after the walk cannot carry a write, chmod, chown
or unlink out of it. Residuals remain, all inside the tree: `os.Root` follows a
link whose target stays in the tree, so such a swap can redirect an operation
within it, including into a catalog-managed subtree `purge` exempts; there is no
`Lchmod`, so `chmod` is preceded by an `Lstat` and a swap between the two is a
race; an empty directory swapped in before a purge's remove is rmdir'd; and a
hard-linked child shares its inode, so its chmod or chown reaches the other
link, as Puppet's does. The `rootFS` doc comment lists them. The walk reads each
directory with `O_NONBLOCK|O_DIRECTORY`, so a directory swapped for a fifo
between its Lstat and its open fails the walk with ENOTDIR instead of hanging
the agent (`internal/ral/file_recurse_test.go:TestFileGenerateDirectorySwappedForAFifoFailsInsteadOfBlocking`).
`openDirNoFollow` does the same for the recursed directory itself, opening it
`O_NONBLOCK|O_DIRECTORY|O_NOFOLLOW` and making the `os.Root` from that handle
through `/proc/self/fd`, because `os.OpenRoot` on a path blocks on a fifo
(`TestFileGenerateRecursedDirectorySwappedForAFifoFailsInsteadOfBlocking`).
That root is named by its `/proc` path, so `rootFS` re-opens every file it hands
out under the real path (`withName`) and errors on the handle name the real
file (`TestRootFSHandleErrorsNameTheRealPath`). A `/proc` that cannot be used
fails the child with an error that is not `fs.ErrNotExist`, so `--noop` does not
fall back to unconfined reads (`TestFileApplyNoopRefusesAChildWhenProcIsUnusable`). A
walked symlink keeps its mode, is `lchown`ed and relabelled itself through
`rootFS` (D-28, retired), and `purge` unlinks it. Purging a directory copies
Puppet's notice and `removed` event and leaves the directory, every run (I-1;
D-5 is retired),
so a purge over an unmanaged directory is never idempotent: G1 compares that
case and G2 leaves it out, as it leaves out the missing-source case, whose first
run fails by design. A file↔directory conflict inside a recursion follows
`force` like any other resource (D-27 is retired).

### `file` links, force and replace

The register rows hold each deviation; this section says where Puppet defines
each piece and how they fit.

**`path`.** A catalog carries `path` only when it differs from the title
(`resource.rb:101-102`). `FilePath` is the one place that resolves it, cleaned
as a title is (`type/file.rb:66-73`); report keys, event sources and refs stay
the title. A `require` naming the path resolves through `fileAliases`
(`resource/catalog.rb:176-178`), and `AutoRequires` offers `FilePath`.

**`links`.** `manage`, the default, acts on the link; `follow` acts on what it
points at (`type/file.rb:272-281, 895-903`): `Lstat` against `Stat` for the
destination. Any other value is refused before the run in Puppet's wording. How
a link at the path is replaced, written through or `lchown`ed is under
"Writing files" (D-4, retired, and I-5). A symlink `source` becomes a link under
`manage` and its target's content under `follow` (`source.rb:148, 170-176`;
D-26 keeps the special file types). `follow` with `recurse => true` is refused
by `Validate` (D-32); a recursion child that inherits `follow` still cannot
chmod through its link (D-31), and a walked link is managed under `manage`
(D-28, retired).

**`force`.** It matters only for a directory in the way, in `removeExisting`
(`type/file.rb:1022-1049`): without it the notice `Not removing directory; use
'force' to override` is logged and the directory stays, with it the tree is
removed. A file, link, fifo or socket is always unlinked (`:814-815`) and a
block or character device fails (`:816-818`). Puppet's consequences without
`force` are reproduced and registered: `ensure => absent` reports `removed`
while the directory stays (I-1), and a link replacing a directory logs the
notice twice before failing (I-3). Recursion children inherit `force`, so
their type conflicts follow it (D-27, retired).

**`replace`.** `replace => false` is consulted where Puppet consults it
(`type/file/ensure.rb:162-165`, `target.rb:66`, `data_sync.rb:38`). Under it
`ensure` is in sync for anything that exists, whatever its type, whatever
`ensure` asks for and whatever `force` says: nothing at the path is removed,
replaced or written (`keptByReplaceFalse`, checked in `Apply` after the source
resolves). So `ensure => absent` keeps a file, `ensure => file` keeps a
directory or a link, `ensure => directory` keeps a file, and a purge removes
nothing, because every purged child inherits `replace`. `content`, `source`
content and `target` are in sync too, while `mode`, `owner`, `group` and the
SELinux properties still reconcile as ordinary properties. All of it was
measured on Puppet 8.10.0. The path is read as `links` says, so under `follow`
a dangling link counts as absent. A missing path is created as usual.

**`show_diff`** is accepted and has no effect: Puppet prints a diff only when
the global `show_diff` setting is on, and voxagent has no such setting.

G2 (`tests/spine/idempotence.sh`) holds the cases that converge. The ones it
leaves out cannot: `keepdir` (I-1), `ensure => file` with a `target` over an
existing path (I-6), and `follow` over an existing link under
`ensure => link`, which Puppet re-creates every run.

### `file` SELinux contexts

`seluser`, `selrole`, `seltype`, `selrange` and `selinux_ignore_defaults` are
implemented in `internal/ral/file_selinux.go`, which cites Puppet's and
libselinux's source line by line. This section says how the pieces fit.
Puppet's side was first read, not measured, because no host or container here
makes Puppet see SELinux as enabled; the SELinux gate (above) has since
measured it on an enforcing VM, where every resulting label agrees.

**Is SELinux enabled?** As libselinux 3.11's `is_selinux_enabled` is
(`enabled.c:11-21`, `init.c:31-152`): a **read-write** selinuxfs mount **and**
`/etc/selinux/config` existing. A container that sees the host's selinuxfs
read-only therefore reads as disabled. It is computed once per process, as
libselinux does in its constructor. When SELinux is disabled every `sel*`
property is a no-op: no lookup, no read, no write.

**Which paths are managed.** Only a path on one of ext2/3/4, gfs, gfs2, xfs,
jfs, btrfs, tmpfs or zfs, chosen by the deepest `/proc/mounts` entry that
prefixes it (`util/selinux.rb:220-226`); on any other filesystem the properties
are in sync. Mount points are not unescaped, as Puppet's are not (I-8).

**Defaults and the `selabel_lookup` dependency.** Unless
`selinux_ignore_defaults` is true, the default context comes from the host's
`selabel_lookup -b file -k <path> -t <mode>` (package `libselinux-utils`), so
matching is libselinux's own and there is no cgo. The mode is the path's
`lstat` mode before the run, or one derived from the declared `ensure` for a
path not there yet (I-10, I-11). A failed lookup is no default, as in Puppet.
When SELinux is enabled and a `File` wants defaults and can sync its SELinux
properties (not a removal, unless `replace => false` keeps the path;
`syncsSELinux`), `Validate` refuses the **whole catalog** before anything is
applied in two cases. The tool is not on `PATH`: the refusal names the
package, because a lookup that cannot run would otherwise leave the node
half-managed. Or the file-contexts handle does not open, on a filesystem that
carries labels: `File[<path>]: Cannot get default context with nil handle`,
because Puppet fails the whole run then too (D-35). The handle is probed once
per run (`handleOpens`, a lookup of `/`); a lookup that still meets a nil
handle during the apply fails that resource. A declared value replaces the
default field by field.

**Reading and writing a label.** Both go through a handle opened with
`O_PATH|O_NOFOLLOW` by the file's own `fileSystem` (the `os.Root` for a
recursion child), using `getxattr`/`setxattr` on `/proc/self/fd/N`, as
`selinux_restorecon` does. That acts on the object opened, a link included, and
never resolves the name again, so a directory swapped for a link out of a
recursed tree cannot carry a relabel out of it.
`lgetxattr`/`lsetxattr` by path would follow such a swap. A walked symlink
child is relabelled itself through that handle, never its target (D-28,
retired).

**Order and silence.** SELinux is applied last in `applyMetadata`, after owner,
group and mode. A path voxagent creates, and a file whose content it rewrites,
is relabelled silently, with no event: that is Puppet's `property_fix` after
`ensure` and after a content write. A path that already exists and is out of
sync gets one `seltype_changed`-style event per property. A relabel that fails
is only a warning, `Failed to set SELinux context <ctx> on <path>`, and the
event still succeeds (I-4). A file whose label cannot be read fails with
Puppet's internal Ruby error text (I-9), and `selrange` and a literal
`unlabeled` are edited as Puppet edits them (I-7, I-12). Mcstrans translation
is not done (D-33).

**What a lookup costs.** One `selabel_lookup` process per managed `File` that
wants defaults, except for recursion children, which are batched (below).
Measured 2026-10-06 in `localhost/fleet-node-el` (`libselinux-utils` and
`selinux-policy-targeted` installed) on an AMD Ryzen 9 5900X: a throwaway Go
benchmark calling `hostSELinux.defaultFor` 5,000 times in sequence for
`/etc/passwd`, three runs. The first run took 9.5 ms per call (cold file
cache), the next two 4.2 and 4.1 ms; 5,000 lookups are therefore about
**20 s** warm and about 47 s on the cold first run. Ruby's whole
5,000-resource `file` apply in `performance.md` is 8.4 s steady and 28.0 s
cold. Compared like for like, per-path lookups alone exceed Ruby's whole apply
both warm (20 s against 8.4 s) and cold (47 s against 28.0 s).

**Top-level `File`s are not batched.** On an SELinux-enabled host each
top-level `File` that wants defaults runs its own `selabel_lookup`, about
4 ms warm, and each also re-reads `/proc/mounts` in `labelSupported` to ask
whether its filesystem carries labels, once in `Validate` (before the
once-per-run handle probe, D-35) and once in `Apply`. Only recursion children share a batch
(below). The SIMP catalog's 146 top-level `File`s cost about 0.6 s in lookups.

The measurement has limits. It looked up one path (`/etc/passwd`)
sequentially, so a policy search over many different paths may cost more or
less per call. Ruby's figures are for a content+mode catalog on a host without
SELinux, not a recursion and not SELinux-enabled, so they are not a like-for-like
total. The lookup time against Ruby's whole apply is therefore a lower bound on
voxagent's extra cost on an SELinux-enabled host, not the difference between
the two agents.

**Batched lookups for recursion children.** A recursed `File`'s `Generate`
(on the provider `Prefetch` returned for the run) looks up its children's
defaults through `matchpathcon -n -m <type> -- <paths...>`, one process per
file type and per 64 KiB of arguments, and each child's `Apply` takes its
answer instead of running `selabel_lookup` (`prefetchChildDefaults`,
`hostSELinux.defaultsFor`). An answer is used only for the exact path and mode
the child's `Apply` asks for, so a child whose type changed in between, or one
the batch could not answer, is looked up per path as before. Not batched:

- a child whose lookup mode has no file type (an absent path with no declared
  `ensure`, I-11, so most children of a `source` copied into a new tree):
  `matchpathcon` cannot force mode 0, and without `-m` it would `lstat` the
  path itself;
- output that does not map back line for line (a lookup error, a warning on
  stderr) or a `matchpathcon` that cannot run: that chunk goes per path;
- top-level `File`s.

`matchpathcon` gives `selabel_lookup`'s answers: it opens the same handle (file
backend, no path, no validation) and makes the same non-raw `selabel_lookup`
call with the bare `S_IF*` bits, which are all the backend compares
(libselinux 3.11 `utils/matchpathcon.c:27-44, 50-71, 134-152`,
`src/label_file.c:2099-2122`). Checked 2026-10-06 in the same container:
1,214 paths (the image's `/etc`, `/usr/bin`, `/var`, `/run`, `/home`, `/root`
trees plus hand-picked ones including `/etc/shadow`, `/tmp`, `/usr/tmp`,
`/home/u/.ssh/authorized_keys`, a missing path, a path with a space, a tab
and a non-ASCII byte) by 10 modes (each file type, with and without permission
bits) gave 12,140 identical answers, single and batched. A `selabel_open`
failure is still `Cannot get default context with nil handle` for every child.

Measured the same way, 5,000 distinct paths through the batch took 28-39 ms
(three runs), against 21.0 s per path in the same container. Asking whether
the parent's filesystem supports labels is done once per recursion; each
child's `Apply` still asks for itself, which reads `/proc/mounts` each time,
about 0.2 ms a call or 1.1 s per 5,000 children.

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
  (`type/exec.rb:497`, `:560`), once per command (a string, or an array of
  strings and argv arrays). With no `path`, an unqualified one is refused as
  `Parameter unless failed on Exec[x]: 'grep' is not qualified and no path was
  specified. ...`, the parameter wrapper rather than the type-level one. The
  order is `cwd`, `creates`, `unless`, `onlyif`, then the command. `creates`
  sets `accept_arrays` (`:448`), so an array of absolute paths is accepted. A
  `command` that is neither a String nor an Array is refused first, as the
  namevar is set first: `Parameter command failed on Exec[x]: Command must be
  a String or Array<String>, got value of class Integer` (`type/exec.rb:216-220`).
  An argv command's executable is its first element, whole, so `['echo',
  'rel']` with no `path` is refused naming `'echo'`; a flat `unless` array is a
  list of string commands, so `['/usr/bin/test', '-d', '/tmp']` is refused
  naming `'-d'`, and an argv check is a nested array. D-54 lists the argv
  shapes voxagent refuses where Puppet does not.

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
