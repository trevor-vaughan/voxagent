# Maintainer notes: what the gates do and do not prove

For maintainers. Split out of the [apply spine](apply-spine.md) notes, which define the terms used here (G1 to G5, Tier 3). How to run each gate is in [docs/dev/gates.md](../dev/gates.md). Back to the [README](../../README.md).

Run a gate with `task dev:<name>`. [docs/dev/gates.md](../dev/gates.md) lists
them and the prerequisites of the container gates (the `fleet-node-el` image, a
systemd-booted container).

| Gate | Proves | Does not prove |
|---|---|---|
| `dev:report-diff` | voxagent's report matches Ruby's from identical catalog bytes, outside a measured allowlist. It also compares the order resources were evaluated in (`gatekit compare-order`, read from each report's log order), over every resource that logs; its `wo*` fixtures cover the walk-order cases, and it reports `walk order agrees over 112 resources`. For `backup` alone it also compares state: the suffix copy's content, mode and mtime, and every entry of each agent's clientbucket, byte for byte | Anything else about the disk. Two implementations can agree on a report while both are wrong about the filesystem |
| `dev:report-diff-extension` | The same differential for Tier 3 types run through the extension host — stdlib's `file_line` and `anchor`, `concat`, and the host's own test types, mixed with native `File`s in refresh and failure chains — from one pluginsync-style `libdir` both sides load. Generation is covered: `concat_file`'s load-time `generate` of its `File`, `concat`'s `eval_generate` overriding that `File`'s content from two fragments declared out of order, one of them `Sensitive` so Ruby's redacted content event and voxagent's must agree (D-8) (and its `ensure` to `absent` for a `concat` with no fragments), `vox_evalgen` making two resources and raising, `vox_gen`'s `generate` making a `Notify`, and a relationship through a Tier 3 name alias. Every `concat` in the fixture runs on the host: a `format => json` `concat_file` and a fragment with `source` are in the catalog, shapes native delegates, and both types move together (D-89); native concat is G5's and the SIMP gate's. `vox_evalgen`'s `ge` returns an existing `File`, applied in `ge`'s unit at its turn with a subscriber refreshed through it (D-43). Then G2: each side applies again on the target its own first run converged, the two second reports must agree too, and the resources that converge must have changed nothing in voxagent's second report. Last, `concat`'s default `backup => 'puppet'` must have left both agents' clientbuckets byte-identical. The comparisons report `1279` and `588 leaves compared, 0 divergent` (measured 2026-11-20), and the walk order agrees over `36` and `16` resources; the first includes `tidy` children, which `generate` places before their parent | That a second apply changes *nothing* everywhere: `Vox_prefetch[ok]`'s test provider never persists, so it changes on every run under Ruby as well; the converging resources are asserted by name instead. Anything under `--noop` beyond one `File` recursion: the `--noop` pass compares only that unit (its `Would have triggered` notice and children's noop events), not the main catalog, whose Tier 3 resources a measured pass once agreed on (D-40). `validate_cmd`, which forks and is G5's. The refusals of D-42, which no fixture reaches, and D-43's sentinel, which `concat` reaches without being able to observe it. The `evaluation_time` of a skipped resource: Ruby writes `null` and voxagent `0`, and `resource_statuses.*.evaluation_time` hides both. The order of the recursing `File[$T/d]` children: they change nothing, so they log nothing and have no position in the walk; a G1 fixture holds an `eval_generate` recursion's order |
| `dev:idempotence` | A second apply changes nothing | Anything about agreement with Ruby. A consistently wrong provider is perfectly idempotent |
| `dev:state-diff` | Both agents leave the same filesystem state, including `exec` and `owner`/`group`, and — since `package` shipped — the same installed package set, compared with `rpm -qa` | Anything outside the paths the fixture names, and anything about the report: `rpm -qa` cannot see an event name, a `desired_value`, or which property a failure was attributed to |
| `dev:derive-run-to-run` | `report-diff`'s allowlist still covers exactly what varies between two Ruby runs | Anything about voxagent. It measures Ruby against itself |
| `dev:report-diff-container` | voxagent's report matches Ruby's from identical catalog bytes for the two types the host cannot measure at all — `package` and `exec` — with both agents inside `fleet-node-el`, against a catalog the image's own Puppet compiled. It also holds `File`'s `validate_cmd`, which forks: a passing and a failing validator, one with no `validate_replacement` token, one that cannot be executed, and `visudo` on a good and a bad `sudoers` file. It also holds the `Sensitive` cases (a marked `File` content, mode and `ensure`, a marked `Exec` command and `unless`, a marked `Notify`) and `Exec`'s `logoutput` (`true`, `false`, `on_failure`, a marked command's `[output redacted]`, a failing command's output, a refresh). The comparison reports `3677 leaves compared, 0 divergent` live and `1500` under `--noop` (the counts from the 2026-11-20 run, openvox 8.33.0); `1668` on 2026-11-17 (`WANT_PUPPET=8.33.0`), `815`, `314` before the five `exec` resources were added, `448` before `validate_cmd`, and `650` before the argv-array `exec` fixtures. The walk order agrees over `103` resources live and `37` under `--noop`. Its augeas half compares every case under `tests/spine/augeas/cases` live, on a rerun and under `--noop`, with the cases both sides refuse agreeing too, 0 divergent (measured 2026-11-21, openvox 8.33.0) | Anything about the disk, exactly as G1 does not. G5 compares `--noop` for the account and concat cases, so only `service`'s noop is unmeasured (D-10). Nothing about the *text inside* a failure detail, which the comparator masks. And nothing about the *host*: the oracle inside the image is openvox, so a leaf where openvox and the gem differ — `package`'s install command line is one — is settled in openvox's favour by this gate and would diverge against the gem by design |
| `dev:report-diff-systemd` | voxagent's report for `service` matches Ruby's from identical catalog bytes, with each agent in its own container booted with systemd as PID 1, and both leave every fixture unit in the same `is-active`/`is-enabled` state. The comparison reports `315 leaves compared, 0 divergent`, and the walk order agrees over `8` resources | Anything under `--noop` (D-10). A failing start: its journal text varies between any two containers, so the fixture has none and `TestServiceStartFailureCarriesTheJournal` pins it. The built-in commands' locale, which no report shows. And any init system other than systemd, which voxagent refuses (D-22) |
| `dev:report-diff-simp` | voxagent applies the public SIMP catalog (737 resources, 63 `Sysctl`, 16 `Binary` values, compiled in the container it is applied to) as Ruby does, each agent in its own systemd-booted container: the reports agree outside the allowlist (`12782 leaves compared, 0 divergent`), the walk order agrees over `340` resources (measured 2026-11-21 with the four augeasproviders types native), and both leave the same `tmp.mount` enablement, `/etc/audit` mode and reboot notifications | Anything about auditd's daemon or about polkit's start path: the image stubs the one and boots with the other running ("The SIMP gate"). `haveged`, which is in EPEL and fails to install on both sides, so only its failure attribution is compared. Any catalog but this one, and any SIMP release but the pinned one |
| `dev:report-diff-selinux` | voxagent's report for `file`'s `sel*` properties matches Ruby's from identical catalog bytes on a CentOS Stream 10 VM with SELinux enforcing, and both leave the same mode, owner, group and label on every fixture path (see "The SELinux gate"). Not part of `task check` | Anything about another type, another policy than targeted, MLS ranges beyond `s0` (`selrange`, I-7), or mcstrans (D-33) |
| `dev:sensitive-canary` | No `Sensitive` canary reaches stdout, stderr or a report from real applies of every type, and from the delegated routes through the extension host, in the openvox image (the container half of `TestSensitiveCanarySweep`). Not part of `task check` | `service` beyond its refusal text, because the container is not systemd-booted; the route a `user` or `group` takes when delegated, because both are native in the image. Exits 2, not 1, when it cannot run |
| `dev:augeas-lib` | The libaugeas binding (`internal/lib/augeas/augeas_lib_linux.go`) against openvox's real library 1.15.0 in the augeas image: the probe accepts it and refuses a changed ruby-augeas file, `aug_set` on an empty variable fails as Ruby's does, an empty file loads as a node, `aug_match` frees zero and many results without a double free (no leak check), a borrowed value is copied, an embedded NUL is refused, `parsed_as?` is true for ciphers and false for compression, and a save keeps the file's mode and owner (the `Test(ProbeRealLibrary\|Real)` tests, built with `-tags augeaslib`). Not part of `task check` | The providers on the real library (G5's augeas half does that), and any other libaugeas: the tests carry the `augeaslib` build tag, so a host's `go test ./...` never builds them, and no host run counts as a pass. Exits 2, not 1, when it cannot run (no podman, no base image, a failed build) |
| `dev:agent-gate` | `voxagent agent` enrols against a real OpenVox server (CA, autosign), refuses a wrong `--ca-fingerprint`, pluginsyncs, applies, and PUTs a report whose run-2 form matches `puppet agent -t`'s from the same server under `gatekit compare-reports` (the spine allowlist merged with `tests/agent/allowlist.json`); run 2 sends no certificate request; with the server stopped the agent exits could-not-run. Not part of `task check` | Anything about catalogs beyond the fixture's; the first-contact TOFU window (it is accepted by design); CA rotation, renewal, or a server that answers differently from the pinned OpenVox image. See `agent-transport.md` |
| `gatekit compare-order`, inside every report gate (`dev:report-diff`, `-extension`, `-container`, `-systemd`, `-selinux`, `-simp`) | Each resource that logs is evaluated in the same order relative to the others as in Ruby's run: each report's resource statuses, ordered by their first log line (a log belongs to the status whose containment path is the longest prefix of its source), compared over the resources both logged for. A status only one report has diverges, and so does a status both report that only one logged for. A comparison that cannot attribute a log (the side without it has a log source no status owns that the other side does not also leave unowned, as both leave `Puppet`), or that compares nothing, cannot run (exit 2), so it never agrees vacuously. Counts are in each gate's row and in D-44 | The position of a resource that logs nothing: one that changes nothing has no place in either order, so most of an idempotent run is not compared. Whits, which have no status. Anything between two log lines of one resource |

Each has been seen to go red on a deliberate break and back to green, except
`dev:report-diff-simp`, which has gone red only on a real divergence, D-8's
six `File[/etc/chrony.keys]` leaves before voxagent redacted them ("The SIMP
gate"); no deliberate break has been run against it. A gate that has only ever passed
is unproven: it may be unable to fail. G5's `package` break was
dropping `-y` from the install argv: `18` divergent leaves naming the command
line, `Execution of '/usr/bin/dnf install tree' returned 1: …` against Ruby's
`-y install`. Its `exec` break was attributing a non-zero status to `command`
instead of `returns` — one line in `internal/units/exec/exec.go` — and it went red
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
stops short of the code it was written for proves nothing about it, and until
that resource existed this one stopped short.

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

- **Anything under `--noop` outside concat and mount.** G5's `--noop` passes
  compare an `exec` noop event only in concat's two refresh cases (M13's
  required exec) and in mount's `notify-refresh` (its `trigger` exec); the main
  fixture's execs run live only.
- **The text inside a failure detail.** `internal/gatekit/reportcmp`'s
  `elideFailureDetail` masks everything after `change from 'notrun' to ['0']
  failed: `, so the wording `'/bin/false' returned 1 instead of one of [0]` is not
  a compared leaf — it was measured byte-identical on 2026-09-26, and it is
  `internal/units/exec/exec_test.go:TestExecFailureRendersTheReturnsListBothWays` that
  holds it. The masked prefix still carries the property and the transition,
  which is the attribution the gate exists to check.
- **`creates`.** The fixture exercises `cwd`, `path`, `onlyif` and `unless`;
  `creates` and the rest of the bounded attribute set are unit tests only.
- **A refresh that FAILS.** `spine-exec-refresh`'s command succeeds, so
  `failed_to_restart` with its `resource_error` event is reached by
  `TestFailedRefreshFailsTheResourceAndSkipsDependents` and by no gate.

**`package` is the mirror image, and is invisible to `dev:report-diff` for a
worse reason.** The host's Ruby cannot fork, so `dnf.rb:27-34`'s load-time
confine fails, `dnf` is unsuitable, and Puppet falls back to the most specific
provider that *is* suitable — `dnfmodule`, which manages dnf **modules**. A
host-side differential would therefore run, produce a plausible report, exit 0,
and answer a different question than the one asked.

`exec` on the host fails
loudly with `Errno::EBADF`; `package` succeeds against the wrong subsystem,
which is why G5 exists rather than a package resource in G1's fixture. It is the
lesson of *"The two oracles are not the same code"* below, one step further
along: there the risk is reading the wrong tree, here it is *measuring* the
wrong one.

## How the gates capture Ruby's report

Every gate that compares a report against Ruby, except `dev:agent-gate`
(`dev:report-diff`, `dev:report-diff-extension`,
`dev:derive-run-to-run`, `dev:report-diff-container`, `dev:report-diff-systemd`,
`dev:report-diff-selinux`, `dev:report-diff-simp`, and the per-type G5 scripts
under `tests/spine/<type>/g5.sh`), captures Ruby's report through one committed
report processor, `tests/spine/lib/puppet/reports/gatejson.rb`. Each runs `puppet apply ...
--libdir <lib> --reports gatejson --reportdir <dir>` and compares
`<dir>/gate.json`; the container and VM gates copy `lib` in as they copy the
catalog. `store` is not requested, because only the conversion this replaces
read its YAML. `dev:agent-gate` reads the YAML the server stored instead and
converts it with `YAML.unsafe_load_file(...).to_data_hash`
(`tests/agent/run.sh`).

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
  ([What OpenVoxDB stores for a binary-encoded value](apply-spine.md#what-openvoxdb-stores-for-a-binary-encoded-value)), and the user made what
  OpenVoxDB stores the report contract.
- **Why a processor, not a conversion of the stored YAML.**
  - The coercion cannot be applied afterwards. `store` writes `to_yaml`
    (`reports/store.rb:38`), and the report's classes serialise through
    `to_data_hash` (`util/psych_support.rb:28-31`).
  - The stored file therefore already holds a bare Base64 string, which reads
    back as UTF-8 with no type marker. Decoding it, in Ruby or in the
    comparator, could not tell an encoded value from one that really is that
    text, and would hide a real divergence.
  - The terminus is itself a report processor, so `gatejson` sees the live
    report exactly where the terminus does.
- **Proven not to change anything else.**
  - One G1 run applied with `--reports store,gatejson`. `store` ran first, so
    its YAML was written before the coercion. That YAML, converted the old way,
    was identical to `gate.json` (`jq -S` then `diff`, empty, over 50 events
    with no ASCII-8BIT value).
  - `dev:derive-run-to-run` reports the same pattern and differing-leaf counts
    (the `run-to-run: N patterns cover M differing leaves` line) through the
    processor.
  - Removing `:@desired_value` from the coercion turns G1 red again on
    `pur/keep.txt`'s Base64 `desired_value`.
  - Making voxagent emit a wrong checksum turns it red on that leaf with the
    plain value on both sides.
- **Limits.**
  - A String that is not valid UTF-8 is changed by PuppetDB too: the terminus
    re-tags it US-ASCII and replaces every non-ASCII byte with U+FFFD. The gate
    then compares PuppetDB's form, not Ruby's bytes.
  - An event value that is not a String (an Array, a Hash, a Symbol) is left as
    `to_data_hash` writes it. The terminus sends `value.to_s` instead, a
    different shape that the gates do not reproduce.
  - The terminus drops unchanged resource statuses unless
    `include_unchanged_resources` is set, but `gatejson` coerces every status.
    That does not change a status whose strings are already UTF-8.

## What the comparator got wrong, and the fix

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
rule is reachable.

Two different rpm debug lines are duplicated in the
`unmeasured-W1-ruby` and `-W2-ruby` reports: the plain `Executing: '/usr/bin/rpm
-q nano ...'` query (twice; `-W3-ruby` carries it once) and a `--whatprovides`
query (twice; `-W3-ruby` lacks it entirely).

The count rule fires only on the
plain query line, adding one divergent log line to each of the four Ruby pairs
ordered W1 or W2 against W3; those pairs already exited 1 before the fix. The
`--whatprovides` line is reported as before, because W3 lacks it. Voxagent emits
neither line, so no natural pair of one Ruby and one voxagent report changes at
all.

G5's reference check, `internal/gatekit/g5ref`, carries a fourth inherited
quirk of the same kind. Its test for a "multi-entry" accepted list is Python's
`len()`, so a `desired_value` that is a two-character *string* passes as if it
were a two-entry list (`exec-returns-two-char-string` in that package's
golden). Ruby writes `returns` as a list, so no real run reaches it.

## The allowlist is measured, not asserted

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

Two entries break that provenance rule and say so in the file:
`metrics.time.values.package.value` and `metrics.time.values.exec.value`.
`dev:derive-run-to-run` reports both **UNUSED** rather than validating them,
because the host fixture has no `package` or `exec` resource and cannot have
either — Puppet on the host resolves `dnfmodule`, and the host Ruby cannot fork
under Puppet. The
`package` entry's provenance is the container instead: G5's own catalog applied twice by Ruby in
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

The nine Tier 3 timings — `anchor`, `file_line`, `vox_prefetch`, `vox_value`,
`concat_file`, `concat_fragment`, `vox_evalgen`, `vox_gen` and `tidy` — are UNUSED on the host fixture for the plainer reason that it
declares none of those types. Their provenance is `report-diff-extension.sh`'s
catalog applied twice by Ruby from an identical starting state, with
`gatekit run-to-run` reporting each `1 differing, 0 stable`.

## Two reference implementations, and why it matters

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
oracle, and one did: the image moved to a newer `openvox-agent` and the gate
exited **2**, refusing to compare, rather than measuring against a reference
nobody had reviewed. The pin was then bumped deliberately, after re-measuring
that parity still holds at the new version across all four compared paths.

It has happened since, and each bump was handled the same way: `dev:state-diff`,
`dev:report-diff-container`, `dev:report-diff-systemd` and
`dev:report-diff-simp` were run with `WANT_PUPPET=<new version>` and all agreed
(the same state across 4 paths and the same installed package set, 0 divergent
leaves, every cron and augeas case agreeing) before the four pins moved
together.

On one bump the first container run did not agree: the new agent rebuilt
ruby-augeas' `_augeas.so` from the same source, its new digest sent the augeas
types to the extension host, and the probe now accepts both builds' digests
(D-91). The SELinux gate's VM installs its own pinned RPM, not this image, so
its pin moves separately.

So the pin is a **tripwire, not a guarantee**: it catches the float rather than
preventing it, and the next rebuild will trip it again. Exit 2 and a demand to
re-measure is the intended outcome. Pinning the Dockerfile would fix it at the
source and belongs to the fleet harness rather than the spine.

### The two oracles are not the same code, and every citation here is the gem

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
| puppet gem 8.10.0 | `provider/package/yum.rb:312-313` — `[command(:cmd)] + no_debug + ["-e", error_level, "-y", install_options, operation, wanted].compact`, with `no_debug` `["-d", "0"]` on EL6 and later |

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

## The bind-mount blind spot, and why `/etc/hosts` is not in the fixture

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
  temp file over the target, and rename works only within one mount, so Ruby
  fails with `EBUSY` and leaves the file untouched.

Proving the instrument and comparing the agents are two different jobs, and the
fixture conflated them. `state-diff.sh` proves it as a **self-check on
every run**: it changes a bind-mounted path, requires that the snapshot notices
and that `podman diff` does not, and exits 2 if either stops being true. A claim
about podman that nobody re-checks is one that quietly stops holding.

Do not put a bind-mounted path back into the compared set. Extend the self-check
instead.

## The systemd gate

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
`internal/units/service/service.go` it reported `315 leaves compared, 17 divergent`, all
naming `Service[voxf]`'s extra `refresh` event and the metrics it moved.

### Running the systemd gate

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
cgroup subtree, as a normal systemd login session does, you should not need to set
anything up. That has not been measured here.

One more thing was measured while getting it to run. Asked straight after
`podman run`, `systemctl is-system-running --wait` fails with `Failed to connect
to system scope bus` and prints nothing, so the gate retries while it prints
nothing. One wall-clock deadline, `BOOT_TIMEOUT` (180 seconds), bounds the
whole wait for each container: every attempt is given only the time left, and
an attempt that times out ends the wait with exit 2.

## The SIMP gate

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

For the augeasproviders types the gate checks, before anything runs, that the
merged libdir carries the fifteen fingerprints (`augeasproviders_fingerprints`
in `tests/spine/simp/lib.sh`, beside `concat_fingerprints`), and after the run
that voxagent's `applied via extension host` lines name none of `sysctl`,
`ssh_config`, `sshd_config`, `sshd_config_subsystem` and `augeas`, and that
`/etc/sysctl.conf`, `/etc/ssh/ssh_config`, `/etc/ssh/sshd_config` and
`/etc/modprobe.d/blacklist.conf` have equal
content, mode and owner on both sides.

The merged libdir holds SIMP's `ssh.aug`
and `resolv.aug` in `augeas/lenses`, so the 49 `Ssh_config` resources take the
loadpath branch. The `Sysctl` coverage is mostly the failure path: rootless, the
real `sysctl -w` is refused for 62 of the 63 and the one `silent => true`
resource ignores its refusal, so the success paths are gated only by G5's
shim cases. The gate exits 2 under a rootful podman, where `sysctl -w` would
change the host kernel and the second agent would read the first one's value.

**Every exception is measured.**

- **Run to run.** The eighteen `metrics.time.values.<type>.value` entries for
  the SIMP types (augeas through user) come from `DERIVE=1 bash
  tests/spine/report-diff-simp.sh`: two Ruby runs, every measured pair in
  `tests/spine/allowlist.json`'s `_comment`. `tidy.value` was already there.
- **Registered, not compared.** None for `Sensitive` any more.
  - `File[/etc/chrony.keys]`'s content is `Sensitive`; before D-8 was resolved
    Ruby wrote `[redacted]` and `redacted: true` where voxagent wrote the digest
    and `null`, and the gate reported `12782 leaves compared, 6 divergent`,
    exactly the four event leaves and the ensure log (a `logs (source: …)` key)
    that were then registered.
  - Those entries are removed and the leaves are compared: `12782 leaves
    compared, 0 divergent`.
  - That earlier red was the gate's evidence that it can go red. It was a real
    divergence rather than a deliberate break.

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

## G5's augeas half and `task dev:augeas-lib`

`dev:report-diff-container` has an augeas half (`tests/spine/augeas/g5.sh`,
sourced by `report-diff-container.sh`) that runs on `localhost/augeas-measure-el`:
the G5 base plus `procps-ng`, `openssh-clients`, `openssh-server`,
`grub2-tools` and `strace` (`tests/spine/images/Containerfile.augeas`;
`fleet-node-el` has none of the files or commands the five types edit or run).
`tests/spine/augeas/image.sh` builds it when it is missing or when its openvox
or libaugeas differs from the base's, and a cold build needs the network (the
CentOS Stream mirrors). Remove the image (`podman rmi
localhost/augeas-measure-el:latest`) after editing the Containerfile, as for
the SIMP image.

- **Cases.** 63 live native cases, 4 live cases the host must apply (`refuse-augeas-*`) and 4
  refusals, one directory each under `tests/spine/augeas/cases/`.
  - 51 of the native ones are the measured ones (numbered), their `site.pp`,
    seed, `files` and lenses committed verbatim from the measurements.
  - 12 are the gate's own (`sysctl-existing-w-fails`, `sysctl-save-fails`,
    `sysctl-prefetch-load-error`, `sysctl-stale-invalid-key`,
    `sysctl-file-before`, `sshd-two-handles`, `sshd-mode-preserved`,
    `lens-dir-known-hosts-only`, `sysctl-delegated`, `augeas-runtime-errors`,
    `augeas-notified`, `augeas-apply-ignores-libdir-lens`).
  - A case with a `podman-args` file (06d and 06e need `--cap-add SYS_ADMIN` to
    unmount `/etc/resolv.conf`) gets those arguments on both sides.
  - Each is compiled alone by the image's openvox and applied in fresh
    containers, one per agent and mode (live with a rerun, then `--noop`); every
    pair goes through `gatekit compare-reports` and `compare-order` against the
    unchanged allowlist, and the state of every path the case lists (mode,
    owner, group, size, content) is compared after the live run.
  - A `refuse-*` case is one Ruby refuses at catalog conversion; both agents
    must refuse it with the same detail.
- **Both loadpath branches.** Cases without a `lenses` file run libaugeas's
  NULL-loadpath branch; the ones with it (M62-M64, M66-M69,
  `lens-dir-known-hosts-only`, `augeas-apply-ignores-libdir-lens`) run the `<libdir>/augeas/lenses` branch, with the
  libdir holding exactly the lenses the file lists.
- **The sysctl shim, both sides.** Rootless, the real `sysctl -w` is refused,
  so the success paths run through the measurement shim
  (`tests/spine/augeas/sysctl-shim.sh`) on both agents, and the shim's log of
  the commands run is compared, since no report shows it. The failure cases run
  the real binary on both sides.
- **Native, not delegated.** The libdir is checked against the fifteen
  fingerprints, and voxagent's output must not name a native type among those
  `applied via extension host`, so a delegated run, Ruby against Ruby, cannot
  pass as native. `sysctl-delegated` is the inverse: its `comment` is outside the
  native shapes, so voxagent must name `sysctl` there; each `refuse-augeas-*` case
  likewise must name `augeas`.
- **Rootful refuses.** Under a rootful podman the half returns 2 and runs no
  case, because a rootful `sysctl -w` would change the host kernel.

`task dev:augeas-lib` is the unit tests that need the real library, run in that
image (the row above); it needs the same image and is not part of `task check`.

## G5's cron half

`dev:report-diff-container` runs a cron half (`tests/spine/cron/g5.sh`, sourced by `report-diff-container.sh`; third after the concat and augeas halves) on
`localhost/cron-measure-el`: the G5 base plus cronie (`tests/spine/images/Containerfile.cron`; `fleet-node-el` has no
`crontab`, so there `cron` is not native, M4). `tests/spine/cron/image.sh` builds it when it is missing, lacks cronie, or
carries a different openvox than the base; a cold build needs the network. Remove the image
(`podman rmi localhost/cron-measure-el:latest`) after editing the Containerfile.

- **Cases.** 21 native cases and one refusal, one directory each (`site.pp`, an optional `seed.sh`, an optional `files`
  listing the paths whose end state is compared).
  - The 19 under `cases/` run with no cron_core in `--libdir`, so both agents use openvox's vendored 1.3.0.
  - The 2 under `cases-2.1.0/` (`env-empty`, `minute-0x1f`) run with the `lib/` of cron_core 2.1.0, taken from the
    module cache the SIMP gate pins and checked against its three digests, for the outcomes the releases differ on.
  - `cases-2.1.0/weekday-true` is the refusal: Ruby refuses it at catalog conversion, and both agents must refuse it
    with the same detail.
  - `cases/line-breaks` is I-38.
- **Judging.**
  - Each case is compiled alone by the image's openvox and applied in fresh containers, one per agent and mode (live,
    a rerun in the same container, then `--noop`).
  - Every pair goes through `gatekit compare-reports` and `compare-order` against the unchanged allowlist.
  - After the live run each side's state is read and compared: every path `files` lists (mode, owner, group, size,
    `cat -A`), with only the header's first-line timestamp normalised, since reports carry no crontab text.
  - `modify` and `same-name-two-targets` also list cronie's backup of the replaced crontab (`/root/.cache/crontab/`),
    which it saves only when the write's environment holds `HOME` (D-95).
- **Native, not delegated.** voxagent's output must not name `cron` among the types `applied via extension host`, so a
  delegated run, Ruby against Ruby, cannot pass as native.
- **Errors.** A seed that fails, and a missing case, are gate errors (2). The half runs after the augeas half.

## G5's host half

`dev:report-diff-container` ends with a host half (`tests/spine/host/g5.sh`) on `localhost/parsedfile-measure-el`: the G5
base plus openssh (`tests/spine/images/Containerfile.parsedfile-types`, the node the ParsedFile-types measurements ran
on). `tests/spine/parsedfile/image.sh` builds it as `cron/image.sh` builds cron's: when it is missing, lacks openssh, or
carries a different openvox than the base. Every container runs with `--no-hosts`, so `/etc/hosts` is the image's own
file, the same for both agents, not podman's bind mount (M2).

- **Cases.** 11 native cases and 7 refusals under `tests/spine/host/cases/`, all with no host_core in `--libdir`
  (vendored 1.3.0 is SIMP's 2.1.0, so there is no second release to run). Native: `create`, `aliases-reordered`,
  `comment-added-removed`, `aliases-empty`, `aliases-after-empty`, `remove`, `preserve`, `alias-string-one-word`,
  `no-ip`, `noop`, `missing-target`. Refused at catalog conversion: `invalid-ip`, `invalid-name`,
  `alias-string-whitespace` and the four I-39 cases `line-break-{ip,name,alias,comment}`. `g5_refusal`
  (`tests/spine/g5-case.sh`) reads a detail that spans lines, as `line-break-name`'s raw newline in `Host[foo<LF>bar]`
  does.
- **Judging.** As the cron half: live, rerun and `--noop` in fresh containers, `compare-reports` and `compare-order`
  against the unchanged allowlist. The state compared after the live run is every path `files` lists (mode, owner,
  group, size, `cat -A`), with only `# HEADER: This file was autogenerated at <time>` normalised, then every
  clientbucket entry, sorted: its directory (the old content's sha256) and its `paths` file.
- **Native, not delegated.** voxagent's output must not name `host` among the types `applied via extension host`.
- **Errors.** As the cron half. The half runs after it.

## G5's sshkey half

`dev:report-diff-container` ends with an sshkey half (`tests/spine/sshkey/g5.sh`) on the host half's image and with
its `--no-hosts` containers; the image's openssh owns `/etc/ssh`, the default target's directory. It runs every case
under one of two libdirs, as the cron half does: `cases/` with no sshkeys_core (openvox's vendored 2.5.0), and
`cases-3.1.0/` with sshkeys_core 3.1.0's `lib/` from the module cache that the SIMP gate pins, for the `key` check that
release adds.

- **Cases.** 19 native cases and 7 refusals.
  - Native under 2.5.0 (18): `create`, `refs` (title `name@type`, a type alias, ed25519), `existing` (0600
    `nobody` file kept), `aliases-reordered`, `aliases-empty`, `aliases-empty-create`, `alias-string`,
    `comment-key-in-sync`, `comment-key-dropped`, `modify-key` (tabs rewritten as spaces), `remove`,
    `absent-no-type`, `leading-whitespace` (prefetch failure), `no-type` (flush error), `missing-target-dir`,
    `target`, `key-whitespace` and `line-break-key` (I-39: written verbatim, re-applied on the rerun).
  - Native under 3.1.0 (1): `create`.
  - Refused at catalog conversion: `name-whitespace`, `name-comma`, `alias-whitespace`, `alias-comma` and `bad-type`
    under 2.5.0, and `key-whitespace` and `line-break-key` under 3.1.0.
- **Judging.** As the host half, end state and clientbucket included. `metrics.time.values.sshkey.value` is in the
  allowlist's `run_to_run`, measured Ruby against Ruby like the other per-type time leaves.
- **Native, not delegated.** voxagent's output must not name `sshkey` among the types `applied via extension host`.
  Because voxagent routes natively only on §2.2's digests, a changed SIMP pin fails the half here.
- **Errors.** As the cron half. The half runs after the host half.

## G5's sak half

`dev:report-diff-container` ends with an `ssh_authorized_key` half (`tests/spine/sak/g5.sh`) on the host half's image
and with its `--no-hosts` containers. Seeds create the users and files each case needs; both agents run as root, so
every `drop_privileges` true flush writes as the key's user, voxagent's through its `asuser` helper. `cases/` run with
no sshkeys_core in `--libdir` (vendored 2.5.0); `cases-3.1.0/create` runs with SIMP's 3.1.0 `lib/`, whose
`ssh_authorized_key` contract is 2.5.0's, to hold that voxagent routes that release natively.

- **Cases.** 25 native cases and 6 refusals.
  - Native under 2.5.0 (24): `create` (no `.ssh`, two keys, options), `existing` (comments, blanks, unnamed and
    `key_v1` lines kept, mode 0640 to 0600), `options-change`, `remove` (one by its generated `unnamed-1` name),
    `key-v1`, `key-v1-unnamed`, `user-in-run`, `user-after-key`, `nonexistent-user`, `no-home`, `no-user-target`,
    `drop-false-file`, `drop-false-untrusted`, `drop-false-dir` (prefetch failure), `write-refused`, `write-symlink`,
    `mkdir-refused`, `chmod-refused`, `move-target`, `move-not-target`, `sensitive`, `line-break-name` and
    `line-break-options` (I-39: the rerun's prefetch fails), and `shared-target-across-types` (an `Sshkey` before and
    after an `Ssh_authorized_key` on alice's file: each type prefetches at its own turn, so the bytes and record order
    must equal Ruby's).
  - Native under 3.1.0 (1): `create`.
  - Refused at catalog conversion: `no-user`, `key-whitespace`, `drop-privileges-maybe`, `options-bad`, `type-bad`,
    `title-vs-name` (the refusal names the title).
- **Judging.** As the sshkey half, with two differences in the state read after the live run: a directory in `files`
  gives its stat line alone, and a clientbucket entry is the sha256 of its `contents` with the header's timestamp
  normalised, not its directory name, because a target backed up after an earlier flush of the run wrote it carries
  that flush's timestamp. `metrics.time.values.ssh_authorized_key.value` is in the allowlist's `run_to_run`, measured
  Ruby against Ruby.
- **What it cannot hold.**
  - The agents run without `--debug`, as in every half, so no debug line is compared (the count
    of `The required user is not yet present on the system` varies,
    spec §6.3).
  - Ruby's `drop_privileges => false` is never trusted
    (§8 item 9): every drop-false case fails
    `not trusted` or the prefetch, and the trusted write is unit-tested only.
  - D-102's leak and D-100's old-target backup are not reproduced, so no case shows them: in
    `move-target` alice's own key changes first, so her file is backed up in both agents before the key moves out of it.
- **Native, not delegated.** voxagent's output must name neither `ssh_authorized_key` nor `sshkey` among the types
  `applied via extension host`.
- **Errors.** As the cron half. The half runs after the sshkey half.

## G5's mount half

`dev:report-diff-container` ends with a `mount` half (`tests/spine/mount/g5.sh`) on the host half's image and with its
`--no-hosts` containers, each also given `--cap-add SYS_ADMIN`: M10 measured it as the one flag that lets the container's
root run `mount`, `mount -o remount` and `umount` on a tmpfs (no `--security-opt` needed), so every ensure transition
and refresh runs live. The image has no `/etc/fstab`; `mount` lists the container's own mounts, the same for both
agents. No case manages `Mount['/']`: the container's `/` is a ghost (`ensure changed 'ghost' to 'defined'`). `cases/`
run with no mount_core in `--libdir` (vendored 1.3.0); `cases-2.1.0/create` runs with SIMP's 2.1.0 `lib/`, whose
contract is 1.3.0's, to hold that voxagent routes that release natively.

- **Cases.** 30 native cases and 1 refusal.
  - Native under 1.3.0, fstab only (14): `defined-absent`, `name-040` (`\040` round trip; `dump`/`pass` reset to
    `0`), `trailing-slash`, `missing-fstype` (every later flush fails with the first resource's text), `fields` (3-,
    4-, 5- and 8-field lines), `dump-pass-reset`, `remove`, `leading-whitespace` (the next write fails `Field 'device'
    is required`), `atboot` (no effect on Linux), `dash-defined` (a name `-o` written), and the I-39 cases
    `line-break-{name,device,dump,pass}` (written verbatim, re-applied on the rerun).
  - Native under 1.3.0, live tmpfs mounts (15):
    - `mounted-from-absent` (mount, then the remount)
    - `unmounted-from-mounted`
    - `ghost-to-mounted`
    - `ghost-to-absent` (other lines and comments kept)
    - `ghost-no-ensure` (five `defined` changes, then the remount)
    - `options-remount`
    - `options-unmounted-to-mounted` (only the ensure change; the flush precedes the mount)
    - `remounts-false` (`umount` then `mount`)
    - `swap` (no remount)
    - `notify-refresh` (an `Exec` refreshes a mounted, an unmounted and a swap entry)
    - `mount-fails` (missing mount point)
    - `dash-mounted` (`mount -o` fails)
    - `umount-busy` (a `sleep` holds the mount: the two errs of a failed ensure sync)
    - `refresh-fails` (an option tmpfs refuses fails the remount: two errs, `failed_to_restart`, the dependent skipped)
    - `relations` (a parent mount autorequired, a `File` on the tmpfs autobefore'd, declared in the reverse order)
  - Native under 2.1.0 (1): `create` (`mounted-from-absent`).
  - Refused at catalog conversion: `dump-bad` (`Invalid value "abc". Valid values match /(0|1)/.`).
- **Judging.** As the host half, end state and clientbucket included, plus `findmnt -rn -o TARGET,FSTYPE,OPTIONS
  --mountpoint` of each path a case's `mounts` file lists (`NOT MOUNTED` when none), with the host-dependent options
  `seclabel`, `uid=<n>` and `gid=<n>` removed (M10: a rootless tmpfs gets the host user's ids).
  `metrics.time.values.mount.value` is in the allowlist's `run_to_run`. No debug line is compared (the agents run
  without `--debug`), so the `Executing:`, `Flushing` and `Adding auto…` lines show only through the end state and
  `compare-order`'s walk order.
- **Native, not delegated.** voxagent's output must not name `mount` among the types `applied via extension host`.
- **Errors.** As the cron half. The half runs after the sak half.

**Run the gates one at a time.** Concurrent gate runs on one machine
coincided with flakes in the SIMP gate's polkit start and systemd boot. The
cause was not isolated; the gates may contend for podman's storage, the
delegated cgroup and the image tags, and none takes a lock.

## Debug logging's memory cost

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

## The SELinux gate

`dev:report-diff-selinux` (`tests/spine/report-diff-selinux.sh`) compares the
two agents on `file`'s SELinux contexts with SELinux **enforcing**. No other gate
can: the host and every container here see SELinux as disabled, and then both
agents make every `sel*` property a no-op, so G1 compares nothing about labels.

This host has no `/dev/kvm`, and RHEL's `qemu-kvm` omits TCG, so the
gate emulates a VM with Fedora's QEMU (`-accel tcg`), from the image
`tests/spine/selinux-vm/Containerfile` builds (Fedora 44, pinned by digest). The
guest is the CentOS Stream 10 GenericCloud image
`CentOS-Stream-GenericCloud-10-20260930.0.x86_64.qcow2`, pinned by the SHA256
CentOS publishes and verified on every run. It is downloaded once to
`${XDG_CACHE_HOME:-~/.cache}/voxagent/selinux-vm` (override with
`SELINUX_VM_CACHE`), outside the tree because it is 1 GiB; each run writes only
a copy-on-write overlay in its own `mktemp -d`.

cloud-init (`selinux-vm/user-data`) installs openvox 8.29.0 (the release RPM pinned by
SHA256, the agent by version), `libselinux-utils` and `dosfstools`, and
authorizes an SSH key generated for that run alone. The gate reaches the guest
by `podman exec … ssh` into QEMU's user-mode network, so nothing is published on
the host. The image ships enforcing; the gate asserts `getenforce` is
`Enforcing` and the agent is 8.29.0, and exits 2 otherwise.

One VM serves both agents, as G1's one directory does. Before each side,
`reset.sh` rebuilds every fixture path and sets every seeded label with
`chcon`, and the labels it left are read and compared between the sides; a
seed that differs exits 2.

The catalog is compiled on the host by its Puppet (`puppet --version` reports
which), exactly as G1 compiles its own, and the same bytes go to `puppet apply
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
915 leaves and found 23.

Each was a value Ruby reported Base64-encoded where
voxagent reports it plain (`previous_value` `dG1wX3Q=` against `tmp_t`); every
`owner`, `group` and `mode` leaf already agreed, in Puppet's order. The encoded
values were every `previous_value` (the context read from the file) and every
`desired_value` that came from the default (`ZXRjX3Q=`); a declared value
(`httpd_sys_content_t`) was plain on both sides.

The cause is Puppet's:
libselinux's Ruby binding returns ASCII-8BIT strings, and
`Event#to_data_hash` runs `ToStringifiedConverter`, which turns any ASCII-8BIT
string into Base64 (`transaction/event.rb:89-105`,
`pops/serialization/to_stringified_converter.rb:97-103`). OpenVoxDB stores the
plain string, so the gates now capture Ruby's report the way the terminus
sends it ("How the gates capture Ruby's report"). The gate was not made green
by an allowlist entry: the leaves are compared, and they agree.

What it does not prove: one policy (targeted, enforcing) and one kernel; no
MLS ranges beyond `s0`, so not `selrange` (I-7); no mcstrans (D-33).

I-9 (an
unreadable context) is not reachable on this host: a file with no
`security.selinux` xattr reads back as `system_u:object_r:unlabeled_t:s0`
(measured on an ext4 loop mount, a file written by `debugfs`), removing the
xattr is refused with `Permission denied`, and an invalid context cannot be set
(`Invalid argument`), so `lgetfilecon` never failed in any case tried. The
literal `unlabeled` of I-12 was not reached either.

`task check` does not run it: a run takes about five minutes.

### Running the SELinux gate

```bash
task dev:report-diff-selinux
```

It needs `podman`, `puppet` on `PATH` (to compile the catalog, as G1
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
