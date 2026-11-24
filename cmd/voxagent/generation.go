package main

import (
	"fmt"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
)

// resolveGenerated normalises and resolves one generated value as Decode
// resolves the catalog's (catalog.ResolveRich), because the host's
// to_data_hash writes an ASCII-8BIT string as a Binary.
func resolveGenerated(v any) (any, error) {
	v, err := catalog.NormaliseNumbers(v)
	if err != nil {
		return nil, err
	}
	return catalog.ResolveRich(v)
}

// convertGeneration turns the host's generation into catalog.Generation, for
// catalog.MergeGeneration.
func convertGeneration(g extension.Generation) (catalog.Generation, error) {
	out := catalog.Generation{Containers: g.Containers, Generators: g.Generators}
	for _, gr := range g.Resources {
		r := catalog.Resource{Type: gr.Type, Title: gr.Title, Tags: gr.Tags, Kind: gr.Kind, Exported: gr.Exported}
		params, err := resolveGenerated(gr.Parameters)
		if err != nil {
			return catalog.Generation{}, fmt.Errorf("generated %s: %w", r.Ref(), err)
		}
		r.Parameters = params.(map[string]any)
		out.Resources = append(out.Resources, r)
	}
	for _, b := range g.Before {
		out.Before = append(out.Before, catalog.Edge{Source: b.Source, Target: b.Target})
	}
	for _, o := range g.Overrides {
		resolved := make(map[string]any, len(o.Parameters))
		for name, v := range o.Parameters {
			n, err := resolveGenerated(v)
			if err != nil {
				return catalog.Generation{}, fmt.Errorf("the override of %s: %w", o.Ref, err)
			}
			resolved[name] = n
		}
		out.Overrides = append(out.Overrides, catalog.GenerationOverride{Ref: o.Ref, Parameters: resolved, Sensitive: o.Sensitive})
	}
	return out, nil
}
