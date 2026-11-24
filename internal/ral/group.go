package ral

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Group is the native provider for the group type: groupadd on the platform
// NativeUserGroup measures, where openvox 8.33.0 gives it manages_members with
// groupmod's -aU flag and system_groups (spec §6). The zero value reads the
// host's NSS and runs the host's commands; tests set ns and runner.
type Group struct {
	ns     nameService
	runner shadowRunner
}

var groupaddProvider = "groupadd"

// Type implements Provider.
func (Group) Type() string { return "group" }

// ProviderUsed implements Provider.
func (Group) ProviderUsed() *string { return &groupaddProvider }

// AutoRequires implements Provider. type/group.rb declares none.
func (Group) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs implements Provider: every attribute type/group.rb declares, including
// those dropped for a missing feature (ConversionDebug).
func (Group) Attrs() Attrs {
	return NewAttrs("ensure", "gid", "members", "attributes", "name", "auth_membership", "allowdupe",
		"system", "forcelocal", "ia_load_module", "attribute_membership", "provider")
}

// Properties implements PropertyDeclaring: the properties groupadd manages.
// attributes is a property too, but needs manages_aix_lam and is dropped.
func (Group) Properties() Attrs { return NewAttrs("ensure", "gid", "members") }

// Prefetch implements Provider. groupadd defines no prefetch: state is read at
// the resource's turn in the walk.
func (g Group) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return g, nil }

// Refresh implements Provider. type/group.rb has no refresh.
func (Group) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// ConversionDebug implements ConversionDebugging: the attributes groupadd
// lacks the feature for, given ones in allattrs order and then forcelocal,
// which has a default and so is always dropped (M35).
func (Group) ConversionDebug(r catalog.Resource) []string {
	var lines []string
	for _, attr := range []string{"attributes", "ia_load_module"} {
		if r.Parameters[attr] != nil {
			lines = append(lines, fmt.Sprintf(featureMissing, groupaddProvider, "manages_aix_lam", attr))
		}
	}
	return append(lines, fmt.Sprintf(featureMissing, groupaddProvider, "manages_local_users_and_groups", "forcelocal"))
}

// groupParams is a Group resource munged as type/group.rb munges it.
type groupParams struct {
	name                    string
	absent                  bool // ensure => absent; present is the unconditional default
	gid                     int64
	hasGID                  bool
	members                 []string // members' should list, elements not split
	hasMembers              bool
	auth, allowdupe, system bool
}

// gidPattern is type/group.rb:69's test for a string gid Integer() reads.
var gidPattern = regexp.MustCompile(`^[-0-9]+$`)

// Validate implements Validating. It reports the first failure in Ruby's
// allattrs order (type.rb:2441-2463): ensure, gid, then the parameters in
// definition order. forcelocal, ia_load_module and attributes are dropped
// for a missing feature before they are munged, so they are never refused.
func (Group) Validate(r catalog.Resource) error {
	_, err := parseGroup(r)
	return err
}

func parseGroup(r catalog.Resource) (groupParams, error) {
	p := groupParams{name: r.Title}
	if n, ok := r.Parameters["name"].(string); ok {
		p.name = n
	}
	switch v := r.Parameters["ensure"]; v {
	case nil, "present":
	case "absent":
		p.absent = true
	default:
		return p, paramFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", rubyInspectValue(v)))
	}
	switch v := r.Parameters["gid"].(type) {
	case nil:
	case string:
		if !gidPattern.MatchString(v) {
			return p, paramFailure(r, "gid", "Invalid GID "+v)
		}
		n, err := rubyInteger(v)
		if err != nil {
			return p, mungeFailure(r, "gid", v, err.Error())
		}
		p.gid, p.hasGID = n, true
	case float64:
		if v != math.Trunc(v) || math.Abs(v) >= 1<<53 {
			return p, &unimplementedValue{r.Ref(), "gid", rubyInspectValue(v)}
		}
		p.gid, p.hasGID = int64(v), true
	default:
		return p, &unimplementedValue{r.Ref(), "gid", rubyInspectValue(v)}
	}
	switch v := r.Parameters["members"].(type) {
	case nil:
	case string:
		p.members, p.hasMembers = []string{v}, true
	case []any:
		p.members, p.hasMembers = make([]string, len(v)), true
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return p, &unimplementedValue{r.Ref(), "members", rubyInspectValue(v)}
			}
			p.members[i] = s
		}
	default:
		return p, &unimplementedValue{r.Ref(), "members", rubyInspectValue(v)}
	}
	boolParam := func(name string, dst *bool) error {
		v := r.Parameters[name]
		if v == nil {
			return nil
		}
		val, err := rubyBool(v)
		if err != nil {
			return mungeFailure(r, name, v, err.Error())
		}
		*dst = val
		return nil
	}
	if err := boolParam("auth_membership", &p.auth); err != nil {
		return p, err
	}
	if err := boolParam("allowdupe", &p.allowdupe); err != nil {
		return p, err
	}
	if v := r.Parameters["attribute_membership"]; v != nil && v != "inclusive" && v != "minimum" {
		return p, paramFailure(r, "attribute_membership", fmt.Sprintf("Invalid value %s. Valid values are inclusive, minimum.", rubyInspectValue(v)))
	}
	if err := boolParam("system", &p.system); err != nil {
		return p, err
	}
	return p, nil
}

// groupCommands are the absolute paths of groupadd's commands
// (groupadd.rb:12), which confine the provider.
type groupCommands struct{ add, del, mod string }

// Apply implements Provider (spec §6). ensure is synced first, and when it
// changes nothing else is (resource_harness.rb:84-99); otherwise gid, then
// members. Under noop no command runs and no member is looked up.
func (g Group) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	p, err := parseGroup(r)
	if err != nil {
		return nil, nil, err
	}
	var cmds groupCommands
	for _, c := range []struct {
		name string
		dst  *string
	}{{"groupadd", &cmds.add}, {"groupdel", &cmds.del}, {"groupmod", &cmds.mod}} {
		if *c.dst, err = shadowCommand(c.name); err != nil {
			// The commands confine the provider; without one it is not
			// suitable, which transaction.rb:130 reports in these words.
			return nil, nil, errors.New("Provider groupadd is not functional on this host")
		}
	}
	ns := g.nss()
	cur, exists, err := ns.group(p.name)
	if err != nil {
		return nil, nil, err
	}
	suffix := ""
	if noop {
		suffix = " (noop)"
	}

	if p.absent {
		if !exists {
			runlog.Debugf(ctx, SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
			return nil, nil, nil
		}
		ch := Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "group_removed"}
		if !noop {
			if err := g.exec(ctx, cmds.del, p.name); err != nil {
				ch.Err = fmt.Errorf("Could not delete group %s: %w", p.name, err)
			}
		}
		return []Change{ch}, nil, nil
	}
	if !exists {
		ch := Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "group_created"}
		if !noop {
			ch.Err = g.create(ctx, r, p, cmds)
		}
		return []Change{ch}, nil, nil
	}

	var changes []Change
	if p.hasGID && cur.GID != p.gid {
		ch := Change{Property: "gid", Was: cur.GID, Is: p.gid,
			Message: fmt.Sprintf("gid changed %d to %d%s", cur.GID, p.gid, suffix)}
		if !noop {
			args := []string{"-g", strconv.FormatInt(p.gid, 10)}
			if p.allowdupe {
				args = append(args, "-o")
			}
			if err := g.exec(ctx, cmds.mod, append(args, p.name)...); err != nil {
				ch.Err = fmt.Errorf("Could not set gid on group[%s]: %w", p.name, err)
			}
		}
		changes = append(changes, ch)
	}
	current := cur.Members
	if current == nil {
		current = []string{}
	}
	if p.hasMembers && !slices.Equal(uniqSorted(current), actualShould(current, p.members, p.auth)) {
		// change_to_s (type/group.rb:89-95) feeds the joined actual_should back
		// through should_to_s, which splits it and applies actual_should again
		// against retrieve -- the provider's cached read, so current.
		joined := strings.Join(actualShould(current, p.members, p.auth), ",")
		ch := Change{Property: "members", Was: current, Is: p.members,
			WasDisplay: strings.Join(current, ","),
			// The failure line's should_to_s applies actual_should once.
			IsDisplay:       joined,
			DisplayVerbatim: true,
		}
		ch.Message = fmt.Sprintf("members changed %s to %s%s", ch.WasDisplay,
			strings.Join(actualShould(current, rubySplitComma(joined), p.auth), ","), suffix)
		if !noop {
			if err := g.validateMembers(p.members); err != nil {
				ch.Err = err
			} else {
				ch.Err = g.setMembers(ctx, p, current, cmds)
			}
		}
		changes = append(changes, ch)
	}
	return changes, nil, nil
}

// create is nameservice.rb create plus groupadd.rb:73-76: groupadd, then the
// members command when members is set. A member that does not resolve raises
// Etc's ArgumentError, which property.rb:492 wraps as ensure's failure; the
// commands' own failures are Puppet::Errors and are not wrapped.
func (g Group) create(ctx context.Context, r catalog.Resource, p groupParams, cmds groupCommands) error {
	var args []string
	if p.hasGID {
		args = append(args, "-g", strconv.FormatInt(p.gid, 10))
	}
	if p.allowdupe {
		args = append(args, "-o")
	}
	if p.system {
		args = append(args, "-r")
	}
	if err := g.exec(ctx, cmds.add, append(args, p.name)...); err != nil {
		return fmt.Errorf("Could not create group %s: %w", p.name, err)
	}
	if !p.hasMembers {
		return nil
	}
	if err := g.validateMembers(p.members); err != nil {
		return fmt.Errorf("Could not set 'present' on ensure: %s%s", err, errorLocation(r.File, r.Line))
	}
	// The provider's cache was emptied by exists? finding nothing, so the
	// purge test reads the new group afresh (nameservice.rb:223-235); it is
	// only made under auth_membership (groupadd.rb:119).
	var current []string
	if p.auth {
		cur, _, err := g.nss().group(p.name)
		if err != nil {
			return fmt.Errorf("Could not set 'present' on ensure: %s%s", err, errorLocation(r.File, r.Line))
		}
		current = cur.Members
	}
	return g.setMembers(ctx, p, current, cmds)
}

// validateMembers is groupadd.rb:99-105: every name in every element, split on
// commas and stripped, must be a user (Etc.getpwnam).
func (g Group) validateMembers(members []string) error {
	ns := g.nss()
	for _, m := range members {
		for _, u := range rubySplitComma(m) {
			u = strings.TrimSpace(u)
			_, ok, err := ns.user(u)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("can't find user for %s", u)
			}
		}
	}
	return nil
}

// setMembers is the rest of modifycmd(:members) once the members are valid:
// under auth_membership each current member is removed with usermod -rG
// (purge_members, unwrapped failure), then groupmod -aU adds the should
// elements joined in manifest order.
func (g Group) setMembers(ctx context.Context, p groupParams, current []string, cmds groupCommands) error {
	if p.auth && len(current) > 0 {
		usermod, err := shadowCommand("usermod")
		if err != nil {
			// usermod is an optional command (groupadd.rb:20), so it is
			// looked up when called (provider/command.rb:24).
			return errors.New("Command purgemember is missing")
		}
		for _, m := range current {
			if err := g.exec(ctx, usermod, "-rG", p.name, m); err != nil {
				return err
			}
		}
	}
	if err := g.exec(ctx, cmds.mod, "-aU", strings.Join(p.members, ","), p.name); err != nil {
		return fmt.Errorf("Could not set members on group[%s]: %w", p.name, err)
	}
	return nil
}

// exec runs one command with Puppet's failonfail: a non-zero exit is a
// Puppet::ExecutionFailure.
func (g Group) exec(ctx context.Context, name string, args ...string) error {
	runner := g.runner
	if runner == nil {
		runner = hostShadowRunner{}
	}
	code, out, err := runner.run(ctx, argvOpts{}, name, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return execFailure(append([]string{name}, args...), false, code, out)
	}
	return nil
}

func (g Group) nss() nameService {
	if g.ns == nil {
		return hostNSS
	}
	return g.ns
}

// actualShould is type/group.rb:123-132 for lists: the should list alone under
// auth_membership, else current plus should; uniq'd and sorted either way.
func actualShould(current, should []string, auth bool) []string {
	if auth {
		return uniqSorted(should)
	}
	return uniqSorted(append(slices.Clone(current), should...))
}

func uniqSorted(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rubySplitComma is Ruby's String#split(','): trailing empty fields are
// dropped, so "" splits to nothing and "a,,b," to a, "", b.
func rubySplitComma(s string) []string {
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
