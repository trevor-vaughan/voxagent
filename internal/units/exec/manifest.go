package exec

import (
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the exec unit. It reproduces type/exec.rb and
// provider/exec/posix.rb, which are part of puppet itself, so it lists no files
// to measure.
var Manifest = unit.Manifest{
	Name:     "exec",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"exec"},
	Requires: []string{},
	Releases: []unit.Release{},
}

// Unit is the exec unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. exec is a core type: native on every run.
func (unitImpl) Route(*unit.Env) unit.Decision {
	return unit.Decision{Providers: []ral.Provider{Exec{}}}
}
