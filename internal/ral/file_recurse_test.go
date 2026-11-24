package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
)

// tree creates files (content "x", mode 0644) and directories (names ending
// "/", mode 0755) under dir.
func tree(t *testing.T, dir string, entries ...string) {
	t.Helper()
	for _, e := range entries {
		p := filepath.Join(dir, e)
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func titles(rs []catalog.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Title
	}
	return out
}

// childOf is the resource File.Generate hands the transaction for path under
// parent.
func childOf(parent catalog.Resource, path string, params map[string]any) catalog.Resource {
	p := parent
	if params == nil {
		params = map[string]any{}
	}
	return catalog.Resource{Type: "File", Title: path, Parameters: params, GeneratedFrom: &p}
}

func noneManaged(string) bool { return false }

func TestFileGenerateIsEmptyWithoutRecurse(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a")
	for _, params := range []map[string]any{nil, {"recurse": false}, {"recurse": "false"}} {
		children, warnings, err := File{}.Generate(context.Background(), fileRes(dir, params), noneManaged)
		if err != nil || len(children) != 0 || len(warnings) != 0 {
			t.Errorf("%v: Generate = %v, %v, %v; want nothing", params, titles(children), warnings, err)
		}
	}
}

// fileset.rb:162-177 walks breadth first and lists dotfiles; newchild
// (type/file.rb:583-603) passes everything but ensure, recurse, source and
// the refused parameters to each child; recurse sorts by path (:643).
func TestFileGenerateWalksBreadthFirstAndInheritsTheParentsParameters(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "b", "a/x", ".hidden", "a/y/z")
	parent := fileRes(dir, map[string]any{"recurse": true, "mode": "0640", "owner": "root",
		"notify": []any{"Exec[x]"}, "purge": false, "tag": []any{"t"}})
	parent.Tags = []string{"file", "class"}
	children, warnings, err := File{}.Generate(context.Background(), parent, noneManaged)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Generate: %v %v", warnings, err)
	}
	want := []string{dir + "/.hidden", dir + "/a", dir + "/a/x", dir + "/a/y", dir + "/a/y/z", dir + "/b"}
	if got := titles(children); !slices.Equal(got, want) {
		t.Fatalf("children = %v\nwant %v", got, want)
	}
	wantParams := map[string]any{"mode": "0640", "owner": "root", "notify": []any{"Exec[x]"},
		"purge": false, "tag": []any{"t"}}
	for _, c := range children {
		if !reflect.DeepEqual(c.Parameters, wantParams) {
			t.Errorf("%s parameters = %v, want %v", c.Title, c.Parameters, wantParams)
		}
		if c.Type != "File" || !slices.Equal(c.Tags, parent.Tags) || c.GeneratedFrom == nil || c.GeneratedFrom.Title != dir {
			t.Errorf("%s: type %q tags %v generated from %v, want File, the parent's tags, the parent",
				c.Title, c.Type, c.Tags, c.GeneratedFrom)
		}
	}
}

// A native generator's children carry no marks, even from a parent whose
// source is marked Sensitive.
func TestFileGenerateChildrenCarryNoSensitiveMarks(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a", "b/")
	parent := fileRes(dir, map[string]any{"recurse": true, "source": "/nonexistent"})
	parent.SensitiveParameters = []string{"source", "recurse"}
	children, _, err := File{}.Generate(context.Background(), parent, noneManaged)
	if err != nil || len(children) == 0 {
		t.Fatalf("Generate = %v, %v; want children", titles(children), err)
	}
	for _, c := range children {
		if len(c.SensitiveParameters) != 0 {
			t.Errorf("%s SensitiveParameters = %v, want none", c.Title, c.SensitiveParameters)
		}
	}
}

// links => manage: directory? is lstat-based (fileset.rb:153-158), so a
// symlink is an entry and never a way in.
func TestFileGenerateNeverDescendsASymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "dst")
	tree(t, base, "outside/secret", "dst/f")
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	children, _, err := File{}.Generate(context.Background(), fileRes(dir, map[string]any{"recurse": true}), noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(children), []string{dir + "/f", dir + "/link"}; !slices.Equal(got, want) {
		t.Errorf("children = %v, want %v", got, want)
	}
}

// recurse_remote (type/file.rb:712-726) adds every source entry with
// "<source>/<rel>" as its source; mark_children_for_purging (:573-579) then
// marks every child without a source absent.
func TestFileGenerateWalksTheSourceAndPurgesTheRest(t *testing.T) {
	for _, purge := range []bool{false, true} {
		base := t.TempDir()
		src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
		tree(t, base, "src/f", "src/sub/g", "dst/f", "dst/extra", "dst/old/")
		children, _, err := File{}.Generate(context.Background(),
			fileRes(dst, map[string]any{"recurse": true, "source": "file://" + src, "purge": purge}), noneManaged)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{dst + "/extra", dst + "/f", dst + "/old", dst + "/sub", dst + "/sub/g"}
		if got := titles(children); !slices.Equal(got, want) {
			t.Fatalf("purge %t: children = %v\nwant %v", purge, got, want)
		}
		for _, c := range children {
			rel := strings.TrimPrefix(c.Title, dst+"/")
			source, hasSource := c.Parameters["source"]
			ensure, hasEnsure := c.Parameters["ensure"]
			switch rel {
			case "f", "sub", "sub/g":
				if source != filepath.Join(src, rel) || hasEnsure {
					t.Errorf("purge %t: %s source = %v ensure = %v, want source %s and no ensure",
						purge, rel, source, ensure, filepath.Join(src, rel))
				}
			default:
				if hasSource || hasEnsure != purge || (purge && ensure != "absent") {
					t.Errorf("purge %t: %s source = %v ensure = %v", purge, rel, source, ensure)
				}
			}
		}
	}
}

// remove_less_specific_files (type/file.rb:648-673): a child at or below
// another catalog File is that File's, so purge can never remove it or
// anything under it. Component-wise: keep2 is not below keep. Review Focus 2.
func TestFileGenerateExemptsManagedSubtrees(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "keep/x", "keep2", "other")
	managed := func(ref string) bool { return ref == "File["+dir+"/keep]" }
	children, _, err := File{}.Generate(context.Background(),
		fileRes(dir, map[string]any{"recurse": true, "purge": true}), managed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(children), []string{dir + "/keep2", dir + "/other"}; !slices.Equal(got, want) {
		t.Errorf("children = %v, want %v", got, want)
	}
}

// fileset.rb:64-75: past 1,000 entries Puppet warns, once per walk, and
// returns every entry. Review Focus 4.
func TestFileGenerateWarnsPastTheSoftLimitAndKeepsEveryChild(t *testing.T) {
	for _, n := range []int{1000, 1001} {
		dir := t.TempDir()
		for i := range n {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		children, warnings, err := File{}.Generate(context.Background(),
			fileRes(dir, map[string]any{"recurse": true}), noneManaged)
		if err != nil || len(children) != n {
			t.Fatalf("%d entries: %d children, err %v", n, len(children), err)
		}
		var want []Warning
		if n > 1000 {
			want = []Warning{{Source: "Puppet", Message: fmt.Sprintf("The directory '%s' contains %d entries, which exceeds the default soft limit 1000 and may cause excessive resource consumption and degraded performance. To remove this warning set a value for `max_files` parameter or consider using an alternate method to manage large directory trees", dir, n)}}
		}
		if !slices.Equal(warnings, want) {
			t.Errorf("%d entries: warnings = %v, want %v", n, warnings, want)
		}
	}
}

// recurse_local returns nothing for a missing path (:700-709), and a missing
// source contributes nothing (recurse_remote_metadata's `next unless
// result`); the parent's own Apply reports the missing source.
func TestFileGenerateFromASourceIntoAMissingDirectory(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "src/a")
	dst := filepath.Join(base, "dst")
	children, _, err := File{}.Generate(context.Background(),
		fileRes(dst, map[string]any{"recurse": true, "source": filepath.Join(base, "src")}), noneManaged)
	if err != nil || !slices.Equal(titles(children), []string{dst + "/a"}) {
		t.Errorf("Generate = %v, %v; want the source's one entry", titles(children), err)
	}
	children, _, err = File{}.Generate(context.Background(),
		fileRes(dst, map[string]any{"recurse": true, "source": filepath.Join(base, "missing")}), noneManaged)
	if err != nil || len(children) != 0 {
		t.Errorf("Generate = %v, %v; want nothing and no error for a missing source", titles(children), err)
	}
}

// Dir.entries raises on a directory it cannot read (FileSetEntry#children),
// which fails generation.
func TestFileGenerateUnreadableDirectoryIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a 0300 directory")
	}
	dir := t.TempDir()
	tree(t, dir, "locked/f")
	locked := filepath.Join(dir, "locked")
	if err := os.Chmod(locked, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	_, _, err := File{}.Generate(context.Background(), fileRes(dir, map[string]any{"recurse": true}), noneManaged)
	if err == nil || !strings.HasPrefix(err.Error(), "read directory "+locked+": ") {
		t.Errorf("err = %v, want a read failure naming %s", err, locked)
	}
}

// walkedLinks makes base/dst holding three links -- to a file outside the
// tree, to one inside it, and dangling -- and returns the recursed directory
// and each link's target by link path ("" for the dangling one).
func walkedLinks(t *testing.T, base string) (string, map[string]string) {
	t.Helper()
	tree(t, base, "outside", "dst/inside")
	dir := filepath.Join(base, "dst")
	links := map[string]string{
		filepath.Join(dir, "lnout"):  filepath.Join(base, "outside"),
		filepath.Join(dir, "lnin"):   filepath.Join(dir, "inside"),
		filepath.Join(dir, "lndead"): "",
	}
	for link, target := range links {
		to := target
		if to == "" {
			to = "nowhere"
		}
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	return dir, links
}

// D-28 retired. Measured as root on Puppet 8.10.0, recurse => true with owner,
// group and mode => '0600': each walked link -- into the tree, out of it, or
// dangling -- reports owner and group changes and no mode change, and ends
// lchowned at 0777 while the outside target keeps its owner and mode.
// Unprivileged, a group change to a group this user is not in is attempted and
// fails (group.rb has no root check); the failing call's name, lchownat, shows
// it was made on the link itself, through the tree's os.Root.
func TestFileWalkedSymlinkIsLchownedAndKeepsItsMode(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root's group change succeeds, so the call made is not visible in its error")
	}
	base := t.TempDir()
	dir, links := walkedLinks(t, base)
	parent := fileRes(dir, map[string]any{"recurse": true, "mode": "0600", "group": "0"})
	for link := range links {
		changes, _, err := File{}.Apply(context.Background(),
			childOf(parent, link, map[string]any{"mode": "0600", "group": "0"}), false)
		want := fmt.Sprintf("group changed '%s' to '%s'", gidName(os.Getgid()), gidName(0))
		if err != nil || len(changes) != 1 || changes[0].Property != "group" || changes[0].Message != want ||
			changes[0].Err == nil || !strings.Contains(changes[0].Err.Error(), "lchownat") {
			t.Errorf("%s: Apply = %+v, %v; want only a failed lchown, %q", link, changes, err, want)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s = %v (err %v); it must stay a link", link, fi, err)
		}
	}
	for _, target := range []string{filepath.Join(base, "outside"), filepath.Join(dir, "inside")} {
		fi, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 || int(fi.Sys().(*syscall.Stat_t).Gid) != os.Getgid() {
			t.Errorf("target %s = %04o gid %d, want it untouched", target, fi.Mode().Perm(), fi.Sys().(*syscall.Stat_t).Gid)
		}
	}
}

// Measured as root on Puppet 8.10.0 with noop: each walked link reports its
// owner and group changes with status noop and keeps its ownership.
func TestFileWalkedSymlinkUnderNoopChangesNothing(t *testing.T) {
	if os.Getgid() == 0 {
		t.Skip("the links are already in group 0, so there is no change to withhold")
	}
	base := t.TempDir()
	dir, links := walkedLinks(t, base)
	parent := fileRes(dir, map[string]any{"recurse": true, "group": "0"})
	for link := range links {
		se := enabledSE(link, tmpT)
		changes, _, err := File{se: se}.Apply(context.Background(),
			childOf(parent, link, map[string]any{"group": "0"}), true)
		if err != nil || len(changes) != 2 || changes[0].Property != "group" || changes[0].Err != nil ||
			changes[1].Message != "seltype changed 'tmp_t' to 'etc_t'" || changes[1].Err != nil {
			t.Errorf("%s: Apply = %+v, %v; want a group and a seltype change, neither made", link, changes, err)
		}
		if len(se.sets) != 0 {
			t.Errorf("%s: sets = %v under noop", link, se.sets)
		}
		fi, err := os.Lstat(link)
		if err != nil || int(fi.Sys().(*syscall.Stat_t).Gid) != os.Getgid() {
			t.Errorf("%s = %v (err %v), want its group untouched", link, fi, err)
		}
	}
}

// Measured on Puppet 8.10.0: a recursion with mode => '0644' leaves the
// generated directories 0755 -- the inherited mode, dirmasked.
func TestFilePermissionsOnlyChildReconcilesFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "f", "sub/")
	parent := fileRes(dir, map[string]any{"recurse": true, "mode": "0640"})
	for rel, want := range map[string]string{"f": "mode changed '0644' to '0640'", "sub": "mode changed '0755' to '0750'"} {
		changes, _, err := File{}.Apply(context.Background(),
			childOf(parent, filepath.Join(dir, rel), map[string]any{"mode": "0640"}), false)
		if err != nil || len(changes) != 1 || changes[0].Message != want {
			t.Errorf("%s: Apply = %+v, %v; want %q", rel, changes, err, want)
		}
	}
}

// Purge unlinks files and links and never follows a link. Review Focus 2.
func TestFilePurgeUnlinksASymlinkNeverItsTarget(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "precious", "dst/stale")
	dir := filepath.Join(base, "dst")
	if err := os.Symlink(filepath.Join(base, "precious"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	parent := fileRes(dir, map[string]any{"recurse": true, "purge": true})
	for name, was := range map[string]string{"stale": "file", "link": "link"} {
		changes, _, err := File{}.Apply(context.Background(),
			childOf(parent, filepath.Join(dir, name), map[string]any{"ensure": "absent"}), false)
		if err != nil || len(changes) != 1 || changes[0].Was != was || changes[0].Name != "file_removed" ||
			changes[0].Message != "removed" || len(changes[0].PathNotices) != 0 || changes[0].Err != nil {
			t.Errorf("%s: Apply = %+v, %v; want one file_removed from %q", name, changes, err, was)
		}
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the purge: %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(base, "precious")); err != nil || string(b) != "x" {
		t.Errorf("the link's target = %q (err %v), want it untouched", b, err)
	}
}

// R8, measured on Puppet 8.10.0: a purged directory logs "Not removing
// directory; use 'force' to override" at its own path, reports a successful
// ensure 'directory' -> 'absent' file_removed event, and stays -- every run.
// Under noop the sync never runs, so there is no notice.
func TestFilePurgedDirectoryReportsRemovedAndStays(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "unm/inner")
	unm := filepath.Join(dir, "unm")
	parent := fileRes(dir, map[string]any{"recurse": true, "purge": true})
	child := childOf(parent, unm, map[string]any{"ensure": "absent"})
	for _, noop := range []bool{false, true} {
		changes, _, err := File{}.Apply(context.Background(), child, noop)
		wantNotices := []string{"Not removing directory; use 'force' to override"}
		if noop {
			wantNotices = nil
		}
		if err != nil || len(changes) != 1 {
			t.Fatalf("noop %t: Apply = %+v, %v", noop, changes, err)
		}
		ch := changes[0]
		if ch.Property != "ensure" || ch.Was != "directory" || ch.Is != "absent" || ch.Name != "file_removed" ||
			ch.Message != "removed" || !slices.Equal(ch.PathNotices, wantNotices) || ch.Err != nil {
			t.Errorf("noop %t: change = %+v", noop, ch)
		}
		if fi, err := os.Stat(unm); err != nil || !fi.IsDir() {
			t.Errorf("noop %t: the directory is gone (err %v)", noop, err)
		}
	}
	// An explicit ensure => absent behaves the same (D-5 is retired).
	changes, _, err := File{}.Apply(context.Background(), fileRes(unm, map[string]any{"ensure": "absent"}), false)
	if err != nil || len(changes) != 1 || changes[0].Message != "removed" || len(changes[0].PathNotices) != 1 {
		t.Errorf("explicit absent: Apply = %+v, %v", changes, err)
	}
}

// A directory inside the recursed tree swapped for a symlink after the walk
// must not let a child's write, unlink or chmod leave the tree: os.Root
// refuses an absolute link and a relative one that climbs out. Review
// Focus 1.
func TestFileChildWriteCannotLeaveTheRecursedTree(t *testing.T) {
	for name, target := range map[string]func(base string) string{
		"absolute link":              func(base string) string { return filepath.Join(base, "outside") },
		"relative link climbing out": func(string) string { return "../outside" },
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			tree(t, base, "dst/", "outside/stale", "src.txt")
			dir := filepath.Join(base, "dst")
			if err := os.Symlink(target(base), filepath.Join(dir, "sub")); err != nil {
				t.Fatal(err)
			}
			parent := fileRes(dir, map[string]any{"recurse": true, "purge": true, "mode": "0600"})
			for _, child := range []catalog.Resource{
				childOf(parent, filepath.Join(dir, "sub", "f"), map[string]any{"source": filepath.Join(base, "src.txt")}),
				childOf(parent, filepath.Join(dir, "sub", "stale"), map[string]any{"ensure": "absent"}),
				childOf(parent, filepath.Join(dir, "sub", "stale"), map[string]any{"mode": "0600"}),
			} {
				if _, _, err := (File{}).Apply(context.Background(), child, false); err == nil {
					t.Errorf("%s %v: Apply succeeded through a link out of the tree", child.Title, child.Parameters)
				}
			}
			if _, err := os.Lstat(filepath.Join(base, "outside", "f")); !os.IsNotExist(err) {
				t.Errorf("a write escaped the tree: %v", err)
			}
			fi, err := os.Stat(filepath.Join(base, "outside", "stale"))
			if err != nil {
				t.Fatalf("an unlink escaped the tree: %v", err)
			}
			if got := fi.Mode().Perm(); got != 0o644 {
				t.Errorf("a chmod escaped the tree: mode %04o", got)
			}
		})
	}
}

// The recursed directory itself swapped for a symlink is refused before any
// child operation (openDirNoFollow). Review Focus 1.
func TestFileChildRefusesASwappedParent(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "real/f")
	dir := filepath.Join(base, "dst")
	if err := os.Symlink(filepath.Join(base, "real"), dir); err != nil {
		t.Fatal(err)
	}
	parent := fileRes(dir, map[string]any{"recurse": true, "mode": "0600"})
	_, _, err := File{}.Apply(context.Background(), childOf(parent, filepath.Join(dir, "f"), map[string]any{"mode": "0600"}), false)
	if want := dir + " is a symlink; links is not implemented"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	fi, err := os.Stat(filepath.Join(base, "real", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %04o, want 0644", got)
	}
}

// D-27 (retired): a source directory where a file exists replaces the file;
// a source file where a directory exists fails the child's ensure unless the
// parent's force, which every child inherits (type/file.rb:595-600), lets it
// replace the directory.
func TestFileChildTypeConflictReplacesOrFailsByForce(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "src/f/", "src/g", "dst/f", "dst/g/")
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	parent := fileRes(dst, map[string]any{"recurse": true, "source": src})
	child := func(rel string) catalog.Resource {
		return childOf(parent, filepath.Join(dst, rel), map[string]any{"source": filepath.Join(src, rel)})
	}
	changes, _, err := File{}.Apply(context.Background(), child("f"), false)
	if err != nil || len(changes) != 1 || changes[0].Name != "directory_created" || changes[0].Was != "file" {
		t.Errorf("f: Apply = %+v, %v", changes, err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "f")); err != nil || !fi.IsDir() {
		t.Errorf("dst/f did not become a directory: %v", err)
	}
	changes, _, err = File{}.Apply(context.Background(), child("g"), false)
	if err != nil || len(changes) != 1 || changes[0].Err == nil || changes[0].Was != "directory" {
		t.Errorf("g without force: Apply = %+v, %v", changes, err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "g")); err != nil || !fi.IsDir() {
		t.Errorf("dst/g did not survive the run without force: %v", err)
	}
	// The forced child comes from Generate, so force reaches it only by
	// inheritance from the parent (newchild, type/file.rb:583-603).
	forced := fileRes(dst, map[string]any{"recurse": true, "source": src, "force": true})
	children, _, err := File{}.Generate(context.Background(), forced, noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	var g *catalog.Resource
	for i := range children {
		if children[i].Title == filepath.Join(dst, "g") {
			g = &children[i]
		}
	}
	if g == nil || g.Parameters["force"] != true {
		t.Fatalf("Generate = %v; want dst/g inheriting force", children)
	}
	changes, _, err = File{}.Apply(context.Background(), *g, false)
	if err != nil || len(changes) != 1 || changes[0].Err != nil || changes[0].Name != "file_created" || changes[0].Was != "directory" {
		t.Errorf("g with force: Apply = %+v, %v", changes, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "g")); err != nil || string(b) != "x" {
		t.Errorf("dst/g = %q, %v; want the source's content", b, err)
	}
}

// D-26: a symlink inside a source tree fails that child; it is never
// followed.
func TestFileSymlinkInASourceTreeFailsThatChild(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "src/real", "dst/")
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	if err := os.Symlink(filepath.Join(src, "real"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	parent := fileRes(dst, map[string]any{"recurse": true, "source": src})
	_, _, err := File{}.Apply(context.Background(),
		childOf(parent, filepath.Join(dst, "link"), map[string]any{"source": filepath.Join(src, "link")}), false)
	if want := "source file://" + src + "/link is a symlink; ensure => link is not implemented"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// Under noop the parent a real run would create does not exist, so a child
// reports what it would do and writes nothing.
func TestFileNoopChildOfAMissingDirectoryReportsWithoutWriting(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "src/a")
	dst := filepath.Join(base, "dst")
	parent := fileRes(dst, map[string]any{"recurse": true, "source": filepath.Join(base, "src")})
	changes, _, err := File{}.Apply(context.Background(),
		childOf(parent, filepath.Join(dst, "a"), map[string]any{"source": filepath.Join(base, "src", "a")}), true)
	if err != nil || len(changes) != 1 || changes[0].Name != "file_created" {
		t.Errorf("Apply = %+v, %v; want the one would-be create", changes, err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("noop created %s: %v", dst, err)
	}
}

func TestFileRecursePurgeAreBoundedAtPreflight(t *testing.T) {
	reg := NewRegistry(File{})
	for _, tc := range []struct {
		params map[string]any
		gap    string
	}{
		{map[string]any{"recurse": true, "purge": true}, ""},
		{map[string]any{"recurse": "false", "purge": false}, ""},
		{map[string]any{"recurse": "remote"}, `File[/d]: recurse => "remote" is not implemented`},
		{map[string]any{"purge": "yes"}, `File[/d]: purge => "yes" is not implemented`},
	} {
		gaps := Preflight(reg, &catalog.Catalog{Resources: []catalog.Resource{
			{Type: "File", Title: "/d", Parameters: tc.params},
		}})
		var got string
		if len(gaps) == 1 {
			got = gaps[0].String()
		}
		if len(gaps) > 1 || got != tc.gap {
			t.Errorf("%v: gaps = %v, want %q", tc.params, gaps, tc.gap)
		}
	}
}

// The transaction applies a generated child as a resource of its own, so no
// child may carry the ref of a catalog resource: belowManaged drops a child
// at a managed path as well as below one.
func TestFileGenerateNeverReturnsACatalogResourcesRef(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a/x", "b/c/y", "b/d")
	refs := map[string]bool{"File[" + dir + "/a]": true, "File[" + dir + "/b/c]": true}
	managed := func(ref string) bool { return refs[ref] }
	children, _, err := File{}.Generate(context.Background(),
		fileRes(dir, map[string]any{"recurse": true, "purge": true}), managed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(children), []string{dir + "/b", dir + "/b/d"}; !slices.Equal(got, want) {
		t.Errorf("children = %v, want %v", got, want)
	}
	for _, c := range children {
		if managed(c.Ref()) {
			t.Errorf("child %s carries a catalog resource's ref", c.Ref())
		}
	}
}

// The transaction keys children by Title and finds a child's generated
// ancestors by cleaned path (nearestGenerated), so titles are clean whatever
// the parent's title looks like.
func TestFileGenerateEmitsCleanTitles(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a/b")
	children, _, err := File{}.Generate(context.Background(),
		fileRes(dir+"//", map[string]any{"recurse": true}), noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(children), []string{dir + "/a", dir + "/a/b"}; !slices.Equal(got, want) {
		t.Errorf("children = %v, want %v", got, want)
	}
}

// A directory inside the source tree swapped for a symlink after the walk
// must not let a child read a file outside that tree: the child's source is
// read through an os.Root on the parent's source directory. Review Focus 1.
func TestFileChildSourceReadCannotLeaveTheSourceTree(t *testing.T) {
	for name, target := range map[string]func(base string) string{
		"absolute link":              func(base string) string { return filepath.Join(base, "outside") },
		"relative link climbing out": func(string) string { return "../outside" },
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			tree(t, base, "src/", "dst/sub/", "outside/secret")
			src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
			if err := os.Symlink(target(base), filepath.Join(src, "sub")); err != nil {
				t.Fatal(err)
			}
			parent := fileRes(dst, map[string]any{"recurse": true, "source": src})
			child := childOf(parent, filepath.Join(dst, "sub", "secret"),
				map[string]any{"source": filepath.Join(src, "sub", "secret")})
			if _, _, err := (File{}).Apply(context.Background(), child, false); err == nil {
				t.Error("Apply succeeded reading through a link out of the source tree")
			}
			if _, err := os.Lstat(filepath.Join(dst, "sub", "secret")); !os.IsNotExist(err) {
				t.Errorf("a file outside the source tree was copied: %v", err)
			}
		})
	}
}

// A directory the walk Lstat'd as a directory but that is a fifo by the time
// it is opened must fail the walk at once, not block the agent on the open.
func TestFileGenerateDirectorySwappedForAFifoFailsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "sub/f")
	sub := filepath.Join(dir, "sub")
	beforeWalkOpen = func(path string) {
		if path != sub {
			return
		}
		if err := os.RemoveAll(sub); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(sub, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeWalkOpen = nil })

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, _, err := File{}.Generate(context.Background(), fileRes(dir, map[string]any{"recurse": true}), noneManaged)
		done <- result{err}
	}()
	select {
	case res := <-done:
		if res.err == nil || !strings.HasPrefix(res.err.Error(), "read directory "+sub+": ") || !errors.Is(res.err, syscall.ENOTDIR) {
			t.Errorf("err = %v, want a read failure naming %s wrapping ENOTDIR", res.err, sub)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Generate blocked opening the fifo that replaced a directory")
	}
}

// The recursed directory itself, swapped for a fifo after the walk's Lstat,
// must fail at once too: os.OpenRoot alone would block on it.
func TestFileGenerateRecursedDirectorySwappedForAFifoFailsInsteadOfBlocking(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dst")
	tree(t, dir, "f")
	beforeDirOpen = func(d string) {
		if d != dir {
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
		if err := syscall.Mkfifo(dir, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeDirOpen = nil })

	done := make(chan error, 1)
	go func() {
		_, _, err := File{}.Generate(context.Background(), fileRes(dir, map[string]any{"recurse": true}), noneManaged)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("err = %v, want ENOTDIR", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Generate blocked opening the fifo that replaced the recursed directory")
	}
}
