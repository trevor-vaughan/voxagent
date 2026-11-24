package report

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
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
	r.AddResource(ResourceStatus{
		Resource: "File_line[d]", ResourceType: "File_line", ChangeCount: 1,
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

	// util/metric.rb's labelize: capitalized, underscores to spaces.
	for name, want := range map[string]string{
		"notify":                 "Notify",
		"file":                   "File",
		"file_line":              "File line",
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

// An agent run adds the phases configurer.rb times with report.add_times,
// measured from a live OpenVox 8 agent report: each under its own name,
// labelled as util/metric.rb labelizes it. A recorded config_retrieval replaces
// the --catalog path's 0, a phase recorded twice accumulates (report.rb:163),
// and total runs from the start StartedAt set, not from New.
func TestTimeMetricCarriesAgentPhases(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	r := New("n", "production", false)
	r.StartedAt(start)
	r.AddTime("startup_time", 2*time.Second)
	r.AddTime("plugin_sync", time.Second)
	r.AddTime("plugin_sync", time.Second/2)
	r.AddTime("config_retrieval", 3*time.Second)
	r.Finalize()

	labels := map[string]string{}
	values := map[string]float64{}
	counts := map[string]int{}
	for _, v := range r.Metrics["time"].Values {
		name := v[0].(string)
		labels[name] = v[1].(string)
		values[name] = v[2].(float64)
		counts[name]++
	}
	for name, want := range map[string]string{
		"startup_time": "Startup time", "plugin_sync": "Plugin sync", "config_retrieval": "Config retrieval",
	} {
		if labels[name] != want || counts[name] != 1 {
			t.Errorf("time.%s: label %q, %d entries; want %q once", name, labels[name], counts[name], want)
		}
	}
	for name, want := range map[string]float64{"startup_time": 2, "plugin_sync": 1.5, "config_retrieval": 3} {
		if values[name] != want {
			t.Errorf("time.%s = %v, want %v", name, values[name], want)
		}
	}
	if values["total"] < 3600 {
		t.Errorf("total = %v, want at least the hour since StartedAt", values["total"])
	}
	if values["catalog_application"] >= 3600 {
		t.Errorf("catalog_application = %v, want it measured from New, not from StartedAt", values["catalog_application"])
	}
	if want := start.UTC().Format(time.RFC3339Nano); r.Time != want {
		t.Errorf("report time = %q, want the run's start %q", r.Time, want)
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

// Measured on Puppet 8.10.0: a generated child's status has file: null and
// line: null. A catalog resource keeps its location, and "" and 0 when it has
// none, exactly as before.
func TestGeneratedResourceStatusHasNullFileAndLine(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[/d/a]", ResourceType: "File", Title: "/d/a",
		Generated: true, ChangeCount: 1})
	r.AddResource(ResourceStatus{Resource: "File[/d]", ResourceType: "File", Title: "/d",
		File: "/etc/site.pp", Line: 4, ChangeCount: 1})
	r.AddResource(ResourceStatus{Resource: "File[/x]", ResourceType: "File", Title: "/x", ChangeCount: 1})
	r.Finalize()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ResourceStatuses map[string]map[string]json.RawMessage `json:"resource_statuses"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for ref, want := range map[string][2]string{
		"File[/d/a]": {"null", "null"},
		"File[/d]":   {`"/etc/site.pp"`, "4"},
		"File[/x]":   {`""`, "0"},
	} {
		rs := doc.ResourceStatuses[ref]
		if string(rs["file"]) != want[0] || string(rs["line"]) != want[1] {
			t.Errorf("%s file/line = %s/%s, want %s/%s", ref, rs["file"], rs["line"], want[0], want[1])
		}
	}
	if len(doc.ResourceStatuses["File[/d/a]"]) != len(doc.ResourceStatuses["File[/d]"]) {
		t.Errorf("a generated status has %d keys and a declared one %d; the key set must not change",
			len(doc.ResourceStatuses["File[/d/a]"]), len(doc.ResourceStatuses["File[/d]"]))
	}
}

// compute_status (transaction/report.rb:189-200) fails the run when a
// generator failed; exit_status (:440-452) ignores it.
func TestFailedToGenerateFailsTheReportButNotTheExitStatus(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[/d]", ResourceType: "File", Title: "/d", ChangeCount: 1})
	r.FailedToGenerate = true
	r.Finalize()
	if r.Status != "failed" {
		t.Errorf("Status = %q, want failed", r.Status)
	}
	if s := r.RunStatus(); s.Failures != 0 || s.Changes != 1 {
		t.Errorf("RunStatus = %+v, want one change and no failures", s)
	}
}

func TestAddLogKeepsFieldsAndStampsTime(t *testing.T) {
	r := New("n", "production", false)
	r.AddLog(LogEntry{Level: "notice", Source: "/S/x", Message: "m", File: "/f.pp", Line: 3},
		LogEntry{Level: "err", Source: "Puppet", Message: "e", Time: "2026-01-01T00:00:00Z"})
	if len(r.Logs) != 2 || r.Logs[0].File != "/f.pp" || r.Logs[0].Line != 3 || r.Logs[0].Time == "" ||
		r.Logs[1].Time != "2026-01-01T00:00:00Z" {
		t.Errorf("Logs = %+v", r.Logs)
	}
}

// A transaction that raised leaves transaction_completed false, which
// compute_status (transaction/report.rb:189-200) reports as failed; configurer
// never reaches the "Applied catalog" benchmark line.
func TestFinalizeIncompleteFailsTheReport(t *testing.T) {
	r := New("n", "production", false)
	r.AddResource(ResourceStatus{Resource: "File[/d]", ResourceType: "File", Title: "/d", ChangeCount: 1})
	r.FinalizeIncomplete()
	if r.Status != "failed" || r.TransactionCompleted {
		t.Errorf("Status = %q, TransactionCompleted = %v; want failed, false", r.Status, r.TransactionCompleted)
	}
	if _, ok := r.Metrics["resources"]; !ok {
		t.Errorf("no resources metric: %v", r.Metrics)
	}
	for _, e := range r.Logs {
		if strings.HasPrefix(e.Message, "Applied catalog in ") {
			t.Errorf("an incomplete run logged %q", e.Message)
		}
	}
}

// The observer sees every entry Log and AddLog append, in order, with the time
// AddLog stamped; nil removes it.
func TestObserveSeesEveryEntryInOrder(t *testing.T) {
	r := New("n", "production", false)
	var seen []LogEntry
	r.Observe(func(e LogEntry) { seen = append(seen, e) })
	r.Log("notice", "Puppet", "a")
	r.AddLog(LogEntry{Level: "info", Source: "S", Message: "b"}, LogEntry{Level: "err", Source: "S", Message: "c"})
	r.Log("debug", "voxagent", "d")
	if len(seen) != 4 || len(r.Logs) != 4 {
		t.Fatalf("seen %d, appended %d; want 4 each", len(seen), len(r.Logs))
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if seen[i].Message != want || seen[i] != r.Logs[i] {
			t.Errorf("seen[%d] = %+v, want message %q equal to Logs[%d] = %+v", i, seen[i], want, i, r.Logs[i])
		}
	}
	r.Observe(nil)
	r.Log("notice", "Puppet", "e")
	if len(seen) != 4 {
		t.Errorf("an observer set to nil still saw an entry: %+v", seen)
	}
}
