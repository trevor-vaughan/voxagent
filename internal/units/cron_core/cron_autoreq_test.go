package croncore

import (
	"context"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/ral"
)

func TestCronAutoRequires(t *testing.T) {
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	only := func(ref string) func(string) bool { return func(r string) bool { return r == ref } }
	tests := []struct {
		name    string
		params  map[string]any
		managed func(string) bool
		want    []string
	}{
		{"user managed", map[string]any{"user": "bin"}, all, []string{"User[bin]"}},
		{"user unmanaged", map[string]any{"user": "bin"}, none, nil},
		{"only the named user counts", map[string]any{"user": "bin"}, only("User[root]"), nil},
		{"no user", map[string]any{"command": "x"}, all, nil},
		{"target does not autorequire", map[string]any{"target": "bin"}, all, nil},
		{"list user is its first", map[string]any{"user": []any{"a", "b"}}, all, []string{"User[a]"}},
		{"non-string user is no edge", map[string]any{"user": float64(5)}, all, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cron{release: "2.1.0", encoding: "UTF-8"}.AutoRequires(cronRes(tt.params), tt.managed)
			if !slices.Equal(got, tt.want) {
				t.Errorf("AutoRequires = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCronIdentity(t *testing.T) {
	c := Cron{}
	if c.Type() != "cron" {
		t.Errorf("Type = %q", c.Type())
	}
	if p := c.ProviderUsed(); p == nil || *p != "crontab" {
		t.Errorf("ProviderUsed = %v, want crontab", p)
	}
	if refresh, err := c.Refresh(context.Background(), cronRes(nil), true); refresh || err != nil {
		t.Errorf("Refresh = %v, %v; want false, nil", refresh, err)
	}
	props := []string{"ensure", "command", "special", "minute", "hour", "weekday", "month", "monthday", "environment", "user", "target"}
	if want := ral.NewAttrs(props...); !equalAttrs(c.Properties(), want) {
		t.Errorf("Properties = %v, want %v", c.Properties().Sorted(), want.Sorted())
	}
	if want := ral.NewAttrs(append(props, "name", "provider")...); !equalAttrs(c.Attrs(), want) {
		t.Errorf("Attrs = %v, want %v", c.Attrs().Sorted(), want.Sorted())
	}
	if !slices.Equal(cronProperties, props) {
		t.Errorf("cronProperties = %v, want %v", cronProperties, props)
	}
	if want := []string{"minute", "hour", "weekday", "month", "monthday"}; !slices.Equal(cronTimeFields, want) {
		t.Errorf("cronTimeFields = %v, want %v", cronTimeFields, want)
	}
}

func equalAttrs(a, b ral.Attrs) bool { return slices.Equal(a.Sorted(), b.Sorted()) }
