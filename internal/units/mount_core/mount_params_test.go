package mountcore

import (
	"errors"
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

func mtRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Mount", Title: title, Parameters: params}
}

func str(s string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.Str, S: s} }

var releases = []string{"1.3.0", "2.1.0"}

func TestMountIdentity(t *testing.T) {
	m := Mount{}
	if m.Type() != "mount" || *m.ProviderUsed() != "parsed" {
		t.Errorf("Type/ProviderUsed = %q/%q", m.Type(), *m.ProviderUsed())
	}
	want := []string{"ensure", "device", "blockdevice", "fstype", "options", "pass", "atboot", "dump", "target"}
	if !reflect.DeepEqual(mountProperties, want) {
		t.Errorf("properties = %v, want %v", mountProperties, want)
	}
	for _, a := range append([]string{"name", "provider", "remounts"}, want...) {
		if !hasAttr(m.Attrs(), a) {
			t.Errorf("Attrs lacks %s", a)
		}
	}
	for _, a := range want {
		if !hasAttr(m.Properties(), a) {
			t.Errorf("Properties lacks %s", a)
		}
	}
	if hasAttr(m.Properties(), "remounts") || hasAttr(m.Properties(), "name") {
		t.Error("remounts and name are parameters, not properties")
	}
}

// TestParseMountRefusals: the texts of spec 4.4 (M12 for dump), the first
// failure in allattrs order, the title kept raw in the ref.
func TestParseMountRefusals(t *testing.T) {
	tests := []struct {
		name   string
		title  string
		params map[string]any
		want   string
	}{
		{"ensure invalid", "/m", map[string]any{"ensure": "latest"}, `Parameter ensure failed on Mount[/m]: Invalid value "latest". Valid values are defined, unmounted, absent, mounted.`},
		{"ensure second element", "/m", map[string]any{"ensure": []any{"mounted", "x"}}, `Parameter ensure failed on Mount[/m]: Invalid value "x". Valid values are defined, unmounted, absent, mounted.`},
		{"fstype whitespace", "/m", map[string]any{"fstype": "a b"}, "Parameter fstype failed on Mount[/m]: fstype must not contain whitespace: a b"},
		{"fstype vertical tab", "/m", map[string]any{"fstype": "a\vb"}, "Parameter fstype failed on Mount[/m]: fstype must not contain whitespace: a\vb"},
		{"fstype empty", "/m", map[string]any{"fstype": ""}, "Parameter fstype failed on Mount[/m]: fstype must not be an empty string"},
		{"options whitespace", "/m", map[string]any{"options": "a\tb"}, "Parameter options failed on Mount[/m]: options must not contain whitespace: a\tb"},
		{"options empty", "/m", map[string]any{"options": ""}, "Parameter options failed on Mount[/m]: options must not be an empty string"},
		{"blockdevice whitespace", "/m", map[string]any{"blockdevice": "a b"}, "Parameter blockdevice failed on Mount[/m]: blockdevice must not contain whitespace: a b"},
		{"dump abc", "/mnt/a", map[string]any{"dump": "abc"}, `Parameter dump failed on Mount[/mnt/a]: Invalid value "abc". Valid values match /(0|1)/.`},
		{"dump 2", "/mnt/a", map[string]any{"dump": "2"}, `Parameter dump failed on Mount[/mnt/a]: Invalid value "2". Valid values match /(0|1)/.`},
		{"dump yes", "/mnt/a", map[string]any{"dump": "yes"}, `Parameter dump failed on Mount[/mnt/a]: Invalid value "yes". Valid values match /(0|1)/.`},
		{"dump integer 2", "/mnt/a", map[string]any{"dump": float64(2)}, `Parameter dump failed on Mount[/mnt/a]: Invalid value 2. Valid values match /(0|1)/.`},
		{"dump empty", "/mnt/a", map[string]any{"dump": ""}, `Parameter dump failed on Mount[/mnt/a]: Invalid value "". Valid values match /(0|1)/.`},
		{"atboot bogus", "/m", map[string]any{"atboot": "maybe"}, `Parameter atboot failed on Mount[/m]: Munging failed for value "maybe" in class atboot: expected a boolean value`},
		{"atboot number", "/m", map[string]any{"atboot": float64(1)}, `Parameter atboot failed on Mount[/m]: Munging failed for value 1 in class atboot: expected a boolean value`},
		{"atboot bad list element", "/m", map[string]any{"atboot": []any{"true", "bad"}}, `Parameter atboot failed on Mount[/m]: Munging failed for value "bad" in class atboot: expected a boolean value`},
		{"remounts bogus", "/m", map[string]any{"remounts": "maybe"}, `Parameter remounts failed on Mount[/m]: Invalid value "maybe". Valid values are true, false.`},
		{"remounts yes is not a value", "/m", map[string]any{"remounts": "yes"}, `Parameter remounts failed on Mount[/m]: Invalid value "yes". Valid values are true, false.`},
		{"remounts list refused whole", "/m", map[string]any{"remounts": []any{true}}, `Parameter remounts failed on Mount[/m]: Invalid value [true]. Valid values are true, false.`},
		{"title is raw in the ref", "/mnt/t/", map[string]any{"dump": "abc"}, `Parameter dump failed on Mount[/mnt/t/]: Invalid value "abc". Valid values match /(0|1)/.`},
		{"ensure before fstype", "/m", map[string]any{"ensure": "x", "fstype": ""}, `Parameter ensure failed on Mount[/m]: Invalid value "x". Valid values are defined, unmounted, absent, mounted.`},
		{"fstype before dump", "/m", map[string]any{"dump": "abc", "fstype": "a b"}, "Parameter fstype failed on Mount[/m]: fstype must not contain whitespace: a b"},
		{"dump before remounts", "/m", map[string]any{"dump": "abc", "remounts": "x"}, `Parameter dump failed on Mount[/m]: Invalid value "abc". Valid values match /(0|1)/.`},
	}
	for _, tc := range tests {
		for _, rel := range releases {
			t.Run(tc.name+"/"+rel, func(t *testing.T) {
				r := mtRes(tc.title, tc.params)
				if _, err := parseMount(r, rel, "UTF-8"); err == nil || err.Error() != tc.want {
					t.Fatalf("err = %v, want %q", err, tc.want)
				}
				if err := (Mount{release: rel, encoding: "UTF-8"}).Validate(r); err == nil || err.Error() != tc.want {
					t.Fatalf("Validate err = %v, want %q", err, tc.want)
				}
			})
		}
	}
}

func TestParseMountSensitiveIsRedacted(t *testing.T) {
	r := mtRes("/m", map[string]any{"fstype": "a b", "dump": "abc"})
	r.SensitiveParameters = []string{"fstype"}
	_, err := parseMount(r, "2.1.0", "UTF-8")
	if want := "Parameter fstype failed on Mount[/m]: fstype must not contain whitespace: [redacted]"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	r = mtRes("/m", map[string]any{"dump": "abc"})
	r.SensitiveParameters = []string{"dump"}
	_, err = parseMount(r, "2.1.0", "UTF-8")
	if want := "Parameter dump failed on Mount[/m]: Invalid value [redacted]. Valid values match /(0|1)/."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestParseMountEncodingQuotesValue(t *testing.T) {
	r := mtRes("/m", map[string]any{"dump": "é"})
	_, err := parseMount(r, "2.1.0", "US-ASCII")
	if want := `Parameter dump failed on Mount[/m]: Invalid value "\u00E9". Valid values match /(0|1)/.`; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestParseMountAccepted(t *testing.T) {
	zero := func(attrs ...string) map[string]bool {
		m := map[string]bool{}
		for _, a := range attrs {
			m[a] = true
		}
		return m
	}
	tests := []struct {
		name     string
		title    string
		params   map[string]any
		wantName string
		ensure   string
		should   map[string]parsedfile.Value
		integers map[string]bool
		remounts bool
	}{
		{"unmanaged: only the target default", "/m", nil, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"ensure alone manages: pass and dump default to Integer 0", "/m", map[string]any{"ensure": "mounted"}, "/m", "mounted",
			map[string]parsedfile.Value{"pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"present is defined", "/m", map[string]any{"ensure": "present"}, "/m", "defined",
			map[string]parsedfile.Value{"pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"trailing slash stripped, title kept", "/mnt/t/", map[string]any{"ensure": "absent"}, "/mnt/t", "absent",
			map[string]parsedfile.Value{"pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"root stays", "/", nil, "/", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"many slashes", "/mnt//", nil, "/mnt", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"name parameter munged, title other", "named", map[string]any{"name": "/mnt/u/"}, "/mnt/u", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"name munged per line", "/a/\n/b//", nil, "/a\n/b", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"name with a space is fine on Linux", "/mnt/my disk", nil, "/mnt/my disk", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"name starting with a dash", "-o", nil, "-o", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"dump 10 and pass given", "/m", map[string]any{"dump": "10", "pass": "2"}, "/m", "",
			map[string]parsedfile.Value{"dump": str("10"), "pass": str("2"), "target": str("/etc/fstab")}, zero(), true},
		{"dump x1y", "/m", map[string]any{"dump": "x1y"}, "/m", "",
			map[string]parsedfile.Value{"dump": str("x1y"), "pass": str("0"), "target": str("/etc/fstab")}, zero("pass"), true},
		{"Integer dump and pass", "/m", map[string]any{"dump": float64(1), "pass": float64(2)}, "/m", "",
			map[string]parsedfile.Value{"dump": str("1"), "pass": str("2"), "target": str("/etc/fstab")}, zero("dump", "pass"), true},
		{"pass any value", "/m", map[string]any{"pass": "-"}, "/m", "",
			map[string]parsedfile.Value{"pass": str("-"), "dump": str("0"), "target": str("/etc/fstab")}, zero("dump"), true},
		{"atboot true", "/m", map[string]any{"atboot": true}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("yes"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"atboot false string", "/m", map[string]any{"atboot": "false"}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("no"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"atboot yes string", "/m", map[string]any{"atboot": "yes"}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("yes"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"atboot TRUE downcases", "/m", map[string]any{"atboot": "TRUE"}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("yes"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"atboot Yes downcases", "/m", map[string]any{"atboot": "Yes"}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("yes"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"atboot No downcases", "/m", map[string]any{"atboot": "No"}, "/m", "",
			map[string]parsedfile.Value{"atboot": str("no"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"remounts false", "/m", map[string]any{"remounts": false}, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), false},
		{"remounts string false", "/m", map[string]any{"remounts": "false"}, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), false},
		{"remounts string true", "/m", map[string]any{"remounts": "true"}, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"explicit target does not count as the default", "/m", map[string]any{"target": "/etc/fstab2"}, "/m", "",
			map[string]parsedfile.Value{"pass": str("0"), "dump": str("0"), "target": str("/etc/fstab2")}, zero("pass", "dump"), true},
		{"empty lists manage nothing", "/m", map[string]any{"options": []any{}}, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"a list takes its first element", "/m", map[string]any{"fstype": []any{"ext4", "xfs"}}, "/m", "",
			map[string]parsedfile.Value{"fstype": str("ext4"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
		{"undef is unset", "/m", map[string]any{"device": nil}, "/m", "", map[string]parsedfile.Value{"target": str("/etc/fstab")}, zero(), true},
		{"full", "/m", map[string]any{"ensure": "unmounted", "device": "/dev/x", "blockdevice": "/dev/y", "fstype": "ext4", "options": "-o", "pass": "1", "dump": "0"}, "/m", "unmounted",
			map[string]parsedfile.Value{"device": str("/dev/x"), "blockdevice": str("/dev/y"), "fstype": str("ext4"), "options": str("-o"), "pass": str("1"), "dump": str("0"), "target": str("/etc/fstab")}, zero(), true},
		{"device with a space", "/m", map[string]any{"device": "/dev/my disk"}, "/m", "",
			map[string]parsedfile.Value{"device": str("/dev/my disk"), "pass": str("0"), "dump": str("0"), "target": str("/etc/fstab")}, zero("pass", "dump"), true},
	}
	for _, tc := range tests {
		for _, rel := range releases {
			t.Run(tc.name+"/"+rel, func(t *testing.T) {
				p, err := parseMount(mtRes(tc.title, tc.params), rel, "UTF-8")
				if err != nil {
					t.Fatal(err)
				}
				if p.Name != tc.wantName || p.Title != tc.title || p.Ensure != tc.ensure || p.Remounts != tc.remounts {
					t.Errorf("name/title/ensure/remounts = %q/%q/%q/%v, want %q/%q/%q/%v", p.Name, p.Title, p.Ensure, p.Remounts, tc.wantName, tc.title, tc.ensure, tc.remounts)
				}
				if !reflect.DeepEqual(p.Should, tc.should) {
					t.Errorf("Should = %+v, want %+v", p.Should, tc.should)
				}
				for _, a := range []string{"pass", "dump"} {
					if p.Integers[a] != tc.integers[a] {
						t.Errorf("Integers[%s] = %v, want %v", a, p.Integers[a], tc.integers[a])
					}
				}
			})
		}
	}
}

// TestParseMountUnimplementedValues: a value the native parser does not
// implement is delegated at its place in allattrs order (spec 2.1 condition 6).
func TestParseMountUnimplementedValues(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"name", map[string]any{"name": float64(1)}, "name"},
		{"ensure", map[string]any{"ensure": float64(1)}, "ensure"},
		{"device", map[string]any{"device": float64(1)}, "device"},
		{"blockdevice", map[string]any{"blockdevice": float64(1)}, "blockdevice"},
		{"fstype", map[string]any{"fstype": true}, "fstype"},
		{"options", map[string]any{"options": []any{"a", float64(1)}}, "options"},
		{"target", map[string]any{"target": float64(1)}, "target"},
		{"dump bool", map[string]any{"dump": true}, "dump"},
		{"dump float", map[string]any{"dump": 1.5}, "dump"},
		{"pass hash", map[string]any{"pass": map[string]any{"a": "b"}}, "pass"},
		{"atboot hash", map[string]any{"atboot": map[string]any{"a": "b"}}, "atboot"},
		{"remounts hash", map[string]any{"remounts": map[string]any{"a": "b"}}, "remounts"},
		{"first in allattrs order", map[string]any{"target": float64(1), "fstype": float64(1)}, "fstype"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mtRes("/m", tc.params)
			_, err := parseMount(r, "2.1.0", "UTF-8")
			var u *rubyvalue.UnimplementedValue
			if !errors.As(err, &u) || u.Attr != tc.attr || u.Ref != "Mount[/m]" {
				t.Fatalf("err = %v, want *rubyvalue.UnimplementedValue for %s", err, tc.attr)
			}
			if err := (Mount{}).Validate(r); !errors.As(err, &u) {
				t.Errorf("Validate err = %v, want *rubyvalue.UnimplementedValue", err)
			}
		})
	}
}

func TestParseMountIntegerDumpAndPassAreImplemented(t *testing.T) {
	for _, v := range []any{float64(0), float64(1), int(1), int64(1)} {
		if _, err := parseMount(mtRes("/m", map[string]any{"dump": v, "pass": v}), "2.1.0", "UTF-8"); err != nil {
			t.Errorf("dump/pass %v: %v", v, err)
		}
	}
}

func hasAttr(a ral.Attrs, name string) bool {
	_, ok := a[name]
	return ok
}
