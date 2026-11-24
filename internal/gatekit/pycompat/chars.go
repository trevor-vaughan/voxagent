// Package pycompat reproduces the handful of Python 3.12 behaviours the gate
// scripts were written against, so that porting them to Go does not quietly
// change a verdict. Each function names the Python behaviour it reproduces, and
// each is tested against a table CPython itself produced.
package pycompat

import (
	"strings"
	"unicode"
)

// otherDigits holds the code points str.isdigit() accepts beyond Unicode's Nd
// category: superscripts, circled and parenthesised digits, and the like
// (Numeric_Type=Digit). Measured by CPython 3.12.14 against Unicode 15.0.0,
// the same Unicode version as Go's unicode package; testdata/isdigit.json is
// the full measurement and the test checks every code point against it.
var otherDigits = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00B2, Hi: 0x00B3, Stride: 1},
		{Lo: 0x00B9, Hi: 0x00B9, Stride: 1},
		{Lo: 0x1369, Hi: 0x1371, Stride: 1},
		{Lo: 0x19DA, Hi: 0x19DA, Stride: 1},
		{Lo: 0x2070, Hi: 0x2070, Stride: 1},
		{Lo: 0x2074, Hi: 0x2079, Stride: 1},
		{Lo: 0x2080, Hi: 0x2089, Stride: 1},
		{Lo: 0x2460, Hi: 0x2468, Stride: 1},
		{Lo: 0x2474, Hi: 0x247C, Stride: 1},
		{Lo: 0x2488, Hi: 0x2490, Stride: 1},
		{Lo: 0x24EA, Hi: 0x24EA, Stride: 1},
		{Lo: 0x24F5, Hi: 0x24FD, Stride: 1},
		{Lo: 0x24FF, Hi: 0x24FF, Stride: 1},
		{Lo: 0x2776, Hi: 0x277E, Stride: 1},
		{Lo: 0x2780, Hi: 0x2788, Stride: 1},
		{Lo: 0x278A, Hi: 0x2792, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x10A40, Hi: 0x10A43, Stride: 1},
		{Lo: 0x10E60, Hi: 0x10E68, Stride: 1},
		{Lo: 0x11052, Hi: 0x1105A, Stride: 1},
		{Lo: 0x1F100, Hi: 0x1F10A, Stride: 1},
	},
}

func isDigitRune(r rune) bool {
	return unicode.IsDigit(r) || unicode.Is(otherDigits, r)
}

// IsDigit reports what Python's s.isdigit() would: true when s is non-empty
// and every character is a digit in Python's sense, which is wider than
// unicode.IsDigit.
func IsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isDigitRune(r) {
			return false
		}
	}
	return true
}

// isSpace is Python's str.isspace() for one character: Go's unicode.IsSpace
// plus U+001C..U+001F, the ASCII information separators, which Python counts
// as whitespace because their bidirectional class is B or S.
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1C && r <= 0x1F)
}

// Strip reproduces Python's s.strip() with no argument.
func Strip(s string) string {
	return strings.TrimFunc(s, isSpace)
}
