package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func TestRenderTextIdentical(t *testing.T) {
	var sb strings.Builder
	renderText(&sb, catalog.Report{})
	got := sb.String()
	if !strings.Contains(got, "IDENTICAL") {
		t.Errorf("renderText = %q, want it to contain IDENTICAL", got)
	}
	if !strings.Contains(got, "version, catalog_uuid") {
		t.Errorf("renderText = %q, want it to name the ignored fields", got)
	}
}

func TestRenderTextListsDifferences(t *testing.T) {
	rep := catalog.Report{Differences: []catalog.Difference{
		{Path: "resources[Notify[a]].parameters.message", Kind: catalog.KindChanged, Left: "x", Right: "y"},
	}}
	var sb strings.Builder
	renderText(&sb, rep)
	got := sb.String()
	if !strings.Contains(got, "1 difference") {
		t.Errorf("renderText = %q, want a difference count", got)
	}
	if !strings.Contains(got, "resources[Notify[a]].parameters.message") {
		t.Errorf("renderText = %q, want the difference path", got)
	}
}

func TestRenderJSONIsValid(t *testing.T) {
	rep := catalog.Report{Differences: []catalog.Difference{
		{Path: "environment", Kind: catalog.KindChanged, Left: "a", Right: "b"},
	}}
	var sb strings.Builder
	if err := renderJSON(&sb, rep); err != nil {
		t.Fatalf("renderJSON returned error: %v", err)
	}
	var back struct {
		Identical   bool `json:"identical"`
		Differences []struct {
			Path string `json:"path"`
		} `json:"differences"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &back); err != nil {
		t.Fatalf("renderJSON emitted invalid JSON: %v", err)
	}
	if back.Identical {
		t.Error("identical = true, want false")
	}
	if len(back.Differences) != 1 || back.Differences[0].Path != "environment" {
		t.Errorf("differences = %+v, want one entry for environment", back.Differences)
	}
}

func TestExitCodeForReport(t *testing.T) {
	if got := exitCodeFor(catalog.Report{}); got != 0 {
		t.Errorf("exitCodeFor(identical) = %d, want 0", got)
	}
	rep := catalog.Report{Differences: []catalog.Difference{{Path: "x", Kind: catalog.KindChanged}}}
	if got := exitCodeFor(rep); got != 1 {
		t.Errorf("exitCodeFor(differences) = %d, want 1", got)
	}
}

// A file whose extension is not .pp/.rb must never be read, even if it is
// unreadable. Reading it first (the old behaviour) would turn an irrelevant
// permission problem into a spurious scan error. This also exercises real
// module-tree contents (tarballs, vendored blobs) that scan has no business
// opening at all.
func TestRunScanNeverReadsFilesWithIrrelevantExtensions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.pp"), "class a {\n  @@nagios_host { 'web01': ensure => present }\n}\n")

	blob := filepath.Join(dir, "vendored.tar")
	writeFile(t, blob, "not actually a tarball, but unreadable")
	if err := os.Chmod(blob, 0o000); err != nil {
		t.Fatalf("chmod 000 on %s: %v", blob, err)
	}

	var stdout, stderr strings.Builder
	got := runScan([]string{dir}, &stdout, &stderr)

	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty; the unreadable non-.pp/.rb file should never have been opened", stderr.String())
	}
	if !strings.Contains(stdout.String(), "exported_resource") {
		t.Errorf("stdout = %q, want it to contain the finding from good.pp", stdout.String())
	}
	if got != exitDiverged {
		t.Errorf("runScan = %d, want %d (exitDiverged)", got, exitDiverged)
	}
}

// An unreadable file that scan does try to open must not discard findings
// already gathered from other files, and must not silently vanish either: it
// is reported on stderr, kept separate from the findings stream on stdout,
// and forces exitFailure even though findings were also produced -- an
// incomplete scan must never be reported as if it were a clean or
// merely-findings result.
func TestRunScanAccumulatesErrorsAndKeepsFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.pp"), "class a {\n  @@nagios_host { 'web01': ensure => present }\n}\n")

	// A dangling symlink, not a chmod 000 file.
	//
	// Root bypasses the permission bits, so a mode-based fixture makes the read
	// succeed and the scan return 1 (findings) instead of 2 (could not run).
	// Measured: this test failed exactly that way the first time the suite was
	// run as root in a container. A test that asserts an outage is reported
	// correctly, and which silently stops creating the outage in the one
	// environment where the code runs as root, is worse than no test.
	//
	// WalkDir reports a symlink as a non-directory entry, so the walk proceeds
	// to ReadFile, which follows the link and fails with ENOENT. Nothing about
	// that depends on privilege.
	bad := filepath.Join(dir, "bad.pp")
	if err := os.Symlink(filepath.Join(dir, "no-such-target"), bad); err != nil {
		t.Fatalf("symlink %s: %v", bad, err)
	}

	var stdout, stderr strings.Builder
	got := runScan([]string{dir}, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "exported_resource") {
		t.Errorf("stdout = %q, want it to contain the finding from good.pp despite bad.pp failing", stdout.String())
	}
	if !strings.Contains(stderr.String(), bad) {
		t.Errorf("stderr = %q, want it to mention %s", stderr.String(), bad)
	}
	if strings.Contains(stdout.String(), bad) {
		t.Errorf("stdout = %q, want the error kept out of the findings stream", stdout.String())
	}
	if got != exitFailure {
		t.Errorf("runScan = %d, want %d (exitFailure), even though findings were also produced", got, exitFailure)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
