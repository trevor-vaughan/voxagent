package exec

import (
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// Ruby's split(/\n/): trailing empty strings go, interior ones and \r stay
// (measured, 7-exec-output).
func TestSplitOutput(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"a\n\nb\n", []string{"a", "", "b"}},
		{"a\n\n\n", []string{"a"}},
		{"a\r\nb", []string{"a\r", "b"}},
		{"abc", []string{"abc"}},
		{"", nil},
		{"\n\n\n", nil},
	} {
		if got := command.SplitOutput(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("splitOutput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Encoding.default_external: the codeset of the first non-empty of LC_ALL,
// LC_CTYPE, LANG; UTF-8 only for a UTF-8 codeset, US-ASCII otherwise (D-79).
func TestOutputEncoding(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none set", nil, "US-ASCII"},
		{"C", map[string]string{"LANG": "C"}, "US-ASCII"},
		{"POSIX", map[string]string{"LANG": "POSIX"}, "US-ASCII"},
		{"no codeset", map[string]string{"LANG": "en_US"}, "US-ASCII"},
		{"C.UTF-8", map[string]string{"LANG": "C.UTF-8"}, "UTF-8"},
		{"utf8 spelling", map[string]string{"LANG": "en_US.utf8"}, "UTF-8"},
		{"mixed case", map[string]string{"LANG": "x.Utf-8"}, "UTF-8"},
		{"modifier", map[string]string{"LANG": "en_US.UTF-8@euro"}, "UTF-8"},
		{"other codeset", map[string]string{"LANG": "en_US.ISO-8859-1"}, "US-ASCII"},
		{"LC_ALL beats LANG", map[string]string{"LC_ALL": "C", "LANG": "C.UTF-8"}, "US-ASCII"},
		{"LC_CTYPE beats LANG", map[string]string{"LC_CTYPE": "C.UTF-8", "LANG": "C"}, "UTF-8"},
		{"empty LC_ALL is skipped", map[string]string{"LC_ALL": "", "LANG": "C.UTF-8"}, "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := rubyvalue.ExternalEncoding(getenv); got != tc.want {
				t.Errorf("ExternalEncoding = %q, want %q", got, tc.want)
			}
		})
	}
}
