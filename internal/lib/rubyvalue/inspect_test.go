package rubyvalue

import (
	"testing"
)

// Expected values were measured with ruby 3.4.9, e.g.
//
//	ruby -e 'p "a\"b\\c\n\t\r\e\f\v\b\a"'
//	ruby -e 'p "\#{x} \#$y \#@z #a"'
//	ruby -e 'p "\u0001\u007f\u0085\u00a0\u200b\u2028"'
//	ruby -e 'p "\xff".force_encoding("UTF-8")'
func TestRubyInspect(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"\x01", `"\u0001"`},
		{"\x00", `"\u0000"`},
		// D-53: Ruby escapes this (ruby -e 'p "\u{FFFF}"' prints "\uFFFF"); voxagent keeps it raw.
		{"\uffff", "\"\uffff\""},
		{"\x7f", `"\u007F"`},
		{"a\"b\\c\n\t\r\x1b\f\v\b\a", `"a\"b\\c\n\t\r\e\f\v\b\a"`},
		{"#{x} #$y #@z #a", `"\#{x} \#$y \#@z #a"`},
		{"trailing#", `"trailing#"`},
		{"é", `"é"`},
		{"\u00a0\u200b", "\"\u00a0\u200b\""},
		{"\u0085", `"\u0085"`},
		{"\u2028", `"\u2028"`},
		{"\u2029", `"\u2029"`},
		{"\xff", `"\xFF"`},
		{"", `""`},
	} {
		if got := Inspect(tt.in); got != tt.want {
			t.Errorf("Inspect(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestRubyInspectValue(t *testing.T) {
	for _, tt := range []struct {
		in   any
		want string
	}{
		{[]any{"/bin/sh", []any{"-c", float64(1), true, nil}}, `["/bin/sh", ["-c", 1, true, nil]]`},
		// ruby -e 'p 5.5' ; 'p 1234567.5' ; 'p 123456789.123' ; 'p 1e-05' ; 'p 1e+20'
		// ruby -e 'p 999999999999999.5' ; 'p 1000000000000000.5' ; 'p 1234567890123456.5' ; 'p 9007199254740994.0' ; 'p 0.0001' ; 'p 9.999e-05' ; 'p -2.5e-07' ; 'p 123456789012345.6'
		{float64(5.5), "5.5"}, {float64(1234567.5), "1234567.5"}, {123456789.123, "123456789.123"},
		{1e-05, "1.0e-05"}, {1e+20, "1.0e+20"}, {999999999999999.5, "999999999999999.5"},
		{123456789012345.6, "123456789012345.6"}, {1000000000000000.5, "1000000000000000.5"}, {1234567890123456.5, "1234567890123456.5"}, {9007199254740994.0, "9.007199254740994e+15"}, {0.0001, "0.0001"},
		{9.999e-05, "9.999e-05"}, {-2.5e-07, "-2.5e-07"}, {1.5e300, "1.5e+300"},
		{float64(-3), "-3"},
		{false, "false"},
		{[]any{}, "[]"},
		{nil, "nil"},
	} {
		if got := InspectValue(tt.in); got != tt.want {
			t.Errorf("InspectValue(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
