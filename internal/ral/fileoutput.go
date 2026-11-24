package ral

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/runlog"
)

// fileOutputRunner is the commandRunner service uses. It and execRunner share
// runArgv: for both, the command's stdout and stderr are a temporary file, not
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
// The contract is execRunner's: a non-zero exit is a value, and only a command
// that cannot start (or whose output cannot be captured) is an error.
type fileOutputRunner struct{}

// run implements commandRunner.
func (fileOutputRunner) run(ctx context.Context, name string, args ...string) (int, string, error) {
	return runArgv(ctx, name, args...)
}

// redactCommandKey marks a context whose commands name a value Sensitive
// marks, so the runners log them in Ruby's redacted forms (D-78).
type redactCommandKey struct{}

// withRedactedCommands returns ctx marking every command run under it as
// naming a marked value. The commandRunner interface has no room for a flag,
// so the caller that composes the argv says so here.
func withRedactedCommands(ctx context.Context) context.Context {
	return context.WithValue(ctx, redactCommandKey{}, true)
}

// runArgv is the body of both argv runners: it runs name and args with no
// shell through runLogged, showing the command as '[redacted]' in the
// Executing line and as [command redacted] in the truncation warning when ctx
// is marked by withRedactedCommands.
func runArgv(ctx context.Context, name string, args ...string) (int, string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	redact, _ := ctx.Value(redactCommandKey{}).(bool)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = withoutUserEnv(os.Environ())
	return runLogged(ctx, cmd, quoteOrRedact(line, redact), redactIf(redact, "%s", line))
}

// rubyLocaleVars is util/posix.rb:6-7's LOCALE_ENV_VARS: the variables Puppet
// deletes in the child, which util/execution.rb:372-379 then replaces with
// LANG=C and LC_ALL=C.
var rubyLocaleVars = []string{
	"LANG", "LC_ALL", "LC_MESSAGES", "LANGUAGE", "LC_COLLATE",
	"LC_CTYPE", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
}

// argvOpts shapes one runArgvWith command.
type argvOpts struct {
	// Stdin is the child's standard input; nil is an empty input.
	Stdin []byte
	// Redact shows the command as '[redacted]' in the Executing line and as
	// [command redacted] in the truncation warning: its argv or Stdin names a
	// value that must not reach a log, such as a password hash.
	Redact bool
}

// runArgvWith is runArgv for a command Puppet runs through Puppet::Util::
// Execution.execute with the locale override (util/execution.rb:372-377): the
// locale variables are removed and LANG=C, LC_ALL=C set, so a tool's messages
// are the C locale's. o gives its stdin and whether it is logged redacted.
func runArgvWith(ctx context.Context, o argvOpts, name string, args ...string) (int, string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	cmd := exec.CommandContext(ctx, name, args...)
	env := withoutUserEnv(os.Environ())
	cmd.Env = make([]string, 0, len(env)+2)
	for _, kv := range env {
		varName, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(rubyLocaleVars, varName) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "LANG=C", "LC_ALL=C")
	if o.Stdin != nil {
		cmd.Stdin = bytes.NewReader(o.Stdin)
	}
	return runLogged(ctx, cmd, quoteOrRedact(line, o.Redact), redactIf(o.Redact, "%s", line))
}

// shadowRunner is how the user and group providers run shadow-utils commands;
// tests substitute a fake.
type shadowRunner interface {
	run(ctx context.Context, o argvOpts, name string, args ...string) (int, string, error)
}

// hostShadowRunner runs the command on the host through runArgvWith.
type hostShadowRunner struct{}

// run implements shadowRunner.
func (hostShadowRunner) run(ctx context.Context, o argvOpts, name string, args ...string) (int, string, error) {
	return runArgvWith(ctx, o, name, args...)
}

// execFailure is the Puppet::ExecutionFailure for a command that exited with
// code (util/execution.rb:312). cmd is joined by single spaces, so an empty
// element leaves a double space; redact shows it as [redacted] (:162).
func execFailure(cmd []string, redact bool, code int, output string) error {
	return &executionFailure{command: redactIf(redact, "%s", strings.Join(cmd, " ")), status: code, output: output}
}

// maxCommandOutput caps the output kept from one command, so a command that
// floods its output cannot exhaust the agent's memory (D-80). Puppet keeps all
// of it.
const maxCommandOutput = 64 << 20

// outputCap is maxCommandOutput; tests lower it.
var outputCap = maxCommandOutput

// runLogged is runCapped for a command that is new to the run: it first logs
// Ruby's debug line for the execution, once per command (util/execution.rb:181,
// source Puppet). executing is the command as Ruby's command_str has it, the
// words joined by a space, or "[redacted]" when Sensitive marks the command
// (redactIf), which Ruby shows as '[redacted]' (:162). Every command runner
// comes through here, so validate_cmd, Package, Service and SELinux commands
// all log it, and Exec logs it itself before runExec; a retry of the same
// command (ENOEXEC) goes to runCapped or runExec and does not log it again.
func runLogged(ctx context.Context, cmd *exec.Cmd, what, executing string) (int, string, error) {
	runlog.Debugf(ctx, "Puppet", "Executing: '%s'", executing)
	return runCapped(ctx, cmd, what, (*exec.Cmd).Run)
}

// runCapped is runCapturingToFile for a caller that has a context: when the
// output was cut it logs the D-80 warning, which is never a failure. what is
// the string runCapturingToFile gets, so a command Sensitive marks stays
// redacted. Type-wide prefetch runs with no resource in the context, so the
// warning then has no reference prefix. run is runCapturingToFile's.
func runCapped(ctx context.Context, cmd *exec.Cmd, what string, run func(*exec.Cmd) error) (int, string, error) {
	code, out, truncated, err := runCapturingToFile(cmd, what, run)
	if truncated {
		msg := fmt.Sprintf("output of %s truncated at 64 MiB", what)
		if ref := SourcesFrom(ctx).Ref; ref != "" {
			msg = ref + ": " + msg
		}
		runlog.From(ctx).Log("warning", "voxagent", msg)
	}
	return code, out, err
}

// runCapturingToFile runs cmd with its stdout and stderr on a temporary file,
// for the reason fileOutputRunner gives, and returns the exit status and the
// combined output. Exec.execute uses it too: an exec command that backgrounds
// a process meets the same pipe. cmd's Stdout and Stderr are overwritten;
// what is the command as errors name it, quoted or redacted (quoteOrRedact).
//
// At most outputCap bytes of output are kept (cutOutput); truncated says the
// command wrote more. The command still runs to its end and its exit status is
// reported as usual. Callers report the truncation through runCapped.
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
	all, err := io.ReadAll(io.LimitReader(f, int64(outputCap)+1))
	if err != nil {
		return 0, "", false, fmt.Errorf("run %s: read output: %w", what, err)
	}
	kept, truncated := cutOutput(all, outputCap)
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
