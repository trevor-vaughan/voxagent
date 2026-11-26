package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/units/notify"
)

const sinkSecret = "SINKSECRET-1"

// leakyProvider (Vox_leak) puts its secret parameter, which is not a
// property and so is never redacted per attribute, into every sink its title
// chooses: "invalid" into a validation refusal, "log" into a run log line and
// an event message.
type leakyProvider struct{ notify.Notify }

func (leakyProvider) Type() string     { return "vox_leak" }
func (leakyProvider) Attrs() ral.Attrs { return ral.NewAttrs("name", "secret") }

// Prefetch keeps leakyProvider: Notify's would prime a plain Notify.
func (p leakyProvider) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

func (leakyProvider) Validate(r catalog.Resource) error {
	if r.Title == "invalid" {
		return fmt.Errorf("%s: secret %v is refused", r.Ref(), rubyvalue.Param(r, "secret"))
	}
	return nil
}

func (leakyProvider) Apply(ctx context.Context, r catalog.Resource, _ bool) ([]ral.Change, []ral.Warning, error) {
	secret := fmt.Sprint(rubyvalue.Param(r, "secret"))
	runlog.From(ctx).Log("notice", r.Ref(), "saw "+secret)
	return []ral.Change{{Property: "message", Was: "absent", Is: "present", Message: "set " + secret}}, nil, nil
}

// leakGen (Vox_leakgen) generates a refused Vox_leak whose marked secret only
// the generated resource carries.
type leakGen struct{ notify.Notify }

func (leakGen) Type() string     { return "vox_leakgen" }
func (leakGen) Attrs() ral.Attrs { return ral.NewAttrs() }

func (leakGen) LoadGenerate(_ *catalog.Catalog, r catalog.Resource) (ral.LoadGeneration, error) {
	leak := catalog.Resource{Type: "Vox_leak", Title: "invalid", Tags: []string{"vox_leak"},
		Parameters: map[string]any{"secret": "SINKSECRET-2"}, SensitiveParameters: []string{"secret"}}
	return ral.LoadGeneration{Generation: catalog.Generation{Resources: []catalog.Resource{leak}, Generators: map[string]string{leak.Ref(): r.Ref()}}}, nil
}

type sinkOutput struct {
	stdout, consoleErr, stderr string // the console's two writers and the process's stderr
	rep                        *report.Report
}

// sinkRun applies resources in process, with Vox_leak and Vox_leakgen native
// and a debug console, and returns what each sink received.
func sinkRun(t *testing.T, resources []map[string]any, o applyOptions) sinkOutput {
	t.Helper()
	raw, err := os.ReadFile(writeCatalog(t, t.TempDir(), resources))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, consoleErr bytes.Buffer
	o.noRuby = true
	o.providers = append(o.providers, leakyProvider{}, leakGen{})
	o.console = &puppetConsole{level: "debug", stdout: &stdout, stderr: &consoleErr}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Drained as it is written, so a run's stderr cannot fill the pipe and
	// block it; restored however applyCatalog ends.
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(rd)
		rd.Close()
		read <- b
	}()
	var rep *report.Report
	func() {
		saved := os.Stderr
		os.Stderr = wr
		defer func() {
			os.Stderr = saved
			wr.Close()
		}()
		_, rep = applyCatalog(raw, o)
	}()
	stderr := <-read
	return sinkOutput{stdout: stdout.String(), consoleErr: consoleErr.String(), stderr: string(stderr), rep: rep}
}

func leakResource(title, secret string, marked bool) map[string]any {
	var names []string
	if marked {
		names = []string{"secret"}
	}
	return markedResource("Vox_leak", title, map[string]any{"secret": secret}, names...)
}

func TestStderrRefusalIsScrubbed(t *testing.T) {
	out := sinkRun(t, []map[string]any{leakResource("invalid", sinkSecret, true)}, applyOptions{})
	want := "voxagent: Vox_leak[invalid]: secret [redacted] is refused\n"
	if !strings.Contains(out.stderr, want) || strings.Contains(out.stderr, sinkSecret) {
		t.Errorf("stderr lacks %q or holds %s:\n%s", want, sinkSecret, out.stderr)
	}
}

func TestReportAndConsoleAreScrubbed(t *testing.T) {
	out := sinkRun(t, []map[string]any{leakResource("log", sinkSecret, true)}, applyOptions{})
	if out.rep == nil {
		t.Fatalf("no report; stderr:\n%s", out.stderr)
	}
	body, err := json.Marshal(out.rep)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"report": string(body), "console stdout": out.stdout,
		"console stderr": out.consoleErr, "stderr": out.stderr} {
		if strings.Contains(text, sinkSecret) {
			t.Errorf("%s holds %s:\n%s", name, sinkSecret, text)
		}
	}
	// Each sink scrubs once: the console prints the report's stored message.
	var saw, set bool
	for _, e := range out.rep.Logs {
		saw = saw || e.Message == "saw [redacted]"
		set = set || e.Message == "set [redacted]"
		if line := "Notice: " + e.Source + ": " + e.Message + "\n"; strings.Contains(e.Message, "[redacted]") &&
			!strings.Contains(out.stdout, line) {
			t.Errorf("console lacks the report's line %q:\n%s", line, out.stdout)
		}
	}
	if !saw || !set {
		t.Errorf("report lacks the scrubbed log or event line: %v", out.rep.Logs)
	}
}

// "dact" matches inside "[redacted]" (a form over 3 bytes matches anywhere), so
// a sink that scrubbed twice would hold "[re[redacted]ed]".
func TestEachSinkScrubsOnce(t *testing.T) {
	out := sinkRun(t, []map[string]any{leakResource("log", "dact", true)}, applyOptions{})
	if out.rep == nil {
		t.Fatalf("no report; stderr:\n%s", out.stderr)
	}
	var messages []string
	for _, e := range out.rep.Logs {
		messages = append(messages, e.Message)
	}
	for _, want := range []string{"saw [redacted]", "set [redacted]"} {
		if !slices.Contains(messages, want) {
			t.Errorf("report messages lack %q: %q", want, messages)
		}
	}
	for _, want := range []string{"Notice: Vox_leak[log]: saw [redacted]\n", "Notice: /Vox_leak[log]/message: set [redacted]\n"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("console lacks %q:\n%s", want, out.stdout)
		}
	}
	body, err := json.Marshal(out.rep)
	if err != nil {
		t.Fatal(err)
	}
	if text := string(body) + out.stdout; strings.Contains(text, "[re[redacted]ed]") {
		t.Errorf("a sink scrubbed twice:\n%s", text)
	}
}

func TestApplyScrubsPreReportRunLogs(t *testing.T) {
	out := sinkRun(t, []map[string]any{leakResource("log", sinkSecret, true)}, applyOptions{
		runLogs: []report.LogEntry{{Level: "notice", Source: "Puppet", Message: "early " + sinkSecret}}})
	if out.rep == nil {
		t.Fatalf("no report; stderr:\n%s", out.stderr)
	}
	if got := out.rep.Logs[0].Message; got != "early [redacted]" {
		t.Errorf("first report message = %q, want %q", got, "early [redacted]")
	}
}

// Over-budget debug lines and lines logged before the report exists reach
// print directly, not through the report.
func TestConsolePrintScrubs(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Vox_leak", Title: "log",
		Parameters: map[string]any{"secret": sinkSecret}, SensitiveParameters: []string{"secret"}}}}
	var stdout bytes.Buffer
	c := &puppetConsole{level: "debug", stdout: &stdout, stderr: &bytes.Buffer{}, scrub: redact.New(cat)}
	c.print(report.LogEntry{Level: "debug", Source: "voxagent", Message: "early " + sinkSecret})
	if got, want := stdout.String(), "Debug: voxagent: early [redacted]\n"; got != want {
		t.Errorf("console = %q, want %q", got, want)
	}
}

func TestLoadGenerateRefusalIsScrubbed(t *testing.T) {
	out := sinkRun(t, []map[string]any{ext("Vox_leakgen", "g", map[string]any{})}, applyOptions{})
	want := "voxagent: Vox_leak[invalid]: secret [redacted] is refused\n"
	if !strings.Contains(out.stderr, want) || strings.Contains(out.stderr, "SINKSECRET-2") {
		t.Errorf("stderr lacks %q or holds SINKSECRET-2:\n%s", want, out.stderr)
	}
}

// With nothing marked, every sink prints exactly what it did before scrubbing.
func TestUnmarkedRunUnchanged(t *testing.T) {
	out := sinkRun(t, []map[string]any{leakResource("log", sinkSecret, false)}, applyOptions{})
	if out.rep == nil {
		t.Fatalf("no report; stderr:\n%s", out.stderr)
	}
	var messages []string
	for _, e := range out.rep.Logs {
		messages = append(messages, e.Source+": "+e.Message)
	}
	for _, s := range out.rep.ResourceStatuses {
		for _, e := range s.Events {
			messages = append(messages, "event: "+e.Message)
		}
	}
	wantStdout := "Notice: Vox_leak[log]: saw " + sinkSecret + "\n" +
		"Notice: /Vox_leak[log]/message: set " + sinkSecret + "\n"
	if !strings.Contains(out.stdout, wantStdout) {
		t.Errorf("console stdout lacks %q:\n%s", wantStdout, out.stdout)
	}
	if out.consoleErr != "" || out.stderr != "" {
		t.Errorf("console stderr %q, stderr %q, want none", out.consoleErr, out.stderr)
	}
	for _, want := range []string{"Vox_leak[log]: saw " + sinkSecret,
		"/Vox_leak[log]/message: set " + sinkSecret, "event: set " + sinkSecret} {
		if !strings.Contains(strings.Join(messages, "\n"), want) {
			t.Errorf("report lacks %q:\n%s", want, strings.Join(messages, "\n"))
		}
	}
}
