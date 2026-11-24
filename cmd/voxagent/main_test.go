package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
			map[string]any{"ensure": "directory", "checksum": "md5"}),
	})

	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("plain exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `attribute "checksum" is not implemented`) {
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
// Dropping exec.Exec{} or notify.Notify{} from the registry turns any catalog
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
		fileResource("rel/f1", map[string]any{"source": "puppet:///modules/m/f"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `source => "puppet:///modules/m/f" is not implemented`) {
		t.Errorf("stderr lacks the pre-flight gap:\n%s", stderr)
	}
	if strings.Contains(stderr, "fully qualified") {
		t.Errorf("validation ran although pre-flight refused:\n%s", stderr)
	}
}

// Measured before pre-flight judged non-strings: a JSON false skipped value
// bounding, the valid file was written, and the run exited 6 -- a
// half-applied catalog. false is now absent; true, which Puppet fails as a link
// target that is not a path, is refused by validation before anything runs.
func TestFileEnsureTrueIsRefusedBeforeApplying(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(valid, map[string]any{"ensure": "file", "content": "x\n"}),
		fileResource(filepath.Join(dir, "b"), map[string]any{"ensure": true}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `ensure => true (bool) is not implemented`) {
		t.Errorf("stderr lacks the refusal:\n%s", stderr)
	}
	if _, err := os.Stat(valid); err == nil {
		t.Error("the valid resource was applied despite the ensure => true refusal")
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

// service is registered: a catalog with one is not refused as an unimplemented
// type. A host whose PID 1 is not systemd refuses the whole catalog as invalid
// (Service.Validate); a systemd host applies it. The test host may be either,
// so it asserts only that the pre-flight coverage gap is gone.
func TestServiceIsRegistered(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := writeCatalog(t, dir, []map[string]any{{
		"type": "Service", "title": "voxagent-no-such-unit", "tags": []any{},
		"parameters": map[string]any{"enable": false},
	}})
	_, stderr, _ := run(t, bin, "apply", "--catalog", cat, "--noop")
	if strings.Contains(stderr, "resource type Service is not implemented") {
		t.Errorf("service is still refused at pre-flight:\n%s", stderr)
	}
	if strings.Contains(stderr, "gap(s)") {
		t.Errorf("pre-flight reported gaps:\n%s", stderr)
	}
	if strings.Contains(stderr, "systemd is not the init system") &&
		!strings.Contains(stderr, "invalid resource(s)") {
		t.Errorf("non-systemd host did not refuse the catalog as invalid:\n%s", stderr)
	}
}

// The core promise on a host that cannot run service: a catalog holding a
// valid file and a service is refused whole, before the file is written,
// rather than applying the file and failing the service (D-22).
func TestServiceOnANonSystemdHostRefusesTheWholeCatalog(t *testing.T) {
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil {
		t.Skipf("cannot read /proc/1/comm: %v", err)
	}
	if strings.Contains(string(comm), "systemd") {
		t.Skip("PID 1 is systemd here; the refusal this test pins cannot happen")
	}
	bin := buildBinary(t)
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.txt")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(valid, map[string]any{"ensure": "file", "content": "x\n"}),
		{
			"type": "Service", "title": "voxagent-no-such-unit", "tags": []any{},
			"parameters": map[string]any{"ensure": "running"},
		},
	})

	_, stderr, code := run(t, bin, "apply", "--catalog", cat)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not run)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "outcome=could-not-run") {
		t.Errorf("stderr lacks outcome=could-not-run:\n%s", stderr)
	}
	if !strings.Contains(stderr, "systemd is not the init system") {
		t.Errorf("stderr does not name the reason:\n%s", stderr)
	}
	if _, err := os.Stat(valid); err == nil {
		t.Errorf("%s was written despite the refusal", valid)
	}
}

// readReport reads the one report a run wrote into dir.
func readReport(t *testing.T, dir string) map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one report in %s: %v (err %v)", dir, entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	return rep
}

// End to end: a source is copied, and a source missing everywhere is a
// resource failure naming the catalog's environment, whose dependent is
// skipped while the rest of the catalog applies.
func TestFileSourceAppliesAndAMissingSourceSkipsItsDependent(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("from source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "copied.txt")
	never := filepath.Join(dir, "never.txt")
	reportDir := filepath.Join(dir, "reports")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(copied, map[string]any{"source": src}),
		fileResource(never, map[string]any{"source": []any{filepath.Join(dir, "absent")}}),
		notifyResource("after", "never", map[string]any{"require": []any{"File[" + never + "]"}}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes", "--report-dir", reportDir)
	if code != 6 {
		t.Fatalf("exit = %d, want 6 (changes and failures)\n%s", code, stderr)
	}
	if b, err := os.ReadFile(copied); err != nil || string(b) != "from source\n" {
		t.Errorf("copied = %q (err %v)", b, err)
	}
	if _, err := os.Lstat(never); !os.IsNotExist(err) {
		t.Errorf("the resource with a missing source created its path: %v", err)
	}
	rep := readReport(t, reportDir)
	wantErr := "Could not evaluate: Could not retrieve information from environment production source(s) file://" +
		filepath.Join(dir, "absent")
	found := false
	for _, l := range rep["logs"].([]any) {
		entry := l.(map[string]any)
		found = found || (entry["level"] == "err" && entry["message"] == wantErr)
	}
	if !found {
		t.Errorf("no err log %q in %v", wantErr, rep["logs"])
	}
	statuses := rep["resource_statuses"].(map[string]any)
	if after, ok := statuses["Notify[after]"].(map[string]any); !ok || after["skipped"] != true {
		t.Errorf("Notify[after] = %v, want skipped", statuses["Notify[after]"])
	}
}

// End to end: recursion from a source with purge, beside an explicitly managed
// child and a subscriber. The managed child survives the purge, a symlink is
// unlinked without touching its target, generated children report null
// file/line, and the subscriber is refreshed by exactly one event. Review
// Focus 2 and 5.
func TestRecursePurgeAppliesAsOneUnit(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	precious := filepath.Join(dir, "precious")
	for path, body := range map[string]string{
		filepath.Join(src, "new.txt"):          "new\n",
		filepath.Join(src, "sub", "inner.txt"): "inner\n",
		filepath.Join(dst, "stale.txt"):        "stale\n",
		filepath.Join(dst, "keep.txt"):         "old\n",
		precious:                               "precious\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(precious, filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "refreshed")
	reportDir := filepath.Join(dir, "reports")
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(dst, map[string]any{"ensure": "directory", "source": src, "recurse": true, "purge": true}),
		fileResource(filepath.Join(dst, "keep.txt"), map[string]any{"ensure": "file", "content": "kept\n"}),
		execResource("reload", "/bin/sh -c 'echo x >> "+marker+"'", map[string]any{
			"refreshonly": true, "subscribe": []any{"File[" + dst + "]"}}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes", "--report-dir", reportDir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (changes, no failures)\n%s", code, stderr)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "new.txt"):          "new\n",
		filepath.Join(dst, "sub", "inner.txt"): "inner\n",
		filepath.Join(dst, "keep.txt"):         "kept\n",
		precious:                               "precious\n",
	} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s = %q (err %v), want %q", path, b, err, want)
		}
	}
	for _, gone := range []string{"stale.txt", "link"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the purge: %v", gone, err)
		}
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "x\n" {
		t.Errorf("marker = %q (err %v), want exactly one refresh", b, err)
	}
	rep := readReport(t, reportDir)
	statuses := rep["resource_statuses"].(map[string]any)
	child, ok := statuses["File["+filepath.Join(dst, "new.txt")+"]"].(map[string]any)
	if !ok || child["file"] != nil || child["line"] != nil {
		t.Errorf("generated child status = %v, want present with null file and line", child)
	}
	var messages []string
	for _, ev := range statuses["Exec[reload]"].(map[string]any)["events"].([]any) {
		messages = append(messages, ev.(map[string]any)["message"].(string))
	}
	if !slices.Equal(messages, []string{"Triggered 'refresh' from 1 event"}) {
		t.Errorf("Exec[reload] events = %v, want one refresh from 1 event", messages)
	}
}

// A catalog File written with a trailing or doubled slash is the same path
// to Puppet (type/file.rb:66-73 munges it clean), so a recurse+purge parent
// leaves it and everything under it alone, exactly as for a clean title;
// an unmanaged sibling is still purged. Measured on Puppet 8.10.0 for the
// trailing slash.
func TestRecursePurgeExemptsUncleanlyWrittenCatalogFiles(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	for _, rel := range []string{"keep/x", "keep2", "stale"} {
		path := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(dst, map[string]any{"ensure": "directory", "recurse": true, "purge": true}),
		fileResource(dst+"/keep/", map[string]any{"ensure": "directory"}),
		fileResource(dst+"//keep2", map[string]any{"ensure": "file"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (the purge changed something, no failures)\n%s", code, stderr)
	}
	// Content too: a purged keep2 would be recreated empty by its own
	// ensure => file.
	for _, kept := range []string{"keep/x", "keep2"} {
		if b, err := os.ReadFile(filepath.Join(dst, kept)); err != nil || string(b) != "x\n" {
			t.Errorf("%s = %q (err %v): purged though a catalog File manages it", kept, b, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Errorf("the unmanaged stale survived the purge: %v", err)
	}
}

// End to end, a trailing slash does not walk a File through a symlink: the
// title is cleaned to the path Puppet's munge gives it (type/file.rb:66-73)
// before anything reads the filesystem, so under links => manage the link
// itself is replaced by the directory, as Puppet replaces it (measured), and
// its target keeps its mode.
func TestTrailingSlashTitleOverASymlinkReplacesTheLink(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(dir+"/link/", map[string]any{"ensure": "directory", "mode": "0700"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (the link was replaced)\n%s", code, stderr)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "link")); err != nil || !fi.IsDir() {
		t.Errorf("link = %v (err %v), want a directory in its place", fi, err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o755 {
		t.Errorf("target mode = %o, want 0755 (chmod went through the link)", got)
	}
}

// --vardir is where backup => 'puppet' keeps the old content: the
// clientbucket under it, and nothing beside the file.
func TestVardirHoldsTheClientbucket(t *testing.T) {
	bin := buildBinary(t)
	dir, vardir := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := writeCatalog(t, dir, []map[string]any{
		fileResource(path, map[string]any{"content": "new\n", "backup": "puppet"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--vardir", vardir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	const sum = "01d09d19c2139a46aebfb577780d123d7396e97201bc7ead210a2ebff8239dee"
	contents := filepath.Join(vardir, "clientbucket", "0", "1", "d", "0", "9", "d", "1", "9", sum, "contents")
	if b, err := os.ReadFile(contents); err != nil || string(b) != "old\n" {
		t.Errorf("contents = %q, %v; want the old content", b, err)
	}
}

func richNotify(message any) map[string]any {
	return map[string]any{"type": "Notify", "title": "n", "tags": []any{}, "kind": "compilable_type",
		"parameters": map[string]any{"message": message}}
}

// An ASCII Binary applies as the string it holds.
func TestBinaryParameterAppliesAsAString(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	reports := filepath.Join(dir, "reports")
	c := writeCatalog(t, dir, []map[string]any{richNotify(map[string]any{"__ptype": "Binary", "__pvalue": "aGVsbG8="})})
	_, stderr, code := run(t, bin, "apply", "--catalog", c, "--report-dir", reports)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	st := readReport(t, reports)["resource_statuses"].(map[string]any)["Notify[n]"].(map[string]any)
	ev := st["events"].([]any)[0].(map[string]any)
	if ev["message"] != "defined 'message' as 'hello'" || ev["desired_value"] != "hello" {
		t.Errorf("event = %v", ev)
	}
}

// Ruby renders a non-ASCII Binary byte-wise; voxagent
// refuses it on a native resource (D-51).
func TestNonASCIIBinaryOnANativeResourceIsRefused(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "a.txt")
	c := writeCatalog(t, dir, []map[string]any{
		richNotify(map[string]any{"__ptype": "Binary", "__pvalue": "w6k="}),
		fileResource(target, map[string]any{"ensure": "file", "content": "x\n"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", c)
	const want = "voxagent: Notify[n]: parameter message holds a Binary value that is not ASCII, " +
		"which voxagent does not support on a native resource"
	if code != 2 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "nothing applied") {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("a resource was applied despite the refusal")
	}
}

// Deferred is refused before the extension host starts,
// whose resolve_and_replace would otherwise resolve or defer it.
func TestDeferredIsRefusedBeforeTheHost(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{
		richNotify(map[string]any{"__ptype": "Deferred", "name": "join", "arguments": []any{[]any{"a", "b"}, "-"}}),
		ext("Vox_value", "a", map[string]any{"value": "x"}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", c, "--ruby", "/nonexistent/ruby")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	for _, want := range []string{
		"voxagent: Notify[n]: parameter message is Deferred; voxagent does not resolve Deferred values\n",
		"voxagent: 1 Deferred value(s); nothing applied\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "extension host is unavailable") {
		t.Errorf("the host was started before the refusal:\n%s", stderr)
	}
}

// A Deferred wrapped in a Sensitive, the usual shape of a secret looked up at
// apply time, is refused before the host too: Puppet's resolver would find it.
func TestSensitiveDeferredOnATier3ResourceIsRefusedBeforeTheHost(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Vox_value", "a", map[string]any{"value": map[string]any{
		"__ptype": "Sensitive", "__pvalue": map[string]any{"__ptype": "Deferred", "name": "join", "arguments": []any{[]any{"a", "b"}, "-"}}}})})
	_, stderr, code := run(t, bin, "apply", "--catalog", c, "--ruby", "/nonexistent/ruby")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	for _, want := range []string{
		"voxagent: Vox_value[a]: parameter value is Deferred; voxagent does not resolve Deferred values\n",
		"voxagent: 1 Deferred value(s); nothing applied\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "extension host is unavailable") {
		t.Errorf("the host was started before the refusal:\n%s", stderr)
	}
}

// Invalid base64 fails the whole catalog, as Ruby's
// strict_decode64 does.
func TestInvalidBase64IsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{richNotify(map[string]any{"__ptype": "Binary", "__pvalue": "aGVsbG8"})})
	_, stderr, code := run(t, bin, "apply", "--catalog", c)
	const want = "voxagent: Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: invalid base64\n"
	if code != 2 || !strings.Contains(stderr, want) {
		t.Errorf("exit %d, stderr:\n%s", code, stderr)
	}
}

// End to end on real paths: unrelated resources
// apply in catalog order, a recursed directory's generated children follow it
// contiguously in bytewise path order (a-b before a/x), and whatever depends on
// the directory -- including the catalog-managed d/c, through its autorequire
// -- waits for the whole unit. after_d runs before File[d] because File[d] is
// blocked by before_d: the Key orders only the ready set.
func TestWalkOrderAroundARecursedDirectory(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	d := filepath.Join(dir, "d")
	for _, sub := range []string{"a", "zdir"} {
		if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"0", "a/x", "a-b", "b", "c", "zdir/q"} {
		if err := os.WriteFile(filepath.Join(d, f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{d, filepath.Join(d, "a"), filepath.Join(d, "zdir")} {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	reportDir := filepath.Join(dir, "reports")
	cat := writeCatalog(t, dir, []map[string]any{
		notifyResource("first", "first", nil),
		fileResource(d, map[string]any{"recurse": true, "mode": "0700"}),
		notifyResource("after_d", "after_d", nil),
		notifyResource("dep_on_d", "dep_on_d", map[string]any{"require": []any{"File[" + d + "]"}}),
		fileResource(filepath.Join(d, "c"), map[string]any{"mode": "0600"}),
		notifyResource("before_d", "before_d", map[string]any{"before": []any{"File[" + d + "]"}}),
	})
	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--detailed-exitcodes", "--report-dir", reportDir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (changes, no failures)\n%s", code, stderr)
	}
	var got []string
	for _, l := range readReport(t, reportDir)["logs"].([]any) {
		source := l.(map[string]any)["source"].(string)
		var name string
		if i := strings.Index(source, "Notify["); i >= 0 {
			name = source[i+len("Notify[") : strings.LastIndex(source, "]")]
		} else if i := strings.Index(source, "File["+d); i >= 0 {
			name = "d" + source[i+len("File["+d):strings.LastIndex(source, "]")]
		} else {
			continue
		}
		if !slices.Contains(got, name) {
			got = append(got, name)
		}
	}
	want := []string{"first", "after_d", "before_d", "d", "d/0", "d/a", "d/a-b", "d/a/x", "d/b",
		"d/zdir", "d/zdir/q", "dep_on_d", "d/c"}
	if !slices.Equal(got, want) {
		t.Errorf("first log of each resource, in order:\n got %v\nwant %v", got, want)
	}
}

// Ruby prints "Unable to mark 'cwd' as sensitive" for a parameter that is not a
// property and applies the resource anyway. voxagent prints it on stderr
// in Ruby's console form after validation, keeps it out of the report, and
// still runs the exec.
func TestApplyWarnsOnAnUnmarkableSensitiveParameter(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	e := execResource("e", "/usr/bin/touch "+marker, map[string]any{"cwd": dir})
	e["sensitive_parameters"] = []any{"cwd"}
	cat := writeCatalog(t, dir, []map[string]any{e})
	reports := filepath.Join(dir, "reports")

	_, stderr, code := run(t, bin, "apply", "--catalog", cat, "--report-dir", reports)
	if code != 2 && code != 0 {
		t.Fatalf("exit = %d, want the exec applied\n%s", code, stderr)
	}
	want := "Warning: /Exec[e]: Unable to mark 'cwd' as sensitive: cwd is a parameter and not a property, and cannot be automatically redacted.\n"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the exec did not run: %v", err)
	}
	b, err := json.Marshal(readReport(t, reports))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Unable to mark") {
		t.Errorf("the warning is in the report: %s", b)
	}
}

// A File's content is a property, so marking it warns about nothing.
func TestApplyDoesNotWarnOnAMarkableSensitiveProperty(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	f := fileResource(filepath.Join(dir, "f"), map[string]any{"ensure": "file", "content": "x\n"})
	f["sensitive_parameters"] = []any{"content"}
	cat := writeCatalog(t, dir, []map[string]any{f})

	_, stderr, _ := run(t, bin, "apply", "--catalog", cat)
	if strings.Contains(stderr, "Unable to mark") {
		t.Errorf("unexpected warning:\n%s", stderr)
	}
}

// containedNotifyCatalog is a Notify "n" (message "n") in Class[main], so its
// log source is Ruby's /Stage[main]/Main/Notify[n].
func containedNotifyCatalog(t *testing.T, dir string) string {
	t.Helper()
	mainClass := map[string]any{"type": "Class", "title": "main", "tags": []any{}, "kind": "class",
		"parameters": map[string]any{"name": "main"}}
	stage := map[string]any{"type": "Stage", "title": "main", "tags": []any{}, "parameters": map[string]any{}}
	path := writeCatalog(t, dir, []map[string]any{stage, mainClass, notifyResource("n", "n", nil)})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": "Notify[n]"}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// apply prints the run's log as Ruby's console does, at the level the flags
// name, and the report holds exactly the levels printed. --debug implies
// --verbose in either order.
func TestApplyConsoleLevels(t *testing.T) {
	notice := "Notice: /Stage[main]/Main/Notify[n]/message: defined 'message' as 'n'\n"
	info := "Info: Applying configuration version '1'\n"
	for _, tc := range []struct {
		name        string
		flags       []string
		info, debug bool
	}{
		{"default", nil, false, false},
		{"verbose", []string{"--verbose"}, true, false},
		{"debug", []string{"--debug"}, true, true},
		{"debug then verbose", []string{"--debug", "--verbose"}, true, true},
		{"verbose then debug", []string{"--verbose", "--debug"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := buildBinary(t)
			dir := t.TempDir()
			cat := containedNotifyCatalog(t, dir)
			reports := filepath.Join(dir, "reports")
			stdout, stderr, code := run(t, bin, append([]string{"apply", "--catalog", cat, "--report-dir", reports}, tc.flags...)...)
			if code != 0 {
				t.Fatalf("exit = %d\n%s", code, stderr)
			}
			if !strings.Contains(stdout, notice) {
				t.Errorf("stdout lacks %q:\n%s", notice, stdout)
			}
			if got := strings.Contains(stdout, info); got != tc.info {
				t.Errorf("stdout has %q = %v, want %v:\n%s", info, got, tc.info, stdout)
			}
			if got := strings.Contains(stdout, "\nDebug: voxagent: ") || strings.HasPrefix(stdout, "Debug: voxagent: "); got != tc.debug {
				t.Errorf("stdout has a Debug line = %v, want %v:\n%s", got, tc.debug, stdout)
			}
			if got := lastLine(stderr); !strings.HasPrefix(got, "voxagent: outcome=") {
				t.Errorf("last stderr line = %q\n%s", got, stderr)
			}
			var printed []string
			for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
				level, rest, _ := strings.Cut(line, ": ")
				printed = append(printed, strings.ToLower(level)+" "+rest)
			}
			var held []string
			for _, l := range reportLogs(t, readReport(t, reports)) {
				// "<level> <source> <message>" against the console's "<Level>: [<source>: ]<message>".
				level, rest, _ := strings.Cut(l, " ")
				source, message, _ := strings.Cut(rest, " ")
				if source != "Puppet" {
					message = source + ": " + message
				}
				held = append(held, level+" "+message)
			}
			if !slices.Equal(printed, held) {
				t.Errorf("console and report differ:\nconsole:\n%s\nreport:\n%s", strings.Join(printed, "\n"), strings.Join(held, "\n"))
			}
		})
	}
}

// Review Focus 5: no --report-dir writes no report, and the console still
// shows the debug lines.
func TestApplyDebugWithoutReportDir(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := containedNotifyCatalog(t, dir)
	stdout, stderr, code := run(t, bin, "apply", "--catalog", cat, "--debug")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Debug: voxagent: voxagent ") || !strings.Contains(stdout, " apply noop=false level=debug; catalog 3 resources, code_id none\n") {
		t.Errorf("stdout lacks the run-start debug line:\n%s", stdout)
	}
	if want := "Debug: /Stage[main]/Main/Notify[n]: The container Class[Main] will propagate my refresh event\n"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks the transaction's debug line %q:\n%s", want, stdout)
	}
	if strings.Contains(stderr, "panic") {
		t.Errorf("stderr reports a panic:\n%s", stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "catalog.json" {
		t.Errorf("directory holds %v (err %v); want only the catalog", entries, err)
	}
}
