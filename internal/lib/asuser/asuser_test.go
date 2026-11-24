package asuser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// TestMain makes the test binary its own fake helper: ProcStarter starts
// /proc/self/exe, which here is this binary, with "asuser" and the op.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "asuser" {
		os.Exit(fakeHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// fakeHelper plays a helper whose output an attacker controls.
//   - emit <exit> <path>: copies stdin to stdout and exits <exit>.
//   - noisy <path>: floods stderr with a marker, then prints "ok 0".
//   - flood <path>: writes 1 MiB of "ok 0\n" lines to stdout.
//   - grandchild <path>: leaves a sleep holding stdout, prints "ok 0", exits 0.
//   - kill <path>: prints "ok 0" and dies of SIGKILL.
//   - touch <path>: creates <path>, prints "ok 0".
//   - hang <path>: sleeps for a minute.
func fakeHelper(args []string) int {
	switch args[0] {
	case "touch":
		if err := os.WriteFile(args[1], nil, 0o600); err != nil {
			return 3
		}
		_, _ = os.Stdout.WriteString("ok 0\n")
		return 0
	case "hang":
		time.Sleep(time.Minute)
		return 0
	case "emit":
		code, _ := strconv.Atoi(args[1])
		_, _ = io.Copy(os.Stdout, os.Stdin)
		return code
	case "noisy":
		_, _ = os.Stderr.WriteString(strings.Repeat("STDERR-MARKER open 13\n", 1<<16))
		_, _ = os.Stdout.WriteString("ok 0\n")
		return 0
	case "flood":
		_, _ = os.Stdout.WriteString(strings.Repeat("ok 0\n", 1<<18))
		return 0
	case "grandchild":
		c := exec.Command("sleep", "5")
		c.Stdout = os.Stdout
		if err := c.Start(); err != nil {
			return 3
		}
		_, _ = os.Stdout.WriteString("ok 0\n")
		return 0
	case "kill":
		_, _ = os.Stdout.WriteString("ok 0\n")
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		return 0
	}
	return 99
}

// recordingCtx records every log entry, to show the ops log nothing.
func recordingCtx() (context.Context, *[]report.LogEntry) {
	var logs []report.LogEntry
	rec := func(e report.LogEntry) { logs = append(logs, e) }
	return runlog.WithLogger(context.Background(), runlog.New("debug", rec, rec)), &logs
}

func noRelabel(context.Context, string) (string, error) { return "", nil }

// selfCred is the test's own credential, for starting the fake helper.
// ProcStarter refuses uid 0, so the fake-helper tests run unprivileged only.
func selfCred(t *testing.T) *syscall.Credential {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("ProcStarter refuses uid 0; the fake-helper tests run unprivileged")
	}
	return &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}
}

func TestMkdirExisting(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	// Puppet::FileSystem.exist? follows links and takes any existing entry.
	for _, p := range []string{dir, file, link} {
		if r := Mkdir(p); r != (Result{StepOK, 0}) {
			t.Errorf("Mkdir(%s) = %+v; want ok 0", p, r)
		}
	}
}

// mkdir is subject to the umask (M4: umask 0127 gives 0600).
func TestMkdirCreated(t *testing.T) {
	for _, tc := range []struct{ umask, want int }{{0o022, 0o700}, {0o127, 0o600}} {
		p := filepath.Join(t.TempDir(), ".ssh")
		old := syscall.Umask(tc.umask)
		r := Mkdir(p)
		syscall.Umask(old)
		if r != (Result{StepMkdirCreated, 0}) {
			t.Fatalf("Mkdir = %+v; want mkdir-created 0", r)
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() || int(st.Mode().Perm()) != tc.want {
			t.Errorf("umask %o: %v, %v; want a %o directory", tc.umask, st, err, tc.want)
		}
	}
}

func TestMkdirFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent", ".ssh")
	if r := Mkdir(p); r != (Result{StepMkdir, syscall.ENOENT}) {
		t.Errorf("Mkdir = %+v; want mkdir ENOENT", r)
	}
	// A link to nothing does not exist; the mkdir then meets the link.
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nothing"), link); err != nil {
		t.Fatal(err)
	}
	if r := Mkdir(link); r != (Result{StepMkdir, syscall.EEXIST}) {
		t.Errorf("Mkdir(dangling) = %+v; want mkdir EEXIST", r)
	}
}

func TestWrite(t *testing.T) {
	ctx, logs := recordingCtx()
	p := filepath.Join(t.TempDir(), "authorized_keys")
	var relabelled []string
	relabel := func(_ context.Context, path string) (string, error) {
		relabelled = append(relabelled, path)
		return "", nil
	}
	r, err := Write(ctx, p, "UTF-8", "ssh-rsa AAAA k\n", relabel)
	if err != nil || r != (Result{StepOK, 0}) {
		t.Fatalf("Write = %+v, %v; want ok 0", r, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "ssh-rsa AAAA k\n" {
		t.Errorf("file = %q", b)
	}
	if !reflect.DeepEqual(relabelled, []string{p}) {
		t.Errorf("relabelled %v; want [%s]", relabelled, p)
	}
	if len(*logs) != 0 {
		t.Errorf("Write logged %v", *logs)
	}
}

// A refused open is open EACCES, and the text the parent renders from it is
// the in-process write's.
func TestWriteRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a read-only file")
	}
	ctx, logs := recordingCtx()
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, []byte("keep"), 0o400); err != nil {
		t.Fatal(err)
	}
	r, err := Write(ctx, p, "UTF-8", "x\n", noRelabel)
	if err != nil || r != (Result{StepOpen, syscall.EACCES}) {
		t.Fatalf("Write = %+v, %v; want open EACCES", r, err)
	}
	if len(*logs) != 0 {
		t.Errorf("Write logged %v", *logs)
	}
	nop := func(context.Context, string) error { return nil }
	flat, err := parsedfile.NewFlat(p, 0, nop, nop, "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	inProcess := flat.Write(context.Background(), "x\n")
	var wf *parsedfile.WriteFailure
	if !errors.As(inProcess, &wf) {
		t.Fatalf("in-process error %v carries no WriteFailure", inProcess)
	}
	rendered := parsedfile.FlatWriteError(p, parsedfile.WriteFailure{Step: string(r.Step), Errno: r.Errno})
	if rendered.Error() != inProcess.Error() {
		t.Errorf("rendered %q; in-process %q", rendered, inProcess)
	}
}

// A relabel failure is reported after the file is written; an errno-less
// failure is reported as EIO.
func TestWriteRelabelFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want syscall.Errno
	}{
		{&os.PathError{Op: "setxattr", Path: "p", Err: syscall.EACCES}, syscall.EACCES},
		{errors.New("no errno"), syscall.EIO},
	} {
		p := filepath.Join(t.TempDir(), "f")
		relabel := func(context.Context, string) (string, error) { return "system_u:object_r:ssh_home_t:s0", tc.err }
		r, err := Write(context.Background(), p, "UTF-8", "k\n", relabel)
		if err != nil || r != (Result{StepRelabel, tc.want}) {
			t.Errorf("%v: Write = %+v, %v; want relabel %d", tc.err, r, err, tc.want)
		}
		if b, _ := os.ReadFile(p); string(b) != "k\n" {
			t.Errorf("%v: file = %q; want it written", tc.err, b)
		}
	}
}

// The encoding failure has no step; Write returns parsedfile's error and
// leaves the target alone.
func TestWriteEncodingFailure(t *testing.T) {
	ctx, logs := recordingCtx()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Write(ctx, p, "US-ASCII", "café\n", noRelabel)
	want := parsedfile.FlatEncodeError(p, "US-ASCII", "café\n")
	if err == nil || want == nil || err.Error() != want.Error() {
		t.Errorf("err = %v; want %v", err, want)
	}
	if b, _ := os.ReadFile(p); string(b) != "keep" {
		t.Errorf("target = %q", b)
	}
	if len(*logs) != 0 {
		t.Errorf("Write logged %v", *logs)
	}
}

func TestChmod(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if r := Chmod(p); r != (Result{StepOK, 0}) {
		t.Fatalf("Chmod = %+v", r)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := Chmod(link); r != (Result{StepOK, 0}) {
		t.Fatalf("Chmod(link) = %+v", r)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode through the link = %o; want 600", st.Mode().Perm())
	}
	if r := Chmod(filepath.Join(dir, "absent")); r != (Result{StepChmod, syscall.ENOENT}) {
		t.Errorf("Chmod(absent) = %+v; want chmod ENOENT", r)
	}
}

func TestProcCmd(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TMPDIR", "/var/tmp")
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy")
	cred := &syscall.Credential{Uid: 1234, Gid: 5678}
	cmd := procCmd(context.Background(), cred, []string{"chmod", "/h/.ssh/authorized_keys"}, []byte("text"))
	if cmd.Path != "/proc/self/exe" {
		t.Errorf("Path = %q", cmd.Path)
	}
	if want := []string{"/proc/self/exe", "asuser", "chmod", "/h/.ssh/authorized_keys"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("Args = %q; want %q", cmd.Args, want)
	}
	if want := []string{"PATH=/usr/bin:/bin", "TMPDIR=/var/tmp"}; !reflect.DeepEqual(cmd.Env, want) {
		t.Errorf("Env = %q; want %q", cmd.Env, want)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential != cred {
		t.Errorf("SysProcAttr = %+v; want Credential %p", cmd.SysProcAttr, cred)
	}
	if cmd.Stderr != nil {
		t.Errorf("Stderr = %v; want nil (the null device)", cmd.Stderr)
	}
	if cmd.Dir != "" {
		t.Errorf("Dir = %q; want the agent's cwd", cmd.Dir)
	}
	if cmd.WaitDelay <= 0 {
		t.Errorf("WaitDelay = %v; want a bound", cmd.WaitDelay)
	}
	if in, err := io.ReadAll(cmd.Stdin); err != nil || string(in) != "text" {
		t.Errorf("stdin = %q, %v", in, err)
	}

	if err := os.Unsetenv("TMPDIR"); err != nil {
		t.Fatal(err)
	}
	cmd = procCmd(context.Background(), cred, []string{"mkdir", "/h/.ssh"}, nil)
	if want := []string{"PATH=/usr/bin:/bin"}; !reflect.DeepEqual(cmd.Env, want) {
		t.Errorf("Env without TMPDIR = %q; want %q", cmd.Env, want)
	}
}

func TestProcStarterValidResult(t *testing.T) {
	r, err := ProcStarter{}.Start(context.Background(), selfCred(t), []string{"emit", "1", "/p"}, []byte("open 13\n"))
	if err != nil || r != (Result{StepOpen, syscall.EACCES}) {
		t.Errorf("Start = %+v, %v; want open 13", r, err)
	}
	r, err = ProcStarter{}.Start(context.Background(), selfCred(t), []string{"noisy", "/p"}, nil)
	if err != nil || r != (Result{StepOK, 0}) {
		t.Errorf("noisy stderr: Start = %+v, %v; want ok 0", r, err)
	}
}

// Whatever the helper prints or however it ends, a bad result is one fixed
// error that quotes nothing from it.
func TestProcStarterInvalidResult(t *testing.T) {
	const want = "asuser helper for /p returned an invalid result"
	tests := []struct {
		name  string
		args  []string
		stdin string
	}{
		{"garbage", []string{"emit", "0", "/p"}, "Error: pwned\n"},
		{"injection", []string{"emit", "1", "/p"}, "open 13\nNotice: forged\n"},
		{"mismatched exit", []string{"emit", "0", "/p"}, "open 13\n"},
		{"exit 2", []string{"emit", "2", "/p"}, ""},
		{"flood", []string{"flood", "/p"}, ""},
		{"signal", []string{"kill", "/p"}, ""},
	}
	for _, tc := range tests {
		_, err := ProcStarter{}.Start(context.Background(), selfCred(t), tc.args, []byte(tc.stdin))
		if err == nil || err.Error() != want || !errors.Is(err, ErrInvalidResult) {
			t.Errorf("%s: err = %v; want %q", tc.name, err, want)
		}
	}
}

// A grandchild holding stdout open cannot hang the parent.
func TestProcStarterGrandchildHoldingStdout(t *testing.T) {
	start := time.Now()
	_, err := ProcStarter{}.Start(context.Background(), selfCred(t), []string{"grandchild", "/p"}, nil)
	if !errors.Is(err, ErrInvalidResult) {
		t.Errorf("err = %v; want ErrInvalidResult", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("Start waited %v for the grandchild", d)
	}
}

func TestProcStarterStartFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may change its credential")
	}
	cred := &syscall.Credential{Uid: uint32(os.Getuid()) + 1, Gid: uint32(os.Getgid()) + 1, NoSetGroups: true}
	_, err := ProcStarter{}.Start(context.Background(), cred, []string{"chmod", "/p"}, nil)
	const prefix = "Could not start the asuser helper via /proc/self/exe (is /proc mounted?): "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) || errors.Is(err, ErrInvalidResult) {
		t.Errorf("err = %v; want prefix %q", err, prefix)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("err = %v; want it to wrap EPERM", err)
	}
}

func TestCappedBuffer(t *testing.T) {
	var b cappedBuffer
	big := bytes.Repeat([]byte("x"), 100)
	if n, err := b.Write(big); n != 100 || err != nil {
		t.Errorf("Write = %d, %v; want every byte taken", n, err)
	}
	if len(b.buf) != maxResult+1 {
		t.Errorf("kept %d bytes; want %d", len(b.buf), maxResult+1)
	}
}

// Defence in depth (spec §3.4): the helper never runs as uid 0, so Start
// refuses a missing or root credential itself, without starting anything.
func TestProcStarterRefusesRoot(t *testing.T) {
	for _, cred := range []*syscall.Credential{nil, {Uid: 0, Gid: 0}, {Uid: 0, Gid: 1000, NoSetGroups: true}} {
		p := filepath.Join(t.TempDir(), "started")
		_, err := ProcStarter{}.Start(context.Background(), cred, []string{"touch", p}, nil)
		if err == nil || err.Error() != "refusing to start the asuser helper as root" || errors.Is(err, ErrInvalidResult) {
			t.Errorf("cred %+v: err = %v; want the refusal", cred, err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Errorf("cred %+v: the helper was started", cred)
		}
	}
}

// A cancelled run is the context's error, not a helper fault.
func TestProcStarterCancelled(t *testing.T) {
	cred := selfCred(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ProcStarter{}.Start(ctx, cred, []string{"hang", "/p"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrInvalidResult) {
		t.Errorf("err = %v; want the context's deadline error", err)
	}

	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	_, err = ProcStarter{}.Start(done, cred, []string{"touch", filepath.Join(t.TempDir(), "x")}, nil)
	if !errors.Is(err, context.Canceled) || strings.HasPrefix(err.Error(), "Could not start") {
		t.Errorf("already cancelled: err = %v; want context.Canceled", err)
	}
}

// No op is an error, not a panic, and starts nothing.
func TestProcStarterNoArgs(t *testing.T) {
	cred := &syscall.Credential{Uid: 65534, Gid: 65534, NoSetGroups: true} // never used: nothing starts
	for _, args := range [][]string{nil, {}} {
		_, err := ProcStarter{}.Start(context.Background(), cred, args, nil)
		if err == nil || err.Error() != "the asuser helper needs an op" {
			t.Errorf("args %q: err = %v", args, err)
		}
	}
}
