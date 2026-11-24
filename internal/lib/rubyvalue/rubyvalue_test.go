package rubyvalue

import (
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// error_location's branches, including the empty one. voxagent previously
// rendered "(file: , line: 0)" for a resource with no source metadata, which is
// a real input: a catalog resource carries file/line only when the compiler
// knew them.
//
// util/errors.rb error_location treats "" as not-known and returns an empty
// string when neither is known, and error_location_with_space adds the space only
// when the result is non-empty -- so the space belongs to the suffix, not to
// the caller's format string.
func TestErrorLocationBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		line int
		want string
	}{
		{"file and line", "/etc/site.pp", 6, " (file: /etc/site.pp, line: 6)"},
		{"line only", "", 6, " (line: 6)"},
		{"file only", "/etc/site.pp", 0, " (file: /etc/site.pp)"},
		{"neither", "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorLocation(tc.file, tc.line); got != tc.want {
				t.Errorf("ErrorLocation(%q, %d) = %q, want %q", tc.file, tc.line, got, tc.want)
			}
		})
	}
}

func TestRubyBool(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want bool
	}{{true, true}, {false, false}, {"true", true}, {"Yes", true}, {"FALSE", false}, {"no", false}} {
		if got, err := Bool(tc.in); err != nil || got != tc.want {
			t.Errorf("Bool(%#v) = %v, %v", tc.in, got, err)
		}
	}
	for _, in := range []any{"maybe", "", "y", 1.0, nil, []any{"true"}} {
		if _, err := Bool(in); errText(err) != "expected a boolean value" {
			t.Errorf("Bool(%#v) err = %v", in, err)
		}
	}
}

func TestPuppetQuote(t *testing.T) {
	for in, want := range map[string]string{
		"new, comment": "'new, comment'",
		"O'Brien":      `'O\'Brien'`,
		`a\b`:          `'a\b'`,
		`a\`:           `'a\'`,
		"":             "''",
		"tab\there":    `"tab\there"`,
		"x$y\x01":      `"x\$y\u{1}"`,
		"é":            "'é'",
	} {
		if got := PuppetQuote(in); got != want {
			t.Errorf("PuppetQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestStrip(t *testing.T) {
	for in, want := range map[string]string{"\x00 a\v\x00": "a", " \t\n\v\f\ra b\r": "a b", " a ": " a "} {
		if got := Strip(in); got != want {
			t.Errorf("strip(%q) = %q, want %q", in, got, want)
		}
	}
}

// D-78: Integer's error repeats the value, so a Sensitive name redacts it in
// the detail as well as in the munge wrapper.
func TestMungeIntegerFailureRedactsSensitiveDetail(t *testing.T) {
	_, err := Integer("09")
	for _, tc := range []struct {
		name      string
		sensitive []string
		want      string
	}{
		{"unmarked", nil, `Parameter gid failed on Group[g]: Munging failed for value "09" in class gid: invalid value for Integer(): "09"`},
		{"marked", []string{"gid"}, "Parameter gid failed on Group[g]: Munging failed for value [redacted] in class gid: invalid value for Integer(): [redacted]"},
		{"another name marked", []string{"members"}, `Parameter gid failed on Group[g]: Munging failed for value "09" in class gid: invalid value for Integer(): "09"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := catalog.Resource{Type: "Group", Title: "g", SensitiveParameters: tc.sensitive}
			if got := errText(MungeIntegerFailure(r, "gid", "09", err)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInspectIn(t *testing.T) {
	for _, tc := range []struct{ name, in, enc, want string }{
		{"fullwidth digit, US-ASCII", "１", "US-ASCII", `"\uFF11"`},
		{"fullwidth digit, UTF-8", "１", "UTF-8", Inspect("１")},
		{"latin letter", "é", "US-ASCII", `"\u00E9"`},
		{"astral", "😀", "US-ASCII", `"\u{1F600}"`},
		{"mixed", "a\"é\n", "US-ASCII", `"a\"\u00E9\n"`},
		{"ASCII, US-ASCII", "a\"b#{c}\x01", "US-ASCII", Inspect("a\"b#{c}\x01")},
		{"ASCII, UTF-8", "a\"b#{c}\x01", "UTF-8", Inspect("a\"b#{c}\x01")},
		{"invalid byte, US-ASCII", "\xff", "US-ASCII", `"\xFF"`},
	} {
		if got := InspectIn(tc.in, tc.enc); got != tc.want {
			t.Errorf("%s: InspectIn(%q, %q) = %s; want %s", tc.name, tc.in, tc.enc, got, tc.want)
		}
	}
}

// Encoding.default_external: the codeset of the first non-empty of LC_ALL,
// LC_CTYPE, LANG; UTF-8 only for a UTF-8 codeset, US-ASCII otherwise (D-79).
func TestExternalEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none set", nil, "US-ASCII"},
		{"C", map[string]string{"LANG": "C"}, "US-ASCII"},
		{"POSIX", map[string]string{"LANG": "POSIX"}, "US-ASCII"},
		{"C.UTF-8", map[string]string{"LANG": "C.UTF-8"}, "UTF-8"},
		{"utf8 spelling", map[string]string{"LANG": "en_US.utf8"}, "UTF-8"},
		{"other codeset", map[string]string{"LANG": "en_US.ISO-8859-1"}, "US-ASCII"},
		{"modifier", map[string]string{"LANG": "de_DE.UTF-8@euro"}, "UTF-8"},
		{"LC_ALL beats LC_CTYPE and LANG", map[string]string{"LC_ALL": "C", "LC_CTYPE": "C.UTF-8", "LANG": "C.UTF-8"}, "US-ASCII"},
		{"LC_CTYPE beats LANG", map[string]string{"LC_CTYPE": "C.UTF-8", "LANG": "C"}, "UTF-8"},
		{"empty skipped", map[string]string{"LC_ALL": "", "LC_CTYPE": "", "LANG": "C.UTF-8"}, "UTF-8"},
	} {
		if got := ExternalEncoding(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("%s: ExternalEncoding = %q, want %q", tc.name, got, tc.want)
		}
	}
}
