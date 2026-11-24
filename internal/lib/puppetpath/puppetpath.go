// Package puppetpath holds the path and file-mode rules Puppet applies to the
// File and Concat types: clean absolute paths, O_PATH, and the mode attribute.
package puppetpath

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// Go's ^ and $ are text anchors, Ruby's are line anchors: Puppet accepts
// "u+x\nj" where this rejects it. Controller ruling 2 keeps the stricter form.
var (
	octalModeRe    = regexp.MustCompile(`^0?[0-7]{1,4}$`)
	symbolicModeRe = regexp.MustCompile(`^([ugoa]*[-=+][-=+rstwxXugo]*)(,[ugoa]*[-=+][-=+rstwxXugo]*)*$`)
)

// DeclaredMode is File's mode as Puppet validates and computes it
// (type/file/mode.rb:78-190, util/symbolic_file_mode.rb:19-153).
type DeclaredMode struct {
	Raw    string // as written
	Octal  bool
	value  uint32 // the octal mode, when octal
	redact bool   // errors show the mode as [redacted]
}

// ParseDeclaredMode validates a mode attribute. "000755" is invalid by
// util/symbolic_file_mode.rb:19-20 (read from the source, not measured).
//
// redact shows the mode as [redacted] in errors, for a marked mode (D-78:
// Ruby leaks it).
func ParseDeclaredMode(raw any, redact bool) (DeclaredMode, error) {
	s, ok := raw.(string)
	if !ok {
		return DeclaredMode{}, fmt.Errorf("The file mode specification must be a string, not '%s'", catalog.RubyClassName(raw))
	}
	if octalModeRe.MatchString(s) {
		v, err := strconv.ParseUint(s, 8, 32)
		if err != nil {
			return DeclaredMode{}, err
		}
		return DeclaredMode{Raw: s, Octal: true, value: uint32(v), redact: redact}, nil
	}
	if !symbolicModeRe.MatchString(s) {
		return DeclaredMode{}, fmt.Errorf("The file mode specification is invalid: %s", rubyvalue.RedactIf(redact, "%s", rubyvalue.Inspect(s)))
	}
	return DeclaredMode{Raw: s, redact: redact}, nil
}

// Apply is symbolic_mode_to_int: the mode this declaration gives a path
// whose current mode is current (low 12 bits). An octal mode ignores current.
func (m DeclaredMode) Apply(current uint32, isDir bool) (uint32, error) {
	if m.Octal {
		return m.value, nil
	}
	// Digits are indexed by who: 'u', 'g', 'o'; special bits live in sticky.
	final := map[byte]uint32{'u': current >> 6 & 7, 'g': current >> 3 & 7, 'o': current & 7}
	special := current >> 9 & 7
	anyX := current&0o111 != 0 // the original mode, never the one in progress
	clauses := strings.Split(m.Raw, ",")
	for _, clause := range clauses {
		whoEnd := strings.IndexAny(clause, "-=+")
		who := clause[:whoEnd]
		if who == "" || strings.Contains(who, "a") {
			who = "ugo"
		}
		snapshot := map[byte]uint32{'u': final['u'], 'g': final['g'], 'o': final['o']}
		for i := 0; i < len(who); i++ {
			w := who[i]
			value := snapshot[w]
			var action byte
			for j := whoEnd; j < len(clause); j++ {
				switch c := clause[j]; c {
				case '-', '+', '=':
					action = c
					if c == '=' {
						value = 0
					}
				case 'u', 'g', 'o':
					value = combineBits(value, snapshot[c], action)
				case 'r':
					value = combineBits(value, 4, action)
				case 'w':
					value = combineBits(value, 2, action)
				case 'x':
					value = combineBits(value, 1, action)
				case 'X':
					if action != '+' {
						return 0, m.xError(clause, len(clauses) > 1)
					}
					if isDir || anyX {
						value |= 1
					}
				case 's', 't':
					bit := uint32(1)
					if c == 's' {
						bit = map[byte]uint32{'u': 4, 'g': 2, 'o': 1}[w]
					}
					special = combineBits(special, bit, action)
				}
			}
			final[w] = value
		}
	}
	return special<<9 | final['u']<<6 | final['g']<<3 | final['o'], nil
}

// combineBits clears bits under '-' and sets them otherwise, which is how
// Puppet treats '+' and '=' once '=' has zeroed the value.
func combineBits(value, bits uint32, action byte) uint32 {
	if action == '-' {
		return value &^ bits
	}
	return value | bits
}

func (m DeclaredMode) xError(clause string, multiClause bool) error {
	msg := "X only works with the '+' operator"
	if multiClause {
		msg += " at " + rubyvalue.RedactIf(m.redact, "%s", rubyvalue.Inspect(clause))
	}
	return fmt.Errorf("%s in symbolic mode %s", msg, rubyvalue.RedactIf(m.redact, "%s", rubyvalue.Inspect(m.Raw)))
}

// Desired is the event's desired_value: octal normalised to four digits,
// symbolic as written.
func (m DeclaredMode) Desired() string {
	if m.Octal {
		return fmt.Sprintf("%04o", m.value)
	}
	return m.Raw
}

// FailureDisplay is should_to_s for a failed change: raw left-padded with
// '0' to four characters (mode.rb:178-180), so "u-X" renders "0u-X".
func (m DeclaredMode) FailureDisplay() string {
	if n := utf8.RuneCountInString(m.Raw); n < 4 {
		return strings.Repeat("0", 4-n) + m.Raw
	}
	return m.Raw
}

// Clean reports whether p, which starts with "/", is in the clean
// form §0 row 1 (e) lets native handle.
func Clean(p string) bool {
	if p == "/" || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// OPath is O_PATH (<bits/fcntl-linux.h>:92, __O_PATH 010000000), absent from
// Go's syscall package.
const OPath = 0x200000
