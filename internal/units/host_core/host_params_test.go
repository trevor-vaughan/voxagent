package hostcore

import (
	"errors"
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

func hostRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Host", Title: title, Parameters: params}
}

func str(s string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.Str, S: s} }

func list(s ...string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.List, L: s} }

func TestParseHostRefusals(t *testing.T) {
	const aliasEmpty = "Parameter host_aliases failed on Host[h]: Host aliases cannot be an empty string. Use an empty array to delete all host_aliases "
	const aliasSpace = "Parameter host_aliases failed on Host[h]: Host aliases cannot include whitespace"
	badIP := func(shown string) string { return "Parameter ip failed on Host[h]: Invalid IP address " + shown }
	tests := []struct {
		name   string
		title  string
		params map[string]any
		want   string
	}{
		{"ip junk", "h", map[string]any{"ip": "bogus"}, badIP(`"bogus"`)},
		{"ip v4 octet over 255", "h", map[string]any{"ip": "1.2.3.256"}, badIP(`"1.2.3.256"`)},
		{"ip v4 four digits", "h", map[string]any{"ip": "1.2.3.1000"}, badIP(`"1.2.3.1000"`)},
		{"ip v4 trailing LF", "h", map[string]any{"ip": "1.2.3.4\n"}, badIP(`"1.2.3.4\n"`)},
		{"ip v4 trailing CR", "h", map[string]any{"ip": "1.2.3.4\r"}, badIP(`"1.2.3.4\r"`)},
		{"ip v6 trailing LF", "h", map[string]any{"ip": "::1\n"}, badIP(`"::1\n"`)},
		{"ip v6 too many groups", "h", map[string]any{"ip": "1:2:3:4:5:6:7:8:9"}, badIP(`"1:2:3:4:5:6:7:8:9"`)},
		{"ip empty", "h", map[string]any{"ip": ""}, badIP(`""`)},
		{"ip second element", "h", map[string]any{"ip": []any{"1.2.3.4", "bogus"}}, badIP(`"bogus"`)},
		{"alias with space", "h", map[string]any{"host_aliases": "a  b   c"}, aliasSpace},
		{"alias element with space", "h", map[string]any{"host_aliases": []any{"a", "b c"}}, aliasSpace},
		{"alias with tab", "h", map[string]any{"host_aliases": []any{"a\tb"}}, aliasSpace},
		{"alias with vertical tab", "h", map[string]any{"host_aliases": []any{"a\vb"}}, aliasSpace},
		{"alias with newline", "h", map[string]any{"host_aliases": []any{"a\nb"}}, aliasSpace},
		{"alias empty", "h", map[string]any{"host_aliases": []any{""}}, aliasEmpty},
		{"alias empty second", "h", map[string]any{"host_aliases": []any{"a", ""}}, aliasEmpty},
		{"comment LF", "h", map[string]any{"comment": "a\nb"}, "Parameter comment failed on Host[h]: Comment cannot include newline"},
		{"comment CR", "h", map[string]any{"comment": "a\rb"}, "Parameter comment failed on Host[h]: Comment cannot include newline"},
		{"name double dot", "a..b", nil, "Parameter name failed on Host[a..b]: Invalid host name"},
		{"name leading dot", ".a", nil, "Parameter name failed on Host[.a]: Invalid host name"},
		{"name space", "a b", nil, "Parameter name failed on Host[a b]: Invalid host name"},
		{"name trailing hyphen", "a-", nil, "Parameter name failed on Host[a-]: Invalid host name"},
		{"name leading hyphen", "-ab", nil, "Parameter name failed on Host[-ab]: Invalid host name"},
		{"name newline", "a\nb", nil, "Parameter name failed on Host[a\nb]: Hostname cannot include newline"},
		{"name CR is no line break for the part check", "a\rb", nil, "Parameter name failed on Host[a\rb]: Invalid host name"},
		{"name param overrides title", "ok", map[string]any{"name": "a..b"}, "Parameter name failed on Host[ok]: Invalid host name"},
		{"ensure invalid", "h", map[string]any{"ensure": "latest"}, `Parameter ensure failed on Host[h]: Invalid value "latest". Valid values are present, absent.`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := hostRes(tc.title, tc.params)
			want := tc.want
			_, err := parseHost(r, "UTF-8")
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if err := (Host{encoding: "UTF-8"}).Validate(r); err == nil || err.Error() != want {
				t.Fatalf("Validate err = %v, want %q", err, want)
			}
		})
	}
}

func TestParseHostSensitiveIPIsRedacted(t *testing.T) {
	r := hostRes("h", map[string]any{"ip": "bogus"})
	r.SensitiveParameters = []string{"ip"}
	_, err := parseHost(r, "UTF-8")
	if want := "Parameter ip failed on Host[h]: Invalid IP address [redacted]"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Ruby validates in allattrs order and stops at the first failure.
func TestParseHostFirstFailureOrder(t *testing.T) {
	r := hostRes("a..b", map[string]any{"ip": "bogus", "host_aliases": "x y", "comment": "a\nb"})
	_, err := parseHost(r, "UTF-8")
	if want := "Parameter name failed on Host[a..b]: Invalid host name"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	r = hostRes("h", map[string]any{"ip": "bogus", "host_aliases": "x y", "comment": "a\nb"})
	_, err = parseHost(r, "UTF-8")
	if want := `Parameter ip failed on Host[h]: Invalid IP address "bogus"`; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	r = hostRes("h", map[string]any{"host_aliases": "x y", "comment": "a\nb"})
	_, err = parseHost(r, "UTF-8")
	if want := "Parameter host_aliases failed on Host[h]: Host aliases cannot include whitespace"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestParseHostAccepted(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		params     map[string]any
		wantEnsure string
		want       map[string]parsedfile.Value
	}{
		{"name only", "h", nil, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"name trailing dot", "a.", map[string]any{"ip": "192.0.2.1"}, "present", map[string]parsedfile.Value{"ip": str("192.0.2.1"), "target": str(defaultTarget)}},
		{"name dotted fqdn", "a-b.example.com", nil, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"name empty", "", nil, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"name only dots", "...", nil, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"v4 boundaries", "h", map[string]any{"ip": "0.0.0.255"}, "present", map[string]parsedfile.Value{"ip": str("0.0.0.255"), "target": str(defaultTarget)}},
		{"v6 loopback", "h", map[string]any{"ip": "::1"}, "present", map[string]parsedfile.Value{"ip": str("::1"), "target": str(defaultTarget)}},
		{"v6 full", "h", map[string]any{"ip": "2001:db8:0:0:0:0:0:1"}, "present", map[string]parsedfile.Value{"ip": str("2001:db8:0:0:0:0:0:1"), "target": str(defaultTarget)}},
		{"v6 zone", "h", map[string]any{"ip": "fe80::1%eth0"}, "present", map[string]parsedfile.Value{"ip": str("fe80::1%eth0"), "target": str(defaultTarget)}},
		{"v6 mapped v4", "h", map[string]any{"ip": "::ffff:192.0.2.1"}, "present", map[string]parsedfile.Value{"ip": str("::ffff:192.0.2.1"), "target": str(defaultTarget)}},
		{"v6 surrounding space", "h", map[string]any{"ip": " ::1 "}, "present", map[string]parsedfile.Value{"ip": str(" ::1 "), "target": str(defaultTarget)}},
		{"one alias string", "h", map[string]any{"host_aliases": "one"}, "present", map[string]parsedfile.Value{"host_aliases": list("one"), "target": str(defaultTarget)}},
		{"alias array", "h", map[string]any{"host_aliases": []any{"x", "y"}}, "present", map[string]parsedfile.Value{"host_aliases": list("x", "y"), "target": str(defaultTarget)}},
		{"alias empty array", "h", map[string]any{"host_aliases": []any{}}, "present", map[string]parsedfile.Value{"host_aliases": {Kind: parsedfile.List}, "target": str(defaultTarget)}},
		{"comment empty", "h", map[string]any{"comment": ""}, "present", map[string]parsedfile.Value{"comment": str(""), "target": str(defaultTarget)}},
		{"comment text", "h", map[string]any{"comment": "x # y"}, "present", map[string]parsedfile.Value{"comment": str("x # y"), "target": str(defaultTarget)}},
		{"target set", "h", map[string]any{"target": "/tmp/newhosts"}, "present", map[string]parsedfile.Value{"target": str("/tmp/newhosts")}},
		{"ensure absent only", "h", map[string]any{"ensure": "absent"}, "absent", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"ensure absent with ip", "h", map[string]any{"ensure": "absent", "ip": "::1"}, "absent", map[string]parsedfile.Value{"ip": str("::1"), "target": str(defaultTarget)}},
		{"ip first of list", "h", map[string]any{"ip": []any{"1.2.3.4", "::1"}}, "present", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "target": str(defaultTarget)}},
		{"ip empty list unset", "h", map[string]any{"ip": []any{}}, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"ip nil unset", "h", map[string]any{"ip": nil}, "", map[string]parsedfile.Value{"target": str(defaultTarget)}},
		{"name param", "title", map[string]any{"name": "other.example", "ip": "::1"}, "present", map[string]parsedfile.Value{"ip": str("::1"), "target": str(defaultTarget)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseHost(hostRes(tc.title, tc.params), "UTF-8")
			if err != nil {
				t.Fatal(err)
			}
			if p.Ensure != tc.wantEnsure {
				t.Errorf("Ensure = %q, want %q", p.Ensure, tc.wantEnsure)
			}
			if !reflect.DeepEqual(p.Should, tc.want) {
				t.Errorf("Should = %#v, want %#v", p.Should, tc.want)
			}
			if p.Title != tc.title {
				t.Errorf("Title = %q, want %q", p.Title, tc.title)
			}
		})
	}
}

func TestParseHostName(t *testing.T) {
	p, err := parseHost(hostRes("title", map[string]any{"name": "other.example"}), "UTF-8")
	if err != nil || p.Name != "other.example" || p.Title != "title" {
		t.Fatalf("Name = %q, Title = %q, err = %v", p.Name, p.Title, err)
	}
	p, err = parseHost(hostRes("title", nil), "UTF-8")
	if err != nil || p.Name != "title" {
		t.Fatalf("Name = %q, err = %v", p.Name, err)
	}
}

func TestParseHostEncodingQuotesRefusal(t *testing.T) {
	r := hostRes("h", map[string]any{"ip": "é"})
	_, err := parseHost(r, "UTF-8")
	if want := `Parameter ip failed on Host[h]: Invalid IP address "é"`; err == nil || err.Error() != want {
		t.Errorf("UTF-8 err = %v, want %q", err, want)
	}
	_, err = parseHost(r, "US-ASCII")
	if want := `Parameter ip failed on Host[h]: Invalid IP address "\u00E9"`; err == nil || err.Error() != want {
		t.Errorf("US-ASCII err = %v, want %q", err, want)
	}
}

// A non-String where Ruby's validate calls a String method is delegated
// (spec §2 condition 6), at its place in allattrs order.
func TestParseHostUnimplementedValues(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"ip integer", map[string]any{"ip": float64(1)}, "ip"},
		{"ip true", map[string]any{"ip": true}, "ip"},
		{"ip hash", map[string]any{"ip": map[string]any{"a": "b"}}, "ip"},
		{"ip nil element", map[string]any{"ip": []any{nil}}, "ip"},
		{"ip array element", map[string]any{"ip": []any{[]any{"x"}}}, "ip"},
		{"alias integer", map[string]any{"host_aliases": float64(5)}, "host_aliases"},
		{"alias element nil", map[string]any{"host_aliases": []any{"a", nil}}, "host_aliases"},
		{"comment integer", map[string]any{"comment": float64(5)}, "comment"},
		{"comment bool", map[string]any{"comment": false}, "comment"},
		{"target integer", map[string]any{"target": float64(5)}, "target"},
		{"name integer", map[string]any{"name": float64(5)}, "name"},
		{"name array", map[string]any{"name": []any{"a"}}, "name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := hostRes("h", tc.params)
			_, err := parseHost(r, "UTF-8")
			var u *rubyvalue.UnimplementedValue
			if !errors.As(err, &u) {
				t.Fatalf("err = %v, want *rubyvalue.UnimplementedValue", err)
			}
			if u.Ref != "Host[h]" || u.Attr != tc.attr {
				t.Errorf("Ref/Attr = %q/%q, want Host[h]/%s", u.Ref, u.Attr, tc.attr)
			}
			if err := (Host{encoding: "UTF-8"}).Validate(r); !errors.As(err, &u) {
				t.Errorf("Validate err = %v, want *rubyvalue.UnimplementedValue", err)
			}
		})
	}
}

// An earlier refusal wins over a later delegated value and the reverse.
func TestParseHostUnimplementedOrder(t *testing.T) {
	_, err := parseHost(hostRes("h", map[string]any{"ip": "bogus", "comment": float64(1)}), "UTF-8")
	var u *rubyvalue.UnimplementedValue
	if errors.As(err, &u) {
		t.Errorf("ip refusal must precede comment's delegation, got %v", err)
	}
	_, err = parseHost(hostRes("h", map[string]any{"ip": float64(1), "comment": "a\nb"}), "UTF-8")
	if !errors.As(err, &u) || u.Attr != "ip" {
		t.Errorf("ip delegation must precede comment's refusal, got %v", err)
	}
}

// SIMP's Host carries ip as a Binary rich value (M14); catalog.ResolveRich
// turns an ASCII one into its String before the unit sees it.
func TestParseHostBinaryIP(t *testing.T) {
	resolved, err := catalog.ResolveRich(map[string]any{"__ptype": "Binary", "__pvalue": "MTcyLjE2LjIuMTE="})
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseHost(hostRes("simp.example.com", map[string]any{"ip": resolved}), "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Should["ip"]; !reflect.DeepEqual(got, str("172.16.2.11")) {
		t.Errorf("ip = %#v, want 172.16.2.11", got)
	}
}

// A Binary that is not ASCII stays a rich map, which is delegated.
func TestParseHostNonASCIIBinaryIPDelegated(t *testing.T) {
	resolved, err := catalog.ResolveRich(map[string]any{"__ptype": "Binary", "__pvalue": "/w=="})
	if err != nil {
		t.Fatal(err)
	}
	_, err = parseHost(hostRes("h", map[string]any{"ip": resolved}), "UTF-8")
	var u *rubyvalue.UnimplementedValue
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want *rubyvalue.UnimplementedValue", err)
	}
}

func TestHostDeclaration(t *testing.T) {
	h := Host{}
	if h.Type() != "host" || *h.ProviderUsed() != "parsed" {
		t.Errorf("Type/ProviderUsed = %q/%q", h.Type(), *h.ProviderUsed())
	}
	if want := []string{"ensure", "ip", "host_aliases", "comment", "target"}; !reflect.DeepEqual(hostProperties, want) {
		t.Errorf("hostProperties = %v, want %v", hostProperties, want)
	}
	for _, a := range []string{"name", "provider", "ensure", "ip", "host_aliases", "comment", "target"} {
		if _, ok := h.Attrs()[a]; !ok {
			t.Errorf("Attrs lacks %s", a)
		}
	}
	if _, ok := h.Attrs()["bogus"]; ok {
		t.Error("Attrs holds bogus")
	}
	if _, ok := h.Properties()["name"]; ok {
		t.Error("Properties holds name")
	}
	if refresh, err := h.Refresh(t.Context(), hostRes("h", nil), true); refresh || err != nil {
		t.Errorf("Refresh = %v, %v", refresh, err)
	}
	if got := h.AutoRequires(hostRes("h", map[string]any{"target": "/etc/hosts"}), func(string) bool { return true }); got != nil {
		t.Errorf("AutoRequires = %v", got)
	}
}
