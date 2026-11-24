package ral

import (
	"fmt"
	"strconv"
	"strings"
)

// rubyInteger is Kernel#Integer(String) (Ruby's rb_str_to_inum with badcheck):
// surrounding whitespace, an optional sign, a 0b/0o/0d/0x prefix (any case) or a
// bare leading 0 for octal, and single underscores between digits. Puppet
// relies on it in get_posix_value (util/posix.rb:174), so a group named "0x1f"
// is the number 31. A value that does not fit int64 is rejected with the same
// error; no id in this slice can be that large.
func rubyInteger(s string) (int64, error) {
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
