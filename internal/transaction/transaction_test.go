package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/units/exec"
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

	// sourcesSeen records the sources.Sources each Apply and Refresh call's context
	// carried, when the test cares.
	sourcesSeen *[]sources.Sources
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

func (p recorder) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	*p.applied = append(*p.applied, r.Ref())
	if p.sourcesSeen != nil {
		*p.sourcesSeen = append(*p.sourcesSeen, sources.SourcesFrom(ctx))
	}
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
	case "invalidate":
		// service's start: ensure => running carries invalidate_refreshes
		// (type/service.rb:109), so the resource's own pending refresh is
		// dropped.
		return []ral.Change{{
			Property: "ensure", Was: "stopped", Is: "running",
			Name: "service_started", Message: "ensure changed 'stopped' to 'running'",
			InvalidateRefreshes: true,
		}}, p.warn, nil
	case "invalidate-failed":
		// The same change, failed. Ruby queues events only for a resource
		// that did not fail, so nothing is dequeued and the refresh still runs.
		return []ral.Change{{
			Property: "ensure", Was: "stopped", Is: "running",
			Name: "service_started", InvalidateRefreshes: true,
			Err: errors.New("Systemd start for svc failed!\njournalctl log for svc:\n"),
		}}, p.warn, nil
	case "pathnotice":
		// file's purge of a directory: a resource-level notice beside a
		// successful ensure change (type/file.rb:1040).
		return []ral.Change{{
			Property: "ensure", Was: "directory", Is: "absent",
			Name: "file_removed", Message: "removed",
			PathNotices: []string{"Not removing directory; use 'force' to override"},
		}}, p.warn, nil
	case "twonotices":
		// remove_existing runs twice on one failed ensure => file over a
		// directory (I-3): the notice twice, then the failure.
		notice := "Not removing directory; use 'force' to override"
		return []ral.Change{{
			Property: "ensure", Was: "directory", Is: "file", Name: "file_created",
			PathNotices: []string{notice, notice},
			Err:         errors.New("Could not set 'file' on ensure: Is a directory"),
		}}, p.warn, nil
	case "pathwarnings":
		// file's backup over a directory without force (type/file.rb:803),
		// warned each time remove_existing runs, beside a successful change
		// and beside a failed one.
		warning := "Could not back up file of type directory"
		return []ral.Change{
			{Property: "ensure", Was: "directory", Is: "absent",
				Name: "file_removed", Message: "removed",
				PathWarnings: []string{warning},
				PathNotices:  []string{"Not removing directory; use 'force' to override"}},
			{Property: "content", Was: "{sha256}a", Is: "{sha256}b",
				PathWarnings: []string{warning, warning},
				Err:          errors.New("boom")},
		}, p.warn, nil
	case "propertynotices":
		// Exec's logged output (type/exec.rb sync): notices at the returns
		// property, logged during the sync that then records the event.
		return []ral.Change{{
			Property: "returns", Name: "executed_command", Was: "notrun", Is: []string{"0"},
			Message: "executed successfully", PathNotices: []string{"path notice"},
			PropertyNotices: []string{"a", "", "b"},
		}}, p.warn, nil
	case "propertynoticesfail":
		return []ral.Change{{
			Property: "returns", Name: "executed_command", Was: "notrun", Is: []string{"0"},
			PathNotices: []string{"path notice"}, PropertyNotices: []string{"a"},
			Err: errors.New("'x' returned 2 instead of one of [0]"),
		}}, p.warn, nil
	case "verbatim":
		// Group's members failure (M39): an empty current list and an
		// unquoted should, which only verbatim display can render.
		return []ral.Change{{
			Property: "members", Was: []any{}, Is: []any{"nosuchuser"},
			WasDisplay: "", IsDisplay: "nosuchuser", DisplayVerbatim: true,
			Err: errors.New("can't find user for nosuchuser"),
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

func (p recorder) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	*p.refreshed = append(*p.refreshed, r.Ref())
	if p.sourcesSeen != nil {
		*p.sourcesSeen = append(*p.sourcesSeen, sources.SourcesFrom(ctx))
	}
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
	return runCatalogCtx(t, context.Background(), tx, c)
}

func runCatalogCtx(t *testing.T, ctx context.Context, tx *Transaction, c *catalog.Catalog) *report.Report {
	t.Helper()
	g, err := BuildGraph(ctx, c, tx.Registry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	rep := report.New("n", "production", tx.Noop)
	if err := tx.Run(ctx, c, g, rep); err != nil {
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
	g, err := BuildGraph(context.Background(), c, tx.Registry)
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

// DisplayVerbatim renders WasDisplay and IsDisplay as given -- empty included,
// no quotes added -- while the event keeps the raw values (M39 Group[g15a]).
func TestDisplayVerbatimRendersTheFailureLineAsGiven(t *testing.T) {
	var applied, refreshed []string
	tx := &Transaction{Registry: ral.NewRegistry(recorder{typ: "group", applied: &applied, refreshed: &refreshed,
		failChange: map[string]string{"Group[g15a]": "verbatim"}, changes: 1})}

	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{{Type: "Group", Title: "g15a"}}})

	st := rep.ResourceStatuses["Group[g15a]"]
	if len(st.Events) != 1 {
		t.Fatalf("want 1 event, got %+v", st.Events)
	}
	ev := st.Events[0]
	want := "change from  to nosuchuser failed: can't find user for nosuchuser"
	if ev.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", ev.Message, want)
	}
	if !reflect.DeepEqual(ev.PreviousValue, []any{}) || !reflect.DeepEqual(ev.DesiredValue, []any{"nosuchuser"}) {
		t.Errorf("previous/desired = %#v / %#v, want the raw lists", ev.PreviousValue, ev.DesiredValue)
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

// The Puppet-sourced log precedes the resource/property one. Ruby's
// resource_harness.rb:149 Puppet.log_exception runs in the rescue, while the
// event's send_log runs later, in the ensure clause at :172. On the ensure
// path property.rb:496's log_exception fires earlier still, inside the sync.
// Measured on openvox 8.34.0: both "Error: Could not set ..." lines print
// before ".../ensure: change from ... failed: ...", and for a failed owner
// "Error: Could not find user ..." prints before ".../owner: change from".
func TestFailedPropertyLogsAtPuppetBeforeTheResource(t *testing.T) {
	for _, tc := range []struct {
		shape    string
		property string
	}{
		{"ensure", "ensure"},
		{"mixed", "owner"},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var applied, refreshed []string
			tx := txWithFailChange(&applied, &refreshed,
				map[string]string{"File[/f]": tc.shape})

			c := &catalog.Catalog{Resources: []catalog.Resource{
				{Type: "File", Title: "/f", File: "/etc/site.pp", Line: 6},
			}}
			rep := runCatalog(t, tx, c)

			var sources []string
			for _, l := range rep.Logs {
				if l.Level == "err" {
					sources = append(sources, l.Source)
				}
			}
			want := []string{"Puppet", "/File[/f]/" + tc.property}
			if !slices.Equal(sources, want) {
				t.Errorf("err log sources = %q, want %q", sources, want)
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
	if got := display(1000, "llm", false); got != "'llm'" {
		t.Errorf("display(1000, \"llm\") = %q, want %q", got, "'llm'")
	}
	if got := display(1000, "", false); got != "1000" {
		t.Errorf("display(1000, \"\") = %q, want %q -- no override means fmtValue", got, "1000")
	}
	if got := display("absent", "", false); got != "'absent'" {
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

// A type whose every resource was skipped for a failed prefetch has no
// metrics.time entry: their evaluation_time is null, and
// calculate_time_metrics (transaction/report.rb:499-503) adds only non-null
// ones. Measured by G5's sysctl-prefetch-load-error, where Ruby's report has
// no "sysctl" time and voxagent's had "Sysctl" 0.
func TestPrefetchFailedTypeHasNoTimeMetric(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New("unparsable")},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("Notify", "hi", nil),
	}})
	rep.Finalize()
	var names []any
	for _, v := range rep.Metrics["time"].Values {
		names = append(names, v[0])
	}
	if slices.Contains(names, any("file")) {
		t.Errorf("metrics.time names %v; a prefetch-failed type was never evaluated", names)
	}
	if !slices.Contains(names, any("notify")) {
		t.Errorf("metrics.time names %v; the applied notify must stay", names)
	}
}

// A type whose every resource was skipped for failed dependencies has no
// metrics.time entry either: transaction.rb skips it before evaluation, so its
// evaluation_time is null. Measured by G5's sak user-after-key, where Ruby's
// report has no "user" time and voxagent's had "User" 0.
func TestDependencySkippedTypeHasNoTimeMetric(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil),
		res("Notify", "n", map[string]any{"require": []any{"File[a]"}}),
	}})
	rep.Finalize()
	if rs := rep.ResourceStatuses["Notify[n]"]; !rs.Skipped {
		t.Fatalf("Notify[n] skipped = false; the test needs it skipped for File[a]")
	}
	var names []any
	for _, v := range rep.Metrics["time"].Values {
		names = append(names, v[0])
	}
	if slices.Contains(names, any("notify")) {
		t.Errorf("metrics.time names %v; a dependency-skipped type was never evaluated", names)
	}
	if !slices.Contains(names, any("file")) {
		t.Errorf("metrics.time names %v; the evaluated, failed file must stay", names)
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

// A relationship that names a defined type reaches the define's contents. If
// the instance were an ordinary vertex the subscribe would attach to a vertex
// that never changes, Exec[svc] would never refresh, and nothing would fail.
func TestDefinedTypeInstanceIsAContainer(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			{Type: "Wrap::It", Title: "w", Kind: "defined_type",
				Parameters: map[string]any{"m": "x"}},
			res("File", "z", nil),
			res("Exec", "svc", map[string]any{"subscribe": []any{"Wrap::It[w]"}}),
		},
		Edges: []catalog.Edge{{Source: "Wrap::It[w]", Target: "File[z]"}},
	}
	rep := runCatalog(t, tx, c)
	if _, ok := rep.ResourceStatuses["Wrap::It[w]"]; ok {
		t.Error("the defined-type instance has a report status; Puppet's report lists none")
	}
	for _, ref := range applied {
		if ref == "Wrap::It[w]" {
			t.Error("the defined-type instance was applied")
		}
	}
	if len(refreshed) != 1 || refreshed[0] != "Exec[svc]" {
		t.Errorf("refreshed = %v, want [Exec[svc]] (File[z] inside the define changed)", refreshed)
	}
}

// A resource started this run is not also refreshed by its notifiers
// (event_manager.rb:82). A failed start dequeues nothing, so its refresh
// still runs, and noop invalidates exactly as a live run does.
func TestInvalidateRefreshesDropsTheResourcesOwnRefresh(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		noop        bool
		wantRefresh bool
	}{
		{"started this run", "invalidate", false, false},
		{"started this run, noop", "invalidate", true, false},
		{"start failed", "invalidate-failed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied, refreshed []string
			tx := txWithFailChange(&applied, &refreshed, map[string]string{"Exec[svc]": tc.mode})
			tx.Noop = tc.noop
			c := &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "conf", map[string]any{"notify": []any{"Exec[svc]"}}),
				res("Exec", "svc", nil),
			}}
			rep := runCatalog(t, tx, c)
			if got := len(refreshed) == 1; got != tc.wantRefresh {
				t.Errorf("refreshed = %v, want refresh=%v", refreshed, tc.wantRefresh)
			}
			for _, ev := range rep.ResourceStatuses["Exec[svc]"].Events {
				if ev.Name != nil && *ev.Name == "refresh" && !tc.wantRefresh {
					t.Error("a refresh event was reported for a refresh that was invalidated")
				}
			}
		})
	}
}

// transaction.rb:305-322 and :418-424. Measured on Puppet 8.10.0 with a
// failing File and a chain of two dependents: the first logs "Dependency
// File[...] has failures: true" (notice) and "Skipping because of failed
// dependencies" (warning); the second logs only the warning, because
// @failed_dependencies_already_notified names each failed resource once per
// run.
func TestSkippedResourcesLogPuppetsDependencyLines(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil),
		res("File", "b", map[string]any{"require": []any{"File[a]"}}),
		res("File", "c", map[string]any{"require": []any{"File[b]"}}),
	}})
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[b]" || l.Source == "/File[c]" {
			got = append(got, l.Level+" "+l.Source+" "+l.Message)
		}
	}
	want := []string{
		"notice /File[b] Dependency File[a] has failures: true",
		"warning /File[b] Skipping because of failed dependencies",
		"warning /File[c] Skipping because of failed dependencies",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// propagate_failure (transaction.rb:333-350) collects every failed resource
// behind a dependent, so one blocked by two failures names both, each once,
// and a resource further down names neither again.
func TestASkippedResourceNamesEveryFailedDependencyOnce(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true, "File[b]": true}, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil),
		res("File", "b", nil),
		res("File", "c", map[string]any{"require": []any{"File[a]", "File[b]"}}),
		res("File", "d", map[string]any{"require": []any{"File[c]"}}),
	}})
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[c]" || l.Source == "/File[d]" {
			got = append(got, l.Level+" "+l.Source+" "+l.Message)
		}
	}
	want := []string{
		"notice /File[c] Dependency File[a] has failures: true",
		"notice /File[c] Dependency File[b] has failures: true",
		"warning /File[c] Skipping because of failed dependencies",
		"warning /File[d] Skipping because of failed dependencies",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A failed member blocks the class's
// dependent through the class's completed whit, which skips silently
// (transaction.rb:293-323, :401-424), so the dependent names the member.
func TestFailedMemberSkipsTheClassDependentWithTheMembersName(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"Exec[cf_fail]": true}, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Class", "cf", nil), res("Exec", "cf_fail", nil), res("Notify", "cf_ok", nil),
			res("Exec", "x_cf", map[string]any{"subscribe": "Class[cf]"}),
		},
		Edges: []catalog.Edge{{Source: "Class[cf]", Target: "Exec[cf_fail]"}, {Source: "Class[cf]", Target: "Notify[cf_ok]"}},
	})
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/Exec[x_cf]" {
			got = append(got, l.Level+" "+l.Message)
		}
		if strings.Contains(l.Source, "Whit") || strings.TrimPrefix(l.Source, "/") == "Class[Cf]" {
			t.Errorf("a whit logged: %s %s %s", l.Level, l.Source, l.Message)
		}
	}
	want := []string{"notice Dependency Exec[cf_fail] has failures: true", "warning Skipping because of failed dependencies"}
	if !slices.Equal(got, want) {
		t.Errorf("Exec[x_cf] logs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if slices.Contains(refreshed, "Exec[x_cf]") {
		t.Error("Exec[x_cf] was refreshed despite its failed dependency")
	}
}

// resource/status.rb:176 names the provider on every status it builds, so a
// File skipped for a failed dependency carries "posix" in Ruby's report.
func TestASkippedResourceNamesItsProvider(t *testing.T) {
	var applied, refreshed []string
	file := recorder{typ: "file", applied: &applied, refreshed: &refreshed,
		failOn: map[string]bool{"File[a]": true}, providerName: "posix", changes: 1}
	rep := runCatalog(t, &Transaction{Registry: ral.NewRegistry(file)}, &catalog.Catalog{
		Resources: []catalog.Resource{
			res("File", "a", nil),
			res("File", "b", map[string]any{"require": []any{"File[a]"}}),
		}})
	rs := rep.ResourceStatuses["File[b]"]
	if !rs.Skipped || rs.ProviderUsed == nil || *rs.ProviderUsed != "posix" {
		t.Errorf("File[b] skipped = %t provider_used = %v, want skipped with \"posix\"",
			rs.Skipped, rs.ProviderUsed)
	}
}

// The prefetch-failed branch picks the dependency reason the same way skip?
// does (transaction.rb:418-424 before :427), so it logs the same two lines.
func TestAPrefetchFailedDependentLogsTheDependencyLines(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{
		prefetchErr: map[string]error{"file": errors.New("cannot read the filesystem")},
	})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/head", nil),
		res("File", "/tail", map[string]any{"require": []any{"File[/head]"}}),
	}})
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[/tail]" {
			got = append(got, l.Level+" "+l.Message)
		}
	}
	want := []string{
		"notice Dependency File[/head] has failures: true",
		"warning Skipping because of failed dependencies",
	}
	if !slices.Equal(got, want) {
		t.Errorf("File[/tail] logs = %v, want %v", got, want)
	}
}

// genRecorder is a recorder that also implements ral.Generator: Generate
// returns the children configured for a parent, the way File.Generate returns
// the paths under a recursed directory.
type genRecorder struct {
	recorder
	children map[string][]catalog.Resource
	genErr   map[string]error
	genWarn  []ral.Warning
	// generated records "<parent> after <n> applies" per call, which pins WHEN
	// generation ran relative to the walk.
	generated *[]string
	// probes are refs asked of Generate's managed callback; answers records
	// "<ref> <answer>" for each.
	probes  []string
	answers *[]string
}

func (p genRecorder) Generate(_ context.Context, r catalog.Resource, managed func(string) bool) ([]catalog.Resource, []ral.Warning, error) {
	if p.answers != nil {
		for _, ref := range p.probes {
			*p.answers = append(*p.answers, fmt.Sprintf("%s %t", ref, managed(ref)))
		}
	}
	if p.generated != nil {
		*p.generated = append(*p.generated, fmt.Sprintf("%s after %d applies", r.Ref(), len(*p.applied)))
	}
	if err := p.genErr[r.Ref()]; err != nil {
		return nil, p.genWarn, err
	}
	return p.children[r.Ref()], p.genWarn, nil
}

// Prefetch must return the genRecorder rather than the embedded recorder: the
// promoted method would hand back the inner value, dropping Generate, and the
// walk would never see a generator (stubValueProvider in
// internal/ral/preflight_test.go is the same trap).
func (p genRecorder) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	inner, err := p.recorder.Prefetch(ctx, rs)
	if err != nil {
		return nil, err
	}
	p.recorder = inner.(recorder)
	return p, nil
}

// generatedChildren builds what a Generator returns for parent, in the order
// given.
func generatedChildren(parent catalog.Resource, titles ...string) []catalog.Resource {
	out := make([]catalog.Resource, len(titles))
	for i, title := range titles {
		out[i] = catalog.Resource{Type: parent.Type, Title: title, Tags: parent.Tags, GeneratedFrom: &parent}
	}
	return out
}

// txWithGenerator registers gen as the file provider, beside plain exec and
// notify recorders that share its probes and failure maps.
func txWithGenerator(gen genRecorder) *Transaction {
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: gen.applied, refreshed: gen.refreshed,
			failOn: gen.failOn, noChange: gen.noChange, changes: gen.changes}
	}
	return &Transaction{Registry: ral.NewRegistry(gen, mk("exec"), mk("notify"))}
}

// A unit is applied at the parent's turn: generation after every resource
// ordered before the parent, then the parent, then each child in bytewise
// title order whatever order Generate returned, and only then anything that
// depends on the parent.
func TestGeneratedChildrenApplyAsAUnitAtTheParentsTurn(t *testing.T) {
	var applied, refreshed, generated []string
	parent := res("File", "/d", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		children: map[string][]catalog.Resource{
			"File[/d]": generatedChildren(parent, "/d/b", "/d/a/x", "/d/a"),
		},
		generated: &generated,
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", nil),
		parent,
		res("Notify", "after", map[string]any{"require": []any{"File[/d]"}}),
	}})
	wantApplied := []string{"File[/c]", "File[/d]", "File[/d/a]", "File[/d/a/x]", "File[/d/b]", "Notify[after]"}
	if !slices.Equal(applied, wantApplied) {
		t.Errorf("applied = %v, want %v", applied, wantApplied)
	}
	wantGenerated := []string{"File[/c] after 0 applies", "File[/d] after 1 applies"}
	if !slices.Equal(generated, wantGenerated) {
		t.Errorf("generated = %v, want %v", generated, wantGenerated)
	}
	for _, ref := range []string{"File[/d/a]", "File[/d/a/x]", "File[/d/b]"} {
		if _, ok := rep.ResourceStatuses[ref]; !ok {
			t.Errorf("no status for the generated %s", ref)
		}
	}
}

// A generated child's status takes the parent's provenance (measured on
// Puppet 8.10.0): its containment path is the parent's with the last element
// replaced, its tags and provider are the parent's, and file and line are
// null.
func TestGeneratedChildStatusCarriesTheParentsProvenance(t *testing.T) {
	var applied, refreshed []string
	parent := catalog.Resource{Type: "File", Title: "/d", Tags: []string{"file", "class"},
		File: "/etc/site.pp", Line: 4}
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
			changes: 1, providerName: "posix"},
		children: map[string][]catalog.Resource{"File[/d]": generatedChildren(parent, "/d/a")},
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{
		Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil), parent},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: "File[/d]"},
		},
	})
	rs := rep.ResourceStatuses["File[/d/a]"]
	if want := []string{"Stage[main]", "Main", "File[/d/a]"}; !slices.Equal(rs.ContainmentPath, want) {
		t.Errorf("containment_path = %v, want %v", rs.ContainmentPath, want)
	}
	if !slices.Equal(rs.Tags, parent.Tags) || rs.ResourceType != "File" || rs.Title != "/d/a" {
		t.Errorf("tags/type/title = %v/%s/%s, want the parent's tags, File, /d/a",
			rs.Tags, rs.ResourceType, rs.Title)
	}
	if rs.ProviderUsed == nil || *rs.ProviderUsed != "posix" || !rs.Generated {
		t.Errorf("provider_used = %v generated = %t, want posix and true", rs.ProviderUsed, rs.Generated)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ResourceStatuses map[string]map[string]json.RawMessage `json:"resource_statuses"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	child, declared := doc.ResourceStatuses["File[/d/a]"], doc.ResourceStatuses["File[/d]"]
	if string(child["file"]) != "null" || string(child["line"]) != "null" {
		t.Errorf("child file/line = %s/%s, want null/null", child["file"], child["line"])
	}
	if string(declared["file"]) != `"/etc/site.pp"` || string(declared["line"]) != "4" {
		t.Errorf("parent file/line = %s/%s, want its manifest location", declared["file"], declared["line"])
	}
	sourced := false
	for _, l := range rep.Logs {
		sourced = sourced || l.Source == "/Stage[main]/Main/File[/d/a]/p"
	}
	if !sourced {
		t.Errorf("no log sourced at the child's containment path: %+v", rep.Logs)
	}
}

// A failed child skips its own descendants and the parent's dependents, and
// the notice names the child. Measured on Puppet 8.10.0 with an unwritable
// target: sub/b was skipped, and so were the parent's notify and require
// dependents, with "Dependency File[.../a] has failures: true". The notice
// goes to the first resource the failure blocks -- here the child's own
// descendant -- and the parent's dependent only warns.
func TestAFailedChildSkipsItsDescendantsAndTheParentsDependents(t *testing.T) {
	var applied, refreshed []string
	parent := res("File", "/d", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
			failOn: map[string]bool{"File[/d/a]": true}, changes: 1},
		children: map[string][]catalog.Resource{
			"File[/d]": generatedChildren(parent, "/d/a", "/d/a/x", "/d/b"),
		},
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		parent,
		res("Notify", "after", map[string]any{"require": []any{"File[/d]"}}),
	}})
	if want := []string{"File[/d]", "File[/d/a]", "File[/d/b]"}; !slices.Equal(applied, want) {
		t.Errorf("applied = %v, want %v", applied, want)
	}
	for _, ref := range []string{"File[/d/a/x]", "Notify[after]"} {
		if rs := rep.ResourceStatuses[ref]; !rs.Skipped || rs.Failed {
			t.Errorf("%s skipped = %t failed = %t, want skipped only", ref, rs.Skipped, rs.Failed)
		}
	}
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[/d/a/x]" || l.Source == "/Notify[after]" {
			got = append(got, l.Level+" "+l.Source+" "+l.Message)
		}
	}
	want := []string{
		"notice /File[/d/a/x] Dependency File[/d/a] has failures: true",
		"warning /File[/d/a/x] Skipping because of failed dependencies",
		"warning /Notify[after] Skipping because of failed dependencies",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// pre_process generates before the parent is evaluated, so a failed parent
// still generates and every child is reported skipped behind it.
func TestAFailedParentStillGeneratesAndSkipsEveryChild(t *testing.T) {
	var applied, refreshed, generated []string
	parent := res("File", "/d", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
			failOn: map[string]bool{"File[/d]": true}, changes: 1},
		children:  map[string][]catalog.Resource{"File[/d]": generatedChildren(parent, "/d/a", "/d/b")},
		generated: &generated,
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !slices.Equal(applied, []string{"File[/d]"}) || !slices.Equal(generated, []string{"File[/d] after 0 applies"}) {
		t.Errorf("applied = %v generated = %v, want the parent applied once after generating", applied, generated)
	}
	for _, ref := range []string{"File[/d/a]", "File[/d/b]"} {
		if rs := rep.ResourceStatuses[ref]; !rs.Skipped || !rs.Generated {
			t.Errorf("%s skipped = %t generated = %t, want both", ref, rs.Skipped, rs.Generated)
		}
	}
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[/d/a]" || l.Source == "/File[/d/b]" {
			got = append(got, l.Level+" "+l.Source+" "+l.Message)
		}
	}
	want := []string{
		"notice /File[/d/a] Dependency File[/d] has failures: true",
		"warning /File[/d/a] Skipping because of failed dependencies",
		"warning /File[/d/b] Skipping because of failed dependencies",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A parent skipped for its own failed dependency still generates, and its
// children inherit its reason; the failed resource was already named once.
func TestASkippedParentStillGeneratesAndSkipsEveryChild(t *testing.T) {
	var applied, refreshed, generated []string
	parent := res("File", "/d", map[string]any{"require": []any{"File[/c]"}})
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
			failOn: map[string]bool{"File[/c]": true}, changes: 1},
		children:  map[string][]catalog.Resource{"File[/d]": generatedChildren(parent, "/d/a")},
		generated: &generated,
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", nil), parent,
	}})
	if !slices.Equal(applied, []string{"File[/c]"}) || !slices.Contains(generated, "File[/d] after 1 applies") {
		t.Errorf("applied = %v generated = %v, want only File[/c] applied and File[/d] generated", applied, generated)
	}
	if !rep.ResourceStatuses["File[/d]"].Skipped || !rep.ResourceStatuses["File[/d/a]"].Skipped {
		t.Error("the parent and its child must both be skipped")
	}
	var got []string
	for _, l := range rep.Logs {
		if l.Source == "/File[/d/a]" {
			got = append(got, l.Level+" "+l.Message)
		}
	}
	if !slices.Equal(got, []string{"warning Skipping because of failed dependencies"}) {
		t.Errorf("child logs = %v, want only the warning", got)
	}
}

// Puppet's completed whit (additional_resource_generator.rb:86-117) gives a
// generating parent's subscribers exactly one event when any member of the
// unit produced one and none failed; with nothing generated there is no whit
// and the per-change count applies.
func TestAUnitSendsItsSubscribersOneRefreshEvent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		children    []string
		noChange    map[string]bool
		failOn      map[string]bool
		wantRefresh bool
		wantMessage string
	}{
		{"children changed, the parent did not", []string{"/d/a", "/d/b"},
			map[string]bool{"File[/d]": true, "Exec[svc]": true}, nil,
			true, "Triggered 'refresh' from 1 event"},
		{"only the parent changed", []string{"/d/a", "/d/b"},
			map[string]bool{"File[/d/a]": true, "File[/d/b]": true, "Exec[svc]": true}, nil,
			true, "Triggered 'refresh' from 1 event"},
		{"nothing generated: the per-change count", nil,
			map[string]bool{"Exec[svc]": true}, nil,
			true, "Triggered 'refresh' from 2 events"},
		{"nothing in the unit changed", []string{"/d/a"},
			map[string]bool{"File[/d]": true, "File[/d/a]": true, "Exec[svc]": true}, nil,
			false, ""},
		{"a child failed", []string{"/d/a", "/d/b"},
			map[string]bool{"Exec[svc]": true}, map[string]bool{"File[/d/b]": true},
			false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied, refreshed []string
			parent := res("File", "/d", nil)
			gen := genRecorder{
				recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
					noChange: tc.noChange, failOn: tc.failOn, changes: 2},
				children: map[string][]catalog.Resource{"File[/d]": generatedChildren(parent, tc.children...)},
			}
			rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
				parent,
				res("Exec", "svc", map[string]any{"subscribe": []any{"File[/d]"}}),
			}})
			if got := slices.Contains(refreshed, "Exec[svc]"); got != tc.wantRefresh {
				t.Fatalf("refreshed = %v, want Exec[svc] refreshed = %t", refreshed, tc.wantRefresh)
			}
			if !tc.wantRefresh {
				return
			}
			var messages []string
			for _, ev := range rep.ResourceStatuses["Exec[svc]"].Events {
				messages = append(messages, ev.Message)
			}
			if !slices.Equal(messages, []string{tc.wantMessage}) {
				t.Errorf("Exec[svc] events = %v, want [%s]", messages, tc.wantMessage)
			}
		})
	}
}

// additional_resource_generator.rb:66-70: a generation failure is logged
// against the parent, the parent still applies, and the report is failed
// while the exit code is not.
func TestAGenerationFailureIsLoggedAndTheParentStillApplies(t *testing.T) {
	var applied, refreshed []string
	parent := res("File", "/d", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		genErr:   map[string]error{"File[/d]": errors.New("walk refused")},
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !slices.Equal(applied, []string{"File[/d]"}) {
		t.Errorf("applied = %v, want the parent", applied)
	}
	found := false
	for _, l := range rep.Logs {
		found = found || (l.Level == "err" && l.Source == "/File[/d]" &&
			l.Message == "Failed to generate additional resources using 'eval_generate': walk refused")
	}
	if !found {
		t.Errorf("no generation-failure err log at the parent: %+v", rep.Logs)
	}
	if rep.Status != "failed" {
		t.Errorf("Status = %q, want failed", rep.Status)
	}
	if s := rep.RunStatus(); s.Failures != 0 || s.Changes != 1 {
		t.Errorf("RunStatus = %+v, want the exit code untouched: one change, no failures", s)
	}
}

// A Generator's warnings are logged like Apply's.
func TestGeneratorWarningsAreLogged(t *testing.T) {
	var applied, refreshed []string
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		genWarn:  []ral.Warning{{Source: "Puppet", Message: "soft limit"}},
	}
	rep := runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{res("File", "/d", nil)}})
	n := 0
	for _, l := range rep.Logs {
		if l.Level == "warning" && l.Source == "Puppet" && l.Message == "soft limit" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("found %d soft-limit warnings, want 1: %+v", n, rep.Logs)
	}
}

// type/file.rb:1040's notice belongs to the resource, not a property.
// Measured on Puppet 8.10.0 for a purged directory:
// ".../File[/tmp/pm/tgt/unm]: Not removing directory; use 'force' to override"
// beside ".../File[/tmp/pm/tgt/unm]/ensure: removed".
func TestAPathNoticeIsLoggedAtTheResource(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/d]": "pathnotice"})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{res("File", "/d", nil)}})
	var got []string
	for _, l := range rep.Logs {
		if strings.HasPrefix(l.Source, "/File[/d]") {
			got = append(got, l.Level+" "+l.Source+" "+l.Message)
		}
	}
	want := []string{
		"notice /File[/d] Not removing directory; use 'force' to override",
		"notice /File[/d]/ensure removed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs = %v, want %v", got, want)
	}
}

// File's path parameter munges its title to a clean path (type/file.rb:66-73,
// File.join(File.split(File.expand_path(value)))), and the recursion's
// exemption compares those paths (remove_less_specific_files, :648-673). So
// Generate's managed callback finds File['/d/keep/'] and File['/d//keep2']
// at their cleaned paths, and still finds an exact ref; a sibling is not
// managed. Measured on Puppet 8.10.0: a recurse+purge parent keeps the files
// under File['<dir>/keep/'].
func TestGenerateSeesCatalogFilesAtTheirCleanedPaths(t *testing.T) {
	var applied, refreshed, answers []string
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed},
		probes: []string{"File[/d/keep]", "File[/d/keep2]", "File[/d/keep/]", "File[/d/other]",
			"Notify[/d/n]"},
		answers: &answers,
	}
	runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/d", nil),
		res("File", "/d/keep/", nil),
		res("File", "/d//keep2", nil),
		res("Notify", "/d/n/", nil),
	}})
	want := []string{"File[/d/keep] true", "File[/d/keep2] true", "File[/d/keep/] true", "File[/d/other] false",
		"Notify[/d/n] false"}
	// Generate runs once per File: the first call's answers are enough.
	if len(answers) < len(want) || !slices.Equal(answers[:len(want)], want) {
		t.Errorf("managed answers = %v, want %v", answers, want)
	}
}

// Puppet capitalises every resource type when it builds the resource
// (resource.rb:583, munge_type_name :602-606), so a catalog File spelled
// "file" manages its path exactly as one spelled "File" does, and a recursing
// parent spelled "file" sees the catalog's Files. The exemption is keyed by
// the cleaned path, whatever the spelling of either type.
func TestGenerateSeesCatalogFilesWhateverTheTypeSpelling(t *testing.T) {
	var applied, refreshed, answers []string
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed},
		probes:   []string{"File[/d/low]", "file[/d/keep]", "file[/d/other]"},
		answers:  &answers,
	}
	runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/d", nil),
		res("file", "/d/low/", nil),
		res("File", "/d/keep", nil),
	}})
	want := []string{"File[/d/low] true", "file[/d/keep] true", "file[/d/other] false"}
	if len(answers) < len(want) || !slices.Equal(answers[:len(want)], want) {
		t.Errorf("managed answers = %v, want %v", answers, want)
	}
}

// A File managed under another title is exempt from a purge by its path
// parameter, not its title (remove_less_specific_files compares paths).
func TestGenerateSeesCatalogFilesByPathParameter(t *testing.T) {
	var applied, refreshed, answers []string
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed},
		probes:   []string{"File[/x/pd/keep]", "File[/x/pd/other]", "File[keep-me]"},
		answers:  &answers,
	}
	runCatalog(t, txWithGenerator(gen), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/x/pd", nil),
		res("File", "keep-me", map[string]any{"path": "/x/pd/keep"}),
	}})
	want := []string{"File[/x/pd/keep] true", "File[/x/pd/other] false", "File[keep-me] true"}
	if len(answers) < len(want) || !slices.Equal(answers[:len(want)], want) {
		t.Errorf("managed answers = %v, want %v", answers, want)
	}
}

// A backup's warning is logged at the resource path, ahead of the change's
// notices, on a successful change and on a failed one.
func TestPathWarningsAreLoggedAtTheResourceBeforeItsNotices(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/d]": "pathwarnings"})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{res("File", "/d", nil)}})
	var got []string
	for _, l := range rep.Logs {
		got = append(got, l.Level+" "+l.Source+" "+l.Message)
	}
	warning := "warning /File[/d] Could not back up file of type directory"
	want := []string{
		warning,
		"notice /File[/d] Not removing directory; use 'force' to override",
		"notice /File[/d]/ensure removed",
		warning, warning,
	}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("logs = %v;\nwant them to start %v", got, want)
	}
}

// remove_existing runs twice on one failed ensure => file over a directory, so
// the same notice is logged twice at the resource path, before the failure's
// own lines (measured, Puppet 8.10.0).
func TestPathNoticesAreLoggedInOrderBeforeAFailure(t *testing.T) {
	var applied, refreshed []string
	tx := txWithFailChange(&applied, &refreshed, map[string]string{"File[/d]": "twonotices"})
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{res("File", "/d", nil)}})
	var got []string
	for _, l := range rep.Logs {
		got = append(got, l.Level+" "+l.Source+" "+l.Message)
	}
	notice := "notice /File[/d] Not removing directory; use 'force' to override"
	if len(got) < 2 || got[0] != notice || got[1] != notice {
		t.Errorf("logs = %v; want the notice twice first", got)
	}
}

// classCatalog: Stage[main] holds Class[main] and each class in classes;
// members maps a container to its contents; rs are the resources in order.
func classCatalog(rs []catalog.Resource, members map[string][]string, classes ...string) *catalog.Catalog {
	c := &catalog.Catalog{Resources: append([]catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil)}, rs...)}
	c.Edges = []catalog.Edge{{Source: "Stage[main]", Target: "Class[main]"}}
	for _, cl := range classes {
		c.Edges = append(c.Edges, catalog.Edge{Source: "Stage[main]", Target: cl})
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, m := range members[k] {
			c.Edges = append(c.Edges, catalog.Edge{Source: k, Target: m})
		}
	}
	return c
}

// Three members change; Class[A] ~> Exec[x] refreshes x from ONE event.
func TestRefreshThroughAClassIsOneEvent(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	c := classCatalog([]catalog.Resource{
		res("Class", "A", nil), res("Notify", "a1", nil), res("Notify", "a2", nil), res("Notify", "a3", nil),
		res("Exec", "x", map[string]any{"subscribe": "Class[A]"})},
		map[string][]string{"Class[A]": {"Notify[a1]", "Notify[a2]", "Notify[a3]"}, "Class[main]": {"Exec[x]"}},
		"Class[A]")
	rep := runCatalog(t, tx, c)
	if logs := fmtLogs(rep); !strings.Contains(logs, "notice /Stage[main]/Main/Exec[x] Triggered 'refresh' from 1 event") {
		t.Errorf("logs:\n%s", logs)
	}
	for ref := range rep.ResourceStatuses {
		if strings.HasPrefix(ref, "Whit[") || strings.HasPrefix(ref, "Class[") {
			t.Errorf("status for %s; whits and containers have none", ref)
		}
	}
}

// Every container whose members would change
// logs its own line under --noop, with the raw count.
func TestNoopWhitsLogWouldHaveTriggered(t *testing.T) {
	var applied, refreshed []string
	// x_an is a refreshonly exec: it changes nothing itself and contributes
	// only its noop_restart, so Class[Main] collects 1 and Stage[main] 2.
	quiet := map[string]bool{"Exec[x_an]": true}
	mk := func(typ string) recorder {
		return recorder{typ: typ, applied: &applied, refreshed: &refreshed, changes: 1, noChange: quiet}
	}
	tx := &Transaction{Registry: ral.NewRegistry(mk("file"), mk("exec"), mk("notify")), Noop: true}
	c := classCatalog([]catalog.Resource{
		res("Class", "Allnoop", nil), res("Notify", "an1", nil), res("Notify", "an2", nil), res("Notify", "an3", nil),
		res("Exec", "x_an", map[string]any{"subscribe": "Class[Allnoop]"})},
		map[string][]string{"Class[Allnoop]": {"Notify[an1]", "Notify[an2]", "Notify[an3]"}, "Class[main]": {"Exec[x_an]"}},
		"Class[Allnoop]")
	logs := fmtLogs(runCatalog(t, tx, c))
	for _, want := range []string{
		"notice Class[Allnoop] Would have triggered 'refresh' from 3 events",
		"notice /Stage[main]/Main/Exec[x_an] Would have triggered 'refresh' from 1 event",
		"notice Class[Main] Would have triggered 'refresh' from 1 event",
		"notice Stage[main] Would have triggered 'refresh' from 2 events",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

// unrefreshable is a recorder whose type defines no refresh, as Puppet's
// File and Notify define none (ral.Refreshing).
type unrefreshable struct{ recorder }

func (unrefreshable) Refreshable(catalog.Resource) bool { return false }

// Prefetch keeps the wrapper: the walk applies through the primed provider.
func (u unrefreshable) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	p, err := u.recorder.Prefetch(ctx, rs)
	if err != nil {
		return nil, err
	}
	return unrefreshable{p.(recorder)}, nil
}

// classTx is the registry the class cases run under, shaped as Puppet's types
// are: File and Notify cannot be refreshed, Exec can. Every resource changes
// once (a File fileChanges times) except those in quiet, which change nothing.
func classTx(applied, refreshed *[]string, noop bool, quiet map[string]bool, fileChanges int) *Transaction {
	mk := func(typ string, changes int) recorder {
		return recorder{typ: typ, applied: applied, refreshed: refreshed, changes: changes, noChange: quiet}
	}
	return &Transaction{Noop: noop, Registry: ral.NewRegistry(
		unrefreshable{mk("file", fileChanges)}, unrefreshable{mk("notify", 1)}, mk("exec", 1))}
}

// Class refresh cases, each built as the measured manifest compiles: a refreshonly exec is a quiet Exec.
func TestClassRefreshCases(t *testing.T) {
	type row struct {
		name    string
		noop    bool
		quiet   []string
		files   int // changes per File; 1 when zero
		rs      []catalog.Resource
		members map[string][]string
		classes []string
		// tx, when set, replaces classTx.
		tx   func(applied, refreshed *[]string) *Transaction
		want []string
		// count is how often a line must appear, where once is not enough.
		count map[string]int
		// unrefreshed must not have been refreshed.
		unrefreshed []string
		check       func(t *testing.T, rep *report.Report, refreshed []string)
	}
	for _, tc := range []row{
		{
			// contain gives Inner_c a second parent; include
			// gives Inner_i only Stage[main].
			name:  "a contained class's changes reach its container's subscriber as one event",
			quiet: []string{"Exec[x_outer]", "Exec[x_inner_c]", "Exec[ii_ro]"},
			rs: []catalog.Resource{
				res("Class", "inner_c", nil), res("Notify", "ic1", nil), res("Notify", "ic2", nil),
				res("Class", "inner_i", nil), res("Exec", "ii_ro", nil),
				res("Class", "outer", nil), res("Notify", "o1", nil),
				res("Exec", "x_outer", map[string]any{"subscribe": "Class[outer]"}),
				res("Exec", "x_inner_c", map[string]any{"subscribe": "Class[inner_c]"}),
				res("Notify", "s", map[string]any{"notify": "Class[outer]"}),
			},
			members: map[string][]string{
				"Class[inner_c]": {"Notify[ic1]", "Notify[ic2]"},
				"Class[inner_i]": {"Exec[ii_ro]"},
				"Class[outer]":   {"Class[inner_c]", "Notify[o1]"},
				"Class[main]":    {"Exec[x_outer]", "Exec[x_inner_c]", "Notify[s]"},
			},
			classes: []string{"Class[inner_c]", "Class[inner_i]", "Class[outer]"},
			want: []string{
				"notice /Stage[main]/Main/Exec[x_outer] Triggered 'refresh' from 1 event",
				"notice /Stage[main]/Main/Exec[x_inner_c] Triggered 'refresh' from 1 event",
			},
			unrefreshed: []string{"Exec[ii_ro]"},
		},
		{
			name:  "an included class's changes do not reach the including class's subscriber",
			quiet: []string{"Notify[o_quiet]", "Exec[x_outer2]"},
			rs: []catalog.Resource{
				res("Class", "inner_i", nil), res("Notify", "ii1", nil),
				res("Class", "outer2", nil), res("Notify", "o_quiet", nil),
				res("Exec", "x_outer2", map[string]any{"subscribe": "Class[outer2]"}),
			},
			members: map[string][]string{
				"Class[inner_i]": {"Notify[ii1]"},
				"Class[outer2]":  {"Notify[o_quiet]"},
				"Class[main]":    {"Exec[x_outer2]"},
			},
			classes:     []string{"Class[inner_i]", "Class[outer2]"},
			unrefreshed: []string{"Exec[x_outer2]"},
		},
		{
			name:  "Class ~> Class refreshes the target's member from one event",
			quiet: []string{"Exec[tgt_ro]"},
			rs: []catalog.Resource{
				res("Class", "outer", map[string]any{"notify": "Class[tgt]"}), res("Notify", "o1", nil),
				res("Class", "tgt", nil), res("Exec", "tgt_ro", nil),
			},
			members: map[string][]string{"Class[outer]": {"Notify[o1]"}, "Class[tgt]": {"Exec[tgt_ro]"}},
			classes: []string{"Class[outer]", "Class[tgt]"},
			want:    []string{"notice /Stage[main]/Tgt/Exec[tgt_ro] Triggered 'refresh' from 1 event"},
		},
		{
			name:  "a defined type is a container both ways",
			quiet: []string{"Exec[x_dt]", "Exec[tdt_z_ro]"},
			rs: []catalog.Resource{
				{Type: "Dt", Title: "one", Kind: "defined_type"},
				res("Notify", "dt_one_a", nil), res("Notify", "dt_one_b", nil),
				res("Exec", "x_dt", map[string]any{"subscribe": "Dt[one]"}),
				{Type: "Tdt", Title: "z", Kind: "defined_type"}, res("Exec", "tdt_z_ro", nil),
				res("Notify", "s3", map[string]any{"notify": "Tdt[z]"}),
			},
			members: map[string][]string{
				"Dt[one]":     {"Notify[dt_one_a]", "Notify[dt_one_b]"},
				"Tdt[z]":      {"Exec[tdt_z_ro]"},
				"Class[main]": {"Dt[one]", "Exec[x_dt]", "Tdt[z]", "Notify[s3]"},
			},
			want: []string{
				"notice /Stage[main]/Main/Exec[x_dt] Triggered 'refresh' from 1 event",
				"notice /Stage[main]/Main/Tdt[z]/Exec[tdt_z_ro] Triggered 'refresh' from 1 event",
			},
		},
		{
			name: "a class of two noop members logs the raw count; its subscriber one", noop: true,
			quiet: []string{"Exec[x_nn]"},
			rs: []catalog.Resource{
				res("Class", "nn", nil), res("Notify", "nn1", nil), res("Notify", "nn2", nil),
				res("Exec", "x_nn", map[string]any{"subscribe": "Class[nn]"}),
			},
			members: map[string][]string{"Class[nn]": {"Notify[nn1]", "Notify[nn2]"}, "Class[main]": {"Exec[x_nn]"}},
			classes: []string{"Class[nn]"},
			want: []string{
				"notice Class[Nn] Would have triggered 'refresh' from 2 events",
				"notice /Stage[main]/Main/Exec[x_nn] Would have triggered 'refresh' from 1 event",
			},
			check: func(t *testing.T, rep *report.Report, _ []string) {
				// The status the report prunes as unchanged.
				if st := rep.ResourceStatuses["Exec[x_nn]"]; len(st.Events) > 0 || st.OutOfSync {
					t.Errorf("Exec[x_nn] = %+v; a noop refresh adds nothing to its status", st)
				}
			},
		},
		{
			// The admissible whit, then the completed one: the same text twice.
			name: "a noop source into a class logs at both of its whits", noop: true,
			quiet: []string{"Exec[tn_ro]"},
			rs: []catalog.Resource{
				res("Class", "tn", nil), res("Exec", "tn_ro", nil),
				res("Notify", "src_noop", map[string]any{"notify": "Class[tn]"}),
			},
			members: map[string][]string{"Class[tn]": {"Exec[tn_ro]"}, "Class[main]": {"Notify[src_noop]"}},
			classes: []string{"Class[tn]"},
			count: map[string]int{
				"notice Class[Tn] Would have triggered 'refresh' from 1 event":                   2,
				"notice /Stage[main]/Tn/Exec[tn_ro] Would have triggered 'refresh' from 1 event": 1,
			},
		},
		{
			name:  "one event per class edge plus every direct event",
			quiet: []string{"Exec[x_multi]"},
			rs: []catalog.Resource{
				res("Class", "ca", nil), res("Notify", "ca1", nil), res("Notify", "ca2", nil),
				res("Class", "cb", nil), res("Notify", "cb1", nil),
				res("Notify", "direct", nil),
				res("Exec", "x_multi", map[string]any{"subscribe": []any{"Class[ca]", "Class[cb]", "Notify[direct]"}}),
			},
			members: map[string][]string{
				"Class[ca]": {"Notify[ca1]", "Notify[ca2]"}, "Class[cb]": {"Notify[cb1]"},
				"Class[main]": {"Notify[direct]", "Exec[x_multi]"},
			},
			classes: []string{"Class[ca]", "Class[cb]"},
			want:    []string{"notice /Stage[main]/Main/Exec[x_multi] Triggered 'refresh' from 3 events"},
		},
		{
			name:  "a direct edge carries every event of its source",
			quiet: []string{"Exec[x_file]"}, files: 2,
			rs: []catalog.Resource{
				res("File", "/tmp/f2", nil),
				res("Exec", "x_file", map[string]any{"subscribe": "File[/tmp/f2]"}),
			},
			members: map[string][]string{"Class[main]": {"File[/tmp/f2]", "Exec[x_file]"}},
			want:    []string{"notice /Stage[main]/Main/Exec[x_file] Triggered 'refresh' from 2 events"},
		},
		{
			name:  "a member's direct edge and its class's edge add up",
			quiet: []string{"Exec[x_both]"},
			rs: []catalog.Resource{
				res("Class", "cd", nil),
				res("Notify", "cd1", map[string]any{"notify": "Exec[x_both]"}), res("Notify", "cd2", nil),
				res("Exec", "x_both", map[string]any{"subscribe": "Class[cd]"}),
			},
			members: map[string][]string{"Class[cd]": {"Notify[cd1]", "Notify[cd2]"}, "Class[main]": {"Exec[x_both]"}},
			classes: []string{"Class[cd]"},
			want:    []string{"notice /Stage[main]/Main/Exec[x_both] Triggered 'refresh' from 2 events"},
		},
		{
			// a_member_ro and x are refreshed; the whits, which
			// refreshed too, are not counted.
			name:  "restarted counts real resources only",
			quiet: []string{"Exec[a_member_ro]", "Exec[x]"},
			rs: []catalog.Resource{
				res("Class", "a", nil), res("Notify", "a1", nil), res("Notify", "a2", nil), res("Notify", "a3", nil),
				res("Exec", "a_member_ro", map[string]any{"subscribe": "Notify[a1]"}),
				res("Exec", "x", map[string]any{"subscribe": "Class[a]"}),
			},
			members: map[string][]string{
				"Class[a]":    {"Notify[a1]", "Notify[a2]", "Notify[a3]", "Exec[a_member_ro]"},
				"Class[main]": {"Exec[x]"},
			},
			classes: []string{"Class[a]"},
			want:    []string{"notice /Stage[main]/Main/Exec[x] Triggered 'refresh' from 1 event"},
			check: func(t *testing.T, rep *report.Report, refreshed []string) {
				found := false
				for _, v := range rep.Metrics["resources"].Values {
					if v[0] == "restarted" {
						found = true
						if v[2] != 2 {
							t.Errorf("restarted = %v, want 2 (%v)", v[2], refreshed)
						}
					}
				}
				if !found {
					t.Errorf("resources metric has no restarted entry: %v", rep.Metrics["resources"].Values)
				}
			},
		},
		{
			// A case cut to
			// two resources: a changes one property and f two, each
			// self-refreshes, so Class[Main] collects 1+1 and 2+1.
			name: "self-refresh noop_restarts reach Class[Main]", noop: true,
			tx: func(applied, refreshed *[]string) *Transaction {
				noopNamed := func(name string) report.Event {
					ev := hostNamed(name, name+" (noop)")
					ev.Status = "noop"
					return ev
				}
				var calls []string
				stub := evaluatorStub{typ: "vox_selfref", calls: &calls, refreshable: true, selfRefresh: true,
					evals: map[string]ral.Evaluation{
						"Vox_selfref[a]": {Events: []report.Event{noopNamed("value_changed")}},
						"Vox_selfref[f]": {Events: []report.Event{noopNamed("value_changed"), noopNamed("other_changed")}},
					}}
				tx := evalTx(stub, applied, refreshed)
				tx.Noop = true
				return tx
			},
			rs:      []catalog.Resource{res("Vox_selfref", "a", nil), res("Vox_selfref", "f", nil)},
			members: map[string][]string{"Class[main]": {"Vox_selfref[a]", "Vox_selfref[f]"}},
			want: []string{
				"notice /Stage[main]/Main/Vox_selfref[a] Would have triggered 'refresh' from 1 event",
				"notice /Stage[main]/Main/Vox_selfref[f] Would have triggered 'refresh' from 2 events",
				"notice Class[Main] Would have triggered 'refresh' from 5 events",
				"notice Stage[main] Would have triggered 'refresh' from 1 event",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied, refreshed []string
			var tx *Transaction
			if tc.tx != nil {
				tx = tc.tx(&applied, &refreshed)
			} else {
				quiet := map[string]bool{}
				for _, q := range tc.quiet {
					quiet[q] = true
				}
				files := tc.files
				if files == 0 {
					files = 1
				}
				tx = classTx(&applied, &refreshed, tc.noop, quiet, files)
			}
			rep := runCatalog(t, tx, classCatalog(tc.rs, tc.members, tc.classes...))
			logs := fmtLogs(rep)
			for _, want := range tc.want {
				if !strings.Contains(logs, want+"\n") {
					t.Errorf("logs lack %q:\n%s", want, logs)
				}
			}
			for line, n := range tc.count {
				if got := strings.Count(logs, line+"\n"); got != n {
					t.Errorf("%q logged %d times, want %d:\n%s", line, got, n, logs)
				}
			}
			for _, ref := range tc.unrefreshed {
				if slices.Contains(refreshed, ref) {
					t.Errorf("%s was refreshed: %v", ref, refreshed)
				}
			}
			if tc.check != nil {
				tc.check(t, rep, refreshed)
			}
		})
	}
}

// Two sources notify Class[out]. out contains
// in_c, so admissible_Class[Out] passes one event to admissible_Class[In_c]
// and on to in_c_ro, while in_c's second parent, Stage[main], sends nothing;
// in_i is only included, so out's refresh never reaches it.
func TestRefreshIntoAContainedClassIsOneEvent(t *testing.T) {
	var applied, refreshed []string
	quiet := map[string]bool{"Exec[in_c_ro]": true, "Exec[in_i_ro]": true, "Exec[out_ro]": true}
	tx := classTx(&applied, &refreshed, false, quiet, 1)
	c := classCatalog([]catalog.Resource{
		res("Class", "in_c", nil), res("Exec", "in_c_ro", nil),
		res("Class", "in_i", nil), res("Exec", "in_i_ro", nil),
		res("Class", "out", nil), res("Exec", "out_ro", nil),
		res("Notify", "s1", map[string]any{"notify": "Class[out]"}),
		res("Notify", "s2", map[string]any{"notify": "Class[out]"}),
	}, map[string][]string{
		"Class[in_c]": {"Exec[in_c_ro]"},
		"Class[in_i]": {"Exec[in_i_ro]"},
		"Class[out]":  {"Class[in_c]", "Exec[out_ro]"},
		"Class[main]": {"Notify[s1]", "Notify[s2]"},
	}, "Class[in_c]", "Class[in_i]", "Class[out]")
	logs := fmtLogs(runCatalog(t, tx, c))
	for _, want := range []string{
		"notice /Stage[main]/In_c/Exec[in_c_ro] Triggered 'refresh' from 1 event\n",
		"notice /Stage[main]/Out/Exec[out_ro] Triggered 'refresh' from 1 event\n",
	} {
		if n := strings.Count(logs, want); n != 1 {
			t.Errorf("%q logged %d times, want once:\n%s", want, n, logs)
		}
	}
	if slices.Contains(refreshed, "Exec[in_i_ro]") {
		t.Errorf("Exec[in_i_ro] was refreshed; an included class is not under out: %v", refreshed)
	}
}

// Retires apply-spine.md D-41: event_manager.rb:73 queues nothing for a
// target whose type has no refresh, so a noop run logs no "Would have
// triggered" for it.
func TestNoopDoesNotScheduleANonRefreshableNative(t *testing.T) {
	var applied, refreshed []string
	tx := classTx(&applied, &refreshed, true, nil, 1)
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", map[string]any{"notify": "File[b]"}),
		res("File", "b", nil),
	}})
	for _, l := range rep.Logs {
		if strings.HasPrefix(l.Message, "Would have triggered") {
			t.Errorf("logged %q at %s; File[b] cannot be refreshed", l.Message, l.Source)
		}
	}
	if len(refreshed) != 0 {
		t.Errorf("refreshed = %v, want none", refreshed)
	}
}

// A b_plain exec in a class, through the real Exec: its unless blocks the command, yet
// the refresh admissible_Class[B] sends it is reported as one that happened.
func TestGuardBlockedExecInAClassIsTriggered(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	var applied, refreshed []string
	tx := &Transaction{Registry: ral.NewRegistry(
		unrefreshable{recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1}}, exec.Exec{})}
	c := classCatalog([]catalog.Resource{
		res("Class", "b", nil),
		res("Exec", "b_plain", map[string]any{"command": "/usr/bin/touch " + marker, "unless": "/bin/true"}),
		res("Notify", "src1", map[string]any{"notify": "Class[b]"}),
	}, map[string][]string{"Class[b]": {"Exec[b_plain]"}, "Class[main]": {"Notify[src1]"}}, "Class[b]")
	rep := runCatalog(t, tx, c)
	if logs := fmtLogs(rep); !strings.Contains(logs, "notice /Stage[main]/B/Exec[b_plain] Triggered 'refresh' from 1 event\n") {
		t.Errorf("logs:\n%s", logs)
	}
	st := rep.ResourceStatuses["Exec[b_plain]"]
	if len(st.Events) != 1 || st.Events[0].Name == nil || *st.Events[0].Name != "refresh" || !st.Restarted {
		t.Errorf("Exec[b_plain] = %+v, want one refresh event and restarted", st)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran though unless blocked it")
	}
}

// Logged Exec output precedes the event's own line, on success and on
// failure, at <source>/<property>, after the resource-level notices.
func TestPropertyNoticesPrecedeTheChangeLine(t *testing.T) {
	for _, tc := range []struct {
		shape string
		want  []string
	}{
		{"propertynotices", []string{
			"notice /Exec[e] path notice",
			"notice /Exec[e]/returns a",
			"notice /Exec[e]/returns ",
			"notice /Exec[e]/returns b",
			"notice /Exec[e]/returns executed successfully",
		}},
		{"propertynoticesfail", []string{
			"notice /Exec[e] path notice",
			"notice /Exec[e]/returns a",
			"err Puppet 'x' returned 2 instead of one of [0]",
			"err /Exec[e]/returns change from 'notrun' to ['0'] failed: 'x' returned 2 instead of one of [0]",
		}},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			var applied, refreshed []string
			tx := txWithFailChange(&applied, &refreshed, map[string]string{"Exec[e]": tc.shape})
			rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{res("Exec", "e", nil)}})
			var got []string
			for _, l := range rep.Logs {
				got = append(got, l.Level+" "+l.Source+" "+l.Message)
			}
			if len(got) < len(tc.want) || !slices.Equal(got[:len(tc.want)], tc.want) {
				t.Errorf("logs = %q;\nwant them to start %q", got, tc.want)
			}
		})
	}
}

// refreshLogger is a native provider whose refresh reports its own lines, as
// Exec's does (ral.RefreshLogging).
type refreshLogger struct {
	recorder
	err error
}

// Prefetch keeps the wrapper: the recorder's would return the bare recorder.
func (p refreshLogger) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

func (p refreshLogger) RefreshLogged(_ context.Context, r catalog.Resource, _ bool) (ral.RefreshOutcome, error) {
	*p.refreshed = append(*p.refreshed, r.Ref())
	out := ral.RefreshOutcome{
		PropertyNotices: []ral.PropertyNotice{{Property: "returns", Message: "refreshed"}},
		Logs:            []report.LogEntry{{Level: "notice", Source: "/Exec[svc]", Message: "a callback line"}},
	}
	if p.err != nil {
		return out, p.err
	}
	out.Refreshed = true
	return out, nil
}

// A native RefreshLogging provider's lines are logged before the walk's
// Triggered 'refresh' or Failed to call refresh line. A failure is followed by
// resource.log_exception's line at the resource path, carrying the detail
// alone (event_manager.rb:160-165; 7-exec-output/report.json logs 26-27).
func TestNativeRefreshLogsBeforeTriggered(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		last []string
	}{
		{"success", nil, []string{"notice /Exec[svc] Triggered 'refresh' from 1 event"}},
		{"failure", errors.New("'x' returned 4 instead of one of [0]"), []string{
			"err /Exec[svc] Failed to call refresh: 'x' returned 4 instead of one of [0]",
			"err /Exec[svc] 'x' returned 4 instead of one of [0]",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied, refreshed []string
			rec := func(typ string) recorder {
				return recorder{typ: typ, applied: &applied, refreshed: &refreshed, changes: 1,
					noChange: map[string]bool{"Exec[svc]": true}}
			}
			tx := &Transaction{Registry: ral.NewRegistry(rec("file"), refreshLogger{recorder: rec("exec"), err: tc.err})}
			rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "/f", nil),
				res("Exec", "svc", map[string]any{"subscribe": []any{"File[/f]"}}),
			}})
			var got []string
			for _, l := range rep.Logs {
				if strings.HasPrefix(l.Source, "/Exec[svc]") {
					got = append(got, l.Level+" "+l.Source+" "+l.Message)
				}
			}
			want := append([]string{
				"notice /Exec[svc]/returns refreshed",
				"notice /Exec[svc] a callback line",
			}, tc.last...)
			if !slices.Equal(got, want) {
				t.Errorf("logs = %q;\nwant %q", got, want)
			}
		})
	}
}

// inMain is a catalog with the resources rs inside Stage[main] and Class[main],
// the shape every compiled catalog has.
func inMain(rs ...catalog.Resource) *catalog.Catalog {
	c := &catalog.Catalog{Resources: append([]catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil)}, rs...),
		Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[main]"}}}
	for _, r := range rs {
		c.Edges = append(c.Edges, catalog.Edge{Source: "Class[main]", Target: r.Ref()})
	}
	return c
}

// transaction.rb:282: a resource skipped for a failed dependency logs at its
// own path, and so does each container whose completed whit inherits the
// failure (Ruby names a whit by its container).
func TestSkippedResourceAndContainersLogRubysDebugLine(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, inMain(res("File", "a", nil),
		res("Exec", "b", map[string]any{"require": "File[a]"})))
	lines := debugLines(*got)
	for _, source := range []string{"/Stage[main]/Main/Exec[b]", "Class[Main]", "Stage[main]"} {
		if want := source + ": Resource is being skipped, unscheduling all events"; !slices.Contains(lines, want) {
			t.Errorf("lines = %q, missing %q", lines, want)
		}
	}
	if slices.Contains(lines, "/Stage[main]/Main/File[a]: Resource is being skipped, unscheduling all events") {
		t.Errorf("lines = %q: File[a] failed, it was not skipped", lines)
	}
}

// A type whose prefetch failed is skipped too (transaction.rb:427 falls
// through to :282).
func TestPrefetchFailedResourceLogsRubysSkipLine(t *testing.T) {
	var applied, refreshed []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetchErr: map[string]error{"file": errors.New("no")}})
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, inMain(res("File", "a", nil)))
	want := "/Stage[main]/Main/File[a]: Resource is being skipped, unscheduling all events"
	if lines := debugLines(*got); !slices.Contains(lines, want) {
		t.Errorf("lines = %q, missing %q", lines, want)
	}
}

// event_manager.rb:107: a resource that changed tells its container's
// completed whit to propagate the event; so does the whit for its own parent.
func TestChangedResourceLogsThatItsContainerPropagatesTheRefresh(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, inMain(res("Notify", "n", nil)))
	lines := debugLines(*got)
	for _, want := range []string{
		"/Stage[main]/Main/Notify[n]: The container Class[Main] will propagate my refresh event",
		"Class[Main]: The container Stage[main] will propagate my refresh event",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("lines = %q, missing %q", lines, want)
		}
	}
}

// A resource that refreshed (its event is the restart) propagates too.
func TestRefreshedResourceLogsThatItsContainerPropagatesTheRefresh(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	tx.Registry = ral.NewRegistry(
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1},
		recorder{typ: "exec", applied: &applied, refreshed: &refreshed, noChange: map[string]bool{"Exec[b]": true}})
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, inMain(res("Notify", "a", map[string]any{"notify": "Exec[b]"}), res("Exec", "b", nil)))
	want := "/Stage[main]/Main/Exec[b]: The container Class[Main] will propagate my refresh event"
	if lines := debugLines(*got); !slices.Contains(lines, want) {
		t.Errorf("lines = %q, missing %q", lines, want)
	}
}

// A resource that changed nothing queues no event, so nothing propagates.
func TestUnchangedResourceLogsNoPropagation(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 0)
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, inMain(res("Notify", "n", nil)))
	for _, line := range debugLines(*got) {
		if strings.Contains(line, "will propagate") {
			t.Errorf("line %q: nothing changed", line)
		}
	}
}

func TestTransactionLogsNothingBelowDebug(t *testing.T) {
	var applied, refreshed []string
	for _, level := range []string{"notice", "info"} {
		tx := newTx(&applied, &refreshed, map[string]bool{"File[a]": true}, nil, 1)
		ctx, got := logContext(level)
		runCatalogCtx(t, ctx, tx, inMain(res("File", "a", nil), res("Exec", "b", map[string]any{"require": "File[a]"}),
			res("Notify", "n", nil)))
		if len(*got) != 0 {
			t.Errorf("level %s: entries = %+v, want none", level, *got)
		}
	}
}

// The walk puts the resource's three names on the context before each call
// into its provider, so a provider never rebuilds them.
func TestWalkPutsSourcesOnTheProviderContext(t *testing.T) {
	var applied, refreshed []string
	var seen []sources.Sources
	mk := func(typ, name string) recorder {
		return recorder{typ: typ, applied: &applied, refreshed: &refreshed, providerName: name,
			changes: 1, sourcesSeen: &seen}
	}
	tx := &Transaction{Registry: ral.NewRegistry(mk("exec", "posix"), mk("notify", ""))}
	runCatalog(t, tx, inMain(res("Notify", "n", map[string]any{"notify": "Exec[x]"}), res("Exec", "x", nil)))
	want := []sources.Sources{
		{Resource: "/Stage[main]/Main/Notify[n]", Provider: "Notify[n]", Ref: "Notify[n]"},
		{Resource: "/Stage[main]/Main/Exec[x]", Provider: "Exec[x](provider=posix)", Ref: "Exec[x]"},
		{Resource: "/Stage[main]/Main/Exec[x]", Provider: "Exec[x](provider=posix)", Ref: "Exec[x]"},
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("sources = %+v, want %+v (Notify apply, Exec apply, Exec refresh)", seen, want)
	}
}

// event_manager.rb:107 for a recursed unit (6-failure-and-recurse/debug.txt):
// each member, the parent included, says the unit's whit propagates its event,
// and the unit's whit says its container does. A notice logger sees none.
func TestGeneratedUnitLogsRubysPropagationLines(t *testing.T) {
	var applied, refreshed []string
	parent := res("File", "/d", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		children: map[string][]catalog.Resource{"File[/d]": generatedChildren(parent, "/d/a", "/d/b")},
	}
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, txWithGenerator(gen), inMain(parent))
	lines := debugLines(*got)
	for _, want := range []string{
		"/Stage[main]/Main/File[/d]: The container /d will propagate my refresh event",
		"/Stage[main]/Main/File[/d/a]: The container /d will propagate my refresh event",
		"/Stage[main]/Main/File[/d/b]: The container /d will propagate my refresh event",
		"/d: The container Class[Main] will propagate my refresh event",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("lines = %q, missing %q", lines, want)
		}
	}
	if slices.Contains(lines, "/Stage[main]/Main/File[/d]: The container Class[Main] will propagate my refresh event") {
		t.Errorf("lines = %q: the parent of a unit sends to the unit, not to its container", lines)
	}

	ctx, got = logContext("notice")
	runCatalogCtx(t, ctx, txWithGenerator(gen), inMain(parent))
	if len(*got) != 0 {
		t.Errorf("entries at notice = %+v, want none", *got)
	}
}

// Each native provider's prefetch is logged at debug: its type, how many
// resources it covers and how long it took. Sorted by type, as the prefetch is.
func TestPrefetchLogsTypeCountAndDuration(t *testing.T) {
	var applied, refreshed, prefetched []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetched: &prefetched})
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("Notify", "hi", nil), res("File", "/two", nil),
	}})
	var lines []string
	for _, e := range *got {
		if e.Source == "voxagent" && strings.HasPrefix(e.Message, "prefetch ") {
			lines = append(lines, e.Message)
		}
	}
	wants := []*regexp.Regexp{
		regexp.MustCompile(`^prefetch file: 2 resources in \d+(\.\d+)?m?s$`),
		regexp.MustCompile(`^prefetch notify: 1 resources in \d+(\.\d+)?m?s$`),
	}
	if len(lines) != len(wants) {
		t.Fatalf("prefetch lines = %q, want %d", lines, len(wants))
	}
	for i, re := range wants {
		if !re.MatchString(lines[i]) {
			t.Errorf("line %d = %q, want a match for %v", i, lines[i], re)
		}
	}
}

func TestPrefetchLogsNothingBelowDebug(t *testing.T) {
	var applied, refreshed, prefetched []string
	tx := txWithPrefetch(&applied, &refreshed, prefetchProbe{prefetched: &prefetched})
	ctx, got := logContext("info")
	runCatalogCtx(t, ctx, tx, &catalog.Catalog{Resources: []catalog.Resource{res("File", "/one", nil)}})
	for _, e := range *got {
		if strings.HasPrefix(e.Message, "prefetch ") {
			t.Errorf("prefetch line at info: %+v", e)
		}
	}
}

// A type first met through a generated resource is prefetched mid-walk, and
// logged the same way.
func TestGeneratedChildPrefetchIsLogged(t *testing.T) {
	var f evalGenFixture
	parent := res("Vox_evalgen", "p", nil)
	stub := evalGenStub{gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {
		Children: []catalog.Resource{made(parent, "Notify", "c")}}}}
	ctx, got := logContext("debug")
	runCatalogCtx(t, ctx, f.tx(stub, nil), &catalog.Catalog{Resources: []catalog.Resource{parent}})
	re := regexp.MustCompile(`^prefetch notify: 1 resources in \d+(\.\d+)?m?s$`)
	n := 0
	for _, e := range *got {
		if re.MatchString(e.Message) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("matching prefetch lines = %d, want 1: %v", n, *got)
	}
}

// M1 and M20 as the console shows them: a timed-out or exit-127 main command
// logs its detail bare at Puppet and fails returns with it.
func TestExecTimeoutAndExit127Logs(t *testing.T) {
	tx := &Transaction{Registry: ral.NewRegistry(exec.Exec{})}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "slow", map[string]any{"command": "sleep 5", "timeout": 0.2}),
		res("Exec", "gone", map[string]any{"command": "echo gone >&2; exit 127"}),
		res("Exec", "empty", map[string]any{"command": "exit 127", "returns": []any{127.0}}),
	}})
	logs := fmtLogs(rep)
	for _, want := range []string{
		"err Puppet Command exceeded timeout\n",
		"err /Exec[slow]/returns change from 'notrun' to ['0'] failed: Command exceeded timeout\n",
		"err Puppet gone\n\n",
		"err /Exec[gone]/returns change from 'notrun' to ['0'] failed: gone\n\n",
		"err Puppet \n",
		"err /Exec[empty]/returns change from 'notrun' to ['127'] failed: \n",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
}

// Puppet.warn_once's set is the run's (util/logging.rb:181-201): two execs,
// one with a guard, that set E empty warn once per run, and the next run warns
// again.
func TestExecEmptyEnvironmentWarnsOncePerRun(t *testing.T) {
	tx := &Transaction{Registry: ral.NewRegistry(exec.Exec{})}
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "a", map[string]any{"command": "/bin/true", "onlyif": "/bin/true", "environment": []any{"E="}}),
		res("Exec", "b", map[string]any{"command": "/bin/true", "environment": []any{"E="}}),
	}}
	ctx, got := logContext("warning")
	for run := 1; run <= 2; run++ {
		runCatalogCtx(t, ctx, tx, c)
		n := 0
		for _, e := range *got {
			if strings.HasPrefix(e.Message, "Empty environment setting 'E'") {
				n++
			}
		}
		if n != run {
			t.Errorf("after run %d: %d empty-setting warnings, want %d", run, n, run)
		}
	}
}

// stagingBreaker is a recorder whose Apply for one resource fails as Exec's
// does when the agent's cwd could not be restored.
type stagingBreaker struct {
	recorder
	breakOn string
}

func (p stagingBreaker) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	if r.Ref() == p.breakOn {
		*p.applied = append(*p.applied, r.Ref())
		return nil, nil, fmt.Errorf("run '/bin/true': %w: permission denied", exec.ErrStagingBroken)
	}
	return p.recorder.Apply(ctx, r, noop)
}

func (p stagingBreaker) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

// §4.2 item 5: a run whose cwd could not be restored applies nothing more;
// Run returns the error, which the command reports as could_not_run.
func TestRunStopsOnStagingBroken(t *testing.T) {
	var applied, refreshed []string
	tx := &Transaction{Registry: ral.NewRegistry(stagingBreaker{
		recorder: recorder{typ: "exec", applied: &applied, refreshed: &refreshed, changes: 1},
		breakOn:  "Exec[a]",
	})}
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "a", map[string]any{"command": "/bin/true"}),
		res("Exec", "b", map[string]any{"command": "/bin/true", "require": []any{"Exec[a]"}}),
		res("Exec", "c", map[string]any{"command": "/bin/true"}),
	}}
	g, err := BuildGraph(context.Background(), c, tx.Registry)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.New("n", "production", false)
	err = tx.Run(context.Background(), c, g, rep)
	if !errors.Is(err, exec.ErrStagingBroken) {
		t.Fatalf("Run returned %v, want ErrStagingBroken", err)
	}
	if !strings.Contains(err.Error(), "Exec[a]") {
		t.Errorf("Run's error %q does not name the resource", err)
	}
	if i := slices.Index(applied, "Exec[a]"); i < 0 || len(applied) != i+1 {
		t.Errorf("applied %v; want nothing after Exec[a]", applied)
	}
}

// The same stop when the breakage comes from the refresh Exec[a] runs.
func TestRunStopsOnStagingBrokenRefresh(t *testing.T) {
	var applied, refreshed []string
	tx := &Transaction{Registry: ral.NewRegistry(
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1},
		refreshBreaker{recorder{typ: "exec", applied: &applied, refreshed: &refreshed, noChange: map[string]bool{"Exec[a]": true}}},
	)}
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "n", map[string]any{"notify": []any{"Exec[a]"}}),
		res("Exec", "a", map[string]any{"command": "/bin/true"}),
		res("Exec", "b", map[string]any{"command": "/bin/true", "require": []any{"Exec[a]"}}),
	}}
	g, err := BuildGraph(context.Background(), c, tx.Registry)
	if err != nil {
		t.Fatal(err)
	}
	err = tx.Run(context.Background(), c, g, report.New("n", "production", false))
	if !errors.Is(err, exec.ErrStagingBroken) {
		t.Fatalf("Run returned %v, want ErrStagingBroken", err)
	}
	if slices.Contains(applied, "Exec[b]") {
		t.Errorf("applied %v; Exec[b] came after the breakage", applied)
	}
}

type refreshBreaker struct{ recorder }

func (p refreshBreaker) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, fmt.Errorf("run '/bin/true': %w: permission denied", exec.ErrStagingBroken)
}

func (p refreshBreaker) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

// D-25 retired: under --noop a recursing File's unit logs the whit's notice at
// the parent's title, counting the parent's events and each child's.
func TestFileRecursionUnitNoopNotice(t *testing.T) {
	var applied, refreshed []string
	parent := res("File", "/tmp/rx/tgt", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed,
			noChange: map[string]bool{"File[/tmp/rx/tgt]": true}, changes: 1},
		children: map[string][]catalog.Resource{"File[/tmp/rx/tgt]": generatedChildren(parent,
			"/tmp/rx/tgt/a", "/tmp/rx/tgt/b", "/tmp/rx/tgt/c")},
	}
	tx := txWithGenerator(gen)
	tx.Noop = true
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !logged(rep, "notice", "/tmp/rx/tgt", "Would have triggered 'refresh' from 3 events") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
}

// The parent's own event counts too: a parent that changes plus three
// children that each change is four events (parentResult.sent).
func TestFileRecursionUnitNoopNoticeCountsParentEvent(t *testing.T) {
	var applied, refreshed []string
	parent := res("File", "/tmp/rx/tgt", nil)
	gen := genRecorder{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		children: map[string][]catalog.Resource{"File[/tmp/rx/tgt]": generatedChildren(parent,
			"/tmp/rx/tgt/a", "/tmp/rx/tgt/b", "/tmp/rx/tgt/c")},
	}
	tx := txWithGenerator(gen)
	tx.Noop = true
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{parent}})
	if !logged(rep, "notice", "/tmp/rx/tgt", "Would have triggered 'refresh' from 4 events") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
}

// endProbe is a provider holding per-run state: it counts EndRun calls through
// a pointer shared with every copy, as a primed provider shares its handle with
// the unprimed value that created it.
type endProbe struct {
	recorder
	ended       *int
	prefetchErr error
	// cancel, when set, is called by Apply to cancel the run mid-walk.
	cancel context.CancelFunc
}

func (p *endProbe) EndRun() { *p.ended++ }

func (p *endProbe) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	if p.prefetchErr != nil {
		return nil, p.prefetchErr
	}
	primed := *p
	return &primed, nil
}

func (p *endProbe) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	if p.cancel != nil {
		p.cancel()
	}
	return p.recorder.Apply(ctx, r, noop)
}

func newEndProbe(ended *int) *endProbe {
	var applied, refreshed []string
	return &endProbe{
		recorder: recorder{typ: "file", applied: &applied, refreshed: &refreshed, changes: 1},
		ended:    ended,
	}
}

func TestEndRunCalledOnceAfterWalk(t *testing.T) {
	var ended int
	tx := &Transaction{Registry: ral.NewRegistry(newEndProbe(&ended))}
	runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("File", "/two", nil),
	}})
	if ended != 1 {
		t.Errorf("EndRun called %d times, want 1", ended)
	}
}

func TestEndRunAfterPrefetchFailureAndCancel(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/one", nil), res("File", "/two", nil),
	}}

	var failedEnded int
	failing := newEndProbe(&failedEnded)
	failing.prefetchErr = errors.New("prefetch refused")
	runCatalog(t, &Transaction{Registry: ral.NewRegistry(failing)}, cat)
	if failedEnded != 1 {
		t.Errorf("after a failed prefetch EndRun called %d times, want 1", failedEnded)
	}

	var cancelledEnded int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling := newEndProbe(&cancelledEnded)
	cancelling.cancel = cancel
	runCatalogCtx(t, ctx, &Transaction{Registry: ral.NewRegistry(cancelling)}, cat)
	if cancelledEnded != 1 {
		t.Errorf("after a mid-walk cancel EndRun called %d times, want 1", cancelledEnded)
	}
}

func TestEndRunNotRequiredOfOtherProviders(t *testing.T) {
	var applied, refreshed []string
	tx := newTx(&applied, &refreshed, nil, nil, 1)
	runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{res("File", "/one", nil)}})
	if len(applied) != 1 {
		t.Errorf("applied = %v, want the one resource applied", applied)
	}
}

// flushFailing is a Sysctl-typed provider whose Apply made its change and then
// failed the flush, the shape augeasproviders produces when `sysctl -w` fails
// after the file edit (01e-sysctl-silent-real-sysctl). It stands in for a real
// provider because none returns ral.FlushError yet.
type flushFailing struct {
	applied   *[]string
	refreshed *[]string
}

func (p flushFailing) Type() string                                              { return "sysctl" }
func (p flushFailing) ProviderUsed() *string                                     { return nil }
func (p flushFailing) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (p flushFailing) Attrs() ral.Attrs                                          { return ral.NewAttrs("value") }

func (p flushFailing) Apply(_ context.Context, r catalog.Resource, _ bool) ([]ral.Change, []ral.Warning, error) {
	*p.applied = append(*p.applied, r.Ref())
	changes := []ral.Change{{
		Property: "value", Was: nil, Is: "1",
		Message: "changed configuration value from '' to '1' and live value from '0' to '1'",
	}}
	return changes, nil, &ral.FlushError{Err: errors.New(
		"Execution of '/usr/sbin/sysctl -w net.ipv4.ip_forward=1' returned 1: " +
			"sysctl: permission denied on key \"net.ipv4.ip_forward\"")}
}

func (p flushFailing) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

func (p flushFailing) Refresh(_ context.Context, r catalog.Resource, _ bool) (bool, error) {
	*p.refreshed = append(*p.refreshed, r.Ref())
	return true, nil
}

// flushCatalog is the 01e shape: a Sysctl whose flush fails, with a Notify that
// subscribes to it and a Notify that requires it.
func flushCatalog() (*Transaction, *[]string, *[]string, *catalog.Catalog) {
	var applied, refreshed []string
	tx := &Transaction{Registry: ral.NewRegistry(
		flushFailing{applied: &applied, refreshed: &refreshed},
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed, changes: 1})}
	const sysctl = "Sysctl[net.ipv4.ip_forward]"
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil), res("Class", "main", nil),
			res("Sysctl", "net.ipv4.ip_forward", nil),
			res("Notify", "subscriber", map[string]any{"subscribe": sysctl}),
			res("Notify", "dependent", map[string]any{"require": sysctl}),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: sysctl},
			{Source: "Class[main]", Target: "Notify[subscriber]"},
			{Source: "Class[main]", Target: "Notify[dependent]"},
		},
	}
	return tx, &applied, &refreshed, c
}

// resource_harness.rb:23-31: the change is recorded and logged as a success,
// then the flush's error becomes a resource_error. Measured in
// 01e-sysctl-silent-real-sysctl: changed and failed, change_count 1,
// out_of_sync_count 2.
func TestFlushErrorKeepsChangesThenFails(t *testing.T) {
	tx, _, _, c := flushCatalog()
	rep := runCatalog(t, tx, c)

	const path = "/Stage[main]/Main/Sysctl[net.ipv4.ip_forward]"
	const flushMsg = "Execution of '/usr/sbin/sysctl -w net.ipv4.ip_forward=1' returned 1: " +
		"sysctl: permission denied on key \"net.ipv4.ip_forward\""
	var got [][3]string
	for _, l := range rep.Logs {
		if strings.HasPrefix(l.Source, path) {
			got = append(got, [3]string{l.Level, l.Source, l.Message})
		}
	}
	want := [][3]string{
		{"notice", path + "/value", "changed configuration value from '' to '1' and live value from '0' to '1'"},
		{"err", path, "Could not evaluate: " + flushMsg},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("logs =\n%q\nwant\n%q", got, want)
	}

	st := rep.ResourceStatuses["Sysctl[net.ipv4.ip_forward]"]
	if !st.Failed || st.ChangeCount != 1 || st.OutOfSyncCount != 2 {
		t.Errorf("failed=%t change_count=%d out_of_sync_count=%d, want true 1 2",
			st.Failed, st.ChangeCount, st.OutOfSyncCount)
	}
	if len(st.Events) != 2 {
		t.Fatalf("want 2 events, got %+v", st.Events)
	}
	ok, fail := st.Events[0], st.Events[1]
	if ok.Property != "value" || *ok.Name != "value_changed" || ok.Status != "success" ||
		ok.PreviousValue != nil || ok.DesiredValue != "1" {
		t.Errorf("first event = %+v, want value/value_changed/success nil->\"1\"", ok)
	}
	if fail.Property != nil || *fail.Name != "resource_error" || fail.Status != "failure" ||
		fail.Message != flushMsg {
		t.Errorf("second event = %+v, want property-less resource_error failure %q", fail, flushMsg)
	}
}

// A failed resource sends no events (transaction.rb:269), so its subscriber is
// not refreshed even though the change was recorded.
func TestFlushErrorQueuesNoRefresh(t *testing.T) {
	tx, _, refreshed, c := flushCatalog()
	rep := runCatalog(t, tx, c)

	if slices.Contains(*refreshed, "Notify[subscriber]") {
		t.Errorf("Notify[subscriber] was refreshed by a failed resource; refreshed = %v", *refreshed)
	}
	for _, l := range rep.Logs {
		if strings.Contains(l.Message, "Scheduling refresh") || strings.Contains(l.Message, "Triggered 'refresh'") {
			t.Errorf("a refresh was logged: %+v", l)
		}
	}
}

func TestFlushErrorBlocksDependents(t *testing.T) {
	tx, applied, _, c := flushCatalog()
	rep := runCatalog(t, tx, c)

	if slices.Contains(*applied, "Notify[dependent]") {
		t.Errorf("Notify[dependent] was applied after a failed flush; applied = %v", *applied)
	}
	if st := rep.ResourceStatuses["Notify[dependent]"]; !st.Skipped {
		t.Errorf("Notify[dependent] not skipped: %+v", st)
	}
}

// turnProbe is a ral.TurnPrefetching provider of type vox_turn. Its Prefetch
// reads path and records what it saw, so a test can tell WHEN it ran from what
// the file held and how many applies had happened.
type turnProbe struct {
	path       string
	applied    *[]string
	rec        *turnRecord
	prefetchOK error
}

type turnRecord struct {
	calls         int
	content       string
	titles        []string
	appliedBefore int
}

func (p turnProbe) Type() string                                              { return "vox_turn" }
func (p turnProbe) ProviderUsed() *string                                     { s := "turn"; return &s }
func (p turnProbe) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (p turnProbe) Attrs() ral.Attrs                                          { return ral.NewAttrs("message") }
func (p turnProbe) PrefetchAtTurn()                                           {}
func (p turnProbe) Apply(_ context.Context, r catalog.Resource, _ bool) ([]ral.Change, []ral.Warning, error) {
	*p.applied = append(*p.applied, r.Ref())
	return nil, nil, nil
}
func (p turnProbe) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }
func (p turnProbe) Prefetch(_ context.Context, rs []catalog.Resource) (ral.Provider, error) {
	p.rec.calls++
	p.rec.appliedBefore = len(*p.applied)
	for _, r := range rs {
		p.rec.titles = append(p.rec.titles, r.Title)
	}
	b, _ := os.ReadFile(p.path)
	p.rec.content = string(b)
	if p.prefetchOK != nil {
		return nil, p.prefetchOK
	}
	return p, nil
}

// turnFile is a file provider whose Apply writes its content parameter to its
// title, so a later Prefetch can observe the write.
type turnFile struct {
	recorder
}

func (p turnFile) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	if err := os.WriteFile(r.Title, []byte(r.Parameters["content"].(string)), 0o600); err != nil {
		return nil, nil, err
	}
	return p.recorder.Apply(ctx, r, noop)
}

func (p turnFile) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	inner, err := p.recorder.Prefetch(ctx, rs)
	if err != nil {
		return nil, err
	}
	p.recorder = inner.(recorder)
	return p, nil
}

func turnTx(t *testing.T, probeErr error) (*Transaction, *turnRecord, *[]string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sysctl.conf")
	var applied, refreshed []string
	rec := &turnRecord{}
	mk := func(typ, name string) recorder {
		return recorder{typ: typ, applied: &applied, refreshed: &refreshed, providerName: name, changes: 1,
			failOn: map[string]bool{"Exec[bad]": true}}
	}
	return &Transaction{Registry: ral.NewRegistry(
		turnProbe{path: path, applied: &applied, rec: rec, prefetchOK: probeErr},
		turnFile{mk("file", "posix")}, mk("exec", "posix"))}, rec, &applied, path
}

// Puppet prefetches at the first traversed resource of the type
// (transaction.rb:110-115), so an earlier File's content is on disk by then.
func TestTurnPrefetchSeesEarlierResourcesChanges(t *testing.T) {
	tx, rec, _, path := turnTx(t, nil)
	runCatalog(t, tx, inMain(
		res("File", path, map[string]any{"content": "a = 1\n", "before": "Vox_turn[one]"}),
		res("Vox_turn", "one", nil), res("Vox_turn", "two", nil)))
	if rec.calls != 1 {
		t.Fatalf("Prefetch called %d times, want 1", rec.calls)
	}
	if rec.content != "a = 1\n" {
		t.Errorf("Prefetch saw %q, want the File's content", rec.content)
	}
	if !slices.Equal(rec.titles, []string{"one", "two"}) {
		t.Errorf("titles = %v, want catalog order", rec.titles)
	}
}

// pre_process prefetches for a resource it then skips (transaction.rb:111-121).
func TestTurnPrefetchRunsForSkippedResource(t *testing.T) {
	tx, rec, _, _ := turnTx(t, nil)
	var events []string
	l := runlog.New("debug", func(e report.LogEntry) { events = append(events, e.Source+": "+e.Message) }, nil)
	ctx := runlog.WithLogger(context.Background(), l)
	runCatalogCtx(t, ctx, tx, inMain(
		res("Exec", "bad", nil),
		res("Vox_turn", "a", map[string]any{"require": "Exec[bad]"}), res("Vox_turn", "b", nil)))
	if rec.calls != 1 {
		t.Fatalf("Prefetch called %d times, want 1", rec.calls)
	}
	skip := slices.Index(events, "/Stage[main]/Main/Vox_turn[a]: Resource is being skipped, unscheduling all events")
	fetch := slices.IndexFunc(events, func(e string) bool { return strings.HasPrefix(e, "voxagent: prefetch vox_turn") })
	if skip < 0 || fetch < 0 || fetch > skip {
		t.Errorf("prefetch line at %d, skip line at %d, want prefetch first; events = %q", fetch, skip, events)
	}
}

func TestTurnPrefetchFailureFailsEveryResource(t *testing.T) {
	tx, _, applied, _ := turnTx(t, errors.New("boom"))
	rep := runCatalog(t, tx, inMain(res("Vox_turn", "a", nil), res("Vox_turn", "b", nil)))
	var errs int
	for _, l := range rep.Logs {
		if l.Level == "err" && l.Source == "Puppet" && l.Message == "Could not prefetch vox_turn provider 'turn': boom" {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("cause logged %d times, want once; logs = %+v", errs, rep.Logs)
	}
	var failed int
	for _, s := range rep.ResourceStatuses {
		if s.ResourceType != "Vox_turn" {
			continue
		}
		failed++
		if len(s.Events) != 1 || s.Events[0].Message != "Prefetch failed for vox_turn provider 'turn'" {
			t.Errorf("%s events = %+v", s.Resource, s.Events)
		}
	}
	if failed != 2 || len(*applied) != 0 {
		t.Errorf("failed statuses = %d, applied = %v, want 2 and none", failed, *applied)
	}
}

// Nothing prefetches a turn type before the walk reaches its first resource.
func TestTurnPrefetchNotEager(t *testing.T) {
	tx, rec, _, _ := turnTx(t, nil)
	runCatalog(t, tx, inMain(
		res("Exec", "first", map[string]any{"before": "Vox_turn[a]"}), res("Vox_turn", "a", nil)))
	if rec.calls != 1 || rec.appliedBefore != 1 {
		t.Errorf("calls = %d, applies before prefetch = %d, want 1 and 1", rec.calls, rec.appliedBefore)
	}
}
