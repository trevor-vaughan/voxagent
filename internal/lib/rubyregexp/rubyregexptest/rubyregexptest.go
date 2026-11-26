// Package rubyregexptest holds test support shared by the packages that use
// internal/lib/rubyregexp. It imports only the standard library.
package rubyregexptest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rubyWhitespaceEscape matches \s or \S that is a regexp escape (an even
// number of backslashes precede the one before the letter).
var rubyWhitespaceEscape = regexp.MustCompile(`(?:^|[^\\])(?:\\\\)*\\[sS]`)

// whitespaceEscapes returns the string literals in src that contain \s or \S
// as a regexp escape, wherever the literal is (a call argument, a const, a
// var). Ruby's \s includes \v, RE2's does not, so patterns use Space and
// NotSpace (spec 3.4).
func whitespaceEscapes(t testing.TB, filename string, src any) []string {
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

// NoRubyWhitespaceEscapes fails the test for every string literal in the
// non-test .go files of the calling test's package directory (the test's
// working directory) that uses \s or \S.
func NoRubyWhitespaceEscapes(t testing.TB) {
	t.Helper()
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
			t.Errorf("%s: literals use \\s or \\S (use rubyregexp.Space or rubyregexp.NotSpace): %q", name, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("no sources found")
	}
}
