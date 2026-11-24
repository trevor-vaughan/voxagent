package user

import (
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/group"
)

// Manifest describes the user unit. It reproduces type/user.rb and the useradd
// provider, which are part of puppet itself, so it lists no files to measure.
// It requires group: it routes with group's platform check and reads the
// catalog's group gids.
var Manifest = unit.Manifest{
	Name:     "user",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet", Version: "8.10.0"},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"user"},
	Requires: []string{"group"},
	Releases: []unit.Release{},
}

// Unit is the user unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. User is native on a measured platform unless a
// User resource names a provider other than useradd, sets purge_ssh_keys to
// anything but false, "false" or [] (it generates Ssh_authorized_key resources
// only the host can apply), or has a value shape the native parser does not
// implement (rubyvalue.UnimplementedValue), which the host applied before the
// type was native; Ruby's own refusals stay native. Each delegation says why,
// and only when the catalog holds a User.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var users []catalog.Resource
	for _, r := range env.Catalog.Resources {
		if strings.ToLower(r.Type) == "user" {
			users = append(users, r)
		}
	}
	delegate := func(why string) unit.Decision {
		if len(users) == 0 {
			return unit.Decision{}
		}
		return unit.Decision{Delegated: map[string]string{"user": why}}
	}
	if why := group.UnmeasuredReason(env.Platform); why != "" {
		return delegate(why)
	}
	var uv *rubyvalue.UnimplementedValue
	for _, r := range users {
		_, err := parseUserParams(r)
		if why := group.ForeignProvider(r, "useradd"); why != "" {
			return delegate(why)
		}
		if !PurgeKeepsNative(r) {
			return delegate("purge_ssh_keys generates Ssh_authorized_key resources")
		}
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
	}
	return unit.Decision{Providers: []ral.Provider{User{CatalogGroups: GroupGIDs(env.Catalog)}}}
}

// PurgeKeepsNative reports whether r's purge_ssh_keys is absent, false, "false"
// or an empty list: the values that generate no Ssh_authorized_key resources.
func PurgeKeepsNative(r catalog.Resource) bool {
	v, set := r.Parameters["purge_ssh_keys"]
	if !set {
		return true
	}
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return t == "false"
	case []any:
		return len(t) == 0
	}
	return false
}
