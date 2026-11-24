package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
)

// procState returns the state letter of /proc/<pid>/stat ("R", "S", "Z", ...)
// and false when the process no longer exists.
func procState(t *testing.T, pid int) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	// The command name is in parentheses and may hold spaces; the state is
	// the first field after the last ")".
	i := strings.LastIndexByte(string(b), ')')
	fields := strings.Fields(string(b[i+1:]))
	if i < 0 || len(fields) == 0 {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, b)
	}
	return fields[0], true
}

// pollUntil checks cond until it holds or timeout passes, and reports whether
// it held.
func pollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readPid waits for the command to write its pid file and returns the pid, and
// SIGKILLs that pid when the test ends so a failing test leaves nothing behind.
func readPid(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	if !pollUntil(2*time.Second, func() bool {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}) {
		t.Fatalf("no pid in %s", pidFile)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// M1: the main command fails returns with "Command exceeded timeout", and
// execute returns at once rather than waiting for the command.
func TestExecTimeoutFailsReturns(t *testing.T) {
	for name, timeout := range map[string]any{"float": 0.2, "string": "0.2"} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			change := execChange(t, raltest.ExecRes("c", map[string]any{"command": "sleep 5", "timeout": timeout}))
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("Apply took %v; want it to return at the 0.2s timeout", elapsed)
			}
			if change.Err == nil || change.Err.Error() != "Command exceeded timeout" {
				t.Errorf("returns error = %v, want Command exceeded timeout", change.Err)
			}
			if !slices.Equal(change.Is.([]string), []string{"0"}) {
				t.Errorf("Is = %v, want [0]", change.Is)
			}
			if len(change.PropertyNotices) != 0 {
				t.Errorf("PropertyNotices = %q; Ruby discards a timed-out command's output", change.PropertyNotices)
			}
		})
	}
}

// M1 'array': a list timeout uses its first element.
func TestExecTimeoutListFirstElement(t *testing.T) {
	start := time.Now()
	change := execChange(t, raltest.ExecRes("c", map[string]any{"command": "sleep 5", "timeout": []any{0.2, 100.0}}))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Apply took %v; want the first element (0.2s) to bound it", elapsed)
	}
	if change.Err == nil || change.Err.Error() != "Command exceeded timeout" {
		t.Errorf("returns error = %v, want Command exceeded timeout", change.Err)
	}
}

// M1 'zero' and 'negative': 0 (and a negative value, clamped to 0) is no
// timeout at all.
func TestExecTimeoutZeroDisables(t *testing.T) {
	for name, timeout := range map[string]any{"zero": 0.0, "zero string": "0", "negative": -5.0} {
		t.Run(name, func(t *testing.T) {
			change := execChange(t, raltest.ExecRes("c", map[string]any{"command": "sleep 0.3", "timeout": timeout}))
			if change.Err != nil {
				t.Errorf("returns failed: %v; want no timeout", change.Err)
			}
		})
	}
}

// D-83: the timeout TERMs the exec's process group, so a backgrounded
// grandchild dies with the shell. Ruby TERMs the shell alone (M3 'orphan').
// A grandchild orphaned to a PID 1 that does not reap it stays a zombie, so a
// "Z" state counts as gone: the property is that TERM reached it.
func TestExecTimeoutKillsGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	change := execChange(t, raltest.ExecRes("c", map[string]any{
		"command": fmt.Sprintf("sleep 30 & echo $! > %s; wait", pidFile),
		"timeout": 0.3,
	}))
	if change.Err == nil || change.Err.Error() != "Command exceeded timeout" {
		t.Fatalf("returns error = %v, want Command exceeded timeout", change.Err)
	}
	pid := readPid(t, pidFile)
	if !pollUntil(time.Second, func() bool {
		state, alive := procState(t, pid)
		return !alive || state == "Z"
	}) {
		state, _ := procState(t, pid)
		t.Errorf("grandchild %d still running (state %s) 1s after the timeout", pid, state)
	}
}

// D-83: TERM is never escalated. A command that ignores it keeps running after
// execute has returned, and the waiter reaps it when it does exit.
func TestExecTimeoutTermTrapperSurvivesAndIsReaped(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	change := execChange(t, raltest.ExecRes("c", map[string]any{
		"command": fmt.Sprintf(`echo $$ > %s; trap "" TERM; sleep 2`, pidFile),
		"timeout": 0.2,
	}))
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Apply took %v; want it to return at the timeout without waiting", elapsed)
	}
	if change.Err == nil || change.Err.Error() != "Command exceeded timeout" {
		t.Fatalf("returns error = %v, want Command exceeded timeout", change.Err)
	}
	pid := readPid(t, pidFile)
	if state, alive := procState(t, pid); !alive || state == "Z" {
		t.Fatalf("command %d not running right after the timeout (alive %v, state %s); TERM must not kill a trapper", pid, alive, state)
	}
	// This test process is the leader's parent, so an unreaped leader stays a
	// zombie for as long as the test runs.
	if !pollUntil(5*time.Second, func() bool {
		_, alive := procState(t, pid)
		return !alive
	}) {
		state, _ := procState(t, pid)
		t.Errorf("command %d not reaped after it exited (state %s)", pid, state)
	}
}

// Review Focus 5: cancelling the run's context after a timeout has returned
// must not SIGKILL the timed-out command -- the agent's normal exit cancels
// its context, and that would escalate what D-83 never escalates.
func TestExecTimeoutChildSurvivesLaterCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes, _, err := Exec{}.Apply(ctx, raltest.ExecRes("c", map[string]any{
		"command": fmt.Sprintf(`echo $$ > %s; trap "" TERM; sleep 2`, pidFile),
		"timeout": 0.2,
	}), false)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != "Command exceeded timeout" {
		t.Fatalf("changes = %+v, want one returns change failing with Command exceeded timeout", changes)
	}
	pid := readPid(t, pidFile)
	cancel()
	// Nothing to poll for: the assertion is that no kill arrives.
	time.Sleep(200 * time.Millisecond)
	if state, alive := procState(t, pid); !alive || state == "Z" {
		t.Errorf("command %d killed after the context was cancelled (alive %v, state %s)", pid, alive, state)
	}
}

// M1 'unless-timeout' and 'onlyif-timeout': a timed-out guard logs an error at
// <resource>/<guard> and blocks the command, as type/exec.rb's check rescues
// Timeout::Error and returns false.
func TestExecGuardTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		guard  string
		value  any
		marked bool
		check  string
	}{
		{"unless", "unless", "sleep 5", false, `Check "sleep 5" exceeded timeout`},
		{"onlyif", "onlyif", "sleep 5", false, `Check "sleep 5" exceeded timeout`},
		{"argv", "unless", []any{[]any{"/bin/sleep", "5"}}, false, `Check ["/bin/sleep", "5"] exceeded timeout`},
		{"marked", "unless", "sleep 5", true, "Check [redacted] exceeded timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			command := "/usr/bin/touch " + marker
			r := raltest.ExecRes("x", map[string]any{"command": command, tc.guard: tc.value, "timeout": 0.2})
			shown := command
			if tc.marked {
				r.SensitiveParameters = []string{tc.guard}
				shown = "[command redacted]"
			}
			ctx, got := raltest.DebugContext("debug")
			ctx = sources.WithSources(ctx, debugSources)
			start := time.Now()
			changes, _, err := Exec{}.Apply(ctx, r, false)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("Apply took %v; want the guard bounded by the 0.2s timeout", elapsed)
			}
			if err != nil || len(changes) != 0 {
				t.Fatalf("Apply = %+v, %v; want no change and no error (the guard blocks)", changes, err)
			}
			var kept []report.LogEntry
			for _, e := range *got {
				if e.Source != "Puppet" && e.Source != debugSources.Provider {
					kept = append(kept, e)
				}
			}
			raltest.AssertWarnings(t, kept, []report.LogEntry{
				{Level: "err", Source: debugSources.Resource + "/" + tc.guard, Message: tc.check},
				dbg(debugSources.Resource, fmt.Sprintf("'%s' won't be executed because of failed check '%s'", shown, tc.guard)),
			})
			if _, err := os.Stat(marker); err == nil {
				t.Error("the command ran though its guard timed out")
			}
		})
	}
}

// M20: exit status 127 fails the command with its output as the error text,
// whatever returns accepts. The main command fails returns; a guard fails the
// resource ("Could not evaluate: <output>"). type/exec.rb sync never gets the
// output back, so none is logged even with logoutput.
func TestExecExit127(t *testing.T) {
	t.Run("main command", func(t *testing.T) {
		change := execChange(t, raltest.ExecRes("c", map[string]any{
			"command": "echo gone >&2; exit 127", "logoutput": "true"}))
		if change.Err == nil || change.Err.Error() != "gone\n" {
			t.Errorf("returns error = %q, want %q", change.Err, "gone\n")
		}
		if len(change.PropertyNotices) != 0 {
			t.Errorf("PropertyNotices = %q, want none", change.PropertyNotices)
		}
	})
	t.Run("returns accepts 127", func(t *testing.T) {
		change := execChange(t, raltest.ExecRes("c", map[string]any{"command": "exit 127", "returns": []any{127.0}}))
		if change.Err == nil || change.Err.Error() != "" {
			t.Errorf("returns error = %v, want the empty output as the error", change.Err)
		}
		if !slices.Equal(change.Is.([]string), []string{"127"}) {
			t.Errorf("Is = %v, want [127]", change.Is)
		}
	})
	t.Run("argv", func(t *testing.T) {
		change := execChange(t, raltest.ExecRes("c", map[string]any{
			"command": []any{"/bin/sh", "-c", "echo argv; exit 127"}}))
		if change.Err == nil || change.Err.Error() != "argv\n" {
			t.Errorf("returns error = %q, want %q", change.Err, "argv\n")
		}
	})
	for _, tc := range []struct{ guard, script, want string }{
		{"unless", "echo u; exit 127", "u\n"},
		{"onlyif", "exit 127", ""},
	} {
		t.Run(tc.guard, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			changes, _, err := Exec{}.Apply(context.Background(), raltest.ExecRes("c", map[string]any{
				"command": "/usr/bin/touch " + marker, tc.guard: tc.script}), false)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Apply error = %v, want exactly %q", err, tc.want)
			}
			if len(changes) != 0 {
				t.Errorf("changes = %+v, want none", changes)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Error("the command ran though its guard exited 127")
			}
		})
	}
}

// D-78: a marked command's 127 output is "[output redacted]", the rendering
// its logged output already has, in the returns failure and in a guard's
// "Could not evaluate:" alike. A missing executable's shell error names it.
func TestExecExit127RedactedWhenMarked(t *testing.T) {
	const want = "[output redacted]"
	change := execChange(t, markedExec(map[string]any{
		"command": "/nonexistent/SECRET-127"}, "command"))
	if change.Err == nil || change.Err.Error() != want {
		t.Errorf("returns error = %q, want %q", change.Err, want)
	}
	_, _, err := Exec{}.Apply(context.Background(), markedExec(map[string]any{
		"command": "/bin/true", "unless": "echo SECRET-guard; exit 127"}, "unless"), false)
	if err == nil || err.Error() != want {
		t.Errorf("guard error = %v, want exactly %q", err, want)
	}
}
