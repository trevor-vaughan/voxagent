package runlog

import (
	"context"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
)

// sink collects entries handed to a record or print callback.
type sink struct{ entries []report.LogEntry }

func (s *sink) add(e report.LogEntry) { s.entries = append(s.entries, e) }

func (s *sink) messages() []string {
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Message)
	}
	return out
}

func TestLevelGate(t *testing.T) {
	var rec, pr sink
	l := New("notice", rec.add, pr.add)
	l.Log("debug", "s", "d")
	l.Log("info", "s", "i")
	l.Log("notice", "s", "n")
	l.Log("warning", "s", "w")
	if got := strings.Join(rec.messages(), ","); got != "n,w" {
		t.Errorf("notice logger recorded %q, want n,w", got)
	}

	rec = sink{}
	d := New("debug", rec.add, pr.add)
	for _, lv := range []string{"debug", "info", "notice", "warning", "err", "alert", "emerg", "crit"} {
		d.Log(lv, "s", lv)
	}
	if len(rec.entries) != 8 {
		t.Errorf("debug logger recorded %d entries, want 8", len(rec.entries))
	}
	if !d.Enabled("debug") || l.Enabled("debug") || l.Enabled("info") || !l.Enabled("err") {
		t.Error("Enabled disagrees with the level gate")
	}
}

func TestLogStampsEntry(t *testing.T) {
	var rec sink
	New("debug", rec.add, nil).Log("debug", "Puppet", "hello")
	e := rec.entries[0]
	if e.Level != "debug" || e.Source != "Puppet" || e.Message != "hello" {
		t.Errorf("entry = %+v", e)
	}
	if !strings.HasSuffix(e.Time, "Z") || !strings.Contains(e.Time, "T") {
		t.Errorf("Time %q is not RFC3339 UTC", e.Time)
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	ctx := context.Background()
	if From(ctx) != nil {
		t.Fatal("From on a bare context must be nil")
	}
	var l *Logger
	if l.Enabled("emerg") {
		t.Error("nil logger must not be enabled")
	}
	l.Log("crit", "s", "m")
	Debugf(ctx, "s", "m %d", 1)
	l.Redirect(func(report.LogEntry) {})
}

func TestContextRoundTrip(t *testing.T) {
	l := New("debug", func(report.LogEntry) {}, func(report.LogEntry) {})
	if got := From(WithLogger(context.Background(), l)); got != l {
		t.Errorf("From = %p, want %p", got, l)
	}
}

func TestDebugfFormatsAndGates(t *testing.T) {
	var rec sink
	ctx := WithLogger(context.Background(), New("debug", rec.add, nil))
	Debugf(ctx, "Puppet", "x=%d", 7)
	if got := rec.messages(); len(got) != 1 || got[0] != "x=7" {
		t.Errorf("messages = %v", got)
	}
}

func TestDebugfDisabledAllocatesNothing(t *testing.T) {
	ctx := WithLogger(context.Background(), New("notice", func(report.LogEntry) {}, func(report.LogEntry) {}))
	n := testing.AllocsPerRun(100, func() { Debugf(ctx, "s", "v=%d", 42) })
	if n != 0 {
		t.Errorf("disabled Debugf allocated %v times, want 0", n)
	}
}

func TestReportDebugBudget(t *testing.T) {
	var rec, pr sink
	l := New("debug", rec.add, pr.add)
	chunk := strings.Repeat("x", 1<<20)
	for range 15 {
		l.Log("debug", "s", chunk)
	}
	l.Log("debug", "s", strings.Repeat("y", (1<<20)-1)) // 16 MiB - 1 byte recorded
	if len(rec.entries) != 16 || len(pr.entries) != 0 {
		t.Fatalf("under budget: recorded %d printed %d", len(rec.entries), len(pr.entries))
	}

	l.Log("debug", "s", "zz") // would reach 16 MiB + 1
	if len(rec.entries) != 17 {
		t.Fatalf("want exactly one warning recorded, got %d entries", len(rec.entries)-16)
	}
	w := rec.entries[16]
	const want = "debug log truncated in the report after 16 MiB; the console has the rest"
	if w.Level != "warning" || w.Source != "voxagent" || w.Message != want {
		t.Errorf("warning = %+v", w)
	}
	if len(pr.entries) != 1 || pr.entries[0].Message != "zz" {
		t.Errorf("over-budget line not printed: %v", pr.messages())
	}

	l.Log("notice", "s", "later")
	if rec.entries[len(rec.entries)-1].Message != "later" {
		t.Error("notice after truncation must still be recorded")
	}
	n := len(rec.entries)
	l.Log("debug", "s", "again")
	if len(rec.entries) != n || len(pr.entries) != 2 {
		t.Errorf("later debug: recorded %d->%d printed %d", n, len(rec.entries), len(pr.entries))
	}
}

func TestBudgetExactFitIsRecorded(t *testing.T) {
	var rec, pr sink
	l := New("debug", rec.add, pr.add)
	l.Log("debug", "s", strings.Repeat("x", reportDebugBudget))
	if len(rec.entries) != 1 || len(pr.entries) != 0 {
		t.Errorf("exact fit: recorded %d printed %d", len(rec.entries), len(pr.entries))
	}
}

func TestRedirectKeepsBudget(t *testing.T) {
	var first, second, pr sink
	l := New("debug", first.add, pr.add)
	l.Log("debug", "s", strings.Repeat("x", reportDebugBudget-1))
	l.Redirect(second.add)
	l.Log("debug", "s", "zz")
	if len(second.entries) != 1 || second.entries[0].Level != "warning" {
		t.Errorf("after Redirect want the warning on the new sink, got %v", second.messages())
	}
	if len(pr.entries) != 1 {
		t.Errorf("over-budget line must print, got %d", len(pr.entries))
	}
	if len(first.entries) != 1 {
		t.Errorf("old sink got %d entries, want 1", len(first.entries))
	}
}

func TestSmallDebugAfterTruncationIsNotRecorded(t *testing.T) {
	var rec, pr sink
	l := New("debug", rec.add, pr.add)
	l.Log("debug", "s", strings.Repeat("x", reportDebugBudget-1))
	l.Log("debug", "s", "zz") // does not fit: truncates
	l.Log("debug", "s", "a")  // would fit, but the report is already truncated
	if len(rec.entries) != 2 {
		t.Errorf("recorded %v, want the big line and one warning", rec.messages())
	}
	if got := strings.Join(pr.messages(), ","); got != "zz,a" {
		t.Errorf("printed %q, want zz,a", got)
	}
}

// Host-sourced entries are built already; AddLogs keeps their fields and holds
// them to the level and the debug budget like Log's (D-81).
func TestAddLogsKeepsFieldsAndFollowsBudget(t *testing.T) {
	rep := report.New("n", "production", false)
	var pr sink
	l := New("debug", func(e report.LogEntry) { rep.AddLog(e) }, pr.add)
	ctx := WithLogger(context.Background(), l)

	host := report.LogEntry{Level: "notice", Source: "/File[/x]", Message: "m", File: "/f.pp", Line: 3, Time: "2026-01-01T00:00:00Z"}
	AddLogs(ctx, rep, host)
	if len(rep.Logs) != 1 || rep.Logs[0] != host {
		t.Fatalf("Logs = %+v, want the entry unchanged", rep.Logs)
	}

	big := report.LogEntry{Level: "debug", Source: "Puppet", Message: strings.Repeat("x", reportDebugBudget-1)}
	AddLogs(ctx, rep, big, report.LogEntry{Level: "debug", Source: "Puppet", Message: "zz"},
		report.LogEntry{Level: "debug", Source: "Puppet", Message: "again"},
		report.LogEntry{Level: "err", Source: "Puppet", Message: "boom"})
	var warnings, debugs int
	for _, e := range rep.Logs {
		switch {
		case e.Level == "warning" && e.Message == truncationWarning:
			warnings++
		case e.Level == "debug":
			debugs++
		}
	}
	if warnings != 1 || debugs != 1 {
		t.Errorf("recorded %d warnings and %d debug entries, want 1 and 1", warnings, debugs)
	}
	if got := pr.messages(); len(got) != 2 || got[0] != "zz" || got[1] != "again" {
		t.Errorf("printed %v, want the two over-budget lines", got)
	}
	if last := rep.Logs[len(rep.Logs)-1]; last.Message != "boom" {
		t.Errorf("last entry = %+v, want the err after the truncation", last)
	}
}

func TestAddLogsDropsBelowLevel(t *testing.T) {
	rep := report.New("n", "production", false)
	ctx := WithLogger(context.Background(), New("notice", func(e report.LogEntry) { rep.AddLog(e) }, nil))
	AddLogs(ctx, rep, report.LogEntry{Level: "info", Message: "i"}, report.LogEntry{Level: "notice", Message: "n"})
	if len(rep.Logs) != 1 || rep.Logs[0].Message != "n" {
		t.Errorf("Logs = %+v, want only the notice", rep.Logs)
	}
}

// Without a logger nothing is dropped or bounded: the report takes every entry.
func TestAddLogsWithoutLoggerAddsEverything(t *testing.T) {
	rep := report.New("n", "production", false)
	AddLogs(context.Background(), rep,
		report.LogEntry{Level: "debug", Message: "d"}, report.LogEntry{Level: "info", Message: "i"})
	if len(rep.Logs) != 2 || rep.Logs[0].Message != "d" || rep.Logs[1].Message != "i" {
		t.Errorf("Logs = %+v, want both entries in order", rep.Logs)
	}
}
