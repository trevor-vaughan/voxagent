package main

import (
	"encoding/json"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const dur = `\d+(\.\d+)?m?s`

// debugReportLines returns the message of each debug entry of rep that
// voxagent logged, in order.
func debugReportLines(t *testing.T, rep map[string]any) []string {
	t.Helper()
	var out []string
	for _, e := range rep["logs"].([]any) {
		m := e.(map[string]any)
		if m["level"] == "debug" && m["source"] == "voxagent" {
			out = append(out, m["message"].(string))
		}
	}
	return out
}

// anyMatches reports whether some line matches re.
func anyMatches(lines []string, re string) bool {
	r := regexp.MustCompile(re)
	return slices.ContainsFunc(lines, r.MatchString)
}

// containerCatalog is Stage, Class, a defined-type instance and a notify,
// contained so the walk is well formed.
func containerCatalog(t *testing.T, dir string) string {
	t.Helper()
	defined := map[string]any{"type": "Foo::Bar", "title": "x", "tags": []any{}, "kind": "defined_type",
		"parameters": map[string]any{"secret_param": "hunter2"}}
	mainClass := map[string]any{"type": "Class", "title": "main", "tags": []any{}, "kind": "class",
		"parameters": map[string]any{"name": "main"}}
	stage := map[string]any{"type": "Stage", "title": "main", "tags": []any{}, "parameters": map[string]any{}}
	path := writeCatalog(t, dir, []map[string]any{stage, mainClass, defined,
		notifyResource("n", "sensitive-message-text", nil)})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": "Foo::Bar[x]"}, {"source": "Foo::Bar[x]", "target": "Notify[n]"}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --debug logs why each container was accepted and each prefetch, to the
// console and the report alike; a run without it holds neither.
func TestApplyLogsContainersAndPrefetch(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	cat := containerCatalog(t, dir)
	reports := filepath.Join(dir, "debug-reports")
	stdout, stderr, code := run(t, bin, "apply", "--catalog", cat, "--report-dir", reports, "--debug")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	lines := debugReportLines(t, readReport(t, reports))
	for _, want := range []string{"Stage[main]: container (type Stage)", "Class[main]: container (type Class)",
		"Foo::Bar[x]: container (kind defined_type)"} {
		if !slices.Contains(lines, want) {
			t.Errorf("report lacks %q; has %q", want, lines)
		}
		if !strings.Contains(stdout, "Debug: voxagent: "+want+"\n") {
			t.Errorf("console lacks %q:\n%s", want, stdout)
		}
	}
	if !anyMatches(lines, `^prefetch notify: 1 resources in `+dur+`$`) {
		t.Errorf("report lacks the notify prefetch line: %q", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "hunter2") || strings.Contains(l, "sensitive-message-text") {
			t.Errorf("a voxagent line carries a parameter value: %q", l)
		}
	}

	quiet := filepath.Join(dir, "quiet-reports")
	_, stderr, code = run(t, bin, "apply", "--catalog", cat, "--report-dir", quiet)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	for _, e := range readReport(t, quiet)["logs"].([]any) {
		m := e.(map[string]any)
		if m["source"] == "voxagent" {
			t.Errorf("a run without --debug reports a voxagent line: %v", m)
		}
	}
}

// The extension host's own lines: start-up, load, each call with its ref, and
// finish. None names a parameter value.
func TestApplyLogsExtensionHostCalls(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "v")
	mainClass := ext("Class", "main", map[string]any{"name": "main"})
	mainClass["kind"] = "class"
	c := writeCatalog(t, dir, []map[string]any{ext("Stage", "main", nil), mainClass,
		ext("Vox_selfref", "s", map[string]any{"value": "supersecretvalue"}),
		ext("Vox_evalgen", "two", map[string]any{"make": "2"}),
		ext("Vox_value", "a", map[string]any{"path": target, "value": "one"})})
	raw, err := os.ReadFile(c)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": "Vox_selfref[s]"}, {"source": "Class[main]", "target": "Vox_evalgen[two]"},
		{"source": "Class[main]", "target": "Vox_value[a]"}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports, "--debug"}, hostArgs(t)...)...)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	rep := readReport(t, reports)
	lines := debugReportLines(t, rep)
	for _, re := range []string{
		`^extension host: started in ` + dur + `$`,
		`^extension host: load in ` + dur + `$`,
		`^extension host: evaluate Vox_value\[a\] in ` + dur + `$`,
		`^extension host: eval_generate Vox_evalgen\[two\] in ` + dur + `$`,
		`^extension host: refresh Vox_selfref\[s\] in ` + dur + `$`,
		`^extension host: finish in ` + dur + `$`,
	} {
		if !anyMatches(lines, re) {
			t.Errorf("report lacks a line matching %s:\n%s", re, strings.Join(lines, "\n"))
		}
	}
	for _, l := range lines {
		if strings.Contains(l, "supersecretvalue") {
			t.Errorf("a voxagent line carries a value: %q", l)
		}
	}
}

// The run's level reaches the host: a provider's debug line is in the report at
// level debug under --debug, and absent without it.
func TestApplyPassesTheLevelToTheHost(t *testing.T) {
	bin := buildBinary(t)
	const line = "vox_value provider wrote the value"
	for _, tc := range []struct {
		flags []string
		seen  bool
	}{{nil, false}, {[]string{"--verbose"}, false}, {[]string{"--debug"}, true}} {
		dir := t.TempDir()
		c := writeCatalog(t, dir, []map[string]any{
			ext("Vox_value", "a", map[string]any{"path": filepath.Join(dir, "v"), "value": "one"})})
		reports := filepath.Join(dir, "reports")
		args := append([]string{"apply", "--catalog", c, "--report-dir", reports}, tc.flags...)
		_, stderr, code := run(t, bin, append(args, hostArgs(t)...)...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s", tc.flags, code, stderr)
		}
		seen := false
		for _, e := range readReport(t, reports)["logs"].([]any) {
			m := e.(map[string]any)
			if m["message"] == line {
				seen = true
				if m["level"] != "debug" {
					t.Errorf("%v: level = %v, want debug", tc.flags, m["level"])
				}
			}
		}
		if seen != tc.seen {
			t.Errorf("%v: the provider's debug line in the report = %v, want %v", tc.flags, seen, tc.seen)
		}
	}
}

// libuserPlatform is CentOS Stream 10 with libuser's tools and sysctl present.
type libuserPlatform struct{}

func (libuserPlatform) Executable(path string) bool {
	return path == "/usr/sbin/luseradd" || path == "/usr/sbin/lgroupadd" || path == "/usr/sbin/sysctl"
}
func (libuserPlatform) OSRelease() ([]byte, error) { return augeasPlatform{}.OSRelease() }

// Every delegated type the catalog holds is reported at debug, sorted by type
// name and each with its reason; at notice none is (spec §6).
func TestDelegationReasonsSortedByType(t *testing.T) {
	ruby := extensiontest.Ruby(t)
	dir := t.TempDir()
	raw, err := os.ReadFile(writeCatalog(t, dir, []map[string]any{
		ext("User", "u", map[string]any{"ensure": "absent"}),
		ext("Sysctl", "vm.swappiness", map[string]any{"value": "10"}),
		ext("Concat_file", filepath.Join(dir, "c"), nil),
	}))
	if err != nil {
		t.Fatal(err)
	}
	old := ral.HostPlatform
	ral.HostPlatform = libuserPlatform{}
	t.Cleanup(func() { ral.HostPlatform = old })
	// a changed byte leaves concat_file.rb unmeasured
	tampered := func(b []byte) []byte { return append(b, '\n') }
	libdir := concatLibdir(t, tampered, augeasLibdir(t, extensiontest.Libdir(t)))
	const via = " applied via extension host: "
	run := func(level string) []string {
		o := applyOptions{noop: true, ruby: ruby, libdir: libdir, vardir: t.TempDir(),
			level: level, logger: runlog.New(level, nil, nil), certname: "n.example",
			timeouts:    extension.Timeouts{Evaluate: time.Minute, Other: time.Minute},
			augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(map[string]string{"AUGEAS_ROOT": "/tmp"})}
		_, rep := applyCatalog(raw, o)
		if rep == nil {
			t.Fatal("the run was refused")
		}
		var lines []string
		for _, e := range rep.Logs {
			if e.Source == "voxagent" && strings.Contains(e.Message, via) {
				if e.Level != "debug" {
					t.Errorf("%q logged at %s", e.Message, e.Level)
				}
				lines = append(lines, e.Message)
			}
		}
		return lines
	}
	got := run("debug")
	if len(got) != 3 {
		t.Fatalf("debug lines = %q, want three", got)
	}
	// concat's reason carries the digests, so it is matched by shape
	for i, re := range []string{
		`^concat_file applied via extension host: puppet/type/concat_file\.rb has sha256 [0-9a-f]{64}, not the measured [0-9a-f]{64}$`,
		`^sysctl applied via extension host: AUGEAS_ROOT is set$`,
		`^user applied via extension host: libuser present$`,
	} {
		if !regexp.MustCompile(re).MatchString(got[i]) {
			t.Errorf("line %d = %q, want it to match %s", i, got[i], re)
		}
	}
	if got := run("notice"); len(got) != 0 {
		t.Errorf("notice level reported %q", got)
	}
}
