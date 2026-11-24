package file

import (
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the file unit. It reproduces type/file.rb and
// provider/file/posix.rb, which are part of puppet itself, so it lists no files
// to measure.
var Manifest = unit.Manifest{
	Name:     "file",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"file"},
	Requires: []string{},
	Releases: []unit.Release{},
}

// Unit is the file unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. file is a core type: native on every run. It
// names the catalog's environment in a missing source's message
// (type/file/source.rb:208).
func (unitImpl) Route(env *unit.Env) unit.Decision {
	return unit.Decision{Providers: []ral.Provider{File{Environment: env.Catalog.Environment, Sources: env.Sources, Vardir: env.Vardir}}}
}
