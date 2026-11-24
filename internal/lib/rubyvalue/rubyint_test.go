package rubyvalue

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestRubyInteger(t *testing.T) {
	ok := map[string]int64{
		"10": 10, "0010": 8, "0x10": 16, "0X1f": 31, "0b11": 3, "0o17": 15, "0d19": 19,
		"-5": -5, "+5": 5, " 7 ": 7, "\t7\n": 7, "1_000": 1000, "0": 0, "-0x10": -16, "0_7": 7,
		"0d9": 9, "0D9": 9, "1_2_3": 123, "\v1": 1, " 0x1f ": 31, " -1": -1, "+0d1_2": 12, "0_1": 1, "07": 7,
		"0o7": 7, "0b1": 1, "0x1f": 31,
	}
	for in, want := range ok {
		got, err := Integer(in)
		if err != nil || got != want {
			t.Errorf("Integer(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"09", "1-2", "abc", "", " ", "1__0", "_1", "1_", "0x", "0x_1", "- 5", "--5", "0b2", "08", "0d", "0d_1", "+-1", "１", "1e3", "12abc"} {
		_, err := Integer(in)
		if want := `invalid value for Integer(): "` + in + `"`; err == nil || err.Error() != want {
			t.Errorf("Integer(%q) error = %v; want %s", in, err, want)
		}
	}
}

// An in-syntax literal beyond int64 is a Ruby Bignum, so it is a range error
// rather than a syntax error.
func TestRubyIntegerRange(t *testing.T) {
	for _, in := range []string{"0x1_0000_0000_0000_0000", "99999999999999999999", "-99999999999999999999"} {
		if _, err := Integer(in); !errors.Is(err, strconv.ErrRange) {
			t.Errorf("Integer(%q) error = %v; want ErrRange", in, err)
		}
	}
	for _, in := range []string{"0x1_0000_0000_0000_0000_", "0x_1", "0xg", "99999999999999999999z", "0b" + strings.Repeat("1", 70) + "2"} {
		if _, err := Integer(in); err == nil || errors.Is(err, strconv.ErrRange) {
			t.Errorf("Integer(%q) error = %v; want a syntax error", in, err)
		}
	}
}

func TestToI(t *testing.T) {
	for in, want := range map[string]int64{
		"0x1f": 0, "0b1": 0, "0o7": 0, "0d9": 9, "0D9": 9, "1_000": 1000, "1_2_3": 123,
		" 1": 1, "\v1": 1, " 0x1f ": 0, "-1": -1, " -1": -1, "+5": 5, "+0d1_2": 12, "0_1": 1,
		"07": 7, "08": 8, "1__0": 1, "1_": 1, "_1": 0, "0x": 0, "0d": 0, "0d_1": 0, "+-1": 0,
		"- 1": 0, "１": 0, "1e3": 1, "12abc": 12, "0x1_0000_0000_0000_0000": 0, "": 0,
		"99999999999999999999": math.MaxInt64, "-99999999999999999999": math.MinInt64,
		"9223372036854775807": math.MaxInt64, "-9223372036854775808": math.MinInt64,
	} {
		if got := ToI(in); got != want {
			t.Errorf("ToI(%q) = %d; want %d", in, got, want)
		}
	}
}
