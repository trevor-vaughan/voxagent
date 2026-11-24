package ral

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ErrStagingBroken is a staged exec start after which the agent's working
// directory could not be restored, or could not be saved first. The run must
// stop: every relative path the agent resolves afterwards would be wrong
// (spec §4.2 item 5). Errors wrap it with their cause.
var ErrStagingBroken = errors.New("cannot restore the agent's working directory")

// identity is the credential an exec's child drops to; nil cred is none.
type identity struct{ cred *syscall.Credential }

// invalidIdentityError is convert_xid's failure (suidmanager.rb:150). Ruby
// raises it in the child, whose rescue logs it to the command's output and
// exits 1 (util/execution.rb:393-395, M6, M18).
type invalidIdentityError struct{ kind, name string }

func (e invalidIdentityError) Error() string { return "Invalid " + e.kind + ": " + e.name }

// resolveIdentity turns exec's user and group into the child's credential
// (spec §4.1), with the os/user lookups File uses. A string of digits or an
// Integer is an id that must exist; anything else is a name. The group is
// resolved first, as change_privileges changes the group first.
//
// marked says the catalog marks "user" or "group" Sensitive. No error then
// carries its value (D-78), nor the lookup's own error, whose text names it
// (cgo's "user: lookup username <name>: <errno>"), as lookupUID does not wrap
// its lookup error either.
func resolveIdentity(usr, group any, marked func(kind string) bool) (identity, error) {
	if usr == nil && group == nil {
		return identity{}, nil
	}
	var gid uint32
	if group != nil {
		key, byID, err := identityKey("group", group, marked("group"))
		if err != nil {
			return identity{}, err
		}
		var g *user.Group
		if byID {
			g, err = user.LookupGroupId(key)
		} else {
			g, err = user.LookupGroup(key)
		}
		if err != nil {
			return identity{}, unknownIdentity("group", group, marked("group"), err)
		}
		if gid, err = parseXID(g.Gid); err != nil {
			return identity{}, err
		}
	}
	// The agent's uid comes from the kernel's 32-bit uid_t.
	agentUID := uint32(os.Getuid())
	if usr == nil {
		// M15: the uid stays and so do the agent's supplementary groups.
		return identity{&syscall.Credential{Uid: agentUID, Gid: gid, NoSetGroups: true}}, nil
	}
	key, byID, err := identityKey("user", usr, marked("user"))
	if err != nil {
		return identity{}, err
	}
	var u *user.User
	if byID {
		u, err = user.LookupId(key)
	} else {
		u, err = user.Lookup(key)
	}
	if err != nil {
		return identity{}, unknownIdentity("user", usr, marked("user"), err)
	}
	uid, err := parseXID(u.Uid)
	if err != nil {
		return identity{}, err
	}
	if group == nil {
		if gid, err = parseXID(u.Gid); err != nil {
			return identity{}, err
		}
	}
	if uid == agentUID {
		// change_user returns at once for the process's own uid
		// (suidmanager.rb:121), so no initgroups: a non-root agent could not.
		return identity{&syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true}}, nil
	}
	// initgroups(name, primary gid) (suidmanager.rb:161-164).
	ids, err := u.GroupIds()
	if err != nil {
		if marked("user") {
			return identity{}, errors.New("Could not read the supplementary groups of user [redacted]")
		}
		return identity{}, fmt.Errorf("Could not read the supplementary groups of user %s: %w", u.Username, err)
	}
	groups := make([]uint32, len(ids))
	for i, id := range ids {
		if groups[i], err = parseXID(id); err != nil {
			return identity{}, err
		}
	}
	return identity{&syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}, nil
}

// identityKey is the lookup key for a user or group value and whether it is
// an id. An id beyond 32 bits names no entry, so it fails as an unknown one.
func identityKey(kind string, v any, redact bool) (key string, byID bool, err error) {
	switch v := v.(type) {
	case string:
		if v == "" || strings.Trim(v, "0123456789") != "" {
			return v, false, nil
		}
		if _, err := strconv.ParseUint(v, 10, 32); err != nil {
			return "", false, invalidIdentityError{kind, redactIf(redact, "%s", v)}
		}
		return v, true, nil
	case float64:
		// Kernel#Integer truncates a Float (posix.rb:174).
		n := math.Trunc(v)
		if n < 0 || n > math.MaxUint32 {
			return "", false, invalidIdentityError{kind, redactIf(redact, "%s", rubyInspectValue(v))}
		}
		return strconv.FormatUint(uint64(n), 10), true, nil
	default:
		return "", false, fmt.Errorf("%s is %T, not a string or an integer", kind, v)
	}
}

// unknownIdentity is the invalid-identity error for a lookup that found no
// entry; any other lookup failure is wrapped, unless redact withholds it.
func unknownIdentity(kind string, v any, redact bool, err error) error {
	var (
		noUser    user.UnknownUserError
		noUID     user.UnknownUserIdError
		noGroup   user.UnknownGroupError
		noGroupID user.UnknownGroupIdError
	)
	if !errors.As(err, &noUser) && !errors.As(err, &noUID) && !errors.As(err, &noGroup) && !errors.As(err, &noGroupID) {
		if redact {
			return fmt.Errorf("Could not look up %s [redacted]", kind)
		}
		return fmt.Errorf("Could not look up %s: %w", kind, err)
	}
	name, ok := v.(string)
	if !ok {
		name = rubyInspectValue(v)
	}
	return invalidIdentityError{kind, redactIf(redact, "%s", name)}
}

// parseXID reads an id from the user or group database. A malformed one is
// not quoted: it may belong to a marked user or group.
func parseXID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errors.New("the user or group database holds an id that is not a 32-bit number")
	}
	return uint32(n), nil
}

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
			fd, err := syscall.Open(".", oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
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
