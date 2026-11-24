// Package augeasproviderscore is the library unit of augeasproviders_core: the
// run object and mixin its types (augeasproviders_sysctl and
// augeasproviders_ssh) share, and the parameter readers they have in common.
// It serves no type of its own.
//
// Changelog: 1.0.0 — moved from internal/ral.
package augeasproviderscore

import "github.com/voxagent/voxagent/internal/unit"

// Manifest describes the augeasproviders_core unit: the three files of
// augeasproviders_core 4.2.0 that every augeasproviders type's provider
// depends on (spec §0 row 4). puppet/feature/augeas.rb is byte-identical in
// augeas_core, which lists it too.
var Manifest = unit.Manifest{
	Name:      "augeasproviders_core",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppet-augeasproviders_core", Version: ""},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:     []string{},
	Requires:  []string{},
	Platforms: unit.AnyPlatform,
	Releases: []unit.Release{{
		Version: "4.2.0",
		Files: []unit.FileDigest{
			{Path: "puppet/provider/augeasprovider/default.rb", SHA256: "3900c7e789d651b9f5271087ef3a3b3a9b08cfef2b725d30b559bfff280af42a"},
			{Path: "puppet/type/augeasprovider.rb", SHA256: "0cd37a04489790a03adcae071cf3ee62a81ba238c4ce4959314092d1ab69e795"},
			{Path: "puppet/feature/augeas.rb", SHA256: "d98e8ef39433632b9e835bbb3e85a75f90a8d17a33c2540f2e14f851ca2791b3"},
		},
		Types: map[string][]string{},
	}},
}

// Unit is the augeasproviders_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit: a library unit serves and delegates nothing.
func (unitImpl) Route(*unit.Env) unit.Decision { return unit.Decision{} }
