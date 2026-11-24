package croncore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// rubyWhitespaceEscape matches \s or \S that is a regexp escape (an even
// number of backslashes precede the one before the letter).
var rubyWhitespaceEscape = regexp.MustCompile(`(?:^|[^\\])(?:\\\\)*\\[sS]`)

// TestNoRubyWhitespaceEscapes: Ruby's \s includes \v, RE2's does not, so this
// unit's patterns spell the class out with parsedfile.Space (spec 4.5). No
// string literal in a non-test source may use \s or \S.
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
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
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
				t.Errorf("%s: literal %s uses \\s or \\S (use parsedfile.Space)", name, lit.Value)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("no sources found")
	}
}
