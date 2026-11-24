package ral

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// applyOne applies r and requires exactly one change and no resource-level
// error, the shape of every ensure => link and target event.
func applyOne(t *testing.T, params map[string]any, path string, noop bool) Change {
	t.Helper()
	changes, _, err := (File{}).Apply(context.Background(), fileRes(path, params), noop)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = %+v, %v; want one change", changes, err)
	}
	return changes[0]
}

func mustSymlink(t *testing.T, target, l string) {
	t.Helper()
	if err := os.Symlink(target, l); err != nil {
		t.Fatal(err)
	}
}

func wantReadlink(t *testing.T, l, want string) {
	t.Helper()
	if got, err := os.Readlink(l); err != nil || got != want {
		t.Errorf("readlink %s = %q, %v; want %q", l, got, err, want)
	}
}

func wantChange(t *testing.T, ch Change, property, name, was, is, message string) {
	t.Helper()
	if ch.Err != nil || ch.Property != property || ch.Name != name || ch.Was != was || ch.Is != is || ch.Message != message {
		t.Errorf("change = %+v; want %s %s %q -> %q %q", ch, property, name, was, is, message)
	}
}

// Measured on Puppet 8.10.0: link_created | absent | link | created.
func TestLinkCreatesAndReportsLinkCreated(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x"}, l, false)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created")
	wantReadlink(t, l, "x")
}

// Measured: link_created | file | link | ensure changed 'file' to 'link'.
func TestLinkOverAFile(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	mustWrite(t, l, "x\n")
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x"}, l, false)
	wantChange(t, ch, "ensure", "link_created", "file", "link", "ensure changed 'file' to 'link'")
	wantReadlink(t, l, "x")
}

// Measured: link_created | directory | link | ensure changed 'directory' to
// 'link', the tree removed.
func TestLinkOverADirectoryWithForce(t *testing.T) {
	d := filepath.Join(t.TempDir(), "d")
	mustMkdir(t, d)
	mustWrite(t, filepath.Join(d, "f"), "f")
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x", "force": true}, d, false)
	wantChange(t, ch, "ensure", "link_created", "directory", "link", "ensure changed 'directory' to 'link'")
	wantReadlink(t, d, "x")
}

// Measured: the failure is "Could not remove existing file", unwrapped, after
// the notice twice (I-3): ensure's sync and mklink each call remove_existing
// (type/file/ensure.rb:186, target.rb:43).
func TestLinkOverADirectoryWithoutForceFails(t *testing.T) {
	d := filepath.Join(t.TempDir(), "d")
	mustMkdir(t, d)
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x"}, d, false)
	notice := "Not removing directory; use 'force' to override"
	if ch.Err == nil || ch.Err.Error() != "Could not remove existing file" || ch.Was != "directory" ||
		ch.Name != "link_created" || !slices.Equal(ch.PathNotices, []string{notice, notice}) {
		t.Errorf("change = %+v", ch)
	}
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		t.Errorf("the directory must stay: %v", err)
	}
}

// Measured: an absent path fails with Puppet's own sentence, unwrapped. Over a
// file the file is removed first (ensure.rb:186 runs before the link block),
// and over a directory the notice is logged once.
func TestLinkWithoutATargetFails(t *testing.T) {
	dir := t.TempDir()
	const msg = "Cannot create a symlink without a target"
	l := filepath.Join(dir, "l")
	ch := applyOne(t, map[string]any{"ensure": "link"}, l, false)
	if ch.Err == nil || ch.Err.Error() != msg || ch.Was != "absent" || ch.Is != "link" || ch.Name != "link_created" {
		t.Errorf("absent: change = %+v", ch)
	}

	f := filepath.Join(dir, "f")
	mustWrite(t, f, "x\n")
	ch = applyOne(t, map[string]any{"ensure": "link"}, f, false)
	if ch.Err == nil || ch.Err.Error() != msg || ch.Was != "file" {
		t.Errorf("file: change = %+v", ch)
	}
	if _, err := os.Lstat(f); !os.IsNotExist(err) {
		t.Errorf("the file must be removed before the failure, as Puppet does: %v", err)
	}

	d := filepath.Join(dir, "d")
	mustMkdir(t, d)
	ch = applyOne(t, map[string]any{"ensure": "link"}, d, false)
	if ch.Err == nil || ch.Err.Error() != msg || ch.Was != "directory" ||
		!slices.Equal(ch.PathNotices, []string{"Not removing directory; use 'force' to override"}) {
		t.Errorf("directory: change = %+v", ch)
	}
}

// Measured: an existing link with no target is in sync.
func TestLinkWithoutATargetLeavesALink(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	mustSymlink(t, "y", l)
	changes, _, err := (File{}).Apply(context.Background(), fileRes(l, map[string]any{"ensure": "link"}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("Apply = %+v, %v; want nothing", changes, err)
	}
}

// Measured: target_changed | old | new | target changed 'old' to 'new'.
func TestLinkRetargets(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	mustSymlink(t, "old", l)
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "new"}, l, false)
	wantChange(t, ch, "target", "", "old", "new", "target changed 'old' to 'new'")
	wantReadlink(t, l, "new")

	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(l, map[string]any{"ensure": "link", "target": "new"}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("second Apply = %+v, %v; want nothing", changes, err)
	}
}

// Measured: an explicit target wins over ensure's own value.
func TestEnsureSymlinkWithTargetLinksToTheTarget(t *testing.T) {
	dir := t.TempDir()
	l := filepath.Join(dir, "l")
	real := filepath.Join(dir, "real")
	ch := applyOne(t, map[string]any{"ensure": "symlink", "target": real}, l, false)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created")
	wantReadlink(t, l, real)
}

// Measured (I-2): ensure => symlink is a link to the relative path symlink.
func TestEnsureSymlinkWithoutTargetLinksToTheWordSymlink(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	ch := applyOne(t, map[string]any{"ensure": "symlink"}, l, false)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created")
	wantReadlink(t, l, "symlink")
}

// type/file.rb:561-563: a target with no ensure means ensure => link.
func TestTargetWithoutEnsureCreatesALink(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	ch := applyOne(t, map[string]any{"target": "x"}, l, false)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created")
	wantReadlink(t, l, "x")
}

// Measured: ensure => file creates a regular file and the target is not
// reached, because a created path syncs nothing after ensure.
func TestFileWithATargetIgnoresItWhenCreating(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	ch := applyOne(t, map[string]any{"ensure": "file", "target": "x"}, f, false)
	wantChange(t, ch, "ensure", "file_created", "absent", "file", "created")
	if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("want a regular file: %v", err)
	}
}

// Measured: over an existing file ensure => file is in sync, so target syncs
// as an ordinary property and replaces the file with a link (I-6). mode is
// then skipped: mode.rb:133 checks the fresh stat, which is the new link.
func TestFileWithATargetReplacesAnExistingFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	mustWrite(t, f, "x\n")
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	ch := applyOne(t, map[string]any{"ensure": "file", "target": "x", "mode": "0600"}, f, false)
	wantChange(t, ch, "target", "", "notlink", "x", "target changed 'notlink' to 'x'")
	wantReadlink(t, f, "x")

	// Measured: the next run replaces the link with a file again.
	ch = applyOne(t, map[string]any{"ensure": "file", "target": "x", "mode": "0600"}, f, false)
	wantChange(t, ch, "ensure", "file_created", "link", "file", "ensure changed 'link' to 'file'")
}

// Measured under noop: the target and the mode are both reported, against the
// file that is still there.
func TestFileWithATargetUnderNoopReportsTargetAndMode(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	mustWrite(t, f, "x\n")
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(f, map[string]any{"ensure": "file", "target": "x", "mode": "0600"}), true)
	if err != nil || len(changes) != 2 {
		t.Fatalf("Apply = %+v, %v; want target and mode", changes, err)
	}
	wantChange(t, changes[0], "target", "", "notlink", "x", "target changed 'notlink' to 'x' (noop)")
	wantChange(t, changes[1], "mode", "", "0644", "0600", "mode changed '0644' to '0600' (noop)")
	if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("noop changed the file: %v", err)
	}
}

// Measured: ensure => directory over a directory, with a target and no force,
// fails the target with one notice; with force the tree becomes the link.
func TestDirectoryWithATargetReplacesItOnlyWithForce(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "d")
	mustMkdir(t, d)
	ch := applyOne(t, map[string]any{"ensure": "directory", "target": "x"}, d, false)
	if ch.Property != "target" || ch.Was != "notlink" || ch.Is != "x" || ch.Err == nil ||
		ch.Err.Error() != "Could not remove existing file" ||
		!slices.Equal(ch.PathNotices, []string{"Not removing directory; use 'force' to override"}) {
		t.Errorf("no force: change = %+v", ch)
	}

	mustWrite(t, filepath.Join(d, "f"), "f")
	ch = applyOne(t, map[string]any{"ensure": "directory", "target": "x", "force": true}, d, false)
	wantChange(t, ch, "target", "", "notlink", "x", "target changed 'notlink' to 'x'")
	wantReadlink(t, d, "x")
}

// Measured: replace => false leaves a file where the link would go.
func TestLinkReplaceFalseLeavesAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	mustWrite(t, f, "x\n")
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(f, map[string]any{"ensure": "link", "target": "x", "replace": false}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("Apply = %+v, %v; want nothing", changes, err)
	}
	if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the file must stay: %v", err)
	}
}

// Measured: replace => false keeps a link whose target exists, but a dangling
// link is still retargeted -- target.rb:66 tests exist?, which follows it.
func TestLinkReplaceFalseRetargetsADanglingLink(t *testing.T) {
	dir := t.TempDir()
	l := filepath.Join(dir, "l")
	mustSymlink(t, "nowhere", l)
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x", "replace": false}, l, false)
	wantChange(t, ch, "target", "", "nowhere", "x", "target changed 'nowhere' to 'x'")
	wantReadlink(t, l, "x")

	mustWrite(t, filepath.Join(dir, "real"), "r")
	k := filepath.Join(dir, "k")
	mustSymlink(t, "real", k)
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(k, map[string]any{"ensure": "link", "target": "x", "replace": false}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("live link: Apply = %+v, %v; want nothing", changes, err)
	}
	wantReadlink(t, k, "real")
}

// Measured: an existing link with a mode is in sync, and its target's mode is
// untouched.
func TestLinkModeIsNotManaged(t *testing.T) {
	real, l := seedLink(t, "a\n", 0o644)
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(l, map[string]any{"ensure": "link", "target": real, "mode": "0600"}), false)
	if err != nil || len(changes) != 0 {
		t.Errorf("Apply = %+v, %v; want nothing", changes, err)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o644 {
		t.Errorf("target mode = %o; want 0644", fi.Mode().Perm())
	}
}

// Measured: under links => follow the mode applies through a created link, by
// property_fix, without an event of its own.
func TestLinkFollowCreateAppliesModeThroughTheLink(t *testing.T) {
	dir := t.TempDir()
	real, l := filepath.Join(dir, "real"), filepath.Join(dir, "l")
	mustWrite(t, real, "a\n")
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "real", "links": "follow", "mode": "0600"}, l, false)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created")
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %o; want 0600", fi.Mode().Perm())
	}
}

// Measured: under links => follow ensure reads the link's target, which is
// never a link, so an existing link is replaced on every run and its target
// is untouched.
func TestLinkFollowReplacesTheLinkEveryRun(t *testing.T) {
	dir := t.TempDir()
	l := filepath.Join(dir, "l")
	mustWrite(t, filepath.Join(dir, "x"), "x")
	mustSymlink(t, "x", l)
	for range 2 {
		ch := applyOne(t, map[string]any{"ensure": "link", "target": "x", "links": "follow"}, l, false)
		wantChange(t, ch, "ensure", "link_created", "file", "link", "ensure changed 'file' to 'link'")
		wantReadlink(t, l, "x")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "x")); string(b) != "x" {
		t.Errorf("the link's target changed: %q", b)
	}
}

// Measured: under noop, creating and retargeting are reported and nothing
// changes.
func TestLinkNoopTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	l := filepath.Join(dir, "l")
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "new"}, l, true)
	wantChange(t, ch, "ensure", "link_created", "absent", "link", "created (noop)")
	if _, err := os.Lstat(l); !os.IsNotExist(err) {
		t.Errorf("noop created the link: %v", err)
	}

	mustSymlink(t, "old", l)
	ch = applyOne(t, map[string]any{"ensure": "link", "target": "new"}, l, true)
	wantChange(t, ch, "target", "", "old", "new", "target changed 'old' to 'new' (noop)")
	wantReadlink(t, l, "old")

	f := filepath.Join(dir, "f")
	mustWrite(t, f, "x\n")
	ch = applyOne(t, map[string]any{"ensure": "link", "target": "new"}, f, true)
	wantChange(t, ch, "ensure", "link_created", "file", "link", "ensure changed 'file' to 'link' (noop)")
	if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("noop replaced the file: %v", err)
	}
}

// Measured: ensure => false is ensure => absent (type/file/ensure.rb:62).
func TestEnsureFalseRemoves(t *testing.T) {
	for _, v := range []any{false, "false"} {
		f := filepath.Join(t.TempDir(), "f")
		mustWrite(t, f, "x\n")
		ch := applyOne(t, map[string]any{"ensure": v}, f, false)
		wantChange(t, ch, "ensure", "file_removed", "file", "absent", "removed")
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("ensure => %#v left the file: %v", v, err)
		}
	}
}

// Measured: Puppet names all three creators whichever two collide, and a
// target that ensure supplies counts.
func TestFileValidateRejectsTargetWithContent(t *testing.T) {
	want := "Validation of File[/x/f] failed: You cannot specify more than one of content, source, target"
	for _, params := range []map[string]any{
		{"target": "y", "content": "c"},
		{"target": "y", "source": "/src"},
		{"ensure": "/y", "content": "c"},
	} {
		if err := (File{}).Validate(fileRes("/x/f", params)); err == nil || err.Error() != want {
			t.Errorf("Validate(%v) = %v; want %q", params, err, want)
		}
	}
	for _, params := range []map[string]any{
		{"target": "y"},
		{"ensure": "link", "content": "c"},
		{"ensure": "symlink"},
	} {
		if err := (File{}).Validate(fileRes("/x/f", params)); err != nil {
			t.Errorf("Validate(%v) = %v; want nil", params, err)
		}
	}
}

// Ours: Puppet mirrors a target's tree as links under recurse
// (type/file.rb:634-636, 680-700), which is not implemented, and fails a
// non-string ensure or target as a link it cannot create (measured: "FileSystem
// implementation expected Pathname, got: 'TrueClass'").
func TestFileValidateRefusesWhatLinksDoNotImplement(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"target": "y", "recurse": "true"}, "target with recurse => true is not implemented"},
		{map[string]any{"ensure": "link", "recurse": "true"}, "ensure => link with recurse => true is not implemented"},
		{map[string]any{"ensure": true}, "ensure => true (bool) is not implemented"},
		{map[string]any{"ensure": float64(42)}, "ensure => 42 (float64) is not implemented"},
		{map[string]any{"ensure": []any{"q", "r"}}, "ensure => [q r] ([]interface {}) is not implemented"},
		{map[string]any{"target": float64(1)}, "target => 1 (float64) is not implemented"},
	} {
		err := (File{}).Validate(fileRes("/x/f", tc.params))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "File[/x/f]: ") {
			t.Errorf("Validate(%v) = %v; want %q", tc.params, err, tc.want)
		}
	}
}

// type/file.rb:398-399: a link autorequires its managed target as well as its
// nearest managed ancestor.
func TestFileAutoRequiresAManagedLinkTarget(t *testing.T) {
	managed := func(refs ...string) func(string) bool {
		return func(ref string) bool { return slices.Contains(refs, ref) }
	}
	for _, tc := range []struct {
		params  map[string]any
		managed []string
		want    []string
	}{
		{map[string]any{"ensure": "link", "target": "/t/real/"}, []string{"File[/t]", "File[/t/real]"},
			[]string{"File[/t]", "File[/t/real]"}},
		{map[string]any{"ensure": "/t/real"}, []string{"File[/t/real]"}, []string{"File[/t/real]"}},
		{map[string]any{"ensure": "link", "target": "/t/real"}, nil, nil},
		{map[string]any{"ensure": "link", "target": "real"}, []string{"File[real]"}, nil},
		{map[string]any{"ensure": "link", "target": "/t"}, []string{"File[/t]"}, []string{"File[/t]"}},
	} {
		got := (File{}).AutoRequires(fileRes("/t/l", tc.params), managed(tc.managed...))
		if !slices.Equal(got, tc.want) {
			t.Errorf("AutoRequires(%v) = %v; want %v", tc.params, got, tc.want)
		}
	}
}

// Every ensure string is implemented now: a keyword or a link target.
func TestFileEnsureEveryStringIsSupported(t *testing.T) {
	for _, v := range []string{"file", "present", "directory", "absent", "false", "link", "symlink", "/etc/target", "true"} {
		if !(File{}).SupportsValue("ensure", v) {
			t.Errorf("SupportsValue(ensure, %q) = false", v)
		}
	}
	if _, ok := (File{}).Attrs()["target"]; !ok {
		t.Error("Attrs lacks target")
	}
}

// Measured (links => follow over a dangling link): ensure reads the path as
// absent and the symlink fails inside set_link, which property.rb:492 wraps.
func TestLinkFollowOverADanglingLinkWrapsTheSymlinkFailure(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	mustSymlink(t, "nowhere", l)
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x", "links": "follow"}, l, false)
	if ch.Was != "absent" || ch.Name != "link_created" || ch.Err == nil ||
		!strings.HasPrefix(ch.Err.Error(), "Could not set 'link' on ensure: ") ||
		!strings.Contains(ch.Err.Error(), "file exists") {
		t.Errorf("change = %+v", ch)
	}
	wantReadlink(t, l, "nowhere")
}

// linkFaultFS injects failures into an otherwise real filesystem.
type linkFaultFS struct {
	osFS
	symlinkErr error
	chmodErr   error
	chownErr   error
	// statAfterChown fails every Stat once a Chown has been attempted, so a
	// later property's retrieve fails after an earlier property's sync did.
	statAfterChown error
	chowned        *bool
}

func (f linkFaultFS) Symlink(oldname, newname string) error {
	if f.symlinkErr != nil {
		return f.symlinkErr
	}
	return f.osFS.Symlink(oldname, newname)
}

func (f linkFaultFS) Chmod(name string, mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.osFS.Chmod(name, mode)
}

func (f linkFaultFS) Chown(name string, uid, gid int) error {
	if f.chowned != nil {
		*f.chowned = true
	}
	if f.chownErr != nil {
		return f.chownErr
	}
	return f.osFS.Chown(name, uid, gid)
}

func (f linkFaultFS) Stat(name string) (os.FileInfo, error) {
	if f.chowned != nil && *f.chowned && f.statAfterChown != nil {
		return nil, f.statAfterChown
	}
	return f.osFS.Stat(name)
}

// target's value block runs through instance_eval (property.rb:500-504), so a
// failed symlink on a retarget reaches the event unwrapped.
func TestLinkRetargetSymlinkFailureIsNotWrapped(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	mustSymlink(t, "old", l)
	injected := errors.New("symlink injected failure")
	changes, _, err := applyLink(context.Background(), linkFaultFS{symlinkErr: injected}, l,
		fileRes(l, map[string]any{"ensure": "link", "target": "new"}), false, nil, nil)
	if err != nil || len(changes) != 1 {
		t.Fatalf("applyLink = %+v, %v; want one change", changes, err)
	}
	ch := changes[0]
	if ch.Property != "target" || ch.Was != "old" || ch.Is != "new" || ch.Err != injected {
		t.Errorf("change = %+v; want the injected error, unwrapped", ch)
	}
}

// property_fix runs inside set_link, so a metadata failure on a created link
// is collapsed onto the ensure change and wrapped, with no event of its own.
func TestLinkCreatedMetadataFailureIsCollapsedOntoEnsure(t *testing.T) {
	l := filepath.Join(t.TempDir(), "l")
	ch := applyOne(t, map[string]any{"ensure": "link", "target": "x", "owner": "nosuchuser-voxagent"}, l, false)
	want := "Could not set 'link' on ensure: Could not find user nosuchuser-voxagent"
	if ch.Property != "ensure" || ch.Was != "absent" || ch.Err == nil || ch.Err.Error() != want {
		t.Errorf("change = %+v; want Err %q", ch, want)
	}
	wantReadlink(t, l, "x")
}

// property_fix walks the resource's properties in declaration order --
// `properties.each` with an include? filter (type/file.rb:1113-1114), so the
// filter list's own order (mode first) decides nothing -- and raises out of
// the first that fails. Owner and group therefore outrank mode, and an
// attributed failure outranks a later property's retrieve failing too: the
// precedence a created file or directory uses as well (recordMetadata).
func TestLinkCreatedMetadataBlamesTheFirstFailure(t *testing.T) {
	otherGID := os.Getgid() + 1
	for _, tc := range []struct {
		name   string
		fsys   func() linkFaultFS
		params map[string]any
		want   string
	}{
		{"owner before a failing chmod", func() linkFaultFS {
			return linkFaultFS{chmodErr: errors.New("chmod injected failure")}
		}, map[string]any{"owner": "nosuchuser-voxagent", "mode": "0600"},
			"Could not set 'link' on ensure: Could not find user nosuchuser-voxagent"},
		{"group before a failing chmod and mode's failing retrieve", func() linkFaultFS {
			chowned := false
			return linkFaultFS{chownErr: errors.New("chown injected failure"),
				chmodErr:       errors.New("chmod injected failure"),
				statAfterChown: errors.New("stat injected failure"), chowned: &chowned}
		}, map[string]any{"group": strconv.Itoa(otherGID), "mode": "0600"},
			fmt.Sprintf("Could not set 'link' on ensure: Failed to set group to '%d': chown injected failure", otherGID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			real, l := filepath.Join(dir, "real"), filepath.Join(dir, "l")
			mustWrite(t, real, "a\n")
			if err := os.Chmod(real, 0o644); err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"ensure": "link", "target": "real", "links": "follow"}
			maps.Copy(params, tc.params)
			changes, _, err := applyLink(context.Background(), tc.fsys(), l, fileRes(l, params), false, nil, nil)
			if err != nil || len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("applyLink = %+v, %v; want one failed ensure change", changes, err)
			}
			if got := changes[0].Err.Error(); got != tc.want {
				t.Errorf("Err = %q; want %q", got, tc.want)
			}
		})
	}
}
