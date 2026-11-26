package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/units/file"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/units/notify"
)

// fakeGen is a native type (Vox_lg) whose generate builds File[<dir>/<title>],
// so the load-time hook is tested before any real provider implements it.
type fakeGen struct {
	notify.Notify
	typ     string // the type it claims; "" is vox_lg, which only the native path knows
	dir     string
	fileOpt map[string]any // extra File parameters
	invalid error          // Validate's answer for a Vox_lg
	calls   *[]string
}

func (f fakeGen) Type() string {
	if f.typ == "" {
		return "vox_lg"
	}
	return f.typ
}

func (f fakeGen) Validate(catalog.Resource) error { return f.invalid }

func (f fakeGen) LoadGenerate(_ *catalog.Catalog, r catalog.Resource) (ral.LoadGeneration, error) {
	*f.calls = append(*f.calls, r.Ref())
	params := map[string]any{"ensure": "present"}
	for k, v := range f.fileOpt {
		params[k] = v
	}
	file := catalog.Resource{Type: "File", Title: filepath.Join(f.dir, r.Title), Tags: []string{"file"}, Parameters: params}
	return ral.LoadGeneration{Generation: catalog.Generation{
		Resources:  []catalog.Resource{file},
		Generators: map[string]string{file.Ref(): r.Ref()},
	}}, nil
}

// applyInProcess runs applyCatalog with the fake registered and returns what
// it wrote to stderr.
func applyInProcess(t *testing.T, resources []map[string]any, o applyOptions) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	raw, err := os.ReadFile(writeCatalog(t, dir, resources))
	if err != nil {
		t.Fatal(err)
	}
	o.noop = true
	o.reportDir = filepath.Join(dir, "reports")
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = wr
	st, _ := applyCatalog(raw, o)
	os.Stderr = saved
	wr.Close()
	out, _ := io.ReadAll(rd)
	return string(out), st.CouldNotRun
}

func TestLoadGenerateRunsWithoutHost(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	reg := ral.NewRegistry(file.File{}, fakeGen{dir: dir, calls: &calls})
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Vox_lg", Title: "a"}, {Type: "Notify", Title: "n"}, {Type: "Vox_lg", Title: "b"},
	}}
	if _, _, refused := loadGenerate(cat, reg, nil); refused {
		t.Fatal("loadGenerate refused a valid generation")
	}
	var refs []string
	for _, r := range cat.Resources {
		refs = append(refs, r.Ref())
	}
	want := []string{"Vox_lg[a]", "File[" + dir + "/a]", "Notify[n]", "Vox_lg[b]", "File[" + dir + "/b]"}
	if strings.Join(refs, "\n") != strings.Join(want, "\n") {
		t.Errorf("resources = %q, want %q", refs, want)
	}
	if got := cat.Resources[1].GeneratedFrom; got == nil || got.Ref() != "Vox_lg[a]" {
		t.Errorf("GeneratedFrom = %v, want Vox_lg[a]", got)
	}
	if strings.Join(calls, ",") != "Vox_lg[a],Vox_lg[b]" {
		t.Errorf("generator calls = %q, want one per resource in catalog order", calls)
	}
}

func TestLoadGenerateSkippedWhenHostLoaded(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	// Schedule is a Puppet builtin the host loads, so claiming it lets the host
	// load a catalog the fake would otherwise generate for.
	o := applyOptions{providers: []ral.Provider{fakeGen{typ: "schedule", dir: dir, calls: &calls}},
		ruby: extensiontest.Ruby(t), libdir: extensiontest.Libdir(t), vardir: t.TempDir(), level: "notice",
		timeouts: extension.Timeouts{Evaluate: time.Minute, Other: time.Minute}}
	stderr, couldNot := applyInProcess(t, []map[string]any{
		ext("Vox_value", "v", map[string]any{"path": filepath.Join(dir, "v"), "value": "x"}),
		ext("Schedule", "a", map[string]any{}),
	}, o)
	if couldNot || len(calls) != 0 {
		t.Errorf("could-not-run %v, generator calls %q, want a run without native generation\n%s", couldNot, calls, stderr)
	}
}

func TestLoadGenerateRunsAfterValidation(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	o := applyOptions{noRuby: true, providers: []ral.Provider{
		fakeGen{dir: dir, calls: &calls, invalid: os.ErrInvalid}}}
	stderr, couldNot := applyInProcess(t, []map[string]any{ext("Vox_lg", "a", map[string]any{})}, o)
	if !couldNot || len(calls) != 0 {
		t.Errorf("could-not-run %v, generator calls %q, want a refused run and no call", couldNot, calls)
	}
	if want := "voxagent: invalid argument\nvoxagent: 1 invalid resource(s); nothing applied\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr)
	}
}

func TestLoadGeneratedResourceIsJudged(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	o := applyOptions{noRuby: true, providers: []ral.Provider{
		fakeGen{dir: dir, calls: &calls, fileOpt: map[string]any{"ensure": float64(5)}}}}
	stderr, couldNot := applyInProcess(t, []map[string]any{ext("Vox_lg", "a", map[string]any{})}, o)
	if !couldNot || len(calls) != 1 {
		t.Fatalf("could-not-run %v, generator calls %q\n%s", couldNot, calls, stderr)
	}
	if !strings.Contains(stderr, "File["+dir+"/a]") || !strings.Contains(stderr, "nothing applied") {
		t.Errorf("stderr lacks the generated File's refusal:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err == nil {
		t.Error("the generated File was applied")
	}
}

// richGen (Fake) is a native type whose generate logs a notice at its own
// ref, fails non-fatally and places its Notify before itself: what a
// LoadGeneration can say beyond catalog.Generation.
type richGen struct{ notify.Notify }

func (richGen) Type() string { return "fake" }

func (richGen) LoadGenerate(_ *catalog.Catalog, r catalog.Resource) (ral.LoadGeneration, error) {
	gen := catalog.Resource{Type: "Notify", Title: "gen", Tags: []string{"notify"},
		Parameters: map[string]any{"message": "generated"}}
	return ral.LoadGeneration{
		Generation: catalog.Generation{
			Resources:  []catalog.Resource{gen},
			Containers: map[string]string{gen.Ref(): "Class[main]"},
			Generators: map[string]string{gen.Ref(): r.Ref()},
		},
		Logs:       []report.LogEntry{{Level: "notice", Source: r.Ref(), Message: "generating"}},
		Failed:     true,
		DepthFirst: true,
	}, nil
}

// A generator's logs reach the report at the walk's source for its resource,
// ahead of "Applying configuration version"; its failure fails the run; and a
// depth-first generation runs before the generator.
func TestLoadGenerationLogsFailsAndPlacesDepthFirst(t *testing.T) {
	dir := t.TempDir()
	stage := map[string]any{"type": "Stage", "title": "main", "tags": []any{}, "parameters": map[string]any{}}
	mainClass := map[string]any{"type": "Class", "title": "main", "tags": []any{}, "kind": "class",
		"parameters": map[string]any{"name": "main"}}
	path := writeCatalog(t, dir, []map[string]any{stage, mainClass, ext("Fake", "t", map[string]any{"message": "fake"})})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": "Fake[t]"}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	_, rep := applyCatalog(raw, applyOptions{noRuby: true, noop: true, level: "info",
		reportDir: filepath.Join(dir, "reports"), providers: []ral.Provider{richGen{}}})
	if rep == nil {
		t.Fatal("no report")
	}
	index := func(substr string) int {
		for i, l := range rep.Logs {
			if strings.Contains(l.Source+": "+l.Message, substr) {
				return i
			}
		}
		t.Fatalf("no log holding %q in %+v", substr, rep.Logs)
		return -1
	}
	generating := index("/Stage[main]/Main/Fake[t]: generating")
	if rep.Logs[generating].Level != "notice" {
		t.Errorf("level = %q, want notice", rep.Logs[generating].Level)
	}
	if applying := index("Applying configuration version"); generating > applying {
		t.Errorf("generate log at %d follows the version line at %d", generating, applying)
	}
	if generated, own := index("Notify[gen]/message"), index("Fake[t]/message"); generated > own {
		t.Errorf("generated resource logged at %d, after its generator at %d", generated, own)
	}
	if rep.Status != "failed" {
		t.Errorf("status = %q, want failed", rep.Status)
	}
}
