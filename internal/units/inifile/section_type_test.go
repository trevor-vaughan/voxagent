package inifile

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// sectionCorpus is the Ruby-generated ini_section corpus: 6.6.1 with PR #581's
// lib hunk applied (D-105; testdata/gen/gen.sh 6.6.1 section).
const sectionCorpus = "section-6.6.1-pr581.json"

type sectionEvent struct {
	Name          string `json:"name"`
	Property      string `json:"property"`
	Status        string `json:"status"`
	Message       string `json:"message"`
	PreviousValue string `json:"previous_value"`
	DesiredValue  string `json:"desired_value"`
}

type sectionRun struct {
	Noop     bool           `json:"noop"`
	Failed   bool           `json:"failed"`
	Events   []sectionEvent `json:"events"`
	FinalB64 *string        `json:"final_b64"`
}

type sectionCase struct {
	Name          string         `json:"name"`
	Encoding      string         `json:"encoding"`
	InputB64      string         `json:"input_b64"`
	Missing       bool           `json:"missing"`
	Directory     bool           `json:"directory"`
	MissingParent bool           `json:"missing_parent"`
	Params        map[string]any `json:"params"`
	Runs          []sectionRun   `json:"runs"`
}

type sectionFile struct {
	Openvox string        `json:"openvox"`
	Ruby    string        `json:"ruby"`
	Cases   []sectionCase `json:"cases"`
}

func readSectionCorpus(t *testing.T) sectionFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", sectionCorpus))
	if err != nil {
		t.Fatal(err)
	}
	var f sectionFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s: %v", sectionCorpus, err)
	}
	return f
}

func sectionRes(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "ini_section", Title: "s", Parameters: params}
}

// eventOf is Ruby's report event for a change: the harness's failure message wraps the
// provider's error, and noop is a status of its own.
func eventOf(ch ral.Change, path string) sectionEvent {
	status, msg := "success", ch.Message
	switch {
	case ch.Err != nil:
		status = "failure"
		msg = "change from '" + ch.Was.(string) + "' to '" + ch.Is.(string) + "' failed: " + strings.ReplaceAll(ch.Err.Error(), path, "<path>")
	case strings.HasSuffix(msg, " (noop)"):
		status = "noop"
	}
	return sectionEvent{Name: ch.Name, Property: ch.Property, Status: status, Message: msg,
		PreviousValue: ch.Was.(string), DesiredValue: ch.Is.(string)}
}

func runSectionCase(t *testing.T, c sectionCase) {
	t.Helper()
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
	}
	if c.Encoding == "UTF-8" {
		t.Setenv("LANG", "C.UTF-8")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.ini")
	switch {
	case c.InputB64 != "":
		in, err := base64.StdEncoding.DecodeString(c.InputB64)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, in, 0o644); err != nil {
			t.Fatal(err)
		}
	case c.Directory:
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	case c.MissingParent:
		path = filepath.Join(dir, "nodir", "f.ini")
	case !c.Missing:
		if err := os.WriteFile(path, nil, 0o644); err != nil { // input_b64 is "" for an empty file
			t.Fatal(err)
		}
	}
	params := map[string]any{"path": path}
	for k, v := range c.Params {
		params[k] = v
	}
	r := sectionRes(params)
	for i, run := range c.Runs {
		changes, warnings, err := IniSection{}.Apply(context.Background(), r, run.Noop)
		if len(warnings) != 0 {
			t.Fatalf("run %d: warnings %v", i, warnings)
		}
		var got []sectionEvent
		if err != nil { // a read failure fails the evaluation: Ruby's resource_error
			got = []sectionEvent{{Name: "resource_error", Status: "failure", Message: err.Error()}}
			if len(run.Events) == 1 {
				got[0].Property = run.Events[0].Property
				got[0].PreviousValue, got[0].DesiredValue = run.Events[0].PreviousValue, run.Events[0].DesiredValue
			}
		}
		for _, ch := range changes {
			got = append(got, eventOf(ch, path))
		}
		if !equalEvents(got, run.Events) {
			t.Errorf("run %d (noop %t): events\n got %+v\nRuby %+v", i, run.Noop, got, run.Events)
		}
		checkSectionFinal(t, i, run, path)
	}
}

func equalEvents(a, b []sectionEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkSectionFinal(t *testing.T, run int, want sectionRun, path string) {
	t.Helper()
	info, statErr := os.Stat(path)
	if want.FinalB64 == nil {
		if statErr == nil && info.Mode().IsRegular() {
			t.Errorf("run %d: a file exists, Ruby's does not", run)
		}
		return
	}
	bytesWant, err := base64.StdEncoding.DecodeString(*want.FinalB64)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(have, bytesWant) {
		t.Errorf("run %d: file %q (%v), Ruby's %q", run, have, err, bytesWant)
	}
}

func TestSectionMatchesRuby(t *testing.T) {
	f := readSectionCorpus(t)
	if f.Openvox != oracleOpenvox || f.Ruby != oracleRuby {
		t.Errorf("%s: generated on openvox %s ruby %s, want %s %s", sectionCorpus, f.Openvox, f.Ruby, oracleOpenvox, oracleRuby)
	}
	if len(f.Cases) == 0 {
		t.Fatal("section corpus is empty")
	}
	for _, c := range f.Cases {
		t.Run(c.Name+"/"+c.Encoding, func(t *testing.T) { runSectionCase(t, c) })
	}
}

// TestRubyMatchesSection re-runs the generator against a copy of the 6.6.1 lib
// with PR #581's hunk applied, and fails on any difference from the committed
// corpus.
func TestRubyMatchesSection(t *testing.T) {
	ruby := oracleRubyPath(t)
	patch, err := exec.LookPath("patch")
	if err != nil {
		t.Skip("patch is not on PATH")
	}
	lib := filepath.Join(t.TempDir(), "lib")
	if out, err := exec.Command("cp", "-r", filepath.Join("testdata", "6.6.1", "lib"), lib).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, out)
	}
	diff, err := filepath.Abs(filepath.Join("..", "..", "..", "tests", "spine", "inifile", "581-lib.diff"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(patch, "-p2", "-d", lib, "-i", diff).CombinedOutput(); err != nil {
		t.Fatalf("patch: %v\n%s", err, out)
	}
	cmd := exec.Command(ruby, filepath.Join("testdata", "gen", "section.rb"))
	cmd.Env = append(os.Environ(), "RUBYLIB="+lib)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ruby: %v\n%s", err, stderr.String())
	}
	committed, err := os.ReadFile(filepath.Join("testdata", sectionCorpus))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(committed)) {
		t.Errorf("%s differs from the generator's output; rerun testdata/gen/gen.sh 6.6.1 section", sectionCorpus)
	}
}

// TestSectionNoRefreshonlyParam: Ruby's ini_section has no refreshonly (the
// override that read it is what PR #581 deletes), so a resource that sets one
// is a pre-flight gap, not a silently ignored attribute.
func TestSectionNoRefreshonlyParam(t *testing.T) {
	r := sectionRes(map[string]any{"path": "/a/b/m.ini", "refreshonly": true})
	gaps := ral.ResourceGaps(IniSection{}, r)
	if len(gaps) != 1 || gaps[0].Attribute != "refreshonly" || gaps[0].Value != "" {
		t.Fatalf("gaps %+v, want one for the refreshonly attribute", gaps)
	}
	if gaps := ral.ResourceGaps(IniSection{}, sectionRes(map[string]any{"path": "/a/b/m.ini", "section_prefix": "<"})); len(gaps) != 0 {
		t.Fatalf("gaps %+v for implemented attributes", gaps)
	}
}

func TestSectionProviderShape(t *testing.T) {
	p := IniSection{}
	if _, ok := p.Properties()["ensure"]; !ok || len(p.Properties()) != 1 {
		t.Errorf("Properties = %v, want ensure alone", p.Properties().Sorted())
	}
	if p.Refreshable(sectionRes(nil)) { // tsc: defines no refresh
		t.Error("ini_section is refreshable")
	}
}

func TestSectionAutoRequiresParent(t *testing.T) { // tsc:55, as ini_setting's ts:151
	r := sectionRes(map[string]any{"path": "/a/b/m.ini"})
	if got := (IniSection{}).AutoRequires(r, func(ref string) bool { return ref == "File[/a/b]" }); len(got) != 1 || got[0] != "File[/a/b]" {
		t.Errorf("managed parent: %v", got)
	}
	if got := (IniSection{}).AutoRequires(r, func(string) bool { return false }); len(got) != 0 {
		t.Errorf("unmanaged parent: %v", got)
	}
}

func TestSectionInvalidParamsFail(t *testing.T) {
	for name, c := range map[string]struct {
		params map[string]any
		want   string
	}{
		"relative path": {map[string]any{"path": "relative", "section": "a"}, "Parameter path failed on ini_section[s]: File paths must be fully qualified, not 'relative'"},
		"empty section": {map[string]any{"path": "/a/m.ini", "section": "  "}, "Parameter section failed on ini_section[s]: Section names must not be empty"},
	} {
		if _, _, err := (IniSection{}).Apply(context.Background(), sectionRes(c.params), false); err == nil || err.Error() != c.want {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestSectionNoPathIsTypeError(t *testing.T) { // File.file?(nil), as ini_setting
	_, _, err := IniSection{}.Apply(context.Background(), sectionRes(map[string]any{"section": "a"}), false)
	if err == nil || err.Error() != "no implicit conversion of nil into String" {
		t.Fatalf("err = %v", err)
	}
}

func TestSectionNoopDoesNotWrite(t *testing.T) { // review focus 3
	path := writeIni(t, "[a]\nx = 1\n")
	for _, ensure := range []string{"present", "absent"} {
		section := map[string]string{"present": "b", "absent": "a"}[ensure]
		r := sectionRes(map[string]any{"path": path, "section": section, "ensure": ensure})
		if got := applySetting(t, IniSection{}, r, true); len(got) != 1 || !strings.HasSuffix(got[0].Message, "(noop)") {
			t.Fatalf("%s: changes %#v", ensure, got)
		}
		if got := readFile(t, path); got != "[a]\nx = 1\n" {
			t.Fatalf("%s: noop wrote %q", ensure, got)
		}
	}
}
