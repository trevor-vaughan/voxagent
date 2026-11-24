# Fleet harness

For contributors and maintainers. Back to the [developer docs](README.md).


Requires `podman` and `podman-compose`.

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
check can be made to go red and recovers, the cross-implementation comparison (voxagent against Ruby Puppet) completes, the
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

Artifacts are written to `.test-output/fleet/`.
See [the maintainer notes](../maintainers/fleet.md) for the environment constraints that
are not obvious from the compose file, and for what the gate does and does not prove.
The differential harness behind the cross-implementation comparison is described in
[the harness notes](../maintainers/harness.md).
