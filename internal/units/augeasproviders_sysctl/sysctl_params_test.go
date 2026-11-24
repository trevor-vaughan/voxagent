package augeasproviderssysctl

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// augValidator is what the augeasproviders types share.
type augValidator interface {
	Validate(catalog.Resource) error
	Attrs() ral.Attrs
	Type() string
	ProviderUsed() *string
	AutoRequires(catalog.Resource, func(string) bool) []string
}

var augTypes = map[string]augValidator{
	"Sysctl": Sysctl{},
}

func TestAugeasValidation(t *testing.T) {
	cases := []struct {
		name   string
		typ    string
		title  string
		params map[string]any
		marked []string
		want   string // "" = accepted
	}{
		{"sysctl ensure", "Sysctl", "vm.swappiness", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "bogus". Valid values are present, absent.`},
		{"ensure number", "Sysctl", "vm.swappiness", map[string]any{"ensure": float64(1)}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value 1. Valid values are present, absent.`},
		{"ensure case", "Sysctl", "vm.swappiness", map[string]any{"ensure": "Present"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "Present". Valid values are present, absent.`},
		{"apply yes", "Sysctl", "vm.swappiness", map[string]any{"apply": "yes"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "yes". Valid values are true, false.`},
		{"apply True", "Sysctl", "vm.swappiness", map[string]any{"apply": "True"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "True". Valid values are true, false.`},
		{"apply 1", "Sysctl", "vm.swappiness", map[string]any{"apply": float64(1)}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value 1. Valid values are true, false.`},
		{"apply array", "Sysctl", "vm.swappiness", map[string]any{"apply": []any{"x", true}}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value ["x", true]. Valid values are true, false.`},
		{"persist", "Sysctl", "vm.swappiness", map[string]any{"persist": "maybe"}, nil, `Parameter persist failed on Sysctl[vm.swappiness]: Invalid value "maybe". Valid values are true, false.`},
		{"silent", "Sysctl", "vm.swappiness", map[string]any{"silent": float64(0)}, nil, `Parameter silent failed on Sysctl[vm.swappiness]: Invalid value 0. Valid values are true, false.`},
		{"apply string true", "Sysctl", "vm.swappiness", map[string]any{"apply": "false", "silent": "true"}, nil, ""},
		{"first failure ensure", "Sysctl", "vm.swappiness", map[string]any{"ensure": "bogus", "apply": "yes"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "bogus". Valid values are present, absent.`},
		{"first failure sysctl apply before silent", "Sysctl", "vm.swappiness", map[string]any{"silent": "x", "apply": "y"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "y". Valid values are true, false.`},
		{"sysctl plain", "Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"}, nil, ""},
		{"sysctl integer", "Sysctl", "vm.swappiness", map[string]any{"value": float64(10), "ensure": "present"}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := augTypes[c.typ].Validate(raltest.AugRes(c.typ, c.title, c.params, c.marked...))
			if c.want == "" {
				if err != nil && c.marked == nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v\nwant %s", err, c.want)
			}
			var uv *rubyvalue.UnimplementedValue
			if errors.As(err, &uv) {
				t.Fatalf("a Ruby refusal must not be an rubyvalue.UnimplementedValue: %v", err)
			}
		})
	}
}

// TestAugeasDelegates: what Ruby accepts and native does not implement is an
// rubyvalue.UnimplementedValue, so the router sends the type to the host (spec §2 d-f).
func TestAugeasDelegates(t *testing.T) {
	big := float64(1 << 53)
	cases := []struct {
		name   string
		typ    string
		title  string
		params map[string]any
		marked []string
	}{
		{"sysctl val", "Sysctl", "vm.swappiness", map[string]any{"val": "1"}, nil},
		{"sysctl comment", "Sysctl", "vm.swappiness", map[string]any{"comment": "c"}, nil},
		{"sysctl persist false", "Sysctl", "vm.swappiness", map[string]any{"persist": false}, nil},
		{"sysctl persist string false", "Sysctl", "vm.swappiness", map[string]any{"persist": "false"}, nil},
		{"sysctl ensure absent", "Sysctl", "vm.swappiness", map[string]any{"ensure": "absent"}, nil},
		{"sysctl ensure array", "Sysctl", "vm.swappiness", map[string]any{"ensure": []any{"present"}}, nil},
		{"sysctl value big", "Sysctl", "vm.swappiness", map[string]any{"value": big}, nil},
		{"sysctl value float", "Sysctl", "vm.swappiness", map[string]any{"value": 1.5}, nil},
		{"sysctl value bool", "Sysctl", "vm.swappiness", map[string]any{"value": true}, nil},
		{"sysctl value array", "Sysctl", "vm.swappiness", map[string]any{"value": []any{"1"}}, nil},
		{"sysctl value control", "Sysctl", "vm.swappiness", map[string]any{"value": "1\n2"}, nil},
		{"sysctl title digit", "Sysctl", "net.1x", map[string]any{"value": "1"}, nil},
		{"sysctl title empty segment", "Sysctl", "net..x", nil, nil},
		{"sysctl target relative", "Sysctl", "vm.swappiness", map[string]any{"target": "etc/sysctl.conf"}, nil},
		{"sysctl target dotdot", "Sysctl", "vm.swappiness", map[string]any{"target": "/etc/../sysctl.conf"}, nil},
		{"sysctl target trailing slash", "Sysctl", "vm.swappiness", map[string]any{"target": "/etc/"}, nil},
		{"sysctl target charset", "Sysctl", "vm.swappiness", map[string]any{"target": "/etc/a b"}, nil},
		{"sysctl target number", "Sysctl", "vm.swappiness", map[string]any{"target": float64(1)}, nil},
		{"provider other", "Sysctl", "vm.swappiness", map[string]any{"provider": "parsed"}, nil},
		{"name differs", "Sysctl", "vm.swappiness", map[string]any{"name": "vm.other"}, nil},
		{"sensitive value", "Sysctl", "vm.swappiness", map[string]any{"value": "1"}, []string{"value"}},
		{"hash", "Sysctl", "vm.swappiness", map[string]any{"value": map[string]any{"a": "b"}}, nil},
		{"apply marked", "Sysctl", "vm.swappiness", map[string]any{"apply": "yes"}, []string{"apply"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := augTypes[c.typ].Validate(raltest.AugRes(c.typ, c.title, c.params, c.marked...))
			var uv *rubyvalue.UnimplementedValue
			if !errors.As(err, &uv) {
				t.Fatalf("want rubyvalue.UnimplementedValue, got %v", err)
			}
			if len(c.marked) > 0 && strings.Contains(err.Error(), `"1"`) {
				t.Errorf("marked value leaks: %v", err)
			}
		})
	}
}

func TestAugeasParse(t *testing.T) {
	got, err := parseSysctl(raltest.AugRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": float64(1), "target": "/etc/sysctl.d/a.conf", "apply": "false", "silent": true, "ensure": "present"}))
	want := sysctlParams{Name: "net.ipv4.ip_forward", Target: "/etc/sysctl.d/a.conf", Value: "1", HasValue: true, Apply: false, Silent: true, EnsureSet: true}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
	}
	got, err = parseSysctl(raltest.AugRes("Sysctl", "vm.swappiness", nil))
	if want := (sysctlParams{Name: "vm.swappiness", Apply: true}); err != nil || got != want {
		t.Fatalf("defaults: got %+v, %v\nwant %+v", got, err, want)
	}
	got, err = parseSysctl(raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": ""}))
	if err != nil || !got.HasValue || got.Value != "" {
		t.Fatalf("empty value: %+v, %v", got, err)
	}
}

func TestAugeasAttrs(t *testing.T) {
	want := map[string][]string{
		"Sysctl": {"name", "provider", "ensure", "val", "value", "comment", "target", "apply", "persist", "silent"},
	}
	for typ, names := range want {
		if got, w := augTypes[typ].Attrs(), ral.NewAttrs(names...); !reflect.DeepEqual(got, w) {
			t.Errorf("%s Attrs = %v, want %v", typ, got.Sorted(), w.Sorted())
		}
		if got := augTypes[typ].Type(); got != strings.ToLower(typ) {
			t.Errorf("%s Type = %q", typ, got)
		}
		if p := augTypes[typ].ProviderUsed(); p == nil || *p != "augeas" {
			t.Errorf("%s ProviderUsed = %v", typ, p)
		}
	}
}

func TestAugeasAutoRequires(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	for typ, v := range augTypes {
		title := "vm.swappiness"
		r := raltest.AugRes(typ, title, map[string]any{"target": "/etc/x.conf"})
		if got := v.AutoRequires(r, all); !reflect.DeepEqual(got, []string{"File[/etc/x.conf]"}) {
			t.Errorf("%s with target: %v", typ, got)
		}
		if got := v.AutoRequires(r, none); len(got) != 0 {
			t.Errorf("%s unmanaged is no edge: %v", typ, got)
		}
		if got := v.AutoRequires(raltest.AugRes(typ, title, nil), all); len(got) != 0 {
			t.Errorf("%s without target: %v", typ, got)
		}
		if got := v.AutoRequires(raltest.AugRes(typ, title, map[string]any{"target": "rel", "ensure": "bogus"}), all); len(got) != 0 {
			t.Errorf("%s invalid: %v", typ, got)
		}
	}
}

// TestAugeasValidateNeverPanics gives every attribute every JSON shape:
// Validate and AutoRequires answer and never panic.
func TestAugeasValidateNeverPanics(t *testing.T) {
	shapes := map[string]any{
		"array": []any{true}, "nested": []any{[]any{"a"}, map[string]any{"a": "b"}}, "hash": map[string]any{"a": float64(1)},
		"empty array": []any{}, "string": "x", "empty": "", "number": float64(3), "float": 1.5, "huge": 1e300, "nan": math.NaN(),
		"true": true, "false": false, "int": 7,
	}
	for typ, v := range augTypes {
		for attr := range v.Attrs() {
			for shape, val := range shapes {
				for _, marked := range []bool{false, true} {
					t.Run(typ+"/"+attr+"/"+shape, func(t *testing.T) {
						var m []string
						if marked {
							m = []string{attr}
						}
						r := raltest.AugRes(typ, "Port", map[string]any{attr: val}, m...)
						_ = v.Validate(r)
						_ = v.AutoRequires(r, func(string) bool { return true })
					})
				}
			}
		}
	}
}
