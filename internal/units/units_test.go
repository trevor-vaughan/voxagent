package units_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units"
)

// root is the repository root; go test runs in this package's directory.
const root = "../.."

// modulePath reads the module's import path from go.mod.
func modulePath(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if path, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(path)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

// goFiles lists every .go file under dir (relative to the repository root),
// test files included, as slash paths.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// importsOf returns the import paths of the file at path (relative to the
// repository root).
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, p)
	}
	return out
}

// unitDirs lists the directory names under internal/units.
func unitDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

// A unit imports another unit only if its manifest requires it, and the
// packages below the units never import a unit (layering rules 1-5 of the
// restructure: libraries and ral are importable by units, not the reverse).
func TestImportsWithinRequires(t *testing.T) {
	mod := modulePath(t)
	unitsPrefix := mod + "/internal/units/"
	byName := map[string]unit.Manifest{}
	for _, u := range units.All() {
		byName[u.Manifest().Name] = u.Manifest()
	}
	for _, name := range unitDirs(t) {
		m, ok := byName[name]
		if !ok {
			t.Errorf("internal/units/%s is not in units.All()", name)
			continue
		}
		for _, file := range goFiles(t, "internal/units/"+name) {
			for _, imp := range importsOf(t, file) {
				rest, ok := strings.CutPrefix(imp, unitsPrefix)
				if !ok {
					continue
				}
				// A unit's own subpackage (augeasproviders_core/augeastest) is the unit.
				if dep, _, _ := strings.Cut(rest, "/"); dep != name && !slices.Contains(m.Requires, dep) {
					t.Errorf("%s imports unit %q, which %s's Requires %v does not list", file, dep, name, m.Requires)
				}
			}
		}
	}

	// Rules 1, 2, 3, 5: what each layer may never import.
	ral, unitPkg := mod+"/internal/ral", mod+"/internal/unit"
	for _, layer := range []struct {
		dir       string
		forbidden []string // import path, or prefix when it ends in "/"
	}{
		{"internal/lib", []string{ral, unitPkg, unitsPrefix}}, // rules 1 and 5 (raltest is under lib)
		{"internal/ral", []string{unitPkg, unitsPrefix}},      // rule 2
		{"internal/unit", []string{unitsPrefix}},              // rule 3
	} {
		for _, file := range goFiles(t, layer.dir) {
			if layer.dir == "internal/ral" && strings.Contains(strings.TrimPrefix(file, "internal/ral/"), "/") {
				continue // ral's testdata and subdirectories hold no packages of ral
			}
			for _, imp := range importsOf(t, file) {
				for _, bad := range layer.forbidden {
					if imp == bad || (strings.HasSuffix(bad, "/") && strings.HasPrefix(imp, bad)) {
						t.Errorf("%s imports %s, which %s may never import", file, imp, layer.dir)
					}
				}
			}
		}
	}
}

// nativeFixtures holds, per manifest type, the minimal resource the all-native
// environment below needs for the type to route native. A unit task that adds
// a type adds its row (and its module unit's testdata/lib) and nothing else.
var nativeFixtures = map[string]catalog.Resource{
	"augeas":                {Type: "Augeas", Title: "x", Parameters: map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"}},
	"ssh_config":            {Type: "Ssh_config", Title: "Ciphers", Parameters: map[string]any{"value": "aes256-ctr"}},
	"sshd_config":           {Type: "Sshd_config", Title: "X11Forwarding", Parameters: map[string]any{"value": "no"}},
	"sshd_config_subsystem": {Type: "Sshd_config_subsystem", Title: "sftp", Parameters: map[string]any{"command": "/usr/libexec/openssh/sftp-server"}},
	"sysctl":                {Type: "Sysctl", Title: "net.ipv4.ip_forward", Parameters: map[string]any{"value": "1"}},
	"concat_file":           {Type: "Concat_file", Title: "/etc/c.conf"},
	"concat_fragment":       {Type: "Concat_fragment", Title: "f", Parameters: map[string]any{"target": "/etc/c.conf", "content": "x"}},
	"cron":                  {Type: "Cron", Title: "c", Parameters: map[string]any{"command": "/bin/true"}},
	"exec":                  {Type: "Exec", Title: "e", Parameters: map[string]any{"command": "/bin/true"}},
	"file":                  {Type: "File", Title: "/tmp/f"},
	"group":                 {Type: "Group", Title: "g"},
	"host":                  {Type: "Host", Title: "h"},
	"notify":                {Type: "Notify", Title: "n"},
	"package":               {Type: "Package", Title: "p"},
	"service":               {Type: "Service", Title: "s"},
	"sshkey":                {Type: "Sshkey", Title: "k"},
	"user":                  {Type: "User", Title: "u"},
}

// copyMissing copies the tree src into dst, leaving files dst already holds:
// puppet/feature/augeas.rb is in two units' testdata, byte for byte.
func copyMissing(dst, src string) error {
	return fs.WalkDir(os.DirFS(src), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		at := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(at, 0o755)
		}
		if _, err := os.Stat(at); err == nil {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		return os.WriteFile(at, b, 0o644)
	})
}

// libProbe is a probe that always succeeds with lib.
type libProbe struct{ lib augeas.Lib }

func (p libProbe) Probe() (augeas.Lib, string, bool) { return p.lib, "", true }

// With everything measured and clean, each type a manifest declares is served
// by a provider RouteAll returns, and nothing is delegated: a manifest that
// names a type its Route does not serve is caught here.
func TestManifestTypesAreRoutedTypes(t *testing.T) {
	libdir := t.TempDir()
	for _, name := range unitDirs(t) {
		lib := filepath.Join(name, "testdata", "lib")
		if _, err := os.Stat(lib); err != nil {
			continue
		}
		if err := copyMissing(libdir, lib); err != nil {
			t.Fatalf("assembling %s: %v", lib, err)
		}
	}
	cat := &catalog.Catalog{}
	for _, u := range units.All() {
		for _, typ := range u.Manifest().Types {
			res, ok := nativeFixtures[typ]
			if !ok {
				t.Fatalf("unit %s declares type %q, which has no row in nativeFixtures", u.Manifest().Name, typ)
			}
			cat.Resources = append(cat.Resources, res)
		}
	}
	env := &unit.Env{
		Catalog:  cat,
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Platform: raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{"/usr/sbin/sysctl": true, "/usr/sbin/crontab": true}, Files: map[string]bool{"/etc/hosts": true}},
		Environ:  func(string) string { return "" },
		Command:  "apply",
		Vardir:   t.TempDir(),
		Augeas:   augeas.NewSession(libProbe{raltest.NewScriptLib(t, nil)}),
	}
	got := unit.RouteAll(env, units.All())
	if len(got.Delegated) != 0 {
		t.Errorf("delegated %v, want none", got.Delegated)
	}
	routed := map[string]bool{}
	for _, p := range got.Providers {
		routed[p.Type()] = true
	}
	for _, u := range units.All() {
		for _, typ := range u.Manifest().Types {
			if !routed[typ] {
				t.Errorf("unit %s declares %q, but RouteAll returned no provider with that Type()", u.Manifest().Name, typ)
			}
		}
	}
}

// A measured digest is written once, in the manifest.go of a unit whose
// releases list it, and nowhere else in the Go sources.
func TestDigestsOwnedByManifests(t *testing.T) {
	t.Run("empty digest is refused", func(t *testing.T) {
		bad := unit.Manifest{Name: "bad", Releases: []unit.Release{{Version: "1.0.0", Files: []unit.FileDigest{{Path: "a.rb"}}}}}
		if got := emptyDigests(bad); len(got) != 1 {
			t.Errorf("emptyDigests(%+v) = %v, want one problem", bad, got)
		}
	})
	owners := map[string][]string{} // digest -> manifest.go paths of the units listing it
	for _, u := range units.All() {
		m := u.Manifest()
		for _, msg := range emptyDigests(m) {
			t.Error(msg)
		}
		for _, r := range m.Releases {
			for _, f := range r.Files {
				owners[f.SHA256] = append(owners[f.SHA256], "internal/units/"+m.Name+"/manifest.go")
			}
		}
	}
	var files []string
	for _, dir := range []string{"internal", "cmd"} {
		files = append(files, goFiles(t, dir)...)
	}
	for digest, ownerFiles := range owners {
		found := map[string]bool{}
		for _, file := range files {
			b, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), digest) {
				found[file] = true
				if !slices.Contains(ownerFiles, file) {
					t.Errorf("digest %s appears in %s, outside the manifests that list it %v", digest, file, ownerFiles)
				}
			}
		}
		for _, owner := range ownerFiles {
			if !found[owner] {
				t.Errorf("digest %s is not written in %s", digest, owner)
			}
		}
	}
}

// emptyDigests names each file of m whose SHA256 is empty: an empty digest
// would otherwise be "found" in every Go source.
func emptyDigests(m unit.Manifest) []string {
	var out []string
	for _, r := range m.Releases {
		for _, f := range r.Files {
			if f.SHA256 == "" {
				out = append(out, fmt.Sprintf("%s release %s: file %s has an empty SHA256", m.Name, r.Version, f.Path))
			}
		}
	}
	return out
}

// moduleWithoutReleases reports a module unit (no Source.Version means a
// third-party module) that measures no release.
func moduleWithoutReleases(m unit.Manifest) bool {
	return m.Source.Version == "" && len(m.Releases) == 0
}

var semver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func TestManifestsWellFormed(t *testing.T) {
	t.Run("module unit without releases is refused", func(t *testing.T) {
		if !moduleWithoutReleases(unit.Manifest{Name: "bad", Releases: []unit.Release{}}) {
			t.Error("a manifest with no Source.Version and no Releases was accepted")
		}
		if moduleWithoutReleases(unit.Manifest{Name: "core", Source: unit.Origin{Version: "8.10.0"}}) {
			t.Error("a core unit (Source.Version set) was refused for having no Releases")
		}
	})
	all := units.All()
	names := map[string]bool{}
	for _, u := range all {
		names[u.Manifest().Name] = true
	}
	dirs := unitDirs(t)
	seen := map[string]bool{}
	for _, u := range all {
		m := u.Manifest()
		if seen[m.Name] {
			t.Errorf("unit name %q is listed twice", m.Name)
		}
		seen[m.Name] = true
		if !slices.Contains(dirs, m.Name) {
			t.Errorf("unit %q has no directory internal/units/%s", m.Name, m.Name)
		}
		if !semver.MatchString(m.Version) {
			t.Errorf("%s: Version %q is not semver", m.Name, m.Version)
		}
		for _, r := range m.Requires {
			if !names[r] {
				t.Errorf("%s requires %q, which units.All() does not list", m.Name, r)
			}
		}
		if moduleWithoutReleases(m) {
			t.Errorf("%s is a module unit (no Source.Version) but lists no Releases", m.Name)
		}
		if len(m.Releases) == 0 {
			continue // a core unit measures no files
		}
		union := map[string]bool{}
		for _, r := range m.Releases {
			for typ, files := range r.Types {
				union[typ] = true
				if len(files) == 0 {
					t.Errorf("%s release %s: type %q lists no files", m.Name, r.Version, typ)
				}
			}
		}
		var want []string
		for typ := range union {
			want = append(want, typ)
		}
		slices.Sort(want)
		got := slices.Sorted(slices.Values(m.Types))
		if !slices.Equal(got, want) {
			t.Errorf("%s: Types %v, want the union of its releases' types %v", m.Name, got, want)
		}
	}
	for _, d := range dirs {
		if !names[d] {
			t.Errorf("internal/units/%s is not in units.All()", d)
		}
	}
}

// internal/ral is the engine: apart from extension.go (the Ruby-host
// extension type) it declares no resource type, i.e. no func (...) Type() string.
func TestRalHoldsNoTypes(t *testing.T) {
	dir := filepath.Join(root, "internal", "ral")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "extension.go" {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Type" || fn.Type.Params.NumFields() != 0 {
				continue
			}
			if res := fn.Type.Results; res != nil && res.NumFields() == 1 {
				if id, ok := res.List[0].Type.(*ast.Ident); ok && id.Name == "string" {
					t.Errorf("internal/ral/%s declares a Type() string method; types belong in internal/units", name)
				}
			}
		}
	}
}
