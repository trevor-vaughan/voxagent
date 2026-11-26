package mountcore

import (
	"testing"

	"github.com/voxagent/voxagent/internal/lib/rubyregexp/rubyregexptest"
)

// TestNoRubyWhitespaceEscapes: Ruby's \s includes \v, RE2's does not, so this
// unit's patterns spell the class out with rubyregexp.Space (spec 4.5).
func TestNoRubyWhitespaceEscapes(t *testing.T) {
	rubyregexptest.NoRubyWhitespaceEscapes(t)
}
