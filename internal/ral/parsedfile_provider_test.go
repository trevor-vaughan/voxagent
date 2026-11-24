package ral

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// memFile is an in-memory FileType that keeps every text written to it.
type memFile struct {
	text     string
	readErr  error
	writeErr error
	writes   []string
}

func (f *memFile) Read(context.Context) (string, error) { return f.text, f.readErr }

func (f *memFile) Write(_ context.Context, text string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.text = text
	f.writes = append(f.writes, text)
	return nil
}

// pfResource is a parsedfile.Resource whose target should is /t.
type pfResource struct{ name string }

func (r pfResource) Name() string                 { return r.name }
func (r pfResource) Title() string                { return r.name }
func (r pfResource) ShouldTarget() (string, bool) { return "/t", true }

// pfEnv is one ParsedFile run over a single target, /t, holding "<name> <val>"
// lines, with a log of the hooks' calls and the run's log lines.
type pfEnv struct {
	ctx   context.Context
	file  *memFile
	class *parsedfile.Class
	calls []string
	logs  []report.LogEntry
}

func newPFEnv(t *testing.T, text string) *pfEnv {
	t.Helper()
	g, err := parsedfile.NewGrammar(parsedfile.LineType{
		Name: "item", Match: parsedfile.RE2(regexp.MustCompile(`^(\w+) (\w+)$`)), Fields: []string{"name", "val"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &pfEnv{file: &memFile{text: text}}
	l := runlog.New("debug", func(le report.LogEntry) { e.logs = append(e.logs, le) }, func(report.LogEntry) {})
	e.ctx = sources.WithSources(runlog.WithLogger(context.Background(), l), sources.Sources{Resource: "/Stage[main]/Thing[b]"})
	e.class = &parsedfile.Class{
		Name: "item", TypeName: "thing", Grammar: g, DefaultTarget: "/t",
		Header:   func(time.Time) string { return "" },
		FileType: func(string) (parsedfile.FileType, error) { return e.file, nil },
	}
	return e
}

// ensurable is a type whose one property, val, is in sync when equal; its
// hooks log their calls.
func (e *pfEnv) ensurable() *ParsedFileEnsurable {
	return &ParsedFileEnsurable{
		Name:       "thing",
		Properties: []string{"val"},
		Current: func(rec *parsedfile.Record, prop string) parsedfile.Value {
			e.calls = append(e.calls, "current "+prop)
			if v := rec.Fields[prop]; v.Kind != parsedfile.Nil {
				return v
			}
			return parsedfile.Value{Kind: parsedfile.Absent}
		},
		Change: func(prop string, is, should parsedfile.Value) *Change {
			if is.Kind == parsedfile.Str && is.S == should.S {
				return nil
			}
			return &Change{Property: prop, Was: is.ReportValue(), Is: should.ReportValue(), Message: fmt.Sprintf("%s changed", prop)}
		},
		Store: func(rec *parsedfile.Record, prop string, v parsedfile.Value) {
			e.calls = append(e.calls, "store "+prop)
			rec.Fields[prop] = v
		},
	}
}

func (e *pfEnv) prefetch(t *testing.T, names ...string) *ParsedFileRun {
	t.Helper()
	rs := make([]parsedfile.Resource, 0, len(names))
	for _, n := range names {
		rs = append(rs, pfResource{n})
	}
	st, err := PrefetchParsedFile(e.ctx, e.class, rs)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func str(s string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.Str, S: s} }

// should is resource b's should values with the name key.
func should(ensure string, props map[string]parsedfile.Value) ParsedFileShould {
	return ParsedFileShould{Resource: pfResource{"b"}, Ensure: ensure, Should: props, Keys: map[string]parsedfile.Value{"name": str("b")}}
}

func TestPrefetchParsedFileBindsMatchedAndFreshRecords(t *testing.T) {
	e := newPFEnv(t, "a 1\n")
	st := e.prefetch(t, "a", "b")
	if a := st.Records["a"]; a == nil || a.Ensure == "absent" || !reflect.DeepEqual(a.Fields["val"], str("1")) {
		t.Errorf("a is bound to %+v, want the record read", a)
	}
	want := &parsedfile.Record{Type: "item", Ensure: "absent", Fields: map[string]parsedfile.Value{}, Flags: map[string]bool{}}
	if b := st.Records["b"]; !reflect.DeepEqual(b, want) {
		t.Errorf("b is bound to %+v, want a fresh absent record %+v", b, want)
	}
	if len(st.Records) != 2 {
		t.Errorf("records %v, want a and b only", st.Records)
	}
}

func TestPrefetchParsedFileReturnsTheEngineError(t *testing.T) {
	e := newPFEnv(t, "")
	boom := errors.New("boom")
	e.file.readErr = boom
	st, err := PrefetchParsedFile(e.ctx, e.class, []parsedfile.Resource{pfResource{"a"}})
	if !errors.Is(err, boom) || st != nil {
		t.Errorf("got %v, %v; want nil, boom", st, err)
	}
}

func TestParsedFileApplyCreates(t *testing.T) {
	for _, noop := range []bool{false, true} {
		t.Run(fmt.Sprint("noop=", noop), func(t *testing.T) {
			e := newPFEnv(t, "a 1\n")
			st := e.prefetch(t, "a", "b")
			changes, warnings, err := st.Apply(e.ctx, e.ensurable(), should("present", map[string]parsedfile.Value{"val": str("2")}), noop)
			if err != nil || warnings != nil {
				t.Fatalf("got %v, %v", warnings, err)
			}
			msg, writes, ensure := "created", []string{"a 1\nb 2\n"}, "present"
			if noop {
				msg, writes, ensure = "created (noop)", nil, "absent"
			}
			want := []Change{{Property: "ensure", Was: "absent", Is: "present", Message: msg, Name: "thing_created"}}
			if !reflect.DeepEqual(changes, want) {
				t.Errorf("changes %+v, want %+v", changes, want)
			}
			if !slices.Equal(e.file.writes, writes) {
				t.Errorf("writes %q, want %q", e.file.writes, writes)
			}
			if got := st.Records["b"].Ensure; got != ensure {
				t.Errorf("ensure %q, want %q", got, ensure)
			}
		})
	}
}

func TestParsedFileApplyRemoves(t *testing.T) {
	for _, noop := range []bool{false, true} {
		t.Run(fmt.Sprint("noop=", noop), func(t *testing.T) {
			e := newPFEnv(t, "a 1\nb 2\n")
			st := e.prefetch(t, "a", "b")
			changes, _, err := st.Apply(e.ctx, e.ensurable(), should("absent", map[string]parsedfile.Value{"val": str("3")}), noop)
			if err != nil {
				t.Fatal(err)
			}
			msg, writes := "removed", []string{"a 1\n"}
			if noop {
				msg, writes = "removed (noop)", nil
			}
			want := []Change{{Property: "ensure", Was: "present", Is: "absent", Message: msg, Name: "thing_removed"}}
			if !reflect.DeepEqual(changes, want) {
				t.Errorf("changes %+v, want %+v", changes, want)
			}
			if !slices.Equal(e.file.writes, writes) {
				t.Errorf("writes %q, want %q", e.file.writes, writes)
			}
			if len(e.calls) != 0 {
				t.Errorf("hooks called %v, want none: ensure changes nothing else", e.calls)
			}
		})
	}
}

func TestParsedFileApplyNothingToManage(t *testing.T) {
	for _, ensure := range []string{"", "absent"} {
		t.Run("ensure="+ensure, func(t *testing.T) {
			e := newPFEnv(t, "")
			st := e.prefetch(t, "b")
			changes, warnings, err := st.Apply(e.ctx, e.ensurable(), should(ensure, map[string]parsedfile.Value{"val": str("2")}), false)
			if changes != nil || warnings != nil || err != nil {
				t.Fatalf("got %v, %v, %v; want nothing", changes, warnings, err)
			}
			want := report.LogEntry{Level: "debug", Source: "/Stage[main]/Thing[b]", Message: "Nothing to manage: no ensure and the resource doesn't exist"}
			if n := len(e.logs); n == 0 || e.logs[n-1].Level != want.Level || e.logs[n-1].Source != want.Source || e.logs[n-1].Message != want.Message {
				t.Errorf("logs %+v, want last %+v", e.logs, want)
			}
			if e.file.writes != nil {
				t.Errorf("wrote %q", e.file.writes)
			}
		})
	}
}

func TestParsedFileApplySyncsProperties(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ensure string
		noop   bool
		msg    string
		writes []string
		calls  []string
	}{
		{"live", "present", false, "val changed", []string{"b 3\n"}, []string{"current val", "store val"}},
		{"no ensure", "", false, "val changed", []string{"b 3\n"}, []string{"current val", "store val"}},
		{"noop", "present", true, "val changed (noop)", nil, []string{"current val"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPFEnv(t, "b 2\n")
			st := e.prefetch(t, "b")
			changes, _, err := st.Apply(e.ctx, e.ensurable(), should(tc.ensure, map[string]parsedfile.Value{"val": str("3")}), tc.noop)
			if err != nil {
				t.Fatal(err)
			}
			want := []Change{{Property: "val", Was: "2", Is: "3", Message: tc.msg}}
			if !reflect.DeepEqual(changes, want) {
				t.Errorf("changes %+v, want %+v", changes, want)
			}
			if !slices.Equal(e.file.writes, tc.writes) {
				t.Errorf("writes %q, want %q", e.file.writes, tc.writes)
			}
			if !slices.Equal(e.calls, tc.calls) {
				t.Errorf("calls %v, want %v", e.calls, tc.calls)
			}
		})
	}
}

// Every current value is read before the first store (resource_harness.rb:22,
// 315), properties go in the type's order, and one without a should is left.
func TestParsedFileApplyReadsEveryCurrentFirst(t *testing.T) {
	e := newPFEnv(t, "b 2\n")
	st := e.prefetch(t, "b")
	typ := e.ensurable()
	typ.Properties = []string{"x", "val", "y"}
	changes, _, err := st.Apply(e.ctx, typ, should("present", map[string]parsedfile.Value{"val": str("3"), "y": str("4")}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Property != "val" || changes[1].Property != "y" {
		t.Errorf("changes %+v, want val then y", changes)
	}
	wantCalls := []string{"current x", "current val", "current y", "store val", "store y"}
	if !slices.Equal(e.calls, wantCalls) {
		t.Errorf("calls %v, want %v", e.calls, wantCalls)
	}
}

func TestParsedFileApplyInSyncWritesNothing(t *testing.T) {
	e := newPFEnv(t, "b 2\n")
	st := e.prefetch(t, "b")
	changes, _, err := st.Apply(e.ctx, e.ensurable(), should("present", map[string]parsedfile.Value{"val": str("2")}), false)
	if changes != nil || err != nil || e.file.writes != nil {
		t.Errorf("got %v, %v, writes %q; want nothing", changes, err, e.file.writes)
	}
}

// The flush fills a key attribute the record lacks from the resource and
// keeps one it has (pf:448-462).
func TestParsedFileApplyFillsKeys(t *testing.T) {
	e := newPFEnv(t, "")
	st := e.prefetch(t, "b")
	s := should("present", map[string]parsedfile.Value{"val": str("2")})
	if _, _, err := st.Apply(e.ctx, e.ensurable(), s, false); err != nil {
		t.Fatal(err)
	}
	if got := st.Records["b"].Fields["name"]; !reflect.DeepEqual(got, str("b")) {
		t.Errorf("name %+v, want filled with b", got)
	}

	e = newPFEnv(t, "b 2\n")
	st = e.prefetch(t, "b")
	s = should("present", map[string]parsedfile.Value{"val": str("3")})
	s.Keys["name"] = str("other")
	if _, _, err := st.Apply(e.ctx, e.ensurable(), s, false); err != nil {
		t.Fatal(err)
	}
	if got := st.Records["b"].Fields["name"]; !reflect.DeepEqual(got, str("b")) {
		t.Errorf("name %+v, want the record's b kept", got)
	}
}

func TestParsedFileApplyWrapsFlushError(t *testing.T) {
	e := newPFEnv(t, "")
	st := e.prefetch(t, "b")
	boom := errors.New("boom")
	e.file.writeErr = boom
	changes, _, err := st.Apply(e.ctx, e.ensurable(), should("present", map[string]parsedfile.Value{"val": str("2")}), false)
	var fe *FlushError
	if !errors.As(err, &fe) || !errors.Is(err, boom) {
		t.Errorf("error %#v, want a FlushError wrapping boom", err)
	}
	if len(changes) != 1 || changes[0].Name != "thing_created" {
		t.Errorf("changes %+v, want the create before the flush error", changes)
	}
}
