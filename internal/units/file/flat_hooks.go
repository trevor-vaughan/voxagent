package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/voxagent/voxagent/internal/runlog"
)

// The hooks parsedfile.NewFlat takes from its consumer: a flat filetype backs
// its target up into the local clientbucket before it writes it, and relabels
// it after (uft:76-78,102-104,131). They are the file resource's own backup
// and SELinux code, so a flat target is bucketed and labelled as a file
// resource's would be.

// BucketBackup backs a flat target up into the clientbucket under vardir, as
// Ruby's mkdefaultbucket.bucket.backup does (uft:76-78, 102-104), logging
// what its dipper logs: info "Computing checksum on file <path>", then, when
// the content is saved, debug "Locked '<paths>'" and "Unlocked '<paths>'".
// All are source Puppet. A missing target, or a link to nothing, is no backup
// and logs nothing. The path recorded in the entry is the resolved one, so a
// link's target is what the bucket names (M5); the checksum line shows the
// path as given.
func BucketBackup(vardir string) func(ctx context.Context, path string) error {
	return func(ctx context.Context, path string) error {
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		logger := runlog.From(ctx)
		logger.Log("info", "Puppet", "Computing checksum on file "+path)
		b := &backup{value: defaultBucket, vardir: vardir,
			log: func(level, message string) { logger.Log(level, "Puppet", message) }}
		err = b.toBucket(osFS{}, resolved)
		for _, w := range b.warnings {
			logger.Log("warning", w.Source, w.Message)
		}
		return err
	}
}

// DefaultContext is the policy's default SELinux context for path, as
// set_selinux_default_context finds it (sel:166-176, get_selinux_default_context
// sel:57-70): the mode looked up is the path's lstat st_mode, 0 when it is not
// there or unreadable. It is false without SELinux support, on a filesystem
// without labels, and where the policy has no default or the lookup fails
// (matchpathcon's -1 is no default).
func DefaultContext(ctx context.Context, path string) (string, bool) {
	se := defaultSELinux
	if !se.enabled() || !se.labelSupported(path) {
		return "", false
	}
	var mode uint32
	if fi, err := os.Lstat(path); err == nil {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return "", false
		}
		mode = st.Mode
	} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
		return "", false
	}
	def, ok, err := se.defaultFor(ctx, path, mode)
	if err != nil || !ok {
		return "", false
	}
	return def.String(), true
}

// Relabel is set_selinux_default_context (sel:166-176): when the default
// context differs from the one path has, it is set, and returned, even when
// setting it failed (err). It returns "", nil when SELinux gives no default
// or path already has it. It never logs: the asuser child calls it, and the
// parent renders what the child reports.
func Relabel(ctx context.Context, path string) (seContext string, err error) {
	want, ok := DefaultContext(ctx, path)
	if !ok {
		return "", nil
	}
	se := defaultSELinux
	if cur, _ := se.current(osFS{}, path); cur == want {
		return "", nil
	}
	return want, se.set(osFS{}, path, want)
}

// RelabelHook is Relabel for a flat filetype in the root process: a failure
// is Puppet's warning "Failed to set SELinux context <c> on <f>" (sel:152-156,
// source Puppet) and no error, so the flush succeeds.
func RelabelHook(ctx context.Context, path string) error {
	if c, err := Relabel(ctx, path); err != nil {
		runlog.From(ctx).Log("warning", "Puppet", fmt.Sprintf("Failed to set SELinux context %s on %s", c, path))
	}
	return nil
}
