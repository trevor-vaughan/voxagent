package parsedfile

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/rubyregexp/rubyregexptest"
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

// TestNoRubyWhitespaceEscapes scans this package's non-test sources for string
// literals that use \s or \S.
func TestNoRubyWhitespaceEscapes(t *testing.T) {
	rubyregexptest.NoRubyWhitespaceEscapes(t)
}
