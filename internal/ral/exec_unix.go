//go:build unix

package ral

import (
	"os/exec"
	"syscall"
	"time"
)

// cancelGrace bounds how long Wait may block after the context is cancelled and
// the process group has been killed.
//
// Killing the group is what actually stops the work; this is the second line of
// defence for a shell that somehow survives the signal. Exec's output is a
// temporary file rather than a pipe, so no descendant can hold Wait open.
const cancelGrace = time.Second

// setupProcessGroup makes a cancelled context kill the whole process tree, not
// just the shell.
//
// Measured, and the reason this exists: `exec.CommandContext` kills only the
// process it started. Whether that is enough depends entirely on which shell
// /bin/sh happens to be.
//
//	host        /bin/sh -> bash   `sh -c 'sleep 5'` is optimised into an exec,
//	                              so the process Go started IS sleep and
//	                              killing it works
//	container   /bin/sh -> dash   dash FORKS for the command, so the kill
//	                              reaps dash while sleep survives, keeps the
//	                              stdout/stderr pipe open, and CombinedOutput
//	                              blocks in Wait for the full five seconds
//
// dash is /bin/sh on Debian and Ubuntu, so the broken case is the common one --
// including this project's own fleet-node-deb image. The cancellation test
// passed on the host for a reason that had nothing to do with the code being
// right, and a mutation removing CommandContext entirely was "caught" by that
// same accident.
//
// Setpgid puts the shell in a new process group; killing the negated pid
// signals every member, so a forked grandchild dies with its parent.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = cancelGrace
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid means "the process group led by this pid". The group id
		// equals the leader's pid because of Setpgid above.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
