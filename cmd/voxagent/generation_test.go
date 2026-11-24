package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
)

func TestConvertGeneration(t *testing.T) {
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
			gen, err := convertGeneration(tt.gen)
			if err == nil {
				err = catalog.MergeGeneration(c, gen, catalog.PlaceAppend)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("convert and merge: %v", err)
			}
			tt.check(t, c)
		})
	}
}
