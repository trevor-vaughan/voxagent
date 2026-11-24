package concat

import (
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/units/file"

	"github.com/voxagent/voxagent/internal/catalog"
)

// simpSudoers is SIMP's Concat_file[/etc/sudoers] and its container, as
// .test-output/ranking/simp.json gives them.
func simpSudoers() (*catalog.Catalog, catalog.Resource) {
	cf := catalog.Resource{
		Type:  "Concat_file",
		Title: "/etc/sudoers",
		Tags:  []string{"_etc_sudoers", "concat_file", "concat", "class", "sudo", "simp::sudoers", "simp", "sudoers", "simp::admin", "admin"},
		File:  "/tmp/env/production/modules/concat/manifests/init.pp",
		Line:  126,
		Kind:  "compilable_type",
		Parameters: map[string]any{
			"tag": "_etc_sudoers", "owner": "root", "group": "root", "mode": "0440",
			"replace": true, "backup": "puppet", "show_diff": true, "order": "alpha",
			"ensure_newline": false, "validate_cmd": "/usr/sbin/visudo -q -c -f %",
			"format": "plain", "force": false, "create_empty_file": true,
		},
	}
	cat := &catalog.Catalog{
		Resources: []catalog.Resource{
			{Type: "Class", Title: "Sudo"},
			{Type: "Concat", Title: "/etc/sudoers"},
			cf,
		},
		Edges: []catalog.Edge{
			{Source: "Class[Sudo]", Target: "Concat[/etc/sudoers]"},
			{Source: "Concat[/etc/sudoers]", Target: "Concat_file[/etc/sudoers]"},
		},
	}
	return cat, cf
}

func TestConcatGenerateNewFile(t *testing.T) {
	cat, cf := simpSudoers()
	g, err := ConcatFile{}.LoadGenerate(cat, cf)
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.Generation{
		Resources: []catalog.Resource{{
			Type:  "File",
			Title: "/etc/sudoers",
			Tags:  []string{"file", "_etc_sudoers", "concat_file", "concat", "class", "sudo", "simp::sudoers", "simp", "sudoers", "simp::admin", "admin"},
			Parameters: map[string]any{
				"ensure": "file", "owner": "root", "group": "root",
				"mode": "0440", "replace": true, "backup": "puppet",
				"validate_cmd": "/usr/sbin/visudo -q -c -f %", "show_diff": true, "loglevel": "notice",
			},
		}},
		Containers: map[string]string{"File[/etc/sudoers]": "Concat[/etc/sudoers]"},
		Generators: map[string]string{"File[/etc/sudoers]": "Concat_file[/etc/sudoers]"},
		Before:     []catalog.Edge{{Source: "Concat_file[/etc/sudoers]", Target: "File[/etc/sudoers]"}},
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("generation =\n%#v\nwant\n%#v", g, want)
	}
	if cat.Resources[2].Parameters["before"] != nil || len(cat.Resources) != 3 {
		t.Errorf("LoadGenerate modified the catalog: %#v", cat.Resources)
	}
	// loadGenerate judges the new File as a catalog's.
	fresh := &catalog.Catalog{Resources: g.Resources}
	if gaps, invalid := ral.Preflight(ral.NewRegistry(file.File{}), fresh), ral.Validate(ral.NewRegistry(file.File{}), fresh); len(gaps)+len(invalid) != 0 {
		t.Errorf("generated File refused: gaps %v, invalid %v", gaps, invalid)
	}

	// The generation merges: the File follows its Concat_file, which is before it.
	if err := catalog.MergeGeneration(cat, g, catalog.PlaceAfterGenerator); err != nil {
		t.Fatal(err)
	}
	if got := cat.Resources[3].Ref(); got != "File[/etc/sudoers]" {
		t.Errorf("resource after Concat_file = %s", got)
	}
	if got := cat.Resources[2].Parameters["before"]; !reflect.DeepEqual(got, []any{"File[/etc/sudoers]"}) {
		t.Errorf("Concat_file before = %#v", got)
	}

	cases := []struct {
		name   string
		params map[string]any
		check  func(p map[string]any) bool
	}{
		{"ensure absent", map[string]any{"ensure": "absent"}, func(p map[string]any) bool { return p["ensure"] == "absent" }},
		{"ensure present", map[string]any{"ensure": "present"}, func(p map[string]any) bool { return p["ensure"] == "file" }},
		{"replace yes", map[string]any{"replace": "yes"}, func(p map[string]any) bool { return p["replace"] == true }},
		{"replace default", nil, func(p map[string]any) bool {
			return reflect.DeepEqual(p, map[string]any{"ensure": "file", "replace": true, "loglevel": "notice"})
		}},
		{"selinux and never-copied", map[string]any{
			"selinux_ignore_defaults": "no", "selrange": "s0", "selrole": "object_r", "seltype": "etc_t", "seluser": "system_u",
			"tag": "t", "order": "alpha", "ensure_newline": true, "format": "plain", "force": true, "create_empty_file": false,
			"before": "Notify[n]", "notify": "Notify[n]", "require": "Notify[n]", "subscribe": "Notify[n]", "loglevel": "notice",
		}, func(p map[string]any) bool {
			return reflect.DeepEqual(p, map[string]any{
				"ensure": "file", "replace": true, "loglevel": "notice",
				"selinux_ignore_defaults": false, "selrange": "s0", "selrole": "object_r", "seltype": "etc_t", "seluser": "system_u",
			})
		}},
		{"backup false", map[string]any{"backup": false}, func(p map[string]any) bool { return p["backup"] == false }},
	}
	for _, c := range cases {
		cf := concatFileRes("/etc/c.conf", c.params)
		g, err := ConcatFile{}.LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(g.Resources) != 1 || !c.check(g.Resources[0].Parameters) {
			t.Errorf("%s: generated %#v", c.name, g.Resources)
		}
		if len(g.Containers) != 0 {
			t.Errorf("%s: an uncontained Concat_file's File is contained: %v", c.name, g.Containers)
		}
	}

	// A path parameter names the File; the title is not used.
	cf = concatFileRes("sudoers", map[string]any{"path": "/etc/s"})
	g, err = ConcatFile{}.LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Resources) != 1 || g.Resources[0].Ref() != "File[/etc/s]" || g.Resources[0].Parameters["path"] != nil ||
		!reflect.DeepEqual(g.Before, []catalog.Edge{{Source: "Concat_file[sudoers]", Target: "File[/etc/s]"}}) {
		t.Errorf("path parameter: %#v", g)
	}

	// A Concat_file tag "file" is not repeated (Puppet's TagSet).
	cf = catalog.Resource{Type: "Concat_file", Title: "/etc/t", Tags: []string{"concat_file", "file", "x"}}
	g, err = ConcatFile{}.LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"file", "concat_file", "x"}; !reflect.DeepEqual(g.Resources[0].Tags, want) {
		t.Errorf("tags = %v, want %v", g.Resources[0].Tags, want)
	}

	// An invalid Concat_file generates nothing.
	cf = concatFileRes("relative", nil)
	if _, err := (ConcatFile{}).LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf); err == nil {
		t.Error("a relative path generated a File")
	}
}

func TestConcatGenerateReusesDeclaredFile(t *testing.T) {
	// M14 a: the declared File keeps its parameters; only the order is added.
	declared := catalog.Resource{Type: "File", Title: "/etc/c14a.conf", Parameters: map[string]any{
		"ensure": "file", "content": "declared\n", "owner": "daemon", "mode": "0600"}}
	cf := concatFileRes("/etc/c14a.conf", map[string]any{"owner": "root", "mode": "0644"})
	cat := &catalog.Catalog{Resources: []catalog.Resource{declared, cf}}
	g, err := ConcatFile{}.LoadGenerate(cat, cf)
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.Generation{Before: []catalog.Edge{{Source: "Concat_file[/etc/c14a.conf]", Target: "File[/etc/c14a.conf]"}}}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("M14 a generation = %#v", g)
	}

	// A File titled otherwise whose path is the Concat_file's.
	aliased := catalog.Resource{Type: "File", Title: "x", Parameters: map[string]any{"path": "/etc/c14a.conf"}}
	cat = &catalog.Catalog{Resources: []catalog.Resource{aliased, cf}}
	g, err = ConcatFile{}.LoadGenerate(cat, cf)
	if err != nil {
		t.Fatal(err)
	}
	want = catalog.Generation{Before: []catalog.Edge{{Source: "Concat_file[/etc/c14a.conf]", Target: "File[x]"}}}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("aliased File generation = %#v", g)
	}

	// The File titled with the path wins over one whose path parameter matches.
	cat = &catalog.Catalog{Resources: []catalog.Resource{aliased, declared, cf}}
	if ref, ok := concatFileRef(cat, "/etc/c14a.conf"); !ok || ref != "File[/etc/c14a.conf]" {
		t.Errorf("concatFileRef = %q, %v", ref, ok)
	}
	if ref, ok := concatFileRef(cat, "/etc/other"); ok {
		t.Errorf("concatFileRef found %q for an unmanaged path", ref)
	}
	// Another type with the title is not a File.
	cat = &catalog.Catalog{Resources: []catalog.Resource{{Type: "Concat", Title: "/etc/c14a.conf"}, cf}}
	if ref, ok := concatFileRef(cat, "/etc/c14a.conf"); ok {
		t.Errorf("concatFileRef found %q", ref)
	}
}

func TestConcatGenerateAddsNoBeforeWhenAnEdgeExists(t *testing.T) {
	const (
		p    = "/etc/c21.conf"
		fRef = "File[/etc/c21.conf]"
		cRef = "Concat_file[/etc/c21.conf]"
	)
	for _, side := range []string{"concat_file", "file"} {
		for _, param := range []string{"before", "notify", "require", "subscribe"} {
			fileParams := map[string]any{"ensure": "file"}
			cfParams := map[string]any{}
			if side == "file" {
				fileParams[param] = cRef
			} else {
				cfParams[param] = fRef
			}
			cf := concatFileRes(p, cfParams)
			cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "File", Title: p, Parameters: fileParams}, cf}}
			g, err := ConcatFile{}.LoadGenerate(cat, cf)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(g, catalog.Generation{}) {
				t.Errorf("%s %s: generation = %#v", side, param, g)
			}
		}
	}

	// M21 a, b, c, with the relationships in arrays beside others.
	m21 := []struct {
		name           string
		fileRel, cfRel map[string]any
	}{
		{"a", map[string]any{"before": []any{"Notify[x]", cRef}}, map[string]any{"notify": "Exec[c21a-refresh]"}},
		{"b", nil, map[string]any{"require": []any{fRef}}},
		{"c", nil, map[string]any{"before": fRef, "notify": "Exec[c21c-refresh]"}},
	}
	for _, c := range m21 {
		cf := concatFileRes(p, c.cfRel)
		cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "File", Title: p, Parameters: c.fileRel}, cf}}
		g, err := ConcatFile{}.LoadGenerate(cat, cf)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(g, catalog.Generation{}) {
			t.Errorf("M21 %s: generation = %#v", c.name, g)
		}
	}

	// A relationship naming other resources is no edge between the two.
	cf := concatFileRes(p, map[string]any{"require": []any{"File[/etc]"}, "before": "Concat_file[/etc/other]"})
	cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "File", Title: p, Parameters: map[string]any{"notify": "Concat_file[/etc/other]"}}, cf}}
	g, err := ConcatFile{}.LoadGenerate(cat, cf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []catalog.Edge{{Source: cRef, Target: fRef}}; !reflect.DeepEqual(g.Before, want) {
		t.Errorf("unrelated relationships: Before = %#v", g.Before)
	}
}

// D-78: Ruby's generate drops the marks of the values it copies; voxagent
// carries those of owner, group and mode to a new File.
func TestConcatGenerateCarriesMarks(t *testing.T) {
	cf := concatFileRes("/etc/m.conf", map[string]any{"owner": "o", "group": "g", "mode": "0600", "show_diff": true},
		"owner", "show_diff", "ensure")
	g, err := ConcatFile{}.LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Resources) != 1 || !reflect.DeepEqual(g.Resources[0].SensitiveParameters, []string{"owner"}) {
		t.Errorf("new File marks = %#v", g.Resources)
	}

	// A mark on an attribute the Concat_file does not set carries nothing.
	cf = concatFileRes("/etc/m.conf", map[string]any{"group": "g"}, "owner", "mode")
	g, err = ConcatFile{}.LoadGenerate(&catalog.Catalog{Resources: []catalog.Resource{cf}}, cf)
	if err != nil {
		t.Fatal(err)
	}
	if g.Resources[0].SensitiveParameters != nil {
		t.Errorf("unset attributes marked: %v", g.Resources[0].SensitiveParameters)
	}

	// A declared File is not touched: Ruby discards the Concat_file's owner
	// and mode (M14), so no marked value reaches it.
	cf = concatFileRes("/etc/m.conf", map[string]any{"owner": "o", "mode": "0600"}, "owner", "mode")
	declared := catalog.Resource{Type: "File", Title: "/etc/m.conf",
		Parameters: map[string]any{"ensure": "file", "content": "c"}, SensitiveParameters: []string{"content"}}
	cat := &catalog.Catalog{Resources: []catalog.Resource{declared, cf}}
	g, err = ConcatFile{}.LoadGenerate(cat, cf)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Resources) != 0 || g.Overrides != nil {
		t.Errorf("declared File generation = %#v", g)
	}
}
