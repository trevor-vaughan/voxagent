# Units

For maintainers. Operators want the README (`voxagent units`). This page says how the code that exists works: what a
unit is, how units are laid out and routed, and how to add one. Go citations are files in this repository.

## What a unit is

A unit is a Go package under `internal/units/<name>` that reproduces one Ruby origin: either Puppet itself (one unit per
core type: `exec`, `file`, `group`, `notify`, `package`, `service`, `user`) or one Ruby module (`concat`, `augeas_core`,
`augeasproviders_sysctl`, `augeasproviders_ssh`). `augeasproviders_core` is a library unit: it serves no type, and holds
the run object, mixin and fingerprinted files its two dependents share. A unit supplies two things through
`unit.Unit` (`internal/unit/unit.go`):

- `Manifest()`: what it reproduces and which Ruby files it was measured against.
- `Route(env *Env) Decision`: per run, which catalog types it serves natively (`Decision.Providers`) and, for each
  catalog type it leaves to Ruby, why (`Decision.Delegated`). `Delegated` holds only types the catalog contains.

Every unit package exports `Manifest` (a `unit.Manifest` variable in its `manifest.go`) and `Unit` (the `unit.Unit`
that `units.All()` lists). Package clauses drop underscores (`augeasproviders_core` is `augeasproviderscore`);
`internal/units/package` is `pkg` because `package` is a keyword.

## Layering

```text
internal/units/<u>  ->  internal/unit  ->  internal/ral  ->  internal/lib/*
```

`TestImportsWithinRequires` (`internal/units/units_test.go`) reads the imports of every Go file and enforces:

| Package | May never import |
|---|---|
| `internal/lib/*` (including `lib/raltest`, test support) | `internal/ral`, `internal/unit`, `internal/units/*` |
| `internal/ral` (its own files, not subdirectories) | `internal/unit`, `internal/units/*` |
| `internal/unit` | `internal/units/*` |
| `internal/units/<u>` | another `internal/units/<r>` unless `<r>` is in `<u>`'s `Manifest.Requires` |

The same test fails when a directory under `internal/units` is not in `units.All()`. A unit's own subpackage
(`augeasproviders_core/augeastest`) counts as the unit.

Consequences worth knowing before you move code:

- `internal/ral` is the engine: providers' interface, evaluator, transaction support, the extension-host type
  (`extension.go`). `TestRalHoldsNoTypes` fails if any other non-test file declares a `Type() string` method.
  `internal/ral/augeas_checks.go` is the one place type-neutral augeas routing checks live (`AugeasRunChecks`,
  `AugeasProviderDir`, `HeldAugeas`), because both augeas units use them.
- `internal/unit` cannot import `internal/units`, so `unit.RouteAll` takes the list as a parameter and
  `internal/units/units.go` (`units.All()`) supplies it. `units` is the only package that imports every unit.
- A library cannot import `ral`, so a helper a library needs that is pure (no engine state, no engine-owned interface)
  lives in the library and `ral` imports it (`puppetpath` is one). `plugintree.LibdirTree` is an example of the
  other direction: the cgo probe in `lib/augeas` reads the tree, so the implementation is in `internal/lib/plugintree`;
  only `lib/augeas` and `cmd/voxagent/apply.go` import it, and `ral` holds just the `PluginTree` interface.
- A package's internal tests cannot import a test-support package that imports that package. Such a test is an
  external test (`package x_test`) or moves to the package it really tests.

### Shared libraries (`internal/lib`)

| Package | Holds |
|---|---|
| `rubyvalue` | Ruby/Puppet value rendering and coercion (`Inspect`, `Integer()` parsing), parameter access, `UnimplementedValue` |
| `puppetpath` | Puppet's clean-absolute-path and file-mode rules, used by `file`, `concat`, `exec`, `augeasproviders_core` and `ral` (`augeas_checks.go`) |
| `sources` | the per-resource `Sources` context value |
| `command` | running external commands, output capping, shadow and argv runners, `ExecFailure`, executable checks |
| `nameservice` | libc NSS access through cgo (`nss_linux.go`) for `user` and `group` |
| `plugintree` | `LibdirTree`, reading the plugin tree under `--libdir` |
| `augeas` | the libaugeas `dlopen` probe and `Session` (the probe is made at most once per run) |
| `raltest` | test support: fakes, catalog builders, the libaugeas trace replay (`trace.go`). Never imports `ral` or a unit |

`augeasproviders_core/augeastest` is test support inside a unit, not a library: it needs the unit's `Run`, so it cannot
live in `lib`.

## The manifest

`unit.Manifest` (`internal/unit/unit.go`):

| Field | Meaning |
|---|---|
| `Name` | the unit; equals its directory name under `internal/units` |
| `Version` | the unit's own semver, independent of voxagent's (see "Versioning") |
| `Source` | `Origin{Implementation, Version}`: the Ruby implementation reproduced. Core units: `puppet` `8.10.0`. Module units: the module name and an empty `Version` (the versions are in `Releases`) |
| `Oracle` | `Origin` the behaviour was measured against: `openvox` `8.33.0` for every unit |
| `Types` | the catalog types the unit serves, lower case. Empty for a library unit |
| `Requires` | names of units this one imports; the layering test allows imports of exactly these |
| `Releases` | `[]Release{Version, Files, Types}`: one entry per measured version of the module's Ruby files |

`Release.Files` lists `FileDigest{Path, SHA256}` (lowercase hex, path relative to the plugin tree). `Release.Types` maps
each served type to the files that must all match for that type: a path in this release's `Files`, or
`"<unit>:<path>"` for a file listed by a required unit's manifest (`augeasproviders_ssh` uses
`augeasproviders_core:puppet/type/augeasprovider.rb`). Manifests may leave a slice nil; `nonNil` in `cmd/voxagent/units.go` is the only thing that makes the units command print
empty ones as `[]` rather than `null`.

Digests are written once, in the `manifest.go` of the unit whose release lists them. `puppet/feature/augeas.rb` is
listed by both `augeas_core` and `augeasproviders_core` because both measured it.

### The origin rule

`Source` is what the Go reproduces; `Oracle` is what the test evidence was taken against. A core unit's `Source` is
Puppet's own release (no files to fingerprint, so `Releases` is empty). A module unit's `Source.Version` is empty and its
`Releases` is not: a module's Ruby may be any version a site ships, so the unit says which releases it matches and
delegates the rest. `TestManifestsWellFormed` refuses a manifest with neither a `Source.Version` nor a release.

## Routing

`cmd/voxagent/apply.go:nativeProviders` builds one `unit.Env` per run (catalog, `Libdir`, `Tree` from
`plugintree.LibdirTree`, `Platform`, `Environ`, `Command`, `Vardir`, `Sources`, and `Augeas`, a `*augeas.Session` over the
run's libaugeas probe) and calls `unit.RouteAll(env, units.All())`.

`RouteAll` (`internal/unit/route.go`) sorts units by name, calls each `Route` once, concatenates the providers in that
order and merges the `Delegated` maps. It panics if two units delegate the same type, because that is a bug in the
units. `nativeProviders` returns RouteAll's providers followed by `applyOptions.providers` (extra providers only tests set), and returns `Delegated` to `applyCatalog`, which sends
the delegated types to the extension host and, under `--debug`, logs `<type> applied via extension host: <reason>` for
each, sorted by type (`cmd/voxagent/apply.go`).

### Fingerprinting: `unit.MatchFiles`

Module units decide whether the libdir's plugin tree is one they measured with
`MatchFiles(tree, manifest, typ, requires)` (`internal/unit/match.go`). It tries each release that lists `typ` and
returns the first whose files all read from the tree with the measured sha256. Every file of one release must match, so a
tree mixing two releases matches neither. Otherwise it returns the reason the last release tried failed:

- `<typ>: no measured release lists it`
- `<path>: not listed in the <unit> manifest`
- `<path>: <read error>` (missing, unreadable, oversized)
- `<path> has sha256 <got>, not the measured <want>`

That text is the debug line's reason, verbatim. `requires` supplies the manifests a `"<unit>:<path>"` reference resolves
in. `concat` matches `concat_file` and passes `nil` for `requires` (`concat_route.go:nativeConcat`); the two augeasproviders
units pass `core.Manifest`.

### After the fingerprint

Each unit's remaining checks are its own, and every failure is a `Delegated` reason, never an error:

- `concat`: value shapes the native parser does not implement (`rubyvalue.UnimplementedValue`) and two `Concat_file`s
  sharing a path (`concat_route.go`).
- `user`, `group`: the measured platform and libuser (`group.UnmeasuredReason`), a `provider` other than
  `useradd`/`groupadd` (`group.ForeignProvider`), and value shapes the native parser does not implement; `user` also
  `purge_ssh_keys`. `user` requires `group`, whose helpers it calls.
- `augeas_core`, `augeasproviders_sysctl`, `augeasproviders_ssh`: fingerprints, then provider directory
  (`ral.AugeasProviderDir`), then the environment (`ral.AugeasRunChecks`: no `AUGEAS_*`/`LD_*` override, clean
  `--libdir`), then the probe, then value shapes, then (sysctl) the measured release (`ral.MeasuredRelease`) and an executable `sysctl` on `PATH`, `/usr/sbin` or `/sbin` (`resolveSysctl`: `PATH` entries first, then those two, first executable wins). The probe comes after the
  environment checks so libaugeas never reads an `AUGEAS_*` override. `Env.Augeas.Probe()` makes the probe at most once
  per run however many types hold, and the sysctl and ssh units get one shared run from `core.SharedRun(env.Augeas, libdir)`
  (`augeasproviders_core/augeas_mixin.go`, whose `Run.EndRun` closes every handle).
- `exec`, `file`, `notify`, `package`, `service`: native on every run.

D-87, D-89 and D-91 in [apply-spine.md](apply-spine.md) say what is measured and why each condition delegates.

## Versioning and changelog

Each unit starts at `1.0.0` and bumps only when its own behaviour or manifest changes; it is independent of voxagent's
version. The changelog is a `Changelog:` line in the unit's package doc comment (for example `internal/units/file/file.go`:
`1.0.0 — moved from internal/ral`). `TestManifestsWellFormed` checks `Version` is semver; nothing checks the changelog,
so a reviewer must.

## Adding a unit

1. Create `internal/units/<name>/` with the type code, a package clause that drops underscores, and a package doc with a
   `Changelog:` line.
2. Write `manifest.go`: `Manifest` (version `1.0.0`, `Source`, `Oracle`, `Types`, `Requires`, `Releases`; use `[]string{}`
   and `[]unit.Release{}` rather than nil), `Unit`, and a `Route`. For a module, put the measured Ruby files under
   `testdata/lib` (so they are in the layout `--libdir` has) and list their digests in `Releases`; use `unit.MatchFiles`
   in `Route`.
3. Register it in `units.All()` (`internal/units/units.go`), in name order.
4. Add a row for each type to `nativeFixtures` in `internal/units/units_test.go`: the minimal resource that makes the type
   route native.
5. Import only `ral`, `lib/*`, `unit` and the units named in `Requires`. If a helper you need is in `ral` and pure, move it
   to a library rather than breaking a layering rule.

The unit-wide tests then impose, with no further wiring:

| Test (`internal/units/units_test.go`) | Requires |
|---|---|
| `TestImportsWithinRequires` | the layering above; the unit is in `units.All()` |
| `TestManifestTypesAreRoutedTypes` | with every unit's `testdata/lib` assembled into one libdir, `RouteAll` returns a provider for each `Types` entry and delegates nothing |
| `TestDigestsOwnedByManifests` | no empty digest; each digest appears in the Go sources only in the `manifest.go` of the units listing it |
| `TestManifestsWellFormed` | unique names, semver, directory exists, `Requires` are listed units, module units have releases; for a unit that has releases, `Types` equals the union of its releases' types and no type lists no files (a core unit with no releases is not checked) |
| `TestRalHoldsNoTypes` | no `Type() string` in `ral` outside `extension.go` |

The `voxagent units` output (README) is the manifests, so a new unit appears in it with no further change
(`cmd/voxagent/units.go`, tested by `cmd/voxagent/units_test.go`).
