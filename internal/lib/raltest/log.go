// Package raltest is shared test support for the ral engine and the unit
// packages: fakes, catalog builders and the libaugeas trace replay. It imports
// only the standard library, catalog, runlog, report and other internal/lib
// packages, never internal/ral or a unit, so any of their tests can use it.
package raltest

import (
	"context"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// DebugContext returns a context whose run logger keeps entries at level and
// the entries it keeps.
func DebugContext(level string) (context.Context, *[]report.LogEntry) {
	var got []report.LogEntry
	l := runlog.New(level, func(e report.LogEntry) { got = append(got, e) }, nil)
	return runlog.WithLogger(context.Background(), l), &got
}

// LinesAt returns the messages of the debug entries logged at source, in
// order. Entries at other sources (the Executing: line at Puppet, say) are
// left out so a test names only the rows it covers.
func LinesAt(entries []report.LogEntry, source string) []string {
	var out []string
	for _, e := range entries {
		if e.Source == source && e.Level == "debug" {
			out = append(out, e.Message)
		}
	}
	return out
}

func AssertWarnings(t *testing.T, got, want []report.LogEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Level != want[i].Level || got[i].Source != want[i].Source || got[i].Message != want[i].Message {
			t.Errorf("entry %d = {%s %s %q}, want {%s %s %q}", i,
				got[i].Level, got[i].Source, got[i].Message, want[i].Level, want[i].Source, want[i].Message)
		}
	}
}

// WithOutputCap lowers the cap for the test.
func WithOutputCap(t *testing.T, n int) {
	t.Helper()
	old := command.OutputCap
	command.OutputCap = n
	t.Cleanup(func() { command.OutputCap = old })
}

// DebugCtx returns a context whose run logger keeps debug entries and the
// messages it keeps.
func DebugCtx() (context.Context, *[]string) {
	var msgs []string
	l := runlog.New("debug", func(e report.LogEntry) { msgs = append(msgs, e.Message) }, nil)
	return runlog.WithLogger(context.Background(), l), &msgs
}

func ErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
