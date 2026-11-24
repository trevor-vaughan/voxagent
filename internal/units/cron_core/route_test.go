package croncore

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
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

const wantCrontab = "/usr/bin/crontab"

// fixtureLib is the lib directory of a measured release's fixture files.
func fixtureLib(release string) string {
	return filepath.Join("testdata", "cron_core-"+release, "lib")
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

func routeCron(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Cron", Title: "job", Parameters: params}
}

// routeEnv is a run on the measured platform with crontab on PATH and the
// given libdir; each test changes what it exercises.
func routeEnv(libdir string, rs ...catalog.Resource) *unit.Env {
	return &unit.Env{
		Catalog:  &catalog.Catalog{Resources: rs},
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Platform: raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{wantCrontab: true}},
		Environ:  func(k string) string { return map[string]string{"PATH": "/usr/bin:/bin", "USER": "alice"}[k] },
		LookupEnv: func(k string) (string, bool) {
			v, ok := map[string]string{"USER": "alice"}[k]
			return v, ok
		},
	}
}

func delegatedWhy(t *testing.T, d unit.Decision) string {
	t.Helper()
	if len(d.Providers) != 0 {
		t.Fatalf("routed natively, want delegated: %+v", d)
	}
	return d.Delegated["cron"]
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
	if len(Manifest.Releases) != 2 || Manifest.Releases[0].Version != "1.3.0" || Manifest.Releases[1].Version != "2.1.0" {
		t.Fatalf("releases: %+v", Manifest.Releases)
	}
	for _, rel := range Manifest.Releases {
		if len(rel.Files) != 3 || len(rel.Types["cron"]) != 3 {
			t.Errorf("%s: files %d, cron files %v", rel.Version, len(rel.Files), rel.Types["cron"])
		}
	}
	if strings.Join(Manifest.Requires, ",") != "group" || strings.Join(Manifest.Types, ",") != "cron" {
		t.Errorf("requires %v, types %v", Manifest.Requires, Manifest.Types)
	}
}

func TestRouteNoCronIsEmpty(t *testing.T) {
	env := routeEnv(t.TempDir(), catalog.Resource{Type: "File", Title: "/x"})
	env.Platform = raltest.FakePlatform{} // would fail condition 1 and 2 if a Cron were held
	if d := Unit.Route(env); len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestRouteNativeReleases(t *testing.T) {
	cases := []struct{ name, libdir, release string }{
		{"empty libdir is the vendored 1.3.0", t.TempDir(), "1.3.0"},
		{"1.3.0 fixtures", libdirOf(t, "1.3.0"), "1.3.0"},
		{"2.1.0 fixtures", libdirOf(t, "2.1.0"), "2.1.0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Unit.Route(routeEnv(c.libdir, routeCron(nil)))
			if len(d.Providers) != 1 || len(d.Delegated) != 0 {
				t.Fatalf("got %+v", d)
			}
			cron, ok := d.Providers[0].(Cron)
			if !ok || cron.release != c.release || cron.crontab != wantCrontab {
				t.Fatalf("provider %+v, want release %s crontab %s", d.Providers[0], c.release, wantCrontab)
			}
		})
	}
}

func TestRouteProviderGetsRunDependencies(t *testing.T) {
	var sink strings.Builder
	env := routeEnv(t.TempDir(), routeCron(nil))
	env.Stderr = &sink
	cron := Unit.Route(env).Providers[0].(Cron)
	if cron.stderr != &sink {
		t.Fatal("the provider does not write to env.Stderr")
	}
	if user, set := cron.environ("USER"); !set || user != "alice" {
		t.Fatalf("USER = %q, %v", user, set)
	}
	if _, set := cron.environ("NO_SUCH_VARIABLE"); set {
		t.Fatal("an unset variable reads as set")
	}
	env.LookupEnv = func(k string) (string, bool) { return "", k == "USER" }
	if user, set := Unit.Route(env).Providers[0].(Cron).environ("USER"); !set || user != "" {
		t.Fatalf("USER set to empty reads as %q, %v", user, set)
	}
	if cron.encoding != "US-ASCII" {
		t.Fatalf("encoding %q for an environment with no locale", cron.encoding)
	}
	env.Environ = func(k string) string { return map[string]string{"PATH": "/usr/bin", "LANG": "en_US.UTF-8"}[k] }
	if got := Unit.Route(env).Providers[0].(Cron).encoding; got != "UTF-8" {
		t.Fatalf("encoding %q under LANG=en_US.UTF-8", got)
	}
}

// TestRouteConditionsInOrder breaks one condition at a time and then every
// later one too, so each case proves the first failing condition is the
// reason.
func TestRouteConditionsInOrder(t *testing.T) {
	purge := catalog.Resource{Type: "Resources", Title: "cron", Parameters: map[string]any{"purge": true}}
	badShape := routeCron(map[string]any{"command": []any{1}})
	const shapeReason = "Cron[job]: command => 1 is not implemented"
	foreign := routeCron(map[string]any{"provider": "other"})

	// everything breaks: the platform, crontab, provider, tree, purge and shape.
	allBroken := func(t *testing.T) *unit.Env {
		libdir := libdirOf(t, "1.3.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/cron/other.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		env := routeEnv(libdir, foreign, badShape, purge)
		env.Platform = raltest.FakePlatform{Release: "ID=debian\nVERSION_ID=12\n"}
		return env
	}
	cases := []struct {
		name string
		fix  []func(*testing.T, *unit.Env) // applied before the break
		want string
	}{
		{name: "platform", want: "not the measured platform"},
		{name: "crontab", fix: []func(*testing.T, *unit.Env){fixPlatform}, want: "crontab not found on PATH"},
		{name: "provider", fix: []func(*testing.T, *unit.Env){fixPlatform, fixCrontab}, want: "provider other"},
		{name: "tree", fix: []func(*testing.T, *unit.Env){fixPlatform, fixCrontab, fixProvider}, want: "another cron provider in libdir"},
		{name: "purge", fix: []func(*testing.T, *unit.Env){fixPlatform, fixCrontab, fixProvider, fixTree}, want: "resources purge of cron"},
		{name: "shape", fix: []func(*testing.T, *unit.Env){fixPlatform, fixCrontab, fixProvider, fixTree, fixPurge},
			want: shapeReason},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := allBroken(t)
			for _, f := range c.fix {
				f(t, env)
			}
			got := delegatedWhy(t, Unit.Route(env))
			if got != c.want {
				t.Fatalf("reason %q, want %q", got, c.want)
			}
		})
	}
}

func fixPlatform(t *testing.T, env *unit.Env) {
	env.Platform = raltest.FakePlatform{Release: raltest.CentOS10}
}

func fixCrontab(t *testing.T, env *unit.Env) {
	env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Exec: map[string]bool{wantCrontab: true}}
	env.Environ = func(k string) string { return map[string]string{"PATH": "/usr/bin"}[k] }
}

func fixProvider(t *testing.T, env *unit.Env) {
	for i := range env.Catalog.Resources {
		delete(env.Catalog.Resources[i].Parameters, "provider")
	}
}

func fixTree(t *testing.T, env *unit.Env) {
	if err := os.Remove(filepath.Join(env.Libdir, "puppet/provider/cron/other.rb")); err != nil {
		t.Fatal(err)
	}
}

func fixPurge(t *testing.T, env *unit.Env) {
	delete(env.Catalog.Resources[2].Parameters, "purge")
}

func TestRouteCrontabLookup(t *testing.T) {
	cases := []struct {
		name string
		path string
		exec map[string]bool
		want bool
	}{
		{"on PATH", "/usr/bin", map[string]bool{"/usr/bin/crontab": true}, true},
		{"puppet appends /usr/sbin", "/usr/bin", map[string]bool{"/usr/sbin/crontab": true}, true},
		{"puppet appends /sbin", "/usr/bin", map[string]bool{"/sbin/crontab": true}, true},
		{"not executable", "/usr/bin", map[string]bool{}, false},
		{"elsewhere", "/usr/bin", map[string]bool{"/opt/crontab": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := routeEnv(t.TempDir(), routeCron(nil))
			env.Platform = raltest.FakePlatform{Release: raltest.CentOS10, Exec: c.exec}
			env.Environ = func(k string) string { return map[string]string{"PATH": c.path}[k] }
			d := Unit.Route(env)
			if (len(d.Providers) == 1) != c.want {
				t.Fatalf("got %+v", d)
			}
			if !c.want && d.Delegated["cron"] != "crontab not found on PATH" {
				t.Fatalf("reason %q", d.Delegated["cron"])
			}
		})
	}
}

func TestRoutePlatformUnreadable(t *testing.T) {
	env := routeEnv(t.TempDir(), routeCron(nil))
	env.Platform = raltest.FakePlatform{Err: os.ErrNotExist, Exec: map[string]bool{wantCrontab: true}}
	if got := delegatedWhy(t, Unit.Route(env)); got != "not the measured platform" {
		t.Fatalf("reason %q", got)
	}
}

func TestRouteEveryCronChecked(t *testing.T) {
	ok, foreign := routeCron(nil), routeCron(map[string]any{"provider": "other"})
	if got := delegatedWhy(t, Unit.Route(routeEnv(t.TempDir(), ok, foreign))); got != "provider other" {
		t.Fatalf("reason %q", got)
	}
	if d := Unit.Route(routeEnv(t.TempDir(), routeCron(map[string]any{"provider": "crontab"}))); len(d.Providers) != 1 {
		t.Fatalf("provider crontab delegated: %+v", d)
	}
}

func TestRouteTreeConditions(t *testing.T) {
	t.Run("one of the three files only", func(t *testing.T) {
		libdir := libdirOf(t, "1.3.0")
		if err := os.Remove(filepath.Join(libdir, "puppet/provider/cron/filetype.rb")); err != nil {
			t.Fatal(err)
		}
		_, want, _ := unit.MatchFiles(plugintree.LibdirTree(libdir), Manifest, "cron", nil)
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeCron(nil)))); got != want || want == "" {
			t.Fatalf("reason %q, want MatchFiles's %q", got, want)
		}
	})
	t.Run("unmeasured digest", func(t *testing.T) {
		libdir := libdirOf(t, "2.1.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/type/cron.rb"), []byte("# edited\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeCron(nil))))
		if !strings.Contains(got, "puppet/type/cron.rb has sha256") {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("files of two releases", func(t *testing.T) {
		libdir := libdirOf(t, "2.1.0")
		b, err := os.ReadFile(filepath.Join(fixtureLib("1.3.0"), "puppet/type/cron.rb"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, "puppet/type/cron.rb"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := Unit.Route(routeEnv(libdir, routeCron(nil))); len(d.Providers) != 0 {
			t.Fatalf("a mixed tree routed natively: %+v", d)
		}
	})
	t.Run("a second provider beside measured files", func(t *testing.T) {
		libdir := libdirOf(t, "2.1.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/cron/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeCron(nil)))); got != "another cron provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a second provider with no cron_core file", func(t *testing.T) {
		libdir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(libdir, "puppet/provider/cron"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/cron/extra.rb"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := delegatedWhy(t, Unit.Route(routeEnv(libdir, routeCron(nil)))); got != "another cron provider in libdir" {
			t.Fatalf("reason %q", got)
		}
	})
	t.Run("a non-ruby file in the provider directory", func(t *testing.T) {
		libdir := libdirOf(t, "2.1.0")
		if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/cron/README"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if d := Unit.Route(routeEnv(libdir, routeCron(nil))); len(d.Providers) != 1 {
			t.Fatalf("got %+v", d)
		}
	})
}

func TestRoutePurge(t *testing.T) {
	purge := func(typ, title string, v any) catalog.Resource {
		params := map[string]any{}
		if v != nil {
			params["purge"] = v
		}
		return catalog.Resource{Type: typ, Title: title, Parameters: params}
	}
	cases := []struct {
		name string
		res  catalog.Resource
		want bool // native
	}{
		{"true", purge("Resources", "cron", true), false},
		{"string true", purge("Resources", "cron", "true"), false},
		{"string yes", purge("Resources", "cron", "yes"), false},
		{"string TRUE", purge("Resources", "cron", "TRUE"), false},
		{"string Yes", purge("Resources", "cron", "Yes"), false},
		{"other value", purge("Resources", "cron", 1), false},
		{"title Cron", purge("Resources", "Cron", true), false},
		{"type resources", purge("resources", "cron", true), false},
		{"named by name", catalog.Resource{Type: "Resources", Title: "x", Parameters: map[string]any{"name": "cron", "purge": true}}, false},
		{"false", purge("Resources", "cron", false), true},
		{"string false", purge("Resources", "cron", "false"), true},
		{"absent", purge("Resources", "cron", nil), true},
		{"another type", purge("Resources", "user", true), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Unit.Route(routeEnv(t.TempDir(), routeCron(nil), c.res))
			if (len(d.Providers) == 1) != c.want {
				t.Fatalf("got %+v", d)
			}
			if !c.want && d.Delegated["cron"] != "resources purge of cron" {
				t.Fatalf("reason %q", d.Delegated["cron"])
			}
		})
	}
}

func TestRouteValueShapesPerRelease(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		native1 bool // under 1.3.0
		native2 bool // under 2.1.0
	}{
		{"command not a string", map[string]any{"command": true}, false, false},
		{"weekday true", map[string]any{"command": "c", "weekday": true}, false, true},
		{"minute float", map[string]any{"command": "c", "minute": 5.5}, false, true},
		{"environment integer", map[string]any{"command": "c", "environment": []any{5}}, false, true},
		{"plain", map[string]any{"command": "c", "minute": "5", "hour": 3, "environment": "A=b"}, true, true},
		{"ruby's own refusal stays native", map[string]any{"command": "c", "minute": "99"}, true, true},
	}
	for _, c := range cases {
		for release, want := range map[string]bool{"1.3.0": c.native1, "2.1.0": c.native2} {
			t.Run(c.name+"/"+release, func(t *testing.T) {
				d := Unit.Route(routeEnv(libdirOf(t, release), routeCron(c.params)))
				if (len(d.Providers) == 1) != want {
					t.Fatalf("got %+v", d)
				}
			})
		}
	}
}

// TestPreflightKnowsEveryCronAttribute: acceptance 3. Pre-flight refuses an
// attribute the provider's Attrs does not declare, so a Cron setting every
// attribute type/cron.rb has must pass it, and one setting an undeclared
// attribute must not.
func TestPreflightKnowsEveryCronAttribute(t *testing.T) {
	p := Unit.Route(routeEnv(t.TempDir(), routeCron(nil))).Providers[0]
	attrs := p.Attrs().Sorted()
	want := []string{"command", "ensure", "environment", "hour", "minute", "month", "monthday", "name", "provider", "special", "target", "user", "weekday"}
	if !slices.Equal(attrs, want) {
		t.Fatalf("Attrs %v, want spec 4.1's %v", attrs, want)
	}
	params := map[string]any{}
	for _, a := range attrs {
		params[a] = "x"
	}
	reg := ral.NewRegistry(p)
	if gaps := ral.Preflight(reg, &catalog.Catalog{Resources: []catalog.Resource{routeCron(params)}}); len(gaps) != 0 {
		t.Fatalf("pre-flight refused a declared attribute: %v", gaps)
	}
	params["no_such_attribute"] = "x"
	gaps := ral.Preflight(reg, &catalog.Catalog{Resources: []catalog.Resource{routeCron(params)}})
	if len(gaps) != 1 || gaps[0].Attribute != "no_such_attribute" {
		t.Fatalf("pre-flight gaps for an undeclared attribute: %v", gaps)
	}
}
