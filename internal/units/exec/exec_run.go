package exec

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/lib/command"
)

// errExecTimeout is the timeout as Ruby's Timeout::Error has it. type/exec.rb
// rewords it for the main command and the guards; a refresh command's failure
// shows it as it is.
var errExecTimeout = errors.New("execution expired")

// exit127Error is a command that exited 127. provider/exec.rb:80-83 raises its
// output as the error, whatever returns accepts (M20), so the text is the
// output alone.
type exit127Error struct{ output string }

func (e exit127Error) Error() string { return e.output }

// runExec starts cmd with start, its output on a capture file (runCapped), and
// waits for it under timeout (0 is none) and ctx. cmd must lead its own process
// group (SysProcAttr.Setpgid), because both ways of stopping it signal the
// group, not the process.
//
// Measured, and the reason the group is signalled: what the process Go started
// is depends entirely on which shell /bin/sh happens to be.
//
//	host        /bin/sh -> bash   `sh -c 'sleep 5'` is optimised into an exec,
//	                              so the process Go started IS sleep and
//	                              killing it works
//	container   /bin/sh -> dash   dash FORKS for the command, so killing it
//	                              reaps dash while sleep survives
//
// dash is /bin/sh on Debian and Ubuntu, so the broken case is the common one --
// including this project's own fleet-node-deb image. A cancellation test once
// passed on the host for a reason that had nothing to do with the code being
// right. Signalling the negated pid reaches every member of the group, so a
// forked grandchild gets the signal with its parent.
//
// On timeout the group gets SIGTERM, never more (D-83: Ruby TERMs the direct
// child alone, M3), and runExec returns errExecTimeout at once, discarding the
// output as Ruby does. A command that traps TERM keeps running; the waiter
// goroutine, which only calls Wait, reaps it when it exits, as Ruby's
// Thread.new { Process.waitpid(child_pid) } does (util/execution.rb:283).
//
// When ctx is done while runExec waits, the group gets SIGKILL: the operator
// stopped the run. The wait for the leader is then bounded by cancelGrace and
// its status is returned as usual. Nothing is signalled once runExec has
// returned, so the agent's own exit, which cancels ctx, never escalates a
// timeout. This is why cmd is not exec.CommandContext's: its Cancel would fire
// at any time, and its WaitDelay expiry SIGKILLs the leader.
func runExec(ctx context.Context, cmd *exec.Cmd, start func(*exec.Cmd) error, timeout time.Duration, what string) (int, string, error) {
	timedOut := false
	code, out, err := command.RunCapped(ctx, cmd, what, func(cmd *exec.Cmd) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := start(cmd); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var expired <-chan time.Time
		if timeout > 0 {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			expired = timer.C
		}
		// The group id is the leader's pid because of Setpgid. Until the
		// waiter has reaped the leader, that id cannot name another group. A
		// signal that loses the race with the reaping meets ESRCH, unless
		// descendants still hold the group, which are then signalled as meant.
		pgid := cmd.Process.Pid
		select {
		case err := <-done:
			return err
		case <-expired:
			if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("terminate process group on timeout: %w", err)
			}
			timedOut = true
			return errExecTimeout
		case <-ctx.Done():
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill process group: %w", err)
			}
			grace := time.NewTimer(cancelGrace)
			defer grace.Stop()
			select {
			case err := <-done:
				return err
			case <-grace.C:
				return ctx.Err()
			}
		}
	})
	if timedOut {
		return 0, "", errExecTimeout
	}
	return code, out, err
}
