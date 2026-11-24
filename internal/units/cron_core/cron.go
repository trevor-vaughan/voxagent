// Package croncore applies Puppet's cron type natively (puppetlabs-cron_core
// releases 1.3.0 and 2.1.0, crontab provider).
//
// This file is the provider's identity and declared attribute set;
// cron_params.go validates and munges the attributes, cron_grammar.go is the
// crontab provider's ParsedFile class and cron_apply.go its prefetch, apply
// and flush. Line references `type:<n>` are puppet/type/cron.rb and name the
// release when the two differ.
//
// Changelog: 1.0.0 — attribute validation and munge, prefetch and apply.
package croncore

import (
	"context"
	"io"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/ral"
)

// Cron is the native provider for the cron type on the crontab provider.
//
// release is the cron_core release the plugin tree selected ("1.3.0" or
// "2.1.0"; spec §2 condition 4) and encoding is Ruby's
// Encoding.default_external ("UTF-8" or "US-ASCII", rubyvalue.ExternalEncoding):
// the first picks the per-release rules of spec §4.3, the second how refusal
// texts quote a value (spec §4). The other fields are what the run needs
// (cron_apply.go): crontab is the executable route resolution found, environ
// the agent's environment ($USER, prov:4), run and ns the command runner and
// passwd lookups, stderr where a write's output is copied (D-95), spoolDir
// the directory prov:266-275 scans and now the header's clock. state is nil
// until Prefetch returns the primed provider that holds the run (D-96).
type Cron struct {
	release  string
	encoding string

	crontab  string
	environ  func(string) (string, bool)
	run      command.ShadowRunner
	ns       nameservice.NameService
	stderr   io.Writer
	spoolDir string
	now      func() time.Time

	state *cronRun
}

// newCron is the provider for one run. Its spool directory is /var/spool/cron
// and its clock the local time, which the header's offset shows (M8).
func newCron(release, crontabPath, encoding string, environ func(string) (string, bool), run command.ShadowRunner, ns nameservice.NameService, stderr io.Writer) Cron {
	return Cron{release: release, encoding: encoding, crontab: crontabPath, environ: environ, run: run, ns: ns,
		stderr: stderr, spoolDir: "/var/spool/cron", now: time.Now}
}

var crontabProvider = "crontab"

// cronProperties are the type's properties in declaration and sync order
// (type:57-410). Ruby processes attributes in this order after name and
// provider, so the first failure reported is the first in this list.
var cronProperties = []string{
	"ensure", "command", "special", "minute", "hour", "weekday", "month", "monthday", "environment", "user", "target",
}

// cronTimeFields are the five time properties in declaration order
// (type:260-306; month is declared before monthday).
var cronTimeFields = []string{"minute", "hour", "weekday", "month", "monthday"}

// Type implements Provider.
func (Cron) Type() string { return "cron" }

// ProviderUsed implements Provider.
func (Cron) ProviderUsed() *string { return &crontabProvider }

// Attrs implements Provider: every attribute type/cron.rb declares.
func (Cron) Attrs() ral.Attrs {
	return ral.NewAttrs(append([]string{"name", "provider"}, cronProperties...)...)
}

// Properties implements PropertyDeclaring.
func (Cron) Properties() ral.Attrs { return ral.NewAttrs(cronProperties...) }

// Refresh implements Provider. type/cron.rb defines no refresh.
func (Cron) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// AutoRequires implements Provider (type:380-382): the user the crontab entry
// belongs to, when the catalog manages it. target does not autorequire.
func (Cron) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	user, set, err := textProperty(r, "user")
	if err != nil || !set {
		return nil
	}
	if ref := "User[" + user + "]"; managed(ref) {
		return []string{ref}
	}
	return nil
}
