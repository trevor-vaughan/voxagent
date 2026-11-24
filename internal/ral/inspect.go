package ral

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rubyInspect is Ruby's String#inspect for a UTF-8 string (measured with
// ruby 3.4.9): double quotes; \" \\ \n \t \r \f \v \b \a \e; "#" escaped
// only before "{", "$" or "@"; Cc, Zl and Zp code points as \uXXXX
// (uppercase hex); invalid bytes as \xFF; every other code point raw ("é",
// U+00A0 and U+200B stay raw; U+0085 and U+2028 do not). Cc, Zl and Zp are
// all in the BMP, so the \u{X} form Ruby uses above U+FFFF never arises.
func rubyInspect(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02X`, s[i])
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '#':
			if next := s[i+1:]; next != "" && strings.ContainsRune("{$@", rune(next[0])) {
				b.WriteByte('\\')
			}
			b.WriteByte('#')
		case rubyEscapes[r] != "":
			b.WriteString(rubyEscapes[r])
		case unicode.In(r, unicode.Cc, unicode.Zl, unicode.Zp):
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}

var rubyEscapes = map[rune]string{
	'\n': `\n`, '\t': `\t`, '\r': `\r`, '\f': `\f`, '\v': `\v`, '\b': `\b`, '\a': `\a`, '\x1b': `\e`,
}

// rubyInspectValue is Array#inspect / Object#inspect for a decoded catalog
// value: strings via rubyInspect, integral float64 as an integer, other
// float64 via rubyFloat, bools, nil as "nil", []any as "[a, b]". Any other
// type (a Hash, which catalog values pass through only inside rich data) falls
// back to Go's %v (docs/maintainers/apply-spine.md D-53).
func rubyInspectValue(v any) string {
	switch v := v.(type) {
	case string:
		return rubyInspect(v)
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return strconv.FormatInt(int64(v), 10)
		}
		return rubyFloat(v)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "nil"
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = rubyInspectValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// rubyFloat is Float#to_s for a finite float64 (measured with ruby 3.4.9):
// the shortest round-trip digits in decimal notation when the decimal
// exponent is at least -4 ("0.0001", "1000000000000000.5"), otherwise
// "d.ddde+XX" with a ".0" fraction for a single digit and an exponent of at
// least two digits ("1.0e+20", "1.234e-05"). A whole number also switches to
// that form from 1e15 ("9.007199254740994e+15", measured); a fractional
// float64 is below 2^52, so that is the only upper limit that can arise.
func rubyFloat(f float64) string {
	e := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mantissa, expPart, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expPart)
	digits := strings.Replace(mantissa, ".", "", 1)
	switch {
	case exp < -4 || (exp >= 15 && f == math.Trunc(f)):
		frac := digits[1:]
		if frac == "" {
			frac = "0"
		}
		expSign := "+"
		if exp < 0 {
			expSign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%s.%se%s%02d", sign, digits[:1], frac, expSign, exp)
	case exp < 0:
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}
