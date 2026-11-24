package unit

import (
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// ResourcesPurge reports whether a Resources resource for typ may purge (spec
// §2 condition 5): Ruby matches the type name case-insensitively and reads yes
// and any capitalisation of true or yes as true, so only an absent purge,
// false and "false" are known not to. Anything else counts as purging and the
// type goes to the host.
func ResourcesPurge(cat *catalog.Catalog, typ string) bool {
	for _, r := range cat.Resources {
		if !strings.EqualFold(r.Type, "resources") {
			continue
		}
		name, ok := r.Parameters["name"].(string)
		if !ok {
			name = r.Title
		}
		if !strings.EqualFold(name, typ) {
			continue
		}
		switch v := r.Parameters["purge"].(type) {
		case nil:
		case bool:
			if v {
				return true
			}
		case string:
			if v != "false" {
				return true
			}
		default:
			return true
		}
	}
	return false
}
