package ral

import (
	"fmt"
	"strings"
	"testing"
)

func TestLoadTraceParses01a(t *testing.T) {
	calls := loadTrace(t, "01a")
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
// facts-resolution handle loadTrace drops.
func TestLoadTraceRoundTripsEveryCase(t *testing.T) {
	for _, tc := range traceCases(t) {
		raw := readTraceFile(t, tc)
		lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
		calls := loadTrace(t, tc)
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
	calls := parseTrace(t, "script", `h2 aug_set "/input" "a \"b\" c\\d\n" => 0`+"\n")
	v := calls[0].args[1]
	if v == nil || *v != "a \"b\" c\\d\n" {
		t.Fatalf("value = %s", quoteTrace(v))
	}
}

func TestParseTraceAugMv(t *testing.T) {
	const line = `h1 aug_mv "/a" "/b" => -1`
	c, err := parseTraceLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if c.fn != "aug_mv" || c.rc != -1 || len(c.args) != 2 || *c.args[0] != "/a" || *c.args[1] != "/b" {
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
		if _, err := parseTraceLine(line); err == nil {
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
	r := newScriptLib(ft, parseTrace(t, "script", "h1 aug_init NULL NULL 64\nh1 aug_save => 0\nh1 aug_close\n"))
	r.Init(nil, nil, augNoModlAutoload)
	if ft.failed {
		t.Fatalf("failed early: %s", ft.msg)
	}
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "2 recorded call(s) not made; next is call 2 (script:2): h1 aug_save => 0") {
		t.Fatalf("unconsumed trace not reported: %q", ft.msg)
	}
}

func TestReplayFailsOnFirstDivergence(t *testing.T) {
	calls := loadTrace(t, "01a")
	ft := &fakeT{TB: t}
	r := newScriptLib(ft, calls)
	a, err := openRubyAug(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.match("/augeas/load/Sysctl"); err != nil {
		t.Fatal(err)
	}
	a.set("/augeas/load/Sysctl/lens", "Sysctl.lns")
	if ft.failed {
		t.Fatalf("failed early: %s", ft.msg)
	}
	a.set("/augeas/load/Sysctl/incl[last()+1]", "/etc/sysctl.d/x.conf")
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
	a.close()
	if ft.msg != "" {
		t.Errorf("second report: %s", ft.msg)
	}
}

func TestReplayComparesHandle(t *testing.T) {
	ft := &fakeT{TB: t}
	r := newScriptLib(ft, parseTrace(t, "script", "h1 aug_init NULL NULL 64\nh2 aug_init NULL NULL 64\nh1 aug_load => 0\n"))
	r.Init(nil, nil, augNoModlAutoload)
	h2, _ := r.Init(nil, nil, augNoModlAutoload)
	r.Load(h2)
	if !ft.failed || !strings.Contains(ft.msg, "got:  h2 aug_load") {
		t.Fatalf("wrong handle not reported: %q", ft.msg)
	}
}

func TestReplayFailsOnExtraAndMissingCalls(t *testing.T) {
	ft := &fakeT{TB: t}
	r := newScriptLib(ft, parseTrace(t, "script", "h1 aug_init NULL NULL 64\n"))
	r.done()
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "1 recorded call(s) not made") || !strings.Contains(ft.msg, "h1 aug_init NULL NULL 64") {
		t.Fatalf("missing call not reported: %q", ft.msg)
	}
	ft = &fakeT{TB: t}
	r = newScriptLib(ft, nil)
	r.Load(1)
	ft.cleanup()
	if !ft.failed || !strings.Contains(ft.msg, "past the end") {
		t.Fatalf("extra call not reported: %q", ft.msg)
	}
}

// The whole 01a trace replays when driven by its own calls, returning the
// recorded results.
func TestReplayAnswersRecordedResults(t *testing.T) {
	calls := loadTrace(t, "01a")
	r := newScriptLib(t, calls)
	for _, c := range calls {
		h := augHandle(c.handle)
		switch c.fn {
		case "aug_init":
			got, ok := r.Init(c.args[0], c.args[1], c.flags)
			if !ok || got != h {
				t.Fatalf("init = %d %v", got, ok)
			}
		case "aug_close":
			r.Close(h)
		case "aug_get":
			v, rc := r.Get(h, *c.args[0])
			if rc != c.rc || (v == nil) != (c.value == nil) || (v != nil && *v != *c.value) {
				t.Fatalf("get = %v %d", v, rc)
			}
		case "aug_set":
			if rc := r.Set(h, *c.args[0], c.args[1]); rc != c.rc {
				t.Fatalf("set = %d", rc)
			}
		case "aug_match":
			ps, rc := r.Match(h, *c.args[0])
			if rc != c.rc || strings.Join(ps, "|") != strings.Join(c.matches, "|") {
				t.Fatalf("match = %v %d", ps, rc)
			}
		case "aug_load":
			if rc := r.Load(h); rc != c.rc {
				t.Fatalf("load = %d", rc)
			}
		case "aug_save":
			if rc := r.Save(h); rc != c.rc {
				t.Fatalf("save = %d", rc)
			}
		case "aug_defvar":
			if rc := r.Defvar(h, *c.args[0], *c.args[1]); rc != c.rc {
				t.Fatalf("defvar = %d", rc)
			}
		case "aug_defnode":
			if rc := r.Defnode(h, *c.args[0], *c.args[1], c.args[2]); rc != c.rc {
				t.Fatalf("defnode = %d", rc)
			}
		default:
			t.Fatalf("01a has no %s", c.fn)
		}
	}
	r.done()
}

func TestReplayInitFailure(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", "h0 aug_init NULL NULL 64\n"))
	if h, ok := r.Init(nil, nil, augNoModlAutoload); ok || h != 0 {
		t.Fatalf("init = %d %v", h, ok)
	}
	r.done()
}

func TestRubyAugGetNilUnlessExactlyOne(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_get "/a" => 0 NULL
h1 aug_get "/b" => -1 NULL
h1 aug_get "/c" => 1 NULL
h1 aug_get "/d" => 1 "v"
h1 aug_get "/e" => 1 ""
`))
	a, err := openRubyAug(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if v, ok := a.get(p); ok {
			t.Errorf("get(%s) = %q, want nil", p, v)
		}
	}
	if v, ok := a.get("/d"); !ok || v != "v" {
		t.Errorf("get(/d) = %q %v", v, ok)
	}
	if v, ok := a.get("/e"); !ok || v != "" {
		t.Errorf("get(/e) = %q %v", v, ok)
	}
	r.done()
}

func TestRubyAugMatchError(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", `h1 aug_init NULL "/lenses" 64
h1 aug_match "[[[" => -1
h1 aug_match "/m" => 2 "/m/a" "/m/b"
h1 aug_match "/n" => 0
`))
	lp := "/lenses"
	a, err := openRubyAug(r, &lp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.match("[[["); err == nil || err.Error() != "unknown error - Matching path expression '[[[' failed" {
		t.Fatalf("match error = %v", err)
	}
	if m, err := a.match("/m"); err != nil || strings.Join(m, ",") != "/m/a,/m/b" {
		t.Fatalf("match = %v %v", m, err)
	}
	if m, err := a.match("/n"); err != nil || len(m) != 0 {
		t.Fatalf("match = %v %v", m, err)
	}
	r.done()
}

func TestRubyAugLoadSaveBang(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_load => -1
h1 aug_save => -1
h1 aug_load => 0
h1 aug_save => 0
`))
	a, err := openRubyAug(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.loadBang(); err == nil || err.Error() != "Augeas::Error" {
		t.Errorf("load! = %v", err)
	}
	if err := a.saveBang(); err == nil || err.Error() != "Augeas::Error" {
		t.Errorf("save! = %v", err)
	}
	if err := a.loadBang(); err != nil {
		t.Errorf("load! = %v", err)
	}
	if err := a.saveBang(); err != nil {
		t.Errorf("save! = %v", err)
	}
	r.done()
}

func TestOpenRubyAugFailure(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", "h0 aug_init NULL NULL 64\n"))
	a, err := openRubyAug(r, nil)
	if a != nil || err == nil || err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Fatalf("open = %v %v", a, err)
	}
	r.done()
}

// The remaining wrappers pass Ruby's arguments and return libaugeas' results.
func TestRubyAugPassThrough(t *testing.T) {
	r := newScriptLib(t, parseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_set "/a" "1" => -1
h1 aug_set "/b" NULL => 0
h1 aug_rm "/c" => 3
h1 aug_insert "/d" "e" 1 => 0
h1 aug_insert "/f" "g" 0 => -1
h1 aug_defvar "v" "/x" => 2
h1 aug_defnode "n" "/y" NULL NULL => 1
h1 aug_text_store "Ssh.lns" "/input" "/parsed" => 0
h1 aug_text_store "Ssh.lns" "/input" "/parsed" => -1
h1 aug_close
`))
	a, err := openRubyAug(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.set("/a", "1")
	a.clear("/b")
	if n := a.rm("/c"); n != 3 {
		t.Errorf("rm = %d", n)
	}
	if n := a.insert("/d", "e", true); n != 0 {
		t.Errorf("insert = %d", n)
	}
	if n := a.insert("/f", "g", false); n != -1 {
		t.Errorf("insert = %d", n)
	}
	a.defvar("v", "/x")
	a.defnode("n", "/y")
	if !a.textStore("Ssh.lns", "/input", "/parsed") {
		t.Error("text_store 0 = false")
	}
	if a.textStore("Ssh.lns", "/input", "/parsed") {
		t.Error("text_store -1 = true")
	}
	a.close()
	a.close()
	r.done()
}
