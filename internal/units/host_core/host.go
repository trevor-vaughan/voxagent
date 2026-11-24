// Package hostcore applies Puppet's host type natively (puppetlabs-host_core
// release 1.3.0, parsed provider).
//
// This file is the provider's identity and declared attribute set;
// host_params.go validates and munges the attributes, host_grammar.go is the
// parsed provider's ParsedFile class and host_apply.go its prefetch, apply and
// flush. Line references `type:<n>` are puppet/type/host.rb.
//
// Changelog: 1.0.0 — attribute validation and munge, prefetch and apply.
package hostcore

import (
	"context"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// Host is the native provider for the host type on the parsed provider.
//
// encoding is Ruby's Encoding.default_external ("UTF-8" or "US-ASCII",
// rubyvalue.ExternalEncoding): it decides how refusal texts quote a value
// (spec §4) and how a target is read and written. backup and relabel are the
// flat filetype's filebucket and SELinux hooks (spec §3.2), now the header's
// clock. state is nil until Prefetch returns the primed provider that holds
// the run (D-96).
type Host struct {
	encoding        string
	backup, relabel func(ctx context.Context, path string) error
	now             func() time.Time

	state *ral.ParsedFileRun
}

// newHost is the provider for one run, on the local time, which the header's
// offset shows (M4).
func newHost(encoding string, backup, relabel func(ctx context.Context, path string) error) Host {
	return Host{encoding: encoding, backup: backup, relabel: relabel, now: time.Now}
}

var parsedProvider = "parsed"

// hostProperties are the type's properties in sync order (spec §6.1; ensure
// first as ensurable's, then type:11-75 in declaration order). Ruby processes
// attributes in this order after name and provider, so the first failure
// reported is the first in this list.
var hostProperties = []string{"ensure", "ip", "host_aliases", "comment", "target"}

// Type implements Provider.
func (Host) Type() string { return "host" }

// ProviderUsed implements Provider.
func (Host) ProviderUsed() *string { return &parsedProvider }

// Attrs implements Provider: every attribute type/host.rb declares.
func (Host) Attrs() ral.Attrs {
	return ral.NewAttrs(append([]string{"name", "provider"}, hostProperties...)...)
}

// Properties implements PropertyDeclaring.
func (Host) Properties() ral.Attrs { return ral.NewAttrs(hostProperties...) }

// Refresh implements Provider. type/host.rb defines no refresh.
func (Host) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// AutoRequires implements Provider. type/host.rb declares no autorequire.
func (Host) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
