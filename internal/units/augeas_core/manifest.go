// Package augeascore is the native augeas type of puppetlabs-augeas_core.
//
// Changelog: 1.0.0 — moved from internal/ral.
package augeascore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// augeasFiles are augeas_core 2.1.0's files that identify the release, in the
// order the route checks them: its parser and feature, then the type and
// provider (spec §0 row 4). puppet/feature/augeas.rb is byte-identical in
// augeasproviders_core, which lists it too (spec row 7).
var augeasFiles = []string{
	"puppet_x/augeas/util/parser.rb",
	"puppet/feature/augeas.rb",
	"puppet/type/augeas.rb",
	"puppet/provider/augeas/augeas.rb",
}

// Manifest describes the augeas_core unit.
var Manifest = unit.Manifest{
	Name:     "augeas_core",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppetlabs-augeas_core", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"augeas"},
	Requires: []string{},
	Releases: []unit.Release{{
		Version: "2.1.0",
		Files: []unit.FileDigest{
			{Path: "puppet_x/augeas/util/parser.rb", SHA256: "586daa7ddf5a741286f805c90c5e9fc464bb91f184c0c746231bbc5ca0374a7a"},
			{Path: "puppet/feature/augeas.rb", SHA256: "d98e8ef39433632b9e835bbb3e85a75f90a8d17a33c2540f2e14f851ca2791b3"},
			{Path: "puppet/type/augeas.rb", SHA256: "1b8fe183607c21efb9a3a6431a8644d1dbb7e061a83b03bdd6744c1d9549b071"},
			{Path: "puppet/provider/augeas/augeas.rb", SHA256: "4eedaf2d530bc89909b97f36f552cf820bfbe9808040e6c9caa748a10df31c93"},
		},
		Types: map[string][]string{"augeas": augeasFiles},
	}},
}

// Unit is the augeas_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. The generic augeas type is applied natively
// when the conditions of the augeasproviders units' routes (a)-(f) hold over
// augeas_core's own files (its parser and feature, not augeasproviders_core),
// else it goes to the extension host with the first condition that failed as
// its reason. The probe is the run's, so it is not repeated after another
// unit's.
//
// Only an agent run gives the type a lens path: <libdir>/augeas/lenses, when
// that exists, as pluginsync'd lenses are loaded by the agent alone
// (provider/augeas/augeas.rb:372-375); otherwise "".
func (unitImpl) Route(env *unit.Env) unit.Decision {
	const typ = "augeas"
	if !ral.HeldAugeas(env.Catalog, typ) {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{typ: why}} }
	if _, why, ok := unit.MatchFiles(env.Tree, Manifest, typ, nil); !ok {
		return delegate(why)
	}
	if why := ral.AugeasProviderDir(env.Tree, typ); why != "" {
		return delegate(why)
	}
	if why := ral.AugeasRunChecks(env.Environ, env.Libdir); why != "" {
		return delegate(why)
	}
	lib, why, ok := env.Augeas.Probe()
	if !ok {
		return delegate(why)
	}
	var uv *rubyvalue.UnimplementedValue
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, typ) {
			if _, err := parseAugeas(r); errors.As(err, &uv) {
				return delegate(err.Error())
			}
		}
	}
	lensPath := ""
	if env.Command == "agent" {
		lenses := filepath.Join(env.Libdir, "augeas", "lenses")
		if _, err := os.Stat(lenses); err == nil {
			lensPath = lenses
		}
	}
	return unit.Decision{Providers: []ral.Provider{newAugeas(lib, lensPath)}}
}
