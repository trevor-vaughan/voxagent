package parsedfile

import (
	"reflect"
	"regexp"
	"testing"
)

// TestSplitRuby pins Split to Ruby's String#split with a Regexp. Every want
// was measured with ruby 3.4.9 ("str".split(/re/)).
func TestSplitRuby(t *testing.T) {
	for _, c := range []struct {
		name, s, re string
		want        []string
	}{
		{"leading empty kept", " a b", ` +`, []string{"", "a", "b"}},
		{"trailing empties dropped", "a b  ", ` +`, []string{"a", "b"}},
		{"empty string", "", ` +`, []string{}},
		{"only separators", "  ", ` +`, []string{}},
		{"inner empty kept", "a,,b", `,`, []string{"a", "", "b"}},
		{"lone separator", ",", `,`, []string{}},
		{"no separator", "ab", ` +`, []string{"ab"}},
		{"empty pattern splits characters", "abc", ``, []string{"a", "b", "c"}},
		{"empty pattern splits runes", "éa", ``, []string{"é", "a"}},
		{"possibly empty pattern", "abc", `x*`, []string{"a", "b", "c"}},
		{"spaces pattern", "a b", ` *`, []string{"a", "b"}},
		{"empty match then non-empty", "ab", `b*`, []string{"a"}},
		{"non-empty match at start", "aab", `a*`, []string{"", "b"}},
		{"pattern empty only at end", "ab", `$`, []string{"ab"}},
		{"capture group is kept", "a,b", `(,)`, []string{"a", ",", "b"}},
		{"trailing capture kept", "a,b,", `(,)`, []string{"a", ",", "b", ","}},
		{"unmatched group skipped", "a1b", `(\d)|(x)`, []string{"a", "1", "b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Split(c.s, regexp.MustCompile(c.re))
			if got == nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("Split(%q, /%s/) = %#v, want %#v", c.s, c.re, got, c.want)
			}
		})
	}
}
