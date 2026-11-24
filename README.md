# voxagent

A Go Puppet agent with opportunistic Ruby fallback.

This repository contains **P0**, `voxdiff` and the fleet harness, and **P1a**,
the `voxagent` apply spine.

## voxagent

Applies a Puppet catalog: reads it from a file, refuses anything it cannot fully
implement, applies the rest, and writes a `report_format 12` report.

```bash
task build
./bin/voxagent apply --catalog catalog.json --detailed-exitcodes
```

Three resource types, each with a bounded attribute set — `notify` (`message`),
`file` (`ensure`, `content`, `mode`, `owner`, `group`), and `exec` (`command`,
`path`, `cwd`, `returns`, `refreshonly`, `creates`, `onlyif`, `unless`).
Anything else is refused **before** anything is applied, so a run either applies
the whole catalog or changes nothing. Declaring an attribute is a promise to
read it, so the refusal list is the honest statement of coverage — and doubles
as the porting roadmap.
Some catalogs Puppet itself would refuse are refused the same way, after the
coverage check and before anything is applied, and reported in Puppet's own
words. Today: a `file` whose path is not absolute, and an `exec` with a
relative `cwd` or `creates`, or an unqualified command and no `path`.

Exit codes follow Puppet's, measured rather than assumed. Plain `puppet apply`
exits 0 for changes *and* for failures — its status says only that the tool ran
— so voxagent does the same, and `--detailed-exitcodes` carries the verdict as
Puppet's 0/2/4/6 bitmask. "Could not run" takes the value each mode leaves free:
`2` plain, `1` under the flag. Every run prints `outcome=` to stderr so the
status never has to be decoded from the argv.

### Gates

```bash
task dev:report-diff         # voxagent's report vs Ruby's, identical catalog bytes
task dev:idempotence         # a second apply must change nothing
task dev:derive-run-to-run   # re-measure report-diff's allowlist against Ruby
task dev:state-diff          # both agents, two containers, same resulting state
task dev:report-diff-container  # the report comparison for package and exec, in a container
```

The first two run inside `task check`. `dev:state-diff` and
`dev:report-diff-container` need a built container image and take minutes, so
they are run explicitly, the way `task fleet:gate` is. The container report
differential exists because the host is not a valid oracle for `package` or for
`exec`, and for one underlying reason: the host's Ruby cannot fork under Puppet.
So Puppet's `dnf` provider confines itself out and falls back to `dnfmodule`,
which manages dnf *modules*, while every `exec` dies with `Errno::EBADF` before
running anything. Inside the container both work, and it is the only gate that
compares an `exec` event — `executed_command`, `returns` as the property a
non-zero status is attributed to, the refresh callback event — against Ruby.

See [the maintainer notes](docs/maintainers/apply-spine.md) for the divergence
register, the known defects, and what each gate does and does not prove.
Measured speed and memory against Ruby are in [the performance notes](docs/maintainers/performance.md).

## voxdiff

Compiles a Puppet catalog twice and reports any divergence, ignoring the two
fields that legitimately vary between compiles.

### Build

```bash
task build
```

### Compare two compiles

```bash
./bin/voxdiff compare \
  -node web01.example.com \
  -environmentpath /etc/puppetlabs/code/environments \
  -environment production
```

Exit codes: `0` identical, `1` diverged, `2` operational failure. The
distinction matters in CI — "the catalogs differ" is a result, "the compile
broke" is not.

Add `-json` for machine-readable output, and `-ignore-source-locations` when
comparing across a manifest reorganisation.

### Compare two implementations

`-left` and `-right` select the binary for each side, so the same tool that
verifies Ruby against itself today will verify Go against Ruby later:

```bash
./bin/voxdiff compare -node web01 -left puppet -right /usr/bin/openvox
```

### Scan for non-deterministic constructs

```bash
./bin/voxdiff scan /etc/puppetlabs/code/environments/production/modules
```

Flags exported resources, exported collectors, `generate()` calls, and I/O in
custom functions — the cases where compile determinism stops holding.

Exit codes: `0` clean, `1` findings, `2` incomplete — a file could not be
read or scanned, so the tree was not fully reviewed and the result should
not be trusted. Exit `2` can happen alongside real findings on stdout: an
error on one file does not stop the scan of the rest, so treat exit `2` as
"re-run after fixing access, don't just read the findings" rather than a
plain crash. Findings always go to stdout; errors always go to stderr.

## Fleet harness

A containerised Puppet fleet — layered hiera, three nodes with different facts, real SIMP
modules, and real exported resources through OpenVoxDB — used to prove the allowlist holds
against realistic data rather than a flat fixture.

```bash
task fleet:up      # clone pinned sources, build images, start, wait for OpenVoxDB
task fleet:gate    # run the whole gate (brings the stack up first if it is not)
task fleet:down    # stop, keeping volumes
task fleet:clean   # stop and drop volumes
```

The gate is a Venom suite, `fleet/tests/fleet.venom.yml`, with eight checks: the per-node and
per-OS-family hiera axes are live, every node compiles identically twice, that determinism
check can be made to go red and recovers, the cross-implementation comparison completes, the
module-tree scan is not truncated, exported resources diverge as expected,
`compliance_markup` enforcement reaches the catalog, and that compliance check can be made
to go red and recovers.

Each check is also a task of its own — `fleet:gate:axes`, `fleet:gate:determinism`,
`fleet:gate:negative`, `fleet:gate:implementations`, `fleet:gate:scan`, `fleet:gate:exports`,
`fleet:gate:compliance`, `fleet:gate:compliance-negative`.
Run them with `-x`, because `task` reports `201` for any non-zero exit and these scripts
distinguish `1` ("a result about the fleet") from `2` ("could not run"):

```bash
task -x fleet:gate:axes
```

Two of those tasks mutate the fixture the containers are compiling. `fleet:gate:negative`
briefly rewrites `fleet_profile`'s `init.pp`; `fleet:gate:compliance-negative` briefly
renames `fleet_profile`'s `SIMP/` directory away. Both are bind-mounted into all three
containers, so run either one alone — never alongside another gate task or a second
terminal, which would silently measure the broken fixture. Both restore what they touched,
on their normal path and from an `EXIT` trap; `task fleet:gate` is safe because Venom runs a
suite's testcases sequentially.

Requires `podman` and `podman-compose`. Artifacts are written to `.test-output/fleet/`.
See [the maintainer notes](docs/maintainers/fleet.md) for the environment constraints that
are not obvious from the compose file, and for what the gate does and does not prove.

## Testing

```bash
task test              # human-readable output
task test MODE=llm     # failures only
task integration:venom # end-to-end against a real Puppet
task check             # all quality gates
```

Artifacts are written to `.test-output/`.
