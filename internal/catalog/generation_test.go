package catalog

import (
	"reflect"
	"strings"
	"testing"
)

func TestMergeGeneration(t *testing.T) {
	base := func() *Catalog {
		return &Catalog{Resources: []Resource{
			{Type: "Class", Title: "main", Parameters: map[string]any{"before": "Stage[x]"}},
			{Type: "File", Title: "/a", Parameters: map[string]any{"content": "old", "before": []any{"File[/b]"}}},
			{Type: "File", Title: "/b"},
		}}
	}
	tests := []struct {
		name    string
		gen     Generation
		check   func(t *testing.T, c *Catalog)
		wantErr string
	}{
		{
			name: "container edge",
			gen:  Generation{Containers: map[string]string{"File[/b]": "Class[main]"}},
			check: func(t *testing.T, c *Catalog) {
				want := []Edge{{Source: "Class[main]", Target: "File[/b]"}}
				if !reflect.DeepEqual(c.Edges, want) {
					t.Errorf("edges = %v, want %v", c.Edges, want)
				}
			},
		},
		{
			name: "before on a resource without one",
			gen:  Generation{Before: []Edge{{Source: "File[/b]", Target: "File[/a]"}}},
			check: func(t *testing.T, c *Catalog) {
				if got := c.Resources[2].Parameters["before"]; !reflect.DeepEqual(got, []any{"File[/a]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "before on a string before",
			gen:  Generation{Before: []Edge{{Source: "Class[main]", Target: "File[/a]"}}},
			check: func(t *testing.T, c *Catalog) {
				if got := c.Resources[0].Parameters["before"]; !reflect.DeepEqual(got, []any{"Stage[x]", "File[/a]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "before on a list before",
			gen:  Generation{Before: []Edge{{Source: "File[/a]", Target: "Class[main]"}}},
			check: func(t *testing.T, c *Catalog) {
				if got := c.Resources[1].Parameters["before"]; !reflect.DeepEqual(got, []any{"File[/b]", "Class[main]"}) {
					t.Errorf("before = %#v", got)
				}
			},
		},
		{
			name: "override replaces content",
			gen: Generation{Overrides: []GenerationOverride{{Ref: "File[/a]",
				Parameters: map[string]any{"content": "new", "mode": float64(384)}, Sensitive: []string{"content"}}}},
			check: func(t *testing.T, c *Catalog) {
				p := c.Resources[1].Parameters
				if p["content"] != "new" || p["mode"] != float64(384) {
					t.Errorf("parameters = %#v", p)
				}
			},
		},
		{
			name:    "override of a missing ref",
			gen:     Generation{Overrides: []GenerationOverride{{Ref: "File[/nope]", Parameters: map[string]any{"a": "b"}}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "before from a missing ref",
			gen:     Generation{Before: []Edge{{Source: "File[/nope]", Target: "File[/a]"}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "before to a missing ref",
			gen:     Generation{Before: []Edge{{Source: "File[/a]", Target: "File[/nope]"}}},
			wantErr: "File[/nope]",
		},
		{
			name:    "container edge whose container is absent",
			gen:     Generation{Containers: map[string]string{"File[/b]": "Class[nope]"}},
			wantErr: "Class[nope] is not in the catalog",
		},
		{
			name:    "container edge whose child is absent",
			gen:     Generation{Containers: map[string]string{"File[/nope]": "Class[main]"}},
			wantErr: "File[/nope] is not in the catalog",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			err := MergeGeneration(c, tt.gen, PlaceAppend)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("MergeGeneration: %v", err)
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
	c := &Catalog{Resources: []Resource{{Type: "Concat_file", Title: "/c"}}}
	gen := Generation{
		Resources: []Resource{
			{Type: "File", Title: "/c", Kind: "compilable_type"},
			{Type: "Notify", Title: "nested", Kind: "compilable_type"},
			{Type: "Notify", Title: "orphan", Kind: "compilable_type"},
		},
		Generators: map[string]string{"File[/c]": "Concat_file[/c]", "Notify[nested]": "File[/c]"},
	}
	if err := MergeGeneration(c, gen, PlaceAppend); err != nil {
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
	if err := MergeGeneration(c, Generation{Generators: map[string]string{"File[/c]": "Concat_file[/nope]"}}, PlaceAppend); err == nil ||
		!strings.Contains(err.Error(), "Concat_file[/nope]") {
		t.Errorf("a generator the catalog lacks: err = %v", err)
	}
}

// An override's Sensitive names join the target's marks once each, and an
// override that names none leaves the target's list alone.
func TestMergeGenerationKeepsSensitive(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "mode"
	c := &Catalog{Resources: []Resource{
		{Type: "File", Title: "/a", SensitiveParameters: shared},
		{Type: "File", Title: "/b"},
	}}
	gen := Generation{Overrides: []GenerationOverride{
		{Ref: "File[/a]", Parameters: map[string]any{"content": "x"}, Sensitive: []string{"content", "content", "mode"}},
		{Ref: "File[/b]", Parameters: map[string]any{"content": "y"}},
	}}
	if err := MergeGeneration(c, gen, PlaceAppend); err != nil {
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
	c := &Catalog{Resources: []Resource{
		{Type: "Notify", Title: "a"}, {Type: "Tidy", Title: "/t"}, {Type: "File", Title: "/t/x.old"},
	}}
	if err := ApplyOrder(c, []string{"Notify[a]", "File[/t/x.old]", "Tidy[/t]"}); err != nil {
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
			c := &Catalog{Resources: []Resource{{Type: "Notify", Title: "a"}, {Type: "Notify", Title: "b"}}}
			err := ApplyOrder(c, tt.order)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("applyOrder = %v, want an error naming %s", err, tt.want)
			}
			if c.Resources[0].Ref() != "Notify[a]" || c.Resources[1].Ref() != "Notify[b]" || len(c.Resources) != 2 {
				t.Errorf("a refused order changed the catalog: %v", c.Resources)
			}
		})
	}
}

// PlaceAfterGenerator puts each new resource right after its generator, in
// the order the generation returned them, and a resource generated by a new
// one follows that one.
func TestMergeGenerationPlacesAfterGenerator(t *testing.T) {
	refs := func(c *Catalog) []string {
		var got []string
		for _, r := range c.Resources {
			got = append(got, r.Ref())
		}
		return got
	}
	base := func() *Catalog {
		return &Catalog{Resources: []Resource{
			{Type: "Class", Title: "Main"},
			{Type: "Concat_file", Title: "/etc/c01.conf"},
			{Type: "Concat_fragment", Title: "a"},
		}}
	}
	tests := []struct {
		name string
		gen  Generation
		want []string
	}{
		{
			name: "one new resource follows its generator",
			gen: Generation{
				Resources:  []Resource{{Type: "File", Title: "/etc/c01.conf"}},
				Generators: map[string]string{"File[/etc/c01.conf]": "Concat_file[/etc/c01.conf]"},
			},
			want: []string{"Class[Main]", "Concat_file[/etc/c01.conf]", "File[/etc/c01.conf]", "Concat_fragment[a]"},
		},
		{
			name: "two from one generator keep returned order",
			gen: Generation{
				Resources: []Resource{{Type: "File", Title: "/z"}, {Type: "File", Title: "/a"}},
				Generators: map[string]string{
					"File[/z]": "Concat_file[/etc/c01.conf]", "File[/a]": "Concat_file[/etc/c01.conf]"},
			},
			want: []string{"Class[Main]", "Concat_file[/etc/c01.conf]", "File[/z]", "File[/a]", "Concat_fragment[a]"},
		},
		{
			name: "one generated by a new resource follows it",
			gen: Generation{
				Resources: []Resource{{Type: "Notify", Title: "n"}, {Type: "File", Title: "/f"}},
				Generators: map[string]string{
					"File[/f]": "Concat_file[/etc/c01.conf]", "Notify[n]": "File[/f]"},
			},
			want: []string{"Class[Main]", "Concat_file[/etc/c01.conf]", "File[/f]", "Notify[n]", "Concat_fragment[a]"},
		},
		{
			name: "one with no generator goes last",
			gen:  Generation{Resources: []Resource{{Type: "Notify", Title: "orphan"}}},
			want: []string{"Class[Main]", "Concat_file[/etc/c01.conf]", "Concat_fragment[a]", "Notify[orphan]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			if err := MergeGeneration(c, tt.gen, PlaceAfterGenerator); err != nil {
				t.Fatal(err)
			}
			if got := refs(c); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("order = %q, want %q", got, tt.want)
			}
		})
	}
}

// A generator a new resource names must be in the catalog, as for PlaceAppend.
func TestMergeGenerationPlacementRefusesAMissingGenerator(t *testing.T) {
	c := &Catalog{Resources: []Resource{{Type: "Class", Title: "Main"}}}
	gen := Generation{
		Resources:  []Resource{{Type: "File", Title: "/f"}},
		Generators: map[string]string{"File[/f]": "Concat_file[/nope]"},
	}
	err := MergeGeneration(c, gen, PlaceAfterGenerator)
	if err == nil || !strings.Contains(err.Error(), "Concat_file[/nope]") {
		t.Fatalf("err = %v, want one naming Concat_file[/nope]", err)
	}
}
