package tidy

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/units/file"
)

const (
	tidyFile = "/etc/puppetlabs/code/environments/production/manifests/site.pp"
	tidyLine = 3
)

// past is far enough back that no clock skew between writing and generating
// a tree can change an age comparison.
var past = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// tree makes each path under dir: a trailing "/" is a directory, anything
// else an empty file. Parents come first in the list.
func tree(t *testing.T, dir string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(dir, p)
		var err error
		if strings.HasSuffix(p, "/") {
			err = os.Mkdir(full, 0o755)
		} else {
			err = os.WriteFile(full, nil, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// tidyCatalog is Tidy[path] in Class[Main], after extra.
func tidyCatalog(path string, params map[string]any, extra ...catalog.Resource) (*catalog.Catalog, catalog.Resource) {
	if params == nil {
		params = map[string]any{}
	}
	r := catalog.Resource{Type: "Tidy", Title: path, Tags: []string{"tidy", "class"},
		File: tidyFile, Line: tidyLine, Kind: "compilable_type", Parameters: params}
	cat := &catalog.Catalog{Resources: append([]catalog.Resource{{Type: "Class", Title: "Main"}}, extra...)}
	cat.Resources = append(cat.Resources, r)
	cat.Edges = []catalog.Edge{{Source: "Class[Main]", Target: r.Ref()}}
	return cat, r
}

func generate(t *testing.T, cat *catalog.Catalog, r catalog.Resource) ral.LoadGeneration {
	t.Helper()
	g, err := Tidy{}.LoadGenerate(cat, r)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// child is the File Tidy[tidyPath] generates for path, before extra parameters.
func child(path string, extra map[string]any) catalog.Resource {
	params := map[string]any{"ensure": "absent", "force": true, "backup": false, "loglevel": "notice"}
	for k, v := range extra {
		params[k] = v
	}
	return catalog.Resource{Type: "File", Title: path, Tags: []string{"file", "tidy", "class"}, Parameters: params}
}

// generated is what tidying children (already sorted descending) from
// Tidy[tidyPath] amounts to, every child ordered before the Tidy, with the
// debug lines in candidate order.
func generated(tidyPath string, candidates []string, children ...catalog.Resource) ral.LoadGeneration {
	ref := "Tidy[" + tidyPath + "]"
	g := ral.LoadGeneration{DepthFirst: true}
	if len(children) == 0 {
		return g
	}
	g.Containers, g.Generators = map[string]string{}, map[string]string{}
	for _, c := range children {
		g.Resources = append(g.Resources, c)
		g.Containers[c.Ref()] = "Class[Main]"
		g.Generators[c.Ref()] = ref
		g.Before = append(g.Before, catalog.Edge{Source: c.Ref(), Target: ref})
	}
	for _, p := range candidates {
		g.Logs = append(g.Logs, tidyLog(tidyPath, "debug", "Tidying File["+p+"]"))
	}
	g.Logs = append(g.Logs, tidyLog(tidyPath, "notice", fmt.Sprintf("Tidying %d files", len(children))))
	return g
}

func tidyLog(tidyPath, level, msg string) report.LogEntry {
	return report.LogEntry{Level: level, Source: "Tidy[" + tidyPath + "]", Message: msg, File: tidyFile, Line: tidyLine}
}

func failed(tidyPath, msg string, before ...report.LogEntry) ral.LoadGeneration {
	return ral.LoadGeneration{DepthFirst: true, Failed: true,
		Logs: append(before, tidyLog(tidyPath, "err", "Failed to generate additional resources using 'generate': "+msg))}
}

// assertGen compares exactly; with unorderedDebug the "Tidying File[...]"
// lines, whose order is readdir's, are compared as a set.
func assertGen(t *testing.T, got, want ral.LoadGeneration, unorderedDebug bool) {
	t.Helper()
	if unorderedDebug {
		norm := func(ls []report.LogEntry) []report.LogEntry {
			ls = slices.Clone(ls)
			slices.SortStableFunc(ls, func(a, b report.LogEntry) int {
				if a.Level == "debug" && b.Level == "debug" {
					return strings.Compare(a.Message, b.Message)
				}
				return 0
			})
			return ls
		}
		got.Logs, want.Logs = norm(got.Logs), norm(want.Logs)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", got, want)
	}
}

func TestGenerateNoRecurseFile(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "f", "s/", "s/x")
	f := filepath.Join(dir, "f")
	cat, r := tidyCatalog(f, nil)
	assertGen(t, generate(t, cat, r), generated(f, []string{f}, child(f, nil)), false)

	// A directory without recursion or rmdirs is not a candidate.
	s := filepath.Join(dir, "s")
	cat, r = tidyCatalog(s, nil)
	assertGen(t, generate(t, cat, r), generated(s, nil), false)

	// With rmdirs it is, alone (TY:294).
	cat, r = tidyCatalog(s, map[string]any{"rmdirs": true})
	assertGen(t, generate(t, cat, r), generated(s, []string{s}, child(s, nil)), false)
}

func TestGenerateMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, params := range []map[string]any{nil, {"recurse": true, "max_files": "5"}} {
		cat, r := tidyCatalog(missing, params)
		want := ral.LoadGeneration{DepthFirst: true, Logs: []report.LogEntry{tidyLog(missing, "debug", "File does not exist")}}
		assertGen(t, generate(t, cat, r), want, false)
	}
}

func TestGenerateRecurseLimits(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a", "s/", "s/b", "s/t/", "s/t/c")
	j := func(p string) string { return filepath.Join(dir, p) }
	for _, tc := range []struct {
		recurse any
		want    []string // sorted descending
	}{
		{float64(0), nil},
		{float64(-1), nil},
		{false, nil},
		{float64(1), []string{j("a")}},
		{"2", []string{j("s/b"), j("a")}},
		{true, []string{j("s/t/c"), j("s/b"), j("a")}},
		{"inf", []string{j("s/t/c"), j("s/b"), j("a")}},
	} {
		t.Run(fmt.Sprint(tc.recurse), func(t *testing.T) {
			cat, r := tidyCatalog(dir, map[string]any{"recurse": tc.recurse})
			var children []catalog.Resource
			for _, p := range tc.want {
				children = append(children, child(p, nil))
			}
			assertGen(t, generate(t, cat, r), generated(dir, tc.want, children...), true)
		})
	}
}

func TestGenerateMatches(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a.log", "b.txt", ".hidden.log", "s/", "s/c.log", "1pub.tmp")
	j := func(p string) string { return filepath.Join(dir, p) }
	for _, tc := range []struct {
		name    string
		matches any
		want    []string
	}{
		// FNM_DOTMATCH: * matches a leading dot.
		{"glob", "*.log", []string{j("s/c.log"), j("a.log"), j(".hidden.log")}},
		// Flattened, as [value].flatten (TY:107); a class and a negation.
		{"nested array", []any{[]any{"[0-9]pub*.tmp"}, "[!a-z]*.txt"}, []string{j("1pub.tmp")}},
		{"empty array", []any{}, nil},
		// Braces are literal without FNM_EXTGLOB.
		{"braces", "{a,b}.log", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, r := tidyCatalog(dir, map[string]any{"recurse": true, "matches": tc.matches})
			var children []catalog.Resource
			for _, p := range tc.want {
				children = append(children, child(p, nil))
			}
			assertGen(t, generate(t, cat, r), generated(dir, tc.want, children...), true)
		})
	}

	// File.fnmatch's ArgumentError fails generation (ARG:24-29).
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true, "matches": "a\x00"})
	assertGen(t, generate(t, cat, r), failed(dir, "string contains null byte"), false)
}

func TestGenerateAgeByType(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "old-atime", "old-mtime")
	recent := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "old-atime"), past, recent); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "old-mtime"), recent, past); err != nil {
		t.Fatal(err)
	}
	saved := now
	t.Cleanup(func() { now = saved })
	j := func(p string) string { return filepath.Join(dir, p) }
	for _, tc := range []struct {
		typ   any
		later time.Duration // how far now is moved past the wall clock
		want  []string
	}{
		{nil, 0, []string{j("old-atime")}},
		{"atime", 0, []string{j("old-atime")}},
		{"mtime", 0, []string{j("old-mtime")}},
		// ctime is the inode change, now for both: only a later now ages it.
		{"ctime", 0, nil},
		{"ctime", 48 * time.Hour, []string{j("old-mtime"), j("old-atime")}},
	} {
		t.Run(fmt.Sprint(tc.typ, tc.later), func(t *testing.T) {
			at := time.Now().Add(tc.later)
			now = func() time.Time { return at }
			params := map[string]any{"recurse": true, "age": "1d"}
			if tc.typ != nil {
				params["type"] = tc.typ
			}
			cat, r := tidyCatalog(dir, params)
			var children []catalog.Resource
			for _, p := range tc.want {
				children = append(children, child(p, nil))
			}
			assertGen(t, generate(t, cat, r), generated(dir, tc.want, children...), true)
		})
	}
}

func TestGenerateSize(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{"small": 1023, "exact": 1024, "big": 4096, "old": 0} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "old"), past, past); err != nil {
		t.Fatal(err)
	}
	j := func(p string) string { return filepath.Join(dir, p) }
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   []string
	}{
		{"size", map[string]any{"size": "1k"}, []string{j("exact"), j("big")}},
		{"bytes", map[string]any{"size": "4096b"}, []string{j("big")}},
		// age and size are OR'd (TY:357-364).
		{"age or size", map[string]any{"size": "2k", "age": "1w"}, []string{j("old"), j("big")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params["recurse"] = true
			cat, r := tidyCatalog(dir, tc.params)
			var children []catalog.Resource
			for _, p := range tc.want {
				children = append(children, child(p, nil))
			}
			assertGen(t, generate(t, cat, r), generated(dir, tc.want, children...), true)
		})
	}
}

// Each candidate is logged in candidate order (breadth-first), then the
// count; the Files themselves are sorted descending. Every directory here
// has one entry, so readdir order cannot vary.
func TestGenerateLogOrder(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "s/", "s/u/", "s/u/y")
	j := func(p string) string { return filepath.Join(dir, p) }
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true, "rmdirs": true})
	want := generated(dir, []string{dir, j("s"), j("s/u"), j("s/u/y")},
		child(j("s/u/y"), nil),
		child(j("s/u"), map[string]any{"require": []any{"File[" + j("s/u/y") + "]"}}),
		child(j("s"), map[string]any{"require": []any{"File[" + j("s/u") + "]"}}),
		child(dir, map[string]any{"require": []any{"File[" + j("s") + "]"}}))
	assertGen(t, generate(t, cat, r), want, false)
}

func TestGenerateRmdirsRequires(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "s/", "s/x", "s/u/", "s/u/y")
	j := func(p string) string { return filepath.Join(dir, p) }
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true, "rmdirs": true, "require": "Notify[n]"})
	ref := func(p string) string { return "File[" + p + "]" }
	// Visiting paths descending (TY:311): s/x, s/u/y, s/u, s, dir.
	want := generated(dir, []string{dir, j("s"), j("s/x"), j("s/u"), j("s/u/y")},
		child(j("s/x"), map[string]any{"require": "Notify[n]"}),
		child(j("s/u/y"), map[string]any{"require": "Notify[n]"}),
		child(j("s/u"), map[string]any{"require": []any{"Notify[n]", ref(j("s/u/y"))}}),
		child(j("s"), map[string]any{"require": []any{"Notify[n]", ref(j("s/x")), ref(j("s/u"))}}),
		child(dir, map[string]any{"require": []any{"Notify[n]", ref(j("s"))}}))
	assertGen(t, generate(t, cat, r), want, true)
	if r.Parameters["require"] != "Notify[n]" {
		t.Errorf("the Tidy's require changed: %#v", r.Parameters["require"])
	}
}

func TestGenerateManagedExcluded(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a", "b", "c")
	j := func(p string) string { return filepath.Join(dir, p) }
	byTitle := catalog.Resource{Type: "File", Title: j("a"), Parameters: map[string]any{"ensure": "file"}}
	byPath := catalog.Resource{Type: "File", Title: "keep b", Parameters: map[string]any{"path": j("b") + "/"}}
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true}, byTitle, byPath)
	assertGen(t, generate(t, cat, r), generated(dir, []string{j("c")}, child(j("c"), nil)), false)
}

// The lookup matches a File by its title even when its path is elsewhere, by
// its path once cleaned, and nothing but a File.
func TestGenerateManagedLookup(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a", "b", "c", "d")
	j := func(p string) string { return filepath.Join(dir, p) }
	titleOnly := catalog.Resource{Type: "File", Title: j("a"), Parameters: map[string]any{"path": j("elsewhere")}}
	uncleanTitle := catalog.Resource{Type: "File", Title: dir + "//./b", Parameters: map[string]any{"ensure": "file"}}
	notAFile := catalog.Resource{Type: "Notify", Title: j("c")}
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true}, titleOnly, uncleanTitle, notAFile)
	assertGen(t, generate(t, cat, r), generated(dir, []string{j("d"), j("c")},
		child(j("d"), nil), child(j("c"), nil)), true)
}

func TestGenerateSecondTidySkipsFirstsFiles(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a.log", "b")
	j := func(p string) string { return filepath.Join(dir, p) }
	first := catalog.Resource{Type: "Tidy", Title: "logs", Tags: []string{"tidy", "class"}, File: tidyFile, Line: tidyLine,
		Parameters: map[string]any{"path": dir, "recurse": float64(1), "matches": "*.log"}}
	cat, second := tidyCatalog(dir, map[string]any{"recurse": true}, first)
	cat.Edges = append(cat.Edges, catalog.Edge{Source: "Class[Main]", Target: "Tidy[logs]"})
	g := generate(t, cat, first)
	if len(g.Resources) != 1 || g.Resources[0].Title != j("a.log") {
		t.Fatalf("first Tidy generated %#v", g.Resources)
	}
	if err := catalog.MergeGeneration(cat, g.Generation, catalog.PlaceBeforeGenerator); err != nil {
		t.Fatal(err)
	}
	assertGen(t, generate(t, cat, second), generated(dir, []string{j("b")}, child(j("b"), nil)), false)
}

func TestGenerateCopiesMetaparams(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "a")
	a := filepath.Join(dir, "a")
	params := map[string]any{"recurse": true, "require": "Notify[r]", "notify": []any{"Notify[n]"},
		"subscribe": "Notify[s]", "before": "Notify[b]", "tag": "extra::x"}
	cat, r := tidyCatalog(dir, params)
	r.Tags = []string{"tidy", "class", "extra::x", "extra", "x"}
	c := child(a, map[string]any{"require": "Notify[r]", "notify": []any{"Notify[n]"},
		"subscribe": "Notify[s]", "before": "Notify[b]", "tag": "extra::x"})
	// The tag metaparameter is copied before merge_tags_from (type.rb:365-370, ARG:139).
	c.Tags = []string{"file", "extra::x", "extra", "x", "tidy", "class"}
	assertGen(t, generate(t, cat, r), generated(dir, []string{a}, c), false)

	// The Tidy already requires the child: no before edge (ARG:172-194).
	cat, r = tidyCatalog(dir, map[string]any{"recurse": true, "require": []any{"File[" + a + "]"}})
	want := generated(dir, []string{a}, child(a, map[string]any{"require": []any{"File[" + a + "]"}}))
	want.Before = nil
	assertGen(t, generate(t, cat, r), want, false)
}

func TestGenerateSortedDescendingBeforeTidy(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "B", "a", "Z")
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true})
	g := generate(t, cat, r)
	if err := catalog.MergeGeneration(cat, g.Generation, catalog.PlaceBeforeGenerator); err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, x := range cat.Resources {
		refs = append(refs, x.Ref())
	}
	j := func(p string) string { return "File[" + filepath.Join(dir, p) + "]" }
	if want := []string{"Class[Main]", j("a"), j("Z"), j("B"), r.Ref()}; !slices.Equal(refs, want) {
		t.Errorf("catalog order = %q, want %q", refs, want)
	}
	// loadGenerate judges the new Files as a catalog's.
	fresh := &catalog.Catalog{Resources: g.Resources}
	reg := ral.NewRegistry(file.File{})
	if gaps, invalid := ral.Preflight(reg, fresh), ral.Validate(reg, fresh); len(gaps)+len(invalid) != 0 {
		t.Errorf("generated Files refused: gaps %v, invalid %v", gaps, invalid)
	}
}

func TestGenerateMaxFiles(t *testing.T) {
	small := t.TempDir()
	tree(t, small, "a", "b", "s/", "s/c")
	j := func(p string) string { return filepath.Join(small, p) }
	all := []catalog.Resource{child(j("s/c"), nil), child(j("b"), nil), child(j("a"), nil)}
	allPaths := []string{j("s/c"), j("b"), j("a")}

	// A String passes validation and fails Fileset's comparison (FS:70), but
	// only when Fileset is built: with recursion.
	cat, r := tidyCatalog(small, map[string]any{"recurse": true, "max_files": "5"})
	assertGen(t, generate(t, cat, r), failed(small, "comparison of String with 0 failed"), false)
	cat, r = tidyCatalog(small, map[string]any{"recurse": float64(0), "max_files": "5"})
	assertGen(t, generate(t, cat, r), failed(small, "comparison of String with 0 failed"), false)
	cat, r = tidyCatalog(small, map[string]any{"max_files": "5"})
	assertGen(t, generate(t, cat, r), generated(small, nil), false)

	// Four entries, not counting the path itself.
	cat, r = tidyCatalog(small, map[string]any{"recurse": true, "max_files": float64(4)})
	assertGen(t, generate(t, cat, r), generated(small, allPaths, all...), true)
	cat, r = tidyCatalog(small, map[string]any{"recurse": true, "max_files": float64(3)})
	assertGen(t, generate(t, cat, r), failed(small, "The directory '"+small+"' contains 4 entries, which exceeds the limit of 3 specified by the max_files parameter for this resource. The limit may be increased, but be aware that large number of file resources can result in excessive resource consumption and degraded performance. Consider using an alternate method to manage large directory trees"), false)

	// The limit is checked before the names are (FS:70 before FS:78).
	tree(t, small, "bad\xff")
	cat, r = tidyCatalog(small, map[string]any{"recurse": true, "max_files": float64(1)})
	if g := generate(t, cat, r); !g.Failed || !strings.Contains(g.Logs[0].Message, "contains 5 entries") {
		t.Errorf("max_files with a non-UTF-8 name = %#v, want the limit's failure", g)
	}

	big := t.TempDir()
	for i := range 1001 {
		tree(t, big, fmt.Sprintf("f%04d", i))
	}
	warning := report.LogEntry{Level: "warning", Source: "Puppet", Message: "The directory '" + big + "' contains 1001 entries, which exceeds the default soft limit 1000 and may cause excessive resource consumption and degraded performance. To remove this warning set a value for `max_files` parameter or consider using an alternate method to manage large directory trees"}
	cat, r = tidyCatalog(big, map[string]any{"recurse": true, "matches": "none"})
	assertGen(t, generate(t, cat, r), ral.LoadGeneration{DepthFirst: true, Logs: []report.LogEntry{warning}}, false)
	// The warning stays when generation then fails.
	tree(t, big, "bad\xff")
	cat, r = tidyCatalog(big, map[string]any{"recurse": true})
	want := failed(big, "invalid byte sequence in UTF-8", warning)
	want.Logs[0].Message = strings.Replace(warning.Message, "1001", "1002", 1)
	assertGen(t, generate(t, cat, r), want, false)
	if err := os.Remove(filepath.Join(big, "bad\xff")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(big, "f0000")); err != nil {
		t.Fatal(err)
	}
	// 1000 entries: no warning.
	if g := generate(t, cat, r); len(g.Resources) != 1000 || g.Failed || len(g.Logs) != 1001 || g.Logs[1000].Message != "Tidying 1000 files" {
		t.Errorf("1000 entries: %d resources, failed %v, %d logs", len(g.Resources), g.Failed, len(g.Logs))
	}
}

func TestGenerateNonUTF8(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "ok", "bad\xff", "s/", "s/deep\xfe")
	for _, params := range []map[string]any{
		{"recurse": true},
		{"recurse": true, "matches": "ok"},
		{"recurse": true, "size": "1b"},
	} {
		cat, r := tidyCatalog(dir, params)
		assertGen(t, generate(t, cat, r), failed(dir, "invalid byte sequence in UTF-8"), false)
	}
	// Names past the depth limit are never read.
	s := filepath.Join(dir, "s")
	cat, r := tidyCatalog(s, map[string]any{"recurse": float64(0)})
	assertGen(t, generate(t, cat, r), generated(s, nil), false)
	// Nor without recursion.
	ok := filepath.Join(dir, "ok")
	cat, r = tidyCatalog(ok, nil)
	assertGen(t, generate(t, cat, r), generated(ok, []string{ok}, child(ok, nil)), false)
}

func TestGenerateEACCES(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir, other := t.TempDir(), t.TempDir()
	tree(t, dir, "y", "locked/", "locked/x")
	tree(t, other, "noexec/", "noexec/z", "unread/")
	j := func(p string) string { return filepath.Join(dir, p) }
	o := func(p string) string { return filepath.Join(other, p) }
	for p, mode := range map[string]os.FileMode{j("locked"): 0o000, o("noexec"): 0o600, o("unread"): 0o300} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}

	// S4: an unreadable directory in the walk fails generation (FS:141).
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true})
	assertGen(t, generate(t, cat, r), failed(dir, "Permission denied @ dir_initialize - "+j("locked")), false)
	// So does the path itself.
	cat, r = tidyCatalog(o("unread"), map[string]any{"recurse": true})
	assertGen(t, generate(t, cat, r), failed(o("unread"), "Permission denied @ dir_initialize - "+o("unread")), false)

	// A directory that lists but cannot be searched: its entries cannot be
	// stat'd, which TY:377-380 warns about and skips.
	cat, r = tidyCatalog(o("noexec"), map[string]any{"recurse": true})
	want := ral.LoadGeneration{DepthFirst: true, Logs: []report.LogEntry{
		tidyLog(o("noexec"), "warning", "Could not stat; permission denied")}}
	assertGen(t, generate(t, cat, r), want, false)
	// The same for the path itself (TY:277).
	z := o("noexec/z")
	cat, r = tidyCatalog(z, nil)
	want = ral.LoadGeneration{DepthFirst: true, Logs: []report.LogEntry{tidyLog(z, "warning", "Could not stat; permission denied")}}
	assertGen(t, generate(t, cat, r), want, false)
}

// An errno lstat does not rescue (TY:372-381) fails generation with Ruby's
// SystemCallError text.
func TestGenerateENOTDIR(t *testing.T) {
	dir := t.TempDir()
	tree(t, dir, "f")
	p := filepath.Join(dir, "f", "x")
	cat, r := tidyCatalog(p, nil)
	assertGen(t, generate(t, cat, r), failed(p, "Not a directory @ rb_file_s_lstat - "+p), false)
}

// Fileset keeps "/" unchomped (FS:34) and File.join("/", "tmp") is "/tmp".
func TestGenerateRootPath(t *testing.T) {
	if fi, err := os.Lstat("/tmp"); err != nil || !fi.IsDir() {
		t.Skip("no /tmp directory")
	}
	cat, r := tidyCatalog("/", map[string]any{"recurse": float64(1), "rmdirs": true, "matches": "tmp"})
	assertGen(t, generate(t, cat, r), generated("/", []string{"/tmp"}, child("/tmp", nil)), false)
}

func TestGenerateSymlinkedDirNotDescended(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tree(t, outside, "f")
	if err := os.Symlink(outside, filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	l := filepath.Join(dir, "l")
	cat, r := tidyCatalog(dir, map[string]any{"recurse": true})
	assertGen(t, generate(t, cat, r), generated(dir, []string{l}, child(l, nil)), false)
}

func TestGeneratePathIsLink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	tree(t, outside, "f")
	l := filepath.Join(dir, "l")
	if err := os.Symlink(outside, l); err != nil {
		t.Fatal(err)
	}
	for _, params := range []map[string]any{nil, {"recurse": true}} {
		cat, r := tidyCatalog(l, params)
		assertGen(t, generate(t, cat, r), generated(l, []string{l}, child(l, nil)), false)
	}
}
