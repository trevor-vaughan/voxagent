// Package augeastest is the replay harness the augeasproviders units' tests
// share: a case applies catalog resources through providers over a Run whose
// libaugeas calls replay a recorded trace. It is not a _test package because
// the sysctl and ssh units' tests import it; production code never does.
package augeastest

import (
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// CaseResult is what one resource's turn in a replayed case produced: its
// Apply's results, or the prefetch error it failed with, and the log entries
// recorded during the turn (its type's prefetch included, when the turn ran
// it).
type CaseResult struct {
	Ref      string
	Changes  []ral.Change
	Warnings []ral.Warning
	Err      error
	Logs     []report.LogEntry
}

// Messages is each change's Message, in order.
func (c CaseResult) Messages() []string {
	var out []string
	for _, ch := range c.Changes {
		out = append(out, ch.Message)
	}
	return out
}

// DebugAt is the debug messages logged at source during the turn.
func (c CaseResult) DebugAt(source string) []string { return raltest.LinesAt(c.Logs, source) }

// CaseLibdir is the libdir of every case: the recordings' --libdir.
// File.exist? is faked, so its lens directory is the loadpath exactly when
// present names it, and nothing on disk is consulted.
const CaseLibdir = "/tmp/gatelib"

// NewRun is a run over calls whose File.exist? answers true exactly for the
// paths in present.
func NewRun(t *testing.T, calls []raltest.TraceCall, libdir string, present ...string) *core.Run {
	t.Helper()
	r := core.NewRun(raltest.NewScriptLib(t, calls), libdir)
	r.Exists = func(p string) bool { return slices.Contains(present, p) }
	return r
}

// WithVersionMemo replaces the per-process version memo for the test.
func WithVersionMemo(t *testing.T, types map[string]bool) {
	t.Helper()
	core.VersionChecked.Lock()
	old := core.VersionChecked.Types
	core.VersionChecked.Types = types
	core.VersionChecked.Unlock()
	t.Cleanup(func() {
		core.VersionChecked.Lock()
		core.VersionChecked.Types = old
		core.VersionChecked.Unlock()
	})
}

// RunAugeasCase applies rs as the walk would over a run replaying calls
// (File.exist? true exactly for present): each type is prefetched at its first
// resource with all of that type's resources in catalog order, then every
// resource is applied with noop, with the transaction's Sources on the
// context, and the run ends. providers builds the providers over the run; a
// resource's provider is the one whose Type is its lowercased type.
func RunAugeasCase(t *testing.T, calls []raltest.TraceCall, present []string, providers func(*core.Run) []ral.Provider, rs []catalog.Resource, noop bool) []CaseResult {
	t.Helper()
	run := NewRun(t, calls, CaseLibdir, present...)
	defer run.EndRun()
	byType := map[string]ral.Provider{}
	for _, p := range providers(run) {
		byType[p.Type()] = p
	}
	ctx, logs := raltest.DebugContext("debug")
	prefetched := map[string]error{}
	var out []CaseResult
	for _, r := range rs {
		typ := strings.ToLower(r.Type)
		p, ok := byType[typ]
		if !ok {
			t.Fatalf("no provider for %s", r.Type)
		}
		ref := r.Type + "[" + r.Title + "]"
		res := CaseResult{Ref: ref}
		from := len(*logs)
		perr, done := prefetched[typ]
		if !done {
			var same []catalog.Resource
			for _, o := range rs {
				if strings.ToLower(o.Type) == typ {
					same = append(same, o)
				}
			}
			primed, err := p.Prefetch(ctx, same)
			if err == nil {
				byType[typ] = primed
				p = primed
			}
			prefetched[typ], perr = err, err
		}
		if perr != nil {
			res.Err = perr
		} else {
			rctx := sources.WithSources(ctx, sources.Sources{
				Resource: "/Stage[main]/Main/" + ref,
				Provider: ref + "(provider=" + *p.ProviderUsed() + ")",
				Ref:      ref,
			})
			res.Changes, res.Warnings, res.Err = p.Apply(rctx, r, noop)
		}
		res.Logs = append([]report.LogEntry(nil), (*logs)[from:]...)
		out = append(out, res)
	}
	return out
}

// WantFailure checks that r failed in retrieve with msg (Could not evaluate).
func WantFailure(t *testing.T, r CaseResult, msg string) {
	t.Helper()
	if r.Err == nil || r.Err.Error() != msg || len(r.Changes) != 0 {
		t.Errorf("%s: changes %+v err %v, want failure %q", r.Ref, r.Changes, r.Err, msg)
	}
}

// WantMessages fails unless the results' change messages are want, by
// resource.
func WantMessages(t *testing.T, got []CaseResult, want ...[]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d results, want %d", len(got), len(want))
	}
	for i, r := range got {
		if !slices.Equal(r.Messages(), want[i]) {
			t.Errorf("%s: messages %q, want %q", r.Ref, r.Messages(), want[i])
		}
	}
}

// WantNoErrors fails if any result carries an error or a failed change.
func WantNoErrors(t *testing.T, got []CaseResult) {
	t.Helper()
	for _, r := range got {
		if r.Err != nil {
			t.Errorf("%s: err %v", r.Ref, r.Err)
		}
		for _, ch := range r.Changes {
			if ch.Err != nil {
				t.Errorf("%s: change err %v", r.Ref, ch.Err)
			}
		}
	}
}
