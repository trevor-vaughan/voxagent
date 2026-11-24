package ral

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Validating is implemented by a provider that can tell a resource must not be
// applied: a catalog Puppet itself refuses, or a resource this host cannot
// apply the way Puppet would (service's systemd provider on a host whose init
// is not systemd).
//
// It is deliberately separate from pre-flight. Pre-flight answers "does
// voxagent implement this?", and its gaps are the porting roadmap. A resource
// Puppet rejects -- a relative file path fails the whole run with "Could not
// run" while the catalog is converted -- is not a porting gap, and counting it
// as one would put invalid catalogs into the roadmap.
type Validating interface {
	// Validate returns the reason this resource must not be applied, in
	// Puppet's words where Puppet has them, or nil.
	Validate(r catalog.Resource) error
}

// Invalid is one reason a catalog must not be applied: a resource (see
// Validating), or, with an empty Ref, the catalog itself.
type Invalid struct {
	Ref string
	Err error
}

// String returns the error text alone: providers build the text with the
// resource reference already in it, as Puppet's messages carry it.
func (i Invalid) String() string { return i.Err.Error() }

// Validate reports every resource that must not be applied: one Puppet
// would refuse, or one this host cannot apply the way Puppet would.
//
// Like Preflight it returns all of them, sorted, so a refusing run prints the
// same message every time and an operator sees the whole list at once.
func Validate(reg *Registry, c *catalog.Catalog) []Invalid {
	var out []Invalid
	// A static catalog carries file metadata the server computed, and
	// Puppet's source reads it in place of the file (type/file/source.rb:186).
	// voxagent always reads the source itself, so applying the catalog would
	// silently ignore part of it.
	for _, inlined := range []struct {
		key string
		raw json.RawMessage
	}{{"metadata", c.Metadata}, {"recursive_metadata", c.RecursiveMetadata}} {
		if len(inlined.raw) > 0 && string(inlined.raw) != "null" {
			out = append(out, Invalid{Err: fmt.Errorf(
				"the catalog carries %s (a static catalog's inlined file metadata), which is not implemented", inlined.key)})
		}
	}
	for _, r := range c.Resources {
		if IsContainerResource(r) {
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
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}
