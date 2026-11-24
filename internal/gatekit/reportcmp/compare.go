// Package reportcmp compares two report_format 12 documents over a measured
// allowlist, and checks that allowlist's run_to_run half against two reports
// from the same implementation.
//
// It is the judge in the report differentials, so it must not share code with
// what it judges: it reads reports as decoded JSON and never imports voxagent's
// report, transaction or ral packages. A struct shared with the code under test
// would let a bug in that struct agree with itself. TestJudgeImportsNothingItJudges
// enforces this.
//
// Two allowlists, kept separate on purpose. run_to_run holds the fields that
// differ between two runs of the SAME implementation, measured by
// tests/spine/derive-run-to-run.sh. cross_implementation holds fields that
// legitimately differ between Ruby and Go, each with its reason as the value.
// Merging them would let a real cross-implementation divergence hide behind
// "it varies anyway".
//
// Pattern semantics are fnmatch's, ported in pycompat: `*` crosses dots. That
// is load-bearing -- a resource reference is itself full of dots and slashes,
// so `resource_statuses.*.time` can only match
// `resource_statuses.File[/etc/hosts].time` because the wildcard spans them.
//
// This is a port of a Python comparator and reaches its verdicts, except in
// three places where they were wrong: log entries are compared by count, not
// membership; a bool equals only a bool; and an absent leaf is compared by its
// absence, not as the string "<absent>".
package reportcmp

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// absent is how a leaf one side does not have is shown. It is only ever
// displayed: leavesEqual compares absence as absence, so a present leaf holding
// this string still diverges from an absent one.
const absent = "<absent>"

// Allowlist is the decoded allowlist.json.
type Allowlist struct {
	RunToRun            []string
	CrossImplementation []string // the keys; the values are reasons
}

// ParseAllowlist reads the two pattern lists out of a decoded allowlist. A
// missing list is empty. A list of the wrong shape is refused: Python would
// have crashed on most of them, and on the rest -- a string where a list
// belongs -- would have iterated its characters as patterns.
func ParseAllowlist(doc any) (Allowlist, error) {
	m, ok := doc.(map[string]any)
	if !ok {
		return Allowlist{}, fmt.Errorf("the allowlist is %s, not an object", pycompat.Shape(doc))
	}
	var allow Allowlist
	if raw, present := m["run_to_run"]; present {
		list, ok := raw.([]any)
		if !ok {
			return Allowlist{}, fmt.Errorf("run_to_run is %s, not a list", pycompat.Shape(raw))
		}
		for _, p := range list {
			s, ok := p.(string)
			if !ok {
				return Allowlist{}, fmt.Errorf("run_to_run holds %s, not a string pattern", pycompat.Shape(p))
			}
			allow.RunToRun = append(allow.RunToRun, s)
		}
	}
	if raw, present := m["cross_implementation"]; present {
		obj, ok := raw.(map[string]any)
		if !ok {
			return Allowlist{}, fmt.Errorf("cross_implementation is %s, not an object", pycompat.Shape(raw))
		}
		for k := range obj {
			allow.CrossImplementation = append(allow.CrossImplementation, k)
		}
		sort.Strings(allow.CrossImplementation)
	}
	return allow, nil
}

// pattern is one compiled allowlist entry.
type pattern struct {
	text string
	re   *regexp.Regexp
}

func compile(texts []string) ([]pattern, error) {
	out := make([]pattern, len(texts))
	for i, t := range texts {
		re, err := pycompat.CompileFnmatch(t)
		if err != nil {
			return nil, err
		}
		out[i] = pattern{text: t, re: re}
	}
	return out, nil
}

// generalise collapses every all-digit path segment to "*", so a pattern can
// name "any index" without the allowlist enumerating them. "Digit" is Python's
// str.isdigit(), which accepts more than ASCII.
func generalise(path string) string {
	segments := strings.Split(path, ".")
	for i, s := range segments {
		if pycompat.IsDigit(s) {
			segments[i] = "*"
		}
	}
	return strings.Join(segments, ".")
}

// ignored reports whether path, or path with its numeric indices collapsed,
// matches any of patterns.
func ignored(path string, patterns []pattern) bool {
	general := generalise(path)
	for _, p := range patterns {
		if p.re.MatchString(path) || p.re.MatchString(general) {
			return true
		}
	}
	return false
}

// flatten maps every leaf of doc to its dotted path. An empty object or list
// has no leaves and so vanishes, as it did in Python. Two leaves that flatten
// to the same path are refused: Python kept whichever came later in the
// document and silently dropped the other.
func flatten(doc map[string]any) (map[string]any, error) {
	leaves := map[string]any{}
	var walk func(node any, path string) error
	walk = func(node any, path string) error {
		switch x := node.(type) {
		case map[string]any:
			for k, v := range x {
				child := k
				// Python's `f"{path}.{key}" if path else key`: the test is on
				// the path being empty, not on being at the top.
				if path != "" {
					child = path + "." + k
				}
				if err := walk(v, child); err != nil {
					return err
				}
			}
		case []any:
			for i, v := range x {
				if err := walk(v, path+"."+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		default:
			if _, dup := leaves[path]; dup {
				return fmt.Errorf("two leaves flatten to the path %q, so one would be silently dropped", path)
			}
			leaves[path] = node
		}
		return nil
	}
	if err := walk(doc, ""); err != nil {
		return nil, err
	}
	return leaves, nil
}

// logKey is the part of a log entry that carries meaning across
// implementations.
type logKey struct{ source, message, level string }

func (k logKey) String() string {
	return "(" + strconv.Quote(k.source) + ", " + strconv.Quote(k.message) + ", " + strconv.Quote(k.level) + ")"
}

func (k logKey) less(o logKey) bool {
	if k.source != o.source {
		return k.source < o.source
	}
	if k.message != o.message {
		return k.message < o.message
	}
	return k.level < o.level
}

// sortedLogKeys returns the (source, message, level) of every log entry,
// sorted, so logs compare as a multiset. A missing field is "". An entry that
// is not an object, or a field that is not a string, is refused: Python
// crashed on the first, and on the second crashed or not depending on what it
// happened to be sorted against.
func sortedLogKeys(doc map[string]any, side string) ([]logKey, error) {
	raw, present := doc["logs"]
	if !present {
		return nil, nil
	}
	logs, ok := raw.([]any)
	if !ok {
		// Python iterated whatever it found. An empty string or object yields
		// nothing and passed; anything else crashed.
		if l, err := pycompat.Len(raw); err == nil && l == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("the %s report's logs are %s, not a list", side, pycompat.Shape(raw))
	}
	keys := make([]logKey, len(logs))
	for i, e := range logs {
		entry, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the %s report's logs.%d is %s, not an object", side, i, pycompat.Shape(e))
		}
		var fields [3]string
		for j, name := range []string{"source", "message", "level"} {
			v, present := entry[name]
			if !present {
				continue
			}
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("the %s report's logs.%d.%s is %s, not a string", side, i, name, pycompat.Shape(v))
			}
			fields[j] = s
		}
		keys[i] = logKey{fields[0], fields[1], fields[2]}
	}
	sort.SliceStable(keys, func(a, b int) bool { return keys[a].less(keys[b]) })
	return keys, nil
}

// Divergence is one field on which the two reports disagree, with each side's
// value already rendered for display.
type Divergence struct {
	Path, Ruby, Voxagent string
}

// Result is the outcome of a comparison.
type Result struct {
	Compared    int // leaves of the Ruby report outside the allowlist
	Divergences []Divergence
}

// Write prints the result in the comparator's established format.
func (r Result) Write(w io.Writer) {
	fmt.Fprintf(w, "%d leaves compared, %d divergent\n", r.Compared, len(r.Divergences))
	for _, d := range r.Divergences {
		fmt.Fprintf(w, "  %s\n    ruby:     %s\n    voxagent: %s\n", d.Path, d.Ruby, d.Voxagent)
	}
}

func show(v any, present bool) string {
	if !present {
		return absent
	}
	return pycompat.Render(v)
}

// asObject is the top-level check both sides need: Python called .get on the
// document and crashed when it was anything but an object.
func asObject(doc any, side string) (map[string]any, error) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the %s report is %s, not an object", side, pycompat.Shape(doc))
	}
	return m, nil
}

// normalise applies the normalisations both reports receive.
func normalise(doc map[string]any) (map[string]any, error) {
	doc, err := normaliseMetrics(doc)
	if err != nil {
		return nil, err
	}
	return normaliseFailureDetails(normaliseLogs(doc)), nil
}

// Compare reports every leaf on which ruby and voxagent disagree outside the
// allowlist, and every log entry only one of them has. An error means the
// comparison could not run.
func Compare(ruby, voxagent any, allow Allowlist) (Result, error) {
	rubyDoc, err := asObject(ruby, "Ruby")
	if err != nil {
		return Result{}, err
	}
	goDoc, err := asObject(voxagent, "voxagent")
	if err != nil {
		return Result{}, err
	}

	// logs is compared as a multiset rather than a sequence, because voxagent
	// commits to its own deterministic walk order rather than reproducing
	// Puppet's. Order is not a divergence; content is.
	texts := append(append(append([]string{}, allow.RunToRun...), allow.CrossImplementation...), "logs.*")
	patterns, err := compile(texts)
	if err != nil {
		return Result{}, err
	}

	if rubyDoc, err = normalise(dropWrappedExceptionLogs(rubyDoc)); err != nil {
		return Result{}, fmt.Errorf("the Ruby report: %w", err)
	}
	if goDoc, err = normalise(goDoc); err != nil {
		return Result{}, fmt.Errorf("the voxagent report: %w", err)
	}

	rubyLogs, err := sortedLogKeys(rubyDoc, "Ruby")
	if err != nil {
		return Result{}, err
	}
	goLogs, err := sortedLogKeys(goDoc, "voxagent")
	if err != nil {
		return Result{}, err
	}

	rubyLeaves, err := comparedLeaves(rubyDoc, patterns)
	if err != nil {
		return Result{}, fmt.Errorf("the Ruby report: %w", err)
	}
	goLeaves, err := comparedLeaves(goDoc, patterns)
	if err != nil {
		return Result{}, fmt.Errorf("the voxagent report: %w", err)
	}

	result := Result{Compared: len(rubyLeaves)}
	for _, path := range unionPaths(rubyLeaves, goLeaves) {
		left, inRuby := rubyLeaves[path]
		right, inGo := goLeaves[path]
		if !leavesEqual(left, inRuby, right, inGo) {
			result.Divergences = append(result.Divergences, Divergence{path, show(left, inRuby), show(right, inGo)})
		}
	}

	// By count, not membership: one side logging a message twice and the other
	// once is a divergence, reported as the one copy that has nothing on the
	// other side to pair with.
	for _, k := range surplus(rubyLogs, goLogs) {
		result.Divergences = append(result.Divergences, Divergence{"logs (only in ruby)", k.String(), absent})
	}
	for _, k := range surplus(goLogs, rubyLogs) {
		result.Divergences = append(result.Divergences, Divergence{"logs (only in voxagent)", absent, k.String()})
	}
	return result, nil
}

func comparedLeaves(doc map[string]any, patterns []pattern) (map[string]any, error) {
	all, err := flatten(doc)
	if err != nil {
		return nil, err
	}
	kept := make(map[string]any, len(all))
	for path, v := range all {
		if !ignored(path, patterns) {
			kept[path] = v
		}
	}
	return kept, nil
}

// leavesEqual reports whether the two sides agree on one path: both present
// with equal values, or both absent. Values compare by Python's == -- so 1
// equals 1.0, as JSON draws no distinction between them -- except that a bool
// equals only a bool, where Python would have true equal 1.
func leavesEqual(a any, inA bool, b any, inB bool) bool {
	if !inA || !inB {
		return inA == inB
	}
	_, aBool := a.(bool)
	_, bBool := b.(bool)
	if aBool || bBool {
		return aBool && bBool && a == b
	}
	return pycompat.Equal(a, b)
}

// unionPaths returns every path in either map, sorted. Byte order on UTF-8 is
// code point order, which is Python's string order.
func unionPaths(a, b map[string]any) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var paths []string
	for _, m := range []map[string]any{a, b} {
		for p := range m {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// surplus returns, in keys' sorted order, every copy in keys that cannot be
// paired with a copy in other: all of them for a key other lacks, and the
// excess for a key keys holds more often.
func surplus(keys, other []logKey) []logKey {
	unpaired := make(map[logKey]int, len(other))
	for _, k := range other {
		unpaired[k]++
	}
	var out []logKey
	for _, k := range keys {
		if unpaired[k] > 0 {
			unpaired[k]--
			continue
		}
		out = append(out, k)
	}
	return out
}
