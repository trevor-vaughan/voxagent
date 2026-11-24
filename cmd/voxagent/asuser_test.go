package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/lib/suid"
)

// TestMain lets asuser.ProcStarter start this test binary as the helper, as
// main's switch does for the real binary.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == "asuser" {
		os.Exit(asuserCommand(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func runAsuser(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = asuserCommand(args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestAsuserRefusesEuidZero(t *testing.T) {
	old := asuserEuid
	asuserEuid = func() int { return 0 }
	defer func() { asuserEuid = old }()
	p := filepath.Join(t.TempDir(), "d")
	stdout, stderr, code := runAsuser(t, "", "mkdir", p)
	if code != 2 || stdout != "" || stderr != "asuser: refusing to run as root\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("the refused helper created the directory")
	}
}

// A malformed invocation prints nothing and exits 2.
func TestAsuserRefusesBadArguments(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the helper refuses root")
	}
	p := filepath.Join(t.TempDir(), "f")
	for _, args := range [][]string{
		nil,
		{"rm", p},
		{"mkdir"},
		{"mkdir", p, p},
		{"chmod"},
		{"write", p},
		{"write", "--encoding", "UTF-8"},
		{"write", "--encoding", "UTF-8", p, p},
		{"write", "--user", "UTF-8", p},
		{"mkdir", "--user", "alice", p},
	} {
		stdout, stderr, code := runAsuser(t, "x\n", args...)
		if code != 2 || stdout != "" || stderr != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("a refused invocation touched the path")
	}
}

// Each op prints exactly one result line, with the exit status that agrees.
func TestAsuserOps(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the helper refuses root")
	}
	dir := filepath.Join(t.TempDir(), ".ssh")
	keys := filepath.Join(dir, "authorized_keys")
	tests := []struct {
		stdin string
		args  []string
		want  string
		code  int
	}{
		{"", []string{"mkdir", dir}, "mkdir-created 0\n", 0},
		{"", []string{"mkdir", dir}, "ok 0\n", 0},
		{"", []string{"mkdir", filepath.Join(dir, "no", "d")}, "mkdir 2\n", 1},
		{"ssh-rsa AAAA k\n", []string{"write", "--encoding", "UTF-8", keys}, "ok 0\n", 0},
		{"x\n", []string{"write", "--encoding", "UTF-8", filepath.Join(dir, "no", "f")}, "open 2\n", 1},
		{"", []string{"chmod", keys}, "ok 0\n", 0},
		{"", []string{"chmod", filepath.Join(dir, "absent")}, "chmod 2\n", 1},
	}
	for _, tc := range tests {
		stdout, stderr, code := runAsuser(t, tc.stdin, tc.args...)
		if stdout != tc.want || code != tc.code || stderr != "" {
			t.Errorf("%q: stdout %q exit %d stderr %q; want %q exit %d", tc.args, stdout, code, stderr, tc.want, tc.code)
		}
		if _, ok := asuser.ParseResult([]byte(stdout), code); !ok {
			t.Errorf("%q: ParseResult refuses its own output %q", tc.args, stdout)
		}
	}
	if b, _ := os.ReadFile(keys); string(b) != "ssh-rsa AAAA k\n" {
		t.Errorf("keys = %q", b)
	}
	if st, _ := os.Stat(keys); st.Mode().Perm() != 0o600 {
		t.Errorf("keys mode %o", st.Mode().Perm())
	}
}

// Text the encoding cannot take has no result: nothing printed, exit 2, the
// target untouched. The parent checks the encoding before it starts the write.
func TestAsuserWriteEncodingFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the helper refuses root")
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runAsuser(t, "café\n", "write", "--encoding", "US-ASCII", p)
	if code != 2 || stdout != "" || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if b, _ := os.ReadFile(p); string(b) != "keep" {
		t.Errorf("target = %q", b)
	}
}

// rootHelperUser is the unprivileged user the root tests drop to: nobody,
// present on every RHEL-family host and in the test container.
const rootHelperUser = "nobody"

// rootFixture is, under a 0755 root-owned temp dir, a directory the helper's
// user owns ("user", 0700) and one only root may write ("root", 0755) holding
// a root file "root/target" (0644).
func rootFixture(t *testing.T) (cred *syscall.Credential, userDir, rootDir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: build with go test -c and run the binary under sudo")
	}
	u, err := user.Lookup(rootHelperUser)
	if err != nil {
		t.Skipf("no %s user: %v", rootHelperUser, err)
	}
	cred, err = suid.Resolve(rootHelperUser, nil, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp("", "asuser-root-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	userDir, rootDir = filepath.Join(base, "user"), filepath.Join(base, "root")
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, d := range []string{userDir, rootDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(userDir, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "target"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cred, userDir, rootDir
}

func startHelper(t *testing.T, cred *syscall.Credential, stdin string, args ...string) asuser.Result {
	t.Helper()
	r, err := asuser.ProcStarter{}.Start(context.Background(), cred, args, []byte(stdin))
	if err != nil {
		t.Fatalf("%q: %v", args, err)
	}
	return r
}

func TestAsuserRootWriteAsUser(t *testing.T) {
	cred, userDir, _ := rootFixture(t)
	ssh := filepath.Join(userDir, ".ssh")
	keys := filepath.Join(ssh, "authorized_keys")
	if r := startHelper(t, cred, "", "mkdir", ssh); r != (asuser.Result{Step: asuser.StepMkdirCreated}) {
		t.Fatalf("mkdir = %v", r)
	}
	if r := startHelper(t, cred, "ssh-rsa AAAA k\n", "write", "--encoding", "UTF-8", keys); r != (asuser.Result{Step: asuser.StepOK}) {
		t.Fatalf("write = %v", r)
	}
	if r := startHelper(t, cred, "", "chmod", keys); r != (asuser.Result{Step: asuser.StepOK}) {
		t.Fatalf("chmod = %v", r)
	}
	for _, p := range []string{ssh, keys} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		sys := st.Sys().(*syscall.Stat_t)
		if sys.Uid != cred.Uid || sys.Gid != cred.Gid {
			t.Errorf("%s owned %d:%d; want %d:%d", p, sys.Uid, sys.Gid, cred.Uid, cred.Gid)
		}
	}
	if b, _ := os.ReadFile(keys); string(b) != "ssh-rsa AAAA k\n" {
		t.Errorf("keys = %q", b)
	}
}

func TestAsuserRootRootOnlyDir(t *testing.T) {
	cred, _, rootDir := rootFixture(t)
	if r := startHelper(t, cred, "x\n", "write", "--encoding", "UTF-8", filepath.Join(rootDir, "new")); r.String() != "open 13" {
		t.Errorf("write = %v; want open 13", r)
	}
	if r := startHelper(t, cred, "", "mkdir", filepath.Join(rootDir, ".ssh")); r.String() != "mkdir 13" {
		t.Errorf("mkdir = %v; want mkdir 13", r)
	}
	if r := startHelper(t, cred, "", "chmod", filepath.Join(rootDir, "target")); r.String() != "chmod 1" {
		t.Errorf("chmod = %v; want chmod 1", r)
	}
	if _, err := os.Stat(filepath.Join(rootDir, "new")); err == nil {
		t.Error("the helper created a file in a root-only directory")
	}
}

// Review Focus 2: a link the user plants to a root file is opened as the
// user, so the write is refused where root's would succeed.
func TestAsuserRootSymlinkIntoForeignDir(t *testing.T) {
	cred, userDir, rootDir := rootFixture(t)
	target := filepath.Join(rootDir, "target")
	link := filepath.Join(userDir, "authorized_keys")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if r := startHelper(t, cred, "pwned\n", "write", "--encoding", "UTF-8", link); r.String() != "open 13" {
		t.Errorf("write = %v; want open 13", r)
	}
	if b, _ := os.ReadFile(target); string(b) != "root\n" {
		t.Errorf("root's file = %q", b)
	}
}

// Root never reaches the helper: ProcStarter refuses a missing or uid-0
// credential without starting it, and the helper itself, started as root
// another way, refuses too (exit 2, no output).
func TestAsuserRootHelperRefusesRoot(t *testing.T) {
	_, _, rootDir := rootFixture(t)
	p := filepath.Join(rootDir, ".ssh")
	for _, cred := range []*syscall.Credential{nil, {Uid: 0, Gid: 0}} {
		_, err := asuser.ProcStarter{}.Start(context.Background(), cred, []string{"mkdir", p}, nil)
		if err == nil || err.Error() != "refusing to start the asuser helper as root" {
			t.Errorf("cred %+v: err = %v; want the refusal", cred, err)
		}
	}
	cmd := exec.Command("/proc/self/exe", "asuser", "mkdir", p)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || stdout.Len() != 0 {
		t.Errorf("helper as root: err %v, stdout %q; want exit 2 and no output", err, stdout.String())
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("the helper ran as root")
	}
}

// The real helper through ProcStarter, as the test's own uid: the protocol
// end to end without root.
func TestAsuserProcStarterOwnUID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the helper refuses root")
	}
	cred := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}
	dir := filepath.Join(t.TempDir(), ".ssh")
	keys := filepath.Join(dir, "authorized_keys")
	if r := startHelper(t, cred, "", "mkdir", dir); r != (asuser.Result{Step: asuser.StepMkdirCreated}) {
		t.Errorf("mkdir = %v", r)
	}
	if r := startHelper(t, cred, "k\n", "write", "--encoding", "UTF-8", keys); r != (asuser.Result{Step: asuser.StepOK}) {
		t.Errorf("write = %v", r)
	}
	if b, _ := os.ReadFile(keys); string(b) != "k\n" {
		t.Errorf("keys = %q", b)
	}
	_, err := asuser.ProcStarter{}.Start(context.Background(), cred, []string{"write", "--encoding", "US-ASCII", keys}, []byte("café\n"))
	if !errors.Is(err, asuser.ErrInvalidResult) {
		t.Errorf("encoding failure: err = %v; want the invalid result", err)
	}
}
