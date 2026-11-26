package ral

import (
	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/report"
)

// LoadGeneration is what one LoadGenerator did: the catalog change, and what
// Puppet's generate also reports.
type LoadGeneration struct {
	catalog.Generation
	// Logs are the generator's log lines. A Source naming a catalog resource
	// (normally the generator's own ref) becomes that resource's log source;
	// any other Source (Puppet) is kept.
	Logs []report.LogEntry
	// Failed is Ruby's resources_failed_to_generate (transaction.rb:197): the
	// run fails, though the resources generated so far still apply. The
	// generator logs the failure itself.
	Failed bool
	// DepthFirst is Ruby's depthfirst?: the new resources go before the
	// generator, not after it (additional_resource_generator.rb:36-50).
	DepthFirst bool
}

// LoadGenerator is a native type's Puppet `generate` (transaction.rb:100),
// run once per resource in catalog order when the extension host did not
// load the catalog.
//
// It returns what the generation adds to c and changes in it, with values
// resolved, for catalog.MergeGeneration. It must not modify c or r. A non-nil
// error is a voxagent logic error that refuses the run; a generation Ruby
// fails non-fatally sets Failed instead.
type LoadGenerator interface {
	LoadGenerate(c *catalog.Catalog, r catalog.Resource) (LoadGeneration, error)
}
