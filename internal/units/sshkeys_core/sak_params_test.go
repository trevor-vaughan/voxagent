package sshkeyscore

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// wantSakTypes is kt:67-70's list, which the refusal prints in this order.
const wantSakTypes = "ssh-dss, ssh-rsa, ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, ssh-ed25519, " +
	"sk-ecdsa-sha2-nistp256@openssh.com, sk-ssh-ed25519@openssh.com, ssh-rsa-cert-v01@openssh.com, " +
	"ssh-ed25519-cert-v01@openssh.com, ssh-dss-cert-v01@openssh.com, ecdsa-sha2-nistp256-cert-v01@openssh.com, " +
	"ecdsa-sha2-nistp384-cert-v01@openssh.com, ecdsa-sha2-nistp521-cert-v01@openssh.com"

func sakRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Ssh_authorized_key", Title: title, Parameters: params}
}

var absentValue = parsedfile.Value{Kind: parsedfile.Absent}

func TestParseSakRefusals(t *testing.T) {
	const ref = "Ssh_authorized_key[k1]"
	tests := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"key whitespace is raw, not inspected", map[string]any{"user": "u", "key": "a b"}, "Parameter key failed on " + ref + ": Key must not contain whitespace: a b"},
		{"key newline is a real line break", map[string]any{"user": "u", "key": "a\nb"}, "Parameter key failed on " + ref + ": Key must not contain whitespace: a\nb"},
		{"key vertical tab", map[string]any{"user": "u", "key": "a\vb"}, "Parameter key failed on " + ref + ": Key must not contain whitespace: a\vb"},
		{"key second element", map[string]any{"user": "u", "key": []any{"ok", "a b"}}, "Parameter key failed on " + ref + ": Key must not contain whitespace: a b"},
		{"type bogus", map[string]any{"user": "u", "type": "ssh-bogus"}, "Parameter type failed on " + ref + `: Invalid value "ssh-bogus". Valid values are ` + wantSakTypes + "."},
		{"type is case sensitive", map[string]any{"user": "u", "type": "RSA"}, "Parameter type failed on " + ref + `: Invalid value "RSA". Valid values are ` + wantSakTypes + "."},
		{"type empty", map[string]any{"user": "u", "type": ""}, "Parameter type failed on " + ref + `: Invalid value "". Valid values are ` + wantSakTypes + "."},
		{"drop_privileges maybe", map[string]any{"user": "u", "drop_privileges": "maybe"}, "Parameter drop_privileges failed on " + ref + `: Invalid value "maybe". Valid values are true, false, yes, no.`},
		{"drop_privileges is case sensitive", map[string]any{"user": "u", "drop_privileges": "True"}, "Parameter drop_privileges failed on " + ref + `: Invalid value "True". Valid values are true, false, yes, no.`},
		{"drop_privileges number", map[string]any{"user": "u", "drop_privileges": 1.0}, "Parameter drop_privileges failed on " + ref + ": Invalid value 1. Valid values are true, false, yes, no."},
		{"options bad", map[string]any{"user": "u", "options": []any{`command="x"`, "bad,opt"}}, "Parameter options failed on " + ref + ": Option bad,opt is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array"},
		{"options comma string", map[string]any{"user": "u", "options": "cert-authority,no-port-forwarding"}, "Parameter options failed on " + ref + ": Option cert-authority,no-port-forwarding is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array"},
		{"options empty string", map[string]any{"user": "u", "options": ""}, "Parameter options failed on " + ref + ": Option  is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array"},
		{"options space", map[string]any{"user": "u", "options": "a b"}, "Parameter options failed on " + ref + ": Option a b is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array"},
		{"options unterminated value", map[string]any{"user": "u", "options": `command="x`}, "Parameter options failed on " + ref + ": Option command=\"x is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array"},
		{"ensure invalid", map[string]any{"user": "u", "ensure": "latest"}, "Parameter ensure failed on " + ref + `: Invalid value "latest". Valid values are present, absent.`},
		{"neither user nor target", nil, "Validation of " + ref + " failed: Attribute 'user' or 'target' is mandatory"},
		{"target undef is not given", map[string]any{"target": nil}, "Validation of " + ref + " failed: Attribute 'user' or 'target' is mandatory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sakRes("k1", tc.params)
			for _, release := range []string{rel25, rel31} {
				_, err := parseSak(r, release, "UTF-8")
				if err == nil || err.Error() != tc.want {
					t.Fatalf("%s: err = %v, want %q", release, err, tc.want)
				}
				if err := (Sak{release: release, encoding: "UTF-8"}).Validate(r); err == nil || err.Error() != tc.want {
					t.Fatalf("%s: Validate err = %v, want %q", release, err, tc.want)
				}
			}
		})
	}
}

// Ruby's ref reads the catalog title, not the name parameter (oracle,
// 17h-sak-title-vs-name).
func TestParseSakRefusalNamesTheTitle(t *testing.T) {
	r := sakRes("t", map[string]any{"name": "n", "user": "u", "key": "a b"})
	_, err := parseSak(r, rel25, "UTF-8")
	if want := "Parameter key failed on Ssh_authorized_key[t]: Key must not contain whitespace: a b"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParseSakAccepted(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		params     map[string]any
		wantName   string
		wantUser   string
		wantEnsure string
		wantDrop   bool
		wantShould map[string]parsedfile.Value
	}{
		{"user only", "k1", map[string]any{"user": "alice"}, "k1", "alice", "present", true,
			map[string]parsedfile.Value{"options": absentValue}},
		{"target only", "k1", map[string]any{"target": "/t"}, "k1", "", "present", true,
			map[string]parsedfile.Value{"target": str("/t"), "options": absentValue}},
		{"name param beats the title", "t", map[string]any{"name": "n", "user": "u"}, "n", "u", "present", true,
			map[string]parsedfile.Value{"options": absentValue}},
		{"name keeps whitespace and newlines", "a b\nc", map[string]any{"user": "u"}, "a b\nc", "u", "present", true,
			map[string]parsedfile.Value{"options": absentValue}},
		{"full", "k1", map[string]any{"user": "u", "type": "rsa", "key": "AAAA", "target": "/t", "options": []any{"no-pty", `command="x y"`}, "ensure": "absent", "drop_privileges": "no"},
			"k1", "u", "absent", false,
			map[string]parsedfile.Value{"type": str("ssh-rsa"), "key": str("AAAA"), "target": str("/t"), "options": list("no-pty", `command="x y"`)}},
		{"alias dsa", "k1", map[string]any{"user": "u", "type": "dsa"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"type": str("ssh-dss"), "options": absentValue}},
		{"alias ed25519", "k1", map[string]any{"user": "u", "type": "ed25519"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"type": str("ssh-ed25519"), "options": absentValue}},
		{"alias ecdsa-sk", "k1", map[string]any{"user": "u", "type": "ecdsa-sk"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"type": str("sk-ecdsa-sha2-nistp256@openssh.com"), "options": absentValue}},
		{"alias ed25519-sk", "k1", map[string]any{"user": "u", "type": "ed25519-sk"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"type": str("sk-ssh-ed25519@openssh.com"), "options": absentValue}},
		{"cert type", "k1", map[string]any{"user": "u", "type": "ecdsa-sha2-nistp521-cert-v01@openssh.com"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"type": str("ecdsa-sha2-nistp521-cert-v01@openssh.com"), "options": absentValue}},
		{"drop_privileges yes", "k1", map[string]any{"user": "u", "drop_privileges": "yes"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": absentValue}},
		{"drop_privileges false", "k1", map[string]any{"user": "u", "drop_privileges": false}, "k1", "u", "present", false,
			map[string]parsedfile.Value{"options": absentValue}},
		{"string options is one element", "k1", map[string]any{"user": "u", "options": "cert-authority"}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": list("cert-authority")}},
		{"options kv with quotes and commas", "k1", map[string]any{"user": "u", "options": `from="*.a,b"`}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": list(`from="*.a,b"`)}},
		{"options line break matches line-anchored", "k1", map[string]any{"user": "u", "options": "no-pty\nfoo=\"bar\""}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": list("no-pty\nfoo=\"bar\"")}},
		{"options element absent is the string", "k1", map[string]any{"user": "u", "options": []any{"absent"}}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": list("absent")}},
		{"empty options list", "k1", map[string]any{"user": "u", "options": []any{}}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"options": {Kind: parsedfile.List}}},
		{"key first of an array", "k1", map[string]any{"user": "u", "key": []any{"K1", "K2"}}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"key": str("K1"), "options": absentValue}},
		{"target absent is a path", "k1", map[string]any{"target": "absent"}, "k1", "", "present", true,
			map[string]parsedfile.Value{"target": str("absent"), "options": absentValue}},
		{"empty target list is no target and satisfies the check", "k1", map[string]any{"target": []any{}}, "k1", "", "", true,
			map[string]parsedfile.Value{"target": {Kind: parsedfile.Nil}, "options": absentValue}},
		{"ensure absent alone", "k1", map[string]any{"user": "u", "ensure": "absent"}, "k1", "u", "absent", true,
			map[string]parsedfile.Value{"options": absentValue}},
		{"2.5.0 and 3.1.0 accept the same key", "k1", map[string]any{"user": "u", "key": "AAAA=="}, "k1", "u", "present", true,
			map[string]parsedfile.Value{"key": str("AAAA=="), "options": absentValue}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, release := range []string{rel25, rel31} {
				p, err := parseSak(sakRes(tc.title, tc.params), release, "UTF-8")
				if err != nil {
					t.Fatalf("%s: %v", release, err)
				}
				if p.Name != tc.wantName || p.Title != tc.title || p.User != tc.wantUser || p.Ensure != tc.wantEnsure || p.DropPrivileges != tc.wantDrop {
					t.Errorf("%s: got name %q title %q user %q ensure %q drop %v; want %q %q %q %v", release, p.Name, p.Title, p.User, p.Ensure, p.DropPrivileges, tc.wantName, tc.wantUser, tc.wantEnsure, tc.wantDrop)
				}
				if len(p.Should) != len(tc.wantShould) {
					t.Errorf("%s: Should = %+v, want %+v", release, p.Should, tc.wantShould)
				}
				for k, want := range tc.wantShould {
					got := p.Should[k]
					if got.Kind != want.Kind || got.S != want.S || strings.Join(got.L, "\x00") != strings.Join(want.L, "\x00") {
						t.Errorf("%s: Should[%s] = %+v, want %+v", release, k, got, want)
					}
				}
			}
		})
	}
}

// A sak's ensure defaults to present only when a property is given; options'
// and target's own defaults come after ensure's (set_parameters).
func TestParseSakEnsureDefault(t *testing.T) {
	// Every shape that has a should counts; an empty first-of list has none.
	for name, params := range map[string]map[string]any{
		"type":    {"user": "u", "type": "rsa"},
		"key":     {"user": "u", "key": "K"},
		"user":    {"user": "u"},
		"target":  {"target": "/t"},
		"options": {"target": "/t", "options": []any{}},
	} {
		p, err := parseSak(sakRes("k1", params), rel25, "UTF-8")
		if err != nil || p.Ensure != "present" {
			t.Errorf("%s: ensure = %q, %v; want present", name, p.Ensure, err)
		}
	}
	for name, params := range map[string]map[string]any{
		"empty user list":   {"user": []any{}, "target": []any{}},
		"empty target list": {"target": []any{}},
	} {
		p, err := parseSak(sakRes("k1", params), rel25, "UTF-8")
		if err != nil || p.Ensure != "" {
			t.Errorf("%s: ensure = %q, %v; want unmanaged", name, p.Ensure, err)
		}
	}
}

func TestParseSakSensitiveValuesAreRedacted(t *testing.T) {
	r := sakRes("k1", map[string]any{"user": "u", "key": "a b"})
	r.SensitiveParameters = []string{"key"}
	_, err := parseSak(r, rel31, "UTF-8")
	if want := "Parameter key failed on Ssh_authorized_key[k1]: Key must not contain whitespace: [redacted]"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	r = sakRes("k1", map[string]any{"user": "u", "options": "bad,opt"})
	r.SensitiveParameters = []string{"options"}
	if _, err = parseSak(r, rel31, "UTF-8"); err == nil || strings.Contains(err.Error(), "bad,opt") || !strings.Contains(err.Error(), "Option [redacted] is not valid.") {
		t.Fatalf("err = %v, want the option redacted", err)
	}
	r = sakRes("k1", map[string]any{"user": "u", "type": "secret"})
	r.SensitiveParameters = []string{"type"}
	if _, err = parseSak(r, rel31, "UTF-8"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want the type redacted", err)
	}
	r = sakRes("k1", map[string]any{"user": "u", "drop_privileges": "secret"})
	r.SensitiveParameters = []string{"drop_privileges"}
	if _, err = parseSak(r, rel31, "UTF-8"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want drop_privileges redacted", err)
	}
}

func TestParseSakInspectsUnderEncoding(t *testing.T) {
	r := sakRes("k1", map[string]any{"user": "u", "type": "bög"})
	_, err := parseSak(r, rel25, "US-ASCII")
	if want := `Parameter type failed on Ssh_authorized_key[k1]: Invalid value "b\u00F6g". Valid values are ` + wantSakTypes + "."; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Ruby validates in allattrs order: name, provider, ensure, type, key, user,
// target, options, then the parameters (drop_privileges); the type-level
// validate runs last.
func TestParseSakFirstFailureOrder(t *testing.T) {
	all := map[string]any{"ensure": "latest", "type": "bogus", "key": "a b", "options": "x y", "drop_privileges": "maybe"}
	for _, step := range []struct {
		drop []string
		want string
	}{
		{nil, "Parameter ensure failed on "},
		{[]string{"ensure"}, "Parameter type failed on "},
		{[]string{"ensure", "type"}, "Parameter key failed on "},
		{[]string{"ensure", "type", "key"}, "Parameter options failed on "},
		{[]string{"ensure", "type", "key", "options"}, "Parameter drop_privileges failed on "},
		{[]string{"ensure", "type", "key", "options", "drop_privileges"}, "Validation of "},
	} {
		params := map[string]any{}
		for k, v := range all {
			params[k] = v
		}
		for _, d := range step.drop {
			delete(params, d)
		}
		_, err := parseSak(sakRes("k1", params), rel31, "UTF-8")
		if err == nil || !strings.HasPrefix(err.Error(), step.want) {
			t.Errorf("drop %v: err = %v, want prefix %q", step.drop, err, step.want)
		}
	}
}

func TestParseSakUnimplementedValues(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"name integer", map[string]any{"name": 5.0, "user": "u"}, "name"},
		{"ensure integer", map[string]any{"user": "u", "ensure": 5.0}, "ensure"},
		{"type integer", map[string]any{"user": "u", "type": 5.0}, "type"},
		{"key integer", map[string]any{"user": "u", "key": 5.0}, "key"},
		{"key hash", map[string]any{"user": "u", "key": map[string]any{"a": "b"}}, "key"},
		{"user integer", map[string]any{"user": 5.0}, "user"},
		{"target boolean", map[string]any{"user": "u", "target": true}, "target"},
		{"options element integer", map[string]any{"user": "u", "options": []any{"a", 5.0}}, "options"},
		{"options element nil", map[string]any{"user": "u", "options": []any{nil}}, "options"},
		{"options hash", map[string]any{"user": "u", "options": map[string]any{"a": "b"}}, "options"},
		{"drop_privileges hash", map[string]any{"user": "u", "drop_privileges": map[string]any{"a": "b"}}, "drop_privileges"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sakRes("k1", tc.params)
			var u *rubyvalue.UnimplementedValue
			_, err := parseSak(r, rel31, "UTF-8")
			if !errors.As(err, &u) {
				t.Fatalf("err = %v, want *rubyvalue.UnimplementedValue", err)
			}
			if u.Attr != tc.attr || u.Ref != r.Ref() {
				t.Errorf("Attr %q Ref %q, want %q %q", u.Attr, u.Ref, tc.attr, r.Ref())
			}
			if err := (Sak{release: rel31, encoding: "UTF-8"}).Validate(r); !errors.As(err, &u) {
				t.Errorf("Validate err = %v, want *rubyvalue.UnimplementedValue", err)
			}
		})
	}
}

// A shape found before a Ruby refusal is delegated first; one after it never
// is reached.
func TestParseSakUnimplementedOrder(t *testing.T) {
	var u *rubyvalue.UnimplementedValue
	r := sakRes("k1", map[string]any{"user": "u", "key": 5.0, "options": "a b"})
	if _, err := parseSak(r, rel31, "UTF-8"); !errors.As(err, &u) || u.Attr != "key" {
		t.Fatalf("err = %v, want unimplemented key before the options refusal", err)
	}
	r = sakRes("k1", map[string]any{"user": "u", "type": "bogus", "key": 5.0})
	if _, err := parseSak(r, rel31, "UTF-8"); errors.As(err, &u) {
		t.Fatalf("err = %v, want the type refusal first", err)
	}
}

func TestSakIdentity(t *testing.T) {
	s := Sak{}
	if s.Type() != "ssh_authorized_key" || s.ProviderUsed() == nil || *s.ProviderUsed() != "parsed" {
		t.Fatalf("Type %q ProviderUsed %v", s.Type(), s.ProviderUsed())
	}
	wantProps := []string{"ensure", "type", "key", "user", "target", "options"}
	if got, want := s.Properties().Sorted(), ral.NewAttrs(wantProps...).Sorted(); !slices.Equal(got, want) {
		t.Errorf("Properties = %v, want %v", got, want)
	}
	if !slices.Equal(sakProperties, wantProps) {
		t.Errorf("sakProperties = %v, want the sync order %v", sakProperties, wantProps)
	}
	wantAttrs := ral.NewAttrs(append([]string{"name", "provider", "drop_privileges"}, wantProps...)...)
	if got, want := s.Attrs().Sorted(), wantAttrs.Sorted(); !slices.Equal(got, want) {
		t.Errorf("Attrs = %v, want %v", got, want)
	}
}

func TestSakAutoRequires(t *testing.T) {
	s := Sak{release: rel25, encoding: "UTF-8"}
	managed := func(ref string) bool { return ref == "User[alice]" }
	if got := s.AutoRequires(sakRes("k1", map[string]any{"user": "alice"}), managed); len(got) != 1 || got[0] != "User[alice]" {
		t.Errorf("AutoRequires = %v, want [User[alice]]", got)
	}
	if got := s.AutoRequires(sakRes("k1", map[string]any{"user": "bob"}), managed); len(got) != 0 {
		t.Errorf("an unmanaged user is no edge: %v", got)
	}
	if got := s.AutoRequires(sakRes("k1", map[string]any{"target": "/t"}), managed); len(got) != 0 {
		t.Errorf("no user, no edge: %v", got)
	}
	if got := s.AutoRequires(sakRes("k1", map[string]any{"user": "alice", "key": "a b", "type": "bogus"}), managed); len(got) != 0 {
		t.Errorf("a refused resource has no edge: %v", got)
	}
}
