package ral

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/command"
)

// Platform is what the user and group units and the augeas routes learn from the host. It is
// an interface so tests do not depend on the machine they run on.
type Platform interface {
	Executable(path string) bool
	OSRelease() ([]byte, error)
}

// HostPlatform is the platform of the running host.
var HostPlatform Platform = realPlatform{}

type realPlatform struct{}

// Executable is FileTest.file? && FileTest.executable?, following symlinks.
func (realPlatform) Executable(path string) bool { return command.ExecutableFile(path) }

func (realPlatform) OSRelease() ([]byte, error) { return os.ReadFile("/etc/os-release") }

// minNativeMajor is the first RHEL major the native user and group providers
// were measured against.
const minNativeMajor = 10

// MeasuredRelease reports whether /etc/os-release names RHEL family major >=
// 10, not Fedora. An unreadable or unparsable os-release does not.
func MeasuredRelease(p Platform) bool {
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

// Environ is os.Getenv: the value of key in voxagent's environment, "" when
// unset. It is a function so tests do not depend on the agent's environment.
type Environ func(key string) string
