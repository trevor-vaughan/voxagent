package ral

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// runResult is what recordingRunner answers for one command.
type runResult struct {
	code int
	out  string
	err  error
}

// recordingRunner is a shadowRunner that records each command line (the
// absolute path and its arguments joined by spaces) and answers from results,
// keyed by the command's base name and arguments ("groupdel g16c"). Unlisted
// commands succeed with no output.
type recordingRunner struct {
	calls   []string
	results map[string]runResult
}

func (r *recordingRunner) run(_ context.Context, _ argvOpts, name string, args ...string) (int, string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	res := r.results[strings.Join(append([]string{filepath.Base(name)}, args...), " ")]
	return res.code, res.out, res.err
}

// groupNSS is fakeNSS plus what the recording runner has done: a group that
// groupadd created reads back as added[name], so getgrnam_r after the command
// sees it. It counts user lookups, which noop must not make.
type groupNSS struct {
	fakeNSS
	runner    *recordingRunner
	added     map[string]groupEntry
	userReads int
}

func (n *groupNSS) user(name string) (passwdEntry, bool, error) {
	n.userReads++
	return n.fakeNSS.user(name)
}

func (n *groupNSS) group(name string) (groupEntry, bool, error) {
	if g, ok, err := n.fakeNSS.group(name); ok || err != nil {
		return g, ok, err
	}
	for _, c := range n.runner.calls {
		if strings.Contains(c, "/groupadd ") && strings.HasSuffix(c, " "+name) {
			if g, ok := n.added[name]; ok {
				return g, true, nil
			}
			return groupEntry{Name: name, GID: 5000}, true, nil
		}
	}
	return groupEntry{}, false, nil
}

// withShadowBin points shadowCommand at a directory holding executable
// stand-ins for the shadow-utils commands and returns it.
func withShadowBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range []string{"groupadd", "groupdel", "groupmod", "usermod"} {
		if err := os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := shadowPath
	shadowPath = []string{dir}
	t.Cleanup(func() { shadowPath = saved })
	return dir
}

var hostUsers = map[string]passwdEntry{
	"root": {Name: "root"}, "bin": {Name: "bin", UID: 1}, "daemon": {Name: "daemon", UID: 2},
	"a": {Name: "a", UID: 1001}, "b": {Name: "b", UID: 1002},
}

func groupRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Group", Title: title, Parameters: params, File: "/etc/site.pp", Line: 3}
}

// applyGroup runs Group.Apply against groups with a recording runner and
// returns the changes, the commands run (with dir stripped to "/") and the NSS.
func applyGroup(t *testing.T, r catalog.Resource, noop bool, groups []groupEntry, results map[string]runResult) ([]Change, []string, *groupNSS) {
	t.Helper()
	dir := withShadowBin(t)
	runner := &recordingRunner{results: results}
	ns := &groupNSS{fakeNSS: fakeNSS{users: hostUsers, groups: groups}, runner: runner}
	changes, warnings, err := Group{ns: ns, runner: runner}.Apply(context.Background(), r, noop)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	calls := make([]string, len(runner.calls))
	for i, c := range runner.calls {
		calls[i] = strings.TrimPrefix(c, dir+"/")
	}
	return changes, calls, ns
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestGroupValidate(t *testing.T) {
	for _, tc := range []struct {
		title  string
		params map[string]any
		want   string
	}{
		{"g16", map[string]any{"gid": "x"}, "Parameter gid failed on Group[g16]: Invalid GID x"},
		{"g", map[string]any{"gid": "09"}, `Parameter gid failed on Group[g]: Munging failed for value "09" in class gid: invalid value for Integer(): "09"`},
		{"g", map[string]any{"ensure": "role"}, `Parameter ensure failed on Group[g]: Invalid value "role". Valid values are present, absent.`},
		{"g", map[string]any{"allowdupe": "maybe"}, `Parameter allowdupe failed on Group[g]: Munging failed for value "maybe" in class allowdupe: expected a boolean value`},
		{"g", map[string]any{"auth_membership": 1.0}, `Parameter auth_membership failed on Group[g]: Munging failed for value 1 in class auth_membership: expected a boolean value`},
		{"g", map[string]any{"system": "maybe"}, `Parameter system failed on Group[g]: Munging failed for value "maybe" in class system: expected a boolean value`},
		{"g", map[string]any{"attribute_membership": "most"}, `Parameter attribute_membership failed on Group[g]: Invalid value "most". Valid values are inclusive, minimum.`},
		// The first failure in allattrs order: ensure, gid, then parameters.
		{"g", map[string]any{"ensure": "role", "gid": "x", "allowdupe": "maybe"}, `Parameter ensure failed on Group[g]: Invalid value "role". Valid values are present, absent.`},
		{"g", map[string]any{"gid": "x", "allowdupe": "maybe"}, "Parameter gid failed on Group[g]: Invalid GID x"},
		{"g", map[string]any{"system": "maybe", "allowdupe": "nah"}, `Parameter allowdupe failed on Group[g]: Munging failed for value "nah" in class allowdupe: expected a boolean value`},
		// Dropped for a missing provider feature before munging: never refused.
		{"g", map[string]any{"forcelocal": "maybe", "ia_load_module": "x", "attributes": []any{"a=b"}}, ""},
		{"g", map[string]any{"ensure": "absent", "gid": "0010", "members": "a,b", "auth_membership": "Yes", "allowdupe": false, "system": "TRUE"}, ""},
		{"g", map[string]any{"members": []any{"a", "b"}, "gid": 3000.0}, ""},
		{"g", map[string]any{"gid": 3000.5}, "Group[g]: gid => 3000.5 is not implemented"},
		{"g", map[string]any{"members": []any{"a", 1.0}}, `Group[g]: members => ["a", 1] is not implemented`},
	} {
		got := errText(Group{}.Validate(groupRes(tc.title, tc.params)))
		if got != tc.want {
			t.Errorf("%v:\n got %q\nwant %q", tc.params, got, tc.want)
		}
	}
}

func TestGroupAttrs(t *testing.T) {
	want := NewAttrs("ensure", "gid", "members", "attributes", "name", "auth_membership", "allowdupe",
		"system", "forcelocal", "ia_load_module", "attribute_membership", "provider")
	if got := (Group{}).Attrs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Attrs = %v, want %v", got, want)
	}
	if got := (Group{}).Properties(); !reflect.DeepEqual(got, NewAttrs("ensure", "gid", "members")) {
		t.Errorf("Properties = %v", got)
	}
	if got, err := (Group{}).Prefetch(t.Context(), nil); err != nil || got.Type() != "group" {
		t.Errorf("Prefetch = %v, %v", got, err)
	}
	if again, err := (Group{}).Refresh(t.Context(), groupRes("g", nil), false); again || err != nil {
		t.Errorf("Refresh = %v, %v", again, err)
	}
}

func TestGroupConversionDebug(t *testing.T) {
	forcelocal := "Provider groupadd does not support features manages_local_users_and_groups; not managing attribute forcelocal"
	var _ ConversionDebugging = Group{}
	if got := (Group{}).ConversionDebug(groupRes("g", nil)); !reflect.DeepEqual(got, []string{forcelocal}) {
		t.Errorf("no attributes: %q", got)
	}
	got := (Group{}).ConversionDebug(groupRes("g", map[string]any{"forcelocal": true, "attributes": []any{"x=y"}, "ia_load_module": "files"}))
	want := []string{
		"Provider groupadd does not support features manages_aix_lam; not managing attribute attributes",
		"Provider groupadd does not support features manages_aix_lam; not managing attribute ia_load_module",
		forcelocal,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("all three:\n got %q\nwant %q", got, want)
	}
}

// M35 Group[g13]: groupadd, then the members command; one created event.
func TestGroupCreate(t *testing.T) {
	r := groupRes("g13", map[string]any{"ensure": "present", "gid": 3000.0, "system": true, "members": []any{"root", "bin"}})
	changes, calls, _ := applyGroup(t, r, false, nil, nil)
	if want := []string{"groupadd -g 3000 -r g13", "groupmod -aU root,bin g13"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("commands = %q, want %q", calls, want)
	}
	want := []Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created", Name: "group_created"}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v, want %+v", changes, want)
	}
}

// M35 Group[g13b]: no gid, no flags.
func TestGroupCreateBare(t *testing.T) {
	_, calls, _ := applyGroup(t, groupRes("g13b", map[string]any{"members": []any{"daemon"}, "allowdupe": "yes"}), false, nil, nil)
	if want := []string{"groupadd -o g13b", "groupmod -aU daemon g13b"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("commands = %q, want %q", calls, want)
	}
}

// create's set(:members) raises Etc's ArgumentError, which property.rb:492
// wraps because it is not a Puppet::Error. The group exists by then.
func TestGroupCreateUnknownMemberFailsEnsure(t *testing.T) {
	changes, calls, _ := applyGroup(t, groupRes("g", map[string]any{"members": []any{"x"}}), false, nil, nil)
	if want := []string{"groupadd g"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("commands = %q, want %q", calls, want)
	}
	if len(changes) != 1 || errText(changes[0].Err) != "Could not set 'present' on ensure: can't find user for x (file: /etc/site.pp, line: 3)" {
		t.Errorf("changes = %+v", changes)
	}
}

// auth_membership on create purges whatever the new group reads back with:
// the provider's cache is empty after exists?, so members is read afresh.
func TestGroupCreatePurgesReadBackMembers(t *testing.T) {
	dir := withShadowBin(t)
	runner := &recordingRunner{}
	ns := &groupNSS{fakeNSS: fakeNSS{users: hostUsers}, runner: runner,
		added: map[string]groupEntry{"g": {Name: "g", GID: 5000, Members: []string{"bin"}}}}
	r := groupRes("g", map[string]any{"members": []any{"daemon"}, "auth_membership": true})
	if _, _, err := (Group{ns: ns, runner: runner}).Apply(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	want := []string{dir + "/groupadd g", dir + "/usermod -rG g bin", dir + "/groupmod -aU daemon g"}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("commands = %q, want %q", runner.calls, want)
	}
}

func TestGroupCreateFailure(t *testing.T) {
	dir := withShadowBin(t)
	runner := &recordingRunner{results: map[string]runResult{"groupadd g": {code: 9, out: "groupadd: group 'g' already exists\n"}}}
	ns := &groupNSS{fakeNSS: fakeNSS{users: hostUsers}, runner: runner}
	changes, _, err := Group{ns: ns, runner: runner}.Apply(context.Background(), groupRes("g", map[string]any{"members": []any{"bin"}}), false)
	if err != nil {
		t.Fatal(err)
	}
	want := "Could not create group g: Execution of '" + dir + "/groupadd g' returned 9: groupadd: group 'g' already exists"
	if len(changes) != 1 || errText(changes[0].Err) != want {
		t.Errorf("changes = %+v, want Err %q", changes, want)
	}
	if len(runner.calls) != 1 {
		t.Errorf("members command ran after a failed groupadd: %q", runner.calls)
	}
}

// M37: gid then members; minimum membership adds, inclusive purges first.
func TestGroupModify(t *testing.T) {
	t.Run("minimum", func(t *testing.T) {
		groups := []groupEntry{{Name: "g14a", GID: 3000, Members: []string{"bin"}}}
		r := groupRes("g14a", map[string]any{"gid": 3001.0, "members": []any{"daemon"}, "auth_membership": false})
		changes, calls, _ := applyGroup(t, r, false, groups, nil)
		if want := []string{"groupmod -g 3001 g14a", "groupmod -aU daemon g14a"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		want := []Change{
			{Property: "gid", Was: int64(3000), Is: int64(3001), Message: "gid changed 3000 to 3001"},
			{Property: "members", Was: []string{"bin"}, Is: []string{"daemon"}, Message: "members changed bin to bin,daemon",
				WasDisplay: "bin", IsDisplay: "bin,daemon", DisplayVerbatim: true},
		}
		if !reflect.DeepEqual(changes, want) {
			t.Errorf("changes =\n %+v\nwant\n %+v", changes, want)
		}
	})
	t.Run("inclusive", func(t *testing.T) {
		groups := []groupEntry{{Name: "g14b", GID: 3002, Members: []string{"bin"}}}
		r := groupRes("g14b", map[string]any{"gid": "3003", "members": []any{"daemon"}, "auth_membership": true, "allowdupe": true})
		changes, calls, _ := applyGroup(t, r, false, groups, nil)
		want := []string{"groupmod -g 3003 -o g14b", "usermod -rG g14b bin", "groupmod -aU daemon g14b"}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 2 || changes[1].Message != "members changed bin to daemon" || changes[1].Err != nil {
			t.Errorf("changes = %+v", changes)
		}
	})
}

// M39/M40.
func TestGroupMembersContract(t *testing.T) {
	t.Run("unknown user", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g15a", map[string]any{"members": []any{"nosuchuser"}}), false,
			[]groupEntry{{Name: "g15a", GID: 1}}, nil)
		if len(calls) != 0 {
			t.Errorf("commands ran: %q", calls)
		}
		want := []Change{{Property: "members", Was: []string{}, Is: []string{"nosuchuser"},
			Message: "members changed  to nosuchuser", WasDisplay: "", IsDisplay: "nosuchuser", DisplayVerbatim: true,
			Err: errors.New("can't find user for nosuchuser")}}
		if len(changes) != 1 || errText(changes[0].Err) != "can't find user for nosuchuser" {
			t.Fatalf("changes = %+v", changes)
		}
		changes[0].Err = want[0].Err
		if !reflect.DeepEqual(changes, want) {
			t.Errorf("changes =\n %+v\nwant\n %+v", changes, want)
		}
	})
	t.Run("comma string never in sync", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g15b", map[string]any{"members": "a,b"}), false,
			[]groupEntry{{Name: "g15b", GID: 1, Members: []string{"a", "b"}}}, nil)
		if want := []string{"groupmod -aU a,b g15b"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 1 || changes[0].Message != "members changed a,b to a,b" || !reflect.DeepEqual(changes[0].Is, []string{"a,b"}) {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("empty inclusive", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g15c", map[string]any{"members": []any{}, "auth_membership": true}), false,
			[]groupEntry{{Name: "g15c", GID: 1, Members: []string{"bin", "daemon"}}}, nil)
		want := []string{"usermod -rG g15c bin", "usermod -rG g15c daemon", "groupmod -aU  g15c"}
		if !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 1 || changes[0].Message != "members changed bin,daemon to " || !reflect.DeepEqual(changes[0].Is, []string{}) {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("csv string adds", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g15d", map[string]any{"members": "bin,daemon"}), false,
			[]groupEntry{{Name: "g15d", GID: 1}}, nil)
		if len(changes) != 1 || changes[0].Message != "members changed  to bin,daemon" {
			t.Errorf("changes = %+v", changes)
		}
	})
}

func TestGroupMembersFailures(t *testing.T) {
	t.Run("groupmod", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"members": []any{"bin"}}), false,
			[]groupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -aU bin g": {code: 6, out: "groupmod: bad\n"}})
		if len(changes) != 1 || !strings.Contains(errText(changes[0].Err), "Could not set members on group[g]: Execution of '") ||
			!strings.Contains(errText(changes[0].Err), "/groupmod -aU bin g' returned 6: groupmod: bad") {
			t.Errorf("changes = %+v", changes)
		}
		if changes[0].WasDisplay != "" || changes[0].IsDisplay != "bin" {
			t.Errorf("failure display = %q to %q", changes[0].WasDisplay, changes[0].IsDisplay)
		}
	})
	t.Run("purge stops at the first failure, unwrapped", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g", map[string]any{"members": []any{"bin"}, "auth_membership": true}), false,
			[]groupEntry{{Name: "g", GID: 1, Members: []string{"a", "b"}}}, map[string]runResult{"usermod -rG g a": {code: 1, out: "usermod: no\n"}})
		if want := []string{"usermod -rG g a"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 1 || !strings.HasPrefix(errText(changes[0].Err), "Execution of '") {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("gid", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"gid": 7.0}), false,
			[]groupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -g 7 g": {code: 4, out: "groupmod: GID '7' already exists\n"}})
		if len(changes) != 1 || !strings.HasPrefix(errText(changes[0].Err), "Could not set gid on group[g]: Execution of '") {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"gid": 7.0}), false,
			[]groupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -g 7 g": {err: errors.New("fork/exec: no such file")}})
		if len(changes) != 1 || errText(changes[0].Err) != "Could not set gid on group[g]: fork/exec: no such file" {
			t.Errorf("changes = %+v", changes)
		}
	})
}

// SIMP's Group[root]: inclusive membership already as wanted.
func TestGroupMembersAuthInSyncRunsNothing(t *testing.T) {
	changes, calls, ns := applyGroup(t, groupRes("root", map[string]any{"members": []any{"root"}, "auth_membership": true, "gid": 0.0}), false,
		[]groupEntry{{Name: "root", GID: 0, Members: []string{"root"}}}, nil)
	if len(changes) != 0 || len(calls) != 0 || ns.userReads != 0 {
		t.Errorf("changes = %+v, commands = %q, user lookups = %d", changes, calls, ns.userReads)
	}
}

// M42.
func TestGroupDelete(t *testing.T) {
	dir := withShadowBin(t)
	runner := &recordingRunner{results: map[string]runResult{
		"groupdel g16c": {code: 8, out: "groupdel: cannot remove the primary group of user 'u16c'\n"}}}
	ns := &groupNSS{fakeNSS: fakeNSS{groups: []groupEntry{{Name: "g16b", GID: 3016}, {Name: "g16c", GID: 3017}}}, runner: runner}
	g := Group{ns: ns, runner: runner}

	changes, _, err := g.Apply(context.Background(), groupRes("g16b", map[string]any{"ensure": "absent"}), false)
	want := []Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed", Name: "group_removed"}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Errorf("g16b: %+v, %v", changes, err)
	}

	changes, _, _ = g.Apply(context.Background(), groupRes("g16c", map[string]any{"ensure": "absent"}), false)
	wantErr := "Could not delete group g16c: Execution of '" + dir + "/groupdel g16c' returned 8: groupdel: cannot remove the primary group of user 'u16c'"
	if len(changes) != 1 || errText(changes[0].Err) != wantErr {
		t.Errorf("g16c: %+v, want Err %q", changes, wantErr)
	}

	src := Sources{Resource: "/Stage[main]/Main/Group[g16d]"}
	ctx, logs := debugContext("debug")
	changes, _, err = g.Apply(WithSources(ctx, src), groupRes("g16d", map[string]any{"ensure": "absent"}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("g16d: %+v, %v", changes, err)
	}
	if len(*logs) != 1 || (*logs)[0].Source != src.Resource || (*logs)[0].Message != "Nothing to manage: no ensure and the resource doesn't exist" {
		t.Errorf("g16d logs = %+v", *logs)
	}
	if want := []string{dir + "/groupdel g16b", dir + "/groupdel g16c"}; !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("commands = %q, want %q", runner.calls, want)
	}
}

// §5.2: under noop no command runs and no member is looked up.
func TestGroupNoop(t *testing.T) {
	changes, calls, ns := applyGroup(t, groupRes("g13", map[string]any{"gid": 3000.0, "members": []any{"nosuchuser"}}), true, nil, nil)
	if len(changes) != 1 || changes[0].Message != "created (noop)" || changes[0].Err != nil {
		t.Errorf("create: %+v", changes)
	}
	if len(calls) != 0 || ns.userReads != 0 {
		t.Errorf("create: commands %q, user lookups %d", calls, ns.userReads)
	}

	changes, calls, ns = applyGroup(t, groupRes("g15a", map[string]any{"gid": 3001.0, "members": []any{"nosuchuser"}}), true,
		[]groupEntry{{Name: "g15a", GID: 3000}}, nil)
	if len(changes) != 2 || changes[0].Message != "gid changed 3000 to 3001 (noop)" ||
		changes[1].Message != "members changed  to nosuchuser (noop)" || changes[1].Err != nil {
		t.Errorf("modify: %+v", changes)
	}
	if len(calls) != 0 || ns.userReads != 0 {
		t.Errorf("modify: commands %q, user lookups %d", calls, ns.userReads)
	}

	changes, calls, _ = applyGroup(t, groupRes("g16b", map[string]any{"ensure": "absent"}), true, []groupEntry{{Name: "g16b", GID: 1}}, nil)
	if len(changes) != 1 || changes[0].Message != "removed (noop)" || len(calls) != 0 {
		t.Errorf("delete: %+v, %q", changes, calls)
	}
}

// Ruby confines groupadd on its three commands; without them the provider is
// unsuitable and transaction.rb:130 fails the resource.
func TestGroupMissingCommandFailsTheResource(t *testing.T) {
	saved := shadowPath
	shadowPath = []string{t.TempDir()}
	t.Cleanup(func() { shadowPath = saved })
	runner := &recordingRunner{}
	_, _, err := Group{ns: &groupNSS{runner: runner}, runner: runner}.Apply(context.Background(), groupRes("g", nil), true)
	if errText(err) != "Provider groupadd is not functional on this host" {
		t.Errorf("err = %v", err)
	}
}

func TestGroupPurgeWithoutUsermod(t *testing.T) {
	dir := withShadowBin(t)
	if err := os.Remove(filepath.Join(dir, "usermod")); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	ns := &groupNSS{fakeNSS: fakeNSS{users: hostUsers, groups: []groupEntry{{Name: "g", GID: 1, Members: []string{"a"}}}}, runner: runner}
	changes, _, err := Group{ns: ns, runner: runner}.Apply(context.Background(),
		groupRes("g", map[string]any{"members": []any{"bin"}, "auth_membership": true}), false)
	if err != nil || len(changes) != 1 || errText(changes[0].Err) != "Command purgemember is missing" {
		t.Errorf("changes = %+v, err = %v", changes, err)
	}
}

func TestRubyBool(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want bool
	}{{true, true}, {false, false}, {"true", true}, {"Yes", true}, {"FALSE", false}, {"no", false}} {
		if got, err := rubyBool(tc.in); err != nil || got != tc.want {
			t.Errorf("rubyBool(%#v) = %v, %v", tc.in, got, err)
		}
	}
	for _, in := range []any{"maybe", "", "y", 1.0, nil, []any{"true"}} {
		if _, err := rubyBool(in); errText(err) != "expected a boolean value" {
			t.Errorf("rubyBool(%#v) err = %v", in, err)
		}
	}
}

func TestShadowCommandSearchesRubysPathInOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "groupadd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(first, "groupmod"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"groupadd", "groupmod"} {
		if err := os.WriteFile(filepath.Join(second, c), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := shadowPath
	shadowPath = []string{first, second}
	t.Cleanup(func() { shadowPath = saved })
	for _, c := range []string{"groupadd", "groupmod"} {
		if got, err := shadowCommand(c); err != nil || got != filepath.Join(second, c) {
			t.Errorf("shadowCommand(%s) = %q, %v; want the executable file in %s", c, got, err, second)
		}
	}
	if _, err := shadowCommand("groupdel"); err == nil {
		t.Error("missing command resolved")
	}
	if !reflect.DeepEqual(saved, []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}) {
		t.Errorf("default search order = %q", saved)
	}
}
