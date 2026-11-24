package file

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// linkTarget is the resource's link target after Puppet's munging: an
// explicit string target wins (measured), and otherwise an ensure that is not
// one of its keywords is itself the target (type/file/ensure.rb:109-120).
// ensure => symlink is therefore a link to the relative path "symlink" (I-2).
func linkTarget(r catalog.Resource) (string, bool) {
	if t, ok := r.Parameters["target"].(string); ok {
		return t, true
	}
	if e, ok := r.Parameters["ensure"].(string); ok {
		switch e {
		case "present", "file", "directory", "absent", "false", "link":
			return "", false
		}
		return e, true
	}
	return "", false
}

// applyLink applies ensure => link, which ensure => <target> and a target
// with no ensure also mean.
//
// The current state is read as links says (type/file.rb:895-903). Under
// follow that is Stat, which never answers "link", so an existing link is
// replaced on every run (measured), and a dangling one reads as absent, which
// the symlink then fails on.
//
// On a path that is already a link ensure is in sync, and target and the
// metadata are ordinary properties (propertiesAfterEnsure). So they are on
// any existing path under replace => false (type/file/ensure.rb:163), which
// Apply keeps before it gets here (keptByReplaceFalse).
func applyLink(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, sel *selinuxPlan, bk *backup) ([]ral.Change, []ral.Warning, error) {
	cur, err := statFor(fsys, path, linksFollow(r))
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("lstat %s: %w", path, err)
	}
	if exists && cur.Mode()&os.ModeSymlink != 0 {
		return propertiesAfterEnsure(ctx, fsys, path, r, noop, cur, sel, bk)
	}

	was := "absent"
	if exists {
		was = ftype(cur)
	}
	ch := ral.Change{Property: "ensure", Was: was, Is: "link", Name: "link_created",
		Message: ensureChangeMessage(was, "link")}
	if noop {
		return []ral.Change{ch}, nil, nil
	}
	// ensure's sync calls remove_existing before the link block runs
	// (type/file/ensure.rb:186), so whatever is in the way is removed even
	// when there is no target to link to (measured), and a directory kept
	// for want of force is noticed here and again in mklink (I-3), and a
	// backup is made, or warned about, each time. Its failure is unwrapped,
	// as applyAbsent's is.
	if exists {
		pathWarnings, err := bk.existing(fsys, path, cur)
		ch.PathWarnings = pathWarnings
		if err == nil {
			_, ch.PathNotices, err = removeExisting(ctx, fsys, path, cur, forceTrue(r), wantedType(r, "link"))
		}
		if err != nil {
			ch.Err = err
			return []ral.Change{ch}, nil, nil
		}
	}
	target, ok := linkTarget(r)
	if !ok {
		// ensure.rb:102's fail raises a Puppet::Error, which property.rb:490
		// re-raises unwrapped.
		ch.Err = errors.New("Cannot create a symlink without a target")
		return []ral.Change{ch}, nil, nil
	}
	warnings, err := mklink(ctx, fsys, path, r, target, true, sel, bk, &ch)
	ch.Err = err
	return []ral.Change{ch}, warnings, nil
}

// applyTarget is the target property on a path that existed and whose ensure
// is in sync, whatever ensure is: measured, ensure => file over an existing
// file with a target replaces the file with the link (I-6), and the next run
// replaces the link with a file again. It returns nil when target is in sync
// or not declared.
//
// The current value is the link's readlink, or "notlink" for anything else
// (type/file/target.rb:73-84). replace => false puts it in sync when the path
// exists through any link (:66), so a dangling link is still retargeted
// (measured).
func applyTarget(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, cur os.FileInfo, sel *selinuxPlan, bk *backup) (*ral.Change, []ral.Warning, error) {
	want, ok := linkTarget(r)
	if !ok {
		return nil, nil, nil
	}
	was := "notlink"
	if cur.Mode()&os.ModeSymlink != 0 {
		l, err := fsys.Readlink(path)
		if err != nil {
			return nil, nil, err
		}
		was = l
	}
	if replaceFalse(r) {
		if _, err := fsys.Stat(path); err == nil {
			return nil, nil, nil
		}
	}
	if was == want {
		return nil, nil, nil
	}
	ch := ral.Change{Property: "target", Was: was, Is: want,
		Message: fmt.Sprintf("target changed '%s' to '%s'", was, want)}
	var warnings []ral.Warning
	if !noop {
		// target's value block runs through instance_eval, not a setter
		// method (property.rb:500-504), so nothing it raises is wrapped.
		warnings, ch.Err = mklink(ctx, fsys, path, r, want, false, sel, bk, &ch)
	}
	return &ch, warnings, nil
}

// mklink is type/file/target.rb:36-61: clear the path, refuse if anything is
// still there, link it to target and apply the metadata as property_fix does
// (type/file.rb:1112-1121), unreported. ensureSet is true when ensure's
// set_link called it, whose Errno failures property.rb:492 wraps; its
// Puppet::Errors -- the refusal and a failed removal -- are never wrapped.
//
// The removal reads the path as links says, and a failed read is treated as
// nothing to remove, as Puppet's stat rescues it (type/file.rb:906-917): the
// symlink then reports why the path is unusable.
//
// remove_existing's backup and its notices land on ch, the change mklink
// syncs: its warning and notice lines are appended, in the order they occur.
func mklink(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, target string, ensureSet bool, sel *selinuxPlan, bk *backup, ch *ral.Change) ([]ral.Warning, error) {
	wrap := func(err error) error {
		if ensureSet {
			return wrapEnsureError("link", r, err)
		}
		return err
	}
	// remove_existing's argument is ensure's should (:link) when ensure called
	// us and target's own value otherwise (type/file/target.rb:43).
	wanted := wantedType(r, "link")
	if !ensureSet {
		wanted = rubyvalue.RedactIf(ral.IsMarked(File{}, r, "target"), "%s", target)
	}
	if fi, err := statFor(fsys, path, linksFollow(r)); err == nil {
		pathWarnings, err := bk.existing(fsys, path, fi)
		ch.PathWarnings = append(ch.PathWarnings, pathWarnings...)
		if err != nil {
			return nil, err
		}
		_, notices, err := removeExisting(ctx, fsys, path, fi, forceTrue(r), wanted)
		ch.PathNotices = append(ch.PathNotices, notices...)
		if err != nil {
			return nil, err
		}
	}
	// Puppet::FileSystem.exist? follows a link (target.rb:45); only a
	// directory kept for want of force can still be here.
	if _, err := fsys.Stat(path); err == nil {
		return nil, errors.New("Could not remove existing file")
	}
	if err := fsys.Symlink(target, path); err != nil {
		// os.LinkError names the target; a marked one is taken out (D-78).
		if ral.IsMarked(File{}, r, "target") || ral.IsMarked(File{}, r, "ensure") {
			err = errors.New(strings.ReplaceAll(err.Error(), target, "[redacted]"))
		}
		return nil, wrap(err)
	}
	// Against a fresh read: under manage the new link keeps its mode
	// (type/file/mode.rb:133) and is lchown'ed; under follow its target is
	// chmod'ed and chown'ed through it (measured).
	fixed, warnings, err := applyMetadata(ctx, fsys, path, r, false, nil, sel)
	if err := propertyFixErr(fixed, err); err != nil {
		return warnings, wrap(err)
	}
	return warnings, nil
}

// propertiesAfterEnsure files target, then owner, group and mode, on a path
// that existed and whose ensure is in sync: the order Puppet's properties are
// declared in (type/file.rb:1127-1139). before is the path as it was read
// first, which the properties compare against.
//
// A target sync that left a link at the path puts mode in sync under links =>
// manage, because mode.rb:133 checks a fresh stat (measured: no mode event
// after "target changed 'notlink' to 'x'"). Under noop nothing changed and
// mode is compared as usual.
func propertiesAfterEnsure(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, before os.FileInfo, sel *selinuxPlan, bk *backup) ([]ral.Change, []ral.Warning, error) {
	var changes []ral.Change
	tch, warnings, err := applyTarget(ctx, fsys, path, r, noop, before, sel, bk)
	if err != nil {
		return nil, nil, err
	}
	if tch != nil {
		changes = append(changes, *tch)
		if _, hasMode := r.Parameters["mode"]; hasMode && !noop && !linksFollow(r) {
			if fi, err := fsys.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				r.Parameters = maps.Clone(r.Parameters)
				delete(r.Parameters, "mode")
			}
		}
	}
	metaChanges, metaWarnings, err := applyMetadata(ctx, fsys, path, r, noop, before, sel)
	return append(changes, metaChanges...), append(warnings, metaWarnings...), err
}
