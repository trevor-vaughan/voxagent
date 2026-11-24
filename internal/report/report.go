package report

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Format is the report format voxagent emits. Puppet 8.10.0 sets 12 at
// transaction/report.rb:234, and honouring it is what lets existing report
// processors, PuppetDB, and dashboards keep working across the swap.
const Format = 12

// PuppetVersion is the version voxagent claims compatibility with. It is a
// cross-implementation allowlist entry in the report differential -- Ruby
// reports its own version here and Go cannot match it, which is expected and
// recorded rather than normalised away.
const PuppetVersion = "8.10.0"

// Event is one property that moved, or failed to -- or, when Property is nil, a
// callback or resource-level event that describes no property at all.
//
// The key set is Puppet 8.10.0's transaction/event.rb to_data_hash exactly.
// Audited and HistoricalValue are always the zero value for the spine, because
// auditing is outside the bounded attribute set -- but Puppet emits both
// unconditionally, and a consumer reading `event['audited']` must not get a
// missing key from one implementation and false from the other.
//
// Property and Redacted are `any` rather than string and bool because Puppet
// writes NULL in each of them, on different events, and the two nulls have one
// cause. event.rb:58 does `self.property = property if property`, and
// event.rb:38 defaults the `redacted:` keyword to FALSE, so:
//
//	a property event   property.rb:260 passes redacted: @sensitive, which is nil
//	                   for every property in the spine's bounded sets, and the
//	                   property's own name -> {"property":"returns",
//	                   "redacted":null}
//	a callback event    add_callback_status_event and fail_with_event call
//	or resource_error   Event.new directly with neither, so property stays nil
//	                   and redacted takes the default -> {"property":null,
//	                   "redacted":false}
//
// Measured in one report: G5's exec fixture produces both shapes, a returns
// event and a refresh event, in the same run. Spelling either of them as Go's
// zero value emitted "" where Ruby writes null and null where Ruby writes false.
type Event struct {
	Audited          bool    `json:"audited"`
	Property         any     `json:"property"`
	PreviousValue    any     `json:"previous_value"`
	DesiredValue     any     `json:"desired_value"`
	HistoricalValue  any     `json:"historical_value"`
	Message          string  `json:"message"`
	Name             *string `json:"name"`
	Status           string  `json:"status"`
	Time             string  `json:"time"`
	Redacted         any     `json:"redacted"`
	CorrectiveChange bool    `json:"corrective_change"`
}

// ResourceStatus is the per-resource half of the report. It is keyed by
// resource reference in the report body, so the order resources were applied in
// is not observable here -- which is what lets the spine choose its own
// deterministic walk order rather than reproducing Puppet's.
type ResourceStatus struct {
	Resource        string   `json:"resource"`
	ResourceType    string   `json:"resource_type"`
	Title           string   `json:"title"`
	File            string   `json:"file"`
	Line            int      `json:"line"`
	ProviderUsed    *string  `json:"provider_used"`
	ContainmentPath []string `json:"containment_path"`
	Tags            []string `json:"tags"`
	EvaluationTime  float64  `json:"evaluation_time"`
	Failed          bool     `json:"failed"`
	FailedToRestart bool     `json:"failed_to_restart"`

	// Restarted is not serialised, and that is measured rather than an
	// oversight. resource/status.rb lists it in STATES and the transaction sets
	// it, but to_data_hash never writes it -- it reaches the report only through
	// the resources.restarted metric. Emitting it produced a key Ruby has never
	// written, on every resource of every report.
	Restarted bool `json:"-"`

	// Untimed marks a status Ruby writes with a null evaluation_time because
	// it never evaluated the resource. It is not serialised (EvaluationTime is
	// then 0, D-36); it keeps the status out of the per-type time metric, as
	// report.rb:502's calculate_time_metrics skips a null evaluation_time.
	Untimed bool `json:"-"`

	Changed          bool    `json:"changed"`
	OutOfSync        bool    `json:"out_of_sync"`
	Skipped          bool    `json:"skipped"`
	ChangeCount      int     `json:"change_count"`
	OutOfSyncCount   int     `json:"out_of_sync_count"`
	CorrectiveChange bool    `json:"corrective_change"`
	Events           []Event `json:"events"`
	Time             string  `json:"time"`

	// Generated marks a generated resource: one whose catalog.Resource has a
	// GeneratedFrom, whether a Generator or EvalGenerator made it during the
	// walk or the extension host's generate made it at load. It is not
	// serialised itself: it makes file and line serialise as null, which is
	// what Ruby writes for a generated resource (measured;
	// resource/status.rb:169-170), where a catalog resource keeps its
	// manifest location, or "" and 0 when it has none.
	Generated bool `json:"-"`
}

// LogEntry is one line of the run's log. logs is an ordered array, and because
// the spine commits to its own walk order rather than Puppet's, the report
// differential compares it as a multiset under a declared normalisation rule.
type LogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
	Source  string `json:"source"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Time    string `json:"time"`
}

// Metric mirrors Puppet's metric shape: a category with a display name and a
// list of [name, label, value] triples.
type Metric struct {
	Name   string  `json:"name"`
	Label  string  `json:"label"`
	Values [][]any `json:"values"`
}

// Report is a report_format 12 document.
type Report struct {
	Host                 string `json:"host"`
	Time                 string `json:"time"`
	ConfigurationVersion any    `json:"configuration_version"`
	TransactionUUID      string `json:"transaction_uuid"`
	ReportFormat         int    `json:"report_format"`
	PuppetVersion        string `json:"puppet_version"`
	Status               string `json:"status"`
	TransactionCompleted bool   `json:"transaction_completed"`
	Noop                 bool   `json:"noop"`
	NoopPending          bool   `json:"noop_pending"`
	Environment          string `json:"environment"`
	CorrectiveChange     bool   `json:"corrective_change"`

	// The five below are emitted only when set, matching report.rb:354-358,
	// which appends each one "unless nil" after building the unconditional
	// hash. Without omitempty, Go would emit `"code_id": null` where Ruby emits
	// no key at all, and the differential would report five divergences on
	// every single run -- noise that would bury a real one.
	//
	// ServerUsed, not MasterUsed. report.rb:354 writes `server_used`, and
	// nothing anywhere writes `master_used`; lines 266-269 accept it on READ
	// only, as a backward-compatibility alias for old reports. Naming the Go
	// field after the reader's legacy alias would have produced a key Ruby
	// never emits.
	ServerUsed          *string `json:"server_used,omitempty"`
	CatalogUUID         string  `json:"catalog_uuid,omitempty"`
	CodeID              *string `json:"code_id,omitempty"`
	JobID               *string `json:"job_id,omitempty"`
	CachedCatalogStatus string  `json:"cached_catalog_status,omitempty"`

	ResourceStatuses map[string]ResourceStatus `json:"resource_statuses"`
	Logs             []LogEntry                `json:"logs"`
	Metrics          map[string]Metric         `json:"metrics"`

	// EvaluationSpan is how long the transaction walk itself took, which
	// Finalize reports as the transaction_evaluation timing. It is set by the
	// transaction rather than measured here because this type cannot see where
	// the walk begins and ends, and inferring it from the report's own lifetime
	// would report the same number as catalog_application and quietly assert
	// that the two phases are identical.
	EvaluationSpan time.Duration `json:"-"`

	// FailedToGenerate is Ruby's resources_failed_to_generate
	// (transaction/additional_resource_generator.rb:66): a Generator failed
	// this run. compute_status (transaction/report.rb:189-200) reports the run
	// failed for it, while exit_status (:440-452) ignores it, so it changes
	// Status and nothing RunStatus derives.
	FailedToGenerate bool `json:"-"`

	finalized bool
	started   time.Time      // New: catalog_application, and total unless runStart is set
	runStart  time.Time      // StartedAt: total, when set
	phases    []phase        // AddTime, in the order recorded
	observer  func(LogEntry) // Observe: called after each appended log entry
}

// phase is one duration configurer.rb records with report.add_times.
type phase struct {
	name    string
	seconds float64
}

// StartedAt sets when an agent run began, as Report.new's start_time does
// (configurer.rb:305): the report's time, and the start of the total timing,
// which configurer.rb:523 measures from it. catalog_application still runs
// from New, where applying begins.
func (r *Report) StartedAt(t time.Time) {
	r.runStart = t
	r.Time = t.UTC().Format(time.RFC3339Nano)
}

// AddTime records the duration of one phase of an agent run under Puppet's
// name for it (configurer.rb's add_times: startup_time, node_retrieval,
// plugin_sync, fact_generation, convert_catalog, config_retrieval). Finalize
// emits each in the time metric; a recorded config_retrieval replaces the 0 a
// handed-in catalog reports. A name recorded twice accumulates, as add_times
// does by default (report.rb:163).
func (r *Report) AddTime(name string, d time.Duration) {
	for i := range r.phases {
		if r.phases[i].name == name {
			r.phases[i].seconds += d.Seconds()
			return
		}
	}
	r.phases = append(r.phases, phase{name, d.Seconds()})
}

// labelize is util/metric.rb's labelize of a metric name: capitalized,
// underscores to spaces -- notify/Notify, file_line/File line.
func labelize(name string) string {
	label := strings.ReplaceAll(name, "_", " ")
	if label != "" {
		label = strings.ToUpper(label[:1]) + label[1:]
	}
	return label
}

// keepInReport reports whether a resource status survives into the serialised
// report.
//
// Puppet's exclude_unchanged_resources defaults to true (defaults.rb:1497) and
// report.rb:326 prunes on it, so matching the default is what makes the
// differential compare like with like. The predicate is report.rb:43 exactly.
//
// Note out_of_sync rather than changed: a --noop run leaves a resource out of
// sync without changing it, and that status is kept. Using changed here would
// silently empty every noop report.
func keepInReport(rs ResourceStatus) bool {
	return rs.Failed || rs.FailedToRestart || rs.OutOfSync || rs.Skipped
}

// MarshalJSON writes a generated status's file and line as null. The outer
// fields sit one level shallower than the embedded ones of the same JSON
// name, and encoding/json takes the shallowest, so they replace the
// embedded pair and every other key is unchanged.
func (rs ResourceStatus) MarshalJSON() ([]byte, error) {
	type plain ResourceStatus
	if !rs.Generated {
		return json.Marshal(plain(rs))
	}
	return json.Marshal(struct {
		plain
		File *string `json:"file"`
		Line *int    `json:"line"`
	}{plain: plain(rs)})
}

// MarshalJSON emits the report with unchanged resource statuses pruned.
//
// Pruning happens here rather than in Finalize because Puppet computes its
// metrics from the unpruned set and prunes only on the way out (to_data_hash).
// Pruning earlier would make a clean run report zero resources rather than
// zero changes across N resources.
//
// The alias type breaks the recursion into this method, and the pruned map is
// built fresh rather than deleted from: mutating r.ResourceStatuses here would
// make RunStatus -- and therefore the process exit status -- depend on whether
// the report had been serialised yet.
func (r *Report) MarshalJSON() ([]byte, error) {
	type alias Report
	kept := make(map[string]ResourceStatus, len(r.ResourceStatuses))
	for ref, rs := range r.ResourceStatuses {
		if keepInReport(rs) {
			kept[ref] = rs
		}
	}
	shadow := alias(*r)
	shadow.ResourceStatuses = kept
	return json.Marshal(shadow)
}

// New starts a report. The caller supplies the clock-independent fields; time
// and the transaction UUID are set here because both vary between two runs of
// the same implementation and nothing downstream should depend on their values.
func New(host, environment string, noop bool) *Report {
	now := time.Now().UTC()
	return &Report{
		Host:                 host,
		Time:                 now.Format(time.RFC3339Nano),
		TransactionUUID:      NewUUID(),
		ReportFormat:         Format,
		PuppetVersion:        PuppetVersion,
		Status:               "unchanged",
		TransactionCompleted: false,
		Noop:                 noop,
		Environment:          environment,
		CachedCatalogStatus:  "not_used",
		ResourceStatuses:     map[string]ResourceStatus{},
		Logs:                 []LogEntry{},
		Metrics:              map[string]Metric{},
		started:              now,
	}
}

// AddResource records one resource's outcome. The transaction visits each
// resource exactly once, so a repeat call is a programming error rather than a
// runtime condition; the later call simply overwrites.
func (r *Report) AddResource(rs ResourceStatus) {
	if rs.Time == "" {
		rs.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	rs.Changed = rs.ChangeCount > 0
	if rs.OutOfSyncCount == 0 && rs.ChangeCount > 0 {
		rs.OutOfSyncCount = rs.ChangeCount
	}
	rs.OutOfSync = rs.OutOfSyncCount > 0
	// Ruby emits [] for a resource with no events, tags or containers, never
	// null. A nil slice in Go marshals to null, which is a different JSON type
	// and breaks any consumer that iterates the field without checking.
	if rs.Events == nil {
		rs.Events = []Event{}
	}
	if rs.Tags == nil {
		rs.Tags = []string{}
	}
	if rs.ContainmentPath == nil {
		rs.ContainmentPath = []string{}
	}
	r.ResourceStatuses[rs.Resource] = rs
}

// Observe sets the one function called with each log entry right after Log or
// AddLog appends it, in order; nil removes it. The run's console is the
// observer, so every entry is printed as it is recorded.
func (r *Report) Observe(f func(LogEntry)) { r.observer = f }

// Log appends a log entry.
func (r *Report) Log(level, source, message string) {
	r.AddLog(LogEntry{Level: level, Message: message, Source: source})
}

// AddLog appends whole entries: a log line another implementation already
// worded, such as the extension host's, with its file and line. Time is
// stamped when the entry has none.
func (r *Report) AddLog(entries ...LogEntry) {
	for _, e := range entries {
		if e.Time == "" {
			e.Time = time.Now().UTC().Format(time.RFC3339Nano)
		}
		r.Logs = append(r.Logs, e)
		if r.observer != nil {
			r.observer(e)
		}
	}
}

// Finalize computes the metrics and the status string. Until it is called the
// report carries no metrics, which is exactly the state report.rb:449 answers
// with -1; RunStatus reports that as could-not-run instead.
func (r *Report) Finalize() { r.finalize(true) }

// FinalizeIncomplete finalizes the report of a transaction that stopped with an
// error: transaction_completed stays false, which compute_status
// (transaction/report.rb:189-200) reports as failed, and there is no "Applied
// catalog" line, as configurer.rb's benchmark raised before logging it.
func (r *Report) FinalizeIncomplete() { r.finalize(false) }

func (r *Report) finalize(completed bool) {
	var changes, failed, restarts, restarted, skipped, changed, outOfSync, corrective int
	var events, evFailure, evSuccess, evNoop int
	byType := map[string]float64{}
	for _, rs := range r.ResourceStatuses {
		if !rs.Untimed {
			byType[rs.ResourceType] += rs.EvaluationTime
		}
		changes += rs.ChangeCount
		if rs.Failed {
			failed++
		}
		if rs.FailedToRestart {
			restarts++
		}
		if rs.Restarted {
			restarted++
		}
		if rs.Skipped {
			skipped++
		}
		if rs.Changed {
			changed++
		}
		if rs.OutOfSync {
			outOfSync++
		}
		if rs.CorrectiveChange {
			corrective++
		}
		for _, e := range rs.Events {
			events++
			switch e.Status {
			case "failure":
				evFailure++
			case "success":
				evSuccess++
			case "noop":
				evNoop++
			}
		}
	}
	total := len(r.ResourceStatuses)

	// Key set copied from a real Puppet 8.10.0 report, not invented. The order
	// is Puppet's too, but nothing depends on it: `gatekit compare-reports` re-keys
	// every metric group by name before flattening, so a correct set in a
	// different order is not a divergence, and a missing or extra metric shows
	// up as the one absent path rather than as every later index misaligning.
	//
	// restarted and scheduled were absent from the first version of this. Both
	// are always zero for the spine today: nothing here declares `schedule`, and
	// restarted is set only when a refresh succeeds. Emitting them anyway is
	// what keeps the shape identical, and a zero Puppet also emits is not a
	// placeholder -- it is the value.
	r.Metrics["resources"] = Metric{
		Name: "resources", Label: "Resources",
		Values: [][]any{
			{"total", "Total", total},
			{"skipped", "Skipped", skipped},
			{"failed", "Failed", failed},
			{"failed_to_restart", "Failed to restart", restarts},
			{"restarted", "Restarted", restarted},
			{"changed", "Changed", changed},
			{"out_of_sync", "Out of sync", outOfSync},
			{"scheduled", "Scheduled", 0},
			{"corrective_change", "Corrective change", corrective},
		},
	}
	r.Metrics["changes"] = Metric{
		Name: "changes", Label: "Changes",
		Values: [][]any{{"total", "Total", changes}},
	}
	// Measured: Puppet emits an events metric and the first version of this
	// omitted it entirely, which would have been a whole missing category in
	// the differential rather than a wrong value inside one.
	//
	// total, failure and success are always present; Puppet counts any other
	// status only when an event has it (report.rb:467-478), labelled by
	// util/metric.rb:65-67's labelize. noop is the only other status voxagent
	// produces: event.rb:19's audit comes from the audit metaparameter.
	eventValues := [][]any{
		{"total", "Total", events},
		{"failure", "Failure", evFailure},
		{"success", "Success", evSuccess},
	}
	if evNoop > 0 {
		eventValues = append(eventValues, []any{"noop", "Noop", evNoop})
	}
	r.Metrics["events"] = Metric{Name: "events", Label: "Events", Values: eventValues}
	// A per-resource-type breakdown, an agent run's phases (AddTime), then the
	// three phase timings, measured field for field from a live Puppet 8.10.0
	// report.
	//
	// The differential allowlists the individual DURATIONS as run-to-run
	// variance but compares every name and label, so the shape here is asserted
	// rather than assumed. config_retrieval is the exception and is compared as
	// a value: on the --catalog path neither implementation retrieves a catalog,
	// so both must report a literal 0. Allowlisting it would throw away a real
	// assertion in order to hide a number that never varies. An agent run
	// retrieves one and records how long that took.
	elapsed := time.Since(r.started).Seconds()
	types := make([]string, 0, len(byType))
	for typ := range byType {
		types = append(types, typ)
	}
	// Sorted only so two runs of voxagent order the category identically. The
	// differential re-keys metrics by name before comparing, so order is not
	// what makes this match Ruby -- Puppet's own per-type order is neither
	// alphabetical nor stable.
	sort.Strings(types)
	values := make([][]any, 0, len(byType)+4)
	for _, typ := range types {
		// Puppet keys the metric by the lowercased type and labels it with
		// util/metric.rb's labelize of that key.
		name := strings.ToLower(typ)
		values = append(values, []any{name, labelize(name), byType[typ]})
	}
	retrieval := float64(0)
	for _, p := range r.phases {
		if p.name == "config_retrieval" {
			retrieval = p.seconds
			continue
		}
		values = append(values, []any{p.name, labelize(p.name), p.seconds})
	}
	totalTime := elapsed
	if !r.runStart.IsZero() {
		totalTime = time.Since(r.runStart).Seconds()
	}
	r.Metrics["time"] = Metric{
		Name: "time", Label: "Time",
		Values: append(values,
			[]any{"config_retrieval", "Config retrieval", retrieval},
			[]any{"transaction_evaluation", "Transaction evaluation", r.EvaluationSpan.Seconds()},
			[]any{"catalog_application", "Catalog application", elapsed},
			[]any{"total", "Total", totalTime},
		),
	}

	switch {
	case !completed || failed > 0 || restarts > 0 || r.FailedToGenerate:
		r.Status = "failed"
	case changes > 0:
		r.Status = "changed"
	default:
		r.Status = "unchanged"
	}
	// The line Ruby logs when a catalog finishes. configurer.rb wraps the
	// application in benchmark(:notice, "Applied catalog in %{seconds} seconds")
	// and util.rb's benchmark formats the number with "%0.2f". The source is
	// "Puppet" rather than a resource because it comes from the global logger.
	//
	// Logged here rather than by the command so this number and the
	// catalog_application metric are one measurement instead of two readings of
	// the same clock that can disagree in the last decimal place.
	if completed {
		r.Log("notice", "Puppet", fmt.Sprintf("Applied catalog in %.2f seconds", elapsed))
	}

	r.CorrectiveChange = corrective > 0
	// Any noop event, as transaction/report.rb:223 has it (has_noop_events?,
	// :203-205): NOT change_count, which only 'success' events credit
	// (resource/status.rb:122-133), and not out_of_sync_count, which a
	// failure event also raises. Not gated on --noop either, as Ruby's is not;
	// a live run's resource-level noop is unreachable while a per-resource noop
	// stays a pre-flight gap, so there it is false.
	r.NoopPending = evNoop > 0
	r.TransactionCompleted = completed
	r.finalized = true
}

// RunStatus reduces the report to the counts the exit contract needs.
//
// An unfinalised report is could-not-run. Ruby answers the same condition with
// -1, which its exit(-1) surfaces as 255; this is divergence D-2.
func (r *Report) RunStatus() Status {
	if !r.finalized {
		return Status{CouldNotRun: true}
	}
	s := Status{}
	for _, rs := range r.ResourceStatuses {
		s.Changes += rs.ChangeCount
		if rs.Failed {
			s.Failures++
		}
		if rs.FailedToRestart {
			s.FailedRestarts++
		}
	}
	return s
}

// NewUUID returns a random version-4 UUID. crypto/rand.Read is documented never
// to return an error as of Go 1.24, so a failure here is not a runtime
// condition to handle but a broken runtime to panic on.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("report: crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
