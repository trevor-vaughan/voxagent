package asuser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The ops run as the key's user: in the helper child, or in-process when the
// agent is not root or already runs as that uid (spec §3.4). They log nothing;
// the caller renders every message from the Result.

// Mkdir is kp:81-84: when dir does not exist (os.Stat, following links; any
// failure counts as absent, as Puppet::FileSystem.exist?), mkdir(dir, 0700),
// subject to the umask. ok when it existed, mkdir-created, or mkdir <errno>.
func Mkdir(dir string) Result {
	if _, err := os.Stat(dir); err == nil {
		return Result{Step: StepOK}
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Result{Step: StepMkdir, Errno: errnoOf(err)}
	}
	return Result{Step: StepMkdirCreated}
}

// Write is the flat filetype's write (uft:123-132) of text to path under
// encoding, with no backup (the parent backed the target up before the drop,
// kp:72-76) and relabel as its last act (Ruby's set_selinux_default_context,
// uft:131). It reports the failed step and errno (parsedfile.WriteFailure),
// or relabel <errno> when only the relabel failed: the file is written, and
// the failure is the parent's warning.
//
// The one failure without a step is the encoding check
// (parsedfile.FlatEncodeError), which the caller makes before Write; Write
// returns that error, and any error without a step, and no Result. An
// errno-less step failure is reported as EIO (errnoOf).
func Write(ctx context.Context, path, encoding, text string, relabel func(ctx context.Context, path string) (string, error)) (Result, error) {
	ctx = runlog.WithLogger(ctx, runlog.New("notice", nil, nil))
	if err := parsedfile.FlatEncodeError(path, encoding, text); err != nil {
		return Result{}, err
	}
	var relabelErr error
	hook := func(ctx context.Context, path string) error {
		_, relabelErr = relabel(ctx, path)
		return nil
	}
	nop := func(context.Context, string) error { return nil }
	flat, err := parsedfile.NewFlat(path, 0, nop, hook, encoding)
	if err != nil {
		return Result{}, err
	}
	if err := flat.Write(ctx, text); err != nil {
		var wf *parsedfile.WriteFailure
		if !errors.As(err, &wf) {
			// Only the encoding check, made above, fails without a step.
			return Result{}, err
		}
		return Result{Step: Step(wf.Step), Errno: errnoOf(wf.Errno)}, nil
	}
	if relabelErr != nil {
		return Result{Step: StepRelabel, Errno: errnoOf(relabelErr)}, nil
	}
	return Result{Step: StepOK}, nil
}

// Chmod is kp:87: chmod(path, 0600), following a link.
func Chmod(path string) Result {
	if err := os.Chmod(path, 0o600); err != nil {
		return Result{Step: StepChmod, Errno: errnoOf(err)}
	}
	return Result{Step: StepOK}
}

// tempDirFallbacks are the candidates Dir.tmpdir tries after TMPDIR, TMP and
// TEMP: Etc.systmpdir, which is /tmp on Linux, then /tmp (the same, so tried
// once), then ".". Tests replace them.
var tempDirFallbacks = []string{"/tmp", "."}

// TempDir is Ruby 3.2's Dir.tmpdir (tmpdir.rb:21-39) for the process's
// effective user, which the helper's write op uses for its temp file as
// Ruby's Tempfile.new does inside asuser. The candidates are TMPDIR, TMP and
// TEMP (each when set and not empty), then tempDirFallbacks; each is made
// absolute (File.expand_path), and the first one stat (following links)
// finds to be a directory, writable by File::Stat#writable? (rb_stat_w: mode
// bits only, euid 0 always; owner bits for the owner, group bits for a member
// of its group, else other bits) and not world-writable without the sticky
// bit, is the result. None is "could not find a temporary directory".
// Ruby's warning on stderr for each candidate it passes over is not printed:
// the helper's stderr is the null device (D-101).
func TempDir() (string, error) {
	var candidates []string
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if v := os.Getenv(name); v != "" {
			candidates = append(candidates, v)
		}
	}
	for _, dir := range append(candidates, tempDirFallbacks...) {
		// Ruby raises where expand_path cannot read the working directory, and
		// expands a leading "~"; here the first is passed over and the second
		// is a relative name (D-101).
		dir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !statWritable(st) {
			continue
		}
		if fi.Mode()&0o002 != 0 && fi.Mode()&os.ModeSticky == 0 {
			continue
		}
		return dir, nil
	}
	return "", errors.New("could not find a temporary directory")
}

// statWritable is File::Stat#writable? (rb_stat_w): true for euid 0, else the
// owner's write bit when euid owns the file, the group's when the file's group
// is the real or effective gid or a supplementary group (rb_group_member),
// else the other write bit.
func statWritable(st *syscall.Stat_t) bool {
	if os.Geteuid() == 0 {
		return true
	}
	if int(st.Uid) == os.Geteuid() {
		return st.Mode&syscall.S_IWUSR != 0
	}
	gid := int(st.Gid)
	groups, err := os.Getgroups()
	if gid == os.Getgid() || gid == os.Getegid() || err == nil && slices.Contains(groups, gid) {
		return st.Mode&syscall.S_IWGRP != 0
	}
	return st.Mode&syscall.S_IWOTH != 0
}
