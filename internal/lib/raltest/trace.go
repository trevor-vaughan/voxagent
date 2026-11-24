package raltest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
)

// Ruby's libaugeas calls, recorded per measured case by the LD_PRELOAD
// interposer aug-trace.c (testdata/augeas/traces/README), and a fake augeas.Lib
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

// TraceCall is one recorded libaugeas call and its results.
type TraceCall struct {
	Src     string // "<file>:<line>", for failure messages
	Handle  augeas.Handle
	Fn      string
	Args    []*string // the string arguments in order
	Flags   uint      // aug_init
	Before  bool      // aug_insert
	Created bool      // aug_defnode: the created pointer was non-NULL
	RC      int
	Value   *string  // aug_get
	Matches []string // aug_match
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

// QuoteTrace writes s as aug-trace.c's q() does.
func QuoteTrace(s *string) string {
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
func (c TraceCall) call() string {
	parts := []string{fmt.Sprintf("h%d", c.Handle), c.Fn}
	shape := traceArgs[c.Fn].shape
	si := 0
	for _, k := range shape {
		switch k {
		case 's':
			parts = append(parts, QuoteTrace(c.Args[si]))
			si++
		case 'u':
			parts = append(parts, strconv.FormatUint(uint64(c.Flags), 10))
		case 'b':
			parts = append(parts, map[bool]string{false: "0", true: "1"}[c.Before])
		case 'c':
			parts = append(parts, map[bool]string{false: "NULL", true: "created"}[c.Created])
		}
	}
	return strings.Join(parts, " ")
}

// String renders the whole trace line.
func (c TraceCall) String() string {
	if !traceArgs[c.Fn].hasRC {
		return c.call()
	}
	s := c.call() + " => " + strconv.Itoa(c.RC)
	switch c.Fn {
	case "aug_get":
		s += " " + QuoteTrace(c.Value)
	case "aug_match":
		for _, m := range c.Matches {
			s += " " + QuoteTrace(&m)
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

// ParseTraceLine parses one trace line strictly: an unknown function (the
// interposer's UNEXPECTED lines included), a wrong argument count or kind, a
// missing or extra result is an error.
func ParseTraceLine(line string) (TraceCall, error) {
	toks, quoted, err := splitTraceTokens(line)
	if err != nil {
		return TraceCall{}, err
	}
	var c TraceCall
	if len(toks) < 2 || quoted[0] || quoted[1] || !strings.HasPrefix(toks[0], "h") {
		return c, errors.New("want h<N> <function>")
	}
	h, err := traceInt(toks[0][1:], true)
	if err != nil {
		return c, fmt.Errorf("handle: %v", err)
	}
	c.Handle, c.Fn = augeas.Handle(h), toks[1]
	spec, ok := traceArgs[c.Fn]
	if !ok {
		return c, fmt.Errorf("unknown function %q", c.Fn)
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
			c.Args = append(c.Args, s)
		case 'u':
			w, err := word()
			if err != nil {
				return c, err
			}
			n, err := traceInt(w, true)
			if err != nil {
				return c, err
			}
			c.Flags = uint(n)
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
			c.Before, c.Created = k == 'b' && w == yes, k == 'c' && w == yes
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
		c.RC = int(rc)
		switch c.Fn {
		case "aug_get":
			if c.Value, err = str(); err != nil {
				return c, err
			}
		case "aug_match":
			for n := 0; n < c.RC; n++ {
				m, err := str()
				if err != nil {
					return c, err
				}
				if m == nil {
					return c, errors.New("NULL match")
				}
				c.Matches = append(c.Matches, *m)
			}
		}
	}
	if i != len(toks) {
		return c, fmt.Errorf("extra fields from field %d", i+1)
	}
	return c, nil
}

// ParseTrace parses a whole trace; name labels each call's src. Any bad line
// fails the test.
func ParseTrace(t testing.TB, name, text string) []TraceCall {
	t.Helper()
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("%s: trace does not end in a newline", name)
	}
	var calls []TraceCall
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		c, err := ParseTraceLine(line)
		if err != nil {
			t.Fatalf("%s:%d: %v: %q", name, n+1, err, line)
		}
		c.Src = fmt.Sprintf("%s:%d", name, n+1)
		calls = append(calls, c)
	}
	return calls
}

const traceDir = "testdata/augeas/traces"

// TraceCases lists the case names ("01a-sysctl-new-file-absent") of the
// committed traces; traceFile also takes the short id ("01a").
func TraceCases(t testing.TB) []string {
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

// ReadTraceFile is the raw text of case id's trace.
func ReadTraceFile(t testing.TB, id string) string {
	t.Helper()
	raw, err := os.ReadFile(traceFile(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// FactsHandle is the three lines every recorded apply starts with: facts
// resolution's own handle, not a provider's (spec §0). The provider handle
// that follows is numbered h1 again.
var FactsHandle = []string{
	"h1 aug_init NULL NULL 68",
	`h1 aug_get "/augeas/version" => 1 "1.15.0"`,
	"h1 aug_close",
}

// LoadTrace is case id's recorded trace (e.g. "01a") without the facts handle.
func LoadTrace(t *testing.T, id string) []TraceCall {
	t.Helper()
	calls := ParseTrace(t, filepath.Base(traceFile(t, id)), ReadTraceFile(t, id))
	for i, want := range FactsHandle {
		if i >= len(calls) || calls[i].String() != want {
			t.Fatalf("trace %s: line %d is not the facts handle's %q", id, i+1, want)
		}
	}
	return calls[len(FactsHandle):]
}

// TraceLines is case id's recorded calls at the given trace file lines
// (1-based, inclusive spans, facts handle included in the numbering), in order.
func TraceLines(t *testing.T, id string, spans ...[2]int) []TraceCall {
	t.Helper()
	calls := LoadTrace(t, id)
	var out []TraceCall
	for _, s := range spans {
		from, to := s[0]-len(FactsHandle)-1, s[1]-len(FactsHandle)
		if from < 0 || to > len(calls) || from >= to {
			t.Fatalf("trace %s: bad span %v", id, s)
		}
		out = append(out, calls[from:to]...)
	}
	return out
}
