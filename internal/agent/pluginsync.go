package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/puppetapi"
	"github.com/voxagent/voxagent/internal/runlog"
)

// pluginsIgnore is Puppet's default pluginsignore.
var pluginsIgnore = []string{".svn", "CVS", ".git", ".hg"}

// contentHashes are the checksum types accepted as a basis for content.
// md5, mtime, ctime and none are refused.
var contentHashes = map[string]func() hash.Hash{
	"sha256": sha256.New,
	"sha384": sha512.New384,
	"sha512": sha512.New,
	"sha224": sha256.New224,
}

// LogFunc receives a Puppet-sourced log line of the run (level is Puppet's:
// "info", "notice", …), worded as Puppet words it.
type LogFunc func(level, message string)

// Pluginsync mirrors the plugins and pluginfacts mounts into libdir and
// pluginfactdest in Puppet's order, purging files the server no
// longer has. Before each mount it logs Puppet's "Retrieving pluginfacts" or
// "Retrieving plugin" (configurer/downloader.rb) through log, which may be
// nil. Both directories must exist; the caller checks them with
// checkTrusted first. Any failure is returned and must
// stop the run: nothing else falls back.
//
// Requests, as Puppet makes them: a non-recursive probe of plugins, then
// pluginfacts (metadata, content, purge), then plugins.
//
// A 404 for any metadata request, probe or recursive, is an error like any
// other (fail closed): nothing is purged and the local tree is left as
// it was. How the server answers for a mount with no content is not measured
// (every capture got 200); the integration gate
// measures it.
func Pluginsync(ctx context.Context, c *puppetapi.Client, environment, libdir, pluginfactdest string, log LogFunc) error {
	if log == nil {
		log = func(string, string) {}
	}
	_, err := c.FileMetadatas(ctx, "plugins", environment, puppetapi.MetadatasParams{
		Links: "manage", SourcePermissions: "ignore",
	})
	if err != nil {
		return fmt.Errorf("pluginsync: %w", err)
	}
	log("info", "Retrieving pluginfacts")
	if err := syncMount(ctx, c, environment, "pluginfacts", "use", pluginfactdest); err != nil {
		return err
	}
	log("info", "Retrieving plugin")
	return syncMount(ctx, c, environment, "plugins", "ignore", libdir)
}

// entry is one validated metadata entry below the mount root.
type entry struct {
	rel    string
	dir    bool
	mode   os.FileMode // files only
	hash   string      // checksum type, files only
	digest []byte      // files only
}

// syncMount mirrors one mount into dir.
func syncMount(ctx context.Context, c *puppetapi.Client, environment, mount, sourcePermissions, dir string) error {
	ms, err := c.FileMetadatas(ctx, mount, environment, puppetapi.MetadatasParams{
		Recurse: true, MaxFiles: -1, Links: "follow", SourcePermissions: sourcePermissions, Ignore: pluginsIgnore,
	})
	if err != nil {
		return fmt.Errorf("pluginsync %s: %w", mount, err)
	}
	entries, err := validateMetadata(ms, sourcePermissions == "use")
	if err != nil {
		return fmt.Errorf("pluginsync %s: %w", mount, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("pluginsync %s: %w", mount, err)
	}
	defer root.Close()

	// Sorted, a directory comes before everything under it.
	for _, e := range entries {
		if e.dir {
			if err := ensureDir(root, e.rel); err != nil {
				return fmt.Errorf("pluginsync %s: %w", mount, err)
			}
		}
	}
	fetched, kept := 0, 0
	for _, e := range entries {
		if !e.dir {
			_, got, err := syncFile(ctx, c, environment, mount+"/"+e.rel, root, e)
			if err != nil {
				return fmt.Errorf("pluginsync %s: %s: %w", mount, e.rel, err)
			}
			if got {
				fetched++
			} else {
				kept++
			}
		}
	}
	want := make(map[string]bool, len(entries))
	for _, e := range entries {
		want[e.rel] = e.dir
	}
	if err := purge(root, ".", want); err != nil {
		return fmt.Errorf("pluginsync %s: purge: %w", mount, err)
	}
	runlog.Debugf(ctx, "voxagent", "pluginsync %s: %d fetched, %d unchanged", mount, fetched, kept)
	return nil
}

// validateMetadata checks the whole metadata set before anything touches the
// disk and returns the entries below the root, sorted by relative path.
// useMode applies the server's execute bit (pluginfacts, source_permissions
// use); otherwise every file is 0644.
func validateMetadata(ms []puppetapi.Metadata, useMode bool) ([]entry, error) {
	seen := make(map[string]bool, len(ms)) // relative path -> is a directory
	var entries []entry
	for _, m := range ms {
		switch m.Type {
		case "directory", "file":
		case "link":
			// links=follow: the server resolves links, so one here is refused.
			return nil, fmt.Errorf("relative_path %q: link metadata refused", m.RelativePath)
		default:
			return nil, fmt.Errorf("relative_path %q: unknown type %q", m.RelativePath, m.Type)
		}
		if m.RelativePath == "." {
			if m.Type != "directory" {
				return nil, fmt.Errorf("mount root is a %s, not a directory", m.Type)
			}
			continue
		}
		if err := validRelPath(m.RelativePath); err != nil {
			return nil, err
		}
		if _, dup := seen[m.RelativePath]; dup {
			return nil, fmt.Errorf("relative_path %q listed twice", m.RelativePath)
		}
		e := entry{rel: m.RelativePath, dir: m.Type == "directory"}
		seen[e.rel] = e.dir
		if !e.dir {
			typ, digest, err := parseChecksum(m.Checksum.Type, m.Checksum.Value)
			if err != nil {
				return nil, fmt.Errorf("relative_path %q: %w", m.RelativePath, err)
			}
			e.hash, e.digest = typ, digest
			e.mode = 0o644
			if useMode && m.Mode&0o111 != 0 {
				e.mode = 0o755
			}
		}
		entries = append(entries, e)
	}
	// Every parent must be listed as a directory: a recursive listing always
	// includes them, and a file listed as a parent would make the writes
	// undo each other.
	for _, e := range entries {
		for p := path.Dir(e.rel); p != "."; p = path.Dir(p) {
			if isDir, ok := seen[p]; !ok || !isDir {
				return nil, fmt.Errorf("relative_path %q: parent %q is not listed as a directory", e.rel, p)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, nil
}

// parseChecksum decodes a metadata checksum, "{sha256}<hex>" with type
// "sha256".
func parseChecksum(typ, value string) (string, []byte, error) {
	newHash, ok := contentHashes[typ]
	if !ok {
		return "", nil, fmt.Errorf("checksum type %q refused: only sha256, sha384, sha512 and sha224 are accepted", typ)
	}
	hexDigest, ok := strings.CutPrefix(value, "{"+typ+"}")
	if !ok {
		return "", nil, fmt.Errorf("checksum %q does not start with {%s}", value, typ)
	}
	digest, err := hex.DecodeString(hexDigest)
	if err != nil || len(digest) != newHash().Size() {
		return "", nil, fmt.Errorf("checksum %q is not a %s digest", value, typ)
	}
	return typ, digest, nil
}

// syncFile makes e.rel match e. It reports how many content bytes it downloaded
// and whether it downloaded at all: an unchanged file is neither.
func syncFile(ctx context.Context, c *puppetapi.Client, environment, mountPath string, root *os.Root, e entry) (int64, bool, error) {
	same, err := unchanged(root, e)
	if err != nil || same {
		return 0, false, err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return 0, false, err
	}
	tmp := path.Join(path.Dir(e.rel), ".voxagent-"+hex.EncodeToString(rnd[:])+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, false, err
	}
	placed := false
	defer func() {
		if !placed {
			f.Close()
			root.Remove(tmp)
		}
	}()
	h := contentHashes[e.hash]()
	size := &countWriter{}
	if err := c.FileContent(ctx, mountPath, environment, io.MultiWriter(f, h, size)); err != nil {
		return 0, false, err
	}
	if got := h.Sum(nil); !bytes.Equal(got, e.digest) {
		return 0, false, fmt.Errorf("content %s %x does not match the metadata checksum %x", e.hash, got, e.digest)
	}
	// fchmod: exact mode, independent of the umask.
	if err := f.Chmod(e.mode); err != nil {
		return 0, false, err
	}
	if err := f.Sync(); err != nil {
		return 0, false, err
	}
	if err := f.Close(); err != nil {
		return 0, false, err
	}
	// rename replaces a file or a symlink (the link itself, never its
	// target) but not a directory.
	if fi, err := root.Lstat(e.rel); err == nil && fi.IsDir() {
		if err := root.RemoveAll(e.rel); err != nil {
			return 0, false, err
		}
	}
	if err := root.Rename(tmp, e.rel); err != nil {
		return 0, false, err
	}
	placed = true
	return size.n, true, nil
}

// countWriter counts the bytes written to it.
type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// unchanged reports whether e.rel is already a regular file with e's
// digest, fixing its mode if that is all that differs. A symlink, a
// non-regular file or a file with other hard links is never unchanged: it is
// replaced by rename, so nothing outside the root is read or chmodded.
func unchanged(root *os.Root, e entry) (bool, error) {
	lfi, err := root.Lstat(e.rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !lfi.Mode().IsRegular() {
		return false, nil
	}
	if st, ok := lfi.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return false, nil
	}
	f, err := root.Open(e.rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(lfi, fi) {
		return false, nil
	}
	h := contentHashes[e.hash]()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	if !bytes.Equal(h.Sum(nil), e.digest) {
		return false, nil
	}
	if fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != e.mode {
		if err := f.Chmod(e.mode); err != nil {
			return false, err
		}
	}
	return true, nil
}

// purge removes every entry under dir that is not in want (relative path ->
// is a directory), except pluginsIgnore names, which Puppet's recursion skips
// at every level. It descends only into real directories, never through a
// symlink; RemoveAll removes a symlink itself, not its target.
func purge(root *os.Root, dir string, want map[string]bool) error {
	lfi, err := root.Lstat(dir)
	if err != nil {
		return err
	}
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	fi, err := d.Stat()
	if err != nil {
		return err
	}
	if !lfi.IsDir() || !os.SameFile(lfi, fi) {
		return fmt.Errorf("%s changed while purging", dir)
	}
	des, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, de := range des {
		if slices.Contains(pluginsIgnore, de.Name()) {
			continue
		}
		rel := path.Join(dir, de.Name())
		isDir, ok := want[rel]
		switch {
		case !ok:
			if err := root.RemoveAll(rel); err != nil {
				return err
			}
		case isDir != de.IsDir():
			// Only a concurrent change can cause this: the write phase
			// just made every listed path its listed type.
			return fmt.Errorf("%s changed type during pluginsync", rel)
		case isDir:
			if err := purge(root, rel, want); err != nil {
				return err
			}
		}
	}
	return nil
}
