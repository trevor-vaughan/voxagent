package ral

import (
	"sort"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Validating is implemented by a provider that can tell a catalog Puppet itself
// would refuse.
//
// It is deliberately separate from pre-flight. Pre-flight answers "does
// voxagent implement this?", and its gaps are the porting roadmap. A resource
// Puppet rejects -- a relative file path fails the whole run with "Could not
// run" while the catalog is converted -- is not a porting gap, and counting it
// as one would put invalid catalogs into the roadmap.
type Validating interface {
	// Validate returns the reason Puppet would refuse this resource, in
	// Puppet's words where Puppet has them, or nil.
	Validate(r catalog.Resource) error
}

// Invalid is one resource Puppet would refuse.
type Invalid struct {
	Ref string
	Err error
}

// String returns the error text alone: providers build the text with the
// resource reference already in it, as Puppet's messages carry it.
func (i Invalid) String() string { return i.Err.Error() }

// Validate reports every resource Puppet would refuse.
//
// Like Preflight it returns all of them, sorted, so a refusing run prints the
// same message every time and an operator sees the whole list at once.
func Validate(reg *Registry, c *catalog.Catalog) []Invalid {
	var out []Invalid
	for _, r := range c.Resources {
		if IsContainer(r.Type) {
			continue
		}
		p, ok := reg.Provider(r.Type)
		if !ok {
			continue
		}
		v, ok := p.(Validating)
		if !ok {
			continue
		}
		if err := v.Validate(r); err != nil {
			out = append(out, Invalid{Ref: r.Ref(), Err: err})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}
