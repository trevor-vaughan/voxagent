package file

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// fileSystem is every filesystem operation the file provider performs on a
// managed path. Names are absolute in every implementation, so messages keep
// naming the path the catalog names.
//
// osFS serves catalog resources; rootFS confines a recursed directory's
// generated children to it. This interface is the seam that makes the
// confinement total, which is why no file helper may call os directly
// (TestFileHelpersReachTheFilesystemOnlyThroughFsys).
type fileSystem interface {
	Lstat(name string) (os.FileInfo, error)
	Stat(name string) (os.FileInfo, error)
	// OpenFile is the only open: every read of a managed path passes
	// O_NONBLOCK, so a fifo swapped in cannot block the agent (openSource).
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	// CreateTemp creates a new file in dir as os.CreateTemp does and returns
	// it with its absolute name.
	CreateTemp(dir, pattern string) (*os.File, string, error)
	Mkdir(name string, perm os.FileMode) error
	Chmod(name string, mode os.FileMode) error
	Chown(name string, uid, gid int) error
	// Lchown is Chown on name itself: a final symlink is changed, not
	// followed.
	Lchown(name string, uid, gid int) error
	Remove(name string) error
	// RemoveAll removes name and everything below it. It never follows a
	// symlink: a link in the tree is unlinked, not descended.
	RemoveAll(name string) error
	Rename(oldname, newname string) error
	// Symlink creates newname as a link to oldname, which is not resolved.
	Symlink(oldname, newname string) error
	// Readlink returns what the link at name points to, without following it.
	Readlink(name string) (string, error)
	// Contains refuses the file open as f unless it lies where this
	// fileSystem's names may reach. A file opened other than by name -- a
	// link entry relative to a directory descriptor (bucketDir) -- is checked
	// with it.
	Contains(f *os.File) error
}

// osFS is the os package itself: names resolve the way the kernel resolves
// them, symlinks included. Callers decide with statFor whether a final link is
// followed, and only Stat, Chmod, Chown and an OpenFile without O_NOFOLLOW
// ever follow one.
//
// Residual race: under links => manage, "no mode through a link" rests on
// statFor's Lstat (carried to applyMetadata as before), and Chmod is os.Chmod,
// which follows. Someone who can write the parent directory and swaps a link
// in between the Lstat and the chmod gets the chmod through it. Puppet has the
// same window (type/file/mode.rb:133 tests the stat, posix.rb:156 chmods the
// path). rootFS.Chmod has the same Lstat-then-chmod window, confined to its
// tree.
type osFS struct{}

func (osFS) Lstat(name string) (os.FileInfo, error) { return os.Lstat(name) }
func (osFS) Stat(name string) (os.FileInfo, error)  { return os.Stat(name) }

func (osFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func (osFS) CreateTemp(dir, pattern string) (*os.File, string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, "", err
	}
	return f, f.Name(), nil
}

func (osFS) Mkdir(name string, perm os.FileMode) error { return os.Mkdir(name, perm) }
func (osFS) Chmod(name string, mode os.FileMode) error { return os.Chmod(name, mode) }
func (osFS) Chown(name string, uid, gid int) error     { return os.Chown(name, uid, gid) }
func (osFS) Lchown(name string, uid, gid int) error    { return os.Lchown(name, uid, gid) }
func (osFS) Remove(name string) error                  { return os.Remove(name) }
func (osFS) RemoveAll(name string) error               { return os.RemoveAll(name) }
func (osFS) Rename(oldname, newname string) error      { return os.Rename(oldname, newname) }
func (osFS) Symlink(oldname, newname string) error     { return os.Symlink(oldname, newname) }
func (osFS) Readlink(name string) (string, error)      { return os.Readlink(name) }
func (osFS) Contains(*os.File) error                   { return nil }

// rootFS confines every operation to the directory root was opened on: a
// generated child is applied through one, opened on the recursed directory.
//
// os.Root refuses any name that escapes the root, by ".." or through a
// symlink pointing outside it, and refuses absolute symlinks. It does FOLLOW
// a symlink whose target stays inside the root, so a directory in the tree
// swapped -- between the walk and a child's apply -- for a link to another
// directory in the tree redirects that child's operation within the tree,
// never out of it. Chown is Lchown, which never follows a final link. There
// is no Lchmod, so Chmod Lstats first and refuses a link; a swap between that
// Lstat and the chmod is the residual race, confined to the tree like the
// rest.
//
// Residuals the root does not close, each confined to the tree:
//   - Hard links. A child that is a hard link shares its inode with a file
//     that may live outside the tree, so chmod or chown on it changes that
//     file too. Only fs.protected_hardlinks=0 lets an unprivileged user link
//     a file they do not own; Puppet chmods and chowns the same inode.
//   - Empty-directory swap. A directory swapped in between a purged child's
//     Lstat and Root.Remove is removed only if empty (rmdir), and only inside
//     the tree.
//   - Swap into an exempt subtree. A writer of the recursed directory who
//     swaps an in-tree directory for a link to a catalog-managed subtree,
//     which purge exempts, redirects an operation into it: sub/x purged
//     unlinks keep/x when sub has become a link to keep.
type rootFS struct {
	root *os.Root
	dir  string // the cleaned absolute path root was opened on
}

// rel maps an absolute name to root's relative one, refusing a name outside.
func (rfs rootFS) rel(name string) (string, error) {
	rel, err := filepath.Rel(rfs.dir, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside %s", name, rfs.dir)
	}
	return rel, nil
}

// named puts the absolute name back into an error os.Root reported with the
// relative one, so messages name the path the catalog's recursion names.
func named(err error, name string) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &os.PathError{Op: pe.Op, Path: name, Err: pe.Err}
	}
	return err
}

func (rfs rootFS) Lstat(name string) (os.FileInfo, error) {
	rel, err := rfs.rel(name)
	if err != nil {
		return nil, err
	}
	fi, err := rfs.root.Lstat(rel)
	return fi, named(err, name)
}

func (rfs rootFS) Stat(name string) (os.FileInfo, error) {
	rel, err := rfs.rel(name)
	if err != nil {
		return nil, err
	}
	fi, err := rfs.root.Stat(rel)
	return fi, named(err, name)
}

func (rfs rootFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	rel, err := rfs.rel(name)
	if err != nil {
		return nil, err
	}
	f, err := rfs.root.OpenFile(rel, flag, perm)
	if err != nil {
		return nil, named(err, name)
	}
	f, err = withName(f, name)
	return f, named(err, name)
}

// withName returns f's open file under name. A file opened through the
// root is named after the root (openDirNoFollow: a /proc path), and that name
// is what its Read, Write, Sync and ReadDir errors report. The descriptor is
// duplicated through SyscallConn, which unlike Fd leaves O_NONBLOCK alone.
func withName(f *os.File, name string) (*os.File, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	var nfd int
	var dupErr error
	if err := sc.Control(func(fd uintptr) { nfd, dupErr = syscall.Dup(int(fd)) }); err != nil {
		dupErr = err
	}
	_ = f.Close()
	if dupErr != nil {
		return nil, dupErr
	}
	syscall.CloseOnExec(nfd)
	return os.NewFile(uintptr(nfd), name), nil
}

// CreateTemp is R9's write path: O_CREATE|O_EXCL|O_WRONLY at 0600 on a random
// name in dir, retried on the vanishing chance of a clash. os.Root has no
// CreateTemp of its own.
func (rfs rootFS) CreateTemp(dir, pattern string) (*os.File, string, error) {
	relDir, err := rfs.rel(dir)
	if err != nil {
		return nil, "", err
	}
	for range 100 {
		base := pattern + rand.Text()
		f, err := rfs.root.OpenFile(filepath.Join(relDir, base), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", named(err, filepath.Join(dir, base))
		}
		tmp := filepath.Join(dir, base)
		f, err = withName(f, tmp)
		if err != nil {
			return nil, "", named(err, tmp)
		}
		return f, tmp, nil
	}
	return nil, "", fmt.Errorf("could not create a temporary file in %s", dir)
}

func (rfs rootFS) Mkdir(name string, perm os.FileMode) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	return named(rfs.root.Mkdir(rel, perm), name)
}

func (rfs rootFS) Chmod(name string, mode os.FileMode) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	fi, err := rfs.root.Lstat(rel)
	if err != nil {
		return named(err, name)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to chmod through it", name)
	}
	return named(rfs.root.Chmod(rel, mode), name)
}

func (rfs rootFS) Chown(name string, uid, gid int) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	return named(rfs.root.Lchown(rel, uid, gid), name)
}

func (rfs rootFS) Lchown(name string, uid, gid int) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	return named(rfs.root.Lchown(rel, uid, gid), name)
}

func (rfs rootFS) Remove(name string) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	return named(rfs.root.Remove(rel), name)
}

func (rfs rootFS) RemoveAll(name string) error {
	rel, err := rfs.rel(name)
	if err != nil {
		return err
	}
	return named(rfs.root.RemoveAll(rel), name)
}

func (rfs rootFS) Rename(oldname, newname string) error {
	oldRel, err := rfs.rel(oldname)
	if err != nil {
		return err
	}
	newRel, err := rfs.rel(newname)
	if err != nil {
		return err
	}
	err = rfs.root.Rename(oldRel, newRel)
	var le *os.LinkError
	if errors.As(err, &le) {
		return &os.LinkError{Op: le.Op, Old: oldname, New: newname, Err: le.Err}
	}
	return err
}

// Symlink does not confine oldname, as os.Root.Symlink does not: a link's
// target is data until something follows it, and every rootFS operation that
// follows one is confined.
func (rfs rootFS) Symlink(oldname, newname string) error {
	rel, err := rfs.rel(newname)
	if err != nil {
		return err
	}
	err = rfs.root.Symlink(oldname, rel)
	var le *os.LinkError
	if errors.As(err, &le) {
		return &os.LinkError{Op: le.Op, Old: oldname, New: newname, Err: le.Err}
	}
	return err
}

func (rfs rootFS) Readlink(name string) (string, error) {
	rel, err := rfs.rel(name)
	if err != nil {
		return "", err
	}
	l, err := rfs.root.Readlink(rel)
	return l, named(err, name)
}

// Contains refuses f unless the file it is open on lies below the tree: its
// real path, read from the kernel through the descriptor (realPathOf), must
// be below the tree's own, read the same way.
func (rfs rootFS) Contains(f *os.File) error {
	top, err := rfs.root.Open(".")
	if err != nil {
		return named(err, rfs.dir)
	}
	defer top.Close()
	topReal, err := realPathOf(top)
	if err != nil {
		return err
	}
	real, err := realPathOf(f)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(topReal, real); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is outside %s", real, rfs.dir)
	}
	return nil
}

// openDirNoFollow opens dir as an os.Root without following a symlink at dir
// itself, which os.OpenRoot would. dir is Lstat'd first and the opened
// directory compared with that Lstat afterwards, so a directory swapped for a
// link between the two is refused rather than recursed through.
//
// os.OpenRoot opens by path without O_NONBLOCK, so a directory swapped for a
// fifo after the Lstat would block it forever. dir is therefore opened first
// with O_NONBLOCK|O_DIRECTORY|O_NOFOLLOW, which fails at once on anything but
// a directory, and the root is made from that handle through /proc/self/fd,
// which names the directory already opened and not whatever is at dir now.
// That root's own name is the /proc path, so files opened through it are
// renamed to their real path (withName). A /proc that cannot be used is an
// error that is not fs.ErrNotExist: Apply reads that as "the parent does not
// exist yet" under --noop and falls back to unconfined reads.
//
// beforeDirOpen, when set by a test, runs after the Lstat: the swap window.
var beforeDirOpen func(dir string)

// procFdDir is where the opened directory is reached; a test points it
// elsewhere to simulate a missing /proc.
var procFdDir = "/proc/self/fd"

func openDirNoFollow(dir string) (*os.Root, error) {
	before, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; links is not implemented", dir)
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if beforeDirOpen != nil {
		beforeDirOpen(dir)
	}
	h, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	root, err := os.OpenRoot(fmt.Sprintf("%s/%d", procFdDir, h.Fd()))
	if err != nil {
		return nil, fmt.Errorf("open %s through %s (is /proc mounted?): %v", dir, procFdDir, err)
	}
	after, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if !os.SameFile(before, after) {
		_ = root.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", dir)
	}
	return root, nil
}
