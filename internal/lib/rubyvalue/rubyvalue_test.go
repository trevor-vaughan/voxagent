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
