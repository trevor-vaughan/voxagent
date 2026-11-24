package transaction

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// evaluatorStub is a ral.Evaluator whose answers are set per ref, as the
// extension host's would be.
type evaluatorStub struct {
	typ         string
	evals       map[string]ral.Evaluation
	errs        map[string]error
	skips       map[string]ral.Evaluation
	skipErrs    map[string]error
	refreshable bool
	refreshErr  error
	selfRefresh bool
	// retitled maps a catalog ref to Puppet's own ref and title for it.
	retitled map[string][2]string
	// unsuitableLogs and unsuitableErr are what FailUnsuitable returns.
	unsuitableLogs map[string][]report.LogEntry
	unsuitableErr  error
	calls          *[]string
}

func (e evaluatorStub) Type() string                                              { return e.typ }
func (e evaluatorStub) ProviderUsed() *string                                     { return nil }
func (e evaluatorStub) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (e evaluatorStub) Attrs() ral.Attrs                                          { return ral.NewAttrs("value") }
func (e evaluatorStub) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return e, nil
}
func (e evaluatorStub) Apply(context.Context, catalog.Resource, bool) ([]ral.Change, []ral.Warning, error) {
	return nil, nil, errors.New("Apply must not be called on an Evaluator")
}
func (e evaluatorStub) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, errors.New("Refresh must not be called on an Evaluator")
}
func (e evaluatorStub) ProviderUsedFor(r catalog.Resource) *string {
	s := "prov-" + r.Title
	return &s
}
func (e evaluatorStub) Evaluate(_ context.Context, r catalog.Resource, noop bool) (ral.Evaluation, error) {
	*e.calls = append(*e.calls, "evaluate "+r.Ref())
	if err := e.errs[r.Ref()]; err != nil {
		return ral.Evaluation{}, err
	}
	return e.evals[r.Ref()], nil
}
func (e evaluatorStub) SkipEvaluation(_ context.Context, r catalog.Resource) (ral.Evaluation, error) {
	*e.calls = append(*e.calls, "skip "+r.Ref())
	if err := e.skipErrs[r.Ref()]; err != nil {
		return ral.Evaluation{}, err
	}
	return e.skips[r.Ref()], nil
}
func (e evaluatorStub) RefreshLogged(_ context.Context, r catalog.Resource, noop bool) (ral.RefreshOutcome, error) {
	*e.calls = append(*e.calls, "refresh "+r.Ref())
	if noop {
		return ral.RefreshOutcome{}, nil
	}
	logs := []report.LogEntry{{Level: "notice", Source: "/x", Message: "host refresh log"}}
	if e.refreshErr != nil {
		exception := []report.LogEntry{{Level: "err", Source: "/x", Message: "host exception log"}}
		return ral.RefreshOutcome{Logs: logs, ExceptionLogs: exception}, e.refreshErr
	}
	return ral.RefreshOutcome{Refreshed: true, Logs: logs}, nil
}
func (e evaluatorStub) FailUnsuitable(_ context.Context, r catalog.Resource) ([]report.LogEntry, error) {
	*e.calls = append(*e.calls, "fail_unsuitable "+r.Ref())
	if e.unsuitableErr != nil {
		return nil, e.unsuitableErr
	}
	return e.unsuitableLogs[r.Ref()], nil
}
func (e evaluatorStub) Refreshable(catalog.Resource) bool   { return e.refreshable }
func (e evaluatorStub) SelfRefreshes(catalog.Resource) bool { return e.selfRefresh }
func (e evaluatorStub) PuppetRef(r catalog.Resource) (ref, title string, ok bool) {
	named, ok := e.retitled[r.Ref()]
	return named[0], named[1], ok
}

// Unresolvable is false: retitled cannot say a title is unresolvable, and
// TestBuildGraphRefusesAnUnresolvableTitle covers one that is.
func (e evaluatorStub) Unresolvable(catalog.Resource) bool { return false }

func hostSuccess(msg string) report.Event {
	name, prop := "value_changed", "value"
	return report.Event{Property: prop, Message: msg, Name: &name, Status: "success", Time: "t"}
}

func hostNamed(name, msg string) report.Event {
	return report.Event{Message: msg, Name: &name, Status: "success", Time: "t"}
}

func hostFailure(msg string) report.Event {
	name := "resource_error"
	return report.Event{Message: msg, Name: &name, Status: "failure", Time: "t", Redacted: false}
}

func evalTx(stub evaluatorStub, applied, refreshed *[]string) *Transaction {
	return &Transaction{Registry: ral.NewRegistry(stub,
		recorder{typ: "file", applied: applied, refreshed: refreshed, changes: 1},
		recorder{typ: "exec", applied: applied, refreshed: refreshed, changes: 1})}
}

func TestEvaluatorEventsAndLogsAreRecordedVerbatim(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Events: []report.Event{hostSuccess("defined 'value' as 'one'")},
			Logs: []report.LogEntry{{Level: "notice", Source: "/Vox_value[a]/value", Message: "defined 'value' as 'one'", File: "/s.pp", Line: 3}}},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil)}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if len(st.Events) != 1 || st.Events[0].Message != "defined 'value' as 'one'" || st.ChangeCount != 1 || st.Failed {
		t.Errorf("status = %+v", st)
	}
	if st.ProviderUsed == nil || *st.ProviderUsed != "prov-a" {
		t.Errorf("provider_used = %v, want the per-resource answer", st.ProviderUsed)
	}
	var found bool
	for _, l := range rep.Logs {
		if l.Message == "defined 'value' as 'one'" {
			found = l.File == "/s.pp" && l.Line == 3
		}
	}
	if !found {
		t.Errorf("the host's log line is missing or lost its file/line: %+v", rep.Logs)
	}
	// The walk words no change line of its own for an Evaluator.
	if n := strings.Count(fmtLogs(rep), "defined 'value' as 'one'"); n != 1 {
		t.Errorf("the change was logged %d times", n)
	}
}

func fmtLogs(rep *report.Report) string {
	var b strings.Builder
	for _, l := range rep.Logs {
		b.WriteString(l.Level + " " + l.Source + " " + l.Message + "\n")
	}
	return b.String()
}

func TestEvaluatorFailureBlocksDependents(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Events: []report.Event{hostFailure("change from 'absent' to 'fail' failed: no")}, Failed: true},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil), res("File", "/t", map[string]any{"require": "Vox_value[a]"})}})
	if !rep.ResourceStatuses["Vox_value[a]"].Failed {
		t.Error("the evaluator's failure was not recorded")
	}
	if !rep.ResourceStatuses["File[/t]"].Skipped || len(applied) != 0 {
		t.Errorf("the native dependent was not skipped: applied=%v", applied)
	}
}

func TestEvaluatorErrorIsAResourceError(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls,
		errs: map[string]error{"Vox_value[a]": errors.New("extension host failed: boom")}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil)}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || len(st.Events) != 1 || st.Events[0].Message != "extension host failed: boom" ||
		!strings.Contains(fmtLogs(rep), "Could not evaluate: extension host failed: boom") {
		t.Errorf("status = %+v, logs:\n%s", st, fmtLogs(rep))
	}
}

func TestEvaluatorPrefetchSkipBlocksAndIsNotRefreshed(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, refreshable: true, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Events: []report.Event{hostFailure("Prefetch failed for vox_value provider 'bad'")}, Failed: true, Skipped: true},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", map[string]any{"notify": "Vox_value[a]"}),
		res("Vox_value", "a", nil), res("Exec", "e", map[string]any{"require": "Vox_value[a]"})}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || !st.Skipped || st.OutOfSyncCount != 1 {
		t.Errorf("status = %+v", st)
	}
	// Ruby never applies it (transaction.rb:280-284), so it has no span.
	if st.EvaluationTime != 0 {
		t.Errorf("evaluation_time = %v, want 0 for a resource that was never evaluated", st.EvaluationTime)
	}
	if slices.Contains(calls, "refresh Vox_value[a]") {
		t.Error("a resource skipped for its prefetch was refreshed")
	}
	if !rep.ResourceStatuses["Exec[e]"].Skipped {
		t.Error("the dependent of a prefetch-failed resource was applied")
	}
}

// relationship_graph.rb:116-137 never evaluates a resource with no suitable
// provider: the overly-deferred handler marks it failed (transaction.rb:124-136),
// so it is failed but not skipped, carries no event and no time, is not
// refreshed, and blocks its dependents.
func TestEvaluatorUnsuitableFailsWithoutEvaluation(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, refreshable: true, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Failed: true, Unsuitable: true},
	}, unsuitableLogs: map[string][]report.LogEntry{
		"Vox_value[a]": {{Level: "err", Source: "/a", Message: "Provider never is not functional on this host"}},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", map[string]any{"notify": "Vox_value[a]"}),
		res("Vox_value", "a", nil), res("Exec", "e", map[string]any{"require": "Vox_value[a]"})}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || st.Skipped || len(st.Events) != 0 || st.OutOfSync || st.OutOfSyncCount != 0 || st.EvaluationTime != 0 {
		t.Errorf("status = %+v", st)
	}
	if slices.Contains(calls, "refresh Vox_value[a]") {
		t.Error("a resource with no suitable provider was refreshed")
	}
	if !rep.ResourceStatuses["Exec[e]"].Skipped {
		t.Error("the dependent of an unsuitable resource was applied")
	}
	if !strings.Contains(fmtLogs(rep), "Provider never is not functional on this host") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
	rep.Finalize()
	for _, v := range rep.Metrics["time"].Values {
		if v[0] == "vox_value" {
			t.Errorf("metrics.time has %v; calculate_time_metrics skips a status with no evaluation_time", v)
		}
	}
}

// An unsuitable resource is set aside, offered again once the walk
// drains after progress, and failed only when a whole drain makes none, so it
// is answered after every resource independent of it and its dependent is
// skipped last (relationship_graph.rb:118-144, transaction.rb:124-136).
func TestRunDefersAnUnsuitableResourceToTheEnd(t *testing.T) {
	var calls, applied, refreshed []string
	unsuitable := "Provider never is not functional on this host"
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[nonesuch]": {Failed: true, Unsuitable: true},
	}, unsuitableLogs: map[string][]report.LogEntry{
		"Vox_value[nonesuch]": {{Level: "err", Source: "/Vox_value[nonesuch]", Message: unsuitable}},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", nil), res("Vox_value", "nonesuch", nil),
		res("File", "b", map[string]any{"require": "Vox_value[nonesuch]"}), res("File", "c", nil)}})
	if want := []string{"evaluate Vox_value[nonesuch]", "evaluate Vox_value[nonesuch]",
		"fail_unsuitable Vox_value[nonesuch]"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if want := []string{"File[a]", "File[c]"}; !slices.Equal(applied, want) {
		t.Errorf("applied = %q, want %q", applied, want)
	}
	st := rep.ResourceStatuses["Vox_value[nonesuch]"]
	if !st.Failed || st.Skipped || len(st.Events) != 0 || st.EvaluationTime != 0 {
		t.Errorf("Vox_value[nonesuch] status = %+v", st)
	}
	if !rep.ResourceStatuses["File[b]"].Skipped {
		t.Error("File[b] was not skipped for its unsuitable dependency")
	}
	fileC, unsuitableAt, skipAt := -1, -1, -1
	for i, l := range rep.Logs {
		switch {
		case fileC < 0 && strings.HasPrefix(l.Source, "/File[c]"):
			fileC = i
		case unsuitableAt < 0 && l.Message == unsuitable:
			unsuitableAt = i
		case skipAt < 0 && l.Source == "/File[b]" && l.Message == "Skipping because of failed dependencies":
			skipAt = i
		}
	}
	if fileC < 0 || unsuitableAt < fileC || skipAt < unsuitableAt {
		t.Errorf("want File[c]'s line, then the unsuitable line, then File[b]'s skip; logs:\n%s", fmtLogs(rep))
	}
}

// A host that cannot answer for an overly deferred resource leaves it a
// resource_error, still failed and blocking its dependents.
func TestFailUnsuitableHostErrorIsAResourceError(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Failed: true, Unsuitable: true},
	}, unsuitableErr: errors.New("extension host failed: gone")}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil), res("File", "/t", map[string]any{"require": "Vox_value[a]"})}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || st.Skipped || len(st.Events) != 1 || st.Events[0].Message != "extension host failed: gone" ||
		*st.Events[0].Name != "resource_error" || st.OutOfSyncCount != 1 {
		t.Errorf("status = %+v", st)
	}
	if !logged(rep, "err", "/Vox_value[a]", "Could not evaluate: extension host failed: gone") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
	if !rep.ResourceStatuses["File[/t]"].Skipped || len(applied) != 0 {
		t.Errorf("the dependent was not skipped: applied=%v", applied)
	}
}

// The deferral comes before the dependency check: an unsuitable resource whose
// dependency failed is still failed, not skipped, and Puppet logs no
// dependency lines for it.
func TestEvaluatorUnsuitableWithFailedDependencyIsFailedNotSkipped(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[x]": {Events: []report.Event{hostFailure("boom")}, Failed: true},
	}, skips: map[string]ral.Evaluation{"Vox_value[a]": {Failed: true, Unsuitable: true}}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "x", nil), res("Vox_value", "a", map[string]any{"require": "Vox_value[x]"})}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || st.Skipped || len(st.Events) != 0 {
		t.Errorf("status = %+v", st)
	}
	if logs := fmtLogs(rep); strings.Contains(logs, "Vox_value[a] Dependency") || strings.Contains(logs, "Vox_value[a] Skipping") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestEvaluatorRefreshIsLoggedAndTriggered(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, refreshable: true, evals: map[string]ral.Evaluation{}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", map[string]any{"notify": "Vox_value[a]"}), res("Vox_value", "a", nil)}})
	logs := fmtLogs(rep)
	if !strings.Contains(logs, "host refresh log") || !strings.Contains(logs, "Triggered 'refresh' from 1 event") {
		t.Errorf("logs:\n%s", logs)
	}
	if st := rep.ResourceStatuses["Vox_value[a]"]; !st.Restarted {
		t.Errorf("status = %+v, want restarted", st)
	}
}

func TestEvaluatorRefreshFailure(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, refreshable: true, evals: map[string]ral.Evaluation{},
		refreshErr: errors.New("refresh refused")}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", map[string]any{"notify": "Vox_value[a]"}), res("Vox_value", "a", nil)}})
	st := rep.ResourceStatuses["Vox_value[a]"]
	if !st.Failed || !st.FailedToRestart || !strings.Contains(fmtLogs(rep), "Failed to call refresh: refresh refused") {
		t.Errorf("status = %+v, logs:\n%s", st, fmtLogs(rep))
	}
	// event_manager.rb:160-165: the callback's own lines, then the err line,
	// then resource.log_exception's.
	var order []string
	for _, l := range rep.Logs {
		if strings.HasPrefix(l.Message, "host ") || strings.HasPrefix(l.Message, "Failed to call") {
			order = append(order, l.Message)
		}
	}
	want := []string{"host refresh log", "Failed to call refresh: refresh refused", "host exception log"}
	if !slices.Equal(order, want) {
		t.Errorf("log order = %q, want %q", order, want)
	}
}

// event_manager.rb:73 queues an event only for a target that responds to the
// callback, so a non-refreshable host type gets no refresh and, under noop,
// no "Would have triggered" line either.
func TestNonRefreshableEvaluatorGetsNoRefresh(t *testing.T) {
	for _, noop := range []bool{false, true} {
		var calls, applied, refreshed []string
		stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{}}
		tx := evalTx(stub, &applied, &refreshed)
		tx.Noop = noop
		rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
			res("File", "/c", map[string]any{"notify": "Vox_value[a]"}), res("Vox_value", "a", nil)}})
		if slices.Contains(calls, "refresh Vox_value[a]") || strings.Contains(fmtLogs(rep), "triggered") ||
			strings.Contains(fmtLogs(rep), "Triggered") {
			t.Errorf("noop=%t: calls=%v logs:\n%s", noop, calls, fmtLogs(rep))
		}
	}
}

func TestEvaluatorEventsPropagateRefresh(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Events: []report.Event{hostSuccess("m")}},
	}}
	runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", map[string]any{"notify": "Exec[e]"}), res("Exec", "e", nil)}})
	if !slices.Equal(refreshed, []string{"Exec[e]"}) {
		t.Errorf("refreshed = %v, want the native target refreshed by the host's event", refreshed)
	}
}

func TestSkippedEvaluatorRecordsItsPrefetchFailure(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{},
		skips: map[string]ral.Evaluation{"Vox_value[b]": {Events: []report.Event{hostFailure("Prefetch failed for vox_value provider 'bad'")}, Failed: true, Skipped: true}}}
	rec := recorder{typ: "exec", applied: &applied, refreshed: &refreshed, failOn: map[string]bool{"Exec[x]": true}}
	tx := &Transaction{Registry: ral.NewRegistry(stub, rec)}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "x", nil), res("Vox_value", "b", map[string]any{"require": "Exec[x]"})}})
	st := rep.ResourceStatuses["Vox_value[b]"]
	if !slices.Contains(calls, "skip Vox_value[b]") || !st.Skipped || !st.Failed || len(st.Events) != 1 {
		t.Errorf("calls=%v status=%+v", calls, st)
	}
	if !strings.Contains(fmtLogs(rep), "Skipping because of failed dependencies") {
		t.Errorf("logs:\n%s", fmtLogs(rep))
	}
}

func TestSkippedEvaluatorHostErrorIsAResourceError(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{},
		skipErrs: map[string]error{"Vox_value[b]": errors.New("extension host failed: gone")}}
	rec := recorder{typ: "exec", applied: &applied, refreshed: &refreshed, failOn: map[string]bool{"Exec[x]": true}}
	tx := &Transaction{Registry: ral.NewRegistry(stub, rec)}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "x", nil), res("Vox_value", "b", map[string]any{"require": "Exec[x]"})}})
	st := rep.ResourceStatuses["Vox_value[b]"]
	if !st.Skipped || !st.Failed || len(st.Events) != 1 || st.OutOfSyncCount != 1 ||
		st.Events[0].Message != "extension host failed: gone" || st.Events[0].Name == nil || *st.Events[0].Name != "resource_error" ||
		!strings.Contains(fmtLogs(rep), "Could not evaluate: extension host failed: gone") {
		t.Errorf("status = %+v, logs:\n%s", st, fmtLogs(rep))
	}
}

func TestEvaluatorInvalidateRefreshesDropsOwnPendingRefresh(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, refreshable: true, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Events: []report.Event{hostSuccess("m")}, InvalidateRefreshes: true},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/c", map[string]any{"notify": "Vox_value[a]"}), res("Vox_value", "a", nil)}})
	if slices.Contains(calls, "refresh Vox_value[a]") || rep.ResourceStatuses["Vox_value[a]"].Restarted {
		t.Errorf("a pending refresh survived invalidate_refreshes: calls=%v", calls)
	}
}

// A self-refreshing
// type queues one event per event name to itself (event_manager.rb:52-82),
// beside whatever other resources sent it, but not while it is deleting.
func TestSelfRefreshingResourceRefreshesOncePerEventName(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_selfref", calls: &calls, refreshable: true, selfRefresh: true,
		evals: map[string]ral.Evaluation{
			"Vox_selfref[a_change]": {Events: []report.Event{hostSuccess("value changed 'old' to 'new'")}},
			"Vox_selfref[f_twoprops]": {Events: []report.Event{hostSuccess("value changed 'old' to 'new'"),
				hostNamed("other_changed", "other changed 'old' to 'new'")}},
			"Vox_selfref[e_notified]": {Events: []report.Event{hostSuccess("value changed 'old' to 'new'")}},
			"Vox_selfref[d_absent]":   {Events: []report.Event{hostNamed("vox_selfref_removed", "removed")}},
			"Vox_selfref[c_nochange]": {},
		}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_selfref", "a_change", nil), res("Vox_selfref", "f_twoprops", nil),
		res("File", "e_src", map[string]any{"notify": "Vox_selfref[e_notified]"}),
		res("Vox_selfref", "e_notified", nil),
		res("Vox_selfref", "d_absent", map[string]any{"ensure": "absent"}),
		res("Vox_selfref", "c_nochange", nil)}})
	logs := fmtLogs(rep)
	for _, want := range []string{
		"notice /Vox_selfref[a_change] Triggered 'refresh' from 1 event",
		"notice /Vox_selfref[f_twoprops] Triggered 'refresh' from 2 events",
		"notice /Vox_selfref[e_notified] Triggered 'refresh' from 2 events",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	for _, ref := range []string{"Vox_selfref[d_absent]", "Vox_selfref[c_nochange]"} {
		if slices.Contains(calls, "refresh "+ref) {
			t.Errorf("%s refreshed itself", ref)
		}
	}
	if st := rep.ResourceStatuses["Vox_selfref[f_twoprops]"]; st.ChangeCount != 3 || !st.Restarted {
		t.Errorf("f_twoprops = %+v, want change_count 3 and restarted", st)
	}
}

// A failed self refresh fails the resource and blocks its
// dependents, as any failed refresh does.
func TestSelfRefreshFailure(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_selfref", calls: &calls, refreshable: true, selfRefresh: true,
		refreshErr: errors.New("boom from b_fail"), evals: map[string]ral.Evaluation{
			"Vox_selfref[b_fail]": {Events: []report.Event{hostSuccess("value changed 'old' to 'new'")}},
		}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_selfref", "b_fail", nil),
		res("Exec", "h_after_fail", map[string]any{"require": "Vox_selfref[b_fail]"})}})
	st := rep.ResourceStatuses["Vox_selfref[b_fail]"]
	var names, messages []string
	for _, ev := range st.Events {
		names = append(names, *ev.Name)
		messages = append(messages, ev.Message)
	}
	if !st.Failed || !st.FailedToRestart || st.ChangeCount != 1 || st.OutOfSyncCount != 2 ||
		!slices.Equal(names, []string{"value_changed", "resource_error"}) ||
		messages[1] != "Failed to call refresh: boom from b_fail" {
		t.Errorf("b_fail = %+v", st)
	}
	logs := fmtLogs(rep)
	if !rep.ResourceStatuses["Exec[h_after_fail]"].Skipped || len(applied) != 0 ||
		!strings.Contains(logs, "Dependency Vox_selfref[b_fail] has failures: true") {
		t.Errorf("the dependent was not skipped: applied=%v logs:\n%s", applied, logs)
	}
}

// transaction.rb:269 queues no event, to itself or
// anyone, from a failed status.
func TestSelfRefreshSkippedWhenTheResourceFailed(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_selfref", calls: &calls, refreshable: true, selfRefresh: true,
		evals: map[string]ral.Evaluation{
			"Vox_selfref[i_partialfail]": {Failed: true, Events: []report.Event{hostSuccess("value changed 'old' to 'new'"),
				hostFailure("change from 'old' to 'new' failed: set failed")}},
		}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_selfref", "i_partialfail", nil)}})
	if slices.Contains(calls, "refresh Vox_selfref[i_partialfail]") || strings.Contains(fmtLogs(rep), "Triggered") {
		t.Errorf("a failed resource refreshed itself: calls=%v logs:\n%s", calls, fmtLogs(rep))
	}
}

// Under noop the self refresh is a "Would have
// triggered" line and no event, and its noop_restart reaches the resource's
// subscribers beside its own noop event (event_manager.rb:52-83, :191-196).
func TestSelfRefreshUnderNoop(t *testing.T) {
	var calls, applied, refreshed []string
	noopEvent := hostSuccess("value changed 'old' to 'new' (noop)")
	noopEvent.Status = "noop"
	stub := evaluatorStub{typ: "vox_selfref", calls: &calls, refreshable: true, selfRefresh: true,
		evals: map[string]ral.Evaluation{"Vox_selfref[a_change]": {Events: []report.Event{noopEvent}}}}
	tx := evalTx(stub, &applied, &refreshed)
	tx.Noop = true
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_selfref", "a_change", nil),
		res("Exec", "e", map[string]any{"subscribe": "Vox_selfref[a_change]"})}})
	logs := fmtLogs(rep)
	for _, want := range []string{
		"notice /Vox_selfref[a_change] Would have triggered 'refresh' from 1 event",
		"notice /Exec[e] Would have triggered 'refresh' from 2 events",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if st := rep.ResourceStatuses["Vox_selfref[a_change]"]; len(st.Events) != 1 || st.Restarted || st.ChangeCount != 0 {
		t.Errorf("a_change = %+v, want only its noop event", st)
	}
}

// A type that overrides title is reported under Puppet's ref, title and
// containment leaf, and a dependent names that ref. The walk still addresses it by the catalog's ref.
func TestRetitledResourceIsReportedUnderPuppetsRef(t *testing.T) {
	retitled := map[string][2]string{"Vox_titled[fips]": {"Vox_titled[fips:all]", "fips:all"}}
	c := func() *catalog.Catalog {
		return &catalog.Catalog{
			Resources: []catalog.Resource{
				res("Stage", "main", nil), res("Class", "main", nil),
				res("Vox_titled", "fips", nil),
				res("File", "/t", map[string]any{"require": "Vox_titled[fips]"}),
			},
			Edges: []catalog.Edge{
				{Source: "Stage[main]", Target: "Class[main]"},
				{Source: "Class[main]", Target: "Vox_titled[fips]"},
				{Source: "Class[main]", Target: "File[/t]"},
			},
		}
	}

	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_titled", calls: &calls, retitled: retitled, evals: map[string]ral.Evaluation{
		"Vox_titled[fips]": {Events: []report.Event{hostSuccess("value changed 'old' to 'x'")}},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), c())
	if _, ok := rep.ResourceStatuses["Vox_titled[fips]"]; ok {
		t.Error("the status is keyed by the catalog's ref")
	}
	st, ok := rep.ResourceStatuses["Vox_titled[fips:all]"]
	if !ok {
		t.Fatalf("no status for Vox_titled[fips:all]: %v", rep.ResourceStatuses)
	}
	if st.Resource != "Vox_titled[fips:all]" || st.Title != "fips:all" || st.ResourceType != "Vox_titled" {
		t.Errorf("status = %+v", st)
	}
	if want := []string{"Stage[main]", "Main", "Vox_titled[fips:all]"}; !slices.Equal(st.ContainmentPath, want) {
		t.Errorf("containment_path = %v, want %v", st.ContainmentPath, want)
	}
	if !slices.Contains(calls, "evaluate Vox_titled[fips]") {
		t.Errorf("the host was not called by the catalog's ref: %v", calls)
	}

	calls, applied, refreshed = nil, nil, nil
	stub.evals = map[string]ral.Evaluation{
		"Vox_titled[fips]": {Events: []report.Event{hostFailure("change from 'old' to 'x' failed: set failed")}, Failed: true},
	}
	rep = runCatalog(t, evalTx(stub, &applied, &refreshed), c())
	logs := fmtLogs(rep)
	if !strings.Contains(logs, "notice /Stage[main]/Main/File[/t] Dependency Vox_titled[fips:all] has failures: true") {
		t.Errorf("the dependent does not name Puppet's ref:\n%s", logs)
	}
	if !rep.ResourceStatuses["File[/t]"].Skipped {
		t.Error("the dependent was not skipped")
	}
}

// An Evaluator's debug lines are held to the report's debug budget like
// voxagent's own (D-81): the first past it is preceded by one
// warning, later ones are printed only, and notices still land.
func TestEvaluatorDebugLogsFollowTheBudget(t *testing.T) {
	const mib = 1 << 20
	dbg := func(n int) report.LogEntry {
		return report.LogEntry{Level: "debug", Source: "Puppet", Message: strings.Repeat("x", n)}
	}
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Logs: []report.LogEntry{dbg(10 * mib), dbg(10 * mib), {Level: "notice", Source: "/Vox_value[a]", Message: "kept"}}},
		"Vox_value[b]": {Logs: []report.LogEntry{dbg(1), {Level: "notice", Source: "/Vox_value[b]", Message: "kept too"}}},
	}}
	c := &catalog.Catalog{Resources: []catalog.Resource{res("Vox_value", "a", nil), res("Vox_value", "b", nil)}}
	tx := evalTx(stub, &applied, &refreshed)
	rep := report.New("n", "production", false)
	var printed []report.LogEntry
	ctx := runlog.WithLogger(context.Background(), runlog.New("debug", func(e report.LogEntry) { rep.AddLog(e) },
		func(e report.LogEntry) { printed = append(printed, e) }))
	g, err := BuildGraph(ctx, c, tx.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Run(ctx, c, g, rep); err != nil {
		t.Fatal(err)
	}

	var warnings, recordedDebug int
	for _, l := range rep.Logs {
		switch {
		case l.Level == "warning" && strings.HasPrefix(l.Message, "debug log truncated"):
			warnings++
		case l.Level == "debug" && l.Source == "Puppet":
			recordedDebug++
		}
	}
	if warnings != 1 || recordedDebug != 1 {
		t.Errorf("recorded %d truncation warnings and %d host debug lines, want 1 and 1", warnings, recordedDebug)
	}
	if n := len(printed); n != 2 {
		t.Errorf("%d host debug lines were print-only, want 2 (the one over budget and the later one)", n)
	}
	if got := fmtLogs(rep); !strings.Contains(got, "kept\n") || !strings.Contains(got, "kept too\n") {
		t.Errorf("a notice after the truncation is missing: %s", got)
	}
}

// With no run logger the transaction has no budget to apply and drops nothing.
func TestEvaluatorLogsWithoutLoggerAreAllKept(t *testing.T) {
	var calls, applied, refreshed []string
	stub := evaluatorStub{typ: "vox_value", calls: &calls, evals: map[string]ral.Evaluation{
		"Vox_value[a]": {Logs: []report.LogEntry{
			{Level: "debug", Source: "Puppet", Message: "d"}, {Level: "notice", Source: "/Vox_value[a]", Message: "n"}}},
	}}
	rep := runCatalog(t, evalTx(stub, &applied, &refreshed), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil)}})
	if got := fmtLogs(rep); !strings.Contains(got, "debug Puppet d\n") || !strings.Contains(got, "notice /Vox_value[a] n\n") {
		t.Errorf("host entries were dropped without a logger: %s", got)
	}
}
