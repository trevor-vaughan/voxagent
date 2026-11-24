package extension_test

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
)

type res = map[string]any

func resource(typ, title string, params res) res {
	kind := "compilable_type"
	if typ == "Class" {
		kind = "class"
	}
	return res{"type": typ, "title": title, "kind": kind, "tags": []string{strings.ToLower(typ)},
		"exported": false, "file": "/x/site.pp", "line": 3, "parameters": params}
}

// fixture returns the shared test resources over dir.
func fixture(dir string) []res {
	return []res{
		resource("File", dir, res{"ensure": "directory"}),
		resource("Vox_value", "a", res{"path": dir + "/a", "value": "one", "peer": "b"}),
		resource("Vox_value", "b", res{"path": dir + "/b", "value": "two"}),
		resource("Vox_value", "f", res{"path": dir + "/f", "value": "fail"}),
		resource("Vox_value", "c", res{"path": dir + "/c", "value": "chatty"}),
		resource("Notify", "after-a", res{}), resource("Notify", "before-a", res{}),
		resource("Vox_prefetch", "g1", res{"ensure": "present", "provider": "good"}),
		resource("Vox_prefetch", "b1", res{"ensure": "present", "provider": "bad"}),
		resource("Vox_prefetch", "b2", res{"ensure": "present", "provider": "bad"}),
		resource("Vox_rapi", "r", res{}), resource("Vox_gen", "quiet", res{}),
		resource("Vox_gen", "loud", res{"make": true}), resource("Vox_gen", "broken", res{"make": "raise"}),
		resource("Vox_gen", "ordered", res{"make": "before"}),
		resource("Vox_value", "r", res{"path": dir + "/r", "value": "refusing-refresh"}), resource("Vox_value", "bogus", res{"nosuch": 1}),
		resource("Nosuch", "x", res{}),
		resource("Vox_conf", "none", res{"value": "v"}), resource("Vox_conf", "none2", res{"value": "v"}),
		resource("Vox_conf", "chosen", res{"value": "v", "provider": "never"}),
		resource("Vox_evalgen", "two", res{"make": "2"}), resource("Vox_evalgen", "boom", res{"make": "raise"}),
		resource("Vox_value", "vt", res{"name": "vn", "path": dir + "/vt", "value": "v"}),
	}
}

func launch(t *testing.T) (*extension.Host, extension.LoadResult, string) {
	t.Helper()
	return launchWith(t, fixture)
}

// launchWith builds a catalog of Stage[main], Class[main] and build(dir)'s
// resources, each contained by Class[main], then launches a real host and
// loads it like launch. The host's vardir is <dir>/vardir, so nothing a test
// evaluates writes under the owner's Puppet vardir.
//
// The main class is spelled Class[main], as a compiled catalog spells it,
// although Puppet canonicalises the ref to Class[Main] when it reads one: the
// host must answer in the catalog's refs, not Puppet's.
func launchWith(t *testing.T, build func(dir string) []res) (*extension.Host, extension.LoadResult, string) {
	t.Helper()
	return launchAt(t, "notice", build)
}

// launchAt is launchWith with the host's log level set to level.
func launchAt(t *testing.T, level string, build func(dir string) []res) (*extension.Host, extension.LoadResult, string) {
	t.Helper()
	ruby := extensiontest.Ruby(t)
	h, err := extension.Launch(extension.Config{Ruby: ruby,
		Timeouts: extension.Timeouts{Evaluate: time.Minute, Other: time.Minute}}, os.Environ())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	dir := t.TempDir()
	vardir := filepath.Join(dir, "vardir")
	if err := os.Mkdir(vardir, 0o755); err != nil {
		t.Fatal(err)
	}
	resources := append([]res{resource("Stage", "main", res{}), resource("Class", "main", res{"name": "main"})},
		build(dir)...)
	edges := []res{{"source": "Stage[main]", "target": "Class[main]"}}
	var refs []string
	for _, r := range resources {
		ref := r["type"].(string) + "[" + r["title"].(string) + "]"
		refs = append(refs, ref)
		if r["type"] != "Stage" && r["type"] != "Class" {
			edges = append(edges, res{"source": "Class[main]", "target": ref})
		}
	}
	cat, _ := json.Marshal(res{"name": "n", "version": 7, "environment": "production", "catalog_format": 2,
		"tags": []string{}, "classes": []string{}, "resources": resources, "edges": edges})
	load, err := h.Load(cat, refs, extensiontest.Libdir(t), vardir, level)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return h, load, dir
}

func TestRealHostLoadClassifies(t *testing.T) {
	_, load, _ := launch(t)
	for typ, want := range map[string]extension.TypeInfo{
		"vox_value": {Class: "tier3", Origin: "libdir", Refreshable: true},
		"vox_rapi":  {Class: "tier2", Origin: "libdir"},
		"nosuch":    {Class: "missing"},
		"file":      {Class: "tier3", Origin: "core", EvalGenerates: true},
	} {
		if got := load.Types[typ]; got != want {
			t.Errorf("Types[%s] = %+v, want %+v", typ, got, want)
		}
	}
	if !slices.Equal(load.Metaparams, []string{"alias", "audit", "before", "loglevel", "noop", "notify",
		"require", "schedule", "stage", "subscribe", "tag"}) {
		t.Errorf("Metaparams = %v", load.Metaparams)
	}
	if !slices.Contains(load.Invalid, extension.Refusal{Ref: "Vox_value[bogus]", Message: "no parameter named 'nosuch'"}) {
		t.Errorf("Invalid = %v", load.Invalid)
	}
	if p := load.Resources["Vox_prefetch[g1]"].ProviderUsed; p == nil || *p != "good" {
		t.Errorf("provider_used of Vox_prefetch[g1] = %v", p)
	}
}

func TestRealHostRelationships(t *testing.T) {
	h, _, dir := launch(t)
	rels, err := h.Relationships([]string{"Vox_value[a]"})
	if err != nil {
		t.Fatal(err)
	}
	want := extension.Relations{Require: []string{"File[" + dir + "]"}, Before: []string{"Notify[after-a]"},
		Subscribe: []string{"Notify[before-a]"}, Notify: []string{"Vox_value[b]"}}
	got := rels["Vox_value[a]"]
	if !slices.Equal(got.Require, want.Require) || !slices.Equal(got.Before, want.Before) ||
		!slices.Equal(got.Subscribe, want.Subscribe) || !slices.Equal(got.Notify, want.Notify) {
		t.Errorf("Relations = %+v, want %+v", got, want)
	}
}

func TestRealHostEvaluateNoopThenLive(t *testing.T) {
	h, _, dir := launch(t)
	ev, err := h.Evaluate("Vox_value[a]", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Events) != 1 || ev.Events[0].Status != "noop" || ev.Events[0].Message != "defined 'value' as 'one' (noop)" {
		t.Errorf("noop events = %+v", ev.Events)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Errorf("noop wrote the file: %v", err)
	}
	ev, err = h.Evaluate("Vox_value[a]", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Events) != 1 || ev.Events[0].Status != "success" || ev.Failed {
		t.Errorf("live evaluation = %+v", ev)
	}
	if len(ev.Logs) != 1 || ev.Logs[0].Source != "/Stage[main]/Main/Vox_value[a]/value" ||
		ev.Logs[0].File != "/x/site.pp" || ev.Logs[0].Line != 3 {
		t.Errorf("logs = %+v", ev.Logs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a")); string(b) != "one\n" {
		t.Errorf("the file holds %q", b)
	}
}

func TestRealHostEvaluateFailure(t *testing.T) {
	h, _, _ := launch(t)
	ev, err := h.Evaluate("Vox_value[f]", false)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Failed || len(ev.Events) != 1 || ev.Events[0].Status != "failure" ||
		ev.Events[0].Message != "change from 'absent' to 'fail' failed: refusing to write fail" {
		t.Errorf("failure = %+v", ev)
	}
}

func TestRealHostPrefetchFailureIsPerProviderClass(t *testing.T) {
	h, _, _ := launch(t)
	good, err := h.Evaluate("Vox_prefetch[g1]", false)
	if err != nil || good.Failed {
		t.Fatalf("the good provider's resource = %+v, %v", good, err)
	}
	bad, err := h.Evaluate("Vox_prefetch[b1]", false)
	if err != nil {
		t.Fatal(err)
	}
	if !bad.Failed || !bad.Skipped || len(bad.Events) != 1 ||
		bad.Events[0].Message != "Prefetch failed for vox_prefetch provider 'bad'" || *bad.Events[0].Name != "resource_error" {
		t.Errorf("bad = %+v", bad)
	}
	var lines []string
	for _, l := range bad.Logs {
		lines = append(lines, l.Level+" "+l.Source+" "+l.Message)
	}
	want := []string{"err Puppet Could not prefetch vox_prefetch provider 'bad': bad prefetch",
		"warning /Stage[main]/Main/Vox_prefetch[b1] Skipping because provider prefetch failed"}
	if !slices.Equal(lines, want) {
		t.Errorf("logs = %q, want %q", lines, want)
	}
	skipped, err := h.Skip("Vox_prefetch[b2]")
	if err != nil || !skipped.Failed || len(skipped.Events) != 1 || len(skipped.Logs) != 0 {
		t.Errorf("Skip = %+v, %v", skipped, err)
	}
	logs, err := h.Finish()
	if err != nil || len(logs) != 1 || logs[0].Message != "vox_prefetch good post_resource_eval ran" {
		t.Errorf("Finish = %+v, %v", logs, err)
	}
}

func TestRealHostRefresh(t *testing.T) {
	h, _, dir := launch(t)
	rr, err := h.Refresh("Vox_value[b]")
	if err != nil || !rr.Refreshed || rr.Error != nil {
		t.Fatalf("Refresh = %+v, %v", rr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.refreshed")); string(b) != "refreshed\n" {
		t.Errorf("refresh side effect = %q", b)
	}
}

// The host's Puppet[:vardir] is the vardir voxagent gives it, which a Tier 3
// type's own code writes under, and self_refresh is classified per type. After
// Finish the vardir holds only what the type wrote: Puppet::Util::Storage is
// never stored, because the host never runs transaction.evaluate or the
// Configurer, which are what store state.yaml.
func TestRealHostSelfRefreshAndVardir(t *testing.T) {
	h, load, dir := launchWith(t, func(dir string) []res {
		return []res{resource("Vox_selfref", "x", res{"value": "new"}),
			resource("Vox_value", "v", res{"path": dir + "/v", "value": "v"})}
	})
	if !load.Types["vox_selfref"].SelfRefresh || load.Types["vox_value"].SelfRefresh {
		t.Errorf("Types = %+v, want self_refresh on vox_selfref only", load.Types)
	}
	if ev, err := h.Evaluate("Vox_selfref[x]", false); err != nil || ev.Failed {
		t.Fatalf("Evaluate = %+v, %v", ev, err)
	}
	if rr, err := h.Refresh("Vox_selfref[x]"); err != nil || !rr.Refreshed || rr.Error != nil {
		t.Fatalf("Refresh = %+v, %v", rr, err)
	}
	vardir := filepath.Join(dir, "vardir")
	if b, err := os.ReadFile(filepath.Join(vardir, "vox_selfref.log")); err != nil || string(b) != "x\n" {
		t.Errorf("vox_selfref.log = %q, %v; want the refresh to write it under the given vardir", b, err)
	}
	if _, err := h.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	var written []string
	err := filepath.WalkDir(vardir, func(path string, _ os.DirEntry, err error) error {
		if err != nil || path == vardir {
			return err
		}
		rel, err := filepath.Rel(vardir, path)
		written = append(written, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vox_selfref.log"}; !slices.Equal(written, want) {
		t.Errorf("vardir holds %q, want %q", written, want)
	}
}

// Review Focus 1: module code printing to $stdout, STDOUT or a child's stdout
// must reach stderr and leave the protocol intact.
func TestHostSurvivesChattyProvider(t *testing.T) {
	h, _, _ := launch(t)
	ev, err := h.Evaluate("Vox_value[c]", false)
	if err != nil || ev.Failed {
		t.Fatalf("chatty evaluation = %+v, %v", ev, err)
	}
	if _, err := h.Evaluate("Vox_value[a]", false); err != nil {
		t.Fatalf("the next call after a chatty provider: %v", err)
	}
}

func TestRealHostCallErrorForUnknownRef(t *testing.T) {
	h, _, _ := launch(t)
	if _, err := h.Evaluate("Nope[x]", false); err == nil {
		t.Fatal("a ref outside the catalog was evaluated")
	}
	if _, err := h.Evaluate("Vox_value[a]", false); err != nil {
		t.Errorf("the host is unusable after refusing a foreign ref: %v", err)
	}
}

func TestRealHostGenerateFailureIsReportedNotFatal(t *testing.T) {
	_, load, _ := launch(t)
	if !load.FailedToGenerate {
		t.Error("FailedToGenerate = false")
	}
	var lines []string
	for _, l := range load.GenerateLogs {
		lines = append(lines, l.Level+" "+l.Source+" "+l.Message)
	}
	t.Logf("generate logs: %q", lines)
	const warn = "warning Puppet Found multiple default providers for vox_prefetch: bad, good; using bad"
	var loadLines []string
	for _, l := range load.Logs {
		loadLines = append(loadLines, l.Level+" "+l.Source+" "+l.Message)
	}
	if !slices.Contains(loadLines, warn) || slices.Contains(lines, warn) {
		t.Errorf("Logs = %q, GenerateLogs = %q: the load warning belongs in Logs only", loadLines, lines)
	}
	want := "err /Stage[main]/Main/Vox_gen[broken] Failed to generate additional resources using 'generate': generate broke"
	if !slices.Contains(lines, want) {
		t.Errorf("GenerateLogs = %q, want it to contain %q", lines, want)
	}
	if slices.Contains(loadLines, want) {
		t.Errorf("the generate failure is also in Logs: %q", loadLines)
	}
}

func TestRealHostRefreshFailure(t *testing.T) {
	h, _, _ := launch(t)
	rr, err := h.Refresh("Vox_value[r]")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Refreshed || rr.Error == nil || *rr.Error != "refresh refused" {
		t.Fatalf("Refresh = %+v", rr)
	}
	// event_manager.rb:160-165 logs "Failed to call refresh" between the
	// callback's own lines and log_exception's, so the host keeps them apart.
	format := func(logs []extension.Log) []string {
		var lines []string
		for _, l := range logs {
			lines = append(lines, l.Level+" "+l.Source+" "+l.Message)
		}
		return lines
	}
	if got, want := format(rr.Logs), []string{"notice /Stage[main]/Main/Vox_value[r] refusing to refresh"}; !slices.Equal(got, want) {
		t.Errorf("callback logs = %q, want %q", got, want)
	}
	if got, want := format(rr.ExceptionLogs), []string{"err /Stage[main]/Main/Vox_value[r] refresh refused"}; !slices.Equal(got, want) {
		t.Errorf("exception logs = %q, want %q", got, want)
	}
}

// A resource with no suitable provider is never evaluated (relationship_graph.rb:116-137):
// transaction.rb:124-136 marks it failed, logging per resource only when the
// provider was chosen, and teardown logs "Could not find a suitable provider"
// once per type for the rest.
func TestRealHostUnsuitableProvider(t *testing.T) {
	h, load, _ := launch(t)
	if p := load.Resources["Vox_conf[chosen]"].ProviderUsed; p == nil || *p != "never" {
		t.Errorf("provider_used of Vox_conf[chosen] = %v", p)
	}
	if p := load.Resources["Vox_conf[none]"].ProviderUsed; p != nil {
		t.Errorf("provider_used of Vox_conf[none] = %q, want null", *p)
	}
	for _, ref := range []string{"Vox_conf[none]", "Vox_conf[none2]"} {
		ev, err := h.Evaluate(ref, false)
		if err != nil || !ev.Failed || ev.Skipped || !ev.Unsuitable || len(ev.Events) != 0 || len(ev.Logs) != 0 {
			t.Errorf("Evaluate(%s) = %+v, %v", ref, ev, err)
		}
	}
	// The answer logs nothing: Puppet logs at the overly-deferred moment,
	// which the walk reaches later (relationship_graph.rb:136-139).
	ev, err := h.Skip("Vox_conf[chosen]")
	if err != nil || !ev.Failed || !ev.Unsuitable || len(ev.Events) != 0 || len(ev.Logs) != 0 {
		t.Errorf("Skip(Vox_conf[chosen]) = %+v, %v", ev, err)
	}
	logs, err := h.OverlyDeferred("Vox_conf[chosen]")
	if err != nil || len(logs) != 1 || logs[0].Level != "err" || logs[0].Source != "/Stage[main]/Main/Vox_conf[chosen]" ||
		logs[0].Message != "Provider never is not functional on this host" {
		t.Errorf("OverlyDeferred(Vox_conf[chosen]) = %+v, %v", logs, err)
	}
	for _, ref := range []string{"Vox_conf[none]", "Vox_conf[none2]"} {
		if logs, err := h.OverlyDeferred(ref); err != nil || len(logs) != 0 {
			t.Errorf("OverlyDeferred(%s) = %+v, %v; want teardown to log it", ref, logs, err)
		}
	}
	logs, err = h.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range logs {
		lines = append(lines, l.Level+" "+l.Source+" "+l.Message)
	}
	if want := []string{"err Puppet Could not find a suitable provider for vox_conf"}; !slices.Equal(lines, want) {
		t.Errorf("Finish logs = %q, want %q", lines, want)
	}
}

func TestRealHostLoadGenerates(t *testing.T) {
	_, load, _ := launch(t)
	if len(load.Generated.Resources) != 1 {
		t.Fatalf("Generated.Resources = %+v, want only Notify[made-by-loud]", load.Generated.Resources)
	}
	made := load.Generated.Resources[0]
	if made.Type != "Notify" || made.Title != "made-by-loud" || made.Kind != "compilable_type" ||
		made.Exported || !slices.Contains(made.Tags, "vox_gen") {
		t.Errorf("generated resource = %+v", made)
	}
	// Puppet's order, not sorted: the resource's own tags, then its
	// generator's (additional_resource_generator.rb:139, merge_tags_from), as
	// the report writes them (resource/status.rb:172,216). Measured in
	// report-diff-extension.sh's Ruby report. The fixture's catalog tags
	// Vox_gen[loud] only "vox_gen", so its title is not among them.
	if want := []string{"notify", "made-by-loud", "vox_gen"}; !slices.Equal(made.Tags, want) {
		t.Errorf("tags = %q, want %q", made.Tags, want)
	}
	// add_resource's parent_resource (additional_resource_generator.rb:137).
	if got := load.Generated.Generators["Notify[made-by-loud]"]; got != "Vox_gen[loud]" {
		t.Errorf("generator = %q, want Vox_gen[loud]", got)
	}
	if got := load.Generated.Containers["Notify[made-by-loud]"]; got != "Class[main]" {
		t.Errorf("container = %q, want Class[main]", got)
	}
	// Puppet spells the main class Class[Main]; the edge names it as the catalog does.
	want := []extension.BeforeEdge{{Source: "Vox_gen[loud]", Target: "Notify[made-by-loud]"},
		{Source: "Vox_gen[ordered]", Target: "Class[main]"}}
	if got := load.Generated.Before; !slices.Equal(got, want) && !slices.Equal(got, []extension.BeforeEdge{want[1], want[0]}) {
		t.Errorf("Before = %v, want %v", load.Generated.Before, want)
	}
	if len(load.Generated.Overrides) != 0 {
		t.Errorf("Overrides = %v", load.Generated.Overrides)
	}
	if _, ok := load.Resources["Notify[made-by-loud]"]; !ok {
		t.Error("the generated resource is not in Resources")
	}
	if !load.FailedToGenerate {
		t.Error("Vox_gen[broken] did not set FailedToGenerate")
	}
	var lines []string
	for _, l := range load.GenerateLogs {
		lines = append(lines, l.Source+" "+l.Message)
	}
	if !slices.Contains(lines, "/Stage[main]/Main/Vox_gen[broken] Failed to generate additional resources using 'generate': generate broke") {
		t.Errorf("GenerateLogs = %q", lines)
	}
}

// Puppet inserts what generate makes next to its parent
// in the catalog array (additional_resource_generator.rb:36-50,137-145,
// resource/catalog.rb:99-124). Read from the source and measured here against
// the real host's Puppet 8.10.0: a breadth-first generate's resources go after
// the parent, in generate order, because the reverse! cancels
// add_resource_after's reversal; a depth-first one's go before it, in generate
// order, because each add_resource_before inserts at the parent's index.
func TestRealHostOrdersGeneratedResourcesLikePuppet(t *testing.T) {
	_, load, _ := launchWith(t, func(string) []res {
		return []res{
			resource("Notify", "first", res{}),
			resource("Vox_gen", "loud", res{"make": true}),
			resource("Vox_deep", "deep", res{}),
			resource("Notify", "last", res{}),
		}
	})
	var got []string
	for _, ref := range load.Order {
		if !strings.HasPrefix(ref, "Stage[") && !strings.HasPrefix(ref, "Class[") {
			got = append(got, ref)
		}
	}
	want := []string{"Notify[first]", "Vox_gen[loud]", "Notify[made-by-loud]",
		"Notify[deep-1]", "Notify[deep-2]", "Vox_deep[deep]", "Notify[last]"}
	if !slices.Equal(got, want) {
		t.Errorf("Order = %q, want %q", got, want)
	}
	// A depth-first parent's children are ordered before it: the `before` is
	// appended to each new resource (additional_resource_generator.rb:160-167),
	// so its source is the generated ref, as `generated` spells it.
	for _, edge := range []extension.BeforeEdge{{Source: "Notify[deep-1]", Target: "Vox_deep[deep]"},
		{Source: "Notify[deep-2]", Target: "Vox_deep[deep]"}} {
		if !slices.Contains(load.Generated.Before, edge) {
			t.Errorf("Before = %v, want %v among them", load.Generated.Before, edge)
		}
	}
}

func TestRealHostEvalGenerate(t *testing.T) {
	h, _, _ := launch(t)
	two, err := h.EvalGenerate("Vox_evalgen[two]")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range two.Resources {
		titles = append(titles, r.Title)
		if two.Containers[r.Type+"["+r.Title+"]"] == "" {
			t.Errorf("%s[%s] has no container", r.Type, r.Title)
		}
	}
	// The order eval_generate returned them, which is the order Puppet applies
	// them in (additional_resource_generator.rb:131-135), not catalog order.
	if !slices.Equal(titles, []string{"two-child-1", "two-child-2"}) || two.Failed {
		t.Errorf("titles = %v, Failed = %v", titles, two.Failed)
	}
	for _, ref := range []string{"Notify[two-child-1]", "Notify[two-child-2]"} {
		if got := two.Generators[ref]; got != "Vox_evalgen[two]" {
			t.Errorf("%s's generator = %q, want Vox_evalgen[two]", ref, got)
		}
	}
	if len(two.Existing) != 0 {
		t.Errorf("Existing = %v, want none", two.Existing)
	}
	plain, err := h.EvalGenerate("Vox_value[a]")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Resources) != 0 || len(plain.Existing) != 0 || plain.Failed || len(plain.Logs) != 0 {
		t.Errorf("a type without eval_generate answered %+v, want an empty generation", plain)
	}
	boom, err := h.EvalGenerate("Vox_evalgen[boom]")
	if err != nil {
		t.Fatal(err)
	}
	const msg = "Failed to generate additional resources using 'eval_generate': eval_generate broke"
	var found bool
	for _, l := range boom.Logs {
		if l.Level == "err" && l.Message == msg && strings.HasSuffix(l.Source, "Vox_evalgen[boom]") {
			found = true
		}
	}
	if !boom.Failed || !found || len(boom.Resources) != 0 {
		t.Errorf("boom = %+v, want Failed with the err log %q", boom, msg)
	}
	if _, err := h.Evaluate("Notify[two-child-1]", false); err != nil {
		t.Errorf("Evaluate of a generated resource: %v", err)
	}
}

func TestRealHostAliases(t *testing.T) {
	_, load, _ := launch(t)
	if got := load.Resources["Vox_value[vt]"].Aliases; !slices.Equal(got, []string{"Vox_value[vn]"}) {
		t.Errorf("Aliases = %v, want [Vox_value[vn]]", got)
	}
	// A compiled catalog's Class[main] carries name => main, which Puppet
	// registers as the alias Class[main]: the resource's own ref.
	if got := load.Resources["Class[main]"].Aliases; len(got) != 0 {
		t.Errorf("Aliases of Class[main] = %v, want none", got)
	}
	if got := load.Resources["Vox_value[a]"].Aliases; len(got) != 0 {
		t.Errorf("Aliases of a plain resource = %v", got)
	}
}

// A type that overrides title is named by Puppet's own ref, which its catalog
// cannot look the resource up by. A title already in Puppet's spelling is no override.
func TestRealHostReportsPuppetRefs(t *testing.T) {
	h, load, _ := launchWith(t, func(string) []res {
		return []res{
			resource("Vox_titled", "fips", res{"value": "x"}),
			resource("Vox_titled", "quiet:normal", res{"name": "quiet", "bootmode": "normal", "value": "x"}),
			resource("Vox_titled", "vga", res{"bootmode": "recovery", "value": "x"}),
			resource("Vox_titled", "audit:all", res{"name": "audit", "bootmode": "all", "value": "x"}),
		}
	})
	for ref, want := range map[string]extension.ResourceInfo{
		"Vox_titled[fips]":         {PuppetRef: "Vox_titled[fips:all]", PuppetTitle: "fips:all", Unresolvable: true},
		"Vox_titled[quiet:normal]": {PuppetRef: "Vox_titled[quiet:default]", PuppetTitle: "quiet:default", Unresolvable: true},
		"Vox_titled[vga]":          {PuppetRef: "Vox_titled[vga:recovery]", PuppetTitle: "vga:recovery", Unresolvable: true},
		"Vox_titled[audit:all]":    {},
	} {
		got := load.Resources[ref]
		if got.PuppetRef != want.PuppetRef || got.PuppetTitle != want.PuppetTitle || got.Unresolvable != want.Unresolvable {
			t.Errorf("Resources[%s] = %+v, want %+v", ref, got, want)
		}
	}
	for _, ref := range []string{"Stage[main]", "Class[main]"} {
		if got := load.Resources[ref]; got.PuppetRef != "" || got.PuppetTitle != "" || got.Unresolvable {
			t.Errorf("Resources[%s] = %+v: a container's canonical ref is no override", ref, got)
		}
	}
	ev, err := h.Evaluate("Vox_titled[fips]", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Logs) != 1 || ev.Logs[0].Source != "/Stage[main]/Main/Vox_titled[fips:all]/value" {
		t.Errorf("logs = %+v, want one at Puppet's own ref", ev.Logs)
	}
}

// richValue is a catalog's rich-data encoding of a Puppet value.
func richValue(ptype, pvalue string) res {
	return res{"__ptype": ptype, "__pvalue": pvalue}
}

// seededValues returns Vox_value resources, one per title, whose value is
// values[title] and whose file already holds "old".
func seededValues(t *testing.T, values map[string]any) func(dir string) []res {
	return func(dir string) []res {
		var out []res
		for _, title := range slices.Sorted(maps.Keys(values)) {
			path := filepath.Join(dir, title)
			if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out = append(out, resource("Vox_value", title, res{"path": path, "value": values[title]}))
		}
		return out
	}
}

// A Binary parameter reaches the type as its bytes, as puppet apply's
// DeferredResolver pass leaves it, and the event reports those bytes as text
// rather than Base64.
func TestRealHostResolvesBinaryLikeApply(t *testing.T) {
	h, load, _ := launchWith(t, seededValues(t, map[string]any{
		"bin": richValue("Binary", "aGVsbG8="), "utf": richValue("Binary", "w6k="), "raw": richValue("Binary", "AAEC/w=="),
	}))
	if len(load.Invalid) != 0 {
		t.Fatalf("Invalid = %v", load.Invalid)
	}
	for _, c := range []struct{ title, message, desired string }{
		{"bin", "value changed 'old' to 'hello'", "hello"},
		{"utf", "value changed 'old' to 'Ã©'", "é"},
		{"raw", "value changed 'old' to \"\\u{0}\\u{1}\\u{2}ÿ\"", "\u0000\u0001\u0002\ufffd"},
	} {
		ev, err := h.Evaluate("Vox_value["+c.title+"]", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.Events) != 1 || ev.Events[0].Message != c.message || ev.Events[0].DesiredValue != c.desired {
			t.Errorf("%s events = %+v, want message %q and desired_value %q", c.title, ev.Events, c.message, c.desired)
		}
	}
}

// Every rich value other than Binary and a top-level Sensitive reaches the
// type as Puppet's own object, as it does under puppet apply.
func TestRealHostKeepsOtherRichValuesForPuppet(t *testing.T) {
	h, load, _ := launchWith(t, seededValues(t, map[string]any{
		"ts": richValue("Timestamp", "2020-01-02T03:04:05.000000000 UTC"),
	}))
	if len(load.Invalid) != 0 {
		t.Fatalf("Invalid = %v", load.Invalid)
	}
	ev, err := h.Evaluate("Vox_value[ts]", false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "value changed 'old' to 2020-01-02T03:04:05.000000000 UTC"; len(ev.Events) != 1 || ev.Events[0].Message != want {
		t.Errorf("events = %+v, want message %q", ev.Events, want)
	}
}

func debugLines(logs []extension.Log) []string {
	var out []string
	for _, l := range logs {
		if l.Level == "debug" {
			out = append(out, l.Message)
		}
	}
	return out
}

// Load passes the run's level to Puppet: at debug the provider's debug line
// comes back as a log entry of level debug, at notice it does not.
func TestRealHostLogLevelSurfacesProviderDebugLines(t *testing.T) {
	const want = "vox_value provider wrote the value"
	for _, tc := range []struct {
		level string
		seen  bool
	}{{"notice", false}, {"info", false}, {"debug", true}} {
		t.Run(tc.level, func(t *testing.T) {
			h, _, _ := launchAt(t, tc.level, fixture)
			ev, err := h.Evaluate("Vox_value[a]", false)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(debugLines(ev.Logs), want); got != tc.seen {
				t.Errorf("debug lines %q hold %q = %v, want %v", debugLines(ev.Logs), want, got, tc.seen)
			}
		})
	}
}

// host.rb accepts notice, info and debug and refuses any other level with an
// error reply, leaving the host usable.
func TestRealHostRefusesOtherLogLevels(t *testing.T) {
	ruby := extensiontest.Ruby(t)
	for _, level := range []string{"", "warning", "DEBUG", "err", "bogus"} {
		t.Run("level="+level, func(t *testing.T) {
			h, err := extension.Launch(extension.Config{Ruby: ruby,
				Timeouts: extension.Timeouts{Evaluate: time.Minute, Other: time.Minute}}, os.Environ())
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			t.Cleanup(func() { _ = h.Close() })
			cat, _ := json.Marshal(res{"name": "n", "environment": "production", "catalog_format": 2,
				"tags": []string{}, "classes": []string{}, "resources": []res{}, "edges": []res{}})
			_, err = h.Load(cat, nil, extensiontest.Libdir(t), t.TempDir(), level)
			var ce *extension.CallError
			if !errors.As(err, &ce) || !strings.Contains(ce.Message, "log_level") {
				t.Fatalf("Load at %q = %v, want a CallError naming log_level", level, err)
			}
		})
	}
}
