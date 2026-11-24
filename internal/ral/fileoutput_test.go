package ral

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/report"
)

// runWithin runs the command through fileOutputRunner and fails the test,
// rather than hanging it, if run has not returned within limit.
func runWithin(t *testing.T, limit time.Duration, name string, args ...string) (int, string, error) {
	t.Helper()
	type result struct {
		code int
		out  string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, out, err := fileOutputRunner{}.run(context.Background(), name, args...)
		done <- result{code, out, err}
	}()
	select {
	case r := <-done:
		return r.code, r.out, r.err
	case <-time.After(limit):
		t.Fatalf("run did not return within %s", limit)
		return 0, "", nil
	}
}

// A custom start such as `/opt/app/run &` leaves a child holding the
// command's stdout. The runner must return when the command it started exits,
// as Puppet's execute does (util/execution.rb:237-240), not when the last
// holder of the output closes it.
func TestFileOutputRunnerReturnsWhenABackgroundedChildInheritsStdout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "sleep.pid")
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	code, out, err := runWithin(t, 5*time.Second,
		"/bin/sh", "-c", `sleep 30 & echo $! > "$1"; echo started`, "sh", pidFile)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 || out != "started\n" {
		t.Errorf("got (%d, %q), want (0, %q)", code, out, "started\n")
	}
}

func TestFileOutputRunnerCombinesStdoutAndStderr(t *testing.T) {
	_, out, err := runWithin(t, 5*time.Second, "/bin/sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if out != "out\nerr\n" {
		t.Errorf("out = %q, want %q", out, "out\nerr\n")
	}
}

func TestFileOutputRunnerNonZeroExitIsAValue(t *testing.T) {
	code, out, err := runWithin(t, 5*time.Second, "/bin/sh", "-c", "echo nope; exit 3")
	if err != nil {
		t.Fatalf("a non-zero exit must not be an error: %v", err)
	}
	if code != 3 || out != "nope\n" {
		t.Errorf("got (%d, %q), want (3, %q)", code, out, "nope\n")
	}
}

func TestFileOutputRunnerStartFailureIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-command")
	_, _, err := runWithin(t, 5*time.Second, missing, "arg")
	if err == nil {
		t.Fatal("want an error for a command that cannot start")
	}
	if !strings.Contains(err.Error(), missing+" arg") {
		t.Errorf("err = %q, want it to name the command line", err)
	}
}

// The temporary file is removed once its output has been read.
func TestFileOutputRunnerRemovesItsTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	if _, _, err := runWithin(t, 5*time.Second, "/bin/sh", "-c", "echo x"); err != nil {
		t.Fatalf("err = %v", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// Service's commands go through the file runner by default, so a custom start
// that backgrounds a daemon cannot hang the run.
func TestServiceDefaultRunnerUsesAFile(t *testing.T) {
	if _, ok := (Service{}).runner().(fileOutputRunner); !ok {
		t.Errorf("default runner is %T, want fileOutputRunner", Service{}.runner())
	}
}

func TestCutOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       string
		max      int
		want     string
		wantTrun bool
	}{
		{"under max", "ab\n", 10, "ab\n", false},
		{"exactly max", "ab\ncd", 5, "ab\ncd", false},
		{"cut after the last newline", "ab\ncd\nef", 7, "ab\ncd\n", true},
		{"max+1 with a newline inside", "ab\ncdef", 6, "ab\n", true},
		{"newline is the last kept byte", "abc\nxyz", 4, "abc\n", true},
		{"no newline, rune straddles max", "ab€", 4, "ab", true},
		{"no newline, rune ends exactly at max", "ab€x", 5, "ab€", true},
		{"no newline, ASCII", "abcdef", 3, "abc", true},
		{"empty", "", 3, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, truncated := cutOutput([]byte(tc.in), tc.max)
			if string(kept) != tc.want || truncated != tc.wantTrun {
				t.Errorf("cutOutput(%q, %d) = (%q, %v), want (%q, %v)",
					tc.in, tc.max, kept, truncated, tc.want, tc.wantTrun)
			}
			if !utf8.Valid(kept) {
				t.Errorf("kept %q is not valid UTF-8", kept)
			}
		})
	}
}

// withOutputCap lowers the cap for the test.
func withOutputCap(t *testing.T, n int) {
	t.Helper()
	old := outputCap
	outputCap = n
	t.Cleanup(func() { outputCap = old })
}

func TestRunCapturingToFileCaps(t *testing.T) {
	withOutputCap(t, 10)
	for _, tc := range []struct {
		name     string
		script   string
		wantOut  string
		wantTrun bool
	}{
		{"exactly the cap", `printf '123456789\n'`, "123456789\n", false},
		{"one byte over", `printf '12345\n6789012'`, "12345\n", true},
		{"far over, no newline", `printf 'abcdefghijklmnopqrstuvwxyz'`, "abcdefghij", true},
		{"rune straddles the cap", `printf 'abcdefghi\342\202\254'`, "abcdefghi", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, truncated, err := runCapturingToFile(exec.Command("/bin/sh", "-c", tc.script), "sh", (*exec.Cmd).Run)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if code != 0 || out != tc.wantOut || truncated != tc.wantTrun {
				t.Errorf("got (%d, %q, %v), want (0, %q, %v)", code, out, truncated, tc.wantOut, tc.wantTrun)
			}
		})
	}
}

// A command that fails still has its exit status when its output is cut.
func TestRunCapturingToFileKeepsExitStatusWhenTruncated(t *testing.T) {
	withOutputCap(t, 4)
	code, out, truncated, err := runCapturingToFile(exec.Command("/bin/sh", "-c", "printf 'abcdefgh'; exit 3"), "sh", (*exec.Cmd).Run)
	if err != nil || code != 3 || out != "abcd" || !truncated {
		t.Errorf("got (%d, %q, %v, %v), want (3, %q, true, nil)", code, out, truncated, err, "abcd")
	}
}

func TestFileOutputRunnerWarnsOnTruncation(t *testing.T) {
	withOutputCap(t, 4)
	for _, tc := range []struct {
		name    string
		sources Sources
		script  string
		want    []report.LogEntry
	}{
		{"with a ref", Sources{Ref: "Service[x]"}, "printf abcdefgh",
			[]report.LogEntry{{Level: "warning", Source: "voxagent", Message: "Service[x]: output of '/bin/sh -c printf abcdefgh' truncated at 64 MiB"}}},
		{"no ref during prefetch", Sources{}, "printf abcdefgh",
			[]report.LogEntry{{Level: "warning", Source: "voxagent", Message: "output of '/bin/sh -c printf abcdefgh' truncated at 64 MiB"}}},
		{"exactly the cap warns of nothing", Sources{Ref: "Service[x]"}, "printf abcd", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, got := debugContext("notice")
			ctx = WithSources(ctx, tc.sources)
			_, out, err := fileOutputRunner{}.run(ctx, "/bin/sh", "-c", tc.script)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tc.want != nil && out != "abcd" {
				t.Errorf("out = %q, want %q", out, "abcd")
			}
			assertWarnings(t, *got, tc.want)
		})
	}
}

func TestFileOutputRunnerWithoutALoggerStillTruncates(t *testing.T) {
	withOutputCap(t, 4)
	_, out, err := fileOutputRunner{}.run(context.Background(), "/bin/sh", "-c", "printf abcdefgh")
	if err != nil || out != "abcd" {
		t.Errorf("got (%q, %v), want (%q, nil)", out, err, "abcd")
	}
}

// Review Focus 3: the cut is on a character boundary, so logoutput, which
// decodes the output, does not fail on it.
func TestExecLogoutputTruncatedStaysDecodable(t *testing.T) {
	withOutputCap(t, 4)
	setLocale(t, "C.UTF-8")
	ctx, got := debugContext("notice")
	ctx = WithSources(ctx, Sources{Ref: "Exec[big]"})
	r := execRes("big", map[string]any{"command": `printf 'ab\342\202\254'`, "logoutput": true})
	changes, _, err := Exec{}.Apply(ctx, r, false)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = (%v, %v)", changes, err)
	}
	if changes[0].Err != nil {
		t.Fatalf("change failed: %v", changes[0].Err)
	}
	if want := []string{"ab"}; !slices.Equal(changes[0].PropertyNotices, want) {
		t.Errorf("PropertyNotices = %q, want %q", changes[0].PropertyNotices, want)
	}
	assertWarnings(t, *got, []report.LogEntry{{Level: "warning", Source: "voxagent",
		Message: "Exec[big]: output of 'printf 'ab\\342\\202\\254'' truncated at 64 MiB"}})
}

// A Sensitive command stays redacted in the warning.
func TestExecTruncationWarningRedactsASensitiveCommand(t *testing.T) {
	withOutputCap(t, 4)
	ctx, got := debugContext("notice")
	ctx = WithSources(ctx, Sources{Ref: "Exec[s]"})
	r := markedExec(map[string]any{"command": "printf abcdefgh"}, "command")
	if _, _, err := (Exec{}).Apply(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	assertWarnings(t, *got, []report.LogEntry{{Level: "warning", Source: "voxagent",
		Message: "Exec[s]: output of [command redacted] truncated at 64 MiB"}})
}

func TestValidateCmdTruncationWarns(t *testing.T) {
	withOutputCap(t, 4)
	ctx, got := debugContext("notice")
	ctx = WithSources(ctx, Sources{Ref: "File[/x]"})
	path := filepath.Join(t.TempDir(), "f")
	r := fileRes(path, map[string]any{"content": "new\n", "validate_cmd": "printf abcdefgh; true"})
	if _, _, err := (File{}).Apply(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].Level != "warning" || !strings.HasPrefix((*got)[0].Message, "File[/x]: output of ") ||
		!strings.HasSuffix((*got)[0].Message, " truncated at 64 MiB") {
		t.Errorf("entries = %+v, want one truncation warning", *got)
	}
}

func assertWarnings(t *testing.T, got, want []report.LogEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Level != want[i].Level || got[i].Source != want[i].Source || got[i].Message != want[i].Message {
			t.Errorf("entry %d = {%s %s %q}, want {%s %s %q}", i,
				got[i].Level, got[i].Source, got[i].Message, want[i].Level, want[i].Source, want[i].Message)
		}
	}
}

// execRunner serves package and SELinux commands; it is capped like the rest.
func TestExecRunnerTruncatesAndWarns(t *testing.T) {
	withOutputCap(t, 4)
	ctx, got := debugContext("notice")
	ctx = WithSources(ctx, Sources{Ref: "Package[x]"})
	code, out, err := execRunner{}.run(ctx, "/bin/sh", "-c", "printf abcdefgh; exit 100")
	if err != nil || code != 100 || out != "abcd" {
		t.Errorf("got (%d, %q, %v), want (100, %q, nil)", code, out, err, "abcd")
	}
	assertWarnings(t, *got, []report.LogEntry{{Level: "warning", Source: "voxagent",
		Message: "Package[x]: output of '/bin/sh -c printf abcdefgh; exit 100' truncated at 64 MiB"}})
}

func TestExecRunnerStartFailureNamesTheCommand(t *testing.T) {
	_, _, err := execRunner{}.run(context.Background(), "/no/such/cmd", "a")
	if err == nil || !strings.Contains(err.Error(), "run '/no/such/cmd a'") {
		t.Errorf("err = %v, want it to name 'run '/no/such/cmd a''", err)
	}
}

// util/execution.rb:181: Service, Package and SELinux commands log the same
// Executing: line at Puppet, once per command.
func TestSharedRunnersLogExecuting(t *testing.T) {
	for name, run := range map[string]func(context.Context, string, ...string) (int, string, error){
		"fileOutputRunner": fileOutputRunner{}.run,
		"execRunner":       execRunner{}.run,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, got := debugContext("debug")
			if _, _, err := run(ctx, "/bin/echo", "a", "b"); err != nil {
				t.Fatal(err)
			}
			assertWarnings(t, *got, []report.LogEntry{dbg("Puppet", "Executing: '/bin/echo a b'")})
		})
	}
}

// D-78: a command that names a marked value is logged in Ruby's redacted forms
// by both argv runners. The Executing line and the truncation warning's <what>
// would otherwise carry the value into the console and the report.
func TestArgvRunnersRedactACommandNamingAMarkedValue(t *testing.T) {
	for _, runner := range []struct {
		name string
		r    commandRunner
	}{{"execRunner", execRunner{}}, {"fileOutputRunner", fileOutputRunner{}}} {
		for _, marked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s marked=%v", runner.name, marked), func(t *testing.T) {
				withOutputCap(t, 4)
				ctx, got := debugContext("debug")
				if marked {
					ctx = withRedactedCommands(ctx)
				}
				if _, _, err := runner.r.run(ctx, "/bin/echo", "tree-s3cret", "0123456789"); err != nil {
					t.Fatal(err)
				}
				wantExec, wantWarn := "Executing: '/bin/echo tree-s3cret 0123456789'", "output of '/bin/echo tree-s3cret 0123456789' truncated at 64 MiB"
				if marked {
					wantExec, wantWarn = "Executing: '[redacted]'", "output of [command redacted] truncated at 64 MiB"
				}
				if lines := linesAt(*got, "Puppet"); !slices.Equal(lines, []string{wantExec}) {
					t.Errorf("Puppet lines = %q, want %q", lines, wantExec)
				}
				var warns []string
				for _, e := range *got {
					if e.Level == "warning" {
						warns = append(warns, e.Message)
					}
				}
				if !slices.Equal(warns, []string{wantWarn}) {
					t.Errorf("warnings = %q, want %q", warns, wantWarn)
				}
			})
		}
	}
}

// util/execution.rb:372-377: the child runs with the locale variables removed
// and LANG=C, LC_ALL=C, so a tool's messages are in the C locale.
func TestRunArgvWithLocale(t *testing.T) {
	for _, name := range []string{"LC_MESSAGES", "LANGUAGE", "LC_CTYPE"} {
		t.Setenv(name, "de_DE.UTF-8")
	}
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	code, out, err := runArgvWith(context.Background(), argvOpts{}, "/usr/bin/env")
	if err != nil || code != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", code, err)
	}
	lines := strings.Split(out, "\n")
	for _, want := range []string{"LANG=C", "LC_ALL=C"} {
		if n := countOf(lines, want); n != 1 {
			t.Errorf("%s appears %d times in the child env, want 1", want, n)
		}
	}
	for _, l := range lines {
		for _, banned := range []string{"LC_MESSAGES=", "LANGUAGE=", "LC_CTYPE=", "de_DE"} {
			if strings.Contains(l, banned) {
				t.Errorf("child env has %q", l)
			}
		}
	}
}

func countOf(lines []string, s string) int {
	n := 0
	for _, l := range lines {
		if l == s {
			n++
		}
	}
	return n
}

func TestRunArgvWithStdin(t *testing.T) {
	code, out, err := runArgvWith(context.Background(), argvOpts{Stdin: []byte("foo:$6$hash\n")}, "/bin/cat")
	if err != nil || code != 0 || out != "foo:$6$hash\n" {
		t.Errorf("got (%d, %q, %v), want (0, the stdin bytes, nil)", code, out, err)
	}
	code, out, err = runArgvWith(context.Background(), argvOpts{}, "/bin/cat")
	if err != nil || code != 0 || out != "" {
		t.Errorf("no stdin: got (%d, %q, %v), want (0, \"\", nil)", code, out, err)
	}
}

func TestRunArgvWithRedactedDebug(t *testing.T) {
	for _, redact := range []bool{false, true} {
		ctx, got := debugContext("debug")
		if _, _, err := runArgvWith(ctx, argvOpts{Redact: redact}, "/bin/echo", "s3cret-hash"); err != nil {
			t.Fatal(err)
		}
		want := "Executing: '/bin/echo s3cret-hash'"
		if redact {
			want = "Executing: '[redacted]'"
		}
		if lines := linesAt(*got, "Puppet"); !slices.Equal(lines, []string{want}) {
			t.Errorf("redact=%v: Puppet lines = %q, want %q", redact, lines, want)
		}
	}
}

func TestRunArgvWithRedactedTruncationWarning(t *testing.T) {
	withOutputCap(t, 4)
	ctx, got := debugContext("debug")
	if _, _, err := runArgvWith(ctx, argvOpts{Redact: true}, "/bin/echo", "s3cret-hash", "0123456789"); err != nil {
		t.Fatal(err)
	}
	for _, e := range *got {
		if strings.Contains(e.Message, "s3cret") {
			t.Errorf("log entry leaks the argument: %q", e.Message)
		}
	}
}

func TestHostShadowRunnerRunsTheCommand(t *testing.T) {
	var r shadowRunner = hostShadowRunner{}
	code, out, err := r.run(context.Background(), argvOpts{Stdin: []byte("x")}, "/bin/cat")
	if err != nil || code != 0 || out != "x" {
		t.Errorf("got (%d, %q, %v), want (0, \"x\", nil)", code, out, err)
	}
}

func TestExecFailureText(t *testing.T) {
	cmd := []string{"/usr/sbin/usermod", "-u", "5", "foo"}
	for _, tc := range []struct {
		name   string
		cmd    []string
		redact bool
		code   int
		output string
		want   string
	}{
		{"plain", cmd, false, 4, "usermod: x\n", "Execution of '/usr/sbin/usermod -u 5 foo' returned 4: usermod: x"},
		{"redacted", cmd, true, 4, "usermod: x\n", "Execution of '[redacted]' returned 4: usermod: x"},
		{"empty element", []string{"/usr/sbin/usermod", "-c", "", "foo"}, false, 1, "bad",
			"Execution of '/usr/sbin/usermod -c  foo' returned 1: bad"},
		{"strip NUL and space", cmd, false, 2, " \x00\n x y \x00\t\n", "Execution of '/usr/sbin/usermod -u 5 foo' returned 2: x y"},
		{"empty output", cmd, false, 3, "", "Execution of '/usr/sbin/usermod -u 5 foo' returned 3: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := execFailure(tc.cmd, tc.redact, tc.code, tc.output).Error(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
