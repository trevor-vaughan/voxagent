package ral

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// User is the native provider for the user type (useradd, on the platform
// NativeUserGroup measures).
//
// Attrs lists every attribute type/user.rb declares, so pre-flight never
// refuses one; Validate parses them (parseUserParams); Apply is in
// user_apply.go. The zero value reads the host's NSS and runs the host's
// commands; tests set ns and runner.
type User struct {
	// CatalogGroups lists the catalog's Group resources that set a gid, in
	// catalog order (GroupGIDs). AutoRequires maps a numeric gid through it.
	CatalogGroups []GroupGID

	ns     nameService
	runner shadowRunner
}

// GroupGID is a catalog Group's reference and its munged gid.
type GroupGID struct {
	Ref string
	GID int64
}

// GroupGIDs lists cat's Group resources that set a gid, in catalog order,
// munged as Group's validation munges it. A group whose gid does not parse
// is left out; pre-flight has already refused the catalog.
func GroupGIDs(cat *catalog.Catalog) []GroupGID {
	var out []GroupGID
	for _, r := range cat.Resources {
		if !strings.EqualFold(r.Type, "group") {
			continue
		}
		if p, err := parseGroup(r); err == nil && p.hasGID {
			out = append(out, GroupGID{Ref: r.Ref(), GID: p.gid})
		}
	}
	return out
}

var useraddProvider = "useradd"

// Type implements Provider.
func (User) Type() string { return "user" }

// ProviderUsed implements Provider.
func (User) ProviderUsed() *string { return &useraddProvider }

// AutoRequires implements Provider (type/user.rb:465-503): the gid's group,
// then the groups property's. A gid written as digits or an integer names the
// first catalog Group whose gid munges to it, and no edge if none does; any
// other gid is a group name. The groups are the should list, plus the user's
// current groups under membership => minimum, sorted (names hold no comma, so
// Property::List's join and split is the identity). Only managed groups are edges.
func (u User) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseUserParams(r)
	if err != nil {
		return nil
	}
	var out []string
	add := func(ref string) {
		if ref != "" && !slices.Contains(out, ref) && managed(ref) {
			out = append(out, ref)
		}
	}
	switch v := r.Parameters["gid"].(type) {
	case string:
		if !gidNumber.MatchString(v) {
			add("Group[" + v + "]")
			break
		}
		if n, err := rubyInteger(v); err == nil {
			add(u.groupWithGID(n))
		}
	case float64:
		if v == math.Trunc(v) {
			add(u.groupWithGID(int64(v)))
		}
	}
	if p.Groups != nil {
		names := slices.Clone(p.Groups)
		if !p.Inclusive {
			// A failed read leaves the should list alone: Ruby's retrieve has no
			// error path.
			if current, err := groupsOf(u.nss(), p.Name); err == nil {
				names = append(names, current...)
			}
		}
		slices.Sort(names)
		for _, name := range names {
			add("Group[" + name + "]")
		}
	}
	return out
}

// groupWithGID is the reference of the first catalog Group whose gid is gid.
func (u User) groupWithGID(gid int64) string {
	for _, g := range u.CatalogGroups {
		if g.GID == gid {
			return g.Ref
		}
	}
	return ""
}

// Attrs implements Provider: every attribute type/user.rb declares, including
// those dropped for a missing feature (ConversionDebug).
func (User) Attrs() Attrs {
	return NewAttrs("ensure", "home", "uid", "gid", "comment", "shell", "password", "password_min_age",
		"password_max_age", "password_warn_days", "groups", "expiry", "roles", "auths", "profiles", "keys",
		"project", "attributes", "salt", "iterations", "loginclass", "name", "membership", "system", "allowdupe",
		"managehome", "role_membership", "auth_membership", "profile_membership", "key_membership",
		"ia_load_module", "attribute_membership", "forcelocal", "purge_ssh_keys", "provider")
}

// Prefetch implements Provider.
func (u User) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return u, nil }

// Refresh implements Provider.
func (User) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }
