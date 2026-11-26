package rubyregexp

import (
	"regexp"
	"testing"
)

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
