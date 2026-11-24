package parsedfile

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Ported from Puppet's parsedfile_spec.rb (spec §10.2): record? before load
// (25-29: a new Run holds no records), resource_for_record (100-109), backup
// once per prefetch and again after a new one (125-153), flush-failure
// isolation (156-183), native header dropped (227-235), and the text round
// trip (207-213, TestRunToFileRoundTrip).
//
// Not ported, with reasons:
//   - 49-68 swallowed ReadError: unreachable, only the crontab filetype raises
//     one and cron remembers it (spec §3.6).
//   - 70-81 skip_record? override: unused, text records are the only skips.
//   - 83-91 nil read: the type rules it out.
//   - 215-229 native header moved to the top: unused, cron drops it.
//   - 238-262 String separator: unused; Separator is a *regexp.Regexp.

// fakeFile is a FileType that records its reads, writes and backups.
type fakeFile struct {
	text     string
	readErr  error
	writeErr error
	reads    int
	writes   []string
	backups  int
	log      *[]string // shared event log, "<event> <target>"
	target   string
}

func (f *fakeFile) Read(context.Context) (string, error) {
	f.reads++
	*f.log = append(*f.log, "read "+f.target)
	return f.text, f.readErr
}

func (f *fakeFile) Write(_ context.Context, text string) error {
	*f.log = append(*f.log, "write "+f.target)
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, text)
	return nil
}

// backupFile is a fakeFile that is also a Backuper.
type backupFile struct{ *fakeFile }

func (f backupFile) Backup(context.Context) error {
	f.backups++
	*f.log = append(*f.log, "backup "+f.target)
	return nil
}

// fakeRes is a Resource.
type fakeRes struct {
	name, title, target string
	hasTarget           bool
}

func (r *fakeRes) Name() string                 { return r.name }
func (r *fakeRes) Title() string                { return r.title }
func (r *fakeRes) ShouldTarget() (string, bool) { return r.target, r.hasTarget }

func res(name string) *fakeRes { return &fakeRes{name: name, title: name} }

func resAt(name, target string) *fakeRes {
	return &fakeRes{name: name, title: name, target: target, hasTarget: true}
}

type env struct {
	t     *testing.T
	ctx   context.Context
	logs  *[]report.LogEntry
	log   []string // every filetype event, in order
	files map[string]*fakeFile
	made  []string // FileType factory calls, in order
	class *Class
	// plain lists the targets whose filetype is not a Backuper.
	plain map[string]bool
}

func testGrammar(t *testing.T) *Grammar {
	t.Helper()
	g, err := NewGrammar(
		LineType{Name: "comment", Text: true, Match: RE2(regexp.MustCompile(`^#`))},
		LineType{Name: "item", Match: RE2(regexp.MustCompile(`^(\w+) (\w+)$`)), Fields: []string{"name", "val"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func newEnv(t *testing.T, texts map[string]string) *env {
	t.Helper()
	var logs []report.LogEntry
	e := &env{t: t, logs: &logs, files: map[string]*fakeFile{}, plain: map[string]bool{}}
	// Debug lines join the filetype event log so their order is observable.
	l := runlog.New("debug", func(le report.LogEntry) {
		logs = append(logs, le)
		e.log = append(e.log, le.Level+" "+le.Message)
	}, func(report.LogEntry) {})
	e.ctx = runlog.WithLogger(context.Background(), l)
	for target, text := range texts {
		e.files[target] = &fakeFile{text: text, log: &e.log, target: target}
	}
	e.class = &Class{
		Name: "item", TypeName: "thing", Grammar: testGrammar(t), DefaultTarget: "/def",
		Now: func() time.Time { return time.Date(2026, 11, 22, 18, 3, 14, 0, time.UTC) },
		FileType: func(target string) (FileType, error) {
			e.made = append(e.made, target)
			f, ok := e.files[target]
			if !ok {
				f = &fakeFile{log: &e.log, target: target}
				e.files[target] = f
			}
			if e.plain[target] {
				return f, nil
			}
			return backupFile{f}, nil
		},
	}
	return e
}

func (e *env) run() *Run { return NewRun(e.class) }

func record(name, val string) *Record {
	return &Record{Type: "item", Fields: map[string]Value{"name": {Kind: Str, S: name}, "val": {Kind: Str, S: val}}, Flags: map[string]bool{}}
}

func recName(r *Record) string { return r.Fields["name"].S }

func TestPrefetchTargets(t *testing.T) {
	e := newEnv(t, nil)
	e.class.Targets = func(_ context.Context, base []string) ([]string, error) {
		return append(slices.Clone(base), "/extra"), nil
	}
	_, err := e.run().Prefetch(e.ctx, []Resource{resAt("a", "/b"), res("n"), resAt("c", "/def"), resAt("d", "/b"), resAt("e", "")})
	if err != nil {
		t.Fatal(err)
	}
	// Default first, then should-targets first-seen and deduplicated ("" is a
	// real target), then Class.Targets.
	if want := []string{"/def", "/b", "", "/extra"}; !slices.Equal(e.made, want) {
		t.Fatalf("targets read = %q, want %q", e.made, want)
	}
}

func TestPrefetchEmptyDefaultTarget(t *testing.T) {
	e := newEnv(t, map[string]string{"": "a 1\n"})
	e.class.DefaultTarget = ""
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a")})
	if err != nil || got["a"] == nil || got["a"].Target != "" || !got["a"].OnDisk {
		t.Fatalf("got %v, %v", got, err)
	}
	if !slices.Equal(e.made, []string{""}) {
		t.Fatalf("made %q", e.made)
	}
}

func TestPrefetchTargetObjectsCached(t *testing.T) {
	e := newEnv(t, nil)
	r := e.run()
	if _, err := r.Prefetch(e.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Instances(e.ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.made, []string{"/def"}) {
		t.Fatalf("FileType called %q, want once per target per run", e.made)
	}
}

func TestPrefetchRecords(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "# c\na 1\nb 2\n", "/b": "c 3\n"})
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a"), res("b"), resAt("c", "/b"), res("zzz")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["zzz"] != nil {
		t.Fatalf("matched %v", got)
	}
	for name, target := range map[string]string{"a": "/def", "b": "/def", "c": "/b"} {
		r := got[name]
		if r.Target != target || !r.OnDisk || r.Ensure != "present" || r.Fields["val"].S == "" {
			t.Errorf("%s = %+v", name, r)
		}
	}
}

func TestPrefetchParseError(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n", "/b": "a 1\nbogus line here\n", "/c": "x 1\n"})
	_, err := e.run().Prefetch(e.ctx, []Resource{resAt("a", "/b"), resAt("c", "/c")})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if want := `Could not parse line "bogus line here" (file: /b, line: 3)`; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	if slices.Contains(e.made, "/c") {
		t.Fatalf("targets after the failing one were read: %q", e.made)
	}
}

func TestPrefetchFileTypeAndWrapperErrors(t *testing.T) {
	boom := errors.New("boom")
	e := newEnv(t, nil)
	e.class.FileType = func(string) (FileType, error) { return nil, boom }
	if _, err := e.run().Prefetch(e.ctx, nil); !errors.Is(err, boom) {
		t.Fatalf("factory error = %v", err)
	}
	e = newEnv(t, map[string]string{"/def": ""})
	e.files["/def"].readErr = boom
	if _, err := e.run().Prefetch(e.ctx, nil); !errors.Is(err, boom) {
		t.Fatalf("read error = %v", err)
	}
}

func TestPrefetchReadErrorRemembered(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n", "/bad": "ignored 1\n"})
	e.files["/bad"].readErr = ReadError{"Could not read crontab for bad: nope"}
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{resAt("x", "/bad"), res("a")})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] == nil || got["x"] != nil {
		t.Fatalf("got %v: the failed target must read as empty", got)
	}
	if len(*e.logs) != 0 {
		t.Fatalf("a ReadError must not be logged: %v", *e.logs)
	}
	x := &Record{Type: "item", Ensure: "absent", Fields: map[string]Value{"name": {Kind: Str, S: "x"}}}
	err = r.Flush(e.ctx, x, "/bad", true)
	want := "Failed to read /bad's records when prefetching them. Reason: Could not read crontab for bad: nope"
	if err == nil || err.Error() != want {
		t.Fatalf("Flush err = %v, want %q", err, want)
	}
	if len(e.files["/bad"].writes) != 0 || slices.Contains(e.log, "write /bad") {
		t.Fatal("nothing may be written to the failed target")
	}
}

func TestReadErrorFromFactoryRemembered(t *testing.T) {
	e := newEnv(t, nil)
	e.class.FileType = func(target string) (FileType, error) {
		if target == "/bad" {
			return nil, ReadError{"Could not retrieve user bad: x"}
		}
		return &fakeFile{log: &e.log, target: target}, nil
	}
	r := e.run()
	if _, err := r.Prefetch(e.ctx, []Resource{resAt("x", "/bad")}); err != nil {
		t.Fatal(err)
	}
	err := r.Flush(e.ctx, record("x", "1"), "/bad", true)
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to read /bad's records when prefetching them. Reason: Could not retrieve user bad: x") {
		t.Fatalf("err = %v", err)
	}
}

func TestMatchByNameAndReplace(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\nb 2\na 3\n"})
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a"), res("b")})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].Fields["val"].S != "3" || got["b"].Fields["val"].S != "2" {
		t.Fatalf("a=%v b=%v: a later hit replaces an earlier one", got["a"], got["b"])
	}
}

func TestResourceForRecordOverride(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\nb 2\n"})
	e.class.ResourceForRecord = func(rec *Record, byName map[string]Resource) (Resource, bool) {
		if rec.Fields["val"].S == "2" {
			return byName["a"], true
		}
		return nil, false
	}
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a"), res("b")})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].Fields["val"].S != "2" || got["b"] != nil {
		t.Fatalf("got %v", got)
	}
}

func TestResourceForRecordDefaultMisses(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "other 1\n"})
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a")})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func adoptFirst(rec *Record, matchers map[string]Resource, order []string) (Resource, bool) {
	if rec.Fields["val"].S == "3" || len(order) == 0 {
		return nil, false
	}
	return matchers[order[0]], true
}

func TestAdoptNameEqualsTitle(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "# c\nq 1\nr 2\ns 3\n"})
	var orders [][]string
	e.class.Adopt = func(rec *Record, matchers map[string]Resource, order []string) (Resource, bool) {
		orders = append(orders, slices.Clone(order))
		return adoptFirst(rec, matchers, order)
	}
	got, err := e.run().Prefetch(e.ctx, []Resource{res("n1"), res("n2")})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"n1", "n2"}, {"n2"}, {}}; !slices.EqualFunc(orders, want, slices.Equal[[]string]) {
		t.Fatalf("orders = %q, want %q (text records skipped, adopted removed)", orders, want)
	}
	if recName(got["n1"]) != "n1" || recName(got["n2"]) != "n2" || got["n1"].Fields["val"].S != "1" || got["n2"].Fields["val"].S != "2" {
		t.Fatalf("got %v: adopt renames the record to the resource's name", got)
	}
	if len(got) != 2 {
		t.Fatalf("the third record must stay unadopted: %v", got)
	}
}

func TestAdoptNameDiffersFromTitle(t *testing.T) {
	// Ruby keys matchers by name but deletes by title (pf:239, 249): the
	// resource stays adoptable, a later record adopts it too and takes the
	// binding.
	e := newEnv(t, map[string]string{"/def": "q 1\nr 2\n"})
	var orders [][]string
	e.class.Adopt = func(rec *Record, matchers map[string]Resource, order []string) (Resource, bool) {
		orders = append(orders, slices.Clone(order))
		return matchers["n1"], true
	}
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{&fakeRes{name: "n1", title: "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"n1"}, {"n1"}}; !slices.EqualFunc(orders, want, slices.Equal[[]string]) {
		t.Fatalf("orders = %q, want %q", orders, want)
	}
	if got["n1"].Fields["val"].S != "2" || recName(got["n1"]) != "n1" {
		t.Fatalf("got %v: the later record replaces the binding", got)
	}
	for _, rec := range r.records {
		if recName(rec) != "n1" {
			t.Fatalf("record %v not renamed", rec)
		}
	}
}

func TestAdoptNilResourceIsMiss(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "q 1\n"})
	e.class.Adopt = func(*Record, map[string]Resource, []string) (Resource, bool) { return nil, true }
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a")})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestAdoptNotUsedWhenNameMatches(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	e.class.Adopt = func(*Record, map[string]Resource, []string) (Resource, bool) {
		t.Fatal("Adopt called for a record the default lookup matched")
		return nil, false
	}
	if _, err := e.run().Prefetch(e.ctx, []Resource{res("a")}); err != nil {
		t.Fatal(err)
	}
}

func TestPrefetchHook(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\nb 2\n"})
	var seen string
	e.class.PrefetchHook = func(_ context.Context, target string, rs []*Record) ([]*Record, error) {
		seen = target
		for _, r := range rs {
			if !r.OnDisk || r.Ensure != "present" || r.Target != target {
				t.Errorf("hook ran before the engine set %+v", r)
			}
		}
		return rs[1:], nil
	}
	got, err := e.run().Prefetch(e.ctx, []Resource{res("a"), res("b")})
	if err != nil || seen != "/def" || got["a"] != nil || got["b"] == nil {
		t.Fatalf("got %v, %v (hook saw %q)", got, err, seen)
	}
}

func TestInstances(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "# c\na 1\n", "/extra": "b 2\n"})
	e.class.Targets = func(_ context.Context, base []string) ([]string, error) {
		return append(slices.Clone(base), "/extra"), nil
	}
	r := e.run()
	got, err := r.Instances(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || recName(got[0]) != "a" || recName(got[1]) != "b" || got[1].Target != "/extra" {
		t.Fatalf("got %v", got)
	}
	// The run's own records are untouched: a flush writes none of them.
	if err := r.Flush(e.ctx, record("n", "9"), "", false); err != nil {
		t.Fatal(err)
	}
	if w := e.files["/def"].writes; len(w) != 1 || strings.Contains(w[0], "a 1") {
		t.Fatalf("Instances leaked into the run's records: %q", w)
	}
}

func TestInstancesParseError(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "junk junk junk\n"})
	var pe *ParseError
	if _, err := e.run().Instances(e.ctx); !errors.As(err, &pe) || pe.File != "/def" {
		t.Fatalf("err = %v", err)
	}
}

func TestFlushAppendsNewRecordAtEnd(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	r := e.run()
	if _, err := r.Prefetch(e.ctx, []Resource{res("a"), res("n")}); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(e.ctx, record("n", "2"), "", false); err != nil {
		t.Fatal(err)
	}
	w := e.files["/def"].writes
	if len(w) != 1 || !strings.HasSuffix(w[0], "a 1\nn 2\n") {
		t.Fatalf("writes = %q", w)
	}
	if !slices.Contains(*logTexts(e), "Flushing thing provider target /def") {
		t.Fatalf("logs = %v", *e.logs)
	}
}

func logTexts(e *env) *[]string {
	var out []string
	for _, l := range *e.logs {
		if l.Level == "debug" && l.Source == "Puppet" {
			out = append(out, l.Message)
		}
	}
	return &out
}

func TestFlushNewRecordTargets(t *testing.T) {
	e := newEnv(t, nil)
	r := e.run()
	n := record("n", "1")
	if err := r.Flush(e.ctx, n, "/b", true); err != nil {
		t.Fatal(err)
	}
	if n.Target != "/b" || !n.OnDisk || len(e.files["/b"].writes) != 1 {
		t.Fatalf("n = %+v", n)
	}
	m := record("m", "1")
	if err := r.Flush(e.ctx, m, "", false); err != nil {
		t.Fatal(err)
	}
	if m.Target != "/def" || len(e.files["/def"].writes) != 1 {
		t.Fatalf("m = %+v", m)
	}
}

func TestFlushWithoutPriorPrefetch(t *testing.T) {
	// Ruby's record? returns nil before any load (parsedfile_spec 25-29): the
	// run holds no records, so a flush writes only what it is given.
	e := newEnv(t, map[string]string{"/def": "old 1\n"})
	if err := e.run().Flush(e.ctx, record("n", "1"), "", false); err != nil {
		t.Fatal(err)
	}
	if w := e.files["/def"].writes; len(w) != 1 || strings.Contains(w[0], "old") {
		t.Fatalf("writes = %q", w)
	}
}

func TestFlushWritesModifiedTargetsSorted(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n", "/b": "b 1\n", "/a": "c 1\n"})
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{res("a"), resAt("b", "/b"), resAt("c", "/a")})
	if err != nil {
		t.Fatal(err)
	}
	r.MarkModified(got["b"], "", false)
	r.MarkModified(got["c"], "", false)
	if err := r.Flush(e.ctx, got["a"], "", false); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, ev := range e.log {
		if strings.HasPrefix(ev, "write ") {
			writes = append(writes, ev)
		}
	}
	if want := []string{"write /a", "write /b"}; !slices.Equal(writes, want) {
		t.Fatalf("writes %q, want %q (sorted, unmodified /def skipped)", writes, want)
	}
	if want := []string{"Flushing thing provider target /a", "Flushing thing provider target /b"}; !slices.Equal(*logTexts(e), want) {
		t.Fatalf("debug = %q", *logTexts(e))
	}
	// Each debug line precedes its target's backup and write.
	for _, tgt := range []string{"/a", "/b"} {
		d := slices.Index(e.log, "debug Flushing thing provider target "+tgt)
		b, w := slices.Index(e.log, "backup "+tgt), slices.Index(e.log, "write "+tgt)
		if d < 0 || b < d || w < b {
			t.Fatalf("log %q: want debug, backup, write for %s", e.log, tgt)
		}
	}
}

// writtenTargets lists the targets written so far, in order.
func writtenTargets(e *env) []string {
	var w []string
	for _, ev := range e.log {
		if strings.HasPrefix(ev, "write ") {
			w = append(w, strings.TrimPrefix(ev, "write "))
		}
	}
	return w
}

// markThree prefetches /a, /b and /c and marks all three modified.
func markThree(e *env) (*Run, map[string]*Record) {
	e.t.Helper()
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{resAt("a", "/a"), resAt("b", "/b"), resAt("c", "/c")})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, rec := range got {
		r.MarkModified(rec, "", false)
	}
	return r, got
}

func TestFlushTargetsWritesOnlyOwn(t *testing.T) {
	e := newEnv(t, map[string]string{"/a": "a 1\n", "/b": "b 1\n", "/c": "c 1\n"})
	r, got := markThree(e)
	if err := r.FlushTargets(e.ctx, got["c"], "", false, []string{"/a"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a", "/c"}; !slices.Equal(writtenTargets(e), want) {
		t.Fatalf("writes %q, want %q", writtenTargets(e), want)
	}
	if want := []string{"Flushing thing provider target /a", "Flushing thing provider target /c"}; !slices.Equal(*logTexts(e), want) {
		t.Fatalf("debug = %q", *logTexts(e))
	}
	// /b stayed marked: a later Flush writes it.
	if err := r.Flush(e.ctx, got["a"], "", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a", "/c", "/b"}; !slices.Equal(writtenTargets(e), want) {
		t.Fatalf("writes %q, want %q", writtenTargets(e), want)
	}
}

func TestFlushTargetsNewRecordTarget(t *testing.T) {
	e := newEnv(t, map[string]string{"/b": "b 1\n"})
	r := e.run()
	if _, err := r.Prefetch(e.ctx, []Resource{resAt("b", "/b")}); err != nil {
		t.Fatal(err)
	}
	r.modified["/b"] = true
	if err := r.FlushTargets(e.ctx, record("n", "1"), "/new", true, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/new"}; !slices.Equal(writtenTargets(e), want) {
		t.Fatalf("writes %q, want %q", writtenTargets(e), want)
	}
	if !r.modified["/b"] {
		t.Fatal("/b must stay marked")
	}
}

func TestFlushTargetsFailureKeepsLaterMarked(t *testing.T) {
	boom := errors.New("Failed to write to bad file")
	e := newEnv(t, map[string]string{"/a": "a 1\n", "/b": "b 1\n", "/c": "c 1\n"})
	e.files["/b"].writeErr = boom
	r, got := markThree(e)
	err := r.FlushTargets(e.ctx, got["a"], "", false, []string{"/b", "/c"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// /a written, /b attempted and failed, /c never reached.
	if want := []string{"/a", "/b"}; !slices.Equal(writtenTargets(e), want) {
		t.Fatalf("writes %q, want %q", writtenTargets(e), want)
	}
	if r.modified["/a"] || r.modified["/b"] || !r.modified["/c"] {
		t.Fatalf("modified = %v; want only /c left", r.modified)
	}
}

func TestMarkModifiedShouldTarget(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{res("a")})
	if err != nil {
		t.Fatal(err)
	}
	// should-target differs from the record's: both are marked.
	r.MarkModified(got["a"], "/new", true)
	if err := r.Flush(e.ctx, got["a"], "", false); err != nil {
		t.Fatal(err)
	}
	if len(e.files["/def"].writes) != 1 || len(e.files["/new"].writes) != 1 {
		t.Fatalf("writes def=%q new=%q", e.files["/def"].writes, e.files["/new"].writes)
	}
}

func TestMarkModifiedNoTargetNoShould(t *testing.T) {
	e := newEnv(t, nil)
	r := e.run()
	r.MarkModified(&Record{Type: "item"}, "", false)
	r.MarkModified(&Record{Type: "item", Target: "/def"}, "/def", true)
	if err := r.Flush(e.ctx, &Record{Type: "item", OnDisk: true, Target: "/def"}, "", false); err != nil {
		t.Fatal(err)
	}
	if got := len(e.files["/def"].writes); got != 1 {
		t.Fatalf("/def written %d times", got)
	}
	if len(e.made) != 1 {
		t.Fatalf("the empty target must not be marked: %q", e.made)
	}
}

func TestFlushUnchangedRunWritesNothing(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{res("a")})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(e.ctx, got["a"], "", false); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(e.log, func(s string) bool { return strings.HasPrefix(s, "write ") || strings.HasPrefix(s, "backup ") }) {
		t.Fatalf("log %q", e.log)
	}
	if len(*logTexts(e)) != 0 {
		t.Fatalf("debug %q", *logTexts(e))
	}
}

func TestFlushDropsAbsent(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\nb 2\n"})
	r := e.run()
	got, err := r.Prefetch(e.ctx, []Resource{res("a"), res("b")})
	if err != nil {
		t.Fatal(err)
	}
	got["a"].Ensure = "absent"
	r.MarkModified(got["a"], "", false)
	if err := r.Flush(e.ctx, got["a"], "", false); err != nil {
		t.Fatal(err)
	}
	if w := e.files["/def"].writes; len(w) != 1 || strings.Contains(w[0], "a 1") || !strings.Contains(w[0], "b 2") {
		t.Fatalf("writes = %q", w)
	}
}

func TestFlushFailureIsolation(t *testing.T) {
	boom := errors.New("Failed to write to bad file")
	e := newEnv(t, map[string]string{"/a": "", "/b": "", "/c": ""})
	e.files["/b"].writeErr = boom
	r := e.run()
	if _, err := r.Prefetch(e.ctx, []Resource{resAt("x", "/a"), resAt("y", "/b"), resAt("z", "/c")}); err != nil {
		t.Fatal(err)
	}
	for _, tgt := range []string{"/a", "/b", "/c"} {
		r.MarkModified(&Record{Type: "item", Target: tgt}, "", false)
	}
	err := r.Flush(e.ctx, &Record{Type: "item", OnDisk: true}, "", false)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// /a written, /b attempted and failed, /c never reached.
	if len(e.files["/a"].writes) != 1 || len(e.files["/c"].writes) != 0 {
		t.Fatalf("a=%q c=%q", e.files["/a"].writes, e.files["/c"].writes)
	}
	// Attempted targets are cleared; later ones stay marked: the next flush
	// writes only /c.
	e.files["/b"].writeErr = nil
	if err := r.Flush(e.ctx, &Record{Type: "item", OnDisk: true}, "", false); err != nil {
		t.Fatal(err)
	}
	if len(e.files["/a"].writes) != 1 || len(e.files["/b"].writes) != 0 || len(e.files["/c"].writes) != 1 {
		t.Fatalf("a=%q b=%q c=%q", e.files["/a"].writes, e.files["/b"].writes, e.files["/c"].writes)
	}
}

func TestFlushFailureDoesNotBlockLaterRecord(t *testing.T) {
	// parsedfile_spec 156-183: after one resource's flush fails, another
	// resource's target still flushes.
	boom := errors.New("Failed to write to bad file")
	e := newEnv(t, nil)
	e.files["broken"] = &fakeFile{log: &e.log, target: "broken", writeErr: boom}
	r := e.run()
	if err := r.Flush(e.ctx, record("one", "1"), "broken", true); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := r.Flush(e.ctx, record("two", "2"), "writable", true); err != nil {
		t.Fatal(err)
	}
	if len(e.files["writable"].writes) != 1 {
		t.Fatal("writable not written")
	}
}

func TestToFileError(t *testing.T) {
	e := newEnv(t, nil)
	err := e.run().Flush(e.ctx, &Record{Type: "nosuch"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "Invalid record type") {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupOncePerPrefetch(t *testing.T) {
	// parsedfile_spec 125-153.
	e := newEnv(t, map[string]string{"/my": ""})
	r := e.run()
	if _, err := r.Prefetch(e.ctx, []Resource{resAt("x", "/my")}); err != nil {
		t.Fatal(err)
	}
	backups := func() int { return strings.Count(strings.Join(e.log, "\n"), "backup /my") }
	for range 2 {
		if err := r.BackupTarget(e.ctx, "/my"); err != nil {
			t.Fatal(err)
		}
	}
	if backups() != 1 {
		t.Fatalf("backups = %d, want 1", backups())
	}
	if _, err := r.Prefetch(e.ctx, []Resource{resAt("x", "/my")}); err != nil {
		t.Fatal(err)
	}
	if err := r.BackupTarget(e.ctx, "/my"); err != nil {
		t.Fatal(err)
	}
	if backups() != 2 {
		t.Fatalf("backups = %d, want 2 after a new prefetch", backups())
	}
}

func TestBackupOnceAcrossFlushes(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": ""})
	r := e.run()
	for _, n := range []string{"a", "b"} {
		if err := r.Flush(e.ctx, record(n, "1"), "", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(strings.Join(e.log, "\n"), "backup /def"); got != 1 {
		t.Fatalf("backups = %d", got)
	}
}

func TestBackupSkippedWithoutBackuper(t *testing.T) {
	e := newEnv(t, nil)
	e.plain["/def"] = true
	r := e.run()
	if err := r.BackupTarget(e.ctx, "/def"); err != nil {
		t.Fatal(err)
	}
	if err := r.Flush(e.ctx, record("a", "1"), "", false); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(e.log, func(s string) bool { return strings.HasPrefix(s, "backup") }) {
		t.Fatalf("log %q", e.log)
	}
}

func TestBackupErrors(t *testing.T) {
	boom := errors.New("boom")
	e := newEnv(t, nil)
	e.class.FileType = func(string) (FileType, error) { return nil, boom }
	if err := e.run().BackupTarget(e.ctx, "/x"); !errors.Is(err, boom) {
		t.Fatalf("factory err = %v", err)
	}
	f := &failingBackup{fakeFile: &fakeFile{log: &e.log}}
	e.class.FileType = func(string) (FileType, error) { return f, nil }
	r := e.run()
	if err := r.BackupTarget(e.ctx, "/x"); !errors.Is(err, errBoom) {
		t.Fatalf("backup err = %v", err)
	}
	if err := r.BackupTarget(e.ctx, "/x"); !errors.Is(err, errBoom) || f.calls != 2 {
		t.Fatalf("a failed backup must not be marked done: %v, %d calls", err, f.calls)
	}
}

var errBoom = errors.New("boom")

type failingBackup struct {
	*fakeFile
	calls int
}

func (f *failingBackup) Backup(context.Context) error { f.calls++; return errBoom }

func TestFlushStopsOnBackupFailure(t *testing.T) {
	e := newEnv(t, nil)
	f := &failingBackup{fakeFile: &fakeFile{log: &e.log, target: "/def"}}
	e.class.FileType = func(string) (FileType, error) { return f, nil }
	if err := e.run().Flush(e.ctx, record("a", "1"), "", false); err == nil {
		t.Fatal("want the backup error")
	}
	if len(f.writes) != 0 {
		t.Fatal("written despite a failed backup")
	}
}

const wantHeader = "# HEADER: This file was autogenerated at 2026-11-22 18:03:14 +0000\n" +
	"# HEADER: by puppet.  While it can still be managed manually, it\n" +
	"# HEADER: is definitely not recommended.\n"

func TestRunToFileRoundTrip(t *testing.T) {
	// parsedfile_spec 207-213: nothing changes but the added header.
	input := "# one\n\na 1\n"
	e := newEnv(t, map[string]string{"/def": input})
	e.class.Grammar = mustGrammar(t,
		LineType{Name: "text", Text: true, Match: RE2(regexp.MustCompile(`^#`))},
		LineType{Name: "blank", Text: true, Match: RE2(regexp.MustCompile(`^$`))},
		LineType{Name: "item", Match: RE2(regexp.MustCompile(`^(\w+) (\w+)$`)), Fields: []string{"name", "val"}},
	)
	r := e.run()
	if _, err := r.Prefetch(e.ctx, nil); err != nil {
		t.Fatal(err)
	}
	r.MarkModified(&Record{Target: "/def"}, "", false)
	if err := r.Flush(e.ctx, &Record{OnDisk: true}, "", false); err != nil {
		t.Fatal(err)
	}
	if got := e.files["/def"].writes[0]; got != wantHeader+input {
		t.Fatalf("got %q", got)
	}
}

func mustGrammar(t *testing.T, types ...LineType) *Grammar {
	t.Helper()
	g, err := NewGrammar(types...)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestToFileOrder(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "# keep\n# VENDOR\na 1\n# VENDOR\n"})
	e.class.NativeHeader = regexp.MustCompile(`# VENDOR\n`)
	e.class.Header = func(now time.Time) string { return "H" + now.Format("2006") + "\n" }
	e.class.ToFile = func(text string) string { return strings.ToUpper(text) }
	r := e.run()
	if _, err := r.Prefetch(e.ctx, nil); err != nil {
		t.Fatal(err)
	}
	r.MarkModified(&Record{Target: "/def"}, "", false)
	if err := r.Flush(e.ctx, &Record{OnDisk: true}, "", false); err != nil {
		t.Fatal(err)
	}
	// Grammar text, first native header span removed, header prepended (after
	// the removal, so a header cannot be matched), then Class.ToFile.
	if got, want := e.files["/def"].writes[0], "H2026\n# KEEP\nA 1\n# VENDOR\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestToFileNativeHeaderNotMatchedInGeneratedHeader(t *testing.T) {
	e := newEnv(t, nil)
	e.class.NativeHeader = regexp.MustCompile(`# HEADER.*\n`)
	if err := e.run().Flush(e.ctx, record("a", "1"), "", false); err != nil {
		t.Fatal(err)
	}
	if got := e.files["/def"].writes[0]; got != wantHeader+"a 1\n" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultHeaderUsesLocalZone(t *testing.T) {
	// Ruby's Time#to_s of a local time: M8 measured +0530 under Asia/Kolkata.
	e := newEnv(t, nil)
	kolkata := time.FixedZone("IST", 5*3600+1800)
	e.class.Now = func() time.Time { return time.Date(2026, 11, 22, 18, 3, 14, 0, kolkata) }
	if err := e.run().Flush(e.ctx, record("a", "1"), "", false); err != nil {
		t.Fatal(err)
	}
	if got := e.files["/def"].writes[0]; !strings.HasPrefix(got, "# HEADER: This file was autogenerated at 2026-11-22 18:03:14 +0530\n") {
		t.Fatalf("got %q", got)
	}
}

func TestNilNowUsesClock(t *testing.T) {
	e := newEnv(t, nil)
	e.class.Now = nil
	before := time.Now()
	if err := e.run().Flush(e.ctx, record("a", "1"), "", false); err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(e.files["/def"].writes[0], "\n")
	stamp := strings.TrimPrefix(line, "# HEADER: This file was autogenerated at ")
	got, err := time.Parse("2006-01-02 15:04:05 -0700", stamp)
	if err != nil || got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("stamp %q: %v", stamp, err)
	}
}

func TestRunsAreIndependent(t *testing.T) {
	// D-96: no state outside the Run.
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	first := e.run()
	if _, err := first.Prefetch(e.ctx, []Resource{resAt("x", "/b")}); err != nil {
		t.Fatal(err)
	}
	first.MarkModified(&Record{Target: "/b"}, "", false)
	e.made = nil
	second := e.run()
	if _, err := second.Prefetch(e.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.made, []string{"/def"}) {
		t.Fatalf("second run read %q", e.made)
	}
	if err := second.Flush(e.ctx, &Record{Type: "item", OnDisk: true}, "", false); err != nil {
		t.Fatal(err)
	}
	if len(e.files["/b"].writes) != 0 {
		t.Fatal("a modified mark leaked between runs")
	}
}

func TestPrefetchHookErrorAbortsUndecorated(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	e.class.PrefetchHook = func(context.Context, string, []*Record) ([]*Record, error) { return nil, errors.New("hook failed") }
	run := e.run()
	if got, err := run.Prefetch(e.ctx, nil); err == nil || err.Error() != "hook failed" || got != nil {
		t.Errorf("Prefetch = %v, %v", got, err)
	}
	if _, err := run.Instances(e.ctx); err == nil || err.Error() != "hook failed" {
		t.Errorf("Instances error = %v", err)
	}
}

func TestTargetsErrorAbortsPrefetchAndInstances(t *testing.T) {
	e := newEnv(t, map[string]string{"/def": "a 1\n"})
	e.class.Targets = func(context.Context, []string) ([]string, error) { return nil, errors.New("scan failed") }
	run := e.run()
	if _, err := run.Prefetch(e.ctx, nil); err == nil || err.Error() != "scan failed" {
		t.Errorf("Prefetch error = %v", err)
	}
	if _, err := run.Instances(e.ctx); err == nil || err.Error() != "scan failed" {
		t.Errorf("Instances error = %v", err)
	}
	if len(e.log) != 0 {
		t.Errorf("a file was read before the targets were known: %v", e.log)
	}
}
