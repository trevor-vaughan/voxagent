// Package rubyregexp holds the pattern fragments Go regexp (RE2) needs to
// match as Ruby's Regexp does, for ports of Ruby code that match text.
// Package rubyregexptest checks that a package's patterns use them.
package rubyregexp

// Space and NotSpace are Ruby's \s and \S (ASCII whitespace including \v);
// RE2's \s lacks \v, so patterns written for Ruby use these instead.
const (
	Space    = `[\t\n\v\f\r ]`
	NotSpace = `[^\t\n\v\f\r ]`
)
