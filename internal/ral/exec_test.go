package ral

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
)

func execRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Exec", Title: title, Parameters: params}
}

// execChange applies an exec and returns the one Change it produced. A
// resource-level error here is a test failure by itself: an exec whose command
// RAN reports its outcome on the returns property, and a bare error would make
// the transaction write a resource_error naming no property.
func execChange(t *testing.T, r catalog.Resource) Change {
	t.Helper()
	changes, _, err := Exec{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	return changes[0]
}

func TestExecRunsCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	changes, _, err := Exec{}.Apply(context.Background(),
		execRes("touch", map[string]any{"command": "/usr/bin/touch " + marker}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("command did not run: %v", err)
	}
}

// A command that exits outside `returns` fails the RETURNS PROPERTY. It is not a
// resource-level error: Ruby attributes it, and a bare error would become a
// resource_error event naming no property.
//
// Measured against Puppet 8.10.0 in a container (the host Ruby cannot fork under
// Puppet), `exec { 'bad': command => '/bin/false', path => '/bin:/usr/bin' }`:
//
//	{"audited"=>false, "property"=>"returns", "previous_value"=>"notrun",
//	 "desired_value"=>["0"], "historical_value"=>nil,
//	 "message"=>"change from 'notrun' to ['0'] failed: '/bin/false' returned 1 instead of one of [0]",
//	 "name"=>"executed_command", "status"=>"failure", "corrective_change"=>false}
//
// The detail reaches the event unwrapped -- no "Could not set ... on returns:".
// Read in type/exec.rb sync: a mismatch calls self.fail, which raises a
// Puppet::Error, and property.rb:490's rescue re-raises a Puppet::Error
// untouched, wrapping only other StandardErrors at :492.
func TestExecNonZeroExitFailsTheReturnsProperty(t *testing.T) {
	ch := execChange(t, execRes("bad", map[string]any{"command": "/bin/false"}))
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a command that exited 1; the failure must be attributed to returns")
	}
	if ch.Property != "returns" {
		t.Errorf("Change.Property = %q, want \"returns\"", ch.Property)
	}
	if ch.Name != "executed_command" {
		t.Errorf("Change.Name = %q, want \"executed_command\"", ch.Name)
	}
	if ch.Was != "notrun" {
		t.Errorf("Change.Was = %#v, want \"notrun\"", ch.Was)
	}
	if got, ok := ch.Is.([]string); !ok || !slices.Equal(got, []string{"0"}) {
		t.Errorf("Change.Is = %#v, want []string{\"0\"} -- desired_value is the accepted returns list", ch.Is)
	}
	if got, want := ch.Err.Error(), "'/bin/false' returned 1 instead of one of [0]"; got != want {
		t.Errorf("Change.Err = %q, want %q", got, want)
	}
}

// The accepted list appears TWICE in one event, rendered two different ways, and
// both come from Ruby sources that cannot be collapsed into one:
//
//   - desired_value, and so the message's "to ['0', '3']", is the munged
//     property value -- type/exec.rb declares `munge(&:to_s)` with
//     :array_matching => :all, so the list is strings, and
//     format_value_for_display puts ", " between them.
//   - the detail's "instead of one of [0,3]" is `should.join(",")` in
//     type/exec.rb sync -- no space.
//
// Measured against Puppet 8.10.0, command `/bin/sh -c "exit 5"` with
// returns => [0, 3]:
//
//	"desired_value"=>["0", "3"],
//	"message"=>"change from 'notrun' to ['0', '3'] failed: '/bin/sh -c \"exit 5\"' returned 5 instead of one of [0,3]"
func TestExecFailureRendersTheReturnsListBothWays(t *testing.T) {
	ch := execChange(t, execRes("multi_fail", map[string]any{
		"command": "exit 5", "returns": []any{float64(0), float64(3)}}))
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for exit 5 with returns => [0, 3]")
	}
	if got, ok := ch.Is.([]string); !ok || !slices.Equal(got, []string{"0", "3"}) {
		t.Errorf("Change.Is = %#v, want []string{\"0\", \"3\"}", ch.Is)
	}
	if got, want := ch.Err.Error(), "'exit 5' returned 5 instead of one of [0,3]"; got != want {
		t.Errorf("Change.Err = %q, want %q -- Ruby joins the list with a bare comma here", got, want)
	}
}

// The failure detail carries no command output. Measured against Puppet 8.10.0,
// `command => '/bin/sh -c "echo hello; exit 7"'`: the event message is
//
//	"change from 'notrun' to ['0'] failed: '/bin/sh -c \"echo hello; exit 7\"' returned 7 instead of one of [0]"
//
// and "hello" appears only as its own notice log at the returns property, which
// type/exec.rb sync emits from the logoutput branch BEFORE it fails. Appending
// the output to the detail invents text Ruby's message does not hold, and for a
// chatty command it would bury the status in it.
func TestExecFailureDetailOmitsCommandOutput(t *testing.T) {
	// The output must not be findable in the detail, so it cannot be a literal
	// in the command: `echo hello` would put "hello" in the message as part of
	// the command text alone. A file the command reads separates the two.
	noisy := filepath.Join(t.TempDir(), "noisy")
	if err := os.WriteFile(noisy, []byte("SECRET_STDOUT\n"), 0o644); err != nil {
		t.Fatalf("seed output file: %v", err)
	}
	command := "cat " + noisy + "; exit 7"

	ch := execChange(t, execRes("noisy", map[string]any{"command": command}))
	if ch.Err == nil {
		t.Fatal("Change.Err is nil for a command that exited 7")
	}
	if strings.Contains(ch.Err.Error(), "SECRET_STDOUT") {
		t.Errorf("Change.Err = %q, want no command output in it", ch.Err)
	}
	if got, want := ch.Err.Error(), "'"+command+"' returned 7 instead of one of [0]"; got != want {
		t.Errorf("Change.Err = %q, want %q", got, want)
	}
}

func TestExecHonoursReturns(t *testing.T) {
	// returns => [1] makes exit 1 the success condition.
	ch := execChange(t, execRes("ok", map[string]any{
		"command": "/bin/false", "returns": []any{float64(1)}}))
	if ch.Err != nil {
		t.Errorf("Change.Err = %v for a declared-acceptable exit status", ch.Err)
	}
	// And a status NOT in the list still fails, so the list is a filter rather
	// than a blanket permission.
	ch = execChange(t, execRes("bad", map[string]any{
		"command": "/bin/true", "returns": []any{float64(3)}}))
	if ch.Err == nil {
		t.Error("exit 0 with returns => [3] reported no failure")
	}
}

func TestExecCreatesSkipsWhenPathExists(t *testing.T) {
	dir := t.TempDir()
	guard := filepath.Join(dir, "guard")
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(guard, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changes, _, err := Exec{}.Apply(context.Background(), execRes("guarded", map[string]any{
		"command": "/usr/bin/touch " + marker, "creates": guard,
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("Apply reported %d changes, want 0 when creates exists", len(changes))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran despite creates being satisfied")
	}
}

func TestExecOnlyifAndUnless(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		wantRun bool
	}{
		{"onlyif true runs", map[string]any{"onlyif": "/bin/true"}, true},
		{"onlyif false skips", map[string]any{"onlyif": "/bin/false"}, false},
		{"unless true skips", map[string]any{"unless": "/bin/true"}, false},
		{"unless false runs", map[string]any{"unless": "/bin/false"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			params := map[string]any{"command": "/usr/bin/touch " + marker}
			for k, v := range tt.params {
				params[k] = v
			}
			if _, _, err := (Exec{}).Apply(context.Background(), execRes("c", params), false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			_, statErr := os.Stat(marker)
			if ran := statErr == nil; ran != tt.wantRun {
				t.Errorf("command ran = %v, want %v", ran, tt.wantRun)
			}
		})
	}
}

// refreshonly => true means Apply does nothing and Refresh does the work. This
// is the path that reaches failed_to_restart.
func TestExecRefreshonly(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	r := execRes("on-refresh", map[string]any{
		"command": "/usr/bin/touch " + marker, "refreshonly": true,
	})
	changes, _, err := Exec{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("refreshonly Apply reported %d changes, want 0", len(changes))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("refreshonly command ran during Apply")
	}
	refreshed, err := Exec{}.Refresh(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if !refreshed {
		t.Error("Refresh reported no refresh")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("refresh did not run the command: %v", err)
	}
}

// A failing refresh is what sets failed_to_restart. In Ruby it sets failed too
// (event_manager.rb calls fail_with_event in the same rescue), so the two 0x4
// clauses always fire together rather than one being reachable alone.
func TestExecFailingRefreshIsAnError(t *testing.T) {
	r := execRes("bad", map[string]any{"command": "/bin/false", "refreshonly": true})
	refreshed, err := Exec{}.Refresh(context.Background(), r, false)
	if err == nil {
		t.Fatal("Refresh succeeded on a failing command; want an error")
	}
	if refreshed {
		t.Error("Refresh reported success alongside an error")
	}
}

func TestExecNoopRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	changes, _, err := Exec{}.Apply(context.Background(),
		execRes("c", map[string]any{"command": "/usr/bin/touch " + marker}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("noop Apply reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("noop Apply ran the command")
	}
}

// Refresh under noop must also run nothing. The refresh path is separate code
// from Apply and would not inherit the guard.
func TestExecNoopRefreshRunsNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r := execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker, "refreshonly": true})
	if _, err := (Exec{}).Refresh(context.Background(), r, true); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("noop Refresh ran the command")
	}
}

func TestExecRequiresCommand(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(), execRes("c", nil), false); err == nil {
		t.Fatal("Apply accepted an exec with no command; want an error")
	}
}

// A guard that cannot be started at all is an error, not a false. "We could not
// check" is a different claim from "the condition does not hold", and treating
// the first as the second would silently skip the command.
func TestExecUnrunnableGuardIsAnError(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"cwd":     "/no/such/directory",
		"onlyif":  "/bin/true",
	}), false)
	if err == nil {
		t.Error("Apply succeeded when the guard could not be started; want an error")
	}
}

// --- Array-valued parameters ---
//
// Measured against a real compiled catalog: exec { onlyif => [...], path =>
// [...] } produces []any parameters, not strings. Puppet's semantics (type/
// exec.rb:657-675, check_all_attributes) wrap a scalar into a one-element
// array so scalar and array behave identically:
//
//	onlyif   passes when EVERY entry exits 0
//	unless   passes when EVERY entry exits non-zero
//	creates  passes when EVERY entry is missing
//	path     joins with ':' (exec.rb:223-228), same as File::PATH_SEPARATOR

func TestExecPathArrayJoinsWithColon(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "helper",
		"path":    []any{dir, "/usr/bin"},
	}), false)
	if err != nil {
		t.Errorf("Apply returned error with path => array containing the script's directory: %v", err)
	}
}

func TestExecOnlyifArrayRequiresAll(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  []any{"/bin/true", "/bin/false"},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran though one onlyif array entry failed")
	}
}

func TestExecOnlyifArrayAllPassRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  []any{"/bin/true", "/bin/true"},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("command did not run though every onlyif array entry passed")
	}
}

func TestExecUnlessArrayRequiresAllNonZero(t *testing.T) {
	// unless only runs the command when EVERY entry exits non-zero; one entry
	// exiting 0 must skip it.
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"unless":  []any{"/bin/false", "/bin/true"},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran though one unless array entry exited 0")
	}
}

func TestExecUnlessArrayAllNonZeroRuns(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"unless":  []any{"/bin/false", "/bin/false"},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("command did not run though every unless array entry exited non-zero")
	}
}

func TestExecCreatesArrayRequiresAllMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	exists := filepath.Join(dir, "exists")
	marker := filepath.Join(dir, "ran")
	if err := os.WriteFile(exists, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// missing comes first so a mutant that only inspects the first entry sees
	// "missing" and would (wrongly) run the command.
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"creates": []any{missing, exists},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran though one creates array entry already exists")
	}
}

func TestExecCreatesArrayAllMissingRuns(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	marker := filepath.Join(dir, "ran")
	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"creates": []any{a, b},
	}), false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("command did not run though every creates array entry was missing")
	}
}

func TestExecCommandArrayIsRefused(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": []any{"/bin/echo", "hi"}}), false); err == nil {
		t.Error("Apply accepted an array command; want an error naming the type")
	}
}

func TestExecCwdArrayIsRefused(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "/bin/true", "cwd": []any{"/tmp"}}), false); err == nil {
		t.Error("Apply accepted an array cwd; want an error naming the type")
	}
}

// --- Refresh must not assert a refresh that never happened ---

func TestExecRefreshSkippedByGuardReportsNoRefresh(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r := execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  "/bin/false",
	})
	refreshed, err := Exec{}.Refresh(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if refreshed {
		t.Error("Refresh reported a refresh though the onlyif guard skipped the command")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("command ran though the onlyif guard should have skipped it")
	}
}

// --- noop must evaluate guards before reporting ---
//
// noop promises not to change state. Running a read-only onlyif/unless/
// creates check is not a change, so noop can and must still evaluate guards --
// otherwise it cannot honestly report "no change" for a resource whose guard
// would have skipped it anyway.

func TestExecNoopEvaluatesGuardsBeforeReporting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")

	changes, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  "/bin/false",
	}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("noop with a failing onlyif reported %d changes, want 0 (guards must run under noop)", len(changes))
	}

	changes, _, err = Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  "/bin/true",
	}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("noop with a passing onlyif reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("noop Apply ran the command")
	}
}

// --- path must reach the child's environment ---

func TestExecPathReachesEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "voxagent-exec-test-helper")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	// Without path, the bare name is not resolvable: the script's directory is
	// not on any default PATH. The shell exits 127, which is a returns failure
	// like any other unacceptable status.
	if ch := execChange(t, execRes("c", map[string]any{"command": "voxagent-exec-test-helper"})); ch.Err == nil {
		t.Error("Apply resolved a bare command name with no path set; want a failed returns change")
	}

	// With path supplying the directory, the same bare name resolves.
	if _, _, err := (Exec{}).Apply(context.Background(), execRes("c", map[string]any{
		"command": "voxagent-exec-test-helper",
		"path":    dir,
	}), false); err != nil {
		t.Errorf("Apply returned error with path supplying the script's directory: %v", err)
	}
}

// --- returns: pin the already-correct scalar/string/exit-code handling ---

func TestExecReturnsScalarAcceptsOnlyDeclaredValue(t *testing.T) {
	// returns => 2 (a bare JSON number, decoded to float64) means exit 2 is the
	// only acceptable status -- not "0 is the only acceptable status", which is
	// what a hardcoded [0] fallback for non-array, non-nil returns would give.
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "exit 2", "returns": float64(2)}), false); err != nil {
		t.Errorf("Apply returned error for exit 2 with returns => 2: %v", err)
	}
	ch := execChange(t, execRes("c", map[string]any{"command": "exit 0", "returns": float64(2)}))
	if ch.Err == nil {
		t.Error("exit 0 with returns => 2 reported no failure")
	}
}

func TestExecReturnsStringIsParsed(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "exit 2", "returns": "2"}), false); err != nil {
		t.Errorf("Apply returned error for exit 2 with returns => '2': %v", err)
	}
}

func TestExecReturnsReportsActualExitStatus(t *testing.T) {
	ch := execChange(t, execRes("c", map[string]any{"command": "exit 2", "returns": []any{float64(3)}}))
	if ch.Err == nil {
		t.Fatal("exit 2 with returns => [3] reported no failure")
	}
	if !strings.Contains(ch.Err.Error(), "returned 2") {
		t.Errorf("Change.Err = %q, want it to report the actual exit status 2, not a hardcoded 1", ch.Err)
	}
}

// The event a SUCCESSFUL exec writes, measured against Puppet 8.10.0. Two
// fixtures, because one cannot tell the actual exit status from the accepted
// list when they are the same:
//
//	exec { 'ok': command => '/bin/true' }
//	  ..."previous_value"=>"notrun", "desired_value"=>["0"],
//	     "message"=>"executed successfully", "name"=>"executed_command",
//	     "status"=>"success"
//
//	exec { 'multi_ok': command => '/bin/sh -c "exit 3"', returns => [0, 3] }
//	  ..."desired_value"=>["0", "3"], "message"=>"executed successfully"
//
// The second settles it: the command exited 3, and desired_value is the whole
// accepted list, not the status. Reporting the status as desired_value would
// make every exec event claim the run's outcome was what the catalog asked for.
//
// The message is type/exec.rb's change_to_s, read at the returns property:
// `_("executed successfully")` -- it names neither the command nor the status.
func TestExecSuccessEventMatchesPuppetsExecutedCommand(t *testing.T) {
	for _, tt := range []struct {
		name   string
		params map[string]any
		wantIs []string
	}{
		{"default returns", map[string]any{"command": "/bin/true"}, []string{"0"}},
		{"declared returns list", map[string]any{
			"command": "exit 3", "returns": []any{float64(0), float64(3)}}, []string{"0", "3"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ch := execChange(t, execRes("c", tt.params))
			if ch.Err != nil {
				t.Fatalf("Change.Err = %v, want nil", ch.Err)
			}
			if ch.Property != "returns" {
				t.Errorf("Change.Property = %q, want \"returns\"", ch.Property)
			}
			if ch.Name != "executed_command" {
				t.Errorf("Change.Name = %q, want \"executed_command\"", ch.Name)
			}
			if ch.Was != "notrun" {
				t.Errorf("Change.Was = %#v, want \"notrun\"", ch.Was)
			}
			if got, ok := ch.Is.([]string); !ok || !slices.Equal(got, tt.wantIs) {
				t.Errorf("Change.Is = %#v, want %#v -- desired_value is the accepted returns list", ch.Is, tt.wantIs)
			}
			if ch.Message != "executed successfully" {
				t.Errorf("Change.Message = %q, want \"executed successfully\"", ch.Message)
			}
		})
	}
}

// A catalog problem is not a property that moved. These inputs never produce a
// usable returns comparison at all, so they stay resource-level errors -- the
// transaction's resource_error path -- rather than being blamed on returns.
func TestExecResourceLevelFailuresStayBareErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		params map[string]any
	}{
		{"no command", nil},
		{"command is not a string", map[string]any{"command": []any{"/bin/echo", "hi"}}},
		{"command is empty", map[string]any{"command": ""}},
		{"returns is unparseable", map[string]any{"command": "/bin/true", "returns": "abc"}},
		{"returns is an empty list", map[string]any{"command": "/bin/true", "returns": []any{}}},
		{"creates is not a string", map[string]any{"command": "/bin/true", "creates": []any{1}}},
		{"guard cannot be started", map[string]any{
			"command": "/bin/true", "cwd": "/no/such/directory", "onlyif": "/bin/true"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changes, _, err := Exec{}.Apply(context.Background(), execRes("c", tt.params), false)
			if err == nil {
				t.Fatal("Apply returned no error; want a bare resource-level error")
			}
			if changes != nil {
				t.Errorf("Apply also reported %d changes; a catalog problem must not be attributed to a property",
					len(changes))
			}
		})
	}
}

// --- returns: unparseable values must be a resource error, not an invented policy ---

func TestExecReturnsUnparseableStringIsAnError(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "/bin/true", "returns": "abc"}), false); err == nil {
		t.Error("Apply accepted returns => 'abc'; want an error naming the unparseable value")
	}
}

func TestExecReturnsUnparseableArrayElementIsAnError(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "/bin/true", "returns": []any{true}}), false); err == nil {
		t.Error("Apply accepted returns => [true]; want an error naming the unparseable value")
	}
}

func TestExecReturnsEmptyArrayIsAnError(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		execRes("c", map[string]any{"command": "/bin/true", "returns": []any{}}), false); err == nil {
		t.Error("Apply accepted returns => []; want an error rather than inventing an acceptable-status policy")
	}
}

// --- an unreadable creates path must be an error, not "missing" ---

func TestExecCreatesStatErrorIsNotTreatedAsMissing(t *testing.T) {
	if os.Getuid() == 0 {
		// The suite also runs as root via task dev:test-privileged. Root
		// bypasses the traversal permission bit this fixture relies on to
		// produce EACCES, so a chmod-based fixture cannot create the
		// condition there. Skip explicitly rather than passing quietly.
		t.Skip("running as root: permission bits cannot produce EACCES for this fixture")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0o755) })
	target := filepath.Join(blocked, "path")
	marker := filepath.Join(dir, "ran")

	_, _, err := Exec{}.Apply(context.Background(), execRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"creates": target,
	}), false)
	if err == nil {
		t.Error("Apply succeeded though the creates path could not be stat'ed (EACCES); want an error")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("command ran though the creates guard could not be evaluated")
	}
}

// --- a cancelled context must stop a running command rather than hang ---

func TestExecContextCancellationStopsCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// `sleep 5 & wait`, not a bare `sleep 5`, so the shell is FORCED to fork
	// whatever shell it is.
	//
	// A bare `sleep 5` is optimised into an exec by bash, which makes the
	// process Go started the sleep itself -- so killing just the shell happens
	// to work and this test passes whether or not the code is right. It only
	// failed once the suite was run in a container where /bin/sh is dash, which
	// forks. Backgrounding plus wait forks under every shell, so the test now
	// pins process-group cancellation rather than pinning which shell the host
	// happens to ship.
	start := time.Now()
	changes, _, err := Exec{}.Apply(ctx, execRes("c", map[string]any{"command": "sleep 5 & wait"}), false)
	elapsed := time.Since(start)

	// The killed shell reports a signal rather than an acceptable status, so the
	// outcome arrives as a failed returns change. Ruby's own timeout path is
	// also a returns failure -- type/exec.rb sync rescues Timeout::Error and
	// calls self.fail -- though its detail reads "Command exceeded timeout",
	// because it times the command itself rather than being cancelled from
	// outside. voxagent has no timeout attribute to reproduce that wording.
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}
	if len(changes) != 1 || changes[0].Err == nil {
		t.Errorf("Apply reported %+v; want one returns change carrying the failure", changes)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Apply took %v to return after a 50ms context deadline; want it to return promptly", elapsed)
	}
}

// --- noop must not fabricate an exit code for a command that never ran ---

func TestExecNoopChangeDoesNotFabricateExitCode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	changes, _, err := Exec{}.Apply(context.Background(),
		execRes("c", map[string]any{"command": "/usr/bin/touch " + marker}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	if changes[0].Is == "0" {
		t.Error(`noop Change.Is == "0" claims an exit status that has not happened; the command never ran`)
	}
	// The event name is the property's :event option, so it does not depend on
	// whether the command ran. Measured under --noop:
	//
	//	{"previous_value"=>"notrun", "desired_value"=>["0"],
	//	 "message"=>"executed successfully (noop)", "name"=>"executed_command",
	//	 "status"=>"noop"}
	//
	// Only the name is reproduced. exec.go's noop branch records why
	// desired_value and the message deliberately still differ.
	if changes[0].Name != "executed_command" {
		t.Errorf("noop Change.Name = %q, want \"executed_command\"", changes[0].Name)
	}
}

// An exec autorequires the managed files its command names.
//
// Measured against Puppet 8.10.0, type/exec.rb autorequire(:file): cwd if set,
// plus every absolute path found by scanning command, onlyif and unless with
// /^(\/\S+)/ and /^"([^"]+)"/. Only managed paths become edges -- autorequire is
// soft, and a reference to a file the catalog does not manage is silently no
// edge rather than an error.
func TestExecAutoRequiresTheFilesItNames(t *testing.T) {
	for _, tt := range []struct {
		name    string
		params  map[string]any
		managed []string
		want    []string
	}{
		{
			name:    "the command's own binary",
			params:  map[string]any{"command": "/usr/bin/touch /opt/spine/reloaded"},
			managed: []string{"File[/usr/bin/touch]"},
			want:    []string{"File[/usr/bin/touch]"},
		},
		{
			name:    "cwd",
			params:  map[string]any{"command": "make", "cwd": "/opt/spine"},
			managed: []string{"File[/opt/spine]"},
			want:    []string{"File[/opt/spine]"},
		},
		{
			name:    "a quoted command path",
			params:  map[string]any{"command": `"/opt/my tool/run" --now`},
			managed: []string{"File[/opt/my tool/run]"},
			want:    []string{"File[/opt/my tool/run]"},
		},
		{
			name: "the guards too",
			params: map[string]any{
				"command": "make", "onlyif": "/usr/bin/test -f /tmp/x",
				"unless": "/usr/bin/false",
			},
			managed: []string{"File[/usr/bin/test]", "File[/usr/bin/false]"},
			want:    []string{"File[/usr/bin/test]", "File[/usr/bin/false]"},
		},
		{
			name:    "an unmanaged path is no edge",
			params:  map[string]any{"command": "/usr/bin/touch /tmp/x"},
			managed: nil,
			want:    nil,
		},
		{
			// Only the START of the string is scanned, so an argument that
			// happens to be a path is not a dependency. Puppet anchors with ^.
			name:    "an argument is not a dependency",
			params:  map[string]any{"command": "/bin/cp /etc/src /etc/dst"},
			managed: []string{"File[/bin/cp]", "File[/etc/src]", "File[/etc/dst]"},
			want:    []string{"File[/bin/cp]"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			managed := map[string]bool{}
			for _, ref := range tt.managed {
				managed[ref] = true
			}
			got := Exec{}.AutoRequires(
				catalog.Resource{Type: "Exec", Title: "e", Parameters: tt.params},
				func(ref string) bool { return managed[ref] },
			)
			if !slices.Equal(got, tt.want) {
				t.Errorf("AutoRequires = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Validate refuses what Puppet refuses while converting the catalog ---

// Messages are read from Puppet 8.10.0 source, not measured: cwd and creates
// from parameter/path.rb validate_path, wrapped by type.rb:692's "Parameter
// %{name} failed on %{ref}"; the command from provider/exec.rb validatecmd,
// called by type/exec.rb:589 and wrapped by type.rb:2360's "Validation of
// %{ref} failed".
func TestExecValidate(t *testing.T) {
	const unqualified = "Validation of Exec[x] failed: '%s' is not qualified and no path was specified. Please qualify the command or specify a path."
	for _, tc := range []struct {
		name   string
		title  string
		params map[string]any
		want   string // "" means valid
	}{
		{"qualified command", "x", map[string]any{"command": "/bin/true"}, ""},
		{"unqualified command with path", "x", map[string]any{"command": "touch m", "path": "/usr/bin"}, ""},
		{"unqualified command with path array", "x", map[string]any{"command": "touch m", "path": []any{"/usr/bin"}}, ""},
		{"unqualified command, no path", "x", map[string]any{"command": "touch m"},
			fmt.Sprintf(unqualified, "touch")},
		// extractexe: a quoted first word is the executable, spaces and all.
		{"double-quoted qualified", "x", map[string]any{"command": `"/opt/my app/run" --flag`}, ""},
		{"single-quoted qualified", "x", map[string]any{"command": `'/opt/my app/run' --flag`}, ""},
		{"double-quoted unqualified", "x", map[string]any{"command": `"my app" --flag`},
			fmt.Sprintf(unqualified, "my app")},
		// Without a match the executable is everything before the first space,
		// so a leading space leaves an empty, unqualified executable.
		{"leading space", "x", map[string]any{"command": " /bin/true"},
			fmt.Sprintf(unqualified, "")},
		// Ruby's ^ matches at every line start, so a quoted path opening a
		// later line is the executable extractexe finds.
		{"quoted path on a later line", "x", map[string]any{"command": "true\n\"/bin/true\""}, ""},
		// The command is the namevar: absent the parameter, the title is it.
		{"title as command, unqualified", "x", map[string]any{},
			fmt.Sprintf(unqualified, "x")},
		{"title as command, qualified", "/bin/true", map[string]any{}, ""},
		{"array command, qualified", "x", map[string]any{"command": []any{"/bin/echo", "hi"}}, ""},
		{"array command, unqualified", "x", map[string]any{"command": []any{"echo", "hi"}},
			fmt.Sprintf(unqualified, "echo")},

		{"absolute cwd", "x", map[string]any{"command": "/bin/true", "cwd": "/tmp"}, ""},
		{"relative cwd", "x", map[string]any{"command": "/bin/true", "cwd": "rel"},
			"Parameter cwd failed on Exec[x]: cwd must be a fully qualified path"},
		{"empty cwd", "x", map[string]any{"command": "/bin/true", "cwd": ""},
			"Parameter cwd failed on Exec[x]: cwd must be a fully qualified path"},
		{"array cwd", "x", map[string]any{"command": "/bin/true", "cwd": []any{"/tmp"}},
			"Parameter cwd failed on Exec[x]: cwd only accepts a single path, not an array of paths"},
		{"non-string cwd", "x", map[string]any{"command": "/bin/true", "cwd": 42},
			"Parameter cwd failed on Exec[x]: cwd must be a fully qualified path"},

		{"absolute creates", "x", map[string]any{"command": "/bin/true", "creates": "/tmp/m"}, ""},
		{"absolute creates array", "x", map[string]any{"command": "/bin/true", "creates": []any{"/tmp/a", "/tmp/b"}}, ""},
		{"relative creates", "x", map[string]any{"command": "/bin/true", "creates": "m"},
			"Parameter creates failed on Exec[x]: creates must be a fully qualified path"},
		{"relative creates in array", "x", map[string]any{"command": "/bin/true", "creates": []any{"/tmp/a", "b"}},
			"Parameter creates failed on Exec[x]: creates must be a fully qualified path"},
		{"non-string creates element", "x", map[string]any{"command": "/bin/true", "creates": []any{"/tmp/a", 42}},
			"Parameter creates failed on Exec[x]: creates must be a fully qualified path"},

		// Puppet sets cwd before creates (allattrs order) and validates the
		// command after every parameter, so the first failure is reported.
		{"cwd before creates", "x", map[string]any{"command": "touch m", "cwd": "rel", "creates": "m"},
			"Parameter cwd failed on Exec[x]: cwd must be a fully qualified path"},
		{"creates before command", "x", map[string]any{"command": "touch m", "creates": "m"},
			"Parameter creates failed on Exec[x]: creates must be a fully qualified path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Exec{}.Validate(execRes(tc.title, tc.params))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Errorf("Validate = nil, want %q", tc.want)
			case tc.want != "" && err.Error() != tc.want:
				t.Errorf("Validate =\n %q\nwant\n %q", err.Error(), tc.want)
			}
		})
	}
}
