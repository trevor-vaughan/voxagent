package report

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestReportMarshalsRequiredFields(t *testing.T) {
	r := New("node1.example.com", "production", false)
	r.AddResource(ResourceStatus{
		Resource:       "Notify[hello]",
		ResourceType:   "Notify",
		Title:          "hello",
		File:           "/etc/site.pp",
		Line:           3,
		ChangeCount:    1,
		OutOfSync:      true,
		OutOfSyncCount: 1,
		Events: []Event{{
			Property:         "message",
			PreviousValue:    nil,
			DesiredValue:     "hi",
			Message:          "defined 'message' as 'hi'",
			Status:           "success",
			CorrectiveChange: false,
		}},
	})
	r.Finalize()

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	// The fifteen keys report.rb:335-351 builds unconditionally. The five it
	// appends "unless nil" afterwards are covered by
	// TestReportOmitsUnsetConditionalFields.
	for _, k := range []string{
		"host", "time", "configuration_version", "transaction_uuid",
		"report_format", "puppet_version", "status", "transaction_completed",
		"noop", "noop_pending", "environment", "logs", "metrics",
		"resource_statuses", "corrective_change",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("report is missing required field %q", k)
		}
	}
	if got["report_format"] != float64(12) {
		t.Errorf("report_format = %v, want 12", got["report_format"])
	}
}

// report.rb:354-358 appends five keys "unless nil" after building the
// unconditional hash, so Ruby emits no key at all for an unset one. Go would
// otherwise emit `"code_id": null`, and the differential would report five
// divergences on every run -- noise that would bury a real one.
//
// Note server_used, not master_used: report.rb:354 writes `server_used` and
// nothing anywhere writes `master_used`. Lines 266-269 accept the latter on
// READ only, as a backward-compatibility alias for reports written by older
// Puppet. A Go field named after the reader's alias would emit a key Ruby
// never produces.
func TestReportOmitsUnsetConditionalFields(t *testing.T) {
	r := New("n", "production", false)
	r.CachedCatalogStatus = "" // New sets this; clear it to test the unset case
	r.Finalize()

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	for _, k := range []string{
		"server_used", "catalog_uuid", "code_id", "job_id", "cached_catalog_status",
	} {
		if v, present := got[k]; present {
			t.Errorf("unset conditional field %q was emitted as %v; Ruby omits the key entirely", k, v)
		}
	}
	if _, present := got["master_used"]; present {
		t.Error("report emits master_used, which Puppet only ever accepts on read, never writes")
	}
}

// Set, they are emitted -- otherwise omitempty would be hiding real values and
// the test above would pass for the wrong reason.
func TestReportEmitsSetConditionalFields(t *testing.T) {
	r := New("n", "production", false)
	r.CatalogUUID = "3f2b1a00-0000-4000-8000-000000000000"
	server, code, job := "puppet.example.com", "code-1", "job-7"
	r.ServerUsed, r.CodeID, r.JobID = &server, &code, &job
	r.Finalize()

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	for k, want := range map[string]any{
		"server_used":           "puppet.example.com",
		"catalog_uuid":          "3f2b1a00-0000-4000-8000-000000000000",
		"code_id":               "code-1",
		"job_id":                "job-7",
		"cached_catalog_status": "not_used",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

// The status string is what report processors and dashboards switch on, so it
// must follow Puppet's vocabulary exactly.
func TestReportStatusVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Report)
		want   string
	}{
		{"unchanged", func(r *Report) {}, "unchanged"},
		{"changed", func(r *Report) {
			r.AddResource(ResourceStatus{Resource: "Notify[a]", ChangeCount: 1})
		}, "changed"},
		{"failed", func(r *Report) {
			r.AddResource(ResourceStatus{Resource: "Notify[a]", Failed: true})
		}, "failed"},
		{"failed outranks changed", func(r *Report) {
			r.AddResource(ResourceStatus{Resource: "Notify[a]", ChangeCount: 1})
			r.AddResource(ResourceStatus{Resource: "Notify[b]", Failed: true})
		}, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New("n", "production", false)
			tt.mutate(r)
			r.Finalize()
			if r.Status != tt.want {
				t.Errorf("Status = %q, want %q", r.Status, tt.want)
			}
		})
	}
}

// Finalize must produce the counts Status.Exit consumes, including the
// failed_to_restart clause that only a failed refresh can set.
func TestReportStatusCounts(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[a]", ChangeCount: 2})
	r.AddResource(ResourceStatus{Resource: "File[b]", Failed: true})
	r.AddResource(ResourceStatus{Resource: "Exec[c]", FailedToRestart: true})
	r.Finalize()

	got := r.RunStatus()
	want := Status{Changes: 2, Failures: 1, FailedRestarts: 1}
	if got != want {
		t.Errorf("RunStatus() = %+v, want %+v", got, want)
	}
	if got.Exit(true) != 6 {
		t.Errorf("Exit(true) = %d, want 6", got.Exit(true))
	}
}

// An unfinalised report has no metrics, which is the case report.rb:449 answers
// with -1. voxagent calls it could-not-run instead (divergence D-2).
func TestUnfinalizedReportIsCouldNotRun(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[a]", ChangeCount: 1})
	got := r.RunStatus()
	if !got.CouldNotRun {
		t.Errorf("RunStatus() on an unfinalised report = %+v, want CouldNotRun", got)
	}
	if got.Exit(false) != 2 || got.Exit(true) != 1 {
		t.Errorf("Exit = %d/%d, want 2/1", got.Exit(false), got.Exit(true))
	}
}

// Measured against Puppet 8.10.0: exclude_unchanged_resources defaults to true
// (defaults.rb:1497) and report.rb:326 prunes on it, so a no-op run over an
// eight-resource catalog emitted exactly one resource status. The keep
// predicate is report.rb:43 -- failed, failed_to_restart, out_of_sync, or
// skipped. Note out_of_sync rather than changed: a noop run leaves a resource
// out of sync without changing it, and that status is kept.
func TestMarshalPrunesUnchangedResourceStatuses(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[insync]"})
	r.AddResource(ResourceStatus{Resource: "File[changed]", ChangeCount: 1})
	r.AddResource(ResourceStatus{Resource: "File[failed]", Failed: true})
	r.AddResource(ResourceStatus{Resource: "Exec[norestart]", FailedToRestart: true})
	r.AddResource(ResourceStatus{Resource: "File[skipped]", Skipped: true})
	r.AddResource(ResourceStatus{Resource: "File[noop]", OutOfSyncCount: 1})
	r.Finalize()

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var got struct {
		ResourceStatuses map[string]json.RawMessage `json:"resource_statuses"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	for _, keep := range []string{
		"File[changed]", "File[failed]", "Exec[norestart]", "File[skipped]", "File[noop]",
	} {
		if _, ok := got.ResourceStatuses[keep]; !ok {
			t.Errorf("resource_statuses is missing %q, which must be kept", keep)
		}
	}
	if _, ok := got.ResourceStatuses["File[insync]"]; ok {
		t.Error("resource_statuses contains File[insync], which must be pruned")
	}
	if len(got.ResourceStatuses) != 5 {
		t.Errorf("resource_statuses has %d entries, want 5", len(got.ResourceStatuses))
	}
}

// Metrics are computed from the UNPRUNED set. Puppet computes them in
// finalize_report and prunes only in to_data_hash, so a run where every
// resource is in sync still reports its true total.
func TestMetricsCountPrunedResources(t *testing.T) {
	r := New("n", "production", false)
	for _, ref := range []string{"File[a]", "File[b]", "File[c]"} {
		r.AddResource(ResourceStatus{Resource: ref})
	}
	r.Finalize()

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var got struct {
		ResourceStatuses map[string]json.RawMessage `json:"resource_statuses"`
		Metrics          map[string]Metric          `json:"metrics"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if len(got.ResourceStatuses) != 0 {
		t.Errorf("resource_statuses has %d entries, want 0", len(got.ResourceStatuses))
	}
	var total any
	for _, v := range got.Metrics["resources"].Values {
		if len(v) == 3 && v[0] == "total" {
			total = v[2]
		}
	}
	if total != float64(3) {
		t.Errorf("resources.total = %v, want 3 -- metrics must count the unpruned set", total)
	}
}

// Pruning must not mutate the report. RunStatus and a second Marshal both read
// the full set, so a MarshalJSON that pruned in place would make the exit
// status depend on whether the report had been serialised yet.
func TestMarshalDoesNotMutateTheReport(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[insync]"})
	r.AddResource(ResourceStatus{Resource: "File[changed]", ChangeCount: 1})
	r.Finalize()

	before := r.RunStatus()
	if _, err := json.Marshal(r); err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	if len(r.ResourceStatuses) != 2 {
		t.Errorf("Marshal pruned the report in place: %d statuses remain, want 2",
			len(r.ResourceStatuses))
	}
	if after := r.RunStatus(); after != before {
		t.Errorf("RunStatus() changed across Marshal: %+v then %+v", before, after)
	}
}

// The resources metric's key set AND order come from a real Puppet 8.10.0
// report. `values` is a JSON array, so the differential compares it
// positionally: a correct set in the wrong order reads as nine divergences, not
// zero. Measured from a live run.
func TestResourcesMetricMatchesPuppetShape(t *testing.T) {
	r := New("n", "production", false)
	r.Finalize()

	want := []string{
		"total", "skipped", "failed", "failed_to_restart", "restarted",
		"changed", "out_of_sync", "scheduled", "corrective_change",
	}
	got := r.Metrics["resources"]
	if len(got.Values) != len(want) {
		t.Fatalf("resources has %d values, want %d: %v", len(got.Values), len(want), got.Values)
	}
	for i, key := range want {
		if got.Values[i][0] != key {
			t.Errorf("resources.values[%d][0] = %v, want %q", i, got.Values[i][0], key)
		}
		if len(got.Values[i]) != 3 {
			t.Errorf("resources.values[%d] has %d entries, want 3 (name, label, value)",
				i, len(got.Values[i]))
		}
	}
}

// A serialised resource status carries exactly the keys Puppet's
// Puppet::Resource::Status#to_data_hash emits -- no more and no fewer.
//
// The key sets below are transcribed from resource/status.rb and
// transaction/event.rb in Puppet 8.10.0 rather than from a sample report, so a
// field that happens to be absent from one run cannot make this pass.
//
// An EXTRA key matters as much as a missing one, which is why this compares the
// whole set rather than checking for presence. `restarted` is the case in
// point: it is in status.rb's STATES and feeds the resources.restarted metric,
// but to_data_hash never emits it, so voxagent carrying it in JSON was a key
// Puppet has never written.
func TestResourceStatusKeysMatchPuppet(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{
		Resource: "File[a]", ResourceType: "File", Title: "a",
		ChangeCount: 1, Tags: []string{"file"},
		ContainmentPath: []string{"Stage[main]", "File[a]"},
		Events:          []Event{{Property: "ensure", Status: "success"}},
	})
	r.Finalize()

	body, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		ResourceStatuses map[string]map[string]json.RawMessage `json:"resource_statuses"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	status, ok := doc.ResourceStatuses["File[a]"]
	if !ok {
		t.Fatal("File[a] is not in the serialised report")
	}

	wantStatus := []string{
		"title", "file", "line", "resource", "resource_type", "provider_used",
		"containment_path", "evaluation_time", "tags", "time", "failed",
		"failed_to_restart", "changed", "out_of_sync", "skipped", "change_count",
		"out_of_sync_count", "events", "corrective_change",
	}
	assertKeys(t, "resource status", status, wantStatus)

	var events []map[string]json.RawMessage
	if err := json.Unmarshal(status["events"], &events); err != nil {
		t.Fatalf("events is not an array of objects: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	assertKeys(t, "event", events[0], []string{
		"audited", "property", "previous_value", "desired_value",
		"historical_value", "message", "name", "status", "time",
		"redacted", "corrective_change",
	})
}

// assertKeys reports every key present that should not be and every key missing
// that should be, rather than failing on the first difference -- the whole set
// is more useful than its first element when a shape is being matched.
func assertKeys(t *testing.T, what string, got map[string]json.RawMessage, want []string) {
	t.Helper()
	expected := map[string]bool{}
	for _, key := range want {
		expected[key] = true
		if _, ok := got[key]; !ok {
			t.Errorf("%s is missing the %q key that Puppet emits", what, key)
		}
	}
	for key := range got {
		if !expected[key] {
			t.Errorf("%s carries a %q key that Puppet never emits", what, key)
		}
	}
}

// The time metric carries a per-resource-type breakdown alongside the three
// phase timings, and the report differential compares those NAMES and LABELS
// even though it allowlists their values as run-to-run variance.
//
// Measured from a live Puppet 8.10.0 report over a notify and two files:
//
//	notify                 Notify                 0.000699418
//	file                   File                   0.0017622
//	config_retrieval       Config retrieval       0
//	transaction_evaluation Transaction evaluation 0.00922023318707943
//	catalog_application    Catalog application    0.010665008798241615
//	total                  Total                  0.01696293
//
// config_retrieval is 0 and must be emitted as 0 rather than omitted: on the
// --catalog path there is no catalog to retrieve, on either implementation, so
// the zero is the value and not a placeholder.
func TestTimeMetricMatchesPuppetShape(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{
		Resource: "Notify[a]", ResourceType: "Notify", ChangeCount: 1,
		EvaluationTime: 0.25,
	})
	r.AddResource(ResourceStatus{
		Resource: "File[b]", ResourceType: "File", ChangeCount: 1,
		EvaluationTime: 0.5,
	})
	r.AddResource(ResourceStatus{
		Resource: "File[c]", ResourceType: "File", ChangeCount: 1,
		EvaluationTime: 0.25,
	})
	r.Finalize()

	labels := map[string]string{}
	values := map[string]any{}
	for _, v := range r.Metrics["time"].Values {
		if len(v) != 3 {
			t.Fatalf("time value %v has %d entries, want 3", v, len(v))
		}
		name, _ := v[0].(string)
		label, _ := v[1].(string)
		labels[name] = label
		values[name] = v[2]
	}

	for name, want := range map[string]string{
		"notify":                 "Notify",
		"file":                   "File",
		"config_retrieval":       "Config retrieval",
		"transaction_evaluation": "Transaction evaluation",
		"catalog_application":    "Catalog application",
		"total":                  "Total",
	} {
		got, ok := labels[name]
		if !ok {
			t.Errorf("time metric has no %q entry", name)
			continue
		}
		if got != want {
			t.Errorf("time.%s label = %q, want %q", name, got, want)
		}
	}

	if got := values["config_retrieval"]; got != float64(0) {
		t.Errorf("config_retrieval = %v (%T), want float64(0)", got, got)
	}
	// The per-type entry is the sum over that type, not a count and not one
	// resource's time: two files at 0.5 and 0.25 make 0.75.
	if got := values["file"]; got != 0.75 {
		t.Errorf("time.file = %v, want 0.75 (the sum across both File resources)", got)
	}
	if got := values["notify"]; got != 0.25 {
		t.Errorf("time.notify = %v, want 0.25", got)
	}
}

// Puppet emits an events metric. The first version of this report omitted it
// entirely, which is a whole missing category rather than a wrong value inside
// one -- and categories are what a report processor switches on.
func TestEventsMetricCountsByStatus(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[a]", ChangeCount: 2, Events: []Event{
		{Property: "ensure", Status: "success"},
		{Property: "mode", Status: "success"},
	}})
	r.AddResource(ResourceStatus{Resource: "File[b]", Failed: true, Events: []Event{
		{Property: "ensure", Status: "failure"},
	}})
	r.Finalize()

	got := map[string]any{}
	for _, v := range r.Metrics["events"].Values {
		got[v[0].(string)] = v[2]
	}
	for key, want := range map[string]int{"total": 3, "failure": 1, "success": 2} {
		if got[key] != want {
			t.Errorf("events.%s = %v, want %d", key, got[key], want)
		}
	}
}

// restarted counts refreshes that SUCCEEDED, and is a different field from
// failed_to_restart. Conflating them would report every successful
// notify-triggered reload as a failure.
func TestRestartedIsSeparateFromFailedToRestart(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "Exec[ok]", Restarted: true, ChangeCount: 1})
	r.AddResource(ResourceStatus{Resource: "Exec[bad]", FailedToRestart: true})
	r.Finalize()

	got := map[string]any{}
	for _, v := range r.Metrics["resources"].Values {
		got[v[0].(string)] = v[2]
	}
	if got["restarted"] != 1 {
		t.Errorf("resources.restarted = %v, want 1", got["restarted"])
	}
	if got["failed_to_restart"] != 1 {
		t.Errorf("resources.failed_to_restart = %v, want 1", got["failed_to_restart"])
	}
	// A successful restart must not reach the exit status; a failed one must.
	if s := r.RunStatus(); s.FailedRestarts != 1 {
		t.Errorf("RunStatus().FailedRestarts = %d, want 1", s.FailedRestarts)
	}
}

// Finalize logs the line Puppet logs when a catalog finishes.
//
// Measured: configurer.rb wraps the application in
// benchmark(:notice, "Applied catalog in %{seconds} seconds"), and util.rb's
// benchmark formats the number with "%0.2f". The entry's source is "Puppet"
// because it comes from the global logger rather than from any resource.
//
// Emitted from Finalize rather than from the command so the number and the
// catalog_application metric are the same measurement, not two readings of the
// same clock that can disagree.
func TestFinalizeLogsTheAppliedCatalogLine(t *testing.T) {
	r := New("n", "production", false)
	r.Finalize()

	var found *LogEntry
	for i := range r.Logs {
		if strings.HasPrefix(r.Logs[i].Message, "Applied catalog in ") {
			found = &r.Logs[i]
		}
	}
	if found == nil {
		t.Fatalf("no 'Applied catalog in' entry in %+v", r.Logs)
	}
	if found.Source != "Puppet" {
		t.Errorf("source = %q, want \"Puppet\"", found.Source)
	}
	if found.Level != "notice" {
		t.Errorf("level = %q, want \"notice\"", found.Level)
	}
	// Two decimal places, as Ruby's "%0.2f" produces -- "0.01", never "0.0105".
	if !regexp.MustCompile(`^Applied catalog in \d+\.\d{2} seconds$`).MatchString(found.Message) {
		t.Errorf("message = %q, want 'Applied catalog in N.NN seconds'", found.Message)
	}
}
