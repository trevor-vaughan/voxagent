package ral

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// recordingFS delegates to osFS and records each operation's name. A helper
// that called os directly would be invisible here -- and in recursion it would
// bypass the os.Root a generated child is confined to.
type recordingFS struct {
	osFS
	ops *[]string
}

func (f recordingFS) note(op string) { *f.ops = append(*f.ops, op) }

func (f recordingFS) Lstat(name string) (os.FileInfo, error) {
	f.note("Lstat")
	return f.osFS.Lstat(name)
}

func (f recordingFS) Stat(name string) (os.FileInfo, error) {
	f.note("Stat")
	return f.osFS.Stat(name)
}

func (f recordingFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	f.note("OpenFile")
	return f.osFS.OpenFile(name, flag, perm)
}

func (f recordingFS) CreateTemp(dir, pattern string) (*os.File, string, error) {
	f.note("CreateTemp")
	return f.osFS.CreateTemp(dir, pattern)
}

func (f recordingFS) Mkdir(name string, perm os.FileMode) error {
	f.note("Mkdir")
	return f.osFS.Mkdir(name, perm)
}

func (f recordingFS) Chmod(name string, mode os.FileMode) error {
	f.note("Chmod")
	return f.osFS.Chmod(name, mode)
}

func (f recordingFS) Chown(name string, uid, gid int) error {
	f.note("Chown")
	return f.osFS.Chown(name, uid, gid)
}

func (f recordingFS) Lchown(name string, uid, gid int) error {
	f.note("Lchown")
	return f.osFS.Lchown(name, uid, gid)
}

func (f recordingFS) Remove(name string) error {
	f.note("Remove")
	return f.osFS.Remove(name)
}

func (f recordingFS) Rename(oldname, newname string) error {
	f.note("Rename")
	return f.osFS.Rename(oldname, newname)
}

func (f recordingFS) Symlink(oldname, newname string) error {
	f.note("Symlink")
	return f.osFS.Symlink(oldname, newname)
}

func (f recordingFS) Readlink(name string) (string, error) {
	f.note("Readlink")
	return f.osFS.Readlink(name)
}

func TestFileHelpersReachTheFilesystemOnlyThroughFsys(t *testing.T) {
	seedFile := func(path string) error {
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			return err
		}
		return os.Chmod(path, 0o644)
	}
	uid, gid := strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid())
	for _, tc := range []struct {
		name  string
		seed  func(path string) error
		apply func(fsys fileSystem, path string) error
		want  []string
	}{
		{"create a file", nil, func(fsys fileSystem, path string) error {
			_, _, err := applyFile(context.Background(), fsys, path, fileRes(path, map[string]any{
				"ensure": "file", "content": "x", "mode": "0640"}), false, false, nil, nil, nil)
			return err
		}, []string{"Lstat", "CreateTemp", "Chmod", "Rename", "Lstat"}}, // the last is mode's re-read (type/file.rb:1117)
		{"replace content", seedFile, func(fsys fileSystem, path string) error {
			_, _, err := applyFile(context.Background(), fsys, path, fileRes(path, map[string]any{
				"ensure": "file", "content": "new"}), false, false, nil, nil, nil)
			return err
		}, []string{"Lstat", "OpenFile", "Lstat", "CreateTemp", "Chown", "Chmod", "Rename"}},
		{"create a directory", nil, func(fsys fileSystem, path string) error {
			_, _, err := applyDirectory(context.Background(), fsys, path, fileRes(path, map[string]any{"ensure": "directory"}), false, nil, nil)
			return err
		}, []string{"Lstat", "Stat", "Mkdir", "Chmod"}},
		{"remove a file", seedFile, func(fsys fileSystem, path string) error {
			_, err := applyAbsent(context.Background(), fsys, path, "absent", false, false, false, nil)
			return err
		}, []string{"Lstat", "Remove"}},
		{"create a link", nil, func(fsys fileSystem, path string) error {
			_, _, err := applyLink(context.Background(), fsys, path, fileRes(path, map[string]any{"ensure": "link", "target": "x"}), false, nil, nil)
			return err
		}, []string{"Lstat", "Lstat", "Stat", "Symlink"}},
		{"retarget a link", func(path string) error { return os.Symlink("old", path) }, func(fsys fileSystem, path string) error {
			_, _, err := applyLink(context.Background(), fsys, path, fileRes(path, map[string]any{"ensure": "link", "target": "x"}), false, nil, nil)
			return err
		}, []string{"Lstat", "Readlink", "Lstat", "Remove", "Stat", "Symlink"}},
		{"mode only", seedFile, func(fsys fileSystem, path string) error {
			_, _, err := applyMetadataOnly(context.Background(), fsys, path, fileRes(path, map[string]any{"mode": "0600"}), false, nil)
			return err
		}, []string{"Lstat", "Chmod"}},
		{"ownership in sync", seedFile, func(fsys fileSystem, path string) error {
			_, _, err := applyMetadataOnly(context.Background(), fsys, path, fileRes(path, map[string]any{"owner": uid, "group": gid}), false, nil)
			return err
		}, []string{"Lstat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p")
			if tc.seed != nil {
				if err := tc.seed(path); err != nil {
					t.Fatal(err)
				}
			}
			var ops []string
			if err := tc.apply(recordingFS{ops: &ops}, path); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !slices.Equal(ops, tc.want) {
				t.Errorf("operations = %v, want %v", ops, tc.want)
			}
		})
	}
}

func openTestRoot(t *testing.T) (rootFS, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return rootFS{root: root, dir: dir}, dir
}

// Errors name the absolute path the catalog's recursion names, not the
// root-relative one os.Root reports, and a name outside the root is refused
// before os.Root is asked.
func TestRootFSNamesAbsolutePathsAndRefusesOutsiders(t *testing.T) {
	fsys, dir := openTestRoot(t)
	missing := filepath.Join(dir, "missing")
	if _, err := fsys.Lstat(missing); err == nil || !os.IsNotExist(err) || !strings.Contains(err.Error(), missing) {
		t.Errorf("Lstat = %v, want a not-exist error naming %s", err, missing)
	}
	outside := filepath.Join(filepath.Dir(dir), "elsewhere")
	if _, err := fsys.Lstat(outside); err == nil || !strings.Contains(err.Error(), "is outside") {
		t.Errorf("Lstat(%s) = %v, want a refusal", outside, err)
	}
}

// provider/file/posix.rb:99-144: owner= and group= lchown under
// links => manage and chown under follow. Unprivileged, a group change to a
// group this user is not in is attempted and fails (group.rb has no root
// check), which is enough to see which call was made and that the target's
// group did not move.
func TestFileOwnershipOnALinkLchownsUnderManageAndChownsUnderFollow(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can change the group, so the change is not observable without moving it")
	}
	for _, tc := range []struct{ links, want, never string }{
		{"manage", "Lchown", "Chown"},
		{"follow", "Chown", "Lchown"},
	} {
		t.Run(tc.links, func(t *testing.T) {
			dir := t.TempDir()
			target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
			if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			var ops []string
			changes, _, err := applyMetadataOnly(context.Background(), recordingFS{ops: &ops}, link,
				fileRes(link, map[string]any{"group": "0", "links": tc.links}), false, nil)
			if err != nil || len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("changes = %+v, err %v; want one failed group change", changes, err)
			}
			if !slices.Contains(ops, tc.want) || slices.Contains(ops, tc.never) {
				t.Errorf("operations = %v, want %s and never %s", ops, tc.want, tc.never)
			}
		})
	}
}

// D-31: a source-recursed child under an inherited links => follow whose
// destination is a link (content already in sync) goes through rootFS, which
// refuses to chmod through the link and lchowns rather than chowns. Puppet
// chmods and chowns through it (provider/file/posix.rb:131-144, :155-156).
// Unprivileged, the group change fails either way, and the failing call's name
// -- lchownat, not chownat -- shows which one was made.
func TestFileFollowChildLinkIsNeitherChmodedNorChownedThroughTheRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root's group change succeeds, so the call made is not visible in its error")
	}
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(src, "f"), "x\n")
	real := filepath.Join(dst, "real")
	mustWrite(t, real, "x\n")
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dst, "f")
	if err := os.Symlink("real", link); err != nil {
		t.Fatal(err)
	}
	parent := fileRes(dst, map[string]any{"source": src, "recurse": true,
		"links": "follow", "mode": "0600", "group": "0"})
	children, _, err := File{}.Generate(context.Background(), parent, noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(children, func(c catalog.Resource) bool { return c.Title == link })
	if i < 0 {
		t.Fatalf("no child for %s in %+v", link, children)
	}
	changes, _, err := File{}.Apply(context.Background(), children[i], false)
	if err != nil || len(changes) != 2 {
		t.Fatalf("Apply = %+v, %v; want a group and a mode change", changes, err)
	}
	if ch := changes[1]; ch.Property != "mode" || ch.Message != "mode changed '0644' to '0600'" ||
		ch.Err == nil || !strings.Contains(ch.Err.Error(), "is a symlink; refusing to chmod through it") {
		t.Errorf("mode change = %+v; want it reported against the target and refused", ch)
	}
	if ch := changes[0]; ch.Property != "group" || ch.Err == nil || !strings.Contains(ch.Err.Error(), "lchownat") {
		t.Errorf("group change = %+v; want a failed lchown", ch)
	}
	if fi, err := os.Stat(real); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("target = %v (err %v), want it untouched at 0644", fi, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link = %v (err %v); it must stay a link", fi, err)
	}
}

// os.Root has no Lchmod, so Chmod refuses a link rather than chmod its target.
func TestRootFSChmodRefusesASymlink(t *testing.T) {
	fsys, dir := openTestRoot(t)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chmod(link, 0o600); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("Chmod = %v, want a refusal", err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("target = %v (err %v), want 0644", fi.Mode(), err)
	}
}

func TestRootFSCreateTempIsExclusiveAndInsideTheRoot(t *testing.T) {
	fsys, dir := openTestRoot(t)
	f, name, err := fsys.CreateTemp(dir, "conf.voxagent")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if filepath.Dir(name) != dir || !strings.HasPrefix(filepath.Base(name), "conf.voxagent") {
		t.Errorf("name = %s, want conf.voxagent* in %s", name, dir)
	}
	if fi, err := os.Stat(name); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("temp file = %v (err %v), want mode 0600", fi.Mode(), err)
	}
}

// A file opened through the root is named after the root, which is a /proc
// path; a failure of an operation on the handle must still name the real path.
func TestRootFSHandleErrorsNameTheRealPath(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "f")
	root, err := openDirNoFollow(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rfs := rootFS{root: root, dir: dir}
	p := filepath.Join(dir, "f")

	f, err := rfs.OpenFile(p, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("x")); err == nil || !strings.Contains(err.Error(), p) || strings.Contains(err.Error(), "/proc/") {
		t.Errorf("write on a read-only handle: %v, want it to name %s", err, p)
	}

	tmp, tmpName, err := rfs.CreateTemp(dir, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	if tmp.Name() != tmpName {
		t.Errorf("temp handle name = %q, want %q", tmp.Name(), tmpName)
	}
}

// With /proc unusable the directory cannot be opened as a root. The error must
// not read as "the directory does not exist", or Apply under --noop would fall
// back to unconfined reads of the plain filesystem.
func TestFileApplyNoopRefusesAChildWhenProcIsUnusable(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "f")
	procFdDir = filepath.Join(dir, "no-such-proc")
	t.Cleanup(func() { procFdDir = "/proc/self/fd" })
	child := childOf(fileRes(dir, map[string]any{"recurse": true}), filepath.Join(dir, "f"), map[string]any{"ensure": "file"})
	for _, noop := range []bool{false, true} {
		_, _, err := File{}.Apply(context.Background(), child, noop)
		if err == nil || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "/proc") {
			t.Errorf("noop %t: err = %v, want a /proc failure that is not ErrNotExist", noop, err)
		}
	}
}
