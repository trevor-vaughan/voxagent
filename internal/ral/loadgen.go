package ral

import "github.com/voxagent/voxagent/internal/catalog"

// LoadGenerator is a native type's Puppet `generate` (transaction.rb:100),
// run once per resource in catalog order when the extension host did not
// load the catalog.
//
// It returns what the generation adds to c and changes in it, with values
// resolved, for catalog.MergeGeneration. It must not modify c or r.
type LoadGenerator interface {
	LoadGenerate(c *catalog.Catalog, r catalog.Resource) (catalog.Generation, error)
}
