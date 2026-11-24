# Developer documentation

For maintainers and contributors: how it works, how to build and test it, and why it is built this way. Users start
at the [README](../../README.md).

- [Testing](testing.md): the `task` entry points.
- [Gates](gates.md): the differential gates that compare voxagent against Ruby Puppet.
- [Fleet harness](fleet-harness.md): the containerised Puppet fleet used to exercise the allowlist.

## Maintainer notes

How the code works, with the decisions behind it.

- [Apply spine](../maintainers/apply-spine.md): the apply path, its gates, and the design decisions.
  - [The provider boundary](../maintainers/provider-boundary.md): what a provider returns and how the transaction treats it.
  - [What the gates do and do not prove](../maintainers/gate-evidence.md): each gate's evidence, blind spots and how to run it.
  - [What the spine does not implement](../maintainers/not-implemented.md): per type, what is refused or differs from Puppet.
  - [What per-property attribution cost, and what it proved](../maintainers/attribution-cost.md)
- [Units](../maintainers/units.md): what a unit is, how units are routed, and how to add one.
- [Ruby extension host](../maintainers/extension-host.md)
  - [The protocol](../maintainers/extension-host-protocol.md)
- [`voxagent agent`: transport and identity](../maintainers/agent-transport.md)
- [The run logger](../maintainers/runlog.md)
- [Differential harness internals](../maintainers/harness.md)
- [Fleet harness internals](../maintainers/fleet.md)
- [Performance evidence](../maintainers/performance.md)
- [Improvements deferred until after parity](../maintainers/improvements.md)
