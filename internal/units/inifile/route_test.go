package inifile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/unit"
)

// libdirOf copies a release's testdata lib into a fresh libdir the test may
// change.
func libdirOf(t *testing.T, release string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", release, "lib"))); err != nil {
		t.Fatal(err)
	}
	return dir
}

func routeEnv(libdir string, rs ...catalog.Resource) *unit.Env {
	return &unit.Env{
		Catalog:  &catalog.Catalog{Resources: rs},
		Libdir:   libdir,
		Tree:     plugintree.LibdirTree(libdir),
		Platform: raltest.FakePlatform{Release: raltest.CentOS10},
		Environ:  func(string) string { return "" },
	}
}

// routeRes is a resource of typ (lower case, as providers name it) that is
// routable natively once params are added.
func routeRes(typ string, params map[string]any) catalog.Resource {
	capital := strings.ToUpper(typ[:1]) + typ[1:]
	p := map[string]any{"path": "/etc/x.ini", "section": "s", "setting": "k", "subsetting": "a"}
	for k, v := range params {
		p[k] = v
	}
	return catalog.Resource{Type: capital, Title: "t-" + typ, Parameters: p}
}

// routeTypes is each type with the release that first lists it.
var routeTypes = []struct{ typ, from string }{{"ini_setting", "6.5.0"}, {"ini_subsetting", "6.5.0"}, {"ini_section", "6.6.0"}}

// delegation is the reason d gives for typ, failing the test when typ was
// routed natively.
func delegation(t *testing.T, d unit.Decision, typ string) string {
	t.Helper()
	why, ok := d.Delegated[typ]
	if !ok {
		t.Fatalf("%s routed natively, want delegated: %+v", typ, d)
	}
	return why
}

func TestRouteNoInifile(t *testing.T) {
	env := routeEnv(t.TempDir(), catalog.Resource{Type: "File", Title: "/x"})
	if d := Unit.Route(env); len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestRouteNative(t *testing.T) {
	for _, release := range []string{"6.5.0", "6.6.0", "6.6.1"} {
		for _, c := range routeTypes {
			if semverLess(release, c.from) {
				continue
			}
			t.Run(release+"/"+c.typ, func(t *testing.T) {
				d := Unit.Route(routeEnv(libdirOf(t, release), routeRes(c.typ, nil), routeRes(c.typ, map[string]any{"section": "u"})))
				if len(d.Providers) != 1 || len(d.Delegated) != 0 || d.Providers[0].Type() != c.typ {
					t.Fatalf("got %+v", d)
				}
			})
		}
	}
}

func TestRouteSection65(t *testing.T) {
	d := Unit.Route(routeEnv(libdirOf(t, "6.5.0"), routeRes("ini_section", nil)))
	// Spec 2 words this "no measured release lists it", but 6.6.x list the type, so
	// unit.MatchFiles reports the file the 6.5.0 tree lacks.
	if got := delegation(t, d, "ini_section"); !strings.Contains(got, "puppet/type/ini_section.rb") {
		t.Errorf("reason %q", got)
	}
}

// Condition 1: a tree no release matches, whether a file differs or is gone.
func TestRouteReleaseMismatch(t *testing.T) {
	for _, c := range routeTypes {
		t.Run(c.typ, func(t *testing.T) {
			libdir := libdirOf(t, "6.6.1")
			path := filepath.Join(libdir, "puppet/provider", c.typ, "ruby.rb")
			if err := os.WriteFile(path, []byte("# patched\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			got := delegation(t, Unit.Route(routeEnv(libdir, routeRes(c.typ, nil))), c.typ)
			if !strings.Contains(got, "puppet/provider/"+c.typ+"/ruby.rb has sha256") {
				t.Errorf("reason %q", got)
			}
		})
	}
	t.Run("empty libdir", func(t *testing.T) {
		got := delegation(t, Unit.Route(routeEnv(t.TempDir(), routeRes("ini_setting", nil))), "ini_setting")
		if !strings.Contains(got, "puppet/type/ini_setting.rb") {
			t.Errorf("reason %q", got)
		}
	})
}

// Condition 2: another provider .rb in the type's directory.
func TestRouteSecondProvider(t *testing.T) {
	for _, c := range routeTypes {
		t.Run(c.typ, func(t *testing.T) {
			libdir := libdirOf(t, "6.6.1")
			if err := os.WriteFile(filepath.Join(libdir, "puppet/provider", c.typ, "other.rb"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			got := delegation(t, Unit.Route(routeEnv(libdir, routeRes(c.typ, nil))), c.typ)
			if got != "another "+c.typ+" provider in libdir" {
				t.Errorf("reason %q", got)
			}
		})
	}
}

// Condition 3: a value shape the parser does not implement, per type.
func TestRouteUnimplementedValue(t *testing.T) {
	bad := map[string]map[string]any{
		"ini_setting":    {"value": map[string]any{"a": 1}},
		"ini_subsetting": {"value": []any{"x"}},
		"ini_section":    {"section": 1},
	}
	for _, c := range routeTypes {
		t.Run(c.typ, func(t *testing.T) {
			got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes(c.typ, bad[c.typ]))), c.typ)
			if !strings.Contains(got, "is not implemented") {
				t.Errorf("reason %q", got)
			}
		})
	}
}

// Condition 4: a line break in each attribute a regexp or the header sees.
func TestRouteLineBreak(t *testing.T) {
	attrs := map[string][]string{
		"ini_setting":    {"section", "setting", "value", "key_val_separator", "section_prefix", "section_suffix"},
		"ini_subsetting": {"section", "setting", "subsetting", "value", "key_val_separator", "subsetting_separator", "subsetting_key_val_separator", "insert_value"},
		"ini_section":    {"section", "section_prefix", "section_suffix"},
	}
	for _, c := range routeTypes {
		for _, attr := range attrs[c.typ] {
			t.Run(c.typ+"/"+attr, func(t *testing.T) {
				got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes(c.typ, map[string]any{attr: "a\nb"}))), c.typ)
				if !strings.Contains(got, attr+" has a line break") || strings.Contains(got, "a\nb") {
					t.Errorf("reason %q", got)
				}
			})
		}
	}
	t.Run("any element of value", func(t *testing.T) {
		r := routeRes("ini_setting", map[string]any{"value": []any{"fine", "a\nb"}})
		if got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)), "ini_setting"); !strings.Contains(got, "value has a line break") {
			t.Errorf("reason %q", got)
		}
	})
	t.Run("section defaulted from the title", func(t *testing.T) {
		r := catalog.Resource{Type: "Ini_section", Title: "a\nb", Parameters: map[string]any{"path": "/etc/x.ini"}}
		if got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)), "ini_section"); !strings.Contains(got, "section has a line break") {
			t.Errorf("reason %q", got)
		}
	})
}

// Ruling R6: a key_val_separator with regexp syntax beyond "." goes to the host.
func TestRouteKeyValSeparatorSyntax(t *testing.T) {
	for _, c := range routeTypes[:2] {
		for _, sep := range []string{"+", "*", "?", "(", ")", "|", "[", "]", "{", "}", "^", "$", `\`, "-", "&", " = (", "x|y"} {
			t.Run(c.typ+"/"+sep, func(t *testing.T) {
				got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes(c.typ, map[string]any{"key_val_separator": sep}))), c.typ)
				if !strings.Contains(got, "key_val_separator has regexp syntax") {
					t.Errorf("reason %q", got)
				}
			})
		}
		// "." is pinned by the corpus; the rest are literals in both engines.
		for _, sep := range []string{" = ", "=", ": ", ".", " : ", "=>", "", "  "} {
			t.Run(c.typ+"/native "+sep, func(t *testing.T) {
				d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes(c.typ, map[string]any{"key_val_separator": sep})))
				if len(d.Delegated) != 0 {
					t.Fatalf("separator %q delegated: %+v", sep, d.Delegated)
				}
			})
		}
	}
	t.Run("whitespace-only is a plain space", func(t *testing.T) {
		d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes("ini_setting", map[string]any{"key_val_separator": " \t "})))
		if len(d.Delegated) != 0 {
			t.Fatalf("%+v", d.Delegated)
		}
	})
	t.Run("stripped before the test", func(t *testing.T) {
		d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes("ini_setting", map[string]any{"key_val_separator": " = "})))
		if len(d.Delegated) != 0 {
			t.Fatalf("%+v", d.Delegated)
		}
	})
	t.Run("ini_section ignores it", func(t *testing.T) {
		d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), routeRes("ini_section", map[string]any{"key_val_separator": "("})))
		if len(d.Delegated) != 0 {
			t.Fatalf("%+v", d.Delegated)
		}
	})
}

// Ruling R8: input the SettingValue port refuses is routed to the host, by the
// same predicates the port applies.
func TestRouteSettingValueRefusals(t *testing.T) {
	for _, sep := range []string{"a", "1", "_", " [", " \\", " \xff"} {
		t.Run("separator "+sep, func(t *testing.T) {
			r := routeRes("ini_subsetting", map[string]any{"subsetting_separator": sep})
			got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)), "ini_subsetting")
			if !strings.Contains(got, "subsetting_separator") {
				t.Errorf("reason %q", got)
			}
		})
	}
	for _, sep := range []string{" ", ",", ", ", ";", ":", "\t", ".", "é", "|", " a", `\`, ""} {
		t.Run("native separator "+sep, func(t *testing.T) {
			r := routeRes("ini_subsetting", map[string]any{"subsetting_separator": sep})
			if d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)); len(d.Delegated) != 0 {
				t.Fatalf("%+v", d.Delegated)
			}
		})
	}
	t.Run("insert_value index beyond 64 bits", func(t *testing.T) {
		r := routeRes("ini_subsetting", map[string]any{"insert_type": "index", "insert_value": "-99999999999999999999"})
		got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)), "ini_subsetting")
		if !strings.Contains(got, "insert_value") {
			t.Errorf("reason %q", got)
		}
	})
	t.Run("insert_value is no index unless insert_type is index", func(t *testing.T) {
		r := routeRes("ini_subsetting", map[string]any{"insert_type": "after", "insert_value": "-99999999999999999999"})
		if d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)); len(d.Delegated) != 0 {
			t.Fatalf("%+v", d.Delegated)
		}
	})
	t.Run("large positive index is clamped natively", func(t *testing.T) {
		r := routeRes("ini_subsetting", map[string]any{"insert_type": "index", "insert_value": "99999999999999999999"})
		if d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)); len(d.Delegated) != 0 {
			t.Fatalf("%+v", d.Delegated)
		}
	})
}

// Ruby's own refusals stay native: Apply reports them as Ruby does.
func TestRouteRubyRefusalStaysNative(t *testing.T) {
	r := routeRes("ini_subsetting", map[string]any{"quote_char": "x"})
	d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r))
	if len(d.Providers) != 1 || len(d.Delegated) != 0 {
		t.Fatalf("got %+v", d)
	}
}

// Each type is routed on its own.
func TestRouteTypesIndependent(t *testing.T) {
	d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"),
		routeRes("ini_setting", nil),
		routeRes("ini_subsetting", map[string]any{"subsetting": "a\nb"}),
		routeRes("ini_section", nil)))
	if len(d.Providers) != 2 || len(d.Delegated) != 1 {
		t.Fatalf("got %+v", d)
	}
	delegation(t, d, "ini_subsetting")
}

// Conditions are tried in spec order: with every one broken, each fix reveals
// the next reason.
func TestRouteConditionsInOrder(t *testing.T) {
	libdir := libdirOf(t, "6.6.1")
	if err := os.WriteFile(filepath.Join(libdir, "puppet/provider/ini_subsetting/other.rb"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := routeRes("ini_subsetting", map[string]any{
		"value": []any{"x"}, "section": "a\nb", "key_val_separator": "(", "subsetting_separator": "a",
	})
	env := routeEnv(libdir, r)
	if err := os.WriteFile(filepath.Join(libdir, "puppet/type/ini_subsetting.rb"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		want string
		fix  func()
	}{
		{"has sha256", func() {
			b, _ := os.ReadFile(filepath.Join("testdata", "6.6.1", "lib", "puppet/type/ini_subsetting.rb"))
			_ = os.WriteFile(filepath.Join(libdir, "puppet/type/ini_subsetting.rb"), b, 0o644)
		}},
		{"another ini_subsetting provider", func() { _ = os.Remove(filepath.Join(libdir, "puppet/provider/ini_subsetting/other.rb")) }},
		{"is not implemented", func() { delete(r.Parameters, "value") }},
		{"section has a line break", func() { r.Parameters["section"] = "s" }},
		{"key_val_separator has regexp syntax", func() { delete(r.Parameters, "key_val_separator") }},
		{"subsetting_separator", func() { delete(r.Parameters, "subsetting_separator") }},
	}
	for _, s := range steps {
		if got := delegation(t, Unit.Route(env), "ini_subsetting"); !strings.Contains(got, s.want) {
			t.Fatalf("reason %q, want it to contain %q", got, s.want)
		}
		s.fix()
	}
	if d := Unit.Route(env); len(d.Delegated) != 0 || len(d.Providers) != 1 {
		t.Fatalf("after every fix: %+v", d)
	}
}

// semverLess compares the dotted releases of routeTypes numerically.
func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range as {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			return x < y
		}
	}
	return false
}

// Ruling R11: a provider other than ruby goes to the host, with the sibling
// units' wording; ruby stays native; a Sensitive name is redacted.
func TestRouteForeignProvider(t *testing.T) {
	for _, c := range routeTypes {
		t.Run(c.typ, func(t *testing.T) {
			env := func(r catalog.Resource) *unit.Env { return routeEnv(libdirOf(t, "6.6.1"), r) }
			if got := delegation(t, Unit.Route(env(routeRes(c.typ, map[string]any{"provider": "posix"}))), c.typ); got != "provider posix" {
				t.Errorf("reason %q", got)
			}
			if d := Unit.Route(env(routeRes(c.typ, map[string]any{"provider": "ruby"}))); len(d.Delegated) != 0 || len(d.Providers) != 1 {
				t.Errorf("provider ruby: %+v", d)
			}
			r := routeRes(c.typ, map[string]any{"provider": "D8SECRET-provider"})
			r.SensitiveParameters = []string{"provider"}
			got := delegation(t, Unit.Route(env(r)), c.typ)
			if strings.Contains(got, "D8SECRET") || !strings.Contains(got, "redacted") {
				t.Errorf("reason %q", got)
			}
		})
	}
}

// Ruling R15: Ruby's behaviour for `ensure => []` (no ensure value, other
// properties still evaluated) and for an unset ini_subsetting setting or
// subsetting (nil, not "") is unmeasured, so the resource goes to the host.
func TestRouteUnmeasuredShapes(t *testing.T) {
	cases := []struct {
		name, typ, want string
		params          map[string]any
	}{
		{"ini_setting empty ensure", "ini_setting", "ensure", map[string]any{"ensure": []any{}}},
		{"ini_subsetting empty ensure", "ini_subsetting", "ensure", map[string]any{"ensure": []any{}}},
		{"ini_section empty ensure", "ini_section", "ensure", map[string]any{"ensure": []any{}}},
		{"ini_subsetting no setting", "ini_subsetting", "setting", map[string]any{"setting": nil}},
		{"ini_subsetting no subsetting", "ini_subsetting", "subsetting", map[string]any{"subsetting": nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := routeRes(c.typ, c.params)
			got := delegation(t, Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)), c.typ)
			if !strings.Contains(got, c.want) {
				t.Errorf("reason %q lacks %q", got, c.want)
			}
		})
	}
	for _, typ := range []string{"ini_setting", "ini_subsetting", "ini_section"} {
		for _, ensure := range []any{"present", "absent", []any{"absent", "present"}} {
			t.Run(typ+" stays native", func(t *testing.T) {
				r := routeRes(typ, map[string]any{"ensure": ensure})
				if d := Unit.Route(routeEnv(libdirOf(t, "6.6.1"), r)); len(d.Delegated) != 0 {
					t.Fatalf("%+v", d.Delegated)
				}
			})
		}
	}
}
