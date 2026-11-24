package hostcore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/unit"
)

// fixtureLib is the lib directory of the measured release's fixture files.
func fixtureLib() string { return filepath.Join("testdata", "host_core-1.3.0", "lib") }

// libdirOf copies the fixture files into a fresh libdir the test may change.
func libdirOf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(fixtureLib())); err != nil {
		t.Fatal(err)
	}
	return dir
}

func routeHost(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Host", Title: "web", Parameters: params}
}

// routeEnv is a run on the measured platform with /etc/hosts present and the
// given libdir; each test changes what it exercises.
func routeEnv(libdir string, rs ...catalog.Resource) *unit.Env {
	return &unit.Env{
		Catalog:  &catalog.Catalog{Resources: rs},
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Vardir:   "/var/lib/puppet",
		Platform: raltest.FakePlatform{Release: raltest.CentOS10, Files: map[string]bool{"/etc/hosts": true}},
		Environ:  func(k string) string { return map[string]string{"LANG": "en_US.UTF-8"}[k] },
	}
}

func delegatedWhy(t *testing.T, d unit.Decision) string {
	t.Helper()
	if len(d.Providers) != 0 {
		t.Fatalf("routed natively, want delegated: %+v", d)
	}
	return d.Delegated["host"]
}

func TestFixturesMatchManifest(t *testing.T) {
	for _, rel := range Manifest.Releases {
		for _, f := range rel.Files {
			b, err := os.ReadFile(filepath.Join(fixtureLib(), f.Path))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(b)
			if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
				t.Errorf("%s: sha256 %s, manifest %s", f.Path, got, f.SHA256)
			}
		}
	}
}

func TestManifestShape(t *testing.T) {
	m := Manifest
	if m.Oracle != (unit.Origin{Implementation: "openvox", Version: "8.34.0"}) || m.Source.Version != "" {
		t.Errorf("oracle %+v source %+v", m.Oracle, m.Source)
	}
	if strings.Join(m.Requires, ",") != "group,file" || strings.Join(m.Types, ",") != "host" {
		t.Errorf("requires %v, types %v", m.Requires, m.Types)
	}
	if len(m.Releases) != 1 || m.Releases[0].Version != "1.3.0" {
		t.Fatalf("releases: %+v", m.Releases)
	}
	files := m.Releases[0].Files
	if len(files) != 2 || files[0].Path != "puppet/type/host.rb" || files[1].Path != "puppet/provider/host/parsed.rb" {
		t.Errorf("files %+v", files)
	}
	if got := m.Releases[0].Types["host"]; len(got) != 2 || got[0] != files[0].Path || got[1] != files[1].Path {
		t.Errorf("host files %v", got)
	}
}

func TestRouteNoHostIsEmpty(t *testing.T) {
	env := routeEnv(t.TempDir(), catalog.Resource{Type: "File", Title: "/x"})
	env.Platform = raltest.FakePlatform{} // would fail conditions 1 and 2 if a Host were held
	if d := Unit.Route(env); len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestRouteNative(t *testing.T) {
	for name, libdir := range map[string]string{"empty libdir is the vendored release": t.TempDir(), "fixtures": libdirOf(t)} {
		t.Run(name, func(t *testing.T) {
			d := Unit.Route(routeEnv(libdir, routeHost(nil), routeHost(map[string]any{"provider": "parsed"})))
			if len(d.Providers) != 1 || len(d.Delegated) != 0 {
				t.Fatalf("got %+v", d)
			}
			if _, ok := d.Providers[0].(Host); !ok {
				t.Fatalf("provider %T", d.Providers[0])
			}
		})
	}
}

func TestRouteProviderGetsRunDependencies(t *testing.T) {
	env := routeEnv(t.TempDir(), routeHost(nil))
	env.Environ = func(string) string { return "" }
	h := Unit.Route(env).Providers[0].(Host)
	if h.encoding != "US-ASCII" {
		t.Errorf("encoding %q for an environment with no locale", h.encoding)
	}
	if h.backup == nil || h.relabel == nil {
		t.Errorf("hooks: backup nil %t, relabel nil %t", h.backup == nil, h.relabel == nil)
	}
	env.Environ = func(k string) string { return map[string]string{"LANG": "en_US.UTF-8"}[k] }
	vardir := t.TempDir()
	env.Vardir = vardir
	target := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(target, []byte("127.0.0.1 a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h = Unit.Route(env).Providers[0].(Host)
	if h.encoding != "UTF-8" {
		t.Errorf("encoding %q under LANG=en_US.UTF-8", h.encoding)
	}
	if err := h.backup(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(vardir, "clientbucket")); err != nil {
		t.Errorf("backup did not use the run's vardir: %v", err)
	}
}

// TestRouteConditionsInOrder breaks one condition at a time and then every
// later one too, so each case proves the first failing condition is the
// reason.
func TestRouteConditionsInOrder(t *testing.T) {
	purge := catalog.Resource{Type: "Resources", Title: "host", Parameters: map[string]any{"purge": true}}
	badShape := routeHost(map[string]any{"ip": 1})
	foreign := routeHost(map[string]any{"provider": "other"})

	allBroken := func(t *testing.T) *unit.Env {
		libdir := libdirOf(t)
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/host/other.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		env := routeEnv(libdir, foreign, badShape, purge)
		env.Platform = raltest.FakePlatform{Release: "ID=debian\nVERSION_ID=12\n"}
		return env
	}
	fixPlatform := func(t *testing.T, env *unit.Env) {
		env.Platform = raltest.FakePlatform{Release: raltest.CentOS10}
	}
	fixHosts := func(t *testing.T, env *unit.Env) {
		env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Files: map[string]bool{"/etc/hosts": true}}
	}
	fixProvider := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[0].Parameters, "provider") }
	fixTree := func(t *testing.T, env *unit.Env) {
		if err := os.Remove(filepath.Join(env.Libdir, "puppet/provider/host/other.rb")); err != nil {
			t.Fatal(err)
		}
	}
	fixPurge := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[2].Parameters, "purge") }
	steps := []func(*testing.T, *unit.Env){fixPlatform, fixHosts, fixProvider, fixTree, fixPurge}

	cases := []struct {
		name  string
		fixed int // how many of steps are applied before the break
		want  string
	}{
		{"platform", 0, "not the measured platform"},
		{"/etc/hosts", 1, "/etc/hosts does not exist"},
		{"provider", 2, "provider other"},
		{"tree", 3, "another host provider in libdir"},
		{"purge", 4, "resources purge of host"},
		{"shape", 5, "Host[web]: ip => 1 is not implemented"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := allBroken(t)
			for _, f := range steps[:c.fixed] {
				f(t, env)
			}
			got := delegatedWhy(t, Unit.Route(env))
			if got != c.want {
				t.Fatalf("reason %q, want %q", got, c.want)
			}
		})
	}
}

func TestRouteEveryHostChecked(t *testing.T) {
	d := Unit.Route(routeEnv(t.TempDir(), routeHost(nil), routeHost(map[string]any{"provider": "other"})))
	if why := delegatedWhy(t, d); why == "" {
		t.Fatal("a foreign provider on the second Host did not delegate")
	}
}

func TestRouteSuitabilityFollowsSymlinkSemantics(t *testing.T) {
	env := routeEnv(t.TempDir(), routeHost(nil))
	env.Platform = raltest.FakePlatform{Release: raltest.CentOS10}
	if got := delegatedWhy(t, Unit.Route(env)); got != "/etc/hosts does not exist" {
		t.Fatalf("reason %q", got)
	}
}

func TestRouteTreeConditions(t *testing.T) {
	t.Run("one of the two files only", func(t *testing.T) {
		libdir := libdirOf(t)
		if err := os.Remove(filepath.Join(libdir, "puppet/provider/host/parsed.rb")); err != nil {
			t.Fatal(err)
		}
		_, want, _ := unit.MatchFiles(plugintree.LibdirTree(libdir), Manifest, "host", nil)
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeHost(nil)))); got != want || want == "" {
			t.Fatalf("reason %q, want MatchFiles's %q", got, want)
		}
	})
	t.Run("unmeasured digest", func(t *testing.T) {
		libdir := libdirOf(t)
		if err := os.WriteFile(filepath.Join(libdir, "puppet/type/host.rb"), []byte("# edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeHost(nil)))); !strings.Contains(got, "puppet/type/host.rb has sha256") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider beside measured files", func(t *testing.T) {
		libdir := libdirOf(t)
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/host/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeHost(nil)))); got != "another host provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider with no host_core file", func(t *testing.T) {
		libdir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(libdir, "puppet/provider/host"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/host/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeHost(nil)))); got != "another host provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a non-ruby file in the provider directory", func(t *testing.T) {
		libdir := libdirOf(t)
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/host/README"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := Unit.Route(routeEnv(libdir, routeHost(nil))); len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestRoutePurge(t *testing.T) {
	purging := catalog.Resource{Type: "Resources", Title: "host", Parameters: map[string]any{"purge": "yes"}}
	if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), routeHost(nil), purging))); got != "resources purge of host" {
		t.Fatalf("reason %q", got)
	}
	other := catalog.Resource{Type: "Resources", Title: "cron", Parameters: map[string]any{"purge": true}}
	if d := Unit.Route(routeEnv(t.TempDir(), routeHost(nil), other)); len(d.Providers) != 1 {
		t.Fatalf("another type's purge delegated host: %+v", d)
	}
}
