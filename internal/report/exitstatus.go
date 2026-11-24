// Package report builds the report a run emits and maps that run to a process
// exit status.
package report

import "fmt"

// Outcome is the category a run falls into, independent of which exit-code
// convention the caller asked for.
//
// It exists because the meaning of a bare exit integer depends on whether
// --detailed-exitcodes was passed, and an exit code is read out of band from
// the invocation that produced it -- by a monitoring wrapper, or by a human
// reading a log six weeks later. Every run therefore prints its outcome by
// name, so nobody has to know the argv to interpret the status.
type Outcome int

const (
	// OutcomeApplied: the run completed and the node was already in the
	// declared state.
	OutcomeApplied Outcome = iota
	// OutcomeChanged: the run completed and changed something.
	//
	// NOT set for would-be changes under --noop. Measured against Puppet
	// 8.10.0: `--noop --detailed-exitcodes` over a file that would be created
	// exits 0, not 2. resource/status.rb:122-133 increments change_count only
	// for events whose status is 'success'; a noop event's status is 'noop', so
	// it reaches out_of_sync_count and never the changes metric that the 0x2
	// bit is computed from. Puppet carries would-be changes in a separate
	// channel, the report's noop_pending field, which is why they do not reach
	// the bitmask at all.
	//
	// voxagent matches that rather than diverging, for the same reason plain
	// mode reports no verdict: the exit code is not the only channel, and
	// overriding an inherited contract to be "more informative" is what breaks
	// drop-in compatibility. A drift check wanting would-be changes reads
	// noop_pending.
	OutcomeChanged
	// OutcomeFailed: the run completed with at least one resource failure or
	// one failed refresh.
	OutcomeFailed
	// OutcomeCouldNotRun: the run produced no verdict about the node.
	OutcomeCouldNotRun
)

// String returns the name printed on the `outcome=` line.
func (o Outcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeChanged:
		return "changed"
	case OutcomeFailed:
		return "failed"
	case OutcomeCouldNotRun:
		return "could-not-run"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Status is a run reduced to the counts the exit contract needs.
//
// CouldNotRun outranks the counts. A run that produced no verdict has no
// meaningful change count whatever it managed to do before it stopped, and
// reporting one would be a claim about the node the run is not entitled to
// make.
type Status struct {
	CouldNotRun bool
	Changes     int
	Failures    int
	// FailedRestarts is Puppet's failed_to_restart: the second and independent
	// clause of the 0x4 bit at transaction/report.rb:447. It counts refreshes
	// that FAILED, never refreshes that happened -- a successful refresh is an
	// ordinary change. Populating this from a refresh counter would turn every
	// notify-triggered reload into exit 4, so the name says "failed" rather
	// than leaving a comment to negate it.
	//
	// Only a refreshable resource type can produce one, which is why the spine
	// implements exec at all.
	FailedRestarts int
}

// Outcome categorises the status, worst news first.
func (s Status) Outcome() Outcome {
	switch {
	case s.CouldNotRun:
		return OutcomeCouldNotRun
	case s.Failures > 0 || s.FailedRestarts > 0:
		return OutcomeFailed
	case s.Changes > 0:
		return OutcomeChanged
	default:
		return OutcomeApplied
	}
}

// Exit maps the status to a process exit status.
//
// Two conventions meet here and they assign opposite meanings to 2: this
// repository's gates use 2 for "could not run", and Puppet's bitmask uses 2 for
// "changes were made". The resolution rests on a measurement rather than a
// preference. Measured against Puppet 8.10.0:
//
//	                        plain   --detailed-exitcodes
//	changes made              0      2
//	a resource failed         0      4
//
// Plain `puppet apply` exits 0 even when resources fail, so its exit status
// carries no verdict at all -- it reports only that the tool ran, and
// --detailed-exitcodes is the verdict channel. There is therefore nothing to
// reconcile in plain mode: it was never making a claim that could be falsified.
//
//	outcome                 plain   --detailed-exitcodes
//	completed, no changes     0      0
//	completed, changes        0      2
//	completed, failures       0      4 or 6
//	could not run             2      1
//
// Every RESULT row matches measured Puppet exactly, which is what drop-in
// compatibility depends on: a wrapper reading the status of a run that
// completed sees what it saw before.
//
// The could-not-run row does NOT match, and saying "compatible on both paths"
// would overclaim. Measured: Puppet exits 1 for could-not-run in BOTH modes --
// apply.rb:281 and :289 sit above the detailed_exitcodes branch, so a syntax
// error or a missing catalog file exits 1 with or without the flag. voxagent's
// plain mode returns 2 instead, matching voxdiff and the fleet gates. That is a
// deliberate divergence and is registered as D-3; a wrapper testing `exit == 1`
// for could-not-run needs to know. Under the flag there is no divergence,
// because 2 is taken by the changes bit and 1 is free.
//
// 255 is never emitted. report.rb:449 returns -1 when the metrics are absent,
// which Ruby's exit(-1) surfaces as 255 -- and only under the flag, since plain
// mode discards the value at apply.rb:285. voxagent calls that could-not-run
// instead: divergence D-2.
func (s Status) Exit(detailed bool) int {
	if s.CouldNotRun {
		if detailed {
			return 1
		}
		return 2
	}
	if !detailed {
		return 0
	}
	code := 0
	if s.Changes > 0 {
		code |= 2
	}
	if s.Failures > 0 || s.FailedRestarts > 0 {
		code |= 4
	}
	return code
}
