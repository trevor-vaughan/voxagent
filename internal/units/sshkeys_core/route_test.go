package sshkeyscore

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

// fixtureLib is the lib directory of one release's fixture files.
func fixtureLib(release string) string {
	return filepath.Join("testdata", "sshkeys_core-"+release, "lib")
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

func routeSshkey(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Sshkey", Title: "web", Parameters: params}
}

// routeEnv is a run on the measured platform with the given libdir; each test
// changes what it exercises.
func routeEnv(libdir string, rs ...catalog.Resource) *unit.Env {
	return &unit.Env{
		Catalog:  &catalog.Catalog{Resources: rs},
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Vardir:   "/var/lib/puppet",
		Platform: raltest.FakePlatform{Release: raltest.CentOS10},
		Environ:  func(k string) string { return map[string]string{"LANG": "en_US.UTF-8"}[k] },
	}
}

func delegatedWhy(t *testing.T, d unit.Decision) string {
	t.Helper()
	if len(d.Providers) != 0 {
		t.Fatalf("routed natively, want delegated: %+v", d)
	}
	return d.Delegated["sshkey"]
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
	if strings.Join(m.Requires, ",") != "group,file,user" || strings.Join(m.Types, ",") != "sshkey" {
		t.Errorf("requires %v, types %v", m.Requires, m.Types)
	}
	if len(m.Releases) != 2 || m.Releases[0].Version != "2.5.0" || m.Releases[1].Version != "3.1.0" {
		t.Fatalf("releases: %+v", m.Releases)
	}
	for _, rel := range m.Releases {
		if len(rel.Files) != 4 {
			t.Errorf("%s files %+v", rel.Version, rel.Files)
		}
		if got := rel.Types["sshkey"]; strings.Join(got, ",") != strings.Join(sshkeyFiles, ",") {
			t.Errorf("%s sshkey files %v", rel.Version, got)
		}
	}
}

func TestRouteNoSshkeyIsEmpty(t *testing.T) {
	env := routeEnv(t.TempDir(), catalog.Resource{Type: "File", Title: "/x"})
	env.Platform = raltest.FakePlatform{} // would fail condition 1 if an Sshkey were held
	if d := Unit.Route(env); len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestRouteNative(t *testing.T) {
	for name, c := range map[string]struct{ libdir, release string }{
		"empty libdir is the vendored release": {t.TempDir(), "2.5.0"},
		"2.5.0 fixtures":                       {libdirOf(t, "2.5.0"), "2.5.0"},
		"3.1.0 fixtures":                       {libdirOf(t, "3.1.0"), "3.1.0"},
	} {
		t.Run(name, func(t *testing.T) {
			d := Unit.Route(routeEnv(c.libdir, routeSshkey(nil), routeSshkey(map[string]any{"provider": "parsed"})))
			if len(d.Providers) != 1 || len(d.Delegated) != 0 {
				t.Fatalf("got %+v", d)
			}
			s, ok := d.Providers[0].(Sshkey)
			if !ok {
				t.Fatalf("provider %T", d.Providers[0])
			}
			if s.release != c.release {
				t.Errorf("release %q, want %q", s.release, c.release)
			}
		})
	}
}

func TestRouteProviderGetsRunDependencies(t *testing.T) {
	env := routeEnv(t.TempDir(), routeSshkey(nil))
	env.Environ = func(string) string { return "" }
	s := Unit.Route(env).Providers[0].(Sshkey)
	if s.encoding != "US-ASCII" {
		t.Errorf("encoding %q for an environment with no locale", s.encoding)
	}
	if s.backup == nil || s.relabel == nil {
		t.Errorf("hooks: backup nil %t, relabel nil %t", s.backup == nil, s.relabel == nil)
	}
	env.Environ = func(k string) string { return map[string]string{"LANG": "en_US.UTF-8"}[k] }
	vardir := t.TempDir()
	env.Vardir = vardir
	target := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(target, []byte("a ssh-rsa AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = Unit.Route(env).Providers[0].(Sshkey)
	if s.encoding != "UTF-8" {
		t.Errorf("encoding %q under LANG=en_US.UTF-8", s.encoding)
	}
	if err := s.backup(t.Context(), target); err != nil {
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
	purge := catalog.Resource{Type: "Resources", Title: "sshkey", Parameters: map[string]any{"purge": true}}
	badShape := routeSshkey(map[string]any{"name": 1})
	foreign := routeSshkey(map[string]any{"provider": "other"})

	allBroken := func(t *testing.T) *unit.Env {
		libdir := libdirOf(t, "2.5.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/sshkey/other.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		env := routeEnv(libdir, foreign, badShape, purge)
		env.Platform = raltest.FakePlatform{Release: "ID=debian\nVERSION_ID=12\n"}
		return env
	}
	fixPlatform := func(t *testing.T, env *unit.Env) { env.Platform = raltest.FakePlatform{Release: raltest.CentOS10} }
	fixProvider := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[0].Parameters, "provider") }
	fixTree := func(t *testing.T, env *unit.Env) {
		if err := os.Remove(filepath.Join(env.Libdir, "puppet/provider/sshkey/other.rb")); err != nil {
			t.Fatal(err)
		}
	}
	fixPurge := func(t *testing.T, env *unit.Env) { delete(env.Catalog.Resources[2].Parameters, "purge") }
	steps := []func(*testing.T, *unit.Env){fixPlatform, fixProvider, fixTree, fixPurge}

	cases := []struct {
		name  string
		fixed int // how many of steps are applied before the break
		want  string
	}{
		{"platform", 0, "not the measured platform"},
		{"provider", 1, "provider other"},
		{"tree", 2, "another sshkey provider in libdir"},
		{"purge", 3, "resources purge of sshkey"},
		{"shape", 4, "Sshkey[web]: name => 1 is not implemented"},
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

func TestRouteEverySshkeyChecked(t *testing.T) {
	d := Unit.Route(routeEnv(t.TempDir(), routeSshkey(nil), routeSshkey(map[string]any{"provider": "other"})))
	if why := delegatedWhy(t, d); why == "" {
		t.Fatal("a foreign provider on the second Sshkey did not delegate")
	}
}

func TestRouteTreeConditions(t *testing.T) {
	for _, release := range []string{"2.5.0", "3.1.0"} {
		t.Run("one of the two files only "+release, func(t *testing.T) {
			libdir := libdirOf(t, release)
			if err := os.Remove(filepath.Join(libdir, "puppet/provider/sshkey/parsed.rb")); err != nil {
				t.Fatal(err)
			}
			_, want, _ := unit.MatchFiles(plugintree.LibdirTree(libdir), Manifest, "sshkey", nil)
			if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeSshkey(nil)))); got != want || want == "" {
				t.Fatalf("reason %q, want MatchFiles's %q", got, want)
			}
		})
	}
	t.Run("unmeasured digest", func(t *testing.T) {
		libdir := libdirOf(t, "2.5.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/type/sshkey.rb"), []byte("# edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeSshkey(nil)))); !strings.Contains(got, "puppet/type/sshkey.rb has sha256") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider beside measured files", func(t *testing.T) {
		libdir := libdirOf(t, "3.1.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/sshkey/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeSshkey(nil)))); got != "another sshkey provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider with no sshkeys_core file", func(t *testing.T) {
		libdir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(libdir, "puppet/provider/sshkey"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/sshkey/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeSshkey(nil)))); got != "another sshkey provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a non-ruby file in the provider directory", func(t *testing.T) {
		libdir := libdirOf(t, "2.5.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/sshkey/README"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := Unit.Route(routeEnv(libdir, routeSshkey(nil))); len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("the other type's file does not identify the release", func(t *testing.T) {
		libdir := t.TempDir()
		if err := os.CopyFS(libdir, os.DirFS(fixtureLib("3.1.0"))); err != nil {
			t.Fatal(err)
		}
		for _, f := range sshkeyFiles {
			if err := os.Remove(filepath.Join(libdir, f)); err != nil {
				t.Fatal(err)
			}
		}
		d := Unit.Route(routeEnv(libdir, routeSshkey(nil)))
		if len(d.Providers) != 1 || d.Providers[0].(Sshkey).release != "2.5.0" {
			t.Fatalf("got %+v", d)
		}
	})
}

// TestRouteValueShapeUnderEveryRelease: a non-String key is delegated whichever
// release the tree holds, the shape check running with the release found.
func TestRouteValueShapeUnderEveryRelease(t *testing.T) {
	r := routeSshkey(map[string]any{"key": 1})
	for _, release := range []string{"2.5.0", "3.1.0"} {
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdirOf(t, release), r))); !strings.Contains(got, "key => 1 is not implemented") {
			t.Fatalf("%s: reason %q", release, got)
		}
	}
}

func TestRoutePurge(t *testing.T) {
	purging := catalog.Resource{Type: "Resources", Title: "sshkey", Parameters: map[string]any{"purge": "yes"}}
	if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), routeSshkey(nil), purging))); got != "resources purge of sshkey" {
		t.Fatalf("reason %q", got)
	}
	other := catalog.Resource{Type: "Resources", Title: "cron", Parameters: map[string]any{"purge": true}}
	if d := Unit.Route(routeEnv(t.TempDir(), routeSshkey(nil), other)); len(d.Providers) != 1 {
		t.Fatalf("another type's purge delegated sshkey: %+v", d)
	}
}
