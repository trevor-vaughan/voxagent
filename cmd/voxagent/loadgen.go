package main

import (
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/ral"
)

// loadGenerate runs every registered LoadGenerator over cat's resources, as
// they stood when it began and in catalog order, and merges what each adds
// right after its generator. It then judges the new resources as pre-flight
// and validation judge the catalog's, so a refusal prints through the same
// lines, scrubbed of the values the catalog marks: sc's before generation,
// and once it has added resources, theirs too. It reports whether the run must
// not start.
func loadGenerate(cat *catalog.Catalog, reg *ral.Registry, sc *redact.Scrubber) (refused bool) {
	var added []catalog.Resource
	for _, r := range slices.Clone(cat.Resources) {
		p, ok := reg.Provider(r.Type)
		if !ok {
			continue
		}
		g, ok := p.(ral.LoadGenerator)
		if !ok {
			continue
		}
		gen, err := g.LoadGenerate(cat, r)
		if err == nil {
			err = catalog.MergeGeneration(cat, gen, catalog.PlaceAfterGenerator)
		}
		if err != nil {
			stderrf(sc, "voxagent: %s: could not generate: %v; nothing applied\n", r.Ref(), err)
			return true
		}
		added = append(added, gen.Resources...)
	}
	fresh := &catalog.Catalog{Resources: added}
	sc = redact.New(cat)
	if gaps := ral.Preflight(reg, fresh); len(gaps) > 0 {
		resources := map[string]struct{}{}
		for _, g := range gaps {
			stderrf(sc, "voxagent: %s\n", g)
			resources[g.Ref] = struct{}{}
		}
		stderrf(sc, "voxagent: %d gap(s) across %d resource(s); nothing applied\n",
			len(gaps), len(resources))
		return true
	}
	if invalid := ral.Validate(reg, fresh); len(invalid) > 0 {
		for _, i := range invalid {
			stderrf(sc, "voxagent: %s\n", i)
		}
		stderrf(sc, "voxagent: %d invalid resource(s); nothing applied\n", len(invalid))
		return true
	}
	return false
}
