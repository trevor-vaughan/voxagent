package transaction

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/graph"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
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

	byRef := make(map[string]catalog.Resource, len(c.Resources))
	for _, r := range c.Resources {
		byRef[r.Ref()] = r
	}

	// Provenance the report needs for every resource it mentions, applied or
	// skipped: which containers hold it and what the catalog tagged it with.
	paths := containmentPaths(c)

	// Read the current state of every type in the catalog before applying any
	// of it. primed holds the provider each type is applied THROUGH for the rest
	// of the run, in place of the registry's; prefetchFailed holds the types
	// that could not be read at all.
	primed, prefetchFailed := tx.prefetch(ctx, c, rep)

	skipped := make(map[string]struct{})
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
	warned := make(map[string]struct{})
	// Counted, not merely flagged: Puppet's refresh notice reports how many
	// events triggered it ("from 1 event", "from 3 events"), and
	// queue_events_for_resource concatenates the notifier's events onto the
	// target's queue rather than setting a bit.
	pendingRefresh := make(map[string]int)

	for _, ref := range g.Order() {
		r, ok := byRef[ref]
		if !ok {
			return fmt.Errorf("graph names %s, which is not in the catalog", ref)
		}
		if ral.IsContainer(r.Type) {
			// Containers structure the catalog and apply nothing. Measured:
			// Puppet's report lists none of them.
			continue
		}
		// Ruby sources every log line for a resource at its full containment
		// path -- "/Stage[main]/Main/Node[default]/File[x]" -- with the property
		// appended for a property change. The leading slash and the containers
		// are part of the value consumers match on, so the bare reference is a
		// different string rather than a shorter spelling of the same one.
		source := "/" + strings.Join(paths[ref], "/")

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
		if failure, failedType := prefetchFailed[strings.ToLower(r.Type)]; failedType {
			status := report.ResourceStatus{
				Resource: ref, ResourceType: r.Type, Title: r.Title,
				File: r.File, Line: r.Line,
				ContainmentPath: paths[ref], Tags: r.Tags,
				// Named even though it could not be read. Measured: the
				// non-root report carries provider_used "dnf" on all six
				// packages -- Ruby resolves the provider before it prefetches,
				// so the report can still say which one refused.
				ProviderUsed: failure.providerUsed,
				// EvaluationTime is deliberately left at zero. Ruby writes null:
				// the resource was never evaluated, so there is no span. The
				// leaf is declared volatile in tests/spine/allowlist.json, so
				// the two spellings of "no time was spent" never meet.
				Failed: true, Skipped: true,
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
			rep.AddResource(status)
			// A WARNING, not the err that carried the cause. The cause is logged
			// once for the run, by prefetch; repeating it per resource would put
			// six err entries in a log Ruby gives exactly one, and the report
			// differential compares log content as a multiset.
			if _, dependencyFailed := skipped[ref]; dependencyFailed {
				rep.Log("notice", source, "skipped because a prerequisite failed")
			} else {
				rep.Log("warning", source, "Skipping because provider prefetch failed")
			}
			for dependent := range g.Dependents(ref) {
				skipped[dependent] = struct{}{}
			}
			continue
		}

		if _, ok := skipped[ref]; ok {
			rep.AddResource(report.ResourceStatus{
				Resource: ref, ResourceType: r.Type, Title: r.Title,
				File: r.File, Line: r.Line, Skipped: true,
				ContainmentPath: paths[ref], Tags: r.Tags,
			})
			rep.Log("notice", source, "skipped because a prerequisite failed")
			// No propagation here on purpose. g.Dependents is the TRANSITIVE
			// closure, so the failure branch below has already marked every
			// resource downstream of the failure, however deep. Re-deriving a
			// closure per skipped resource can only re-insert keys the set
			// already holds, and the cost is quadratic: measured on a chain of
			// 8000 whose head fails, 5.197s against 11ms, quadrupling per
			// doubling. It changed no result at any size.
			continue
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
		provider, ok := primed[strings.ToLower(r.Type)]
		if !ok {
			// Pre-flight should have refused this catalog. Reaching here means
			// it was skipped or is wrong, and either way the run cannot make a
			// claim about a catalog it did not understand.
			return fmt.Errorf("no provider for %s; pre-flight did not run or is incomplete", ref)
		}

		started := time.Now()
		changes, warnings, applyErr := provider.Apply(ctx, r, tx.Noop)

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
		//
		// Source and dedupe come from the warning itself and are independent of
		// each other; ral.Warning's doc comment is where the Ruby that makes
		// them so is set out. The \x00 separator is there because a class name
		// and a message are both arbitrary strings, and concatenating without
		// one makes two different pairs collide.
		for _, w := range warnings {
			if w.DedupeClass != "" {
				key := w.DedupeClass + "\x00" + w.Message
				if _, seen := warned[key]; seen {
					continue
				}
				warned[key] = struct{}{}
			}
			rep.Log("warning", w.Source, w.Message)
		}
		status := report.ResourceStatus{
			Resource: ref, ResourceType: r.Type, Title: r.Title,
			File: r.File, Line: r.Line,
			ContainmentPath: paths[ref], Tags: r.Tags,
			ProviderUsed:   provider.ProviderUsed(),
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
			// case -- refuseSymlink, a stat that is not IsNotExist, an
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
			rep.AddResource(status)
			rep.Log("err", source, "Could not evaluate: "+applyErr.Error())
			for dependent := range g.Dependents(ref) {
				skipped[dependent] = struct{}{}
			}
			continue
		}

		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, ch := range changes {
			name := ch.Name
			if name == "" {
				// Puppet's own fallback -- property.rb event_name uses the
				// property's :event option when it has one and
				// "#{name}_changed" otherwise.
				name = ch.Property + "_changed"
			}
			if ch.Err != nil {
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
				message := "change from " + display(ch.Was, ch.WasDisplay) +
					" to " + display(ch.Is, ch.IsDisplay) + " failed: " + ch.Err.Error()
				status.Events = append(status.Events, report.Event{
					Property:      ch.Property,
					PreviousValue: ch.Was,
					DesiredValue:  ch.Is,
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
				rep.Log("err", source+"/"+ch.Property, message)
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
				rep.Log("err", "Puppet", ch.Err.Error())
				status.Failed = true
				continue
			}
			status.Events = append(status.Events, report.Event{
				Property:      ch.Property,
				PreviousValue: ch.Was,
				DesiredValue:  ch.Is,
				Message:       ch.Message,
				Name:          &name,
				Status:        eventStatus(tx.Noop),
				Time:          now,
			})
			rep.Log("notice", source+"/"+ch.Property, ch.Message)
			// A provider may also speak to the run log directly. Only notify
			// does, and its entry carries source "Puppet" because Ruby routes it
			// through the global logger rather than the resource.
			if ch.Notice != "" {
				rep.Log("notice", "Puppet", ch.Notice)
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
		if !status.Failed && len(changes) > 0 {
			for _, target := range g.RefreshTargets(ref) {
				pendingRefresh[target] += len(changes)
			}
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
		// voxagent's own exec does not implement `refresh` -- it is outside the
		// bounded attribute set -- so the reachable shape here is the other one
		// Ruby produces: the refresh re-runs the same failing command and also
		// fails, giving failed_to_restart and a second failure event where
		// voxagent previously emitted one event and no flag.
		if pending, due := pendingRefresh[ref]; due {
			delete(pendingRefresh, ref)
			chained := false
			refreshed, err := provider.Refresh(ctx, r, tx.Noop)
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
				rep.Log("err", source, message)
				// Dependents are skipped by the common block below, which fires
				// on status.Failed however it was set.
			case tx.Noop:
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
				rep.Log("notice", source, refreshMessage("Would have triggered", pending))
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
				rep.Log("notice", source, message)
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
			if chained {
				for _, target := range g.RefreshTargets(ref) {
					pendingRefresh[target]++
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
			for dependent := range g.Dependents(ref) {
				skipped[dependent] = struct{}{}
			}
		}

		rep.AddResource(status)
	}
	return nil
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
		if ral.IsContainer(r.Type) {
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
		ready, err := provider.Prefetch(ctx, byType[typ])
		if err == nil {
			primed[typ] = ready
			continue
		}

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
		failed[typ] = prefetchFailure{
			providerUsed: provider.ProviderUsed(),
			// transaction.rb:338, verbatim. The type name is the catalog type
			// lowercased -- Ruby's resource.type symbol -- so a Package
			// resource reads "package" here and not "Package".
			eventMessage: fmt.Sprintf("Prefetch failed for %s provider '%s'", typ, providerName),
		}
	}
	return primed, failed
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
func display(v any, override string) string {
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
