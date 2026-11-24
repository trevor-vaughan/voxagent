package augeasproviderssysctl

import (
	"regexp"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// Sysctl parses its parameters here. A value Ruby refuses is an error carrying
// Ruby's text (spec §3); a value Ruby accepts but native does not implement
// (spec §2 (d)-(f)) is an *rubyvalue.UnimplementedValue, which sends the type
// to the extension host.

// sysctlParams is a Sysctl as type/sysctl.rb munges it.
type sysctlParams struct {
	Name, Target, Value string
	HasValue            bool
	Apply, Silent       bool
	EnsureSet           bool // ensure => present was declared
}

// sysctlTitle is §2 (d): dot-separated segments, none starting with a
// digit, `-` or `.`.
var sysctlTitle = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z_][A-Za-z0-9_-]*)*$`)

func parseSysctl(r catalog.Resource) (sysctlParams, error) {
	p := sysctlParams{Name: r.Title}
	if err := augeas.Prelude(r); err != nil {
		return p, err
	}
	if !sysctlTitle.MatchString(r.Title) {
		return p, augeas.Unimplemented(r, "name")
	}
	ensure, err := core.Ensure(r)
	if err != nil {
		return p, err
	}
	if ensure == "absent" {
		return p, augeas.Unimplemented(r, "ensure")
	}
	p.EnsureSet = ensure == "present"
	if r.Parameters["val"] != nil {
		return p, augeas.Unimplemented(r, "val")
	}
	if v := r.Parameters["value"]; v != nil {
		s, ok := core.Scalar(v)
		if !ok {
			return p, augeas.Unimplemented(r, "value")
		}
		p.Value, p.HasValue = s, true
	}
	if err := core.Comment(r); err != nil {
		return p, err
	}
	if p.Target, err = core.Target(r, false); err != nil {
		return p, err
	}
	if p.Apply, err = core.Bool(r, "apply", "true, false", true); err != nil {
		return p, err
	}
	persist, err := core.Bool(r, "persist", "true, false", true)
	if err != nil {
		return p, err
	}
	if !persist {
		return p, augeas.Unimplemented(r, "persist")
	}
	p.Silent, err = core.Bool(r, "silent", "true, false", false)
	return p, err
}
