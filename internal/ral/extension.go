package ral

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// ExtensionHost is what an Extension needs from the extension host.
// *extension.Host implements it.
type ExtensionHost interface {
	Evaluate(ref string, noop bool) (extension.Evaluation, error)
	Skip(ref string) (extension.Evaluation, error)
	Refresh(ref string) (extension.RefreshResult, error)
	EvalGenerate(ref string) (extension.EvalGeneration, error)
	OverlyDeferred(ref string) ([]extension.Log, error)
}

// Extension applies one legacy (Tier 3) type through the extension host,
// where Puppet's own ResourceHarness evaluates each resource. The transaction
// reaches it as an Evaluator.
type Extension struct {
	typ       string
	host      ExtensionHost
	attrs     Attrs
	info      extension.TypeInfo
	resources map[string]extension.ResourceInfo
	relations map[string]extension.Relations
	// metaparams are Puppet's metaparameter names, from load.
	metaparams Attrs
}

// NewExtension builds the provider for typ (lowercase) from the host's load
// answer and the auto-relations of typ's resources.
//
// Its attributes are every name the catalog gives typ's resources, minus the
// metaparameters the engine does not handle. The host validates the rest in
// Puppet's words (load's invalid list), so a misspelt parameter is refused as
// invalid rather than counted as a porting gap. A metaparameter such as noop
// or schedule stays a pre-flight gap here as it does for every native type,
// because the walk -- not Puppet -- would have to honour it.
func NewExtension(typ string, host ExtensionHost, load extension.LoadResult,
	relations map[string]extension.Relations, c *catalog.Catalog) Extension {
	meta := make(Attrs, len(load.Metaparams))
	for _, m := range load.Metaparams {
		meta[m] = struct{}{}
	}
	attrs := Attrs{}
	for _, r := range c.Resources {
		if !strings.EqualFold(r.Type, typ) {
			continue
		}
		for name := range r.Parameters {
			if _, isMeta := meta[name]; isMeta && !IsEngineAttr(name) {
				continue
			}
			attrs[name] = struct{}{}
		}
	}
	return Extension{typ: typ, host: host, attrs: attrs, info: load.Types[typ],
		resources: load.Resources, relations: relations, metaparams: meta}
}

// Type implements Provider.
func (e Extension) Type() string { return e.typ }

// ProviderUsed implements Provider. The real answer is per resource
// (ProviderUsedFor); this is the fallback for a type-wide question, which no
// Extension is asked because its prefetch never fails.
func (e Extension) ProviderUsed() *string { return nil }

// ProviderUsedFor implements ResourceProviderUsed, from load.
func (e Extension) ProviderUsedFor(r catalog.Resource) *string {
	return e.resources[r.Ref()].ProviderUsed
}

// Aliases implements Aliasing, from load.
func (e Extension) Aliases(r catalog.Resource) []string { return e.resources[r.Ref()].Aliases }

// PuppetRef implements Retitling, from load: ok is false when Puppet names r
// by its catalog ref.
func (e Extension) PuppetRef(r catalog.Resource) (ref, title string, ok bool) {
	info := e.resources[r.Ref()]
	return info.PuppetRef, info.PuppetTitle, info.PuppetRef != ""
}

// Unresolvable implements Retitling, from load.
func (e Extension) Unresolvable(r catalog.Resource) bool { return e.resources[r.Ref()].Unresolvable }

// AutoRequires implements Provider. The type's autorequires arrive with its
// other auto-relations, through AutoRelations.
func (e Extension) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// AutoRelations implements AutoRelating, from the host's relationships answer.
func (e Extension) AutoRelations(r catalog.Resource) []AutoRelation {
	rel := e.relations[r.Ref()]
	var out []AutoRelation
	for _, group := range []struct {
		kind RelationKind
		refs []string
	}{{RelRequire, rel.Require}, {RelBefore, rel.Before}, {RelSubscribe, rel.Subscribe}, {RelNotify, rel.Notify}} {
		for _, ref := range group.refs {
			out = append(out, AutoRelation{Kind: group.kind, Ref: ref})
		}
	}
	return out
}

// Attrs implements Provider. A copy: callers must not reach this provider's set.
func (e Extension) Attrs() Attrs { return maps.Clone(e.attrs) }

// Apply implements Provider by refusing: the walk evaluates an Extension
// through Evaluate, and converting Puppet's status into Changes would
// re-word it (see Evaluator).
func (e Extension) Apply(context.Context, catalog.Resource, bool) ([]Change, []Warning, error) {
	return nil, nil, fmt.Errorf("%s resources are evaluated by the extension host, not applied", e.typ)
}

// Prefetch implements Provider. The host prefetches lazily, per provider
// class, inside Evaluate and SkipEvaluation, as transaction.rb:115 does.
func (e Extension) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return e, nil }

// logCall logs at debug one host round trip: the call, the resource's ref and
// the time since started. Never a parameter, a log line or a reply.
func (Extension) logCall(ctx context.Context, call string, r catalog.Resource, started time.Time) {
	runlog.Debugf(ctx, "voxagent", "extension host: %s %s in %s", call, r.Ref(), time.Since(started).Round(time.Millisecond))
}

// Refresh implements Provider.
func (e Extension) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	out, err := e.RefreshLogged(ctx, r, noop)
	return out.Refreshed, err
}

// Evaluate implements Evaluator.
func (e Extension) Evaluate(ctx context.Context, r catalog.Resource, noop bool) (Evaluation, error) {
	started := time.Now()
	ev, err := e.host.Evaluate(r.Ref(), noop)
	e.logCall(ctx, "evaluate", r, started)
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Events: ev.Events, Logs: ConvertLogs(ev.Logs), Failed: ev.Failed,
		Skipped: ev.Skipped, Unsuitable: ev.Unsuitable, InvalidateRefreshes: ev.InvalidateRefreshes}, nil
}

// SkipEvaluation implements Evaluator.
func (e Extension) SkipEvaluation(_ context.Context, r catalog.Resource) (Evaluation, error) {
	ev, err := e.host.Skip(r.Ref())
	if err != nil {
		return Evaluation{}, err
	}
	return Evaluation{Events: ev.Events, Logs: ConvertLogs(ev.Logs), Failed: ev.Failed, Skipped: ev.Skipped,
		Unsuitable: ev.Unsuitable}, nil
}

// FailUnsuitable implements Evaluator.
func (e Extension) FailUnsuitable(_ context.Context, r catalog.Resource) ([]report.LogEntry, error) {
	logs, err := e.host.OverlyDeferred(r.Ref())
	if err != nil {
		return nil, err
	}
	return ConvertLogs(logs), nil
}

// RefreshLogged implements Evaluator. Under noop the walk writes Puppet's
// "Would have triggered" line itself and nothing is evaluated.
func (e Extension) RefreshLogged(ctx context.Context, r catalog.Resource, noop bool) (RefreshOutcome, error) {
	if noop {
		return RefreshOutcome{}, nil
	}
	started := time.Now()
	rr, err := e.host.Refresh(r.Ref())
	e.logCall(ctx, "refresh", r, started)
	if err != nil {
		return RefreshOutcome{}, err
	}
	out := RefreshOutcome{Refreshed: rr.Refreshed, Logs: ConvertLogs(rr.Logs), ExceptionLogs: ConvertLogs(rr.ExceptionLogs)}
	if rr.Error != nil {
		return out, errors.New(*rr.Error)
	}
	return out, nil
}

// Refreshable implements Evaluator.
func (e Extension) Refreshable(catalog.Resource) bool { return e.info.Refreshable }

// SelfRefreshes implements SelfRefreshing, from load.
func (e Extension) SelfRefreshes(catalog.Resource) bool { return e.info.SelfRefresh }

// MetaparamGaps implements MetaparamJudging.
func (e Extension) MetaparamGaps(r catalog.Resource) []Gap { return MetaparamGaps(e.metaparams, r) }

// EvalGenerates implements EvalGenerator, from load.
func (e Extension) EvalGenerates(catalog.Resource) bool { return e.info.EvalGenerates }

// EvalGenerate implements EvalGenerator. The host has validated every ref it
// names. Two parts of its answer are dropped, because the walk cannot act on
// them (docs/maintainers/apply-spine.md): Existing, the resources eval_generate
// returned that the catalog already held, which Puppet uses to move the
// parent's out-edges onto a completed sentinel; and Before, relationship
// values appended mid-walk, after the walk's order was fixed.
// Children and overrides are applied natively, so their rich values are
// resolved as Decode resolves the catalog's (catalog.ResolveRich): the host's
// to_data_hash writes an ASCII-8BIT string as a Binary.
// Generators is not read: every child's GeneratedFrom is r, which would
// flatten a nested generator to the parent. Today none can arrive: the
// host's eval_generate passes r to add_resource for every new resource, so
// its Generators names only r, and a child that generates in turn is run or
// refused at its own turn (D-42).
func (e Extension) EvalGenerate(ctx context.Context, r catalog.Resource) (EvalGeneration, error) {
	started := time.Now()
	g, err := e.host.EvalGenerate(r.Ref())
	e.logCall(ctx, "eval_generate", r, started)
	if err != nil {
		return EvalGeneration{}, err
	}
	out := EvalGeneration{Containers: g.Containers, Logs: ConvertLogs(g.Logs), Failed: g.Failed}
	for _, gr := range g.Resources {
		child := catalog.Resource{Type: gr.Type, Title: gr.Title, Tags: gr.Tags, Kind: gr.Kind,
			Exported: gr.Exported, GeneratedFrom: &r}
		params, err := catalog.NormaliseNumbers(gr.Parameters)
		if err == nil {
			params, err = catalog.ResolveRich(params)
		}
		if err != nil {
			return EvalGeneration{}, fmt.Errorf("generated %s: %w", child.Ref(), err)
		}
		child.Parameters = params.(map[string]any)
		out.Children = append(out.Children, child)
	}
	for _, o := range g.Overrides {
		params, err := catalog.NormaliseNumbers(o.Parameters)
		if err == nil {
			params, err = catalog.ResolveRich(params)
		}
		if err != nil {
			return EvalGeneration{}, fmt.Errorf("the override of %s: %w", o.Ref, err)
		}
		out.Overrides = append(out.Overrides, Override{Ref: o.Ref, Parameters: params.(map[string]any), Sensitive: o.Sensitive})
	}
	return out, nil
}

// ConvertLogs turns the host's log lines into report entries; the report
// stamps their time.
func ConvertLogs(logs []extension.Log) []report.LogEntry {
	out := make([]report.LogEntry, 0, len(logs))
	for _, l := range logs {
		out = append(out, report.LogEntry{Level: l.Level, Source: l.Source, Message: l.Message, File: l.File, Line: l.Line})
	}
	return out
}
