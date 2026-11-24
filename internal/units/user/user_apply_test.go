package user

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/voxagent/voxagent/internal/ral"
)

// testHash is M7's password hash. It is a measurement fixture, not a secret,
// but the tests still print it only as [redacted] (scrub), as the agent must.
const testHash = "$6$saltsalt1234$HimiUiYsyc2p0rqG.cX8/iShr9/K5g3WzW5..pCTGTgbsq96UiBD4shzEFnvokdAe6TQ0basOKThrVS/UCByk."

func scrub(v any) string { return strings.ReplaceAll(fmt.Sprintf("%q", v), testHash, "[redacted]") }

// runResult is what argvRunner answers for one command (copied from group, whose
// test files cannot be imported).
type runResult struct {
	code int
	out  string
	err  error
}

// argvCall is one command argvRunner saw: its argv, with the shadow-utils
// directory stripped, and its options.
type argvCall struct {
	argv []string
	opts command.ArgvOpts
}

// argvRunner is a shadowRunner that keeps each argv element apart, so a
// comment with a space or an empty expiry is checked as one element. It answers
// from results keyed by the command's base name and arguments joined by
// spaces ("usermod -u 2001 u8d"); unlisted commands succeed with no output.
type argvRunner struct {
	dir     string
	calls   []argvCall
	results map[string]runResult
}

func (r *argvRunner) Run(_ context.Context, o command.ArgvOpts, name string, args ...string) (int, string, error) {
	argv := append([]string{strings.TrimPrefix(name, r.dir+"/")}, args...)
	r.calls = append(r.calls, argvCall{argv: argv, opts: o})
	res := r.results[strings.Join(argv, " ")]
	return res.code, res.out, res.err
}

func (r *argvRunner) argvs() [][]string {
	out := make([][]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.argv
	}
	return out
}

// userNSS is raltest.FakeNSS with shadow entries.
type userNSS struct {
	raltest.FakeNSS
	shadows map[string]nameservice.ShadowEntry
}

func (n userNSS) Shadow(name string) (nameservice.ShadowEntry, bool, error) {
	s, ok := n.shadows[name]
	return s, ok, nil
}

// withUserBin points nameservice.ShadowCommand at stand-ins for useradd's commands.
func withUserBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range []string{"useradd", "userdel", "usermod", "chage", "chpasswd"} {
		if err := os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := nameservice.ShadowPath
	nameservice.ShadowPath = []string{dir}
	t.Cleanup(func() { nameservice.ShadowPath = saved })
	return dir
}

// The groups the measured image has, plus the ones the cases seed.
var userHostGroups = []nameservice.GroupEntry{
	{Name: "root", GID: 0}, {Name: "adm", GID: 4}, {Name: "wheel", GID: 10}, {Name: "audio", GID: 63},
	{Name: "g2", GID: 2200}, {Name: "g4", GID: 2500},
}

func applyUser(t *testing.T, ctx context.Context, r catalog.Resource, noop bool, ns nameservice.NameService, results map[string]runResult) ([]ral.Change, *argvRunner) {
	t.Helper()
	runner := &argvRunner{dir: withUserBin(t), results: results}
	changes, warnings, err := User{ns: ns, runner: runner}.Apply(ctx, r, noop)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	return changes, runner
}

func TestUserCreateArgv(t *testing.T) {
	for _, tc := range []struct {
		name   string
		title  string
		params map[string]any
		groups []nameservice.GroupEntry
		want   [][]string
		redact bool
	}{
		{"M1 minimal", "u1", map[string]any{"ensure": "present"}, nil, [][]string{{"useradd", "-M", "u1"}}, false},
		{"M31 implicit ensure", "u11", map[string]any{"uid": 2011.0}, nil, [][]string{{"useradd", "-u", "2011", "-M", "u11"}}, false},
		{"M5 same-name group", "u3", map[string]any{"ensure": "present"}, []nameservice.GroupEntry{{Name: "u3", GID: 1000}},
			[][]string{{"useradd", "-g", "u3", "-M", "u3"}}, false},
		{"M3 full without password", "u2b", map[string]any{
			"ensure": "present", "comment": "A, B", "expiry": "2030-01-02", "gid": "g2", "groups": []any{"wheel", "adm"},
			"home": "/home/u2", "shell": "/bin/bash", "uid": 2002.0, "allowdupe": true, "managehome": true,
			"password_min_age": 1.0, "password_max_age": 90.0, "password_warn_days": 7.0,
		}, nil, [][]string{
			{"useradd", "-c", "A, B", "-e", "2030-01-02", "-g", "g2", "-G", "adm,wheel", "-d", "/home/u2", "-s", "/bin/bash", "-u", "2002", "-o", "-m", "u2b"},
			{"chage", "-m", "1", "-M", "90", "-W", "7", "u2b"},
		}, false},
		{"M2 full with password", "u2", map[string]any{
			"ensure": "present", "comment": "A, B", "uid": 2002.0, "password": testHash, "password_max_age": 90.0,
		}, nil, [][]string{
			{"useradd", "-c", "A, B", "-u", "2002", "-M", "u2"},
			{"chage", "-M", "90", "u2"},
			{"usermod", "-p", testHash, "u2"},
		}, true},
		{"expiry absent", "u", map[string]any{"expiry": "absent"}, nil, [][]string{{"useradd", "-e", "", "-M", "u"}}, false},
		{"empty comment omitted", "u", map[string]any{"comment": ""}, nil, [][]string{{"useradd", "-M", "u"}}, false},
		{"system user", "u", map[string]any{"ensure": "present", "system": true}, nil, [][]string{{"useradd", "-M", "-r", "u"}}, false},
		{"gid number", "u", map[string]any{"gid": "0010"}, nil, [][]string{{"useradd", "-g", "8", "-M", "u"}}, false},
		// Under minimum the should csv adds the groups that already list the
		// not-yet-created user (posix.rb's fallback scan).
		{"minimum adds listed groups", "u", map[string]any{"groups": []any{"wheel"}},
			[]nameservice.GroupEntry{{Name: "audio", GID: 63, Members: []string{"u"}}},
			[][]string{{"useradd", "-G", "audio,wheel", "-M", "u"}}, false},
		{"inclusive ignores listed groups", "u", map[string]any{"groups": []any{"wheel"}, "membership": "inclusive"},
			[]nameservice.GroupEntry{{Name: "audio", GID: 63, Members: []string{"u"}}},
			[][]string{{"useradd", "-G", "wheel", "-M", "u"}}, false},
		{"empty inclusive groups omitted", "u", map[string]any{"groups": []any{}, "membership": "inclusive"}, nil,
			[][]string{{"useradd", "-M", "u"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}, Groups: tc.groups}}
			changes, runner := applyUser(t, context.Background(), userRes(tc.title, tc.params), false, ns, nil)
			if got := runner.argvs(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("argv:\n got %s\nwant %s", scrub(got), scrub(tc.want))
			}
			for _, c := range runner.calls {
				if c.opts.Redact != tc.redact || c.opts.Stdin != nil {
					t.Errorf("%s: opts = %+v, want Redact %v and no stdin", scrub(c.argv), c.opts, tc.redact)
				}
			}
			want := []ral.Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created", Name: "user_created"}}
			if !reflect.DeepEqual(changes, want) {
				t.Errorf("changes = %s, want %s", scrub(changes), scrub(want))
			}
		})
	}
}

func TestUserCreateFailures(t *testing.T) {
	notExec := filepath.Join(t.TempDir(), "sh")
	if err := os.WriteFile(notExec, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		title    string
		params   map[string]any
		results  map[string]runResult
		wantErr  string // dir/ is the shadow-utils directory
		wantCmds int
	}{
		{"M22 uid octal", "u9", map[string]any{"uid": "0010"},
			map[string]runResult{"useradd -u 8 -M u9": {code: 4, out: "useradd: UID 8 is not unique\n"}},
			"Could not create user u9: Execution of 'dir/useradd -u 8 -M u9' returned 4: useradd: UID 8 is not unique", 1},
		{"M23 uid abc", "u9", map[string]any{"uid": "abc"},
			map[string]runResult{"useradd -u abc -M u9": {code: 3, out: "useradd: invalid user ID 'abc'\n"}},
			"Could not create user u9: Execution of 'dir/useradd -u abc -M u9' returned 3: useradd: invalid user ID 'abc'", 1},
		{"chage fails", "u", map[string]any{"password_min_age": 1.0},
			map[string]runResult{"chage -m 1 u": {code: 1, out: "chage: bad\n"}},
			"Could not create user u: Execution of 'dir/chage -m 1 u' returned 1: chage: bad", 2},
		{"M14 bad shell", "u8a", map[string]any{"shell": "/nonexistent/sh"}, nil, "Shell /nonexistent/sh must exist", 0},
		{"shell not executable", "u", map[string]any{"shell": notExec}, nil, "Shell " + notExec + " must be executable", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
			changes, runner := applyUser(t, context.Background(), userRes(tc.title, tc.params), false, ns, tc.results)
			if len(changes) != 1 || changes[0].Message != "created" || changes[0].Property != "ensure" {
				t.Fatalf("changes = %+v", changes)
			}
			want := strings.ReplaceAll(tc.wantErr, "dir/", runner.dir+"/")
			if got := raltest.ErrText(changes[0].Err); got != want {
				t.Errorf("err:\n got %q\nwant %q", got, want)
			}
			if len(runner.calls) != tc.wantCmds {
				t.Errorf("ran %d commands, want %d: %v", len(runner.calls), tc.wantCmds, runner.argvs())
			}
		})
	}
}

// A user with a password has every create command redacted, so a failure
// names the command as [redacted] (M2, util/execution.rb:162).
func TestUserCreateFailureWithPasswordIsRedacted(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
	for _, tc := range []struct {
		failing string
		want    string
	}{
		{"useradd -M u", "Could not create user u: Execution of '[redacted]' returned 4: useradd: boom"},
		{"usermod -p " + testHash + " u", "Could not set password on user[u]: Execution of '[redacted]' returned 4: useradd: boom"},
	} {
		changes, runner := applyUser(t, context.Background(), userRes("u", map[string]any{"password": testHash}), false, ns,
			map[string]runResult{tc.failing: {code: 4, out: "useradd: boom\n"}})
		if len(changes) != 1 || raltest.ErrText(changes[0].Err) != tc.want {
			t.Errorf("changes = %s, want one with err %q", scrub(changes), tc.want)
		}
		if strings.Contains(raltest.ErrText(changes[0].Err), testHash) || len(runner.calls) == 0 {
			t.Errorf("calls %s", scrub(runner.argvs()))
		}
	}
}

// existingU4 is M7's seeded user: uid and gid 2001 (its private group), in adm,
// with a shadow entry whose expiry is 2030-01-01 (day 21915).
func existingU4() userNSS {
	groups := append(slices.Clone(userHostGroups), nameservice.GroupEntry{Name: "u4", GID: 2001})
	groups[1].Members = []string{"u4"} // adm
	return userNSS{
		FakeNSS: raltest.FakeNSS{
			Users:    map[string]nameservice.PasswdEntry{"u4": {Name: "u4", UID: 2001, GID: 2001, Gecos: "old comment", Dir: "/home/old4", Shell: "/bin/sh"}},
			Groups:   groups,
			Grouplst: map[string][]int64{"u4": {2001, 4}},
		},
		shadows: map[string]nameservice.ShadowEntry{"u4": {Hash: "$6$pepperpepper$old", Min: 2, Max: 50, Warn: 3, Expire: 21915}},
	}
}

func m7Params() map[string]any {
	return map[string]any{
		"uid": 2002.0, "gid": "g4", "comment": "new, comment", "home": "/home/new4", "shell": "/bin/bash",
		"expiry": "absent", "password": testHash, "password_min_age": 5.0, "password_max_age": 60.0, "password_warn_days": 9.0,
		"groups": []any{"wheel"}, "membership": "minimum",
	}
}

func TestUserModifyM7(t *testing.T) {
	changes, runner := applyUser(t, context.Background(), userRes("u4", m7Params()), false, existingU4(), nil)
	want := []struct {
		prop, msg string
		was, is   any
	}{
		{"home", "home changed '/home/old4' to '/home/new4'", "/home/old4", "/home/new4"},
		{"uid", "uid changed 2001 to 2002", int64(2001), int64(2002)},
		{"gid", "gid changed 2001 to 'g4'", int64(2001), "g4"},
		{"comment", "comment changed 'old comment' to 'new, comment'", "old comment", "new, comment"},
		{"shell", "shell changed '/bin/sh' to '/bin/bash'", "/bin/sh", "/bin/bash"},
		{"password", "changed [redacted] to [redacted]", "[redacted]", "[redacted]"},
		{"password_min_age", "password_min_age changed 2 to 5", int64(2), int64(5)},
		{"password_max_age", "password_max_age changed 50 to 60", int64(50), int64(60)},
		{"password_warn_days", "password_warn_days changed 3 to 9", int64(3), int64(9)},
		{"groups", "groups changed adm to ['adm', 'wheel']", []string{"adm"}, "adm,wheel"},
		{"expiry", "undefined 'expiry' from '2030-01-01'", "2030-01-01", "absent"},
	}
	if len(changes) != len(want) {
		t.Fatalf("got %d changes, want %d: %s", len(changes), len(want), scrub(changes))
	}
	for i, w := range want {
		c := changes[i]
		if c.Property != w.prop || c.Message != w.msg || !reflect.DeepEqual(c.Was, w.was) || !reflect.DeepEqual(c.Is, w.is) || c.Err != nil || c.Name != "" {
			t.Errorf("change %d = %s\nwant %s %q %#v -> %#v", i, scrub(c), w.prop, w.msg, w.was, w.is)
		}
	}
	wantArgv := [][]string{
		{"usermod", "-d", "/home/new4", "u4"},
		{"usermod", "-u", "2002", "u4"},
		{"usermod", "-g", "2500", "u4"},
		{"usermod", "-c", "new, comment", "u4"},
		{"usermod", "-s", "/bin/bash", "u4"},
		{"chpasswd", "-e"},
		{"chage", "-m", "5", "u4"},
		{"chage", "-M", "60", "u4"},
		{"chage", "-W", "9", "u4"},
		{"usermod", "-G", "adm,wheel", "u4"},
		{"usermod", "-e", "", "u4"},
	}
	if got := runner.argvs(); !reflect.DeepEqual(got, wantArgv) {
		t.Errorf("argv:\n got %s\nwant %s", scrub(got), scrub(wantArgv))
	}
	// Only the password command is redacted on modify: set() asks
	// has_sensitive_data?(param) for its own property (M7 debug.txt).
	for _, c := range runner.calls {
		isPassword := c.argv[0] == "chpasswd"
		if c.opts.Redact != isPassword {
			t.Errorf("%s: Redact = %v", scrub(c.argv), c.opts.Redact)
		}
		wantStdin := ""
		if isPassword {
			wantStdin = "u4:" + testHash + "\n"
		}
		if string(c.opts.Stdin) != wantStdin {
			t.Errorf("%s: stdin = %s", scrub(c.argv), scrub(string(c.opts.Stdin)))
		}
	}
}

// has_sensitive_data? (useradd.rb:311-318) redacts every create command when
// any property is marked Sensitive, not only password (nameservice.rb:162),
// and a modify command when its own property is (nameservice.rb:270).
func TestUserMarkedPropertyRedactsItsCommands(t *testing.T) {
	marked := func(title string, params map[string]any, names ...string) catalog.Resource {
		r := userRes(title, params)
		r.SensitiveParameters = names
		return r
	}
	t.Run("create", func(t *testing.T) {
		ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
		r := marked("u", map[string]any{"comment": "SECRET", "password_max_age": 90.0}, "comment")
		changes, runner := applyUser(t, context.Background(), r, false, ns, nil)
		if len(changes) != 1 || changes[0].Err != nil || len(runner.calls) != 2 {
			t.Fatalf("changes = %+v, calls = %v", changes, runner.argvs())
		}
		for _, c := range runner.calls {
			if !c.opts.Redact {
				t.Errorf("%v: not redacted", c.argv)
			}
		}
		changes, _ = applyUser(t, context.Background(), r, false, ns,
			map[string]runResult{"useradd -c SECRET -M u": {code: 6, out: "boom\n"}})
		if want := "Could not create user u: Execution of '[redacted]' returned 6: boom"; raltest.ErrText(changes[0].Err) != want {
			t.Errorf("err = %q, want %q", raltest.ErrText(changes[0].Err), want)
		}
	})
	t.Run("modify", func(t *testing.T) {
		r := marked("u4", map[string]any{"comment": "SECRET", "home": "/home/new4", "password_min_age": 5.0, "password_max_age": 60.0},
			"comment", "password_min_age")
		changes, runner := applyUser(t, context.Background(), r, false, existingU4(),
			map[string]runResult{"usermod -c SECRET u4": {code: 6, out: "boom\n"}})
		wantRedact := map[string]bool{"usermod -d": false, "usermod -c": true, "chage -m": true, "chage -M": false}
		if len(runner.calls) != len(wantRedact) {
			t.Fatalf("calls = %v", runner.argvs())
		}
		for _, c := range runner.calls {
			if want := wantRedact[c.argv[0]+" "+c.argv[1]]; c.opts.Redact != want {
				t.Errorf("%v: Redact = %v, want %v", c.argv, c.opts.Redact, want)
			}
		}
		for _, c := range changes {
			if c.Property == "comment" {
				if want := "Could not set comment on user[u4]: Execution of '[redacted]' returned 6: boom"; raltest.ErrText(c.Err) != want {
					t.Errorf("err = %q, want %q", raltest.ErrText(c.Err), want)
				}
			}
		}
	})
}

func TestUserModifyNoopM7(t *testing.T) {
	changes, runner := applyUser(t, context.Background(), userRes("u4", m7Params()), true, existingU4(), nil)
	if len(runner.calls) != 0 {
		t.Errorf("noop ran %s", scrub(runner.argvs()))
	}
	if len(changes) != 11 {
		t.Fatalf("got %d changes", len(changes))
	}
	if got := changes[5].Message; got != "current_value [redacted], should be [redacted] (noop)" {
		t.Errorf("password noop message = %q", got)
	}
	if got := changes[2].Message; got != "gid changed 2001 to 'g4' (noop)" {
		t.Errorf("gid noop message = %q", got)
	}
}

// chpasswd's exit status is ignored; any output fails the property, unwrapped
// and verbatim (M15).
func TestUserChpasswdOutputFails(t *testing.T) {
	out := "chpasswd: existing lock file /etc/passwd.lock with an invalid PID '1\n'\nchpasswd: cannot lock /etc/passwd; try again later.\n"
	changes, _ := applyUser(t, context.Background(), userRes("u4", map[string]any{"password": testHash}), false, existingU4(),
		map[string]runResult{"chpasswd -e": {code: 0, out: out}})
	if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "chpasswd said "+out {
		t.Fatalf("changes = %s", scrub(changes))
	}
	c := changes[0]
	if c.WasDisplay != "[redacted]" || c.IsDisplay != "[redacted]" || !c.DisplayVerbatim {
		t.Errorf("display = %q %q %v", c.WasDisplay, c.IsDisplay, c.DisplayVerbatim)
	}
	// Exit status alone is not a failure.
	changes, _ = applyUser(t, context.Background(), userRes("u4", map[string]any{"password": testHash}), false, existingU4(),
		map[string]runResult{"chpasswd -e": {code: 1}})
	if len(changes) != 1 || changes[0].Err != nil {
		t.Errorf("changes = %s", scrub(changes))
	}
}

func TestUserPasswordInSync(t *testing.T) {
	ns := existingU4()
	ns.shadows["u4"] = nameservice.ShadowEntry{Hash: testHash, Min: -1, Max: -1, Warn: -1, Expire: -1}
	changes, runner := applyUser(t, context.Background(), userRes("u4", map[string]any{"password": testHash, "expiry": "absent"}), false, ns, nil)
	if len(changes) != 0 || len(runner.calls) != 0 {
		t.Errorf("changes %s, calls %s", scrub(changes), scrub(runner.argvs()))
	}
}

func TestUserGroupsMembership(t *testing.T) {
	withGroups := func(user string, member ...string) userNSS {
		groups := slices.Clone(userHostGroups)
		var gids []int64
		for i := range groups {
			if slices.Contains(member, groups[i].Name) {
				groups[i].Members = []string{user}
				gids = append(gids, groups[i].GID)
			}
		}
		return userNSS{FakeNSS: raltest.FakeNSS{
			Users:    map[string]nameservice.PasswdEntry{user: {Name: user, UID: 2005, GID: 2005}},
			Groups:   groups,
			Grouplst: map[string][]int64{user: gids},
		}}
	}
	for _, tc := range []struct {
		name    string
		params  map[string]any
		member  []string
		msg     string
		was     []string
		is      string
		argv    []string
		inSync  bool
		display string // IsDisplay of the failure line
	}{
		{"M9 inclusive", map[string]any{"groups": []any{"wheel"}, "membership": "inclusive"}, []string{"adm"},
			"groups changed adm to ['wheel']", []string{"adm"}, "wheel", []string{"usermod", "-G", "wheel", "u"}, false, "'wheel'"},
		{"M9 inclusive empty", map[string]any{"groups": []any{}, "membership": "inclusive"}, []string{"adm", "wheel"},
			"groups changed adm,wheel to []", []string{"adm", "wheel"}, "", []string{"usermod", "-G", "", "u"}, false, "''"},
		{"M45 minimum from none", map[string]any{"groups": []any{"wheel", "adm"}}, nil,
			"groups changed  to ['adm', 'wheel']", []string{}, "adm,wheel", []string{"usermod", "-G", "adm,wheel", "u"}, false, "'adm,wheel'"},
		{"minimum subset in sync", map[string]any{"groups": []any{"adm"}}, []string{"adm", "wheel"}, "", nil, "", nil, true, ""},
		{"minimum empty in sync", map[string]any{"groups": []any{}}, []string{"adm"}, "", nil, "", nil, true, ""},
		{"inclusive same set in sync", map[string]any{"groups": []any{"wheel", "adm"}, "membership": "inclusive"}, []string{"adm", "wheel"}, "", nil, "", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, runner := applyUser(t, context.Background(), userRes("u", tc.params), false, withGroups("u", tc.member...), nil)
			if tc.inSync {
				if len(changes) != 0 || len(runner.calls) != 0 {
					t.Errorf("changes %+v, calls %v", changes, runner.argvs())
				}
				return
			}
			if len(changes) != 1 {
				t.Fatalf("changes = %+v", changes)
			}
			c := changes[0]
			if c.Property != "groups" || c.Message != tc.msg || !reflect.DeepEqual(c.Was, tc.was) || c.Is != tc.is {
				t.Errorf("change = %+v", c)
			}
			if c.WasDisplay != strings.Join(tc.was, ",") || c.IsDisplay != tc.display || !c.DisplayVerbatim {
				t.Errorf("display = %q %q %v", c.WasDisplay, c.IsDisplay, c.DisplayVerbatim)
			}
			if got := runner.argvs(); !reflect.DeepEqual(got, [][]string{tc.argv}) {
				t.Errorf("argv = %q", got)
			}
		})
	}
}

// The current list is shown in getgrouplist's order; only the comparison sorts.
func TestUserGroupsDisplayUsesCurrentOrder(t *testing.T) {
	groups := slices.Clone(userHostGroups)
	groups[1].Members = []string{"u"} // adm
	groups[2].Members = []string{"u"} // wheel
	ns := userNSS{FakeNSS: raltest.FakeNSS{
		Users:    map[string]nameservice.PasswdEntry{"u": {Name: "u", UID: 2005, GID: 2005}},
		Groups:   groups,
		Grouplst: map[string][]int64{"u": {10, 4}},
	}}
	changes, _ := applyUser(t, context.Background(), userRes("u", map[string]any{"groups": []any{"audio"}}), true, ns, nil)
	if len(changes) != 1 || changes[0].Message != "groups changed wheel,adm to ['adm', 'audio', 'wheel'] (noop)" ||
		!reflect.DeepEqual(changes[0].Was, []string{"wheel", "adm"}) || changes[0].Is != "adm,audio,wheel" {
		t.Errorf("changes = %+v", changes)
	}
}

func TestUserModifyFailures(t *testing.T) {
	existing := func(name string, uid int64) userNSS {
		return userNSS{FakeNSS: raltest.FakeNSS{
			Users:  map[string]nameservice.PasswdEntry{name: {Name: name, UID: uid, GID: uid, Shell: "/bin/sh"}},
			Groups: userHostGroups,
		}}
	}
	for _, tc := range []struct {
		name     string
		title    string
		uid      int64
		params   map[string]any
		results  map[string]runResult
		msg      string
		was, is  string // the failure line's displays
		wantErr  string
		wantCmds int
	}{
		{"M14 u8b gid without group", "u8b", 2101, map[string]any{"gid": "nosuchgroup"}, nil,
			"gid changed 2101 to 'nosuchgroup'", "2101", "'nosuchgroup'", "Could not find group(s) nosuchgroup", 0},
		{"gid integer without group", "u", 2192, map[string]any{"gid": 2600.0}, nil,
			"gid changed 2192 to 2600", "2192", "2600", "Could not find group(s) 2600", 0},
		{"M14 u8c groups", "u8c", 2102, map[string]any{"groups": []any{"nosuchgroup"}},
			map[string]runResult{"usermod -G nosuchgroup u8c": {code: 6, out: "usermod: group 'nosuchgroup' does not exist\n"}},
			"groups changed  to ['nosuchgroup']", "", "'nosuchgroup'",
			"Could not set groups on user[u8c]: Execution of 'dir/usermod -G nosuchgroup u8c' returned 6: usermod: group 'nosuchgroup' does not exist", 1},
		{"M14 u8d uid collision", "u8d", 2103, map[string]any{"uid": 2001.0},
			map[string]runResult{"usermod -u 2001 u8d": {code: 4, out: "usermod: UID '2001' already exists\n"}},
			"uid changed 2103 to 2001", "2103", "2001",
			"Could not set uid on user[u8d]: Execution of 'dir/usermod -u 2001 u8d' returned 4: usermod: UID '2001' already exists", 1},
		{"groups with whitespace", "u", 2000, map[string]any{"groups": []any{"a b"}}, nil,
			"groups changed  to ['a b']", "", "'a b'", "Invalid value a b: Groups must be comma-separated", 0},
		{"shell missing", "u", 2000, map[string]any{"shell": "/nonexistent/sh"}, nil,
			"shell changed '/bin/sh' to '/nonexistent/sh'", "'/bin/sh'", "'/nonexistent/sh'", "Shell /nonexistent/sh must exist", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, runner := applyUser(t, context.Background(), userRes(tc.title, tc.params), false, existing(tc.title, tc.uid), tc.results)
			if len(changes) != 1 {
				t.Fatalf("changes = %+v", changes)
			}
			c := changes[0]
			want := strings.ReplaceAll(tc.wantErr, "dir/", runner.dir+"/")
			if c.Message != tc.msg || raltest.ErrText(c.Err) != want {
				t.Errorf("message %q err %q\nwant %q err %q", c.Message, raltest.ErrText(c.Err), tc.msg, want)
			}
			if c.WasDisplay != tc.was || c.IsDisplay != tc.is || !c.DisplayVerbatim {
				t.Errorf("display = %q %q %v, want %q %q", c.WasDisplay, c.IsDisplay, c.DisplayVerbatim, tc.was, tc.is)
			}
			if len(runner.calls) != tc.wantCmds {
				t.Errorf("calls = %v", runner.argvs())
			}
		})
	}
}

func TestUserModifyAllowdupeAndComment(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{"u19c": {Name: "u19c", UID: 2000, Gecos: "had comment"}}}}
	changes, runner := applyUser(t, context.Background(), userRes("u19c", map[string]any{"comment": "", "uid": 0.0, "allowdupe": true}), false, ns, nil)
	if len(changes) != 2 || changes[0].Message != "uid changed 2000 to 0" || changes[1].Message != "comment changed 'had comment' to ''" {
		t.Errorf("changes = %+v", changes)
	}
	want := [][]string{{"usermod", "-u", "0", "-o", "u19c"}, {"usermod", "-c", "", "u19c"}}
	if got := runner.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
}

// A user with no shadow entry reads every shadow property as absent: an age
// or a date is "defined", and expiry absent is in sync (M45, property.rb:205-211).
func TestUserDefinedUndefinedRule(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{"u": {Name: "u", UID: 2000}}}}
	changes, runner := applyUser(t, context.Background(), userRes("u", map[string]any{
		"password_min_age": 1.0, "expiry": "2031-02-03",
	}), false, ns, nil)
	if len(changes) != 2 || changes[0].Message != "defined 'password_min_age' as 1" || changes[0].Was != "absent" ||
		changes[1].Message != "defined 'expiry' as '2031-02-03'" || changes[1].Was != "absent" || changes[1].Is != "2031-02-03" {
		t.Errorf("changes = %+v", changes)
	}
	if changes[0].WasDisplay != "'absent'" || changes[0].IsDisplay != "1" {
		t.Errorf("failure displays = %q %q", changes[0].WasDisplay, changes[0].IsDisplay)
	}
	want := [][]string{{"chage", "-m", "1", "u"}, {"usermod", "-e", "2031-02-03", "u"}}
	if got := runner.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
	changes, _ = applyUser(t, context.Background(), userRes("u", map[string]any{"expiry": "absent"}), false, ns, nil)
	if len(changes) != 0 {
		t.Errorf("expiry absent vs no shadow: %+v", changes)
	}
}

func TestUserDelete(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{"u7a": {Name: "u7a", UID: 2007}}}}
	// userdel is never redacted: delete passes no :sensitive (nameservice.rb:180).
	changes, runner := applyUser(t, context.Background(), userRes("u7a", map[string]any{"ensure": "absent", "managehome": true, "password": testHash}), false, ns, nil)
	want := []ral.Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed", Name: "user_removed"}}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %s", scrub(changes))
	}
	if got := runner.argvs(); !reflect.DeepEqual(got, [][]string{{"userdel", "-r", "u7a"}}) || runner.calls[0].opts.Redact {
		t.Errorf("calls = %+v", runner.calls)
	}
	changes, runner = applyUser(t, context.Background(), userRes("u7a", map[string]any{"ensure": "absent"}), false, ns,
		map[string]runResult{"userdel u7a": {code: 8, out: "userdel: busy\n"}})
	if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "Could not delete user u7a: Execution of '"+runner.dir+"/userdel u7a' returned 8: userdel: busy" {
		t.Errorf("changes = %+v", changes)
	}
	changes, runner = applyUser(t, context.Background(), userRes("u7a", map[string]any{"ensure": "absent"}), true, ns, nil)
	if len(changes) != 1 || changes[0].Message != "removed (noop)" || len(runner.calls) != 0 {
		t.Errorf("noop changes = %+v, calls %v", changes, runner.argvs())
	}
}

// A missing user that is to be absent, or that the catalog manages nothing of
// (M30), is left alone with resource_harness.rb:97's debug line.
func TestUserNothingToManage(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
	for _, params := range []map[string]any{
		{"ensure": "absent", "managehome": true},
		{"roles": "x", "forcelocal": true},
	} {
		ctx, msgs := raltest.DebugCtx()
		changes, runner := applyUser(t, ctx, userRes("u7b", params), false, ns, nil)
		if len(changes) != 0 || len(runner.calls) != 0 {
			t.Errorf("%v: changes %+v calls %v", params, changes, runner.argvs())
		}
		if !slices.Contains(*msgs, "Nothing to manage: no ensure and the resource doesn't exist") {
			t.Errorf("%v: debug = %q", params, *msgs)
		}
	}
}

// Under noop no command runs and no provider check is made: the bad shell is
// "created (noop)" and the unknown gid a plain change (M14 noop.txt).
func TestUserNoopMakesNoProviderChecks(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{"u8b": {Name: "u8b", UID: 2101, GID: 2101}}, Groups: userHostGroups}}
	changes, runner := applyUser(t, context.Background(), userRes("u8a", map[string]any{"shell": "/nonexistent/sh"}), true, ns, nil)
	if len(changes) != 1 || changes[0].Message != "created (noop)" || changes[0].Err != nil {
		t.Errorf("u8a = %+v", changes)
	}
	changes2, runner2 := applyUser(t, context.Background(), userRes("u8b", map[string]any{"gid": "nosuchgroup"}), true, ns, nil)
	if len(changes2) != 1 || changes2[0].Message != "gid changed 2101 to 'nosuchgroup' (noop)" || changes2[0].Err != nil {
		t.Errorf("u8b = %+v", changes2)
	}
	if len(runner.calls)+len(runner2.calls) != 0 {
		t.Errorf("noop ran commands")
	}
}

// When ensure changes nothing else is synced (resource_harness.rb:84-99; M2
// has one event).
func TestUserCreateSyncsNothingElse(t *testing.T) {
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
	changes, _ := applyUser(t, context.Background(), userRes("u2", map[string]any{"uid": 2002.0, "groups": []any{"wheel"}, "password": testHash}), false, ns, nil)
	if len(changes) != 1 || changes[0].Property != "ensure" {
		t.Errorf("changes = %s", scrub(changes))
	}
}

func TestUserProviderNotFunctional(t *testing.T) {
	dir := withUserBin(t)
	if err := os.Remove(filepath.Join(dir, "chpasswd")); err != nil {
		t.Fatal(err)
	}
	ns := userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}}}
	_, _, err := User{ns: ns, runner: &argvRunner{}}.Apply(context.Background(), userRes("u", map[string]any{"ensure": "present"}), false)
	if raltest.ErrText(err) != "Provider useradd is not functional on this host" {
		t.Errorf("err = %v", err)
	}
}

// password is a sensitive property in Ruby whether or not the catalog wraps
// it in Sensitive() (type/user.rb:280), so MarkSensitive marks it.
func TestUserPasswordIsAlwaysMarked(t *testing.T) {
	marked, logs := ral.MarkSensitive(User{}, userRes("u", map[string]any{"password": "x"}))
	if _, ok := marked["password"]; !ok || len(logs) != 0 {
		t.Errorf("marked = %v, logs = %v", marked, logs)
	}
	r := userRes("u", map[string]any{"password": "x"})
	r.SensitiveParameters = []string{"password"}
	if marked, logs := ral.MarkSensitive(User{}, r); len(marked) != 1 || len(logs) != 0 {
		t.Errorf("Sensitive-wrapped: marked = %v, logs = %v", marked, logs)
	}
	if marked, _ := ral.MarkSensitive(User{}, userRes("u", map[string]any{"uid": 1.0})); len(marked) != 0 {
		t.Errorf("no password: marked = %v", marked)
	}
}

// groupErrNSS fails every group lookup.
type groupErrNSS struct{ userNSS }

func (groupErrNSS) Group(string) (nameservice.GroupEntry, bool, error) {
	return nameservice.GroupEntry{}, false, errors.New("getgrnam_r g4: EIO")
}

// A gid lookup that fails is the in-sync test failing, which fails the event
// under noop too.
func TestUserGIDLookupErrorFailsUnderNoop(t *testing.T) {
	ns := groupErrNSS{userNSS{FakeNSS: raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{"u": {Name: "u", UID: 2000, GID: 2000}}}}}
	for _, noop := range []bool{true, false} {
		changes, runner := applyUser(t, context.Background(), userRes("u", map[string]any{"gid": "g4"}), noop, ns, nil)
		if len(changes) != 1 || raltest.ErrText(changes[0].Err) != "getgrnam_r g4: EIO" || len(runner.calls) != 0 {
			t.Errorf("noop %v: changes %+v calls %v", noop, changes, runner.argvs())
		}
	}
}
