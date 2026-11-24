package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/group"
	"github.com/voxagent/voxagent/internal/units/user"
)

// nativeUserGroup says whether the user and group units each route cat natively
// on p, as nativeProviders decides it.
func nativeUserGroup(p ral.Platform, cat *catalog.Catalog) (hasUser, hasGroup bool) {
	env := &unit.Env{Catalog: cat, Platform: p}
	for _, pr := range unit.RouteAll(env, []unit.Unit{group.Unit, user.Unit}).Providers {
		switch pr.Type() {
		case "user":
			hasUser = true
		case "group":
			hasGroup = true
		}
	}
	return hasUser, hasGroup
}

// fakePlatform answers the user and group units' probes without the build host.
type fakePlatform struct{ release string }

func (fakePlatform) Executable(string) bool       { return false }
func (f fakePlatform) OSRelease() ([]byte, error) { return []byte(f.release), nil }

// User and Group are native only where their contract was measured (spec
// §2); elsewhere each type still needs the extension host.
func TestUserGroupAreNativeOnlyOnTheMeasuredPlatform(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Group", Title: "g"}, {Type: "User", Title: "u"}}}
	for _, tc := range []struct {
		name    string
		release string
		native  bool
	}{
		{"CentOS Stream 10", "ID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"10\"\n", true},
		{"RHEL 9", "ID=\"rhel\"\nVERSION_ID=\"9.4\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed := map[string]struct{}{}
			natives, _ := nativeProviders(cat, applyOptions{}, fakePlatform{tc.release})
			for _, p := range natives {
				claimed[strings.ToLower(p.Type())] = struct{}{}
			}
			needed := nonNativeTypes(cat, claimed)
			for _, typ := range []string{"group", "user"} {
				if got := !slices.Contains(needed, typ); got != tc.native {
					t.Errorf("%s native = %v, want %v (host needs %q)", typ, got, tc.native, needed)
				}
			}
		})
	}
}

// The feature lines are logged while the catalog is converted: after
// validation, before "Applying configuration", at the resource's own path.
func TestGroupConversionDebugIsLoggedBeforeApplying(t *testing.T) {
	probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Group", Title: "root"}}}
	if _, group := nativeUserGroup(ral.HostPlatform, probe); !group {
		t.Skip("group is not native on this host")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{
		{"type": "Group", "title": "root", "tags": []any{}, "parameters": map[string]any{"attributes": []any{"x=y"}}},
	})
	stdout, stderr, code := run(t, bin, "apply", "--catalog", c, "--report-dir", filepath.Join(dir, "r"), "--debug", "--noop", "--no-ruby")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	aix := "Debug: /Group[root]: Provider groupadd does not support features manages_aix_lam; not managing attribute attributes\n"
	local := "Debug: /Group[root]: Provider groupadd does not support features manages_local_users_and_groups; not managing attribute forcelocal\n"
	applying := "Info: Applying configuration version '1'\n"
	a, l, v := strings.Index(stdout, aix), strings.Index(stdout, local), strings.Index(stdout, applying)
	if a < 0 || l < 0 || v < 0 || !(a < l && l < v) {
		t.Errorf("want the aix line, the forcelocal line, then Applying; got\n%s", stdout)
	}
}

// M43: a user declared before its groups is applied after them, because it
// autorequires the group of its numeric gid and the groups it lists.
func TestUserIsAppliedAfterItsGroups(t *testing.T) {
	probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: "User", Title: "u"}, {Type: "Group", Title: "g"}}}
	if user, group := nativeUserGroup(ral.HostPlatform, probe); !user || !group {
		t.Skip("user and group are not native on this host")
	}
	bin := buildBinary(t)
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"numeric gid", map[string]any{"gid": "63017"}},
		{"groups", map[string]any{"groups": []any{"voxg17"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := writeCatalog(t, dir, []map[string]any{
				{"type": "User", "title": "voxu17", "tags": []any{}, "parameters": tc.params},
				{"type": "Group", "title": "voxg17", "tags": []any{}, "parameters": map[string]any{"gid": 63017}},
			})
			stdout, stderr, code := run(t, bin, "apply", "--catalog", c, "--report-dir", filepath.Join(dir, "r"), "--noop", "--no-ruby")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, stderr)
			}
			g, u := strings.Index(stdout, "Group[voxg17]/"), strings.Index(stdout, "User[voxu17]/")
			if g < 0 || u < 0 || g > u {
				t.Errorf("want the group before the user; got\n%s", stdout)
			}
		})
	}
}

// The delegation reasons are logged at --debug, to the console and the
// report; a Sensitive value must reach neither, nor must a Sensitive provider.
// Only shapes the host accepts are here: a bad provider or gid is refused by
// the host's own Ruby validation before the line is logged (route_test.go
// covers those reasons).
func TestDebugDelegationReasonsRedactSensitiveValues(t *testing.T) {
	bin := buildBinary(t)
	cases := []struct {
		name, typ, attr string
		value           any
		reason          string
	}{
		{"user comment", "User", "comment", []any{"D8SECRET-user-comment"}, "User[leak]: comment => [redacted] is not implemented"},
		{"group members", "Group", "members", []any{"D8SECRET-group-members", 1.0}, "Group[leak]: members => [redacted] is not implemented"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			res := markedResource(c.typ, "leak", map[string]any{c.attr: c.value}, c.attr)
			res["kind"] = "compilable_type"
			cat := writeCatalog(t, dir, []map[string]any{res})
			reports := filepath.Join(dir, "reports")
			// --noop: the host must not create the account.
			stdout, stderr, _ := run(t, bin, append([]string{"apply", "--catalog", cat, "--report-dir", reports, "--debug", "--noop"}, hostArgs(t)...)...)
			want := "Debug: voxagent: " + strings.ToLower(c.typ) + " applied via extension host: " + c.reason + "\n"
			if !strings.Contains(stdout, want) {
				t.Errorf("console lacks %q:\n%s", want, stdout)
			}
			assertNoLeak(t, "console", stdout+stderr, nil)
			rep := readReport(t, reports)
			reportLines := debugReportLines(t, rep)
			if !slices.Contains(reportLines, strings.TrimSuffix(strings.TrimPrefix(want, "Debug: voxagent: "), "\n")) {
				t.Errorf("report lacks the redacted reason; has %q", reportLines)
			}
			raw, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			assertNoLeak(t, "report", string(raw), nil)
		})
	}
}

// Ruby's own refusal of a Sensitive value (Invalid value ...) reaches the
// console as a pre-flight error; it must show [redacted], not the value.
func TestInvalidSensitiveUserGroupValueIsRedactedInRefusal(t *testing.T) {
	bin := buildBinary(t)
	for _, typ := range []string{"User", "Group"} {
		t.Run(typ, func(t *testing.T) {
			probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: typ, Title: "probe"}}}
			user, group := nativeUserGroup(ral.HostPlatform, probe)
			if typ == "User" && !user {
				t.Skip("user is not native on this host")
			}
			if typ == "Group" && !group {
				t.Skip("group is not native on this host")
			}
			dir := t.TempDir()
			cat := writeCatalog(t, dir, []map[string]any{
				markedResource(typ, "leak", map[string]any{"ensure": "D8SECRET-" + typ + "-ensure"}, "ensure"),
			})
			stdout, stderr, code := run(t, bin, "apply", "--catalog", cat, "--report-dir", filepath.Join(dir, "reports"), "--debug", "--noop")
			if code == 0 {
				t.Fatalf("an invalid ensure applied:\n%s%s", stdout, stderr)
			}
			assertNoLeak(t, "console", stdout+stderr, nil)
			if !strings.Contains(stderr, "Invalid value [redacted]") {
				t.Errorf("stderr lacks the redacted refusal:\n%s", stderr)
			}
		})
	}
}
