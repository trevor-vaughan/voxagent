package augeasprovidersssh

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

// augValidator is what the three types share.
type augValidator interface {
	Validate(catalog.Resource) error
	Attrs() ral.Attrs
	Type() string
	ProviderUsed() *string
	AutoRequires(catalog.Resource, func(string) bool) []string
}

var augTypes = map[string]augValidator{
	"Ssh_config":            SshConfig{},
	"Sshd_config":           SshdConfig{},
	"Sshd_config_subsystem": SshdConfigSubsystem{},
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
		{"ssh_config ensure", "Ssh_config", "x", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Ssh_config[x]: Invalid value "bogus". Valid values are present, absent.`},
		{"sshd_config ensure", "Sshd_config", "x", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sshd_config[x]: Invalid value "bogus". Valid values are present, absent.`},
		{"subsystem ensure", "Sshd_config_subsystem", "sftp", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sshd_config_subsystem[sftp]: Invalid value "bogus". Valid values are present, absent.`},
		{"target relative", "Ssh_config", "x", map[string]any{"target": "etc/ssh/ssh_config"}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target windows", "Ssh_config", "x", map[string]any{"target": "C:/x"}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target empty", "Ssh_config", "x", map[string]any{"target": ""}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target absolute", "Ssh_config", "x", map[string]any{"target": "/etc/ssh/ssh_config"}, nil, ""},
		{"array_append", "Sshd_config", "AllowUsers", map[string]any{"array_append": "maybe"}, nil, `Parameter array_append failed on Sshd_config[AllowUsers]: Invalid value "maybe". Valid values are false, true.`},
		{"array_append true", "Sshd_config", "AllowUsers", map[string]any{"array_append": "true", "value": []any{"a"}}, nil, ""},
		{"ssh_config plain", "Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}, "host": "*"}, nil, ""},
		{"sshd_config condition", "Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"}, nil, ""},
		{"subsystem plain", "Sshd_config_subsystem", "sftp", map[string]any{"command": "/usr/libexec/sftp-server"}, nil, ""},
		{"nothing managed", "Sshd_config", "AllowUsers", nil, nil, ""},
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
	cases := []struct {
		name   string
		typ    string
		title  string
		params map[string]any
		marked []string
	}{
		{"ssh value integer", "Ssh_config", "x", map[string]any{"value": float64(1)}, nil},
		{"ssh value empty array", "Ssh_config", "x", map[string]any{"value": []any{}}, nil},
		{"ssh value array of number", "Ssh_config", "x", map[string]any{"value": []any{float64(1)}}, nil},
		{"ssh present without value", "Ssh_config", "x", map[string]any{"ensure": "present"}, nil},
		{"ssh comment", "Ssh_config", "x", map[string]any{"comment": "c"}, nil},
		{"ssh key charset", "Ssh_config", "my-key", nil, nil},
		{"ssh key param charset", "Ssh_config", "x", map[string]any{"key": "a b"}, nil},
		{"ssh host quote", "Ssh_config", "x", map[string]any{"host": "a'b"}, nil},
		{"ssh host backslash", "Ssh_config", "x", map[string]any{"host": `a\b`}, nil},
		{"ssh host number", "Ssh_config", "x", map[string]any{"host": float64(1)}, nil},
		{"ssh target charset", "Ssh_config", "x", map[string]any{"target": "/etc/ssh/a b"}, nil},
		{"sshd value array of bool", "Sshd_config", "x", map[string]any{"value": []any{true}}, nil},
		{"sshd present without value", "Sshd_config", "x", map[string]any{"ensure": "present"}, nil},
		{"sshd condition hash", "Sshd_config", "x", map[string]any{"value": "a", "condition": map[string]any{"Host": "a"}}, nil},
		{"sshd condition odd", "Sshd_config", "x", map[string]any{"value": "a", "condition": "Host a User"}, nil},
		{"sshd condition empty", "Sshd_config", "x", map[string]any{"value": "a", "condition": " "}, nil},
		{"sshd condition key charset", "Sshd_config", "x", map[string]any{"value": "a", "condition": "Ho-st a"}, nil},
		{"sshd condition value slash", "Sshd_config", "x", map[string]any{"value": "a", "condition": "Host a/b"}, nil},
		{"sshd condition value quote", "Sshd_config", "x", map[string]any{"value": "a", "condition": `Host a"b`}, nil},
		{"sshd condition number", "Sshd_config", "x", map[string]any{"value": "a", "condition": float64(1)}, nil},
		{"sshd array_append hash", "Sshd_config", "x", map[string]any{"array_append": map[string]any{}}, nil},
		{"subsystem title", "Sshd_config_subsystem", "1sftp", nil, nil},
		{"subsystem command number", "Sshd_config_subsystem", "sftp", map[string]any{"command": float64(1)}, nil},
		{"subsystem command control", "Sshd_config_subsystem", "sftp", map[string]any{"command": "a\x00b"}, nil},
		{"subsystem comment", "Sshd_config_subsystem", "sftp", map[string]any{"comment": "c"}, nil},
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
	t.Run("ssh_config", func(t *testing.T) {
		got, err := parseSshConfig(raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}, "key": "MACs", "ensure": "present", "target": "/etc/ssh/x"}))
		want := sshConfigParams{Name: "Ciphers", Key: "MACs", Host: "*", Target: "/etc/ssh/x", Value: []string{"a", "b"}, Ensure: "present"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSshConfig(raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "a", "host": "web*"}))
		want = sshConfigParams{Name: "Ciphers", Key: "Ciphers", Host: "web*", Value: []string{"a"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
	})
	t.Run("sshd_config", func(t *testing.T) {
		got, err := parseSshdConfig(raltest.AugRes("Sshd_config", "Port", map[string]any{"value": []any{float64(22), "2222"}, "array_append": true, "condition": "User a Host b"}))
		want := sshdConfigParams{Name: "Port", Key: "Port", Value: []string{"22", "2222"}, ArrayAppend: true, Condition: []condPair{{"User", "a"}, {"Host", "b"}}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSshdConfig(raltest.AugRes("Sshd_config", "x", map[string]any{"key": "PermitRootLogin", "value": float64(3), "ensure": "absent"}))
		want = sshdConfigParams{Name: "x", Key: "PermitRootLogin", Value: []string{"3"}, Ensure: "absent"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
	})
	t.Run("subsystem", func(t *testing.T) {
		got, err := parseSubsystem(raltest.AugRes("Sshd_config_subsystem", "sftp", map[string]any{"command": "/x", "ensure": "present", "target": "/etc/ssh/sshd_config"}))
		want := subsystemParams{Name: "sftp", Target: "/etc/ssh/sshd_config", Command: "/x", HasCommand: true, Ensure: "present"}
		if err != nil || got != want {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSubsystem(raltest.AugRes("Sshd_config_subsystem", "sftp", map[string]any{"ensure": "absent"}))
		if err != nil || got.HasCommand || got.Ensure != "absent" {
			t.Fatalf("absent: %+v, %v", got, err)
		}
	})
}

func TestAugeasAttrs(t *testing.T) {
	want := map[string][]string{
		"Ssh_config":            {"name", "provider", "ensure", "value", "comment", "key", "target", "host"},
		"Sshd_config":           {"name", "provider", "ensure", "value", "comment", "key", "array_append", "target", "condition"},
		"Sshd_config_subsystem": {"name", "provider", "ensure", "command", "comment", "target"},
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
		title := map[string]string{"Sshd_config_subsystem": "sftp"}[typ]
		if title == "" {
			title = "Port"
		}
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
	r := raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root", "target": "/etc/ssh/sshd_config"})
	want := []string{
		"File[/etc/ssh/sshd_config]",
		"Sshd_config_match[Host example.net User root]",
		"Sshd_config_match[Host example.net User root in /etc/ssh/sshd_config]",
		"Sshd_config_match[User root Host example.net]",
		"Sshd_config_match[User root Host example.net in /etc/ssh/sshd_config]",
	}
	// The catalog's names in another order than the edges'.
	sshd := SshdConfig{cat: matchCatalog(
		"User root Host example.net in /etc/ssh/sshd_config", "User root Host example.net",
		"Host example.net User root in /etc/ssh/sshd_config", "Host example.net User root",
	)}
	if got := sshd.AutoRequires(r, all); !reflect.DeepEqual(got, want) {
		t.Errorf("condition with target:\n got %q\nwant %q", got, want)
	}
	r = raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"})
	want = []string{
		"Sshd_config_match[Host example.net User root]",
		"Sshd_config_match[Host example.net User root in ]",
		"Sshd_config_match[User root Host example.net]",
		"Sshd_config_match[User root Host example.net in ]",
	}
	sshd = SshdConfig{cat: matchCatalog(
		"User root Host example.net in ", "Host example.net User root in ",
		"User root Host example.net", "Host example.net User root",
	)}
	if got := sshd.AutoRequires(r, all); !reflect.DeepEqual(got, want) {
		t.Errorf("condition without target:\n got %q\nwant %q", got, want)
	}
	managed := func(ref string) bool { return ref == "Sshd_config_match[User root Host example.net]" }
	if got := sshd.AutoRequires(r, managed); !reflect.DeepEqual(got, want[2:3]) {
		t.Errorf("only managed refs: %q", got)
	}
}

func TestRubySplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"\vHost a\tUser  b", []string{"Host", "a", "User", "b"}},
		{"", []string{}},
		{" \t\n\v\f\r ", []string{}},
		{"a", []string{"a"}},
		{"a\u00a0b c", []string{"a\u00a0b", "c"}},
		{"a\x00b", []string{"a\x00b"}},
		{"a \u2003b", []string{"a", "\u2003b"}},
	}
	for _, c := range cases {
		if got := rubySplit(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("rubySplit(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestConditionHash(t *testing.T) {
	cases := []struct {
		in   string
		want []condPair
	}{
		{"Host a User b", []condPair{{"Host", "a"}, {"User", "b"}}},
		{"User a User b", []condPair{{"User", "b"}}},
		{"Host x User y Host z", []condPair{{"Host", "z"}, {"User", "y"}}},
	}
	for _, c := range cases {
		got, err := parseSshdConfig(raltest.AugRes("Sshd_config", "x", map[string]any{"value": "v", "condition": c.in}))
		if err != nil || !reflect.DeepEqual(got.Condition, c.want) {
			t.Errorf("%q: %+v, %v; want %v", c.in, got.Condition, err, c.want)
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

// TestConditionKeysUnbounded: any number of distinct keys stays native; a
// repeated key counts once.
func TestConditionKeysUnbounded(t *testing.T) {
	res := func(c string) catalog.Resource {
		return raltest.AugRes("Sshd_config", "x", map[string]any{"value": "a", "condition": c})
	}
	if err := (SshdConfig{}).Validate(res("A a B b C c D d E e F f G g H h I i")); err != nil {
		t.Errorf("nine keys: %v", err)
	}
	if err := (SshdConfig{}).Validate(res("A a B b C c D d E e F f G g H h I i I j")); err != nil {
		t.Errorf("nine distinct keys, one repeated: %v", err)
	}
}
