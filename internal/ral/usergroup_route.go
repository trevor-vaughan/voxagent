package ral

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Platform is what NativeUserGroup and NativeAugeas learn from the host. It is
// an interface so tests do not depend on the machine they run on.
type Platform interface {
	Executable(path string) bool
	OSRelease() ([]byte, error)
}

// HostPlatform is the platform of the running host.
var HostPlatform Platform = realPlatform{}

type realPlatform struct{}

// Executable is FileTest.file? && FileTest.executable?, following symlinks.
func (realPlatform) Executable(path string) bool { return executableFile(path) }

func (realPlatform) OSRelease() ([]byte, error) { return os.ReadFile("/etc/os-release") }

// minNativeMajor is the first RHEL major the native user and group providers
// were measured against.
const minNativeMajor = 10

// NativeUserGroup says whether the user and group types are applied natively
// on this host, each decided on its own; otherwise they go to the extension
// host, which is the oracle for everything not measured (spec §0, §2).
//
// Both types need the platform to be RHEL family major >= 10 but not Fedora,
// and Puppet's libuser feature to be off (/usr/sbin/luseradd and
// /usr/sbin/lgroupadd not both executable). A type also stays with the host
// when a resource of it names a provider other than useradd/groupadd; User
// additionally when any resource's purge_ssh_keys is not false, "false" or [],
// because that generates Ssh_authorized_key resources only the host can apply.
// And a type stays with the host when a resource of it has a value shape the
// native parser does not implement (unimplementedValue), which the host
// applied before the type was native; Ruby's own refusals stay native.
func NativeUserGroup(p Platform, cat *catalog.Catalog) (user, group bool) {
	if !measuredPlatform(p) {
		return false, false
	}
	user, group = true, true
	var uv *unimplementedValue
	for _, r := range cat.Resources {
		switch strings.ToLower(r.Type) {
		case "user":
			_, err := parseUserParams(r)
			if !providerIs(r, "useradd") || !purgeKeepsNative(r) || errors.As(err, &uv) {
				user = false
			}
		case "group":
			_, err := parseGroup(r)
			if !providerIs(r, "groupadd") || errors.As(err, &uv) {
				group = false
			}
		}
	}
	return user, group
}

// measuredPlatform reports whether the host is one NativeUserGroup was
// measured on. An unreadable or unparsable os-release is not.
func measuredPlatform(p Platform) bool {
	if p.Executable("/usr/sbin/luseradd") && p.Executable("/usr/sbin/lgroupadd") {
		return false
	}
	return measuredRelease(p)
}

// measuredRelease reports whether /etc/os-release names RHEL family major >=
// 10, not Fedora. An unreadable or unparsable os-release does not.
func measuredRelease(p Platform) bool {
	data, err := p.OSRelease()
	if err != nil {
		return false
	}
	fields := parseOSRelease(data)
	if fields["ID"] == "fedora" {
		return false
	}
	rhel := fields["ID"] == "rhel"
	for _, w := range strings.Fields(fields["ID_LIKE"]) {
		rhel = rhel || w == "rhel"
	}
	major, _, _ := strings.Cut(fields["VERSION_ID"], ".")
	n, err := strconv.ParseUint(major, 10, 31)
	return rhel && err == nil && n >= minNativeMajor
}

// parseOSRelease reads KEY=value lines; a value may be double- or
// single-quoted. Lines that are not assignments are skipped.
func parseOSRelease(data []byte) map[string]string {
	fields := map[string]string{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		k, v, ok := strings.Cut(strings.TrimSpace(string(line)), "=")
		if !ok || k == "" || strings.HasPrefix(k, "#") {
			continue
		}
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		fields[k] = v
	}
	return fields
}

// providerIs reports whether r leaves the provider unset or names want.
func providerIs(r catalog.Resource, want string) bool {
	v, set := r.Parameters["provider"]
	if !set {
		return true
	}
	s, ok := v.(string)
	return ok && s == want
}

// purgeKeepsNative reports whether r's purge_ssh_keys is absent, false, "false"
// or an empty list: the values that generate no Ssh_authorized_key resources.
func purgeKeepsNative(r catalog.Resource) bool {
	v, set := r.Parameters["purge_ssh_keys"]
	if !set {
		return true
	}
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		return t == "false"
	case []any:
		return len(t) == 0
	}
	return false
}
