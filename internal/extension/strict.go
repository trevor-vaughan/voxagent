package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"unicode/utf8"
)

// MaxItems bounds the elements of every JSON array and the members of every
// JSON object in a host message. The frame limit already bounds the bytes; this
// bounds the count, and is enforced on the raw bytes before encoding/json
// decodes anything, so a 64 MiB frame of empty objects cannot become tens of
// millions of decoded values. It is a functional limit too: a call that logs
// more than MaxItems lines is a protocol error and kills the host.
const MaxItems = 1 << 16

// MaxDepth bounds how deeply arrays and objects nest in a host message. The
// deepest legitimate message is a property value inside an event inside a
// reply; 64 levels leaves room for nested property values and keeps the
// pre-scan's counter stack a fixed size.
const MaxDepth = 64

// decodeStrict decodes one JSON value from raw into v, refusing anything the
// protocol does not allow:
//   - bytes that are not valid UTF-8;
//   - a \u escape that encodes a lone UTF-16 surrogate. encoding/json would
//     silently decode it to U+FFFD, smuggling a replacement character past the
//     UTF-8 rule;
//   - an array or object with more than MaxItems elements, or nesting deeper
//     than MaxDepth, refused by prescan before anything is decoded;
//   - a field v does not declare;
//   - anything after the value.
//
// Numbers decode as json.Number, so an integer the host reports reaches the
// report verbatim rather than through float64.
//
// encoding/json matches field names case-insensitively, so "Failed" fills
// `json:"failed"`. Ruby's JSON never emits a key in another case, and a
// case-variant key cannot carry a value the declared field could not, so this
// is recorded in docs/maintainers/extension-host.md rather than worked around.
func decodeStrict(raw []byte, v any) error {
	if !utf8.Valid(raw) {
		return errors.New("extension: message is not valid UTF-8")
	}
	if err := prescan(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("extension: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("extension: trailing data after the JSON value")
	}
	return nil
}

// prescan walks the raw bytes once, before encoding/json sees them, and
// refuses:
//   - a \u escape inside a JSON string that is a lone surrogate: a high one
//     not followed by an escaped low one, or a low one on its own;
//   - an array with more than MaxItems elements or an object with more than
//     MaxItems members;
//   - arrays and objects nested more than MaxDepth deep.
//
// It tracks string and escape state, so brackets, commas and quotes inside a
// string are text, and keeps one element counter per open container in a
// fixed-size stack, so it runs in O(n) and allocates nothing in proportion to
// the input. Elements are counted by the commas between them, plus one for a
// non-empty container. It is not a JSON validator: malformed input it lets
// through is encoding/json's to refuse.
func prescan(raw []byte) error {
	var counts [MaxDepth]int
	depth := 0
	opened := false // the innermost container has seen nothing but whitespace
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch c {
			case '"':
				inString = false
			case '\\':
				n, err := checkEscape(raw, i)
				if err != nil {
					return err
				}
				i += n
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case ']', '}':
			if depth > 0 {
				depth--
			}
			opened = false
			continue
		}
		if opened {
			counts[depth-1] = 1
			opened = false
		}
		switch c {
		case '"':
			inString = true
		case ',':
			if depth > 0 {
				counts[depth-1]++
				if counts[depth-1] > MaxItems {
					return fmt.Errorf("extension: an array or object has more than %d elements", MaxItems)
				}
			}
		case '[', '{':
			if depth == MaxDepth {
				return fmt.Errorf("extension: arrays and objects nested deeper than %d", MaxDepth)
			}
			counts[depth] = 0
			depth++
			opened = true
		}
	}
	return nil
}

// checkEscape checks the escape whose backslash is at raw[i] and returns how
// many bytes past the backslash it covers. Only a \u escape that is a lone
// surrogate is an error; a truncated or malformed escape is encoding/json's to
// report.
func checkEscape(raw []byte, i int) (int, error) {
	if i+1 >= len(raw) {
		return 0, nil
	}
	if raw[i+1] != 'u' {
		return 1, nil // the escaped character, which may be a quote or a backslash
	}
	r, ok := hex4(raw, i+2)
	if !ok {
		return 0, nil
	}
	switch {
	case r >= 0xDC00 && r <= 0xDFFF:
		return 0, fmt.Errorf("extension: lone low surrogate \\u%04x", r)
	case r >= 0xD800 && r <= 0xDBFF:
		j := i + 5
		low, ok := uint32(0), false
		if j+6 < len(raw) && raw[j+1] == '\\' && raw[j+2] == 'u' {
			low, ok = hex4(raw, j+3)
		}
		if !ok || low < 0xDC00 || low > 0xDFFF {
			return 0, fmt.Errorf("extension: lone high surrogate \\u%04x", r)
		}
		return 11, nil
	}
	return 5, nil
}

func hex4(raw []byte, at int) (uint32, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(raw[at:at+4]), 16, 32)
	return uint32(v), err == nil
}
