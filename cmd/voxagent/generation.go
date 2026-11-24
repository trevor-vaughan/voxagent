package main

import (
	"fmt"
	"sort"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
)

// mergeGeneration folds what Puppet's load-time generate pass added to the
// host's catalog into cat, so the rest of the run sees one catalog. Puppet
// orders and applies generated resources as ordinary ones, contained and
// ordered by the generator, so they are appended as catalog resources, with
// their containment edges, appended before values and overrides. Each is
// also marked with the resource that generated it (GeneratedFrom, from the
// host's generators), because Ruby's report gives a generated resource no
// file or line.
//
// The host validated every ref against the catalog it holds; a ref cat lacks
// is a voxagent logic error, reported rather than skipped.
//
// Values are resolved as Decode resolves the catalog's (catalog.ResolveRich),
// because the host's to_data_hash writes an ASCII-8BIT string as a Binary.
func mergeGeneration(cat *catalog.Catalog, g extension.Generation) error {
	index := make(map[string]int, len(cat.Resources)+len(g.Resources))
	for i, r := range cat.Resources {
		index[r.Ref()] = i
	}
	for _, gr := range g.Resources {
		r := catalog.Resource{Type: gr.Type, Title: gr.Title, Tags: gr.Tags, Kind: gr.Kind, Exported: gr.Exported}
		params, err := catalog.NormaliseNumbers(gr.Parameters)
		if err == nil {
			params, err = catalog.ResolveRich(params)
		}
		if err != nil {
			return fmt.Errorf("generated %s: %w", r.Ref(), err)
		}
		r.Parameters = params.(map[string]any)
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
		cat.Edges = append(cat.Edges, catalog.Edge{Source: container, Target: child})
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
			n, err := catalog.NormaliseNumbers(v)
			if err == nil {
				n, err = catalog.ResolveRich(n)
			}
			if err != nil {
				return fmt.Errorf("the override of %s: %w", o.Ref, err)
			}
			r.Parameters[name] = n
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
	return nil
}

// applyOrder reorders cat.Resources to order, which must name each of them once.
// order is the host's catalog after the generation pass, so each generated
// resource takes the place Puppet gave it beside its parent
// (additional_resource_generator.rb:36-50), and the walk's catalog-order keys
// follow Puppet's. A mismatch is a voxagent logic error, as for
// mergeGeneration: the host validated order against the refs it was given.
func applyOrder(cat *catalog.Catalog, order []string) error {
	index := make(map[string]int, len(cat.Resources))
	for i, r := range cat.Resources {
		index[r.Ref()] = i
	}
	placed := make(map[string]struct{}, len(order))
	resources := make([]catalog.Resource, 0, len(cat.Resources))
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
