package unit

import (
	"fmt"
	"sort"
)

// RouteAll routes every unit once, in name order, and merges the decisions.
// It panics if two units delegate the same type: that is a bug in the units,
// not a runtime condition.
func RouteAll(env *Env, all []Unit) Decision {
	sorted := append([]Unit(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Manifest().Name < sorted[j].Manifest().Name
	})
	var out Decision
	by := map[string]string{} // type -> name of the unit that delegated it
	for _, u := range sorted {
		d := u.Route(env)
		out.Providers = append(out.Providers, d.Providers...)
		name := u.Manifest().Name
		for typ, reason := range d.Delegated {
			if first, dup := by[typ]; dup {
				panic(fmt.Sprintf("unit: type %q delegated by both %s and %s", typ, first, name))
			}
			if out.Delegated == nil {
				out.Delegated = map[string]string{}
			}
			by[typ] = name
			out.Delegated[typ] = reason
		}
	}
	return out
}
