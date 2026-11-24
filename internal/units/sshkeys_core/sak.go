package sshkeyscore

import (
	"context"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/suid"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/units/file"
)

// Line references `kt:<n>` are puppet/type/ssh_authorized_key.rb and `kp:<n>`
// puppet/provider/ssh_authorized_key/parsed.rb of puppetlabs-sshkeys_core.
// sak_params.go validates and munges the attributes, sak_grammar.go is the
// provider's ParsedFile class, sak_apply.go its prefetch, apply and flush, and
// sak_file.go the filetype that writes as the key's user.

// Sak is the native provider for the ssh_authorized_key type on the parsed
// provider. release is the sshkeys_core release whose type file the catalog
// runs under ("2.5.0" or "3.1.0"); the two validate every attribute alike, so
// it only travels with the provider as Sshkey's does. encoding is Ruby's
// Encoding.default_external, which decides how refusal texts quote a value and
// how a target is read and written.
//
// ns answers the passwd and group lookups (the default target, the flush's
// uid and the drop-false chown). backup and relabel are the flat filetype's
// hooks (spec §3.2), used in the root parent and for an in-process write.
// start runs the asuser helper and euid is the agent's effective uid, which
// decides whether the flush's ops run in the helper (spec §3.4). now is the
// header's clock, resolve the helper's credential (suid.Resolve) and
// defaultContext the SELinux context a helper's relabel warning names
// (file.DefaultContext). state and drop are nil until Prefetch returns the
// primed provider that holds the run (D-96).
type Sak struct {
	release         string
	encoding        string
	ns              nameservice.NameService
	backup, relabel func(ctx context.Context, path string) error
	start           asuser.Starter
	euid            func() int
	now             func() time.Time
	resolve         func(usr, group any, marked func(kind string) bool) (*syscall.Credential, error)
	defaultContext  func(ctx context.Context, path string) (string, bool)

	state *ral.ParsedFileRun
	drop  *sakDrop
}

// newSak is the provider for one run, on the local time, which the header's
// offset shows (M4).
func newSak(release, encoding string, ns nameservice.NameService, backup, relabel func(ctx context.Context, path string) error,
	start asuser.Starter, euid func() int) Sak {
	return Sak{release: release, encoding: encoding, ns: ns, backup: backup, relabel: relabel, start: start, euid: euid,
		now: time.Now, resolve: suid.Resolve, defaultContext: file.DefaultContext}
}

// sakProperties are the type's properties in sync order (spec §6.3: ensure
// first as ensurable's, then kt:64-151 in declaration order).
var sakProperties = []string{"ensure", "type", "key", "user", "target", "options"}

// Type implements Provider.
func (Sak) Type() string { return "ssh_authorized_key" }

// ProviderUsed implements Provider.
func (Sak) ProviderUsed() *string { return &parsedProvider }

// Attrs implements Provider: every attribute kt declares.
func (Sak) Attrs() ral.Attrs {
	return ral.NewAttrs(append([]string{"name", "provider", "drop_privileges"}, sakProperties...)...)
}

// Properties implements PropertyDeclaring.
func (Sak) Properties() ral.Attrs { return ral.NewAttrs(sakProperties...) }

// Refresh implements Provider. kt defines no refresh.
func (Sak) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// AutoRequires implements Provider: User[<user>] when the catalog manages that
// user (kt:153-155). Validation refused an unparseable resource before the
// graph is built, so a parse error here means no edge.
func (s Sak) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSak(r, s.release, s.encoding)
	if err != nil || p.User == "" {
		return nil
	}
	if ref := "User[" + p.User + "]"; managed(ref) {
		return []string{ref}
	}
	return nil
}
