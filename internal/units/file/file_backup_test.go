package file

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
)

// The cases are those measured against Puppet 8.10.0.

var backupEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// seedOld writes "old\n" at path with mode and Puppet's measured mtime.
func seedOld(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	mustWrite(t, path, "old\n")
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, backupEpoch, backupEpoch); err != nil {
		t.Fatal(err)
	}
}

// applyBackup applies r through a File whose vardir is vardir and fails the
// test on a resource error.
func applyBackup(t *testing.T, vardir string, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning) {
	t.Helper()
	changes, warnings, err := File{Vardir: vardir}.Apply(context.Background(), r, noop)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return changes, warnings
}

// shardDir is where the bucket keeps content whose sha256 is sum
// (indirector/file_bucket_file/file.rb:219-226).
func shardDir(vardir, sum string) string {
	parts := []string{vardir, "clientbucket"}
	for _, c := range sum[:8] {
		parts = append(parts, string(c))
	}
	return filepath.Join(append(parts, sum)...)
}

func sha(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// siblings lists dir's entries other than keep.
func siblings(t *testing.T, dir string, keep ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// applyContentChange applies r and asserts a backup left the report as case
// 1's: one successful content change, no path warning, no run warning.
func applyContentChange(t *testing.T, vardir string, r catalog.Resource) {
	t.Helper()
	changes, warnings := applyBackup(t, vardir, r, false)
	if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err != nil || len(changes[0].PathWarnings) != 0 {
		t.Errorf("changes = %+v, want one successful content change", changes)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

// Case 1 and case 5: false, "false" and no backup at all write nothing.
func TestFileBackupFalseWritesNothing(t *testing.T) {
	for _, v := range []any{false, "false", nil} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			dir, vardir := t.TempDir(), t.TempDir()
			path := filepath.Join(dir, "f")
			seedOld(t, path, 0o644)
			params := map[string]any{"content": "new\n"}
			if v != nil {
				params["backup"] = v
			}
			applyContentChange(t, vardir, raltest.FileRes(path, params))
			if got := siblings(t, dir, "f"); len(got) != 0 {
				t.Errorf("siblings = %v, want none", got)
			}
			if got := siblings(t, vardir); len(got) != 0 {
				t.Errorf("vardir holds %v, want nothing", got)
			}
		})
	}
}

// Case 2a: a copy of the old file, its mode and mtime preserved (cp_r
// preserve), made before the write.
func TestFileBackupSuffixOnContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	seedOld(t, path, 0o640)
	applyContentChange(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": ".bak"}))
	if got := readString(t, path); got != "new\n" {
		t.Errorf("f = %q", got)
	}
	bak := path + ".bak"
	if got := readString(t, bak); got != "old\n" {
		t.Errorf("f.bak = %q, want old", got)
	}
	if got := permOf(t, bak); got != 0o640 {
		t.Errorf("f.bak mode = %o, want 640", got)
	}
	fi, _ := os.Stat(bak)
	if !fi.ModTime().Equal(backupEpoch) {
		t.Errorf("f.bak mtime = %v, want %v", fi.ModTime(), backupEpoch)
	}
}

// Case 2b: a stale backup is unlinked first. A symlink there is unlinked,
// never written through; a directory there is refused.
func TestFileBackupSuffixReplacesAStaleBackup(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		seedOld(t, path, 0o644)
		mustWrite(t, path+".bak", "prev\n")
		applyContentChange(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": ".bak"}))
		if got := readString(t, path+".bak"); got != "old\n" {
			t.Errorf("f.bak = %q, want old", got)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir, elsewhere := t.TempDir(), t.TempDir()
		path, victim := filepath.Join(dir, "f"), filepath.Join(elsewhere, "victim")
		seedOld(t, path, 0o644)
		mustWrite(t, victim, "untouched\n")
		if err := os.Symlink(victim, path+".bak"); err != nil {
			t.Fatal(err)
		}
		applyContentChange(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": ".bak"}))
		if got := readString(t, victim); got != "untouched\n" {
			t.Errorf("the link's target = %q; the backup was written through the link", got)
		}
		fi, err := os.Lstat(path + ".bak")
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("f.bak = %v, %v; want a regular file in place of the link", fi, err)
		}
		if got := readString(t, path+".bak"); got != "old\n" {
			t.Errorf("f.bak = %q, want old", got)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		seedOld(t, path, 0o644)
		mustMkdir(t, path+".bak")
		changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": ".bak"}), false)
		want := "Will not remove directory backup " + path + ".bak; use a filebucket"
		if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err == nil || changes[0].Err.Error() != want {
			t.Fatalf("changes = %+v, want content failing with %q", changes, want)
		}
		if got := readString(t, path); got != "old\n" {
			t.Errorf("f = %q; a failed backup must leave it", got)
		}
	})
}

// Case 2c: ensure => absent backs up before it removes.
func TestFileBackupSuffixOnAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	seedOld(t, path, 0o644)
	changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"ensure": "absent", "backup": ".bak"}), false)
	if len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err != nil || changes[0].Message != "removed" {
		t.Fatalf("changes = %+v", changes)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("f still there: %v", err)
	}
	if got := readString(t, path+".bak"); got != "old\n" {
		t.Errorf("f.bak = %q, want old", got)
	}
}

// Cases 2d and 2e: neither a mode-only change nor a created file is backed up.
func TestFileBackupNotOnModeOnlyOrCreate(t *testing.T) {
	t.Run("mode only", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		seedOld(t, path, 0o600)
		changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "old\n", "mode": "0644", "backup": ".bak"}), false)
		if len(changes) != 1 || changes[0].Property != "mode" {
			t.Fatalf("changes = %+v, want one mode change", changes)
		}
		if got := siblings(t, dir, "f"); len(got) != 0 {
			t.Errorf("siblings = %v, want none", got)
		}
	})
	t.Run("create", func(t *testing.T) {
		dir, vardir := t.TempDir(), t.TempDir()
		path := filepath.Join(dir, "f")
		for _, b := range []string{".bak", "puppet"} {
			applyBackup(t, vardir, raltest.FileRes(path, map[string]any{"content": "new\n", "backup": b}), false)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if got := siblings(t, dir); len(got) != 0 {
			t.Errorf("siblings = %v, want none", got)
		}
		if got := siblings(t, vardir); len(got) != 0 {
			t.Errorf("vardir holds %v, want nothing", got)
		}
	})
}

// Case 5's munge table: true and "true" are the suffix .puppet-bak.
func TestFileBackupTrueIsPuppetBak(t *testing.T) {
	for _, v := range []any{true, "true"} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "f")
			seedOld(t, path, 0o644)
			applyContentChange(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": v}))
			if got := readString(t, path+".puppet-bak"); got != "old\n" {
				t.Errorf("f.puppet-bak = %q, want old", got)
			}
		})
	}
}

// Case 3: the default bucket keeps the old bytes under the digest's shard,
// contents 0440, paths 0640 holding the real path, created directories 0770;
// the clientbucket itself is 0750, as Puppet's settings create it.
func TestFileBackupBucketOnContentChange(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "f")
	seedOld(t, path, 0o644)
	applyContentChange(t, vardir, raltest.FileRes(path, map[string]any{"content": "new\n", "backup": "puppet"}))
	if got := readString(t, path); got != "new\n" {
		t.Errorf("f = %q", got)
	}
	shard := shardDir(vardir, sha("old\n"))
	if got := readString(t, filepath.Join(shard, "contents")); got != "old\n" {
		t.Errorf("contents = %q", got)
	}
	if got := readString(t, filepath.Join(shard, "paths")); got != realPath(t, path)+"\n" {
		t.Errorf("paths = %q, want %q", got, realPath(t, path)+"\n")
	}
	if got := permOf(t, filepath.Join(shard, "contents")); got != 0o440 {
		t.Errorf("contents mode = %o, want 440", got)
	}
	if got := permOf(t, filepath.Join(shard, "paths")); got != 0o640 {
		t.Errorf("paths mode = %o, want 640", got)
	}
	if got := permOf(t, filepath.Join(vardir, "clientbucket")); got != 0o750 {
		t.Errorf("clientbucket mode = %o, want 750", got)
	}
	for d := shard; d != filepath.Join(vardir, "clientbucket"); d = filepath.Dir(d) {
		if got := permOf(t, d); got != 0o770 {
			t.Errorf("%s mode = %o, want 770", d, got)
		}
	}
	if got := siblings(t, dir, "f"); len(got) != 0 {
		t.Errorf("siblings = %v, want none", got)
	}
}

// Case 3, repeats: the same content from the same path touches nothing
// (dipper.rb:51-53); from a new path it touches contents and appends the path.
func TestFileBackupBucketRepeats(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	f, g := filepath.Join(dir, "f"), filepath.Join(dir, "g")
	seedOld(t, f, 0o644)
	applyBackup(t, vardir, raltest.FileRes(f, map[string]any{"content": "new\n", "backup": "puppet"}), false)
	shard := shardDir(vardir, sha("old\n"))
	contents := filepath.Join(shard, "contents")
	if err := os.Chtimes(contents, backupEpoch, backupEpoch); err != nil {
		t.Fatal(err)
	}

	seedOld(t, f, 0o644)
	applyBackup(t, vardir, raltest.FileRes(f, map[string]any{"content": "new\n", "backup": "puppet"}), false)
	if got := readString(t, filepath.Join(shard, "paths")); got != realPath(t, f)+"\n" {
		t.Errorf("same path: paths = %q", got)
	}
	if fi, _ := os.Stat(contents); !fi.ModTime().Equal(backupEpoch) {
		t.Errorf("same path: contents mtime = %v; it must not be touched", fi.ModTime())
	}

	seedOld(t, g, 0o644)
	applyBackup(t, vardir, raltest.FileRes(g, map[string]any{"content": "new\n", "backup": "puppet"}), false)
	if got, want := readString(t, filepath.Join(shard, "paths")), realPath(t, f)+"\n"+realPath(t, g)+"\n"; got != want {
		t.Errorf("new path: paths = %q, want %q", got, want)
	}
	if fi, _ := os.Stat(contents); fi.ModTime().Equal(backupEpoch) {
		t.Error("new path: contents was not touched")
	}
	if got := readString(t, contents); got != "old\n" {
		t.Errorf("contents = %q", got)
	}
}

// Case 3c: ensure => absent into the bucket.
func TestFileBackupBucketOnAbsent(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "f")
	seedOld(t, path, 0o644)
	changes, _ := applyBackup(t, vardir, raltest.FileRes(path, map[string]any{"ensure": "absent", "backup": "puppet"}), false)
	if len(changes) != 1 || changes[0].Err != nil || changes[0].Message != "removed" {
		t.Fatalf("changes = %+v", changes)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("f still there: %v", err)
	}
	shard := shardDir(vardir, sha("old\n"))
	if got := readString(t, filepath.Join(shard, "contents")); got != "old\n" {
		t.Errorf("contents = %q", got)
	}
	if got := readString(t, filepath.Join(shard, "paths")); got != realPath(t, dir)+"/f\n" {
		t.Errorf("paths = %q", got)
	}
}

// Case 4: a bucket name other than puppet names a filebucket resource, which
// is not implemented, so the backup fails as Puppet's does when the resource is
// missing; the file is left as it was.
func TestFileBackupUnknownBucketFails(t *testing.T) {
	const want = "Could not find filebucket nosuchbucket specified in backup"
	t.Run("content", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		seedOld(t, path, 0o644)
		changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"content": "new\n", "backup": "nosuchbucket"}), false)
		if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err == nil || changes[0].Err.Error() != want {
			t.Fatalf("changes = %+v, want content failing with %q", changes, want)
		}
		if changes[0].Was != checksum([]byte("old\n")) || changes[0].Is != checksum([]byte("new\n")) {
			t.Errorf("transition = %v -> %v", changes[0].Was, changes[0].Is)
		}
		if got := readString(t, path); got != "old\n" {
			t.Errorf("f = %q, want it unchanged", got)
		}
		if got := siblings(t, dir, "f"); len(got) != 0 {
			t.Errorf("siblings = %v", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		seedOld(t, path, 0o644)
		changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"ensure": "absent", "backup": "nosuchbucket"}), false)
		if len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err == nil || changes[0].Err.Error() != want {
			t.Fatalf("changes = %+v, want ensure failing with %q", changes, want)
		}
		if got := readString(t, path); got != "old\n" {
			t.Errorf("f = %q, want it kept", got)
		}
	})
}

// seedTree makes case 6's directory: d/y ("a\n"), d/sub/x ("b\n"), plus an
// empty directory, a link to a file outside and a dangling link.
func seedTree(t *testing.T, d, outside string) {
	t.Helper()
	mustMkdir(t, d)
	mustMkdir(t, filepath.Join(d, "sub"))
	mustMkdir(t, filepath.Join(d, "empty"))
	mustWrite(t, filepath.Join(d, "y"), "a\n")
	mustWrite(t, filepath.Join(d, "sub", "x"), "b\n")
	mustWrite(t, outside, "c\n")
	if err := os.Symlink(outside, filepath.Join(d, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(d, "nowhere"), filepath.Join(d, "dang")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(d, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
}

// Case 6a: with force a directory's whole tree is copied, links as links.
func TestFileBackupSuffixCopiesADirectoryTree(t *testing.T) {
	dir := t.TempDir()
	d, outside := filepath.Join(dir, "d"), filepath.Join(t.TempDir(), "other")
	seedTree(t, d, outside)
	changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(d, map[string]any{
		"ensure": "file", "content": "new\n", "force": true, "backup": ".bak"}), false)
	if len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err != nil {
		t.Fatalf("changes = %+v", changes)
	}
	if got := readString(t, d); got != "new\n" {
		t.Errorf("d = %q", got)
	}
	bak := d + ".bak"
	if got := readString(t, filepath.Join(bak, "y")); got != "a\n" {
		t.Errorf("d.bak/y = %q", got)
	}
	if got := readString(t, filepath.Join(bak, "sub", "x")); got != "b\n" {
		t.Errorf("d.bak/sub/x = %q", got)
	}
	if got := permOf(t, filepath.Join(bak, "sub")); got != 0o750 {
		t.Errorf("d.bak/sub mode = %o, want 750", got)
	}
	if fi, err := os.Stat(filepath.Join(bak, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("d.bak/empty = %v, %v", fi, err)
	}
	for l, want := range map[string]string{"lnk": outside, "dang": filepath.Join(d, "nowhere")} {
		if got, err := os.Readlink(filepath.Join(bak, l)); err != nil || got != want {
			t.Errorf("d.bak/%s links to %q (%v), want %q", l, got, err, want)
		}
	}
}

// Case 6b: into the bucket every regular file of the tree goes, each under
// its own digest with its own real path; a link to a file is backed up as the
// file it reaches (File.file? follows, measured); empty directories and
// dangling links are not.
func TestFileBackupBucketBacksUpEachFileOfADirectory(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	d, outside := filepath.Join(dir, "d"), filepath.Join(t.TempDir(), "other")
	seedTree(t, d, outside)
	// The real paths, taken while the tree is still there.
	reals := map[string]string{
		"a\n": realPath(t, filepath.Join(d, "y")), "b\n": realPath(t, filepath.Join(d, "sub", "x")),
		"c\n": realPath(t, outside),
	}
	changes, _ := applyBackup(t, vardir, raltest.FileRes(d, map[string]any{
		"ensure": "file", "content": "new\n", "force": true, "backup": "puppet"}), false)
	if len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("changes = %+v", changes)
	}
	for content, real := range reals {
		shard := shardDir(vardir, sha(content))
		if got := readString(t, filepath.Join(shard, "contents")); got != content {
			t.Errorf("%q: contents = %q", content, got)
		}
		want := real + "\n"
		if got := readString(t, filepath.Join(shard, "paths")); got != want {
			t.Errorf("%q: paths = %q, want %q", content, got, want)
		}
	}
	var files int
	if err := filepath.WalkDir(filepath.Join(vardir, "clientbucket"), func(_ string, e os.DirEntry, err error) error {
		if err == nil && e.Name() == "contents" {
			files++
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if files != 3 {
		t.Errorf("%d contents files, want 3", files)
	}
}

// A top-level link is copied as the file it reaches (cp_r dereferences its
// root, measured), and the link's target is not written.
func TestFileBackupSuffixOfALinkCopiesItsTarget(t *testing.T) {
	dir := t.TempDir()
	path, target := filepath.Join(dir, "l"), filepath.Join(dir, "tgt")
	seedOld(t, target, 0o600)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	applyBackup(t, t.TempDir(), raltest.FileRes(path, map[string]any{"ensure": "file", "content": "new\n", "backup": ".bak"}), false)
	fi, err := os.Lstat(path + ".bak")
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("l.bak = %v, %v; want a regular 0600 file", fi, err)
	}
	if got := readString(t, path+".bak"); got != "old\n" {
		t.Errorf("l.bak = %q", got)
	}
	if got := readString(t, target); got != "old\n" {
		t.Errorf("tgt = %q; it must be untouched", got)
	}
}

// Case 6c: without force a directory cannot be backed up, which is warned at
// the resource each time remove_existing runs (twice, I-3).
func TestFileBackupWarnsForADirectoryWithoutForce(t *testing.T) {
	d := filepath.Join(t.TempDir(), "d")
	mustMkdir(t, d)
	changes, _ := applyBackup(t, t.TempDir(), raltest.FileRes(d, map[string]any{"ensure": "file", "content": "new\n", "backup": ".bak"}), false)
	want := []string{"Could not back up file of type directory", "Could not back up file of type directory"}
	if len(changes) != 1 || !slices.Equal(changes[0].PathWarnings, want) || changes[0].Err == nil {
		t.Fatalf("changes = %+v, want a failed ensure with path warnings %q", changes, want)
	}
	if _, err := os.Lstat(d + ".bak"); !os.IsNotExist(err) {
		t.Errorf("d.bak exists: %v", err)
	}
}

// Under --noop nothing is written, by either kind of backup.
func TestFileBackupNoopWritesNothing(t *testing.T) {
	for _, b := range []string{".bak", "puppet"} {
		for _, ensure := range []string{"file", "absent"} {
			t.Run(b+" "+ensure, func(t *testing.T) {
				dir, vardir := t.TempDir(), t.TempDir()
				path := filepath.Join(dir, "f")
				seedOld(t, path, 0o644)
				applyBackup(t, vardir, raltest.FileRes(path, map[string]any{"ensure": ensure, "content": "new\n", "backup": b}), true)
				if got := siblings(t, dir, "f"); len(got) != 0 {
					t.Errorf("siblings = %v", got)
				}
				if got := siblings(t, vardir); len(got) != 0 {
					t.Errorf("vardir holds %v", got)
				}
			})
		}
	}
}

// Puppet's munge refuses a value that is neither a boolean nor a string
// (type/file.rb:141), while converting the catalog; a list is its first
// element (:131).
func TestFileValidateBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	for _, tc := range []struct {
		value any
		want  string
	}{
		{float64(5), "Parameter backup failed on File[" + path + "]: Invalid backup type 5"},
		{5.5, "Parameter backup failed on File[" + path + "]: Invalid backup type 5.5"},
		{[]any{float64(5)}, "Parameter backup failed on File[" + path + "]: Invalid backup type 5"},
		{[]any{}, "Parameter backup failed on File[" + path + "]: Invalid backup type nil"},
		{[]any{".bak", "x"}, ""},
		{".bak", ""}, {"puppet", ""}, {"main", ""}, {true, ""}, {false, ""},
	} {
		err := (File{}).Validate(raltest.FileRes(path, map[string]any{"backup": tc.value}))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("backup => %v: %v, want accepted", tc.value, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("backup => %v: %v, want %q", tc.value, err, tc.want)
		}
	}
	if _, ok := (File{}).Attrs()["backup"]; !ok {
		t.Error("backup is not among File's attributes")
	}
}

// The bucket is written only under the vardir and never through a symlink:
// a clientbucket or a shard directory that is a link fails the resource, and
// nothing is written where the link points.
func TestFileBackupBucketRefusesSymlinks(t *testing.T) {
	sum := sha("old\n")
	for _, tc := range []struct {
		name string
		link func(vardir string) string // the path to make a link
	}{
		{"clientbucket", func(v string) string { return filepath.Join(v, "clientbucket") }},
		{"shard", func(v string) string { return filepath.Join(v, "clientbucket", sum[:1], sum[1:2]) }},
		{"digest", func(v string) string { return shardDir(v, sum) }},
		{"paths", func(v string) string { return filepath.Join(shardDir(v, sum), "paths") }},
		{"contents", func(v string) string { return filepath.Join(shardDir(v, sum), "contents") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, vardir, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
			path := filepath.Join(dir, "f")
			seedOld(t, path, 0o644)
			link := tc.link(vardir)
			if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
			changes, _ := applyBackup(t, vardir, raltest.FileRes(path, map[string]any{"content": "new\n", "backup": "puppet"}), false)
			if len(changes) != 1 || changes[0].Err == nil ||
				!strings.HasPrefix(changes[0].Err.Error(), "Could not back up "+path+": ") ||
				!strings.Contains(changes[0].Err.Error(), "symlink") {
				t.Fatalf("changes = %+v, want content failing with a symlink refusal", changes)
			}
			if got := siblings(t, elsewhere); len(got) != 0 {
				t.Errorf("written through the link: %v", got)
			}
			if got := readString(t, path); got != "old\n" {
				t.Errorf("f = %q; a failed backup must leave it", got)
			}
		})
	}
}

// A contents file that does not hold its digest's bytes is overwritten, with
// Puppet's warning (indirector/file_bucket_file/file.rb:180-185).
func TestFileBackupBucketOverwritesACorruptedBackup(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "f")
	seedOld(t, path, 0o644)
	shard := shardDir(vardir, sha("old\n"))
	if err := os.MkdirAll(shard, 0o770); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(shard, "contents"), "corrupt")
	_, warnings := applyBackup(t, vardir, raltest.FileRes(path, map[string]any{"content": "new\n", "backup": "puppet"}), false)
	want := ral.Warning{Source: "Puppet", Message: "Existing backup does not match its expected sum, {sha256}" + sha("old\n") + ". Overwriting corrupted backup."}
	if len(warnings) != 1 || warnings[0] != want {
		t.Errorf("warnings = %+v, want %+v", warnings, want)
	}
	if got := readString(t, filepath.Join(shard, "contents")); got != "old\n" {
		t.Errorf("contents = %q", got)
	}
	if got := permOf(t, filepath.Join(shard, "contents")); got != 0o440 {
		t.Errorf("contents mode = %o", got)
	}
}

// A generated child inherits backup (newchild) and its suffix copy is written
// through the rootFS of the recursed directory.
func TestFileBackupOfAGeneratedChild(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "c")
	seedOld(t, child, 0o644)
	parent := raltest.FileRes(dir, map[string]any{"ensure": "directory", "recurse": true, "purge": true, "backup": ".bak"})
	r := raltest.FileRes(child, map[string]any{"ensure": "absent", "backup": ".bak"})
	r.GeneratedFrom = &parent
	changes, _ := applyBackup(t, t.TempDir(), r, false)
	if len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("changes = %+v", changes)
	}
	if got := readString(t, child+".bak"); got != "old\n" {
		t.Errorf("c.bak = %q", got)
	}

	// Confinement: neither a child that links out of the tree nor a suffix
	// that climbs out of it reads or writes outside the recursed directory.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	mustWrite(t, secret, "secret\n")
	for _, tc := range []struct{ name, suffix string }{
		{"a link out of the tree", ".bak"},
		{"a suffix out of the tree", ".bak/../../" + filepath.Base(outside) + "/escaped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := filepath.Join(dir, "esc")
			_ = os.Remove(c)
			if tc.suffix == ".bak" {
				if err := os.Symlink(secret, c); err != nil {
					t.Fatal(err)
				}
			} else {
				seedOld(t, c, 0o644)
			}
			r := raltest.FileRes(c, map[string]any{"ensure": "absent", "backup": tc.suffix})
			r.GeneratedFrom = &parent
			changes, _ := applyBackup(t, t.TempDir(), r, false)
			if len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("changes = %+v, want the backup refused", changes)
			}
			if _, err := os.Lstat(c); err != nil {
				t.Errorf("%s was removed although its backup was refused: %v", c, err)
			}
			if _, err := os.Lstat(c + ".bak"); !os.IsNotExist(err) {
				t.Errorf("esc.bak exists: %v", err)
			}
			if got := siblings(t, outside, "secret"); len(got) != 0 {
				t.Errorf("written outside the tree: %v", got)
			}
		})
	}
}

// swapOnOpenFS replaces path with a link to swapTo just before path is
// opened: the window between the stat the backup copies metadata from and
// the open it copies bytes from.
type swapOnOpenFS struct {
	osFS
	path, swapTo string
}

func (s swapOnOpenFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if name == s.path {
		if err := os.Remove(s.path); err != nil {
			return nil, err
		}
		if err := os.Symlink(s.swapTo, s.path); err != nil {
			return nil, err
		}
	}
	return s.osFS.OpenFile(name, flag, perm)
}

// A path swapped for a link to another file after it was stat'ed is not
// copied: the copy would carry the other file's bytes under the first
// file's owner and mode.
func TestFileBackupSuffixRefusesAPathSwappedBeforeTheOpen(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	path, secret := filepath.Join(dir, "f"), filepath.Join(elsewhere, "secret")
	seedOld(t, path, 0o644)
	mustWrite(t, secret, "secret\n")
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	bk := &backup{value: ".bak"}
	_, err = bk.existing(swapOnOpenFS{path: path, swapTo: secret}, path, fi)
	if err == nil || !strings.HasPrefix(err.Error(), "Could not back "+path+" up: ") {
		t.Fatalf("existing = %v, want the backup refused", err)
	}
	if _, err := os.Lstat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("f.bak exists (%v); it may hold the swapped-in file's bytes", err)
	}
}

// A tree copy that fails part way leaves no partial <path><suffix>, so the
// next run is not refused by "Will not remove directory backup".
func TestFileBackupSuffixRemovesAPartialTreeCopy(t *testing.T) {
	d := filepath.Join(t.TempDir(), "d")
	mustMkdir(t, d)
	mustWrite(t, filepath.Join(d, "a"), "a\n")
	if err := syscall.Mkfifo(filepath.Join(d, "p"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := raltest.FileRes(d, map[string]any{"ensure": "file", "content": "new\n", "force": true, "backup": ".bak"})
	for run := 1; run <= 2; run++ {
		changes, _ := applyBackup(t, t.TempDir(), r, false)
		want := "Could not back " + d + " up: cannot copy " + filepath.Join(d, "p") + " of type fifo"
		if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
			t.Fatalf("run %d: changes = %+v, want %q", run, changes, want)
		}
		if _, err := os.Lstat(d + ".bak"); !os.IsNotExist(err) {
			t.Errorf("run %d: a partial d.bak was left: %v", run, err)
		}
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			t.Errorf("run %d: d = %v, %v; want the directory kept", run, fi, err)
		}
	}
}

// exclusive_open waits at most its timeout in all, however the doubling
// waits fall.
func TestLockExclusiveWaitsNoLongerThanTheTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paths")
	mustWrite(t, path, "")
	holder, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Doubling from 1ms, an uncapped wait would reach 255ms before giving up.
	const timeout = 130 * time.Millisecond
	start := time.Now()
	err = lockExclusive(f, timeout)
	elapsed := time.Since(start)
	if err == nil || err.Error() != "Timeout waiting for exclusive lock on "+path {
		t.Fatalf("lockExclusive = %v", err)
	}
	if elapsed < timeout || elapsed > timeout+60*time.Millisecond {
		t.Errorf("waited %v, want about %v", elapsed, timeout)
	}
}

// A map has no Ruby rendering here: JSON decoding has lost its key order,
// which Ruby's Hash#inspect prints in (D-46). It is refused all the same.
func TestFileValidateBackupRefusesAMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	err := (File{}).Validate(raltest.FileRes(path, map[string]any{"backup": map[string]any{"k": "v"}}))
	want := "Parameter backup failed on File[" + path + "]: Invalid backup type map[k:v]"
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v, want %q", err, want)
	}
}

// Find.find walks names in sorted order, so content met twice in a tree
// records its paths in that order: here a link named before the file.
func TestFileBackupBucketWalksATreeInSortedOrder(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	d, outside := filepath.Join(dir, "d"), filepath.Join(t.TempDir(), "real")
	mustMkdir(t, d)
	mustWrite(t, outside, "a\n")
	var want string
	for _, name := range []string{"z9", "y8", "x7", "w6", "v5", "u4", "t3", "s2", "r1"} {
		mustWrite(t, filepath.Join(d, name), "a\n")
	}
	if err := os.Symlink(outside, filepath.Join(d, "a0")); err != nil {
		t.Fatal(err)
	}
	want = realPath(t, outside) + "\n"
	for _, name := range []string{"r1", "s2", "t3", "u4", "v5", "w6", "x7", "y8", "z9"} {
		want += realPath(t, filepath.Join(d, name)) + "\n"
	}
	applyBackup(t, vardir, raltest.FileRes(d, map[string]any{"ensure": "absent", "force": true, "backup": "puppet"}), false)
	if got := readString(t, filepath.Join(shardDir(vardir, sha("a\n")), "paths")); got != want {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

// A directory below the copied tree swapped for a link once it has been
// checked -- between its lstat and its open, or after it is opened -- to a
// private directory outside or to another directory in the tree, never has
// the swapped-in directory's entries copied: the copy is refused, or holds
// only the original entries.
func TestFileBackupSuffixNeverCopiesASwappedInSubdirectory(t *testing.T) {
	for _, phase := range []string{"lstat", "open"} {
		for _, inside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s inside=%v", phase, inside), func(t *testing.T) {
				dir := t.TempDir()
				d := filepath.Join(dir, "d")
				sub := filepath.Join(d, "sub")
				mustMkdir(t, d)
				mustMkdir(t, sub)
				mustWrite(t, filepath.Join(sub, "x"), "x\n")
				private := filepath.Join(t.TempDir(), "private")
				if inside {
					private = filepath.Join(d, "other")
				}
				mustMkdir(t, private)
				mustWrite(t, filepath.Join(private, "x"), "secret\n")
				backupSwapWindow = func(at, path string) {
					if at == phase && path == sub {
						if err := os.Rename(sub, filepath.Join(dir, "moved")); err != nil {
							t.Error(err)
						}
						if err := os.Symlink(private, sub); err != nil {
							t.Error(err)
						}
					}
				}
				t.Cleanup(func() { backupSwapWindow = nil })
				applyBackup(t, t.TempDir(), raltest.FileRes(d, map[string]any{
					"ensure": "file", "content": "new\n", "force": true, "backup": ".bak"}), false)
				if err := filepath.WalkDir(d+".bak", func(p string, e os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !inside && e.Type().IsRegular() && strings.Contains(readString(t, p), "secret") {
						t.Errorf("%s holds the swapped-in directory's file", p)
					}
					return nil
				}); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if _, err := os.Lstat(d + ".bak"); err == nil {
					if got := readString(t, filepath.Join(d+".bak", "sub", "x")); got != "x\n" {
						t.Errorf("d.bak/sub/x = %q, want the original entry", got)
					}
				}
			})
		}
	}
}

// Into the bucket a generated child that links out of its tree is a link,
// which Puppet does not bucket (util/backups.rb:31): it is removed, and
// nothing outside the tree is read.
func TestFileBackupBucketOfAGeneratedChildLinkingOut(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, secret, "secret\n")
	c := filepath.Join(dir, "esc")
	if err := os.Symlink(secret, c); err != nil {
		t.Fatal(err)
	}
	parent := raltest.FileRes(dir, map[string]any{"ensure": "directory", "recurse": true, "purge": true, "backup": "puppet"})
	r := raltest.FileRes(c, map[string]any{"ensure": "absent", "backup": "puppet"})
	r.GeneratedFrom = &parent
	changes, _ := applyBackup(t, vardir, r, false)
	if len(changes) != 1 || changes[0].Err != nil || changes[0].Message != "removed" {
		t.Fatalf("changes = %+v, want the link removed", changes)
	}
	if _, err := os.Lstat(c); !os.IsNotExist(err) {
		t.Errorf("esc still there: %v", err)
	}
	if got := siblings(t, vardir); len(got) != 0 {
		t.Errorf("vardir holds %v; a link is not bucketed", got)
	}
}

// The bucket's walk of a directory is the suffix copy's: a subdirectory
// swapped for a link once checked never has the swapped-in directory's bytes
// stored, under any path; a restore would write them to the managed path.
func TestFileBackupBucketNeverStoresASwappedInSubdirectory(t *testing.T) {
	for _, phase := range []string{"lstat", "open"} {
		for _, inside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s inside=%v", phase, inside), func(t *testing.T) {
				dir, vardir := t.TempDir(), t.TempDir()
				d := filepath.Join(dir, "d")
				sub := filepath.Join(d, "sub")
				mustMkdir(t, d)
				mustMkdir(t, sub)
				mustWrite(t, filepath.Join(sub, "x"), "x\n")
				private := filepath.Join(t.TempDir(), "private")
				if inside {
					private = filepath.Join(d, "other")
				}
				mustMkdir(t, private)
				mustWrite(t, filepath.Join(private, "x"), "secret\n")
				backupSwapWindow = func(at, path string) {
					if at == phase && path == sub {
						if err := os.Rename(sub, filepath.Join(dir, "moved")); err != nil {
							t.Error(err)
						}
						if err := os.Symlink(private, sub); err != nil {
							t.Error(err)
						}
					}
				}
				t.Cleanup(func() { backupSwapWindow = nil })
				applyBackup(t, vardir, raltest.FileRes(d, map[string]any{
					"ensure": "file", "content": "new\n", "force": true, "backup": "puppet"}), false)
				secretShard := shardDir(vardir, sha("secret\n"))
				if inside {
					// d/other/x is in the tree and is bucketed under its own
					// path; it must never be recorded under sub's.
					if b, err := os.ReadFile(filepath.Join(secretShard, "paths")); err == nil &&
						strings.Contains(string(b), filepath.Join("sub", "x")) {
						t.Errorf("the swapped-in file is recorded under sub: %q", b)
					}
					return
				}
				if _, err := os.Lstat(secretShard); !os.IsNotExist(err) {
					t.Errorf("the swapped-in directory's bytes were bucketed (%v)", err)
				}
			})
		}
	}
}

// A link below a bucketed directory is opened in the directory already
// opened and checked, never by a joined path: when that directory is swapped
// for one holding a link of the same name, the swapped-in link's target never
// enters the bucket.
func TestFileBackupBucketOpensALinkEntryInTheCheckedDirectory(t *testing.T) {
	for _, phase := range []string{"lstat", "open"} {
		t.Run(phase, func(t *testing.T) {
			dir, vardir := t.TempDir(), t.TempDir()
			d := filepath.Join(dir, "d")
			sub := filepath.Join(d, "sub")
			link := filepath.Join(sub, "l")
			mustMkdir(t, d)
			mustMkdir(t, sub)
			mustWrite(t, filepath.Join(sub, "x"), "x\n")
			if err := os.Symlink("x", link); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(t.TempDir(), "secret")
			mustWrite(t, secret, "secret\n")
			private := filepath.Join(t.TempDir(), "private")
			mustMkdir(t, private)
			if err := os.Symlink(secret, filepath.Join(private, "l")); err != nil {
				t.Fatal(err)
			}
			// "open" fires once sub is opened and checked, "lstat" once its
			// entry l is lstat'ed there: both before l is opened.
			at := map[string]string{"open": sub, "lstat": link}[phase]
			backupSwapWindow = func(window, path string) {
				if window == phase && path == at {
					if err := os.Rename(sub, filepath.Join(dir, "moved")); err != nil {
						t.Error(err)
					}
					if err := os.Symlink(private, sub); err != nil {
						t.Error(err)
					}
				}
			}
			t.Cleanup(func() { backupSwapWindow = nil })
			applyBackup(t, vardir, raltest.FileRes(d, map[string]any{
				"ensure": "file", "content": "new\n", "force": true, "backup": "puppet"}), false)
			if _, err := os.Lstat(shardDir(vardir, sha("secret\n"))); !os.IsNotExist(err) {
				t.Errorf("the swapped-in link's target was bucketed (%v)", err)
			}
			if got := readString(t, filepath.Join(shardDir(vardir, sha("x\n")), "paths")); got != filepath.Join(realPath(t, dir), "moved", "x")+"\n" {
				t.Errorf("x's paths = %q, want only the original x, through l or not", got)
			}
		})
	}
}

// swapDirFS replaces the directory at path with the directory with once
// path has been made (Mkdir), just before the next call named at -- "Lstat"
// or "OpenFile" -- reaches path.
type swapDirFS struct {
	osFS
	path, with, at string
	made           *bool
}

func (s swapDirFS) swap(op, name string) error {
	if op != s.at || name != s.path || !*s.made {
		return nil
	}
	*s.made = false
	if err := os.Remove(s.path); err != nil {
		return err
	}
	return os.Rename(s.with, s.path)
}

func (s swapDirFS) Mkdir(name string, perm os.FileMode) error {
	err := s.osFS.Mkdir(name, perm)
	if err == nil && name == s.path {
		*s.made = true
	}
	return err
}

func (s swapDirFS) Lstat(name string) (os.FileInfo, error) {
	if err := s.swap("Lstat", name); err != nil {
		return nil, err
	}
	return s.osFS.Lstat(name)
}

func (s swapDirFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if err := s.swap("OpenFile", name); err != nil {
		return nil, err
	}
	return s.osFS.OpenFile(name, flag, perm)
}

// The top of a directory copy is checked as each directory below it is, and
// must be the private directory the copy made: a directory swapped in after
// the Mkdir -- between it and the Lstat, or between the Lstat and the open --
// is neither written into nor removed.
func TestFileBackupSuffixRefusesADestinationSwappedAfterItsMkdir(t *testing.T) {
	for _, tc := range []struct{ at, want string }{
		{"Lstat", "is not the private directory the backup made"},
		{"OpenFile", "changed while it was being backed up"},
	} {
		t.Run(tc.at, func(t *testing.T) {
			dir := t.TempDir()
			d := filepath.Join(dir, "d")
			mustMkdir(t, d)
			mustWrite(t, filepath.Join(d, "x"), "x\n")
			other := filepath.Join(dir, "other")
			mustMkdir(t, other)
			if err := os.Chmod(other, 0o755); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(other, "keep"), "keep\n")
			fi, err := os.Lstat(d)
			if err != nil {
				t.Fatal(err)
			}
			bk := &backup{value: ".bak", force: true}
			_, err = bk.existing(swapDirFS{path: d + ".bak", with: other, at: tc.at, made: new(bool)}, d, fi)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("existing = %v, want the swapped-in destination refused (%s)", err, tc.want)
			}
			if got := siblings(t, d+".bak", "keep"); len(got) != 0 {
				t.Errorf("the swapped-in directory was written into: %v", got)
			}
			if got := readString(t, filepath.Join(d+".bak", "keep")); got != "keep\n" {
				t.Errorf("keep = %q; the swapped-in directory was removed", got)
			}
		})
	}
}

// A link below a bucketed directory to anything but a regular file is
// skipped on its stat, as File.file? only stats: what it reaches is never
// opened, so a device such as a watchdog is never armed. A fifo stands in
// for the device; inotify reports any open of it.
func TestFileBackupBucketNeverOpensALinkedNonRegularFile(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	d := filepath.Join(dir, "d")
	mustMkdir(t, d)
	mustWrite(t, filepath.Join(d, "x"), "x\n")
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, filepath.Join(d, "dev")); err != nil {
		t.Fatal(err)
	}
	in, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(in)
	if _, err := syscall.InotifyAddWatch(in, fifo, syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	applyBackup(t, vardir, raltest.FileRes(d, map[string]any{
		"ensure": "file", "content": "new\n", "force": true, "backup": "puppet"}), false)
	if _, err := os.Lstat(shardDir(vardir, sha("x\n"))); err != nil {
		t.Errorf("x was not bucketed: %v", err)
	}
	buf := make([]byte, 4096)
	if n, err := syscall.Read(in, buf); n > 0 {
		t.Errorf("the fifo the link reaches was opened (%d bytes of inotify events)", n)
	} else if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("reading inotify: %v", err)
	}
}

// D-47: a generated child's directory is bucketed through its rootFS, so a
// link below it is bucketed only when what it reaches lies inside the
// recursed tree; Puppet buckets one reaching anywhere.
func TestFileBackupBucketOfAGeneratedDirectoryStaysInItsTree(t *testing.T) {
	dir, vardir := t.TempDir(), t.TempDir()
	c := filepath.Join(dir, "c")
	mustMkdir(t, c)
	mustWrite(t, filepath.Join(dir, "in"), "in\n")
	secret := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, secret, "secret\n")
	for name, target := range map[string]string{"abs-in": filepath.Join(dir, "in"), "out": secret} {
		if err := os.Symlink(target, filepath.Join(c, name)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := openDirNoFollow(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	bk := &backup{value: defaultBucket, vardir: vardir}
	if err := bk.toBucket(rootFS{root: root, dir: dir}, c); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(shardDir(vardir, sha("in\n")), "paths")); got != filepath.Join(realPath(t, dir), "in")+"\n" {
		t.Errorf("in's paths = %q, want the in-tree target bucketed", got)
	}
	if _, err := os.Lstat(shardDir(vardir, sha("secret\n"))); !os.IsNotExist(err) {
		t.Errorf("a target outside the tree was read into the bucket (%v)", err)
	}
}
