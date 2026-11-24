// Package ral is the resource abstraction layer: the provider registry, the
// capability sets that drive the pre-flight check, and the Tier 1 providers.
//
// It exposes exactly one interface upward. The transaction cannot tell how a
// resource was applied, and today every answer comes from a Go-native provider
// -- but keeping that boundary now is what makes each later bridge to a
// Ruby-implemented type a swap rather than a rewrite of the engine.
package ral

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Attrs is the set of attribute names a provider implements.
type Attrs map[string]struct{}

// NewAttrs builds an attribute set.
func NewAttrs(names ...string) Attrs {
	a := make(Attrs, len(names))
	for _, n := range names {
		a[n] = struct{}{}
	}
	return a
}

// Sorted returns the attribute names in a stable order, for messages.
func (a Attrs) Sorted() []string {
	out := make([]string, 0, len(a))
	for n := range a {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Change is one property that moved.
type Change struct {
	Property string
	Was      any
	Is       any
	Message  string

	// Name is the report event's name, e.g. file_created. Leave it empty and
	// the transaction derives "<property>_changed", which is Puppet's own
	// fallback: property.rb event_name returns the property's :event option
	// when it declares one and "#{name}_changed" otherwise.
	//
	// Across file, exec and notify only ensure declares per-value events, so
	// only an ensure change sets this today. That is a fact about the three
	// implemented types, NOT about Puppet: any property may declare them.
	// type/service.rb:68-92 puts five on enable -- :service_enabled (:68),
	// :service_disabled (:72, and again at :82 for mask), :service_manual_start
	// (:76) and :service_delayed_start (:90). Name is per-Change, so service
	// needs no interface change; it needs an implementer who does not read the
	// current usage as the rule.
	Name string

	// Notice is an extra line for the run log, carrying source "Puppet" rather
	// than the resource's path.
	//
	// Only notify uses it, and it is the whole point of the type: type/notify.rb
	// calls Puppet.send(loglevel, message) on the global logger, so a Ruby run
	// logs the message twice -- once as the property change at the resource
	// path, once as bare text sourced "Puppet".
	Notice string

	// PathNotices are extra notices for the run log sourced at the resource's
	// own path, with no property suffix: resource-level notices, as
	// type/file.rb:1040's "Not removing directory; use 'force' to override".
	// They are logged in order before the change's own line.
	//
	// There can be more than one because Puppet can log the same notice twice:
	// remove_existing runs twice on one sync (ensure.rb:186, target.rb:36-61),
	// which is I-3.
	PathNotices []string

	// PathWarnings are warnings for the run log sourced at the resource's own
	// path, logged before PathNotices: type/file.rb:803's "Could not back up
	// file of type <ftype>", which remove_existing raises each time it runs
	// (twice on one sync, I-3) when backup is set and the type cannot be
	// backed up.
	PathWarnings []string

	// PropertyNotices are notices for the run log sourced at the resource's
	// path plus the property, <source>/<Property>, logged after PathNotices
	// and before the change's own line, on the success and the failure path:
	// Exec's logged output, which type/exec.rb sync sends from the returns
	// property before it records the event or fails.
	PropertyNotices []string

	// Err, when non-nil, means this property was attempted and did not
	// succeed. Was and Is then describe the transition that was TRIED, not
	// one that happened, and Message is unused -- the transaction builds
	// Puppet's failure wording from Was, Is and Err.
	//
	// Measured: resource_harness.rb:123 sync_if_needed wraps each property's
	// sync, and its rescue at :147 -- commented "Execution will continue on
	// StandardErrors" at :148 -- records a failure event for THAT property
	// and lets the next property proceed. A provider must therefore keep
	// going after one property fails rather than returning early.
	Err error
	// InvalidateRefreshes drops the resource's own pending refresh when this
	// change succeeds: Puppet's property option of the same name
	// (property.rb:259), used by service's ensure => running
	// (type/service.rb:109) so that a service started this run is not then
	// restarted by its notifiers (event_manager.rb:82). Engine-internal: no
	// report field carries it.
	InvalidateRefreshes bool

	// WasDisplay and IsDisplay override how Was and Is are rendered INTO A
	// MESSAGE, without changing the values the event carries.
	//
	// This is Ruby's is_to_s/should_to_s, which type/file/owner.rb and
	// group.rb each override to print names -- while previous_value stays the
	// integer uid. Measured: one event with previous_value 1000 and the
	// message "change from 'llm' to 'nosuchuser' failed: ...".
	//
	// Empty means "render Was/Is with fmtValue", which is every property
	// except owner and group.
	WasDisplay string
	IsDisplay  string
}

// posix is the provider name Puppet resolves file and exec to on Linux. A
// package-level variable because ProviderUsed returns a pointer -- Ruby
// distinguishes a named provider from null, and the address of a shared
// constant string is the cheapest way to say "named".
var posix = "posix"

// Warning is one line for the run log that belongs to no property.
//
// Source and dedupe are independent in Ruby and so are independent here.
// util/logging.rb:298-302's log_source returns the resource path only for a
// Puppet::Type or Puppet::Parameter; a provider instance falls through to its
// own to_s (provider.rb:559, "Package[foo](provider=dnf)") and a provider
// class to the class name. Separately, only maybe_log dedupes
// (util/warnings.rb:26-31) -- shared by warnonce, notice_once and debug_once
// alike -- and it keys on the calling class AND the message (:28-29). Plain
// Puppet.warning does not dedupe at all.
type Warning struct {
	// Source is the report log entry's source field.
	Source string

	Message string

	// DedupeClass empty means the warning is logged every time it is raised.
	// Non-empty is maybe_log's @stampwarnings: a message already seen from the
	// same class is suppressed. The key is the pair, never the message alone.
	DedupeClass string
}

// Provider applies one resource type.
type Provider interface {
	// Type is the catalog type name in lowercase, e.g. "file".
	Type() string

	// ProviderUsed is the name the report's provider_used field carries, or
	// nil for a type that has no providers.
	//
	// Measured against Puppet 8.10.0: file and exec resolve to "posix" on
	// Linux, and notify reports null because it is implemented on the type
	// itself and has no provider to name. The distinction is nil versus the
	// empty string -- Ruby writes null, and "" would be a different value.
	ProviderUsed() *string

	// AutoRequires returns the references this resource must be applied AFTER,
	// beyond anything the catalog states.
	//
	// Puppet calls these autorelations and every type declares its own: a file
	// autorequires its nearest managed ancestor directory, an exec autorequires
	// the managed files its command names. They carry no refresh -- type.rb sets
	// `edge.event = :NONE` for an autorequire.
	//
	// managed reports whether a reference is in the catalog. Autorequire is
	// SOFT: type.rb does `dep = rel_catalog.resource(type, dep); next unless
	// dep`, so naming a resource the catalog does not manage is silently no edge
	// rather than an error. That is the opposite of an explicit relationship,
	// which is could-not-run when its target is absent.
	//
	// Without this, the most ordinary Puppet pattern there is -- a directory and
	// the files inside it -- fails, because nothing in the catalog orders them.
	AutoRequires(r catalog.Resource, managed func(ref string) bool) []string

	// Attrs is the set of attributes this provider implements.
	//
	// This value drives both the pre-flight refusal and the "what to port
	// next" histogram. One source, so the two cannot disagree -- and the
	// histogram is then measured rather than asserted.
	//
	// Callers MUST NOT mutate the returned set. Attrs is a map, so it is
	// returned by reference, and the providers here happen to build a fresh one
	// per call -- but a provider that cached it in a field would hand out a
	// live handle to the data pre-flight uses to decide what may be applied.
	// Corrupting it would not fail loudly; it would make pre-flight accept an
	// attribute nothing implements, which is a silent partial application.
	Attrs() Attrs

	// Apply brings the resource to its declared state, returning one Change
	// per property that moved, and one carrying Err per property that was
	// attempted and failed. Under noop it reports what it would have
	// changed and changes nothing.
	//
	// The error return is narrower than it looks: it means the resource
	// could not be applied and NO property can be blamed -- Ruby's
	// status.failed_because (resource/status.rb:140), which synthesizes a
	// resource_error event. A failure that can name a property belongs in a
	// Change.Err instead. It is a result about the resource, not about the
	// run: the transaction records it, skips the resource's dependents, and
	// carries on. Returning a non-nil error DISCARDS the accompanying change
	// list, so a provider must not rely on reporting both.
	//
	// warnings are run-level conditions that belong in the run log rather than
	// on any resource. The transaction logs each at level "warning" with the
	// warning's own Source, and dedupes it across the run only when it names a
	// DedupeClass -- the key being that class AND the message together, so a
	// condition every file resource hits is reported once, while an empty class
	// is logged every time it is raised. See Warning for why the two are
	// independent. A provider therefore returns the warning every time it
	// applies and does not try to remember whether it already has -- it is
	// stateless and could not.
	Apply(ctx context.Context, r catalog.Resource, noop bool) (changes []Change, warnings []Warning, err error)

	// Prefetch reads the current state of every resource of this type in ONE
	// pass, before any of them is applied, and returns the provider that should
	// apply them.
	//
	// Returning a primed provider rather than a state map is what keeps Apply's
	// signature fixed and providers immutable during the walk: the transaction
	// swaps the returned value in for the run and every Apply then reads from a
	// value that cannot change under it.
	//
	// Puppet calls this once per (type, provider class) --
	// transaction.rb:243-260 delegates at :259 to Transaction#prefetch
	// (:374-389), which invokes provider_class.prefetch at :381. dnf inherits
	// provider/package/yum.rb:72, which RAISES unless root.
	//
	// An error is TYPE-WIDE, never per-resource: every resource of this type
	// gets a resource_error and none is applied (transaction.rb:333-339, and
	// the "Skipping because provider prefetch failed" warning at :427).
	// Measured: six package resources, six resource_error events, six warnings.
	// A provider with nothing to pre-read returns itself and a nil error.
	Prefetch(ctx context.Context, rs []catalog.Resource) (Provider, error)

	// Refresh responds to a refresh event from a notifying resource. A
	// provider with nothing to refresh returns (false, nil).
	//
	// An error here sets failed_to_restart AND failed, and blocks the
	// resource's dependents. Measured: event_manager.rb process_callback's
	// rescue sets failed_to_restart and then calls fail_with_event, which adds
	// a status "failure" event that resource/status.rb turns into failed =
	// true. The failed_to_restart clause of Puppet's 0x4 is therefore never
	// uniquely reachable -- an earlier comment here claimed it was.
	Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error)
}

// Registry maps a catalog type name to its provider.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry builds a registry. Two providers claiming one type is a
// programming error caught at startup, not a runtime condition.
func NewRegistry(ps ...Provider) *Registry {
	reg := &Registry{providers: make(map[string]Provider, len(ps))}
	for _, p := range ps {
		key := strings.ToLower(p.Type())
		if _, dup := reg.providers[key]; dup {
			panic(fmt.Sprintf("ral: two providers registered for type %q", key))
		}
		reg.providers[key] = p
	}
	return reg
}

// Provider looks up a provider. Catalogs spell types in title case and
// providers in lowercase, so the lookup normalises.
func (reg *Registry) Provider(typ string) (Provider, bool) {
	p, ok := reg.providers[strings.ToLower(typ)]
	return p, ok
}

// Types returns every registered type name, sorted.
func (reg *Registry) Types() []string {
	out := make([]string, 0, len(reg.providers))
	for t := range reg.providers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// containerTypes are the compiler-internal types that structure a catalog but
// apply nothing.
//
// Stage, Class and Node are measured, not assumed: a catalog compiled from a
// one-line manifest already contains Stage[main], Class[Settings], Class[main],
// and Node[default]. A pre-flight check that did not know about these would
// refuse every catalog ever compiled, so this set is load-bearing rather than
// defensive.
//
// whit and component are Puppet's own container types. Compiled catalogs do
// not normally carry them: a defined type's instance keeps its own type name
// (Foo::Bar[x]) and is recognised by its kind instead -- see
// IsContainerResource. They are listed on the same reasoning as the measured
// three rather than the same evidence.
var containerTypes = map[string]struct{}{
	"stage": {}, "class": {}, "node": {}, "whit": {}, "component": {},
}

// IsContainer reports whether a type structures the catalog rather than
// describing state on the node.
func IsContainer(typ string) bool {
	_, ok := containerTypes[strings.ToLower(typ)]
	return ok
}

// compilableType is the kind Puppet gives a resource backed by a real type
// (resource.rb:40).
const compilableType = "compilable_type"

// IsContainerResource reports whether a catalog resource structures the
// catalog rather than describing state on the node.
//
// Puppet's to_ral (resource.rb:515-526) makes every catalog_format 2 resource
// whose kind is not compilable_type an inert Component: classes and nodes
// ("unknown" or "class") and defined-type instances ("defined_type"). Stage
// is compilable_type but still a container (relationship_graph.rb:240), so the
// type list applies as well.
//
// A resource with no kind at all falls back to the type list. Puppet would
// make it a Component; voxagent applies it. Puppet 8 emits kind on every
// resource it compiles, so only hand-written catalogs -- test fixtures -- reach
// this branch. It is recorded in the divergence register.
func IsContainerResource(r catalog.Resource) bool {
	if IsContainer(r.Type) {
		return true
	}
	return r.Kind != "" && r.Kind != compilableType
}

// engineAttrs are the metaparameters the engine handles rather than a provider.
//
// The four relationship metaparameters become graph edges; tag is recorded in
// the report. Everything else -- schedule, audit, alias, loglevel, per-resource
// noop -- is deliberately absent, so it becomes a pre-flight gap rather than
// being silently ignored. Silently ignoring `schedule` would mean applying a
// resource the catalog said not to apply yet.
var engineAttrs = map[string]struct{}{
	"before": {}, "require": {}, "notify": {}, "subscribe": {}, "tag": {},
}

// IsEngineAttr reports whether an attribute is the engine's responsibility.
func IsEngineAttr(name string) bool {
	_, ok := engineAttrs[strings.ToLower(name)]
	return ok
}
