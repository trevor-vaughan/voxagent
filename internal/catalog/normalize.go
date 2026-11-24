package catalog

// AllowlistedFields names the top-level catalog fields that differ between two
// compiles of identical inputs, and which comparison therefore ignores.
//
// This list is measured, not assumed. Three compiles of one manifest exercising
// hash iteration, a defined type, resource defaults, class inheritance with a
// resource override, a virtual resource collector, and a collector-based
// override produced byte-identical output apart from these two fields, across
// all 20 resources and 19 edges.
//
// Adding to this list weakens the harness. Any addition needs the same kind of
// measurement behind it.
var AllowlistedFields = []string{"version", "catalog_uuid"}

// Normalize zeroes the allowlisted fields so that two catalogs compiled from
// identical inputs compare equal.
//
// It deliberately does not sort Resources. Puppet hardcodes
// SequentialPrioritizer (lib/puppet/resource/catalog.rb:561), which derives
// apply-order tie-breaks from catalog position, so resource order is
// behavioural. Sorting here would hide real divergence.
func Normalize(c *Catalog) {
	c.Version = nil
	c.CatalogUUID = ""
}
