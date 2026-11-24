package sshkeyscore

import (
	"testing"

	"github.com/voxagent/voxagent/internal/lib/parsedfile/parsedfiletest"
)

// TestNoRubyWhitespaceEscapes: Ruby's \s includes \v, RE2's does not, so this
// unit's patterns spell the class out with parsedfile.Space (spec 4.5).
func TestNoRubyWhitespaceEscapes(t *testing.T) {
	parsedfiletest.NoRubyWhitespaceEscapes(t)
}
