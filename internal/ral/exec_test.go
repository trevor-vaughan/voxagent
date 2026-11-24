package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
)

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
		raltest.ExecRes("touch", map[string]any{"command": "/usr/bin/touch " + marker}), false)
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
	ch := execChange(t, raltest.ExecRes("bad", map[string]any{"command": "/bin/false"}))
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
	ch := execChange(t, raltest.ExecRes("multi_fail", map[string]any{
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

	ch := execChange(t, raltest.ExecRes("noisy", map[string]any{"command": command}))
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
	ch := execChange(t, raltest.ExecRes("ok", map[string]any{
		"command": "/bin/false", "returns": []any{float64(1)}}))
	if ch.Err != nil {
		t.Errorf("Change.Err = %v for a declared-acceptable exit status", ch.Err)
	}
	// And a status NOT in the list still fails, so the list is a filter rather
	// than a blanket permission.
	ch = execChange(t, raltest.ExecRes("bad", map[string]any{
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
	changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("guarded", map[string]any{
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
			if _, _, err := (Exec{}).Apply(context.Background(), raltest.ExecRes("c", params), false); err != nil {
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
	r := raltest.ExecRes("on-refresh", map[string]any{
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
	r := raltest.ExecRes("bad", map[string]any{"command": "/bin/false", "refreshonly": true})
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
		raltest.ExecRes("c", map[string]any{"command": "/usr/bin/touch " + marker}), true)
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
	r := raltest.ExecRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker, "refreshonly": true})
	if _, err := (Exec{}).Refresh(context.Background(), r, true); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("noop Refresh ran the command")
	}
}

func TestExecRequiresCommand(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(), raltest.ExecRes("c", nil), false); err == nil {
		t.Fatal("Apply accepted an exec with no command; want an error")
	}
}

// A guard that cannot be started at all is an error, not a false. "We could not
// check" is a different claim from "the condition does not hold", and treating
// the first as the second would silently skip the command.
func TestExecUnrunnableGuardIsAnError(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
		"command": "helper",
		"path":    []any{dir, "/usr/bin"},
	}), false)
	if err != nil {
		t.Errorf("Apply returned error with path => array containing the script's directory: %v", err)
	}
}

func TestExecOnlyifArrayRequiresAll(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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

func TestExecCwdArrayIsRefused(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "/bin/true", "cwd": []any{"/tmp"}}), false); err == nil {
		t.Error("Apply accepted an array cwd; want an error naming the type")
	}
}

// --- A refresh a guard blocks still happened ---
//
// type/exec.rb:690-699's refresh is `if check_all_attributes(true) ... end`:
// it returns normally whether or not a check passed, and process_callback
// counts any normal return as a refresh. Measured on a direct edge for
// unless, unless with refreshonly, and onlyif: `Triggered 'refresh' from 1 event`, a refresh event, restarted, and
// the command not run.

func TestExecRefreshBlockedByAGuardStillTriggers(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		guard map[string]any
	}{
		{"onlyif", map[string]any{"onlyif": "/bin/false"}},
		{"unless", map[string]any{"unless": "/bin/true"}},
		{"unless refreshonly", map[string]any{"unless": "/bin/true", "refreshonly": true}},
		{"creates", map[string]any{"creates": dir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(dir, "ran")
			params := map[string]any{"command": "/usr/bin/touch " + marker}
			for k, v := range tc.guard {
				params[k] = v
			}
			refreshed, err := Exec{}.Refresh(context.Background(), raltest.ExecRes("c", params), false)
			if err != nil {
				t.Fatalf("Refresh returned error: %v", err)
			}
			if !refreshed {
				t.Error("Refresh reported no refresh; Puppet triggers one when a guard blocks the command")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("command ran though the guard should have blocked it")
			}
		})
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

	changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
		"command": "/usr/bin/touch " + marker,
		"onlyif":  "/bin/false",
	}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("noop with a failing onlyif reported %d changes, want 0 (guards must run under noop)", len(changes))
	}

	changes, _, err = Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	if ch := execChange(t, raltest.ExecRes("c", map[string]any{"command": "voxagent-exec-test-helper"})); ch.Err == nil {
		t.Error("Apply resolved a bare command name with no path set; want a failed returns change")
	}

	// With path supplying the directory, the same bare name resolves.
	if _, _, err := (Exec{}).Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
		raltest.ExecRes("c", map[string]any{"command": "exit 2", "returns": float64(2)}), false); err != nil {
		t.Errorf("Apply returned error for exit 2 with returns => 2: %v", err)
	}
	ch := execChange(t, raltest.ExecRes("c", map[string]any{"command": "exit 0", "returns": float64(2)}))
	if ch.Err == nil {
		t.Error("exit 0 with returns => 2 reported no failure")
	}
}

func TestExecReturnsStringIsParsed(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "exit 2", "returns": "2"}), false); err != nil {
		t.Errorf("Apply returned error for exit 2 with returns => '2': %v", err)
	}
}

func TestExecReturnsReportsActualExitStatus(t *testing.T) {
	ch := execChange(t, raltest.ExecRes("c", map[string]any{"command": "exit 2", "returns": []any{float64(3)}}))
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
			ch := execChange(t, raltest.ExecRes("c", tt.params))
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
		{"command is not a string or an array", map[string]any{"command": float64(5)}},
		{"command is empty", map[string]any{"command": ""}},
		{"returns is unparseable", map[string]any{"command": "/bin/true", "returns": "abc"}},
		{"returns is an empty list", map[string]any{"command": "/bin/true", "returns": []any{}}},
		{"creates is not a string", map[string]any{"command": "/bin/true", "creates": []any{1}}},
		{"guard cannot be started", map[string]any{
			"command": "/bin/true", "cwd": "/no/such/directory", "onlyif": "/bin/true"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", tt.params), false)
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
		raltest.ExecRes("c", map[string]any{"command": "/bin/true", "returns": "abc"}), false); err == nil {
		t.Error("Apply accepted returns => 'abc'; want an error naming the unparseable value")
	}
}

func TestExecReturnsUnparseableArrayElementIsAnError(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "/bin/true", "returns": []any{true}}), false); err == nil {
		t.Error("Apply accepted returns => [true]; want an error naming the unparseable value")
	}
}

func TestExecReturnsEmptyArrayIsAnError(t *testing.T) {
	if _, _, err := (Exec{}).Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "/bin/true", "returns": []any{}}), false); err == nil {
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

	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
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
	changes, _, err := Exec{}.Apply(ctx, raltest.ExecRes("c", map[string]any{"command": "sleep 5 & wait"}), false)
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

// `command => '/opt/app/run &'` leaves a child holding the command's stdout.
// Puppet returns when the shell it started exits (util/execution.rb:236-258)
// and the exec succeeds. Measured before the fix: the output pipe stayed open,
// WaitDelay expired after a second and the command -- which had exited 0 --
// failed with "exec: WaitDelay expired before I/O complete". The same holds
// for a guard, which then made the whole resource fail.
func TestExecReturnsWhenABackgroundedChildInheritsStdout(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleep.pid")
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(field); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	background := fmt.Sprintf(`sleep 30 & echo $! >> %s`, pidFile)

	for name, params := range map[string]map[string]any{
		"command": {"command": background},
		"onlyif":  {"command": "/bin/true", "onlyif": background},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			change := execChange(t, raltest.ExecRes("c", params))
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Errorf("Apply took %v; want it to return when the shell exits", elapsed)
			}
			if change.Err != nil {
				t.Errorf("returns change failed: %v", change.Err)
			}
		})
	}
}

// --- noop must not fabricate an exit code for a command that never ran ---

func TestExecNoopChangeDoesNotFabricateExitCode(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	changes, _, err := Exec{}.Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "/usr/bin/touch " + marker}), true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply reported %d changes, want 1", len(changes))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("noop ran the command")
	}
	// Measured under --noop (and again by G5's concat M13 noop pass):
	//
	//	{"previous_value"=>"notrun", "desired_value"=>["0"],
	//	 "message"=>"executed successfully (noop)", "name"=>"executed_command",
	//	 "status"=>"noop"}
	//
	// desired_value is the should value, the accepted list, whether or not the
	// command ran; no exit status is claimed.
	want := Change{Property: "returns", Name: "executed_command", Was: "notrun",
		Is: []string{"0"}, Message: "executed successfully (noop)"}
	if !reflect.DeepEqual(changes[0], want) {
		t.Errorf("noop change = %+v, want %+v", changes[0], want)
	}
	changes, _, err = Exec{}.Apply(context.Background(),
		raltest.ExecRes("c", map[string]any{"command": "/usr/bin/touch " + marker, "returns": []any{0.0, 3.0}}), true)
	if err != nil || len(changes) != 1 || !reflect.DeepEqual(changes[0].Is, []string{"0", "3"}) {
		t.Errorf("noop with returns [0, 3] = %+v, %v; want desired [0 3]", changes, err)
	}
}

// An exec autorequires the managed files its command names.
//
// Read from Puppet 8.10.0, type/exec.rb:593-640 autorequire(:file): cwd if set,
// every path /^(\/\S+)/ and /^"([^"]+)"/ find in the command, and every path
// /^(\/\S+)/ alone finds in each onlyif and unless command. The quoted pattern
// is the command's only, measured. Only
// managed paths become edges -- autorequire is soft, and a reference to a file
// the catalog does not manage is silently no edge rather than an error.
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
			name:    "a guard's quoted path is no edge",
			params:  map[string]any{"command": "make", "onlyif": `"/opt/q" x`},
			managed: []string{"File[/opt/q]"},
			want:    nil,
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

		// onlyif and unless are check parameters whose validate block runs
		// validatecmd on each command (type/exec.rb:497, :560), so the failure
		// carries the Parameter wrapper, not the type-level one. creates sets
		// accept_arrays (:448), so the array rows above are accepted.
		{"unqualified onlyif, no path", "x", map[string]any{"command": "/bin/true", "onlyif": "test -f /x"},
			"Parameter onlyif failed on Exec[x]: 'test' is not qualified and no path was specified. Please qualify the command or specify a path."},
		{"unqualified unless in array, no path", "x", map[string]any{"command": "/bin/true", "unless": []any{"/bin/true", "grep x /y"}},
			"Parameter unless failed on Exec[x]: 'grep' is not qualified and no path was specified. Please qualify the command or specify a path."},
		{"unqualified onlyif as nested array", "x", map[string]any{"command": "/bin/true", "onlyif": []any{[]any{"test", "-f", "/x"}}},
			"Parameter onlyif failed on Exec[x]: 'test' is not qualified and no path was specified. Please qualify the command or specify a path."},
		{"qualified onlyif as nested array", "x", map[string]any{"command": "/bin/true", "onlyif": []any{[]any{"/usr/bin/test", "-f", "/x"}}}, ""},
		{"unqualified onlyif with path", "x", map[string]any{"command": "/bin/true", "onlyif": "test -f /x", "path": "/usr/bin"}, ""},
		{"qualified onlyif", "x", map[string]any{"command": "/bin/true", "onlyif": "/usr/bin/test -f /x"}, ""},
		// Declaration order is creates, unless, onlyif; the command goes last.
		{"creates before unless", "x", map[string]any{"command": "/bin/true", "creates": "m", "unless": "test -f /x"},
			"Parameter creates failed on Exec[x]: creates must be a fully qualified path"},
		{"unless before onlyif", "x", map[string]any{"command": "/bin/true", "unless": "grep x", "onlyif": "test -f /x"},
			"Parameter unless failed on Exec[x]: 'grep' is not qualified and no path was specified. Please qualify the command or specify a path."},
		{"onlyif before command", "x", map[string]any{"command": "touch m", "onlyif": "test -f /x"},
			"Parameter onlyif failed on Exec[x]: 'test' is not qualified and no path was specified. Please qualify the command or specify a path."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Exec{}.Validate(raltest.ExecRes(tc.title, tc.params))
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

// --- argv-array commands ---

// Every element is one argument and no shell sees any of them.
func TestExecArgvNeverUsesAShell(t *testing.T) {
	dir := t.TempDir()
	weird := []string{"a b;c", "$(id)", "`id`", `"q"`, "it's", ">x"}
	argv := []any{"/usr/bin/touch"}
	for _, w := range weird {
		argv = append(argv, filepath.Join(dir, w))
	}
	changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("t", map[string]any{"command": argv}), false)
	if err != nil || len(changes) != 1 || changes[0].Err != nil || changes[0].Message != "executed successfully" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	for _, w := range weird {
		if _, err := os.Stat(filepath.Join(dir, w)); err != nil {
			t.Errorf("%q was not passed as one literal argument: %v", w, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Error("a redirection ran: a shell saw the argv")
	}
	out := filepath.Join(dir, "empty")
	if _, _, err := (Exec{}).Apply(context.Background(), raltest.ExecRes("e", map[string]any{"command": []any{
		"/bin/sh", "-c", `printf '[%s]' "$1" > "$2"`, "sh", "", out}}), false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "[]" {
		t.Errorf("empty argument arrived as %q, want []", b)
	}
}

// The failure names the command as Array#inspect of the original value.
func TestExecArgvFailureRendersArrayInspect(t *testing.T) {
	for _, tt := range []struct {
		command []any
		want    string
	}{
		{[]any{"/bin/sh", "-c", "echo out; exit 3"},
			`'["/bin/sh", "-c", "echo out; exit 3"]' returned 3 instead of one of [0]`},
		{[]any{"/bin/sh", "-c", "exit 9", float64(1), true},
			`'["/bin/sh", "-c", "exit 9", 1, true]' returned 9 instead of one of [0]`},
		{[]any{"/bin/sh", []any{"-c", "exit 9"}},
			`'["/bin/sh", ["-c", "exit 9"]]' returned 9 instead of one of [0]`},
	} {
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("f", map[string]any{"command": tt.command}), false)
		if err != nil || len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != tt.want {
			t.Errorf("%v: %+v, %v; want Err %q", tt.command, changes, err, tt.want)
		}
	}
}

// checkexe's three texts, and its measured corners: File.ftype is an lstat,
// so a link to a directory is a "link", and a path File.expand_path changes
// is not absolute, so with a path it is which's, and without one it is not
// found.
func TestExecArgvCheckExe(t *testing.T) {
	dir := t.TempDir()
	noexec := filepath.Join(dir, "noexec")
	if err := os.WriteFile(noexec, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"command": []any{"nosuchcmd", "x"}, "path": "/usr/bin"}, "Could not find command 'nosuchcmd'"},
		{map[string]any{"command": []any{"/no/such", "x"}}, "Could not find command '/no/such'"},
		{map[string]any{"command": []any{dir, "x"}}, "'" + dir + "' is a directory, not a file"},
		{map[string]any{"command": []any{noexec}}, "'" + noexec + "' is not executable"},
		{map[string]any{"command": []any{"/bin/echo hi"}}, "Could not find command '/bin/echo hi'"},
		{map[string]any{"command": []any{link, "x"}}, "'" + link + "' is a link, not a file"},
		{map[string]any{"command": []any{"/dev/null"}}, "'/dev/null' is a characterSpecial, not a file"},
		{map[string]any{"command": []any{"/bin//true"}}, "Could not find command '/bin//true'"},
		{map[string]any{"command": []any{"/bin//true"}, "path": "/nowhere"}, ""},
		{map[string]any{"command": []any{"//bin/true"}}, ""},
	} {
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", tt.params), false)
		if tt.want == "" {
			if err != nil || len(changes) != 1 || changes[0].Err != nil {
				t.Errorf("%v: %+v, %v; want success", tt.params, changes, err)
			}
			continue
		}
		if err != nil || len(changes) != 1 || changes[0].Property != "returns" ||
			changes[0].Name != "executed_command" || changes[0].Was != "notrun" ||
			!slices.Equal(changes[0].Is.([]string), []string{"0"}) ||
			changes[0].Err == nil || changes[0].Err.Error() != tt.want {
			t.Errorf("%v: %+v, %v; want %q", tt.params, changes, err, tt.want)
		}
	}
}

// A slash-less argv[0] is found through the resource's path.
func TestExecArgvRelativeWithPath(t *testing.T) {
	ch := execChange(t, raltest.ExecRes("r", map[string]any{"command": []any{"true"}, "path": "/usr/bin:/bin"}))
	if ch.Err != nil || ch.Message != "executed successfully" {
		t.Errorf("change = %+v, want executed successfully", ch)
	}
}

// Measured: checkexe's which resolves a
// "." entry against the AGENT's working directory and an empty one as "/"
// (File.join("", f) is "/f"), while the child, after its chdir to cwd, runs
// the file a "." or empty entry finds in the RESOURCE's cwd (I-26).
func TestExecArgvRelativePathEntries(t *testing.T) {
	agent, resource := t.TempDir(), t.TempDir()
	for _, d := range []string{agent, resource} {
		script := "#!/bin/sh\ntouch " + filepath.Join(d, "marker") + "\n"
		if err := os.WriteFile(filepath.Join(d, "run.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	elsewhere := t.TempDir()
	apply := func(path string) Change {
		return execChange(t, raltest.ExecRes("p", map[string]any{"command": []any{"run.sh"}, "cwd": resource, "path": path}))
	}

	t.Chdir(elsewhere)
	for _, path := range []string{":/usr/bin", ".:/usr/bin"} {
		if ch := apply(path); ch.Err == nil || ch.Err.Error() != "Could not find command 'run.sh'" {
			t.Errorf("path %q from an agent cwd without run.sh: %+v, want Could not find command 'run.sh'", path, ch)
		}
	}

	t.Chdir(agent)
	if ch := apply(".:/usr/bin"); ch.Err != nil {
		t.Fatalf("path .:/usr/bin: %+v, want success", ch)
	}
	if _, err := os.Stat(filepath.Join(resource, "marker")); err != nil {
		t.Errorf("the resource cwd's run.sh did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agent, "marker")); err == nil {
		t.Error("the agent cwd's run.sh ran; Puppet's child runs the one in cwd")
	}
}

// Returns applies to an argv as to a string.
func TestExecArgvReturnsList(t *testing.T) {
	ch := execChange(t, raltest.ExecRes("r", map[string]any{
		"command": []any{"/usr/bin/test", "-d", "/nonexist"}, "returns": []any{float64(0), float64(1)}}))
	if ch.Err != nil || !slices.Equal(ch.Is.([]string), []string{"0", "1"}) {
		t.Errorf("change = %+v, want success with Is [0 1]", ch)
	}
}

// A nested array in onlyif or unless is one argv check.
func TestExecArgvGuards(t *testing.T) {
	argvCheck := []any{[]any{"/usr/bin/test", "-d", "/tmp"}}
	for _, tt := range []struct {
		name    string
		params  map[string]any
		changes int
	}{
		{"unless passing", map[string]any{"unless": argvCheck}, 0},
		{"onlyif passing", map[string]any{"onlyif": argvCheck}, 1},
		{"onlyif mixed", map[string]any{"onlyif": []any{[]any{"/usr/bin/test", "-d", "/tmp"}, "/usr/bin/test -d /nope"}}, 0},
	} {
		tt.params["command"] = []any{"/bin/true"}
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("g", tt.params), false)
		if err != nil || len(changes) != tt.changes {
			t.Errorf("%s: %+v, %v; want %d changes", tt.name, changes, err, tt.changes)
		}
	}
	_, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("g", map[string]any{
		"command": []any{"/bin/true"}, "onlyif": []any{[]any{"/no/such"}}}), false)
	if err == nil || err.Error() != `guard '["/no/such"]': Could not find command '/no/such'` {
		t.Errorf("unexecutable argv guard: %v", err)
	}
}

// A failing refreshonly argv fails the refresh with the inspect rendering.
func TestExecArgvRefreshFailure(t *testing.T) {
	_, err := Exec{}.Refresh(context.Background(), raltest.ExecRes("rf", map[string]any{
		"command": []any{"/bin/sh", "-c", "exit 4"}, "refreshonly": true}), false)
	if err == nil || err.Error() != `'["/bin/sh", "-c", "exit 4"]' returned 4 instead of one of [0]` {
		t.Errorf("Refresh error = %v", err)
	}
}

func TestExecArgvNoopMessage(t *testing.T) {
	changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("n", map[string]any{"command": []any{"/bin/echo", "n"}}), true)
	if err != nil || len(changes) != 1 || changes[0].Message != "executed successfully (noop)" {
		t.Errorf("noop = %+v, %v", changes, err)
	}
}

// A Kernel.exec that raises in Puppet's child exits 1 (util/execution.rb:396),
// and a file with no #! line is retried as a /bin/sh script, as Ruby's exec
// does.
func TestExecArgvStartFailures(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "noshebang")
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(script, []byte("touch \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ch := execChange(t, raltest.ExecRes("s", map[string]any{"command": []any{script, marker}})); ch.Err != nil {
		t.Fatalf("no #! line: %+v", ch)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the #!-less script did not run as a shell script: %v", err)
	}
	ch := execChange(t, raltest.ExecRes("c", map[string]any{"command": []any{"/bin/true"}, "cwd": filepath.Join(dir, "gone")}))
	if ch.Err == nil || ch.Err.Error() != `'["/bin/true"]' returned 1 instead of one of [0]` {
		t.Errorf("missing cwd: %+v, want returned 1", ch)
	}
	ch = execChange(t, raltest.ExecRes("e", map[string]any{"command": []any{}, "path": "/bin/true"}))
	if ch.Err == nil || ch.Err.Error() != `'[]' returned 1 instead of one of [0]` {
		t.Errorf("empty argv: %+v, want returned 1", ch)
	}
}

func TestExecValidateCommandType(t *testing.T) {
	const unqualified = "'%s' is not qualified and no path was specified. Please qualify the command or specify a path."
	for _, tt := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"command": 5.0},
			"Parameter command failed on Exec[v]: Command must be a String or Array<String>, got value of class Integer"},
		{map[string]any{"command": map[string]any{"a": "b"}},
			"Parameter command failed on Exec[v]: Command must be a String or Array<String>, got value of class Hash"},
		{map[string]any{"command": []any{"/bin/echo", []any{map[string]any{"a": "b"}}}},
			"Exec[v]: command => an argv element of class Hash is not implemented"},
		{map[string]any{"command": "/bin/true", "unless": []any{[]any{"/bin/echo", map[string]any{"a": "b"}}}},
			"Exec[v]: unless => an argv element of class Hash is not implemented"},
		{map[string]any{"command": []any{"echo", "rel"}},
			"Validation of Exec[v] failed: " + fmt.Sprintf(unqualified, "echo")},
		{map[string]any{"command": "/bin/true", "unless": []any{"/usr/bin/test", "-d", "/tmp"}},
			"Parameter unless failed on Exec[v]: " + fmt.Sprintf(unqualified, "-d")},
		// Read: extractexe is argv.first, to_s'd by the message (nil is "").
		{map[string]any{"command": []any{}}, "Validation of Exec[v] failed: " + fmt.Sprintf(unqualified, "")},
		{map[string]any{"command": []any{float64(5)}}, "Validation of Exec[v] failed: " + fmt.Sprintf(unqualified, "5")},
		// Measured: with a path, Puppet fails the whole catalog in
		// autorequire, scanning a first element that is not a String.
		{map[string]any{"command": []any{}, "path": "/bin/true"},
			"Exec[v]: command => an empty argv is not implemented"},
		{map[string]any{"command": []any{[]any{"/bin/echo", "a"}, "b"}, "path": "/usr/bin"},
			"Exec[v]: command => an argv whose first element is of class Array is not implemented"},
		{map[string]any{"command": "/bin/true", "onlyif": []any{[]any{true}}, "path": "/usr/bin"},
			"Exec[v]: onlyif => an argv whose first element is of class TrueClass is not implemented"},
		{map[string]any{"command": []any{"/bin/echo", float64(1), true, nil, []any{"x"}}}, ""},
	} {
		err := Exec{}.Validate(raltest.ExecRes("v", tt.params))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tt.want {
			t.Errorf("%v:\n got %q\nwant %q", tt.params, got, tt.want)
		}
	}
}

// type/exec.rb:593-640: the command's first argv element is scanned with both
// patterns, and each guard's (or the guard itself) with ^(/\S+) only.
func TestExecAutoRequiresFollowPuppet(t *testing.T) {
	managed := func(string) bool { return true }
	for _, tt := range []struct {
		params map[string]any
		want   []string
	}{
		{map[string]any{"command": []any{"/opt/tool", "x"}}, []string{"File[/opt/tool]"}},
		{map[string]any{"command": []any{`"/opt/q x"`, "x"}}, []string{"File[/opt/q x]"}},
		{map[string]any{"command": "make", "unless": []any{[]any{"/opt/check", "-q"}}}, []string{"File[/opt/check]"}},
		{map[string]any{"command": "make", "onlyif": []any{"/opt/a x", "/opt/b"}}, []string{"File[/opt/a]", "File[/opt/b]"}},
	} {
		got := Exec{}.AutoRequires(raltest.ExecRes("a", tt.params), managed)
		if !slices.Equal(got, tt.want) {
			t.Errorf("%v: AutoRequires = %v, want %v", tt.params, got, tt.want)
		}
	}
}

// An exec autorequires its user's User[...] after its file edges, when the
// catalog manages that user.
func TestExecAutorequiresUser(t *testing.T) {
	managed := map[string]bool{"File[/usr/bin/touch]": true, "User[app]": true}
	got := Exec{}.AutoRequires(
		raltest.ExecRes("a", map[string]any{"command": "/usr/bin/touch /opt/x", "user": "app"}),
		func(ref string) bool { return managed[ref] },
	)
	want := []string{"File[/usr/bin/touch]", "User[app]"}
	if !slices.Equal(got, want) {
		t.Errorf("AutoRequires = %v, want %v", got, want)
	}
}

// A numeric user is a uid, not a name, so no User[...] edge (type/exec.rb:642-648
// skips /^\d+$/).
func TestExecNoAutorequireNumericUser(t *testing.T) {
	managed := func(string) bool { return true }
	for _, user := range []any{"1000", int64(1000), float64(1000)} {
		got := Exec{}.AutoRequires(
			raltest.ExecRes("a", map[string]any{"command": "make", "user": user}),
			managed,
		)
		if len(got) != 0 {
			t.Errorf("user %v (%T): AutoRequires = %v, want none", user, user, got)
		}
	}
}

// An unmanaged user is no edge: autorequire is soft.
func TestExecNoAutorequireUnmanagedUser(t *testing.T) {
	got := Exec{}.AutoRequires(
		raltest.ExecRes("a", map[string]any{"command": "make", "user": "app"}),
		func(string) bool { return false },
	)
	if len(got) != 0 {
		t.Errorf("AutoRequires = %v, want none", got)
	}
}

// A string command runs before its returns is read, as it always has; an
// argv's returns is read first, because a checkexe failure carries the list.
func TestExecReturnsIsReadAfterAStringRunsAndBeforeAnArgv(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		command any
		marker  string
		ran     bool
	}{
		{"/usr/bin/touch " + filepath.Join(dir, "string"), filepath.Join(dir, "string"), true},
		{[]any{"/usr/bin/touch", filepath.Join(dir, "argv")}, filepath.Join(dir, "argv"), false},
	} {
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("r", map[string]any{
			"command": tt.command, "returns": "abc"}), false)
		if err == nil || changes != nil {
			t.Errorf("%v: %+v, %v; want a bare returns error", tt.command, changes, err)
		}
		if _, statErr := os.Stat(tt.marker); (statErr == nil) != tt.ran {
			t.Errorf("%v: ran = %v, want %v", tt.command, statErr == nil, tt.ran)
		}
	}
}

// markedExec marks the named parameters sensitive on an Exec resource.
func markedExec(params map[string]any, marked ...string) catalog.Resource {
	r := raltest.ExecRes("m", params)
	r.SensitiveParameters = marked
	return r
}

// assertRedacted fails unless text shows the command as [command redacted] and
// leaks no part of secret (voxagent redacts where Ruby leaks, D-78).
func assertRedacted(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Errorf("%q leaks %q", text, secret)
	}
	if !strings.Contains(text, "[command redacted]") {
		t.Errorf("%q lacks [command redacted]", text)
	}
}

// A marked command is replaced, quotes included, in the bad-return detail.
func TestExecMarkedBadReturn(t *testing.T) {
	marked := execChange(t, markedExec(map[string]any{"command": `/bin/sh -c "exit 3"`}, "command"))
	if got, want := marked.Err.Error(), "[command redacted] returned 3 instead of one of [0]"; got != want {
		t.Errorf("marked Err = %q, want %q", got, want)
	}
	unmarked := execChange(t, markedExec(map[string]any{"command": `/bin/sh -c "exit 3"`}))
	if got, want := unmarked.Err.Error(), `'/bin/sh -c "exit 3"' returned 3 instead of one of [0]`; got != want {
		t.Errorf("unmarked Err = %q, want %q", got, want)
	}
}

// Marking any of command, unless and onlyif marks the others given (Ruby's
// override), so a marked unless redacts the command in the failure detail.
func TestExecOnlyUnlessMarkedRedactsCommand(t *testing.T) {
	params := map[string]any{"command": "/bin/false secret-cmd", "unless": "/bin/false"}
	for _, tc := range []struct {
		name   string
		marked []string
		want   string
	}{
		{"marked", []string{"unless"}, "[command redacted] returned 1 instead of one of [0]"},
		{"unmarked", nil, "'/bin/false secret-cmd' returned 1 instead of one of [0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, _, err := Exec{}.Apply(context.Background(), markedExec(params, tc.marked...), false)
			if err != nil || len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != tc.want {
				t.Errorf("run = %+v, %v; want the failure %q", changes, err, tc.want)
			}
		})
	}
}

// The noop message names no command, marked or not.
func TestExecNoopMessageNamesNoCommand(t *testing.T) {
	params := map[string]any{"command": "/bin/echo secret-cmd", "unless": "/bin/false"}
	for _, marked := range [][]string{{"unless"}, nil} {
		changes, _, err := Exec{}.Apply(context.Background(), markedExec(params, marked...), true)
		if err != nil || len(changes) != 1 || changes[0].Message != "executed successfully (noop)" {
			t.Errorf("marked %v: noop = %+v, %v", marked, changes, err)
		}
	}
}

func TestExecMarkedGuardErrors(t *testing.T) {
	argvGuard := map[string]any{"command": []any{"/bin/true"}, "onlyif": []any{[]any{"/no/such-secret"}}}
	_, _, err := Exec{}.Apply(context.Background(), markedExec(argvGuard, "onlyif"), false)
	if err == nil {
		t.Fatal("marked argv guard that cannot run: no error")
	}
	assertRedacted(t, err.Error(), "such-secret")
	_, _, err = Exec{}.Apply(context.Background(), markedExec(argvGuard), false)
	if err == nil || !strings.Contains(err.Error(), "'/no/such-secret'") {
		t.Errorf("unmarked guard error = %v, want the command named", err)
	}

	// A guard that cannot start at all: the temporary file cannot be created.
	t.Setenv("TMPDIR", "/no/such/dir")
	runGuard := map[string]any{"command": "/bin/true", "unless": "/bin/echo run-secret"}
	_, _, err = Exec{}.Apply(context.Background(), markedExec(runGuard, "unless"), false)
	if err == nil {
		t.Fatal("marked guard with no temporary directory: no error")
	}
	assertRedacted(t, err.Error(), "run-secret")
}

func TestExecMarkedMissingExecutable(t *testing.T) {
	params := map[string]any{"command": []any{"/no/such-secret", "x"}}
	ch := execChange(t, markedExec(params, "command"))
	assertRedacted(t, ch.Err.Error(), "such-secret")
	if ch = execChange(t, markedExec(params)); ch.Err.Error() != "Could not find command '/no/such-secret'" {
		t.Errorf("unmarked Err = %q", ch.Err)
	}
}

func TestExecMarkedNotQualified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		marked string
		prefix string
	}{
		{"command", map[string]any{"command": "secretcmd -x"}, "command", "Validation of Exec[m] failed: "},
		{"unless", map[string]any{"command": "/bin/true", "unless": "secretcmd"}, "unless", "Parameter unless failed on Exec[m]: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Exec{}.Validate(markedExec(tc.params, tc.marked))
			if err == nil {
				t.Fatal("marked unqualified command: no error")
			}
			assertRedacted(t, err.Error(), "secretcmd")
			want := tc.prefix + "[command redacted] is not qualified and no path was specified. Please qualify the command or specify a path."
			if err.Error() != want {
				t.Errorf("Validate = %q, want %q", err, want)
			}
			err = Exec{}.Validate(markedExec(tc.params))
			if err == nil || !strings.Contains(err.Error(), "'secretcmd") {
				t.Errorf("unmarked Validate = %v, want the command named", err)
			}
		})
	}
}

func TestExecMarkedRunError(t *testing.T) {
	t.Setenv("TMPDIR", "/no/such/dir")
	params := map[string]any{"command": "/bin/echo run-secret"}
	_, _, err := Exec{}.Apply(context.Background(), markedExec(params, "command"), false)
	if err == nil {
		t.Fatal("run with no temporary directory: no error")
	}
	assertRedacted(t, err.Error(), "run-secret")
	_, _, err = Exec{}.Apply(context.Background(), markedExec(params), false)
	if err == nil || !strings.Contains(err.Error(), "run '/bin/echo run-secret'") {
		t.Errorf("unmarked run error = %v, want the command named", err)
	}
}

// A marked returns whose value is not an integer must not echo it.
func TestExecMarkedReturnsValueError(t *testing.T) {
	params := map[string]any{"command": "/bin/true", "returns": "bogus-secret"}
	_, _, err := Exec{}.Apply(context.Background(), markedExec(params, "returns"), false)
	if err == nil || strings.Contains(err.Error(), "bogus-secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("marked returns error = %v, want [redacted] and no value", err)
	}
	_, _, err = Exec{}.Apply(context.Background(), markedExec(params), false)
	if err == nil || !strings.Contains(err.Error(), "bogus-secret") {
		t.Errorf("unmarked returns error = %v, want the value named", err)
	}
}

// Redaction replaces only the value; an error that quotes none is kept.
func TestExecMarkedReturnsValueFreeErrorIsKept(t *testing.T) {
	params := map[string]any{"command": "/bin/true", "returns": []any{}}
	_, _, err := Exec{}.Apply(context.Background(), markedExec(params, "returns"), false)
	if err == nil || err.Error() != "exec returns: returns is an empty array" {
		t.Errorf("marked empty returns: %v, want the value-free error kept", err)
	}
}

// setLocale sets the only locale variable outputEncoding will see as set. The
// tests that call it cannot be parallel: t.Setenv refuses them.
func setLocale(t *testing.T, lang string) {
	t.Helper()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", lang)
}

// type/exec.rb:120-190 with logoutput at :271: true logs always, false never,
// on_failure (the default) when the status is not in returns. stdout and
// stderr are one stream.
func TestExecLogoutputValues(t *testing.T) {
	setLocale(t, "C")
	output := []string{"out", "err"}
	for _, tc := range []struct {
		name      string
		logoutput any
		absent    bool
		onSuccess bool
		onFailure bool
	}{
		{"true", true, false, true, true},
		{`"true"`, "true", false, true, true},
		{"false", false, false, false, false},
		{`"false"`, "false", false, false, false},
		{"on_failure", "on_failure", false, false, true},
		{"absent", nil, true, false, true},
	} {
		for _, exit := range []int{0, 3} {
			params := map[string]any{"command": fmt.Sprintf("printf 'out\\n'; printf 'err\\n' >&2; exit %d", exit)}
			if !tc.absent {
				params["logoutput"] = tc.logoutput
			}
			ch := execChange(t, raltest.ExecRes("lo", params))
			logged := tc.onSuccess
			if exit != 0 {
				logged = tc.onFailure
			}
			var want []string
			if logged {
				want = output
			}
			if !slices.Equal(ch.PropertyNotices, want) {
				t.Errorf("logoutput %s, exit %d: PropertyNotices = %q, want %q", tc.name, exit, ch.PropertyNotices, want)
			}
			if failed := ch.Err != nil; failed != (exit != 0) {
				t.Errorf("logoutput %s, exit %d: Err = %v", tc.name, exit, ch.Err)
			}
		}
	}
}

// A marked command's output is never read; one [output redacted] line
// stands for it, empty output included.
func TestExecLogoutputMarkedCommand(t *testing.T) {
	setLocale(t, "C")
	for _, params := range []map[string]any{
		{"command": "printf 's-true\\n'", "logoutput": true},
		{"command": "exit 5"},
		{"command": "printf '\\377'", "logoutput": true},
	} {
		ch := execChange(t, markedExec(params, "command"))
		if want := []string{"[output redacted]"}; !slices.Equal(ch.PropertyNotices, want) {
			t.Errorf("%v: PropertyNotices = %q, want %q", params, ch.PropertyNotices, want)
		}
		if ch.Err != nil && strings.Contains(ch.Err.Error(), "invalid byte sequence") {
			t.Errorf("%v: Err = %v, want the output never decoded", params, ch.Err)
		}
	}
	// A marked guard marks the command with it (type/exec.rb:707-728).
	ch := execChange(t, markedExec(map[string]any{"command": "echo s-unless; exit 6", "unless": "/bin/false"}, "unless"))
	if want := []string{"[output redacted]"}; !slices.Equal(ch.PropertyNotices, want) {
		t.Errorf("marked unless: PropertyNotices = %q, want %q", ch.PropertyNotices, want)
	}
}

// Output to be logged that is invalid in the locale's encoding fails the
// change with Ruby's split error, replacing the exit-status error whatever
// the status; output that is not logged is never decoded.
func TestExecLogoutputEncoding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lang      string
		command   string
		logoutput any
		wantErr   string
		want      []string
	}{
		{"UTF-8 under C", "C", `printf 'caf\303\251'`, true, "invalid byte sequence in US-ASCII", nil},
		{"UTF-8 under C.UTF-8", "C.UTF-8", `printf 'caf\303\251'`, true, "", []string{"café"}},
		{"0xff under C.UTF-8", "C.UTF-8", `printf '\377'`, true, "invalid byte sequence in UTF-8", nil},
		{"failure replaced", "C", `printf '\377\376bad\n'; exit 1`, "on_failure", "invalid byte sequence in US-ASCII", nil},
		{"not logged", "C", `printf '\377'`, false, "", nil},
		{"not logged by default", "C", `printf '\377\n'`, nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLocale(t, tc.lang)
			params := map[string]any{"command": tc.command}
			if tc.logoutput != nil {
				params["logoutput"] = tc.logoutput
			}
			ch := execChange(t, raltest.ExecRes("enc", params))
			gotErr := ""
			if ch.Err != nil {
				gotErr = ch.Err.Error()
			}
			if gotErr != tc.wantErr {
				t.Errorf("Err = %q, want %q", gotErr, tc.wantErr)
			}
			if !slices.Equal(ch.PropertyNotices, tc.want) {
				t.Errorf("PropertyNotices = %q, want %q", ch.PropertyNotices, tc.want)
			}
			if tc.wantErr == "" && ch.Message != "executed successfully" {
				t.Errorf("Message = %q, want executed successfully", ch.Message)
			}
		})
	}
}

// newvalues(:true, :false, :on_failure), Ruby's wording. A marked value
// is replaced quotes included (D-78).
func TestExecLogoutputInvalid(t *testing.T) {
	for _, tc := range []struct {
		value  any
		marked []string
		want   string
	}{
		{"maybe", nil, `Parameter logoutput failed on Exec[bad]: Invalid value "maybe". Valid values are true, false, on_failure.`},
		{"maybe", []string{"logoutput"}, `Parameter logoutput failed on Exec[bad]: Invalid value [redacted]. Valid values are true, false, on_failure.`},
		{float64(1), nil, `Parameter logoutput failed on Exec[bad]: Invalid value 1. Valid values are true, false, on_failure.`},
		{"True", nil, `Parameter logoutput failed on Exec[bad]: Invalid value "True". Valid values are true, false, on_failure.`},
	} {
		r := raltest.ExecRes("bad", map[string]any{"command": "/bin/true", "logoutput": tc.value})
		r.SensitiveParameters = tc.marked
		err := Exec{}.Validate(r)
		if err == nil || err.Error() != tc.want {
			t.Errorf("Validate(logoutput %v, marked %v) = %v, want %q", tc.value, tc.marked, err, tc.want)
		}
	}
	for _, v := range []any{true, false, "true", "false", "on_failure"} {
		if err := (Exec{}).Validate(raltest.ExecRes("ok", map[string]any{"command": "/bin/true", "logoutput": v})); err != nil {
			t.Errorf("Validate(logoutput %v) = %v, want nil", v, err)
		}
	}
	// A caller that skipped Validate still runs nothing.
	marker := filepath.Join(t.TempDir(), "ran")
	_, _, err := Exec{}.Apply(context.Background(),
		raltest.ExecRes("bad", map[string]any{"command": "touch " + marker, "logoutput": "maybe"}), false)
	if err == nil {
		t.Error("Apply with an invalid logoutput: no error")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("Apply with an invalid logoutput ran the command")
	}
}

// A refresh is returns.sync (type/exec.rb:690-698), so its output is
// logged by the same rule, at the returns property.
func TestExecRefreshLogoutput(t *testing.T) {
	setLocale(t, "C")
	out, err := Exec{}.RefreshLogged(context.Background(),
		raltest.ExecRes("rt", map[string]any{"command": "echo refreshed", "refreshonly": true, "logoutput": true}), false)
	if err != nil || !out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %v; want refreshed", out, err)
	}
	if want := []PropertyNotice{{Property: "returns", Message: "refreshed"}}; !slices.Equal(out.PropertyNotices, want) {
		t.Errorf("PropertyNotices = %v, want %v", out.PropertyNotices, want)
	}

	out, err = Exec{}.RefreshLogged(context.Background(),
		raltest.ExecRes("rf", map[string]any{"command": "echo refresh-broke; exit 4", "refreshonly": true}), false)
	if err == nil || err.Error() != "'echo refresh-broke; exit 4' returned 4 instead of one of [0]" || out.Refreshed {
		t.Errorf("RefreshLogged = %+v, %v; want the status error", out, err)
	}
	if want := []PropertyNotice{{Property: "returns", Message: "refresh-broke"}}; !slices.Equal(out.PropertyNotices, want) {
		t.Errorf("PropertyNotices = %v, want %v", out.PropertyNotices, want)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	out, err = Exec{}.RefreshLogged(context.Background(),
		raltest.ExecRes("rn", map[string]any{"command": "echo x; touch " + marker, "logoutput": true}), true)
	if err != nil || len(out.PropertyNotices) != 0 {
		t.Errorf("noop RefreshLogged = %+v, %v; want no output", out, err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("noop RefreshLogged ran the command")
	}
}

// debugSources are the sources the transaction puts on the context for the exec
// the debug-line tests run.
var debugSources = sources.Sources{
	Resource: "/Stage[main]/Main/Exec[x]",
	Provider: "Exec[x](provider=posix)",
	Ref:      "Exec[x]",
}

// execDebugLines applies r (or refreshes it) at level and returns the run-log
// entries it kept, as {level, source, message} for raltest.AssertWarnings.
func execDebugLines(t *testing.T, level string, r catalog.Resource) []report.LogEntry {
	t.Helper()
	ctx, got := raltest.DebugContext(level)
	ctx = sources.WithSources(ctx, debugSources)
	if _, _, err := (Exec{}).Apply(ctx, r, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return *got
}

func dbg(source, message string) report.LogEntry {
	return report.LogEntry{Level: "debug", Source: source, Message: message}
}

// provider/exec.rb:48 (Executing '<cmd>') and util/execution.rb:181
// (Executing: '<cmd>'): the first at the provider form, the second at Puppet.
func TestExecDebugLinesForARun(t *testing.T) {
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{"command": "echo hi"}))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing 'echo hi'"),
		dbg("Puppet", "Executing: 'echo hi'"),
	})
}

// An argv shows as Array#inspect in the provider's line (it interpolates the
// original value) and as the words joined by a space in execution.rb's.
func TestExecDebugLinesForAnArgvRun(t *testing.T) {
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{
		"command": []any{"/bin/echo", "hi"}}))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", `Executing '["/bin/echo", "hi"]'`),
		dbg("Puppet", "Executing: '/bin/echo hi'"),
	})
}

func TestExecDebugLinesAreRedactedWhenMarked(t *testing.T) {
	got := execDebugLines(t, "debug", markedExec(map[string]any{"command": "echo SECRET"}, "command"))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing '[redacted]'"),
		dbg("Puppet", "Executing: '[redacted]'"),
	})
}

// util/execution.rb:167-180 (M16): a set user or group names the identity in
// execution.rb's line, which is logged before the identity is resolved, so an
// unknown one still shows it. A marked user or group is [redacted] (D-78).
func TestExecDebugLineNamesIdentity(t *testing.T) {
	setAgentRoot(t, true)
	for _, tc := range []struct {
		name    string
		params  map[string]any
		marked  []string
		command any
		want    string
	}{
		{"user", map[string]any{"user": "12345"}, nil, "/usr/bin/id", "Executing with uid=12345: '/usr/bin/id'"},
		{"group", map[string]any{"group": "4242"}, nil, "/usr/bin/id", "Executing with gid=4242: '/usr/bin/id'"},
		{"both", map[string]any{"user": "12345", "group": "4242"}, nil, "/usr/bin/id", "Executing with uid=12345 gid=4242: '/usr/bin/id'"},
		{"integer user", map[string]any{"user": float64(12345)}, nil, "/usr/bin/id", "Executing with uid=12345: '/usr/bin/id'"},
		{"name", map[string]any{"user": "nosuch"}, nil, []any{"/usr/bin/id", "-u"}, "Executing with uid=nosuch: '/usr/bin/id -u'"},
		{"marked user", map[string]any{"user": "secretname", "group": "4242"}, []string{"user"}, "/usr/bin/id", "Executing with uid=[redacted] gid=4242: '/usr/bin/id'"},
		{"marked group", map[string]any{"user": "12345", "group": "secretgroup"}, []string{"group"}, "/usr/bin/id", "Executing with uid=12345 gid=[redacted]: '/usr/bin/id'"},
		{"marked command", map[string]any{"user": "12345"}, []string{"command"}, "/usr/bin/id", "Executing with uid=12345: '[redacted]'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params["command"] = tc.command
			r := markedExec(tc.params, tc.marked...)
			ctx, got := raltest.DebugContext("debug")
			changes, _, err := (Exec{}).Apply(sources.WithSources(ctx, debugSources), r, false)
			if err != nil || len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("Apply = (%+v, %v), want one failed change", changes, err)
			}
			if lines := raltest.LinesAt(*got, "Puppet"); !slices.Equal(lines, []string{tc.want}) {
				t.Errorf("Puppet debug lines = %q, want %q", lines, []string{tc.want})
			}
			wantOutput := "Error: Could not execute posix command: Invalid "
			if slices.Contains(tc.marked, "command") {
				wantOutput = "[output redacted]"
			}
			if notices := changes[0].PropertyNotices; len(notices) != 1 || !strings.HasPrefix(notices[0], wantOutput) {
				t.Errorf("output = %q, want the identity failure", notices)
			}
			for _, e := range *got {
				for _, secret := range []string{"secretname", "secretgroup"} {
					if strings.Contains(e.Message, secret) {
						t.Errorf("%q leaks %q", e.Message, secret)
					}
				}
			}
		})
	}
}

// A user equal to the agent's own runs; the line names it all the same.
func TestExecDebugLineNamesAgentUser(t *testing.T) {
	self, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{"command": "/bin/true", "user": self.Username}))
	if lines, want := raltest.LinesAt(got, "Puppet"), []string{"Executing with uid=" + self.Username + ": '/bin/true'"}; !slices.Equal(lines, want) {
		t.Errorf("Puppet debug lines = %q, want %q", lines, want)
	}
}

// type/exec.rb:511-514: one debug line per output line of the guard, at the
// guard parameter's path; the check form of the provider line.
func TestExecDebugLinesForAnUnlessGuard(t *testing.T) {
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{
		"command": "true", "unless": "printf 'one\\ntwo\\n'; exit 1"}))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing check 'printf 'one\\ntwo\\n'; exit 1'"),
		dbg("Puppet", "Executing: 'printf 'one\\ntwo\\n'; exit 1'"),
		dbg("/Stage[main]/Main/Exec[x]/unless", "one"),
		dbg("/Stage[main]/Main/Exec[x]/unless", "two"),
		dbg("Exec[x](provider=posix)", "Executing 'true'"),
		dbg("Puppet", "Executing: 'true'"),
	})
}

func TestExecDebugLinesForAnOnlyifGuard(t *testing.T) {
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{
		"command": "true", "onlyif": "echo yes"}))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing check 'echo yes'"),
		dbg("Puppet", "Executing: 'echo yes'"),
		dbg("/Stage[main]/Main/Exec[x]/onlyif", "yes"),
		dbg("Exec[x](provider=posix)", "Executing 'true'"),
		dbg("Puppet", "Executing: 'true'"),
	})
}

// Review Focus 4: one line with no trailing newline is one debug line, and a
// guard with no output logs none.
func TestExecGuardOutputLineCounts(t *testing.T) {
	for _, tc := range []struct {
		name, guard string
		want        []string
	}{
		{"single line without newline", "printf one; exit 1", []string{"one"}},
		{"single line with newline", "echo one; exit 1", []string{"one"}},
		{"interior blank line kept, trailing dropped", "printf 'a\\n\\nb\\n\\n'; exit 1", []string{"a", "", "b"}},
		{"no output", "exit 1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{"command": "true", "unless": tc.guard}))
			var lines []string
			for _, e := range got {
				if e.Source == "/Stage[main]/Main/Exec[x]/unless" {
					lines = append(lines, e.Message)
				}
			}
			if !slices.Equal(lines, tc.want) {
				t.Errorf("guard output lines = %q, want %q", lines, tc.want)
			}
		})
	}
}

func TestExecGuardOutputIsOneRedactedLineWhenMarked(t *testing.T) {
	got := execDebugLines(t, "debug", markedExec(map[string]any{
		"command": "true", "unless": "echo SECRET1; echo SECRET2; exit 1"}, "unless"))
	raltest.AssertWarnings(t, got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing check '[redacted]'"),
		dbg("Puppet", "Executing: '[redacted]'"),
		dbg("/Stage[main]/Main/Exec[x]/unless", "[output redacted]"),
		dbg("Exec[x](provider=posix)", "Executing '[redacted]'"),
		dbg("Puppet", "Executing: '[redacted]'"),
	})
}

// A marked guard with no output still logs the one redacted line, as Ruby does.
func TestExecMarkedGuardWithNoOutputStillLogsRedactedLine(t *testing.T) {
	got := execDebugLines(t, "debug", markedExec(map[string]any{
		"command": "true", "unless": "exit 1"}, "unless"))
	if len(got) < 3 {
		t.Fatalf("entries = %+v, want at least three", got)
	}
	raltest.AssertWarnings(t, got[2:3], []report.LogEntry{dbg("/Stage[main]/Main/Exec[x]/unless", "[output redacted]")})
}

// type/exec.rb:672 and :454: the failed-check line at the resource, the
// creates line at its parameter. Only the lines about the check are compared.
func TestExecDebugLinesForFailedChecks(t *testing.T) {
	res := "/Stage[main]/Main/Exec[x]"
	tmp := t.TempDir()
	for _, tc := range []struct {
		name string
		r    catalog.Resource
		want []report.LogEntry
	}{
		{"creates", raltest.ExecRes("x", map[string]any{"command": "/bin/true", "creates": tmp}), []report.LogEntry{
			dbg(res+"/creates", "Checking that 'creates' path '"+tmp+"' exists"),
			dbg(res, "'/bin/true' won't be executed because of failed check 'creates'"),
		}},
		{"onlyif", raltest.ExecRes("x", map[string]any{"command": "/bin/true", "onlyif": "exit 1"}), []report.LogEntry{
			dbg(res, "'/bin/true' won't be executed because of failed check 'onlyif'"),
		}},
		{"unless", raltest.ExecRes("x", map[string]any{"command": "/bin/true", "unless": "exit 0"}), []report.LogEntry{
			dbg(res, "'/bin/true' won't be executed because of failed check 'unless'"),
		}},
		{"refreshonly", raltest.ExecRes("x", map[string]any{"command": "/bin/true", "refreshonly": true}), []report.LogEntry{
			dbg(res, "'/bin/true' won't be executed because of failed check 'refreshonly'"),
		}},
		{"argv command shows as inspect", raltest.ExecRes("x", map[string]any{
			"command": []any{"/bin/true", "a"}, "refreshonly": true}), []report.LogEntry{
			dbg(res, `'["/bin/true", "a"]' won't be executed because of failed check 'refreshonly'`),
		}},
		{"marked command", markedExec(map[string]any{"command": "/bin/true", "onlyif": "exit 1"}, "command"), []report.LogEntry{
			dbg(res, "'[command redacted]' won't be executed because of failed check 'onlyif'"),
		}},
		{"marked refreshonly", markedExec(map[string]any{"command": "/bin/true", "refreshonly": true}, "command"), []report.LogEntry{
			dbg(res, "'[command redacted]' won't be executed because of failed check 'refreshonly'"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var about []report.LogEntry
			for _, e := range execDebugLines(t, "debug", tc.r) {
				if e.Source == res || e.Source == res+"/creates" {
					about = append(about, e)
				}
			}
			raltest.AssertWarnings(t, about, tc.want)
		})
	}
}

// A blocked command never reaches its own Executing lines.
func TestExecBlockedCommandLogsNoExecutingLine(t *testing.T) {
	for _, e := range execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{
		"command": "/bin/true", "creates": t.TempDir()})) {
		if strings.HasPrefix(e.Message, "Executing") {
			t.Errorf("unexpected %+v", e)
		}
	}
}

// Under refresh the refreshonly check is skipped (check_all_attributes(true)),
// so a refreshonly exec runs and logs no refreshonly line.
func TestExecRefreshOfARefreshonlyLogsNoRefreshonlyLine(t *testing.T) {
	ctx, got := raltest.DebugContext("debug")
	ctx = sources.WithSources(ctx, debugSources)
	r := raltest.ExecRes("x", map[string]any{"command": "true", "refreshonly": true})
	if _, err := (Exec{}).Refresh(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	raltest.AssertWarnings(t, *got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing 'true'"),
		dbg("Puppet", "Executing: 'true'"),
	})
}

// Debug off: no records at all.
func TestExecDebugLinesOffAtNotice(t *testing.T) {
	for _, level := range []string{"notice", "info"} {
		got := execDebugLines(t, level, raltest.ExecRes("x", map[string]any{
			"command": "true", "unless": "echo out; exit 1", "creates": "/nonexistent/x"}))
		if len(got) != 0 {
			t.Errorf("level %s: entries = %+v, want none", level, got)
		}
	}
}

// A command ENOEXEC retries as a script runs once, so it logs one Executing:
// line (execution.rb:181 fires once per execute).
func TestExecENOEXECRetryLogsExecutingOnce(t *testing.T) {
	script := filepath.Join(t.TempDir(), "noshebang")
	if err := os.WriteFile(script, []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := execDebugLines(t, "debug", raltest.ExecRes("x", map[string]any{"command": []any{script}}))
	var n int
	for _, e := range got {
		if e.Source == "Puppet" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("Puppet Executing: lines = %d (%+v), want 1", n, got)
	}
}

// triesCtx is a debug-logging context with the sources a resource at
// /Stage[main]/Main/Exec[x] carries.
func triesCtx() (context.Context, *[]report.LogEntry, string) {
	ctx, entries := raltest.DebugContext("debug")
	const resource = "/Stage[main]/Main/Exec[x]"
	return sources.WithSources(ctx, sources.Sources{Resource: resource, Provider: "Exec[x](provider=posix)", Ref: "Exec[x]"}), entries, resource + "/returns"
}

// countingCommand appends to a counter file and exits with the new count
// unless it has reached succeedAt, when it exits 0.
func countingCommand(counter string, succeedAt int) string {
	return fmt.Sprintf(`n=$(cat %[1]s 2>/dev/null || echo 0); n=$((n+1)); echo $n > %[1]s; echo attempt-$n; [ $n -ge %[2]d ] && exit 0; exit $n`, counter, succeedAt)
}

// M2: a command that fails twice and succeeds on the third try logs each try and
// the sleep between them, and no sleep once it has succeeded.
func TestExecTriesSucceedsOnThird(t *testing.T) {
	ctx, entries, returns := triesCtx()
	counter := filepath.Join(t.TempDir(), "n")
	changes, _, err := Exec{}.Apply(ctx, raltest.ExecRes("x", map[string]any{
		"command": countingCommand(counter, 3), "tries": 3.0, "try_sleep": 0.1}), false)
	if err != nil || len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("Apply = %v, %v; want success", changes, err)
	}
	want := []string{"Exec try 1/3", "Sleeping for 0.1 seconds between tries",
		"Exec try 2/3", "Sleeping for 0.1 seconds between tries", "Exec try 3/3"}
	if got := raltest.LinesAt(*entries, returns); !slices.Equal(got, want) {
		t.Errorf("debug lines at %s = %q, want %q", returns, got, want)
	}
}

// M6, M7: the sleep follows the last failed try (I-32), and the failure carries
// the last try's status and output.
func TestExecTriesNeverSucceeds(t *testing.T) {
	ctx, entries, returns := triesCtx()
	counter := filepath.Join(t.TempDir(), "n")
	changes, _, err := Exec{}.Apply(ctx, raltest.ExecRes("x", map[string]any{
		"command": countingCommand(counter, 99), "tries": 2.0, "try_sleep": 0.1, "logoutput": "on_failure"}), false)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = %v, %v", changes, err)
	}
	if changes[0].Err == nil || !strings.HasSuffix(changes[0].Err.Error(), "returned 2 instead of one of [0]") {
		t.Errorf("Err = %v, want the second try's status 2", changes[0].Err)
	}
	if want := []string{"attempt-2"}; !slices.Equal(changes[0].PropertyNotices, want) {
		t.Errorf("PropertyNotices = %q, want %q", changes[0].PropertyNotices, want)
	}
	want := []string{"Exec try 1/2", "Sleeping for 0.1 seconds between tries",
		"Exec try 2/2", "Sleeping for 0.1 seconds between tries"}
	if got := raltest.LinesAt(*entries, returns); !slices.Equal(got, want) {
		t.Errorf("debug lines = %q, want %q", got, want)
	}
}

// tries 1 logs neither the try nor the sleep, whatever try_sleep says.
func TestExecTriesNoDebugWhenOne(t *testing.T) {
	ctx, entries, returns := triesCtx()
	changes, _, err := Exec{}.Apply(ctx, raltest.ExecRes("x", map[string]any{
		"command": "exit 3", "try_sleep": 0.1}), false)
	if err != nil || len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("Apply = %v, %v; want a returns failure", changes, err)
	}
	if got := raltest.LinesAt(*entries, returns); len(got) != 0 {
		t.Errorf("debug lines at %s = %q, want none", returns, got)
	}
}

// An Integer try_sleep prints without a fraction; a string or Float uses
// Float#to_s (M2: '1' is 1.0 but Integer 1 is 1).
func TestExecTrySleepIntegerShown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sleep any
		want  string
	}{
		{"integer", int64(1), "Sleeping for 1 seconds between tries"},
		{"whole float", 1.0, "Sleeping for 1 seconds between tries"},
		{"string", "0.5", "Sleeping for 0.5 seconds between tries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, entries, returns := triesCtx()
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			time.AfterFunc(200*time.Millisecond, cancel)
			Exec{}.Apply(cctx, raltest.ExecRes("x", map[string]any{
				"command": "exit 1", "tries": 2.0, "try_sleep": tc.sleep}), false)
			got := raltest.LinesAt(*entries, returns)
			if len(got) < 2 || got[1] != tc.want {
				t.Errorf("debug lines = %q, want second line %q", got, tc.want)
			}
		})
	}
}

// M19: a Float tries fails returns before anything runs.
func TestExecTriesFloat(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	ch := execChange(t, raltest.ExecRes("x", map[string]any{"command": "touch " + marker, "tries": 2.5}))
	if ch.Err == nil || ch.Err.Error() != "undefined method `times' for 2.5:Float" {
		t.Errorf("Err = %v, want the Ruby NoMethodError text", ch.Err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran")
	}
}

// A cancelled run stops sleeping between tries and returns the context error.
func TestExecTrySleepCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := Exec{}.Apply(ctx, raltest.ExecRes("x", map[string]any{
		"command": "exit 1", "tries": 3.0, "try_sleep": 30.0}), false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Apply took %v; want it to stop sleeping on cancel", elapsed)
	}
}

// M2: a timeout ends the loop; the remaining tries do not run.
func TestExecTimeoutEndsTries(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "n")
	start := time.Now()
	ch := execChange(t, raltest.ExecRes("x", map[string]any{
		"command": "echo x >> " + counter + "; sleep 5", "tries": 3.0, "timeout": 0.3}))
	if ch.Err == nil || ch.Err.Error() != "Command exceeded timeout" {
		t.Errorf("Err = %v, want Command exceeded timeout", ch.Err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Apply took %v; want one 0.3s timeout", elapsed)
	}
	if b, _ := os.ReadFile(counter); string(b) != "x\n" {
		t.Errorf("command ran %q times, want once", b)
	}
}

// Review Focus 3: a guard evaluated under noop still runs as the resource's
// user and is still bounded by its timeout.
func TestExecNoopGuardHonoursUserAndTimeout(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("x", map[string]any{
			"command": "true", "onlyif": "sleep 5", "timeout": 0.3}), true)
		if err != nil || len(changes) != 0 {
			t.Errorf("Apply = %v, %v; want a blocked command and no change", changes, err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("Apply took %v; want the guard cut at 0.3s", elapsed)
		}
	})
	t.Run("user", func(t *testing.T) {
		needRoot(t, "the guard runs as user nobody")
		lookupNobody(t)
		changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("x", map[string]any{
			"command": "true", "user": "nobody", "onlyif": `test "$(id -un)" = nobody`}), true)
		if err != nil || len(changes) != 1 {
			t.Errorf("Apply = %v, %v; want the guard to pass as nobody and a noop change", changes, err)
		}
	})
}

// M12: a refresh command's status never fails the refresh, logoutput prints
// nothing for it, and tries does not repeat it.
func TestExecRefreshCommandExit1Succeeds(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "n")
	r := raltest.ExecRes("x", map[string]any{
		"command": "/bin/true", "tries": 3.0, "logoutput": true,
		"refresh": fmt.Sprintf(`/bin/sh -c "echo r >> %s; echo refreshed-out; exit 1"`, counter)})
	out, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err != nil || !out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %v; want Refreshed", out, err)
	}
	if len(out.PropertyNotices) != 0 {
		t.Errorf("PropertyNotices = %+v, want none", out.PropertyNotices)
	}
	if got, _ := os.ReadFile(counter); string(got) != "r\n" {
		t.Errorf("refresh ran %q, want once", got)
	}
}

// M12, M20: a refresh that exits 127 fails with its output; the walk adds
// "Failed to call refresh: ".
func TestExecRefreshCommand127Fails(t *testing.T) {
	r := raltest.ExecRes("x", map[string]any{"command": "/bin/true", "refresh": `/bin/sh -c "echo gone; exit 127"`})
	out, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err == nil || err.Error() != "gone\n" || out.Refreshed {
		t.Errorf("RefreshLogged = %+v, %v; want error %q", out, err, "gone\n")
	}
}

// M12: the refresh command's executable is checked before it runs, with no
// Executing line.
func TestExecRefreshCommandMissingExe(t *testing.T) {
	ctx, got := raltest.DebugContext("debug")
	ctx = sources.WithSources(ctx, debugSources)
	r := raltest.ExecRes("x", map[string]any{"command": "/bin/true", "refresh": "/no/such/refresh"})
	out, err := Exec{}.RefreshLogged(ctx, r, false)
	if err == nil || err.Error() != "Could not find command '/no/such/refresh'" || out.Refreshed {
		t.Errorf("RefreshLogged = %+v, %v; want Could not find command", out, err)
	}
	if len(*got) != 0 {
		t.Errorf("debug lines = %+v, want none", *got)
	}
}

func TestExecRefreshCommandTimeout(t *testing.T) {
	r := raltest.ExecRes("x", map[string]any{"command": "/bin/true", "refresh": "/bin/sleep 5", "timeout": 0.2})
	start := time.Now()
	out, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err == nil || err.Error() != "execution expired" || out.Refreshed {
		t.Errorf("RefreshLogged = %+v, %v; want execution expired", out, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; want the 0.2s timeout", elapsed)
	}
}

// M12: a blocking guard skips the refresh command, and the refresh still counts.
func TestExecRefreshCommandGuardsBlock(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	r := raltest.ExecRes("x", map[string]any{"command": "/bin/true", "unless": "/bin/true", "refresh": "/bin/touch " + ran})
	out, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err != nil || !out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %v; want Refreshed", out, err)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("refresh command ran despite the guard (stat err %v)", err)
	}
}

// The refresh command replaces the main command during a refresh.
func TestExecRefreshCommandReplacesMain(t *testing.T) {
	dir := t.TempDir()
	r := raltest.ExecRes("x", map[string]any{
		"command": "/bin/touch " + filepath.Join(dir, "main"), "refresh": "/bin/touch " + filepath.Join(dir, "refresh")})
	if _, err := (Exec{}).RefreshLogged(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "refresh")); err != nil {
		t.Errorf("refresh command did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main")); !os.IsNotExist(err) {
		t.Errorf("main command ran on refresh (stat err %v)", err)
	}
}

// Under noop the guards run and the refresh command does not.
func TestExecRefreshCommandNoop(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	r := raltest.ExecRes("x", map[string]any{"command": "/bin/true", "refresh": "/bin/touch " + ran})
	out, err := Exec{}.RefreshLogged(context.Background(), r, true)
	if err != nil || !out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %v; want Refreshed", out, err)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("refresh command ran under noop (stat err %v)", err)
	}
}

// M12: the debug line names the refresh command, [redacted] when command is marked.
func TestExecRefreshCommandDebugLine(t *testing.T) {
	for _, marked := range []bool{false, true} {
		ctx, got := raltest.DebugContext("debug")
		ctx = sources.WithSources(ctx, debugSources)
		params := map[string]any{"command": "/bin/true", "refresh": "/bin/echo hi"}
		var r catalog.Resource
		want := "Executing '/bin/echo hi'"
		wantRun := "Executing: '/bin/echo hi'"
		if marked {
			r = markedExec(params, "command")
			want, wantRun = "Executing '[redacted]'", "Executing: '[redacted]'"
		} else {
			r = raltest.ExecRes("x", params)
		}
		if _, err := (Exec{}).RefreshLogged(ctx, r, false); err != nil {
			t.Fatal(err)
		}
		raltest.AssertWarnings(t, *got, []report.LogEntry{
			dbg("Exec[x](provider=posix)", want),
			dbg("Puppet", wantRun),
		})
	}
}

// Without a refresh command, a refresh runs the main command with tries.
func TestExecRefreshWithoutRefreshUsesTries(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "n")
	r := raltest.ExecRes("x", map[string]any{"command": countingCommand(counter, 3), "tries": 3.0})
	out, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err != nil || !out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %v; want Refreshed", out, err)
	}
	if got, _ := os.ReadFile(counter); string(got) != "3\n" {
		t.Errorf("counter = %q, want 3 tries", got)
	}
}

// D-78: a catalog that marks only refresh redacts it in the Executing lines and
// in the missing-executable failure.
func TestExecRefreshMarkedOnlyIsRedacted(t *testing.T) {
	ctx, got := raltest.DebugContext("debug")
	ctx = sources.WithSources(ctx, debugSources)
	r := markedExec(map[string]any{"command": "/bin/true", "refresh": "/bin/echo D8SECRET-refresh"}, "refresh")
	if _, err := (Exec{}).RefreshLogged(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	raltest.AssertWarnings(t, *got, []report.LogEntry{
		dbg("Exec[x](provider=posix)", "Executing '[redacted]'"),
		dbg("Puppet", "Executing: '[redacted]'"),
	})

	r = markedExec(map[string]any{"command": "/bin/true", "refresh": "/no/such/D8SECRET-refresh"}, "refresh")
	_, err := Exec{}.RefreshLogged(context.Background(), r, false)
	if err == nil || err.Error() != "Could not find command [command redacted]" {
		t.Errorf("err = %v, want Could not find command [command redacted]", err)
	}
}
