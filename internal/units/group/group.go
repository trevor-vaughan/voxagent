// Package group applies Puppet's group type natively.
//
// Changelog: 1.0.0 — moved from internal/ral.
package group

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
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Group is the native provider for the group type: groupadd on the platform
// group.UnmeasuredReason describes, where openvox 8.33.0 gives it manages_members with
// groupmod's -aU flag and system_groups (spec §6). The zero value reads the
// host's NSS and runs the host's commands; tests set ns and runner.
type Group struct {
	ns     nameservice.NameService
	runner command.ShadowRunner
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
func (Group) Attrs() ral.Attrs {
	return ral.NewAttrs("ensure", "gid", "members", "attributes", "name", "auth_membership", "allowdupe",
		"system", "forcelocal", "ia_load_module", "attribute_membership", "provider")
}

// Properties implements PropertyDeclaring: the properties groupadd manages.
// attributes is a property too, but needs manages_aix_lam and is dropped.
func (Group) Properties() ral.Attrs { return ral.NewAttrs("ensure", "gid", "members") }

// Prefetch implements Provider. groupadd defines no prefetch: state is read at
// the resource's turn in the walk.
func (g Group) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) { return g, nil }

// Refresh implements Provider. type/group.rb has no refresh.
func (Group) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// ConversionDebug implements ConversionDebugging: the attributes groupadd
// lacks the feature for, given ones in allattrs order and then forcelocal,
// which has a default and so is always dropped (M35).
func (Group) ConversionDebug(r catalog.Resource) []string {
	var lines []string
	for _, attr := range []string{"attributes", "ia_load_module"} {
		if r.Parameters[attr] != nil {
			lines = append(lines, fmt.Sprintf(nameservice.FeatureMissing, groupaddProvider, "manages_aix_lam", attr))
		}
	}
	return append(lines, fmt.Sprintf(nameservice.FeatureMissing, groupaddProvider, "manages_local_users_and_groups", "forcelocal"))
}

// groupParams is a Group resource munged as type/group.rb munges it.
type groupParams struct {
	name                    string
	absent                  bool // ensure => absent; present is the unconditional default
	GID                     int64
	HasGID                  bool
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
	_, err := ParseGroup(r)
	return err
}

// ParseGroup munges r as type/group.rb does. Route and the user unit call it to
// tell an unimplemented value from a Ruby refusal and to read a group's gid.
func ParseGroup(r catalog.Resource) (groupParams, error) {
	p := groupParams{name: r.Title}
	if n, ok := r.Parameters["name"].(string); ok {
		p.name = n
	}
	switch v := r.Parameters["ensure"]; v {
	case nil, "present":
	case "absent":
		p.absent = true
	default:
		return p, rubyvalue.ParamFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", rubyvalue.ShownValue(r, "ensure", v)))
	}
	switch v := r.Parameters["gid"].(type) {
	case nil:
	case string:
		if !gidPattern.MatchString(v) {
			return p, rubyvalue.ParamFailure(r, "gid", "Invalid GID "+rubyvalue.RedactIf(slices.Contains(r.SensitiveParameters, "gid"), "%s", v))
		}
		n, err := rubyvalue.Integer(v)
		if err != nil {
			return p, rubyvalue.MungeIntegerFailure(r, "gid", v, err)
		}
		p.GID, p.HasGID = n, true
	case float64:
		if v != math.Trunc(v) || math.Abs(v) >= 1<<53 {
			return p, &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: "gid", Shown: rubyvalue.ShownValue(r, "gid", v)}
		}
		p.GID, p.HasGID = int64(v), true
	default:
		return p, &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: "gid", Shown: rubyvalue.ShownValue(r, "gid", v)}
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
				return p, &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: "members", Shown: rubyvalue.ShownValue(r, "members", v)}
			}
			p.members[i] = s
		}
	default:
		return p, &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: "members", Shown: rubyvalue.ShownValue(r, "members", v)}
	}
	boolParam := func(name string, dst *bool) error {
		v := r.Parameters[name]
		if v == nil {
			return nil
		}
		val, err := rubyvalue.Bool(v)
		if err != nil {
			return rubyvalue.MungeFailure(r, name, v, err.Error())
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
		return p, rubyvalue.ParamFailure(r, "attribute_membership", fmt.Sprintf("Invalid value %s. Valid values are inclusive, minimum.", rubyvalue.ShownValue(r, "attribute_membership", v)))
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
func (g Group) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := ParseGroup(r)
	if err != nil {
		return nil, nil, err
	}
	marked, _ := ral.MarkSensitive(g, r)
	var cmds groupCommands
	for _, c := range []struct {
		name string
		dst  *string
	}{{"groupadd", &cmds.add}, {"groupdel", &cmds.del}, {"groupmod", &cmds.mod}} {
		if *c.dst, err = nameservice.ShadowCommand(c.name); err != nil {
			// The commands confine the provider; without one it is not
			// suitable, which transaction.rb:130 reports in these words.
			return nil, nil, errors.New("Provider groupadd is not functional on this host")
		}
	}
	ns := g.nss()
	cur, exists, err := ns.Group(p.name)
	if err != nil {
		return nil, nil, err
	}
	suffix := ""
	if noop {
		suffix = " (noop)"
	}

	if p.absent {
		if !exists {
			runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
			return nil, nil, nil
		}
		ch := ral.Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "group_removed"}
		if !noop {
			if err := g.exec(ctx, command.ArgvOpts{}, cmds.del, p.name); err != nil {
				ch.Err = fmt.Errorf("Could not delete group %s: %w", p.name, err)
			}
		}
		return []ral.Change{ch}, nil, nil
	}
	if !exists {
		ch := ral.Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "group_created"}
		if !noop {
			ch.Err = g.create(ctx, r, p, cmds, marked)
		}
		return []ral.Change{ch}, nil, nil
	}

	var changes []ral.Change
	if p.HasGID && cur.GID != p.GID {
		ch := ral.Change{Property: "gid", Was: cur.GID, Is: p.GID,
			Message: fmt.Sprintf("gid changed %d to %d%s", cur.GID, p.GID, suffix)}
		if !noop {
			args := []string{"-g", strconv.FormatInt(p.GID, 10)}
			if p.allowdupe {
				args = append(args, "-o")
			}
			_, redact := marked["gid"]
			if err := g.exec(ctx, command.ArgvOpts{Redact: redact, RedactOutput: redact}, cmds.mod, append(args, p.name)...); err != nil {
				ch.Err = fmt.Errorf("Could not set gid on group[%s]: %w", p.name, err)
			}
		}
		changes = append(changes, ch)
	}
	current := cur.Members
	if current == nil {
		current = []string{}
	}
	if p.hasMembers && !slices.Equal(rubyvalue.UniqSorted(current), actualShould(current, p.members, p.auth)) {
		// change_to_s (type/group.rb:89-95) feeds the joined actual_should back
		// through should_to_s, which splits it and applies actual_should again
		// against retrieve -- the provider's cached read, so current.
		joined := strings.Join(actualShould(current, p.members, p.auth), ",")
		ch := ral.Change{Property: "members", Was: current, Is: p.members,
			WasDisplay: strings.Join(current, ","),
			// The failure line's should_to_s applies actual_should once.
			IsDisplay:       joined,
			DisplayVerbatim: true,
		}
		ch.Message = fmt.Sprintf("members changed %s to %s%s", ch.WasDisplay,
			strings.Join(actualShould(current, rubyvalue.SplitComma(joined), p.auth), ","), suffix)
		if !noop {
			if err := g.validateMembers(p.members, marked); err != nil {
				ch.Err = err
			} else {
				ch.Err = g.setMembers(ctx, p, current, cmds, marked)
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
func (g Group) create(ctx context.Context, r catalog.Resource, p groupParams, cmds groupCommands, marked ral.Attrs) error {
	var args []string
	if p.HasGID {
		args = append(args, "-g", strconv.FormatInt(p.GID, 10))
	}
	if p.allowdupe {
		args = append(args, "-o")
	}
	if p.system {
		args = append(args, "-r")
	}
	// Any marked property redacts the command line; the output only when the
	// argv carries a marked value, which is the gid.
	_, gidMarked := marked["gid"]
	if err := g.exec(ctx, command.ArgvOpts{Redact: len(marked) > 0, RedactOutput: gidMarked}, cmds.add, append(args, p.name)...); err != nil {
		return fmt.Errorf("Could not create group %s: %w", p.name, err)
	}
	if !p.hasMembers {
		return nil
	}
	if err := g.validateMembers(p.members, marked); err != nil {
		return fmt.Errorf("Could not set 'present' on ensure: %s%s", err, rubyvalue.ErrorLocation(r.File, r.Line))
	}
	// The provider's cache was emptied by exists? finding nothing, so the
	// purge test reads the new group afresh (nameservice.rb:223-235); it is
	// only made under auth_membership (groupadd.rb:119).
	var current []string
	if p.auth {
		cur, _, err := g.nss().Group(p.name)
		if err != nil {
			return fmt.Errorf("Could not set 'present' on ensure: %s%s", err, rubyvalue.ErrorLocation(r.File, r.Line))
		}
		current = cur.Members
	}
	return g.setMembers(ctx, p, current, cmds, marked)
}

// validateMembers is groupadd.rb:99-105: every name in every element, split on
// commas and stripped, must be a user (Etc.getpwnam).
func (g Group) validateMembers(members []string, marked ral.Attrs) error {
	_, redact := marked["members"]
	ns := g.nss()
	for _, m := range members {
		for _, u := range rubyvalue.SplitComma(m) {
			u = strings.TrimSpace(u)
			_, ok, err := ns.User(u)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("can't find user for %s", rubyvalue.RedactIf(redact, "%s", u))
			}
		}
	}
	return nil
}

// setMembers is the rest of modifycmd(:members) once the members are valid:
// under auth_membership each current member is removed with usermod -rG
// (purge_members, unwrapped failure), then groupmod -aU adds the should
// elements joined in manifest order.
func (g Group) setMembers(ctx context.Context, p groupParams, current []string, cmds groupCommands, marked ral.Attrs) error {
	_, redact := marked["members"]
	if p.auth && len(current) > 0 {
		usermod, err := nameservice.ShadowCommand("usermod")
		if err != nil {
			// usermod is an optional command (groupadd.rb:20), so it is
			// looked up when called (provider/command.rb:24).
			return errors.New("Command purgemember is missing")
		}
		for _, m := range current {
			if err := g.exec(ctx, command.ArgvOpts{Redact: redact, RedactOutput: redact}, usermod, "-rG", p.name, m); err != nil {
				return err
			}
		}
	}
	if err := g.exec(ctx, command.ArgvOpts{Redact: redact, RedactOutput: redact}, cmds.mod, "-aU", strings.Join(p.members, ","), p.name); err != nil {
		return fmt.Errorf("Could not set members on group[%s]: %w", p.name, err)
	}
	return nil
}

// exec runs one command with Puppet's failonfail: a non-zero exit is a
// Puppet::ExecutionFailure. o.Redact logs and names the command as Ruby does
// for a command with sensitive data, o.RedactOutput hides its output (D-78).
func (g Group) exec(ctx context.Context, o command.ArgvOpts, name string, args ...string) error {
	runner := g.runner
	if runner == nil {
		runner = command.HostShadowRunner{}
	}
	code, out, err := runner.Run(ctx, o, name, args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return command.ExecFailure(append([]string{name}, args...), o.Redact, o.RedactOutput, code, out)
	}
	return nil
}

func (g Group) nss() nameservice.NameService {
	if g.ns == nil {
		return nameservice.Host
	}
	return g.ns
}

// actualShould is type/group.rb:123-132 for lists: the should list alone under
// auth_membership, else current plus should; uniq'd and sorted either way.
func actualShould(current, should []string, auth bool) []string {
	if auth {
		return rubyvalue.UniqSorted(should)
	}
	return rubyvalue.UniqSorted(append(slices.Clone(current), should...))
}
