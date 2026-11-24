package pycompat

import (
	"encoding/json"
	"os"
	"testing"
)

// readRanges loads a CPython-measured table of inclusive code point ranges.
func readRanges(t *testing.T, path string) map[rune]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Ranges [][2]rune `json:"ranges"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(table.Ranges) == 0 {
		t.Fatalf("%s holds no ranges", path)
	}
	set := map[rune]bool{}
	for _, r := range table.Ranges {
		for c := r[0]; c <= r[1]; c++ {
			set[c] = true
		}
	}
	return set
}

// Every code point is checked, not a sample: the two sets differ by a few
// scattered characters, and a sample is how those get missed.
func TestIsDigitMatchesCPythonOnEveryCodePoint(t *testing.T) {
	want := readRanges(t, "testdata/isdigit.json")
	for c := rune(0); c < 0x110000; c++ {
		if got := IsDigit(string(c)); got != want[c] {
			t.Errorf("IsDigit(%U) = %v, CPython says %v", c, got, want[c])
		}
	}
}

func TestIsDigitNeedsEveryCharacterAndAtLeastOne(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", false},
		{"0", true},
		{"12", true},
		{"1a", false},
		{"²", true},
		{"١٢", true},
		{"-1", false},
		{"1.0", false},
	} {
		if got := IsDigit(tc.in); got != tc.want {
			t.Errorf("IsDigit(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsSpaceMatchesCPythonOnEveryCodePoint(t *testing.T) {
	want := readRanges(t, "testdata/isspace.json")
	for c := rune(0); c < 0x110000; c++ {
		if got := isSpace(c); got != want[c] {
			t.Errorf("isSpace(%U) = %v, CPython says %v", c, got, want[c])
		}
	}
}

func TestStripRemovesPythonWhitespaceOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"  a b  ", "a b"},
		{"\n\ta\r\n", "a"},
		{"\x1ca\x1f", "a"},
		{"\u00a0a\u3000", "a"},
		{"\x00a", "\x00a"},
		{"\u200ba", "\u200ba"},
	} {
		if got := Strip(tc.in); got != tc.want {
			t.Errorf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
