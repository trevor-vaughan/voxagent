package file

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// backup is a resource's backup parameter (type/file.rb:76-146) with what
// performing it needs. A nil *backup, or one whose value is "", backs
// nothing up.
//
// Puppet backs up inside remove_existing (type/file.rb:791-805), which every
// path that replaces or removes what is at the path runs first: ensure's
// sync (ensure.rb:186), write (type/file.rb:930, so a content change of an
// existing file too) and mklink (target.rb:43). A mode-only change and a
// newly created file run none of them. The callers here are those paths:
// each calls existing before removeExisting or a content write.
type backup struct {
	// value is the munged parameter: a suffix starting with "." (a local
	// copy) or a bucket name.
	value  string
	vardir string
	// force, follow and recurse are the resource's own (can_backup?,
	// remove_backup, perform_backup_with_bucket).
	force, follow, recurse bool
	// redact is set when the catalog lists backup as Sensitive: errors show
	// value as [redacted] (D-78: Ruby prints it).
	redact bool
	// warnings are the run-level, Puppet-sourced warnings backing up raised,
	// which Apply returns.
	warnings []ral.Warning
	// log, when set, receives the lines Puppet's dipper logs while it saves
	// (level, message, all source Puppet): the lock on paths. The file
	// resource leaves it nil.
	log func(level, message string)
}

// defaultBucket is the only bucket voxagent implements: the one backup =>
// 'puppet' names when the catalog declares no filebucket of that name
// (type/file.rb:500-510).
const defaultBucket = "puppet"

// backupValue munges backup as type/file.rb:126-142 does: false and "false"
// are no backup, true and "true" the suffix .puppet-bak, any other string is
// kept, and a list is its first element. Anything else is refused with
// Puppet's message, which it raises while converting the catalog. "" means no
// backup.
func backupValue(r catalog.Resource) (string, error) {
	v, ok := r.Parameters["backup"]
	if !ok {
		return "", nil
	}
	if list, isList := v.([]any); isList {
		// value.shift: an empty list is nil.
		v = nil
		if len(list) > 0 {
			v = list[0]
		}
	}
	switch v := v.(type) {
	case bool:
		if v {
			return ".puppet-bak", nil
		}
		return "", nil
	case string:
		switch v {
		case "false":
			return "", nil
		case "true":
			return ".puppet-bak", nil
		}
		return v, nil
	}
	return "", fmt.Errorf("Parameter backup failed on %s: Invalid backup type %s", r.Ref(), rubyvalue.ShownValue(r, "backup", v))
}

// newBackup is r's backup, or nil when it has none.
func newBackup(r catalog.Resource, vardir string) (*backup, error) {
	value, err := backupValue(r)
	if err != nil || value == "" {
		return nil, err
	}
	return &backup{value: value, vardir: vardir, force: forceTrue(r), follow: linksFollow(r),
		recurse: rubyvalue.ParamString(r, "recurse", "false") == "true",
		redact:  slices.Contains(r.SensitiveParameters, "backup")}, nil
}

// isSuffix is bucket's `backup =~ /^\./` (type/file.rb:492): Ruby's ^
// matches after any newline, so a dot opening any line makes it a suffix.
func (b *backup) isSuffix() bool {
	return strings.HasPrefix(b.value, ".") || strings.Contains(b.value, "\n.")
}

// existing is remove_existing's backup step (type/file.rb:799-805) for what
// is at path, whose statFor is fi. A type that cannot be backed up
// (can_backup?, :1021-1029) is not an error: it returns Puppet's warning,
// which the caller logs at the resource. An error means nothing may be
// replaced or removed; it is Puppet's own message, raised before set and so
// never wrapped.
func (b *backup) existing(fsys fileSystem, path string, fi os.FileInfo) (pathWarnings []string, err error) {
	if b == nil {
		return nil, nil
	}
	if t := ftype(fi); !(t == "file" || t == "link" || (t == "directory" && b.force)) {
		return []string{fmt.Sprintf("Could not back up file of type %s", t)}, nil
	}
	// perform_backup (util/backups.rb:7-17): Puppet::FileSystem.exist? follows
	// a link, and a path that is not there -- missing, a dangling link, a
	// link loop -- is nothing to back up.
	if _, err := fsys.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
			return nil, nil
		}
		if b.isSuffix() {
			// exist? is false for any other failure too, and Puppet then
			// removes what it could not copy. A suffix copy would copy the
			// content, so it is refused instead: through a rootFS that
			// includes a link out of the recursed tree, whose target is
			// neither read nor lost without a word (I-17).
			return nil, fmt.Errorf("Could not back up %s: %s", path, err)
		}
		// A bucket judges the path by its lstat (toBucket): a link -- one
		// out of a recursed tree among them -- is nothing to bucket, as
		// Puppet's is, whatever its target.
	}
	if b.isSuffix() {
		return nil, b.local(fsys, path)
	}
	if b.value != defaultBucket {
		return nil, fmt.Errorf("Could not find filebucket %s specified in backup", rubyvalue.RedactIf(b.redact, "%s", b.value))
	}
	return nil, b.toBucket(fsys, path)
}

// local is perform_backup_with_backuplocal (util/backups.rb:36-54): unlink a
// stale <path><suffix> (remove_backup, :56-79), then copy path there as
// FileUtils.cp_r(preserve: true) does.
//
// Under redact the suffix is [redacted] wherever it appears in an error, the
// file system's own text included.
func (b *backup) local(fsys fileSystem, path string) error {
	newfile := path + b.value
	scrub := func(text string) string {
		if b.redact {
			return strings.ReplaceAll(text, b.value, "[redacted]")
		}
		return text
	}
	// remove_backup reads the old backup as links says: lstat under manage,
	// stat under follow. Either way a link is unlinked, never written through.
	if fi, err := statFor(fsys, newfile, b.follow); err == nil {
		if fi.IsDir() {
			return fmt.Errorf("Will not remove directory backup %s; use a filebucket", scrub(newfile))
		}
		if err := fsys.Remove(newfile); err != nil {
			return fmt.Errorf("Could not remove old backup: %s", scrub(err.Error()))
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Only Errno::ENOENT is rescued (:62-64).
		return err
	}
	if err := copyTree(fsys, path, newfile); err != nil {
		return fmt.Errorf("Could not back %s up: %s", path, scrub(err.Error()))
	}
	return nil
}

// backupSwapWindow, when set by a test, runs in a tree copy between an
// entry's lstat and its open (at "lstat") and after a directory is opened and
// checked (at "open"): the windows in which it can be swapped for a link.
var backupSwapWindow func(at, path string)

// copyTree is FileUtils.cp_r(src, dst, preserve: true) with its default
// dereference_root: src itself is followed, so a link at the path is copied
// as what it reaches; below it a link is copied as a link. Puppet's copy
// makes a fifo or device node anew; that is not implemented, and refused.
//
// src and dst are reached through fsys, so a generated child's copy stays in
// its os.Root. Everything below them is reached relative to a descriptor
// already opened and checked (copyDir), never by a joined path that a
// directory swapped for a link could redirect. Bytes and metadata both come
// from the descriptor opened, and an entry that is not the one lstat'ed is
// refused. dst is created afresh (O_EXCL, Mkdir), so nothing is ever written
// through a link.
//
// A copy that fails part way is removed (RemoveAll never follows a link), so
// a partial tree cannot refuse every later run as a directory backup. Only a
// dst this call created is removed. A removal that fails is named beside
// the cause.
//
// Residual (D-45): the removal is by path. A writer of dst's parent, when
// that parent is not sticky, who renames a directory onto dst between the
// failure and the removal makes the agent delete that directory's contents.
// The robust form empties dst through the descriptor copyRoot opened (an
// os.Root on it), then removes the emptied directory.
func copyTree(fsys fileSystem, src, dst string) error {
	created, err := copyRoot(fsys, src, dst)
	if err != nil && created {
		if rmErr := fsys.RemoveAll(dst); rmErr != nil {
			return fmt.Errorf("%s; removing the partial copy failed: %s", err, rmErr)
		}
	}
	return err
}

// copyRoot copies copyTree's src to dst and reports whether it created dst.
func copyRoot(fsys fileSystem, src, dst string) (bool, error) {
	fi, err := fsys.Stat(src)
	if err != nil {
		return false, err
	}
	switch {
	case fi.Mode().IsRegular():
		in, err := openSource(fsys, src, true)
		if err != nil {
			return false, err
		}
		defer in.Close()
		inFi, err := opened(in, fi, src)
		if err != nil {
			return false, err
		}
		out, err := fsys.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return false, err
		}
		return true, copyBytes(in, out, inFi)
	case fi.IsDir():
		in, err := fsys.OpenFile(src, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
		if err != nil {
			return false, err
		}
		defer in.Close()
		inFi, err := opened(in, fi, src)
		if err != nil {
			return false, err
		}
		if err := fsys.Mkdir(dst, 0o700); err != nil {
			return false, err
		}
		made, err := fsys.Lstat(dst)
		if err != nil {
			return true, err
		}
		out, err := fsys.OpenFile(dst, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return true, err
		}
		defer out.Close()
		// What is open must be the directory made here: the one lstat'ed,
		// and -- since a writer of dst's parent may have swapped their own in
		// before that lstat -- this user's and private, as Mkdir made it.
		// Anything else is neither written into nor removed.
		outFi, err := opened(out, made, dst)
		if err != nil {
			return false, err
		}
		if st, ok := outFi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() || outFi.Mode().Perm()&0o077 != 0 {
			return false, fmt.Errorf("%s is not the private directory the backup made", dst)
		}
		return true, copyDir(in, out, inFi, src, dst)
	}
	return false, fmt.Errorf("cannot copy %s of type %s", src, ftype(fi))
}

// copyBytes copies in to the new file out, gives out in's metadata (inFi)
// and closes out.
func copyBytes(in, out *os.File, inFi os.FileInfo) error {
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := preserveMetadata(out, inFi); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyDir copies the entries of the directory open as in, whose checked stat
// is inFi, into the new empty directory open as out, then gives out inFi's
// metadata: after the children, as cp_r's postorder copy_metadata does, so
// their creation does not move its mtime. src and dst name in and out in
// messages only.
//
// Each entry is lstat'ed and opened through an os.Root made from the
// descriptor itself (rootAt), so a single name is resolved in the directory
// already opened. os.Root still follows a final link that stays inside it,
// so what is opened is checked against the lstat (opened); a directory is
// descended through the descriptor so opened.
func copyDir(in, out *os.File, inFi os.FileInfo, src, dst string) error {
	if backupSwapWindow != nil {
		backupSwapWindow("open", src)
	}
	from, err := rootAt(in, src)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := rootAt(out, dst)
	if err != nil {
		return err
	}
	defer to.Close()
	entries, err := in.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		srcPath, dstPath := filepath.Join(src, name), filepath.Join(dst, name)
		fi, err := from.Lstat(name)
		if err != nil {
			return named(err, srcPath)
		}
		if backupSwapWindow != nil {
			backupSwapWindow("lstat", srcPath)
		}
		switch {
		case fi.Mode().IsRegular():
			err = copyFileAt(from, to, name, fi, srcPath, dstPath)
		case fi.IsDir():
			err = copyDirAt(from, to, name, fi, srcPath, dstPath)
		case fi.Mode()&os.ModeSymlink != 0:
			err = copyLinkAt(from, to, name, fi, srcPath, dstPath)
		default:
			err = fmt.Errorf("cannot copy %s of type %s", srcPath, ftype(fi))
		}
		if err != nil {
			return err
		}
	}
	return preserveMetadata(out, inFi)
}

func copyFileAt(from, to *os.Root, name string, fi os.FileInfo, srcPath, dstPath string) error {
	in, err := from.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return named(err, srcPath)
	}
	defer in.Close()
	inFi, err := opened(in, fi, srcPath)
	if err != nil {
		return err
	}
	out, err := to.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return named(err, dstPath)
	}
	return copyBytes(in, out, inFi)
}

func copyDirAt(from, to *os.Root, name string, fi os.FileInfo, srcPath, dstPath string) error {
	in, err := from.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return named(err, srcPath)
	}
	defer in.Close()
	inFi, err := opened(in, fi, srcPath)
	if err != nil {
		return err
	}
	if err := to.Mkdir(name, 0o700); err != nil {
		return named(err, dstPath)
	}
	made, err := to.Lstat(name)
	if err != nil {
		return named(err, dstPath)
	}
	out, err := to.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return named(err, dstPath)
	}
	defer out.Close()
	if _, err := opened(out, made, dstPath); err != nil {
		return err
	}
	return copyDir(in, out, inFi, srcPath, dstPath)
}

// copyLinkAt copies a link as a link. copy_metadata on a link is lchown,
// whose EPERM and EACCES it rescues, and no utime or chmod (Linux has no
// lchmod).
func copyLinkAt(from, to *os.Root, name string, fi os.FileInfo, srcPath, dstPath string) error {
	target, err := from.Readlink(name)
	if err != nil {
		return named(err, srcPath)
	}
	if err := to.Symlink(target, name); err != nil {
		return named(err, dstPath)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := to.Lchown(name, int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, fs.ErrPermission) {
		return named(err, dstPath)
	}
	return nil
}

// rootAt is an os.Root on the directory open as f, made through its
// descriptor's /proc link, which names the directory already opened and not
// whatever is at its path now. name is f's path, for messages.
func rootAt(f *os.File, name string) (*os.Root, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var root *os.Root
	var openErr error
	if err := sc.Control(func(fd uintptr) {
		root, openErr = os.OpenRoot(fmt.Sprintf("%s/%d", procFdDir, fd))
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, fmt.Errorf("open %s through %s (is /proc mounted?): %v", name, procFdDir, openErr)
	}
	return root, nil
}

// opened is the stat of the descriptor in, opened on src after fi was read,
// refusing anything but the file fi describes.
func opened(in *os.File, fi os.FileInfo, src string) (os.FileInfo, error) {
	inFi, err := in.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, inFi) {
		return nil, fmt.Errorf("%s changed while it was being backed up", src)
	}
	return inFi, nil
}

// preserveMetadata is FileUtils' copy_metadata on the open copy: times, then
// owner, then mode. A chown this user may not make is not an error, and
// clears setuid and setgid as Ruby's does. Every call goes through the
// descriptor, so none can reach anything but the file just created.
func preserveMetadata(f *os.File, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no stat for %s", f.Name())
	}
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	err = sc.Control(func(fd uintptr) {
		// utimensat on the descriptor's /proc link: the kernel resolves it to
		// the open file, at nanosecond precision.
		ts := []syscall.Timespec{st.Atim, st.Mtim}
		if opErr = syscall.UtimesNano(fmt.Sprintf("%s/%d", procFdDir, fd), ts); opErr != nil {
			return
		}
		mode := st.Mode & 0o7777
		if e := syscall.Fchown(int(fd), int(st.Uid), int(st.Gid)); e != nil {
			if !errors.Is(e, syscall.EPERM) && !errors.Is(e, syscall.EACCES) {
				opErr = e
				return
			}
			mode &= 0o1777
		}
		opErr = syscall.Fchmod(int(fd), mode)
	})
	if err != nil {
		return err
	}
	if opErr != nil {
		return &os.PathError{Op: "preserve", Path: f.Name(), Err: opErr}
	}
	return nil
}

// toBucket is perform_backup_with_bucket (util/backups.rb:21-34) for the
// default bucket: a file is backed up, a link is not, and a directory --
// unless the resource recurses -- has each regular file below it backed up,
// as Find.find walks it: depth first, names sorted, never into a linked
// directory, File.file? following a link.
//
// The path is reached through fsys, everything below it through the
// descriptor of the directory already opened and checked, as the suffix copy
// is (copyDir): a directory swapped for a link during the walk is never read
// through; before its open it is refused. A link below the path is opened
// relative to its directory's descriptor too (openLinkAt), following only
// the link itself, so nothing in a swapped-in directory is read through it;
// what a link reaches must lie where fsys reaches (Contains, D-47). Each file's bytes are recorded under
// the real path of the descriptor they came from, so a restore of the bucket
// can never write another file's bytes to the managed path.
func (b *backup) toBucket(fsys fileSystem, path string) error {
	fi, err := fsys.Lstat(path)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode().IsRegular():
		in, err := openSource(fsys, path, false)
		if err != nil {
			return fmt.Errorf("Could not back up %s: %s", path, err)
		}
		defer in.Close()
		if _, err := opened(in, fi, path); err != nil {
			return fmt.Errorf("Could not back up %s: %s", path, err)
		}
		return b.bucketFile(in, path)
	case fi.IsDir():
		if b.recurse {
			return nil
		}
		in, err := fsys.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer in.Close()
		if _, err := opened(in, fi, path); err != nil {
			return err
		}
		return b.bucketDir(fsys, in, path)
	}
	return nil
}

// bucketDir backs up every regular file below the directory open as in,
// which is dir, reaching each entry through in's descriptor (rootAt).
func (b *backup) bucketDir(fsys fileSystem, in *os.File, dir string) error {
	if backupSwapWindow != nil {
		backupSwapWindow("open", dir)
	}
	root, err := rootAt(in, dir)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := in.ReadDir(-1)
	if err != nil {
		return err
	}
	// File.ReadDir is directory order; Find.find sorts, bytewise, and the
	// order is the order paths records a content met twice in.
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(dir, name)
		lfi, err := root.Lstat(name)
		if err != nil {
			return named(err, p)
		}
		if backupSwapWindow != nil {
			backupSwapWindow("lstat", p)
		}
		switch {
		case lfi.IsDir():
			sub, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return named(err, p)
			}
			if _, err := opened(sub, lfi, p); err != nil {
				_ = sub.Close()
				return err
			}
			err = b.bucketDir(fsys, sub, p)
			_ = sub.Close()
			if err != nil {
				return err
			}
		case lfi.Mode().IsRegular():
			f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return fmt.Errorf("Could not back up %s: %s", p, named(err, p))
			}
			if _, err := opened(f, lfi, p); err != nil {
				_ = f.Close()
				return fmt.Errorf("Could not back up %s: %s", p, err)
			}
			err = b.bucketFile(f, p)
			_ = f.Close()
			if err != nil {
				return err
			}
		case lfi.Mode()&os.ModeSymlink != 0:
			// File.file? follows a link: one to a regular file is backed up
			// as that file (measured), under the path the opened descriptor
			// has, so its bytes are recorded where they came from, however
			// the link resolved. Anything else, or nothing, is not a file,
			// and neither is one fsys does not reach (D-47).
			f, err := openLinkAt(in, name, p)
			if err != nil {
				continue
			}
			if err := fsys.Contains(f); err != nil {
				_ = f.Close()
				continue
			}
			err = b.bucketFile(f, p)
			_ = f.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// openLinkAt opens the link name in the directory open as dir, which is p's,
// following it to what it reaches: name is resolved in the directory already
// opened and checked, through its descriptor, not through p, whose directory
// may since have been swapped for another.
//
// What it reaches is stat'ed first, through the descriptor's /proc link, and
// anything but a regular file is refused unopened, as File.file? only stats:
// opening a device as root can act on it (a watchdog arms, a tape rewinds).
// The open is openat on the descriptor, with O_NOCTTY, and is checked again
// for a regular file; a link retargeted between the two is the residual race.
func openLinkAt(dir *os.File, name, p string) (*os.File, error) {
	sc, err := dir.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var openErr error
	if err := sc.Control(func(dirfd uintptr) {
		fi, err := os.Stat(fmt.Sprintf("%s/%d/%s", procFdDir, dirfd, name))
		if err != nil {
			openErr = err
			return
		}
		if !fi.Mode().IsRegular() {
			openErr = fmt.Errorf("%s is not a regular file", p)
			return
		}
		fd, openErr = syscall.Openat(int(dirfd), name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
		if openErr != nil {
			openErr = &os.PathError{Op: "openat", Path: p, Err: openErr}
		}
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, openErr
	}
	f := os.NewFile(uintptr(fd), p)
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is no longer a regular file", p)
	}
	return f, nil
}

// bucketFile is Dipper#backup (file_bucket/dipper.rb:39-60) of the regular
// file open as in, which is file, into a local bucket at
// <vardir>/clientbucket: content already there from the same real path is
// left alone; otherwise save_to_disk
// (indirector/file_bucket_file/file.rb:150-197) stores it.
//
// Every failure is Dipper's "Could not back up <file>: <detail>".
func (b *backup) bucketFile(in *os.File, file string) error {
	if err := b.saveToBucket(in); err != nil {
		return fmt.Errorf("Could not back up %s: %s", file, err)
	}
	return nil
}

func (b *backup) saveToBucket(in *os.File) error {
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	// absolutize_path is Pathname#realpath (dipper.rb:180-182), taken from
	// the descriptor, which names the file whose bytes these are.
	real, err := realPathOf(in)
	if err != nil {
		return err
	}
	if b.vardir == "" {
		return errors.New("no vardir is configured")
	}
	cb, err := openClientbucket(b.vardir)
	if err != nil {
		return err
	}
	defer cb.Close()
	// The HEAD check (dipper.rb:51-53): contents present and the path
	// recorded is no save at all.
	shard, err := openShard(cb, sum, false)
	if err != nil {
		return err
	}
	if shard != nil {
		held, err := shardHolds(shard, real)
		_ = shard.Close()
		if err != nil || held {
			return err
		}
	}
	if shard, err = openShard(cb, sum, true); err != nil {
		return err
	}
	defer shard.Close()
	return b.saveToShard(shard, in, sum, real)
}

// realPathOf is the absolute path, symlinks resolved, of the file open as f:
// its descriptor's /proc link. A file renamed or removed since it was opened
// has no such path, and is refused.
func realPathOf(f *os.File) (string, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var real string
	var readErr error
	if err := sc.Control(func(fd uintptr) {
		real, readErr = os.Readlink(fmt.Sprintf("%s/%d", procFdDir, fd))
	}); err != nil {
		return "", err
	}
	if readErr != nil {
		return "", fmt.Errorf("the path of %s (is /proc mounted?): %v", f.Name(), readErr)
	}
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if at, err := os.Lstat(real); err != nil || !os.SameFile(fi, at) {
		return "", fmt.Errorf("%s changed while it was being backed up", f.Name())
	}
	return real, nil
}

// openClientbucket opens <vardir>/clientbucket, creating the vardir as
// Puppet's settings would and the clientbucket at its setting's mode, 0750
// (defaults.rb clientbucketdir). The vardir is the operator's; the
// clientbucket below it is never followed if it is a link.
func openClientbucket(vardir string) (*os.Root, error) {
	if err := os.MkdirAll(vardir, 0o755); err != nil {
		return nil, err
	}
	v, err := os.OpenRoot(vardir)
	if err != nil {
		return nil, err
	}
	defer v.Close()
	return subdirNoFollow(v, filepath.Join(vardir, "clientbucket"), "clientbucket", 0o750, true)
}

// openShard opens the directory content whose sha256 is sum lives in:
// <d1>/.../<d8>/<sum> (path_for, indirector/file_bucket_file/file.rb:219-226).
// With create, missing directories are made at 0770, the mode save_to_disk's
// umask of 007 gives them (:155); without it, a missing one is (nil, nil).
func openShard(cb *os.Root, sum string, create bool) (*os.Root, error) {
	names := append(strings.Split(sum[:8], ""), sum)
	dir := cb
	display := cb.Name()
	for i, name := range names {
		display = filepath.Join(display, name)
		next, err := subdirNoFollow(dir, display, name, 0o770, create)
		if i > 0 {
			_ = dir.Close()
		}
		if err != nil || next == nil {
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

// subdirNoFollow opens parent's directory name as a root of its own, making
// it at exactly mode when create is set and it is missing. A link there is
// refused, not followed, and so is a directory swapped for one between the
// check and the open. display names it in errors.
func subdirNoFollow(parent *os.Root, display, name string, mode os.FileMode, create bool) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	made := false
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err = parent.Mkdir(name, 0o700); err == nil {
			made = true
		} else if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("mkdir %s: %w", display, err)
		}
		fi, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, fmt.Errorf("lstat %s: %w", display, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to follow it", display)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", display)
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", display, err)
	}
	if after, err := sub.Stat("."); err != nil || !os.SameFile(fi, after) {
		_ = sub.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", display)
	}
	if made {
		if err := sub.Chmod(".", mode); err != nil {
			_ = sub.Close()
			return nil, fmt.Errorf("chmod %s: %w", display, err)
		}
	}
	return sub, nil
}

// regularNoFollow Lstats shard's name and refuses anything but a regular
// file; missing is (nil, nil).
func regularNoFollow(shard *os.Root, name string) (os.FileInfo, error) {
	fi, err := shard.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	display := filepath.Join(shard.Name(), name)
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to follow it", display)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", display)
	}
	return fi, nil
}

// openRegularNoFollow opens shard's existing regular file name with flag,
// refusing a link or a file swapped since the Lstat.
func openRegularNoFollow(shard *os.Root, name string, flag int) (*os.File, error) {
	fi, err := regularNoFollow(shard, name)
	if err != nil {
		return nil, err
	}
	if fi == nil {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(shard.Name(), name), Err: fs.ErrNotExist}
	}
	f, err := shard.OpenFile(name, flag|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if after, err := f.Stat(); err != nil || !os.SameFile(fi, after) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was being opened", filepath.Join(shard.Name(), name))
	}
	return f, nil
}

// shardHolds is head (indirector/file_bucket_file/file.rb:118-124): contents
// exists and paths records real.
func shardHolds(shard *os.Root, real string) (bool, error) {
	fi, err := regularNoFollow(shard, "contents")
	if err != nil || fi == nil {
		return false, err
	}
	paths, err := regularNoFollow(shard, "paths")
	if err != nil || paths == nil {
		return false, err
	}
	f, err := openRegularNoFollow(shard, "paths", os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return pathRecorded(f, real)
}

// pathRecorded is path_match (:132-140): a line of r, chomped, is path.
func pathRecorded(r io.Reader, path string) (bool, error) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		if strings.TrimSuffix(s.Text(), "\r") == path {
			return true, nil
		}
	}
	return false, s.Err()
}

// lockTimeout is exclusive_open's (file_system/file_impl.rb:52).
const lockTimeout = 300 * time.Second

// saveToShard is save_to_disk (indirector/file_bucket_file/file.rb:150-197)
// in shard: under an exclusive lock on paths (0640), store the content as
// contents (0440) unless an identical copy is there, which is touched, and
// append real to paths unless it is recorded.
func (b *backup) saveToShard(shard *os.Root, in *os.File, sum, real string) error {
	paths, err := shard.OpenFile("paths", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640)
	switch {
	case err == nil:
		if err := paths.Chmod(0o640); err != nil {
			_ = paths.Close()
			return err
		}
	case errors.Is(err, fs.ErrExist):
		if paths, err = openRegularNoFollow(shard, "paths", os.O_RDWR); err != nil {
			return err
		}
	default:
		return err
	}
	defer paths.Close()
	if err := lockExclusive(paths, lockTimeout); err != nil {
		return err
	}
	if b.log != nil {
		locked := filepath.Join(shard.Name(), "paths")
		b.log("debug", "Locked '"+locked+"'")
		defer b.log("debug", "Unlocked '"+locked+"'")
	}

	existing, err := regularNoFollow(shard, "contents")
	if err != nil {
		return err
	}
	switch {
	case existing == nil:
		if err := writeContents(shard, in, sum); err != nil {
			return err
		}
	default:
		identical, err := contentsHold(shard, sum)
		if err != nil {
			return err
		}
		if identical {
			// Puppet.info "FileBucket got a duplicate file", and the touch.
			now := time.Now()
			if err := shard.Chtimes("contents", now, now); err != nil {
				return err
			}
			break
		}
		// A sha256 match with different bytes (:172-179) is a collision
		// nobody has found; anything else is a corrupted backup, overwritten.
		b.warnings = append(b.warnings, ral.Warning{Source: "Puppet", Message: fmt.Sprintf(
			"Existing backup does not match its expected sum, {sha256}%s. Overwriting corrupted backup.", sum)})
		if err := writeContents(shard, in, sum); err != nil {
			return err
		}
	}

	if _, err := paths.Seek(0, io.SeekStart); err != nil {
		return err
	}
	recorded, err := pathRecorded(paths, real)
	if err != nil || recorded {
		return err
	}
	if _, err := paths.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	_, err = paths.WriteString(real + "\n")
	return err
}

// contentsHold reports whether shard's contents is the content whose
// sha256 is sum (verify_identical_file).
func contentsHold(shard *os.Root, sum string) (bool, error) {
	f, err := openRegularNoFollow(shard, "contents", os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == sum, nil
}

// writeContents is copy_bucket_file_to_contents_file (:259-265), which is
// replace_file(contents, 0o440): in's bytes, read again from its start, go to
// a new temp file in shard and are renamed over contents, which replaces a
// link rather than writing through it. Bytes that no longer hash to sum,
// because the file changed since it was summed, are discarded.
func writeContents(shard *os.Root, in *os.File, sum string) error {
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var tmp *os.File
	var tmpName string
	var err error
	for range 100 {
		tmpName = ".contents.voxagent" + rand.Text()
		tmp, err = shard.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	discard := func(cause error) error {
		_ = tmp.Close()
		_ = shard.Remove(tmpName)
		return cause
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		return discard(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return discard(fmt.Errorf("%s changed while it was being backed up", in.Name()))
	}
	if err := tmp.Sync(); err != nil {
		return discard(err)
	}
	if err := tmp.Chmod(0o440); err != nil {
		return discard(err)
	}
	if err := tmp.Close(); err != nil {
		_ = shard.Remove(tmpName)
		return err
	}
	if err := shard.Rename(tmpName, "contents"); err != nil {
		_ = shard.Remove(tmpName)
		return err
	}
	return nil
}

// lockExclusive is exclusive_open's lock: a non-blocking flock retried with
// a doubling wait, each capped at the time left, so the whole wait never
// exceeds timeout; then Puppet's timeout error.
func lockExclusive(f *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wait := time.Millisecond
	for {
		sc, err := f.SyscallConn()
		if err != nil {
			return err
		}
		var lockErr error
		if err := sc.Control(func(fd uintptr) {
			lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
		}); err != nil {
			return err
		}
		if lockErr == nil {
			return nil
		}
		if !errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return lockErr
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("Timeout waiting for exclusive lock on %s", f.Name())
		}
		time.Sleep(min(wait, left))
		wait *= 2
	}
}
