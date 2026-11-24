package catalog

import (
	"fmt"
	"sort"
)

// GenerationOverride replaces parameters on a resource the catalog holds.
type GenerationOverride struct {
	Ref        string
	Parameters map[string]any
	Sensitive  []string
}

// Generation is what one generator did to a catalog, with values resolved.
type Generation struct {
	Resources  []Resource        // new resources
	Containers map[string]string // child ref -> container ref
	Generators map[string]string // child ref -> generator ref
	Before     []Edge            // Source before Target, appended to Source's "before"
	Overrides  []GenerationOverride
}

// Placement says where MergeGeneration puts new resources.
type Placement int

const (
	// PlaceAppend adds them after the catalog's resources.
	PlaceAppend Placement = iota
	// PlaceAfterGenerator adds each right after its generator.
	PlaceAfterGenerator
)

// MergeGeneration folds what a load-time generate pass added into c, so the
// rest of the run sees one catalog. Puppet orders and applies generated
// resources as ordinary ones, contained and ordered by the generator, so
// they become catalog resources, with their containment edges, added before
// values and overrides. Each is also marked with the resource that generated
// it (GeneratedFrom, from g.Generators), because Ruby's report gives a
// generated resource no file or line.
//
// With PlaceAppend the new resources follow the catalog's; the extension
// host then reorders them with ApplyOrder. With PlaceAfterGenerator each
// sits right after its generator, in g.Resources order.
//
// A ref c lacks is a voxagent logic error, reported rather than skipped.
// g's values are already resolved (ResolveRich), as Decode resolves the
// catalog's.
func MergeGeneration(cat *Catalog, g Generation, p Placement) error {
	index := make(map[string]int, len(cat.Resources)+len(g.Resources))
	for i, r := range cat.Resources {
		index[r.Ref()] = i
	}
	existing := len(cat.Resources)
	for _, r := range g.Resources {
		index[r.Ref()] = len(cat.Resources)
		cat.Resources = append(cat.Resources, r)
	}
	// Map order is random; sort for a deterministic catalog.
	children := make([]string, 0, len(g.Containers))
	for child := range g.Containers {
		children = append(children, child)
	}
	sort.Strings(children)
	for _, child := range children {
		container := g.Containers[child]
		for _, ref := range []string{container, child} {
			if _, ok := index[ref]; !ok {
				return fmt.Errorf("generation contains %s in %s, but %s is not in the catalog", child, container, ref)
			}
		}
		cat.Edges = append(cat.Edges, Edge{Source: container, Target: child})
	}
	for _, b := range g.Before {
		for _, ref := range []string{b.Source, b.Target} {
			if _, ok := index[ref]; !ok {
				return fmt.Errorf("generation orders %s before %s, but %s is not in the catalog", b.Source, b.Target, ref)
			}
		}
		i := index[b.Source]
		r := &cat.Resources[i]
		if r.Parameters == nil {
			r.Parameters = map[string]any{}
		}
		switch old := r.Parameters["before"].(type) {
		case nil:
			r.Parameters["before"] = []any{b.Target}
		case []any:
			r.Parameters["before"] = append(old, b.Target)
		default:
			r.Parameters["before"] = []any{old, b.Target}
		}
	}
	for _, o := range g.Overrides {
		i, ok := index[o.Ref]
		if !ok {
			return fmt.Errorf("generation overrides %s, which is not in the catalog", o.Ref)
		}
		r := &cat.Resources[i]
		if r.Parameters == nil {
			r.Parameters = map[string]any{}
		}
		r.MarkSensitive(o.Sensitive)
		for name, v := range o.Parameters {
			r.Parameters[name] = v
		}
	}
	// Last, so each generator is recorded as generation left it. The walk
	// reads a generated resource's provenance from GeneratedFrom; Ruby's
	// report gives such a resource no file or line (resource/status.rb:169-170).
	generated := make([]string, 0, len(g.Generators))
	for child := range g.Generators {
		generated = append(generated, child)
	}
	sort.Strings(generated)
	for _, child := range generated {
		generator := g.Generators[child]
		for _, ref := range []string{generator, child} {
			if _, ok := index[ref]; !ok {
				return fmt.Errorf("generation says %s generated %s, but %s is not in the catalog", generator, child, ref)
			}
		}
		from := cat.Resources[index[generator]]
		cat.Resources[index[child]].GeneratedFrom = &from
	}
	if p == PlaceAfterGenerator {
		placeAfterGenerators(cat, existing, g.Generators, index)
	}
	return nil
}

// placeAfterGenerators reorders cat.Resources so each resource from position
// existing on follows its generator (the existing ones keep their order),
// in the order they were added; one with no generator goes last.
func placeAfterGenerators(cat *Catalog, existing int, generators map[string]string, index map[string]int) {
	children := map[int][]int{}
	var orphans []int
	for i := existing; i < len(cat.Resources); i++ {
		if gen, ok := generators[cat.Resources[i].Ref()]; ok {
			children[index[gen]] = append(children[index[gen]], i)
		} else {
			orphans = append(orphans, i)
		}
	}
	placed := make([]bool, len(cat.Resources))
	out := make([]Resource, 0, len(cat.Resources))
	var emit func(i int)
	emit = func(i int) {
		if placed[i] {
			return
		}
		placed[i] = true
		out = append(out, cat.Resources[i])
		for _, child := range children[i] {
			emit(child)
		}
	}
	for i := 0; i < existing; i++ {
		emit(i)
	}
	for _, i := range orphans {
		emit(i)
	}
	// A generator cycle among new resources is unreachable from either.
	for i := existing; i < len(cat.Resources); i++ {
		emit(i)
	}
	cat.Resources = out
}

// ApplyOrder reorders cat.Resources to order, which must name each of them once.
// order is the host's catalog after the generation pass, so each generated
// resource takes the place Puppet gave it beside its parent
// (additional_resource_generator.rb:36-50), and the walk's catalog-order keys
// follow Puppet's. A mismatch is a voxagent logic error, as for
// MergeGeneration: the host validated order against the refs it was given.
func ApplyOrder(cat *Catalog, order []string) error {
	index := make(map[string]int, len(cat.Resources))
	for i, r := range cat.Resources {
		index[r.Ref()] = i
	}
	placed := make(map[string]struct{}, len(order))
	resources := make([]Resource, 0, len(cat.Resources))
	for _, ref := range order {
		i, ok := index[ref]
		if !ok {
			return fmt.Errorf("the host's order names %s, which is not in the catalog", ref)
		}
		if _, dup := placed[ref]; dup {
			return fmt.Errorf("the host's order names %s twice", ref)
		}
		placed[ref] = struct{}{}
		resources = append(resources, cat.Resources[i])
	}
	for _, r := range cat.Resources {
		if _, ok := placed[r.Ref()]; !ok {
			return fmt.Errorf("the host's order omits %s", r.Ref())
		}
	}
	cat.Resources = resources
	return nil
}
