package unit

import (
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func TestResourcesPurge(t *testing.T) {
	res := func(typ, title string, v any) catalog.Resource {
		params := map[string]any{}
		if v != nil {
			params["purge"] = v
		}
		return catalog.Resource{Type: typ, Title: title, Parameters: params}
	}
	cases := []struct {
		name string
		res  catalog.Resource
		typ  string
		want bool
	}{
		{"true", res("Resources", "cron", true), "cron", true},
		{"string true", res("Resources", "cron", "true"), "cron", true},
		{"string yes", res("Resources", "cron", "yes"), "cron", true},
		{"string TRUE", res("Resources", "cron", "TRUE"), "cron", true},
		{"string Yes", res("Resources", "cron", "Yes"), "cron", true},
		{"other value", res("Resources", "cron", 1), "cron", true},
		{"title Cron", res("Resources", "Cron", true), "cron", true},
		{"type resources", res("resources", "cron", true), "cron", true},
		{"named by name", catalog.Resource{Type: "Resources", Title: "x", Parameters: map[string]any{"name": "cron", "purge": true}}, "cron", true},
		{"name overrides title", catalog.Resource{Type: "Resources", Title: "cron", Parameters: map[string]any{"name": "host", "purge": true}}, "cron", false},
		{"false", res("Resources", "cron", false), "cron", false},
		{"string false", res("Resources", "cron", "false"), "cron", false},
		{"absent", res("Resources", "cron", nil), "cron", false},
		{"another type's Resources", res("Resources", "user", true), "cron", false},
		{"host asked, cron purges", res("Resources", "cron", true), "host", false},
		{"host purges", res("Resources", "host", true), "host", true},
		{"not a Resources resource", res("Cron", "cron", true), "cron", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResourcesPurge(&catalog.Catalog{Resources: []catalog.Resource{c.res}}, c.typ); got != c.want {
				t.Fatalf("ResourcesPurge(%s) = %t, want %t", c.typ, got, c.want)
			}
		})
	}
}
