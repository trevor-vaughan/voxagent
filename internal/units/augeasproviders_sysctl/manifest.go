// Package augeasproviderssysctl is the native sysctl type of
// augeasproviders_sysctl.
//
// Changelog: 1.0.0 — moved from internal/ral.
package augeasproviderssysctl

import (
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// Manifest describes the sysctl unit: augeasproviders_sysctl 4.1.0's type and
// provider (spec §0 row 4), with the three files of augeasproviders_core that
// every augeasproviders type depends on. The digests of the core files are in
// augeasproviders_core's manifest.
var Manifest = unit.Manifest{
	Name:     "augeasproviders_sysctl",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet-augeasproviders_sysctl", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"sysctl"},
	Requires: []string{"augeasproviders_core"},
	Releases: []unit.Release{{
		Version: "4.1.0",
		Files: []unit.FileDigest{
			{Path: "puppet/type/sysctl.rb", SHA256: "e3e75816ce52ca14acbf78252e2dc0ae98867e5d3b81637a5d4e29fe2803d863"},
			{Path: "puppet/provider/sysctl/augeas.rb", SHA256: "25ca6b921df6408d69107cba0dcc807c0ff996f8d2e6f2d7b381fa0ef285031d"},
		},
		Types: map[string][]string{"sysctl": {
			"augeasproviders_core:puppet/provider/augeasprovider/default.rb",
			"augeasproviders_core:puppet/type/augeasprovider.rb",
			"augeasproviders_core:puppet/feature/augeas.rb",
			"puppet/type/sysctl.rb",
			"puppet/provider/sysctl/augeas.rb",
		}},
	}},
}

// Unit is the sysctl unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. Sysctl is applied natively, over the run the
// augeasproviders types of the run share, unless a condition sends it to the
// extension host, which is the oracle for everything not measured (spec §2),
// with the first condition that failed as its reason. A catalog without a
// Sysctl reads and probes nothing.
//
// The conditions run in spec §2's order: (a) the files of the manifest match
// and the type's provider directory holds no .rb but augeas.rb; (c)
// ral.AugeasRunChecks; (b) the run's probe accepts the library, made only after
// (c); (d)-(f) no Sysctl has a shape the native code does not implement (Ruby's
// own refusals stay native); (g) the platform is the measured one and sysctl
// resolves over PATH as Puppet::Util.which does.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	const typ = "sysctl"
	if !ral.HeldAugeas(env.Catalog, typ) {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{typ: why}} }

	// (a)
	if _, why, ok := unit.MatchFiles(env.Tree, Manifest, typ, []unit.Manifest{core.Manifest}); !ok {
		return delegate(why)
	}
	if why := ral.AugeasProviderDir(env.Tree, typ); why != "" {
		return delegate(why)
	}

	// (c)
	if why := ral.AugeasRunChecks(env.Environ, env.Libdir); why != "" {
		return delegate(why)
	}

	// (b)
	if _, why, ok := env.Augeas.Probe(); !ok {
		return delegate(why)
	}

	// (d)-(f)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, typ) {
			if _, err := parseSysctl(r); errors.As(err, &uv) {
				return delegate(err.Error())
			}
		}
	}

	// (g)
	if !ral.MeasuredRelease(env.Platform) {
		return delegate("the platform is not RHEL family 10 or later (Fedora excluded)")
	}
	cmd, ok := command.RubyWhich("sysctl", env.Environ("PATH"), env.Platform.Executable)
	if !ok {
		return delegate("no executable sysctl on PATH, /usr/sbin or /sbin")
	}
	return unit.Decision{Providers: []ral.Provider{Sysctl{run: core.SharedRun(env.Augeas, env.Libdir), sysctl: cmd, runner: argvSysctl{}}}}
}
