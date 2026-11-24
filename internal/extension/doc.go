// Package extension is voxagent's side of the extension host: the framed JSON
// protocol, the host process's lifecycle, and the embedded Ruby host that
// evaluates legacy (Tier 3) Puppet types with Puppet's own ResourceHarness.
//
// Everything the host sends is untrusted input at a deserialization point, so
// it is validated however much the host is trusted: frame size, UTF-8, unknown
// fields, list lengths, and every resource reference against the catalog. A
// violation kills the host.
//
// See docs/maintainers/extension-host.md.
package extension
