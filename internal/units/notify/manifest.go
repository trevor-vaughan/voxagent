package notify

import (
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the notify unit. It reproduces type/notify.rb, which is
// part of puppet itself, so it lists no files to measure.
var Manifest = unit.Manifest{
	Name:      "notify",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:     []string{"notify"},
	Requires:  []string{},
	Platforms: unit.AnyPlatform,
	Releases:  []unit.Release{},
}

// Unit is the notify unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. notify is a core type: native on every run.
func (unitImpl) Route(*unit.Env) unit.Decision {
	return unit.Decision{Providers: []ral.Provider{Notify{}}}
}
