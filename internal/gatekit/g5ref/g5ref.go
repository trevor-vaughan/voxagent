// Package g5ref proves that G5's reference run exercised its fixture before
// the comparison is trusted.
//
// Comparing two reports answers "do the agents agree?", which is a different
// question from "was any package or exec work attempted?". If the container
// cannot reach a repository, dnf fails identically on both sides, the reports
// match to the byte, and the gate goes green having exercised nothing -- the
// contaminated-control shape recorded in docs/maintainers/apply-spine.md.
//
// It is asserted against RUBY alone, deliberately. Ruby is the reference, so
// Ruby getting this wrong means the fixture did not run and nothing was
// compared. If only voxagent got it wrong, that is a real divergence and the
// comparator is where it belongs. Asserting it on both sides would convert
// every voxagent package defect into "could not run".
//
// Values in complaints are shown with pycompat.Render. A report shape the
// Python original crashed on is an error here, not a complaint.
package g5ref

import (
	"fmt"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// Fixture names the four packages G5's catalog declares, one per behaviour.
type Fixture struct {
	Install, Remove, Latest, Missing string
}

// checker accumulates complaints about one reference report.
type checker struct {
	statuses   map[string]any
	complaints []string
}

func (c *checker) complain(format string, args ...any) {
	c.complaints = append(c.complaints, fmt.Sprintf(format, args...))
}

// status returns the status recorded for ref, or nil when the report has none
// -- absent or null alike, as Python's statuses.get(ref) is None for both.
func (c *checker) status(ref string) (map[string]any, error) {
	raw := c.statuses[ref]
	if raw == nil {
		return nil, nil
	}
	status, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource_statuses[%q] is %s, not an object", ref, pycompat.Shape(raw))
	}
	return status, nil
}

// eventsOf returns ref's events, or nil after complaining that ref is absent.
// Python's `status.get("events") or []`: a falsy value is no events, and a
// truthy one that is not a list of objects crashed it.
func (c *checker) eventsOf(ref string) ([]map[string]any, bool, error) {
	status, err := c.status(ref)
	if err != nil {
		return nil, false, err
	}
	if status == nil {
		c.complain("%s is absent from the reference report", ref)
		return nil, false, nil
	}
	raw := status["events"]
	if !pycompat.Truthy(raw) {
		return nil, true, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%s's events are %s, not a list", ref, pycompat.Shape(raw))
	}
	events := make([]map[string]any, len(list))
	for i, e := range list {
		event, ok := e.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("%s's events.%d is %s, not an object", ref, i, pycompat.Shape(e))
		}
		events[i] = event
	}
	return events, true, nil
}

func where(events []map[string]any, key, value string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if pycompat.Equal(e[key], value) {
			out = append(out, e)
		}
	}
	return out
}

// Check returns a complaint for every way the reference report shows the
// fixture did not run. No complaints means the comparison can be trusted. An
// error means the inputs are not a report and a catalog at all.
func Check(report, catalog any, f Fixture) ([]string, error) {
	doc, ok := report.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the reference report is %s, not an object", pycompat.Shape(report))
	}
	c := &checker{statuses: map[string]any{}}
	if raw, present := doc["resource_statuses"]; present {
		if c.statuses, ok = raw.(map[string]any); !ok {
			return nil, fmt.Errorf("resource_statuses is %s, not an object", pycompat.Shape(raw))
		}
	}
	declared, err := declaredRefs(catalog)
	if err != nil {
		return nil, err
	}

	for _, name := range []string{f.Install, f.Remove, f.Latest} {
		ref := "Package[" + name + "]"
		status, err := c.status(ref)
		switch {
		case err != nil:
			return nil, err
		case status == nil:
			c.complain("%s is absent from the reference report", ref)
		case !pycompat.Truthy(status["changed"]):
			c.complain("%s did not change on the reference side", ref)
		case !pycompat.Truthy(status["events"]):
			c.complain("%s recorded no event on the reference side", ref)
		}
	}

	missing := "Package[" + f.Missing + "]"
	status, err := c.status(missing)
	switch {
	case err != nil:
		return nil, err
	case status == nil:
		c.complain("%s is absent from the reference report", missing)
	case !pycompat.Truthy(status["failed"]):
		c.complain("%s did not fail, so no attribution was exercised", missing)
	}

	// Each exec fixture is asserted to have reached the shape it was written
	// for. Two reports can agree perfectly about an exec that never ran, and
	// the failure mode is silent: a guard that stopped skipping, or a command
	// that stopped failing, leaves the gate green while the surface it exists
	// to compare -- the event name, the failing property, the accepted list --
	// goes unexercised.
	if events, ok, err := c.eventsOf("Exec[spine-exec-ok]"); err != nil {
		return nil, err
	} else if ok {
		names := make([]any, len(events))
		for i, e := range events {
			names[i] = e["name"]
		}
		if !pycompat.Equal(names, []any{"executed_command"}) {
			c.complain("Exec[spine-exec-ok] did not record exactly one executed_command event "+
				"on the reference side, but %s", pycompat.Render(names))
		}
	}

	if events, ok, err := c.eventsOf("Exec[spine-exec-fail]"); err != nil {
		return nil, err
	} else if ok {
		failures := where(events, "status", "failure")
		if len(failures) != 1 {
			c.complain("Exec[spine-exec-fail] did not record exactly one failure event on " +
				"the reference side, so the attribution was not exercised")
		} else if !pycompat.Equal(failures[0]["property"], "returns") {
			c.complain("Exec[spine-exec-fail]'s reference failure is attributed to "+
				"%s, not to returns", pycompat.Render(failures[0]["property"]))
		}
	}

	if events, ok, err := c.eventsOf("Exec[spine-exec-returns]"); err != nil {
		return nil, err
	} else if ok {
		accepted := where(events, "status", "success")
		if len(accepted) != 1 {
			c.complain("Exec[spine-exec-returns] did not succeed on the reference side, so " +
				"an accepted non-zero status was not exercised")
		} else {
			desired := accepted[0]["desired_value"]
			entries := 0
			if pycompat.Truthy(desired) {
				if entries, err = pycompat.Len(desired); err != nil {
					return nil, fmt.Errorf("Exec[spine-exec-returns]'s desired_value: %w", err)
				}
			}
			if entries < 2 {
				c.complain("Exec[spine-exec-returns]'s reference desired_value is "+
					"%s, so the multi-entry accepted list was not exercised", pycompat.Render(desired))
			}
		}
	}

	// The refresh is asserted through its EVENT, not through `restarted`.
	// resource/status.rb lists restarted in STATES and the transaction sets
	// it, but to_data_hash never writes it -- it reaches a report only as the
	// resources.restarted metric. The callback event is the observable proof:
	// event_manager.rb's add_callback_status_event builds it with a name and
	// no property at all.
	if events, ok, err := c.eventsOf("Exec[spine-exec-refresh]"); err != nil {
		return nil, err
	} else if ok {
		refreshes := where(events, "name", "refresh")
		if len(refreshes) != 1 {
			c.complain("Exec[spine-exec-refresh] has no refresh event on the reference " +
				"side, so refresh propagation was not exercised")
		} else if property := refreshes[0]["property"]; property != nil {
			c.complain("Exec[spine-exec-refresh]'s reference refresh event carries "+
				"property %s, so it is not the "+
				"no-property callback event this fixture exists to compare", pycompat.Render(property))
		}
	}

	// The guarded exec's assertion is its ABSENCE, and absence is only
	// evidence if the resource was declared. Puppet[:exclude_unchanged_resources]
	// defaults true, and report.rb:326 drops every status that is none of
	// failed, failed_to_restart, out_of_sync or skipped -- so a resource whose
	// guard skipped leaves the data hash entirely. Reading the catalog is what
	// tells a skipped guard apart from a misspelled title that was never in
	// the catalog to begin with.
	const guarded = "Exec[spine-exec-guarded]"
	if !declared[guarded] {
		c.complain("%s is not in the compiled catalog, so its absence from the "+
			"report is evidence of nothing", guarded)
	} else if kept, err := c.status(guarded); err != nil {
		return nil, err
	} else if kept != nil {
		events := 0
		if raw := kept["events"]; pycompat.Truthy(raw) {
			if events, err = pycompat.Len(raw); err != nil {
				return nil, fmt.Errorf("%s's events: %w", guarded, err)
			}
		}
		c.complain("%s is PRESENT in the reference report "+
			"(changed=%s, failed=%s, out_of_sync=%s, %d event(s)), so its guard did not "+
			"skip the command", guarded, pycompat.Render(kept["changed"]),
			pycompat.Render(kept["failed"]), pycompat.Render(kept["out_of_sync"]), events)
	}
	return c.complaints, nil
}

// declaredRefs returns "Type[title]" for every resource in the compiled
// catalog. Only string types and titles can spell a reference this checks, so
// a resource with any other is skipped rather than rendered.
func declaredRefs(catalog any) (map[string]bool, error) {
	doc, ok := catalog.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the compiled catalog is %s, not an object", pycompat.Shape(catalog))
	}
	declared := map[string]bool{}
	raw, present := doc["resources"]
	if !present {
		return declared, nil
	}
	resources, ok := raw.([]any)
	if !ok {
		// Python iterated whatever it found. An empty string or object
		// yields nothing and passed; anything else crashed.
		if n, err := pycompat.Len(raw); err == nil && n == 0 {
			return declared, nil
		}
		return nil, fmt.Errorf("the compiled catalog's resources are %s, not a list", pycompat.Shape(raw))
	}
	for i, r := range resources {
		resource, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the compiled catalog's resources.%d is %s, not an object", i, pycompat.Shape(r))
		}
		typ, isString := resource["type"].(string)
		title, titleIsString := resource["title"].(string)
		if isString && titleIsString {
			declared[typ+"["+title+"]"] = true
		}
	}
	return declared, nil
}
