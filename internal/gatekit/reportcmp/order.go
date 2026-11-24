package reportcmp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// statusPrefixes maps each resource status's log-source prefix
// ("/" + containment_path joined by "/") to its reference.
func statusPrefixes(report any) (map[string]string, error) {
	doc, ok := report.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the report is %s, not an object", pycompat.Shape(report))
	}
	raw, ok := doc["resource_statuses"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource_statuses is %s, not an object", pycompat.Shape(doc["resource_statuses"]))
	}
	prefixes := make(map[string]string, len(raw))
	for ref, st := range raw {
		status, ok := st.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource_statuses[%q] is %s, not an object", ref, pycompat.Shape(st))
		}
		path, ok := status["containment_path"].([]any)
		if !ok || len(path) == 0 {
			return nil, fmt.Errorf("resource_statuses[%q].containment_path is %s, not a non-empty list", ref, pycompat.Shape(status["containment_path"]))
		}
		parts := make([]string, len(path))
		for i, p := range path {
			if parts[i], ok = p.(string); !ok {
				return nil, fmt.Errorf("resource_statuses[%q].containment_path.%d is %s, not a string", ref, i, pycompat.Shape(p))
			}
		}
		prefix := "/" + strings.Join(parts, "/")
		if other, dup := prefixes[prefix]; dup {
			first, second := ref, other
			if second < first {
				first, second = second, first
			}
			return nil, fmt.Errorf("resource_statuses %q and %q share the containment path %q", first, second, prefix)
		}
		prefixes[prefix] = ref
	}
	return prefixes, nil
}

// WalkOrder is the order a report's resources were evaluated in: each
// status, in order of its first log line. logging is the number of statuses
// that got at least one log.
//
// A log belongs to the status whose prefix equals its source, or is followed
// in the source by "/"; the longest such prefix wins, so a log for File[/x]
// is never given to File[/x/y]. Logs matching no status (source "Puppet", a
// whit label) are ignored.
func WalkOrder(report any) (order []string, logging int, err error) {
	order, _, err = walk(report)
	return order, len(order), err
}

// walk is WalkOrder's order together with the set of log sources no status
// owns.
func walk(report any) (order []string, unowned map[string]bool, err error) {
	prefixes, err := statusPrefixes(report)
	if err != nil {
		return nil, nil, err
	}
	logs, err := logSources(report)
	if err != nil {
		return nil, nil, err
	}
	seen, unowned := map[string]bool{}, map[string]bool{}
	for _, source := range logs {
		ref, ok := owner(source, prefixes)
		if !ok {
			unowned[source] = true
			continue
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		order = append(order, ref)
	}
	return order, unowned, nil
}

// logSources returns the source of every log, in array order.
func logSources(report any) ([]string, error) {
	raw, present := report.(map[string]any)["logs"]
	if !present {
		return nil, nil
	}
	logs, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("logs is %s, not a list", pycompat.Shape(raw))
	}
	sources := make([]string, len(logs))
	for i, e := range logs {
		entry, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("logs.%d is %s, not an object", i, pycompat.Shape(e))
		}
		if v, present := entry["source"]; present {
			if sources[i], ok = v.(string); !ok {
				return nil, fmt.Errorf("logs.%d.source is %s, not a string", i, pycompat.Shape(v))
			}
		}
	}
	return sources, nil
}

// owner finds the status for a log source by trying the source and then each
// shorter prefix cut at a "/", longest first.
func owner(source string, prefixes map[string]string) (string, bool) {
	for {
		if ref, ok := prefixes[source]; ok {
			return ref, true
		}
		cut := strings.LastIndex(source, "/")
		if cut <= 0 {
			return "", false
		}
		source = source[:cut]
	}
}

// restrict keeps the entries of order that are also in keep.
func restrict(order []string, keep map[string]bool) []string {
	var out []string
	for _, ref := range order {
		if keep[ref] {
			out = append(out, ref)
		}
	}
	return out
}

// refSet is the set of refs a prefix map holds.
func refSet(prefixes map[string]string) map[string]bool {
	s := make(map[string]bool, len(prefixes))
	for _, ref := range prefixes {
		s[ref] = true
	}
	return s
}

func set(refs []string) map[string]bool {
	s := make(map[string]bool, len(refs))
	for _, r := range refs {
		s[r] = true
	}
	return s
}

// CompareOrder compares the two walks over the resources both report.
//
// A status only one report has is a divergence, never a thin comparison:
// thinness must not hide a real difference. Nor is a status both report but
// only one logged for: both sides are attributed by the same rule, so that is
// a log divergence, unless the side without the log has a log source that no
// status owns and the other side does not also leave unowned (as both leave
// "Puppet"). That source may be the missing log, so attribution failed there
// and the comparison is thin. A thin comparison is an error, because it
// proves nothing either way. Comparing nothing at all is thin too: it would
// agree vacuously.
func CompareOrder(ruby, vox any) (agree bool, compared int, detail string, err error) {
	rubyStatuses, err := statusPrefixes(ruby)
	if err != nil {
		return false, 0, "", fmt.Errorf("the ruby report: %w", err)
	}
	voxStatuses, err := statusPrefixes(vox)
	if err != nil {
		return false, 0, "", fmt.Errorf("the voxagent report: %w", err)
	}
	if side, ref := firstUnshared(refSet(rubyStatuses), refSet(voxStatuses)); side != "" {
		return false, 0, fmt.Sprintf("status %s reported only by %s", ref, side), nil
	}
	rubyOrder, rubyUnowned, err := walk(ruby)
	if err != nil {
		return false, 0, "", fmt.Errorf("the ruby report: %w", err)
	}
	voxOrder, voxUnowned, err := walk(vox)
	if err != nil {
		return false, 0, "", fmt.Errorf("the voxagent report: %w", err)
	}

	rubyLogged, voxLogged := set(rubyOrder), set(voxOrder)
	rubySeq, voxSeq := restrict(rubyOrder, voxLogged), restrict(voxOrder, rubyLogged)
	compared = len(rubySeq)
	for i := range rubySeq {
		if rubySeq[i] != voxSeq[i] {
			return false, compared, divergence(i, rubySeq, voxSeq), nil
		}
	}
	for _, c := range []struct {
		side        string
		order       []string
		otherLogged map[string]bool
		strayOther  bool
	}{
		{"ruby", rubyOrder, voxLogged, hasStray(voxUnowned, rubyUnowned)},
		{"voxagent", voxOrder, rubyLogged, hasStray(rubyUnowned, voxUnowned)},
	} {
		if c.strayOther {
			continue
		}
		for _, ref := range c.order {
			if !c.otherLogged[ref] {
				return false, compared, fmt.Sprintf("%s logged only by %s", ref, c.side), nil
			}
		}
	}
	if logging := max(len(rubyOrder), len(voxOrder)); compared < logging {
		return false, compared, "", fmt.Errorf("compared %d of %d logging resources; attribution failed", compared, logging)
	}
	if compared == 0 {
		return false, 0, "", fmt.Errorf("compared 0 of 0 logging resources; nothing to compare")
	}
	return true, compared, "", nil
}

// hasStray reports whether unowned holds a source that other does not.
func hasStray(unowned, other map[string]bool) bool {
	for source := range unowned {
		if !other[source] {
			return true
		}
	}
	return false
}

// firstUnshared names the side that reports a status the other lacks, and the
// alphabetically first such ref; side is "" when both report the same.
func firstUnshared(ruby, vox map[string]bool) (side, ref string) {
	for _, c := range []struct {
		name        string
		have, other map[string]bool
	}{{"ruby", ruby, vox}, {"voxagent", vox, ruby}} {
		var missing []string
		for r := range c.have {
			if !c.other[r] {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return c.name, missing[0]
		}
	}
	return "", ""
}

// divergence describes the first mismatch and the three refs before it.
func divergence(i int, ruby, vox []string) string {
	before := func(seq []string) string {
		return strings.Join(seq[max(0, i-3):i], ", ")
	}
	return fmt.Sprintf("walk order diverges at %d: ruby %s, voxagent %s\n  ruby before: %s\n  voxagent before: %s",
		i, ruby[i], vox[i], before(ruby), before(vox))
}
