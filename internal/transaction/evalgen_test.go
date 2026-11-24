package transaction

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
)

// evalGenStub is an evaluatorStub whose type defines eval_generate, answering
// per ref as the extension host would.
type evalGenStub struct {
	evaluatorStub
	gens    map[string]ral.EvalGeneration
	genErrs map[string]error
	// sources, when set, records the Sources each EvalGenerate call carried.
	sources *[]ral.Sources
}

func (e evalGenStub) EvalGenerates(r catalog.Resource) bool {
	_, ok := e.gens[r.Ref()]
	_, failing := e.genErrs[r.Ref()]
	return ok || failing
}

func (e evalGenStub) EvalGenerate(ctx context.Context, r catalog.Resource) (ral.EvalGeneration, error) {
	*e.calls = append(*e.calls, "eval_generate "+r.Ref())
	if e.sources != nil {
		*e.sources = append(*e.sources, ral.SourcesFrom(ctx))
	}
	if err := e.genErrs[r.Ref()]; err != nil {
		return ral.EvalGeneration{}, err
	}
	return e.gens[r.Ref()], nil
}

// MetaparamGaps implements ral.MetaparamJudging with Puppet's metaparameters.
func (e evalGenStub) MetaparamGaps(r catalog.Resource) []ral.Gap {
	return ral.MetaparamGaps(ral.NewAttrs("alias", "audit", "before", "loglevel", "noop", "notify", "require",
		"schedule", "stage", "subscribe", "tag"), r)
}

// See genRecorder.Prefetch: the promoted method would drop EvalGenerate.
func (e evalGenStub) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return e, nil
}

// paramRecorder is a recorder that also records the parameters each Apply was
// handed, and refuses content "invalid" through ral.Validating.
type paramRecorder struct {
	recorder
	seen *[]string
}

func (p paramRecorder) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	*p.seen = append(*p.seen, fmt.Sprintf("%s content=%v", r.Ref(), r.Parameters["content"]))
	return p.recorder.Apply(ctx, r, noop)
}

func (p paramRecorder) Validate(r catalog.Resource) error {
	if r.Parameters["content"] == "invalid" {
		return errors.New(r.Ref() + ": content is invalid")
	}
	return nil
}

func (p paramRecorder) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	inner, err := p.recorder.Prefetch(ctx, rs)
	if err != nil {
		return nil, err
	}
	p.recorder = inner.(recorder)
	return p, nil
}

// evalGenFixture holds what an eval_generate test reads back.
type evalGenFixture struct {
	calls, applied, refreshed, seen, prefetched, primedSeen []string
	// prefetchErr is what Prefetch returns, per lowercase type.
	prefetchErr map[string]error
	// noChange names the resources whose Apply reports nothing.
	noChange map[string]bool
}

// tx registers stub as vox_evalgen beside a parameter-recording file, and
// recording exec and notify providers that change once per apply.
func (f *evalGenFixture) tx(stub evalGenStub, failOn map[string]bool) *Transaction {
	stub.typ = "vox_evalgen"
	stub.calls = &f.calls
	mk := func(typ, providerName string) recorder {
		return recorder{typ: typ, applied: &f.applied, refreshed: &f.refreshed, failOn: failOn, changes: 1,
			noChange: f.noChange, providerName: providerName, prefetched: &f.prefetched,
			prefetchErr: f.prefetchErr[typ], primedSeen: &f.primedSeen}
	}
	return &Transaction{Registry: ral.NewRegistry(stub, paramRecorder{recorder: mk("file", "posix"), seen: &f.seen},
		mk("exec", "posix"), mk("notify", ""))}
}

// made builds what EvalGenerate returns for a new resource of parent's.
func made(parent catalog.Resource, typ, title string) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Tags: []string{strings.ToLower(typ), title},
		GeneratedFrom: &parent}
}

func logged(rep *report.Report, level, source, message string) bool {
	return slices.ContainsFunc(rep.Logs, func(l report.LogEntry) bool {
		return l.Level == level && l.Source == source && l.Message == message
	})
}

func TestEvalGenerateChildrenApplyThroughTheirOwnProvider(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children:   []catalog.Resource{made(parent, "Notify", "c2"), made(parent, "Notify", "c1")},
		Containers: map[string]string{"Notify[c2]": "Class[main]", "Notify[c1]": "Class[main]"},
	}}}
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil), parent,
			res("Notify", "n", nil), res("Exec", "sub", map[string]any{"subscribe": "Vox_evalgen[p]"})},
		Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: "Vox_evalgen[p]"}, {Source: "Class[main]", Target: "Notify[n]"},
			{Source: "Class[main]", Target: "Exec[sub]"}},
	}
	rep := runCatalog(t, f.tx(stub, nil), c)

	if !slices.Contains(f.calls, "evaluate Vox_evalgen[p]") || slices.Contains(f.calls, "evaluate Notify[c1]") {
		t.Errorf("host calls = %v, want the parent evaluated and no child", f.calls)
	}
	for _, ref := range []string{"Notify[c1]", "Notify[c2]"} {
		if !slices.Contains(f.applied, ref) {
			t.Errorf("%s was not applied by the notify provider: %v", ref, f.applied)
		}
		st := rep.ResourceStatuses[ref]
		want := []string{"Stage[main]", "Main", ref}
		if !slices.Equal(st.ContainmentPath, want) || !st.Generated {
			t.Errorf("%s path = %v generated = %v, want %v and generated", ref, st.ContainmentPath, st.Generated, want)
		}
	}
	if !slices.Equal(f.refreshed, []string{"Exec[sub]"}) {
		t.Errorf("refreshed = %v, want the parent's subscriber once", f.refreshed)
	}
	if !logged(rep, "notice", "/Stage[main]/Main/Exec[sub]", "Triggered 'refresh' from 1 event") {
		t.Errorf("the subscriber's refresh was not one event:\n%s", fmtLogs(rep))
	}
}

func TestEvalGenerateFailureMarksReport(t *testing.T) {
	var f evalGenFixture
	line := "Failed to generate additional resources using 'eval_generate': eval_generate broke"
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Failed: true, Logs: []report.LogEntry{{Level: "err", Source: "/Vox_evalgen[p]", Message: line}}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{res("Vox_evalgen", "p", nil)}})
	if !rep.FailedToGenerate || !logged(rep, "err", "/Vox_evalgen[p]", line) {
		t.Errorf("failed_to_generate = %v, logs:\n%s", rep.FailedToGenerate, fmtLogs(rep))
	}
	if !slices.Contains(f.calls, "evaluate Vox_evalgen[p]") {
		t.Errorf("the parent was not applied: %v", f.calls)
	}
}

func TestEvalGenerateHostErrorIsLogged(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{genErrs: map[string]error{"Vox_evalgen[p]": errors.New("extension host failed: gone")}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{res("Vox_evalgen", "p", nil)}})
	line := "Failed to generate additional resources using 'eval_generate': extension host failed: gone"
	if !rep.FailedToGenerate || !logged(rep, "err", "/Vox_evalgen[p]", line) {
		t.Errorf("failed_to_generate = %v, logs:\n%s", rep.FailedToGenerate, fmtLogs(rep))
	}
	if !slices.Contains(f.calls, "evaluate Vox_evalgen[p]") {
		t.Errorf("the parent was not applied: %v", f.calls)
	}
}

// A native eval_generate logs its own failure at the resource's path, so the
// call carries the resource's Sources as Apply's does.
func TestEvalGenerateCarriesTheResourcesSources(t *testing.T) {
	var f evalGenFixture
	var seen []ral.Sources
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {}}, sources: &seen}
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil), res("Vox_evalgen", "p", nil)},
		Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: "Vox_evalgen[p]"}},
	}
	runCatalog(t, f.tx(stub, nil), c)
	want := ral.Sources{Resource: "/Stage[main]/Main/Vox_evalgen[p]", Provider: "Vox_evalgen[p](provider=prov-p)", Ref: "Vox_evalgen[p]"}
	if !slices.Equal(seen, []ral.Sources{want}) {
		t.Errorf("sources = %+v, want %+v", seen, want)
	}
}

// pre_process generates before the skip is decided, so a parent with a failed
// dependency still generates, and its children are skipped with it.
func TestEvalGenerateRunsForASkippedParent(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", map[string]any{"require": "Exec[fail]"})
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "Notify", "c")}}}}
	rep := runCatalog(t, f.tx(stub, map[string]bool{"Exec[fail]": true}), &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "fail", nil), parent, res("Notify", "n", nil)}})
	if !slices.Contains(f.calls, "eval_generate Vox_evalgen[p]") || !slices.Contains(f.calls, "skip Vox_evalgen[p]") {
		t.Errorf("host calls = %v, want eval_generate then skip", f.calls)
	}
	if st := rep.ResourceStatuses["Notify[c]"]; !st.Skipped || slices.Contains(f.applied, "Notify[c]") {
		t.Errorf("the child was not skipped: %+v, applied %v", st, f.applied)
	}
}

// Review Focus 1's other half: an override reaches a resource the walk has
// not reached yet, so the provider applies the generator's value.
func TestOverrideAppliesBeforeVisit(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Overrides: []ral.Override{{Ref: "File[/f]", Parameters: map[string]any{"content": "new"}}}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "p", map[string]any{"before": "File[/f]"}),
		res("File", "/f", map[string]any{"content": "old"})}})
	if !slices.Equal(f.seen, []string{"File[/f] content=new"}) {
		t.Errorf("file applies = %v, want the overridden content once", f.seen)
	}
	if rep.ResourceStatuses["File[/f]"].Failed {
		t.Error("the overridden File failed")
	}
}

// Review Focus 1: Puppet's edit of a resource it has already evaluated has no
// effect, and neither may voxagent's.
func TestOverrideAfterVisitChangesNothing(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Overrides: []ral.Override{{Ref: "File[/f]", Parameters: map[string]any{"content": "new", "x": "y"}}}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/f", map[string]any{"content": "old", "before": "Vox_evalgen[p]"}),
		res("Vox_evalgen", "p", nil)}})
	if !slices.Equal(f.seen, []string{"File[/f] content=old"}) {
		t.Errorf("file applies = %v, want the original content once", f.seen)
	}
	if st := rep.ResourceStatuses["File[/f]"]; st.Failed || len(rep.ResourceStatuses) != 2 {
		t.Errorf("statuses = %+v", rep.ResourceStatuses)
	}
}

// Review Focus 2: an override the provider cannot honour fails that resource
// with a resource error naming the gap, skips its dependents and lets the
// run continue. It never reaches the provider.
func TestOverrideRevalidatedAgainstProvider(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]any
		message   string
	}{
		{"pre-flight gap", map[string]any{"x": "y"}, `File[/f]: attribute "x" is not implemented`},
		{"validation", map[string]any{"content": "invalid"}, "File[/f]: content is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f evalGenFixture
			stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
				Overrides: []ral.Override{{Ref: "File[/f]", Parameters: tc.overrides}}}}}
			rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
				res("Vox_evalgen", "p", map[string]any{"before": "File[/f]"}),
				res("File", "/f", map[string]any{"content": "old"}),
				res("Notify", "dep", map[string]any{"require": "File[/f]"}),
				res("Notify", "other", nil)}})
			st := rep.ResourceStatuses["File[/f]"]
			if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != tc.message ||
				st.Events[0].Name == nil || *st.Events[0].Name != "resource_error" {
				t.Errorf("File[/f] status = %+v, want a resource_error %q", st, tc.message)
			}
			if !logged(rep, "err", "/File[/f]", "Could not evaluate: "+tc.message) {
				t.Errorf("logs:\n%s", fmtLogs(rep))
			}
			if len(f.seen) != 0 {
				t.Errorf("the refused File reached its provider: %v", f.seen)
			}
			if !rep.ResourceStatuses["Notify[dep]"].Skipped || !slices.Contains(f.applied, "Notify[other]") {
				t.Errorf("dependent skipped = %v, applied = %v", rep.ResourceStatuses["Notify[dep]"].Skipped, f.applied)
			}
		})
	}
}

// Review Focus 4: a generated child whose own generation yields resources is
// refused on that child rather than applied without them. A child whose
// generation yields nothing applies normally. Siblings still apply, and a
// refusal blocks the parent's dependents.
func TestGeneratedChildThatGeneratesIsRefused(t *testing.T) {
	var calls, applied, refreshed []string
	parent := res("Vox_evalgen", "p", nil)
	g := made(parent, "File", "/g")
	stub := evalGenStub{evaluatorStub: evaluatorStub{typ: "vox_evalgen", calls: &calls},
		gens: map[string]ral.EvalGeneration{
			"Vox_evalgen[p]": {Children: []catalog.Resource{g, made(parent, "File", "/plain"),
				made(parent, "Vox_evalgen", "nested"), made(parent, "Vox_evalgen", "quiet"), made(parent, "Notify", "c")}},
			"Vox_evalgen[nested]": {Children: []catalog.Resource{made(parent, "Notify", "grandchild")}},
			"Vox_evalgen[quiet]":  {},
		}}
	gen := genRecorder{recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		children: map[string][]catalog.Resource{"File[/g]": generatedChildren(g, "/g/a")}}
	tx := &Transaction{Registry: ral.NewRegistry(stub, gen,
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1})}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/x", nil), parent, res("Notify", "after", map[string]any{"require": "Vox_evalgen[p]"})}})

	for _, ref := range []string{"File[/g]", "Vox_evalgen[nested]"} {
		message := ref + " generates resources, and resources generated during evaluation that generate further are not implemented"
		st := rep.ResourceStatuses[ref]
		if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != message {
			t.Errorf("%s status = %+v, want the refusal", ref, st)
		}
	}
	if !slices.Contains(calls, "eval_generate Vox_evalgen[nested]") {
		t.Errorf("the nested generator was not asked: %v", calls)
	}
	for _, ref := range []string{"File[/g]", "File[/g/a]", "Notify[grandchild]"} {
		if slices.Contains(applied, ref) {
			t.Errorf("%s was applied: %v", ref, applied)
		}
	}
	if slices.Contains(calls, "evaluate Vox_evalgen[nested]") || !slices.Contains(calls, "evaluate Vox_evalgen[quiet]") {
		t.Errorf("host calls = %v, want quiet evaluated and nested not", calls)
	}
	if !slices.Contains(applied, "File[/plain]") || !slices.Contains(applied, "Notify[c]") {
		t.Errorf("the non-generating children were not applied: %v", applied)
	}
	if !rep.ResourceStatuses["Notify[after]"].Skipped {
		t.Error("the parent's dependent was not skipped for its refused child")
	}
}

// D-42: a new resource whose type has no provider at all is a resource error.
func TestGeneratedChildWithNoProviderIsRefused(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "Package", "tree"), made(parent, "Notify", "c")}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	st := rep.ResourceStatuses["Package[tree]"]
	message := "resource type Package is not implemented"
	if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != message ||
		!logged(rep, "err", "/Package[tree]", "Could not evaluate: "+message) {
		t.Errorf("status = %+v, want the refusal %q; logs:\n%s", st, message, fmtLogs(rep))
	}
	if !slices.Contains(f.applied, "Notify[c]") {
		t.Errorf("applied = %v, want the sibling", f.applied)
	}
}

// A native type the catalog never names is prefetched at its first generated
// resource, with every resource of that type the unit made, as Puppet's
// prefetch_if_necessary does (transaction.rb:243-260), and then applied.
func TestGeneratedChildOfAnUnprimedTypeIsPrimedOnFirstUse(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "Notify", "c2"), made(parent, "Notify", "c1")}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !slices.Contains(f.applied, "Notify[c1]") || !slices.Contains(f.applied, "Notify[c2]") {
		t.Errorf("applied = %v, want both children", f.applied)
	}
	if !slices.Equal(f.prefetched, []string{"notify [Notify[c2] Notify[c1]] after 0 applies"}) {
		t.Errorf("prefetched = %v, want notify once with both children", f.prefetched)
	}
	if st := rep.ResourceStatuses["Notify[c1]"]; st.Failed {
		t.Errorf("status = %+v", st)
	}
}

// A child whose type's prefetch failed, before the walk or at its first
// generated resource, takes the shape every resource of a prefetch-failed type
// takes: failed and skipped, the prefetch failure as its event, and the
// "Skipping because provider prefetch failed" warning. The cause is logged once.
func TestGeneratedChildWhoseTypeFailedPrefetch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		catalog []catalog.Resource
	}{
		{"primed on first use", nil},
		{"failed before the walk", []catalog.Resource{res("Exec", "x", nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := evalGenFixture{prefetchErr: map[string]error{"exec": errors.New("no exec here")}}
			parent := res("Vox_evalgen", "p", nil)
			stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
				Children: []catalog.Resource{made(parent, "Exec", "e1"), made(parent, "Exec", "e2"),
					made(parent, "Notify", "c")}}}}
			resources := append(slices.Clone(tc.catalog), parent,
				res("Notify", "after", map[string]any{"require": "Vox_evalgen[p]"}))
			rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: resources})
			for _, ref := range []string{"Exec[e1]", "Exec[e2]"} {
				st := rep.ResourceStatuses[ref]
				if !st.Failed || !st.Skipped || len(st.Events) != 1 ||
					st.Events[0].Message != "Prefetch failed for exec provider 'posix'" ||
					st.ProviderUsed == nil || *st.ProviderUsed != "posix" || !st.Generated {
					t.Errorf("%s status = %+v", ref, st)
				}
				if !logged(rep, "warning", "/"+ref, "Skipping because provider prefetch failed") {
					t.Errorf("%s has no skip warning:\n%s", ref, fmtLogs(rep))
				}
			}
			if n := strings.Count(fmtLogs(rep), "Could not prefetch exec provider 'posix': no exec here"); n != 1 {
				t.Errorf("the cause was logged %d times:\n%s", n, fmtLogs(rep))
			}
			if slices.Contains(f.applied, "Exec[e1]") || !slices.Contains(f.applied, "Notify[c]") {
				t.Errorf("applied = %v", f.applied)
			}
			if !rep.ResourceStatuses["Notify[after]"].Skipped {
				t.Error("the parent's dependent was not skipped")
			}
		})
	}
}

// A generated resource is judged as pre-flight judges a catalog resource,
// because pre-flight never saw it.
func TestGeneratedChildIsJudgedLikeACatalogResource(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	bad := made(parent, "Notify", "bad")
	bad.Parameters = map[string]any{"x": "y"}
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{bad, made(parent, "Notify", "good")}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	st := rep.ResourceStatuses["Notify[bad]"]
	if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != `Notify[bad]: attribute "x" is not implemented` {
		t.Errorf("status = %+v", st)
	}
	if slices.Contains(f.applied, "Notify[bad]") || !slices.Contains(f.applied, "Notify[good]") {
		t.Errorf("applied = %v", f.applied)
	}
}

// Ruling on Review Focus 2 for Tier 3: Puppet in the host already accepted
// the edit, so a host-evaluated resource is re-judged only by the
// metaparameters the walk would have to honour.
func TestOverrideOfAHostEvaluatedResourceIsJudgedByMetaparamsOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override map[string]any
		refusal  string
	}{
		{"new parameter", map[string]any{"brand_new": "x"}, ""},
		{"schedule", map[string]any{"schedule": "daily"}, `Vox_evalgen[q]: attribute "schedule" is not implemented`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f evalGenFixture
			stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
				Overrides: []ral.Override{{Ref: "Vox_evalgen[q]", Parameters: tc.override}}}}}
			rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
				res("Vox_evalgen", "p", map[string]any{"before": "Vox_evalgen[q]"}), res("Vox_evalgen", "q", nil)}})
			st := rep.ResourceStatuses["Vox_evalgen[q]"]
			if tc.refusal == "" {
				if st.Failed || !slices.Contains(f.calls, "evaluate Vox_evalgen[q]") {
					t.Errorf("status = %+v, calls = %v; want it evaluated", st, f.calls)
				}
				return
			}
			if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != tc.refusal ||
				slices.Contains(f.calls, "evaluate Vox_evalgen[q]") {
				t.Errorf("status = %+v, calls = %v; want the refusal %q", st, f.calls, tc.refusal)
			}
		})
	}
}

// Puppet applies an eval_generate's new resources in the order it returned
// them (additional_resource_generator.rb:131-135; SequentialPrioritizer's
// increasing priorities), not by title.
func TestEvalGenerateChildrenApplyInReturnedOrder(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "Notify", "b"), made(parent, "Notify", "a")}}}}
	runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !slices.Equal(f.applied, []string{"Notify[b]", "Notify[a]"}) {
		t.Errorf("applied = %v, want the returned order", f.applied)
	}
}

// A File child depends on its nearest generated File ancestor
// (connect_resources_to_ancestors, :119-128), so one returned before that
// ancestor waits for it; everything else keeps the returned order.
func TestEvalGenerateFileChildWaitsForItsGeneratedAncestor(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "File", "/d/x/y"), made(parent, "Notify", "n1"),
			made(parent, "File", "/d/x"), made(parent, "Notify", "n2"), made(parent, "File", "/d")}}}}
	runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	want := []string{"Notify[n1]", "Notify[n2]", "File[/d]", "File[/d/x]", "File[/d/x/y]"}
	if !slices.Equal(f.applied, want) {
		t.Errorf("applied = %v, want %v", f.applied, want)
	}
}

// A failed generated ancestor skips its generated descendants, whatever order
// they were returned in.
func TestEvalGenerateFailedAncestorSkipsItsDescendant(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "File", "/d/x"), made(parent, "File", "/d")}}}}
	rep := runCatalog(t, f.tx(stub, map[string]bool{"File[/d]": true}),
		&catalog.Catalog{Resources: []catalog.Resource{parent}})
	if st := rep.ResourceStatuses["File[/d/x]"]; !st.Skipped || slices.Contains(f.applied, "File[/d/x]") {
		t.Errorf("descendant status = %+v, applied = %v", st, f.applied)
	}
}

// D-56: a unit cannot defer one of its members, so an unsuitable parent or
// child is failed at the unit's turn, before the resources after it.
func TestUnsuitableMemberOfAUnitIsFailedAtItsTurn(t *testing.T) {
	for _, unsuitable := range []string{"Vox_evalgen[p]", "Vox_evalgen[c]"} {
		t.Run(unsuitable, func(t *testing.T) {
			var f evalGenFixture
			parent := res("Vox_evalgen", "p", nil)
			stub := evalGenStub{evaluatorStub: evaluatorStub{
				evals: map[string]ral.Evaluation{unsuitable: {Failed: true, Unsuitable: true}},
				unsuitableLogs: map[string][]report.LogEntry{
					unsuitable: {{Level: "err", Source: "/" + unsuitable, Message: "not functional"}}},
			}, gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
				Children: []catalog.Resource{made(parent, "Vox_evalgen", "c")},
			}}}
			rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
				parent, res("Notify", "n", nil)}})
			if n := strings.Count(strings.Join(f.calls, "\n"), "fail_unsuitable "+unsuitable); n != 1 {
				t.Errorf("calls = %q, want fail_unsuitable %s once", f.calls, unsuitable)
			}
			st := rep.ResourceStatuses[unsuitable]
			if !st.Failed || st.Skipped || len(st.Events) != 0 {
				t.Errorf("%s status = %+v", unsuitable, st)
			}
			failedAt := slices.IndexFunc(rep.Logs, func(l report.LogEntry) bool { return l.Message == "not functional" })
			notifyAt := slices.IndexFunc(rep.Logs, func(l report.LogEntry) bool { return strings.HasPrefix(l.Source, "/Notify[n]") })
			if failedAt < 0 || notifyAt < failedAt {
				t.Errorf("want %s failed before Notify[n] applies; logs:\n%s", unsuitable, fmtLogs(rep))
			}
		})
	}
}

// A deferred resource has not had its turn, so a generator's override before
// its next offer still reaches it: Vox_evalgen[u] is set aside, Vox_evalgen[p]
// overrides it with a metaparameter the walk cannot honour, and u is refused
// with that at its next turn rather than failed as unsuitable.
func TestOverrideReachesADeferredResource(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{evaluatorStub: evaluatorStub{
		evals: map[string]ral.Evaluation{"Vox_evalgen[u]": {Failed: true, Unsuitable: true}},
	}, gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Overrides: []ral.Override{{Ref: "Vox_evalgen[u]", Parameters: map[string]any{"schedule": "daily"}}}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "u", nil), res("Vox_evalgen", "p", nil)}})
	if want := []string{"evaluate Vox_evalgen[u]", "eval_generate Vox_evalgen[p]", "evaluate Vox_evalgen[p]"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	st := rep.ResourceStatuses["Vox_evalgen[u]"]
	if want := `Vox_evalgen[u]: attribute "schedule" is not implemented`; !st.Failed || len(st.Events) != 1 || st.Events[0].Message != want {
		t.Errorf("status = %+v, want the refusal %q", st, want)
	}
}

// An override's Sensitive marks reach the target, so its event is redacted as
// a catalog-marked property's is.
func TestOverrideSensitiveMarksTheTarget(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Overrides: []ral.Override{{Ref: "File[/f]", Parameters: map[string]any{"content": "new"},
			Sensitive: []string{"content"}}}}}}
	stub.typ = "vox_evalgen"
	stub.calls = &f.calls
	file := markedProvider{result: []ral.Change{{Property: "content", Was: "old", Is: "new",
		Message: "content changed 'old' to 'new'"}}}
	file.typ = "file"
	tx := &Transaction{Registry: ral.NewRegistry(stub, file)}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "p", map[string]any{"before": "File[/f]"}),
		res("File", "/f", map[string]any{"content": "old"})}})
	evs := rep.ResourceStatuses["File[/f]"].Events
	if len(evs) != 1 || evs[0].PreviousValue != "[redacted]" || evs[0].DesiredValue != "[redacted]" ||
		evs[0].Redacted != true || strings.Contains(evs[0].Message, "new") {
		t.Errorf("events = %+v, want one redacted content event", evs)
	}
}

func TestOverrideMergesSensitiveWithoutMutatingTheCatalog(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "mode"
	target := res("File", "/f", map[string]any{"content": "old"})
	target.SensitiveParameters = shared
	w := &walk{byRef: map[string]catalog.Resource{"File[/f]": target}, visited: map[string]struct{}{},
		refused: map[string]string{}}
	w.override(ral.Override{Ref: "File[/f]", Parameters: map[string]any{"content": "new"},
		Sensitive: []string{"content", "content", "mode"}})
	if got := w.byRef["File[/f]"].SensitiveParameters; !slices.Equal(got, []string{"mode", "content"}) {
		t.Errorf("SensitiveParameters = %v, want [mode content]", got)
	}
	if got := shared[:cap(shared)][1]; got != "" || len(shared) != 1 {
		t.Errorf("the shared backing array was written: %q", got)
	}

	plain := res("File", "/g", nil)
	w.byRef["File[/g]"] = plain
	w.override(ral.Override{Ref: "File[/g]", Parameters: map[string]any{"content": "x"}})
	if got := w.byRef["File[/g]"].SensitiveParameters; got != nil {
		t.Errorf("an override without Sensitive left %v, want nil", got)
	}
}

// existingUnit is inMain over rs with Vox_evalgen[g] first. A resource placed
// after Notify[mid] in rs and applied before it was applied in g's unit.
func existingUnit(gParams map[string]any, rs ...catalog.Resource) *catalog.Catalog {
	return inMain(append([]catalog.Resource{res("Vox_evalgen", "g", gParams), res("Notify", "mid", nil)}, rs...)...)
}

// returning is a stub whose Vox_evalgen[g] returns existing as the catalog
// resources its eval_generate made.
func returning(existing ...string) evalGenStub {
	return evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[g]": {Existing: existing}}}
}

func appliedBefore(applied []string, first, second string) bool {
	i, j := slices.Index(applied, first), slices.Index(applied, second)
	return i >= 0 && j >= 0 && i < j
}

// §5.5 (M1, M13, M14 c): an existing member is applied right after its
// generator with its own out-edges live, so its own notify fires and its
// container counts its event beside the whit's; the whit gives the
// generator's notify target exactly one event.
func TestExistingMemberKeepsOutEdgesAndRefreshesParentTargets(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true, "Exec[n]": true, "Notify[x]": true}}
	ctx, got := logContext("debug")
	rep := runCatalogCtx(t, ctx, f.tx(returning("File[f]"), nil), existingUnit(map[string]any{"notify": "Exec[n]"},
		res("File", "f", map[string]any{"notify": "Notify[x]"}), res("Exec", "n", nil), res("Notify", "x", nil)))

	if !appliedBefore(f.applied, "File[f]", "Notify[mid]") {
		t.Errorf("applied = %v, want File[f] in g's unit, before Notify[mid]", f.applied)
	}
	if n := strings.Count(strings.Join(f.applied, " "), "File[f]"); n != 1 {
		t.Errorf("File[f] applied %d times: %v", n, f.applied)
	}
	if !slices.Contains(f.refreshed, "Notify[x]") {
		t.Errorf("refreshed = %v, want the member's own notify target", f.refreshed)
	}
	if n := strings.Count(strings.Join(f.refreshed, " "), "Exec[n]"); n != 1 ||
		!logged(rep, "notice", "/Stage[main]/Main/Exec[n]", "Triggered 'refresh' from 1 event") {
		t.Errorf("Exec[n] refreshed %d times; logs:\n%s", n, fmtLogs(rep))
	}
	lines := debugLines(*got)
	container := slices.Index(lines, "/Stage[main]/Main/File[f]: The container Class[Main] will propagate my refresh event")
	unit := slices.Index(lines, "/Stage[main]/Main/File[f]: The container g will propagate my refresh event")
	if container < 0 || unit < container {
		t.Errorf("lines = %q, want the container line then the unit line at File[f]", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, "Skipping automatic relationship") {
			t.Errorf("line %q: no edge runs from File[f] to g", line)
		}
	}

	// Class[Main]'s whit is sent the member's event and the unit whit's.
	f = evalGenFixture{noChange: map[string]bool{"Notify[mid]": true}}
	tx := f.tx(returning("File[f]"), nil)
	tx.Noop = true
	rep = runCatalog(t, tx, existingUnit(nil, res("File", "f", nil)))
	if !logged(rep, "notice", "Class[Main]", "Would have triggered 'refresh' from 2 events") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
}

// The member is applied through the primed provider, prefetched once before
// the walk, and the walker's own turn for it applies nothing.
func TestExistingMemberAppliedOnceThroughPrimed(t *testing.T) {
	var f evalGenFixture
	runCatalog(t, f.tx(returning("File[f]"), nil), existingUnit(nil, res("File", "f", nil)))
	var file []string
	for _, line := range f.primedSeen {
		if strings.HasPrefix(line, "File[f]") {
			file = append(file, line)
		}
	}
	if !slices.Equal(file, []string{"File[f] primed=true"}) {
		t.Errorf("File[f] applies = %v, want one through the primed provider", file)
	}
	if !slices.Equal(f.seen, []string{"File[f] content=<nil>"}) {
		t.Errorf("file applies = %v, want File[f] once", f.seen)
	}
	if n := strings.Count(strings.Join(f.prefetched, "\n"), "file "); n != 1 {
		t.Errorf("prefetched = %v, want file once", f.prefetched)
	}
}

// M21 a: a member an edge put before its generator was applied at its own
// turn and is not applied again. It sends the whit nothing, so the
// generator's notify target is not refreshed, and the generator logs
// Puppet's line for the edge it did not add, before it is evaluated. The
// override reaches a resource already applied, so it changes nothing.
func TestExistingMemberAlreadyVisitedIsNotReapplied(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true, "Exec[n]": true}}
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[g]": {Existing: []string{"File[f]"},
		Overrides: []ral.Override{{Ref: "File[f]", Parameters: map[string]any{"content": "concat\n"}}}}}}
	stub.evals = map[string]ral.Evaluation{"Vox_evalgen[g]": {
		Logs: []report.LogEntry{{Level: "notice", Source: "/Stage[main]/Main/Vox_evalgen[g]", Message: "evaluated"}}}}
	ctx, got := logContext("debug")
	rep := runCatalogCtx(t, ctx, f.tx(stub, nil), inMain(
		res("File", "f", map[string]any{"content": "declared\n", "before": "Vox_evalgen[g]"}),
		res("Vox_evalgen", "g", map[string]any{"notify": "Exec[n]"}), res("Exec", "n", nil)))

	if !slices.Equal(f.seen, []string{"File[f] content=declared\n"}) {
		t.Errorf("file applies = %v, want the declared content once", f.seen)
	}
	if slices.Contains(f.refreshed, "Exec[n]") {
		t.Errorf("refreshed = %v: the whit had no event", f.refreshed)
	}
	skipping := slices.IndexFunc(*got, func(e report.LogEntry) bool {
		return e.Level == "debug" && e.Source == "/Stage[main]/Main/Vox_evalgen[g]" &&
			e.Message == "Skipping automatic relationship to File[f]"
	})
	evaluated := slices.IndexFunc(*got, func(e report.LogEntry) bool { return e.Message == "evaluated" })
	if skipping < 0 || evaluated < skipping {
		t.Errorf("want the skip line before g is evaluated; lines = %q", debugLines(*got))
	}
	if slices.Contains(debugLines(*got), "/Stage[main]/Main/File[f]: The container g will propagate my refresh event") {
		t.Errorf("lines = %q: File[f] is not in g's unit", debugLines(*got))
	}
	if st := rep.ResourceStatuses["File[f]"]; st.Failed || st.Skipped {
		t.Errorf("File[f] status = %+v", st)
	}
}

// The D-43 residue: a member with an unvisited predecessor other than its
// generator stays at its own turn, outside the unit.
func TestExistingMemberNotReadyStaysAtItsTurn(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true, "Notify[late]": true, "Exec[n]": true}}
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, f.tx(returning("File[f]"), nil), existingUnit(map[string]any{"notify": "Exec[n]"},
		res("File", "f", map[string]any{"require": "Notify[late]"}), res("Notify", "late", nil), res("Exec", "n", nil)))
	if !appliedBefore(f.applied, "Notify[late]", "File[f]") {
		t.Errorf("applied = %v, want File[f] after Notify[late]", f.applied)
	}
	if n := strings.Count(strings.Join(f.applied, " "), "File[f]"); n != 1 {
		t.Errorf("File[f] applied %d times: %v", n, f.applied)
	}
	if slices.Contains(f.refreshed, "Exec[n]") {
		t.Errorf("refreshed = %v: File[f] was not in g's unit", f.refreshed)
	}
	if slices.Contains(debugLines(*got), "/Stage[main]/Main/File[f]: The container g will propagate my refresh event") {
		t.Errorf("lines = %q: File[f] is not in g's unit", debugLines(*got))
	}
}

// A member whose visited predecessor failed is skipped in the unit, as at its
// own turn, and the skip blocks the generator's dependents.
func TestExistingMemberWithFailedPredecessorIsSkipped(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true}}
	rep := runCatalog(t, f.tx(returning("File[f]"), map[string]bool{"Exec[fail]": true}), inMain(
		res("Exec", "fail", nil), res("Vox_evalgen", "g", nil), res("Notify", "mid", nil),
		res("File", "f", map[string]any{"require": "Exec[fail]"}),
		res("Notify", "dep", map[string]any{"require": "Vox_evalgen[g]"})))
	if st := rep.ResourceStatuses["File[f]"]; !st.Skipped || slices.Contains(f.applied, "File[f]") {
		t.Errorf("File[f] status = %+v, applied = %v", st, f.applied)
	}
	if !logged(rep, "notice", "/Stage[main]/Main/File[f]", "Dependency Exec[fail] has failures: true") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
	if st := rep.ResourceStatuses["Notify[dep]"]; !st.Skipped || slices.Contains(f.applied, "Notify[dep]") {
		t.Errorf("g's dependent status = %+v, applied = %v", st, f.applied)
	}
}

// M10: a failed member blocks its generator's dependents, which name it.
func TestExistingMemberFailureBlocksParentDependents(t *testing.T) {
	var f evalGenFixture
	rep := runCatalog(t, f.tx(returning("File[f]"), map[string]bool{"File[f]": true}), existingUnit(nil,
		res("File", "f", nil), res("Notify", "dep", map[string]any{"require": "Vox_evalgen[g]"})))
	if !rep.ResourceStatuses["File[f]"].Failed {
		t.Errorf("File[f] status = %+v", rep.ResourceStatuses["File[f]"])
	}
	if st := rep.ResourceStatuses["Notify[dep]"]; !st.Skipped ||
		!logged(rep, "notice", "/Stage[main]/Main/Notify[dep]", "Dependency File[f] has failures: true") {
		t.Errorf("g's dependent status = %+v; logs:\n%s", st, fmtLogs(rep))
	}
}

// A generator skipped for a failed dependency still generates, and a member
// is skipped with it, its own dependents too: Puppet's member depends on its
// generator (ARG:119-128).
func TestExistingMemberOfASkippedParentIsSkipped(t *testing.T) {
	var f evalGenFixture
	rep := runCatalog(t, f.tx(returning("File[f]"), map[string]bool{"Exec[fail]": true}), inMain(
		res("Exec", "fail", nil), res("Vox_evalgen", "g", map[string]any{"require": "Exec[fail]"}),
		res("Notify", "mid", nil), res("File", "f", nil),
		res("Notify", "fdep", map[string]any{"require": "File[f]"})))
	if !slices.Contains(f.calls, "eval_generate Vox_evalgen[g]") {
		t.Errorf("calls = %v, want g to generate", f.calls)
	}
	for _, ref := range []string{"File[f]", "Notify[fdep]"} {
		if st := rep.ResourceStatuses[ref]; !st.Skipped || slices.Contains(f.applied, ref) {
			t.Errorf("%s status = %+v, applied = %v", ref, st, f.applied)
		}
	}
	if !slices.Contains(f.applied, "Notify[mid]") {
		t.Errorf("applied = %v, want Notify[mid]", f.applied)
	}
}

// The rules are type-neutral: a host generator may return any catalog
// resource.
func TestExistingMemberOfAnyType(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true, "Exec[n]": true, "Notify[x]": true}}
	ctx, got := logContext("debug")
	rep := runCatalogCtx(t, ctx, f.tx(returning("Notify[m]"), nil), existingUnit(map[string]any{"notify": "Exec[n]"},
		res("Notify", "m", map[string]any{"notify": "Notify[x]"}), res("Exec", "n", nil), res("Notify", "x", nil)))
	if !appliedBefore(f.applied, "Notify[m]", "Notify[mid]") {
		t.Errorf("applied = %v, want Notify[m] in g's unit", f.applied)
	}
	if !slices.Contains(f.refreshed, "Notify[x]") ||
		!logged(rep, "notice", "/Stage[main]/Main/Exec[n]", "Triggered 'refresh' from 1 event") {
		t.Errorf("refreshed = %v; logs:\n%s", f.refreshed, fmtLogs(rep))
	}
	if !slices.Contains(debugLines(*got), "/Stage[main]/Main/Notify[m]: The container g will propagate my refresh event") {
		t.Errorf("lines = %q", debugLines(*got))
	}
}

// A member is applied as at its own turn, so a native Generator's resource
// generates and is applied with what it made, all inside the unit.
func TestExistingMemberGeneratesAsAtItsTurn(t *testing.T) {
	var calls, applied, refreshed []string
	member := res("File", "/d", nil)
	stub := returning("File[/d]")
	stub.typ, stub.calls = "vox_evalgen", &calls
	gen := genRecorder{recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		children: map[string][]catalog.Resource{"File[/d]": generatedChildren(member, "/d/a")}}
	tx := &Transaction{Registry: ral.NewRegistry(stub, gen,
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1})}
	runCatalog(t, tx, existingUnit(nil, member))
	if want := []string{"File[/d]", "File[/d/a]", "Notify[mid]"}; !slices.Equal(applied, want) {
		t.Errorf("applied = %v, want %v", applied, want)
	}
}

// An eval_generate that failed made nothing: Existing is not read, and the
// resource is applied at its own turn.
func TestEvalGenerateFailedHasNoMembers(t *testing.T) {
	f := evalGenFixture{noChange: map[string]bool{"Notify[mid]": true, "Exec[n]": true}}
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[g]": {Failed: true, Existing: []string{"File[f]"}}}}
	runCatalog(t, f.tx(stub, nil), existingUnit(map[string]any{"notify": "Exec[n]"},
		res("File", "f", nil), res("Exec", "n", nil)))
	if !appliedBefore(f.applied, "Notify[mid]", "File[f]") {
		t.Errorf("applied = %v, want File[f] at its own turn", f.applied)
	}
	if slices.Contains(f.refreshed, "Exec[n]") {
		t.Errorf("refreshed = %v: there was no unit", f.refreshed)
	}
}

// A member set aside as unsuitable at its own turn has not had it, so the
// unit takes it; the unit cannot defer it, and fails it at once (D-56). The
// walk's later offer of it does nothing.
func TestExistingMemberDeferredIsFailedInTheUnit(t *testing.T) {
	var f evalGenFixture
	stub := evalGenStub{evaluatorStub: evaluatorStub{
		evals: map[string]ral.Evaluation{"Vox_evalgen[u]": {Failed: true, Unsuitable: true}},
	}, gens: map[string]ral.EvalGeneration{"Vox_evalgen[g]": {Existing: []string{"Vox_evalgen[u]"}}}}
	rep := runCatalog(t, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "u", nil), res("Vox_evalgen", "g", nil)}})
	want := []string{"evaluate Vox_evalgen[u]", "eval_generate Vox_evalgen[g]", "evaluate Vox_evalgen[g]",
		"evaluate Vox_evalgen[u]", "fail_unsuitable Vox_evalgen[u]"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
	if st := rep.ResourceStatuses["Vox_evalgen[u]"]; !st.Failed || st.Skipped {
		t.Errorf("status = %+v", st)
	}
}

// §0 row 8, §5.5 (D-25 retired): under --noop the unit's whit logs "Would have
// triggered" at the generator's title, once, after the members' lines, counting
// the parent's events and every member's.
func TestUnitNoopNoticeCountsEvents(t *testing.T) {
	const title = "/etc/c09exist.conf"
	for _, tc := range []struct {
		name    string
		noop    bool
		changes int    // events the member's Apply reports
		want    string // empty: no notice
	}{
		{"four events", true, 4, "Would have triggered 'refresh' from 4 events"},
		{"one event", true, 1, "Would have triggered 'refresh' from 1 event"},
		{"no events", true, 0, ""},
		{"live run", false, 4, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f evalGenFixture
			stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[" + title + "]": {Existing: []string{"File[f]"}}}}
			stub.typ, stub.calls = "vox_evalgen", &f.calls
			file := recorder{typ: "file", applied: &f.applied, refreshed: &f.refreshed, changes: tc.changes}
			tx := &Transaction{Registry: ral.NewRegistry(stub, file), Noop: tc.noop}
			rep := runCatalog(t, tx, inMain(res("Vox_evalgen", title, nil), res("File", "f", nil)))
			var notices []int
			for i, l := range rep.Logs {
				if strings.Contains(l.Message, "Would have triggered") && l.Source == title {
					notices = append(notices, i)
					if l.Message != tc.want {
						t.Errorf("message = %q, want %q", l.Message, tc.want)
					}
				}
			}
			if tc.want == "" {
				if len(notices) != 0 {
					t.Errorf("notice logged, want none; logs:\n%s", fmtLogs(rep))
				}
				return
			}
			if len(notices) != 1 {
				t.Fatalf("notices = %d, want 1; logs:\n%s", len(notices), fmtLogs(rep))
			}
			for i, l := range rep.Logs {
				if strings.Contains(l.Source, "File[f]") && i > notices[0] {
					t.Errorf("member line %q follows the notice; logs:\n%s", l.Message, fmtLogs(rep))
				}
			}
		})
	}
}
