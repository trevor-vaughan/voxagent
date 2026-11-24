package ral

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/report"
)

type fakeHost struct {
	calls []string
	ev    extension.Evaluation
	err   error
	rr    extension.RefreshResult
	gen   extension.EvalGeneration
	logs  []extension.Log
}

func (f *fakeHost) EvalGenerate(ref string) (extension.EvalGeneration, error) {
	f.calls = append(f.calls, "eval_generate "+ref)
	return f.gen, f.err
}

func (f *fakeHost) Evaluate(ref string, noop bool) (extension.Evaluation, error) {
	f.calls = append(f.calls, "evaluate "+ref)
	return f.ev, f.err
}
func (f *fakeHost) Skip(ref string) (extension.Evaluation, error) {
	f.calls = append(f.calls, "skip "+ref)
	return f.ev, f.err
}
func (f *fakeHost) Refresh(ref string) (extension.RefreshResult, error) {
	f.calls = append(f.calls, "refresh "+ref)
	return f.rr, f.err
}
func (f *fakeHost) OverlyDeferred(ref string) ([]extension.Log, error) {
	f.calls = append(f.calls, "overly_deferred "+ref)
	return f.logs, f.err
}

var load = extension.LoadResult{
	Types:      map[string]extension.TypeInfo{"vox_value": {Class: "tier3", Origin: "libdir", Refreshable: true}},
	Metaparams: []string{"alias", "audit", "before", "loglevel", "noop", "notify", "require", "schedule", "stage", "subscribe", "tag"},
	Resources:  map[string]extension.ResourceInfo{"Vox_value[a]": {ProviderUsed: ptr("file")}},
}

func ptr(s string) *string { return &s }

func cat(params map[string]any) *catalog.Catalog {
	return &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Vox_value", Title: "a", Parameters: params},
		{Type: "File", Title: "/x", Parameters: map[string]any{"noop": true}},
	}}
}

// Review Focus 4: every attribute the catalog gives the type is accepted
// except a metaparameter the engine does not handle, which stays a gap.
func TestExtensionAttrsRefuseUnhandledMetaparams(t *testing.T) {
	c := cat(map[string]any{"value": "one", "path": "/p", "noop": true, "schedule": "daily",
		"audit": "all", "require": "File[/x]", "tag": "t"})
	e := NewExtension("vox_value", &fakeHost{}, load, nil, c)
	reg := NewRegistry(e)
	var gaps []string
	for _, g := range Preflight(reg, &catalog.Catalog{Resources: c.Resources[:1]}) {
		gaps = append(gaps, g.Attribute)
	}
	if !slices.Equal(gaps, []string{"audit", "noop", "schedule"}) {
		t.Errorf("gaps = %v, want the unhandled metaparameters only", gaps)
	}
}

func TestExtensionAnswersPerResource(t *testing.T) {
	rels := map[string]extension.Relations{"Vox_value[a]": {Require: []string{"File[/x]"}, Notify: []string{"Vox_value[b]"}}}
	e := NewExtension("vox_value", &fakeHost{}, load, rels, cat(nil))
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	if got := e.ProviderUsedFor(r); got == nil || *got != "file" {
		t.Errorf("ProviderUsedFor = %v", got)
	}
	want := []AutoRelation{{RelRequire, "File[/x]"}, {RelNotify, "Vox_value[b]"}}
	if got := e.AutoRelations(r); !slices.Equal(got, want) {
		t.Errorf("AutoRelations = %v, want %v", got, want)
	}
	if !e.Refreshable(r) {
		t.Error("Refreshable = false for a refreshable type")
	}
	if e.Type() != "vox_value" {
		t.Errorf("Type = %q", e.Type())
	}
}

func TestExtensionEvaluateConvertsLogs(t *testing.T) {
	name := "value_changed"
	host := &fakeHost{ev: extension.Evaluation{Events: []report.Event{{Name: &name, Status: "success", Time: "t"}},
		InvalidateRefreshes: true, Logs: []extension.Log{{Level: "notice", Source: "/s", Message: "m", File: "/f.pp", Line: 2}}}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	ev, err := e.Evaluate(context.Background(), catalog.Resource{Type: "Vox_value", Title: "a"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Events) != 1 || !ev.InvalidateRefreshes ||
		!slices.Equal(ev.Logs, []report.LogEntry{{Level: "notice", Source: "/s", Message: "m", File: "/f.pp", Line: 2}}) {
		t.Errorf("Evaluate = %+v", ev)
	}
	if !slices.Equal(host.calls, []string{"evaluate Vox_value[a]"}) {
		t.Errorf("calls = %v", host.calls)
	}
}

func TestExtensionRefresh(t *testing.T) {
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	host := &fakeHost{rr: extension.RefreshResult{Refreshed: true}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	if out, err := e.RefreshLogged(context.Background(), r, true); out.Refreshed || err != nil || len(host.calls) != 0 {
		t.Errorf("noop refresh = %+v, %v, calls %v; want no call", out, err, host.calls)
	}
	if out, err := e.RefreshLogged(context.Background(), r, false); !out.Refreshed || err != nil {
		t.Errorf("refresh = %+v, %v", out, err)
	}
	host.rr = extension.RefreshResult{Error: ptr("refresh refused"),
		Logs:          []extension.Log{{Level: "notice", Source: "/s", Message: "callback"}},
		ExceptionLogs: []extension.Log{{Level: "err", Source: "/s", Message: "refresh refused"}}}
	out, err := e.RefreshLogged(context.Background(), r, false)
	if err == nil || err.Error() != "refresh refused" {
		t.Errorf("failed refresh err = %v", err)
	}
	if !slices.Equal(out.Logs, []report.LogEntry{{Level: "notice", Source: "/s", Message: "callback"}}) ||
		!slices.Equal(out.ExceptionLogs, []report.LogEntry{{Level: "err", Source: "/s", Message: "refresh refused"}}) {
		t.Errorf("failed refresh = %+v, want the callback's and the exception's lines apart", out)
	}
	host.err = errors.New("extension host failed: gone")
	if _, err := e.RefreshLogged(context.Background(), r, false); err == nil || err.Error() != "extension host failed: gone" {
		t.Errorf("dead host refresh err = %v", err)
	}
}

func TestExtensionRefusesDirectApply(t *testing.T) {
	e := NewExtension("vox_value", &fakeHost{}, load, nil, cat(nil))
	if _, _, err := e.Apply(context.Background(), catalog.Resource{Type: "Vox_value", Title: "a"}, false); err == nil {
		t.Error("Apply succeeded on an Extension")
	}
	if p, err := e.Prefetch(context.Background(), nil); err != nil || p.Type() != "vox_value" {
		t.Errorf("Prefetch = %v, %v", p, err)
	}
}

func TestExtensionCarriesUnsuitable(t *testing.T) {
	host := &fakeHost{ev: extension.Evaluation{Failed: true, Unsuitable: true}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	ev, err := e.Evaluate(context.Background(), r, false)
	if err != nil || !ev.Unsuitable || !ev.Failed {
		t.Errorf("Evaluate = %+v, %v", ev, err)
	}
	sk, err := e.SkipEvaluation(context.Background(), r)
	if err != nil || !sk.Unsuitable || !sk.Failed {
		t.Errorf("SkipEvaluation = %+v, %v", sk, err)
	}
}

// FailUnsuitable asks the host for the lines Puppet logs for an overly
// deferred resource, and a dead host's error is returned as it is.
func TestExtensionFailUnsuitable(t *testing.T) {
	host := &fakeHost{logs: []extension.Log{{Level: "err", Source: "/s", Message: "Provider never is not functional on this host"}}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	logs, err := e.FailUnsuitable(context.Background(), r)
	if err != nil || !slices.Equal(logs, []report.LogEntry{{Level: "err", Source: "/s", Message: "Provider never is not functional on this host"}}) {
		t.Errorf("FailUnsuitable = %+v, %v", logs, err)
	}
	if !slices.Equal(host.calls, []string{"overly_deferred Vox_value[a]"}) {
		t.Errorf("calls = %v", host.calls)
	}
	host.err = errors.New("extension host failed: gone")
	if logs, err := e.FailUnsuitable(context.Background(), r); err == nil || err.Error() != "extension host failed: gone" || logs != nil {
		t.Errorf("dead host FailUnsuitable = %+v, %v", logs, err)
	}
}

func TestExtensionRetitlingFollowsTheResourceInfo(t *testing.T) {
	tl := load
	tl.Resources = map[string]extension.ResourceInfo{
		"Vox_value[a]": {PuppetRef: "Vox_value[a:x]", PuppetTitle: "a:x", Unresolvable: true},
		"Vox_value[b]": {},
	}
	var p Provider = NewExtension("vox_value", &fakeHost{}, tl, nil, cat(nil))
	re, ok := p.(Retitling)
	if !ok {
		t.Fatal("Extension does not implement Retitling")
	}
	a := catalog.Resource{Type: "Vox_value", Title: "a"}
	if ref, title, ok := re.PuppetRef(a); ref != "Vox_value[a:x]" || title != "a:x" || !ok {
		t.Errorf("PuppetRef = %q, %q, %v, want Vox_value[a:x], a:x, true", ref, title, ok)
	}
	if !re.Unresolvable(a) {
		t.Error("Unresolvable = false for a resource the host reports unresolvable")
	}
	b := catalog.Resource{Type: "Vox_value", Title: "b"}
	if ref, title, ok := re.PuppetRef(b); ref != "" || title != "" || ok {
		t.Errorf("PuppetRef = %q, %q, %v for a resource Puppet names by its catalog ref", ref, title, ok)
	}
	if re.Unresolvable(b) {
		t.Error("Unresolvable = true for a resolvable resource")
	}
}

func TestExtensionEvalGeneratesFollowsTheTypesInfo(t *testing.T) {
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	if NewExtension("vox_value", &fakeHost{}, load, nil, cat(nil)).EvalGenerates(r) {
		t.Error("EvalGenerates = true for a type without eval_generate")
	}
	gl := load
	gl.Types = map[string]extension.TypeInfo{"vox_value": {Class: "tier3", EvalGenerates: true}}
	if !NewExtension("vox_value", &fakeHost{}, gl, nil, cat(nil)).EvalGenerates(r) {
		t.Error("EvalGenerates = false for a type with eval_generate")
	}
}

func TestExtensionSelfRefreshesFollowsTheTypesInfo(t *testing.T) {
	r := catalog.Resource{Type: "Vox_value", Title: "a"}
	if NewExtension("vox_value", &fakeHost{}, load, nil, cat(nil)).SelfRefreshes(r) {
		t.Error("SelfRefreshes = true for a type without self_refresh")
	}
	sl := load
	sl.Types = map[string]extension.TypeInfo{"vox_value": {Class: "tier3", SelfRefresh: true}}
	var p Provider = NewExtension("vox_value", &fakeHost{}, sl, nil, cat(nil))
	if s, ok := p.(SelfRefreshing); !ok || !s.SelfRefreshes(r) {
		t.Error("SelfRefreshes = false for a type with self_refresh")
	}
}

// The host's answer becomes catalog resources as Decode would make them:
// numbers are float64, each child carries its generator, and the overrides
// and logs pass through. Before is dropped (apply-spine.md).
func TestExtensionEvalGenerateConvertsTheHostsAnswer(t *testing.T) {
	host := &fakeHost{gen: extension.EvalGeneration{
		Generation: extension.Generation{
			Resources: []extension.GeneratedResource{{Type: "Notify", Title: "c", Tags: []string{"notify", "c"},
				Kind: "compilable_type", Parameters: map[string]any{"n": json.Number("3"), "l": []any{json.Number("1.5")}}}},
			Containers: map[string]string{"Notify[c]": "Class[main]"},
			Before:     []extension.BeforeEdge{{Source: "Vox_value[a]", Target: "Notify[c]"}},
			Overrides: []extension.Override{{Ref: "File[/x]",
				Parameters: map[string]any{"content": "new", "mode": json.Number("420")}, Sensitive: []string{"content"}}},
		},
		Existing: []string{"File[/x]"},
		Failed:   true,
		Logs:     []extension.Log{{Level: "err", Source: "/Vox_value[a]", Message: "broke", File: "/f.pp", Line: 4}},
	}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	parent := catalog.Resource{Type: "Vox_value", Title: "a"}
	gen, err := e.EvalGenerate(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(gen.Children) != 1 {
		t.Fatalf("children = %+v", gen.Children)
	}
	child := gen.Children[0]
	if child.GeneratedFrom == nil || child.GeneratedFrom.Ref() != "Vox_value[a]" {
		t.Errorf("child GeneratedFrom = %v, want the parent", child.GeneratedFrom)
	}
	child.GeneratedFrom = nil
	wantChild := catalog.Resource{Type: "Notify", Title: "c", Tags: []string{"notify", "c"}, Kind: "compilable_type",
		Parameters: map[string]any{"n": float64(3), "l": []any{1.5}}}
	if !reflect.DeepEqual(child, wantChild) {
		t.Errorf("child = %#v, want %#v", child, wantChild)
	}
	if !reflect.DeepEqual(gen.Containers, map[string]string{"Notify[c]": "Class[main]"}) {
		t.Errorf("containers = %v", gen.Containers)
	}
	wantOverrides := []Override{{Ref: "File[/x]", Parameters: map[string]any{"content": "new", "mode": float64(420)},
		Sensitive: []string{"content"}}}
	if !reflect.DeepEqual(gen.Overrides, wantOverrides) {
		t.Errorf("overrides = %#v, want %#v", gen.Overrides, wantOverrides)
	}
	if !gen.Failed || !slices.Equal(gen.Logs, []report.LogEntry{{Level: "err", Source: "/Vox_value[a]", Message: "broke",
		File: "/f.pp", Line: 4}}) {
		t.Errorf("failed/logs = %v %+v", gen.Failed, gen.Logs)
	}
	if !slices.Equal(host.calls, []string{"eval_generate Vox_value[a]"}) {
		t.Errorf("calls = %v", host.calls)
	}
}

// The resources eval_generate returned that the catalog already held pass
// through in the host's order: the walk applies them in the parent's unit.
func TestExtensionEvalGeneratePassesExisting(t *testing.T) {
	host := &fakeHost{gen: extension.EvalGeneration{Existing: []string{"File[/z]", "Notify[a]"}}}
	e := NewExtension("vox_value", host, load, nil, cat(nil))
	gen, err := e.EvalGenerate(context.Background(), catalog.Resource{Type: "Vox_value", Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(gen.Existing, []string{"File[/z]", "Notify[a]"}) {
		t.Errorf("existing = %v, want [File[/z] Notify[a]]", gen.Existing)
	}
}

func TestExtensionEvalGenerateErrors(t *testing.T) {
	parent := catalog.Resource{Type: "Vox_value", Title: "a"}
	host := &fakeHost{err: errors.New("extension host failed: gone")}
	if _, err := NewExtension("vox_value", host, load, nil, cat(nil)).EvalGenerate(context.Background(), parent); err == nil ||
		err.Error() != "extension host failed: gone" {
		t.Errorf("dead host err = %v", err)
	}
	for _, g := range []extension.Generation{
		{Resources: []extension.GeneratedResource{{Type: "Notify", Title: "c", Kind: "compilable_type",
			Parameters: map[string]any{"n": json.Number("1e400")}}}},
		{Overrides: []extension.Override{{Ref: "File[/x]", Parameters: map[string]any{"mode": json.Number("1e400")}}}},
	} {
		host := &fakeHost{gen: extension.EvalGeneration{Generation: g}}
		_, err := NewExtension("vox_value", host, load, nil, cat(nil)).EvalGenerate(context.Background(), parent)
		if err == nil || !strings.Contains(err.Error(), "1e400") {
			t.Errorf("unrepresentable number err = %v", err)
		}
	}
}

// Puppet in the host already accepted a Tier 3 resource's parameters, so a
// resource changed mid-run is judged only on the metaparameters the walk
// would have to honour.
func TestExtensionMetaparamGaps(t *testing.T) {
	e := NewExtension("vox_value", &fakeHost{}, load, nil, cat(nil))
	r := catalog.Resource{Type: "Vox_value", Title: "a", Parameters: map[string]any{
		"brand_new": "x", "require": "File[/x]", "loglevel": "notice", "schedule": "daily", "noop": true}}
	var attrs []string
	for _, g := range e.MetaparamGaps(r) {
		attrs = append(attrs, g.String())
	}
	want := []string{`Vox_value[a]: attribute "noop" is not implemented`, `Vox_value[a]: attribute "schedule" is not implemented`}
	if !slices.Equal(attrs, want) {
		t.Errorf("gaps = %v, want %v", attrs, want)
	}
	r.Parameters = map[string]any{"loglevel": "info"}
	if gaps := e.MetaparamGaps(r); len(gaps) != 1 || gaps[0].Attribute != "loglevel" {
		t.Errorf("loglevel => info gaps = %v", gaps)
	}
}

// The host writes an ASCII-8BIT string as a Binary map (to_data_hash), and
// children and overrides are applied natively, so both resolve as Decode
// does: an ASCII Binary becomes its string, and invalid base64 is an error.
func TestExtensionEvalGenerateResolvesRichValues(t *testing.T) {
	parent := catalog.Resource{Type: "Vox_value", Title: "a"}
	bin := func(v string) map[string]any { return map[string]any{"__ptype": "Binary", "__pvalue": v} }
	host := &fakeHost{gen: extension.EvalGeneration{Generation: extension.Generation{
		Resources: []extension.GeneratedResource{{Type: "Notify", Title: "c", Kind: "compilable_type",
			Parameters: map[string]any{"message": bin("aGk="), "l": []any{bin("aGk=")}}}},
		Overrides: []extension.Override{{Ref: "File[/x]", Parameters: map[string]any{"content": bin("aGk=")}}},
	}}}
	gen, err := NewExtension("vox_value", host, load, nil, cat(nil)).EvalGenerate(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gen.Children[0].Parameters, map[string]any{"message": "hi", "l": []any{"hi"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("child parameters = %#v, want %#v", got, want)
	}
	if got, want := gen.Overrides[0].Parameters, map[string]any{"content": "hi"}; !reflect.DeepEqual(got, want) {
		t.Errorf("override parameters = %#v, want %#v", got, want)
	}
	for _, g := range []extension.Generation{
		{Resources: []extension.GeneratedResource{{Type: "Notify", Title: "c", Kind: "compilable_type",
			Parameters: map[string]any{"message": bin("aGk")}}}},
		{Overrides: []extension.Override{{Ref: "File[/x]", Parameters: map[string]any{"content": bin("aGk")}}}},
	} {
		host := &fakeHost{gen: extension.EvalGeneration{Generation: g}}
		_, err := NewExtension("vox_value", host, load, nil, cat(nil)).EvalGenerate(context.Background(), parent)
		if err == nil || !strings.Contains(err.Error(), "invalid base64") {
			t.Errorf("invalid base64 err = %v", err)
		}
	}
}
