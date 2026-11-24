package croncore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/parsedfile/vectorstest"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// A time neither pattern matches is rejected, not indexed: the parse fails for
// its line. The line pattern keeps this unreachable through Parse.
func TestSplitTimeRejectsATimeNoPatternMatches(t *testing.T) {
	for _, tm := range []string{"", "1 2 3 4", "x"} {
		r := &parsedfile.Record{Fields: map[string]parsedfile.Value{"time": {Kind: parsedfile.Str, S: tm}}}
		if err := splitTime(r); !errors.Is(err, parsedfile.ErrNoRecord) {
			t.Errorf("splitTime(%q) = %v; want ErrNoRecord", tm, err)
		}
	}
}

// fixedNow is the clock of every class under test: 2026-11-22 03:04:05 UTC.
var fixedNow = func() time.Time { return time.Date(2026, 11, 22, 3, 4, 5, 0, time.UTC) }

// fakeFile is a FileType over a string; writes are recorded.
type fakeFile struct {
	text   string
	writes []string
}

func (f *fakeFile) Read(context.Context) (string, error) { return f.text, nil }
func (f *fakeFile) Write(_ context.Context, text string) error {
	f.writes = append(f.writes, text)
	return nil
}

// testClass builds a cronClass whose filetypes are the given fakes by target.
func testClass(release, spoolDir string, files map[string]*fakeFile) *parsedfile.Class {
	return cronClass(release, "root", spoolDir, func(target string) (parsedfile.FileType, error) {
		if f, ok := files[target]; ok {
			return f, nil
		}
		f := &fakeFile{}
		files[target] = f
		return f, nil
	}, fixedNow)
}

// logCtx returns a context whose debug lines are collected as "source: message".
func logCtx(lines *[]string) context.Context {
	l := runlog.New("debug", func(e report.LogEntry) {
		*lines = append(*lines, e.Level+" "+e.Source+": "+e.Message)
	}, func(report.LogEntry) {})
	return runlog.WithLogger(context.Background(), l)
}

// parsed parses text with cls's grammar and marks the records as prefetch does.
func parsed(t *testing.T, cls *parsedfile.Class, target, text string) []*parsedfile.Record {
	t.Helper()
	recs, err := cls.Grammar.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		r.OnDisk, r.Target, r.Ensure = true, target, "present"
	}
	return recs
}

func TestCronClassIdentity(t *testing.T) {
	var got string
	cls := cronClass("2.1.0", "bin", "/spool", func(target string) (parsedfile.FileType, error) {
		got = target
		return &fakeFile{}, nil
	}, fixedNow)
	if cls.Name != "crontab" || cls.TypeName != "cron" || cls.DefaultTarget != "bin" {
		t.Errorf("identity = %q %q %q", cls.Name, cls.TypeName, cls.DefaultTarget)
	}
	if _, err := cls.FileType("alice"); err != nil || got != "alice" {
		t.Errorf("FileType did not reach newFileType: %q, %v", got, err)
	}
	if cls.Grammar == nil || cls.NativeHeader == nil || cls.PrefetchHook == nil || cls.Adopt == nil ||
		cls.ResourceForRecord == nil || cls.Targets == nil || cls.ToFile == nil || cls.Header == nil || cls.Now == nil {
		t.Errorf("class has an unset member: %+v", cls)
	}
}

// ---- the shared vectors -------------------------------------------------

func TestCronGrammarMatchesSharedVectors(t *testing.T) {
	g := testClass("2.1.0", "/nonexistent", map[string]*fakeFile{}).Grammar
	for _, c := range vectorstest.Read(t, "cron") {
		t.Run(c.Name+"/"+c.Op, func(t *testing.T) { c.Check(t, g) })
	}
}

// ---- grammar edges the vectors do not pin -------------------------------

func TestCronParseSpecialInTimeWord(t *testing.T) {
	// prov:39: the special test is an unanchored search for @(\S+) in the time
	// text, so a five-word time holding an @ is a special.
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	recs := parsed(t, cls, "root", "a@b * * * * cmd")
	if got := recs[0].Fields["special"]; got.Kind != parsedfile.Str || got.S != "b" {
		t.Errorf("special = %+v", got)
	}
	if got := recs[0].Fields["minute"]; got.Kind != parsedfile.Absent {
		t.Errorf("minute = %+v, want Absent", got)
	}
}

func TestCronParseListSplitDropsTrailingEmpties(t *testing.T) {
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	recs := parsed(t, cls, "root", "1,,2,, 5, * * * cmd")
	f := recs[0].Fields
	if want := (parsedfile.Value{Kind: parsedfile.List, L: []string{"1", "", "2"}}); !reflect.DeepEqual(f["minute"], want) {
		t.Errorf("minute = %+v, want %+v", f["minute"], want)
	}
	if want := (parsedfile.Value{Kind: parsedfile.List, L: []string{"5"}}); !reflect.DeepEqual(f["hour"], want) {
		t.Errorf("hour = %+v, want %+v", f["hour"], want)
	}
}

func TestCronParseStarCommandIsAbsent(t *testing.T) {
	// The record type's absent text is "*", so a command of exactly "*" is Absent.
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	recs := parsed(t, cls, "root", "1 * * * * *")
	if got := recs[0].Fields["command"]; got.Kind != parsedfile.Absent {
		t.Errorf("command = %+v, want Absent", got)
	}
}

// ---- generated lines ----------------------------------------------------

func cronRecord(fields map[string]parsedfile.Value, flags ...string) parsedfile.Record {
	r := parsedfile.Record{Type: "crontab", Fields: fields, Flags: map[string]bool{}}
	for _, f := range flags {
		r.Flags[f] = true
	}
	return r
}

func TestCronToLine(t *testing.T) {
	g := testClass("2.1.0", "/x", map[string]*fakeFile{}).Grammar
	cases := []struct {
		name string
		rec  parsedfile.Record
		want string
	}{
		{"named", cronRecord(map[string]parsedfile.Value{"name": str("n"), "minute": list("5"), "command": str("c")}), "# Puppet Name: n\n5 * * * * c"},
		{"unmanaged never gets a name", cronRecord(map[string]parsedfile.Value{"name": str("unmanaged:c-1"), "minute": list("5"), "command": str("c")}, "unmanaged"), "5 * * * * c"},
		{"unmanaged keeps its environment", cronRecord(map[string]parsedfile.Value{"name": str("u"), "environment": list("A=1"), "command": str("c")}, "unmanaged"), "A=1\n* * * * * c"},
		{"special", cronRecord(map[string]parsedfile.Value{"special": str("reboot"), "minute": list("5"), "command": str("c")}), "@reboot c"},
		{"empty special is still special", cronRecord(map[string]parsedfile.Value{"special": str(""), "command": str("c")}), "@ c"},
		{"empty time list is an empty field", cronRecord(map[string]parsedfile.Value{"minute": {Kind: parsedfile.List, L: []string{}}, "command": str("c")}), " * * * * c"},
		{"list with absent element", cronRecord(map[string]parsedfile.Value{"minute": list("5", "absent"), "command": str("c")}), "5,absent * * * * c"},
		{"environment absent", cronRecord(map[string]parsedfile.Value{"name": str("n"), "environment": absent, "command": str("c")}), "# Puppet Name: n\n* * * * * c"},
		{"environment empty", cronRecord(map[string]parsedfile.Value{"name": str("n"), "environment": {Kind: parsedfile.List, L: []string{}}, "command": str("c")}), "# Puppet Name: n\n* * * * * c"},
		{"environment before special", cronRecord(map[string]parsedfile.Value{"environment": list("A=1", "B=2"), "special": str("daily"), "command": str("c")}), "A=1\nB=2\n@daily c"},
		{"empty command", cronRecord(map[string]parsedfile.Value{"command": str("")}), "* * * * * "},
		{"special absent is time fields", cronRecord(map[string]parsedfile.Value{"special": absent, "hour": list("1"), "command": str("c")}), "* 1 * * * c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := g.ToLine(c.rec)
			if err != nil || got != c.want {
				t.Errorf("ToLine = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

// TestCronLineBreaksWritten is the write half of §4.5: every value reaches the
// generated text byte for byte, so a line break adds crontab lines.
func TestCronLineBreaksWritten(t *testing.T) {
	g := testClass("2.1.0", "/x", map[string]*fakeFile{}).Grammar
	values := map[string]parsedfile.Value{
		"name":        str("n\n# x"),
		"minute":      list("1\n2"),
		"hour":        list("3\r4"),
		"command":     str("true\n# y"),
		"environment": list("A=1\nB=2"),
	}
	line, err := g.ToLine(cronRecord(values))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"n\n# x", "1\n2", "3\r4", "true\n# y", "A=1\nB=2"} {
		if !strings.Contains(line, f) {
			t.Errorf("%q is not in %q", f, line)
		}
	}
	special, err := g.ToLine(cronRecord(map[string]parsedfile.Value{"special": str("a\nb"), "command": str("c")}))
	if err != nil || special != "@a\nb c" {
		t.Errorf("special = %q, %v", special, err)
	}
}

func TestCronToFileThroughRun(t *testing.T) {
	vixie := "# DO NOT EDIT THIS FILE - edit the master and reinstall.\n# (- installed on Mon Nov 22 00:00:00 2026)\n# (Cron version V5.0 -- $Id: crontab.c,v 1.12 2004/01/23 18:56:42 vixie Exp $)\n"
	files := map[string]*fakeFile{"root": {text: vixie + "TZ=UTC\n# Puppet Name: a\n1 * * * * x\n"}}
	cls := testClass("2.1.0", "/nonexistent", files)
	run := parsedfile.NewRun(cls)
	ctx := context.Background()
	if _, err := run.Prefetch(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rec := &parsedfile.Record{Type: "crontab", Ensure: "present", Fields: map[string]parsedfile.Value{
		"name": str("b"), "minute": list("2"), "command": str("y"),
	}, Flags: map[string]bool{}}
	if err := run.Flush(ctx, rec, "root", true); err != nil {
		t.Fatal(err)
	}
	want := "TZ=UTC\n" +
		"# HEADER: This file was autogenerated at 2026-11-22 03:04:05 +0000 by puppet.\n" +
		"# HEADER: While it can still be managed manually, it is definitely not recommended.\n" +
		"# HEADER: Note particularly that the comments starting with 'Puppet Name' should\n" +
		"# HEADER: not be deleted, as doing so could cause duplicate cron jobs.\n" +
		"# Puppet Name: a\n1 * * * * x\n# Puppet Name: b\n2 * * * * y\n"
	if w := files["root"].writes; len(w) != 1 || w[0] != want {
		t.Errorf("writes = %q\nwant     %q", w, want)
	}
}

func TestCronHeaderUsesZoneOffset(t *testing.T) {
	zone := time.FixedZone("IST", 5*3600+1800)
	cls := cronClass("2.1.0", "root", "/x", nil, func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, zone) })
	if got := cls.Header(cls.Now()); !strings.HasPrefix(got, "# HEADER: This file was autogenerated at 2026-01-02 03:04:05 +0530 by puppet.\n") {
		t.Errorf("header = %q", got)
	}
}

func TestCronTZHoist(t *testing.T) {
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	cases := []struct{ name, in, want string }{
		{"no TZ line", "H\n1 * * * * x\n", "H\n1 * * * * x\n"},
		{"moved to the top", "H\nTZ=UTC\n1 * * * * x\n", "TZ=UTC\nH\n1 * * * * x\n"},
		// M9: the first literal occurrence of the line's text is cut, wherever it is.
		{"first literal occurrence", "H\n1 * * * * echo TZ=UTC\nTZ=UTC\nx\n", "TZ=UTC\nH\n1 * * * * echo TZ=UTC\nx\n"},
		{"TZ without a newline is not a line", "H\nTZ=UTC", "H\nTZ=UTC"},
		{"empty TZ value is not a line", "H\nTZ=\n", "H\nTZ=\n"},
		{"indented TZ is not a line", "H\n TZ=UTC\n", "H\n TZ=UTC\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cls.ToFile(c.in); got != c.want {
				t.Errorf("ToFile(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ---- PrefetchHook -------------------------------------------------------

// hooked parses text and runs the class's hook on it.
func hooked(t *testing.T, cls *parsedfile.Class, text string) []*parsedfile.Record {
	t.Helper()
	recs, err := cls.PrefetchHook("root", parsed(t, cls, "root", text))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// shape summarises records as type:name[:env] strings for compact comparison.
func shape(recs []*parsedfile.Record) []string {
	var out []string
	for _, r := range recs {
		s := r.Type
		if n, ok := r.Fields["name"]; ok && n.Kind == parsedfile.Str {
			s += ":" + n.S
		}
		if r.Flags["unmanaged"] {
			s += "!"
		}
		switch e := r.Fields["environment"]; e.Kind {
		case parsedfile.List:
			s += fmt.Sprintf("%q", e.L)
		case parsedfile.Absent:
			s += "[absent]"
		}
		out = append(out, s)
	}
	return out
}

func TestCronPrefetchHook(t *testing.T) {
	cases := []struct {
		name, text string
		want130    []string
		want210    []string // nil: same as want130
	}{
		{
			name:    "named job, comment dropped",
			text:    "# Puppet Name: a\n1 * * * * x",
			want130: []string{"crontab:a[absent]"},
			want210: []string{`crontab:a[]`},
		},
		{
			name:    "environment lines of a named job move into it",
			text:    "# Puppet Name: a\nA=1\nB=2\n1 * * * * x\nC=3",
			want130: []string{`crontab:a["A=1" "B=2"]`, "environment"},
		},
		{
			name:    "environment lines with no name stay",
			text:    "A=1\n1 * * * * x",
			want130: []string{"environment", "crontab:unmanaged:x-1![absent]"},
			want210: []string{"environment", "crontab:unmanaged:x-1![]"},
		},
		{
			name:    "blank and unnamed comment stay and change nothing",
			text:    "# Puppet Name: a\n\n# note\nA=1\n1 * * * * x",
			want130: []string{"blank", "comment", `crontab:a["A=1"]`},
		},
		{
			// §8 item 3: the trailing spaces are part of the name.
			name:    "name keeps trailing spaces",
			text:    "# Puppet Name: j2   \n1 * * * * x",
			want130: []string{"crontab:j2   [absent]"},
			want210: []string{"crontab:j2   []"},
		},
		{
			// §8 item 4: an empty envs stays non-nil, so later environment lines
			// attach to the next job.
			name:    "environment lines migrate to the next job",
			text:    "# Puppet Name: a\n1 * * * * x\nA=1\n2 * * * * y",
			want130: []string{"crontab:a[absent]", `crontab:unmanaged:y-1!["A=1"]`},
			want210: []string{"crontab:a[]", `crontab:unmanaged:y-1!["A=1"]`},
		},
		{
			name:    "environment lines vanish when no job follows",
			text:    "# Puppet Name: a\n1 * * * * x\nA=1\nB=2",
			want130: []string{"crontab:a[absent]"},
			want210: []string{"crontab:a[]"},
		},
		{
			name:    "a name with no job is dropped",
			text:    "# Puppet Name: a\n# Puppet Name: b\n",
			want130: []string{},
		},
		{
			name:    "the second name replaces the first",
			text:    "# Puppet Name: a\n# Puppet Name: b\n1 * * * * x",
			want130: []string{"crontab:b[absent]"},
			want210: []string{"crontab:b[]"},
		},
		{
			name:    "unmanaged names number from 1 and squeeze whitespace to one underscore",
			text:    "1 * * * * a   b\tc\n2 * * * * d",
			want130: []string{"crontab:unmanaged:a_b_c-1![absent]", "crontab:unmanaged:d-2![absent]"},
			want210: []string{"crontab:unmanaged:a_b_c-1![]", "crontab:unmanaged:d-2![]"},
		},
		{
			name:    "environment is used once",
			text:    "# Puppet Name: a\nA=1\n1 * * * * x\nB=2\n2 * * * * y",
			want130: []string{`crontab:a["A=1"]`, "environment", "crontab:unmanaged:y-1![absent]"},
			want210: []string{`crontab:a["A=1"]`, "environment", "crontab:unmanaged:y-1![]"},
		},
	}
	for _, c := range cases {
		for _, release := range releases {
			t.Run(c.name+"/"+release, func(t *testing.T) {
				want := c.want130
				if release == "2.1.0" && c.want210 != nil {
					want = c.want210
				}
				got := shape(hooked(t, testClass(release, "/x", map[string]*fakeFile{}), c.text))
				if len(got) == 0 && len(want) == 0 {
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("got  %q\nwant %q", got, want)
				}
			})
		}
	}
}

// TestCronPrefetchHookStarCommand: an unmanaged job whose command is exactly
// "*" has the Absent command, and Ruby's gsub on :absent raises. The text is
// what the oracle printed after "Could not prefetch cron provider 'crontab': "
// (M14a); a named job takes its name and never reaches the gsub.
func TestCronPrefetchHookStarCommand(t *testing.T) {
	const want = "undefined method `gsub' for :absent:Symbol"
	for _, release := range releases {
		cls := testClass(release, "/x", map[string]*fakeFile{})
		recs, err := cls.PrefetchHook("root", parsed(t, cls, "root", "1 * * * * *"))
		if err == nil || err.Error() != want || recs != nil {
			t.Errorf("%s: = %v, %v; want error %q", release, recs, err, want)
		}
		named := hooked(t, testClass(release, "/x", map[string]*fakeFile{}), "# Puppet Name: n\n1 * * * * *")
		if got := shape(named); len(got) != 1 || !strings.HasPrefix(got[0], "crontab:n") {
			t.Errorf("%s: named star job = %q", release, got)
		}
	}
	files := map[string]*fakeFile{"root": {text: "1 * * * * *\n"}}
	_, err := parsedfile.NewRun(testClass("2.1.0", "/nonexistent", files)).Prefetch(context.Background(), nil)
	if err == nil || err.Error() != want {
		t.Errorf("Prefetch error = %v, want %q undecorated", err, want)
	}
}

func TestCronPrefetchHookEmptyEnvironmentValue(t *testing.T) {
	// 2.1.0 gives the empty list (non-nil, prov:224), 1.3.0 Absent.
	r130 := hooked(t, testClass("1.3.0", "/x", map[string]*fakeFile{}), "# Puppet Name: a\n1 * * * * x")
	if got := r130[0].Fields["environment"]; got.Kind != parsedfile.Absent {
		t.Errorf("1.3.0 environment = %+v, want Absent", got)
	}
	r210 := hooked(t, testClass("2.1.0", "/x", map[string]*fakeFile{}), "# Puppet Name: a\n1 * * * * x")
	if got := r210[0].Fields["environment"]; got.Kind != parsedfile.List || got.L == nil || len(got.L) != 0 {
		t.Errorf("2.1.0 environment = %#v, want an empty non-nil list", got)
	}
}

func TestCronPrefetchHookCounterIsPerClass(t *testing.T) {
	a := testClass("2.1.0", "/x", map[string]*fakeFile{})
	b := testClass("2.1.0", "/x", map[string]*fakeFile{})
	first := shape(hooked(t, a, "1 * * * * x"))
	again := shape(hooked(t, a, "1 * * * * x")) // a second target of the same run keeps counting
	other := shape(hooked(t, b, "1 * * * * x")) // a new run starts again at 1
	if !strings.Contains(first[0], "x-1") || !strings.Contains(again[0], "x-2") || !strings.Contains(other[0], "x-1") {
		t.Errorf("counters: %q %q %q", first, again, other)
	}
}

// ---- resource matching --------------------------------------------------

func res(name string, should map[string]parsedfile.Value) cronResource {
	return cronResource{cronParams{Name: name, Title: name, Ensure: "present", Should: should}}
}

func TestCronResourceInterface(t *testing.T) {
	r := cronResource{cronParams{Name: "n", Title: "t", Should: map[string]parsedfile.Value{"target": str("bin")}}}
	if r.Name() != "n" || r.Title() != "t" {
		t.Errorf("Name/Title = %q %q", r.Name(), r.Title())
	}
	if tg, ok := r.ShouldTarget(); !ok || tg != "bin" {
		t.Errorf("ShouldTarget = %q %v", tg, ok)
	}
	if _, ok := (cronResource{cronParams{Should: map[string]parsedfile.Value{}}}).ShouldTarget(); ok {
		t.Error("ShouldTarget set with no target")
	}
}

func TestCronResourceForRecord(t *testing.T) {
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	rec := func(name, target string) *parsedfile.Record {
		return &parsedfile.Record{Type: "crontab", Target: target, Fields: map[string]parsedfile.Value{"name": str(name)}}
	}
	byTarget := map[string]parsedfile.Resource{"j": res("j", map[string]parsedfile.Value{"target": str("root"), "user": str("bin")})}
	byUserOnly := map[string]parsedfile.Resource{"j": res("j", map[string]parsedfile.Value{"user": str("bin")})}
	noTarget := map[string]parsedfile.Resource{"j": res("j", map[string]parsedfile.Value{})}
	cases := []struct {
		name   string
		rec    *parsedfile.Record
		by     map[string]parsedfile.Resource
		wantOK bool
	}{
		{"same name and target", rec("j", "root"), byTarget, true},
		{"target, not user, decides", rec("j", "bin"), byTarget, false},
		{"other target", rec("j", "adm"), byTarget, false},
		{"user is the fallback", rec("j", "bin"), byUserOnly, true},
		{"user fallback mismatch", rec("j", "root"), byUserOnly, false},
		{"neither target nor user", rec("j", "root"), noTarget, false},
		{"no such name", rec("k", "root"), byTarget, false},
		{"record with no name", &parsedfile.Record{Type: "crontab", Target: "root", Fields: map[string]parsedfile.Value{}}, byTarget, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := cls.ResourceForRecord(c.rec, c.by)
			if ok != c.wantOK || (ok && got.Name() != "j") {
				t.Errorf("= %v, %v; want ok %v", got, ok, c.wantOK)
			}
		})
	}
}

func unmanagedRec(target, command string, fields map[string]parsedfile.Value) *parsedfile.Record {
	f := map[string]parsedfile.Value{
		"name": str("unmanaged:c-1"), "command": str(command),
		"minute": absent, "hour": absent, "monthday": absent, "month": absent, "weekday": absent, "special": absent,
	}
	for k, v := range fields {
		f[k] = v
	}
	return &parsedfile.Record{Type: "crontab", Target: target, Fields: f, Flags: map[string]bool{"unmanaged": true}}
}

// runAdopt runs cls.Adopt as the engine does: matchers by name, order as given.
func runAdopt(cls *parsedfile.Class, rec *parsedfile.Record, rs ...cronResource) (string, bool) {
	matchers := map[string]parsedfile.Resource{}
	var order []string
	for _, r := range rs {
		matchers[r.Name()] = r
		order = append(order, r.Name())
	}
	got, ok := cls.Adopt(rec, matchers, order)
	if !ok {
		return "", false
	}
	return got.Name(), true
}

func TestCronAdopt(t *testing.T) {
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	base := map[string]parsedfile.Value{"target": str("root"), "command": str("c")}
	with := func(extra map[string]parsedfile.Value) map[string]parsedfile.Value {
		m := map[string]parsedfile.Value{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name string
		rec  *parsedfile.Record
		rs   []cronResource
		want string // "" = no adoption
	}{
		{"command and target only", unmanagedRec("root", "c", nil), []cronResource{res("a", base)}, "a"},
		{"other target", unmanagedRec("bin", "c", nil), []cronResource{res("a", base)}, ""},
		{"other command", unmanagedRec("root", "d", nil), []cronResource{res("a", base)}, ""},
		{"resource without command", unmanagedRec("root", "c", nil), []cronResource{res("a", map[string]parsedfile.Value{"target": str("root")})}, ""},
		{"record command Absent", unmanagedRec("root", "c", map[string]parsedfile.Value{"command": absent}), []cronResource{res("a", base)}, ""},
		{"resource has no target", unmanagedRec("root", "c", nil), []cronResource{res("a", map[string]parsedfile.Value{"command": str("c")})}, ""},
		{"list equal", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("1", "2")}), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": list("1", "2")}))}, "a"},
		{"list order matters", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("2", "1")}), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": list("1", "2")}))}, ""},
		{"list vs shorter list", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("1")}), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": list("1", "2")}))}, ""},
		{"record Absent vs a value", unmanagedRec("root", "c", nil), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": list("1")}))}, ""},
		{"star-valued resource matches a record's *", unmanagedRec("root", "c", nil), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": absent}))}, "a"},
		{"star-valued resource vs a record value", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("5")}), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": absent}))}, ""},
		{"unmanaged time fields match any record", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("5"), "hour": list("1")}), []cronResource{res("a", base)}, "a"},
		{"special equal", unmanagedRec("root", "c", map[string]parsedfile.Value{"special": str("daily")}), []cronResource{res("a", with(map[string]parsedfile.Value{"special": str("daily")}))}, "a"},
		{"special differs", unmanagedRec("root", "c", map[string]parsedfile.Value{"special": str("daily")}), []cronResource{res("a", with(map[string]parsedfile.Value{"special": str("hourly")}))}, ""},
		{"special on the resource, none on the record", unmanagedRec("root", "c", nil), []cronResource{res("a", with(map[string]parsedfile.Value{"special": str("daily")}))}, ""},
		{"special absent never adopts", unmanagedRec("root", "c", nil), []cronResource{res("a", with(map[string]parsedfile.Value{"special": absent}))}, ""},
		{"special absent never adopts, even a special record", unmanagedRec("root", "c", map[string]parsedfile.Value{"special": str("daily")}), []cronResource{res("a", with(map[string]parsedfile.Value{"special": absent}))}, ""},
		{"a field the record lacks", func() *parsedfile.Record {
			r := unmanagedRec("root", "c", nil)
			delete(r.Fields, "hour")
			return r
		}(), []cronResource{res("a", with(map[string]parsedfile.Value{"hour": absent}))}, ""},
		{"first in catalog order", unmanagedRec("root", "c", nil), []cronResource{res("a", base), res("b", base)}, "a"},
		{"skips non-matching first", unmanagedRec("root", "c", nil), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": list("1")})), res("b", base)}, "b"},
		{"named record is never adopted", &parsedfile.Record{Type: "crontab", Target: "root", Fields: unmanagedRec("root", "c", nil).Fields, Flags: map[string]bool{}}, []cronResource{res("a", base)}, ""},
		{"list value of a [] should", unmanagedRec("root", "c", map[string]parsedfile.Value{"minute": list("1")}), []cronResource{res("a", with(map[string]parsedfile.Value{"minute": {Kind: parsedfile.List, L: []string{}}}))}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := runAdopt(cls, c.rec, c.rs...)
			if ok != (c.want != "") || got != c.want {
				t.Errorf("adopt = %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}

func TestCronAdoptRespectsOrderArgument(t *testing.T) {
	// Adopt walks `order`, not the map: a resource missing from order is not tried.
	cls := testClass("2.1.0", "/x", map[string]*fakeFile{})
	base := map[string]parsedfile.Value{"target": str("root"), "command": str("c")}
	a, b := res("a", base), res("b", base)
	matchers := map[string]parsedfile.Resource{"a": a, "b": b}
	got, ok := cls.Adopt(unmanagedRec("root", "c", nil), matchers, []string{"b"})
	if !ok || got.Name() != "b" {
		t.Errorf("= %v, %v; want b", got, ok)
	}
}

// ---- through the engine -------------------------------------------------

func TestCronPrefetchBindsAdoptedAndNamed(t *testing.T) {
	text := "# Puppet Name: named\n1 * * * * x\n" +
		"2 * * * * y\n" + // unmanaged: adopted by "adopt-me"
		"# Puppet Name: dup\n3 * * * * z\n" +
		"# Puppet Name: dup\n4 * * * * z2\n" // item 14: later wins
	files := map[string]*fakeFile{"root": {text: text}}
	cls := testClass("2.1.0", "/nonexistent", files)
	run := parsedfile.NewRun(cls)
	target := map[string]parsedfile.Value{"target": str("root")}
	with := func(k string, v parsedfile.Value) map[string]parsedfile.Value {
		return map[string]parsedfile.Value{"target": target["target"], k: v}
	}
	rs := []parsedfile.Resource{
		res("named", with("command", str("x"))),
		res("adopt-me", map[string]parsedfile.Value{"target": str("root"), "command": str("y")}),
		res("dup", with("command", str("z"))),
		res("elsewhere", map[string]parsedfile.Value{"target": str("bin"), "command": str("x")}),
	}
	got, err := run.Prefetch(context.Background(), rs)
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(name string) string {
		if r := got[name]; r != nil {
			return r.Fields["command"].S
		}
		return "<none>"
	}
	if cmd("named") != "x" || cmd("adopt-me") != "y" || cmd("dup") != "z2" || cmd("elsewhere") != "<none>" {
		t.Errorf("bindings: named=%s adopt-me=%s dup=%s elsewhere=%s", cmd("named"), cmd("adopt-me"), cmd("dup"), cmd("elsewhere"))
	}
	if !got["adopt-me"].Flags["unmanaged"] {
		t.Error("an adopted record keeps its unmanaged flag")
	}
}

// ---- spool scan ---------------------------------------------------------

func TestCronTargetsSpoolScan(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"alice", "bob", ".keep_pkg-1", "dup"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"subdir"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	other := t.TempDir()
	real := filepath.Join(other, "real")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "gone"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}

	var lines []string
	cls := testClass("2.1.0", dir, map[string]*fakeFile{})
	got, err := cls.Targets(logCtx(&lines), []string{"root", "dup"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) < 2 || got[0] != "root" || got[1] != "dup" {
		t.Fatalf("base targets must lead in order: %q", got)
	}
	// readdir order, as Dir.foreach lists them: the order Readdirnames gives on
	// this directory, less what must be skipped.
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range listed {
		if n == "alice" || n == "bob" || n == "linked" {
			want = append(want, n)
		}
	}
	if len(want) != 3 || !slices.Equal(got[2:], want) {
		t.Errorf("spool targets = %q, want %q in readdir order (dup is not repeated; .keep_*, directories, dangling and directory links are skipped)", got[2:], want)
	}
	if want := []string{"debug Puppet: looking for crontabs in " + dir}; !slices.Equal(lines, want) {
		t.Errorf("log = %q, want %q", lines, want)
	}
}

func TestCronTargetsMissingDirectoryStillLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	var lines []string
	got, err := testClass("2.1.0", dir, map[string]*fakeFile{}).Targets(logCtx(&lines), []string{"root"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"root"}) {
		t.Errorf("targets = %q", got)
	}
	if len(lines) != 1 || lines[0] != "debug Puppet: looking for crontabs in "+dir {
		t.Errorf("log = %q", lines)
	}
}

func TestCronTargetsUnreadableDirectoryAndReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root passes every access check")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	rw := filepath.Join(dir, "rw")
	for _, p := range []string{ro, rw} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(ro, 0o400); err != nil {
		t.Fatal(err)
	}
	var lines []string
	cls := testClass("2.1.0", dir, map[string]*fakeFile{})
	if got, err := cls.Targets(logCtx(&lines), nil); err != nil || !slices.Equal(got, []string{"rw"}) {
		t.Errorf("targets = %q, want only the writable file", got)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if got, err := cls.Targets(logCtx(&lines), []string{"root"}); err != nil || !slices.Equal(got, []string{"root"}) {
		t.Errorf("unreadable directory: targets = %q", got)
	}
}

func TestCronTargetsReadThroughRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alice"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]*fakeFile{"root": {}, "alice": {text: "# Puppet Name: a\n1 * * * * x\n"}}
	cls := testClass("2.1.0", dir, files)
	got, err := parsedfile.NewRun(cls).Prefetch(context.Background(), []parsedfile.Resource{
		res("a", map[string]parsedfile.Value{"target": str("alice"), "command": str("x")}),
	})
	if err != nil || got["a"] == nil {
		t.Fatalf("prefetch = %v, %v", got, err)
	}
}

// TestCronTargetsSpoolPathIsAFile: a readable regular file as the spool path
// passes the access check and fails the listing. Ruby's text, measured in M14b
// on the oracle, with the debug line logged first and no crontab read.
func TestCronTargetsSpoolPathIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	const wantFmt = "Not a directory @ dir_initialize - %s"
	var lines []string
	files := map[string]*fakeFile{"root": {text: "1 * * * * x\n"}}
	cls := testClass("2.1.0", path, files)
	got, err := cls.Targets(logCtx(&lines), []string{"root"})
	if want := fmt.Sprintf(wantFmt, path); err == nil || err.Error() != want || got != nil {
		t.Errorf("Targets = %v, %v; want error %q", got, err, want)
	}
	if want := []string{"debug Puppet: looking for crontabs in " + path}; !slices.Equal(lines, want) {
		t.Errorf("log = %q, want %q", lines, want)
	}

	run := parsedfile.NewRun(cls)
	if _, err := run.Prefetch(logCtx(&lines), nil); err == nil || err.Error() != fmt.Sprintf(wantFmt, path) {
		t.Errorf("Prefetch error = %v", err)
	}
	if _, err := run.Instances(logCtx(&lines)); err == nil || err.Error() != fmt.Sprintf(wantFmt, path) {
		t.Errorf("Instances error = %v", err)
	}
}
