package concat

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/ral"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
)

// concatRes is a catalog resource with params; marked names its Sensitive ones.
func concatRes(typ, title string, params map[string]any, marked ...string) catalog.Resource {
	if params == nil {
		params = map[string]any{}
	}
	return catalog.Resource{Type: typ, Title: title, Parameters: params, SensitiveParameters: marked}
}

// frag is a Concat_fragment as the compiler gives it: order an Integer.
func frag(title string, params map[string]any, marked ...string) catalog.Resource {
	return concatRes("Concat_fragment", title, params, marked...)
}

// evalGen runs c's eval_generate for the catalog's Concat_file titled title,
// with the walk's sources for it on the context.
func evalGen(t *testing.T, v ConcatVersion, cat *catalog.Catalog, title string) ral.EvalGeneration {
	t.Helper()
	i := slices.IndexFunc(cat.Resources, func(r catalog.Resource) bool { return r.Ref() == "Concat_file["+title+"]" })
	if i < 0 {
		t.Fatalf("no Concat_file[%s]", title)
	}
	ctx := sources.WithSources(context.Background(), sources.Sources{Resource: "/Stage[main]/Main/Concat_file[" + title + "]"})
	c := ConcatFile{Version: v, Catalog: cat}
	if !c.EvalGenerates(cat.Resources[i]) {
		t.Fatal("EvalGenerates = false")
	}
	g, err := c.EvalGenerate(ctx, cat.Resources[i])
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// contentOverride is the generation of content for File[path], nothing else.
func contentOverride(path string, params map[string]any, mark ral.ContentMark) ral.EvalGeneration {
	return ral.EvalGeneration{
		Existing:  []string{"File[" + path + "]"},
		Overrides: []ral.Override{{Ref: "File[" + path + "]", Parameters: params, ContentMark: mark}},
	}
}

func TestConcatEvalGenerateBasic(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "/etc/c01.conf", nil),
		concatRes("File", "/etc/c01.conf", map[string]any{"ensure": "file", "path": "/etc/c01.conf"}),
		frag("c", map[string]any{"target": "/etc/c01.conf", "content": "three\n"}),
		frag("a", map[string]any{"target": "/etc/c01.conf", "content": "one\n"}),
		frag("b", map[string]any{"target": "/etc/c01.conf", "content": "two\n"}),
	}}
	got := evalGen(t, Concat910, cat, "/etc/c01.conf")
	want := contentOverride("/etc/c01.conf", map[string]any{"content": "one\ntwo\nthree\n"}, ral.ContentMarkKeep)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", got, want)
	}
}

// A declared File titled otherwise is found by its path, and named as the
// catalog names it.
func TestConcatEvalGenerateDeclaredFileByPath(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("File", "conf", map[string]any{"path": "/etc/c14.conf"}),
		concatRes("Concat_file", "/etc/c14.conf", nil),
		frag("f", map[string]any{"target": "/etc/c14.conf", "content": "x\n"}),
	}}
	got := evalGen(t, Concat910, cat, "/etc/c14.conf")
	want := ral.EvalGeneration{Existing: []string{"File[conf]"},
		Overrides: []ral.Override{{Ref: "File[conf]", Parameters: map[string]any{"content": "x\n"}}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", got, want)
	}
}

func TestConcatEvalGenerateTargeting(t *testing.T) {
	// M8: by path, by title, by tag, and a missed target with a matching tag.
	m8 := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "title08", map[string]any{"path": "/etc/c08.conf", "tag": "grp08"}),
		concatRes("File", "/etc/c08.conf", nil),
		frag("by-path", map[string]any{"target": "/etc/c08.conf", "order": 1.0, "content": "by-path\n"}),
		frag("by-title", map[string]any{"target": "title08", "order": 2.0, "content": "by-title\n"}),
		frag("by-tag", map[string]any{"tag": "grp08", "order": 3.0, "content": "by-tag\n"}),
		frag("target-and-tag", map[string]any{"target": "nowhere", "tag": "grp08", "order": 4.0, "content": "target-and-tag\n"}),
		frag("orphan-target", map[string]any{"target": "/etc/c08-nope.conf", "content": "x\n"}),
		frag("orphan-tag", map[string]any{"tag": "no-such-tag", "content": "x\n"}),
		frag("orphan-both", map[string]any{"target": "/etc/c08-nope2.conf", "tag": "no-such-tag2", "content": "x\n"}),
	}}
	for _, v := range []ConcatVersion{Concat910, Concat1001} {
		got := evalGen(t, v, m8, "title08").Overrides[0].Parameters["content"]
		if got != "by-path\nby-title\nby-tag\ntarget-and-tag\n" {
			t.Errorf("version %d: M8 content = %q", v, got)
		}
	}

	// v10-08: a list tag collects each listed tag on 10.0.1 only.
	list := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "/etc/c08l.conf", map[string]any{"tag": []any{"a", "b"}}),
		concatRes("File", "/etc/c08l.conf", nil),
		frag("frag-a", map[string]any{"tag": "a", "order": 1.0, "content": "from-a\n"}),
		frag("frag-b", map[string]any{"tag": "b", "order": 2.0, "content": "from-b\n"}),
		frag("frag-c", map[string]any{"tag": "c", "order": 3.0, "content": "from-c-not-listed\n"}),
		frag("frag-tgt", map[string]any{"target": "/etc/c08l.conf", "order": 4.0, "content": "from-target\n"}),
		frag("frag-both", map[string]any{"target": "nowhere", "tag": "b", "order": 5.0, "content": "from-target-miss-tag-b\n"}),
		frag("frag-list", map[string]any{"tag": []any{"a", "b"}, "order": 6.0, "content": "from-equal-list\n"}),
	}}
	if got := evalGen(t, Concat1001, list, "/etc/c08l.conf").Overrides[0].Parameters["content"]; got != "from-a\nfrom-b\nfrom-target\nfrom-target-miss-tag-b\nfrom-equal-list\n" {
		t.Errorf("10.0.1 list tag content = %q", got)
	}
	if got := evalGen(t, Concat910, list, "/etc/c08l.conf").Overrides[0].Parameters["content"]; got != "from-target\nfrom-equal-list\n" {
		t.Errorf("9.1.0 list tag content = %q", got)
	}

	// M22: a false tag never matches, even another false tag.
	m22 := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "/etc/c22b.conf", map[string]any{"tag": false}),
		concatRes("File", "/etc/c22b.conf", nil),
		frag("false-tag", map[string]any{"target": "/etc/c22-nope.conf", "tag": false, "content": "f\n"}),
	}}
	for _, v := range []ConcatVersion{Concat910, Concat1001} {
		if got := evalGen(t, v, m22, "/etc/c22b.conf").Overrides[0].Parameters["content"]; got != "" {
			t.Errorf("version %d: M22 content = %q", v, got)
		}
	}
}

func TestConcatEvalGenerateCreateEmptyFalse(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "/etc/c07-false.conf", map[string]any{"create_empty_file": false}),
		concatRes("File", "/etc/c07-false.conf", nil),
		concatRes("Concat_file", "/etc/c07-emptyfrag.conf", map[string]any{"create_empty_file": "no"}),
		concatRes("File", "/etc/c07-emptyfrag.conf", nil),
		frag("e", map[string]any{"target": "/etc/c07-emptyfrag.conf", "content": ""}),
		concatRes("Concat_file", "/etc/c07-true.conf", nil),
		concatRes("File", "/etc/c07-true.conf", nil),
	}}
	for _, path := range []string{"/etc/c07-false.conf", "/etc/c07-emptyfrag.conf"} {
		got := evalGen(t, Concat910, cat, path)
		want := contentOverride(path, map[string]any{"content": "", "ensure": "absent"}, ral.ContentMarkKeep)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: generation =\n%#v\nwant\n%#v", path, got, want)
		}
	}
	got := evalGen(t, Concat910, cat, "/etc/c07-true.conf")
	if want := contentOverride("/etc/c07-true.conf", map[string]any{"content": ""}, ral.ContentMarkKeep); !reflect.DeepEqual(got, want) {
		t.Errorf("create_empty_file true: generation =\n%#v\nwant\n%#v", got, want)
	}
}

func TestConcatEvalGenerateContentMarkV10(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "/etc/c12.conf", nil),
		concatRes("File", "/etc/c12.conf", nil),
		frag("plain", map[string]any{"target": "/etc/c12.conf", "order": 1.0, "content": "public\n"}),
		frag("secret", map[string]any{"target": "/etc/c12.conf", "order": 2.0, "content": "s3cret-token\n"}, "content"),
		concatRes("Concat_file", "/etc/c12plain.conf", nil),
		concatRes("File", "/etc/c12plain.conf", nil),
		frag("plain2", map[string]any{"target": "/etc/c12plain.conf", "content": "public\n"}),
		concatRes("Concat_file", "/etc/c07.conf", nil),
		concatRes("File", "/etc/c07.conf", nil),
	}}
	for _, tc := range []struct {
		path string
		v    ConcatVersion
		want ral.ContentMark
	}{
		{"/etc/c12.conf", Concat1001, ral.ContentMarkSensitive},
		{"/etc/c07.conf", Concat1001, ral.ContentMarkNotSensitive},
		{"/etc/c12plain.conf", Concat1001, ral.ContentMarkUnset},
		{"/etc/c12.conf", Concat910, ral.ContentMarkKeep},
		{"/etc/c07.conf", Concat910, ral.ContentMarkKeep},
		{"/etc/c12plain.conf", Concat910, ral.ContentMarkKeep},
	} {
		o := evalGen(t, tc.v, cat, tc.path).Overrides[0]
		if o.ContentMark != tc.want || o.Sensitive != nil {
			t.Errorf("%s on %d: mark = %d sensitive = %v, want %d and none", tc.path, tc.v, o.ContentMark, o.Sensitive, tc.want)
		}
		// D-78 under both versions: a validator can echo a marked fragment,
		// whether or not the version marks the content.
		if want := tc.path == "/etc/c12.conf"; o.RedactOutput != want {
			t.Errorf("%s on %d: RedactOutput = %v, want %v", tc.path, tc.v, o.RedactOutput, want)
		}
	}
	if got := evalGen(t, Concat1001, cat, "/etc/c12.conf").Overrides[0].Parameters["content"]; got != "public\ns3cret-token\n" {
		t.Errorf("marked content = %q", got)
	}
}

// failedGeneration is a raising eval_generate: one err line at the
// Concat_file's path, file and line, and nothing else.
func failedGeneration(path, detail string) ral.EvalGeneration {
	return ral.EvalGeneration{Failed: true, Logs: []report.LogEntry{{
		Level: "err", Source: "/Stage[main]/Main/Concat_file[" + path + "]",
		Message: "Failed to generate additional resources using 'eval_generate': " + detail,
		File:    "/tmp/e/production/manifests/site.pp", Line: 3,
	}}}
}

func TestConcatEvalGenerateMixedOrderFails(t *testing.T) {
	cf := concatRes("Concat_file", "/etc/c03c.conf", nil)
	cf.File, cf.Line = "/tmp/e/production/manifests/site.pp", 3
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		cf,
		concatRes("File", "/etc/c03c.conf", nil),
		frag("a", map[string]any{"target": "/etc/c03c.conf", "order": 10.0, "content": "a\n"}),
		frag("5", map[string]any{"target": "/etc/c03c.conf", "order": 10.0, "content": "5\n"}),
	}}
	got := evalGen(t, Concat910, cat, "/etc/c03c.conf")
	if want := failedGeneration("/etc/c03c.conf", "comparison of Array with Array failed"); !reflect.DeepEqual(got, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", got, want)
	}
}

func TestConcatEvalGenerateMissingFile(t *testing.T) {
	cf := concatRes("Concat_file", "/etc/c20c.conf", nil)
	cf.File, cf.Line = "/tmp/e/production/manifests/site.pp", 3
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		cf,
		concatRes("File", "/etc/other.conf", nil),
		frag("f", map[string]any{"target": "/etc/c20c.conf", "content": "c\n"}),
	}}
	got := evalGen(t, Concat1001, cat, "/etc/c20c.conf")
	if want := failedGeneration("/etc/c20c.conf", "undefined method `[]=' for nil:NilClass"); !reflect.DeepEqual(got, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", got, want)
	}
}

func TestConcatOrphanWarnings(t *testing.T) {
	const prefix = "Target Concat_file with path or title "
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		concatRes("Concat_file", "title08", map[string]any{"path": "/etc/c08.conf", "tag": "grp08"}),
		concatRes("Concat_file", "/etc/c08l.conf", map[string]any{"tag": []any{"a", "b"}}),
		concatRes("Concat_file", "/etc/c22b.conf", map[string]any{"tag": false}),
		frag("by-path", map[string]any{"target": "/etc/c08.conf", "content": "x\n"}),
		frag("by-title", map[string]any{"target": "title08", "content": "x\n"}),
		frag("by-tag", map[string]any{"tag": "grp08", "content": "x\n"}),
		frag("target-and-tag", map[string]any{"target": "nowhere", "tag": "grp08", "content": "x\n"}),
		frag("orphan-target", map[string]any{"target": "/etc/c08-nope.conf", "content": "x\n"}),
		frag("orphan-tag", map[string]any{"tag": "no-such-tag", "content": "x\n"}),
		frag("orphan-both", map[string]any{"target": "/etc/c08-nope2.conf", "tag": "no-such-tag2", "content": "x\n"}),
		frag("false-tag", map[string]any{"target": "/etc/c22-nope.conf", "tag": false, "content": "f\n"}),
		frag("list-tag", map[string]any{"target": "/etc/c-nope.conf", "tag": []any{"c"}, "content": "x\n"}),
		frag("equal-list", map[string]any{"tag": []any{"a", "b"}, "content": "x\n"}),
		frag("frag-both", map[string]any{"target": "nowhere", "tag": "b", "content": "x\n"}),
		frag("marked", map[string]any{"target": "/etc/secret.conf", "content": "x\n"}, "target"),
		frag("marked-tag", map[string]any{"tag": "hidden", "content": "x\n"}, "tag"),
	}}
	want := map[string]map[ConcatVersion][]string{
		"orphan-target": {Concat910: {prefix + "'/etc/c08-nope.conf' not found in the catalog"}},
		"orphan-tag":    {Concat910: {prefix + "'' or tag 'no-such-tag not found in the catalog"}},
		"orphan-both":   {Concat910: {prefix + "'/etc/c08-nope2.conf' or tag 'no-such-tag2 not found in the catalog"}},
		"false-tag":     {Concat910: {prefix + "'/etc/c22-nope.conf' not found in the catalog"}},
		"list-tag":      {Concat910: {prefix + `'/etc/c-nope.conf' or tag '["c"] not found in the catalog`}},
		"frag-both": {
			Concat910:  {prefix + "'nowhere' or tag 'b not found in the catalog"},
			Concat1001: nil,
		},
		"marked":     {Concat910: {prefix + "'[redacted]' not found in the catalog"}},
		"marked-tag": {Concat910: {prefix + "'' or tag '[redacted] not found in the catalog"}},
	}
	for _, v := range []ConcatVersion{Concat910, Concat1001} {
		for _, r := range cat.Resources {
			if r.Type != "Concat_fragment" {
				continue
			}
			var expect []string
			if byVersion, ok := want[r.Title]; ok {
				expect = byVersion[Concat910]
				if w, own := byVersion[v]; own {
					expect = w
				}
			}
			if got := (ConcatFragment{Version: v}).GraphWarnings(r, cat); !slices.Equal(got, expect) {
				t.Errorf("version %d, %s: warnings = %q, want %q", v, r.Title, got, expect)
			}
		}
	}
}

func TestConcatAutoRequiresItsFile(t *testing.T) {
	managed := func(ref string) bool { return ref == "File[/etc/c08.conf]" }
	if got := (ConcatFile{}).AutoRequires(concatRes("Concat_file", "title08", map[string]any{"path": "/etc/c08.conf"}), managed); !slices.Equal(got, []string{"File[/etc/c08.conf]"}) {
		t.Errorf("by path: autorequires = %v", got)
	}
	if got := (ConcatFile{}).AutoRequires(concatRes("Concat_file", "/etc/c01.conf", nil), managed); got != nil {
		t.Errorf("unmanaged: autorequires = %v, want none", got)
	}
	if got := (ConcatFragment{}).AutoRequires(frag("f", map[string]any{"target": "/etc/c08.conf"}), managed); got != nil {
		t.Errorf("fragment autorequires = %v, want none", got)
	}
}
