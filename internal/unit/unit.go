// Package unit is the framework a native Puppet type plugs into. A unit is a
// Go package under internal/units that reproduces one Ruby implementation (its
// Manifest names the origin and the measured file digests) and decides, per
// run, which catalog types it serves natively (Route). Every other type is
// delegated to the Ruby extension host with a reason.
package unit

import (
	"io"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/ral"
)

// Manifest describes a unit. The slices are never nil: a library unit has no
// Types, and the units command prints them as [].
type Manifest struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Source   Origin   `json:"source"`
	Oracle   Origin   `json:"oracle"`
	Types    []string `json:"types"`
	Requires []string `json:"requires"`
	// Platforms lists the operating systems the unit's parity was measured on;
	// a unit that works anywhere declares AnyPlatform. Never empty.
	Platforms []ral.OS  `json:"platforms"`
	Releases  []Release `json:"releases"`
}

// AnyPlatform is the Platforms of a unit with no platform limit. The "*" id
// matches without reading os-release.
var AnyPlatform = []ral.OS{{ID: "*"}}

// Origin names the Ruby implementation a unit reproduces or was measured
// against.
type Origin struct {
	Implementation string `json:"implementation"`
	Version        string `json:"version"`
}

// Release is one measured version of the Ruby files. Types maps a catalog
// type to the files that must match: a path in Files, or "<unit>:<path>" for
// a file listed by a required unit's manifest.
type Release struct {
	Version string              `json:"version"`
	Files   []FileDigest        `json:"files"`
	Types   map[string][]string `json:"types"`
}

// FileDigest is the lowercase hex sha256 of a file under the plugin tree.
type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Unit is a native implementation of one or more Puppet types.
type Unit interface {
	Manifest() Manifest
	Route(env *Env) Decision
}

// Env is what a unit sees when it routes.
type Env struct {
	Catalog  *catalog.Catalog
	Libdir   string
	Tree     ral.PluginTree
	Platform ral.Platform
	Environ  ral.Environ
	Command  string // "apply" or "agent"
	Vardir   string
	Sources  map[string]string
	Augeas   *augeas.Session // the run's libaugeas probe, made at most once
	// LookupEnv is os.LookupEnv over the agent's environment: unlike Environ it
	// tells a variable set to "" from an unset one (cron's $USER, spec §5.2).
	// Environ stays the string-only reader the other units use.
	LookupEnv func(key string) (string, bool)
	// Stderr is where a native provider copies the output of a command it ran
	// (cron's crontab write, D-95). The run scrubs what it receives, so a
	// provider may pass it unfiltered text. Nil in a test that never reaches
	// such a command.
	Stderr io.Writer
}

// Decision is a unit's routing: the providers it serves natively, and the
// reason for each catalog type it leaves to Ruby. Delegated holds only types
// the catalog contains.
type Decision struct {
	Providers []ral.Provider
	Delegated map[string]string
}
