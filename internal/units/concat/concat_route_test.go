package concat

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/unit"
)

// fakeFingerprints returns a manifest whose measured digests are those of
// fixed fake bytes, so routing is tested without the real module files, and
// each version's two fake files in concatTypeFiles order.
func fakeFingerprints() (unit.Manifest, map[ConcatVersion][2][]byte) {
	files := map[ConcatVersion][2][]byte{
		Concat910:  {[]byte("fake 9.1.0 concat_file.rb\n"), []byte("fake 9.1.0 concat_fragment.rb\n")},
		Concat1001: {[]byte("fake 10.0.1 concat_file.rb\n"), []byte("fake 10.0.1 concat_fragment.rb\n")},
	}
	m := Manifest
	m.Releases = nil
	for _, rel := range Manifest.Releases {
		f := files[releaseVersions[rel.Version]]
		rel.Files = []unit.FileDigest{
			{Path: concatTypeFiles[0], SHA256: raltest.SHA256Hex(f[0])},
			{Path: concatTypeFiles[1], SHA256: raltest.SHA256Hex(f[1])},
		}
		m.Releases = append(m.Releases, rel)
	}
	return m, files
}

// writeTree writes a libdir holding the two type files into a temp dir.
func writeTree(t *testing.T, files [2][]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "puppet", "type"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, rel := range concatTypeFiles {
		if err := os.WriteFile(filepath.Join(dir, rel), files[i], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNativeConcatFingerprints(t *testing.T) {
	m, fake := fakeFingerprints()
	v9, v10 := fake[Concat910], fake[Concat1001]
	file, frag := concatTypeFiles[0], concatTypeFiles[1]
	cases := []struct {
		name string
		dir  func(t *testing.T) string
		want ConcatVersion
	}{
		{"9.1.0", func(t *testing.T) string { return writeTree(t, v9) }, Concat910},
		{"10.0.1", func(t *testing.T) string { return writeTree(t, v10) }, Concat1001},
		{"symlinked files", func(t *testing.T) string {
			src := writeTree(t, v10)
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "puppet", "type"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, rel := range concatTypeFiles {
				if err := os.Symlink(filepath.Join(src, rel), filepath.Join(dir, rel)); err != nil {
					t.Fatal(err)
				}
			}
			return dir
		}, Concat1001},
		{"one byte changed", func(t *testing.T) string {
			b := []byte(string(v9[1]))
			b[len(b)/2] ^= 1
			return writeTree(t, [2][]byte{v9[0], b})
		}, 0},
		{"byte appended", func(t *testing.T) string {
			return writeTree(t, [2][]byte{append([]byte(string(v10[0])), '\n'), v10[1]})
		}, 0},
		{"mixed pair", func(t *testing.T) string { return writeTree(t, [2][]byte{v9[0], v10[1]}) }, 0},
		{"fragment missing", func(t *testing.T) string {
			dir := writeTree(t, v10)
			if err := os.Remove(filepath.Join(dir, frag)); err != nil {
				t.Fatal(err)
			}
			return dir
		}, 0},
		{"file missing", func(t *testing.T) string {
			dir := writeTree(t, v10)
			if err := os.Remove(filepath.Join(dir, file)); err != nil {
				t.Fatal(err)
			}
			return dir
		}, 0},
		{"empty libdir", func(t *testing.T) string { return t.TempDir() }, 0},
		{"libdir missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "none") }, 0},
		{"unreadable: a directory", func(t *testing.T) string {
			dir := writeTree(t, v10)
			if err := os.Remove(filepath.Join(dir, file)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, file), 0o755); err != nil {
				t.Fatal(err)
			}
			return dir
		}, 0},
		{"1 MiB + 1", func(t *testing.T) string {
			return writeTree(t, [2][]byte{append(make([]byte, 1<<20), v10[0]...), v10[1]})
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, _, ok := nativeConcat(plugintree.LibdirTree(c.dir(t)), &catalog.Catalog{}, m)
			if ok != (c.want != 0) || v != c.want {
				t.Fatalf("got %d %v, want %d %v", v, ok, c.want, c.want != 0)
			}
		})
	}
}

// A FIFO in place of a type file is not measured, and reading it does not
// wait for a writer.
func TestNativeConcatFIFO(t *testing.T) {
	m, fake := fakeFingerprints()
	dir := writeTree(t, fake[Concat1001])
	at := filepath.Join(dir, concatTypeFiles[0])
	if err := os.Remove(at); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(at, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		_, _, ok := nativeConcat(plugintree.LibdirTree(dir), &catalog.Catalog{}, m)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a FIFO was fingerprinted as measured")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

func TestLibdirTreeReadLimit(t *testing.T) {
	dir := writeTree(t, [2][]byte{make([]byte, 1<<20), nil})
	if b, err := plugintree.LibdirTree(dir).Read(concatTypeFiles[0]); err != nil || len(b) != 1<<20 {
		t.Fatalf("1 MiB: %d bytes, %v; want it read", len(b), err)
	}
	if err := os.WriteFile(filepath.Join(dir, concatTypeFiles[0]), make([]byte, 1<<20+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := plugintree.LibdirTree(dir).Read(concatTypeFiles[0]); err == nil {
		t.Fatal("1 MiB + 1: read, want an error")
	}
}

// fakeTree serves fixed bytes per relative path; any other path is missing.
type fakeTree map[string][]byte

func (f fakeTree) Read(rel string) ([]byte, error) {
	b, ok := f[rel]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}

// List names the entries directly under rel.
func (f fakeTree) List(rel string) ([]string, error) {
	var names []string
	for p := range f {
		if name, ok := strings.CutPrefix(p, rel+"/"); ok && !strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	if names == nil {
		return nil, os.ErrNotExist
	}
	return names, nil
}

func TestNativeConcatShapes(t *testing.T) {
	// The tree serves the fake 10.0.1 files, so only the catalog decides.
	m, fake := fakeFingerprints()
	files := fake[Concat1001]
	measured := fakeTree{concatTypeFiles[0]: files[0], concatTypeFiles[1]: files[1]}
	file := func(title string, params map[string]any) catalog.Resource { return concatFileRes(title, params) }
	frag := func(params map[string]any) catalog.Resource {
		p := map[string]any{"target": "/etc/a", "content": "x"}
		for k, v := range params {
			p[k] = v
		}
		return concatFragRes("f", p)
	}
	cases := []struct {
		name string
		rs   []catalog.Resource
		want bool
	}{
		{"plain", []catalog.Resource{file("/etc/a", nil), frag(nil)}, true},
		{"no concat at all", nil, true},
		{"format plain", []catalog.Resource{file("/etc/a", map[string]any{"format": "plain"})}, true},
		{"format json", []catalog.Resource{file("/etc/a", map[string]any{"format": "json"})}, false},
		{"format toml stays native", []catalog.Resource{file("/etc/a", map[string]any{"format": "toml"})}, true},
		{"fragment source", []catalog.Resource{file("/etc/a", nil), frag(map[string]any{"source": "/tmp/s"})}, false},
		{"backup true", []catalog.Resource{file("/etc/a", map[string]any{"backup": true})}, false},
		{"backup false", []catalog.Resource{file("/etc/a", map[string]any{"backup": false})}, true},
		{"backup string", []catalog.Resource{file("/etc/a", map[string]any{"backup": ".bak"})}, true},
		{"trailing slash", []catalog.Resource{file("/etc/x/", nil)}, false},
		{"double slash", []catalog.Resource{file("/etc//x", nil)}, false},
		{"dot segment", []catalog.Resource{file("/etc/./x", nil)}, false},
		{"dotdot segment", []catalog.Resource{file("/etc/../x", nil)}, false},
		{"root", []catalog.Resource{file("/", nil)}, false},
		{"windows drive", []catalog.Resource{file("C:/x", nil)}, false},
		{"path param unclean", []catalog.Resource{file("t", map[string]any{"path": "/etc//x"})}, false},
		{"relative path stays native", []catalog.Resource{file("x", map[string]any{"path": "relative.conf"})}, true},
		{"two share a path", []catalog.Resource{file("/etc/a", nil), file("b", map[string]any{"path": "/etc/a"})}, false},
		{"two distinct paths", []catalog.Resource{file("/etc/a", nil), file("/etc/b", nil)}, true},
		{"tag hash", []catalog.Resource{file("/etc/a", map[string]any{"tag": map[string]any{}})}, false},
		{"fragment tag hash", []catalog.Resource{frag(map[string]any{"tag": map[string]any{}})}, false},
		{"rich value", []catalog.Resource{file("/etc/a", map[string]any{"owner": map[string]any{"__ptype": "Sensitive", "__pvalue": "x"}})}, false},
		{"fragment rich value", []catalog.Resource{frag(map[string]any{"content": map[string]any{"__ptype": "Binary", "__pvalue": "eA=="}})}, false},
		{"order 2^53", []catalog.Resource{frag(map[string]any{"order": float64(9007199254740992)})}, false},
		{"order 2^53-1 stays", []catalog.Resource{frag(map[string]any{"order": float64(9007199254740991)})}, true},
		{"Ruby refusal stays native", []catalog.Resource{frag(map[string]any{"order": "a/b"})}, true},
		{"other types ignored", []catalog.Resource{{Type: "File", Title: "/etc//a", Parameters: map[string]any{"backup": true}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, _, ok := nativeConcat(measured, &catalog.Catalog{Resources: c.rs}, m)
			if ok != c.want {
				t.Fatalf("native = %v, want %v", ok, c.want)
			}
			if ok && v != Concat1001 {
				t.Fatalf("version = %d, want %d", v, Concat1001)
			}
		})
	}
}

// concatModule returns the lib/ of the committed copy of puppetlabs-concat
// release v's two type files (see testdata/README).
func concatModule(t *testing.T, v ConcatVersion) string {
	t.Helper()
	name := map[ConcatVersion]string{Concat910: "lib-9.1.0", Concat1001: "lib"}[v]
	if name == "" {
		t.Fatalf("no concat testdata for version %d", v)
	}
	return filepath.Join("testdata", name)
}

// The committed fixtures are byte-identical to the releases whose digests
// Manifest holds, so the constants and the fixtures cannot drift.
func TestConcatTestdataMatchesManifest(t *testing.T) {
	for _, rel := range Manifest.Releases {
		lib := concatModule(t, releaseVersions[rel.Version])
		for _, f := range rel.Files {
			b, err := os.ReadFile(filepath.Join(lib, f.Path))
			if err != nil {
				t.Fatal(err)
			}
			if got := raltest.SHA256Hex(b); got != f.SHA256 {
				t.Errorf("release %s %s: sha256 %s, want %s", rel.Version, f.Path, got, f.SHA256)
			}
		}
	}
}

// The real releases' files carry the digests spec §0 row 2 lists.
func TestNativeConcatRealTrees(t *testing.T) {
	for _, v := range []ConcatVersion{Concat910, Concat1001} {
		lib := concatModule(t, v)
		var files [2][]byte
		for i, rel := range concatTypeFiles {
			b, err := os.ReadFile(filepath.Join(lib, rel))
			if err != nil {
				t.Fatal(err)
			}
			files[i] = b
		}
		if got, _, ok := nativeConcat(plugintree.LibdirTree(writeTree(t, files)), &catalog.Catalog{}, Manifest); !ok || got != v {
			t.Errorf("version %d tree: got %d %v", v, got, ok)
		}
	}
}

// Each reason Route reports names the first condition that sent concat to the
// host (spec §6).
func TestConcatDelegationReasons(t *testing.T) {
	m, fake := fakeFingerprints()
	files := fake[Concat1001]
	measured := fakeTree{concatTypeFiles[0]: files[0], concatTypeFiles[1]: files[1]}
	frag := concatFragRes("f", map[string]any{"target": "/etc/a", "content": "x", "tag": map[string]any{}})
	cases := []struct {
		name string
		tree fakeTree
		rs   []catalog.Resource
		want string
	}{
		{"unimplemented value", measured, []catalog.Resource{frag}, `Concat_fragment[f]: tag => map[] is not implemented`},
		{"two share a path", measured, []catalog.Resource{
			concatFileRes("/etc/a", nil), concatFileRes("b", map[string]any{"path": "/etc/a"}),
		}, "two concat_file resources manage /etc/a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reason, ok := nativeConcat(c.tree, &catalog.Catalog{Resources: c.rs}, m)
			if ok || reason != c.want {
				t.Fatalf("got %v %q, want false %q", ok, reason, c.want)
			}
		})
	}
}

// With no measured release in the tree Route delegates both concat types with
// MatchFiles' own reason, and only the types the catalog holds.
func TestConcatRouteDelegatesWithMatchFilesReason(t *testing.T) {
	tree := fakeTree{}
	_, want, _ := unit.MatchFiles(tree, Manifest, "concat_file", nil)
	rs := []catalog.Resource{concatFileRes("/etc/a", nil)}
	d := Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Tree: tree})
	if want == "" || len(d.Delegated) != 1 || d.Delegated["concat_file"] != want {
		t.Fatalf("Delegated = %v, want only concat_file: %q", d.Delegated, want)
	}
}
