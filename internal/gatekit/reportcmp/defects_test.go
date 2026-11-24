package reportcmp

import (
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// The three places this package departs from the Python comparator's verdicts.
// Python cannot measure a fix to itself, so these expectations are derived from
// the corrected rules, not from a CPython table.

func decode(t *testing.T, s string) any {
	t.Helper()
	v, err := pycompat.Decode([]byte(s))
	if err != nil {
		t.Fatalf("Decode(%s): %v", s, err)
	}
	return v
}

func divergentPaths(t *testing.T, ruby, voxagent string) []string {
	t.Helper()
	result, err := Compare(decode(t, ruby), decode(t, voxagent), Allowlist{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var paths []string
	for _, d := range result.Divergences {
		paths = append(paths, d.Path)
	}
	return paths
}

const logX = `{"source":"Puppet","message":"x","level":"notice"}`

func TestLogsCompareByCount(t *testing.T) {
	for _, tc := range []struct {
		name, ruby, voxagent string
		want                 []string
	}{
		{"equal counts", "[" + logX + "," + logX + "]", "[" + logX + "," + logX + "]", nil},
		{"two against one", "[" + logX + "," + logX + "]", "[" + logX + "]",
			[]string{"logs (only in ruby)"}},
		{"three against one", "[" + logX + "," + logX + "," + logX + "]", "[" + logX + "]",
			[]string{"logs (only in ruby)", "logs (only in ruby)"}},
		{"one against two", "[" + logX + "]", "[" + logX + "," + logX + "]",
			[]string{"logs (only in voxagent)"}},
		{"order is not a divergence", `[{"message":"a"},{"message":"b"}]`, `[{"message":"b"},{"message":"a"}]`, nil},
		{"absent from one side, as before", "[" + logX + "]", "[]",
			[]string{"logs (only in ruby)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := divergentPaths(t, `{"logs":`+tc.ruby+`}`, `{"logs":`+tc.voxagent+`}`)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("divergences = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBoolsEqualOnlyBools(t *testing.T) {
	for _, tc := range []struct {
		ruby, voxagent string
		diverge        bool
	}{
		{`true`, `1`, true},
		{`true`, `1.0`, true},
		{`false`, `0`, true},
		{`1`, `true`, true},
		{`true`, `true`, false},
		{`false`, `false`, false},
		{`true`, `false`, true},
		{`1`, `1.0`, false},
		{`0`, `-0`, false},
	} {
		got := divergentPaths(t, `{"a":`+tc.ruby+`}`, `{"a":`+tc.voxagent+`}`)
		if (len(got) > 0) != tc.diverge {
			t.Errorf("%s against %s: divergences %q, want diverge=%v", tc.ruby, tc.voxagent, got, tc.diverge)
		}
	}
}

func TestAbsenceIsNotAString(t *testing.T) {
	for _, tc := range []struct{ ruby, voxagent string }{
		{`{"a":"<absent>"}`, `{}`},
		{`{}`, `{"a":"<absent>"}`},
	} {
		got := divergentPaths(t, tc.ruby, tc.voxagent)
		if strings.Join(got, ",") != "a" {
			t.Errorf("%s against %s: divergences %q, want [a]", tc.ruby, tc.voxagent, got)
		}
	}
}

// run-to-run splits leaves into differing and stable with the same comparison,
// so an absent leaf against a "<absent>" one is differing, and with no pattern
// covering it the allowlist is incomplete.
func TestRunToRunTreatsAbsenceAsDiffering(t *testing.T) {
	var out strings.Builder
	clean, err := RunToRun(decode(t, `{"a":"<absent>"}`), decode(t, `{}`), Allowlist{RunToRun: []string{"x"}}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if clean || !strings.Contains(out.String(), "1 differing") || !strings.Contains(out.String(), "INCOMPLETE") {
		t.Errorf("RunToRun clean=%v, output:\n%s\nwant leaf a differing and uncovered", clean, out.String())
	}
}

func TestLeavesEqual(t *testing.T) {
	for _, tc := range []struct {
		a, b     any
		inA, inB bool
		want     bool
	}{
		{"x", "x", true, true, true},
		{"<absent>", nil, true, false, false},
		{nil, "<absent>", false, true, false},
		{nil, nil, false, false, true},
		{true, true, true, true, true},
		{true, false, true, true, false},
		{nil, nil, true, true, true},
		{nil, false, true, true, false},
		{nil, nil, true, false, false},
		{nil, nil, false, true, false},
	} {
		if got := leavesEqual(tc.a, tc.inA, tc.b, tc.inB); got != tc.want {
			t.Errorf("leavesEqual(%v,%v, %v,%v) = %v, want %v", tc.a, tc.inA, tc.b, tc.inB, got, tc.want)
		}
	}
}

// run-to-run derives over what Compare compares. A failure detail that varies
// between two runs is masked by failure_detail in Compare, so run-to-run must
// not call it uncovered; the attribution around the detail is still compared,
// so a difference there must still be.
func TestRunToRunDerivesOverTheNormalisedReport(t *testing.T) {
	report := func(value, detail string) any {
		msg := "change from 'absent' to 'file' failed: Could not set '" + value + "' on ensure: " + detail + " (file: /m.pp, line: 1)"
		return decode(t, `{"time":"`+detail+`",
			"resource_statuses":{"File[/x]":{"events":[{"status":"failure","message":"`+msg+`"}]}},
			"logs":[{"source":"/File[/x]/ensure","level":"err","message":"`+msg+`"}]}`)
	}
	allow := Allowlist{RunToRun: []string{"time"}}
	for _, tc := range []struct {
		name          string
		second        any
		wantClean     bool
		wantUncovered string
	}{
		{"only the detail differs", report("file", "lock2"), true, ""},
		{"the value set differs", report("directory", "lock2"), false, "resource_statuses.File[/x].events.0.message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			clean, err := RunToRun(report("file", "lock1"), tc.second, allow, &out)
			if err != nil {
				t.Fatal(err)
			}
			if clean != tc.wantClean || (tc.wantUncovered != "" && !strings.Contains(out.String(), "  "+tc.wantUncovered+"\n")) {
				t.Errorf("clean=%v, want %v (uncovered %q); output:\n%s", clean, tc.wantClean, tc.wantUncovered, out.String())
			}
		})
	}
}
