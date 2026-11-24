package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
)

func TestMergeGeneration(t *testing.T) {
	base := func() *catalog.Catalog {
		return &catalog.Catalog{Resources: []catalog.Resource{
			{Type: "Class", Title: "main", Parameters: map[string]any{"before": "Stage[x]"}},
			{Type: "File", Title: "/a", Parameters: map[string]any{"content": "old", "before": []any{"File[/b]"}}},
			{Type: "File", Title: "/b"},
		}}
	}
	tests := []struct {
		name    string
		gen     extension.Generation
		check   func(t *testing.T, c *catalog.Catalog)
		wantErr string
	}{
		{
			name: "new resource converts json.Number to float64 recursively",
			gen: extension.Generation{Resources: []extension.GeneratedResource{{
				Type: "File", Title: "/new", Tags: []string{"file"}, Kind: "compilable_type", Exported: true,
				Parameters: map[string]any{"mode": json.Number("420"),
					"l": []any{json.Number("1")}, "m": map[string]any{"k": json.Number("2.5")}},
			}}},
			check: func(t *testing.T, c *catalog.Catalog) {
				got := c.Resources[len(c.Resources)-1]
				want := catalog.Resource{Type: "File", Title: "/new", Tags: []string{"file"}, Kind: "compilable_type",
					Exported: true, Parameters: map[string]any{"mode": float64(420),
						"l": []any{float64(1)}, "m": map[string]any{"k": 2.5}}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("resource = %#v, want %#v", got, want)
				}
			},
		},
		{
			name: "container edge",
			gen:  extension.Generation{Containers: map[string]string{"File[/b]": "Class[main]"}},
			check: func(t *testing.T, c *catalog.Catalog) {
				want := []catalog.Edge{{Source: "Class[main]", Target: "File[/b]"}}
				if !reflect.DeepEqual(c.Edges, want) {
					t.Errorf("edges = %v, want %v", c.Edges, want)
				}
			},
		},
		{
			name: "before on a resource without one",
			gen:  extension.Generation{Before: []extension.BeforeEdge{{Source: "File[/b]", Target: "File[/a]"}}},
			check: func(t *testing.T, c *catalog.Catalog) {
				if got := c.Resources[2].Parameters["before"]; !reflect.DeepEqual(got, []any{"File[/a]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "before on a string before",
			gen:  extension.Generation{Before: []extension.BeforeEdge{{Source: "Class[main]", Target: "File[/a]"}}},
			check: func(t *testing.T, c *catalog.Catalog) {
				if got := c.Resources[0].Parameters["before"]; !reflect.DeepEqual(got, []any{"Stage[x]", "File[/a]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "before on a list before",
			gen:  extension.Generation{Before: []extension.BeforeEdge{{Source: "File[/a]", Target: "Class[main]"}}},
			check: func(t *testing.T, c *catalog.Catalog) {
				if got := c.Resources[1].Parameters["before"]; !reflect.DeepEqual(got, []any{"File[/b]", "Class[main]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "override replaces content",
			gen: extension.Generation{Overrides: []extension.Override{{Ref: "File[/a]",
				Parameters: map[string]any{"content": "new", "mode": json.Number("384")}, Sensitive: []string{"content"}}}},
			check: func(t *testing.T, c *catalog.Catalog) {
				p := c.Resources[1].Parameters
				if p["content"] != "new" || p["mode"] != float64(384) {
					t.Errorf("parameters = %#v", p)
				}
			},
		},
		{
			name: "an ASCII Binary from to_data_hash merges as its string",
			gen: extension.Generation{
				Resources: []extension.GeneratedResource{{Type: "File", Title: "/new", Kind: "compilable_type",
					Parameters: map[string]any{"content": map[string]any{"__ptype": "Binary", "__pvalue": "aGk="}}}},
				Overrides: []extension.Override{{Ref: "File[/a]",
					Parameters: map[string]any{"content": map[string]any{"__ptype": "Binary", "__pvalue": "aGk="}}}},
			},
			check: func(t *testing.T, c *catalog.Catalog) {
				if got := c.Resources[len(c.Resources)-1].Parameters["content"]; got != "hi" {
					t.Errorf("generated content = %#v, want \"hi\"", got)
				}
				if got := c.Resources[1].Parameters["content"]; got != "hi" {
					t.Errorf("overridden content = %#v, want \"hi\"", got)
				}
			},
		},
		{
			name: "new resource with invalid base64",
			gen: extension.Generation{Resources: []extension.GeneratedResource{{Type: "File", Title: "/new",
				Kind: "compilable_type", Parameters: map[string]any{"content": map[string]any{"__ptype": "Binary", "__pvalue": "aGk"}}}}},
			wantErr: "generated File[/new]: Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: invalid base64",
		},
		{
			name: "override with invalid base64",
			gen: extension.Generation{Overrides: []extension.Override{{Ref: "File[/a]",
				Parameters: map[string]any{"content": map[string]any{"__ptype": "Binary", "__pvalue": "aGk"}}}}},
			wantErr: "the override of File[/a]: Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: invalid base64",
		},
		{
			name:    "override of a missing ref",
			gen:     extension.Generation{Overrides: []extension.Override{{Ref: "File[/nope]", Parameters: map[string]any{"a": "b"}}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "before from a missing ref",
			gen:     extension.Generation{Before: []extension.BeforeEdge{{Source: "File[/nope]", Target: "File[/a]"}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "before to a missing ref",
			gen:     extension.Generation{Before: []extension.BeforeEdge{{Source: "File[/a]", Target: "File[/nope]"}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "container edge whose container is absent",
			gen:     extension.Generation{Containers: map[string]string{"File[/b]": "Class[nope]"}},
			wantErr: "Class[nope] is not in the catalog",
		},
		{
			name:    "container edge whose child is absent",
			gen:     extension.Generation{Containers: map[string]string{"File[/nope]": "Class[main]"}},
			wantErr: "File[/nope] is not in the catalog",
		},
		{
			name: "new resource with an unrepresentable number",
			gen: extension.Generation{Resources: []extension.GeneratedResource{{Type: "File", Title: "/new",
				Kind: "compilable_type", Parameters: map[string]any{"mode": json.Number("1e400")}}}},
			wantErr: "File[/new]",
		},
		{
			name: "override with an unrepresentable number",
			gen: extension.Generation{Overrides: []extension.Override{{Ref: "File[/a]",
				Parameters: map[string]any{"mode": json.Number("1e400")}}}},
			wantErr: "File[/a]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			err := mergeGeneration(c, tt.gen)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeGeneration: %v", err)
			}
			tt.check(t, c)
		})
	}
}

// A resource Puppet's generate made is marked generated, by the resource
// that made it, so its report status carries Ruby's null file and line
// (report.ResourceStatus.Generated); one the host names no generator for is
// not.
func TestMergeGenerationMarksGenerated(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Concat_file", Title: "/c"}}}
	gen := extension.Generation{
		Resources: []extension.GeneratedResource{
			{Type: "File", Title: "/c", Kind: "compilable_type"},
			{Type: "Notify", Title: "nested", Kind: "compilable_type"},
			{Type: "Notify", Title: "orphan", Kind: "compilable_type"},
		},
		Generators: map[string]string{"File[/c]": "Concat_file[/c]", "Notify[nested]": "File[/c]"},
	}
	if err := mergeGeneration(c, gen); err != nil {
		t.Fatal(err)
	}
	from := func(i int) string {
		if g := c.Resources[i].GeneratedFrom; g != nil {
			return g.Ref()
		}
		return ""
	}
	if got := []string{from(0), from(1), from(2), from(3)}; !reflect.DeepEqual(got,
		[]string{"", "Concat_file[/c]", "File[/c]", ""}) {
		t.Errorf("GeneratedFrom = %q", got)
	}
	if err := mergeGeneration(c, extension.Generation{Generators: map[string]string{"File[/c]": "Concat_file[/nope]"}}); err == nil ||
		!strings.Contains(err.Error(), "Concat_file[/nope]") {
		t.Errorf("a generator the catalog lacks: err = %v", err)
	}
}

// An override's Sensitive names join the target's marks once each, and an
// override that names none leaves the target's list alone.
func TestMergeGenerationKeepsSensitive(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "mode"
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/a", SensitiveParameters: shared},
		{Type: "File", Title: "/b"},
	}}
	gen := extension.Generation{Overrides: []extension.Override{
		{Ref: "File[/a]", Parameters: map[string]any{"content": "x"}, Sensitive: []string{"content", "content", "mode"}},
		{Ref: "File[/b]", Parameters: map[string]any{"content": "y"}},
	}}
	if err := mergeGeneration(c, gen); err != nil {
		t.Fatal(err)
	}
	if got := c.Resources[0].SensitiveParameters; !reflect.DeepEqual(got, []string{"mode", "content"}) {
		t.Errorf("File[/a] SensitiveParameters = %v, want [mode content]", got)
	}
	if shared[:cap(shared)][1] != "" {
		t.Error("the original slice's backing array was written")
	}
	if got := c.Resources[1].SensitiveParameters; got != nil {
		t.Errorf("File[/b] SensitiveParameters = %v, want nil", got)
	}
}

func TestApplyOrderReordersResources(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Notify", Title: "a"}, {Type: "Tidy", Title: "/t"}, {Type: "File", Title: "/t/x.old"},
	}}
	if err := applyOrder(c, []string{"Notify[a]", "File[/t/x.old]", "Tidy[/t]"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range c.Resources {
		got = append(got, r.Ref())
	}
	if want := []string{"Notify[a]", "File[/t/x.old]", "Tidy[/t]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Resources = %q, want %q", got, want)
	}
}

func TestApplyOrderRefusesAMismatch(t *testing.T) {
	for _, tt := range []struct {
		name  string
		order []string
		want  string
	}{
		{"missing", []string{"Notify[a]"}, "Notify[b]"},
		{"extra", []string{"Notify[a]", "Notify[b]", "Notify[c]"}, "Notify[c]"},
		{"repeat", []string{"Notify[a]", "Notify[a]"}, "Notify[a]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Notify", Title: "a"}, {Type: "Notify", Title: "b"}}}
			err := applyOrder(c, tt.order)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("applyOrder = %v, want an error naming %s", err, tt.want)
			}
			if c.Resources[0].Ref() != "Notify[a]" || c.Resources[1].Ref() != "Notify[b]" || len(c.Resources) != 2 {
				t.Errorf("a refused order changed the catalog: %v", c.Resources)
			}
		})
	}
}
