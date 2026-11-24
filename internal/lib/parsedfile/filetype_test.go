package parsedfile

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Not ported from Puppet's filetype_spec.rb (spec §10.2), with reasons:
//   - 12-14 and the :ram, :suntab and :aixtab describes: other filetypes, unused.
//   - 24-30 flat "remove" and 147-160 crontab "remove": FileType has no Remove,
//     no caller needs it.
//   - 81-91 "should use the default filebucket": Backup takes the consumer's hook,
//     there is no default bucket in this package.
//   - 173-190 crontab write "on a temporary file": cft:89 pipes the text to
//     `crontab -`, which TestCrontabWriteArgv pins; Puppet's spec predates that.

// logCtx returns a context whose Logger records every entry.
func logCtx(t *testing.T) (context.Context, *[]report.LogEntry) {
	t.Helper()
	var got []report.LogEntry
	l := runlog.New("debug", func(e report.LogEntry) { got = append(got, e) }, func(report.LogEntry) {})
	return runlog.WithLogger(context.Background(), l), &got
}

func hasLog(entries []report.LogEntry, level, msg string) bool {
	for _, e := range entries {
		if e.Level == level && e.Source == "Puppet" && e.Message == msg {
			return true
		}
	}
	return false
}

func TestStripHeader(t *testing.T) {
	const cls, path = "C", "p"
	tests := []struct {
		name, enc, in, want string
	}{
		{"line removed", "UTF-8", "# HEADER x\nA\n", "A\n"},
		{"mid-line joins", "UTF-8", "A# HEADER x\nB", "AB"},
		{"final line without newline stays", "UTF-8", "A\n# HEADER x", "A\n# HEADER x"},
		{"several", "UTF-8", "# HEADER a\n# HEADER b\nX\n", "X\n"},
		{"no header", "US-ASCII", "A\nB\n", "A\nB\n"},
		{"empty", "US-ASCII", "", ""},
		{"utf-8 valid under utf-8", "UTF-8", "# café\n", "# café\n"},
	}
	for _, tc := range tests {
		got, err := stripHeader(cls, path, tc.enc, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: stripHeader = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	bad := []struct{ name, enc, in, want string }{
		{"valid utf-8 under US-ASCII", "US-ASCII", "# café\n", "C could not read p: invalid byte sequence in US-ASCII"},
		{"invalid byte under US-ASCII", "US-ASCII", "# \xff\n", "C could not read p: invalid byte sequence in US-ASCII"},
		{"invalid under UTF-8", "UTF-8", "# \xff\n", "C could not read p: invalid byte sequence in UTF-8"},
		{"truncated sequence under UTF-8", "UTF-8", "a\xc3", "C could not read p: invalid byte sequence in UTF-8"},
	}
	for _, tc := range bad {
		_, err := stripHeader(cls, path, tc.enc, tc.in)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v; want %q", tc.name, err, tc.want)
		}
	}
}

func noopHook(context.Context, string) error { return nil }

func TestNewFlatRequiresHooks(t *testing.T) {
	if _, err := NewFlat("p", 0, nil, noopHook, "UTF-8"); err == nil {
		t.Error("nil backup accepted")
	}
	if _, err := NewFlat("p", 0, noopHook, nil, "UTF-8"); err == nil {
		t.Error("nil relabel accepted")
	}
}

func newFlat(t *testing.T, path string, mode fs.FileMode, enc string) FileType {
	t.Helper()
	ft, err := NewFlat(path, mode, noopHook, noopHook, enc)
	if err != nil {
		t.Fatal(err)
	}
	return ft
}

func TestFlatRead(t *testing.T) {
	dir := t.TempDir()
	ctx, logs := logCtx(t)

	got, err := newFlat(t, filepath.Join(dir, "missing"), 0, "UTF-8").Read(ctx)
	if got != "" || err != nil {
		t.Errorf("missing file: %q, %v", got, err)
	}

	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("# HEADER x\na=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = newFlat(t, p, 0, "UTF-8").Read(ctx)
	if got != "a=1\n" || err != nil {
		t.Errorf("existing file: %q, %v", got, err)
	}

	if err := os.WriteFile(p, []byte("caf\xc3\xa9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = newFlat(t, p, 0, "US-ASCII").Read(ctx)
	want := "Puppet::Util::FileType::FileTypeFlat could not read " + p + ": invalid byte sequence in US-ASCII"
	if err == nil || err.Error() != want {
		t.Errorf("US-ASCII read err = %v; want %q", err, want)
	}
	if !hasLog(*logs, "err", want) {
		t.Errorf("log_exception line missing: %v", *logs)
	}
}

func TestFlatReadDirectory(t *testing.T) {
	d := t.TempDir()
	_, err := newFlat(t, d, 0, "UTF-8").Read(context.Background())
	want := "Puppet::Util::FileType::FileTypeFlat could not read " + d + ": Is a directory @ io_fread - " + d
	if err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
}

func TestFlatWrite(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	oldMask := syscall.Umask(0o022)
	defer syscall.Umask(oldMask)

	mode := func(p string) fs.FileMode {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}

	p := filepath.Join(dir, "new")
	if err := newFlat(t, p, 0, "UTF-8").Write(ctx, "x\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "x\n" || mode(p) != 0o600 {
		t.Errorf("new file: %q mode %o; want x\\n 0600", b, mode(p))
	}

	p2 := filepath.Join(dir, "new640")
	if err := newFlat(t, p2, 0o666, "UTF-8").Write(ctx, "y"); err != nil {
		t.Fatal(err)
	}
	if mode(p2) != 0o644 { // 0666 less umask 022
		t.Errorf("defaultMode 0666 under umask 022 gave %o; want 0644", mode(p2))
	}

	// Rewrite keeps inode and mode and truncates. Keeping the owner needs root
	// to make the file someone else's; the G5 container gate covers it.
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	if err := newFlat(t, p, 0o666, "UTF-8").Write(ctx, "z"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(p)
	if b, _ := os.ReadFile(p); string(b) != "z" || !os.SameFile(before, after) || mode(p) != 0o640 {
		t.Errorf("rewrite: %q same=%v mode %o", b, os.SameFile(before, after), mode(p))
	}

	// A symlink is followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if err := newFlat(t, link, 0, "UTF-8").Write(ctx, "via link"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "via link" {
		t.Errorf("symlink target holds %q", b)
	}
	if st, _ := os.Lstat(link); st.Mode()&fs.ModeSymlink == 0 {
		t.Error("symlink was replaced")
	}
}

func TestFlatWriteErrors(t *testing.T) {
	dir := t.TempDir()
	ctx, logs := logCtx(t)

	p := filepath.Join(dir, "no", "such", "f")
	err := newFlat(t, p, 0, "UTF-8").Write(ctx, "x")
	want := "Puppet::Util::FileType::FileTypeFlat could not write " + p + ": No such file or directory @ rb_sysopen - " + p
	if err == nil || err.Error() != want {
		t.Errorf("missing parent: %v; want %q", err, want)
	}
	if !hasLog(*logs, "err", want) {
		t.Errorf("log_exception line missing: %v", *logs)
	}
}

// The copy onto the target fails with the errno Ruby's FileUtils.cp raises
// (measured with ruby 3.4.9 on /dev/full: Errno::ENOSPC, "No space left on
// device - write"), after the temp file was written and the target opened.
func TestFlatWriteCopyFailure(t *testing.T) {
	ctx, logs := logCtx(t)
	err := newFlat(t, "/dev/full", 0, "UTF-8").Write(ctx, "x\n")
	want := "Puppet::Util::FileType::FileTypeFlat could not write /dev/full: No space left on device - write"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
	if !hasLog(*logs, "err", want) {
		t.Errorf("log_exception line missing: %v", *logs)
	}
}

// FileUtils.cp into a directory target creates puppet<random> there with the
// temp file's mode less the umask (measured with ruby 3.4.9: mode 100640 for
// defaultMode 0640 under umask 022).
func TestFlatWriteIntoDirectory(t *testing.T) {
	oldMask := syscall.Umask(0o022)
	defer syscall.Umask(oldMask)
	dir := t.TempDir()
	if err := newFlat(t, dir, 0o640, "UTF-8").Write(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), "puppet") {
		t.Fatalf("directory holds %v, %v; want one puppet<random> file", ents, err)
	}
	st, _ := ents[0].Info()
	b, _ := os.ReadFile(filepath.Join(dir, ents[0].Name()))
	if string(b) != "x" || st.Mode().Perm() != 0o640 {
		t.Errorf("copy = %q mode %o; want x 0640", b, st.Mode().Perm())
	}
}

// Under US-ASCII the temp file write fails before the target is touched.
func TestFlatWriteEncoding(t *testing.T) {
	ctx, logs := logCtx(t)
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := newFlat(t, p, 0, "US-ASCII").Write(ctx, "caf\u00e9")
	want := "Puppet::Util::FileType::FileTypeFlat could not write " + p + ": U+00E9 from UTF-8 to US-ASCII"
	if err == nil || err.Error() != want || !hasLog(*logs, "err", want) {
		t.Errorf("err = %v; logs %v", err, *logs)
	}
	if b, _ := os.ReadFile(p); string(b) != "keep" {
		t.Errorf("target changed to %q", b)
	}
	if err := newFlat(t, p, 0, "UTF-8").Write(ctx, "café"); err != nil {
		t.Errorf("UTF-8 write: %v", err)
	}
}

// Ruby's messages for text a US-ASCII pipe cannot take, measured with ruby 3.4.9.
func TestEncodeError(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ascii only\n", ""},
		{"X=\u00e9", "U+00E9 from UTF-8 to US-ASCII"},
		{"a\u9000", "U+9000 from UTF-8 to US-ASCII"},
		{"A\xff", `"\xFF" on UTF-8`},
		{"\x80", `"\x80" on UTF-8`},
		{"\xc0\x80", `"\xC0" on UTF-8`},
		{"x\xe9y", `"\xE9" followed by "y" on UTF-8`},
		{"\xc3(", `"\xC3" followed by "(" on UTF-8`},
		{"\xe9\xff", `"\xE9" followed by "\xFF" on UTF-8`},
		{"a\xe9\x80y", `"\xE9\x80" followed by "y" on UTF-8`},
		{"\xf0\x90\x80\n", `"\xF0\x90\x80" followed by "\n" on UTF-8`},
		{"a\xe9", `incomplete "\xE9" on UTF-8`},
		{"a\xe9\x80", `incomplete "\xE9\x80" on UTF-8`},
		{"a\xf0\x90", `incomplete "\xF0\x90" on UTF-8`},
		{"\xed\xa0\x80", `"\xED" followed by "\xA0" on UTF-8`},
		{"\xe0\x80\x80", `"\xE0" followed by "\x80" on UTF-8`},
		{"\xf4\x90\x80\x80", `"\xF4" followed by "\x90" on UTF-8`},
		{"\xe9\"", `"\xE9" followed by "\"" on UTF-8`},
	}
	for _, tc := range tests {
		if got := encodeError("US-ASCII", tc.in); got != tc.want {
			t.Errorf("encodeError(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	if got := encodeError("UTF-8", "\xff"); got != "" {
		t.Errorf("UTF-8 pipe: %q", got)
	}
}

func TestFlatHooks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	var calls []string
	hook := func(name string) func(context.Context, string) error {
		return func(_ context.Context, path string) error {
			calls = append(calls, name+" "+path)
			return nil
		}
	}
	ft, err := NewFlat(p, 0, hook("backup"), hook("relabel"), "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	b := ft.(Backuper)
	ctx := context.Background()
	if err := b.Backup(ctx); err != nil || len(calls) != 0 {
		t.Fatalf("backup of a missing file: %v, calls %v", err, calls)
	}
	if err := ft.Write(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Backup(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"relabel " + p, "backup " + p}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %v; want %v", calls, want)
	}

	boom := errors.New("boom")
	ft, _ = NewFlat(p, 0, noopHook, func(context.Context, string) error { return boom }, "UTF-8")
	if err := ft.Write(ctx, "x"); !errors.Is(err, boom) {
		t.Errorf("relabel error lost: %v", err)
	}
	ft, _ = NewFlat(p, 0, func(context.Context, string) error { return boom }, noopHook, "UTF-8")
	if err := ft.(Backuper).Backup(ctx); !errors.Is(err, boom) {
		t.Errorf("backup error lost: %v", err)
	}
}

// fakeRunner records its calls and answers with a scripted result.
type fakeRunner struct {
	calls []runCall
	code  int
	out   string
	err   error
}

type runCall struct {
	name           string
	args           []string
	stdin          string
	failOnTruncate bool
	keepUserEnv    bool
	errorName      string
}

func (f *fakeRunner) Run(_ context.Context, o command.ArgvOpts, name string, args ...string) (int, string, error) {
	f.calls = append(f.calls, runCall{name, args, string(o.Stdin), o.FailOnTruncate, o.KeepUserEnv, o.ErrorName})
	return f.code, f.out, f.err
}

// errNSS fails every user lookup.
type errNSS struct{ raltest.FakeNSS }

func (errNSS) User(string) (nameservice.PasswdEntry, bool, error) {
	return nameservice.PasswdEntry{}, false, errors.New("nss down")
}

func testNSS() raltest.FakeNSS {
	self := int64(os.Getuid())
	return raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{
		"me":    {Name: "me", UID: self},
		"other": {Name: "other", UID: self + 1},
	}}
}

func newCrontab(t *testing.T, user string, run command.ShadowRunner, ns nameservice.NameService, stderr *bytes.Buffer, enc string) FileType {
	t.Helper()
	ft, err := NewCrontab(context.Background(), user, "/usr/bin/crontab", run, ns, stderr, enc)
	if err != nil {
		t.Fatal(err)
	}
	return ft
}

func TestCrontabReadArgv(t *testing.T) {
	for _, tc := range []struct {
		user string
		args []string
	}{
		{"me", []string{"-l"}},
		{"other", []string{"-u", "other", "-l"}},
	} {
		run := &fakeRunner{out: "# HEADER x\n1 * * * * true\n"}
		got, err := newCrontab(t, tc.user, run, testNSS(), &bytes.Buffer{}, "UTF-8").Read(context.Background())
		if err != nil || got != "1 * * * * true\n" {
			t.Errorf("%s: %q, %v", tc.user, got, err)
		}
		if len(run.calls) != 1 || run.calls[0].name != "/usr/bin/crontab" || run.calls[0].errorName != "crontab" || strings.Join(run.calls[0].args, " ") != strings.Join(tc.args, " ") || run.calls[0].stdin != "" {
			t.Errorf("%s: calls = %+v; want args %v", tc.user, run.calls, tc.args)
		}
	}
}

func TestCrontabReadNoUser(t *testing.T) {
	ctx, logs := logCtx(t)
	run := &fakeRunner{}
	ft, err := NewCrontab(ctx, "ghost", "crontab", run, testNSS(), &bytes.Buffer{}, "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ft.Read(ctx)
	if got != "" || err != nil || len(run.calls) != 0 {
		t.Errorf("got %q, %v, calls %v", got, err, run.calls)
	}
	want := "The ghost user does not exist. Treating their crontab file as empty in case Puppet creates them in the middle of the run."
	if !hasLog(*logs, "debug", want) {
		t.Errorf("debug line missing: %v", *logs)
	}
}

func TestCrontabReadFailures(t *testing.T) {
	notAuthorized := "The other user is not authorized to use cron. Their crontab file is treated as empty in case Puppet authorizes them in the middle of the run (by, for example, modifying the cron.deny or cron.allow files)."
	tests := []struct {
		name    string
		run     fakeRunner
		want    string
		wantErr string
		debug   string
	}{
		{name: "no crontab", run: fakeRunner{code: 1, out: "no crontab for other\n"}},
		{name: "not allowed", run: fakeRunner{code: 1, out: "You (other) are not allowed to use this program (crontab)\nSee crontab(1) for more information\n"}, debug: notAuthorized},
		{name: "unknown user", run: fakeRunner{code: 1, out: "crontab:  user `other' unknown\n"},
			wantErr: "Could not read crontab for other: Execution of 'crontab -u other -l' returned 1: crontab:  user `other' unknown"},
		{name: "output over the cap", run: fakeRunner{err: errors.New("output of 'crontab -u other -l' exceeded 64 MiB")},
			wantErr: "Could not read crontab for other: output of 'crontab -u other -l' exceeded 64 MiB"},
		{name: "start failure", run: fakeRunner{err: errors.New("run 'crontab -u other -l': fork/exec: no such file or directory")},
			wantErr: "Could not read crontab for other: run 'crontab -u other -l': fork/exec: no such file or directory"},
	}
	for _, tc := range tests {
		ctx, logs := logCtx(t)
		ft, err := NewCrontab(ctx, "other", "crontab", &tc.run, testNSS(), &bytes.Buffer{}, "UTF-8")
		if err != nil {
			t.Fatal(err)
		}
		got, err := ft.Read(ctx)
		if tc.wantErr == "" {
			if err != nil || got != tc.want {
				t.Errorf("%s: %q, %v", tc.name, got, err)
			}
		} else {
			var re ReadError
			if !errors.As(err, &re) || re.Msg != tc.wantErr || err.Error() != tc.wantErr {
				t.Errorf("%s: err = %#v; want ReadError %q", tc.name, err, tc.wantErr)
			}
		}
		if tc.debug != "" && !hasLog(*logs, "debug", tc.debug) {
			t.Errorf("%s: debug line missing: %v", tc.name, *logs)
		}
	}
}

func TestCrontabNSSErrors(t *testing.T) {
	_, err := NewCrontab(context.Background(), "x", "crontab", &fakeRunner{}, errNSS{testNSS()}, &bytes.Buffer{}, "UTF-8")
	var re ReadError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "Could not retrieve user x: ") || !strings.Contains(re.Msg, "nss down") {
		t.Errorf("constructor err = %v", err)
	}

	// The lookup in Read is a second one, as in Ruby (cft:47); it can fail
	// after the constructor's succeeded.
	flaky := &flakyNSS{FakeNSS: testNSS()}
	ft := newCrontab(t, "me", &fakeRunner{}, flaky, &bytes.Buffer{}, "UTF-8")
	flaky.fail = true
	_, err = ft.Read(context.Background())
	if !errors.As(err, &re) || !strings.HasPrefix(re.Msg, "Could not read crontab for me: ") || !strings.Contains(re.Msg, "nss down") {
		t.Errorf("read err = %v", err)
	}
	err = ft.Write(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "nss down") {
		t.Errorf("write err = %v", err)
	}
}

type flakyNSS struct {
	raltest.FakeNSS
	fail bool
}

func (f *flakyNSS) User(name string) (nameservice.PasswdEntry, bool, error) {
	if f.fail {
		return nameservice.PasswdEntry{}, false, errors.New("nss down")
	}
	return f.FakeNSS.User(name)
}

func TestCrontabReadEncoding(t *testing.T) {
	run := &fakeRunner{out: "# caf\xc3\xa9\n"}
	ctx, logs := logCtx(t)
	ft, _ := NewCrontab(ctx, "me", "crontab", run, testNSS(), &bytes.Buffer{}, "US-ASCII")
	_, err := ft.Read(ctx)
	want := "Puppet::Provider::Cron::FileType::FileTypeCrontab could not read me: invalid byte sequence in US-ASCII"
	var re ReadError
	if err == nil || err.Error() != want || errors.As(err, &re) {
		t.Errorf("err = %v; want plain %q", err, want)
	}
	if !hasLog(*logs, "err", want) {
		t.Errorf("log_exception line missing: %v", *logs)
	}
	ft, _ = NewCrontab(ctx, "me", "crontab", run, testNSS(), &bytes.Buffer{}, "UTF-8")
	if got, err := ft.Read(ctx); got != "# café\n" || err != nil {
		t.Errorf("UTF-8: %q, %v", got, err)
	}
}

func TestCrontabWriteArgv(t *testing.T) {
	for _, tc := range []struct {
		user string
		args []string
	}{
		{"me", []string{"-"}},
		{"other", []string{"-u", "other", "-"}},
	} {
		// Exit status is ignored; the output goes to stderr verbatim.
		run := &fakeRunner{code: 1, out: "Backup of x saved\n\"-\":1: bad minute\n"}
		var stderr bytes.Buffer
		err := newCrontab(t, tc.user, run, testNSS(), &stderr, "UTF-8").Write(context.Background(), "1 * * * * true\n")
		if err != nil {
			t.Fatalf("%s: %v", tc.user, err)
		}
		if len(run.calls) != 1 || strings.Join(run.calls[0].args, " ") != strings.Join(tc.args, " ") || run.calls[0].stdin != "1 * * * * true\n" {
			t.Errorf("%s: calls = %+v; want args %v", tc.user, run.calls, tc.args)
		}
		if stderr.String() != run.out {
			t.Errorf("%s: stderr = %q; want %q", tc.user, stderr.String(), run.out)
		}
	}
}

// cft:56's `crontab -l` runs through Execution.execute, which removes HOME,
// USER and LOGNAME; cft:86's write is an IO.popen, whose child inherits them,
// so cronie finds HOME and saves its backup of the replaced crontab.
func TestCrontabUserEnv(t *testing.T) {
	run := &fakeRunner{}
	ft := newCrontab(t, "other", run, testNSS(), &bytes.Buffer{}, "UTF-8")
	if _, err := ft.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ft.Write(context.Background(), "1 * * * * true\n"); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 || run.calls[0].keepUserEnv || !run.calls[1].keepUserEnv {
		t.Errorf("calls = %+v; want the read without the user variables and the write with them", run.calls)
	}
}

func TestCrontabWriteErrors(t *testing.T) {
	ctx := context.Background()

	run := &fakeRunner{}
	ft, _ := NewCrontab(ctx, "ghost", "crontab", run, testNSS(), &bytes.Buffer{}, "UTF-8")
	err := ft.Write(ctx, "x")
	if err == nil || err.Error() != "Cannot write the ghost user's crontab: The user does not exist" || len(run.calls) != 0 {
		t.Errorf("no user: %v, calls %v", err, run.calls)
	}

	ctx, logs := logCtx(t)
	run = &fakeRunner{err: errors.New("run 'crontab -': fork/exec: no such file or directory")}
	err = newCrontab(t, "me", run, testNSS(), &bytes.Buffer{}, "UTF-8").Write(ctx, "x")
	want := "Puppet::Provider::Cron::FileType::FileTypeCrontab could not write me: run 'crontab -': fork/exec: no such file or directory"
	if err == nil || err.Error() != want || !hasLog(*logs, "err", want) {
		t.Errorf("start failure: %v; logs %v", err, *logs)
	}
}

func TestCrontabWriteEncoding(t *testing.T) {
	ctx, logs := logCtx(t)
	run := &fakeRunner{}
	ft := newCrontab(t, "me", run, testNSS(), &bytes.Buffer{}, "US-ASCII")
	err := ft.Write(ctx, "A=1\nX=é\n")
	want := "Puppet::Provider::Cron::FileType::FileTypeCrontab could not write me: U+00E9 from UTF-8 to US-ASCII"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
	if len(run.calls) != 0 {
		t.Errorf("crontab ran: %+v", run.calls)
	}
	if !hasLog(*logs, "err", want) {
		t.Errorf("log_exception line missing: %v", *logs)
	}
	// Each later flush fails the same way.
	if err := ft.Write(ctx, "X=é"); err == nil || err.Error() != want {
		t.Errorf("second write: %v", err)
	}
	// ASCII text is written under US-ASCII, UTF-8 text under UTF-8.
	if err := ft.Write(ctx, "A=1\n"); err != nil || len(run.calls) != 1 {
		t.Errorf("ascii write: %v, calls %d", err, len(run.calls))
	}
	utf := newCrontab(t, "me", run, testNSS(), &bytes.Buffer{}, "UTF-8")
	if err := utf.Write(ctx, "X=é\n"); err != nil || run.calls[1].stdin != "X=é\n" {
		t.Errorf("utf-8 write: %v, calls %+v", err, run.calls)
	}
}

// The read must fail on an over-cap crontab, or a write would shorten it.
func TestCrontabReadSetsFailOnTruncate(t *testing.T) {
	run := &fakeRunner{}
	if _, err := newCrontab(t, "me", run, testNSS(), &bytes.Buffer{}, "UTF-8").Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 || !run.calls[0].failOnTruncate {
		t.Errorf("calls = %+v; want FailOnTruncate set", run.calls)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stderr closed") }

// crontab has installed the text by then; Ruby's inherited stderr cannot fail the flush.
func TestCrontabWriteIgnoresStderrFailure(t *testing.T) {
	run := &fakeRunner{out: "Backup of me saved\n"}
	ft, err := NewCrontab(context.Background(), "me", "crontab", run, testNSS(), failingWriter{}, "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if err := ft.Write(context.Background(), "x\n"); err != nil {
		t.Errorf("Write = %v; want nil", err)
	}
}
