package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles voxagent once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "voxagent")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// writeCatalog writes a minimal catalog containing the given resources.
func writeCatalog(t *testing.T, dir string, resources []map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, "catalog.json")
	body := map[string]any{
		"catalog_format": 2,
		"name":           "n.example",
		"environment":    "production",
		"version":        1,
		"catalog_uuid":   "00000000-0000-4000-8000-000000000000",
		"resources":      resources,
		"edges":          []any{},
		"tags":           []any{},
		"classes":        []any{},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	return path
}

func run(t *testing.T, bin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	switch {
	case err == nil:
		code = 0
	default:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errBuf.String(), code
}

func fileResource(path string, params map[string]any) map[string]any {
	return map[string]any{
		"type": "File", "title": path, "tags": []any{}, "parameters": params,
	}
}

func TestApplyCleanRun(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(filepath.Join(dir, "a.txt"),
			map[string]any{"ensure": "file", "content": "x\n"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (plain mode never reports a verdict)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=changed") {
		t.Errorf("stderr does not carry the outcome line:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("the file was not created: %v", err)
	}
}

// The exit contract, end to end. Plain mode is 0 for every result; the flag is
// the verdict channel.
func TestApplyExitCodes(t *testing.T) {
	bin := buildBinary(t)

	for _, tt := range []struct {
		name     string
		params   map[string]any
		sub      string
		plain    int
		detailed int
		outcome  string
	}{
		{"changes", map[string]any{"ensure": "file", "content": "x\n"}, "",
			0, 2, "outcome=changed"},
		{"failure", map[string]any{"ensure": "file", "content": "x\n"}, "missing",
			0, 4, "outcome=failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, mode := range []string{"plain", "detailed"} {
				dir := t.TempDir()
				target := filepath.Join(dir, tt.sub, "a.txt")
				cat := writeCatalog(t, dir, []map[string]any{fileResource(target, tt.params)})
				args := []string{"apply", "--catalog", cat}
				want := tt.plain
				if mode == "detailed" {
					args = append(args, "--detailed-exitcodes")
					want = tt.detailed
				}
				_, stderr, code := run(t, bin, args...)
				if code != want {
					t.Errorf("%s exit = %d, want %d\n%s", mode, code, want, stderr)
				}
				if !strings.Contains(stderr, tt.outcome) {
					t.Errorf("%s stderr does not carry %q:\n%s", mode, tt.outcome, stderr)
				}
			}
		})
	}
}

// A clean second run is still 0 under the flag: nothing changed.
func TestApplyUnchangedIsZeroUnderTheFlag(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(filepath.Join(dir, "a.txt"),
			map[string]any{"ensure": "file", "content": "x\n"}),
	})
	if _, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes"); code != 2 {
		t.Fatalf("first run exit = %d, want 2\n%s", code, stderr)
	}
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes")
	if code != 0 {
		t.Errorf("second run exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=applied") {
		t.Errorf("stderr does not say applied:\n%s", stderr)
	}
}

func TestPreflightRefusesAndChangesNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource(filepath.Join(dir, "tree"),
			map[string]any{"ensure": "directory", "recurse": true}),
	})

	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("plain exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "recurse") {
		t.Errorf("stderr does not name the unimplemented attribute:\n%s", stderr)
	}
	if !strings.Contains(stderr, "outcome=could-not-run") {
		t.Errorf("stderr does not carry the outcome line:\n%s", stderr)
	}
	// The refusal must be total: the implementable resource must not be applied.
	if _, err := os.Stat(target); err == nil {
		t.Error("a resource was applied despite the pre-flight refusal")
	}

	if _, _, code = run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes"); code != 1 {
		t.Errorf("detailed exit = %d, want 1 (could not run)", code)
	}
}

func TestMissingCatalogIsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	_, stderr, code := run(t, bin, "apply", "--catalog", "/nonexistent/catalog.json")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "outcome=could-not-run") {
		t.Errorf("stderr does not carry the outcome line:\n%s", stderr)
	}
}

func TestMalformedCatalogIsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"not":"a catalog"}`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, stderr, code := run(t, bin, "apply", "--catalog", path)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
}

// A dependency cycle is could-not-run, and nothing may be applied.
func TestCycleIsCouldNotRunAndAppliesNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(a, map[string]any{"ensure": "file", "content": "x\n",
			"require": []any{"File[" + b + "]"}}),
		fileResource(b, map[string]any{"ensure": "file", "content": "y\n",
			"require": []any{"File[" + a + "]"}}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "cycle") {
		t.Errorf("stderr does not mention the cycle:\n%s", stderr)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was applied despite the cycle", p)
		}
	}
}

func TestReportIsWritten(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	reportDir := filepath.Join(dir, "reports")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(filepath.Join(dir, "a.txt"),
			map[string]any{"ensure": "file", "content": "x\n"}),
	})
	if _, stderr, code := run(t, bin, "apply", "--catalog", cat,
		"--report-dir", reportDir); code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	entries, err := os.ReadDir(reportDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no report written to %s: %v", reportDir, err)
	}
	b, err := os.ReadFile(filepath.Join(reportDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if got["report_format"] != float64(12) {
		t.Errorf("report_format = %v, want 12", got["report_format"])
	}
	if got["catalog_uuid"] != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("catalog_uuid = %v, not carried from the catalog", got["catalog_uuid"])
	}
	if got["environment"] != "production" {
		t.Errorf("environment = %v, not carried from the catalog", got["environment"])
	}
}

func TestNoopChangesNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	reportDir := filepath.Join(dir, "reports")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
	})

	// Measured against Puppet 8.10.0: --noop --detailed-exitcodes over a file
	// that would be created exits 0, NOT 2. resource/status.rb:122-133
	// increments change_count only for events whose status is 'success', and a
	// noop event's status is 'noop'. Would-be changes travel in the report's
	// noop_pending field, never in the exit bitmask.
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--noop",
		"--detailed-exitcodes", "--report-dir", reportDir)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (noop changes do not reach the bitmask)\n%s", code, stderr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("--noop created the file")
	}

	// Exiting 0 is only correct if the signal survives somewhere. noop_pending
	// is where Puppet puts it, so assert it rather than letting "the exit code
	// says nothing" quietly mean "nothing was recorded".
	entries, err := os.ReadDir(reportDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no report written: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(reportDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if got["noop"] != true {
		t.Errorf("noop = %v, want true", got["noop"])
	}
	if got["noop_pending"] != true {
		t.Errorf("noop_pending = %v, want true -- the would-be change is recorded nowhere",
			got["noop_pending"])
	}
}

// A positional argument the parser does not recognise is could-not-run, and
// nothing may be applied.
//
// Go's flag package stops parsing at the first non-flag argument and parks the
// remainder in fs.Args(). Measured before this was fixed: `apply --catalog c
// junk --noop` swallowed the --noop, created the file, and exited 0 with
// outcome=changed. The operator asked for a dry run and got a live one, with
// every channel the run has -- exit status and outcome line -- reporting that
// all was well. The same swallow silently disables --detailed-exitcodes and
// --report-dir.
func TestStrayArgumentIsRefusedAndAppliesNothing(t *testing.T) {
	bin := buildBinary(t)

	for _, tt := range []struct {
		name string
		tail []string
	}{
		{"before a flag", []string{"junk", "--noop"}},
		{"after the terminator", []string{"--", "--noop"}},
		{"alone", []string{"junk"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "a.txt")
			cat := writeCatalog(t, dir, []map[string]any{
				fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
			})
			args := append([]string{"apply", "--catalog", cat}, tt.tail...)

			_, stderr, code := run(t, bin, args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (could not run)\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "outcome=could-not-run") {
				t.Errorf("stderr does not carry the outcome line:\n%s", stderr)
			}
			// The whole point: an argument the parser dropped must never be the
			// difference between a dry run and a live one.
			if _, err := os.Stat(target); err == nil {
				t.Error("the catalog was applied despite an unrecognised argument")
			}
		})
	}
}

func TestUsageWithoutApply(t *testing.T) {
	bin := buildBinary(t)
	_, stderr, code := run(t, bin)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr does not print usage:\n%s", stderr)
	}
}

func TestMissingCatalogFlagIsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	_, stderr, code := run(t, bin, "apply")
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "--catalog") {
		t.Errorf("stderr does not say --catalog is required:\n%s", stderr)
	}
}

// execResource builds an exec whose only effect is a marker file, so a test can
// tell whether it ran by looking at the filesystem rather than at the report.
func execResource(title, command string, params map[string]any) map[string]any {
	all := map[string]any{"command": command}
	for k, v := range params {
		all[k] = v
	}
	return map[string]any{
		"type": "Exec", "title": title, "tags": []any{}, "parameters": all,
	}
}

func notifyResource(title, message string, params map[string]any) map[string]any {
	all := map[string]any{"message": message}
	for k, v := range params {
		all[k] = v
	}
	return map[string]any{
		"type": "Notify", "title": title, "tags": []any{}, "parameters": all,
	}
}

// --noop must not run a refreshonly exec that a changing file notifies.
//
// This is the end-to-end form of the worst failure this tool has available: the
// operator asked for a dry run and the machine changed. The providers honour
// the flag, so the only way it happens is the transaction failing to pass it on
// -- and every catalog in this file was a File until now, so the refresh path
// had no CLI coverage at all.
//
// The marker file is the assertion. A report can say a run changed nothing
// while the command ran anyway; the filesystem cannot.
func TestNoopDoesNotRunANotifiedExec(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "conf.txt")
	marker := filepath.Join(dir, "SIDE_EFFECT")

	resources := []map[string]any{
		fileResource(conf, map[string]any{
			"ensure": "file", "content": "x\n",
			"notify": []any{"Exec[reload]"},
		}),
		execResource("reload", "/bin/touch "+marker, map[string]any{"refreshonly": true}),
	}
	cat := writeCatalog(t, dir, resources)

	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--noop")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	if _, err := os.Stat(conf); err == nil {
		t.Error("--noop created the file")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("--noop ran the notified exec; the machine was modified by a dry run")
	}

	// The negative control. Without it this test would pass against a build that
	// never refreshes anything at all, which is a different defect wearing the
	// same green.
	_, stderr, code = run(t, bin, "apply", "--catalog", cat)
	if code != 0 {
		t.Fatalf("real run exit = %d, want 0\n%s", code, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the real run did not trigger the refresh: %v", err)
	}
}

// Every registered provider is reachable through the command.
//
// Dropping ral.Exec{} or ral.Notify{} from the registry turns any catalog
// containing that type into a pre-flight refusal at exit 2. Both survived the
// suite because every fixture here was a File.
func TestAllRegisteredProvidersApply(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(filepath.Join(dir, "a.txt"), map[string]any{"ensure": "file", "content": "x\n"}),
		notifyResource("hello", "a message", nil),
		execResource("runs", "/bin/touch "+marker, nil),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=changed") {
		t.Errorf("stderr does not report a change:\n%s", stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the exec did not run: %v", err)
	}
}

// A run that applied changes and then could not write its report is
// could-not-run, and says the machine was modified.
//
// The most-commented branch in the command and the least tested: both
// "report success anyway" and "swallow the WriteFile error" survived the suite.
// The report directory is pointed at a path whose parent is a regular file, so
// MkdirAll fails for a reason no privilege level can bypass -- unlike a
// permission bit, which root ignores, and these tests run as root under
// dev:test-privileged.
func TestReportWriteFailureIsCouldNotRunAfterApplying(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := filepath.Join(dir, "a.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
	})

	_, stderr, code := run(t, bin, "apply", "--catalog", cat,
		"--report-dir", filepath.Join(blocker, "reports"))
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=could-not-run") {
		t.Errorf("stderr does not carry the outcome line:\n%s", stderr)
	}
	// "could not run" alone would imply nothing happened. It did.
	if !strings.Contains(stderr, "WAS modified") {
		t.Errorf("stderr does not say the machine was modified:\n%s", stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the catalog was not applied, so this tests the wrong branch: %v", err)
	}
}

// A subcommand that is not "apply" is could-not-run.
//
// TestUsageWithoutApply invokes the binary with zero arguments, which exercises
// only the len(os.Args) < 2 half of the guard. Relaxing it to accept any
// subcommand survived that test, so `voxagent destroy --catalog c.json` would
// silently have applied the catalog.
func TestUnknownSubcommandIsRefused(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
	})
	_, stderr, code := run(t, bin, "destroy", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr does not print usage:\n%s", stderr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("an unknown subcommand applied the catalog")
	}
}

// Changes AND failures in one run is 6 under the flag -- both bits, not the
// larger of the two.
//
// internal/report's contract table says "completed, failures -- 4 or 6", and the
// arithmetic is tested there. What was untested is the command's wiring of a
// mixed run: TestApplyExitCodes has a changes row and a failure row but never
// both at once.
func TestMixedRunSetsBothExitBits(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(filepath.Join(dir, "ok.txt"),
			map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource(filepath.Join(dir, "missing", "bad.txt"),
			map[string]any{"ensure": "file", "content": "y\n"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes")
	if code != 6 {
		t.Errorf("exit = %d, want 6 (2 changes | 4 failures)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=failed") {
		t.Errorf("stderr does not report a failure:\n%s", stderr)
	}
}

func TestRelativeFilePathIsRefusedAndChangesNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	// The child inherits this working directory, so a relative path that was
	// applied would land here and be detectable.
	t.Chdir(dir)
	valid := filepath.Join(dir, "valid.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(valid, map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource("rel/f1", map[string]any{"ensure": "file", "content": "y\n"}),
	})
	if err := os.Mkdir(filepath.Join(dir, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("plain exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	want := "Parameter path failed on File[rel/f1]: File paths must be fully qualified, not 'rel/f1'"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks Puppet's message %q:\n%s", want, stderr)
	}
	if !strings.Contains(stderr, "1 invalid resource(s); nothing applied") {
		t.Errorf("stderr lacks the summary line:\n%s", stderr)
	}
	for _, p := range []string{valid, filepath.Join(dir, "rel", "f1")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was applied despite the refusal", p)
		}
	}
	if _, _, code = run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes"); code != 1 {
		t.Errorf("detailed exit = %d, want 1 (could not run)", code)
	}
}

// Pre-flight runs first: a catalog that is both unimplemented and invalid is
// reported as unimplemented, which is the fact an operator can act on first.
func TestPreflightGapIsReportedBeforeValidation(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	t.Chdir(dir)
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource("rel/f1", map[string]any{"ensure": "link"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `ensure => "link" is not implemented`) {
		t.Errorf("stderr lacks the pre-flight gap:\n%s", stderr)
	}
	if strings.Contains(stderr, "fully qualified") {
		t.Errorf("validation ran although pre-flight refused:\n%s", stderr)
	}
}

func TestFileEnsureLinkIsRefusedBeforeApplying(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(valid, map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource(filepath.Join(dir, "l"), map[string]any{"ensure": "link"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if _, err := os.Stat(valid); err == nil {
		t.Error("the valid resource was applied despite the ensure => link refusal")
	}
}

// Measured before the fix: a JSON false skipped value bounding, the valid file
// was written, and the run exited 6 -- a half-applied catalog.
func TestFileEnsureBooleanIsRefusedBeforeApplying(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(valid, map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource(filepath.Join(dir, "b"), map[string]any{"ensure": false}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `ensure => "false" is not implemented`) {
		t.Errorf("stderr lacks the pre-flight gap:\n%s", stderr)
	}
	if _, err := os.Stat(valid); err == nil {
		t.Error("the valid resource was applied despite the ensure => false refusal")
	}
}

// Measured before the fix: {"command":"touch marker","cwd":"rel"} ran in
// voxagent's own working directory and created rel/marker. Puppet refuses the
// catalog while converting it, so nothing runs.
func TestExecRelativeCwdIsRefusedAndRunsNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir(filepath.Join(dir, "rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	cat := writeCatalog(t, dir, []map[string]any{{
		"type": "Exec", "title": "mark", "tags": []any{},
		"parameters": map[string]any{"command": "/bin/touch marker", "cwd": "rel"},
	}})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	want := "Parameter cwd failed on Exec[mark]: cwd must be a fully qualified path"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks Puppet's message %q:\n%s", want, stderr)
	}
	for _, p := range []string{filepath.Join(dir, "rel", "marker"), filepath.Join(dir, "marker")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s exists: the command ran despite the refusal", p)
		}
	}
}
