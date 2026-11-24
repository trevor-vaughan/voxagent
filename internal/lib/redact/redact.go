// Package redact scrubs the values a catalog marks Sensitive out of text. It
// is the Go twin of VoxagentHost.forms and VoxagentHost.redact in
// internal/extension/host.rb; testdata/vectors.json holds the cases both run.
package redact

import (
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

const marker = "[redacted]"

// shortForm is the longest form, in bytes, that matches only as a whole token.
const shortForm = 3

// Scrubber replaces every printed form of a Sensitive value in text. The zero
// value and a nil *Scrubber scrub nothing.
type Scrubber struct {
	forms []string
}

// New collects the forms of every value of every parameter each resource lists
// in SensitiveParameters. A listed name absent from Parameters contributes
// nothing.
func New(cat *catalog.Catalog) *Scrubber {
	var all []string
	for _, r := range cat.Resources {
		for _, name := range r.SensitiveParameters {
			if v, ok := r.Parameters[name]; ok {
				all = append(all, forms(v)...)
			}
		}
	}
	return fromForms(all)
}

func fromForms(all []string) *Scrubber {
	s := &Scrubber{}
	seen := map[string]bool{}
	for _, f := range all {
		if f != "" && !seen[f] {
			seen[f] = true
			s.forms = append(s.forms, f)
		}
	}
	return s
}

// forms is every non-empty string a Sensitive value can print as: Puppet
// formats a value with to_s or inspect, and an Array or Hash's values each the
// same way. An empty collection's text carries no secret; a Hash's keys are not
// forms, and its own text is omitted because map order is lost.
func forms(v any) []string {
	var out []string
	add := func(f string) {
		if f != "" && f != "[]" && f != "{}" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	switch v := v.(type) {
	case nil:
	case string:
		add(v)
		add(rubyvalue.Inspect(v))
	case []any:
		add(rubyvalue.InspectValue(v))
		for _, e := range v {
			for _, f := range forms(e) {
				add(f)
			}
		}
	case map[string]any:
		for _, e := range v {
			for _, f := range forms(e) {
				add(f)
			}
		}
	default:
		add(rubyvalue.InspectValue(v))
	}
	return out
}

// Scrub replaces every form in text with [redacted]. It finds every match of
// every form, overlapping ones too, and replaces each run of overlapping or
// adjacent matches with one marker, so no fragment of a secret is left. It
// matches bytes and leaves the rest of text as it is, so invalid UTF-8
// elsewhere cannot hide a secret. A form of 3 bytes or fewer matches only as a
// whole token. Text with no match is returned as it is, without allocating.
func (s *Scrubber) Scrub(text string) string {
	if s == nil || len(s.forms) == 0 {
		return text
	}
	var spans [][2]int
	for _, form := range s.forms {
		for from := 0; from <= len(text); {
			i := strings.Index(text[from:], form)
			if i < 0 {
				break
			}
			start := from + i
			end := start + len(form)
			if len(form) > shortForm || (!wordByteAt(text, start-1) && !wordByteAt(text, end)) {
				spans = append(spans, [2]int{start, end})
			}
			from = start + 1
		}
	}
	if len(spans) == 0 {
		return text
	}
	slices.SortFunc(spans, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	// A span joins the run while it starts at or before the run's furthest end,
	// so a span nested in a longer one cannot cut the run short.
	var runs [][2]int
	for _, span := range spans {
		if n := len(runs); n > 0 && span[0] <= runs[n-1][1] {
			runs[n-1][1] = max(runs[n-1][1], span[1])
		} else {
			runs = append(runs, span)
		}
	}
	var out strings.Builder
	done := 0
	for _, run := range runs {
		out.WriteString(text[done:run[0]])
		out.WriteString(marker)
		done = run[1]
	}
	out.WriteString(text[done:])
	return out.String()
}

// wordByteAt reports whether src[i] exists and is in [A-Za-z0-9_].
func wordByteAt(src string, i int) bool {
	if i < 0 || i >= len(src) {
		return false
	}
	c := src[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
