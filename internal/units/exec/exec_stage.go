package exec

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"sync"
	"syscall"

	"github.com/voxagent/voxagent/internal/lib/puppetpath"
)

// ErrStagingBroken is a staged exec start after which the agent's working
// directory could not be restored, or could not be saved first. The run must
// stop: every relative path the agent resolves afterwards would be wrong
// (spec §4.2 item 5). Errors wrap it with their cause.
var ErrStagingBroken = errors.New("cannot restore the agent's working directory")

// identity is the credential an exec's child drops to; nil cred is none.
type identity struct{ cred *syscall.Credential }

// staging is what an exec's child must start with besides its command: cwd,
// umask and identity (spec §4.2). Go's child applies Credential before Dir
// (syscall/exec_linux.go), but Ruby enters cwd as root and only then drops
// privileges (M17), so the agent enters cwd itself and the child inherits it.
//
// This changes process-wide state, which is safe only because the agent
// applies serially: the one goroutine internal/ral and internal/transaction
// start is runExec's waiter, which only calls Wait and so never resolves a
// path or creates a file while the cwd or umask is staged
// (TestOnlyTheExecWaiterStartsAGoroutine).
type staging struct {
	cwd   string
	umask *int
	id    identity
}

var (
	// stageMu is held from the chdir through Start to the restore.
	stageMu sync.Mutex
	// stageOrigin is the agent's own cwd, opened at the first staged cwd and
	// kept for the life of the process, so a restore never follows a path.
	stageOrigin = -1
)

// restoreCwd returns the agent to its origin; a variable so a test can fail it.
var restoreCwd = func(fd int) error { return syscall.Fchdir(fd) }

func (s staging) needed() bool { return s.cwd != "" || s.umask != nil || s.id.cred != nil }

// start is runExec's start for a staged command. A cwd that cannot be entered
// is returned as the *fs.PathError Op "fork/exec" a failed start is, with
// nothing started and nothing changed. A failure to save or restore the agent's cwd wraps
// ErrStagingBroken; after a restore failure the child has started.
func (s staging) start(cmd *exec.Cmd) error {
	stageMu.Lock()
	defer stageMu.Unlock()
	cmd.SysProcAttr.Credential = s.id.cred
	if s.cwd != "" {
		if stageOrigin < 0 {
			// O_PATH needs no read permission, so a search-only cwd is saved.
			fd, err := syscall.Open(".", puppetpath.OPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("%w: open it: %v", ErrStagingBroken, err)
			}
			stageOrigin = fd
		}
		// chdir(2), as Ruby's Dir.chdir: search permission only, symlinks
		// followed (I-30). Its failure reads as it did when Go's child made
		// the chdir, which also keeps a Sensitive cwd out of the text.
		if err := syscall.Chdir(s.cwd); err != nil {
			return &fs.PathError{Op: "fork/exec", Path: cmd.Path, Err: err}
		}
	}
	var startErr error
	if s.umask != nil {
		previous := syscall.Umask(*s.umask)
		startErr = cmd.Start()
		syscall.Umask(previous)
	} else {
		startErr = cmd.Start()
	}
	if s.cwd != "" {
		if err := restoreCwd(stageOrigin); err != nil {
			return fmt.Errorf("%w: %v", ErrStagingBroken, err)
		}
	}
	return startErr
}
