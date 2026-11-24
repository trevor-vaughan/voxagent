package ral

import (
	"context"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

func TestSourcesRoundTrip(t *testing.T) {
	want := Sources{Resource: "/Stage[main]/Main/Exec[x]", Provider: "Exec[x](provider=posix)", Ref: "Exec[x]"}
	if got := SourcesFrom(WithSources(context.Background(), want)); got != want {
		t.Errorf("SourcesFrom = %+v, want %+v", got, want)
	}
}

func TestSourcesFromBareContextIsZero(t *testing.T) {
	if got := SourcesFrom(context.Background()); got != (Sources{}) {
		t.Errorf("SourcesFrom = %+v, want the zero Sources", got)
	}
}

// debugContext returns a context whose run logger keeps entries at level and
// the entries it keeps.
func debugContext(level string) (context.Context, *[]report.LogEntry) {
	var got []report.LogEntry
	l := runlog.New(level, func(e report.LogEntry) { got = append(got, e) }, nil)
	return runlog.WithLogger(context.Background(), l), &got
}

// linesAt returns the messages of the debug entries logged at source, in
// order. Entries at other sources (the Executing: line at Puppet, say) are
// left out so a test names only the rows it covers.
func linesAt(entries []report.LogEntry, source string) []string {
	var out []string
	for _, e := range entries {
		if e.Source == source && e.Level == "debug" {
			out = append(out, e.Message)
		}
	}
	return out
}
