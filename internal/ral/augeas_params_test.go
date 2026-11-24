package ral

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func augRes(typ, title string, params map[string]any, marked ...string) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Parameters: params, SensitiveParameters: marked}
}

// augValidator is what the four types share.
type augValidator interface {
	Validate(catalog.Resource) error
	Attrs() Attrs
	Type() string
	ProviderUsed() *string
	AutoRequires(catalog.Resource, func(string) bool) []string
}

var augTypes = map[string]augValidator{
	"Sysctl":                Sysctl{},
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
		{"sysctl ensure", "Sysctl", "vm.swappiness", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "bogus". Valid values are present, absent.`},
		{"ssh_config ensure", "Ssh_config", "x", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Ssh_config[x]: Invalid value "bogus". Valid values are present, absent.`},
		{"sshd_config ensure", "Sshd_config", "x", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sshd_config[x]: Invalid value "bogus". Valid values are present, absent.`},
		{"subsystem ensure", "Sshd_config_subsystem", "sftp", map[string]any{"ensure": "bogus"}, nil, `Parameter ensure failed on Sshd_config_subsystem[sftp]: Invalid value "bogus". Valid values are present, absent.`},
		{"ensure number", "Sysctl", "vm.swappiness", map[string]any{"ensure": float64(1)}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value 1. Valid values are present, absent.`},
		{"ensure case", "Sysctl", "vm.swappiness", map[string]any{"ensure": "Present"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "Present". Valid values are present, absent.`},
		{"apply yes", "Sysctl", "vm.swappiness", map[string]any{"apply": "yes"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "yes". Valid values are true, false.`},
		{"apply True", "Sysctl", "vm.swappiness", map[string]any{"apply": "True"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "True". Valid values are true, false.`},
		{"apply 1", "Sysctl", "vm.swappiness", map[string]any{"apply": float64(1)}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value 1. Valid values are true, false.`},
		{"apply array", "Sysctl", "vm.swappiness", map[string]any{"apply": []any{"x", true}}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value ["x", true]. Valid values are true, false.`},
		{"persist", "Sysctl", "vm.swappiness", map[string]any{"persist": "maybe"}, nil, `Parameter persist failed on Sysctl[vm.swappiness]: Invalid value "maybe". Valid values are true, false.`},
		{"silent", "Sysctl", "vm.swappiness", map[string]any{"silent": float64(0)}, nil, `Parameter silent failed on Sysctl[vm.swappiness]: Invalid value 0. Valid values are true, false.`},
		{"apply string true", "Sysctl", "vm.swappiness", map[string]any{"apply": "false", "silent": "true"}, nil, ""},
		{"target relative", "Ssh_config", "x", map[string]any{"target": "etc/ssh/ssh_config"}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target windows", "Ssh_config", "x", map[string]any{"target": "C:/x"}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target empty", "Ssh_config", "x", map[string]any{"target": ""}, nil, `Parameter target failed on Ssh_config[x]: target must be an absolute path`},
		{"target absolute", "Ssh_config", "x", map[string]any{"target": "/etc/ssh/ssh_config"}, nil, ""},
		{"array_append", "Sshd_config", "AllowUsers", map[string]any{"array_append": "maybe"}, nil, `Parameter array_append failed on Sshd_config[AllowUsers]: Invalid value "maybe". Valid values are false, true.`},
		{"array_append true", "Sshd_config", "AllowUsers", map[string]any{"array_append": "true", "value": []any{"a"}}, nil, ""},
		{"first failure ensure", "Sysctl", "vm.swappiness", map[string]any{"ensure": "bogus", "apply": "yes"}, nil, `Parameter ensure failed on Sysctl[vm.swappiness]: Invalid value "bogus". Valid values are present, absent.`},
		{"first failure sysctl apply before silent", "Sysctl", "vm.swappiness", map[string]any{"silent": "x", "apply": "y"}, nil, `Parameter apply failed on Sysctl[vm.swappiness]: Invalid value "y". Valid values are true, false.`},
		{"sysctl plain", "Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"}, nil, ""},
		{"sysctl integer", "Sysctl", "vm.swappiness", map[string]any{"value": float64(10), "ensure": "present"}, nil, ""},
		{"ssh_config plain", "Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}, "host": "*"}, nil, ""},
		{"sshd_config condition", "Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"}, nil, ""},
		{"subsystem plain", "Sshd_config_subsystem", "sftp", map[string]any{"command": "/usr/libexec/sftp-server"}, nil, ""},
		{"nothing managed", "Sshd_config", "AllowUsers", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := augTypes[c.typ].Validate(augRes(c.typ, c.title, c.params, c.marked...))
			if c.want == "" {
				if err != nil && c.marked == nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v\nwant %s", err, c.want)
			}
			var uv *unimplementedValue
			if errors.As(err, &uv) {
				t.Fatalf("a Ruby refusal must not be an unimplementedValue: %v", err)
			}
		})
	}
}

// TestAugeasDelegates: what Ruby accepts and native does not implement is an
// unimplementedValue, so the router sends the type to the host (spec §2 d-f).
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
		{"apply marked", "Sysctl", "vm.swappiness", map[string]any{"apply": "yes"}, []string{"apply"}},
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
			err := augTypes[c.typ].Validate(augRes(c.typ, c.title, c.params, c.marked...))
			var uv *unimplementedValue
			if !errors.As(err, &uv) {
				t.Fatalf("want unimplementedValue, got %v", err)
			}
			if len(c.marked) > 0 && strings.Contains(err.Error(), `"1"`) {
				t.Errorf("marked value leaks: %v", err)
			}
		})
	}
}

func TestAugeasParse(t *testing.T) {
	t.Run("sysctl", func(t *testing.T) {
		got, err := parseSysctl(augRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": float64(1), "target": "/etc/sysctl.d/a.conf", "apply": "false", "silent": true, "ensure": "present"}))
		want := sysctlParams{Name: "net.ipv4.ip_forward", Target: "/etc/sysctl.d/a.conf", Value: "1", HasValue: true, Apply: false, Silent: true, EnsureSet: true}
		if err != nil || got != want {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSysctl(augRes("Sysctl", "vm.swappiness", nil))
		if want := (sysctlParams{Name: "vm.swappiness", Apply: true}); err != nil || got != want {
			t.Fatalf("defaults: got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSysctl(augRes("Sysctl", "vm.swappiness", map[string]any{"value": ""}))
		if err != nil || !got.HasValue || got.Value != "" {
			t.Fatalf("empty value: %+v, %v", got, err)
		}
	})
	t.Run("ssh_config", func(t *testing.T) {
		got, err := parseSshConfig(augRes("Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}, "key": "MACs", "ensure": "present", "target": "/etc/ssh/x"}))
		want := sshConfigParams{Name: "Ciphers", Key: "MACs", Host: "*", Target: "/etc/ssh/x", Value: []string{"a", "b"}, Ensure: "present"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSshConfig(augRes("Ssh_config", "Ciphers", map[string]any{"value": "a", "host": "web*"}))
		want = sshConfigParams{Name: "Ciphers", Key: "Ciphers", Host: "web*", Value: []string{"a"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
	})
	t.Run("sshd_config", func(t *testing.T) {
		got, err := parseSshdConfig(augRes("Sshd_config", "Port", map[string]any{"value": []any{float64(22), "2222"}, "array_append": true, "condition": "User a Host b"}))
		want := sshdConfigParams{Name: "Port", Key: "Port", Value: []string{"22", "2222"}, ArrayAppend: true, Condition: []condPair{{"User", "a"}, {"Host", "b"}}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSshdConfig(augRes("Sshd_config", "x", map[string]any{"key": "PermitRootLogin", "value": float64(3), "ensure": "absent"}))
		want = sshdConfigParams{Name: "x", Key: "PermitRootLogin", Value: []string{"3"}, Ensure: "absent"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
	})
	t.Run("subsystem", func(t *testing.T) {
		got, err := parseSubsystem(augRes("Sshd_config_subsystem", "sftp", map[string]any{"command": "/x", "ensure": "present", "target": "/etc/ssh/sshd_config"}))
		want := subsystemParams{Name: "sftp", Target: "/etc/ssh/sshd_config", Command: "/x", HasCommand: true, Ensure: "present"}
		if err != nil || got != want {
			t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
		}
		got, err = parseSubsystem(augRes("Sshd_config_subsystem", "sftp", map[string]any{"ensure": "absent"}))
		if err != nil || got.HasCommand || got.Ensure != "absent" {
			t.Fatalf("absent: %+v, %v", got, err)
		}
	})
}

func TestAugeasAttrs(t *testing.T) {
	want := map[string][]string{
		"Sysctl":                {"name", "provider", "ensure", "val", "value", "comment", "target", "apply", "persist", "silent"},
		"Ssh_config":            {"name", "provider", "ensure", "value", "comment", "key", "target", "host"},
		"Sshd_config":           {"name", "provider", "ensure", "value", "comment", "key", "array_append", "target", "condition"},
		"Sshd_config_subsystem": {"name", "provider", "ensure", "command", "comment", "target"},
	}
	for typ, names := range want {
		if got, w := augTypes[typ].Attrs(), NewAttrs(names...); !reflect.DeepEqual(got, w) {
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
		title := map[string]string{"Sysctl": "vm.swappiness", "Sshd_config_subsystem": "sftp"}[typ]
		if title == "" {
			title = "Port"
		}
		r := augRes(typ, title, map[string]any{"target": "/etc/x.conf"})
		if got := v.AutoRequires(r, all); !reflect.DeepEqual(got, []string{"File[/etc/x.conf]"}) {
			t.Errorf("%s with target: %v", typ, got)
		}
		if got := v.AutoRequires(r, none); len(got) != 0 {
			t.Errorf("%s unmanaged is no edge: %v", typ, got)
		}
		if got := v.AutoRequires(augRes(typ, title, nil), all); len(got) != 0 {
			t.Errorf("%s without target: %v", typ, got)
		}
		if got := v.AutoRequires(augRes(typ, title, map[string]any{"target": "rel", "ensure": "bogus"}), all); len(got) != 0 {
			t.Errorf("%s invalid: %v", typ, got)
		}
	}
	r := augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root", "target": "/etc/ssh/sshd_config"})
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
	r = augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"})
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
		got, err := parseSshdConfig(augRes("Sshd_config", "x", map[string]any{"value": "v", "condition": c.in}))
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
						r := augRes(typ, "Port", map[string]any{attr: val}, m...)
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
		return augRes("Sshd_config", "x", map[string]any{"value": "a", "condition": c})
	}
	if err := (SshdConfig{}).Validate(res("A a B b C c D d E e F f G g H h I i")); err != nil {
		t.Errorf("nine keys: %v", err)
	}
	if err := (SshdConfig{}).Validate(res("A a B b C c D d E e F f G g H h I i I j")); err != nil {
		t.Errorf("nine distinct keys, one repeated: %v", err)
	}
}
