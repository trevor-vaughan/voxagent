package parsedfile

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRE2Captures: a group that did not participate is reported !OK, an empty
// capture that did is OK with "" (Ruby's nil versus "").
func TestRE2Captures(t *testing.T) {
	m := RE2(regexp.MustCompile(`^(\w+) ?(\w+)?(x)?$`))
	got, ok := m.Match("a b")
	want := []Capture{{"a", true}, {"b", true}, {"", false}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("Match(\"a b\") = %v, %v; want %v, true", got, ok, want)
	}

	m = RE2(regexp.MustCompile(`^(a*) (b*)$`))
	got, ok = m.Match(" ")
	want = []Capture{{"", true}, {"", true}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("Match(\" \") = %v, %v; want %v, true", got, ok, want)
	}

	if got, ok := m.Match("c"); ok || got != nil {
		t.Errorf("Match(\"c\") = %v, %v; want nil, false", got, ok)
	}
}

// TestSpaceClasses: Space is Ruby's \s (which includes \v, unlike RE2's).
func TestSpaceClasses(t *testing.T) {
	space := regexp.MustCompile(`^` + Space + `$`)
	notSpace := regexp.MustCompile(`^` + NotSpace + `$`)
	for _, r := range []string{" ", "\t", "\n", "\v", "\f", "\r"} {
		if !space.MatchString(r) || notSpace.MatchString(r) {
			t.Errorf("%q: Space %v, NotSpace %v; want true, false", r, space.MatchString(r), notSpace.MatchString(r))
		}
	}
	for _, r := range []string{"a", "\x00", "\u00a0", "\u0085", "\u2003"} {
		if space.MatchString(r) || !notSpace.MatchString(r) {
			t.Errorf("%q: Space %v, NotSpace %v; want false, true", r, space.MatchString(r), notSpace.MatchString(r))
		}
	}
}

// rubyWhitespaceEscape matches \s or \S that is a regexp escape (an even
// number of backslashes precede the one before the letter).
var rubyWhitespaceEscape = regexp.MustCompile(`(?:^|[^\\])(?:\\\\)*\\[sS]`)

// whitespaceEscapes returns the string literals in src that contain \s or \S
// as a regexp escape, wherever the literal is (a call argument, a const, a
// var). Ruby's \s includes \v, RE2's does not, so patterns use Space and
// NotSpace (spec 3.4).
func whitespaceEscapes(t *testing.T, filename string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatal(err)
		}
		if rubyWhitespaceEscape.MatchString(s) {
			bad = append(bad, lit.Value)
		}
		return true
	})
	return bad
}

func TestWhitespaceEscapeScanner(t *testing.T) {
	const src = "package p\n" +
		"var a = regexp.MustCompile(`^\\s+`)\n" +
		"var b = regexp.MustCompile(Space + `\\S`)\n" +
		"var c = regexp.MustCompile(\"[^\\\\s]\")\n" +
		"var d = regexp.MustCompile(`\\\\s`)\n" +
		"var e = regexp.MustCompile(`^\\w+` + Space)\n" +
		"const f = `\\s`\n" +
		"var g = other.Call(\"\\\\S\")\n"
	got := whitespaceEscapes(t, "x.go", src)
	want := []string{"`^\\s+`", "`\\S`", "\"[^\\\\s]\"", "`\\s`", "\"\\\\S\""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flagged %q, want %q", got, want)
	}
}

// TestNoRubyWhitespaceEscapes scans this package's non-test sources for string
// literals that use \s or \S. cron_core gets its own scan with its task.
func TestNoRubyWhitespaceEscapes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bad := whitespaceEscapes(t, name, src); len(bad) > 0 {
			t.Errorf("%s: literals use \\s or \\S (use Space or NotSpace): %q", name, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("no sources found")
	}
}
