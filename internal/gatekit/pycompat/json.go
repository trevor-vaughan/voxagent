package pycompat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DecodeFile reads and decodes one JSON document the way the gate scripts'
// json.load did, apart from the refusals Decode documents.
func DecodeFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// Decode parses one JSON document into map[string]any, []any, string,
// json.Number, bool and nil. Numbers stay json.Number so that integers keep
// the exactness Python's int gives them; Equal compares them as Python would.
//
// It refuses four inputs Python's json.load accepted or crashed on, none of
// which a report can contain:
//
//   - bytes that are not UTF-8. Python raised UnicodeDecodeError, which the
//     scripts did not catch, so the traceback exited 1 -- read by every caller
//     as a finding. encoding/json would instead substitute U+FFFD silently.
//   - the literals NaN, Infinity and -Infinity, which encoding/json rejects.
//   - a \u escape for a surrogate code unit that is not half of a high-then-low
//     pair. Python keeps the lone surrogate; encoding/json turns every one into
//     U+FFFD, so two different lone surrogates would compare equal.
//   - an integer literal of more than 4300 digits. CPython 3.12 raises
//     ValueError, which the scripts did not catch (traceback, exit 1).
//
// Trailing data and a byte order mark are refused by both.
func Decode(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("not valid UTF-8")
	}
	if err := refuseUnrepresentable(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("extra data after the JSON document")
	}
	return v, nil
}

// maxIntDigits is CPython's default sys.int_info.default_max_str_digits.
const maxIntDigits = 4300

// refuseUnrepresentable scans a UTF-8 document for the lone-surrogate escapes
// and over-long integers Decode refuses. It tracks only enough JSON syntax to
// tell string contents from the rest; malformed documents are left to
// encoding/json.
func refuseUnrepresentable(data []byte) error {
	for i := 0; i < len(data); {
		switch c := data[i]; {
		case c == '"':
			end, err := scanString(data, i+1)
			if err != nil {
				return err
			}
			i = end
		case c == '-' || (c >= '0' && c <= '9'):
			start := i
			for i < len(data) && strings.IndexByte("+-.eE0123456789", data[i]) >= 0 {
				i++
			}
			lit := data[start:i]
			if bytes.IndexAny(lit, ".eE") < 0 && len(bytes.TrimPrefix(lit, []byte("-"))) > maxIntDigits {
				return fmt.Errorf("integer literal of more than %d digits, which Python refuses", maxIntDigits)
			}
		default:
			i++
		}
	}
	return nil
}

// scanString walks the string whose contents start at data[i] and returns the
// index just past its closing quote.
func scanString(data []byte, i int) (int, error) {
	for i < len(data) {
		switch data[i] {
		case '"':
			return i + 1, nil
		case '\\':
			if i+1 < len(data) && data[i+1] == 'u' {
				unit, ok := hexUnit(data, i+2)
				if !ok {
					return i + 2, nil
				}
				i += 6
				switch {
				case unit >= 0xDC00 && unit <= 0xDFFF:
					return 0, errors.New(`lone low-surrogate \u escape, which cannot appear in a report`)
				case unit >= 0xD800 && unit <= 0xDBFF:
					low, ok := uint16(0), false
					if i+1 < len(data) && data[i] == '\\' && data[i+1] == 'u' {
						low, ok = hexUnit(data, i+2)
					}
					if !ok || low < 0xDC00 || low > 0xDFFF {
						return 0, errors.New(`lone high-surrogate \u escape, which cannot appear in a report`)
					}
					i += 6
				}
				continue
			}
			i += 2
		default:
			i++
		}
	}
	return i, nil
}

// hexUnit reads four hex digits at data[i:].
func hexUnit(data []byte, i int) (uint16, bool) {
	if i+4 > len(data) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[i:i+4]), 16, 16)
	return uint16(n), err == nil
}

// pyNumber is a JSON number or bool as Python holds it: bool is an int, an
// integer literal is an exact int, and anything with a fraction or exponent is
// a float.
type pyNumber struct {
	isFloat bool
	i       *big.Int
	f       float64
}

func asNumber(v any) (pyNumber, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return pyNumber{i: big.NewInt(1)}, true
		}
		return pyNumber{i: big.NewInt(0)}, true
	case json.Number:
		s := string(x)
		if strings.ContainsAny(s, ".eE") {
			// ParseFloat rounds correctly, as Python's float() does, and on
			// overflow returns ±Inf with ErrRange -- Python's answer too.
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return pyNumber{}, false
			}
			return pyNumber{isFloat: true, f: f}, true
		}
		i, ok := new(big.Int).SetString(s, 10)
		return pyNumber{i: i}, ok
	}
	return pyNumber{}, false
}

func (a pyNumber) equal(b pyNumber) bool {
	switch {
	case !a.isFloat && !b.isFloat:
		return a.i.Cmp(b.i) == 0
	case a.isFloat && b.isFloat:
		return a.f == b.f
	case a.isFloat:
		a, b = b, a
	}
	// a is an int, b a float. Python compares these exactly, so
	// 9007199254740993 != 9007199254740993.0, which float64 cannot tell apart.
	if math.IsInf(b.f, 0) || math.IsNaN(b.f) {
		return false
	}
	return new(big.Rat).SetInt(a.i).Cmp(new(big.Rat).SetFloat64(b.f)) == 0
}

// Equal is Python's == over decoded JSON values. It inherits Python's numeric
// tower: true == 1 == 1.0, and false == 0. That is what the checkers that
// reproduce Python's verdicts need; the report comparator, which must not let
// `true` match `1`, applies its own bool rule on top (reportcmp.leavesEqual).
func Equal(a, b any) bool {
	if na, ok := asNumber(a); ok {
		nb, ok := asNumber(b)
		return ok && na.equal(nb)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// Truthy is Python's bool() over a decoded JSON value.
func Truthy(v any) bool {
	if n, ok := asNumber(v); ok {
		if n.isFloat {
			return n.f != 0
		}
		return n.i.Sign() != 0
	}
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Len is Python's len() over a decoded JSON value: characters for a string,
// elements for a list, keys for an object. Python raises TypeError for a
// number, a bool or null, and so does this.
func Len(v any) (int, error) {
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x), nil
	case []any:
		return len(x), nil
	case map[string]any:
		return len(x), nil
	}
	return 0, fmt.Errorf("len() of %s, which Python refuses", Shape(v))
}

// Shape names the kind of a decoded JSON value, for refusal messages that must
// say what was refused without echoing a whole report to stderr.
func Shape(v any) string {
	switch v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a bool"
	}
	return "null"
}

// Render shows a decoded JSON value in gate output. Strings are Go-quoted;
// everything else is written as its JSON literal, numbers exactly as the
// document spelled them. Object keys are sorted, because the decoded map has
// lost the document's order.
func Render(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return string(x)
	case string:
		return strconv.Quote(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Render(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + Render(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprintf("%v", v)
}
