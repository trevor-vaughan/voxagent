package ral

import (
	"fmt"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

func TestLoadTraceParses01a(t *testing.T) {
	calls := raltest.LoadTrace(t, "01a")
	if len(calls) != 44 {
		t.Fatalf("calls = %d, want 44 (47 lines minus the facts handle's 3)", len(calls))
	}
	if got := calls[0].String(); got != "h1 aug_init NULL NULL 64" {
		t.Errorf("call 1 = %s", got)
	}
	if got := calls[len(calls)-1].String(); got != "h1 aug_close" {
		t.Errorf("last call = %s", got)
	}
	found := false
	for _, c := range calls {
		if c.String() == `h1 aug_set "$resource" "20" => -1` {
			found = true
		}
	}
	if !found {
		t.Error(`aug_set "$resource" "20" => -1 missing`)
	}
}

// Every committed trace parses, round-trips byte for byte and starts with the
// facts-resolution handle raltest.LoadTrace drops.
func TestLoadTraceRoundTripsEveryCase(t *testing.T) {
	for _, tc := range raltest.TraceCases(t) {
		raw := raltest.ReadTraceFile(t, tc)
		lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
		calls := raltest.LoadTrace(t, tc)
		if len(calls) != len(lines)-3 {
			t.Errorf("%s: %d calls from %d lines", tc, len(calls), len(lines))
			continue
		}
		for i, c := range calls {
			if c.String() != lines[i+3] {
				t.Errorf("%s line %d: re-rendered %q, file has %q", tc, i+4, c.String(), lines[i+3])
			}
		}
	}
}

func TestParseTraceEscapes(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", `h2 aug_set "/input" "a \"b\" c\\d\n" => 0`+"\n")
	v := calls[0].Args[1]
	if v == nil || *v != "a \"b\" c\\d\n" {
		t.Fatalf("value = %s", raltest.QuoteTrace(v))
	}
}

func TestParseTraceAugMv(t *testing.T) {
	const line = `h1 aug_mv "/a" "/b" => -1`
	c, err := raltest.ParseTraceLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if c.Fn != "aug_mv" || c.RC != -1 || len(c.Args) != 2 || *c.Args[0] != "/a" || *c.Args[1] != "/b" {
		t.Fatalf("parsed %+v", c)
	}
	if got := c.String(); got != line {
		t.Fatalf("re-rendered %q", got)
	}
}

func TestParseTraceRejects(t *testing.T) {
	for _, line := range []string{
		`h1 aug_error UNEXPECTED`,
		`x1 aug_load => 0`,
		`h1 aug_load`,
		`h1 aug_load => 0 0`,
		`h1 aug_set "/a" => 0`,
		`h1 aug_set "/a" "b => 0`,
		`h1 aug_set "/a" "\t" => 0`,
		`h1 aug_get "/a" => 1`,
		`h1 aug_match "/a" => 2 "/a/1"`,
		`h1 aug_match "/a" => 0 "/a/1"`,
		`h1 aug_init NULL NULL x`,
		`h1 aug_insert "/a" "b" 2 => 0`,
		`h1 aug_defnode "n" "/a" NULL maybe => 1`,
		`h1 aug_close => 0`,
		`h1 aug_mv "/a" => 0`,
		`h1 aug_rm "/a" =>`,
		`h1  aug_load => 0`,
		``,
	} {
		if _, err := raltest.ParseTraceLine(line); err == nil {
			t.Errorf("parsed %q", line)
		}
	}
}

// fakeT records a replay's failure instead of failing the real test.
type fakeT struct {
	testing.TB
	msg      string
	failed   bool
	cleanups []func()
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
}
func (f *fakeT) Errorf(format string, args ...any) { f.Fatalf(format, args...) }
func (f *fakeT) Failed() bool                      { return f.failed }
func (f *fakeT) Cleanup(fn func())                 { f.cleanups = append(f.cleanups, fn) }

// cleanup runs the registered cleanups as testing does: last first.
func (f *fakeT) cleanup() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// A replay that does not reach the end of its trace fails the test by itself,
// so a provider that skips Ruby's trailing calls cannot pass.
func TestReplayUnconsumedFailsAutomatically(t *testing.T) {
	ft := &fakeT{TB: t}
	r := raltest.NewScriptLib(ft, raltest.ParseTrace(t, "script", "h1 aug_init NULL NULL 64\nh1 aug_save => 0\nh1 aug_close\n"))
	r.Init(nil, nil, augeas.NoModlAutoload)
	if ft.failed {
		t.Fatalf("failed early: %s", ft.msg)
	}
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "2 recorded call(s) not made; next is call 2 (script:2): h1 aug_save => 0") {
		t.Fatalf("unconsumed trace not reported: %q", ft.msg)
	}
}

func TestReplayFailsOnFirstDivergence(t *testing.T) {
	calls := raltest.LoadTrace(t, "01a")
	ft := &fakeT{TB: t}
	r := raltest.NewScriptLib(ft, calls)
	a, err := augeas.Open(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Match("/augeas/load/Sysctl"); err != nil {
		t.Fatal(err)
	}
	a.Set("/augeas/load/Sysctl/lens", "Sysctl.lns")
	if ft.failed {
		t.Fatalf("failed early: %s", ft.msg)
	}
	a.Set("/augeas/load/Sysctl/incl[last()+1]", "/etc/sysctl.d/x.conf")
	if !ft.failed {
		t.Fatal("a wrong argument did not fail the test")
	}
	for _, want := range []string{
		"01a-sysctl-new-file-absent.txt:7",
		"call 4",
		`want: h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.conf" => 0`,
		`got:  h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.d/x.conf"`,
	} {
		if !strings.Contains(ft.msg, want) {
			t.Errorf("message lacks %q:\n%s", want, ft.msg)
		}
	}
	// Calls after the divergence (deferred cleanup) report nothing more.
	ft.msg = ""
	a.Close()
	if ft.msg != "" {
		t.Errorf("second report: %s", ft.msg)
	}
}

func TestReplayComparesHandle(t *testing.T) {
	ft := &fakeT{TB: t}
	r := raltest.NewScriptLib(ft, raltest.ParseTrace(t, "script", "h1 aug_init NULL NULL 64\nh2 aug_init NULL NULL 64\nh1 aug_load => 0\n"))
	r.Init(nil, nil, augeas.NoModlAutoload)
	h2, _ := r.Init(nil, nil, augeas.NoModlAutoload)
	r.Load(h2)
	if !ft.failed || !strings.Contains(ft.msg, "got:  h2 aug_load") {
		t.Fatalf("wrong handle not reported: %q", ft.msg)
	}
}

func TestReplayFailsOnExtraAndMissingCalls(t *testing.T) {
	ft := &fakeT{TB: t}
	r := raltest.NewScriptLib(ft, raltest.ParseTrace(t, "script", "h1 aug_init NULL NULL 64\n"))
	r.Done()
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "1 recorded call(s) not made") || !strings.Contains(ft.msg, "h1 aug_init NULL NULL 64") {
		t.Fatalf("missing call not reported: %q", ft.msg)
	}
	ft = &fakeT{TB: t}
	r = raltest.NewScriptLib(ft, nil)
	r.Load(1)
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "past the end") {
		t.Fatalf("extra call not reported: %q", ft.msg)
	}
}

// The whole 01a trace replays when driven by its own calls, returning the
// recorded results.
func TestReplayAnswersRecordedResults(t *testing.T) {
	calls := raltest.LoadTrace(t, "01a")
	r := raltest.NewScriptLib(t, calls)
	for _, c := range calls {
		h := augeas.Handle(c.Handle)
		switch c.Fn {
		case "aug_init":
			got, ok := r.Init(c.Args[0], c.Args[1], c.Flags)
			if !ok || got != h {
				t.Fatalf("init = %d %v", got, ok)
			}
		case "aug_close":
			r.Close(h)
		case "aug_get":
			v, rc := r.Get(h, *c.Args[0])
			if rc != c.RC || (v == nil) != (c.Value == nil) || (v != nil && *v != *c.Value) {
				t.Fatalf("get = %v %d", v, rc)
			}
		case "aug_set":
			if rc := r.Set(h, *c.Args[0], c.Args[1]); rc != c.RC {
				t.Fatalf("set = %d", rc)
			}
		case "aug_match":
			ps, rc := r.Match(h, *c.Args[0])
			if rc != c.RC || strings.Join(ps, "|") != strings.Join(c.Matches, "|") {
				t.Fatalf("match = %v %d", ps, rc)
			}
		case "aug_load":
			if rc := r.Load(h); rc != c.RC {
				t.Fatalf("load = %d", rc)
			}
		case "aug_save":
			if rc := r.Save(h); rc != c.RC {
				t.Fatalf("save = %d", rc)
			}
		case "aug_defvar":
			if rc := r.Defvar(h, *c.Args[0], *c.Args[1]); rc != c.RC {
				t.Fatalf("defvar = %d", rc)
			}
		case "aug_defnode":
			if rc := r.Defnode(h, *c.Args[0], *c.Args[1], c.Args[2]); rc != c.RC {
				t.Fatalf("defnode = %d", rc)
			}
		default:
			t.Fatalf("01a has no %s", c.Fn)
		}
	}
	r.Done()
}

func TestReplayInitFailure(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\n"))
	if h, ok := r.Init(nil, nil, augeas.NoModlAutoload); ok || h != 0 {
		t.Fatalf("init = %d %v", h, ok)
	}
	r.Done()
}
