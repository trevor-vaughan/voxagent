package transaction

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
)

// recorder is a provider that records the order it was called in and can be
// told to fail. It exists so the transaction's behaviour can be tested without
// touching the filesystem.
type recorder struct {
	typ        string
	applied    *[]string
	refreshed  *[]string
	failOn     map[string]bool
	refreshErr map[string]bool
	changes    int

	// noChange names resources whose Apply reports nothing, overriding `changes`.
	// Without it every resource in a fixture changes, which makes "this resource
	// contributed no events of its own" impossible to set up -- and that is
	// exactly the condition needed to count the events a REFRESH contributes.
	noChange map[string]bool

	// failChange names resources whose Apply returns changes carrying Err --
	// properties that were attempted and failed. Distinct from failOn, which
	// returns a bare error meaning "no property can be blamed". The two are
	// different report shapes and the fake must produce both.
	//
	// The value selects which shape: "ensure" for a failure whose error the
	// PROVIDER already wrapped the way ral.wrapEnsureError does, "mixed" for
	// one success plus one failure carrying the raising layer's error as-is.
	failChange map[string]string

	// warn is the run-level warning list Apply returns for EVERY resource,
	// which is the real contract: a provider is a stateless value and cannot
	// know whether it has already reported a condition, so it reports it every
	// time and the transaction dedupes.
	warn []ral.Warning

	// providerName, when non-empty, is what ProviderUsed reports; empty reports
	// nil, which is the notify shape.
	//
	// It exists because the prefetch-failure path carries the provider's name
	// into the report status AND into the event message, and a fixture whose
	// providers all report nil could not tell a name that was carried from one
	// that was dropped.
	providerName string

	// prefetchErr, when non-nil, is what Prefetch returns instead of a primed
	// provider. Set per PROVIDER rather than per resource, because a prefetch
	// failure is type-wide -- that is the shape under test, not an incidental
	// convenience.
	prefetchErr error

	// prefetched records one line per Prefetch call: the type, the resources it
	// was handed, and how many resources had already been applied when it ran.
	//
	// The apply count is what pins the ORDERING. Prefetch must run before the
	// walk, and an implementation that prefetched lazily -- or after the walk --
	// would still record the call, so recording only the call would not tell the
	// two apart.
	prefetched *[]string

	// primed marks the copy Prefetch returned. Prefetch takes a value receiver,
	// sets this, and returns the copy, which is exactly how the real providers
	// hand back a primed value; primedSeen then reports whether the transaction
	// applied through that copy or through the registry's original.
	primed     bool
	primedSeen *[]string

	// noopSeen records the noop flag each call was handed, when the test cares.
	// Optional because seventeen call sites of newTx do not, and threading a
	// probe through all of them to serve one test would be worse than a nil
	// check here.
	//
	// It exists because these fakes previously DISCARDED the flag -- both
	// signatures took `_ bool`. That made it impossible for any test in this
	// package to observe whether tx.Noop reached a provider at all, and a
	// mutant that hard-coded false in the Refresh call survived the entire
	// suite while executing commands under --noop.
	noopSeen *[]string
}

func (p recorder) recordNoop(call, ref string, noop bool) {
	if p.noopSeen == nil {
		return
	}
	*p.noopSeen = append(*p.noopSeen, fmt.Sprintf("%s %s noop=%t", call, ref, noop))
}

func (p recorder) Type() string { return p.typ }
func (p recorder) ProviderUsed() *string {
	if p.providerName == "" {
		return nil
	}
	return &p.providerName
}

func (p recorder) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (p recorder) Attrs() ral.Attrs {
	return ral.NewAttrs("message", "command", "ensure", "content")
}

func (p recorder) Apply(_ context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	*p.applied = append(*p.applied, r.Ref())
	p.recordNoop("Apply", r.Ref(), noop)
	if p.primedSeen != nil {
		*p.primedSeen = append(*p.primedSeen, fmt.Sprintf("%s primed=%t", r.Ref(), p.primed))
	}
	if p.failOn[r.Ref()] {
		return nil, p.warn, errors.New("provider refused")
	}
	if p.noChange[r.Ref()] {
		return nil, p.warn, nil
	}
	switch p.failChange[r.Ref()] {
	case "ensure":
		// Already wrapped, because property.rb:483 set wraps inside the
		// property -- below the transaction. A provider that raised a
		// Puppet::Error equivalent would hand over an unwrapped error instead,
		// and the transaction must not be able to tell the difference.
		return []ral.Change{{
			Property: "ensure", Was: "absent", Is: "file",
			Name: "file_created", Message: "created",
			Err: errors.New("Could not set 'file' on ensure: permission denied" +
				" (file: /etc/site.pp, line: 6)"),
		}}, p.warn, nil
	case "returns":
		// exec's shape: an array-matching property, so Is is a LIST rather than
		// a scalar, and the event's name comes from the property's own :event
		// option rather than the "<property>_changed" fallback.
		return []ral.Change{{
			Property: "returns", Was: "notrun", Is: []string{"0"},
			Name: "executed_command",
			Err:  errors.New("'/bin/false' returned 1 instead of one of [0]"),
		}}, p.warn, nil
	case "mixed":
		// One property that moved and one that was attempted and failed --
		// the shape that distinguishes add_event's two counters. A
		// single-event fixture cannot tell out_of_sync_count from
		// change_count, because both would be 1.
		//
		// owner carries display overrides because its event fields hold raw
		// uids while its message renders names.
		return []ral.Change{
			{Property: "mode", Was: "0644", Is: "0600", Message: "mode changed '0644' to '0600'"},
			{Property: "owner", Was: 1000, Is: 0,
				WasDisplay: "llm", IsDisplay: "root",
				Err: errors.New("operation not permitted")},
		}, p.warn, nil
	}
	out := make([]ral.Change, p.changes)
	for i := range out {
		out[i] = ral.Change{Property: "p", Message: "changed"}
	}
	return out, p.warn, nil
}

func (p recorder) Refresh(_ context.Context, r catalog.Resource, noop bool) (bool, error) {
	*p.refreshed = append(*p.refreshed, r.Ref())
	p.recordNoop("Refresh", r.Ref(), noop)
	if p.refreshErr[r.Ref()] {
		return false, errors.New("refresh refused")
	}
	return true, nil
}

func (p recorder) Prefetch(_ context.Context, rs []catalog.Resource) (ral.Provider, error) {
	if p.prefetched != nil {
		refs := make([]string, len(rs))
		for i, r := range rs {
			refs[i] = r.Ref()
		}
		*p.prefetched = append(*p.prefetched,
			fmt.Sprintf("%s [%s] after %d applies", p.typ, strings.Join(refs, " "), len(*p.applied)))
	}
	if p.prefetchErr != nil {
		return nil, p.prefetchErr
	}
	p.primed = true
	return p, nil
}

func newTx(applied, refreshed *[]string, failOn, refreshErr map[string]bool, changes int) *Transaction {
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: applied, refreshed: refreshed,
			failOn: failOn, refreshErr: refreshErr, changes: changes}
	}
	return &Transaction{
		Registry: ral.NewRegistry(mk("file"), mk("exec"), mk("notify")),
	}
}

// txWithFailChange builds a transaction whose providers report attempted-and-
// failed properties for the named resources. newTx's signature is already five
// parameters wide; a sixth would have to be threaded through every call site
// that does not care.
func txWithFailChange(applied, refreshed *[]string, failChange map[string]string) *Transaction {
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: applied, refreshed: refreshed,
			failChange: failChange, changes: 1}
	}
	return &Transaction{Registry: ral.NewRegistry(mk("file"), mk("exec"), mk("notify"))}
}

// ownerWarning is the file provider's real root-check warning, the only one in
// existence: type/file/owner.rb:36's warnonce, which is maybe_log keyed on the
// owner property and the message, logged by Puppet.warning and so sourced
// "Puppet". The tests below that assert a dedupe need a fixture that asks for
// one, and taking the values from the production site is what keeps them
// asserting about a warning Ruby actually raises.
var ownerWarning = ral.Warning{
	Source:      "Puppet",
	Message:     "Cannot manage ownership unless running as root",
	DedupeClass: "Puppet::Type::File::Owner",
}

// txWithWarnings builds a transaction whose providers return warn from every
// Apply, whatever the resource. That repetition is the point: the real file
// provider returns its root-check warning on every resource that trips it, and
// the dedupe under test is the transaction's.
func txWithWarnings(applied, refreshed *[]string, warn []ral.Warning) *Transaction {
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: applied, refreshed: refreshed,
			warn: warn, changes: 1}
	}
	return &Transaction{Registry: ral.NewRegistry(mk("file"), mk("exec"), mk("notify"))}
}

// prefetchProbe is what txWithPrefetch threads into its providers: the two
// recording slices the prefetch tests read back, and the per-type failures they
// set up. Grouped because every prefetch test needs the same three, and a
// fourth positional *[]string on a builder that already takes two would be
// unreadable at the call site.
type prefetchProbe struct {
	prefetched *[]string
	primedSeen *[]string

	// prefetchErr is keyed on the TYPE, lowercase, because a prefetch failure is
	// type-wide. Keying it on a resource reference would let a test express a
	// per-resource prefetch failure, which is a shape Puppet cannot produce.
	prefetchErr map[string]error
}

// txWithPrefetch builds a transaction whose providers record every Prefetch and
// can be told to fail it per type.
//
// The provider names mirror the real registry -- file and exec resolve to
// "posix", notify reports nil -- so a test that asserts the failure path
// carries a name is asserting against a value Puppet actually emits.
func txWithPrefetch(applied, refreshed *[]string, probe prefetchProbe) *Transaction {
	mk := func(typ, providerName string) recorder {
		return recorder{typ: typ, applied: applied, refreshed: refreshed,
			providerName: providerName,
			prefetched:   probe.prefetched, primedSeen: probe.primedSeen,
			prefetchErr: probe.prefetchErr[typ], changes: 1}
	}
	return &Transaction{Registry: ral.NewRegistry(
		mk("file", "posix"), mk("exec", "posix"), mk("notify", ""))}
}

func runCatalog(t *testing.T, tx *Transaction, c *catalog.Catalog) *report.Report {
	t.Helper()
	g, err := BuildGraph(c, tx.Registry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	rep := report.New("n", "production", tx.Noop)
	if err := tx.Run(context.Background(), c, g, rep); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	rep.Finalize()
	return rep
}

// The walk reports how long it took, which Finalize emits as the report's
// transaction_evaluation timing.
//
// The report cannot measure this itself -- it does not know where the walk
// begins or ends -- so if the transaction does not set it, the metric is
// silently zero on every run forever. A zero is indistinguishable from a fast
// walk, which is what makes it worth pinning rather than eyeballing.
func TestRunRecordsItsOwnEvaluationSpan(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "a", nil), res("Notify", "b", nil),
	}})
	if rep.EvaluationSpan <= 0 {
		t.Errorf("EvaluationSpan = %v, want a positive duration", rep.EvaluationSpan)
	}
}

// Every resource status carries the catalog's tags and the chain of containers
// holding it. Both are compared fields in the report differential.
//
// Asserted on a SKIPPED resource as well as an applied one: the skipped branch
// builds its status separately, so a fix applied to only the applying path
// would leave every skipped resource with an empty provenance and nothing would
// notice until the differential ran with a failure in the fixture.
func TestResourceStatusCarriesCatalogProvenance(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"Notify[head]": true}, nil, 1)
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("Class", "profile", nil),
			// tags is a top-level catalog field, not a parameter. Building the
			// fixture from Parameters would pass against an implementation that
			// read the wrong source.
			{Type: "Notify", Title: "head", Tags: []string{"notify", "head"}},
			{Type: "Notify", Title: "tail", Tags: []string{"notify", "tail"},
				Parameters: map[string]any{"require": []any{"Notify[head]"}}},
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[profile]"},
			{Source: "Class[profile]", Target: "Notify[head]"},
			{Source: "Class[profile]", Target: "Notify[tail]"},
		},
	}
	rep := runCatalog(t, tx, c)

	for _, ref := range []string{"Notify[head]", "Notify[tail]"} {
		rs, ok := rep.ResourceStatuses[ref]
		if !ok {
			t.Fatalf("%s is not in the report", ref)
		}
		wantPath := []string{"Stage[main]", "Profile", ref}
		if !slices.Equal(rs.ContainmentPath, wantPath) {
			t.Errorf("%s containment_path = %v, want %v", ref, rs.ContainmentPath, wantPath)
		}
		wantTags := []string{"notify", strings.TrimSuffix(strings.TrimPrefix(ref, "Notify["), "]")}
		if !slices.Equal(rs.Tags, wantTags) {
			t.Errorf("%s tags = %v, want %v", ref, rs.Tags, wantTags)
		}
	}
	if !rep.ResourceStatuses["Notify[tail]"].Skipped {
		t.Error("Notify[tail] should have been skipped; the fixture proves nothing otherwise")
	}
}

// Containers structure the catalog and apply nothing, and -- measured against
// Puppet 8.10.0 -- they appear in no report. A transaction that applied them
// would look for a provider that does not exist.
func TestRunSkipsContainerTypes(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Stage", "main", nil), res("Class", "main", nil),
		res("Node", "default", nil), res("Notify", "hi", nil),
	}}
	rep := runCatalog(t, tx, c)

	if len(applied) != 1 || applied[0] != "Notify[hi]" {
		t.Errorf("applied = %v, want [Notify[hi]]", applied)
	}
	for _, ref := range []string{"Stage[main]", "Class[main]", "Node[default]"} {
		if _, ok := rep.ResourceStatuses[ref]; ok {
			t.Errorf("report contains a status for the container %s", ref)
		}
	}
}

func TestRunAppliesInGraphOrder(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 0)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "c", map[string]any{"require": []any{"File[b]"}}),
		res("File", "b", map[string]any{"require": []any{"File[a]"}}),
		res("File", "a", nil),
	}}
	runCatalog(t, tx, c)
	want := []string{"File[a]", "File[b]", "File[c]"}
	// Length first. Indexing applied[i] against a walk that applied fewer
	// resources panics, and a panic aborts the whole test binary -- so a
	// regression here would destroy the verdict of every test after it in the
	// package rather than failing one. A no-result reads like a pass.
	if len(applied) != len(want) {
		t.Fatalf("applied = %v, want %v", applied, want)
	}
	for i := range want {
		if applied[i] != want[i] {
			t.Fatalf("applied = %v, want %v", applied, want)
		}
	}
}

// A refresh fires only when the notifying resource actually changed.
// Propagating on every run would restart every notified service on every run,
// which is the classic way a converged fleet stops being quiet.
func TestRefreshFiresOnlyOnChange(t *testing.T) {
	for _, tt := range []struct {
		name        string
		changes     int
		wantRefresh int
	}{
		{"changed notifies", 1, 1},
		{"in sync does not notify", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var applied, refreshed []string
			tx := newTx(&applied, &refreshed, nil, nil, tt.changes)
			c := &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
				res("Exec", "reload", nil),
			}}
			runCatalog(t, tx, c)
			if len(refreshed) != tt.wantRefresh {
				t.Errorf("refreshed = %v, want %d entries", refreshed, tt.wantRefresh)
			}
		})
	}
}

// A successful refresh sets Restarted, NOT FailedToRestart. Conflating them
// would report every successful notify-triggered reload as a failure and set
// the 0x4 exit bit on a healthy run.
func TestSuccessfulRefreshSetsRestarted(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
		res("Exec", "reload", nil),
	}}
	rep := runCatalog(t, tx, c)

	rs, ok := rep.ResourceStatuses["Exec[reload]"]
	if !ok {
		t.Fatal("report has no status for Exec[reload]")
	}
	if !rs.Restarted {
		t.Error("Restarted = false, want true after a successful refresh")
	}
	if rs.FailedToRestart {
		t.Error("FailedToRestart = true after a SUCCESSFUL refresh")
	}
	if s := rep.RunStatus(); s.FailedRestarts != 0 {
		t.Errorf("RunStatus().FailedRestarts = %d, want 0", s.FailedRestarts)
	}
	if got := rep.RunStatus().Exit(true); got&4 != 0 {
		t.Errorf("Exit(true) = %d, want no 0x4 bit on a healthy run", got)
	}
}

// A failed refresh sets FailedToRestart. That it ALSO sets Failed, emits a
// failure event, and skips dependents is pinned separately by
// TestFailedRefreshFailsTheResourceAndSkipsDependents.
func TestFailedRefreshSetsFailedToRestart(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, map[string]bool{"Exec[reload]": true}, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
		res("Exec", "reload", nil),
	}}
	rep := runCatalog(t, tx, c)

	rs, ok := rep.ResourceStatuses["Exec[reload]"]
	if !ok {
		t.Fatal("report has no status for Exec[reload]")
	}
	if !rs.FailedToRestart {
		t.Error("FailedToRestart = false, want true")
	}
	if rs.Restarted {
		t.Error("Restarted = true after a FAILED refresh")
	}
	if s := rep.RunStatus(); s.FailedRestarts != 1 {
		t.Errorf("RunStatus().FailedRestarts = %d, want 1", s.FailedRestarts)
	}
	if got := rep.RunStatus().Exit(true); got&4 == 0 {
		t.Errorf("Exit(true) = %d, want the 0x4 bit set", got)
	}
}

// Puppet's semantics: a resource whose prerequisite failed has not been
// evaluated. Applying it anyway would act on an assumption the run just
// disproved.
func TestFailedResourceSkipsDependents(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil),
		res("File", "b", map[string]any{"require": []any{"File[a]"}}),
		res("File", "c", map[string]any{"require": []any{"File[b]"}}),
		res("File", "unrelated", nil),
	}}
	rep := runCatalog(t, tx, c)

	for _, ref := range []string{"File[b]", "File[c]"} {
		for _, got := range applied {
			if got == ref {
				t.Errorf("%s was applied despite its prerequisite failing", ref)
			}
		}
		rs, ok := rep.ResourceStatuses[ref]
		if !ok {
			t.Fatalf("report has no status for the skipped %s", ref)
		}
		if !rs.Skipped {
			t.Errorf("%s: Skipped = false, want true", ref)
		}
	}
	var sawUnrelated bool
	for _, got := range applied {
		if got == "File[unrelated]" {
			sawUnrelated = true
		}
	}
	if !sawUnrelated {
		t.Error("File[unrelated] was skipped; only dependents of the failure may be")
	}
	if got := rep.RunStatus().Failures; got != 1 {
		t.Errorf("RunStatus().Failures = %d, want 1", got)
	}
}

func TestRunRecordsEventsAndChangeCounts(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 2)
	c := &catalog.Catalog{Resources: []catalog.Resource{res("File", "a", nil)}}
	rep := runCatalog(t, tx, c)

	rs := rep.ResourceStatuses["File[a]"]
	if rs.ChangeCount != 2 {
		t.Errorf("ChangeCount = %d, want 2", rs.ChangeCount)
	}
	if len(rs.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2", len(rs.Events))
	}
	if rs.ResourceType != "File" || rs.Title != "a" {
		t.Errorf("ResourceType/Title = %q/%q, want File/a", rs.ResourceType, rs.Title)
	}
	for _, e := range rs.Events {
		if e.Status != "success" {
			t.Errorf("event Status = %q, want %q", e.Status, "success")
		}
	}
}

// Under --noop a resource is out of sync but NOT changed.
//
// Measured against Puppet 8.10.0: resource/status.rb:122-133 increments
// change_count only for events whose status is 'success', and a noop event's
// status is 'noop'. That is why `puppet apply --noop --detailed-exitcodes`
// exits 0 rather than 2. Setting ChangeCount here would emit 2 where Puppet
// emits 0 -- a drop-in break.
func TestNoopIsOutOfSyncButNotChanged(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 2)
	tx.Noop = true
	c := &catalog.Catalog{Resources: []catalog.Resource{res("File", "a", nil)}}
	rep := runCatalog(t, tx, c)

	rs := rep.ResourceStatuses["File[a]"]
	if rs.ChangeCount != 0 {
		t.Errorf("ChangeCount = %d under noop, want 0", rs.ChangeCount)
	}
	if rs.OutOfSyncCount != 2 {
		t.Errorf("OutOfSyncCount = %d under noop, want 2", rs.OutOfSyncCount)
	}
	for _, e := range rs.Events {
		if e.Status != "noop" {
			t.Errorf("event Status = %q under noop, want %q", e.Status, "noop")
		}
	}
	// The whole point: a noop run with pending drift exits 0, and noop_pending
	// carries the signal instead.
	if got := rep.RunStatus().Exit(true); got != 0 {
		t.Errorf("Exit(true) = %d under noop, want 0", got)
	}
	if !rep.NoopPending {
		t.Error("noop_pending = false; the would-be change is recorded nowhere")
	}
}

// An unregistered type reaching the transaction means pre-flight was skipped or
// is wrong. It is a could-not-run condition, not a resource failure: the run
// cannot make a claim about a catalog it did not understand.
func TestRunRejectsUnregisteredType(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{res("Service", "nginx", nil)}}
	g, err := BuildGraph(c, tx.Registry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	rep := report.New("n", "production", false)
	if err := tx.Run(context.Background(), c, g, rep); err == nil {
		t.Fatal("Run accepted an unregistered type; want an error")
	}
}

// A skipped resource's own dependents are skipped too. Without this, a failure
// three levels up would stop propagating after one hop.
func TestSkipPropagatesThroughSkippedResources(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil),
		res("File", "b", map[string]any{"require": []any{"File[a]"}}),
		res("File", "c", map[string]any{"require": []any{"File[b]"}}),
		res("File", "d", map[string]any{"require": []any{"File[c]"}}),
	}}
	rep := runCatalog(t, tx, c)
	for _, ref := range []string{"File[b]", "File[c]", "File[d]"} {
		if !rep.ResourceStatuses[ref].Skipped {
			t.Errorf("%s was not skipped", ref)
		}
	}
	if len(applied) != 1 {
		t.Errorf("applied = %v, want only File[a]", applied)
	}
}

// A refresh propagates along the chain: File[conf] ~> Exec[mid] ~> Exec[last]
// refreshes BOTH.
//
// Measured against Puppet 8.10.0, transaction/event_manager.rb process_events:
//
//	restarted = false
//	queued_events(resource) { |cb, ev| restarted ||= process_callback(...) }
//	if restarted
//	  queue_events(resource, [resource.event(name: :restarted, status: "success")])
//	  transaction.resource_status(resource).restarted = true
//	end
//
// The queue_events call is the chaining: a resource that was successfully
// refreshed emits a :restarted event of its own, which queue_events dispatches
// along its matching edges. So the refresh reaches Exec[last] even though
// Exec[mid] reported no change of its own.
//
// This test replaces one named TestRefreshDoesNotChainThroughRefreshedResources,
// which asserted the opposite AND could not observe it: its transaction changed
// nothing, so no refresh fired at all and the chain was never walked. What it
// actually pinned -- "a resource that reported no change does not notify" -- is
// TestRefreshFiresOnlyOnChange's job and is still covered there.
func TestRefreshChainsThroughRefreshedResources(t *testing.T) {
	var applied, refreshed []string
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: &applied, refreshed: &refreshed, changes: 1,
			// Only File[conf] changes. If the middle link changed on its own it
			// would notify for that reason, and the test would pass without any
			// chaining at all.
			noChange: map[string]bool{"Exec[mid]": true, "Exec[last]": true}}
	}
	tx := &Transaction{Registry: ral.NewRegistry(mk("file"), mk("exec"))}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "conf", map[string]any{"notify": []any{"Exec[mid]"}}),
		res("Exec", "mid", map[string]any{"notify": []any{"Exec[last]"}}),
		res("Exec", "last", nil),
	}})

	want := []string{"Exec[mid]", "Exec[last]"}
	if !slices.Equal(refreshed, want) {
		t.Errorf("refreshed = %v, want %v", refreshed, want)
	}
	for _, ref := range want {
		if !rep.ResourceStatuses[ref].Restarted {
			t.Errorf("%s is not marked restarted", ref)
		}
	}
}

// A log entry's source is the resource's full containment path plus the
// property, not the bare reference.
//
// Measured: Ruby logs "/Stage[main]/Main/Node[default]/File[x]/ensure". The
// leading slash and every container are part of the value a consumer greps,
// so "File[x]/ensure" is a different string rather than a shorter spelling of
// the same one.
func TestLogSourceIsTheContainmentPath(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("Class", "profile", nil),
			res("Notify", "hi", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[profile]"},
			{Source: "Class[profile]", Target: "Notify[hi]"},
		},
	}
	rep := runCatalog(t, tx, c)

	want := "/Stage[main]/Profile/Notify[hi]/p"
	var sources []string
	for _, l := range rep.Logs {
		sources = append(sources, l.Source)
	}
	if !slices.Contains(sources, want) {
		t.Errorf("log sources = %v, want one of them to be %q", sources, want)
	}
}

// An event with no explicit name gets Puppet's fallback, "<property>_changed".
func TestEventNameFallsBackToPropertyChanged(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("Notify", "hi", nil)},
	})
	events := rep.ResourceStatuses["Notify[hi]"].Events
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Name == nil || *events[0].Name != "p_changed" {
		t.Errorf("event name = %v, want \"p_changed\"", events[0].Name)
	}
}

// A failed refresh fails the resource and blocks its dependents.
//
// Measured against Puppet 8.10.0. transaction/event_manager.rb process_callback
// rescues the failure and does BOTH:
//
//	transaction.resource_status(resource).failed_to_restart = true
//	transaction.resource_status(resource).fail_with_event(resource_error_message)
//
// fail_with_event adds an event with status "failure", and resource/status.rb
// add_event's `when 'failure'` sets self.failed = true. transaction.rb:333
// propagate_failure then records this resource in each dependent's
// failed_dependencies on `s.failed? || s.failed_to_restart?`, and skip? (:401)
// skips them for it.
//
// So a refresh failure is a resource failure in Puppet, and the resources
// downstream of it do not run. voxagent previously recorded failed_to_restart
// alone and carried on applying them -- acting on a prerequisite the run had
// just disproved.
func TestFailedRefreshFailsTheResourceAndSkipsDependents(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, map[string]bool{"Exec[reload]": true}, 1)
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
		res("Exec", "reload", nil),
		res("Notify", "after", map[string]any{"require": []any{"Exec[reload]"}}),
	}}
	rep := runCatalog(t, tx, c)

	reload := rep.ResourceStatuses["Exec[reload]"]
	if !reload.FailedToRestart {
		t.Error("Exec[reload] is not marked failed_to_restart")
	}
	if !reload.Failed {
		t.Error("Exec[reload] is not marked failed; Puppet's fail_with_event sets both")
	}
	var failure *report.Event
	for i := range reload.Events {
		if reload.Events[i].Status == "failure" {
			failure = &reload.Events[i]
		}
	}
	if failure == nil {
		t.Errorf("no failure event on Exec[reload]: %+v", reload.Events)
	} else {
		if failure.Name == nil || *failure.Name != "resource_error" {
			t.Errorf("failure event name = %v, want \"resource_error\"", failure.Name)
		}
		// fail_with_event sets name, status and message and no property: a
		// callback failure describes the callback, not a property that moved.
		// Null, not "" -- event.rb:58 only assigns property when one was given.
		if failure.Property != nil {
			t.Errorf("failure event property = %#v, want nil", failure.Property)
		}
		if failure.Redacted != false {
			t.Errorf("failure event redacted = %#v, want false -- event.rb:38's"+
				" keyword default, which only a property event overrides", failure.Redacted)
		}
	}

	if !rep.ResourceStatuses["Notify[after]"].Skipped {
		t.Error("Notify[after] was applied; a resource whose prerequisite failed to restart must be skipped")
	}
	if slices.Contains(applied, "Notify[after]") {
		t.Errorf("applied = %v; Notify[after] should never have reached a provider", applied)
	}
}

// tx.Noop reaches the provider, on BOTH Apply and Refresh.
//
// This is the one thing standing between `voxagent apply --noop` and arbitrary
// command execution: the providers honour the flag correctly, so the only way a
// noop run can modify the machine is the transaction failing to pass it on.
// Measured before this test existed -- hard-coding false in the Refresh call
// left the whole suite green while a refreshonly exec created its marker file
// under --noop.
//
// Asserted per call rather than as a count: Apply being right does not imply
// Refresh is, and it was Refresh that was unpinned.
func TestNoopReachesBothProviderEntryPoints(t *testing.T) {
	for _, noop := range []bool{true, false} {
		t.Run(fmt.Sprintf("noop=%t", noop), func(t *testing.T) {
			var applied, refreshed []string
			var seen []string
			mk := func(typ string) recorder {
				return recorder{typ: typ, applied: &applied, refreshed: &refreshed,
					changes: 1, noopSeen: &seen}
			}
			tx := &Transaction{
				Registry: ral.NewRegistry(mk("file"), mk("exec")),
				Noop:     noop,
			}
			runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
				res("Exec", "reload", nil),
			}})

			want := []string{
				fmt.Sprintf("Apply File[conf] noop=%t", noop),
				fmt.Sprintf("Apply Exec[reload] noop=%t", noop),
				fmt.Sprintf("Refresh Exec[reload] noop=%t", noop),
			}
			for _, call := range want {
				if !slices.Contains(seen, call) {
					t.Errorf("provider calls = %v, want one of them to be %q", seen, call)
				}
			}
		})
	}
}

// A failed apply carries an event explaining itself, and logs the way Puppet
// logs it.
//
// Measured against Puppet 8.10.0. resource_harness.rb rescues an evaluation
// failure and calls status.failed_because(detail), which is explicit about the
// contract (resource/status.rb):
//
//	There's a contract (implicit unfortunately) that a status of failed
//	will always be accompanied by an event with some explanatory power.
//
// It logs `Could not evaluate: <detail>` and synthesizes an event named
// resource_error with status "failure" and the detail as its message. voxagent
// set Failed and wrote a bare log line, leaving events empty -- so a report
// consumer reading events saw a failed resource with nothing to say about why.
func TestFailedApplyCarriesAResourceErrorEvent(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"Notify[bad]": true}, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "bad", nil),
	}})

	rs := rep.ResourceStatuses["Notify[bad]"]
	if !rs.Failed {
		t.Fatal("Notify[bad] is not marked failed; the fixture proves nothing")
	}
	if len(rs.Events) != 1 {
		t.Fatalf("got %d events, want exactly 1 explaining the failure: %+v",
			len(rs.Events), rs.Events)
	}
	ev := rs.Events[0]
	if ev.Status != "failure" {
		t.Errorf("event status = %q, want \"failure\"", ev.Status)
	}
	if ev.Name == nil || *ev.Name != "resource_error" {
		t.Errorf("event name = %v, want \"resource_error\"", ev.Name)
	}
	if !strings.Contains(ev.Message, "provider refused") {
		t.Errorf("event message = %q, want the provider's error in it", ev.Message)
	}
	// Measured from a live Ruby report over a File that could not be created:
	// failed=true out_of_sync=true out_of_sync_count=1 change_count=0
	// changed=false. resource/status.rb add_event increments out_of_sync_count
	// for every non-audit event, a failure included, and only change_count is
	// reserved for successes.
	if rs.OutOfSyncCount != 1 {
		t.Errorf("out_of_sync_count = %d, want 1 (the failure event is non-audit)",
			rs.OutOfSyncCount)
	}
	if !rs.OutOfSync {
		t.Error("out_of_sync = false, want true")
	}
	if rs.ChangeCount != 0 || rs.Changed {
		t.Errorf("change_count = %d changed = %t, want 0/false: a failure is not a change",
			rs.ChangeCount, rs.Changed)
	}

	var errLog *report.LogEntry
	for i := range rep.Logs {
		if rep.Logs[i].Level == "err" {
			errLog = &rep.Logs[i]
		}
	}
	if errLog == nil {
		t.Fatalf("no err-level log entry: %+v", rep.Logs)
	}
	if !strings.HasPrefix(errLog.Message, "Could not evaluate: ") {
		t.Errorf("err log = %q, want it to start with \"Could not evaluate: \"", errLog.Message)
	}
}

// A property that was attempted and failed is reported against that property,
// with the transition it tried -- not as a bare resource_error -- and its error
// reaches the message verbatim.
//
// resource_harness.rb:153 composes only the "change from X to Y failed: "
// prefix; everything after it is the rescued detail's own to_s. Ruby's
// "Could not set '<value>' on ensure: <detail> (file: F, line: N)" wrapper is
// built one layer down, by property.rb:483 set, and only for a value setter
// that raised something other than a Puppet::Error -- so the transaction must
// neither add it nor assume it.
func TestFailedEnsureIsAttributedAndItsErrorUsedVerbatim(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "ensure"})

	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/f", File: "/etc/site.pp", Line: 6},
	}}
	rep := runCatalog(t, tx, c)

	st, ok := rep.ResourceStatuses["File[/f]"]
	if !ok {
		t.Fatalf("no status for File[/f]")
	}
	if len(st.Events) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(st.Events), st.Events)
	}
	ev := st.Events[0]
	if ev.Name == nil || *ev.Name != "file_created" {
		t.Errorf("name = %v, want file_created", ev.Name)
	}
	if ev.Property != "ensure" {
		t.Errorf("property = %q, want ensure", ev.Property)
	}
	if ev.Status != "failure" {
		t.Errorf("status = %q, want failure", ev.Status)
	}
	want := "change from 'absent' to 'file' failed: Could not set 'file' on ensure: " +
		"permission denied (file: /etc/site.pp, line: 6)"
	if ev.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", ev.Message, want)
	}
	if !st.Failed {
		t.Error("Failed = false, want true")
	}
}

// A failed array-matching property renders its list the way Ruby does, end to
// end. The fmtValue unit test pins the rendering; this pins the message the
// report actually carries, which is the leaf a differential compares.
//
// Measured against Puppet 8.10.0, `exec { 'bad': command => '/bin/false' }`:
//
//	"previous_value"=>"notrun", "desired_value"=>["0"],
//	"message"=>"change from 'notrun' to ['0'] failed: '/bin/false' returned 1 instead of one of [0]",
//	"name"=>"executed_command", "status"=>"failure"
//
// desired_value stays the raw list -- the brackets and quotes belong to the
// message alone, exactly as the uid-versus-name overrides do one test below.
func TestFailedArrayMatchingPropertyRendersItsListLikeRuby(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"Exec[bad]": "returns"})

	c := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Exec", Title: "bad"}}}
	rep := runCatalog(t, tx, c)

	st, ok := rep.ResourceStatuses["Exec[bad]"]
	if !ok {
		t.Fatalf("no status for Exec[bad]")
	}
	if len(st.Events) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(st.Events), st.Events)
	}
	ev := st.Events[0]
	if ev.Name == nil || *ev.Name != "executed_command" {
		t.Errorf("name = %v, want executed_command", ev.Name)
	}
	want := "change from 'notrun' to ['0'] failed: '/bin/false' returned 1 instead of one of [0]"
	if ev.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", ev.Message, want)
	}
	desired, ok := ev.DesiredValue.([]string)
	if !ok || !slices.Equal(desired, []string{"0"}) {
		t.Errorf("desired_value = %#v, want []string{\"0\"} -- the event carries the raw list", ev.DesiredValue)
	}
}

// The transaction adds no wrapper of its own. A property whose error arrives
// unwrapped is reported unwrapped, with no "(file:, line:)" suffix even though
// the fixture's resource carries both -- property.rb:190 call_provider, which
// owner/group/mode/content reach, wraps nothing. And the event's raw values
// survive while only the message renders names.
func TestFailedNonEnsurePropertyIsNotWrapped(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "mixed"})

	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/f", File: "/etc/site.pp", Line: 6},
	}}
	rep := runCatalog(t, tx, c)

	st := rep.ResourceStatuses["File[/f]"]
	if len(st.Events) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(st.Events), st.Events)
	}
	ev := st.Events[1]
	if ev.Property != "owner" {
		t.Fatalf("second event property = %q, want owner", ev.Property)
	}
	if ev.Name == nil || *ev.Name != "owner_changed" {
		t.Errorf("name = %v, want owner_changed", ev.Name)
	}
	want := "change from 'llm' to 'root' failed: operation not permitted"
	if ev.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", ev.Message, want)
	}
	// The display override governs the MESSAGE only. previous_value keeps the
	// raw uid, which is what Ruby's create_change_event stores.
	if ev.PreviousValue != 1000 {
		t.Errorf("previous_value = %#v, want the raw int 1000", ev.PreviousValue)
	}
	if ev.DesiredValue != 0 {
		t.Errorf("desired_value = %#v, want the raw int 0", ev.DesiredValue)
	}
}

// EVERY property failure is logged twice: once at the resource path plus the
// property (event.send_log) and once at source "Puppet", carrying the detail
// alone. Neither log is conditional on the property being ensure --
// resource_harness.rb:149 Puppet.log_exception(detail) sits in the rescue that
// catches all of them, so voxagent logging at "Puppet" for ensure only left a
// failed owner with one err log where Ruby writes two.
func TestEveryFailedPropertyLogsAtTheResourceAndAtPuppet(t *testing.T) {
	for _, tc := range []struct {
		shape    string
		property string
		detail   string
	}{
		{"ensure", "ensure", "Could not set 'file' on ensure: permission denied" +
			" (file: /etc/site.pp, line: 6)"},
		{"mixed", "owner", "operation not permitted"},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var applied, refreshed []string
			tx := txWithFailChange(&applied, &refreshed,
				map[string]string{"File[/f]": tc.shape})

			c := &catalog.Catalog{Resources: []catalog.Resource{
				{Type: "File", Title: "/f", File: "/etc/site.pp", Line: 6},
			}}
			rep := runCatalog(t, tx, c)

			var atProperty, atPuppet int
			for _, l := range rep.Logs {
				if l.Level != "err" {
					continue
				}
				switch {
				case strings.HasSuffix(l.Source, "/"+tc.property):
					atProperty++
				case l.Source == "Puppet":
					atPuppet++
					if l.Message != tc.detail {
						t.Errorf("Puppet-sourced message =\n  %q\nwant the detail alone\n  %q",
							l.Message, tc.detail)
					}
				}
			}
			if atProperty != 1 {
				t.Errorf("err logs sourced at .../%s = %d, want 1", tc.property, atProperty)
			}
			if atPuppet != 1 {
				t.Errorf("err logs sourced at Puppet = %d, want 1", atPuppet)
			}
		})
	}
}

// add_event credits its two counters differently, and conflating them is
// invisible until a resource has both kinds of event.
//
// resource/status.rb:122 add_event, verbatim: every non-audit event credits
// out_of_sync_count; only status 'success' credits change_count and sets
// changed. So one success plus one failure is out_of_sync_count=2,
// change_count=1, failed=true -- and a resource whose ONLY events are
// failures is out of sync but NOT changed, which keeps it out of
// metrics.resources.changed.
func TestMixedSuccessAndFailureCountsFollowAddEvent(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "mixed"})

	c := &catalog.Catalog{Resources: []catalog.Resource{{Type: "File", Title: "/f"}}}
	rep := runCatalog(t, tx, c)

	st := rep.ResourceStatuses["File[/f]"]
	if st.OutOfSyncCount != 2 {
		t.Errorf("OutOfSyncCount = %d, want 2 (every non-audit event counts)", st.OutOfSyncCount)
	}
	if st.ChangeCount != 1 {
		t.Errorf("ChangeCount = %d, want 1 (only the success counts)", st.ChangeCount)
	}
	if !st.Failed {
		t.Error("Failed = false, want true")
	}
}

// A failed resource's dependents are not applied. transaction.rb:333
// propagate_failure records the failure in each dependent's
// failed_dependencies on s.failed?, and skip? (:401) skips them for it --
// applying a resource whose prerequisite the run just disproved is the failure
// mode this guards.
func TestFailedPropertySkipsDependents(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "ensure"})

	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/f"},
		{Type: "Notify", Title: "after", Parameters: map[string]any{"require": "File[/f]"}},
	}}
	rep := runCatalog(t, tx, c)

	if slices.Contains(applied, "Notify[after]") {
		t.Errorf("Notify[after] was applied despite its prerequisite failing; applied = %v", applied)
	}
	_ = rep
}

// A successful refresh reports itself the way Puppet reports it, and a noop run
// reports that it would have.
//
// Measured against Puppet 8.10.0, transaction/event_manager.rb:
//
//	process_callback success:
//	  resource.notice "Triggered 'refresh' from N event(s)"
//	  add_callback_status_event(resource, :refresh, message, "success")
//	    -> an event with name "refresh", NO property, and that same message
//	  returns true, so process_events sets status.restarted = true
//
//	process_callback? false (any global --noop run):
//	  process_noop_events -> notice "Would have triggered 'refresh' from N event(s)"
//	  queues a noop_restart event to OTHER resources, and adds NOTHING to this
//	  resource's own status
//	  returns FALSE, so restarted is never set
//
// So under --noop Puppet reports restarted=false and gives the resource no
// refresh event at all. voxagent set restarted=true and added an event, which
// also kept the resource out of sync and so survived report pruning -- a whole
// extra resource status in every noop report containing a notify.
func TestRefreshReportingMatchesPuppet(t *testing.T) {
	for _, tt := range []struct {
		name          string
		noop          bool
		wantRestarted bool
		wantEvents    int
		wantLog       string
	}{
		{
			name: "a real run", noop: false, wantRestarted: true, wantEvents: 1,
			wantLog: "Triggered 'refresh' from 1 event",
		},
		{
			name: "under noop", noop: true, wantRestarted: false, wantEvents: 0,
			wantLog: "Would have triggered 'refresh' from 1 event",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var applied, refreshed []string
			mk := func(typ string) recorder {
				return recorder{typ: typ, applied: &applied, refreshed: &refreshed,
					changes: 1, noChange: map[string]bool{"Exec[reload]": true}}
			}
			tx := &Transaction{
				Registry: ral.NewRegistry(mk("file"), mk("exec")),
				Noop:     tt.noop,
			}
			rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
				res("Exec", "reload", nil),
			}})

			rs := rep.ResourceStatuses["Exec[reload]"]
			if rs.Restarted != tt.wantRestarted {
				t.Errorf("restarted = %t, want %t", rs.Restarted, tt.wantRestarted)
			}
			// Exec[reload] reports no changes of its own in this fixture, so
			// every event on it comes from the refresh.
			if len(rs.Events) != tt.wantEvents {
				t.Errorf("got %d events on Exec[reload], want %d: %+v",
					len(rs.Events), tt.wantEvents, rs.Events)
			}
			if tt.wantEvents == 1 {
				ev := rs.Events[0]
				if ev.Name == nil || *ev.Name != "refresh" {
					t.Errorf("event name = %v, want \"refresh\"", ev.Name)
				}
				// NULL, not the empty string, and false, not null -- both
				// measured in one Ruby report by G5's exec fixture, which is
				// the first gate to compare an event that has no property:
				//
				//	returns event: "property":"returns", "redacted":null
				//	refresh event: "property":null,      "redacted":false
				//
				// The two halves have one cause. event.rb:38 gives `redacted`
				// the keyword default FALSE and event.rb:58 does
				// `self.property = property if property`, so an event built
				// directly -- add_callback_status_event, fail_with_event --
				// leaves property nil and takes that default. A PROPERTY event
				// is built through property.rb:260, which passes
				// `attrs[:redacted] = @sensitive`, and @sensitive is nil for
				// every property outside the spine's bounded sets. So the two
				// kinds of event disagree about both fields, in opposite
				// directions, and a report processor switching on
				// `event['property']` must get null rather than "".
				if ev.Property != nil {
					t.Errorf("event property = %#v, want nil: a callback event has no property",
						ev.Property)
				}
				if ev.Redacted != false {
					t.Errorf("event redacted = %#v, want false: event.rb:38's keyword default",
						ev.Redacted)
				}
				if ev.Message != tt.wantLog {
					t.Errorf("event message = %q, want %q", ev.Message, tt.wantLog)
				}
			}

			var found bool
			for _, l := range rep.Logs {
				if l.Message == tt.wantLog {
					found = true
				}
			}
			if !found {
				var got []string
				for _, l := range rep.Logs {
					got = append(got, l.Message)
				}
				t.Errorf("logs = %v, want one of them to be %q", got, tt.wantLog)
			}
		})
	}
}

// fmtValue reproduces Puppet's format_value_for_display, whose quoting is not
// decoration: it is the difference between `change from 'llm' to 'root'` and
// `change from 1000 to 0`, and those are different leaves to the differential.
//
// Measured against Puppet 8.10.0 by calling
// Puppet::Parameter.format_value_for_display directly.
func TestFmtValueMatchesPuppetDisplayFormat(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string is quoted", "absent", "'absent'"},
		{"octal-looking string stays a string", "0644", "'0644'"},
		{"string with a space is quoted whole", "a b", "'a b'"},
		{"int is bare", 1000, "1000"},
		{"json number is bare and not floated", float64(1000), "1000"},
		{"bool is bare", true, "true"},
		{"nil renders empty", nil, ""},
		// An array-matching property holds a LIST. exec's returns is one:
		// type/exec.rb declares :array_matching => :all with munge(&:to_s), so
		// desired_value is an array of strings and the message must render it
		// the way Ruby does -- brackets, quoted elements, ", " between them.
		//
		// Measured by calling Puppet::Parameter.format_value_for_display on
		// each of these in Puppet 8.10.0:
		//
		//	["0"]           -> ['0']
		//	["0", "3"]      -> ['0', '3']
		//	["0", "3", "7"] -> ['0', '3', '7']
		//	[]              -> []
		//
		// The spacing is load-bearing: it is the difference between the
		// measured message `change from 'notrun' to ['0', '3'] failed: ...`
		// and a divergent leaf in every failing exec event.
		{"one-element list", []string{"0"}, "['0']"},
		{"multi-element list", []string{"0", "3"}, "['0', '3']"},
		{"three-element list", []string{"0", "3", "7"}, "['0', '3', '7']"},
		{"empty list", []string{}, "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmtValue(tc.in); got != tc.want {
				t.Errorf("fmtValue(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// display is Ruby's is_to_s/should_to_s: property METHODS that
// type/file/owner.rb and group.rb override to print names, while the event's
// previous_value keeps the raw integer uid. Measured: one event with
// previous_value 1000 whose message reads "change from 'llm' to 'nosuchuser'
// failed: ...". The override governs the message only.
func TestDisplayHonoursTheOverride(t *testing.T) {
	if got := display(1000, "llm"); got != "'llm'" {
		t.Errorf("display(1000, \"llm\") = %q, want %q", got, "'llm'")
	}
	if got := display(1000, ""); got != "1000" {
		t.Errorf("display(1000, \"\") = %q, want %q -- no override means fmtValue", got, "1000")
	}
	if got := display("absent", ""); got != "'absent'" {
		t.Errorf("display(\"absent\", \"\") = %q, want %q", got, "'absent'")
	}
}

// A resource that failed one of its own properties is STILL refreshed.
//
// Measured 2026-09-18 against Puppet 8.10.0 in a container, with exec's
// `refresh` parameter decoupling the apply command from the refresh command and
// timestamped side effects counting invocations: the apply command ran, failed,
// and the refresh command ran anyway. Ruby reported failed=true together with a
// refresh/success event, change_count=1, out_of_sync_count=2.
//
// The mechanism: transaction.rb:288 calls process_events(resource)
// unconditionally -- only skip? guards it -- and event_manager.rb's
// process_callback? tests noop, never failure. The one place failure DOES gate
// is transaction.rb:269, on the OUTGOING queue_events.
//
// voxagent short-circuited past its refresh block whenever the resource had
// failed, emitting one event where Ruby emits two.
func TestFailedResourceIsStillRefreshed(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "ensure"})

	// Notify[trigger] succeeds and changes, so it notifies File[/f]. File[/f]
	// then fails its own ensure. Because the NOTIFIER succeeded, File[/f] is not
	// skipped -- it is applied, it fails, and its queued refresh must still run.
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Notify", Title: "trigger", Parameters: map[string]any{
			"notify": "File[/f]",
		}},
		{Type: "File", Title: "/f"},
	}}
	rep := runCatalog(t, tx, c)

	if !slices.Contains(applied, "File[/f]") {
		t.Fatalf("File[/f] was never applied; applied = %v", applied)
	}
	if !slices.Contains(refreshed, "File[/f]") {
		t.Fatalf("File[/f] failed its apply and was never refreshed; refreshed = %v -- "+
			"transaction.rb:288 calls process_events unconditionally", refreshed)
	}

	st := rep.ResourceStatuses["File[/f]"]
	if !st.Failed {
		t.Error("Failed = false; the apply failure must survive a successful refresh")
	}
	if st.FailedToRestart {
		t.Error("FailedToRestart = true, but the refresh succeeded")
	}
	// The measured shape: two events, one failure and one success.
	if len(st.Events) != 2 {
		t.Fatalf("want 2 events (the failure and the refresh), got %d: %+v",
			len(st.Events), st.Events)
	}
	if st.Events[0].Status != "failure" || st.Events[1].Status != "success" {
		t.Errorf("event statuses = %q,%q; want failure,success",
			st.Events[0].Status, st.Events[1].Status)
	}
	if st.OutOfSyncCount != 2 {
		t.Errorf("OutOfSyncCount = %d, want 2 -- add_event credits every non-audit event",
			st.OutOfSyncCount)
	}
	if st.ChangeCount != 1 {
		t.Errorf("ChangeCount = %d, want 1 -- only the refresh succeeded", st.ChangeCount)
	}
}

// A failed notifier's target is skipped, and is therefore never refreshed.
//
// This is the OUTWARD direction, and it is worth being precise about what pins
// what. Puppet gates outgoing propagation on failure at transaction.rb:269,
// which wraps queue_events in `unless status.failed? || status.failed_to_restart?`,
// and voxagent mirrors that gate. But the gate is **unobservable** here: a
// notify edge is also an ordering edge, so the refresh target is always a
// dependent of the failed resource, and a skipped resource never reaches the
// refresh block at all. Removing the gate entirely leaves this test green --
// verified by deliberate sabotage.
//
// So this test does NOT pin the gate. It pins the invariant that makes the gate
// redundant: the target is skipped. If that ever stops holding -- a refresh edge
// without a matching ordering edge -- this test goes red, and the gate stops
// being decoration and starts being load-bearing.
//
// Measured: Ruby reaches the same end state by a different route, logging
// "Dependency Exec[fail] has failures: true" and calling
// dequeue_all_events_for_resource to discard the target's queued callbacks.
func TestFailedNotifiersTargetIsSkippedNotRefreshed(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/f]": "mixed"})

	// File[/f] has one successful change and one failed one, so it is Failed
	// while still having produced a change that would otherwise propagate.
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/f", Parameters: map[string]any{
			"notify": "Exec[downstream]",
		}},
		{Type: "Exec", Title: "downstream"},
	}}
	rep := runCatalog(t, tx, c)

	st, ok := rep.ResourceStatuses["Exec[downstream]"]
	if !ok {
		t.Fatal("no status for Exec[downstream]; a skipped resource still gets one")
	}
	if !st.Skipped {
		t.Errorf("Exec[downstream] was not skipped despite its notifier failing: %+v", st)
	}
	if slices.Contains(applied, "Exec[downstream]") {
		t.Errorf("Exec[downstream] was applied; applied = %v", applied)
	}
	if slices.Contains(refreshed, "Exec[downstream]") {
		t.Errorf("Exec[downstream] was refreshed; refreshed = %v", refreshed)
	}
}

// A provider's run-level warning reaches the report's log, at level "warning"
// and source "Puppet" -- the positive control for the whole channel. Without
// this, the dedupe test below would pass just as well against a transaction
// that logged nothing at all.
//
// Source "Puppet" and not the resource path because the Ruby line this
// reproduces is Puppet.warning on the GLOBAL logger (util/warnings.rb:18),
// which carries no resource context.
func TestProviderWarningReachesTheReportLog(t *testing.T) {
	var applied, refreshed []string
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{ownerWarning})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil)},
	})

	var got []report.LogEntry
	for _, l := range rep.Logs {
		if l.Message == "Cannot manage ownership unless running as root" {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("warning log entries = %d, want 1; logs = %+v", len(got), rep.Logs)
	}
	if got[0].Level != "warning" {
		t.Errorf("level = %q, want %q", got[0].Level, "warning")
	}
	if got[0].Source != "Puppet" {
		t.Errorf("source = %q, want %q", got[0].Source, "Puppet")
	}
}

// Two resources raising the SAME warning produce exactly ONE log entry.
//
// This is Puppet's warnonce, and the dedupe is the whole behaviour:
// util/warnings.rb:17 routes through maybe_log, which keys @stampwarnings on
// the message and returns without yielding when it has seen it. Measured
// 2026-09-18, unprivileged, two file resources both with an out-of-sync owner:
// one "warning | Puppet | Cannot manage ownership unless running as root" line
// in the whole run.
//
// The dedupe cannot live in the provider -- providers are stateless values,
// constructed per registry and holding no run state -- so nothing but this test
// pins it.
func TestRunLogsARepeatedProviderWarningOnlyOnce(t *testing.T) {
	var applied, refreshed []string
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{ownerWarning})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil), res("File", "/two", nil)},
	})

	// Both resources really were applied, so both really did return the
	// warning. Without this the test would also pass if the second resource
	// had never been reached.
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want both resources applied", applied)
	}
	var n int
	for _, l := range rep.Logs {
		if l.Message == "Cannot manage ownership unless running as root" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warning logged %d times, want exactly 1; logs = %+v", n, rep.Logs)
	}
}

// Two DIFFERENT warnings from the same class are both logged: the dedupe key
// includes the message, so it must not collapse distinct conditions into one
// line. Same class deliberately -- that is what leaves the message as the only
// thing telling the two apart.
func TestRunLogsEachDistinctProviderWarning(t *testing.T) {
	var applied, refreshed []string
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{
		{Source: "Puppet", Message: "first condition", DedupeClass: "Puppet::Type::File::Owner"},
		{Source: "Puppet", Message: "second condition", DedupeClass: "Puppet::Type::File::Owner"},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil), res("File", "/two", nil)},
	})

	var got []string
	for _, l := range rep.Logs {
		if l.Level == "warning" {
			got = append(got, l.Message)
		}
	}
	want := []string{"first condition", "second condition"}
	if !slices.Equal(got, want) {
		t.Errorf("warning logs = %v, want %v (each once, in the order first seen)", got, want)
	}
}

// Two warnings with the same message from DIFFERENT dedupe classes are both
// logged. maybe_log keys on the calling class AND the message
// (util/warnings.rb:28-29), so suppressing the second would be voxagent
// deduping more aggressively than Ruby.
//
// The third fixture entry repeats the first pair exactly, which is what pins
// the assertion to 2 rather than to "every warning is logged": a key that
// ignored the class would collapse all three to one, and a key that ignored
// the message would keep all three.
func TestWarningDedupeIsKeyedOnClassAndMessage(t *testing.T) {
	var applied, refreshed []string
	const shared = "Could not look up qualified macaddress fact"
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{
		{Source: "Puppet", Message: shared, DedupeClass: "Puppet::Type::File::Owner"},
		{Source: "Puppet", Message: shared, DedupeClass: "Puppet::Type::File::Group"},
		{Source: "Puppet", Message: shared, DedupeClass: "Puppet::Type::File::Owner"},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil)},
	})

	var n int
	for _, l := range rep.Logs {
		if l.Message == shared {
			n++
		}
	}
	if n != 2 {
		t.Errorf("warning logged %d times, want exactly 2 -- one per class, the"+
			" repeated class suppressed; logs = %+v", n, rep.Logs)
	}
}

// A warning with an empty DedupeClass is logged every time it is raised.
// Puppet.warning does not dedupe at all; only maybe_log does.
func TestWarningWithNoDedupeClassIsLoggedEveryTime(t *testing.T) {
	var applied, refreshed []string
	const msg = "Could not check for updates, '/usr/bin/dnf check-update' exited with 1"
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{
		{Source: "Puppet", Message: msg},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil), res("File", "/two", nil)},
	})

	// Both resources really were applied, so both really did raise the warning.
	// Without this the test would also pass if the second resource had never
	// been reached.
	if len(applied) != 2 {
		t.Fatalf("applied = %v, want both resources applied", applied)
	}
	var n int
	for _, l := range rep.Logs {
		if l.Message == msg {
			n++
		}
	}
	if n != 2 {
		t.Errorf("warning logged %d times, want exactly 2 -- once per resource"+
			" that raised it; logs = %+v", n, rep.Logs)
	}
}

// A warning carries its own source, not a hardcoded "Puppet". A provider
// CLASS's warning is sourced to the class name (util/logging.rb:298-302 falls
// through to to_s for anything that is not a Puppet::Type or Puppet::Parameter)
// -- measured verbatim against Ruby as
// "warning | Puppet::Type::Package::ProviderDnf | Could not check for updates".
func TestWarningCarriesItsOwnSource(t *testing.T) {
	var applied, refreshed []string
	const source = "Puppet::Type::Package::ProviderDnf"
	const msg = "Could not check for updates, '/usr/bin/dnf check-update' exited with 1"
	tx := txWithWarnings(&applied, &refreshed, []ral.Warning{
		{Source: source, Message: msg, DedupeClass: "Puppet::Type::Package::ProviderDnf"},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{res("File", "/one", nil)},
	})

	var got []report.LogEntry
	for _, l := range rep.Logs {
		if l.Message == msg {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("warning log entries = %d, want 1; logs = %+v", len(got), rep.Logs)
	}
	if got[0].Level != "warning" {
		t.Errorf("level = %q, want %q", got[0].Level, "warning")
	}
	if got[0].Source != source {
		t.Errorf("source = %q, want %q", got[0].Source, source)
	}
}

// Every type in the catalog is prefetched exactly once, before any resource is
// applied, and each provider is handed every resource of its own type.
//
// Ruby prefetches lazily -- transaction.rb:243-260 calls prefetch_if_necessary
// as each resource is evaluated and memoises on (type, provider class), so the
// first resource of a type pays and the rest read the cache. Doing it up front
// for every type present is equivalent for the report, because the memo makes
// the call count identical and nothing a provider applies can change what
// another type's prefetch would read.
func TestPrefetchRunsOncePerTypeBeforeTheWalk(t *testing.T) {
	var applied, refreshed, prefetched []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetched: &prefetched})
	runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("Notify", "hi", nil), res("File", "/two", nil),
	}})

	// Sorted by type, so the assertion does not depend on catalog order.
	want := []string{
		"file [File[/one] File[/two]] after 0 applies",
		"notify [Notify[hi]] after 0 applies",
	}
	if !slices.Equal(prefetched, want) {
		t.Errorf("prefetched = %#v, want %#v", prefetched, want)
	}
	if len(applied) != 3 {
		t.Errorf("applied = %v, want all three resources applied", applied)
	}
}

// A registered type with no resources in the catalog is not prefetched.
//
// Ruby cannot prefetch one: prefetch_if_necessary is reached only through a
// resource being evaluated, so a type nothing declares never gets there. An
// up-front implementation has to derive the type set from the CATALOG rather
// than from the registry, and one that walked the registry would run dnf's
// prefetch -- an rpm query and possibly a check-update -- on every run of every
// catalog that mentions no packages at all.
func TestTypeWithNoResourcesIsNotPrefetched(t *testing.T) {
	var applied, refreshed, prefetched []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetched: &prefetched})
	runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "hi", nil),
	}})

	want := []string{"notify [Notify[hi]] after 0 applies"}
	if !slices.Equal(prefetched, want) {
		t.Errorf("prefetched = %#v, want %#v -- file and exec are registered but"+
			" the catalog declares neither", prefetched, want)
	}
}

// The value Prefetch RETURNS is the value Apply is called on, for the whole
// run.
//
// This is the entire reason Prefetch returns a Provider instead of mutating
// one: the providers are stateless values held by a shared registry, so a
// transaction that kept applying through the registry's copy would hand dnf's
// Apply a nil installed-package state and fail every package resource with "the
// dnf provider was not prefetched".
func TestPrimedProviderIsTheOneApplyReceives(t *testing.T) {
	var applied, refreshed, primedSeen []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{primedSeen: &primedSeen})
	runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("Notify", "hi", nil),
	}})

	want := []string{"File[/one] primed=true", "Notify[hi] primed=true"}
	if !slices.Equal(primedSeen, want) {
		t.Errorf("primedSeen = %#v, want %#v", primedSeen, want)
	}
}

// A failed prefetch fails EVERY resource of that type, applies NONE of them,
// and leaves the other types untouched.
//
// Measured against Puppet 8.10.0 in the fleet EL image, non-root, over six
// package resources (.test-output/package-oracle/out/report-nonroot.json): six
// statuses, each failed=true skipped=true out_of_sync=true out_of_sync_count=1
// change_count=0 changed=false provider_used="dnf" evaluation_time=null, each
// carrying exactly one resource_error event whose message is
// "Prefetch failed for package provider 'dnf'" (transaction.rb:333-339, text at
// :338) and whose property is null.
//
// Both flags, not one. propagate_failure adds the failure event before skip?
// runs, and skip? then returns true on `status.failed? &&
// prefetch_failed_providers[...]` -- so Ruby marks the resource failed AND
// skipped, and a report consumer filtering on either sees it.
func TestPrefetchFailureFailsEveryResourceOfTheTypeAndAppliesNone(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New("the yum provider can only be used as root")},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("File", "/two", nil), res("Notify", "hi", nil),
	}})

	if !slices.Equal(applied, []string{"Notify[hi]"}) {
		t.Fatalf("applied = %v, want only Notify[hi] -- no file may be applied"+
			" through a provider that could not be prefetched", applied)
	}
	for _, ref := range []string{"File[/one]", "File[/two]"} {
		rs, ok := rep.ResourceStatuses[ref]
		if !ok {
			t.Fatalf("%s has no resource status; statuses = %v", ref, rep.ResourceStatuses)
		}
		if !rs.Failed || !rs.Skipped {
			t.Errorf("%s failed = %t skipped = %t, want both true", ref, rs.Failed, rs.Skipped)
		}
		if rs.OutOfSyncCount != 1 || !rs.OutOfSync {
			t.Errorf("%s out_of_sync_count = %d out_of_sync = %t, want 1/true",
				ref, rs.OutOfSyncCount, rs.OutOfSync)
		}
		if rs.ChangeCount != 0 || rs.Changed {
			t.Errorf("%s change_count = %d changed = %t, want 0/false",
				ref, rs.ChangeCount, rs.Changed)
		}
		if rs.FailedToRestart || rs.CorrectiveChange {
			t.Errorf("%s failed_to_restart = %t corrective_change = %t, want both false",
				ref, rs.FailedToRestart, rs.CorrectiveChange)
		}
		// Ruby reports evaluation_time null here: the resource was never
		// evaluated, so no span was recorded. Go's field is a float64 and its
		// zero is the closest thing it can write; the leaf is declared volatile
		// in tests/spine/allowlist.json, so the two never meet in the
		// differential. What must not happen is a NON-zero value, which would
		// say a resource that was never applied took time to apply.
		if rs.EvaluationTime != 0 {
			t.Errorf("%s evaluation_time = %v, want 0 (Ruby writes null)", ref, rs.EvaluationTime)
		}
		if rs.ProviderUsed == nil || *rs.ProviderUsed != "posix" {
			t.Errorf("%s provider_used = %v, want \"posix\" -- Ruby names the"+
				" provider even though it could not be prefetched", ref, rs.ProviderUsed)
		}
		if len(rs.Events) != 1 {
			t.Fatalf("%s has %d events, want exactly 1: %+v", ref, len(rs.Events), rs.Events)
		}
		ev := rs.Events[0]
		if ev.Name == nil || *ev.Name != "resource_error" {
			t.Errorf("%s event name = %v, want \"resource_error\"", ref, ev.Name)
		}
		if ev.Status != "failure" {
			t.Errorf("%s event status = %q, want \"failure\"", ref, ev.Status)
		}
		if ev.Property != nil {
			t.Errorf("%s event property = %#v, want nil -- Ruby writes null;"+
				" a prefetch failure blames no property", ref, ev.Property)
		}
		if ev.Redacted != false {
			t.Errorf("%s event redacted = %#v, want false -- event.rb:38's keyword"+
				" default; only a property event carries the property's own nil"+
				" `sensitive`", ref, ev.Redacted)
		}
		const wantMsg = "Prefetch failed for file provider 'posix'"
		if ev.Message != wantMsg {
			t.Errorf("%s event message = %q, want %q", ref, ev.Message, wantMsg)
		}
	}
	if rs := rep.ResourceStatuses["Notify[hi]"]; rs.Failed || rs.Skipped {
		t.Errorf("Notify[hi] failed = %t skipped = %t, want both false -- the"+
			" failure is type-wide, not run-wide", rs.Failed, rs.Skipped)
	}
}

// The CAUSE is logged once for the run, at source "Puppet"; the SKIP is logged
// once per resource, at that resource's own path.
//
// Measured, same non-root run: one err entry
// "Could not prefetch package provider 'dnf': The yum provider can only be used
// as root" sourced "Puppet" (transaction.rb:384, via Puppet.log_exception), and
// six warning entries "Skipping because provider prefetch failed" sourced
// "/Stage[main]/Main/Package[<title>]" (transaction.rb:427).
//
// The two halves are deliberately asymmetric. Ruby rescues inside prefetch,
// which runs once per provider class, so the detail cannot repeat; the warning
// comes from skip?, which runs per resource. Emitting the cause per resource
// would put six identical err entries in a log Ruby gives one, and the report
// differential compares log content as a multiset -- so the count is the
// divergence it would report.
func TestPrefetchFailureLogsTheCauseOnceAndTheSkipPerResource(t *testing.T) {
	var applied, refreshed []string
	const detail = "the yum provider can only be used as root"
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New(detail)},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("File", "/two", nil), res("Notify", "hi", nil),
	}})

	var errLogs, warnLogs []report.LogEntry
	for _, l := range rep.Logs {
		switch l.Level {
		case "err":
			errLogs = append(errLogs, l)
		case "warning":
			warnLogs = append(warnLogs, l)
		}
	}

	if len(errLogs) != 1 {
		t.Fatalf("err log entries = %d, want exactly 1 -- the cause is coalesced"+
			" to one entry for the run: %+v", len(errLogs), errLogs)
	}
	if errLogs[0].Source != "Puppet" {
		t.Errorf("err source = %q, want \"Puppet\" -- Puppet.log_exception writes"+
			" through the global logger, not the resource", errLogs[0].Source)
	}
	wantErr := "Could not prefetch file provider 'posix': " + detail
	if errLogs[0].Message != wantErr {
		t.Errorf("err message = %q, want %q", errLogs[0].Message, wantErr)
	}

	wantWarn := []report.LogEntry{
		{Level: "warning", Source: "/File[/one]",
			Message: "Skipping because provider prefetch failed"},
		{Level: "warning", Source: "/File[/two]",
			Message: "Skipping because provider prefetch failed"},
	}
	if len(warnLogs) != len(wantWarn) {
		t.Fatalf("warning log entries = %d, want %d -- one per resource of the"+
			" failed type: %+v", len(warnLogs), len(wantWarn), warnLogs)
	}
	for i, want := range wantWarn {
		got := warnLogs[i]
		if got.Source != want.Source || got.Message != want.Message {
			t.Errorf("warning[%d] = {source %q, message %q}, want {source %q, message %q}",
				i, got.Source, got.Message, want.Source, want.Message)
		}
	}
}

// A successful prefetch changes nothing about the run.
//
// file, exec and notify have nothing to pre-read: each returns itself and a nil
// error. Wiring prefetch into the walk must therefore leave their reports
// exactly as they were, and this pins the WHOLE report -- every log entry and
// every status field -- for a run in which all three prefetch cleanly.
//
// Pinned literally rather than diffed against a second run of the same code.
// The first version of this test did the latter, and a mutation that made every
// successful prefetch emit an extra log entry passed it: both sides of the
// comparison gained the same entry. A test whose control is the code under test
// cannot see a change the code makes everywhere.
//
// It is the guard on the registration step: Package is added to the registry
// at the same time as this wiring, and the fleet's G1 fixture has no package
// resource -- so if that fixture moves, the wiring has perturbed the other
// three providers and this is the test that says so.
func TestSuccessfulPrefetchLeavesTheRunUnchanged(t *testing.T) {
	var applied, refreshed, prefetched []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetched: &prefetched})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil),
		res("Notify", "hi", nil),
		res("Exec", "/bin/true", map[string]any{"require": []any{"File[/one]"}}),
	}})

	if len(prefetched) != 3 {
		t.Fatalf("prefetched = %#v, want all three types -- otherwise the run"+
			" under test never prefetched anything", prefetched)
	}

	// Exactly one notice per resource: the property change each recorder
	// reports. No err, no warning, and nothing the prefetch added.
	// Finalize's closing notice is part of every report; it is listed rather
	// than filtered out, because filtering by source "Puppet" would also hide
	// the err entry a failed prefetch writes -- which is the thing this test
	// exists to deny.
	const appliedCatalog = "Applied catalog in <elapsed> seconds"
	want := []report.LogEntry{
		{Level: "notice", Source: "/File[/one]/p", Message: "changed"},
		{Level: "notice", Source: "/Notify[hi]/p", Message: "changed"},
		{Level: "notice", Source: "/Exec[/bin/true]/p", Message: "changed"},
		{Level: "notice", Source: "Puppet", Message: appliedCatalog},
	}
	got := make([]report.LogEntry, len(rep.Logs))
	for i, l := range rep.Logs {
		l.Time = ""
		if strings.HasPrefix(l.Message, "Applied catalog in ") {
			l.Message = appliedCatalog
		}
		got[i] = l
	}
	// Sorted, because the assertion is about WHICH entries exist, not the walk
	// order -- which the refresh and ordering tests already pin.
	bySource := func(a, b report.LogEntry) int { return strings.Compare(a.Source, b.Source) }
	slices.SortFunc(got, bySource)
	slices.SortFunc(want, bySource)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("logs = %+v, want %+v", got, want)
	}

	for _, ref := range []string{"File[/one]", "Notify[hi]", "Exec[/bin/true]"} {
		rs, ok := rep.ResourceStatuses[ref]
		if !ok {
			t.Fatalf("%s has no resource status", ref)
		}
		if rs.Failed || rs.Skipped || rs.FailedToRestart {
			t.Errorf("%s failed = %t skipped = %t failed_to_restart = %t, want all false",
				ref, rs.Failed, rs.Skipped, rs.FailedToRestart)
		}
		if !rs.Changed || rs.ChangeCount != 1 || rs.OutOfSyncCount != 1 {
			t.Errorf("%s changed = %t change_count = %d out_of_sync_count = %d,"+
				" want true/1/1", ref, rs.Changed, rs.ChangeCount, rs.OutOfSyncCount)
		}
		if len(rs.Events) != 1 || rs.Events[0].Status != "success" {
			t.Errorf("%s events = %+v, want one success event", ref, rs.Events)
		}
	}
}

// A resource whose type could not be prefetched blocks its dependents, however
// deep.
//
// MEASURED, not reasoned: a non-root run in the fleet EL image over
// `package{'tree'} -> notify{'after-package'} -> exec{'also-after-package'}`
// (.test-output/package-oracle/out/report-nonroot-deps.json) reported the
// package failed=true skipped=true and BOTH downstream resources skipped=true
// failed=false, each warned "Skipping because of failed dependencies". The exec
// is two hops out, so the block is transitive.
//
// That is the same rule as any other failure -- propagate_failure adds the
// resource to each dependent's failed_dependencies on `s.failed?`, and a
// prefetch failure sets failed? -- but "the same rule applies" was worth
// measuring rather than assuming, because the prefetch path reaches failed?
// through fail_with_event rather than through the harness.
func TestPrefetchFailureSkipsDependents(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New("cannot read the filesystem")},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/head", nil),
		res("Notify", "mid", map[string]any{"require": []any{"File[/head]"}}),
		res("Exec", "/bin/tail", map[string]any{"require": []any{"Notify[mid]"}}),
	}})

	if len(applied) != 0 {
		t.Fatalf("applied = %v, want nothing applied", applied)
	}
	for _, ref := range []string{"Notify[mid]", "Exec[/bin/tail]"} {
		rs, ok := rep.ResourceStatuses[ref]
		if !ok {
			t.Fatalf("%s has no resource status", ref)
		}
		if !rs.Skipped {
			t.Errorf("%s skipped = false, want true", ref)
		}
		if rs.Failed {
			t.Errorf("%s failed = true, want false -- a skipped dependent did not"+
				" itself fail", ref)
		}
		if len(rs.Events) != 0 {
			t.Errorf("%s has %d events, want none: %+v", ref, len(rs.Events), rs.Events)
		}
	}
}

// When a resource has BOTH a failed dependency and a failed provider prefetch,
// it still carries the prefetch failure -- but it is skipped for the DEPENDENCY.
//
// MEASURED: a non-root run over `package{'tree'}` and a `package{'zsh'}` that
// requires it (.test-output/package-oracle/out/report-nonroot-pkgdeps.json)
// reported both packages failed=true skipped=true with one resource_error event
// each, while the logs read "Skipping because provider prefetch failed" for tree
// and "Skipping because of failed dependencies" for zsh.
//
// Ruby's order is why: propagate_failure adds the prefetch event to every
// resource of the failed type unconditionally, and only then does skip? pick a
// reason -- testing failed_dependencies? BEFORE the prefetch clause. So the
// status is the same for both and only the log differs.
func TestPrefetchFailureWithAFailedDependencyKeepsTheDependencyReason(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New("cannot read the filesystem")},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/head", nil),
		res("File", "/tail", map[string]any{"require": []any{"File[/head]"}}),
	}})

	rs, ok := rep.ResourceStatuses["File[/tail]"]
	if !ok {
		t.Fatal("File[/tail] has no resource status")
	}
	if !rs.Failed || !rs.Skipped {
		t.Errorf("File[/tail] failed = %t skipped = %t, want both true -- the"+
			" prefetch failure applies to it too", rs.Failed, rs.Skipped)
	}
	if len(rs.Events) != 1 || rs.Events[0].Message != "Prefetch failed for file provider 'posix'" {
		t.Errorf("File[/tail] events = %+v, want exactly the prefetch resource_error", rs.Events)
	}

	// Exactly one resource carries the prefetch skip warning: the one that was
	// not already blocked by a failed dependency.
	var prefetchSkips []string
	for _, l := range rep.Logs {
		if l.Message == "Skipping because provider prefetch failed" {
			prefetchSkips = append(prefetchSkips, l.Source)
		}
	}
	if !slices.Equal(prefetchSkips, []string{"/File[/head]"}) {
		t.Errorf("prefetch skip warnings = %v, want only File[/head] -- File[/tail]"+
			" is skipped for its failed dependency instead", prefetchSkips)
	}
}
