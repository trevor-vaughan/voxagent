package ral

import (
	"context"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/report"
)

// Evaluation is a resource status another implementation produced whole:
// Puppet's own ResourceHarness, through the extension host. Its events and log
// lines are Puppet's words and are recorded verbatim.
type Evaluation struct {
	Events []report.Event
	Logs   []report.LogEntry
	Failed bool
	// Skipped is the prefetch-failure shape (transaction.rb:425-427): failed,
	// skipped, never refreshed.
	Skipped bool
	// Unsuitable is a resource with no suitable provider, which Puppet does not
	// evaluate: relationship_graph.rb:116-137 defers it, and once a drain
	// cannot place it transaction.rb:124-136 marks it failed, not skipped,
	// untimed and never refreshed, and not skipped for failed dependencies
	// either. The walk records that shape itself, with FailUnsuitable's lines.
	Unsuitable bool
	// InvalidateRefreshes is any event's invalidate_refreshes, which
	// event_manager.rb:82 reads and Event#to_data_hash does not carry.
	InvalidateRefreshes bool
}

// Evaluator is implemented by a provider whose resources are evaluated
// elsewhere and come back as a whole status. Puppet's harness logs each
// event itself (Event#send_log) and words each failure; converting that into
// Changes would log every line twice and re-word Puppet's messages. So the
// walk records an Evaluation as it is, and keeps for itself what the host does
// not do: ordering, dependency failure, refresh propagation and counting.
//
// The walk never calls Apply or Refresh on an Evaluator.
type Evaluator interface {
	Provider

	// Evaluate evaluates r. An error is a resource_error: no Puppet status
	// exists, because the host failed or could not serve the call.
	Evaluate(ctx context.Context, r catalog.Resource, noop bool) (Evaluation, error)

	// SkipEvaluation runs what Puppet still runs for a resource skipped for
	// failed dependencies (transaction.rb:111-121): its provider class's
	// prefetch. Events are returned when that prefetch failed.
	SkipEvaluation(ctx context.Context, r catalog.Resource) (Evaluation, error)

	// RefreshLogging's RefreshLogged, for an Evaluator, returns a zero
	// outcome under noop without evaluating anything.
	RefreshLogging

	// FailUnsuitable is called once for a resource the walk deferred for an
	// unsuitable provider and could not place (transaction.rb:124-136); it
	// returns the lines Puppet logs then.
	FailUnsuitable(ctx context.Context, r catalog.Resource) ([]report.LogEntry, error)

	Refreshing
}

// RefreshLogging is implemented by a provider whose refresh reports the log
// lines it produced. The walk calls RefreshLogged in place of Refresh for
// such a provider, native or Evaluator.
type RefreshLogging interface {
	// RefreshLogged is Refresh with the log lines it produced. The lines
	// exclude the walk's "Triggered 'refresh' ..." and "Failed to call
	// refresh: ..." lines, which the walk writes itself, after them. err is
	// the callback's failure; the outcome's lines are meaningful alongside
	// it.
	RefreshLogged(ctx context.Context, r catalog.Resource, noop bool) (RefreshOutcome, error)
}

// Refreshing is implemented by a provider that knows whether r's type
// responds to refresh; event_manager.rb:73 queues an event only for one that
// does. A provider that does not implement it is refreshable.
type Refreshing interface {
	Refreshable(r catalog.Resource) bool
}

// SelfRefreshing is implemented by a provider whose type refreshes itself
// after its own events (type.rb:2186-2189, event_manager.rb:79).
type SelfRefreshing interface {
	SelfRefreshes(r catalog.Resource) bool
}

// RefreshOutcome is what RefreshLogging.RefreshLogged reports.
type RefreshOutcome struct {
	Refreshed bool
	// PropertyNotices are notices the walk logs at <source>/<Property>,
	// before Logs. A native provider does not know its resource's
	// containment path, so it names the property and the walk the source.
	PropertyNotices []PropertyNotice
	// Logs are the lines the callback logged.
	Logs []report.LogEntry
	// ExceptionLogs are the lines Puppet logs for a failed callback's exception
	// (resource.log_exception). event_manager.rb:160-165 writes them after the
	// "Failed to call refresh" line, so the walk does too.
	ExceptionLogs []report.LogEntry
}

// PropertyNotice is one notice line sourced at a property of the resource.
type PropertyNotice struct{ Property, Message string }

// ResourceProviderUsed is implemented by a provider whose provider_used
// differs per resource, as a Puppet type with several provider classes does.
// It takes precedence over Provider.ProviderUsed.
type ResourceProviderUsed interface {
	ProviderUsedFor(r catalog.Resource) *string
}

// Aliasing is implemented by a provider whose resources are known by refs
// other than their own, as a title other than the resource's name makes the
// name an alias (catalog.rb:184-219). Relationships resolve through them.
type Aliasing interface {
	Aliases(r catalog.Resource) []string
}

// Retitling is implemented by a provider whose resources Puppet names by a
// ref other than the catalog's.
type Retitling interface {
	PuppetRef(r catalog.Resource) (ref, title string, ok bool)
	Unresolvable(r catalog.Resource) bool
}

// RelationKind is which of Puppet's four auto-relation blocks declared an
// edge (type.rb:2000-2018).
type RelationKind int

const (
	// RelRequire orders the other resource first, with no events.
	RelRequire RelationKind = iota
	// RelBefore orders the other resource after, with no events.
	RelBefore
	// RelSubscribe orders the other resource first, and refreshes this one on its events.
	RelSubscribe
	// RelNotify orders the other resource after, and refreshes it on this one's events.
	RelNotify
)

// AutoRelation is one auto-relation: the kind, and the OTHER end's ref.
type AutoRelation struct {
	Kind RelationKind
	Ref  string
}

// AutoRelating is implemented by a provider whose type declares auto-relations
// beyond autorequire. The refs it returns are already filtered to the catalog,
// as autorelation (type.rb:2101-2102) filters them.
type AutoRelating interface {
	AutoRelations(r catalog.Resource) []AutoRelation
}

// Override is a generator's change to a resource that already existed.
type Override struct {
	Ref        string
	Parameters map[string]any // catalog representation (float64 numbers)
	// Sensitive names the Parameters the generator marked Sensitive.
	Sensitive []string
}

// EvalGeneration is what a Tier 3 eval_generate produced at the parent's
// turn (additional_resource_generator.rb:56-75).
type EvalGeneration struct {
	// Children are the new resources, each carrying GeneratedFrom = &parent,
	// in the order eval_generate returned them, which is the order the walk
	// applies them in (a File after its generated File ancestor).
	Children []catalog.Resource
	// Containers maps a child's ref to its container's ref, in the catalog's
	// spelling: add_resource contains a generated resource in its generator's
	// container (:131-148).
	Containers map[string]string
	Overrides  []Override
	Logs       []report.LogEntry
	// Failed is Puppet's resources_failed_to_generate: eval_generate raised,
	// and Logs carry the line Puppet logged for it.
	Failed bool
}

// EvalGenerator is implemented by a provider whose type defines eval_generate
// and is evaluated elsewhere (the extension host).
//
// The transaction calls EvalGenerate at the resource's turn in the walk, as
// Puppet's pre_process does (transaction.rb:111-121): before the resource is
// applied, and even when it is about to be skipped for a failed dependency.
// The children are applied as one unit with the resource, each through its
// own type's provider. An error means the host could not serve the call; the
// transaction logs it as Puppet logs a raising eval_generate and still
// applies the resource.
type EvalGenerator interface {
	EvalGenerates(r catalog.Resource) bool
	EvalGenerate(ctx context.Context, r catalog.Resource) (EvalGeneration, error)
}

// MetaparamJudging is implemented by an Evaluator whose resources Puppet
// validates itself, in the extension host. A resource such a generator
// changes mid-run was accepted by Puppet as changed, so the walk re-judges it
// only by MetaparamGaps rather than by the attribute names the catalog gave
// its type before the run.
type MetaparamJudging interface {
	MetaparamGaps(r catalog.Resource) []Gap
}
