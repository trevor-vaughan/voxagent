# The Ruby extension host

For maintainers. Operators want the README. This page says how the code that exists works. Ruby citations are against
Puppet 8.10.0. Go citations are files in this repository.

voxagent applies Tier 1 types (`notify`, `file`, `exec`, `package`, `service`)
natively. Every other legacy `Puppet::Type.newtype` type, core or shipped by a
module, is applied by a Ruby process, the host, that runs Puppet's own
`ResourceHarness` on it. Evaluation, generation, Tier 3 name aliases and
`loglevel => notice` are implemented (see "Generation").

## Responsibilities

| voxagent (Go) | The host (`internal/extension/host.rb`) |
|---|---|
| Reads the catalog, pre-flight, validation and exit status | Converts the catalog with Puppet's own `from_data_hash` and `to_ral`, so Puppet's validation and munging run |
| Builds the graph: stated edges, auto-relations, containment | Answers each resource's four auto-relation blocks |
| Orders the walk, and decides which resources are skipped for failed dependencies | Evaluates one resource with `ResourceHarness#evaluate`, and prefetches lazily |
| Propagates refreshes and writes Puppet's "Triggered", "Would have triggered" and "Failed to call" lines | Runs the `refresh` callback |
| Assembles the report: counts, metrics, resource statuses | Returns each status's events and the log lines Puppet emitted |
| Never calls the host under `--noop` for a refresh | Sets `Puppet[:noop]` for an `evaluate` |

The provider that
fronts the host is `ral.Extension` (`internal/ral/extension.go`). The
walk reaches it through the optional `ral.Evaluator` interface
(`internal/ral/evaluator.go`), so a status Puppet produced is recorded verbatim
and not rebuilt from `Change`s, which would log every line twice and re-word
Puppet's messages. `Apply` on an `Extension` refuses.

Classification is the host's `load` answer, not a scan of the plugin tree
(`cmd/voxagent/extension.go`, `ingestExtensions`). It runs once per run. When
the catalog has no non-native, non-container type, no host starts and the run
is exactly what it was before the host existed.

## The protocol

Transport and framing (`frame.go`): a 4-byte big-endian length, then a UTF-8
JSON object. A frame over 64 MiB (`MaxFrame`) is refused on both read and
write, and the length is checked before anything is allocated. A stream that
ends exactly between frames is `io.EOF`; inside a frame it is
`io.ErrUnexpectedEOF`. Exchanges are strictly one request, one reply.

Handshake: the host speaks first with `{"hello": {"protocol", "puppet_version",
"ruby_version"}}`. voxagent speaks protocol 1 (`Protocol`) and refuses another.

A request is an object with one key, the operation. A reply is an object with
one key: the operation, or `error`. Every reply body carries `logs`.

| Operation | Request | Reply body (`messages.go`) |
|---|---|---|
| `load` | `catalog` (the bytes voxagent read), `libdir`, `vardir` (voxagent's `--vardir`) | `types` (per type: `class` `tier3`, `tier2` or `missing`; `origin` `core` or `libdir`; `refreshable`; `eval_generates`; `self_refresh`, only ever on a `tier3` type), `metaparams`, `invalid` (a list of `{ref, message}`), `generated` (a generation, below: what the load-time `generate` pass added), `resources` (per ref: `provider_used`; `aliases`, the other refs the resource is known by; `puppet_ref` and `puppet_title`, Puppet's own ref and title when its type overrides title, which `checkLoad` requires to be a ref of the resource's own type that no other resource has; `unresolvable`, set only with `puppet_ref`, when Puppet's catalog cannot look the resource up by its catalog ref), `order` (every resource of the host's catalog, catalog and generated refs alike, in Puppet's catalog order after the generation pass), `logs`, `generate_logs`, `failed_to_generate` |
| `relationships` | `refs` | `relations`: per ref, `require`, `before`, `subscribe`, `notify`, each a list of the OTHER end's refs |
| `evaluate` | `ref`, `noop` | `events` (as `Event#to_data_hash`), `failed`, `skipped`, `unsuitable` (no suitable provider: failed, not skipped, no events, nothing logged), `invalidate_refreshes`, `logs` |
| `skip` | `ref` | the same shape as `evaluate` |
| `eval_generate` | `ref` | a generation, plus `existing` (the refs `eval_generate` returned that the catalog already held), `failed` (it raised), `logs` |
| `overly_deferred` | `ref` | `logs`: the overly-deferred handler's `Provider X is not functional on this host`, or none for a type with no provider, which `finish` then logs |
| `refresh` | `ref` | `refreshed`, `error` (the callback's message, or null), `logs` (the callback's own lines), `exception_logs` (`log_exception`'s lines for a failed callback; empty otherwise) |
| `finish` | none | `logs`: teardown's once-per-type "Could not find a suitable provider", then `post_resource_eval`'s |
| `shutdown` | none | `logs`, then the host exits 0 |

A generation (`Generation` in `messages.go`) is what one generator call did to
the host's catalog: `resources`, the new resources in catalog JSON shape
(`type`, `title`, `tags`, `exported`, `kind`, `parameters`); `containers` and
`generators`, each a map from a new resource's ref to its container's and to
the resource that generated it; `before`, the `{source, target}` values a
generator appended to `before`; and `overrides`, `{ref, parameters,
sensitive}` for each resource that already existed and whose parameters the
generator changed. There is no `generate` operation: `load` runs that pass.
`unsupported`, the earlier list of generating resources, is gone.

Points a reader will not guess:

- `load` is called once. It starts Puppet (`Puppet.initialize_settings` with
  run-private `--confdir` and `--codedir`, the configured `--libdir`, and
  voxagent's own `--vardir`), then classifies every non-container type before
  converting, because `to_ral` turns an unknown type without a `kind` into a
  component (`resource.rb:518-524`).
- The vardir is shared, as an agent run shares one, so what Tier 3 type code
  keeps under `Puppet[:vardir]` lands where the agent's does: simplib's
  `reboot_notify` keeps `reboot_notifications.json` there, and a filebucket a
  type backs up into lives there. `Puppet::Util::Storage` is never stored:
  the harness caches `checked` and `synced` in memory, and only
  `Catalog#apply` stores `state.yaml`, which the host never calls (D-6).
- Between reading the catalog and converting it, `load` runs Puppet's own
  `DeferredResolver.resolve_and_replace`, as `puppet apply --catalog` does
  (`application/apply.rb:357`): each `Binary` becomes its bytes and a
  top-level `Sensitive` is unwrapped into `sensitive_parameters`
  (`pops/evaluator/deferred_resolver.rb:87-118`, `:154-172`). Every other rich
  value (`Timestamp`, `Regexp`, `SemVer`, ...) stays Puppet's own object, which
  the type's code then handles as on a Ruby agent. It is Puppet's code rather
  than a Go port because the host must hand the type exactly what an agent
  would. voxagent refuses a `Deferred` before the host starts (D-51), so the
  resolver calls no function and needs no facts.
- `load` converts resource by resource and lists every refusal in `invalid`.
  Puppet stops at the first. This is an improvement over Puppet and is logged
  as I-13 in `improvements.md`.
- `load` also runs Puppet's load-time generation pass (see "Generation") and
  answers what it added as `generated`. A `generate` that raises is logged as
  Puppet logs it, makes the resource generate nothing, and sets
  `failed_to_generate`. Its lines are `generate_logs`; lines emitted while
  converting are `logs`. They are kept apart because they go to different
  places (see Lifecycle).
- The host answers refs in the catalog's own spelling. Puppet canonicalises
  `Class[main]` to `Class[Main]` when it reads one (`resource.rb:581-588`), so
  `convert` keeps a map from Puppet's ref to the catalog's.
- There is no `prefetch` operation. The host prefetches lazily inside
  `evaluate` and `skip`, once per provider class, as `transaction.rb:115` and
  `:252-257` do. A prefetch failure is therefore scoped to its provider class.
  `evaluate` on a resource of a failed class returns the event "Prefetch failed
  for T provider 'P'" with `skipped` true.
- `skip` is for a resource voxagent skips for failed dependencies. Puppet still
  runs `pre_process` for it (`transaction.rb:111-121`), so its provider class is
  still prefetched, and a failed prefetch still yields its failure event
  (`:333-339`). voxagent reproduces this through `skip`. It is a quirk, not
  something to simplify away: a resource that is never evaluated can still run
  its provider's prefetch.
- `invalidate_refreshes` travels beside the events because
  `Event#to_data_hash` drops it and `event_manager.rb:82` reads it.
- Events are serialised as the PuppetDB terminus serialises them, not as
  `Event#to_data_hash` alone would: `event_hash` (`host.rb`) first coerces
  the event's `status`, `property`, `desired_value`, `previous_value` and
  `message` strings to UTF-8, with invalid bytes replaced by U+FFFD, the
  coercion `tests/spine/lib/puppet/reports/gatejson.rb:23-104` reproduces from
  the terminus. Without it, an ASCII-8BIT String (a `Binary` parameter's
  bytes, or anything a provider reads as bytes) would be written as Base64
  (`pops/serialization/to_stringified_converter.rb:97-103`). It is the
  terminus's code because voxagent's report must carry what PuppetDB would
  receive, and the protocol accepts only valid UTF-8. The event gets new
  values; the strings it shares with its resource are left untouched.
- An `error` reply (`{"error": {"message": ...}}`) is for an exception outside
  Puppet's own rescue. It is `extension.CallError`, and unlike every other
  failure it leaves the host usable: an `evaluate` error fails that resource
  (a `resource_error`), a `load` error is could-not-run.
- Types are keyed lowercase. `load`'s `types` also classifies the types of
  generated resources, which the catalog may not name.

### Validation

`decodeStrict` (`strict.go`) is the deserialization point, and every reply goes
through it:

- bytes that are not valid UTF-8 are refused;
- a `\u` escape that is a lone UTF-16 surrogate is refused, because
  `encoding/json` would otherwise turn it into U+FFFD past the UTF-8 rule;
- an array with more than `MaxItems` (65,536) elements, an object with more
  than `MaxItems` members, or arrays and objects nested more than `MaxDepth`
  (64) deep are refused. `prescan` counts on the raw bytes, one counter per
  open container, before `encoding/json` decodes anything: a 64 MiB frame of
  `{}` would otherwise decode to tens of millions of values first;
- a field the Go struct does not declare is refused
  (`DisallowUnknownFields`);
- anything after the JSON value is refused;
- numbers decode as `json.Number`, so an integer reaches the report verbatim.

`host.go` then checks the decoded reply against the request:

- a reply has exactly one key, and it is the operation or `error`;
- the body is never `null`, which would decode as an empty successful reply,
  and an `error` body needs a non-empty `message`;
- a `refresh` reply either refreshed (`refreshed` true, `error` null) or
  failed (`refreshed` false, `error` set); the host never sends the other two
  shapes, nor `exception_logs` without an `error`;
- every ref the host names must be in the catalog voxagent loaded (`known`),
  including the targets of auto-relations, and `relationships` must answer
  exactly the refs asked;
- `load`'s types must be in the catalog, with class `missing` and no origin, or
  `tier2`/`tier3` with origin `core` or `libdir`;
- log levels must be Puppet's, and a log line number cannot be negative;
- event statuses must be `success`, `failure`, `noop` or `audit`; `property`
  must be a string or absent, `redacted` a bool or absent; an event needs a
  `name` and a `time`;
- a generation (`checkGeneration`, for `load` and `eval_generate` alike) adds
  only well-formed resources: a type and a title, no `[` or `]` in the type,
  the type capitalised as Puppet renders one (each `::` segment capitalised,
  the rest lower case, so `file[/x]` cannot sit beside `File[/x]`),
  `kind` `compilable_type`, not exported, never a ref already known, an alias,
  or added twice. A `containers` or `generators` key must be a resource it
  added, and the value a ref known or added by the same generation, as must
  both ends of a `before` value. An override must name a known resource, and
  mark sensitive only parameters it changes. `eval_generate`'s `existing` must
  be known. The new refs join the known set only once all of this passes, so
  every ref the host names is either in the catalog or made by that same
  generation, and is never looked up;
- each alias must be a well-formed ref of its resource's own type, not a
  known ref, and not another resource's alias (`checkAliases`);
- `load`'s `order` must name known refs, generated ones included, each once,
  the catalog's own refs in the catalog's order (Puppet only inserts generated
  resources), and, when `invalid` is empty, every one of them (`checkOrder`). A refused
  load's order may omit refs, as the host's catalog holds no resource whose
  conversion failed;

A violation is a protocol error: the host is killed (see Lifecycle).

**`MaxItems` is a functional limit.** It applies to every list, including the
log lines one call returns. A resource whose evaluation, refresh or prefetch
logs more than 65,536 lines in one call is a protocol error and kills the host,
like any other violation; it is not raised silently.

**Case-insensitive keys.** `encoding/json` matches field names without regard to
case, so `{"Failed": true}` fills `failed`. This is left alone on purpose: Ruby's
JSON never emits a key in another case, and a case-variant key cannot carry a
value the declared field could not. It is recorded at `decodeStrict` in
`strict.go` and here so nobody mistakes it for an oversight. The `error` and
operation keys are matched exactly, because they are map keys.

## Lifecycle

`extension.Launch` (`script.go`) stages and starts the host. The sequence:

1. The Ruby path must be absolute. It is configuration and is never looked up
   on `PATH`.
2. `host.rb`, embedded with `//go:embed`, is written to a fresh `0700`
   directory from `os.MkdirTemp`, with a `0700` `state` directory beside it. The
   written bytes are read back and their SHA-256 is compared with
   `HostScriptSHA256`. A mismatch removes the directory and refuses.
3. The host is run as `ruby host.rb <state-dir>`, with the environment from
   `HostEnv` (`env.go`).
4. `Close` removes the directory.

**Environment.** Exactly `PATH`, `HOME`, `LANG`, `LC_ALL`, `GEM_HOME` and
`GEM_PATH`, taken from voxagent's own, and nothing else. When none are set the
host gets an empty environment, never an inherited one (`exec.Cmd` reads a nil
`Env` as "inherit"). `GEM_*` passes because a gem-managed Ruby cannot find the
puppet gem without them (measured on the G1 host's rvm Ruby); openvox's own Ruby
does not need them. `RUBYLIB`, `RUBYOPT` and `LD_PRELOAD` do not pass, because
they change what Ruby loads. `HOME` passes because the agent process keeps it;
Puppet strips it only from the commands it executes (`util/execution.rb:387`),
and the host's Puppet already does that.

**Channels.** The host's stderr is voxagent's stderr. The protocol runs on
private duplicates of the host's stdin and stdout, taken before anything else
(`host.rb`, the last block). Fd 1 is then pointed at stderr and fd 0 at
`/dev/null`, and `ARGV` is consumed, so module code that prints, reads `$stdin`
or calls `Kernel#gets` cannot corrupt the framing or read the state directory.
Ruby opens descriptors close-on-exec, so commands Puppet runs do not inherit the
duplicates. `TestHostSurvivesChattyProvider` pins this with `vox_value 'chatty'`.

**Timeouts.** Every call is bounded. `evaluate`, `skip`, `refresh` and
`eval_generate` use `--extension-evaluate-timeout` (default 1h, because
`evaluate` includes the lazy prefetch). Every other call, `load` and its
generation pass included, the handshake, and each half of `Close` use
`--extension-timeout` (default 5m).

**Death, without restart.** Any I/O error, timeout, protocol error or
validation failure goes through `Host.fail`, which records the first cause,
kills the process and closes the parent's ends of the pipes. Closing the pipes
is what unblocks a pending read, because a descendant of the host may hold the
other ends open past the kill. The failure is sticky: every later call returns
it. There is no restart, because Puppet's prefetch cannot be replayed mid-run.
The resources not yet evaluated then fail with a `resource_error` naming the
cause, their dependents are skipped, and native resources keep applying.

**Close is time-bounded.** A healthy host is sent `shutdown`; a dead one is
already killed. Then stdin is closed and the process is waited for, for at most
`--extension-timeout`, after which it is killed. `Close` is idempotent. It runs
before the `outcome=` line, which must stay the last line of stderr
(`cmd/voxagent/main.go`, `done`).

**Where logs go.**

| Lines | Destination |
|---|---|
| `load`'s `logs` (emitted while converting, before any report exists) | stderr, as `voxagent: extension host: <level>: <message>` |
| `load`'s `generate_logs` | the report, first, as Puppet's `generate` runs at the start of `Transaction#evaluate` (`transaction.rb:99-100`) |
| `evaluate`, `skip` and `refresh` logs | the report, with the resource's status |
| `overly_deferred`'s logs | the report, when the walk fails the overly deferred resource, with its status |
| `eval_generate`'s `logs` | the report, at the generating resource's turn, before its new resources |
| `finish`'s logs (teardown: no suitable provider, `post_resource_eval`) | the report, last, as in Ruby's |
| `relationships` logs | validated and dropped |

`failed_to_generate`, and an `eval_generate` answering `failed`, are carried into
the report because Puppet fails the run for them (`transaction.rb:197`).

**Self-report.** After the walk voxagent prints one stderr line: host start-up
time, resources evaluated, time spent in the host. At
ingest voxagent also prints, per Tier 3 type, `N <type> resource(s) applied via
extension host (core|libdir)`. That line is the fallback rate.

**Failure cases before the walk** (`ingestExtensions`; all are could-not-run
with nothing applied):

- `--no-ruby` with a catalog that needs the host, naming the types;
- the host cannot start (no Ruby, bad handshake, hash mismatch);
- `load` or `relationships` fails;
- `generated` cannot be merged into voxagent's catalog (`mergeGeneration`,
  `cmd/voxagent/generation.go`), which would be a voxagent logic error, since
  the host already validated it;
- `order` cannot be applied to it (`applyOrder`, the same file), for the same
  reason;
- `invalid` is not empty: each is printed as Puppet's refusal. If voxagent's own
  `Validate` also refused the resource, voxagent's message is used.

A `tier2` (Resource API) type is not given a provider, so pre-flight refuses it
as unimplemented; stderr names it as a Resource API type.

## Auto-relations

`ral.AutoRelating` returns typed relations (require, before, subscribe,
notify). `transaction/build.go` (`autoRequireEdges`) merges them as
`graph/relationship_graph.rb:172-212` does:

- the first edge joining a pair wins, and an edge stated in either direction
  suppresses an automatic one. Automatic edges added earlier count too;
- per resource, in catalog order, the kinds run require, subscribe, before,
  notify;
- require and before edges carry no refresh; subscribe and notify refresh on
  every event.

The host answers `relationships` for every Tier 3 resource, and its answers are
already filtered to resources in the catalog (`type.rb:2076-2113`). The
native `File` also autorequires `User[owner]` and `Group[group]` by name
(`type/file.rb:403-418`; `internal/ral/file.go`), skipping an owner that is all
digits, and the native `exec` autorequires `User[user]` for a `user` that is
not numeric (`type/exec.rb:642-648`; `internal/ral/exec.go`). `user` and
`group` are themselves native on the platform their contract was measured on
(`ral.NativeUserGroup`, apply-spine.md's `user` and `group` provider section)
and Tier 3 everywhere else, so these edges join two native resources there
and cross the boundary elsewhere; the graph orders them the same either way.
A catalog with a `User` whose `purge_ssh_keys` is anything but `false`, `"false"` or
`[]` sends every `User` to the host, so a native `Group` and host-applied
users can share one catalog (the mixed-routing case of
`tests/spine/report-diff-extension.sh`).

**No suitable provider.** Puppet never evaluates a resource that is not
`suitable?` (`type.rb:1923-1939`): `relationship_graph.rb:116-137` defers it,
and when a whole drain of the ready queue places nothing else
`transaction.rb:124-136` marks it failed, not skipped, with no events and no
time, whatever its dependencies did. `evaluate` and `skip` both answer
`unsuitable` and log nothing; the walk sets the resource aside
(`graph.Walker`) and asks again at its next offer.
When the walk hands it back overly deferred, it calls `overly_deferred`, which
does what Puppet's handler does: a chosen but unsuitable provider is logged
then (`Provider X is not functional on this host`); a type with no provider
at all is logged once, at teardown (`Could not find a suitable provider for
X`), which the host emits from `finish`. The walk records the resource's
status, blocks its dependents, and never refreshes it. A member of an
`eval_generate` unit is not deferred but failed at once (D-56 in
apply-spine.md).

A failed refresh is logged in `event_manager.rb:160-165`'s order: the
callback's own lines, then the walk's "Failed to call refresh: ..." line, then
the lines `resource.log_exception` wrote. The host returns the first and last
groups apart (`logs`, `exception_logs`) so the walk can put its line between
them. G1 compares logs as a multiset, so it cannot see this order;
`TestEvaluatorRefreshFailure` and `TestRealHostRefreshFailure` hold it.

A Tier 3 type that does not respond to `refresh` (`refreshable` false) gets no
refresh and so no "Would have triggered" line (`event_manager.rb:73`).

A type declared `Puppet::Type.newtype(:x, self_refresh: true)`
(`type.rb:2186-2189`; `mount_core`'s `Mount`) answers `self_refresh` in
`load`'s `types`, and `checkLoad` refuses it on any class but `tier3`, because
`register_type` never passes it. After such a resource is evaluated, unless it
failed or its `ensure` is `absent`, the walk queues one refresh to the resource
itself per distinct event name, noop events included, as `event_manager.rb:79`
does. Those count beside what other resources sent, and the resource is
refreshed through `refresh` in the same step. A self-refreshing type with no
`refresh` method is D-58 in `apply-spine.md`.

## Generation

**Where it happens, and why it is Puppet's code.** Puppet generates in two
places. Before the walk, `Transaction#evaluate` runs `generate` on every
resource (`transaction.rb:99-100`), and the generator recurses into, tags,
contains and orders what it makes (`additional_resource_generator.rb:21-53`,
`:131-211`). During the walk, `eval_generate` runs at a resource's turn
(`:56-75`). The host runs both through Puppet's own
`AdditionalResourceGenerator`; voxagent never reimplements a generator. The
load-time pass is part of `load` (`generate_all` in `host.rb`) and covers every
resource in the host's catalog, native ones included; of voxagent's five native
types none defines `generate` (in Puppet 8.10.0 only `user`, `resources` and
`tidy` do). `eval_generate` runs when the walk asks for it. voxagent observes
the effects: `observe` in `host.rb` reports what the generator call did to the
host's catalog (resources added, each one's container, `before` values
appended, parameters changed on resources that already existed). Puppet keeps
no record of which resource generated which, so the host prepends a module to
its own generator instance that records `add_resource`'s `parent_resource`
(`:137`) and calls `super` with the same arguments.

**At load.** `ingestExtensions` merges `generated` into voxagent's catalog right
after `load` (`mergeGeneration`, `cmd/voxagent/generation.go`). New resources
are appended, each `containers` entry becomes a containment edge, `before`
values are appended to the source's `before`, overrides replace parameters, and
each new resource's `GeneratedFrom` is set from `generators`, so its report
status has Ruby's null `file` and `line` (`resource/status.rb:169-170`). When
`invalid` is empty, the catalog's resources are then put in the host's `order`
(`applyOrder`): Puppet's `add_resource` puts each generated resource beside its
parent in the catalog array, before it for a depth-first type such as `tidy`
and after it, in generate order, otherwise (`additional_resource_generator.rb:36-50`,
`resource/catalog.rb:99-124`), so the walk's catalog-order keys follow Puppet's.
The needed types are then recomputed, because generation can add types the catalog
did not name. The merged catalog goes through pre-flight and validation like
any other, so a generated resource voxagent cannot apply refuses the run before
anything is applied. An override's `sensitive` marks are kept: `mergeGeneration` adds them to the
resource's own (`Resource.MarkSensitive`), so the merged catalog's event for
that property is redacted (D-8).

**In the walk.** A resource whose provider is a `ral.EvalGenerator` and whose
type has `eval_generates` is a unit, as a recursing native `File` is
(`visitHostUnit`, `internal/transaction/transaction.go`). At its turn, before
the walk decides whether to skip it for failed dependencies, the walk calls
`eval_generate`:

- the answer's logs go into the report at the resource, and `failed` sets
  `failed_to_generate`. A host call that fails is logged at the resource as
  `Failed to generate additional resources using 'eval_generate': <error>`, and
  sets it too;
- each override is applied to the resource if the walk has not reached it yet,
  which is then judged again, as pre-flight would judge it: a native resource
  by its provider's gaps and `Validate`, a resource the host evaluates only by
  the metaparameters the walk would have to honour. A resource that fails is
  refused at its turn, with the reason, as a resource error. An override of a
  resource already applied changes nothing;
- the new resources are applied after the resource, each through its own
  type's provider (`generatedProvider`; a type the catalog never named is
  prefetched at its first new resource, as `transaction.rb:243-260` prefetches
  lazily), and each is judged first, because pre-flight never saw it. They are
  applied in the order `eval_generate` returned them, except that a `File`
  waits for its nearest generated `File` ancestor (`ancestorsFirst`), which is
  the order of Puppet's priorities and ancestor edges
  (`additional_resource_generator.rb:119-135`). A failed new resource blocks
  the generating resource's dependents;
- `existing` and `before` are dropped (`ral.Extension.EvalGenerate`), and so
  is `generators`: every new resource is marked generated by the resource the
  walk asked, which is the only parent the host passes to `add_resource`.

The registered limits, in `apply-spine.md`:

- **D-43, the sentinel.** Puppet moves the generating resource's other
  out-edges onto a `completed_<title>` sentinel that waits for everything
  `eval_generate` returned, and makes those resources depend on it. voxagent's
  order is fixed before the walk, so it does neither, and does not apply
  `before` values an `eval_generate` appends. Unobservable for `concat`.
- **D-42, children voxagent cannot apply.** A new resource is a resource error,
  where Puppet applies it, when its type has no provider in voxagent at all,
  when its judgement refuses it, or when it generates in turn: nested
  generation is run at the new resource's turn, and a new resource whose own
  generation yields resources is refused rather than applied without them. A
  nested generator whose turn is skipped never generates, so its logs,
  `failed_to_generate` and overrides are lost.

**Aliases.** `load` reports, per resource, the other refs it is known by: a
title other than its name makes the name an alias (`catalog.rb:184-219`). The
host leaves out an alias equal to the resource's own ref, as a compiled
`Class[main]` with `name => main` has. `ral.Extension` implements
`ral.Aliasing`, and `resourceAliases` (`internal/transaction/build.go`) adds
them to `File`'s path aliases, so a relationship naming the alias reaches the
resource. A generated ref equal to an alias is a protocol error.

**`loglevel => notice`.** `notice` is Puppet's default level, and `concat`
copies it onto every `File` it generates. Pre-flight (`ral.ResourceGaps`) and
`ral.MetaparamGaps` accept exactly that value on any type; any other `loglevel`
is still a gap (`internal/ral/check.go`).

## Updating `host.rb`

`host.rb` is pinned by hash, so changing it takes three steps:

1. Edit `internal/extension/host.rb`.
2. Set `HostScriptSHA256` in `internal/extension/script.go` to
   `sha256sum internal/extension/host.rb`. `TestHostScriptHashIsPinned` fails
   until you do.
3. Run the Ruby tests with a Ruby that can load Puppet, and with the variable
   that makes their absence a failure:

   ```bash
   VOXAGENT_REQUIRE_RUBY=1 go test -count=1 ./internal/extension/...
   ```

Without `VOXAGENT_REQUIRE_RUBY` the tests in `ruby_test.go` skip when no such
Ruby is found (`extensiontest/ruby.go`), and a skip proves nothing about
`host.rb`. `task test` sets the variable through `scripts/run-tests.sh`, so the
suite cannot pass by skipping them. Then run `task dev:report-diff-extension`.

Keep the invariants in the file's own comments: the protocol duplicates are
taken before `require 'puppet'`; `load` runs once; citations are
`path:line` against Puppet 8.10.0.

## The fixture plugin tree

`internal/extension/testdata/libdir/puppet/` holds the host's test types. It
is a pluginsync-style `libdir`: the unit tests point `load` at it, and
`tests/spine/report-diff-extension.sh` copies it into the `libdir` both agents
load from.

| Type | What it exercises |
|---|---|
| `vox_value` | One property backed by a file; `value => fail` makes the setter raise, `chatty` prints to `$stdout`, `STDOUT` and a child process, `die` kills the host. A `refresh` that appends to a log file (and, for `refusing-refresh`, logs a notice then raises). All four auto-relations: autorequires the parent directory, autobefore `Notify[after-<name>]`, autosubscribe `Notify[before-<name>]`, autonotify the `peer` |
| `vox_prefetch` | Two providers. `good` prefetches successfully, and logs from `post_resource_eval`; `bad`'s prefetch raises. Exercises per-provider-class prefetch failure, the skip of the class's later resources, and `finish` |
| `vox_conf` | One provider, `never`, confined away (`confine false: true`). A resource naming no provider has none, one naming `never` has an unsuitable one; both exercise Puppet's deferral of unsuitable resources (`relationship_graph.rb:116-137`, `transaction.rb:124-146`, D-37) |
| `vox_gen` | `generate` returns a `Notify` when `make` is true, sets `before => Class[main]` and returns nothing when `make` is `before`, returns nothing otherwise, and raises when `make` is `raise`. Exercises the load-time generation pass, a generator's appended `before`, and the logged-not-fatal failed generate |
| `vox_evalgen` | `eval_generate` returns `make` (default 1) `Notify`s named `<title>-child-<n>`, and raises when `make` is `raise`. Exercises `eval_generate`, its order and its failure |
| `vox_die` | Its provider kills the host when its value is set, so a host lost mid-run is tested end to end |
| `vox_rapi` | Registered with `Puppet::ResourceApi.register_type`, so `load` classifies it `tier2` |

The G1 gate also uses `stdlib`'s `file_line` and `anchor`, and `concat`, from
the vendored modules (`task fleet:vendor`).

## What the gates prove

`task dev:report-diff-extension` (`tests/spine/report-diff-extension.sh`;
part of `task check`) compiles one catalog, applies it with Ruby's
`puppet apply --catalog` and with voxagent from identical starting state, and
compares the reports with `gatekit compare-reports` and the same allowlist
`dev:report-diff` uses. Exit 0 agrees, 1 diverges, 2 could not run, so a
missing `puppet`, `ruby` or vendored stdlib is never mistaken for agreement.

- **G1** compares the first applies. Besides evaluation it covers generation:
  `concat_file`'s load-time `generate` of its `File`; `concat`'s
  `eval_generate` filling that `File`'s content from two fragments declared out
  of order, and, for a `concat` with no fragments and `create_empty_file =>
  false`, overriding its `ensure` to `absent` over a seeded file;
  `vox_evalgen`'s two new resources and its raising `eval_generate`;
  `vox_gen`'s generated `Notify`; and `Notify['needs-alias']`'s `require` of
  `Vox_value['vn']`, an alias of `Vox_value['vt']`. One fragment is
  `Sensitive`, which makes Ruby redact the generated `File`'s content event;
  voxagent redacts it the same way through the override's marks (D-8).
- **G2** compares each side's second apply with the other's, and then asserts
  that the resources which converge changed nothing in voxagent's second report.
  `vox_prefetch`'s `good` provider never persists state, so `Vox_prefetch[ok]`
  changes on every run under both agents and the whole fixture cannot be held
  to "changed nothing"; the comparison alone would pass a resource that failed
  to converge under *both* agents. So `File_line[add]`, `Vox_value[refreshed]`,
  `Vox_value[notifier]`, `Vox_value[refuser]` and the files that converge must
  have changed in the first report and be absent (both sides drop a resource
  nothing happened to, as PuppetDB does) or unchanged in the second; `Anchor[mid]`,
  the target directory and the seeded file must be unchanged in both.

Last, both seeded files `concat` replaces or removes are bucketed under its
default `backup => 'puppet'`, and the two agents' clientbuckets must be
byte-identical. Ruby's must hold `c.conf`'s seeded content, or the gate could
not run (exit 2); a voxagent that made no clientbucket diverges (exit 1).

The comparison reports 1037 and 553 leaves compared with 0 divergent (measured
2026-10-08); the gate table in `apply-spine.md` records what it does not prove,
including `--noop`.

Tier 3 under `--noop` is not yet gated: a `--noop` pass of this fixture agreed on
every Tier 3 leaf but diverges on its native resources (D-10 and D-40 in
`apply-spine.md`; D-41, a third, is retired), so it stays out of the gate until
those native defects are fixed.

## What the host refuses

`tier2` (Resource API) types and types that are `missing`, at pre-flight. A
resource generated at load that voxagent cannot apply refuses the run at
pre-flight like a catalog resource; one `eval_generate` makes is a resource
error (D-42).

See also the SIMP gate (`dev:report-diff-simp`, "The SIMP gate" in
`apply-spine.md`) and the performance note.

## Divergences

The extension host's registered divergences are in `apply-spine.md`: D-36 (a
Tier 3 resource skipped for a failed prefetch has `evaluation_time` `0` where
Ruby writes `null`), D-38 and D-56 (a resource with no suitable provider), and
D-42 and D-43 (generation's limits, above), D-51 (a `Deferred` is refused
even on a Tier 3 resource), D-55 (a relationship to a title Puppet cannot look
up, which `unresolvable` reports) and D-58 (`self_refresh` without `refresh`). The improvement it
records (listing every conversion refusal) is I-13 in `improvements.md`.
