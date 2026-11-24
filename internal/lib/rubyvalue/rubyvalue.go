// Package rubyvalue renders and coerces values as Ruby and Puppet do: inspect
// text, Integer()/Float() parsing, parameter access and failure wording.
package rubyvalue

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Inspect is Ruby's String#inspect for a UTF-8 string (measured with
// ruby 3.4.9): double quotes; \" \\ \n \t \r \f \v \b \a \e; "#" escaped
// only before "{", "$" or "@"; Cc, Zl and Zp code points as \uXXXX
// (uppercase hex); invalid bytes as \xFF; every other code point raw ("é",
// U+00A0 and U+200B stay raw; U+0085 and U+2028 do not). Cc, Zl and Zp are
// all in the BMP, so the \u{X} form Ruby uses above U+FFFF never arises.
func Inspect(s string) string {
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

// InspectValue is Array#inspect / Object#inspect for a decoded catalog
// value: strings via Inspect, integral float64 as an integer, other
// float64 via Float, bools, nil as "nil", []any as "[a, b]". Any other
// type (a Hash, which catalog values pass through only inside rich data) falls
// back to Go's %v (docs/maintainers/apply-spine.md D-53).
func InspectValue(v any) string {
	switch v := v.(type) {
	case string:
		return Inspect(v)
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return strconv.FormatInt(int64(v), 10)
		}
		return Float(v)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "nil"
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			parts[i] = InspectValue(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// Float is Float#to_s for a finite float64 (measured with ruby 3.4.9):
// the shortest round-trip digits in decimal notation when the decimal
// exponent is at least -4 ("0.0001", "1000000000000000.5"), otherwise
// "d.ddde+XX" with a ".0" fraction for a single digit and an exponent of at
// least two digits ("1.0e+20", "1.234e-05"). A whole number also switches to
// that form from 1e15 ("9.007199254740994e+15", measured); a fractional
// float64 is below 2^52, so that is the only upper limit that can arise.
func Float(f float64) string {
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

// Integer is Kernel#Integer(String) (Ruby's rb_str_to_inum with badcheck):
// surrounding whitespace, an optional sign, a 0b/0o/0d/0x prefix (any case) or a
// bare leading 0 for octal, and single underscores between digits. Puppet
// relies on it in get_posix_value (util/posix.rb:174), so a group named "0x1f"
// is the number 31. A value that does not fit int64 is rejected with the same
// error; no id in this slice can be that large.
func Integer(s string) (int64, error) {
	invalid := fmt.Errorf("invalid value for Integer(): %q", s)
	body := strings.Trim(s, " \t\n\v\f\r")
	sign := ""
	if body != "" && (body[0] == '+' || body[0] == '-') {
		if body[0] == '-' {
			sign = "-"
		}
		body = body[1:]
	}
	base := 10
	if len(body) > 1 && body[0] == '0' {
		switch body[1] {
		case 'b', 'B':
			base, body = 2, body[2:]
		case 'o', 'O':
			base, body = 8, body[2:]
		case 'd', 'D':
			base, body = 10, body[2:]
		case 'x', 'X':
			base, body = 16, body[2:]
		default:
			base = 8
		}
	}
	if body == "" || body[0] == '_' || body[len(body)-1] == '_' || strings.Contains(body, "__") {
		return 0, invalid
	}
	digits := strings.ReplaceAll(body, "_", "")
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		return 0, invalid
	}
	n, err := strconv.ParseInt(sign+digits, base, 64)
	if err != nil {
		return 0, invalid
	}
	return n, nil
}

// Strip is String#strip: NUL and ASCII whitespace from both ends.
func Strip(s string) string { return strings.Trim(s, "\x00 \t\n\v\f\r") }

// UnimplementedValue is a value shape the native user or group parser does
// not implement, though Ruby accepts it. The user and group units send a type with one
// to the extension host (spec §0); Ruby's own validation failures are other
// errors and stay native. shown is the value as the refusal prints it.
type UnimplementedValue struct{ Ref, Attr, Shown string }

func (e *UnimplementedValue) Error() string {
	return fmt.Sprintf("%s: %s => %s is not implemented", e.Ref, e.Attr, e.Shown)
}

// Bool is Puppet::Parameter::Boolean's munge (Puppet::Coercion.boolean):
// true, false, or a string that downcases to true/yes/false/no.
func Bool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch strings.ToLower(t) {
		case "true", "yes":
			return true, nil
		case "false", "no":
			return false, nil
		}
	}
	return false, errors.New("expected a boolean value")
}

// PuppetQuote is StringConverter#puppet_quote (pops/types/string_converter.rb:
// 862-924), which format_value_for_display's %p applies to a String: single
// quotes, a quote escaped and a backslash kept with the character after it,
// unless the string has a control character, which double-quotes it with
// escapes instead.
func PuppetQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	escaped := false
	for _, c := range s {
		switch {
		case c < 0x20:
			return puppetDoubleQuote(s)
		case escaped:
			b.WriteByte('\\')
			b.WriteRune(c)
			escaped = false
		case c == '\'':
			b.WriteString(`\'`)
		case c == '\\':
			escaped = true
		default:
			b.WriteRune(c)
		}
	}
	if escaped {
		b.WriteByte('\\')
	}
	b.WriteByte('\'')
	return b.String()
}

func puppetDoubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '$':
			b.WriteString(`\$`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u{%X}`, c)
			} else {
				b.WriteRune(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// HoldsHash reports whether v is, or nests, a Hash, which InspectValue
// does not render as Ruby does.
func HoldsHash(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return true
	case []any:
		for _, e := range t {
			if HoldsHash(e) {
				return true
			}
		}
	}
	return false
}

// Rich refuses, as an unimplemented value, a resource carrying a rich
// value anywhere in its parameters (spec §0 row 1 (f)).
func Rich(r catalog.Resource) error {
	if rich := catalog.RichValues(r); len(rich) > 0 {
		return &UnimplementedValue{r.Ref(), rich[0].Param, rich[0].PType}
	}
	return nil
}

// param is the catalog value of a parameter, nil when absent or undef. A
// decoded integer is a float64 throughout, so a value has one shape.
func Param(r catalog.Resource, name string) any {
	switch v := r.Parameters[name].(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return v
	}
}

func ParamFailure(r catalog.Resource, name, detail string) error {
	return fmt.Errorf("Parameter %s failed on %s: %s", name, r.Ref(), detail)
}

// MungeFailure is Puppet::Parameter#munge's wrapper for a non-Puppet error
// (parameter.rb:449): the value as inspect shows it, then the error.
func MungeFailure(r catalog.Resource, name string, value any, detail string) error {
	return ParamFailure(r, name, fmt.Sprintf("Munging failed for value %s in class %s: %s", ShownValue(r, name, value), name, detail))
}

// ShownValue is a value as Ruby's inspect renders it, or [redacted] when the
// catalog marks the parameter Sensitive (D-78: Ruby leaks it).
func ShownValue(r catalog.Resource, name string, value any) string {
	return RedactIf(slices.Contains(r.SensitiveParameters, name), "%s", InspectValue(value))
}

// SplitPath is String#split(":") on the joined path, which keeps leading
// and inner empty entries and drops trailing ones.
func SplitPath(pathList string) []string {
	entries := strings.Split(pathList, ":")
	for len(entries) > 0 && entries[len(entries)-1] == "" {
		entries = entries[:len(entries)-1]
	}
	return entries
}

func UniqSorted(s []string) []string {
	out := slices.Clone(s)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SplitComma is Ruby's String#split(','): trailing empty fields are
// dropped, so "" splits to nothing and "a,,b," to a, "", b.
func SplitComma(s string) []string {
	parts := strings.Split(s, ",")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// RedactIf renders a value as format would, or as [redacted] when marked. The
// replacement takes the format's quotes with it: 'x' becomes [redacted].
func RedactIf(marked bool, format string, a ...any) string {
	if marked {
		return "[redacted]"
	}
	return fmt.Sprintf(format, a...)
}

func ParamString(r catalog.Resource, name, fallback string) string {
	if v, ok := r.Parameters[name]; ok {
		return fmt.Sprintf("%v", v)
	}
	return fallback
}

// ErrorLocation reproduces Puppet::Util::Errors.error_location's branches,
// including its empty result when neither file nor line is known.
//
// Read from util/errors.rb: an empty string counts as absent, so a catalog
// resource with no source metadata gets NO suffix -- not "(file: , line: 0)",
// which voxagent emitted. The leading space is error_location_with_space's,
// which error.rb:45 uses to compose an ExternalFileError's message and which
// adds the space only when the location itself is non-empty.
//
// The column branches are omitted: a resource status carries no column, so
// they are unreachable here.
func ErrorLocation(file string, line int) string {
	switch {
	case file != "" && line != 0:
		return fmt.Sprintf(" (file: %s, line: %d)", file, line)
	case line != 0:
		return fmt.Sprintf(" (line: %d)", line)
	case file != "":
		return fmt.Sprintf(" (file: %s)", file)
	default:
		return ""
	}
}
