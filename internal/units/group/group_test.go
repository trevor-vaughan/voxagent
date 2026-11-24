package group

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
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

func (r *recordingRunner) Run(_ context.Context, _ command.ArgvOpts, name string, args ...string) (int, string, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	res := r.results[strings.Join(append([]string{filepath.Base(name)}, args...), " ")]
	return res.code, res.out, res.err
}

// groupNSS is raltest.FakeNSS plus what the recording runner has done: a group that
// groupadd created reads back as added[name], so getgrnam_r after the command
// sees it. It counts user lookups, which noop must not make.
type groupNSS struct {
	raltest.FakeNSS
	runner    *recordingRunner
	added     map[string]nameservice.GroupEntry
	userReads int
}

func (n *groupNSS) User(name string) (nameservice.PasswdEntry, bool, error) {
	n.userReads++
	return n.FakeNSS.User(name)
}

func (n *groupNSS) Group(name string) (nameservice.GroupEntry, bool, error) {
	if g, ok, err := n.FakeNSS.Group(name); ok || err != nil {
		return g, ok, err
	}
	for _, c := range n.runner.calls {
		if strings.Contains(c, "/groupadd ") && strings.HasSuffix(c, " "+name) {
			if g, ok := n.added[name]; ok {
				return g, true, nil
			}
			return nameservice.GroupEntry{Name: name, GID: 5000}, true, nil
		}
	}
	return nameservice.GroupEntry{}, false, nil
}

// withShadowBin points nameservice.ShadowCommand at a directory holding executable
// stand-ins for the shadow-utils commands and returns it.
func withShadowBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range []string{"groupadd", "groupdel", "groupmod", "usermod"} {
		if err := os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := nameservice.ShadowPath
	nameservice.ShadowPath = []string{dir}
	t.Cleanup(func() { nameservice.ShadowPath = saved })
	return dir
}

var hostUsers = map[string]nameservice.PasswdEntry{
	"root": {Name: "root"}, "bin": {Name: "bin", UID: 1}, "daemon": {Name: "daemon", UID: 2},
	"a": {Name: "a", UID: 1001}, "b": {Name: "b", UID: 1002},
}

func groupRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Group", Title: title, Parameters: params, File: "/etc/site.pp", Line: 3}
}

// applyGroup runs Group.Apply against groups with a recording runner and
// returns the changes, the commands run (with dir stripped to "/") and the NSS.
func applyGroup(t *testing.T, r catalog.Resource, noop bool, groups []nameservice.GroupEntry, results map[string]runResult) ([]ral.Change, []string, *groupNSS) {
	t.Helper()
	dir := withShadowBin(t)
	runner := &recordingRunner{results: results}
	ns := &groupNSS{FakeNSS: raltest.FakeNSS{Users: hostUsers, Groups: groups}, runner: runner}
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
		got := raltest.ErrText(Group{}.Validate(groupRes(tc.title, tc.params)))
		if got != tc.want {
			t.Errorf("%v:\n got %q\nwant %q", tc.params, got, tc.want)
		}
	}
}

func TestGroupAttrs(t *testing.T) {
	want := ral.NewAttrs("ensure", "gid", "members", "attributes", "name", "auth_membership", "allowdupe",
		"system", "forcelocal", "ia_load_module", "attribute_membership", "provider")
	if got := (Group{}).Attrs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Attrs = %v, want %v", got, want)
	}
	if got := (Group{}).Properties(); !reflect.DeepEqual(got, ral.NewAttrs("ensure", "gid", "members")) {
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
	var _ ral.ConversionDebugging = Group{}
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
	want := []ral.Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created", Name: "group_created"}}
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
	if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "Could not set 'present' on ensure: can't find user for x (file: /etc/site.pp, line: 3)" {
		t.Errorf("changes = %+v", changes)
	}
}

// auth_membership on create purges whatever the new group reads back with:
// the provider's cache is empty after exists?, so members is read afresh.
func TestGroupCreatePurgesReadBackMembers(t *testing.T) {
	dir := withShadowBin(t)
	runner := &recordingRunner{}
	ns := &groupNSS{FakeNSS: raltest.FakeNSS{Users: hostUsers}, runner: runner,
		added: map[string]nameservice.GroupEntry{"g": {Name: "g", GID: 5000, Members: []string{"bin"}}}}
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
	ns := &groupNSS{FakeNSS: raltest.FakeNSS{Users: hostUsers}, runner: runner}
	changes, _, err := Group{ns: ns, runner: runner}.Apply(context.Background(), groupRes("g", map[string]any{"members": []any{"bin"}}), false)
	if err != nil {
		t.Fatal(err)
	}
	want := "Could not create group g: Execution of '" + dir + "/groupadd g' returned 9: groupadd: group 'g' already exists"
	if len(changes) != 1 || raltest.ErrText(changes[0].Err) != want {
		t.Errorf("changes = %+v, want Err %q", changes, want)
	}
	if len(runner.calls) != 1 {
		t.Errorf("members command ran after a failed groupadd: %q", runner.calls)
	}
}

// M37: gid then members; minimum membership adds, inclusive purges first.
func TestGroupModify(t *testing.T) {
	t.Run("minimum", func(t *testing.T) {
		groups := []nameservice.GroupEntry{{Name: "g14a", GID: 3000, Members: []string{"bin"}}}
		r := groupRes("g14a", map[string]any{"gid": 3001.0, "members": []any{"daemon"}, "auth_membership": false})
		changes, calls, _ := applyGroup(t, r, false, groups, nil)
		if want := []string{"groupmod -g 3001 g14a", "groupmod -aU daemon g14a"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		want := []ral.Change{
			{Property: "gid", Was: int64(3000), Is: int64(3001), Message: "gid changed 3000 to 3001"},
			{Property: "members", Was: []string{"bin"}, Is: []string{"daemon"}, Message: "members changed bin to bin,daemon",
				WasDisplay: "bin", IsDisplay: "bin,daemon", DisplayVerbatim: true},
		}
		if !reflect.DeepEqual(changes, want) {
			t.Errorf("changes =\n %+v\nwant\n %+v", changes, want)
		}
	})
	t.Run("inclusive", func(t *testing.T) {
		groups := []nameservice.GroupEntry{{Name: "g14b", GID: 3002, Members: []string{"bin"}}}
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
			[]nameservice.GroupEntry{{Name: "g15a", GID: 1}}, nil)
		if len(calls) != 0 {
			t.Errorf("commands ran: %q", calls)
		}
		want := []ral.Change{{Property: "members", Was: []string{}, Is: []string{"nosuchuser"},
			Message: "members changed  to nosuchuser", WasDisplay: "", IsDisplay: "nosuchuser", DisplayVerbatim: true,
			Err: errors.New("can't find user for nosuchuser")}}
		if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "can't find user for nosuchuser" {
			t.Fatalf("changes = %+v", changes)
		}
		changes[0].Err = want[0].Err
		if !reflect.DeepEqual(changes, want) {
			t.Errorf("changes =\n %+v\nwant\n %+v", changes, want)
		}
	})
	t.Run("comma string never in sync", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g15b", map[string]any{"members": "a,b"}), false,
			[]nameservice.GroupEntry{{Name: "g15b", GID: 1, Members: []string{"a", "b"}}}, nil)
		if want := []string{"groupmod -aU a,b g15b"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 1 || changes[0].Message != "members changed a,b to a,b" || !reflect.DeepEqual(changes[0].Is, []string{"a,b"}) {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("empty inclusive", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g15c", map[string]any{"members": []any{}, "auth_membership": true}), false,
			[]nameservice.GroupEntry{{Name: "g15c", GID: 1, Members: []string{"bin", "daemon"}}}, nil)
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
			[]nameservice.GroupEntry{{Name: "g15d", GID: 1}}, nil)
		if len(changes) != 1 || changes[0].Message != "members changed  to bin,daemon" {
			t.Errorf("changes = %+v", changes)
		}
	})
}

func TestGroupMembersFailures(t *testing.T) {
	t.Run("groupmod", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"members": []any{"bin"}}), false,
			[]nameservice.GroupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -aU bin g": {code: 6, out: "groupmod: bad\n"}})
		if len(changes) != 1 || !strings.Contains(raltest.ErrText(changes[0].Err), "Could not set members on group[g]: Execution of '") ||
			!strings.Contains(raltest.ErrText(changes[0].Err), "/groupmod -aU bin g' returned 6: groupmod: bad") {
			t.Errorf("changes = %+v", changes)
		}
		if changes[0].WasDisplay != "" || changes[0].IsDisplay != "bin" {
			t.Errorf("failure display = %q to %q", changes[0].WasDisplay, changes[0].IsDisplay)
		}
	})
	t.Run("purge stops at the first failure, unwrapped", func(t *testing.T) {
		changes, calls, _ := applyGroup(t, groupRes("g", map[string]any{"members": []any{"bin"}, "auth_membership": true}), false,
			[]nameservice.GroupEntry{{Name: "g", GID: 1, Members: []string{"a", "b"}}}, map[string]runResult{"usermod -rG g a": {code: 1, out: "usermod: no\n"}})
		if want := []string{"usermod -rG g a"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("commands = %q, want %q", calls, want)
		}
		if len(changes) != 1 || !strings.HasPrefix(raltest.ErrText(changes[0].Err), "Execution of '") {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("gid", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"gid": 7.0}), false,
			[]nameservice.GroupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -g 7 g": {code: 4, out: "groupmod: GID '7' already exists\n"}})
		if len(changes) != 1 || !strings.HasPrefix(raltest.ErrText(changes[0].Err), "Could not set gid on group[g]: Execution of '") {
			t.Errorf("changes = %+v", changes)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		changes, _, _ := applyGroup(t, groupRes("g", map[string]any{"gid": 7.0}), false,
			[]nameservice.GroupEntry{{Name: "g", GID: 1}}, map[string]runResult{"groupmod -g 7 g": {err: errors.New("fork/exec: no such file")}})
		if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "Could not set gid on group[g]: fork/exec: no such file" {
			t.Errorf("changes = %+v", changes)
		}
	})
}

// SIMP's Group[root]: inclusive membership already as wanted.
func TestGroupMembersAuthInSyncRunsNothing(t *testing.T) {
	changes, calls, ns := applyGroup(t, groupRes("root", map[string]any{"members": []any{"root"}, "auth_membership": true, "gid": 0.0}), false,
		[]nameservice.GroupEntry{{Name: "root", GID: 0, Members: []string{"root"}}}, nil)
	if len(changes) != 0 || len(calls) != 0 || ns.userReads != 0 {
		t.Errorf("changes = %+v, commands = %q, user lookups = %d", changes, calls, ns.userReads)
	}
}

// M42.
func TestGroupDelete(t *testing.T) {
	dir := withShadowBin(t)
	runner := &recordingRunner{results: map[string]runResult{
		"groupdel g16c": {code: 8, out: "groupdel: cannot remove the primary group of user 'u16c'\n"}}}
	ns := &groupNSS{FakeNSS: raltest.FakeNSS{Groups: []nameservice.GroupEntry{{Name: "g16b", GID: 3016}, {Name: "g16c", GID: 3017}}}, runner: runner}
	g := Group{ns: ns, runner: runner}

	changes, _, err := g.Apply(context.Background(), groupRes("g16b", map[string]any{"ensure": "absent"}), false)
	want := []ral.Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed", Name: "group_removed"}}
	if err != nil || !reflect.DeepEqual(changes, want) {
		t.Errorf("g16b: %+v, %v", changes, err)
	}

	changes, _, _ = g.Apply(context.Background(), groupRes("g16c", map[string]any{"ensure": "absent"}), false)
	wantErr := "Could not delete group g16c: Execution of '" + dir + "/groupdel g16c' returned 8: groupdel: cannot remove the primary group of user 'u16c'"
	if len(changes) != 1 || raltest.ErrText(changes[0].Err) != wantErr {
		t.Errorf("g16c: %+v, want Err %q", changes, wantErr)
	}

	src := sources.Sources{Resource: "/Stage[main]/Main/Group[g16d]"}
	ctx, logs := raltest.DebugContext("debug")
	changes, _, err = g.Apply(sources.WithSources(ctx, src), groupRes("g16d", map[string]any{"ensure": "absent"}), false)
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
		[]nameservice.GroupEntry{{Name: "g15a", GID: 3000}}, nil)
	if len(changes) != 2 || changes[0].Message != "gid changed 3000 to 3001 (noop)" ||
		changes[1].Message != "members changed  to nosuchuser (noop)" || changes[1].Err != nil {
		t.Errorf("modify: %+v", changes)
	}
	if len(calls) != 0 || ns.userReads != 0 {
		t.Errorf("modify: commands %q, user lookups %d", calls, ns.userReads)
	}

	changes, calls, _ = applyGroup(t, groupRes("g16b", map[string]any{"ensure": "absent"}), true, []nameservice.GroupEntry{{Name: "g16b", GID: 1}}, nil)
	if len(changes) != 1 || changes[0].Message != "removed (noop)" || len(calls) != 0 {
		t.Errorf("delete: %+v, %q", changes, calls)
	}
}

// Ruby confines groupadd on its three commands; without them the provider is
// unsuitable and transaction.rb:130 fails the resource.
func TestGroupMissingCommandFailsTheResource(t *testing.T) {
	saved := nameservice.ShadowPath
	nameservice.ShadowPath = []string{t.TempDir()}
	t.Cleanup(func() { nameservice.ShadowPath = saved })
	runner := &recordingRunner{}
	_, _, err := Group{ns: &groupNSS{runner: runner}, runner: runner}.Apply(context.Background(), groupRes("g", nil), true)
	if raltest.ErrText(err) != "Provider groupadd is not functional on this host" {
		t.Errorf("err = %v", err)
	}
}

func TestGroupPurgeWithoutUsermod(t *testing.T) {
	dir := withShadowBin(t)
	if err := os.Remove(filepath.Join(dir, "usermod")); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	ns := &groupNSS{FakeNSS: raltest.FakeNSS{Users: hostUsers, Groups: []nameservice.GroupEntry{{Name: "g", GID: 1, Members: []string{"a"}}}}, runner: runner}
	changes, _, err := Group{ns: ns, runner: runner}.Apply(context.Background(),
		groupRes("g", map[string]any{"members": []any{"bin"}, "auth_membership": true}), false)
	if err != nil || len(changes) != 1 || raltest.ErrText(changes[0].Err) != "Command purgemember is missing" {
		t.Errorf("changes = %+v, err = %v", changes, err)
	}
}

// redactRecorder is a shadow runner that records the Redact flag of each call.
type redactRecorder struct {
	redact  []bool
	results map[string]runResult
}

func (r *redactRecorder) Run(_ context.Context, o command.ArgvOpts, name string, args ...string) (int, string, error) {
	r.redact = append(r.redact, o.Redact)
	res := r.results[strings.Join(append([]string{filepath.Base(name)}, args...), " ")]
	return res.code, res.out, res.err
}

// D-78: a Sensitive gid is [redacted] where Ruby prints it, in the format
// refusal and in the munge failure whose detail repeats the value.
func TestGroupParseRedactsSensitiveGID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gid       string
		sensitive bool
		want      string
	}{
		{"format unmarked", "hunter2", false, "Parameter gid failed on Group[g]: Invalid GID hunter2"},
		{"format marked", "hunter2", true, "Parameter gid failed on Group[g]: Invalid GID [redacted]"},
		{"munge marked", "09", true, "Parameter gid failed on Group[g]: Munging failed for value [redacted] in class gid: invalid value for Integer(): [redacted]"},
		{"munge unmarked octal", "09", false, `Parameter gid failed on Group[g]: Munging failed for value "09" in class gid: invalid value for Integer(): "09"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := groupRes("g", map[string]any{"gid": tc.gid})
			if tc.sensitive {
				r.SensitiveParameters = []string{"gid"}
			}
			_, err := ParseGroup(r)
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// D-78: a marked member is [redacted] in the lookup failure, and a marked
// property's commands run redacted with their output hidden.
func TestGroupApplyRedactsSensitiveValues(t *testing.T) {
	dir := withShadowBin(t)
	for _, tc := range []struct {
		name      string
		params    map[string]any
		marked    []string
		groups    []nameservice.GroupEntry
		results   map[string]runResult
		wantErr   string
		wantRedac bool
	}{
		{"member unmarked", map[string]any{"members": []any{"nosuch"}}, nil, nil, nil,
			"Could not set 'present' on ensure: can't find user for nosuch (file: /etc/site.pp, line: 3)", false},
		{"member marked", map[string]any{"members": []any{"nosuch"}}, []string{"members"}, nil, nil,
			"Could not set 'present' on ensure: can't find user for [redacted] (file: /etc/site.pp, line: 3)", true},
		{"create unmarked", map[string]any{"gid": 4321.0}, nil, nil,
			map[string]runResult{"groupadd -g 4321 g": {code: 4, out: "groupadd: GID '4321' already exists\n"}},
			"Could not create group g: Execution of '" + dir + "/groupadd -g 4321 g' returned 4: groupadd: GID '4321' already exists", false},
		{"create marked", map[string]any{"gid": 4321.0}, []string{"gid"}, nil,
			map[string]runResult{"groupadd -g 4321 g": {code: 4, out: "groupadd: GID '4321' already exists\n"}},
			"Could not create group g: Execution of '[redacted]' returned 4: [output redacted]", true},
		{"create with marked members keeps groupadd output", map[string]any{"gid": 4321.0, "members": []any{"bin"}}, []string{"members"}, nil,
			map[string]runResult{"groupadd -g 4321 g": {code: 4, out: "groupadd: GID '4321' already exists\n"}},
			"Could not create group g: Execution of '[redacted]' returned 4: groupadd: GID '4321' already exists", true},
		{"modify gid marked", map[string]any{"gid": 7.0}, []string{"gid"}, []nameservice.GroupEntry{{Name: "g", GID: 1}},
			map[string]runResult{"groupmod -g 7 g": {code: 4, out: "groupmod: GID '7' already exists\n"}},
			"Could not set gid on group[g]: Execution of '[redacted]' returned 4: [output redacted]", true},
		{"modify gid beside a marked members stays plain", map[string]any{"gid": 7.0}, []string{"members"}, []nameservice.GroupEntry{{Name: "g", GID: 1}},
			map[string]runResult{"groupmod -g 7 g": {code: 4, out: "groupmod: GID '7' already exists\n"}},
			"Could not set gid on group[g]: Execution of '" + dir + "/groupmod -g 7 g' returned 4: groupmod: GID '7' already exists", false},
		{"members marked", map[string]any{"members": []any{"bin"}}, []string{"members"}, []nameservice.GroupEntry{{Name: "g", GID: 1}},
			map[string]runResult{"groupmod -aU bin g": {code: 6, out: "groupmod: bin\n"}},
			"Could not set members on group[g]: Execution of '[redacted]' returned 6: [output redacted]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &redactRecorder{results: tc.results}
			r := groupRes("g", tc.params)
			r.SensitiveParameters = tc.marked
			ns := &groupNSS{FakeNSS: raltest.FakeNSS{Users: hostUsers, Groups: tc.groups}, runner: &recordingRunner{}}
			changes, _, err := Group{ns: ns, runner: runner}.Apply(context.Background(), r, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) == 0 || raltest.ErrText(changes[0].Err) != tc.wantErr {
				t.Fatalf("changes = %+v, want Err %q", changes, tc.wantErr)
			}
			if tc.wantRedac && (len(runner.redact) == 0 || !runner.redact[len(runner.redact)-1]) {
				t.Errorf("last command ran unredacted: %v", runner.redact)
			}
			if !tc.wantRedac && slices.Contains(runner.redact, true) {
				t.Errorf("an unmarked command ran redacted: %v", runner.redact)
			}
		})
	}
}
