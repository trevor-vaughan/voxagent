package concat

import (
	"slices"

	"github.com/voxagent/voxagent/internal/units/file"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// concatRelationships are Puppet::Type.relationship_params, the metaparameters
// add_generated_directed_dependency reads for an existing edge.
var concatRelationships = []string{"before", "notify", "require", "subscribe"}

// LoadGenerate implements LoadGenerator: concat_file's generate
// (type/concat_file.rb:323-351) as additional_resource_generator.rb:21-55 and
// :136-212 apply it (spec §4).
//
// The File at the Concat_file's path is reused when the catalog declares it,
// untouched; otherwise a new File takes the copied attributes and their
// marks, `file` and the Concat_file's tags, and the Concat_file's container.
// Either way the Concat_file is ordered before it unless a relationship
// already links the two.
func (c ConcatFile) LoadGenerate(cat *catalog.Catalog, r catalog.Resource) (ral.LoadGeneration, error) {
	p, err := parseConcatFile(r)
	if err != nil {
		return ral.LoadGeneration{}, err
	}
	var g catalog.Generation
	cfRef := r.Ref()
	// D-78: Ruby's generate copies these values bare onto a new File, dropping
	// their marks, so its events print them; voxagent carries the marks.
	var marks []string
	for _, name := range []string{"owner", "group", "mode"} {
		if _, set := p.Copied[name]; set && slices.Contains(r.SensitiveParameters, name) {
			marks = append(marks, name)
		}
	}
	fileRef, declared := concatFileRef(cat, p.Path)
	var file catalog.Resource
	if declared {
		file = cat.Resources[slices.IndexFunc(cat.Resources, func(x catalog.Resource) bool { return x.Ref() == fileRef })]
	} else {
		// No path: the File is titled p.Path, and a catalog omits a namevar
		// equal to the title (resource.rb to_data_hash), as the host's
		// serialisation of Ruby's generated File does.
		params := map[string]any{"ensure": "file", "loglevel": "notice"}
		if p.Absent {
			params["ensure"] = "absent"
		}
		for name, v := range p.Copied {
			params[name] = v
		}
		// res.merge_tags_from(parent): a TagSet, so a repeated tag is kept once.
		tags := []string{"file"}
		for _, t := range r.Tags {
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
		file = catalog.Resource{Type: "File", Title: p.Path, Tags: tags, Parameters: params, SensitiveParameters: marks}
		fileRef = file.Ref()
		g.Resources = []catalog.Resource{file}
		g.Generators = map[string]string{fileRef: cfRef}
		// catalog.container_of(parent): the source of its containment edge.
		for _, e := range cat.Edges {
			if e.Target == cfRef {
				g.Containers = map[string]string{fileRef: e.Source}
				break
			}
		}
	}
	// Compared as written, as Ruby compares res.ref: a relationship naming the
	// File by an alias is not an edge here.
	for _, rel := range concatRelationships {
		if slices.Contains(concatRefs(r.Parameters[rel]), fileRef) ||
			slices.Contains(concatRefs(file.Parameters[rel]), cfRef) {
			return ral.LoadGeneration{Generation: g}, nil
		}
	}
	g.Before = []catalog.Edge{{Source: cfRef, Target: fileRef}}
	return ral.LoadGeneration{Generation: g}, nil
}

// concatFileRef is catalog.resource("File[path]"): the File titled path, else
// the File whose path is path, the alias Puppet's catalog keeps for it.
func concatFileRef(cat *catalog.Catalog, path string) (string, bool) {
	for _, r := range cat.Resources {
		if r.Type == "File" && r.Title == path {
			return r.Ref(), true
		}
	}
	for _, r := range cat.Resources {
		if r.Type == "File" && file.FilePath(r) == path {
			return r.Ref(), true
		}
	}
	return "", false
}

// concatRefs reads a relationship metaparameter's references: one, or an
// Array of them.
func concatRefs(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
