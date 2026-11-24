package group

import (
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// UnmeasuredReason says why the host is not one the user and group units were
// measured on, or "" when it is: RHEL family major >= 10 but not Fedora, and
// Puppet's libuser feature off (/usr/sbin/luseradd and /usr/sbin/lgroupadd not
// both executable). libuser is checked first. An unreadable or unparsable
// os-release is not measured.
func UnmeasuredReason(p ral.Platform) string {
	if p.Executable("/usr/sbin/luseradd") && p.Executable("/usr/sbin/lgroupadd") {
		return "libuser present"
	}
	if !ral.MeasuredRelease(p) {
		return "not the measured platform"
	}
	return ""
}

// ForeignProvider says why r's provider is not want, or "" when r leaves the
// provider unset or names want.
func ForeignProvider(r catalog.Resource, want string) string {
	v, set := r.Parameters["provider"]
	if !set || v == want {
		return ""
	}
	return "provider " + rubyvalue.RedactIf(slices.Contains(r.SensitiveParameters, "provider"), "%v", v)
}
