package tidy

import (
	"strings"

	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the tidy unit: puppet's tidy type (type/tidy.rb, TY:),
// vendored in openvox 8.34.0 and identical in puppet 8.10.0, which is the file
// Attrs is pinned to. It has no Releases: the type ships in the agent, not in a
// plugin tree, so there is nothing to fingerprint. It requires file because its
// generator makes File resources.
var Manifest = unit.Manifest{
	Name:      "tidy",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:     []string{"tidy"},
	Requires:  []string{"file"},
	Platforms: unit.AnyPlatform,
}

// Unit is the tidy unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. Tidy is native unless some Tidy in the catalog
// has a shape delegateReason refuses, in which case every Tidy goes to the
// extension host with the first such reason (spec §0). Ruby's own refusals
// (parseParams) stay native: Validate reports them as Ruby does. A catalog with
// no Tidy gets an empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var found bool
	for _, r := range env.Catalog.Resources {
		if !strings.EqualFold(r.Type, "tidy") {
			continue
		}
		found = true
		if why := delegateReason(r); why != "" {
			return unit.Decision{Delegated: map[string]string{"tidy": why}}
		}
	}
	if !found {
		return unit.Decision{}
	}
	return unit.Decision{Providers: []ral.Provider{Tidy{}}}
}
