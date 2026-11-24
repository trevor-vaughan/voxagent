// Package asuser is the privilege-dropped half of ssh_authorized_key's flush
// (spec §3.4): the three file acts Ruby performs inside SUIDManager.asuser
// (kp:80-88), the closed result they report, and the starter that runs them
// as the key's user in a child "voxagent asuser" process.
//
// The child runs as a user who may ptrace it, so its output is untrusted
// input to root. It reports only a Result, "<step> <errno>", which the parent
// parses strictly and renders into Ruby's texts itself; no byte of the child's
// output reaches a log, an error or the report.
package asuser

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
)

// Step is where an op ended. The set is closed: ParseResult refuses any other.
type Step string

const (
	StepOK           Step = "ok"            // nothing failed (mkdir: the directory existed)
	StepMkdirCreated Step = "mkdir-created" // mkdir created the directory
	StepMkdir        Step = "mkdir"         // mkdir failed
	StepTempfile     Step = "tempfile"      // write: the temp file failed
	StepOpen         Step = "open"          // write: opening the target failed
	StepCopy         Step = "copy"          // write: copying onto the target failed
	StepClose        Step = "close"         // write: closing the target failed
	StepRelabel      Step = "relabel"       // write succeeded, the SELinux relabel failed
	StepChmod        Step = "chmod"         // chmod failed
	StepTmpdir       Step = "tmpdir"        // write: no temporary directory (TempDir)
)

// maxResult is the longest result line ParseResult accepts, newline included.
const maxResult = 64

// maxErrno bounds a reported errno: Linux errnos are below 4096 (the kernel's
// MAX_ERRNO is 4095).
const maxErrno = 4095

// Result is an op's outcome: its Step and, for a failure, the errno (1-4095);
// 0 for ok, mkdir-created and tmpdir (Ruby's failure there has no errno).
type Result struct {
	Step  Step
	Errno syscall.Errno
}

// String is the result line without its newline: "<step> <errno>".
func (r Result) String() string {
	return string(r.Step) + " " + strconv.FormatUint(uint64(r.Errno), 10)
}

// ExitCode is the helper's exit status for r: 0 when the op succeeded (a
// relabel failure is only a warning), 1 when it failed.
func (r Result) ExitCode() int {
	switch r.Step {
	case StepOK, StepMkdirCreated, StepRelabel:
		return 0
	}
	return 1
}

// valid says whether r is a result an op can produce: a known step with no
// errno for ok, mkdir-created and tmpdir, an errno in 1-4095 for every other.
func (r Result) valid() bool {
	switch r.Step {
	case StepOK, StepMkdirCreated, StepTmpdir:
		return r.Errno == 0
	case StepMkdir, StepTempfile, StepOpen, StepCopy, StepClose, StepRelabel, StepChmod:
		return r.Errno >= 1 && r.Errno <= maxErrno
	}
	return false
}

// ParseResult reads the helper's whole stdout and its exit status. It accepts
// exactly one line, at most 64 bytes with its newline, in the canonical form
// String gives (no sign, no leading zero, single spaces), of a valid Result
// whose exit status agrees (ExitCode). Anything else is refused.
func ParseResult(stdout []byte, exit int) (Result, bool) {
	if len(stdout) > maxResult {
		return Result{}, false
	}
	line, ok := strings.CutSuffix(string(stdout), "\n")
	if !ok {
		return Result{}, false
	}
	step, num, ok := strings.Cut(line, " ")
	if !ok {
		return Result{}, false
	}
	n, err := strconv.ParseUint(num, 10, 16)
	if err != nil {
		return Result{}, false
	}
	r := Result{Step: Step(step), Errno: syscall.Errno(n)}
	if !r.valid() || r.String() != line || r.ExitCode() != exit {
		return Result{}, false
	}
	return r, true
}

// errnoOf is err's errno, as a failure step reports it. An error without one
// in 1-4095, which none of the os calls the ops make returns, is EIO, so the
// result stays in the closed protocol.
func errnoOf(err error) syscall.Errno {
	var errno syscall.Errno
	if errors.As(err, &errno) && errno >= 1 && errno <= maxErrno {
		return errno
	}
	return syscall.EIO
}
