package report

import "testing"

// The table is the measured contract, not a preference. Rows marked "measured"
// were produced by running Puppet 8.10.0 directly.
func TestStatusExit(t *testing.T) {
	tests := []struct {
		name     string
		status   Status
		plain    int
		detailed int
	}{
		// measured: `puppet apply site.pp` with nothing to do
		{"clean", Status{}, 0, 0},
		// measured: changes -> plain 0, detailed 2
		{"changes", Status{Changes: 3}, 0, 2},
		// measured: a failed resource -> plain 0, detailed 4
		{"failures", Status{Failures: 1}, 0, 4},
		{"changes and failures", Status{Changes: 2, Failures: 1}, 0, 6},
		// failed_to_restart is the second clause of Puppet's 0x4. It is not
		// independently reachable in Ruby -- event_manager.rb sets failed
		// alongside it -- but this Status is the exit contract's own type, and
		// a caller constructing it with only FailedRestarts must still get 4.
		{"failed refresh only", Status{FailedRestarts: 1}, 0, 4},
		{"changes and failed refresh", Status{Changes: 1, FailedRestarts: 1}, 0, 6},
		{"changes, failures and failed refresh", Status{Changes: 1, Failures: 1, FailedRestarts: 1}, 0, 6},
		// could-not-run outranks the counts: whatever the run managed before
		// it stopped, it produced no verdict.
		//
		// All three counters are paired with CouldNotRun, not just Changes.
		// Measured by mutation: narrowing the guard to
		// `s.CouldNotRun && s.Failures == 0` made
		// Status{CouldNotRun: true, Failures: 1}.Exit(false) return 0 -- an
		// outage reported as success -- and the suite stayed green, because no
		// row paired the two. That is the defect class this repository tracks
		// most closely, so each counter gets its own row.
		{"could not run", Status{CouldNotRun: true}, 2, 1},
		{"could not run after changes", Status{CouldNotRun: true, Changes: 5}, 2, 1},
		{"could not run after failures", Status{CouldNotRun: true, Failures: 3}, 2, 1},
		{"could not run after failed refresh", Status{CouldNotRun: true, FailedRestarts: 1}, 2, 1},
		{"could not run after everything", Status{CouldNotRun: true, Changes: 2, Failures: 2, FailedRestarts: 2}, 2, 1},
		// Negative counts cannot arise from a real run, but `> 0` rather than
		// `!= 0` is deliberate and these rows say so: a hypothetical -1 must
		// read as "nothing happened", not as a change.
		{"negative changes", Status{Changes: -1}, 0, 0},
		{"negative failures", Status{Failures: -1}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.Exit(false); got != tt.plain {
				t.Errorf("Exit(false) = %d, want %d", got, tt.plain)
			}
			if got := tt.status.Exit(true); got != tt.detailed {
				t.Errorf("Exit(true) = %d, want %d", got, tt.detailed)
			}
		})
	}
}

// Exit must only ever return a value from the documented set for its mode.
//
// Asserted as set membership rather than as `!= 255`, which is what this test
// checked first. That predicate admitted 3, 5, 7 and every other value below
// 255, and no code path can produce 255 or a negative anyway -- so it asserted
// something unfalsifiable while its message claimed "a value in 0..6". Measured
// by mutation: making Exit return 7 when all three counters are set left the
// suite green.
//
// 255 is what Ruby's exit(-1) produces for an unfinalised report
// (report.rb:449). Divergence D-2 says voxagent never emits it, and set
// membership still says so -- more strictly than naming the one value did.
func TestStatusExitIsAlwaysInTheDocumentedSet(t *testing.T) {
	plain := map[int]bool{0: true, 2: true}
	detailed := map[int]bool{0: true, 1: true, 2: true, 4: true, 6: true}

	for _, s := range []Status{
		{}, {Changes: 1}, {Failures: 1}, {FailedRestarts: 1},
		{CouldNotRun: true},
		{Changes: 1, Failures: 1, FailedRestarts: 1},
		{CouldNotRun: true, Changes: 1, Failures: 1, FailedRestarts: 1},
		{Changes: -1}, {Failures: -1}, {FailedRestarts: -1},
	} {
		if got := s.Exit(false); !plain[got] {
			t.Errorf("Status%+v.Exit(false) = %d, want one of {0, 2}", s, got)
		}
		if got := s.Exit(true); !detailed[got] {
			t.Errorf("Status%+v.Exit(true) = %d, want one of {0, 1, 2, 4, 6}", s, got)
		}
	}
}

// Exit and Outcome classify the same status independently -- Exit at its bit
// tests, Outcome at its switch -- with nothing binding the two. This pins them
// together so they cannot drift into disagreeing about the same run, which
// would put one verdict on the `outcome=` line and a different one in the exit
// status.
func TestOutcomeAgreesWithExit(t *testing.T) {
	for _, s := range []Status{
		{}, {Changes: 1}, {Failures: 1}, {FailedRestarts: 1},
		{Changes: 1, Failures: 1}, {Changes: 1, FailedRestarts: 1},
		{Failures: 1, FailedRestarts: 1},
		{CouldNotRun: true}, {CouldNotRun: true, Changes: 1, Failures: 1},
	} {
		code := s.Exit(true)
		switch s.Outcome() {
		case OutcomeCouldNotRun:
			if code != 1 {
				t.Errorf("Status%+v: outcome could-not-run but Exit(true) = %d, want 1", s, code)
			}
		case OutcomeFailed:
			if code&4 == 0 {
				t.Errorf("Status%+v: outcome failed but Exit(true) = %d has no 0x4 bit", s, code)
			}
		case OutcomeChanged:
			if code&2 == 0 || code&4 != 0 {
				t.Errorf("Status%+v: outcome changed but Exit(true) = %d", s, code)
			}
		case OutcomeApplied:
			if code != 0 {
				t.Errorf("Status%+v: outcome applied but Exit(true) = %d, want 0", s, code)
			}
		}
	}
}

// An Outcome outside the four defined values must print as something obviously
// wrong. Measured by mutation: with no assertion here, changing the fallback to
// return "applied" left the suite green -- and a malformed verdict silently
// printing the most optimistic one is the worst available failure for a field
// whose whole purpose is to be read by a human six weeks later.
func TestOutcomeStringRejectsUnknownValues(t *testing.T) {
	if got := Outcome(99).String(); got != "Outcome(99)" {
		t.Errorf("Outcome(99).String() = %q, want %q", got, "Outcome(99)")
	}
}

func TestStatusOutcome(t *testing.T) {
	// Named by scenario rather than by expected value. Naming subtests by the
	// outcome gave three cases all called "failed", which Go dedups to
	// "failed", "failed#01", "failed#02" -- so a failure report identified the
	// case by an arbitrary index instead of by what it tests.
	tests := []struct {
		name   string
		status Status
		want   Outcome
		text   string
	}{
		{"nothing to do", Status{}, OutcomeApplied, "applied"},
		{"a change", Status{Changes: 1}, OutcomeChanged, "changed"},
		{"a resource failure", Status{Failures: 1}, OutcomeFailed, "failed"},
		{"a failed refresh", Status{FailedRestarts: 1}, OutcomeFailed, "failed"},
		// A failure outranks a change: the run is reported by its worst news.
		{"failure outranks change", Status{Changes: 9, Failures: 1}, OutcomeFailed, "failed"},
		// Each counter paired with CouldNotRun, mirroring TestStatusExit: a
		// mutation narrowing the guard to `s.CouldNotRun && s.FailedRestarts == 0`
		// survived when only Changes and Failures were covered here.
		{"could not run", Status{CouldNotRun: true}, OutcomeCouldNotRun, "could-not-run"},
		{"could not run after failures", Status{CouldNotRun: true, Failures: 9}, OutcomeCouldNotRun, "could-not-run"},
		{"could not run after failed refresh", Status{CouldNotRun: true, FailedRestarts: 1}, OutcomeCouldNotRun, "could-not-run"},
		{"could not run after everything", Status{CouldNotRun: true, Changes: 9, Failures: 9, FailedRestarts: 9}, OutcomeCouldNotRun, "could-not-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.status.Outcome()
			if got != tt.want {
				t.Errorf("Outcome() = %v, want %v", got, tt.want)
			}
			if got.String() != tt.text {
				t.Errorf("Outcome().String() = %q, want %q", got.String(), tt.text)
			}
		})
	}
}
