package ral

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stagedExec runs command through execute with the exec's other parameters.
func stagedExec(t *testing.T, params map[string]any, command any) (int, string, error) {
	t.Helper()
	all := map[string]any{"command": command}
	for k, v := range params {
		all[k] = v
	}
	return Exec{}.execute(context.Background(), execRes("s", all), command, false, false)
}

func needRoot(t *testing.T, reason string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: " + reason)
	}
}

func lookupNobody(t *testing.T) *user.User {
	t.Helper()
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no nobody user: %v", err)
	}
	return u
}

func lookupDaemonGroup(t *testing.T) *user.Group {
	t.Helper()
	g, err := user.LookupGroup("daemon")
	if err != nil {
		t.Skipf("no daemon group: %v", err)
	}
	return g
}

// idSet is `id -G` output, or any list of ids, as a sorted set.
func idSet(ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, strings.Fields(id)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func mustOutput(t *testing.T, params map[string]any, command any) string {
	t.Helper()
	code, out, err := stagedExec(t, params, command)
	if err != nil || code != 0 {
		t.Fatalf("%v: status %d, output %q, error %v", command, code, out, err)
	}
	return strings.TrimSpace(out)
}

// agentState is what staging must leave as it found it.
func agentState(t *testing.T) (string, int) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	mask := syscall.Umask(0o022)
	syscall.Umask(mask)
	return wd, mask
}

// M6: initgroups' list, as suidmanager.rb:161-164 installs it.
func TestExecUserRunsAsUser(t *testing.T) {
	needRoot(t, "setuid to another user")
	nobody := lookupNobody(t)
	if got := mustOutput(t, map[string]any{"user": "nobody"}, "/usr/bin/id -u"); got != nobody.Uid {
		t.Errorf("id -u = %q, want %s", got, nobody.Uid)
	}
	groups, err := nobody.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	got := idSet(mustOutput(t, map[string]any{"user": "nobody"}, "/usr/bin/id -G"))
	if want := idSet(append(groups, nobody.Gid)...); !slices.Equal(got, want) {
		t.Errorf("id -G = %v, want getgrouplist %v", got, want)
	}
}

// M15: group alone keeps uid 0 and the agent's supplementary groups.
func TestExecGroupAlone(t *testing.T) {
	needRoot(t, "setgid to another group")
	daemon := lookupDaemonGroup(t)
	params := map[string]any{"group": "daemon"}
	if got := mustOutput(t, params, "/usr/bin/id -u"); got != "0" {
		t.Errorf("id -u = %q, want 0", got)
	}
	if got := mustOutput(t, params, "/usr/bin/id -g"); got != daemon.Gid {
		t.Errorf("id -g = %q, want %s", got, daemon.Gid)
	}
	agent, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{daemon.Gid}
	for _, g := range agent {
		want = append(want, strconv.Itoa(g))
	}
	if got := idSet(mustOutput(t, params, "/usr/bin/id -G")); !slices.Equal(got, idSet(want...)) {
		t.Errorf("id -G = %v, want the agent's groups plus daemon %v", got, idSet(want...))
	}
}

// M6: user-and-group is uid nobody, gid daemon, nobody's grouplist.
func TestExecUserAndGroup(t *testing.T) {
	needRoot(t, "setuid to another user")
	nobody, daemon := lookupNobody(t), lookupDaemonGroup(t)
	params := map[string]any{"user": "nobody", "group": "daemon"}
	if got := mustOutput(t, params, "/usr/bin/id -u"); got != nobody.Uid {
		t.Errorf("id -u = %q, want %s", got, nobody.Uid)
	}
	if got := mustOutput(t, params, "/usr/bin/id -g"); got != daemon.Gid {
		t.Errorf("id -g = %q, want %s", got, daemon.Gid)
	}
	groups, err := nobody.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	got := idSet(mustOutput(t, params, "/usr/bin/id -G"))
	if want := idSet(append(groups, daemon.Gid)...); !slices.Equal(got, want) {
		t.Errorf("id -G = %v, want %v", got, want)
	}
}

// M6 numeric: a string of digits, or an Integer, is an id.
func TestExecNumericUser(t *testing.T) {
	needRoot(t, "setuid to another user")
	nobody, daemon := lookupNobody(t), lookupDaemonGroup(t)
	uid, _ := strconv.Atoi(nobody.Uid)
	gid, _ := strconv.Atoi(daemon.Gid)
	for _, params := range []map[string]any{
		{"user": nobody.Uid, "group": daemon.Gid},
		{"user": float64(uid), "group": float64(gid)},
	} {
		if got := mustOutput(t, params, "/usr/bin/id -u"); got != nobody.Uid {
			t.Errorf("%v: id -u = %q, want %s", params, got, nobody.Uid)
		}
		if got := mustOutput(t, params, "/usr/bin/id -g"); got != daemon.Gid {
			t.Errorf("%v: id -g = %q, want %s", params, got, daemon.Gid)
		}
	}
}

// Ruby's change_user returns at once for the agent's own uid
// (suidmanager.rb:121), so a non-root agent naming itself, which validation
// allows, runs with no setgroups, which it could not do.
func TestExecUserSelfRunsWithoutSetgroups(t *testing.T) {
	self, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []any{"/usr/bin/id -u", []any{"/usr/bin/id", "-u"}} {
		if got := mustOutput(t, map[string]any{"user": self.Username}, command); got != self.Uid {
			t.Errorf("%v: id -u = %q, want %s", command, got, self.Uid)
		}
	}
}

// M6, M16, M18: an unknown identity starts no process; status 1 and the
// child's log_exception line. The group is checked first.
func TestExecUnknownIdentity(t *testing.T) {
	setAgentRoot(t, true)
	for _, tt := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"UnknownUser", map[string]any{"user": "nosuch"}, "Invalid user: nosuch"},
		{"UnknownNumericUser", map[string]any{"user": "12345"}, "Invalid user: 12345"},
		{"UnknownIntegerUser", map[string]any{"user": float64(12345)}, "Invalid user: 12345"},
		{"UnknownHugeUser", map[string]any{"user": "99999999999999999999"}, "Invalid user: 99999999999999999999"},
		{"UnknownGroup", map[string]any{"group": "nosuchgroup"}, "Invalid group: nosuchgroup"},
		{"UnknownNumericGroup", map[string]any{"group": "4242"}, "Invalid group: 4242"},
		{"GroupCheckedFirst", map[string]any{"user": "12345", "group": "4242"}, "Invalid group: 4242"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			for _, command := range []any{"/usr/bin/touch " + marker, []any{"/usr/bin/touch", marker}} {
				code, out, err := stagedExec(t, tt.params, command)
				want := "Error: Could not execute posix command: " + tt.want + "\n"
				if err != nil || code != 1 || out != want {
					t.Errorf("%v: (%d, %q, %v), want (1, %q, nil)", command, code, out, err, want)
				}
				if _, err := os.Stat(marker); err == nil {
					t.Fatalf("%v: a process started", command)
				}
			}
		})
	}
}

// D-78: a Sensitive user is not printed.
func TestExecUnknownSensitiveUserIsRedacted(t *testing.T) {
	setAgentRoot(t, true)
	r := execRes("s", map[string]any{"command": "/bin/true", "user": "secretname"})
	r.SensitiveParameters = []string{"user"}
	code, out, err := Exec{}.execute(context.Background(), r, "/bin/true", false, false)
	if want := "Error: Could not execute posix command: Invalid user: [redacted]\n"; err != nil || code != 1 || out != want {
		t.Errorf("(%d, %q, %v), want (1, %q, nil)", code, out, err, want)
	}
}

func TestExecIdentityOfAnotherTypeIsAnError(t *testing.T) {
	setAgentRoot(t, true)
	if _, _, err := stagedExec(t, map[string]any{"group": true}, "/bin/true"); err == nil {
		t.Error("group => true ran; want an error naming the type")
	}
}

// M6 private-cwd, M17: cwd is entered as root, then privileges are dropped.
func TestExecCwdEnteredBeforeDrop(t *testing.T) {
	needRoot(t, "a root-owned 0700 directory nobody cannot enter")
	lookupNobody(t)
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"cwd": private, "user": "nobody"}
	for _, command := range []any{"/bin/pwd", []any{"/bin/pwd"}} {
		if got := mustOutput(t, params, command); got != private {
			t.Errorf("%v: pwd = %q, want %s", command, got, private)
		}
	}
}

// M4: the command and its guards see the umask.
func TestExecUmaskSeen(t *testing.T) {
	params := map[string]any{"umask": "027"}
	if got := mustOutput(t, params, "umask"); got != "0027" {
		t.Errorf("command umask = %q, want 0027", got)
	}
	r := execRes("s", map[string]any{"command": "umask", "umask": "027"})
	code, out, err := Exec{}.execute(context.Background(), r, "umask", false, true)
	if err != nil || code != 0 || strings.TrimSpace(out) != "0027" {
		t.Errorf("guard: (%d, %q, %v), want umask 0027", code, out, err)
	}
	dir := t.TempDir()
	mustOutput(t, map[string]any{"umask": "0077"}, "/usr/bin/touch "+filepath.Join(dir, "f"))
	if fi, err := os.Stat(filepath.Join(dir, "f")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file made under umask 0077: %v %v, want 0600", fi, err)
	}
}

func TestExecStagingRestoresAgentState(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name    string
		params  map[string]any
		command any
	}{
		{"cwd", map[string]any{"cwd": dir}, "/bin/pwd"},
		{"umask", map[string]any{"umask": "0077"}, "umask"},
		{"cwd and umask", map[string]any{"cwd": dir, "umask": "0"}, []any{"/bin/pwd"}},
		{"missing cwd", map[string]any{"cwd": filepath.Join(dir, "gone"), "umask": "0077"}, []any{"/bin/true"}},
		{"start fails", map[string]any{"cwd": dir, "umask": "0077"}, []any{"./missing"}},
		{"timeout", map[string]any{"cwd": dir, "umask": "0077", "timeout": 0.2}, "sleep 5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wd, mask := agentState(t)
			stagedExec(t, tt.params, tt.command)
			if gotWd, gotMask := agentState(t); gotWd != wd || gotMask != mask {
				t.Errorf("agent cwd %s umask %04o after, want %s %04o", gotWd, gotMask, wd, mask)
			}
		})
	}
}

// Today's unenterable-cwd outcome, now reached in the agent's chdir: an argv
// is status 1 with no output, a string an error, and nothing started.
func TestExecStagedMissingCwd(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	marker := filepath.Join(t.TempDir(), "ran")
	code, out, err := stagedExec(t, map[string]any{"cwd": gone}, []any{"/usr/bin/touch", marker})
	if err != nil || code != 1 || out != "" {
		t.Errorf("argv: (%d, %q, %v), want (1, \"\", nil)", code, out, err)
	}
	// The text names the program, not cwd, which may be Sensitive.
	if _, _, err := stagedExec(t, map[string]any{"cwd": gone}, "/usr/bin/touch "+marker); err == nil ||
		!strings.Contains(err.Error(), "fork/exec /bin/sh: no such file or directory") || strings.Contains(err.Error(), gone) {
		t.Errorf("string: error %v, want today's fork/exec error without the cwd", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a command ran")
	}
}

// §4.2 item 3: chdir(2) needs search permission only, as Ruby's Dir.chdir.
// Item 2: the agent's own cwd is saved with O_PATH, which needs no read
// permission either.
func TestExecSearchOnlyCwdNonRoot(t *testing.T) {
	searchOnly := filepath.Join(t.TempDir(), "x")
	if err := os.Mkdir(searchOnly, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(searchOnly, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(searchOnly, 0o755) })
	if got := mustOutput(t, map[string]any{"cwd": searchOnly}, "/bin/pwd"); got != searchOnly {
		t.Errorf("pwd = %q, want %s", got, searchOnly)
	}

	stageMu.Lock()
	saved := stageOrigin
	stageOrigin = -1
	stageMu.Unlock()
	t.Cleanup(func() {
		stageMu.Lock()
		if stageOrigin >= 0 {
			syscall.Close(stageOrigin)
		}
		stageOrigin = saved
		stageMu.Unlock()
	})
	t.Chdir(searchOnly)
	other := t.TempDir()
	if got := mustOutput(t, map[string]any{"cwd": other}, "/bin/pwd"); got != other {
		t.Errorf("pwd = %q, want %s", got, other)
	}
	if wd, _ := agentState(t); wd != searchOnly {
		t.Errorf("agent cwd %s after, want the search-only %s", wd, searchOnly)
	}
}

// Review Focus 1: cmd.Dir is "", so a relative argv[0] resolves in the
// staged cwd.
func TestExecStagedRelativeArgvRunsFromCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustOutput(t, map[string]any{"cwd": dir}, []any{"./run.sh"}); got != "ran" {
		t.Errorf("output %q, want ran", got)
	}
}

// Review Focus 2: the ENOEXEC retry through /bin/sh is staged as the first
// start was.
func TestExecStagedENOEXECRetryKeepsIdentityAndUmask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script"), []byte("umask\nid -u\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"cwd": dir, "umask": "027"}
	want := "0027\n" + strconv.Itoa(os.Getuid())
	if os.Geteuid() == 0 {
		nobody := lookupNobody(t)
		params["user"] = "nobody"
		want = "0027\n" + nobody.Uid
	}
	if got := mustOutput(t, params, []any{"./script"}); got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

// §4.2 item 7: no cwd, umask, user or group takes no lock.
func TestExecNoStagingWithoutNeed(t *testing.T) {
	if (staging{}).needed() {
		t.Error("an empty staging is needed")
	}
	umask := 0o22
	for _, s := range []staging{{cwd: "/"}, {umask: &umask}, {id: identity{cred: &syscall.Credential{}}}} {
		if !s.needed() {
			t.Errorf("%+v is not needed", s)
		}
	}
	stageMu.Lock()
	defer stageMu.Unlock()
	done := make(chan string, 1)
	go func() {
		_, out, _ := stagedExec(t, nil, "/bin/pwd")
		done <- strings.TrimSpace(out)
	}()
	select {
	case got := <-done:
		if wd, _ := os.Getwd(); got != wd {
			t.Errorf("pwd = %q, want the agent's %s", got, wd)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an unstaged command waited for the staging lock")
	}
}

// §4.2 item 5.
func TestExecRestoreFailureIsStagingBroken(t *testing.T) {
	old := restoreCwd
	restoreCwd = func(fd int) error {
		if err := old(fd); err != nil {
			t.Fatal(err)
		}
		return syscall.EACCES
	}
	t.Cleanup(func() { restoreCwd = old })
	for _, command := range []any{"/bin/true", []any{"/bin/true"}} {
		_, _, err := stagedExec(t, map[string]any{"cwd": t.TempDir()}, command)
		if !errors.Is(err, ErrStagingBroken) || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%v: error %v, want ErrStagingBroken wrapping EACCES", command, err)
		}
	}
	r := execRes("s", map[string]any{"command": "/bin/true", "cwd": t.TempDir()})
	if _, _, err := (Exec{}).Apply(context.Background(), r, false); !errors.Is(err, ErrStagingBroken) {
		t.Errorf("Apply: error %v, want ErrStagingBroken", err)
	}
}

// D-78: every invalid-identity path of a marked user or group, including the
// ids identityKey refuses before any lookup, shows [redacted].
func TestExecUnknownSensitiveIdentityIsRedacted(t *testing.T) {
	marked := func(string) bool { return true }
	for _, tt := range []struct{ usr, group any }{
		{"secretname", nil},
		{"99999999999999999999", nil},
		{float64(-1), nil},
		{nil, "secretgroup"},
		{"nobody", "99999999999999999999"},
	} {
		_, err := resolveIdentity(tt.usr, tt.group, marked)
		var invalid invalidIdentityError
		if !errors.As(err, &invalid) || invalid.name != "[redacted]" {
			t.Errorf("user %v group %v: %v, want an invalid identity named [redacted]", tt.usr, tt.group, err)
		}
	}
}
