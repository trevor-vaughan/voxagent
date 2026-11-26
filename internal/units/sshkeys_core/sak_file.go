package sshkeyscore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The ssh_authorized_key filetype and the privilege drop of its flush (spec
// §3.4, §6.3 items 4-5). Root only reads a user's file (prefetch, backup);
// with drop_privileges true every write, mkdir and chmod runs as the key's
// user, in the asuser helper, or in-process when the agent is not root or
// already runs as the user's uid. The helper's output is untrusted: the
// parent accepts only the closed asuser.Result each op can report and
// renders every message itself.

// sakDrop is the privilege drop of the flush in progress, shared by the run's
// filetypes: cred is the identity the ops run as, nil in-process; user is
// how the messages show the user ([redacted] when marked, D-78). Paths are
// shown as Ruby shows them, raw: they are on the helper's argv anyway.
type sakDrop struct {
	start          asuser.Starter
	defaultContext func(ctx context.Context, path string) (string, bool)

	cred *syscall.Credential
	user string
}

// helper runs one op in the helper as d.cred. steps are the results the op can
// report; any other, like output ParseResult refuses, is the invalid result.
// A start failure or the context's error is returned as it is.
func (d *sakDrop) helper(ctx context.Context, args []string, stdin []byte, steps ...asuser.Step) (asuser.Result, error) {
	r, err := d.start.Start(ctx, d.cred, args, stdin)
	if errors.Is(err, asuser.ErrInvalidResult) || err == nil && !slices.Contains(steps, r.Step) {
		return asuser.Result{}, fmt.Errorf("asuser helper for %s returned an %w", args[len(args)-1], asuser.ErrInvalidResult)
	}
	return r, err
}

// mkdir is kp:81-84 on dir. The op, as the user, checks whether dir exists;
// when it tried to create it, debug "Creating <dir> as <user>" is logged
// first, then a failure is "<errno> @ dir_s_mkdir - <dir>".
func (d *sakDrop) mkdir(ctx context.Context, dir string) error {
	r := asuser.Result{}
	if d.cred == nil {
		r = asuser.Mkdir(dir)
	} else {
		var err error
		if r, err = d.helper(ctx, []string{"mkdir", dir}, nil, asuser.StepOK, asuser.StepMkdirCreated, asuser.StepMkdir); err != nil {
			return err
		}
	}
	if r.Step == asuser.StepOK {
		return nil
	}
	runlog.Debugf(ctx, "Puppet", "Creating %s as %s", dir, d.user)
	if r.Step == asuser.StepMkdir {
		return errors.New(rubyvalue.Errno(r.Errno, "dir_s_mkdir", dir))
	}
	return nil
}

// chmod is kp:87, File.chmod(0600, path): a failure is "<errno> @
// apply2files - <path>".
func (d *sakDrop) chmod(ctx context.Context, path string) error {
	r := asuser.Result{}
	if d.cred == nil {
		r = asuser.Chmod(path)
	} else {
		var err error
		if r, err = d.helper(ctx, []string{"chmod", path}, nil, asuser.StepOK, asuser.StepChmod); err != nil {
			return err
		}
	}
	if r.Step == asuser.StepChmod {
		return errors.New(rubyvalue.Errno(r.Errno, "apply2files", path))
	}
	return nil
}

// sakFile is the filetype of one target (spec §6.3): flat's Read and Backup
// in the root parent, and a Write that is the helper's write op while the
// flush has set a credential, else flat's in-process.
type sakFile struct {
	flat     parsedfile.FileType
	path     string
	encoding string
	drop     *sakDrop
}

// Read implements parsedfile.FileType.
func (f *sakFile) Read(ctx context.Context) (string, error) { return f.flat.Read(ctx) }

// Backup implements parsedfile.Backuper: flat's, in the root parent, before
// any drop (kp:72-76).
func (f *sakFile) Backup(ctx context.Context) error { return f.flat.(parsedfile.Backuper).Backup(ctx) }

// Write implements parsedfile.FileType. Through the helper it renders what
// flat's Write would log and return: the encoding failure (checked here,
// before the helper starts, as its result has no step for it) and a failed
// step are logged err Puppet and returned; a relabel failure is the warning
// "Failed to set SELinux context <c> on <path>", <c> the parent's lookup
// (none when it finds no context), and the write succeeds.
func (f *sakFile) Write(ctx context.Context, text string) error {
	d := f.drop
	if d.cred == nil {
		return f.flat.Write(ctx, text)
	}
	logged := func(err error) error {
		runlog.From(ctx).Log("err", "Puppet", err.Error())
		return err
	}
	if err := parsedfile.FlatEncodeError(f.path, f.encoding, text); err != nil {
		return logged(err)
	}
	r, err := d.helper(ctx, []string{"write", "--encoding", f.encoding, f.path}, []byte(text),
		asuser.StepOK, asuser.StepRelabel, asuser.StepTempfile, asuser.StepOpen, asuser.StepCopy, asuser.StepClose,
		asuser.StepTmpdir)
	if err != nil {
		return err
	}
	switch r.Step {
	case asuser.StepOK:
		return nil
	case asuser.StepRelabel:
		// Without a default context Ruby makes no relabel attempt, so warns of
		// none (sel:166-176).
		if c, ok := d.defaultContext(ctx, f.path); ok {
			runlog.From(ctx).Log("warning", "Puppet", fmt.Sprintf("Failed to set SELinux context %s on %s", c, f.path))
		}
		return nil
	}
	return logged(parsedfile.FlatWriteError(f.path, parsedfile.WriteFailure{Step: string(r.Step), Errno: r.Errno}))
}

// trustedPath is trusted_path (kp:53-67), whether drop_privileges false may
// write target as the agent. False, silently, unless target's parent exists
// (Puppet::FileSystem.dir_exist? is exist?(dirname), file_system.rb:72).
// Then each path from dirname(target) upward whose own parent is not / is
// checked, a symlink resolved first: one not owned by euid, or group- or
// world-writable, is untrusted (debug "Path untrusted, will attempt to write
// as the target user"). Passing the walk logs debug "Path trusted, writing
// the file as the current user" and the result is that Puppet.debug call's
// value: true only with debug logging on (spec §8 item 9).
//
// Ruby raises where a stat or realpath fails, and loops forever on a relative
// target (the dirname of "." is "."): here both are untrusted, silently
// (D-103).
func trustedPath(ctx context.Context, target string, euid int) bool {
	path := rubyDirname(target)
	if _, err := os.Stat(path); err != nil || !filepath.IsAbs(path) {
		return false
	}
	for !isRoot(rubyDirname(path)) {
		if fi, err := os.Lstat(path); err != nil {
			return false
		} else if fi.Mode()&os.ModeSymlink != 0 {
			if path, err = filepath.EvalSymlinks(path); err != nil {
				return false
			}
		}
		fi, err := os.Stat(path)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return false
		}
		if int64(st.Uid) != int64(euid) || fi.Mode().Perm()&0o022 != 0 {
			runlog.Debugf(ctx, "Puppet", "Path untrusted, will attempt to write as the target user")
			return false
		}
		path = rubyDirname(path)
	}
	runlog.Debugf(ctx, "Puppet", "Path trusted, writing the file as the current user")
	return runlog.From(ctx).Enabled("debug")
}

// isRoot is Pathname#root?: the path is one or more slashes.
func isRoot(path string) bool { return path != "" && strings.Trim(path, "/") == "" }

// rubyDirname is File.dirname: trailing slashes are dropped, then the last
// component and the slashes before it; "." when nothing is left, "/" for an
// all-slash path. Unlike filepath.Dir it does not clean the result.
func rubyDirname(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		if path == "" {
			return "."
		}
		return "/"
	}
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return "."
	}
	if dir := strings.TrimRight(trimmed[:i], "/"); dir != "" {
		return dir
	}
	return "/"
}
