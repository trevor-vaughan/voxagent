package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
	"github.com/voxagent/voxagent/internal/ral"
)

// concatModuleLib returns the lib directory holding the two type files of
// puppetlabs-concat at version (ral.Concat910 or ral.Concat1001), committed
// under internal/ral/testdata/concat (see its README).
func concatModuleLib(t *testing.T, version ral.ConcatVersion) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	name := map[ral.ConcatVersion]string{ral.Concat910: "9.1.0", ral.Concat1001: "10.0.1"}[version]
	if name == "" {
		t.Fatalf("no concat testdata for version %d", version)
	}
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "ral", "testdata", "concat", name, "lib")
}

// concatLibdir returns a temp libdir holding puppetlabs-concat 10.0.1's lib
// (concatModuleLib), then each extra tree. A non-nil edit changes
// concat_file.rb, so the tree is not measured.
func concatLibdir(t *testing.T, edit func([]byte) []byte, extra ...string) string {
	t.Helper()
	lib := concatModuleLib(t, ral.Concat1001)
	dir := t.TempDir()
	for _, src := range append([]string{lib}, extra...) {
		if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	if edit != nil {
		at := filepath.Join(dir, "puppet", "type", "concat_file.rb")
		b, err := os.ReadFile(at)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, edit(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// m1 is measurement M1's catalog (01-basic, compiled by openvox 8.33.0).
const m1 = `{"tags":["settings"],"name":"spine.example","version":1795067337,"code_id":null,
 "catalog_uuid":"be111265-1502-4b9a-b540-6e17f9cd646d","catalog_format":2,"environment":"production","classes":["settings"],
 "resources":[
  {"type":"Stage","title":"main","tags":["stage"],"exported":false,"kind":"compilable_type","parameters":{"name":"main"}},
  {"type":"Class","title":"Settings","tags":["class","settings"],"exported":false,"kind":"unknown"},
  {"type":"Class","title":"main","tags":["class"],"exported":false,"kind":"unknown","parameters":{"name":"main"}},
  {"type":"Concat_file","title":"/etc/c01.conf","tags":["concat_file","class"],"file":"/tmp/e/production/manifests/site.pp","line":1,"exported":false,"kind":"compilable_type"},
  {"type":"Concat_fragment","title":"c","tags":["concat_fragment","c","class"],"file":"/tmp/e/production/manifests/site.pp","line":2,"exported":false,"kind":"compilable_type","parameters":{"target":"/etc/c01.conf","content":"three\n"}},
  {"type":"Concat_fragment","title":"a","tags":["concat_fragment","a","class"],"file":"/tmp/e/production/manifests/site.pp","line":3,"exported":false,"kind":"compilable_type","parameters":{"target":"/etc/c01.conf","content":"one\n"}},
  {"type":"Concat_fragment","title":"b","tags":["concat_fragment","b","class"],"file":"/tmp/e/production/manifests/site.pp","line":4,"exported":false,"kind":"compilable_type","parameters":{"target":"/etc/c01.conf","content":"two\n"}}],
 "edges":[
  {"source":"Stage[main]","target":"Class[Settings]"},
  {"source":"Stage[main]","target":"Class[main]"},
  {"source":"Class[main]","target":"Concat_file[/etc/c01.conf]"},
  {"source":"Class[main]","target":"Concat_fragment[c]"},
  {"source":"Class[main]","target":"Concat_fragment[a]"},
  {"source":"Class[main]","target":"Concat_fragment[b]"}]}`

// m1Catalog writes M1's catalog with its target moved to <dir>/c01.conf,
// plus extra resources contained in Class[main], and returns the catalog's
// path and the target.
func m1Catalog(t *testing.T, dir string, extra ...map[string]any) (string, string) {
	t.Helper()
	target := filepath.Join(dir, "c01.conf")
	var body map[string]any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(m1, "/etc/c01.conf", target)), &body); err != nil {
		t.Fatal(err)
	}
	for _, r := range extra {
		body["resources"] = append(body["resources"].([]any), r)
		ref := fmt.Sprintf("%s[%s]", r["type"], r["title"])
		body["edges"] = append(body["edges"].([]any), map[string]any{"source": "Class[main]", "target": ref})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, target
}

const m1Content = "one\ntwo\nthree\n"

// M1 end to end with both types native, without a host and with one loading
// the catalog for another type: the run writes the file and prints Ruby's
// console line. Native eval_generate finds the File load generation added
// (natively or by the host), so the provider sees the catalog after it.
func TestConcatAppliesNativelyEndToEnd(t *testing.T) {
	bin := buildBinary(t)
	for _, tc := range []struct {
		name string
		host bool
	}{{"no host", false}, {"host loaded", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"--no-ruby", "--libdir", concatLibdir(t, nil)}
			var extra []map[string]any
			if tc.host {
				args = []string{"--ruby", extensiontest.Ruby(t), "--libdir", concatLibdir(t, nil, extensiontest.Libdir(t))}
				extra = append(extra, ext("Vox_value", "v", map[string]any{"path": filepath.Join(dir, "v"), "value": "x"}))
			}
			c, target := m1Catalog(t, dir, extra...)
			reports := filepath.Join(dir, "reports")
			stdout, stderr, code := run(t, bin, append([]string{"apply", "--catalog", c, "--vardir", t.TempDir(),
				"--report-dir", reports}, args...)...)
			if code != 0 {
				t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if strings.Contains(stderr, "concat_f") {
				t.Errorf("concat went to the host:\n%s", stderr)
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != m1Content {
				t.Errorf("target = %q, %v; want %q", b, err, m1Content)
			}
			sum := sha256.Sum256([]byte(m1Content))
			want := fmt.Sprintf("Notice: /Stage[main]/Main/File[%s]/ensure: defined content as '{sha256}%s'\n", target, hex.EncodeToString(sum[:]))
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, stdout)
			}
			st, ok := readReport(t, reports)["resource_statuses"].(map[string]any)["File["+target+"]"].(map[string]any)
			if !ok {
				t.Fatal("report has no status for the generated File")
			}
			var events []string
			for _, e := range st["events"].([]any) {
				ev := e.(map[string]any)
				events = append(events, fmt.Sprint(ev["name"], " ", ev["status"], " ", ev["previous_value"], "->", ev["desired_value"]))
			}
			if w := []string{"file_created success absent->file"}; !slices.Equal(events, w) {
				t.Errorf("events = %q, want %q", events, w)
			}
		})
	}
}

// The `applied via extension host` line names concat_file when the tree is
// not a measured version, and not when it is; both write the same file.
func TestConcatIsNativeOnlyWithAMeasuredTree(t *testing.T) {
	bin := buildBinary(t)
	ruby := extensiontest.Ruby(t)
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
		host bool
	}{
		{"measured", nil, false},
		{"one byte appended", func(b []byte) []byte { return append(b, '\n') }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c, target := m1Catalog(t, dir)
			_, stderr, code := run(t, bin, "apply", "--catalog", c, "--ruby", ruby,
				"--libdir", concatLibdir(t, tc.edit), "--vardir", t.TempDir())
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr)
			}
			if got := strings.Contains(stderr, "1 concat_file resource(s) applied via extension host"); got != tc.host {
				t.Errorf("concat_file through the host = %v, want %v:\n%s", got, tc.host, stderr)
			}
			if got := strings.Contains(stderr, "concat_fragment resource(s) applied via extension host"); got != tc.host {
				t.Errorf("concat_fragment through the host = %v, want %v:\n%s", got, tc.host, stderr)
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != m1Content {
				t.Errorf("target = %q, %v; want %q", b, err, m1Content)
			}
		})
	}
}

// generated is what a load-time generation left on one resource.
type generated struct {
	Index         int
	Tags          []string
	Parameters    map[string]any
	Sensitive     []string
	GeneratedFrom string
	Containers    []string
}

func generatedOf(cat *catalog.Catalog, ref string) (generated, bool) {
	for i, r := range cat.Resources {
		if r.Ref() != ref {
			continue
		}
		g := generated{Index: i, Tags: r.Tags, Parameters: r.Parameters, Sensitive: r.SensitiveParameters}
		if r.GeneratedFrom != nil {
			g.GeneratedFrom = r.GeneratedFrom.Ref()
		}
		for _, e := range cat.Edges {
			if e.Target == ref {
				g.Containers = append(g.Containers, e.Source)
			}
		}
		return g, true
	}
	return generated{}, false
}

// Ruby's generate in the host and native LoadGenerate build the same File
// for a new path and leave a declared File the same (spec §0 row 3): its
// parameters (before included), tags, containment, generator and position.
func TestConcatGenerationHostAndNativeAgree(t *testing.T) {
	dir := t.TempDir()
	declared := filepath.Join(dir, "d.conf")
	byPath := filepath.Join(dir, "p.conf")
	c, target := m1Catalog(t, dir,
		ext("Concat_file", declared, map[string]any{"mode": "0600", "tag": "dtag"}),
		ext("File", declared, map[string]any{"owner": "root"}),
		ext("Concat_fragment", "d1", map[string]any{"target": declared, "content": "d\n"}),
		// A File declared by its path parameter, under a title that is not the path.
		ext("Concat_file", "titled", map[string]any{"path": byPath}),
		ext("File", "alias", map[string]any{"path": byPath, "owner": "root"}),
		ext("Concat_fragment", "t1", map[string]any{"target": byPath, "content": "t\n"}),
		// Vox_value makes the host load the catalog.
		ext("Vox_value", "v", map[string]any{"path": filepath.Join(dir, "v"), "value": "x"}))
	raw, err := os.ReadFile(c)
	if err != nil {
		t.Fatal(err)
	}
	o := applyOptions{ruby: extensiontest.Ruby(t), libdir: concatLibdir(t, nil, extensiontest.Libdir(t)),
		vardir: t.TempDir(), level: "notice"}

	host, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	in, msg := ingestExtensions(extensionConfig{ruby: o.ruby, libdir: o.libdir, vardir: o.vardir, level: o.level,
		timeouts: extension.Timeouts{Evaluate: time.Minute, Other: time.Minute}}, raw, host, nativeProviders(host, o, ral.HostPlatform))
	if in.host != nil {
		defer in.host.Close()
	}
	if msg != "" || !in.hostLoaded {
		t.Fatalf("host load: loaded %v, %q", in.hostLoaded, msg)
	}

	native, err := catalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	natives := nativeProviders(native, o, ral.HostPlatform)
	if !slices.ContainsFunc(natives, func(p ral.Provider) bool { return p.Type() == "concat_file" }) {
		t.Fatal("concat_file is not native with a measured tree")
	}
	if loadGenerate(native, ral.NewRegistry(natives...)) {
		t.Fatal("native load generation refused the catalog")
	}

	for _, ref := range []string{"File[" + target + "]", "File[" + declared + "]",
		"Concat_file[" + target + "]", "Concat_file[" + declared + "]", "File[alias]", "Concat_file[titled]"} {
		h, okH := generatedOf(host, ref)
		n, okN := generatedOf(native, ref)
		if !okH || !okN {
			t.Fatalf("%s: in host catalog %v, in native %v", ref, okH, okN)
		}
		if !reflect.DeepEqual(h, n) {
			t.Errorf("%s differs:\nhost   %#v\nnative %#v", ref, h, n)
		}
	}
	if g, _ := generatedOf(native, "File["+target+"]"); g.GeneratedFrom != "Concat_file["+target+"]" {
		t.Errorf("new File generated from %q", g.GeneratedFrom)
	}
}

// D-84 holds only without a host: when the host loads the catalog it
// converts the fragment too and refuses a float order as Ruby does.
func TestConcatFloatOrderRefusedWhenHostLoads(t *testing.T) {
	ruby := extensiontest.Ruby(t)
	libdir := concatLibdir(t, nil, extensiontest.Libdir(t))
	resources := func(dir string, host bool) []map[string]any {
		target := filepath.Join(dir, "f.conf")
		rs := []map[string]any{
			ext("Concat_file", target, map[string]any{}),
			ext("Concat_fragment", "z", map[string]any{"target": target, "content": "z\n", "order": json.Number("2.0")}),
		}
		if host {
			rs = append(rs, ext("Vox_value", "v", map[string]any{"path": filepath.Join(dir, "v"), "value": "x"}))
		}
		return rs
	}
	o := applyOptions{ruby: ruby, libdir: libdir, vardir: t.TempDir(), level: "notice",
		timeouts: extension.Timeouts{Evaluate: time.Minute, Other: time.Minute}}

	dir := t.TempDir()
	stderr, couldNot := applyInProcess(t, resources(dir, true), o)
	want := "Parameter order failed on Concat_fragment[z]: $order is not a string or integer."
	if !couldNot || !strings.Contains(stderr, want) {
		t.Errorf("with a host: could-not-run %v, want a refusal naming %q:\n%s", couldNot, want, stderr)
	}

	o.noRuby = true
	if stderr, couldNot := applyInProcess(t, resources(dir, false), o); couldNot {
		t.Errorf("without a host: refused, want it applied (D-84):\n%s", stderr)
	}
}
