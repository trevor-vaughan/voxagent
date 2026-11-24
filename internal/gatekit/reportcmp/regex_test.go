package reportcmp

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// groupZero runs one ported regex the way the original was called and returns
// what Python's group(0) was, or nil for no match.
func groupZero(t *testing.T, name, input string) *string {
	t.Helper()
	switch name {
	case "ERROR_LOCATION":
		// search(); group 1 excludes the final newline Python's $ preceded.
		if m := errorLocation.FindStringSubmatch(input); m != nil {
			return &m[1]
		}
		return nil
	case "APPLIED_CATALOG":
		// match(), and only whether it matched is ever used. \n?\z puts a
		// final newline inside the match where Python's $ left it outside,
		// so strip it before comparing the text.
		if loc := appliedCatalog.FindStringIndex(input); loc != nil {
			s := input[:loc[1]]
			if n := len(s); n > 0 && s[n-1] == '\n' {
				s = s[:n-1]
			}
			return &s
		}
		return nil
	}
	re := map[string]*regexp.Regexp{
		"FAILURE_PREFIX":    failurePrefix,
		"SET_WRAPPER":       setWrapper,
		"EXECUTION_WRAPPER": executionWrapper,
	}[name]
	if re == nil {
		t.Fatalf("testdata/regex.json names an unknown regex %q", name)
	}
	// match(): every one of these is ^-anchored.
	if loc := re.FindStringIndex(input); loc != nil {
		s := input[:loc[1]]
		return &s
	}
	return nil
}

func TestRegexesMatchTheOriginals(t *testing.T) {
	data, err := os.ReadFile("testdata/regex.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Regex [][3]*string `json:"regex"`
		Elide [][2]string  `json:"elide"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Regex) == 0 || len(table.Elide) == 0 {
		t.Fatal("testdata/regex.json is missing its regex or elide rows")
	}
	for _, row := range table.Regex {
		name, input, want := *row[0], *row[1], row[2]
		got := groupZero(t, name, input)
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("%s on %q: got %v, the original gave %v", name, input, quoted(got), quoted(want))
		}
	}
	for _, row := range table.Elide {
		if got := elideFailureDetail(row[0]); got != row[1] {
			t.Errorf("elideFailureDetail(%q) = %q, the original gave %q", row[0], got, row[1])
		}
	}
}

func quoted(s *string) string {
	if s == nil {
		return "no match"
	}
	return `"` + *s + `"`
}
