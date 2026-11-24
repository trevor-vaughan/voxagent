package ral

import (
	"fmt"

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

// ListValued is implemented by a ValueBounded provider with attributes whose
// value may be a list, each element of which the provider bounds on its own:
// file's source, where Puppet takes the first element that exists
// (type/file/source.rb:191-204).
//
// For any other attribute a list is judged whole, as its string form, so a
// scalar attribute handed ["present"] is refused at pre-flight rather than
// passing as "present" and failing at apply time, after the rest of the
// catalog has been applied.
type ListValued interface {
	ListAttrs() Attrs
}

// Gap is one reason a catalog cannot be applied in full.
//
// An empty Attribute means the type itself has no provider. A non-empty
// Attribute with an empty Value means the type is implemented but this
// attribute is not. A non-empty Value means both the type and the attribute are
// implemented but this value of it is not -- the provider bounds its value
// domain through ValueBounded and rejected this one. A non-empty Reason says
// why a type is refused although a provider may claim it (see Filebucket).
// A non-empty Rich is the __ptype of a rich value Attribute holds, which a
// native provider cannot take (catalog.RichValues, decision D-51). Marked is
// set when the catalog marks Attribute sensitive: String then shows Value as
// [redacted] (D-78; Value itself stays for sorting).
type Gap struct {
	Ref       string
	Type      string
	Attribute string
	Value     string
	Reason    string
	Rich      string
	Marked    bool
}

func (g Gap) String() string {
	switch {
	case g.Reason != "":
		return fmt.Sprintf("%s: resource type %s is not implemented: %s", g.Ref, g.Type, g.Reason)
	case g.Rich == "Deferred":
		return fmt.Sprintf("%s: parameter %s is Deferred; voxagent does not resolve Deferred values", g.Ref, g.Attribute)
	case g.Rich == "Binary":
		return fmt.Sprintf("%s: parameter %s holds a Binary value that is not ASCII, which voxagent does not support on a native resource",
			g.Ref, g.Attribute)
	case g.Rich != "":
		return fmt.Sprintf("%s: parameter %s holds a rich %s value, which voxagent does not support", g.Ref, g.Attribute, g.Rich)
	case g.Attribute == "":
		return fmt.Sprintf("%s: resource type %s is not implemented", g.Ref, g.Type)
	case g.Value != "":
		return fmt.Sprintf("%s: %s => %s is not implemented", g.Ref, g.Attribute, redactIf(g.Marked, "%q", g.Value))
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
		if IsContainerResource(r) {
			// Containers structure the catalog and apply nothing, so their
			// parameters are not provider work. A parameterised class is
			// ordinary, and reporting its parameters as gaps would refuse every
			// catalog that uses one.
			continue
		}
		// A type the registry does not know is judged against a nil
		// provider, which ResourceGaps reports as the type's one gap.
		p, _ := reg.Provider(r.Type)
		gaps = append(gaps, ResourceGaps(p, r)...)
	}
	sortGaps(gaps)
	return gaps
}

// valuesToBound returns the strings SupportsValue judges for one parameter.
//
// A non-string is judged as the string the provider will act on. Providers
// read parameters through fmt's %v (paramString), so a JSON false reaches
// Apply as "false"; fmt.Sprint renders a bool exactly as strconv.FormatBool
// does. Skipping non-strings was measured to half-apply a catalog: {"ensure":
// false} on one file passed pre-flight, the rest of the catalog was written,
// and that resource failed at apply. Through the predicate, an attribute the
// provider does not bound still answers true, and a bounded one refuses any
// rendering it does not implement -- "[purged]", "42", "<nil>" -- which is the
// safe failure for a value the provider cannot interpret.
//
// A list is split into its elements only for an attribute the provider
// declares list-valued (ListValued); each element is then judged, and
// reported, on its own.
func valuesToBound(raw any, list bool) []string {
	render := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	if elems, ok := raw.([]any); ok && list {
		out := make([]string, len(elems))
		for i, e := range elems {
			out[i] = render(e)
		}
		return out
	}
	return []string{render(raw)}
}
