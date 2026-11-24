package asuser

import (
	"context"
	"errors"
	"os"

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
