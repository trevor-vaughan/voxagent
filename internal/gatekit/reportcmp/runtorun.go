package reportcmp

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// RunToRun checks the allowlist's run_to_run patterns against two reports from
// the same implementation, writing its analysis to w. It returns true when
// every differing leaf is covered and no pattern suppresses a stable one, and
// an error when the check could not run.
//
// A pattern's coverage is reported both ways round. Matching differing leaves
// is what it is for; matching stable ones is what makes it a blind spot, and
// each entry is only defensible if the second count is zero.
//
// The leaves are the ones Compare sees before its failure-detail elision and
// its Ruby-only log drop, which is what the Python check used.
func RunToRun(first, second any, allow Allowlist, w io.Writer) (bool, error) {
	firstLeaves, err := runLeaves(first, "first")
	if err != nil {
		return false, err
	}
	secondLeaves, err := runLeaves(second, "second")
	if err != nil {
		return false, err
	}
	if len(allow.RunToRun) == 0 {
		return false, errors.New("the allowlist has no run_to_run patterns")
	}
	patterns, err := compile(allow.RunToRun)
	if err != nil {
		return false, err
	}

	every := unionPaths(firstLeaves, secondLeaves)
	var differing, stable []string
	shapes := map[string]bool{}
	for _, p := range every {
		a, inFirst := firstLeaves[p]
		b, inSecond := secondLeaves[p]
		if leavesEqual(a, inFirst, b, inSecond) {
			stable = append(stable, p)
		} else {
			differing = append(differing, p)
			shapes[generalise(p)] = true
		}
	}
	fmt.Fprintf(w, "%d leaves, %d differing across %d raw field shapes\n\n", len(every), len(differing), len(shapes))

	fmt.Fprintln(w, "PATTERN COVERAGE")
	// Ordered and de-duplicated by pattern text, as the Python dict was: a
	// pattern listed twice is reported once, at its first position.
	var overBroad []string
	suppressedBy := map[string][]string{}
	for _, p := range patterns {
		one := []pattern{p}
		var hit, suppressed []string
		for _, path := range differing {
			if ignored(path, one) {
				hit = append(hit, path)
			}
		}
		for _, path := range stable {
			if ignored(path, one) {
				suppressed = append(suppressed, path)
			}
		}
		label := "ok"
		switch {
		case len(suppressed) > 0:
			if _, seen := suppressedBy[p.text]; !seen {
				overBroad = append(overBroad, p.text)
			}
			suppressedBy[p.text] = suppressed
			label = "OVER-BROAD"
		case len(hit) == 0:
			label = "UNUSED"
		}
		fmt.Fprintf(w, "  %-11s %-40s %d differing, %d stable\n", label, p.text, len(hit), len(suppressed))
		for i, path := range suppressed {
			if i == 4 {
				fmt.Fprintf(w, "%14s... and %d more\n", "", len(suppressed)-4)
				break
			}
			v, present := firstLeaves[path]
			fmt.Fprintf(w, "%14salso suppresses: %s = %s\n", "", path, show(v, present))
		}
	}

	var uncovered []string
	for _, path := range differing {
		if !ignored(path, patterns) {
			uncovered = append(uncovered, path)
		}
	}
	fmt.Fprintln(w)

	var problems []string
	if len(uncovered) > 0 {
		problems = append(problems, fmt.Sprintf("%d differing leaf/leaves covered by no pattern", len(uncovered)))
		fmt.Fprintln(w, "INCOMPLETE -- these vary run to run and the differential will report them as divergences:")
		for _, path := range uncovered {
			a, inFirst := firstLeaves[path]
			b, inSecond := secondLeaves[path]
			fmt.Fprintf(w, "  %s\n    run1: %s\n    run2: %s\n", path, show(a, inFirst), show(b, inSecond))
		}
	}
	if len(overBroad) > 0 {
		problems = append(problems, fmt.Sprintf("%d over-broad pattern(s)", len(overBroad)))
		fmt.Fprintln(w, "OVER-BROAD -- these patterns hide leaves that do NOT vary, so the differential will never compare them:")
		for _, text := range overBroad {
			fmt.Fprintf(w, "  %s  (%d stable leaves suppressed)\n", text, len(suppressedBy[text]))
		}
	}
	if len(problems) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "run-to-run: "+strings.Join(problems, "; "))
		return false, nil
	}
	fmt.Fprintf(w, "run-to-run: %d patterns cover %d differing leaves and nothing else\n", len(patterns), len(differing))
	return true, nil
}

// runLeaves is the leaf set the differential will compare, metrics included.
func runLeaves(doc any, which string) (map[string]any, error) {
	m, err := asObject(doc, which)
	if err != nil {
		return nil, err
	}
	m, err = normaliseMetrics(m)
	if err != nil {
		return nil, fmt.Errorf("the %s report: %w", which, err)
	}
	leaves, err := flatten(normaliseLogs(m))
	if err != nil {
		return nil, fmt.Errorf("the %s report: %w", which, err)
	}
	return leaves, nil
}
