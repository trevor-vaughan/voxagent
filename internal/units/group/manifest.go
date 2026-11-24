package group

import (
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the group unit. It reproduces type/group.rb and the
// groupadd provider, which are part of puppet itself, so it lists no files to
// measure.
var Manifest = unit.Manifest{
	Name:     "group",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"group"},
	Requires: []string{},
	Releases: []unit.Release{},
}

// Unit is the group unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. Group is native on a measured platform unless a
// Group resource names a provider other than groupadd, or has a value shape
// the native parser does not implement (rubyvalue.UnimplementedValue), which
// the host applied before the type was native; Ruby's own refusals stay native.
// Each delegation says why, and only when the catalog holds a Group.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var groups []catalog.Resource
	for _, r := range env.Catalog.Resources {
		if strings.ToLower(r.Type) == "group" {
			groups = append(groups, r)
		}
	}
	delegate := func(why string) unit.Decision {
		if len(groups) == 0 {
			return unit.Decision{}
		}
		return unit.Decision{Delegated: map[string]string{"group": why}}
	}
	if why := UnmeasuredReason(env.Platform); why != "" {
		return delegate(why)
	}
	var uv *rubyvalue.UnimplementedValue
	for _, r := range groups {
		_, err := ParseGroup(r)
		if why := ForeignProvider(r, "groupadd"); why != "" {
			return delegate(why)
		}
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
	}
	return unit.Decision{Providers: []ral.Provider{Group{}}}
}
