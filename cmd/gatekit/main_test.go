package main

import (
	"strings"
	"testing"
)

const (
	reportData = "../../internal/gatekit/reportcmp/testdata/"
	g5Data     = "../../internal/gatekit/g5ref/testdata/"
)

func gatekit(args ...string) (int, string, string) {
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestBadArgumentsCannotRun(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"no-such-subcommand"},
		{"compare-reports", "a", "b"},
		{"run-to-run", "a", "b", "c", "d"},
		{"g5-reference", "a", "b", "c"},
	} {
		code, _, stderr := gatekit(args...)
		if code != exitFailure || !strings.Contains(stderr, "gatekit compare-reports") {
			t.Errorf("gatekit %q = %d with stderr %q, want 2 and the usage", args, code, stderr)
		}
	}
}

func TestCompareReportsExitCodes(t *testing.T) {
	allow := reportData + "allowlist.json"
	code, stdout, _ := gatekit("compare-reports", reportData+"corpus/d14-control-ruby.json", reportData+"corpus/d14-control-go.json", allow)
	if code != exitExpected || stdout != "81 leaves compared, 0 divergent\n" {
		t.Errorf("agreeing reports: exit %d, stdout %q", code, stdout)
	}
	code, stdout, _ = gatekit("compare-reports", reportData+"corpus/spine-ruby.json", reportData+"corpus/spine-voxagent.json", allow)
	if code != exitFinding || !strings.HasPrefix(stdout, "181 leaves compared, 1 divergent\n  ") {
		t.Errorf("diverging reports: exit %d, stdout %q", code, stdout)
	}
	code, _, stderr := gatekit("compare-reports", reportData+"edge/not-utf8.ruby.json", reportData+"edge/not-utf8.voxagent.json", allow)
	if code != exitFailure || !strings.Contains(stderr, "could not read an input") {
		t.Errorf("undecodable report: exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = gatekit("compare-reports", reportData+"edge/top-level-list.ruby.json", reportData+"edge/top-level-list.voxagent.json", allow)
	if code != exitFailure || !strings.Contains(stderr, "not an object") {
		t.Errorf("non-object report: exit %d, stderr %q", code, stderr)
	}
}

// Each exit code is checked together with the reason it is given for, so a
// right code reached for the wrong reason still fails.
func TestRunToRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   int
		output string // expected in stdout, or in stderr for a refusal
	}{
		{"run-clean", exitExpected, "patterns cover 1 differing leaves and nothing else"},
		{"run-incomplete", exitFinding, "INCOMPLETE -- these vary run to run"},
		{"run-no-patterns", exitFailure, "the allowlist has no run_to_run patterns"},
	} {
		base := reportData + "edge/" + tc.name
		code, stdout, stderr := gatekit("run-to-run", base+".first.json", base+".second.json", base+".allowlist.json")
		if code != tc.want || !strings.Contains(stdout+stderr, tc.output) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %d and %q", tc.name, code, stdout, stderr, tc.want, tc.output)
		}
	}
}

func TestG5ReferenceExitCodes(t *testing.T) {
	fixture := []string{"tree", "gdb-gdbserver", "rust-toolset-srpm-macros", "voxagent-no-such-package"}
	for _, tc := range []struct {
		report, catalog string
		want            int
	}{
		{"clean.report.json", "catalog.json", exitExpected},
		{"install-absent.report.json", "catalog.json", exitFinding},
		{"crash-statuses-list.report.json", "catalog.json", exitFailure},
	} {
		args := append([]string{"g5-reference", g5Data + tc.report, g5Data + tc.catalog}, fixture...)
		code, stdout, _ := gatekit(args...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.report, code, tc.want)
		}
		if (code == exitFinding) != (stdout != "") {
			t.Errorf("%s: exit %d but stdout %q; complaints and a finding go together", tc.report, code, stdout)
		}
	}
}
