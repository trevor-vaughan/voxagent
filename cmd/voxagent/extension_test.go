package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/extension/extensiontest"
)

func ext(typ, title string, params map[string]any) map[string]any {
	return map[string]any{"type": typ, "title": title, "kind": "compilable_type", "tags": []string{},
		"exported": false, "parameters": params}
}

// Review Focus 5: a native-only catalog never starts a host, so a broken
// --ruby changes nothing.
func TestNativeOnlyCatalogNeverStartsHost(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Notify", "hi", map[string]any{"message": "m"})})
	_, stderr, code := run(t, bin, "apply", "--catalog", c, "--ruby", "/nonexistent/ruby")
	if code != 0 || strings.Contains(stderr, "extension host") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestNoRubyRefusesTier3Catalog(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Vox_value", "a", map[string]any{"value": "x"})})
	_, stderr, code := run(t, bin, "apply", "--catalog", c, "--no-ruby")
	if code != 2 || !strings.Contains(stderr, "--no-ruby: type(s) vox_value need the extension host; nothing applied") {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

func TestUnavailableHostIsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Vox_value", "a", map[string]any{"value": "x"})})
	for _, args := range [][]string{{"--ruby", "/nonexistent/ruby"}, {"--ruby", "ruby"}} {
		_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c}, args...)...)
		if code != 2 || !strings.Contains(stderr, "the extension host is unavailable") {
			t.Errorf("%v: exit %d, stderr:\n%s", args, code, stderr)
		}
	}
}

// hostArgs points a run at the test Ruby and libdir, and at a fresh vardir,
// which the host shares, so no host test writes under the owner's
// /opt/puppetlabs/puppet/cache.
func hostArgs(t *testing.T) []string {
	return []string{"--ruby", extensiontest.Ruby(t), "--libdir", extensiontest.Libdir(t), "--vardir", t.TempDir()}
}

// A self-refreshing Tier 3 type refreshes itself after its own event, and its
// refresh writes under the vardir voxagent was given.
func TestSelfRefreshThroughTheHost(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Vox_selfref", "s", map[string]any{"value": "new"})})
	reports := filepath.Join(dir, "reports")
	args := hostArgs(t)
	vardir := args[slices.Index(args, "--vardir")+1]
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports}, args...)...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	st := readReport(t, reports)["resource_statuses"].(map[string]any)["Vox_selfref[s]"].(map[string]any)
	var events []string
	for _, e := range st["events"].([]any) {
		ev := e.(map[string]any)
		events = append(events, ev["name"].(string)+": "+ev["message"].(string))
	}
	if want := []string{"value_changed: value changed 'old' to 'new'", "refresh: Triggered 'refresh' from 1 event"}; !slices.Equal(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if b, err := os.ReadFile(filepath.Join(vardir, "vox_selfref.log")); err != nil || string(b) != "s\n" {
		t.Errorf("vox_selfref.log = %q, %v", b, err)
	}
}

func TestTier3ResourceAppliesThroughHost(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "v")
	c := writeCatalog(t, dir, []map[string]any{
		ext("Vox_value", "a", map[string]any{"path": target, "value": "one"}),
		ext("Notify", "after", map[string]any{"message": "m", "require": "Vox_value[a]"}),
	})
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports}, hostArgs(t)...)...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if b, _ := os.ReadFile(target); string(b) != "one\n" {
		t.Errorf("target = %q", b)
	}
	for _, want := range []string{"1 vox_value resource(s) applied via extension host (libdir)",
		"extension host: started in", "evaluated 1 resource(s)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	st := readReport(t, reports)["resource_statuses"].(map[string]any)["Vox_value[a]"].(map[string]any)
	if st["provider_used"] != "file" || st["changed"] != true {
		t.Errorf("status = %v", st)
	}
}

// Puppet runs generate inside the transaction: a failing generate lands in the
// report, not on stderr, and marks the run failed without changing the exit.
func TestGenerateFailureIsReportedNotRefused(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{ext("Vox_gen", "g", map[string]any{"make": "raise"}),
		ext("Notify", "n", map[string]any{"message": "m"})})
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports,
		"--detailed-exitcodes"}, hostArgs(t)...)...)
	if code != 2 { // changed (the notify), without the failure bit
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	rep := readReport(t, reports)
	if rep["status"] != "failed" {
		t.Errorf("status = %v, want failed (resources_failed_to_generate)", rep["status"])
	}
	want := "Failed to generate additional resources using 'generate': generate broke"
	// generate runs before any resource is evaluated, so its line precedes the
	// first resource's log line (transaction.rb:99-100).
	genAt, notifyAt := -1, -1
	for i, l := range rep["logs"].([]any) {
		m := l.(map[string]any)["message"].(string)
		if strings.Contains(m, want) && genAt < 0 {
			genAt = i
		}
		if strings.Contains(m, "defined 'message' as 'm'") && notifyAt < 0 {
			notifyAt = i
		}
	}
	if genAt < 0 || notifyAt < 0 || genAt > notifyAt {
		t.Errorf("generate log at %d, notify log at %d, want both found and generate first: %v",
			genAt, notifyAt, rep["logs"])
	}
}

func TestHostRefusalsAreCouldNotRun(t *testing.T) {
	for _, tt := range []struct {
		name string
		res  map[string]any
		want string
	}{
		{"invalid in Puppet's words", ext("Vox_value", "a", map[string]any{"nosuch": 1}),
			"Vox_value[a]: no parameter named 'nosuch'"},
		{"Resource API", ext("Vox_rapi", "r", map[string]any{}),
			"vox_rapi is a Resource API type; Resource API types are not implemented"},
		{"metaparameter", ext("Vox_value", "a", map[string]any{"value": "x", "noop": true}),
			`Vox_value[a]: attribute "noop" is not implemented`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := buildBinary(t)
			dir := t.TempDir()
			c := writeCatalog(t, dir, []map[string]any{tt.res})
			_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c}, hostArgs(t)...)...)
			if code != 2 || !strings.Contains(stderr, tt.want) || !strings.Contains(stderr, "nothing applied") {
				t.Errorf("exit %d, stderr:\n%s", code, stderr)
			}
		})
	}
}

// Review Focus 2: a host killed mid-run fails every Tier 3 resource it has not
// evaluated, naming the cause; native resources keep applying; the run ends.
func TestHostDeathMidRunFailsRemainingTier3Only(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	native := filepath.Join(dir, "native")
	c := writeCatalog(t, dir, []map[string]any{
		ext("Vox_die", "k", map[string]any{"value": "x"}),
		ext("Vox_value", "later", map[string]any{"path": filepath.Join(dir, "later"), "value": "v",
			"require": "Vox_die[k]"}),
		ext("Vox_value", "independent", map[string]any{"path": filepath.Join(dir, "ind"), "value": "v"}),
		ext("File", native, map[string]any{"ensure": "file", "content": "n\n"}),
	})
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports,
		"--detailed-exitcodes"}, hostArgs(t)...)...)
	if code&4 == 0 {
		t.Fatalf("exit %d, want the failure bit; stderr:\n%s", code, stderr)
	}
	if b, _ := os.ReadFile(native); string(b) != "n\n" {
		t.Errorf("the native file was not applied: %q", b)
	}
	sts := readReport(t, reports)["resource_statuses"].(map[string]any)
	for _, ref := range []string{"Vox_die[k]", "Vox_value[independent]", "Vox_value[later]"} {
		st, _ := sts[ref].(map[string]any)
		if st == nil || st["failed"] != true {
			t.Errorf("%s = %v, want failed", ref, st)
			continue
		}
		ev := st["events"].([]any)[0].(map[string]any)
		if !strings.Contains(ev["message"].(string), "extension host failed") {
			t.Errorf("%s event = %v, want the cause named", ref, ev)
		}
	}
}

func TestGeneratedResourcesApply(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	mainClass := ext("Class", "main", map[string]any{"name": "main"})
	mainClass["kind"] = "class"
	c := writeCatalog(t, dir, []map[string]any{ext("Stage", "main", nil), mainClass,
		ext("Vox_gen", "loud", map[string]any{"make": true})})
	// Vox_gen[loud] sits in Class[main], so the generation's container edge
	// has a container to name.
	raw, err := os.ReadFile(c)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": "Vox_gen[loud]"}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports}, hostArgs(t)...)...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "1 vox_gen resource(s) applied via extension host") {
		t.Errorf("stderr lacks the vox_gen fallback line:\n%s", stderr)
	}
	st, ok := readReport(t, reports)["resource_statuses"].(map[string]any)["Notify[made-by-loud]"].(map[string]any)
	if !ok {
		t.Fatal("report has no Notify[made-by-loud] status")
	}
	if st["changed"] != true {
		t.Errorf("status = %v, want changed", st)
	}
	// add_resource contains a generated resource in its generator's container
	// (additional_resource_generator.rb:131-148), not in the generator: the
	// measured concat File sits under Concat[...], beside its Concat_file. The
	// path through Class[main] is what proves the container edge was merged;
	// without it the Notify would have no containers at all.
	path, _ := st["containment_path"].([]any)
	want := []any{"Stage[main]", "Main", "Notify[made-by-loud]"}
	if !reflect.DeepEqual(path, want) {
		t.Errorf("containment_path = %v, want %v", path, want)
	}
}

// vox_value's title may differ from its name, which makes the name an alias
// a relationship can use (catalog.rb:184-219).
func TestAliasReferenceResolves(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{
		ext("Vox_value", "vt", map[string]any{"name": "vn", "path": filepath.Join(dir, "v"), "value": "x"}),
		ext("Notify", "after", map[string]any{"message": "m", "require": "Vox_value[vn]"}),
	})
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c}, hostArgs(t)...)...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
}

// A declared filebucket is a pre-flight gap even though the host could apply
// it: native File's backup implements only the default puppet bucket, so the
// File below would fail (Puppet backs up into main) and a declared 'puppet'
// bucket would be bypassed. The refusal needs no host, and with one available
// the host must not claim the filebucket as applied.
func TestDeclaredFilebucketIsAPreflightGap(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := writeCatalog(t, dir, []map[string]any{
		ext("Filebucket", "main", map[string]any{"path": filepath.Join(dir, "bucket")}),
		fileResource(target, map[string]any{"content": "new\n", "backup": "main"}),
	})
	for _, args := range [][]string{{"--ruby", "/nonexistent/ruby", "--vardir", t.TempDir()},
		{"--no-ruby", "--vardir", t.TempDir()}, hostArgs(t)} {
		_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c}, args...)...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, code, stderr)
		}
		for _, want := range []string{
			"voxagent: Filebucket[main]: resource type Filebucket is not implemented: native File's backup does not implement named filebuckets",
			"1 gap(s) across 1 resource(s); nothing applied", "outcome=could-not-run"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%v: stderr lacks %q:\n%s", args, want, stderr)
			}
		}
		if strings.Contains(stderr, "extension host") {
			t.Errorf("%v: the filebucket reached the extension host:\n%s", args, stderr)
		}
		if b, _ := os.ReadFile(target); string(b) != "old\n" {
			t.Errorf("%v: target = %q, want it unchanged", args, b)
		}
	}
}

// A relationship naming a title Puppet cannot look up fails Ruby's whole run;
// voxagent refuses it as could-not-run and writes no report (D-55).
func TestUnresolvableTitleIsCouldNotRun(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	c := writeCatalog(t, dir, []map[string]any{
		ext("Vox_titled", "fips", map[string]any{"value": "x"}),
		ext("Notify", "probe", map[string]any{"message": "m", "require": "Vox_titled[fips]"}),
	})
	reports := filepath.Join(dir, "reports")
	_, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--report-dir", reports}, hostArgs(t)...)...)
	want := "voxagent: Failed to apply catalog: Could not retrieve dependency 'Vox_titled[fips]' of Notify[probe]; nothing applied"
	if code != 2 || !strings.Contains(stderr, want) {
		t.Fatalf("exit %d, want 2 with %q; stderr:\n%s", code, want, stderr)
	}
	if entries, err := os.ReadDir(reports); !os.IsNotExist(err) && len(entries) != 0 {
		t.Errorf("a report was written: %v, %v", entries, err)
	}
}
