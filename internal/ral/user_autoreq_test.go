package ral

import (
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func autoUser(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "User", Title: "u17", Parameters: params}
}

func autoGroup(title string, gid any) catalog.Resource {
	p := map[string]any{}
	if gid != nil {
		p["gid"] = gid
	}
	return catalog.Resource{Type: "Group", Title: title, Parameters: p}
}

func TestUserAutoRequires(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		autoGroup("sshd", float64(74)), autoGroup("dup", "74"), autoGroup("nogid", nil), autoGroup("neg", "-5"),
	}}
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	only := func(refs ...string) func(string) bool {
		return func(ref string) bool { return slices.Contains(refs, ref) }
	}
	ns := fakeNSS{
		users:    map[string]passwdEntry{"u17": {Name: "u17", GID: 100}},
		groups:   []groupEntry{{Name: "adm", GID: 4, Members: []string{"u17"}}, {Name: "users", GID: 100}},
		grouplst: map[string][]int64{"u17": {100, 4}},
	}
	tests := []struct {
		name    string
		params  map[string]any
		managed func(string) bool
		want    []string
	}{
		{"gid by name", map[string]any{"gid": "g17"}, all, []string{"Group[g17]"}},
		{"gid by name unmanaged", map[string]any{"gid": "g17"}, none, nil},
		{"numeric gid integer", map[string]any{"gid": float64(74)}, all, []string{"Group[sshd]"}},
		{"numeric gid string, first catalog match", map[string]any{"gid": "74"}, all, []string{"Group[sshd]"}},
		{"numeric gid unmatched", map[string]any{"gid": "75"}, all, nil},
		{"negative gid string is a name", map[string]any{"gid": "-5"}, all, []string{"Group[-5]"}},
		{"numeric gid matched group unmanaged", map[string]any{"gid": float64(74)}, only(), nil},
		{"groups minimum adds current", map[string]any{"groups": []any{"g17b"}}, all, []string{"Group[adm]", "Group[g17b]"}},
		{"groups minimum only managed", map[string]any{"groups": []any{"g17b"}}, only("Group[g17b]"), []string{"Group[g17b]"}},
		{"groups inclusive ignores current", map[string]any{"groups": []any{"g17b"}, "membership": "inclusive"}, all, []string{"Group[g17b]"}},
		{"gid then groups, deduplicated", map[string]any{"gid": "adm", "groups": []any{"adm", "g17b"}}, all, []string{"Group[adm]", "Group[g17b]"}},
		{"no gid no groups", map[string]any{}, all, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := User{ns: ns, CatalogGroups: GroupGIDs(cat)}.AutoRequires(autoUser(tt.params), tt.managed)
			if !slices.Equal(got, tt.want) {
				t.Errorf("AutoRequires = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUserAutoRequiresMissingUserUsesGroupScan(t *testing.T) {
	ns := fakeNSS{groups: []groupEntry{{Name: "adm", GID: 4, Members: []string{"u17"}}}}
	got := User{ns: ns}.AutoRequires(autoUser(map[string]any{"groups": "g"}), func(string) bool { return true })
	if want := []string{"Group[adm]", "Group[g]"}; !slices.Equal(got, want) {
		t.Errorf("AutoRequires = %v, want %v", got, want)
	}
}

func TestGroupGIDsSkipsGroupsWithoutGID(t *testing.T) {
	got := GroupGIDs(&catalog.Catalog{Resources: []catalog.Resource{
		autoGroup("a", "7"), autoGroup("b", nil), {Type: "File", Title: "/x"},
	}})
	if want := []GroupGID{{Ref: "Group[a]", GID: 7}}; !slices.Equal(got, want) {
		t.Errorf("GroupGIDs = %v, want %v", got, want)
	}
}
