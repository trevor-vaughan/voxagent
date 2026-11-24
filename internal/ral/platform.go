package ral

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/command"
)

// Platform is what the user and group units and the augeas routes learn from the host. It is
// an interface so tests do not depend on the machine they run on.
type Platform interface {
	Executable(path string) bool
	FileExists(path string) bool
	OSRelease() ([]byte, error)
}

// HostPlatform is the platform of the running host.
var HostPlatform Platform = realPlatform{}

type realPlatform struct{}

// Executable is FileTest.file? && FileTest.executable?, following symlinks.
func (realPlatform) Executable(path string) bool { return command.ExecutableFile(path) }

// FileExists is Puppet::FileSystem.exist?: os.Stat succeeds, following symlinks. Any error,
// including a permission failure on a parent, means the path is absent.
func (realPlatform) FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// OSRelease reads /etc/os-release, or /usr/lib/os-release when that file does not exist.
func (realPlatform) OSRelease() ([]byte, error) {
	return readOSRelease("/etc/os-release", "/usr/lib/os-release")
}

// readOSRelease returns the first path's contents. Only a missing file (including a dangling
// symlink) moves on to the next path; any other error is returned, so an unreadable
// /etc/os-release is not masked by a different file. When every path is missing, the last
// error is returned.
func readOSRelease(paths ...string) ([]byte, error) {
	var err error
	for _, path := range paths {
		var data []byte
		if data, err = os.ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
			return data, err
		}
	}
	return nil, err
}

// OS is one operating system release family a unit was measured on.
type OS struct {
	ID       string `json:"id"`        // os-release ID, e.g. "rhel"; "*" is any platform
	Like     bool   `json:"like"`      // also accept a host whose ID_LIKE names ID
	MinMajor uint   `json:"min_major"` // lowest VERSION_ID major accepted; 0 = any
}

// OnPlatform reports whether the host's os-release matches some entry of list. An entry with ID
// "*" matches without reading os-release. Otherwise an entry matches when the host's ID equals
// e.ID (or, with Like, e.ID is a word of ID_LIKE) and VERSION_ID's major is at least MinMajor; a
// missing or unparsable VERSION_ID fails any entry with MinMajor > 0. An unreadable os-release
// matches nothing. Fedora needs no exclusion: it has no ID_LIKE, so a "rhel" entry never matches it.
func OnPlatform(p Platform, list []OS) bool {
	for _, e := range list {
		if e.ID == "*" {
			return true
		}
	}
	data, err := p.OSRelease()
	if err != nil {
		return false
	}
	fields := parseOSRelease(data)
	major, _, _ := strings.Cut(fields["VERSION_ID"], ".")
	n, verr := strconv.ParseUint(major, 10, 31)
	for _, e := range list {
		family := fields["ID"] == e.ID
		if e.Like {
			for _, w := range strings.Fields(fields["ID_LIKE"]) {
				family = family || w == e.ID
			}
		}
		if family && (e.MinMajor == 0 || (verr == nil && n >= uint64(e.MinMajor))) {
			return true
		}
	}
	return false
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
