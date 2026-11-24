package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// debugCtx returns a context whose logger keeps entries at level, and the
// entries it kept.
func debugCtx(level string) (context.Context, *[]report.LogEntry) {
	var got []report.LogEntry
	l := runlog.New(level, func(e report.LogEntry) { got = append(got, e) }, nil)
	return runlog.WithLogger(context.Background(), l), &got
}

// voxagentLines returns the messages of the debug entries sourced voxagent
// that start with prefix.
func voxagentLines(entries []report.LogEntry, prefix string) []string {
	var out []string
	for _, e := range entries {
		if e.Level == "debug" && e.Source == "voxagent" && strings.HasPrefix(e.Message, prefix) {
			out = append(out, e.Message)
		}
	}
	return out
}

func TestPluginsyncLogsFetchedAndUnchanged(t *testing.T) {
	s := newFakeFileServer()
	standardTree(s)
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	ctx, got := debugCtx("debug")
	for range 2 {
		if err := Pluginsync(ctx, c, "production", libdir, factdest, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"pluginsync pluginfacts: 3 fetched, 0 unchanged",
		"pluginsync plugins: 1 fetched, 0 unchanged",
		"pluginsync pluginfacts: 0 fetched, 3 unchanged",
		"pluginsync plugins: 0 fetched, 1 unchanged",
	}
	lines := voxagentLines(*got, "pluginsync ")
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("pluginsync lines = %q\nwant %q", lines, want)
	}
}

func TestPluginsyncLogsNothingBelowDebug(t *testing.T) {
	s := newFakeFileServer()
	standardTree(s)
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	ctx, got := debugCtx("info")
	if err := Pluginsync(ctx, c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Errorf("entries at info = %v, want none", *got)
	}
}

func TestFactsLogsArgvAndDuration(t *testing.T) {
	ctx, got := debugCtx("debug")
	dir := t.TempDir()
	if _, err := Facts(ctx, FactsConfig{Facter: fakeFacter(t, "facter-basic.sh"), Certname: "secret-node.example.test", PluginFactDest: dir}); err != nil {
		t.Fatal(err)
	}
	lines := voxagentLines(*got, "facter ")
	re := regexp.MustCompile(`^facter --json --external-dir ` + regexp.QuoteMeta(dir) + ` in \d+(\.\d+)?m?s$`)
	if len(lines) != 1 || !re.MatchString(lines[0]) {
		t.Fatalf("facter lines = %q, want one matching %v", lines, re)
	}
	for _, l := range lines {
		if strings.Contains(l, "Linux") || strings.Contains(l, "secret-node") {
			t.Errorf("facter line carries a fact value: %q", l)
		}
	}
}

func TestFactsLogsOnFailureToo(t *testing.T) {
	ctx, got := debugCtx("debug")
	if _, err := Facts(ctx, FactsConfig{Facter: fakeFacter(t, "facter-fail.sh")}); err == nil {
		t.Fatal("facter failure was accepted")
	}
	if lines := voxagentLines(*got, "facter "); len(lines) != 1 {
		t.Errorf("facter lines = %q, want one even when facter fails", lines)
	}
}

func TestFetchSourcesLogsBytesAndDuration(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f"}))
	ctx, got := debugCtx("debug")
	if _, err := FetchSources(ctx, c, cat, "production", testServer, dir); err != nil {
		t.Fatal(err)
	}
	lines := voxagentLines(*got, "source ")
	re := regexp.MustCompile(`^source puppet:///modules/m/f: 7 bytes in \d+(\.\d+)?m?s$`)
	if len(lines) != 1 || !re.MatchString(lines[0]) {
		t.Fatalf("source lines = %q, want one matching %v", lines, re)
	}
	if strings.Contains(lines[0], "served") {
		t.Errorf("source line carries file content: %q", lines[0])
	}
}

func TestFetchSourcesTreeSumsBytes(t *testing.T) {
	s := newSourceServer()
	const mount = "modules/m/tree"
	s.tree(mount)
	s.fakeFileServer.file(mount, "a", 0o644, "A\n")
	s.fakeFileServer.file(mount, "b", 0o644, "BB\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/tree", map[string]any{"source": "puppet:///" + mount, "recurse": true}))
	ctx, got := debugCtx("debug")
	if _, err := FetchSources(ctx, c, cat, "production", testServer, dir); err != nil {
		t.Fatal(err)
	}
	lines := voxagentLines(*got, "source ")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "source puppet:///"+mount+": 5 bytes in ") {
		t.Errorf("source lines = %q, want one with 5 bytes", lines)
	}
}

// The HTTP lines of a pluginsync name the path only.
func TestAgentHTTPLinesCarryNoQuery(t *testing.T) {
	s := newFakeFileServer()
	standardTree(s)
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	ctx, got := debugCtx("debug")
	if err := Pluginsync(ctx, c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	var http []string
	for _, e := range *got {
		if strings.HasPrefix(e.Message, "GET ") {
			http = append(http, e.Message)
		}
	}
	if len(http) == 0 {
		t.Fatal("no HTTP lines logged")
	}
	for _, l := range http {
		if strings.ContainsAny(l, "?=&") {
			t.Errorf("HTTP line carries a query string: %q", l)
		}
	}
}

// A marked source is named [redacted] in voxagent's own lines, and so is its
// path in the HTTP lines; an unmarked one is shown as written (D-78).
func TestFetchSourcesRedactsMarkedSource(t *testing.T) {
	for _, tree := range []bool{false, true} {
		for _, marked := range []bool{true, false} {
			t.Run(fmt.Sprintf("tree=%v/marked=%v", tree, marked), func(t *testing.T) {
				s := newSourceServer()
				const mount = "modules/m/D8SECRET"
				params := map[string]any{"source": "puppet:///" + mount}
				if tree {
					s.tree(mount)
					s.fakeFileServer.file(mount, "a", 0o644, "A\n")
					params["recurse"] = true
				} else {
					s.file(mount, "served\n")
				}
				r := fileResource("/etc/f", params)
				if marked {
					r.SensitiveParameters = []string{"source"}
				}
				c := startSources(t, s)
				_, dir := sourcesDir(t)
				ctx, got := debugCtx("debug")
				if _, err := FetchSources(ctx, c, catalogOf(r), "production", testServer, dir); err != nil {
					t.Fatal(err)
				}
				var lines []string
				for _, e := range *got {
					lines = append(lines, e.Message)
				}
				if len(voxagentLines(*got, "source ")) != 1 {
					t.Fatalf("lines = %q, want one source line", lines)
				}
				text := strings.Join(lines, "\n")
				if marked {
					if strings.Contains(text, "D8SECRET") || !strings.Contains(text, "source [redacted]: ") ||
						!strings.Contains(text, "GET /puppet/v3/file_") || !strings.Contains(text, "/[redacted] -> ") {
						t.Errorf("marked source leaks or is not redacted:\n%s", text)
					}
				} else if !strings.Contains(text, "source puppet:///"+mount+": ") || !strings.Contains(text, "/"+mount+" -> ") {
					t.Errorf("unmarked source is not shown:\n%s", text)
				}
			})
		}
	}
}

// One download serves every File with a source; it is redacted when any of
// them marks it.
func TestFetchSourcesRedactsWhenAnyFileMarks(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/D8SECRET", "served\n")
	marked := fileResource("/etc/b", map[string]any{"source": "puppet:///modules/m/D8SECRET"})
	marked.SensitiveParameters = []string{"source"}
	cat := catalogOf(fileResource("/etc/a", map[string]any{"source": "puppet:///modules/m/D8SECRET"}), marked)
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	ctx, got := debugCtx("debug")
	if _, err := FetchSources(ctx, c, cat, "production", testServer, dir); err != nil {
		t.Fatal(err)
	}
	for _, e := range *got {
		if strings.Contains(e.Message, "D8SECRET") {
			t.Errorf("line leaks a source marked by one of its Files: %q", e.Message)
		}
	}
}
