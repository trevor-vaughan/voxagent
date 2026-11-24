package ral

import (
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/report"
)

// caseResult is what one resource's turn in a replayed case produced: its
// Apply's results, or the prefetch error it failed with, and the log entries
// recorded during the turn (its type's prefetch included, when the turn ran
// it).
type caseResult struct {
	ref      string
	changes  []Change
	warnings []Warning
	err      error
	logs     []report.LogEntry
}

// messages is each change's Message, in order.
func (c caseResult) messages() []string {
	var out []string
	for _, ch := range c.changes {
		out = append(out, ch.Message)
	}
	return out
}

// debugAt is the debug messages logged at source during the turn.
func (c caseResult) debugAt(source string) []string { return linesAt(c.logs, source) }

// augeasCaseLibdir is the libdir of every case: the recordings' --libdir.
// File.exist? is faked, so its lens directory is the loadpath exactly when
// present names it, and nothing on disk is consulted.
const augeasCaseLibdir = "/tmp/gatelib"

// runAugeasCase applies rs as the walk would over a run replaying calls
// (File.exist? true exactly for present): each type is prefetched at its first
// resource with all of that type's resources in catalog order, then every
// resource is applied with noop, with the transaction's Sources on the
// context, and the run ends. providers builds the providers over the run; a
// resource's provider is the one whose Type is its lowercased type.
func runAugeasCase(t *testing.T, calls []traceCall, present []string, providers func(*augeasRun) []Provider, rs []catalog.Resource, noop bool) []caseResult {
	t.Helper()
	run := testAugeasRun(t, calls, augeasCaseLibdir, present...)
	defer run.EndRun()
	byType := map[string]Provider{}
	for _, p := range providers(run) {
		byType[p.Type()] = p
	}
	ctx, logs := debugContext("debug")
	prefetched := map[string]error{}
	var out []caseResult
	for _, r := range rs {
		typ := strings.ToLower(r.Type)
		p, ok := byType[typ]
		if !ok {
			t.Fatalf("no provider for %s", r.Type)
		}
		ref := r.Type + "[" + r.Title + "]"
		res := caseResult{ref: ref}
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
			res.err = perr
		} else {
			rctx := WithSources(ctx, Sources{
				Resource: "/Stage[main]/Main/" + ref,
				Provider: ref + "(provider=" + *p.ProviderUsed() + ")",
				Ref:      ref,
			})
			res.changes, res.warnings, res.err = p.Apply(rctx, r, noop)
		}
		res.logs = append([]report.LogEntry(nil), (*logs)[from:]...)
		out = append(out, res)
	}
	return out
}
