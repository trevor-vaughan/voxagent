package ral

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Ruby's libaugeas calls, recorded per measured case by the LD_PRELOAD
// interposer aug-trace.c (testdata/augeas/traces/README), and a fake augLib
// that replays them: the native providers must make the same calls, in the
// same order, with the same arguments and handles (spec §0, §10).
//
// Trace line grammar, one call per line, tokens separated by one space:
//
//	h<N> <function> <args...> [=> <rc> <results...>]
//
// h<N> numbers handles in aug_init order (h0: not opened by aug_init, which
// is how a failed aug_init is recorded). A string is NULL or double-quoted
// with \" \\ \n escaped. Per function:
//
//	aug_init       root loadpath flags          (no result; h<N> is the result)
//	aug_close                                   (no result)
//	aug_get        path            => rc value
//	aug_set        path value      => rc
//	aug_rm         path            => rc
//	aug_insert     path label 0|1  => rc
//	aug_mv         src dst         => rc
//	aug_match      path            => rc path... (rc paths when rc > 0)
//	aug_load, aug_save             => rc
//	aug_defvar     name expr       => rc
//	aug_defnode    name expr value NULL|created => rc
//	aug_text_store lens node path  => rc

// traceCall is one recorded libaugeas call and its results.
type traceCall struct {
	src     string // "<file>:<line>", for failure messages
	handle  augHandle
	fn      string
	args    []*string // the string arguments in order
	flags   uint      // aug_init
	before  bool      // aug_insert
	created bool      // aug_defnode: the created pointer was non-NULL
	rc      int
	value   *string  // aug_get
	matches []string // aug_match
}

// traceArgs is each function's argument shape: s a string or NULL, u an
// unsigned int, b 0 or 1, c NULL or created. hasRC is whether "=> rc" follows.
var traceArgs = map[string]struct {
	shape string
	hasRC bool
}{
	"aug_init":       {"ssu", false},
	"aug_close":      {"", false},
	"aug_get":        {"s", true},
	"aug_set":        {"ss", true},
	"aug_rm":         {"s", true},
	"aug_insert":     {"ssb", true},
	"aug_mv":         {"ss", true},
	"aug_match":      {"s", true},
	"aug_load":       {"", true},
	"aug_save":       {"", true},
	"aug_defvar":     {"ss", true},
	"aug_defnode":    {"sssc", true},
	"aug_text_store": {"sss", true},
}

// quoteTrace writes s as aug-trace.c's q() does.
func quoteTrace(s *string) string {
	if s == nil {
		return "NULL"
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(*s); i++ {
		switch c := (*s)[i]; c {
		case '\n':
			b.WriteString(`\n`)
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// call renders the call without its results, as the interposer writes it.
func (c traceCall) call() string {
	parts := []string{fmt.Sprintf("h%d", c.handle), c.fn}
	shape := traceArgs[c.fn].shape
	si := 0
	for _, k := range shape {
		switch k {
		case 's':
			parts = append(parts, quoteTrace(c.args[si]))
			si++
		case 'u':
			parts = append(parts, strconv.FormatUint(uint64(c.flags), 10))
		case 'b':
			parts = append(parts, map[bool]string{false: "0", true: "1"}[c.before])
		case 'c':
			parts = append(parts, map[bool]string{false: "NULL", true: "created"}[c.created])
		}
	}
	return strings.Join(parts, " ")
}

// String renders the whole trace line.
func (c traceCall) String() string {
	if !traceArgs[c.fn].hasRC {
		return c.call()
	}
	s := c.call() + " => " + strconv.Itoa(c.rc)
	switch c.fn {
	case "aug_get":
		s += " " + quoteTrace(c.value)
	case "aug_match":
		for _, m := range c.matches {
			s += " " + quoteTrace(&m)
		}
	}
	return s
}

// splitTraceTokens splits a line at single spaces outside quoted strings,
// unescaping quoted tokens. quoted[i] says whether token i was a string.
func splitTraceTokens(line string) (toks []string, quoted []bool, err error) {
	for i := 0; i < len(line); {
		if i > 0 {
			if line[i] != ' ' || i+1 == len(line) {
				return nil, nil, fmt.Errorf("bad separator at byte %d", i)
			}
			i++
		}
		if line[i] != '"' {
			j := i
			for j < len(line) && line[j] != ' ' && line[j] != '"' {
				j++
			}
			if j == i {
				return nil, nil, fmt.Errorf("empty token at byte %d", i)
			}
			toks, quoted = append(toks, line[i:j]), append(quoted, false)
			i = j
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(line) && line[j] != '"'; j++ {
			if line[j] != '\\' {
				b.WriteByte(line[j])
				continue
			}
			j++
			if j == len(line) {
				return nil, nil, errors.New("unterminated escape")
			}
			switch line[j] {
			case 'n':
				b.WriteByte('\n')
			case '"', '\\':
				b.WriteByte(line[j])
			default:
				return nil, nil, fmt.Errorf("unknown escape \\%c", line[j])
			}
		}
		if j == len(line) {
			return nil, nil, errors.New("unterminated string")
		}
		toks, quoted = append(toks, b.String()), append(quoted, true)
		i = j + 1
	}
	if len(toks) == 0 {
		return nil, nil, errors.New("empty line")
	}
	return toks, quoted, nil
}

// traceInt parses a decimal written by printf %d/%u, refusing any other
// spelling so that a parsed line re-renders byte for byte.
func traceInt(s string, unsigned bool) (int64, error) {
	var n int64
	var err error
	if unsigned {
		var u uint64
		u, err = strconv.ParseUint(s, 10, 32)
		n = int64(u)
	} else {
		n, err = strconv.ParseInt(s, 10, 32)
	}
	if err != nil || strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("bad number %q", s)
	}
	return n, nil
}

// parseTraceLine parses one trace line strictly: an unknown function (the
// interposer's UNEXPECTED lines included), a wrong argument count or kind, a
// missing or extra result is an error.
func parseTraceLine(line string) (traceCall, error) {
	toks, quoted, err := splitTraceTokens(line)
	if err != nil {
		return traceCall{}, err
	}
	var c traceCall
	if len(toks) < 2 || quoted[0] || quoted[1] || !strings.HasPrefix(toks[0], "h") {
		return c, errors.New("want h<N> <function>")
	}
	h, err := traceInt(toks[0][1:], true)
	if err != nil {
		return c, fmt.Errorf("handle: %v", err)
	}
	c.handle, c.fn = augHandle(h), toks[1]
	spec, ok := traceArgs[c.fn]
	if !ok {
		return c, fmt.Errorf("unknown function %q", c.fn)
	}
	i := 2
	word := func() (string, error) {
		if i == len(toks) {
			return "", errors.New("too few fields")
		}
		if quoted[i] {
			return "", fmt.Errorf("field %d: unexpected string", i+1)
		}
		i++
		return toks[i-1], nil
	}
	str := func() (*string, error) {
		if i == len(toks) {
			return nil, errors.New("too few fields")
		}
		i++
		if quoted[i-1] {
			s := toks[i-1]
			return &s, nil
		}
		if toks[i-1] != "NULL" {
			return nil, fmt.Errorf("field %d: want a string or NULL", i)
		}
		return nil, nil
	}
	for _, k := range spec.shape {
		switch k {
		case 's':
			s, err := str()
			if err != nil {
				return c, err
			}
			c.args = append(c.args, s)
		case 'u':
			w, err := word()
			if err != nil {
				return c, err
			}
			n, err := traceInt(w, true)
			if err != nil {
				return c, err
			}
			c.flags = uint(n)
		case 'b', 'c':
			w, err := word()
			if err != nil {
				return c, err
			}
			no, yes := "0", "1"
			if k == 'c' {
				no, yes = "NULL", "created"
			}
			if w != no && w != yes {
				return c, fmt.Errorf("field %d: want %s or %s", i, no, yes)
			}
			c.before, c.created = k == 'b' && w == yes, k == 'c' && w == yes
		}
	}
	if spec.hasRC {
		if w, err := word(); err != nil || w != "=>" {
			return c, errors.New("want => after the arguments")
		}
		w, err := word()
		if err != nil {
			return c, err
		}
		rc, err := traceInt(w, false)
		if err != nil {
			return c, err
		}
		c.rc = int(rc)
		switch c.fn {
		case "aug_get":
			if c.value, err = str(); err != nil {
				return c, err
			}
		case "aug_match":
			for n := 0; n < c.rc; n++ {
				m, err := str()
				if err != nil {
					return c, err
				}
				if m == nil {
					return c, errors.New("NULL match")
				}
				c.matches = append(c.matches, *m)
			}
		}
	}
	if i != len(toks) {
		return c, fmt.Errorf("extra fields from field %d", i+1)
	}
	return c, nil
}

// parseTrace parses a whole trace; name labels each call's src. Any bad line
// fails the test.
func parseTrace(t testing.TB, name, text string) []traceCall {
	t.Helper()
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("%s: trace does not end in a newline", name)
	}
	var calls []traceCall
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		c, err := parseTraceLine(line)
		if err != nil {
			t.Fatalf("%s:%d: %v: %q", name, n+1, err, line)
		}
		c.src = fmt.Sprintf("%s:%d", name, n+1)
		calls = append(calls, c)
	}
	return calls
}

const traceDir = "testdata/augeas/traces"

// traceCases lists the case names ("01a-sysctl-new-file-absent") of the
// committed traces; traceFile also takes the short id ("01a").
func traceCases(t testing.TB) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(traceDir, "*.txt"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no traces in %s: %v", traceDir, err)
	}
	var ids []string
	for _, n := range names {
		ids = append(ids, strings.TrimSuffix(filepath.Base(n), ".txt"))
	}
	return ids
}

// traceFile resolves a case name, or a short id ("01a"), to its one trace
// file name.
func traceFile(t testing.TB, id string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(traceDir, id+".txt")); err == nil {
		return filepath.Join(traceDir, id+".txt")
	}
	names, err := filepath.Glob(filepath.Join(traceDir, id+"-*.txt"))
	if err != nil || len(names) != 1 {
		t.Fatalf("trace %q: want one file in %s, found %v (%v)", id, traceDir, names, err)
	}
	return names[0]
}

// readTraceFile is the raw text of case id's trace.
func readTraceFile(t testing.TB, id string) string {
	t.Helper()
	raw, err := os.ReadFile(traceFile(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// factsHandle is the three lines every recorded apply starts with: facts
// resolution's own handle, not a provider's (spec §0). The provider handle
// that follows is numbered h1 again.
var factsHandle = []string{
	"h1 aug_init NULL NULL 68",
	`h1 aug_get "/augeas/version" => 1 "1.15.0"`,
	"h1 aug_close",
}

// loadTrace is case id's recorded trace (e.g. "01a") without the facts handle.
func loadTrace(t *testing.T, id string) []traceCall {
	t.Helper()
	calls := parseTrace(t, filepath.Base(traceFile(t, id)), readTraceFile(t, id))
	for i, want := range factsHandle {
		if i >= len(calls) || calls[i].String() != want {
			t.Fatalf("trace %s: line %d is not the facts handle's %q", id, i+1, want)
		}
	}
	return calls[len(factsHandle):]
}

// replayLib is an augLib answering each call with the next recorded call's
// results. The first call whose function, handle or arguments differ from the
// recording fails the test (Fatalf) with both lines; after that every call
// returns failure without reporting again, so deferred cleanup during the
// test's unwinding adds no noise. If recorded calls remain when the test ends
// (or at an explicit done), the test fails: a replay of a prefix slices calls.
type replayLib struct {
	t        testing.TB
	calls    []traceCall
	next     int
	diverged bool
}

// newScriptLib replays calls, recorded (loadTrace) or hand-written
// (parseTrace), and checks at the test's end that every call was made.
func newScriptLib(t testing.TB, calls []traceCall) *replayLib {
	r := &replayLib{t: t, calls: calls}
	t.Cleanup(func() {
		if !t.Failed() {
			r.done()
		}
	})
	return r
}

// step matches got against the next recorded call; nil after a divergence.
func (r *replayLib) step(got traceCall) *traceCall {
	r.t.Helper()
	if r.diverged {
		return nil
	}
	if r.next == len(r.calls) {
		r.diverged = true
		r.t.Fatalf("augeas replay: call %d is past the end of the %d recorded calls\n  got:  %s",
			r.next+1, len(r.calls), got.call())
		return nil
	}
	want := &r.calls[r.next]
	if got.fn == "aug_init" {
		got.handle = want.handle // aug_init's handle is its result, not an argument
	}
	if got.call() != want.call() {
		r.diverged = true
		r.t.Fatalf("augeas replay: call %d (%s) differs from Ruby's\n  want: %s\n  got:  %s",
			r.next+1, want.src, want, got.call())
		return nil
	}
	r.next++
	return want
}

// done fails the test (Errorf: it also runs as a cleanup) if recorded calls
// remain. It reports once.
func (r *replayLib) done() {
	r.t.Helper()
	if !r.diverged && r.next < len(r.calls) {
		r.diverged = true
		w := r.calls[r.next]
		r.t.Errorf("augeas replay: %d recorded call(s) not made; next is call %d (%s): %s",
			len(r.calls)-r.next, r.next+1, w.src, w)
	}
}

func (r *replayLib) Init(root, loadpath *string, flags uint) (augHandle, bool) {
	r.t.Helper()
	w := r.step(traceCall{fn: "aug_init", args: []*string{root, loadpath}, flags: flags})
	if w == nil || w.handle == 0 {
		return 0, false
	}
	return w.handle, true
}

func (r *replayLib) Close(h augHandle) {
	r.t.Helper()
	r.step(traceCall{handle: h, fn: "aug_close"})
}

func (r *replayLib) Get(h augHandle, path string) (*string, int) {
	r.t.Helper()
	w := r.step(traceCall{handle: h, fn: "aug_get", args: []*string{&path}})
	if w == nil {
		return nil, -1
	}
	if w.value == nil {
		return nil, w.rc
	}
	v := *w.value
	return &v, w.rc
}

// rc replays a call whose only result is its return code.
func (r *replayLib) rc(got traceCall) int {
	r.t.Helper()
	w := r.step(got)
	if w == nil {
		return -1
	}
	return w.rc
}

func (r *replayLib) Set(h augHandle, path string, value *string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_set", args: []*string{&path, value}})
}

func (r *replayLib) Rm(h augHandle, path string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_rm", args: []*string{&path}})
}

func (r *replayLib) Insert(h augHandle, path, label string, before bool) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_insert", args: []*string{&path, &label}, before: before})
}

func (r *replayLib) Mv(h augHandle, src, dst string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_mv", args: []*string{&src, &dst}})
}

func (r *replayLib) Match(h augHandle, path string) ([]string, int) {
	r.t.Helper()
	w := r.step(traceCall{handle: h, fn: "aug_match", args: []*string{&path}})
	if w == nil {
		return nil, -1
	}
	if w.rc < 0 {
		return nil, w.rc
	}
	return slices.Clone(w.matches), w.rc
}

func (r *replayLib) Load(h augHandle) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_load"})
}

func (r *replayLib) Save(h augHandle) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_save"})
}

func (r *replayLib) Defvar(h augHandle, name, expr string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_defvar", args: []*string{&name, &expr}})
}

func (r *replayLib) Defnode(h augHandle, name, expr string, value *string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_defnode", args: []*string{&name, &expr, value}})
}

func (r *replayLib) TextStore(h augHandle, lens, node, path string) int {
	r.t.Helper()
	return r.rc(traceCall{handle: h, fn: "aug_text_store", args: []*string{&lens, &node, &path}})
}
