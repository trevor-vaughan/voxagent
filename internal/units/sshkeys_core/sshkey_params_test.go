package sshkeyscore

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

const (
	rel25 = "2.5.0"
	rel31 = "3.1.0"

	allTypes = "ssh-dss, ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp256, ecdsa-sha2-nistp384, ecdsa-sha2-nistp521, sk-ecdsa-sha2-nistp256@openssh.com, sk-ssh-ed25519@openssh.com"
)

func skRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Sshkey", Title: title, Parameters: params}
}

func str(s string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.Str, S: s} }

func list(s ...string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.List, L: s} }

func TestParseSshkeyRefusals(t *testing.T) {
	badType := func(shown string) string {
		return "Parameter type failed on Sshkey[@]: Invalid value " + shown + ". Valid values are " + allTypes + "."
	}
	tests := []struct {
		name    string
		release string
		title   string
		params  map[string]any
		want    string
	}{
		{"name space", rel25, "a b", map[string]any{"type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name tab", rel25, "a\tb", map[string]any{"type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name vertical tab", rel25, "a\vb", map[string]any{"type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name comma", rel25, "a,b", map[string]any{"type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: No comma in resourcename allowed. If you want to specify aliases use the host_aliases property"},
		{"name space beats comma", rel25, "a, b", map[string]any{"type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name param refused, title fine", rel25, "ok", map[string]any{"name": "a b", "type": "rsa"}, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name from title pattern", rel25, "a b@rsa", nil, "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"},
		{"name failure with no type", rel25, "a b", nil, "Parameter name failed on Sshkey[@]: Resourcename cannot include whitespaces"},
		{"type bogus", rel25, "foo", map[string]any{"type": "bogus"}, badType(`"bogus"`)},
		{"type bogus 3.1.0", rel31, "foo", map[string]any{"type": "bogus"}, badType(`"bogus"`)},
		{"type is case sensitive", rel25, "foo", map[string]any{"type": "RSA"}, badType(`"RSA"`)},
		{"title splits at the first @", rel25, "a@b@rsa", nil, badType(`"b@rsa"`)},
		{"type from title pattern", rel25, "foo@bogus", nil, badType(`"bogus"`)},
		{"type empty from title pattern", rel25, "foo@", nil, badType(`""`)},
		{"type beats name", rel25, "a b", map[string]any{"type": "bogus"}, badType(`"bogus"`)},
		{"key whitespace 3.1.0", rel31, "foo", map[string]any{"type": "rsa", "key": "a b"}, `Parameter key failed on Sshkey[foo@ssh-rsa]: Key must not contain whitespace: "a b"`},
		{"key newline 3.1.0", rel31, "foo", map[string]any{"type": "rsa", "key": "a\nb"}, `Parameter key failed on Sshkey[foo@ssh-rsa]: Key must not contain whitespace: "a\nb"`},
		{"key vertical tab 3.1.0", rel31, "foo", map[string]any{"type": "rsa", "key": "a\vb"}, `Parameter key failed on Sshkey[foo@ssh-rsa]: Key must not contain whitespace: "a\vb"`},
		{"key second element 3.1.0", rel31, "foo", map[string]any{"type": "rsa", "key": []any{"ok", "a b"}}, `Parameter key failed on Sshkey[foo@ssh-rsa]: Key must not contain whitespace: "a b"`},
		{"alias space", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": []any{"a b"}}, "Parameter host_aliases failed on Sshkey[foo@ssh-rsa]: Aliases cannot include whitespace"},
		{"alias string with space", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": "a b"}, "Parameter host_aliases failed on Sshkey[foo@ssh-rsa]: Aliases cannot include whitespace"},
		{"alias comma", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": "a1,a2"}, "Parameter host_aliases failed on Sshkey[foo@ssh-rsa]: Aliases must be provided as an array, not a comma-separated list"},
		{"alias space beats comma", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": []any{"a, b"}}, "Parameter host_aliases failed on Sshkey[foo@ssh-rsa]: Aliases cannot include whitespace"},
		{"alias second element", rel31, "foo", map[string]any{"type": "dsa", "host_aliases": []any{"a", "b,c"}}, "Parameter host_aliases failed on Sshkey[foo@ssh-dss]: Aliases must be provided as an array, not a comma-separated list"},
		{"ensure invalid", rel25, "foo", map[string]any{"type": "rsa", "ensure": "latest"}, `Parameter ensure failed on Sshkey[foo@ssh-rsa]: Invalid value "latest". Valid values are present, absent.`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := skRes(tc.title, tc.params)
			_, err := parseSshkey(r, tc.release, "UTF-8")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if err := (Sshkey{release: tc.release, encoding: "UTF-8"}).Validate(r); err == nil || err.Error() != tc.want {
				t.Fatalf("Validate err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseSshkeyAccepted(t *testing.T) {
	const defTarget = "/etc/ssh/ssh_known_hosts"
	tests := []struct {
		name            string
		release         string
		title           string
		params          map[string]any
		wantName, wantT string
		wantTitle       string
		wantEnsure      string
		wantShould      map[string]parsedfile.Value
	}{
		{"title name, type alias", rel25, "foo", map[string]any{"type": "rsa"}, "foo", "ssh-rsa", "foo@ssh-rsa", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"title pattern splits name@type", rel25, "foo@rsa", nil, "foo", "ssh-rsa", "foo@ssh-rsa", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"explicit params beat the title", rel25, "x@dsa", map[string]any{"name": "foo", "type": "ed25519"}, "foo", "ssh-ed25519", "foo@ssh-ed25519", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"no type is not refused", rel25, "foo", map[string]any{"key": "A"}, "foo", "", "foo@", "present", map[string]parsedfile.Value{"key": str("A"), "target": str(defTarget)}},
		{"no type 3.1.0 is not refused", rel31, "foo", nil, "foo", "", "foo@", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"alias dsa", rel25, "f", map[string]any{"type": "dsa"}, "f", "ssh-dss", "f@ssh-dss", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"alias ed25519", rel25, "f", map[string]any{"type": "ed25519"}, "f", "ssh-ed25519", "f@ssh-ed25519", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"alias ecdsa-sk", rel25, "f", map[string]any{"type": "ecdsa-sk"}, "f", "sk-ecdsa-sha2-nistp256@openssh.com", "f@sk-ecdsa-sha2-nistp256@openssh.com", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"alias ed25519-sk", rel25, "f", map[string]any{"type": "ed25519-sk"}, "f", "sk-ssh-ed25519@openssh.com", "f@sk-ssh-ed25519@openssh.com", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"full name of a type", rel25, "f", map[string]any{"type": "ecdsa-sha2-nistp384"}, "f", "ecdsa-sha2-nistp384", "f@ecdsa-sha2-nistp384", "", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"2.5.0 accepts key with whitespace", rel25, "foo", map[string]any{"type": "rsa", "key": "a b"}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"key": str("a b"), "target": str(defTarget)}},
		{"3.1.0 accepts a plain key", rel31, "foo", map[string]any{"type": "rsa", "key": "AAAA"}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"key": str("AAAA"), "target": str(defTarget)}},
		{"string host_aliases is one element", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": "a1"}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"host_aliases": list("a1"), "target": str(defTarget)}},
		{"array host_aliases keep order", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": []any{"a2", "a1"}}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"host_aliases": list("a2", "a1"), "target": str(defTarget)}},
		{"empty host_aliases is an empty list", rel25, "foo", map[string]any{"type": "rsa", "host_aliases": []any{}}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"host_aliases": {Kind: parsedfile.List}, "target": str(defTarget)}},
		{"explicit target", rel25, "foo", map[string]any{"type": "rsa", "target": "/tmp/kh"}, "foo", "ssh-rsa", "foo@ssh-rsa", "present", map[string]parsedfile.Value{"target": str("/tmp/kh")}},
		{"ensure absent", rel25, "foo", map[string]any{"type": "rsa", "ensure": "absent"}, "foo", "ssh-rsa", "foo@ssh-rsa", "absent", map[string]parsedfile.Value{"target": str(defTarget)}},
		{"explicit ensure absent keeps its key", rel25, "foo", map[string]any{"type": "rsa", "ensure": "absent", "key": "K"}, "foo", "ssh-rsa", "foo@ssh-rsa", "absent", map[string]parsedfile.Value{"key": str("K"), "target": str(defTarget)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseSshkey(skRes(tc.title, tc.params), tc.release, "UTF-8")
			if err != nil {
				t.Fatal(err)
			}
			if p.Name != tc.wantName || p.Type != tc.wantT || p.Title != tc.wantTitle || p.Ensure != tc.wantEnsure {
				t.Errorf("got name %q type %q title %q ensure %q; want %q %q %q %q",
					p.Name, p.Type, p.Title, p.Ensure, tc.wantName, tc.wantT, tc.wantTitle, tc.wantEnsure)
			}
			if !reflect.DeepEqual(p.Should, tc.wantShould) {
				t.Errorf("Should = %+v, want %+v", p.Should, tc.wantShould)
			}
		})
	}
}

// A title without '@' names only the resource: no type, so Ruby's name is "foo@".
func TestParseSshkeyTitleWithoutAt(t *testing.T) {
	p, err := parseSshkey(skRes("foo", nil), rel25, "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "foo" || p.Type != "" || p.Title != "foo@" {
		t.Errorf("got %q %q %q", p.Name, p.Type, p.Title)
	}
}

func TestParseSshkeySensitiveKeyIsRedacted(t *testing.T) {
	r := skRes("foo", map[string]any{"type": "rsa", "key": "a b"})
	r.SensitiveParameters = []string{"key"}
	_, err := parseSshkey(r, rel31, "UTF-8")
	if want := "Parameter key failed on Sshkey[foo@ssh-rsa]: Key must not contain whitespace: [redacted]"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParseSshkeyInspectsUnderEncoding(t *testing.T) {
	r := skRes("foo", map[string]any{"type": "bög"})
	_, err := parseSshkey(r, rel25, "US-ASCII")
	if want := `Parameter type failed on Sshkey[@]: Invalid value "b\u00F6g". Valid values are ` + allTypes + "."; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Ruby validates in allattrs order (type, name, provider, then the
// properties) and stops at the first failure.
func TestParseSshkeyFirstFailureOrder(t *testing.T) {
	all := map[string]any{"type": "bogus", "key": "a b", "host_aliases": "x y", "ensure": "latest"}
	steps := []struct {
		drop []string
		want string
	}{
		{nil, "Parameter type failed on Sshkey[@]: "},
		{[]string{"type"}, "Parameter ensure failed on Sshkey[foo@]: "},
		{[]string{"type", "ensure"}, "Parameter key failed on Sshkey[foo@]: "},
		{[]string{"type", "ensure", "key"}, "Parameter host_aliases failed on Sshkey[foo@]: "},
	}
	for _, s := range steps {
		params := map[string]any{}
		for k, v := range all {
			params[k] = v
		}
		for _, d := range s.drop {
			delete(params, d)
		}
		_, err := parseSshkey(skRes("foo", params), rel31, "UTF-8")
		if err == nil || !strings.HasPrefix(err.Error(), s.want) {
			t.Errorf("drop %v: err = %v, want prefix %q", s.drop, err, s.want)
		}
	}
	// name is checked before every property, after type.
	_, err := parseSshkey(skRes("a b", map[string]any{"type": "rsa", "key": "a b"}), rel31, "UTF-8")
	if want := "Parameter name failed on Sshkey[@ssh-rsa]: Resourcename cannot include whitespaces"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParseSshkeyUnimplementedValues(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"type integer", map[string]any{"type": 5.0}, "type"},
		{"type array", map[string]any{"type": []any{"rsa"}}, "type"},
		{"name integer", map[string]any{"name": 5.0, "type": "rsa"}, "name"},
		{"ensure integer", map[string]any{"type": "rsa", "ensure": 5.0}, "ensure"},
		{"key integer", map[string]any{"type": "rsa", "key": 5.0}, "key"},
		{"key hash", map[string]any{"type": "rsa", "key": map[string]any{"a": "b"}}, "key"},
		{"host_aliases element integer", map[string]any{"type": "rsa", "host_aliases": []any{"a", 5.0}}, "host_aliases"},
		{"host_aliases hash", map[string]any{"type": "rsa", "host_aliases": map[string]any{"a": "b"}}, "host_aliases"},
		{"target boolean", map[string]any{"type": "rsa", "target": true}, "target"},
		{"key array with nil", map[string]any{"type": "rsa", "key": []any{nil}}, "key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := skRes("foo", tc.params)
			_, err := parseSshkey(r, rel31, "UTF-8")
			var u *rubyvalue.UnimplementedValue
			if !errors.As(err, &u) {
				t.Fatalf("err = %v, want *rubyvalue.UnimplementedValue", err)
			}
			if u.Attr != tc.attr || u.Ref != r.Ref() {
				t.Errorf("Attr %q Ref %q, want %q %q", u.Attr, u.Ref, tc.attr, r.Ref())
			}
			if err := (Sshkey{release: rel31, encoding: "UTF-8"}).Validate(r); !errors.As(err, &u) {
				t.Errorf("Validate err = %v, want *rubyvalue.UnimplementedValue", err)
			}
		})
	}
}

// A shape found before a Ruby refusal is delegated first; one after it is
// never reached.
func TestParseSshkeyUnimplementedOrder(t *testing.T) {
	r := skRes("foo", map[string]any{"type": "rsa", "key": 5.0, "host_aliases": "a b"})
	var u *rubyvalue.UnimplementedValue
	if _, err := parseSshkey(r, rel31, "UTF-8"); !errors.As(err, &u) || u.Attr != "key" {
		t.Fatalf("err = %v, want unimplemented key", err)
	}
	r = skRes("foo", map[string]any{"type": "rsa", "host_aliases": "a b", "key": 5.0})
	if _, err := parseSshkey(r, rel31, "UTF-8"); !errors.As(err, &u) || u.Attr != "key" {
		t.Fatalf("err = %v, want unimplemented key before the aliases refusal", err)
	}
	r = skRes("foo", map[string]any{"type": "bogus", "key": 5.0})
	if _, err := parseSshkey(r, rel31, "UTF-8"); errors.As(err, &u) {
		t.Fatalf("err = %v, want the type refusal first", err)
	}
}

func TestSshkeyIdentity(t *testing.T) {
	tests := []struct {
		name         string
		title        string
		params       map[string]any
		wantRef      string
		wantTitle    string
		wantOK       bool
		wantUnresolv bool
		wantAliases  []string
	}{
		{"title with explicit type", "foo", map[string]any{"type": "rsa"}, "Sshkey[foo@ssh-rsa]", "foo@ssh-rsa", true, true, []string{"Sshkey[foo@ssh-rsa]"}},
		{"title name@alias", "bar@rsa", nil, "Sshkey[bar@ssh-rsa]", "bar@ssh-rsa", true, true, []string{"Sshkey[bar@ssh-rsa]"}},
		{"title already the Puppet title", "baz@ssh-ed25519", nil, "", "", false, false, nil},
		{"no type resolves through its [name, nil] alias", "foo", map[string]any{"key": "A"}, "Sshkey[foo@]", "foo@", true, false, []string{"Sshkey[foo@]"}},
		{"no type, name differs from the title", "foo", map[string]any{"name": "bar"}, "Sshkey[bar@]", "bar@", true, true, []string{"Sshkey[bar@]"}},
		{"explicit name and type", "t", map[string]any{"name": "n", "type": "dsa"}, "Sshkey[n@ssh-dss]", "n@ssh-dss", true, true, []string{"Sshkey[n@ssh-dss]"}},
		{"refused resource has no Puppet name", "a b", map[string]any{"type": "rsa"}, "", "", false, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Sshkey{release: rel25, encoding: "UTF-8"}
			r := skRes(tc.title, tc.params)
			ref, title, ok := p.PuppetRef(r)
			if ref != tc.wantRef || title != tc.wantTitle || ok != tc.wantOK {
				t.Errorf("PuppetRef = %q %q %v, want %q %q %v", ref, title, ok, tc.wantRef, tc.wantTitle, tc.wantOK)
			}
			if got := p.Unresolvable(r); got != tc.wantUnresolv {
				t.Errorf("Unresolvable = %v, want %v", got, tc.wantUnresolv)
			}
			if got := p.Aliases(r); !reflect.DeepEqual(got, tc.wantAliases) {
				t.Errorf("Aliases = %v, want %v", got, tc.wantAliases)
			}
		})
	}
}

func TestSshkeyDeclaration(t *testing.T) {
	p := Sshkey{}
	if p.Type() != "sshkey" || p.ProviderUsed() == nil || *p.ProviderUsed() != "parsed" {
		t.Errorf("Type %q ProviderUsed %v", p.Type(), p.ProviderUsed())
	}
	for _, a := range []string{"name", "provider", "type", "ensure", "key", "host_aliases", "target"} {
		if _, ok := p.Attrs()[a]; !ok {
			t.Errorf("Attrs misses %s", a)
		}
	}
	if len(p.Properties()) != 4 {
		t.Errorf("Properties = %v, want ensure key host_aliases target", p.Properties())
	}
	if changed, err := p.Refresh(nil, skRes("foo", nil), false); changed || err != nil {
		t.Errorf("Refresh = %v %v", changed, err)
	}
	if got := p.AutoRequires(skRes("foo", nil), nil); got != nil {
		t.Errorf("AutoRequires = %v", got)
	}
}
