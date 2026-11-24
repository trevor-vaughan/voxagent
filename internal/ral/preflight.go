package ral

import (
	"fmt"
	"sort"

	"github.com/voxagent/voxagent/internal/catalog"
)

// ValueBounded is implemented by a provider that bounds not just WHICH
// attributes it reads but which VALUES of them it can honour.
//
// A provider that does not implement it bounds attributes only, and an
// unimplemented value surfaces at apply time as a resource-level error, which
// leaves the rest of the catalog applied and the node half-converged. Every
// provider with a value it cannot honour should implement this.
type ValueBounded interface {
	// SupportsValue reports whether this provider implements the given value of
	// the given attribute. It is a predicate rather than a set because not every
	// bounded domain is enumerable: package's ensure accepts any version string
	// AND refuses "purged", which no finite set can express.
	//
	// Attributes the provider does not bound return true.
	SupportsValue(attr, value string) bool
}

// Gap is one reason a catalog cannot be applied in full.
//
// An empty Attribute means the type itself has no provider. A non-empty
// Attribute with an empty Value means the type is implemented but this
// attribute is not. A non-empty Value means both the type and the attribute are
// implemented but this value of it is not -- the provider bounds its value
// domain through ValueBounded and rejected this one.
type Gap struct {
	Ref       string
	Type      string
	Attribute string
	Value     string
}

func (g Gap) String() string {
	switch {
	case g.Attribute == "":
		return fmt.Sprintf("%s: resource type %s is not implemented", g.Ref, g.Type)
	case g.Value != "":
		return fmt.Sprintf("%s: %s => %q is not implemented", g.Ref, g.Attribute, g.Value)
	default:
		return fmt.Sprintf("%s: attribute %q is not implemented", g.Ref, g.Attribute)
	}
}

// Preflight reports every reason the registry cannot apply this catalog.
//
// It returns every gap rather than stopping at the first, for two reasons. An
// operator fixing coverage wants the whole list, not one item at a time; and
// the same list, run across a fleet's catalogs, is the ranked porting roadmap
// -- measured from real catalogs rather than guessed.
//
// The result is sorted, so the message a refusing run prints is identical
// between runs on the same catalog. A refusal that reordered itself would be
// indistinguishable from a refusal that changed.
func Preflight(reg *Registry, c *catalog.Catalog) []Gap {
	var gaps []Gap
	for _, r := range c.Resources {
		if IsContainer(r.Type) {
			// Containers structure the catalog and apply nothing, so their
			// parameters are not provider work. A parameterised class is
			// ordinary, and reporting its parameters as gaps would refuse every
			// catalog that uses one.
			continue
		}
		p, ok := reg.Provider(r.Type)
		if !ok {
			// The type is the single actionable fact. Listing its attributes
			// as further gaps would inflate the porting histogram by attribute
			// count and bury the one thing an operator can act on.
			gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type})
			continue
		}
		implemented := p.Attrs()
		// A provider that does not bound its values leaves them to apply time.
		// Resolve the assertion once per resource rather than once per
		// attribute.
		bounded, boundsValues := p.(ValueBounded)
		names := make([]string, 0, len(r.Parameters))
		for name := range r.Parameters {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if IsEngineAttr(name) {
				continue
			}
			if _, ok := implemented[name]; !ok {
				// The attribute has no code at all, so its value is not a
				// separate fact to report. Reporting both would double-count
				// one attribute in the porting histogram this list feeds, and
				// would point an operator at a value of an attribute that does
				// not exist yet.
				gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name})
				continue
			}
			if !boundsValues {
				continue
			}
			// A non-string is judged as the string the provider will act on.
			// Providers read parameters through fmt's %v (paramString), so a
			// JSON false reaches Apply as "false"; fmt.Sprint renders a bool
			// exactly as strconv.FormatBool does. Skipping non-strings was
			// measured to half-apply a catalog: {"ensure": false} on one file
			// passed pre-flight, the rest of the catalog was written, and that
			// resource failed at apply. Through the predicate, an attribute
			// the provider does not bound still answers true, and a bounded
			// one refuses any rendering it does not implement -- "[purged]",
			// "42", "<nil>" -- which is the safe failure for a value the
			// provider cannot interpret.
			value, ok := r.Parameters[name].(string)
			if !ok {
				value = fmt.Sprint(r.Parameters[name])
			}
			if bounded.SupportsValue(name, value) {
				continue
			}
			gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name, Value: value})
		}
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Ref != gaps[j].Ref {
			return gaps[i].Ref < gaps[j].Ref
		}
		return gaps[i].Attribute < gaps[j].Attribute
	})
	return gaps
}
