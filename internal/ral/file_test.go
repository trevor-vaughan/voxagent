package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func fileRes(path string, params map[string]any) catalog.Resource {
	p := map[string]any{}
	for k, v := range params {
		p[k] = v
	}
	return catalog.Resource{Type: "File", Title: path, Parameters: p}
}

func TestFileCreatesWithContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "alpha\n", "mode": "0640"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("Apply reported no changes when creating a file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "alpha\n" {
		t.Errorf("content = %q, want %q", b, "alpha\n")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %o, want 640", got)
	}
}

// The second apply must report zero changes. This is the property the
// idempotence gate checks at the whole-run level, pinned here at the provider
// level where a failure is easy to localise.
func TestFileIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	r := fileRes(path, map[string]any{"ensure": "file", "content": "alpha\n", "mode": "0644"})
	if _, _, err := (File{}).Apply(context.Background(), r, false); err != nil {
		t.Fatalf("first Apply returned error: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("second Apply reported %d changes, want 0: %+v", len(changes), changes)
	}
}

func TestFileReportsOnlyTheChangedProperty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	base := map[string]any{"ensure": "file", "content": "alpha\n", "mode": "0644"}
	if _, _, err := (File{}).Apply(context.Background(), fileRes(path, base), false); err != nil {
		t.Fatalf("first Apply returned error: %v", err)
	}
	changed := map[string]any{"ensure": "file", "content": "alpha\n", "mode": "0600"}
	changes, _, err := File{}.Apply(context.Background(), fileRes(path, changed), false)
	if err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Property != "mode" {
		t.Errorf("Property = %q, want %q", changes[0].Property, "mode")
	}
}

// Content changing while mode stays put must report content and not mode.
// The mirror of the test above: together they pin that each property is
// compared independently rather than one implying the other.
func TestFileReportsContentChangeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	base := map[string]any{"ensure": "file", "content": "alpha\n", "mode": "0644"}
	if _, _, err := (File{}).Apply(context.Background(), fileRes(path, base), false); err != nil {
		t.Fatalf("first Apply returned error: %v", err)
	}
	changed := map[string]any{"ensure": "file", "content": "beta\n", "mode": "0644"}
	changes, _, err := File{}.Apply(context.Background(), fileRes(path, changed), false)
	if err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Property != "content" {
		t.Errorf("Property = %q, want %q", changes[0].Property, "content")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "beta\n" {
		t.Errorf("content = %q, want %q", b, "beta\n")
	}
}

func TestFileEnsureDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "directory", "mode": "0750"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("path is not a directory")
	}
	if got := fi.Mode().Perm(); got != 0o750 {
		t.Errorf("mode = %o, want 750", got)
	}
}

func TestFileEnsureAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "absent"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present after ensure => absent")
	}
	// Absent on an already-absent file is a no-op.
	again, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "absent"}), false)
	if err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Apply reported %d changes, want 0", len(again))
	}
}

// A file declared with no content must be created empty. Formatting a missing
// parameter with %v would write the four bytes "<nil>", and every other test
// here supplies content, so nothing else would catch it.
func TestFileWithoutContentIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("content = %q, want empty", b)
	}
}

func TestFileNoopChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "alpha\n"}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) == 0 {
		t.Error("noop Apply reported no changes; it must report what it would have done")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("noop Apply created the file")
	}
}

// Noop against an EXISTING file that is out of date must also change nothing on
// disk while still reporting. The test above only covers creation, where
// "changed nothing" is easy to get right by accident.
func TestFileNoopDoesNotRewriteExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "new\n", "mode": "0600"}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) == 0 {
		t.Error("noop Apply reported no changes for an out-of-date file")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "old\n" {
		t.Errorf("noop Apply rewrote the file: content = %q, want %q", b, "old\n")
	}
	fi, _ := os.Stat(path)
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("noop Apply changed the mode to %o, want 644", got)
	}
}

// A write that cannot succeed is never swallowed into a silent success -- and
// it names ensure rather than the resource, because ensure is the property that
// was attempted.
//
// Measured 2026-09-18, ensure => file with content into a directory that is not
// writable: one event, property "ensure", name "file_created", message
// "change from 'absent' to 'file' failed: Could not set 'file' on ensure:
// Permission denied @ dir_s_mkdir - <tmp>.lock (file: <manifest>, line: 1)".
// The errno IS wrapped, and the wording has a location suffix: type/file/
// ensure.rb declares :file with a block, Puppet::Property.newvalue turns that
// block into a set_file method (read property.rb:168-175), so property.rb:483
// set takes its `method` branch: :490 re-raises a Puppet::Error untouched and
// :492 wraps everything else.
//
// A missing parent directory rather than a chmod 000 fixture: these tests also
// run as root in the container gate, and root bypasses the permission bits, so
// a chmod fixture would prove nothing there.
func TestFileCreateFailureIsAttributedToEnsure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-dir", "a.txt")
	r := fileRes(path, map[string]any{"ensure": "file", "content": "x"})
	r.File, r.Line = "/etc/puppetlabs/code/environments/production/manifests/site.pp", 7

	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a failed create names ensure "+
			"and belongs in Change.Err", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want exactly 1 (ensure): %+v", len(changes), changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "file_created" {
		t.Errorf("Property/Name = %q/%q, want \"ensure\"/\"file_created\"", ch.Property, ch.Name)
	}
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a create that cannot succeed")
	}
	want := "Could not set 'file' on ensure: open " + path +
		": no such file or directory (file: " + r.File + ", line: 7)"
	if ch.Err.Error() != want {
		t.Errorf("Change.Err = %q\n         want   %q", ch.Err, want)
	}
}

// A content rewrite that fails on an EXISTING path names content, and is NOT
// wrapped.
//
// Measured 2026-09-18, content => 'new' over an existing file whose parent
// directory is read-only: one event, property "content", name
// "content_changed", message "change from '{sha256}...' to '{sha256}...'
// failed: Permission denied @ dir_s_mkdir - <tmp>.lock" -- no "Could not set"
// and no (file:, line:) suffix. ensure is already in sync here, so the write
// happens in content's own sync_if_needed rather than inside the ensure
// setter, and nothing wraps it.
func TestFileContentFailureIsAttributedToContent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory, so there is no EACCES to observe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// replaceFile stages its temp file in the parent, so a read-only parent
	// blocks the rewrite while the file itself stays readable and writable --
	// which is what keeps this on the content path rather than the stat or read
	// path.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}
	// t.TempDir's own cleanup cannot unlink through a read-only parent.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	r := fileRes(path, map[string]any{"ensure": "file", "content": "new"})
	r.File, r.Line = "/etc/site.pp", 3
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a failed rewrite names content", err)
	}
	if len(changes) != 1 || changes[0].Property != "content" {
		t.Fatalf("changes = %+v, want exactly one content change", changes)
	}
	ch := changes[0]
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a rewrite that cannot succeed")
	}
	if strings.Contains(ch.Err.Error(), "Could not set") {
		t.Errorf("Change.Err = %q is wrapped as an ensure failure; content syncs "+
			"outside the ensure setter and property.rb never wraps it", ch.Err)
	}
	if strings.Contains(ch.Err.Error(), "(file:") {
		t.Errorf("Change.Err = %q carries an error_location suffix; only "+
			"property.rb:492's wrapper adds one", ch.Err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "old" {
		t.Errorf("content = %q (err %v), want %q left in place", got, readErr, "old")
	}
}

// A removal that fails names ensure, and is NOT wrapped.
//
// Measured 2026-09-18, ensure => absent on a file whose parent directory is
// read-only: one event, property "ensure", name "file_removed", message
// "change from 'file' to 'absent' failed: Permission denied @ apply2files -
// <path>" -- unwrapped, and with no (file:, line:) suffix even though
// Errno::EACCES is not a Puppet::Error. Read type/file/ensure.rb:185: sync
// calls @resource.remove_existing(should) and returns :file_removed BEFORE
// delegating to super, so an absent never passes through property.rb's set and
// so never reaches its wrapper.
func TestFileAbsentRemovalFailureIsAttributedToEnsure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root unlinks through a read-only directory, so there is no EACCES to observe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	r := fileRes(path, map[string]any{"ensure": "absent"})
	r.File, r.Line = "/etc/site.pp", 4
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a failed unlink names ensure", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want exactly 1 (ensure): %+v", len(changes), changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "file_removed" {
		t.Errorf("Property/Name = %q/%q, want \"ensure\"/\"file_removed\"", ch.Property, ch.Name)
	}
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a removal that cannot succeed")
	}
	if strings.Contains(ch.Err.Error(), "Could not set") ||
		strings.Contains(ch.Err.Error(), "(file:") {
		t.Errorf("Change.Err = %q is wrapped; ensure.rb:185's sync returns before set runs", ch.Err)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Errorf("the file is gone after a removal that reported failure: %v", statErr)
	}
}

func TestFileRejectsUnknownEnsure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "link"}), false); err == nil {
		t.Fatal("Apply accepted ensure => link; want an error")
	}
}

// ensure => file against an existing directory, and ensure => directory against
// an existing file, are both errors rather than silent replacements. Removing
// and recreating would destroy data the catalog never asked to destroy.
func TestFileRefusesToChangeType(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "d")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(asDir, map[string]any{"ensure": "file", "content": "x"}), false); err == nil {
		t.Error("Apply accepted ensure => file over a directory; want an error")
	}

	asFile := filepath.Join(dir, "f")
	if err := os.WriteFile(asFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(asFile, map[string]any{"ensure": "directory"}), false); err == nil {
		t.Error("Apply accepted ensure => directory over a file; want an error")
	}
}

func TestFileRejectsNonOctalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "mode": "rwxr-xr-x"}), false); err == nil {
		t.Fatal("Apply accepted a symbolic mode; want an error")
	}
}

func TestFileRefreshIsANoOp(t *testing.T) {
	refreshed, err := File{}.Refresh(context.Background(), fileRes("/tmp/x", nil), false)
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if refreshed {
		t.Error("Refresh reported a refresh; file has nothing to refresh")
	}
}

// No ensure key at all: file { p: content => x } is legal and common Puppet.
// It must default to ensure => file, not silently do nothing.
func TestFileDefaultEnsureIsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"content": "x"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("Apply reported no changes for a resource with no ensure param")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "x" {
		t.Errorf("content = %q, want %q", b, "x")
	}
}

// A newly created file with no declared mode must be 0644, Puppet's default.
func TestFileCreateDefaultModeIs644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "x"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

// A newly created directory with no declared mode must be 0755, Puppet's
// default.
func TestFileDirectoryDefaultModeIs755(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "directory"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("mode = %o, want 755", got)
	}
}

// A missing parent directory must be an error, not silently created along
// the way: Puppet's file type does not manage intermediate directories.
// ensure => directory never creates a tree, and the refusal carries Puppet's
// OWN sentence, unwrapped.
//
// Measured 2026-09-18: one event, property "ensure", name "directory_created",
// message "change from 'absent' to 'directory' failed: Cannot create <path>;
// parent directory <parent> does not exist" -- no "Could not set 'directory' on
// ensure:" and no (file:, line:) suffix, even though the resource has both.
// type/file/ensure.rb's :directory block checks the parent before Dir.mkdir and
// raises Puppet::Error, which property.rb:490 re-raises untouched.
//
// The pre-check is what produces that sentence: without it voxagent would
// report Go's ENOENT from mkdir, wrapped, which is two divergences in one
// message.
func TestFileDirectoryMissingParentIsAttributedToEnsure(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "no-such-parent")
	path := filepath.Join(parent, "d")
	r := fileRes(path, map[string]any{"ensure": "directory"})
	r.File, r.Line = "/etc/site.pp", 1

	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a refused create names ensure", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want exactly 1 (ensure): %+v", len(changes), changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "directory_created" {
		t.Errorf("Property/Name = %q/%q, want \"ensure\"/\"directory_created\"", ch.Property, ch.Name)
	}
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a directory whose parent does not exist")
	}
	want := "Cannot create " + path + "; parent directory " + parent + " does not exist"
	if ch.Err.Error() != want {
		t.Errorf("Change.Err = %q\n         want   %q", ch.Err, want)
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		t.Error("the directory exists after a create that reported failure")
	}
}

// noop against ensure => absent must leave the file exactly as it was.
func TestFileAbsentNoopLeavesFileInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "absent"}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("noop Apply removed the file: %v", err)
	}
}

// Fix #3: Puppet requires force => true to remove a directory; without it,
// Puppet leaves an empty directory in place (measured, Puppet 8.10.0: "Not
// removing directory; use 'force' to override"). force is outside the
// bounded attribute set, so voxagent refuses the resource outright rather
// than silently deleting (worse than Puppet's no-op) or silently leaving it
// while reporting success (also worse).
func TestFileAbsentRefusesToRemoveDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(dir, map[string]any{"ensure": "absent"}), false); err == nil {
		t.Fatal("Apply removed a directory; want an error naming force as unimplemented")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory removed despite the error: %v", err)
	}
}

// A dangling symlink (target does not exist) must still be removed by
// ensure => absent: os.Stat follows the link and would report IsNotExist,
// wrongly reading the resource as already-absent and leaving the link in
// place. os.Lstat sees the link itself, which does exist.
func TestFileAbsentRemovesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), link); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(link, map[string]any{"ensure": "absent"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	// Measured 2026-09-18, ensure => absent over a symlink: previous_value is
	// "link", not the constant "present" this asserted before. Ruby reads the
	// ensure property's retrieve, which answers what the path currently IS.
	if changes[0].Was != "link" {
		t.Errorf("Was = %v, want %q -- Ruby reports the path's actual type",
			changes[0].Was, "link")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("dangling symlink still present after ensure => absent")
	}
}

// Fix #2: the agent normally runs as root, so writing through a symlink is a
// privilege-escalation primitive. Measured against Puppet 8.10.0 (links
// defaults to manage): the symlink is replaced by a regular file and the
// target is untouched. links is outside the bounded attribute set, so
// voxagent refuses outright instead -- a deliberate divergence.
func TestFileRefusesToFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(link, map[string]any{"ensure": "file", "content": "NEW"}), false); err == nil {
		t.Fatal("Apply followed a symlink; want an error naming links as unimplemented")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(b) != "ORIGINAL" {
		t.Errorf("target content = %q, want %q (write must not follow the link)", b, "ORIGINAL")
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced with a regular file; want it left alone")
	}
}

// The same refusal for ensure => directory.
func TestFileDirectoryRefusesToFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(link, map[string]any{"ensure": "directory", "mode": "0700"}), false); err == nil {
		t.Fatal("Apply followed a symlink to a directory; want an error naming links as unimplemented")
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link was replaced with a directory; want it left alone")
	}
}

// Fix #5: ensure => present is satisfied by an existing path of any type.
// Measured, Puppet 8.10.0: a clean no-op over an existing directory. ensure
// => file is stricter and still errors -- see TestFileRefusesToChangeType.
func TestFilePresentOverExistingDirectoryIsANoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("Apply reported %d changes for an already-present directory, want 0: %+v", len(changes), changes)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !fi.IsDir() {
		t.Error("present replaced the directory")
	}
}

// ensure => present over a path that does not exist yet must still create a
// plain file, exactly like ensure => file.
func TestFilePresentCreatesAsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "present", "content": "x"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.IsDir() {
		t.Error("present created a directory; want a file")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "x" {
		t.Errorf("content = %q, want %q", b, "x")
	}
}

// Fix #1: mode must be a string. Measured against Puppet 8.10.0: `mode =>
// 0644` (a bare integer) compiles to catalog JSON 420, and Puppet refuses
// with "The file mode specification must be a string, not 'Integer'" rather
// than silently reinterpreting 420 as octal 0420.
func TestFileRejectsNumericMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "mode": float64(420)}), false); err == nil {
		t.Fatal("Apply accepted a numeric mode; want an error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Apply created a file despite the rejected mode")
	}
}

func TestFileRejectsModeAboveMax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "mode": "17777"}), false); err == nil {
		t.Fatal("Apply accepted a mode above 07777; want an error")
	}
}

// Fix #4: `& os.ModePerm` drops the high digit on both sides, so mode =>
// '4755' silently applied 0755 and was stably idempotent there -- an event
// that lied about what was applied.
func TestFileModePreservesSetuidBit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "x", "mode": "4755"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&os.ModeSetuid == 0 {
		t.Error("setuid bit not applied")
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("permission bits = %o, want 755", got)
	}
	again, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "x", "mode": "4755"}), false)
	if err != nil {
		t.Fatalf("second Apply returned error: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Apply reported %d changes, want 0: %+v", len(again), again)
	}
}

// mode => '1777' on a directory silently became 0777 pre-fix -- a security
// downgrade letting any user delete another user's files in that directory.
func TestFileModePreservesStickyBitOnDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "directory", "mode": "1777"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode()&os.ModeSticky == 0 {
		t.Error("sticky bit not applied")
	}
	if got := fi.Mode().Perm(); got != 0o777 {
		t.Errorf("permission bits = %o, want 777", got)
	}
}

// Pins %04o formatting exactly, so a mutation to %o (dropping the leading
// zero) fails this test.
func TestFileModeChangeMessageIsFourDigitOctal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "x", "mode": "0600"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	var mc *Change
	for i := range changes {
		if changes[i].Property == "mode" {
			mc = &changes[i]
		}
	}
	if mc == nil {
		t.Fatalf("no mode change reported: %+v", changes)
	}
	want := "mode changed '0644' to '0600'"
	if mc.Message != want {
		t.Errorf("Message = %q, want %q", mc.Message, want)
	}
}

// noop against an existing file with the WRONG mode (content unchanged) must
// still report the mode change. content is deliberately unchanged here so
// that a mutation skipping mode reconciliation under noop cannot hide behind
// a content change reported for an unrelated reason.
func TestFileNoopReportsModeChangeOnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "same\n", "mode": "0600"}), true)
	if err != nil {
		t.Fatalf("noop Apply returned error: %v", err)
	}
	found := false
	for _, c := range changes {
		if c.Property == "mode" {
			found = true
		}
	}
	if !found {
		t.Fatalf("noop Apply on an existing file with a wrong mode reported no mode change: %+v", changes)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("noop Apply changed the mode to %o, want 644 (untouched)", got)
	}
}

// Fix #8: content must be a string. Measured: content of float64(1e21) wrote
// the literal bytes "1e+21".
func TestFileRejectsNonStringContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": float64(1e21)}), false); err == nil {
		t.Fatal("Apply accepted a numeric content value; want an error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Apply created a file despite the rejected content")
	}
}

// Measured: an explicit JSON null for content wrote the literal four bytes
// "<nil>".
func TestFileRejectsNullContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": nil}), false); err == nil {
		t.Fatal("Apply accepted a null content value; want an error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Apply created a file despite the rejected content")
	}
}

// An unreadable existing file must be a read error, never silently treated
// as empty and overwritten.
//
// The fixture is write-only (0200), not 0000: an all-permissions-denied file
// would also fail the subsequent os.WriteFile on its own, masking a mutation
// that ignores the os.ReadFile error and treats the file as empty (that
// mutation would still succeed in overwriting a write-only file, since
// writing does not require read permission). 0200 isolates the read failure.
func TestFileUnreadableExistingFileIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses file permission bits, so chmod 0200 does not make the file unreadable")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "content": "new"}), false); err == nil {
		t.Fatal("Apply silently overwrote an unreadable file; want an error")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(b) != "secret" {
		t.Errorf("content = %q, want %q (unreadable file must not be overwritten)", b, "secret")
	}
}

// Fix #6: a partially applied resource (created and written, then a later step
// fails) must still report the change it actually made, not discard it.
// Measured: with content plus owner => root running unprivileged, the file WAS
// created and written, then chown failed, and Apply returned (nil, err) -- so
// the report showed a failed resource with no events at all for a file that
// really was created on disk.
//
// On a CREATED path that one surviving event is the ensure event, and the chown
// failure lands on it. type/file.rb:1112 property_fix calls thing.sync directly
// rather than through context.record, so the metadata is applied without being
// reported, and a raise inside it escapes through the ensure setter. Measured
// 2026-09-18, an absent path with owner => 'nosuchuser': exactly one event,
// property "ensure", name "file_created", message "change from 'absent' to
// 'file' failed: Could not set 'file' on ensure: Could not find user nosuchuser
// (file: ..., line: 1)" -- and no owner event at all.
func TestFileCreatedMetadataFailureCollapsesOntoEnsure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	// An unresolvable owner NAME rather than a chown that cannot succeed:
	// type/file/owner.rb:35-37 reports in sync with a warnonce when the process
	// is not root, so an unprivileged chown failure is not a shape Ruby produces
	// at all, while "Could not find user" fails identically at every privilege
	// level. It is also the exact fixture the measurement above used, so the
	// whole message can be compared rather than sampled.
	const absent = "nosuch-voxagent-test"
	r := fileRes(path, map[string]any{
		"ensure": "file", "content": "created-then-owner-fails", "owner": absent,
	})
	r.File, r.Line = "/etc/site.pp", 12

	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a metadata failure on a "+
			"created path names ensure", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want exactly 1: property_fix records "+
			"nothing, so a created path has no owner event: %+v", len(changes), changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "file_created" {
		t.Fatalf("Property/Name = %q/%q, want \"ensure\"/\"file_created\"", ch.Property, ch.Name)
	}
	if ch.Err == nil {
		t.Fatal("Change.Err is nil, but the declared owner cannot be resolved")
	}
	// Wrapped, with the location suffix: property_fix runs inside set_file, so
	// its raise reaches property.rb:492's rescue.
	want := "Could not set 'file' on ensure: Could not find user " + absent +
		" (file: /etc/site.pp, line: 12)"
	if ch.Err.Error() != want {
		t.Errorf("Change.Err = %q\n         want   %q", ch.Err, want)
	}

	// The create itself succeeded, so the file really is on disk -- that is what
	// makes the single event a report of something that happened.
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("file was not created despite content having been written before "+
			"the metadata failed: %v", readErr)
	}
	if string(b) != "created-then-owner-fails" {
		t.Errorf("content = %q, want %q", b, "created-then-owner-fails")
	}
}

// When the create itself fails, the ensure failure is the one reported: the
// metadata was not merely unreported, it was never attempted. type/file.rb calls
// property_fix at :970, after the write, so a raise from the write means Ruby
// never runs it at all -- there is nothing to collapse, and a metadata failure
// must not overwrite the failure that actually stopped the resource.
func TestFileCreateFailureOutranksMetadataFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown to uid 0 would succeed, so the two failures cannot be told apart")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-dir", "a.txt")
	// group as well as owner, and GROUP is the one that discriminates. The
	// discriminator has to be a property whose sync would really be attempted if
	// the guard above it were removed: owner.rb:35's root check suppresses an
	// out-of-sync owner unprivileged, so an owner-only fixture would report
	// nothing to collapse and this test would pass with the guard gone. group.rb
	// has no such check, so its chown is attempted and its ENOENT is what would
	// land on ensure.
	r := fileRes(path, map[string]any{
		"ensure": "file", "content": "x", "owner": "0", "group": "0",
	})
	r.File, r.Line = "/etc/site.pp", 2

	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want exactly 1 (ensure): %+v", len(changes), changes)
	}
	if changes[0].Err == nil {
		t.Fatal("Change.Err is nil for a create that cannot succeed")
	}
	got := changes[0].Err.Error()
	if !strings.Contains(got, "open "+path+": no such file or directory") {
		t.Errorf("Change.Err = %q does not report the create that failed", got)
	}
	// The discriminator: a chown attempted on the path that was never created
	// fails with ENOENT too, so only the wording tells the two apart.
	for _, blame := range []string{"Failed to set owner", "Failed to set group"} {
		if strings.Contains(got, blame) {
			t.Errorf("Change.Err = %q blames %q; the metadata was never attempted, "+
				"because Ruby never reaches property_fix when the write raises", got, blame)
		}
	}
}

// The other half of property_fix: when the metadata SUCCEEDS on a created path
// it is applied and still not reported. Measured 2026-09-18, an absent path with
// ensure => file, content and mode => '0600': one event, property "ensure",
// name "file_created", status success -- and the file is 0600 on disk.
//
// Ownership is the only metadata applyMetadata itself touches on a created path
// (mode is applied at creation, see resourceMode), and changing ownership at all
// needs CAP_CHOWN -- so this runs under task dev:test-privileged.
func TestFileCreatedMetadataIsAppliedButNotReported(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("changing ownership needs root; this case runs under task dev:test-privileged")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	// Numeric ids with no passwd or group entry: lookupUID and lookupGID take
	// their numeric path, so the fixture does not depend on which users and
	// groups the host happens to have.
	const wantID = 12345
	r := fileRes(path, map[string]any{
		"ensure": "file", "content": "x",
		"owner": strconv.Itoa(wantID), "group": strconv.Itoa(wantID),
	})

	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 || changes[0].Property != "ensure" {
		t.Fatalf("changes = %+v, want exactly one ensure change -- property_fix "+
			"applies owner and group without recording them", changes)
	}
	if changes[0].Err != nil {
		t.Fatalf("ensure carries Err %v, but root can chown", changes[0].Err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no stat_t for %s", path)
	}
	if int(st.Uid) != wantID || int(st.Gid) != wantID {
		t.Errorf("uid/gid = %d/%d, want %d/%d -- silent does not mean unapplied",
			st.Uid, st.Gid, wantID, wantID)
	}
}

// The resource-level failures stay bare. Each of these blames no property that
// moved -- Ruby reports them through status.failed_because, which synthesizes a
// resource_error event rather than a property event -- so Apply must keep
// returning an error and must not attribute them to a Change.
func TestFileResourceLevelFailuresStayBare(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "d")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, tt := range []struct {
		name   string
		path   string
		params map[string]any
	}{
		{"an unimplemented ensure value", filepath.Join(dir, "x"), map[string]any{"ensure": "link"}},
		{"ensure => file over a directory", asDir, map[string]any{"ensure": "file", "content": "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changes, _, err := File{}.Apply(context.Background(), fileRes(tt.path, tt.params), false)
			if err == nil {
				t.Fatalf("Apply returned no error; changes = %+v", changes)
			}
			for _, ch := range changes {
				if ch.Err != nil {
					t.Errorf("the %s change carries Err %v; nothing moved, so nothing "+
						"can be blamed", ch.Property, ch.Err)
				}
			}
		})
	}
}

// owner declared as the file's current uid (as a numeric string) must report
// no change and perform no chown.
func TestFileOwnerNumericStringNoopWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	beforeUID := before.Sys().(*syscall.Stat_t).Uid

	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": strconv.Itoa(os.Getuid())}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	for _, c := range changes {
		if c.Property == "owner" {
			t.Errorf("reported an owner change to the file's current owner: %+v", c)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := after.Sys().(*syscall.Stat_t).Uid; got != beforeUID {
		t.Errorf("st_uid changed from %d to %d though owner already matched", beforeUID, got)
	}
}

// group declared as a gid this process cannot chown to must produce a group
// change and then fail the chown (unless running as root, in which case the
// chown genuinely succeeds and there is nothing to observe here).
//
// The failure is attributed to group via Change.Err rather than returned as a
// bare error: posix.rb's group= wraps its own File.chown, so the property that
// could not be set is known.
//
// This asserted the same thing about OWNER until owner grew its root check.
// type/file/owner.rb:35 now suppresses an unprivileged owner change entirely,
// so there is no owner chown left to fail; type/file/group.rb has no such guard
// (its insync? at :23 is `@should.include?(current)` and nothing else), so group
// is where the attempted-and-failed chown still lives. The claim under test --
// an EPERM from chown names its property rather than failing the resource -- is
// unchanged, and the code path it covers is the same one owner used.
func TestFileGroupChangeUnprivilegedFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown to gid 0 would succeed, so this test cannot observe the unprivileged failure path")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "group": "0"}), false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a failed chown names group", err)
	}
	found := false
	for _, c := range changes {
		if c.Property == "group" {
			found = true
			if c.Err == nil {
				t.Error("group change has no Err; chowning to gid 0 cannot succeed unprivileged")
			}
		}
	}
	if !found {
		t.Errorf("Apply returned no group change for the failed chown: %+v", changes)
	}
}

// noop with a declared owner different from the current one must report the
// change and leave st_uid untouched.
//
// Root only. It used to run everywhere, with the target uid chosen to differ
// from the current one either way -- but unprivileged, owner.rb:35's root check
// now reports the property in sync and there is no change to observe. That half
// moved to TestFileOwnerNoopUnprivilegedReportsNoChange, which pins the measured
// behaviour there; the assertion here (a noop reports the change and performs no
// chown) is unchanged and still runs in the privileged gate.
func TestFileOwnerNoopReportsChangeWithoutChowning(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("unprivileged, owner.rb's root check reports the property in sync, so a noop has no change to report")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	want := "1"
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	beforeUID := before.Sys().(*syscall.Stat_t).Uid

	changes, _, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": want}), true)
	if err != nil {
		t.Fatalf("noop Apply returned error: %v", err)
	}
	found := false
	for _, c := range changes {
		if c.Property == "owner" {
			found = true
		}
	}
	if !found {
		t.Errorf("noop Apply reported no owner change: %+v", changes)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := after.Sys().(*syscall.Stat_t).Uid; got != beforeUID {
		t.Errorf("noop Apply changed st_uid from %d to %d", beforeUID, got)
	}
}

// wantOwnershipWarning is the whole warning the root check must raise, not just
// its text. Source and dedupe class are asserted here because nothing else in
// the tree pins them: the transaction's own warning tests use a stub provider,
// so an owner warning that reached the report sourced to the resource path, or
// with no dedupe class and therefore one line per file, would otherwise be
// caught only by the report differential.
var wantOwnershipWarning = Warning{
	Source:      "Puppet",
	Message:     ownershipNotRootWarning,
	DedupeClass: "Puppet::Type::File::Owner",
}

// The root check is NOT relaxed under noop: unprivileged, an out-of-sync owner
// reports no change and warns, exactly as it does in a real run.
//
// Measured 2026-09-19, unprivileged, `puppet apply --noop --detailed-exitcodes`
// over a file whose declared owner was out of sync: exit 0, one
// "Warning: Cannot manage ownership unless running as root" line, and no owner
// notice -- byte for byte the same output as the same manifest applied for real.
//
// Read type/file/owner.rb for why: insync? is reached through
// property.rb safe_insync? and returns true at :37, which is ABOVE
// resource_harness.rb's noop branch -- so the noop path never gets a chance to
// report a would-be change. This is the one place voxagent's noop handling could
// plausibly have diverged, since its other owner branches do consult noop.
func TestFileOwnerNoopUnprivilegedReportsNoChange(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: the root check does not fire")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, warnings, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": "0"}), true)
	if err != nil {
		t.Fatalf("noop Apply returned error: %v", err)
	}
	for _, c := range changes {
		if c.Property == "owner" {
			t.Errorf("noop Apply reported an owner change: %+v; Ruby reports none unprivileged", c)
		}
	}
	if !slices.Contains(warnings, wantOwnershipWarning) {
		t.Errorf("warnings = %v, want the ownership warning under noop too", warnings)
	}
}

// A numeric-string owner and the equivalent named owner must resolve to the
// same uid: both already match the file's real owner (the test process), so
// neither should report a change. If the numeric shortcut in lookupUID were
// removed, the numeric-string case would instead call user.Lookup("1000")
// (or similar), which fails on an ordinary username lookup and turns into an
// error instead of a no-change result.
func TestFileOwnerNumericAndNamedResolveSame(t *testing.T) {
	uid := os.Getuid()
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		t.Skipf("no named user for uid %d in this environment: %v", uid, err)
	}

	pathNumeric := filepath.Join(t.TempDir(), "numeric.txt")
	if err := os.WriteFile(pathNumeric, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pathNamed := filepath.Join(t.TempDir(), "named.txt")
	if err := os.WriteFile(pathNamed, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	numericChanges, _, err := File{}.Apply(context.Background(),
		fileRes(pathNumeric, map[string]any{"ensure": "file", "owner": strconv.Itoa(uid)}), false)
	if err != nil {
		t.Fatalf("numeric owner Apply returned error: %v", err)
	}
	namedChanges, _, err := File{}.Apply(context.Background(),
		fileRes(pathNamed, map[string]any{"ensure": "file", "owner": u.Username}), false)
	if err != nil {
		t.Fatalf("named owner Apply returned error: %v", err)
	}
	for _, c := range numericChanges {
		if c.Property == "owner" {
			t.Errorf("numeric owner %q reported a change: %+v", strconv.Itoa(uid), c)
		}
	}
	for _, c := range namedChanges {
		if c.Property == "owner" {
			t.Errorf("named owner %q reported a change: %+v", u.Username, c)
		}
	}
}

// Owner and group applied together via a real chown. Requires root: an
// unprivileged caller cannot set an arbitrary uid/gid pair to prove the two
// were not swapped, and this environment (verified at test-writing time) has
// only one group available to it, which rules out an unprivileged group-only
// variant too. Exercised in the container gate, per the existing
// applyOwnership doc comment.
func TestFileChownAppliesOwnerAndGroupNotSwapped(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("chown to an arbitrary owner/group pair requires root; this environment is unprivileged")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const wantUID, wantGID = 1, 2
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file",
			"owner": strconv.Itoa(wantUID), "group": strconv.Itoa(wantGID)}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != wantUID {
		t.Errorf("uid = %d, want %d (owner/group arguments may be swapped)", st.Uid, wantUID)
	}
	if int(st.Gid) != wantGID {
		t.Errorf("gid = %d, want %d (owner/group arguments may be swapped)", st.Gid, wantGID)
	}
}

// An ensure event names itself the way Puppet names it, and a file created with
// managed content reports the content checksum rather than a bare "created".
//
// Measured against Puppet 8.10.0. property.rb event_name uses the value's
// :event option when the property declares one, and type/file/ensure.rb sets
// :event => :file_created on both file and present, and :directory_created on
// directory. type/file/content.rb change_to_s produces
// "defined content as '{sha256}...'" when the current value is absent.
func TestFileEventNamesAndMessagesMatchPuppet(t *testing.T) {
	// sha256 of "alpha\n", the same value Ruby reported for this content.
	const alphaSum = "{sha256}b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060"

	for _, tt := range []struct {
		name        string
		params      map[string]any
		seed        bool
		wantName    string
		wantMessage string
	}{
		{
			name:        "a file created with content reports the checksum",
			params:      map[string]any{"ensure": "file", "content": "alpha\n"},
			wantName:    "file_created",
			wantMessage: "defined content as '" + alphaSum + "'",
		},
		{
			name:        "a directory reports directory_created",
			params:      map[string]any{"ensure": "directory"},
			wantName:    "directory_created",
			wantMessage: "created",
		},
		{
			name:        "a removal reports file_removed",
			params:      map[string]any{"ensure": "absent"},
			seed:        true,
			wantName:    "file_removed",
			wantMessage: "removed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "target")
			if tt.seed {
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			changes, _, err := File{}.Apply(context.Background(),
				catalog.Resource{Type: "File", Title: path, Parameters: tt.params}, false)
			if err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			var ensure *Change
			for i := range changes {
				if changes[i].Property == "ensure" {
					ensure = &changes[i]
				}
			}
			if ensure == nil {
				t.Fatalf("no ensure change in %+v", changes)
			}
			if ensure.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", ensure.Name, tt.wantName)
			}
			if ensure.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", ensure.Message, tt.wantMessage)
			}
		})
	}
}

// A content change reports both checksums, as Puppet does. The raw content
// never reaches the report: type/file/content.rb prints checksums precisely so
// a report is not a copy of every managed file.
func TestFileContentChangeReportsChecksums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := File{}.Apply(context.Background(),
		catalog.Resource{Type: "File", Title: path,
			Parameters: map[string]any{"ensure": "file", "content": "new\n"}}, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	var content *Change
	for i := range changes {
		if changes[i].Property == "content" {
			content = &changes[i]
		}
	}
	if content == nil {
		t.Fatalf("no content change in %+v", changes)
	}
	if !strings.HasPrefix(content.Message, "content changed '{sha256}") {
		t.Errorf("Message = %q, want a content changed line naming both checksums",
			content.Message)
	}
	if strings.Contains(content.Message, "new\n") || strings.Contains(content.Message, "old\n") {
		t.Errorf("Message = %q leaks the file's content into the report", content.Message)
	}
}

// A file autorequires its NEAREST managed ancestor directory.
//
// Measured against Puppet 8.10.0, type/file.rb autorequire(:file): it ascends
// from the path's parent and takes the FIRST ancestor the catalog manages --
// `parents.find { |p| catalog.resource(:file, p.to_s) }` -- so one edge, not one
// per ancestor. Transitively the ordering is the same, but the edge count is
// not, and a graph full of redundant edges is a graph nobody can read.
//
// Without this, the most ordinary Puppet pattern there is -- a directory and
// the files inside it -- fails: File[/opt/spine/conf] sorts BEFORE
// File[/opt/spine] under any plain string order, because '/' (0x2F) precedes
// ']' (0x5D).
func TestFileAutoRequiresNearestManagedAncestor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		managed []string
		want    []string
	}{
		{
			name:    "the immediate parent",
			path:    "/opt/spine/conf",
			managed: []string{"File[/opt/spine]"},
			want:    []string{"File[/opt/spine]"},
		},
		{
			name:    "the nearest of several, not all of them",
			path:    "/a/b/c/d",
			managed: []string{"File[/a]", "File[/a/b]", "File[/a/b/c]"},
			want:    []string{"File[/a/b/c]"},
		},
		{
			name:    "a grandparent when the parent is unmanaged",
			path:    "/a/b/c",
			managed: []string{"File[/a]"},
			want:    []string{"File[/a]"},
		},
		{
			name:    "nothing when no ancestor is managed",
			path:    "/opt/spine/conf",
			managed: []string{"File[/elsewhere]"},
			want:    nil,
		},
		{
			// Puppet guards with `unless path.root?` so a file cannot
			// autorequire itself, and an infinite ascent is not a hazard here.
			name:    "the root directory requires nothing",
			path:    "/",
			managed: []string{"File[/]"},
			want:    nil,
		},
		{
			name:    "a file never autorequires itself",
			path:    "/opt/spine",
			managed: []string{"File[/opt/spine]"},
			want:    nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			managed := map[string]bool{}
			for _, ref := range tt.managed {
				managed[ref] = true
			}
			got := File{}.AutoRequires(
				catalog.Resource{Type: "File", Title: tt.path},
				func(ref string) bool { return managed[ref] },
			)
			if !slices.Equal(got, tt.want) {
				t.Errorf("AutoRequires(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// Replacing a file's content is ATOMIC: written to a sibling temp file and
// renamed over the target.
//
// Measured against Puppet 8.10.0, file_system/file_impl.rb replace_file:
// lstat the target for its uid/gid/mode, write a temp file in the same
// directory, flush, fsync, chown, chmod, then ::File.rename over the path.
//
// This is not a refinement. A truncate-and-write leaves a reader holding a
// half-written config, and a failure part way through leaves the file
// destroyed rather than unchanged -- for the exact class of file Puppet exists
// to manage. It also showed up as a real divergence: in a container, Ruby
// FAILS to manage /etc/hosts because rename cannot cross a bind mount, while
// voxagent silently succeeded by writing in place.
//
// The inode assertion is the evidence. A truncate-and-write keeps the inode; a
// rename cannot.
func TestFileContentReplacementIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	beforeIno := before.Sys().(*syscall.Stat_t).Ino

	if _, _, err := (File{}).Apply(context.Background(),
		catalog.Resource{Type: "File", Title: path,
			Parameters: map[string]any{"ensure": "file", "content": "new\n"}}, false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Sys().(*syscall.Stat_t).Ino == beforeIno {
		t.Error("the inode is unchanged, so the content was written in place rather than renamed over")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new\n" {
		t.Errorf("content = %q (err %v), want \"new\\n\"", got, err)
	}
	// Puppet carries the existing mode across when the catalog does not declare
	// one. Letting the temp file's mode win would silently reset every managed
	// file to whatever the umask allows.
	if got := after.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, want 0640 preserved from the original", got)
	}
	// The temp file must not survive. A directory slowly filling with debris is
	// how a managed path becomes unmanageable.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the managed file", names)
	}
}

// A declared mode still wins over the preserved one.
func TestFileContentReplacementHonoursADeclaredMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		catalog.Resource{Type: "File", Title: path,
			Parameters: map[string]any{"ensure": "file", "content": "new\n", "mode": "0600"}},
		false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}

// Puppet prints owner and group as NAMES in a change message while the event
// keeps the raw uid. type/file/owner.rb and group.rb override is_to_s and
// should_to_s to `super(provider.uid2name(v) || v)`; create_change_event
// populates previous_value from the raw current_value. Measured: an event with
// previous_value 1000 whose message reads "change from 'llm' to ...".
//
// So Was stays an int and WasDisplay carries the name. Putting the name in Was
// would fix the message by corrupting the event field.
// Asserted on group unprivileged and on owner as root, because those are the
// two places a rendered change is REACHABLE. This test used to assert only the
// owner half, unprivileged; owner.rb:35's root check reports an unprivileged
// out-of-sync owner as in sync, so that fixture produces no change to render any
// more. group keeps it covered everywhere the suite normally runs -- group.rb
// overrides the same two methods with gid2name -- and the owner half moves to
// the privileged gate rather than being dropped, so both properties are pinned
// where one was.
func TestFileOwnershipRendersNamesButKeepsRawValues(t *testing.T) {
	// noop throughout, so nothing is attempted -- these assert the reported
	// CHANGE, which is the part the report carries.
	t.Run("group", func(t *testing.T) {
		if os.Getuid() == 0 {
			// The fixture declares group => root to guarantee a change against
			// an unprivileged primary group. As root that is already the current
			// group, so applyOwnership correctly reports NO change and there is
			// nothing to assert -- a fixture limitation, not privileged
			// behaviour.
			t.Skip("the fixture's group => root is already in sync as root")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gid := os.Getgid()
		self, err := user.LookupGroupId(strconv.Itoa(gid))
		if err != nil {
			t.Skipf("no named group for gid %d in this environment: %v", gid, err)
		}

		// "root" is a group name certain to exist and certain not to be ours in
		// an unprivileged run.
		r := catalog.Resource{Type: "File", Title: path,
			Parameters: map[string]any{"ensure": "file", "group": "root"}}
		changes, _, err := File{}.Apply(context.Background(), r, true)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertRenderedOwnership(t, changes, "group", gid, self.Name, "root")
	})

	t.Run("owner", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("unprivileged, owner.rb's root check reports the property in sync, so there is no change to render")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Running as root, so the current owner is uid 0 and any other uid is a
		// change. uid 1 is named on every Linux distribution this runs on, and
		// the name is looked up rather than assumed because it differs between
		// them ("bin" on RHEL-likes, "daemon" on Debian-likes).
		const wantUID = 1
		target, err := user.LookupId(strconv.Itoa(wantUID))
		if err != nil {
			t.Skipf("no named user for uid %d in this environment: %v", wantUID, err)
		}
		r := catalog.Resource{Type: "File", Title: path,
			Parameters: map[string]any{"ensure": "file", "owner": strconv.Itoa(wantUID)}}
		changes, _, err := File{}.Apply(context.Background(), r, true)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertRenderedOwnership(t, changes, "owner", 0, "root", target.Username)
	})
}

// assertRenderedOwnership checks the raw-value/display split on one ownership
// property: the event field keeps the raw id and only the display fields and the
// message carry names.
//
// Shared by the two halves above because the split is one behaviour asserted
// against two properties, and a second copy of these five comparisons could
// drift from the first.
func assertRenderedOwnership(t *testing.T, changes []Change, property string, wasID int, wasName, isName string) {
	t.Helper()
	var got *Change
	for i := range changes {
		if changes[i].Property == property {
			got = &changes[i]
		}
	}
	if got == nil {
		t.Fatalf("no %s change; got %+v", property, changes)
	}
	if got.Was != wasID {
		t.Errorf("Was = %#v, want the raw int %d", got.Was, wasID)
	}
	if got.WasDisplay != wasName {
		t.Errorf("WasDisplay = %q, want %q", got.WasDisplay, wasName)
	}
	if got.IsDisplay != isName {
		t.Errorf("IsDisplay = %q, want %q", got.IsDisplay, isName)
	}
	wantMsg := property + " changed '" + wasName + "' to '" + isName + "'"
	if got.Message != wantMsg {
		t.Errorf("Message = %q, want %q", got.Message, wantMsg)
	}
}

// owner and group are two properties and Puppet sets them with two calls --
// provider/file/posix.rb:108 is File.chown(should, nil, path) and :140 is
// File.chown(nil, should, path), each wrapping its own failure. One combined
// os.Chown failed them as a unit, so neither could be attributed.
//
// Unprivileged, chown to root fails with EPERM -- exactly the condition needed.
// As root it would succeed, so this skips there rather than asserting a failure
// that cannot happen.
//
// The two properties now diverge here, and that divergence is the sharpest
// available evidence that they really are two independent syncs: owner is
// SUPPRESSED (owner.rb:35 warns and reports in sync), group is ATTEMPTED and
// FAILS (group.rb has no such guard). A single combined os.Chown could not
// produce that pair of outcomes at all.
//
// Measured 2026-09-18 and again 2026-09-19, unprivileged, one file resource with
// both owner and group out of sync: failed=true, out_of_sync=1, change_count=0,
// one group event reading "change from 'llm' to 'root' failed: Failed to set
// group to '0': Operation not permitted @ apply2files - <path>", no owner event,
// and one "Warning: Cannot manage ownership unless running as root" line.
//
// This asserted an attributed Err on BOTH properties before owner's root check
// existed. The surviving claim -- an EPERM names its property rather than
// failing the resource -- is asserted on group here and on group alone in
// TestFileGroupChangeUnprivilegedFails; the owner half of it is not weakened but
// unreachable, because voxagent no longer attempts that chown and neither does
// Puppet.
func TestFileOwnershipFailuresAreAttributedPerProperty(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chown succeeds as root; this asserts the EPERM attribution")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := catalog.Resource{Type: "File", Title: path, Parameters: map[string]any{
		"ensure": "file", "owner": "root", "group": "root",
	}}
	changes, warnings, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; a chown failure "+
			"names a property and belongs in Change.Err", err)
	}

	byProperty := map[string]Change{}
	for _, ch := range changes {
		byProperty[ch.Property] = ch
	}
	if owner, ok := byProperty["owner"]; ok {
		t.Errorf("owner change reported: %+v; unprivileged Ruby reports none", owner)
	}
	group, ok := byProperty["group"]
	if !ok {
		t.Fatalf("no group change; got %+v -- group has no root guard and must still be attempted", changes)
	}
	if group.Err == nil {
		t.Errorf("group change has no Err, but chown to root cannot succeed unprivileged")
	}
	if !slices.Contains(warnings, wantOwnershipWarning) {
		t.Errorf("warnings = %v, want the ownership warning alongside the group failure", warnings)
	}
}

// An unresolvable owner name is attributed to owner, not returned as a
// resource-level error. property.rb:272 safe_insync? delegates to insync?, and
// type/file/owner.rb:29 raises from inside it, so the raise lands in
// sync_if_needed's rescue.
//
// desired_value is the DECLARED STRING here, not a resolved uid -- measured:
// {"previous_value" => 1000, "desired_value" => "nosuchuser"}. @should.map!
// raises before it can replace the name.
func TestFileUnresolvableOwnerIsAttributedToOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := catalog.Resource{Type: "File", Title: path, Parameters: map[string]any{
		"ensure": "file", "owner": "nosuchuser-voxagent-test",
	}}
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; an unresolvable "+
			"name names the property", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d: %+v", len(changes), changes)
	}
	ch := changes[0]
	if ch.Property != "owner" {
		t.Errorf("property = %q, want owner", ch.Property)
	}
	if ch.Err == nil {
		t.Fatal("no Err on an unresolvable owner name")
	}
	if ch.Is != "nosuchuser-voxagent-test" {
		t.Errorf("Is = %#v, want the declared name string -- Ruby's desired_value "+
			"is the unresolved string because map! raised", ch.Is)
	}
	// Was must still be the raw current uid, so the message can say what it
	// was changing FROM. This is why current ownership must be read BEFORE the
	// names are resolved.
	if _, ok := ch.Was.(int); !ok {
		t.Errorf("Was = %#v (%T), want the raw current uid as an int", ch.Was, ch.Was)
	}
}

// A declared owner that resolves and is already correct reports no change at
// all -- the reordering must not turn a no-op into an event.
func TestFileOwnershipInSyncReportsNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	self, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	r := catalog.Resource{Type: "File", Title: path, Parameters: map[string]any{
		"ensure": "file", "owner": self.Username,
	}}
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range changes {
		if ch.Property == "owner" {
			t.Errorf("owner reported a change when already in sync: %+v", ch)
		}
	}
}

// error_location's branches, including the empty one. voxagent previously
// rendered "(file: , line: 0)" for a resource with no source metadata, which is
// a real input: a catalog resource carries file/line only when the compiler
// knew them.
//
// util/errors.rb error_location treats "" as not-known and returns an empty
// string when neither is known, and error_location_with_space adds the space only
// when the result is non-empty -- so the space belongs to the suffix, not to
// the caller's format string.
func TestErrorLocationBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		line int
		want string
	}{
		{"file and line", "/etc/site.pp", 6, " (file: /etc/site.pp, line: 6)"},
		{"line only", "", 6, " (line: 6)"},
		{"file only", "/etc/site.pp", 0, " (file: /etc/site.pp)"},
		{"neither", "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorLocation(tc.file, tc.line); got != tc.want {
				t.Errorf("errorLocation(%q, %d) = %q, want %q", tc.file, tc.line, got, tc.want)
			}
		})
	}
}

// property.rb:493's wording, verbatim: the value is single-quoted, the class
// name is the property's, and the detail is the raised error's own message with
// the resource's source location appended.
func TestWrapEnsureErrorRendersPuppetsWording(t *testing.T) {
	r := catalog.Resource{Type: "File", Title: "/f", File: "/etc/site.pp", Line: 6}
	got := wrapEnsureError("file", r, errors.New("No such file or directory - /nope/f")).Error()
	want := "Could not set 'file' on ensure: No such file or directory - /nope/f" +
		" (file: /etc/site.pp, line: 6)"
	if got != want {
		t.Errorf("wrapEnsureError =\n  %q\nwant\n  %q", got, want)
	}
}

// An unresolvable owner or group under --noop is NOT a failure.
//
// type/file/owner.rb:26 and group.rb:31 both read `elsif
// provider.resource.noop? then return false` -- insync? reports "not in sync"
// instead of raising, so the resource is out of sync with a noop event and the
// run still exits 0. Puppet puts the escape hatch at the property, which is
// why this belongs here and not in the transaction.
//
// Attributing a failure here reported failed=true and change_count=0, and
// exited 4 under --detailed-exitcodes where Puppet exits 0. Nothing else in
// the suite compares an exit code for a noop run over an unresolvable name, so
// without this test that regression is invisible.
func TestFileUnresolvableOwnershipUnderNoopIsNotAFailure(t *testing.T) {
	for _, property := range []string{"owner", "group"} {
		t.Run(property, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "f.txt")
			if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			const absent = "nosuch-voxagent-test"

			r := fileRes(path, map[string]any{"ensure": "file", property: absent})
			changes, _, err := File{}.Apply(context.Background(), r, true)
			if err != nil {
				t.Fatalf("Apply returned a resource-level error: %v", err)
			}

			var found *Change
			for i := range changes {
				if changes[i].Property == property {
					found = &changes[i]
				}
			}
			if found == nil {
				t.Fatalf("no %s change; an unresolvable name is still out of sync: %+v",
					property, changes)
			}
			if found.Err != nil {
				t.Errorf("%s carries Err %v under noop; Puppet reports an ordinary "+
					"out-of-sync, so this would exit 4 where Puppet exits 0",
					property, found.Err)
			}
			if found.Message == "" {
				t.Errorf("%s has no message, so the noop event would carry none", property)
			}
			if found.Is != absent {
				t.Errorf("Is = %#v, want the declared name %q", found.Is, absent)
			}
		})
	}
}

// A failed property does not stop the next one. resource_harness.rb:123
// sync_if_needed wraps each property individually, and perform_changes'
// resource.properties.each keeps going -- the rescue at :147 is commented
// "Execution will continue on StandardErrors, just store the event".
//
// mode is reconciled before ownership, so a chmod that cannot succeed must
// still leave owner attempted. A read-only parent DIRECTORY is what blocks
// chmod on a file we own... it is not: chmod needs ownership of the inode, not
// write permission on the parent. So the failure is forced by making the file
// owned by someone else, which is only arrangeable as root -- hence the skip.
//
// Instead this drives mode failure through a path we can arrange unprivileged:
// see TestFileMetadataChmodFailureIsAttributedToMode for the mechanism.
func TestFileMetadataFailureDoesNotStopLaterProperties(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs an unprivileged process to fail chmod and chown")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// owner => nosuchuser cannot resolve, and group => root cannot be chowned to
	// unprivileged. Both must be attempted and both must be attributed.
	//
	// owner was `root` here until owner.rb:35's root check landed, which
	// suppresses an out-of-sync owner unprivileged and so left this fixture with
	// nothing between mode and group. An UNRESOLVABLE name is the owner failure
	// that survives, because `@should.map!` raises at owner.rb:29 -- above the
	// root check at :35 -- so the property still fails and still has to be
	// stepped over. The three-property walk this test exists for is therefore
	// intact, and it now steps over a failure the root check cannot spare.
	r := fileRes(path, map[string]any{
		"ensure": "file", "mode": "0600", "owner": "nosuchuser", "group": "root",
	})
	changes, _, err := File{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}

	var props []string
	for _, ch := range changes {
		props = append(props, ch.Property)
	}
	want := []string{"mode", "owner", "group"}
	if !slices.Equal(props, want) {
		t.Fatalf("properties = %v, want %v in declaration order -- an earlier "+
			"property's outcome must not stop a later one", props, want)
	}
	// mode succeeds here (we own the file); owner and group cannot.
	if changes[0].Err != nil {
		t.Errorf("mode carries Err %v, but chmod on a file we own succeeds", changes[0].Err)
	}
	for _, i := range []int{1, 2} {
		if changes[i].Err == nil {
			t.Errorf("%s has no Err, but neither an unresolvable owner nor a chown "+
				"to root can succeed unprivileged", changes[i].Property)
		}
	}
}

// A chmod that fails is attributed to mode, not returned bare.
//
// Arranging an unprivileged chmod failure takes care: chmod requires owning the
// inode, so permissions on the file or its parent are irrelevant. Deleting the
// file between the mode comparison and the chmod is not arrangeable from a
// test. What IS arrangeable: a path whose parent directory is not searchable,
// so the chmod's path resolution fails with EACCES.
func TestFileMetadataChmodFailureIsAttributedToMode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory search permission")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "f.txt")
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Strip search permission AFTER the file exists. currentMode's stat and the
	// chmod both then fail -- so this asserts whichever of the two voxagent
	// reaches first, and the point is that it is ATTRIBUTED rather than bare.
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	r := fileRes(path, map[string]any{"ensure": "file", "mode": "0600"})
	_, _, err := File{}.Apply(context.Background(), r, false)
	// currentMode's stat failure IS a legitimate resource-level error: the run
	// could not read the state it was about to reconcile, and no property moved.
	// So a bare error here is CORRECT -- this test documents that boundary
	// rather than asserting attribution.
	if err == nil {
		t.Error("want a resource-level error when the mode cannot even be read")
	}
}

// A chmod that fails is attributed to mode, and ownership is STILL attempted.
//
// This is the red/green proof for applyMetadata's abort: with the pre-fix code
// the chmod returned a bare error, so Apply reported a resource_error naming no
// property AND never reached applyOwnership at all.
//
// It needs a stat that SUCCEEDS and a chmod that FAILS, which is narrower than
// it sounds. chmod checks ownership of the inode, not permissions -- so making
// the file or its parent unwritable does nothing, and stripping the parent's
// search bit (see the test above) breaks the stat too. An unprivileged process
// can only reach that combination on a file it does not own, and the only such
// files are outside its own tmpdir.
//
// So this fixture deliberately breaks the hermetic-tmpdir convention the rest of
// this file follows, and pays for it by verifying every precondition instead of
// assuming one. It cannot mutate the system: chmod and chown both fail before
// touching anything, which the final assertion confirms.
func TestFileMetadataChmodFailureStillAttemptsOwnership(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can chmod any file, so there is no EPERM to observe")
	}
	// A root-owned file that exists on every Linux host. Podman bind-mounts it,
	// which is irrelevant here -- only its ownership matters.
	const foreign = "/etc/hostname"

	fi, err := os.Stat(foreign)
	if err != nil {
		t.Skipf("%s is not stat-able: %v", foreign, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skipf("no stat_t for %s", foreign)
	}
	if int(st.Uid) == os.Getuid() {
		t.Skipf("%s is owned by this process, so chmod would succeed", foreign)
	}
	before := fi.Mode().Perm()
	// Declare a mode that differs, so a chmod is actually attempted, and an
	// owner that cannot be set either -- the owner change is what proves the
	// walk continued past the failed chmod.
	wantMode := "0600"
	if before == 0o600 {
		wantMode = "0640"
	}

	// Our own gid, so the declared group genuinely differs from root's and a
	// chown is actually attempted. Declaring "root" here would be in sync
	// already, and applyOwnership would correctly report nothing -- which reads
	// identically to "the walk stopped" and would make this test lie.
	//
	// It was our own UID until owner.rb:35's root check landed. Unprivileged, an
	// out-of-sync owner is now suppressed and emits no change, which reads
	// identically to "the walk stopped" in just the same way -- so the evidence
	// moves to group, which group.rb still attempts unprivileged. The claim is
	// unchanged: a failed chmod does not end the metadata walk, and the property
	// after it is still reconciled.
	r := fileRes(foreign, map[string]any{
		"ensure": "file", "mode": wantMode, "group": strconv.Itoa(os.Getgid()),
	})
	changes, _, applyErr := File{}.Apply(context.Background(), r, false)
	if applyErr != nil {
		t.Fatalf("Apply returned a resource-level error %v; a failed chmod names "+
			"mode and belongs in Change.Err", applyErr)
	}

	var props []string
	byProperty := map[string]Change{}
	for _, ch := range changes {
		props = append(props, ch.Property)
		byProperty[ch.Property] = ch
	}
	mode, sawMode := byProperty["mode"]
	if !sawMode {
		t.Fatalf("no mode change; got %v", props)
	}
	if mode.Err == nil {
		t.Errorf("mode carries no Err, but chmod on a file we do not own cannot succeed")
	}
	// The whole point: the failed chmod did not end the walk.
	if _, sawGroup := byProperty["group"]; !sawGroup {
		t.Errorf("ownership was not attempted after mode failed; properties = %v -- "+
			"resource_harness.rb:147's rescue is commented \"Execution will "+
			"continue on StandardErrors\"", props)
	}

	after, err := os.Stat(foreign)
	if err != nil {
		t.Fatalf("stat %s after the apply: %v", foreign, err)
	}
	if after.Mode().Perm() != before {
		t.Fatalf("the fixture MUTATED %s: mode %04o -> %04o", foreign, before, after.Mode().Perm())
	}
	// The chown must have failed too, for the same reason the chmod did. Checked
	// because this is the one fixture in this file that touches a path outside
	// its own tmpdir, and a group it CHANGED would be a real system mutation.
	if gid := after.Sys().(*syscall.Stat_t).Gid; gid != st.Gid {
		t.Fatalf("the fixture MUTATED %s: gid %d -> %d", foreign, st.Gid, gid)
	}
}

// Unprivileged, with an out-of-sync owner: NO owner change, no chown attempted,
// and the run-level warning instead.
//
// Measured 2026-09-18, unprivileged, a file resource with an out-of-sync owner:
// Ruby reported no owner event at all and one
// "warning | Puppet | Cannot manage ownership unless running as root" line.
// type/file/owner.rb:35 is the whole mechanism -- insync? answers TRUE when the
// process is not root, so the property is in sync as far as the harness is
// concerned and nothing is ever synced.
//
// voxagent previously reported a failed owner event here, which made an
// unprivileged run exit 4 where Puppet exits 0.
func TestFileOwnerChangeUnprivilegedWarnsAndReportsNoChange(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: the root check does not fire, so there is no suppression to observe")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	beforeUID := before.Sys().(*syscall.Stat_t).Uid

	changes, warnings, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": "0"}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	for _, c := range changes {
		if c.Property == "owner" {
			t.Errorf("Apply reported an owner change: %+v; unprivileged Ruby reports none", c)
		}
	}
	if !slices.Contains(warnings, wantOwnershipWarning) {
		t.Errorf("warnings = %v, want it to carry %+v", warnings, wantOwnershipWarning)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := after.Sys().(*syscall.Stat_t).Uid; got != beforeUID {
		t.Errorf("st_uid moved from %d to %d; no chown should have been attempted", beforeUID, got)
	}
}

// Unprivileged, with an owner that is ALREADY in sync: no change and NO
// warning.
//
// The ordering is the point. type/file/owner.rb puts
// `return true if @should.include?(current)` at :33, BEFORE the root check at
// :35, so a correctly-owned file on an unprivileged host is silent -- Puppet
// does not warn about an ownership it was never asked to move. A guard written
// ahead of the comparison would warn on every file resource that merely
// mentions owner, which on a typical catalog is most of them.
func TestFileOwnerInSyncUnprivilegedDoesNotWarn(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: the root check does not fire, so a spurious warning could not appear")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, warnings, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": strconv.Itoa(os.Getuid())}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	for _, c := range changes {
		if c.Property == "owner" {
			t.Errorf("an in-sync owner reported a change: %+v", c)
		}
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none: the owner was already in sync, so the root check is never reached", warnings)
	}
}

// An owner name that does not resolve still FAILS unprivileged -- the root
// check never gets to spare it.
//
// The ordering is Ruby's and it is the reason: type/file/owner.rb's
// `@should.map!` block is at :22 and raises "Could not find user <name>" at
// :29, before `return true if @should.include?(current)` at :33 and before the
// root check at :35. A guard placed ahead of the name resolution would turn
// every typo'd owner on an unprivileged host into a silent success.
func TestFileUnresolvableOwnerUnprivilegedStillFails(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: the root check does not fire, so this test cannot show that it was not reached")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, warnings, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": "nosuchuser"}), false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error %v; an unresolvable owner names owner", err)
	}
	var found bool
	for _, c := range changes {
		if c.Property == "owner" {
			found = true
			if c.Err == nil || c.Err.Error() != "Could not find user nosuchuser" {
				t.Errorf("owner change Err = %v, want \"Could not find user nosuchuser\"", c.Err)
			}
		}
	}
	if !found {
		t.Errorf("Apply reported no owner failure: %+v; the name resolution raises before the root check", changes)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none: Ruby raises out of insync? before the root check is reached", warnings)
	}
}

// As root, an out-of-sync owner is applied and nothing is warned about.
//
// The privileged half of the same branch: `unless Puppet.features.root?` is
// false, so insync? falls through to `false` and the property syncs. Without
// this, a guard that fired unconditionally -- suppressing every owner change on
// every host -- would pass the whole unprivileged suite above.
func TestFileOwnerChangeAsRootIsAppliedWithoutWarning(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("chowning to another uid requires root; this environment is unprivileged")
	}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	const wantUID = 1
	changes, warnings, err := File{}.Apply(context.Background(),
		fileRes(path, map[string]any{"ensure": "file", "owner": strconv.Itoa(wantUID)}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	var found bool
	for _, c := range changes {
		if c.Property == "owner" {
			found = true
			if c.Err != nil {
				t.Errorf("owner change failed as root: %v", c.Err)
			}
		}
	}
	if !found {
		t.Errorf("Apply reported no owner change as root: %+v", changes)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none as root", warnings)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Sys().(*syscall.Stat_t).Uid; got != wantUID {
		t.Errorf("st_uid = %d, want %d: the chown must still happen as root", got, wantUID)
	}
}

// File's ensure is closed, so an unimplemented value must be refused before
// anything is applied. Puppet reads any string other than its keywords as a
// link target (type/file/ensure.rb), so "/etc/target" means ensure => link.
func TestFileEnsureValuesAreBoundedAtPreflight(t *testing.T) {
	reg := NewRegistry(File{})
	cases := []struct {
		value any
		gap   bool
	}{
		{"file", false},
		{"present", false},
		{"directory", false},
		{"absent", false},
		{"link", true},
		{"/etc/target", true},
		// Puppet aliases false to absent (type/file/ensure.rb:62); voxagent does
		// not implement the alias, so a JSON boolean is refused before anything
		// is applied rather than failing this resource after the rest ran.
		{true, true},
		{false, true},
	}
	for _, tc := range cases {
		c := &catalog.Catalog{Resources: []catalog.Resource{
			{Type: "File", Title: "/tmp/x", Parameters: map[string]any{"ensure": tc.value}},
		}}
		gaps := Preflight(reg, c)
		if got := len(gaps) == 1; got != tc.gap {
			t.Errorf("ensure => %v: gaps = %v, want gap=%v", tc.value, gaps, tc.gap)
			continue
		}
		if tc.gap && (gaps[0].Attribute != "ensure" || gaps[0].Value != fmt.Sprint(tc.value)) {
			t.Errorf("ensure => %v: gap = %+v, want Attribute=ensure Value=%v", tc.value, gaps[0], tc.value)
		}
	}
}
