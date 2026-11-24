package asuser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ErrInvalidResult is wrapped by the error Start returns for any helper output
// or exit status ParseResult refuses: "asuser helper for <path> returned an
// invalid result", which quotes nothing from the helper.
var ErrInvalidResult = errors.New("invalid result")

// Starter runs one op ("mkdir <dir>", "write --encoding <enc> <path>",
// "chmod <path>"; args, whose last element is the path) as cred, with stdin
// as the helper's standard input, and returns its Result. Tests replace it.
type Starter interface {
	Start(ctx context.Context, cred *syscall.Credential, args []string, stdin []byte) (Result, error)
}

// ProcStarter starts the helper as "/proc/self/exe asuser <args>", the
// running binary's inode, so a binary replaced or deleted mid-run is never
// the one started (spec §3.4).
type ProcStarter struct{}

// helperWaitDelay bounds how long Start waits for the helper's stdout to close
// once the helper has exited: a process the helper left behind (its user may
// ptrace it and fork) must not hold root's run.
const helperWaitDelay = time.Second

// Start runs the helper and parses its result. It starts nothing for a nil
// credential or uid 0 (the helper never runs as root; the caller performs the
// ops in-process then, spec §3.4) nor for empty args. A start failure is
// "Could not start the asuser helper via /proc/self/exe (is /proc mounted?):
// <err>"; output or an exit ParseResult refuses, an unexpected wait error and
// a helper killed by a signal are ErrInvalidResult. Once ctx is done, any
// failure is ctx.Err(): the helper was stopped, it did not misbehave. The
// helper's stdout is capped (cappedBuffer) and its stderr goes to the null
// device.
func (ProcStarter) Start(ctx context.Context, cred *syscall.Credential, args []string, stdin []byte) (Result, error) {
	if cred == nil || cred.Uid == 0 {
		return Result{}, errors.New("refusing to start the asuser helper as root")
	}
	if len(args) == 0 {
		return Result{}, errors.New("the asuser helper needs an op")
	}
	cmd := procCmd(ctx, cred, args, stdin)
	var out cappedBuffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("Could not start the asuser helper via /proc/self/exe (is /proc mounted?): %w", err)
	}
	invalid := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("asuser helper for %s returned an %w", args[len(args)-1], ErrInvalidResult)
	}
	var ee *exec.ExitError
	if err := cmd.Wait(); err != nil && !errors.As(err, &ee) {
		return Result{}, invalid()
	}
	r, ok := ParseResult(out.buf, cmd.ProcessState.ExitCode())
	if !ok {
		return Result{}, invalid()
	}
	return r, nil
}

// procCmd is the helper's command: /proc/self/exe with "asuser" and args,
// credential cred set before execve, stdin, the agent's cwd and umask
// (inherited), and an environment of PATH and TMPDIR only, each when set: the
// helper's /proc/<pid>/environ is readable by its user, and the agent's
// environment may hold secrets (spec §11). stderr is the null device.
func procCmd(ctx context.Context, cred *syscall.Credential, args []string, stdin []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/proc/self/exe", append([]string{"asuser"}, args...)...)
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "TMPDIR"} {
		if v, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+v)
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.WaitDelay = helperWaitDelay
	return cmd
}

// cappedBuffer keeps the first maxResult+1 bytes written to it, enough for
// ParseResult to tell a too-long result, and drains the rest, so a helper
// that floods its stdout costs root no memory (D-80) and is never blocked.
type cappedBuffer struct{ buf []byte }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := maxResult + 1 - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
