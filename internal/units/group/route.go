package group

import (
	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// MeasuredPlatform reports whether the host is one the user and group units
// were measured on: RHEL family major >= 10 but not Fedora, and Puppet's
// libuser feature off (/usr/sbin/luseradd and /usr/sbin/lgroupadd not both
// executable). An unreadable or unparsable os-release is not.
func MeasuredPlatform(p ral.Platform) bool {
	if p.Executable("/usr/sbin/luseradd") && p.Executable("/usr/sbin/lgroupadd") {
		return false
	}
	return ral.MeasuredRelease(p)
}

// ProviderIs reports whether r leaves the provider unset or names want.
func ProviderIs(r catalog.Resource, want string) bool {
	v, set := r.Parameters["provider"]
	if !set {
		return true
	}
	s, ok := v.(string)
	return ok && s == want
}
