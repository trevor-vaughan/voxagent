package main

import (
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/transaction"
)

// loadGenerate runs every registered LoadGenerator over cat's resources, as
// they stood when it began and in catalog order, and merges what each adds
// right after its generator, or right before it for a depth-first one. It then judges the new resources as pre-flight
// and validation judge the catalog's, so a refusal prints through the same
// lines, scrubbed of the values the catalog marks: sc's before generation,
// and once it has added resources, theirs too.
//
// It returns the generators' log lines, in generation order, each sourced
// as the walk would source its resource, whether a generator failed
// (resources_failed_to_generate), and whether the run must not start. The
// report does not exist yet, so the caller adds the logs and the failure.
func loadGenerate(cat *catalog.Catalog, reg *ral.Registry, sc *redact.Scrubber) (logs []report.LogEntry, failed, refused bool) {
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
			place := catalog.PlaceAfterGenerator
			if gen.DepthFirst {
				place = catalog.PlaceBeforeGenerator
			}
			err = catalog.MergeGeneration(cat, gen.Generation, place)
		}
		if err != nil {
			stderrf(sc, "voxagent: %s: could not generate: %v; nothing applied\n", r.Ref(), err)
			return nil, false, true
		}
		added = append(added, gen.Resources...)
		logs = append(logs, gen.Logs...)
		failed = failed || gen.Failed
	}
	// Once every generator has run, so the containment the walk will see is
	// complete.
	sources := transaction.LogSources(cat, reg)
	for i := range logs {
		if source, ok := sources[logs[i].Source]; ok {
			logs[i].Source = source
		}
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
		return nil, false, true
	}
	if invalid := ral.Validate(reg, fresh); len(invalid) > 0 {
		for _, i := range invalid {
			stderrf(sc, "voxagent: %s\n", i)
		}
		stderrf(sc, "voxagent: %d invalid resource(s); nothing applied\n", len(invalid))
		return nil, false, true
	}
	return logs, failed, false
}
