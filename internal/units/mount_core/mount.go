// Package mountcore applies Puppet's mount type natively (puppetlabs-mount_core
// releases 1.3.0 and 2.1.0, parsed provider). The two releases differ only in
// Ruby 3.1 hash shorthand, so one behaviour covers both.
//
// This file is the provider's identity and declared attribute set;
// mount_params.go validates and munges the attributes, mount_grammar.go is
// the parsed provider's ParsedFile class and mount_apply.go its prefetch,
// ensure state machine, commands and flush. Line references `mt:<n>` are
// puppet/type/mount.rb, `mm:<n>` puppet/provider/mount.rb and `mp:<n>`
// puppet/provider/mount/parsed.rb.
//
// Changelog: 1.0.0 — attribute validation and munge, the /etc/fstab grammar.
package mountcore

import (
	"context"
	"time"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/ral"
)

// Mount is the native provider for the mount type on the parsed provider.
//
// encoding is Ruby's Encoding.default_external ("UTF-8" or "US-ASCII",
// rubyvalue.ExternalEncoding): it decides how refusal texts quote a value
// (spec §4) and how a target is read and written. release is the mount_core
// release found ("1.3.0" or "2.1.0"). mountPath and umountPath are the
// resolved mount and umount executables, run through run. backup and relabel
// are the flat filetype's filebucket and SELinux hooks (spec §3.2), now the
// header's clock. files are the paths of the catalog's Files in catalog
// order, set by Route, which autobefore reads (mount_relations.go). state is
// nil until prefetch returns the primed provider that holds the run (D-96).
type Mount struct {
	release, encoding     string
	mountPath, umountPath string
	run                   command.ShadowRunner
	backup, relabel       func(ctx context.Context, path string) error
	now                   func() time.Time
	files                 []string

	state *ral.ParsedFileRun
}

var parsedProvider = "parsed"

// mountProperties are the type's properties in sync order (spec §6.4: ensure
// first, then device, blockdevice, fstype, options, pass, atboot, dump,
// target). Ruby validates attributes in this order after name and provider,
// and remounts, the last parameter, follows them.
var mountProperties = []string{"ensure", "device", "blockdevice", "fstype", "options", "pass", "atboot", "dump", "target"}

// Type implements Provider.
func (Mount) Type() string { return "mount" }

// ProviderUsed implements Provider.
func (Mount) ProviderUsed() *string { return &parsedProvider }

// Attrs implements Provider: every attribute type/mount.rb declares.
func (Mount) Attrs() ral.Attrs {
	return ral.NewAttrs(append(append([]string{"name", "provider"}, mountProperties...), "remounts")...)
}

// Properties implements PropertyDeclaring.
func (Mount) Properties() ral.Attrs { return ral.NewAttrs(mountProperties...) }
