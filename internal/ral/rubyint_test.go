package ral

import "testing"

func TestRubyInteger(t *testing.T) {
	ok := map[string]int64{
		"10": 10, "0010": 8, "0x10": 16, "0X1f": 31, "0b11": 3, "0o17": 15, "0d19": 19,
		"-5": -5, "+5": 5, " 7 ": 7, "\t7\n": 7, "1_000": 1000, "0": 0, "-0x10": -16, "0_7": 7,
	}
	for in, want := range ok {
		got, err := rubyInteger(in)
		if err != nil || got != want {
			t.Errorf("rubyInteger(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"09", "1-2", "abc", "", " ", "1__0", "_1", "1_", "0x", "0x_1", "- 5", "--5", "0b2", "99999999999999999999"} {
		_, err := rubyInteger(in)
		if want := `invalid value for Integer(): "` + in + `"`; err == nil || err.Error() != want {
			t.Errorf("rubyInteger(%q) error = %v; want %s", in, err, want)
		}
	}
}
