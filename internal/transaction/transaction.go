package transaction

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/units/file"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/graph"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/units/exec"
)

// Transaction applies a catalog.
//
// It talks to providers through ral.Registry and nothing else. It cannot tell
// how a resource was applied, and today every answer comes from a Go-native
// provider -- keeping that boundary now is what makes each later bridge to a
// Ruby-implemented type a swap rather than a rewrite of this file.
type Transaction struct {
	Registry *ral.Registry
	Noop     bool
}

// walk is the state one Run carries from one resource to the next. It exists
// so that applying a resource is a method rather than a loop body, and can be
// called for a resource the catalog does not list.
type walk struct {
	tx  *Transaction
	ctx context.Context
	g   *graph.Graph
	rep *report.Report

	byRef map[string]catalog.Resource
	// filePaths holds the cleaned path of every catalog File (file.FilePath):
	// the path Puppet's path parameter munges its value to (type/file.rb:66-73,
	// File.join(File.split(File.expand_path(value))), which is
	// filepath.Clean for an absolute path). A recursion's exemption of
	// catalog-managed subtrees compares these paths
	// (remove_less_specific_files, :648-673), so File['/d/keep/'] exempts
	// /d/keep as File['/d/keep'] does. Titles and refs stay as given. The
	// set is keyed by path alone because Puppet capitalises every type name
	// (resource.rb:583, munge_type_name :602-606): a File spelled "file", on
	// either side of the lookup, is the same type.
	filePaths map[string]struct{}
	// Provenance the report needs for every resource it mentions, applied or
	// skipped: which containers hold it and what the catalog tagged it with.
	paths map[string][]string
	// Read the current state of every type in the catalog before applying any
	// of it. primed holds the provider each type is applied THROUGH for the rest
	// of the run, in place of the registry's; prefetchFailed holds the types
	// that could not be read at all.
	primed         map[string]ral.Provider
	prefetchFailed map[string]prefetchFailure
	// turnRefs holds, per lowercase type, the refs (in catalog order) of a
	// ral.TurnPrefetching type not yet prefetched; prefetchAtTurn drains it.
	turnRefs map[string][]string
	// failedDeps is Puppet's per-resource failed_dependencies
	// (transaction.rb:333-350): for each resource, the failed resources
	// that block it. propagate_failure builds it hop by hop from direct
	// dependencies; recording each failure on its transitive dependents
	// (block) is the same set, because a resource with a failed
	// prerequisite is skipped rather than applied and so can never fail in
	// its own right.
	failedDeps map[string]map[string]struct{}
	// notified is @failed_dependencies_already_notified (transaction.rb:316-318):
	// each failed resource is named in a "has failures" notice once per run,
	// by the first resource it blocks.
	notified map[string]struct{}
	// warned is Puppet's @stampwarnings (util/warnings.rb:27), keyed on the
	// calling class AND the message: maybe_log returns without yielding when it
	// has already logged that pair, so a condition every file resource in the
	// catalog trips is reported ONCE for the run. A warning that names no class
	// never enters this set.
	//
	// It lives here and not in a provider because it is run state, and providers
	// are stateless values -- the registry holds one File{} for the whole run and
	// it has nowhere to remember anything. Ruby's own state is process-global for
	// the same reason.
	warned map[string]struct{}
	// Counted, not merely flagged: Puppet's refresh notice reports how many
	// events triggered it ("from 1 event", "from 3 events"), and
	// queue_events_for_resource concatenates the notifier's events onto the
	// target's queue rather than setting a bit.
	pendingRefresh map[string]int
	// visited holds every ref the walk has taken its turn for, whits
	// included, and every catalog resource a unit applied before its turn
	// (§5.5 of the concat design), whose turn then does nothing. A
	// generator's override of one of them has no effect, as Puppet's edit of
	// a resource it has already evaluated has none. A deferred ref leaves it
	// until its next turn.
	visited map[string]struct{}
	// deferred holds, for each catalog resource the walk set aside because
	// its provider is not suitable, the subject and the Evaluator that said
	// so, for failUnsuitable when the walk hands it back overly deferred.
	deferred map[string]deferral
	// refused holds, for a resource an eval_generate override left unable to
	// apply, the reason (a pre-flight gap or a validation error). The
	// resource fails with it at its turn instead of reaching its provider.
	refused map[string]string
	// display and titles are Puppet's own ref and title for each catalog
	// resource whose type overrides title, by catalog ref (puppetNames).
	display map[string]string
	titles  map[string]string
	// broken is the exec.ErrStagingBroken a provider returned: the agent's cwd
	// is wrong, so the walk applies nothing more and Run returns it. It also
	// carries take's could-not-run error for a member of a unit, which has no
	// error return of its own.
	broken error
}

// subject is one resource the walk applies: a catalog resource, or a child a
// Generator produced at its parent's turn.
type subject struct {
	ref string
	// display and title are what the report names it by: Puppet's own ref
	// and title for a resource whose type overrides title, else the
	// catalog's. Everything else addresses it by ref.
	display string
	title   string
	r       catalog.Resource
	path    []string // the report's containment_path
	source  string   // the log source: "/" + path joined
}

// deferral is one resource the walk set aside (walk.deferred).
type deferral struct {
	s  subject
	ev ral.Evaluator
}

// result is what applying or skipping one subject leaves for the unit around
// it.
type result struct {
	// unsuitable is the Evaluator that answered that the subject has no
	// suitable provider. Nothing about the subject has been recorded: the
	// caller defers it (a catalog resource at its turn) or fails it at once
	// with failUnsuitable (a member of a unit, apply-spine.md D-56), and the
	// other fields are unset.
	unsuitable ral.Evaluator
	// blocks is the set of failed resources a dependent of this subject
	// inherits: the subject itself when it failed, its own failed
	// dependencies when it was skipped, nothing when it applied cleanly --
	// the set propagate_failure (transaction.rb:333-350) computes.
	blocks map[string]struct{}
	// eventNames is how many distinct event names the subject's queued events
	// carry; queue_events queues, and logs, once per name.
	eventNames int
	// sent is how many events the subject queued, or would have queued, on
	// its own refresh edges: its successful changes, plus one when it was
	// itself refreshed and so passes a restarted event on.
	sent int
}

// Run walks the graph and applies the catalog, recording everything in rep.
//
// The error return is reserved for could-not-run conditions -- a catalog this
// transaction cannot understand at all. A provider refusing one resource is a
// result about that resource: it is recorded, its dependents are skipped, and
// the walk carries on, which is Puppet's semantics.
func (tx *Transaction) Run(ctx context.Context, c *catalog.Catalog, g *graph.Graph, rep *report.Report) error {
	// The report emits this as transaction_evaluation. Recorded even when the
	// walk returns an error: a run that could not complete still took time, and
	// reporting zero would say it did not happen.
	walkStarted := time.Now()
	defer func() { rep.EvaluationSpan = time.Since(walkStarted) }()
	ctx = ral.WithUniqueWarnings(ctx)
	// The registered, unprimed value owns the per-run state its primed copies
	// point to, so it is the one ended -- on every exit, prefetch failure and
	// cancellation included (Ruby closes every class in post_evalable_providers).
	defer tx.endRun()

	byRef := make(map[string]catalog.Resource, len(c.Resources))
	filePaths := make(map[string]struct{})
	for _, r := range c.Resources {
		byRef[r.Ref()] = r
		if strings.ToLower(r.Type) == "file" {
			filePaths[file.FilePath(r)] = struct{}{}
		}
	}
	primed, prefetchFailed := tx.prefetch(ctx, c, rep)
	turnRefs := tx.turnRefs(c)
	display, titles, _ := puppetNames(c, tx.Registry)
	w := &walk{
		tx: tx, ctx: ctx, g: g, rep: rep,
		byRef: byRef, filePaths: filePaths, paths: containmentPaths(c, display),
		primed: primed, prefetchFailed: prefetchFailed, turnRefs: turnRefs,
		display: display, titles: titles,
		failedDeps:     make(map[string]map[string]struct{}),
		notified:       make(map[string]struct{}),
		warned:         make(map[string]struct{}),
		pendingRefresh: make(map[string]int),
		visited:        make(map[string]struct{}),
		deferred:       make(map[string]deferral),
		refused:        make(map[string]string),
	}
	// relationship_graph.rb:114-145: a resource with no suitable provider is
	// set aside, offered again once the walk drains, and failed only when a
	// whole drain places nothing (graph.Walker).
	walker := g.Walk()
	for {
		ref, overlyDeferred, ok := walker.Next()
		if !ok {
			return nil
		}
		if overlyDeferred {
			d := w.deferred[ref]
			delete(w.deferred, ref)
			w.failUnsuitable(d.s, d.ev)
			walker.Done(ref)
			continue
		}
		deferred, err := w.visit(ref)
		if err != nil {
			return err
		}
		if deferred {
			walker.Defer(ref)
			// So that an override before its next turn still reaches it.
			delete(w.visited, ref)
			continue
		}
		walker.Done(ref)
	}
}

// endRun calls EndRun on every registered provider that holds per-run state.
func (tx *Transaction) endRun() {
	for _, typ := range tx.Registry.Types() {
		p, _ := tx.Registry.Provider(typ)
		if e, ok := p.(ral.RunEnding); ok {
			e.EndRun()
		}
	}
}

// visit takes one vertex of the walk: it passes a whit's refresh on, and
// takes a resource no unit has applied already (take). deferred reports a
// resource whose provider is not suitable, which it records nothing for and
// keeps in w.deferred.
func (w *walk) visit(ref string) (deferred bool, err error) {
	if w.g.IsWhit(ref) {
		// Recorded so that a unit can tell its member's predecessors have
		// all had their turn.
		w.visited[ref] = struct{}{}
		// A container's whit applies nothing and has no status: Puppet's
		// report lists no container.
		// With a failed dependency it is skipped silently, names no
		// dependency as notified, and drops what it was sent
		// (transaction.rb:293-323, :401-424).
		//
		// Otherwise it processes its queue as any resource does
		// (event_manager.rb:32-44), and so collapses however many events
		// it was sent into ONE for each of its targets ("B algorithm"):
		// under --noop process_noop_events, which has no whit guard
		// (:174-179), logs "Would have triggered" at the container's name
		// and queues one noop_restart; on a real run Whit#refresh is a
		// no-op that returns true, so process_events queues one restarted
		// and process_callback logs nothing for a whit (:151-155). All-noop
		// is exactly tx.Noop, because a per-resource noop is a pre-flight
		// gap.
		pending, due := w.pendingRefresh[ref]
		delete(w.pendingRefresh, ref)
		if len(w.failedDeps[ref]) > 0 {
			// transaction.rb:282, at the container's name.
			runlog.Debugf(w.ctx, w.g.Label(ref), "Resource is being skipped, unscheduling all events")
		}
		if due && len(w.failedDeps[ref]) == 0 {
			if w.tx.Noop {
				w.rep.Log("notice", w.g.Label(ref), refreshMessage("Would have triggered", pending))
			}
			w.logContainerPropagation(w.g.Label(ref), ref, 1)
			for _, target := range w.g.RefreshTargets(ref) {
				w.pendingRefresh[target]++
			}
		}
		return false, nil
	}
	if _, applied := w.visited[ref]; applied {
		// An eval_generate returned it, and its generator's unit applied it.
		return false, nil
	}
	r, ok := w.byRef[ref]
	if !ok {
		return false, fmt.Errorf("graph names %s, which is not in the catalog", ref)
	}
	s := w.subject(ref, r)
	res, err := w.take(s)
	if err != nil {
		return false, err
	}
	if res.unsuitable != nil {
		w.deferred[ref] = deferral{s: s, ev: res.unsuitable}
		return true, nil
	}
	return false, nil
}

// subject is catalog resource r, at ref, as the walk applies it.
func (w *walk) subject(ref string, r catalog.Resource) subject {
	// Ruby sources every log line for a resource at its full containment
	// path -- "/Stage[main]/Main/Node[default]/File[x]" -- with the property
	// appended for a property change. The leading slash and the containers
	// are part of the value consumers match on, so the bare reference is a
	// different string rather than a shorter spelling of the same one.
	source := "/" + strings.Join(w.paths[ref], "/")
	s := subject{ref: ref, display: ref, title: r.Title, r: r, path: w.paths[ref], source: source}
	if display, ok := w.display[ref]; ok {
		s.display, s.title = display, w.titles[ref]
	}
	return s
}

// take applies catalog resource s, at its turn or as a member of a unit: it
// refuses a type whose prefetch failed, skips a resource with a failed
// dependency, and otherwise applies the resource through its primed provider,
// as a unit when it generates. Its result is what its dependents, and a unit
// around it, see; result.unsuitable is set, and nothing recorded, for a
// provider that is not suitable.
func (w *walk) take(s subject) (result, error) {
	ref, r := s.ref, s.r
	w.visited[ref] = struct{}{}
	w.prefetchAtTurn(strings.ToLower(r.Type))

	// A type whose prefetch failed is refused WHOLE: nothing of that type is
	// applied, and every resource of it reports the same failure. This is
	// checked before the failed-dependency branch below because Ruby's order
	// is the same -- propagate_failure (transaction.rb:333-339) adds the
	// failure event to every resource of the type unconditionally, and only
	// then does skip? (:401) choose a reason to log. So the STATUS is
	// identical whether or not a dependency also failed, and only the log
	// line differs.
	//
	// Measured non-root over `package{'tree'}` and a `package{'zsh'}` that
	// requires it: both failed=true skipped=true with one resource_error
	// event, tree warned "Skipping because provider prefetch failed" and zsh
	// warned for its failed dependency instead.
	//
	// This branch returns before visitUnit, so a ral.Generator's resource
	// would never generate here; Puppet would generate its children and
	// report them failed as well. No Generator's type can fail Prefetch
	// (see ral.Generator), so the difference cannot arise.
	if failure, failedType := w.prefetchFailed[strings.ToLower(r.Type)]; failedType {
		w.failPrefetched(s, failure)
		return result{blocks: map[string]struct{}{ref: {}}}, nil
	}

	// The PRIMED provider, never the registry's. dnf's Apply reads an
	// installed-package map its Prefetch filled in, and the registry holds
	// the empty value -- so applying through the registry would fail every
	// package resource with "the dnf provider was not prefetched".
	//
	// Absent means the type has no provider at all: prefetch populates this
	// from the registry for every non-container type in the catalog, and
	// leaves out only the ones the registry does not know, which is the
	// condition this branch has always refused.
	provider, hasProvider := w.primed[strings.ToLower(r.Type)]
	if message, refused := w.refused[ref]; refused {
		// Checked before generation: a resource its provider cannot apply
		// as overridden must not generate from those parameters either.
		provider, hasProvider = refuse(provider, r, errors.New(message)), true
	}
	if eg, ok := provider.(ral.EvalGenerator); hasProvider && ok && eg.EvalGenerates(r) {
		return w.visitHostUnit(eg, provider, s), w.broken
	}
	if gen, ok := provider.(ral.Generator); hasProvider && ok {
		return w.visitUnit(gen, provider, s), w.broken
	}
	var res result
	if deps := w.failedDeps[ref]; len(deps) > 0 {
		res = w.skip(s, deps)
	} else if !hasProvider {
		// Pre-flight should have refused this catalog. Reaching here means
		// it was skipped or is wrong, and either way the run cannot make a
		// claim about a catalog it did not understand.
		return result{}, fmt.Errorf("no provider for %s; pre-flight did not run or is incomplete", ref)
	} else {
		res = w.apply(provider, s, true)
	}
	return res, w.broken
}

// failPrefetched records s, a resource of a type whose prefetch failed: the
// type is refused WHOLE, so nothing of it is applied and every resource of it
// reports the same failure, whether the catalog declared it or a generator
// made it.
func (w *walk) failPrefetched(s subject, failure prefetchFailure) {
	status := report.ResourceStatus{
		Resource: s.display, ResourceType: s.r.Type, Title: s.title,
		File: s.r.File, Line: s.r.Line,
		ContainmentPath: s.path, Tags: s.r.Tags, Generated: s.r.GeneratedFrom != nil,
		// Named even though it could not be read. Measured: the
		// non-root report carries provider_used "dnf" on all six
		// packages -- Ruby resolves the provider before it prefetches,
		// so the report can still say which one refused.
		ProviderUsed: failure.providerUsed,
		// EvaluationTime is deliberately left at zero. Ruby writes null:
		// the resource was never evaluated, so there is no span. The
		// leaf is declared volatile in tests/spine/allowlist.json, so
		// the two spellings of "no time was spent" never meet. Untimed
		// keeps it out of metrics.time, as the null does in Ruby.
		Failed: true, Skipped: true, Untimed: true,
		// One non-audit event, so out_of_sync_count is 1 and
		// change_count stays 0 -- resource/status.rb add_event's rule,
		// and measured on every one of the six.
		OutOfSyncCount: 1,
	}
	resourceError := "resource_error"
	// Redacted false, and Property left nil. See report.Event: an event
	// Ruby builds without a property takes event.rb:38's keyword
	// default, where a property event carries the property's own
	// `sensitive` and so reports null.
	status.Events = append(status.Events, report.Event{
		Message: failure.eventMessage, Name: &resourceError,
		Status: "failure", Redacted: false,
		Time: time.Now().UTC().Format(time.RFC3339Nano),
	})
	w.rep.AddResource(status)
	runlog.Debugf(w.ctx, s.source, "Resource is being skipped, unscheduling all events")
	// A WARNING, not the err that carried the cause. The cause is logged
	// once for the run, by prefetch; repeating it per resource would put
	// six err entries in a log Ruby gives exactly one, and the report
	// differential compares log content as a multiset.
	if deps := w.failedDeps[s.ref]; len(deps) > 0 {
		logDependencySkip(w.rep, s.source, deps, w.notified, w.display)
	} else {
		w.rep.Log("warning", s.source, "Skipping because provider prefetch failed")
	}
	block(w.failedDeps, w.g.Dependents(s.ref), s.ref)
}

// skip records s as skipped for the failed resources in deps and logs
// Puppet's lines for it, unless its Evaluator answers that its provider is not
// suitable (result.unsuitable): relationship_graph.rb:116-137 defers such a
// resource before its dependencies are looked at.
func (w *walk) skip(s subject, deps map[string]struct{}) result {
	status := report.ResourceStatus{
		Resource: s.display, ResourceType: s.r.Type, Title: s.title,
		File: s.r.File, Line: s.r.Line, Skipped: true,
		ContainmentPath: s.path, Tags: s.r.Tags,
		Generated: s.r.GeneratedFrom != nil,
		// Never evaluated, so Ruby writes a null evaluation_time and
		// calculate_time_metrics leaves it out of metrics.time.
		Untimed: true,
	}
	// resource/status.rb:176 names the provider on every status, so a skipped
	// File reports "posix" in Ruby.
	if p, ok := w.primed[strings.ToLower(s.r.Type)]; ok {
		status.ProviderUsed = providerUsed(p, s.r)
		if ev, ok := p.(ral.Evaluator); ok {
			// pre_process still prefetches a resource it is about to skip, and
			// propagate_failure adds the prefetch failure to its status
			// (transaction.rb:111-121, :333-339). An unreachable host leaves
			// it unevaluable, which is a resource_error like any other.
			evaluation, err := ev.SkipEvaluation(w.withSources(s, p), s.r)
			if err == nil && evaluation.Unsuitable {
				// Its status waits for failUnsuitable. A line the call
				// logged is Puppet's at this moment, so it is kept.
				runlog.AddLogs(w.ctx, w.rep, evaluation.Logs...)
				return result{unsuitable: ev}
			}
			if err != nil {
				resourceError := "resource_error"
				evaluation = ral.Evaluation{Failed: true, Events: []report.Event{{
					Message: err.Error(), Name: &resourceError, Status: "failure", Redacted: false,
					Time: time.Now().UTC().Format(time.RFC3339Nano)}}}
				w.rep.Log("err", s.source, "Could not evaluate: "+err.Error())
			}
			runlog.AddLogs(w.ctx, w.rep, evaluation.Logs...)
			status.Failed = evaluation.Failed
			if len(evaluation.Events) > 0 {
				status.Events = evaluation.Events
				status.OutOfSyncCount = len(evaluation.Events)
			}
		}
	}
	w.rep.AddResource(status)
	runlog.Debugf(w.ctx, s.source, "Resource is being skipped, unscheduling all events")
	logDependencySkip(w.rep, s.source, deps, w.notified, w.display)
	// No propagation here on purpose. g.Dependents is the TRANSITIVE
	// closure, so the failure branch below has already marked every
	// resource downstream of the failure, however deep. Re-deriving a
	// closure per skipped resource can only re-insert keys the set
	// already holds, and the cost is quadratic: measured on a chain of
	// 8000 whose head fails, 5.197s against 11ms, quadrupling per
	// doubling. It changed no result at any size.
	return result{blocks: deps}
}

// failUnsuitable records s, a resource whose provider is not suitable, as
// Puppet's overly-deferred handler leaves it (transaction.rb:124-136): failed,
// not skipped, untimed, with no events and no dependency lines, never
// refreshed, and blocking its dependents. ev's lines are logged; an error from
// it is a resource_error.
func (w *walk) failUnsuitable(s subject, ev ral.Evaluator) result {
	status := report.ResourceStatus{
		Resource: s.display, ResourceType: s.r.Type, Title: s.title,
		File: s.r.File, Line: s.r.Line,
		ContainmentPath: s.path, Tags: s.r.Tags, Generated: s.r.GeneratedFrom != nil,
		ProviderUsed: providerUsed(ev, s.r),
		// Ruby never applies it, so it is never timed and writes null.
		Failed: true, Untimed: true,
	}
	logs, err := ev.FailUnsuitable(w.withSources(s, ev), s.r)
	if err != nil {
		resourceError := "resource_error"
		status.Events = []report.Event{{Message: err.Error(), Name: &resourceError, Status: "failure",
			Redacted: false, Time: time.Now().UTC().Format(time.RFC3339Nano)}}
		status.OutOfSyncCount = 1
		logs = []report.LogEntry{{Level: "err", Source: s.source, Message: "Could not evaluate: " + err.Error()}}
	}
	runlog.AddLogs(w.ctx, w.rep, logs...)
	w.rep.AddResource(status)
	delete(w.pendingRefresh, s.ref)
	block(w.failedDeps, w.g.Dependents(s.ref), s.ref)
	return result{blocks: map[string]struct{}{s.ref: {}}}
}

// apply applies one subject through provider and records everything about it:
// warnings, its status and events, its inbound refresh, and the dependents its
// failure blocks. propagate false withholds its outgoing refresh events: the
// members of a unit with children send one event for the whole unit instead
// (visitUnit). An Evaluator's answer that the provider is not suitable records
// nothing (result.unsuitable).
func (w *walk) apply(provider ral.Provider, s subject, propagate bool) result {
	ref, r, source := s.ref, s.r, s.source
	ctx := w.withSources(s, provider)
	started := time.Now()
	evaluator, evaluated := provider.(ral.Evaluator)
	var (
		changes    []ral.Change
		warnings   []ral.Warning
		evaluation ral.Evaluation
		applyErr   error
	)
	if evaluated {
		evaluation, applyErr = evaluator.Evaluate(ctx, r, w.tx.Noop)
		if applyErr == nil && evaluation.Unsuitable {
			// Its status waits for failUnsuitable. A line the call logged
			// is Puppet's at this moment, so it is kept.
			runlog.AddLogs(w.ctx, w.rep, evaluation.Logs...)
			return result{unsuitable: evaluator}
		}
	} else {
		changes, warnings, applyErr = provider.Apply(ctx, r, w.tx.Noop)
	}
	// §4.2 item 5 of the exec design: stop at once, recording nothing.
	if errors.Is(applyErr, exec.ErrStagingBroken) {
		w.broken = fmt.Errorf("%s: %w", ref, applyErr)
		return result{}
	}

	// A failed flush (ral.FlushError) made its changes first: they take the
	// success path below, and the failure follows them as the resource_error
	// at the end of that path. Only a native Apply can return one.
	var flushErr *ral.FlushError
	if !evaluated && errors.As(applyErr, &flushErr) {
		applyErr = nil
	}

	// Logged here -- immediately after the apply, before the resource's own
	// status is assembled -- because that is where Ruby reaches it. The
	// warning this channel exists for comes from type/file/owner.rb:36,
	// inside insync?, which runs during the resource's evaluation and ahead
	// of every branch below; and it is emitted whether the resource went on
	// to fail or not, so it must not sit inside one of those branches.
	//
	// Log ORDER relative to the resource's own lines is not compared:
	// tests/spine/allowlist.json declares logs a multiset, because voxagent
	// commits to its own deterministic walk order rather than reproducing
	// Puppet's. Log CONTENT is compared, and the dedupe is content -- one
	// entry versus three is a divergence a multiset comparison catches.
	w.logWarnings(warnings)
	status := report.ResourceStatus{
		Resource: s.display, ResourceType: r.Type, Title: s.title,
		File: r.File, Line: r.Line,
		ContainmentPath: s.path, Tags: r.Tags, Generated: r.GeneratedFrom != nil,
		ProviderUsed:   providerUsed(provider, r),
		EvaluationTime: time.Since(started).Seconds(),
	}

	if applyErr != nil {
		status.Failed = true
		// resource/status.rb states the contract outright: "a status of
		// failed will always be accompanied by an event with some
		// explanatory power". resource_harness.rb honours it by calling
		// failed_because, which logs "Could not evaluate: <detail>" and
		// synthesizes a resource_error event carrying the detail. Setting
		// the flag and writing only a log line left a report consumer
		// reading events with a failed resource and nothing to say about it.
		//
		// The event is resource_error because a bare error return means
		// exactly that: no property can be blamed. That is now the NARROW
		// case -- a stat that is not IsNotExist, an
		// unimplemented ensure value, an unparseable mode. A failure that
		// can name a property travels as a Change.Err and is attributed in
		// the loop below, which is Puppet's commoner path.
		//
		// Ruby has the same two paths and this is its second one:
		// resource_harness.rb:29's outer rescue calls
		// status.failed_because (resource/status.rb:140), for failures
		// outside any property sync.
		resourceError := "resource_error"
		// Redacted false, Property nil -- the no-property event shape
		// documented on report.Event.
		status.Events = append(status.Events, report.Event{
			Message: applyErr.Error(), Name: &resourceError,
			Status: "failure", Redacted: false,
			Time: time.Now().UTC().Format(time.RFC3339Nano),
		})
		// Measured: failed=true out_of_sync=true out_of_sync_count=1
		// change_count=0. add_event credits out_of_sync_count for every
		// non-audit event and reserves change_count for successes, so a
		// failure is out of sync without being a change.
		status.OutOfSyncCount = 1
		w.rep.AddResource(status)
		w.rep.Log("err", source, "Could not evaluate: "+applyErr.Error())
		block(w.failedDeps, w.g.Dependents(ref), ref)
		return result{blocks: map[string]struct{}{ref: {}}}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var sent int
	invalidate := false
	if evaluated {
		// Puppet's own status, recorded as it is: the harness already logged
		// each event and worded each failure (ral.Evaluator).
		runlog.AddLogs(w.ctx, w.rep, evaluation.Logs...)
		status.Events = evaluation.Events
		status.Failed = evaluation.Failed
		if evaluation.Skipped {
			// transaction.rb:425-427: a prefetch-failed resource is failed and
			// skipped, so it neither processes its own refresh queue nor
			// queues events, and it blocks its dependents.
			status.Skipped = true
			// Ruby skips it before apply, so it is never timed and writes
			// null; zero is voxagent's spelling of that, as for a native
			// prefetch failure above. The host's round trip is not its span.
			status.EvaluationTime = 0
			status.Untimed = true
			status.OutOfSyncCount = len(status.Events)
			delete(w.pendingRefresh, ref)
			w.rep.AddResource(status)
			runlog.Debugf(w.ctx, source, "Resource is being skipped, unscheduling all events")
			block(w.failedDeps, w.g.Dependents(ref), ref)
			return result{blocks: map[string]struct{}{ref: {}}}
		}
		sent = len(status.Events)
		invalidate = evaluation.InvalidateRefreshes
	} else {
		// The attributes the catalog marked Sensitive, judged once per
		// resource. The console logs MarkSensitive also returns are printed by
		// apply; they are never part of the report.
		marked, _ := ral.MarkSensitive(provider, r)
		for _, ch := range changes {
			// property.rb:260 (is_to_s/should_to_s) and
			// resource_harness.rb:182 (create_change_event) replace a Sensitive
			// property's values with "[redacted]" in the event and its message
			// and set the event's redacted flag. Only a change that
			// names a property can be marked, so the no-property resource_error
			// event stays unredacted.
			_, redact := marked[ch.Property]
			was, is := ch.Was, ch.Is
			var redacted any
			if set, ok := r.SetSensitivity[ch.Property]; ok && !set {
				redacted = false // explicitly not sensitive (design §5.4)
			}
			if redact {
				// create_change_event (resource_harness.rb:183-184) keeps a nil
				// value nil; the message text redacts either way (property.rb
				// is_to_s/should_to_s).
				if was != nil {
					was = "[redacted]"
				}
				if is != nil {
					is = "[redacted]"
				}
				redacted = true
			}
			name := ch.Name
			if name == "" {
				// Puppet's own fallback -- property.rb event_name uses the
				// property's :event option when it has one and
				// "#{name}_changed" otherwise.
				name = ch.Property + "_changed"
			}
			if ch.Err != nil {
				// A failed change's warnings and notices still come first:
				// remove_existing logs them during the sync that then fails.
				for _, warning := range ch.PathWarnings {
					w.rep.Log("warning", source, warning)
				}
				for _, notice := range ch.PathNotices {
					w.rep.Log("notice", source, notice)
				}
				for _, notice := range ch.PropertyNotices {
					w.rep.Log("notice", source+"/"+ch.Property, notice)
				}
				// resource_harness.rb:153 composes the message from the
				// transition that was TRIED. ch.Message describes a success
				// that did not happen, so it is deliberately unused here.
				//
				// ch.Err is used verbatim. Whether it carries Puppet's
				// "Could not set '<v>' on ensure: ..." wrapper is the
				// PROVIDER's decision, because only the provider knows which
				// layer raised: property.rb:490 re-raises a Puppet::Error
				// untouched and wraps only other StandardErrors at :492. See
				// ral.wrapEnsureError.
				message := "change from " + display(ch.Was, ch.WasDisplay, ch.DisplayVerbatim) +
					" to " + display(ch.Is, ch.IsDisplay, ch.DisplayVerbatim) + " failed: " + ch.Err.Error()
				if redact {
					message = "change from [redacted] to [redacted] failed: " + ch.Err.Error()
				}
				status.Events = append(status.Events, report.Event{
					Property:      ch.Property,
					PreviousValue: was,
					DesiredValue:  is,
					Redacted:      redacted,
					Message:       message,
					Name:          &name,
					// Not eventStatus(tx.Noop). resource_harness.rb:147's
					// rescue encloses safe_insync? and create_change_event,
					// which run BEFORE the noop branch at :137 -- so Ruby does
					// record a failure under --noop for a failure that never
					// touched the system. Where such a failure would be
					// spurious, Ruby suppresses it at the PROPERTY
					// (type/file/owner.rb:26's `elsif provider.resource.noop?`
					// returning false from insync?), not here.
					Status: "failure",
					Time:   now,
				})
				// event.send_log, at the resource path PLUS the property --
				// the convention the success path below already uses.
				w.rep.Log("err", source+"/"+ch.Property, message)
				// resource_harness.rb:149 Puppet.log_exception(detail) fires
				// for EVERY property failure, at source "Puppet", carrying the
				// detail alone. For a non-ensure property detail.original is
				// nil, so format_exception yields detail.to_s with no
				// "Wrapped exception:" line -- which is exactly ch.Err.
				//
				// The ensure path emits TWO Puppet-sourced logs in Ruby, and
				// this line reproduces the one property.rb:496
				// log_exception(detail, error.message) writes -- the wrapper
				// alone, because the detail there is the raw Errno and has no
				// `original`. The other is :149's on that same path, where the
				// rescued detail IS the wrapper, so format_exception walks its
				// `original` and the message embeds a newline and a "Wrapped
				// exception:" line. That chain rendering is deliberately not
				// reproduced: Go has no analogue, and fabricating the text
				// would assert nothing.
				w.rep.Log("err", "Puppet", ch.Err.Error())
				status.Failed = true
				continue
			}
			message := ch.Message
			if redact {
				message = "changed [redacted] to [redacted]"
				if w.tx.Noop {
					message = "current_value [redacted], should be [redacted] (noop)"
				}
			}
			status.Events = append(status.Events, report.Event{
				Property:      ch.Property,
				PreviousValue: was,
				DesiredValue:  is,
				Redacted:      redacted,
				Message:       message,
				Name:          &name,
				Status:        eventStatus(w.tx.Noop),
				Time:          now,
			})
			// A resource-level warning or notice comes first: Puppet logs it
			// during the sync that then records the event (type/file.rb:803,
			// :1040).
			for _, warning := range ch.PathWarnings {
				w.rep.Log("warning", source, warning)
			}
			for _, notice := range ch.PathNotices {
				w.rep.Log("notice", source, notice)
			}
			// Then the property's own lines, Exec's logged output: type/exec.rb
			// sync sends them from the returns property before the event.
			for _, notice := range ch.PropertyNotices {
				w.rep.Log("notice", source+"/"+ch.Property, notice)
			}
			w.rep.Log("notice", source+"/"+ch.Property, message)
			// A provider may also speak to the run log directly. Only notify
			// does, and its entry carries source "Puppet" because Ruby routes it
			// through the global logger rather than the resource.
			if ch.Notice != "" {
				w.rep.Log("notice", "Puppet", ch.Notice)
			}
		}
		if flushErr != nil {
			// resource_harness.rb:23-31: the flush runs after the properties
			// synced, and its failure is status.failed_because -- the same
			// property-less resource_error and "Could not evaluate" line as a
			// bare Apply error, but after the changes' events and logs rather
			// than instead of them. Failed gates the outgoing refresh below
			// and blocks the dependents at the end.
			status.Failed = true
			resourceError := "resource_error"
			status.Events = append(status.Events, report.Event{
				Message: flushErr.Err.Error(), Name: &resourceError,
				Status: "failure", Redacted: false, Time: now,
			})
			w.rep.Log("err", source, "Could not evaluate: "+flushErr.Err.Error())
		}
		sent = len(changes)
		for _, ch := range changes {
			if ch.InvalidateRefreshes {
				invalidate = true
				break
			}
		}
	}

	// A refresh fires only when the notifying resource actually changed.
	// Propagating on every run would restart every notified service on
	// every run, which is the classic way a converged fleet stops being
	// quiet.
	//
	// OUTGOING propagation is what failure gates. transaction.rb:269 wraps
	// queue_events in `unless status.failed? || status.failed_to_restart?`
	// -- evaluated inside apply, before any callback has run, so
	// FailedToRestart is necessarily still false here and Failed is the
	// whole condition.
	//
	// The gate is currently REDUNDANT and kept deliberately. A notify edge is
	// also an ordering edge, so every refresh target is a dependent, and a
	// failed resource's dependents are skipped below -- a skipped resource
	// never reaches the refresh block, so nothing observes the difference.
	// Verified by removing the clause: the whole suite stays green.
	//
	// It stays because it is Ruby's structure, it costs one condition, and it
	// becomes load-bearing the moment a refresh edge exists without a
	// matching ordering edge. TestFailedNotifiersTargetIsSkippedNotRefreshed
	// pins the invariant that makes it redundant, so if that changes the
	// suite says so rather than silently depending on this line.
	if propagate && !status.Failed && sent > 0 {
		w.logContainerPropagation(source, ref, distinctEventNames(status.Events))
		for _, target := range w.g.RefreshTargets(ref) {
			w.pendingRefresh[target] += sent
		}
	}

	// event_manager.rb:79: a self-refreshing type (mount_core's mount) also
	// queues one event per event name to itself, noop events included,
	// unless it is deleting (type.rb deleting?: ensure should is absent). It
	// sits inside the same failure gate as the outgoing events, and before the
	// invalidate_refreshes dequeue below, which empties it too. The inbound
	// block then refreshes the resource in this step, counting these beside
	// what other resources sent. The restarted event Puppet then queues to the
	// resource itself is never processed, so it is not queued here either.
	// Puppet 8.10 never passes eval_resource an ancestor (transaction.rb:98, :190),
	// so a unit's members queue to themselves and propagate does not gate this.
	if sr, ok := provider.(ral.SelfRefreshing); ok && sr.SelfRefreshes(r) && !status.Failed &&
		r.Parameters["ensure"] != "absent" {
		type eventName struct {
			set  bool
			name string
		}
		names := map[eventName]struct{}{}
		for _, ev := range status.Events {
			if ev.Name == nil {
				names[eventName{}] = struct{}{}
			} else {
				names[eventName{set: true, name: *ev.Name}] = struct{}{}
			}
		}
		if len(names) > 0 {
			w.pendingRefresh[ref] += len(names)
		}
	}

	// event_manager.rb:82: a successful change carrying
	// invalidate_refreshes empties this resource's own refresh queue before
	// that queue is processed below. Deleting the key, not zeroing it,
	// because the inbound block tests presence.
	if !status.Failed && invalidate {
		delete(w.pendingRefresh, ref)
	}

	// The INBOUND refresh is NOT gated on failure, and voxagent used to
	// short-circuit past this whole block whenever the resource had failed.
	//
	// Measured 2026-09-18 against Puppet 8.10.0, in a container because the
	// host Ruby cannot fork under Puppet: transaction.rb:288 calls
	// process_events(resource) unconditionally -- only skip? guards it, and
	// event_manager.rb's process_callback? tests noop, never failure. So a
	// resource that failed one of its own properties still has its queued
	// refresh invoked. Proven by decoupling the two commands with exec's
	// `refresh` parameter and counting invocations via timestamped side
	// effects: the apply command ran, failed, and the refresh command then
	// ran anyway. Ruby reported failed=true alongside a refresh/success
	// event with change_count=1.
	//
	// voxagent's exec implements `refresh`, so both shapes are reachable: a
	// `refresh` command runs in place of the failed one, as measured above,
	// and an exec without one re-runs its failing command, which fails again,
	// giving failed_to_restart and a second failure event.
	chained := false
	if pending, due := w.pendingRefresh[ref]; due {
		delete(w.pendingRefresh, ref)
		// event_manager.rb:73: no event is queued for a target that does not
		// respond to the callback, so there is nothing to process -- not even
		// a noop "Would have triggered" line. A native File or Notify is one,
		// as is a Tier 3 type with no refresh.
		if rf, ok := provider.(ral.Refreshing); !ok || rf.Refreshable(r) {
			var refreshed bool
			var err error
			var exceptionLogs []report.LogEntry
			// An Evaluator always logs its refresh; a native provider may
			// (Exec's output, type/exec.rb:690-698). Either way the
			// lines precede the walk's Triggered or Failed line.
			if logging, ok := provider.(ral.RefreshLogging); ok {
				var out ral.RefreshOutcome
				out, err = logging.RefreshLogged(ctx, r, w.tx.Noop)
				refreshed, exceptionLogs = out.Refreshed, out.ExceptionLogs
				for _, n := range out.PropertyNotices {
					w.rep.Log("notice", source+"/"+n.Property, n.Message)
				}
				runlog.AddLogs(w.ctx, w.rep, out.Logs...)
			} else {
				refreshed, err = provider.Refresh(ctx, r, w.tx.Noop)
			}
			if errors.Is(err, exec.ErrStagingBroken) {
				w.broken = fmt.Errorf("%s: %w", ref, err)
				return result{}
			}
			switch {
			case err != nil:
				// BOTH flags, and the dependents are blocked. Measured:
				// event_manager.rb process_callback's rescue sets
				// failed_to_restart = true and then calls fail_with_event, which
				// adds a status "failure" event, and resource/status.rb
				// add_event's `when 'failure'` sets failed = true. So the
				// failed_to_restart clause of Puppet's 0x4 is never UNIQUELY
				// reachable -- the failed clause always fires alongside it.
				//
				// transaction.rb:333 propagate_failure then records this
				// resource in each dependent's failed_dependencies on
				// `s.failed? || s.failed_to_restart?`, and skip? (:401) skips
				// them for it. Recording the flag and carrying on applied
				// resources whose prerequisite the run had just disproved.
				status.FailedToRestart = true
				status.Failed = true
				// Puppet's wording: event_manager.rb builds
				// `_("Failed to call %{callback}: %{detail}")` and both logs it
				// and uses it as the failure event's message.
				message := "Failed to call refresh: " + err.Error()
				resourceError := "resource_error"
				// No property here either. fail_with_event builds the event
				// from `{name: :resource_error, status: "failure", message:}`;
				// a callback failure describes the callback, not a property
				// that moved -- so property is null and redacted takes
				// event.rb:38's false, as on every no-property event.
				status.Events = append(status.Events, report.Event{
					Message: message, Name: &resourceError,
					Status: "failure", Redacted: false, Time: now,
				})
				w.rep.Log("err", source, message)
				// event_manager.rb:164: log_exception follows the err line. The
				// host words an Evaluator's; a native provider cannot know its
				// resource's path, so the walk writes Ruby's line for it: the
				// detail alone at the resource (7-exec-output/report.json logs
				// 26-27, 10-refresh-sensitive logs 5-6).
				if evaluated {
					runlog.AddLogs(w.ctx, w.rep, exceptionLogs...)
				} else {
					w.rep.Log("err", source, err.Error())
				}
				// Dependents are skipped by the common block below, which fires
				// on status.Failed however it was set.
			case w.tx.Noop:
				// A noop run reports what it WOULD have triggered and nothing
				// else. Measured: event_manager.rb's process_callback? is
				// `!(events.all? noop || resource.noop?)`, so under a global
				// --noop it is always false; process_callback then takes the
				// process_noop_events path, which logs "Would have triggered"
				// and queues a noop_restart event to OTHER resources. It adds
				// nothing to this resource's own status, and it returns FALSE,
				// so process_events never sets restarted.
				//
				// voxagent set restarted and added an event, which also left the
				// resource out of sync -- so it survived report pruning, and
				// every noop run over a catalog with a notify emitted a whole
				// extra resource status Ruby does not.
				w.rep.Log("notice", source, refreshMessage("Would have triggered", pending))
				// A noop run still propagates. process_noop_events queues a
				// noop_restart event through queue_events, which dispatches it
				// along the same edges, so the whole chain reports what it would
				// have done rather than just the first link.
				chained = true
			case refreshed:
				// Restarted, NOT FailedToRestart. Conflating them would report
				// every successful notify-triggered reload as a failure.
				status.Restarted = true
				message := refreshMessage("Triggered", pending)
				// name "refresh", and NO property. add_callback_status_event
				// builds the event from `{message:, status:, name: callback.to_s}`
				// -- a callback event describes the callback, not a property that
				// moved, so attributing it to a "refresh" property invented a
				// field name Puppet never emits. Property therefore stays nil,
				// which serialises as null, and redacted takes event.rb:38's
				// keyword default of false. G5 compares both: measured, Ruby's
				// refresh event is {"property":null,"redacted":false} beside a
				// returns event that is {"property":"returns","redacted":null}.
				refreshName := "refresh"
				status.Events = append(status.Events, report.Event{
					Message: message, Name: &refreshName,
					Status: "success", Redacted: false, Time: now,
				})
				w.rep.Log("notice", source, message)
				chained = true
			}

			// The chain. Measured: process_events queues a :restarted event FROM
			// a resource that was successfully refreshed, and queue_events
			// dispatches it along that resource's own matching edges. So
			// File[conf] ~> Exec[mid] ~> Exec[last] refreshes both, even though
			// Exec[mid] reported no change of its own -- being refreshed is
			// itself the event that propagates.
			//
			// Stopping at one hop meant a service whose config was rewritten
			// reloaded, while anything downstream of THAT reload never did.
			if chained && propagate {
				w.logContainerPropagation(source, ref, 1)
				for _, target := range w.g.RefreshTargets(ref) {
					w.pendingRefresh[target]++
				}
			}
		}
	}

	// resource/status.rb:122 add_event, verbatim: every non-audit event
	// credits out_of_sync_count, and only a 'success' event credits
	// change_count. A noop event's status is 'noop', so it reaches the first
	// counter and not the second -- which is what keeps
	// `--noop --detailed-exitcodes` at 0 rather than 2, measured Puppet
	// behaviour: would-be changes travel in noop_pending, not in the exit
	// bitmask.
	//
	// DERIVED from the event list rather than accumulated alongside it, and
	// after the refresh block rather than before, for two reasons. Ruby calls
	// add_event once per event, so deriving is what the Ruby actually does;
	// and an accumulator has to be maintained by every path that appends an
	// event, which the refresh-FAILURE branch did not -- it added an event
	// and credited neither counter, reporting one fewer out-of-sync than
	// Puppet. Deriving makes that class of omission unrepresentable.
	status.OutOfSyncCount = 0
	status.ChangeCount = 0
	for _, ev := range status.Events {
		status.OutOfSyncCount++
		if ev.Status == "success" {
			status.ChangeCount++
		}
	}

	if status.Failed {
		// transaction.rb:333 propagate_failure RECORDS this resource in each
		// dependent's failed_dependencies on `s.failed? ||
		// s.failed_to_restart?`; the skip itself is skip? (:401), reading
		// that list back through failed_dependencies? (:293).
		//
		// This runs AFTER the refresh block, so it covers a resource whose
		// apply failed, one whose refresh failed, and one where both did.
		block(w.failedDeps, w.g.Dependents(ref), ref)
	}

	w.rep.AddResource(status)
	if status.Failed {
		return result{blocks: map[string]struct{}{ref: {}}}
	}
	res := result{sent: sent, eventNames: distinctEventNames(status.Events)}
	if chained {
		res.sent++
		if w.tx.Noop {
			// A noop refresh adds no event to the status but still queues one.
			res.eventNames++
		}
	}
	return res
}

// withSources returns the walk's context carrying s's names, for a call into
// provider. The provider form is the resource's reference when the provider
// names no provider (notify), as Ruby's own provider-form source does.
func (w *walk) withSources(s subject, provider ral.Provider) context.Context {
	src := sources.Sources{Resource: s.source, Provider: s.display, Ref: s.display}
	if used := providerUsed(provider, s.r); used != nil {
		src.Provider = s.display + "(provider=" + *used + ")"
	}
	return sources.WithSources(w.ctx, src)
}

// logContainerPropagation writes event_manager.rb:107 for each container whose
// completed whit ref's refresh reaches: queue_events_for_resource logs one line
// per queued edge, and queue_events queues once per distinct event name, so a
// resource that sent events named differently logs the line that many times.
// source is the log source of ref: its path, or its container's name for a whit.
func (w *walk) logContainerPropagation(source, ref string, eventNames int) {
	if !runlog.From(w.ctx).Enabled("debug") {
		return
	}
	for _, target := range w.g.RefreshTargets(ref) {
		if !strings.HasPrefix(target, completedWhitPrefix) {
			continue
		}
		for range eventNames {
			runlog.Debugf(w.ctx, source, "The container %s will propagate my refresh event", w.g.Label(target))
		}
	}
}

// logUnitMember writes event_manager.rb:107 for a member of unit's generated
// set whose events went to the unit's whit rather than to its own edges (it
// was applied with propagate false): the line names the unit, at the member's
// path, once per distinct event name.
func (w *walk) logUnitMember(unit, member subject, res result) {
	if res.sent == 0 {
		return
	}
	for range res.eventNames {
		runlog.Debugf(w.ctx, member.source, "The container %s will propagate my refresh event", unit.title)
	}
}

// distinctEventNames counts the event names in events, an event with no name
// counting as one name of its own: queue_events groups events by name.
func distinctEventNames(events []report.Event) int {
	names := map[string]struct{}{}
	hasNone := false
	for _, ev := range events {
		if ev.Name == nil {
			hasNone = true
			continue
		}
		names[*ev.Name] = struct{}{}
	}
	if hasNone {
		return len(names) + 1
	}
	return len(names)
}

// visitUnit applies a resource whose provider generates more resources, and
// what it generated, as one unit at the resource's turn in the walk: Puppet's
// eval_generate (transaction.rb:111-121, additional_resource_generator.rb:56-128)
// without changing the graph. Every child is applied through the parent's own
// provider, as only file generates natively.
//
// Generation runs first, against the system as every earlier resource left
// it, and runs even when the parent is about to be skipped: pre_process runs
// for every ready resource and the skip is decided afterwards. applyUnit does
// the rest.
func (w *walk) visitUnit(gen ral.Generator, provider ral.Provider, parent subject) result {
	children := w.generate(gen, parent)
	// Bytewise title order puts every generated ancestor before its
	// descendants.
	sort.SliceStable(children, func(i, j int) bool { return children[i].Title < children[j].Title })
	base := parent.path[:len(parent.path)-1]
	return w.applyUnit(provider, parent, children, nil,
		func(subject) (ral.Provider, error) { return provider, nil },
		// Ruby's pathbuilder for a generated child is the parent's containers
		// and then the child (type/file.rb:608-626). Measured:
		// ["Stage[main]", "Main", "Node[default]", "File[/tmp/rx/tgt/a]"].
		func(c catalog.Resource) []string { return append(slices.Clone(base), c.Ref()) },
		file.FilePath(parent.r))
}

// generate runs a native Generator for s and returns its children, logging
// its warnings and, as Puppet does, a generation failure.
func (w *walk) generate(gen ral.Generator, s subject) []catalog.Resource {
	children, warnings, err := gen.Generate(w.ctx, s.r, w.managed)
	w.logWarnings(warnings)
	if err != nil {
		// :66-70: logged against the parent, the report marked failed, the
		// parent still applied.
		w.rep.Log("err", s.source,
			"Failed to generate additional resources using 'eval_generate': "+err.Error())
		w.rep.FailedToGenerate = true
		return nil
	}
	return children
}

// managed reports whether ref names a catalog resource, for a Generator. A
// generated child's ref is "File[<clean path>]", so a cleaned match never
// makes a child collide with a catalog ref: an exact match is dropped by the
// same test.
func (w *walk) managed(ref string) bool {
	if _, ok := w.byRef[ref]; ok {
		return true
	}
	typ, rest, ok := strings.Cut(ref, "[")
	if !ok || !strings.EqualFold(typ, "file") || !strings.HasSuffix(rest, "]") {
		return false
	}
	_, ok = w.filePaths[strings.TrimSuffix(rest, "]")]
	return ok
}

// visitHostUnit is visitUnit for a resource whose eval_generate runs in the
// extension host (ral.EvalGenerator). New resources are applied as the
// parent's unit, each through its own type's provider. Overrides, its edits to
// resources the catalog already held, are applied to the walk's copy of each
// before the unit, so a resource not yet visited is applied as overridden.
//
// Puppet contains a new resource in its generator's container
// (additional_resource_generator.rb:131-148), so a child's path is that
// container's and then the child.
//
// The catalog resources eval_generate returned (Existing) are in Puppet's
// made too, so its completion whit waits for them (:80-117), and each depends
// on the parent unless an edge already runs the other way
// (add_conditional_directed_dependency, :216-224). One already visited is not
// applied again, and logs Puppet's line for that edge; one every other
// predecessor of which has had its turn joins the unit as a member; the rest
// keep their own turn (concat design §5.5). A failed eval_generate made
// nothing.
func (w *walk) visitHostUnit(eg ral.EvalGenerator, provider ral.Provider, parent subject) result {
	gen := w.evalGenerate(eg, provider, parent)
	existing := gen.Existing
	if gen.Failed {
		existing = nil
	}
	var members []string
	for _, ref := range existing {
		r, ok := w.byRef[ref]
		if !ok || ral.IsContainerResource(r) {
			// An earlier unit's new resource, already applied, or a
			// container, which is not applied at all.
			continue
		}
		if _, done := w.visited[ref]; done {
			if w.g.HasEdge(ref, parent.ref) {
				name := ref
				if shown, ok := w.display[ref]; ok {
					name = shown
				}
				runlog.Debugf(w.ctx, parent.source, "Skipping automatic relationship to %s", name)
			}
			continue
		}
		ready := true
		for _, pred := range w.g.Predecessors(ref) {
			if _, done := w.visited[pred]; !done && pred != parent.ref {
				ready = false
				break
			}
		}
		if ready {
			members = append(members, ref)
		}
	}
	base := parent.path[:len(parent.path)-1]
	pathFor := func(c catalog.Resource) []string {
		if containerPath, ok := w.paths[gen.Containers[c.Ref()]]; ok {
			return append(slices.Clone(containerPath), c.Ref())
		}
		return append(slices.Clone(base), c.Ref())
	}
	providerFor := func(s subject) (ral.Provider, error) {
		p, err := w.generatedProvider(s.r, gen.Children)
		if err != nil {
			return p, err
		}
		// Pre-flight never saw a generated resource.
		if err := judge(p, s.r); err != nil {
			return p, err
		}
		if w.generatesFurther(p, s) {
			// Applying it without what it generated would leave part of
			// what Puppet manages unmanaged and report success.
			return p, fmt.Errorf("%s generates resources, and resources generated during evaluation "+
				"that generate further are not implemented", s.ref)
		}
		return p, nil
	}
	return w.applyUnit(provider, parent, ancestorsFirst(gen.Children, "/"), members, providerFor, pathFor, "/")
}

// ancestorsFirst orders an eval_generate's new resources as Puppet applies
// them: in the order eval_generate returned them, which add_resources turns
// into increasing priorities (additional_resource_generator.rb:131-135,
// graph/sequential_prioritizer.rb:26-31), except that a File waits for its
// nearest generated File ancestor below stop, which it depends on
// (connect_resources_to_ancestors, :119-128). Each step takes the earliest
// resource whose ancestor has been taken, as Puppet's ready queue takes the
// lowest priority.
func ancestorsFirst(children []catalog.Resource, stop string) []catalog.Resource {
	// Every generated File, and whether it has been taken yet.
	taken := map[string]bool{}
	for _, c := range children {
		if strings.EqualFold(c.Type, "file") {
			taken[c.Title] = false
		}
	}
	remaining := slices.Clone(children)
	out := make([]catalog.Resource, 0, len(children))
	for len(remaining) > 0 {
		// An ancestor is strictly shorter than its descendant, so the
		// shortest File waiting has no waiting ancestor: some resource is
		// always ready.
		for i, c := range remaining {
			if strings.EqualFold(c.Type, "file") {
				if done, ok := nearestGenerated(c.Title, stop, taken); ok && !done {
					continue
				}
				taken[c.Title] = true
			}
			out = append(out, c)
			remaining = slices.Delete(remaining, i, i+1)
			break
		}
	}
	return out
}

// evalGenerate runs eg's eval_generate for s, records its logs and failure,
// and applies its overrides. A Go-side error, the host dead or unable to
// serve the call, is logged as Puppet logs an eval_generate that raises
// (additional_resource_generator.rb:66-70) and yields nothing; the resource
// is still applied. The call carries s's Sources, as Apply's does, for a
// native eval_generate that logs its own failure.
func (w *walk) evalGenerate(eg ral.EvalGenerator, provider ral.Provider, s subject) ral.EvalGeneration {
	gen, err := eg.EvalGenerate(w.withSources(s, provider), s.r)
	if err != nil {
		w.rep.Log("err", s.source,
			"Failed to generate additional resources using 'eval_generate': "+err.Error())
		w.rep.FailedToGenerate = true
		return ral.EvalGeneration{}
	}
	// A raising eval_generate arrives here: Puppet's own line is in Logs.
	runlog.AddLogs(w.ctx, w.rep, gen.Logs...)
	if gen.Failed {
		w.rep.FailedToGenerate = true
	}
	for _, o := range gen.Overrides {
		w.override(o)
	}
	return gen
}

// prefetchAtTurn prefetches typ, a ral.TurnPrefetching type, the first time the
// walk traverses one of its resources, with every resource of the type in
// catalog order (transaction.rb:110-115). It does nothing for any other type
// and after the first call.
func (w *walk) prefetchAtTurn(typ string) {
	refs, pending := w.turnRefs[typ]
	if !pending {
		return
	}
	delete(w.turnRefs, typ)
	resources := make([]catalog.Resource, len(refs))
	for i, ref := range refs {
		resources[i] = w.byRef[ref]
	}
	provider, _ := w.tx.Registry.Provider(typ)
	ready, err := prefetch(w.ctx, typ, provider, resources)
	if err != nil {
		w.prefetchFailed[typ] = recordPrefetchFailure(w.rep, typ, provider, err)
		return
	}
	w.primed[typ] = ready
}

// turnRefs lists, per lowercase type, the refs of c's resources of every
// registered ral.TurnPrefetching type, in catalog order.
func (tx *Transaction) turnRefs(c *catalog.Catalog) map[string][]string {
	out := make(map[string][]string)
	for _, r := range c.Resources {
		typ := strings.ToLower(r.Type)
		if p, known := tx.Registry.Provider(typ); known {
			if _, turn := p.(ral.TurnPrefetching); turn {
				out[typ] = append(out[typ], r.Ref())
			}
		}
	}
	return out
}

// generatedProvider returns the provider c, a resource eval_generate made, is
// applied through. A type the catalog never named is prefetched here, at its
// first generated resource, with every resource of the type in unit, as
// Puppet's prefetch_if_necessary prefetches lazily (transaction.rb:243-260);
// the provider is then primed for the rest of the run. A type whose prefetch
// failed returns its prefetchFailure.
func (w *walk) generatedProvider(c catalog.Resource, unit []catalog.Resource) (ral.Provider, error) {
	typ := strings.ToLower(c.Type)
	w.prefetchAtTurn(typ)
	if p, ok := w.primed[typ]; ok {
		return p, nil
	}
	if failure, failed := w.prefetchFailed[typ]; failed {
		return nil, failure
	}
	provider, known := w.tx.Registry.Provider(typ)
	if !known {
		// Puppet would load the type and apply it (apply-spine.md D-42).
		return nil, fmt.Errorf("resource type %s is not implemented", c.Type)
	}
	var same []catalog.Resource
	for _, r := range unit {
		if strings.EqualFold(r.Type, typ) {
			same = append(same, r)
		}
	}
	ready, err := prefetch(w.ctx, typ, provider, same)
	if err != nil {
		failure := recordPrefetchFailure(w.rep, typ, provider, err)
		w.prefetchFailed[typ] = failure
		return nil, failure
	}
	w.primed[typ] = ready
	return ready, nil
}

// generatesFurther runs s's own generation, as Puppet would at its turn, and
// reports whether it made any resource. Its logs, failure and overrides are
// recorded as for any generation.
func (w *walk) generatesFurther(p ral.Provider, s subject) bool {
	if gen, ok := p.(ral.Generator); ok {
		return len(w.generate(gen, s)) > 0
	}
	if eg, ok := p.(ral.EvalGenerator); ok && eg.EvalGenerates(s.r) {
		return len(w.evalGenerate(eg, p, s).Children) > 0
	}
	return false
}

// override applies a generator's edit to an existing resource. A resource the
// walk has not visited yet is re-judged (judge), because the catalog is
// already applying and pre-flight cannot refuse it any more: a gap or
// invalidity becomes a refusal at its turn (refused). A visited resource keeps
// what it was applied with.
func (w *walk) override(o ral.Override) {
	r, ok := w.byRef[o.Ref]
	if !ok {
		// The host names only refs it knows; one the walk's catalog lacks
		// is a resource an eval_generate made. An earlier unit's has been
		// applied. A sibling in the current unit, which a nested
		// generation can override, may not have been, and its override is
		// lost (apply-spine.md D-42).
		return
	}
	params := maps.Clone(r.Parameters)
	if params == nil {
		params = make(map[string]any, len(o.Parameters))
	}
	maps.Copy(params, o.Parameters)
	r.Parameters = params
	r.MarkSensitive(o.Sensitive)
	applyContentMark(&r, o.ContentMark)
	r.OutputRedacted = r.OutputRedacted || o.RedactOutput
	w.byRef[o.Ref] = r
	if _, done := w.visited[o.Ref]; done {
		return
	}
	delete(w.refused, o.Ref)
	typ := strings.ToLower(r.Type)
	p := w.primed[typ]
	if _, pending := w.turnRefs[typ]; pending {
		// A turn-prefetching type is not primed before its first resource's
		// turn; judging needs only what the registered provider declares.
		p, _ = w.tx.Registry.Provider(typ)
	}
	if err := judge(p, r); err != nil {
		w.refused[o.Ref] = err.Error()
	}
}

// applyContentMark sets r's content mark as Ruby's parameter flag (design
// §5.4). Setting it never cascades to ensure, but removing a catalog mark
// keeps the ensure mark the catalog's own cascade already made. It never
// writes the map or slice r held: catalogs share them between copies.
func applyContentMark(r *catalog.Resource, mark ral.ContentMark) {
	if mark == ral.ContentMarkKeep {
		return
	}
	set := maps.Clone(r.SetSensitivity)
	if set == nil {
		set = make(map[string]bool, 2)
	}
	switch mark {
	case ral.ContentMarkSensitive:
		set["content"] = true
	case ral.ContentMarkNotSensitive:
		set["content"] = false
	case ral.ContentMarkUnset:
		delete(set, "content")
	}
	if mark != ral.ContentMarkSensitive && slices.Contains(r.SensitiveParameters, "content") {
		// Ruby's cascade marked ensure whether or not the catalog gave one
		// (type/file.rb:989), so the mark is set, not asked for by name.
		set["ensure"] = true
		r.SensitiveParameters = slices.DeleteFunc(slices.Clone(r.SensitiveParameters), func(n string) bool { return n == "content" })
	}
	r.SetSensitivity = set
}

// judge is pre-flight's judgement of one resource the catalog did not hold as
// it is when pre-flight ran: the first gap ResourceGaps finds, else the
// provider's Validate. A resource Puppet evaluates in the extension host
// (ral.Evaluator) was already accepted by Puppet, so it is judged only by the
// metaparameters the walk would have to honour (ral.MetaparamJudging). p may
// be nil, which is the type's gap.
func judge(p ral.Provider, r catalog.Resource) error {
	if _, evaluated := p.(ral.Evaluator); evaluated {
		if mj, ok := p.(ral.MetaparamJudging); ok {
			if gaps := mj.MetaparamGaps(r); len(gaps) > 0 {
				return errors.New(gaps[0].String())
			}
		}
		return nil
	}
	if gaps := ral.ResourceGaps(p, r); len(gaps) > 0 {
		return errors.New(gaps[0].String())
	}
	if v, ok := p.(ral.Validating); ok {
		return v.Validate(r)
	}
	return nil
}

// applyUnit applies a generating parent and its children as one unit. The
// parent is applied (or skipped) first, then each child in the order given,
// which must put every generated File ancestor before its descendants,
// through providerFor's provider and at pathFor's containment path. A child
// providerFor refuses fails with that error as its resource error, or takes
// the prefetch-failure shape for a prefetchFailure. A File child depends on
// its nearest generated File ancestor strictly below stop, else on the parent
// (connect_resources_to_ancestors, :119-128): only File defines ancestors
// (type/file.rb:539-543).
//
// Puppet moves the parent's out-edges onto a "completed" whit that waits for
// every child (:86-117). Its two observable effects are reproduced from the
// unit's aggregate instead of a vertex: a failure anywhere in the unit blocks
// the parent's dependents, naming the member that failed (measured:
// "Dependency File[/tmp/rx/tgt/a] has failures: true" on the parent's
// dependent); otherwise the parent's subscribers get exactly one event when
// any member produced one, because the whit's own refresh queues a single
// restarted event (event_manager.rb:52-83). With no children and no members
// nothing differs from an ordinary resource: Puppet builds no whit for an
// empty set (:66).
//
// members are catalog resources the unit applies after the children, each
// taken as at its own turn (take) but with the parent's failure or skip
// blocking it and its dependents, as Puppet's member depends on the parent.
// Unlike a child, a member keeps its own out-edges: its container and its own
// subscribers get its events, and the whit is sent them too.
//
// The result is the unit's, for a unit it is itself a member of: every
// failure in it, or the whit's one event.
func (w *walk) applyUnit(provider ral.Provider, parent subject, children []catalog.Resource, members []string,
	providerFor func(subject) (ral.Provider, error), pathFor func(catalog.Resource) []string, stop string) result {
	unit := len(children) > 0 || len(members) > 0
	var parentResult result
	if deps := w.failedDeps[parent.ref]; len(deps) > 0 {
		parentResult = w.skip(parent, deps)
	} else {
		parentResult = w.apply(provider, parent, !unit)
	}
	if w.broken != nil {
		return result{}
	}
	if unit {
		w.logUnitMember(parent, parent, parentResult)
	}
	// Puppet would have deferred an unsuitable member; the unit cannot
	// (apply-spine.md D-56).
	if parentResult.unsuitable != nil {
		parentResult = w.failUnsuitable(parent, parentResult.unsuitable)
	}
	if !unit {
		return parentResult
	}

	unitBlocks := map[string]struct{}{}
	for failed := range parentResult.blocks {
		unitBlocks[failed] = struct{}{}
	}
	sent := parentResult.sent
	// The results of the unit's File children, by title.
	files := make(map[string]result, len(children))
	for _, c := range children {
		ref := c.Ref()
		containment := pathFor(c)
		s := subject{ref: ref, display: ref, title: c.Title, r: c, path: containment,
			source: "/" + strings.Join(containment, "/")}
		isFile := strings.EqualFold(c.Type, "file")
		dep := parentResult
		if isFile {
			if ancestor, ok := nearestGenerated(c.Title, stop, files); ok {
				dep = ancestor
			}
		}
		var res result
		if len(dep.blocks) > 0 {
			res = w.skip(s, dep.blocks)
		} else {
			p, err := providerFor(s)
			var failure prefetchFailure
			switch {
			case errors.As(err, &failure):
				w.failPrefetched(s, failure)
				res = result{blocks: map[string]struct{}{ref: {}}}
			case err != nil:
				res = w.apply(refuse(p, c, err), s, false)
			default:
				res = w.apply(p, s, false)
			}
			if w.broken != nil {
				return result{}
			}
		}
		if res.unsuitable != nil {
			res = w.failUnsuitable(s, res.unsuitable)
		}
		if isFile {
			files[c.Title] = res
		}
		for failed := range res.blocks {
			unitBlocks[failed] = struct{}{}
		}
		w.logUnitMember(parent, s, res)
		sent += res.sent
	}

	for _, ref := range members {
		if _, done := w.visited[ref]; done {
			// A nested unit took it first.
			continue
		}
		if len(parentResult.blocks) > 0 {
			blocked := w.g.Dependents(ref)
			blocked[ref] = struct{}{}
			for failed := range parentResult.blocks {
				block(w.failedDeps, blocked, failed)
			}
		}
		s := w.subject(ref, w.byRef[ref])
		res, err := w.take(s)
		if err != nil {
			w.broken = err
		}
		if w.broken != nil {
			return result{}
		}
		if res.unsuitable != nil {
			res = w.failUnsuitable(s, res.unsuitable)
		}
		for failed := range res.blocks {
			unitBlocks[failed] = struct{}{}
		}
		w.logUnitMember(parent, s, res)
		sent += res.sent
	}

	if len(unitBlocks) > 0 {
		dependents := w.g.Dependents(parent.ref)
		for failed := range unitBlocks {
			block(w.failedDeps, dependents, failed)
		}
		return result{blocks: unitBlocks}
	}
	if sent == 0 {
		return result{}
	}
	// The unit's own whit, named by the parent's title, queues one event. Under
	// --noop its process_noop_events logs the notice instead of refreshing, at
	// the parent's title and counting every event the whit collected.
	if w.tx.Noop {
		w.rep.Log("notice", parent.title, refreshMessage("Would have triggered", sent))
	}
	w.logContainerPropagation(parent.title, parent.ref, 1)
	for _, target := range w.g.RefreshTargets(parent.ref) {
		w.pendingRefresh[target]++
	}
	return result{sent: 1, eventNames: 1}
}

// refusedProvider stands in for the provider of a resource the walk refuses,
// so the refusal takes apply's resource-error path: a failed status with one
// resource_error event carrying err, "Could not evaluate: <err>" at the
// resource's source, and its dependents blocked. It names the resource's own
// provider when it has one. Only apply ever holds it.
type refusedProvider struct {
	used *string
	err  error
}

// refuse returns the refusedProvider for r, whose own provider p may be nil.
func refuse(p ral.Provider, r catalog.Resource, err error) refusedProvider {
	if p == nil {
		return refusedProvider{err: err}
	}
	return refusedProvider{used: providerUsed(p, r), err: err}
}

func (p refusedProvider) Type() string                                              { return "" }
func (p refusedProvider) ProviderUsed() *string                                     { return p.used }
func (p refusedProvider) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (p refusedProvider) Attrs() ral.Attrs                                          { return ral.Attrs{} }
func (p refusedProvider) Apply(context.Context, catalog.Resource, bool) ([]ral.Change, []ral.Warning, error) {
	return nil, nil, p.err
}
func (p refusedProvider) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}
func (p refusedProvider) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, p.err
}

// providerUsed is the report's provider_used for r: resource/status.rb:176
// names the provider on every status, per resource where the type has
// several provider classes.
func providerUsed(p ral.Provider, r catalog.Resource) *string {
	if per, ok := p.(ral.ResourceProviderUsed); ok {
		return per.ProviderUsedFor(r)
	}
	return p.ProviderUsed()
}

// nearestGenerated returns the value generated holds for title's closest
// ancestor among the unit's generated Files, strictly below parentTitle.
// Puppet's connect_resources_to_ancestors walks res.ancestors, which only File
// defines -- the path's parents (type/file.rb:539-543) -- so only File titles
// are asked about and held: titles are paths here. Each unit applies every
// generated ancestor before its descendants (bytewise title order natively,
// ancestorsFirst for eval_generate), so applyUnit's map of applied Files
// already holds the answer.
func nearestGenerated[V any](title, parentTitle string, generated map[string]V) (V, bool) {
	stop := path.Clean(parentTitle)
	for dir := path.Dir(path.Clean(title)); dir != stop && dir != path.Dir(dir); dir = path.Dir(dir) {
		if res, ok := generated[dir]; ok {
			return res, true
		}
	}
	var none V
	return none, false
}

// logWarnings logs a provider's run-level warnings, each with its own source,
// and collapses a class-and-message pair already logged this run
// (util/warnings.rb maybe_log; see ral.Warning on why source and dedupe are
// independent). The \x00 separator is there because a class name and a
// message are both arbitrary strings, and concatenating without one makes
// two different pairs collide.
func (w *walk) logWarnings(warnings []ral.Warning) {
	for _, wn := range warnings {
		if wn.DedupeClass != "" {
			key := wn.DedupeClass + "\x00" + wn.Message
			if _, seen := w.warned[key]; seen {
				continue
			}
			w.warned[key] = struct{}{}
		}
		w.rep.Log("warning", wn.Source, wn.Message)
	}
}

// prefetchFailure is what a type-wide prefetch failure leaves for the walk: the
// two report values every resource of that type then carries. Both are derived
// once, where the provider that refused is in hand, rather than re-derived per
// resource from a provider the failure branch would otherwise have to look up
// again.
type prefetchFailure struct {
	providerUsed *string
	eventMessage string
}

// Error makes a prefetchFailure the error a generated resource's provider
// lookup returns, so applyUnit can give the resource this shape.
func (f prefetchFailure) Error() string { return f.eventMessage }

// prefetch reads the current state of every type the catalog declares, before
// any resource is applied, and returns the provider each type is to be applied
// through plus the types that could not be read.
//
// Ruby prefetches LAZILY -- transaction.rb:243-260's prefetch_if_necessary runs
// as each resource is evaluated and memoises on (type, provider class), so the
// first resource of a type pays for it and the rest read the cache. Doing it up
// front for every type present is equivalent for the report: the memo makes the
// call count the same, each call is handed the same resource set (:252 derives
// it from the whole catalog, not from the walk position), and no provider's
// Apply can change what a DIFFERENT type's prefetch would read. It is also
// simpler to reason about, because "which state does this Apply see" stops
// depending on walk order.
//
// The one thing it does not reproduce is Ruby's ordering of the failure LOG
// relative to the resources of other types, and that is not compared:
// tests/spine/allowlist.json declares logs a multiset.
//
// A type the registry does not know is left alone here. The walk already
// refuses it, with a message that names pre-flight, and moving that refusal
// earlier would change which error a malformed catalog reports.
func (tx *Transaction) prefetch(ctx context.Context, c *catalog.Catalog, rep *report.Report) (map[string]ral.Provider, map[string]prefetchFailure) {
	byType := make(map[string][]catalog.Resource)
	for _, r := range c.Resources {
		if ral.IsContainerResource(r) {
			// Containers apply nothing and have no provider, so there is
			// nothing to pre-read for them. Ruby reaches the same place from
			// the other side: resources_by_provider (:357) indexes only
			// resources whose type declares a provider parameter.
			continue
		}
		typ := strings.ToLower(r.Type)
		byType[typ] = append(byType[typ], r)
	}

	// Sorted so a catalog's resource order cannot change which provider is
	// prefetched first. Nothing observable depends on that order today -- the
	// providers are independent -- but an unordered map range would make any
	// future dependence show up as a test that fails one run in six.
	types := make([]string, 0, len(byType))
	for typ := range byType {
		types = append(types, typ)
	}
	sort.Strings(types)

	primed := make(map[string]ral.Provider, len(types))
	failed := make(map[string]prefetchFailure)
	for _, typ := range types {
		provider, known := tx.Registry.Provider(typ)
		if !known {
			continue
		}
		if _, turn := provider.(ral.TurnPrefetching); turn {
			// Prefetched at the type's first traversed resource (prefetchAtTurn).
			continue
		}
		ready, err := prefetch(ctx, typ, provider, byType[typ])
		if err == nil {
			primed[typ] = ready
			continue
		}

		failed[typ] = recordPrefetchFailure(rep, typ, provider, err)
	}
	return primed, failed
}

// prefetch runs provider's Prefetch over resources, all of type typ, and logs
// at debug what it covered and how long it took, whether or not it failed.
func prefetch(ctx context.Context, typ string, provider ral.Provider, resources []catalog.Resource) (ral.Provider, error) {
	started := time.Now()
	ready, err := provider.Prefetch(ctx, resources)
	runlog.Debugf(ctx, "voxagent", "prefetch %s: %d resources in %s", typ, len(resources),
		time.Since(started).Round(time.Millisecond))
	return ready, err
}

// recordPrefetchFailure logs a type's prefetch failure, once for the run, and
// returns what every resource of the type then reports.
func recordPrefetchFailure(rep *report.Report, typ string, provider ral.Provider, err error) prefetchFailure {
	// providerName is Ruby's provider_class.name, which is also the string
	// the report's provider_used carries -- "dnf" for package. A type whose
	// provider reports nil renders as the empty string, which is what
	// Ruby's %{name} interpolation of a nil produces; no such type can
	// reach here today, because notify is the only one and its Prefetch
	// returns itself.
	providerName := ""
	if used := provider.ProviderUsed(); used != nil {
		providerName = *used
	}
	// transaction.rb:384, via Puppet.log_exception: sourced "Puppet"
	// because it goes through the global logger rather than any resource,
	// and emitted ONCE because the rescue is inside prefetch, which runs
	// once per provider class. Measured verbatim as "Could not prefetch
	// package provider 'dnf': The yum provider can only be used as root".
	rep.Log("err", "Puppet", fmt.Sprintf("Could not prefetch %s provider '%s': %s",
		typ, providerName, err))
	return prefetchFailure{
		providerUsed: provider.ProviderUsed(),
		// transaction.rb:338, verbatim. The type name is the catalog type
		// lowercased -- Ruby's resource.type symbol -- so a Package
		// resource reads "package" here and not "Package".
		eventMessage: fmt.Sprintf("Prefetch failed for %s provider '%s'", typ, providerName),
	}
}

// block records failed on every resource in dependents: each is now blocked by
// it, and will be skipped when its turn comes.
func block(failedDeps map[string]map[string]struct{}, dependents map[string]struct{}, failed string) {
	for d := range dependents {
		if failedDeps[d] == nil {
			failedDeps[d] = make(map[string]struct{})
		}
		failedDeps[d][failed] = struct{}{}
	}
}

// logDependencySkip writes the lines Puppet logs for a resource skipped for
// failed dependencies, at the resource's own source.
//
// transaction.rb:316 notices each failed dependency not yet named this run --
// "Dependency %{dep} has failures: %{status}", where status is
// resource_status(dep).failed and so always true here -- and :422 warns
// "Skipping because of failed dependencies" for every skipped resource. Ruby
// walks failed_dependencies in insertion order; the notices are sorted here so
// two runs log them identically, and the report differential compares logs as
// a multiset. A failed resource is named by its display ref, Puppet's own.
func logDependencySkip(rep *report.Report, source string, deps, notified map[string]struct{},
	display map[string]string) {
	refs := make([]string, 0, len(deps))
	for ref := range deps {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if _, done := notified[ref]; done {
			continue
		}
		notified[ref] = struct{}{}
		if named, ok := display[ref]; ok {
			ref = named
		}
		rep.Log("notice", source, "Dependency "+ref+" has failures: true")
	}
	rep.Log("warning", source, "Skipping because of failed dependencies")
}

// eventStatus is the per-event status string. Puppet uses "noop" for an event
// that was reported but not performed, which is what keeps a noop report
// distinguishable from a real one at the event level rather than only in the
// top-level noop flag.
func eventStatus(noop bool) string {
	if noop {
		return "noop"
	}
	return "success"
}

// fmtValue renders a value the way Puppet renders one inside a change message.
//
// Measured against Puppet 8.10.0's Puppet::Parameter.format_value_for_display
// (parameter.rb:575 -- the Pops StringConverter with DEFAULT_PARAMETER_FORMAT):
//
//	"absent"        -> 'absent'      strings are single-quoted
//	1000            -> 1000          numbers are BARE
//	true            -> true          booleans are bare
//	nil             -> (empty)
//	["0"]           -> ['0']         a list keeps its brackets
//	["0", "3"]      -> ['0', '3']    ", " between elements
//	[]              -> []
//
// The quoting is load-bearing. property.rb is_to_s and should_to_s both route
// here, so this decides the text of every failure message the report carries.
func fmtValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return "'" + t + "'"
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case []string:
		// An array-matching property holds a list. exec's returns is one:
		// type/exec.rb declares it :array_matching => :all with munge(&:to_s),
		// so its desired_value is an array of strings and the failure message
		// reads "change from 'notrun' to ['0', '3'] failed: ...".
		//
		// Elements route back through fmtValue rather than being quoted here, so
		// one rule governs quoting wherever a value appears.
		parts := make([]string, len(t))
		for i, elem := range t {
			parts[i] = fmtValue(elem)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case float64:
		// encoding/json decodes every number to float64. Render 1000 as
		// "1000" and not "1000.000000" -- Ruby prints the integer it holds,
		// and a trailing ".000000" would be a divergent leaf on every
		// numeric property.
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		// Unreachable for the values the three providers produce, which are
		// strings and ints only. Kept total rather than panicking: a report
		// with an oddly-rendered value is recoverable, a crashed agent
		// mid-run is not. If this fires, the provider is the thing to fix.
		return fmt.Sprintf("%v", t)
	}
}

// display renders a value for a change message, honouring a per-property
// display override.
//
// Ruby's is_to_s and should_to_s are property METHODS, overridden by
// type/file/owner.rb and group.rb to print names. The override governs the
// message only -- create_change_event populates previous_value and
// desired_value from the raw values -- so this is a rendering concern and not
// a value one.
//
// verbatim is Change.DisplayVerbatim: the override is the whole rendering,
// even when empty.
func display(v any, override string, verbatim bool) string {
	if verbatim {
		return override
	}
	if override != "" {
		return "'" + override + "'"
	}
	return fmtValue(v)
}

// refreshMessage renders Puppet's refresh notice.
//
// Measured: event_manager.rb uses n_() to pluralize on the event count --
// "Triggered 'refresh' from 1 event" but "... from 2 events" -- and
// process_noop_events emits the same sentence with "Would have triggered".
// The message is not decoration: it is also the refresh event's own message,
// so a report consumer reads this exact string.
func refreshMessage(verb string, events int) string {
	noun := "events"
	if events == 1 {
		noun = "event"
	}
	return fmt.Sprintf("%s 'refresh' from %d %s", verb, events, noun)
}
