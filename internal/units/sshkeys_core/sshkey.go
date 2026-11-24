// Package sshkeyscore applies Puppet's sshkey type natively
// (puppetlabs-sshkeys_core releases 2.5.0 and 3.1.0, parsed provider).
//
// This file is the provider's identity and declared attribute set;
// sshkey_params.go validates and munges the attributes, sshkey_grammar.go is the
// parsed provider's ParsedFile class and sshkey_apply.go its prefetch, apply and
// flush. Line references `type:<n>` are puppet/type/sshkey.rb.
//
// Changelog: 1.0.0 — attribute validation, munge, identity, prefetch and apply.
package sshkeyscore

import (
	"context"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// Sshkey is the native provider for the sshkey type on the parsed provider.
//
// release is the sshkeys_core release whose type file the catalog runs under
// ("2.5.0" or "3.1.0"): 3.1.0 refuses a key with whitespace. encoding is Ruby's
// Encoding.default_external ("UTF-8" or "US-ASCII", rubyvalue.ExternalEncoding),
// which decides how refusal texts quote a value (spec §4) and how a target is
// read and written. backup and relabel are the flat filetype's filebucket and
// SELinux hooks (spec §3.2), now the header's clock. state is nil until Prefetch
// returns the primed provider that holds the run (D-96).
type Sshkey struct {
	release         string
	encoding        string
	backup, relabel func(ctx context.Context, path string) error
	now             func() time.Time

	state *ral.ParsedFileRun
}

// newSshkey is the provider for one run, on the local time, which the header's
// offset shows (M4).
func newSshkey(release, encoding string, backup, relabel func(ctx context.Context, path string) error) Sshkey {
	return Sshkey{release: release, encoding: encoding, backup: backup, relabel: relabel, now: time.Now}
}

var parsedProvider = "parsed"

// sshkeyProperties are the type's properties in sync order (spec §6.2; ensure
// first as ensurable's, then type:54-116 in declaration order).
var sshkeyProperties = []string{"ensure", "key", "host_aliases", "target"}

// Type implements Provider.
func (Sshkey) Type() string { return "sshkey" }

// ProviderUsed implements Provider.
func (Sshkey) ProviderUsed() *string { return &parsedProvider }

// Attrs implements Provider: every attribute type/sshkey.rb declares.
func (Sshkey) Attrs() ral.Attrs {
	return ral.NewAttrs(append([]string{"name", "type", "provider"}, sshkeyProperties...)...)
}

// Properties implements PropertyDeclaring.
func (Sshkey) Properties() ral.Attrs { return ral.NewAttrs(sshkeyProperties...) }

// Refresh implements Provider. type/sshkey.rb defines no refresh.
func (Sshkey) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// AutoRequires implements Provider. type/sshkey.rb declares no autorequire.
func (Sshkey) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// PuppetRef implements ral.Retitling. Ruby's title is "<name>@<munged type>"
// (type:11-15), so a resource titled any other way is reported as
// Sshkey[<name>@<type>] (M7). ok is false when that is the catalog's own ref,
// and for a resource the type refuses, which never reaches a report.
func (s Sshkey) PuppetRef(r catalog.Resource) (ref, title string, ok bool) {
	p, err := parseSshkey(r, s.release, s.encoding)
	if err != nil || p.Title == r.Title {
		return "", "", false
	}
	return r.Type + "[" + p.Title + "]", p.Title, true
}

// Unresolvable implements ral.Retitling. Puppet's catalog holds the resource
// under its own title and under an alias [name, type]; a lookup by the catalog
// title misses the title table, then retries with
// Puppet::Resource.new(type, title).uniqueness_key (resource/catalog.rb:378-395),
// which is the title patterns' [name, type]. That hits the alias only for a
// resource with no type, whose key is [name, nil], titled with just its name
// (probed: a relationship to such a resource resolves, one to a typed resource
// or to a typeless one whose name parameter differs from the title does not).
// Any other retitled resource cannot be found by its catalog ref.
func (s Sshkey) Unresolvable(r catalog.Resource) bool {
	if _, _, retitled := s.PuppetRef(r); !retitled {
		return false
	}
	p, _ := parseSshkey(r, s.release, s.encoding)
	if p.Type == "" && !titleNameType.MatchString(r.Title) {
		if m := titleName.FindStringSubmatch(r.Title); m != nil && m[1] == p.Name {
			return false
		}
	}
	return true
}

// Aliases implements ral.Aliasing: the ref Puppet gives the resource, which a
// relationship may name instead of the catalog's.
func (s Sshkey) Aliases(r catalog.Resource) []string {
	ref, _, ok := s.PuppetRef(r)
	if !ok {
		return nil
	}
	return []string{ref}
}
