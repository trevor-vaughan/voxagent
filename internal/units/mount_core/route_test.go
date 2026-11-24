package mountcore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/unit"
)

const (
	wantMount  = "/usr/bin/mount"
	wantUmount = "/usr/bin/umount"
)

// fixtureLib is the lib directory of one release's fixture files.
func fixtureLib(release string) string {
	return filepath.Join("testdata", "mount_core-"+release, "lib")
}

// libdirOf copies a release's fixture files into a fresh libdir the test may
// change.
func libdirOf(t *testing.T, release string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(fixtureLib(release))); err != nil {
		t.Fatal(err)
	}
	return dir
}

func routeMount(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Mount", Title: title, Parameters: params}
}

// routeEnv is a run on the measured platform with mount and umount on PATH
// and the given libdir; each test changes what it exercises.
func routeEnv(libdir string, rs ...catalog.Resource) *unit.Env {
	return &unit.Env{
		Catalog:  &catalog.Catalog{Resources: rs},
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Vardir:   "/var/lib/puppet",
		Platform: raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{wantMount: true, wantUmount: true}},
		Environ:  func(k string) string { return map[string]string{"PATH": "/usr/bin:/bin", "LANG": "en_US.UTF-8"}[k] },
	}
}

func delegatedWhy(t *testing.T, d unit.Decision) string {
	t.Helper()
	if len(d.Providers) != 0 {
		t.Fatalf("routed natively, want delegated: %+v", d)
	}
	return d.Delegated["mount"]
}

func TestFixturesMatchManifest(t *testing.T) {
	for _, rel := range Manifest.Releases {
		for _, f := range rel.Files {
			b, err := os.ReadFile(filepath.Join(fixtureLib(rel.Version), f.Path))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
				t.Errorf("%s %s: sha256 %s, manifest %s", rel.Version, f.Path, got, f.SHA256)
			}
		}
	}
}

func TestManifestShape(t *testing.T) {
	m := Manifest
	if m.Oracle != (unit.Origin{Implementation: "openvox", Version: "8.34.0"}) || m.Source.Version != "" {
		t.Errorf("oracle %+v source %+v", m.Oracle, m.Source)
	}
	if strings.Join(m.Requires, ",") != "group,file" || strings.Join(m.Types, ",") != "mount" {
		t.Errorf("requires %v, types %v", m.Requires, m.Types)
	}
	if len(m.Releases) != 2 || m.Releases[0].Version != "1.3.0" || m.Releases[1].Version != "2.1.0" {
		t.Fatalf("releases: %+v", m.Releases)
	}
	want := []string{"puppet/type/mount.rb", "puppet/provider/mount/parsed.rb", "puppet/provider/mount.rb"}
	for _, rel := range m.Releases {
		var paths []string
		for _, f := range rel.Files {
			paths = append(paths, f.Path)
		}
		if !slices.Equal(paths, want) || !slices.Equal(rel.Types["mount"], want) {
			t.Errorf("%s: files %v, mount files %v", rel.Version, paths, rel.Types["mount"])
		}
	}
}

func TestRouteNoMountIsEmpty(t *testing.T) {
	env := routeEnv(t.TempDir(), catalog.Resource{Type: "File", Title: "/x"})
	env.Platform = raltest.FakePlatform{} // would fail conditions 1 and 2 if a Mount were held
	if d := Unit.Route(env); len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestRouteNative(t *testing.T) {
	for name, c := range map[string]struct{ libdir, release string }{
		"empty libdir is the vendored release": {t.TempDir(), "1.3.0"},
		"1.3.0 fixtures":                       {libdirOf(t, "1.3.0"), "1.3.0"},
		"2.1.0 fixtures":                       {libdirOf(t, "2.1.0"), "2.1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Unit.Route(routeEnv(c.libdir, routeMount("/mnt/a", nil), routeMount("/mnt/b", map[string]any{"provider": "parsed"})))
			if len(d.Providers) != 1 || len(d.Delegated) != 0 {
				t.Fatalf("got %+v", d)
			}
			m, ok := d.Providers[0].(Mount)
			if !ok || m.release != c.release {
				t.Fatalf("provider %+v, want release %s", d.Providers[0], c.release)
			}
		})
	}
}

func TestRouteProviderGetsRunDependencies(t *testing.T) {
	env := routeEnv(t.TempDir(), routeMount("/mnt/a", nil))
	env.Environ = func(k string) string { return map[string]string{"PATH": "/usr/bin"}[k] }
	m := Unit.Route(env).Providers[0].(Mount)
	if m.mountPath != wantMount || m.umountPath != wantUmount {
		t.Errorf("paths %q %q, want %q %q", m.mountPath, m.umountPath, wantMount, wantUmount)
	}
	if m.encoding != "US-ASCII" {
		t.Errorf("encoding %q for an environment with no locale", m.encoding)
	}
	if m.run == nil || m.backup == nil || m.relabel == nil || m.now == nil {
		t.Errorf("run, hooks or clock unset: %+v", m)
	}
	env.Environ = func(k string) string { return map[string]string{"PATH": "/usr/bin", "LANG": "en_US.UTF-8"}[k] }
	if got := Unit.Route(env).Providers[0].(Mount).encoding; got != "UTF-8" {
		t.Errorf("encoding %q under LANG=en_US.UTF-8", got)
	}
}

// TestRouteResolvesCommandsAsPuppetDoes: Util.which over PATH, then /usr/sbin
// and /sbin appended; each command is named alone when it is the one missing,
// mount first.
func TestRouteResolvesCommandsAsPuppetDoes(t *testing.T) {
	cases := []struct {
		name, path string
		exec       []string
		wantWhy    string
		wantPaths  [2]string
	}{
		{"both on PATH", "/usr/bin", []string{"/usr/bin/mount", "/usr/bin/umount"}, "", [2]string{"/usr/bin/mount", "/usr/bin/umount"}},
		{"puppet appends /usr/sbin and /sbin", "/usr/bin", []string{"/usr/sbin/mount", "/sbin/umount"}, "", [2]string{"/usr/sbin/mount", "/sbin/umount"}},
		{"mount missing", "/usr/bin", []string{"/usr/bin/umount"}, "mount not found on PATH", [2]string{}},
		{"only umount missing", "/usr/bin", []string{"/usr/bin/mount"}, "umount not found on PATH", [2]string{}},
		{"both missing names mount", "/usr/bin", nil, "mount not found on PATH", [2]string{}},
		{"elsewhere", "/usr/bin", []string{"/opt/mount", "/opt/umount"}, "mount not found on PATH", [2]string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := routeEnv(t.TempDir(), routeMount("/mnt/a", nil))
			exec := map[string]bool{}
			for _, p := range c.exec {
				exec[p] = true
			}
			env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Exec: exec}
			env.Environ = func(k string) string { return map[string]string{"PATH": c.path}[k] }
			d := Unit.Route(env)
			if c.wantWhy != "" {
				if got := delegatedWhy(t, d); got != c.wantWhy {
					t.Fatalf("reason %q, want %q", got, c.wantWhy)
				}
				return
			}
			if len(d.Providers) != 1 {
				t.Fatalf("got %+v", d)
			}
			if m := d.Providers[0].(Mount); [2]string{m.mountPath, m.umountPath} != c.wantPaths {
				t.Fatalf("paths %q %q, want %v", m.mountPath, m.umountPath, c.wantPaths)
			}
		})
	}
}

// TestRouteConditionsInOrder breaks one condition at a time and then every
// later one too, so each case proves the first failing condition is the
// reason.
func TestRouteConditionsInOrder(t *testing.T) {
	purge := catalog.Resource{Type: "Resources", Title: "mount", Parameters: map[string]any{"purge": true}}
	badShape := routeMount("/mnt/shape", map[string]any{"fstype": 1})
	foreign := routeMount("/mnt/foreign", map[string]any{"provider": "other"})
	badName := routeMount("/mnt/a(b", nil)
	aFile := catalog.Resource{Type: "File", Title: "/mnt/a(b/f"}

	allBroken := func(t *testing.T) *unit.Env {
		libdir := libdirOf(t, "1.3.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/mount/other.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		env := routeEnv(libdir, foreign, badShape, badName, aFile, purge)
		env.Platform = raltest.FakePlatform{Release: "ID=debian\nVERSION_ID=12\n"}
		return env
	}
	fixPlatform := func(t *testing.T, env *unit.Env) { env.Platform = raltest.FakePlatform{Release: raltest.CentOS10} }
	fixMount := func(t *testing.T, env *unit.Env) {
		env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{wantMount: true}}
	}
	fixUmount := func(t *testing.T, env *unit.Env) {
		env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{wantMount: true, wantUmount: true}}
	}
	fixProvider := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[0].Parameters, "provider") }
	fixTree := func(t *testing.T, env *unit.Env) {
		if err := os.Remove(filepath.Join(env.Libdir, "puppet/provider/mount/other.rb")); err != nil {
			t.Fatal(err)
		}
	}
	fixPurge := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[4].Parameters, "purge") }
	fixShape := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[1].Parameters, "fstype") }
	steps := []func(*testing.T, *unit.Env){fixPlatform, fixMount, fixUmount, fixProvider, fixTree, fixPurge, fixShape}

	cases := []struct {
		name  string
		fixed int // how many of steps are applied before the break
		want  string
	}{
		{"platform", 0, "not the measured platform"},
		{"mount", 1, "mount not found on PATH"},
		{"umount", 2, "umount not found on PATH"},
		{"provider", 3, "provider other"},
		{"tree", 4, "another mount provider in libdir"},
		{"purge", 5, "resources purge of mount"},
		{"shape", 6, "Mount[/mnt/shape]: fstype => 1 is not implemented"},
		{"autobefore pattern", 7, "mount name /mnt/a(b is not a portable regular expression"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := allBroken(t)
			for _, f := range steps[:c.fixed] {
				f(t, env)
			}
			if got := delegatedWhy(t, Unit.Route(env)); got != c.want {
				t.Fatalf("reason %q, want %q", got, c.want)
			}
		})
	}
}

func TestRouteEveryMountChecked(t *testing.T) {
	d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a", nil), routeMount("/mnt/b", map[string]any{"provider": "other"})))
	if why := delegatedWhy(t, d); why == "" {
		t.Fatal("a foreign provider on the second Mount did not delegate")
	}
}

func TestRouteValueShapes(t *testing.T) {
	for _, attr := range []string{"fstype", "options", "device"} {
		t.Run(attr, func(t *testing.T) {
			want := "Mount[/mnt/a]: " + attr + " => 1 is not implemented"
			d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a", map[string]any{attr: 1})))
			if got := delegatedWhy(t, d); got != want {
				t.Fatalf("reason %q, want %q", got, want)
			}
		})
	}
	t.Run("name", func(t *testing.T) {
		r := routeMount("m", map[string]any{"name": 1})
		if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), r))); !strings.HasSuffix(got, "is not implemented") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("integers in dump and pass are implemented", func(t *testing.T) {
		d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a", map[string]any{"dump": 1, "pass": 2})))
		if len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestRouteTreeConditions(t *testing.T) {
	for _, release := range []string{"1.3.0", "2.1.0"} {
		t.Run("one of the three files only "+release, func(t *testing.T) {
			libdir := libdirOf(t, release)
			if err := os.Remove(filepath.Join(libdir, "puppet/provider/mount/parsed.rb")); err != nil {
				t.Fatal(err)
			}
			_, want, _ := unit.MatchFiles(plugintree.LibdirTree(libdir), Manifest, "mount", nil)
			if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeMount("/mnt/a", nil)))); got != want || want == "" {
				t.Fatalf("reason %q, want MatchFiles's %q", got, want)
			}
		})
	}
	t.Run("unmeasured digest", func(t *testing.T) {
		libdir := libdirOf(t, "2.1.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/type/mount.rb"), []byte("# edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeMount("/mnt/a", nil)))); !strings.Contains(got, "puppet/type/mount.rb has sha256") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider beside measured files", func(t *testing.T) {
		libdir := libdirOf(t, "1.3.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/mount/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeMount("/mnt/a", nil)))); got != "another mount provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider with no mount_core file", func(t *testing.T) {
		libdir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(libdir, "puppet/provider/mount"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/mount/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeMount("/mnt/a", nil)))); got != "another mount provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a non-ruby file in the provider directory", func(t *testing.T) {
		libdir := libdirOf(t, "1.3.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/mount/README"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := Unit.Route(routeEnv(libdir, routeMount("/mnt/a", nil))); len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestRoutePurge(t *testing.T) {
	purging := catalog.Resource{Type: "Resources", Title: "mount", Parameters: map[string]any{"purge": "yes"}}
	if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a", nil), purging))); got != "resources purge of mount" {
		t.Fatalf("reason %q", got)
	}
	other := catalog.Resource{Type: "Resources", Title: "cron", Parameters: map[string]any{"purge": true}}
	if d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a", nil), other)); len(d.Providers) != 1 {
		t.Fatalf("another type's purge delegated mount: %+v", d)
	}
}

// TestRouteAutobeforePattern is condition 7: the name matters only when the
// catalog holds a File.
func TestRouteAutobeforePattern(t *testing.T) {
	file := catalog.Resource{Type: "File", Title: "/mnt/f"}
	reason := func(n string) string { return "mount name " + n + " is not a portable regular expression" }

	t.Run("no File: an uncompilable name is native", func(t *testing.T) {
		d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a(b", nil)))
		if len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
	for _, name := range []string{`/mnt/a(b`, `/mnt/a\b`, `/mnt/a(?b`, `/mnt/a{,3}`, `/mnt/a[b]`} {
		t.Run("a File delegates "+name, func(t *testing.T) {
			got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), routeMount(name, nil), file)))
			if got != reason(name) {
				t.Fatalf("reason %q, want %q", got, reason(name))
			}
		})
	}
	t.Run("a File and a name with operators only is native", func(t *testing.T) {
		d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a.b+c", nil), routeMount("/mnt/a_b-c", nil), file))
		if len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("the munged name is the one interpolated", func(t *testing.T) {
		d := Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/x", map[string]any{"name": "/mnt/a(b//"}), file))
		if got := delegatedWhy(t, d); got != reason("/mnt/a(b") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a File of any case of the type", func(t *testing.T) {
		lower := catalog.Resource{Type: "file", Title: "/mnt/f"}
		if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), routeMount("/mnt/a(b", nil), lower))); got != reason("/mnt/a(b") {
			t.Fatalf("reason %q", got)
		}
	})
}

// TestRouteInvalidMountValidatesBeforeThePattern: Ruby refuses an invalid
// Mount at catalog conversion, before the graph build compiles any autobefore
// pattern, so a Mount that fails validation is routed natively whatever its
// name and a File beside it, and its refusal is the validation's (Ruby's
// order); the valid Mount's name is checked as usual.
func TestRouteInvalidMountValidatesBeforeThePattern(t *testing.T) {
	invalid := routeMount("/mnt/a(b", map[string]any{"dump": "abc"})
	file := catalog.Resource{Type: "File", Title: "/mnt/a(b/f"}
	d := Unit.Route(routeEnv(t.TempDir(), invalid, routeMount("/mnt/ok", nil), file))
	if len(d.Providers) != 1 {
		t.Fatalf("got %+v, want native", d)
	}
	err := d.Providers[0].(Mount).Validate(invalid)
	if want := `Parameter dump failed on Mount[/mnt/a(b]: Invalid value "abc". Valid values match /(0|1)/.`; err == nil || err.Error() != want {
		t.Fatalf("Validate = %v, want %q", err, want)
	}
	valid := routeMount("/mnt/c(d", nil)
	if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), invalid, valid, file))); got != "mount name /mnt/c(d is not a portable regular expression" {
		t.Fatalf("reason %q", got)
	}
}

// TestRouteFilesAreMungedPathsInCatalogOrder: the provider's files are what
// the graph's File refs use (file.FilePath), in catalog order.
func TestRouteFilesAreMungedPathsInCatalogOrder(t *testing.T) {
	rs := []catalog.Resource{
		{Type: "File", Title: "/mnt//z/"},
		routeMount("/mnt", nil),
		{Type: "File", Title: "t", Parameters: map[string]any{"path": "/mnt/a/../b"}},
		{Type: "Notify", Title: "/mnt/n"},
		{Type: "File", Title: "/mnt/c"},
	}
	m := Unit.Route(routeEnv(t.TempDir(), rs...)).Providers[0].(Mount)
	if want := []string{"/mnt/z", "/mnt/b", "/mnt/c"}; !slices.Equal(m.files, want) {
		t.Fatalf("files %v, want %v", m.files, want)
	}
}
