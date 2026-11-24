package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units"
)

const unitsUsage = "usage: voxagent units"

// unitsCommand prints the manifest of every native unit as JSON, sorted by
// name. It takes no arguments and reads nothing from the host.
func unitsCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "voxagent: unexpected argument %q\n", args[0])
		fmt.Fprintln(stderr, unitsUsage)
		return 2
	}
	manifests := []unit.Manifest{}
	for _, u := range units.All() {
		manifests = append(manifests, nonNil(u.Manifest()))
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Name < manifests[j].Name })
	b, err := json.MarshalIndent(manifests, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "voxagent: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return 1
	}
	return 0
}

// nonNil makes every slice and map of m non-nil so none marshals as null.
func nonNil(m unit.Manifest) unit.Manifest {
	if m.Types == nil {
		m.Types = []string{}
	}
	if m.Requires == nil {
		m.Requires = []string{}
	}
	releases := make([]unit.Release, len(m.Releases))
	for i, r := range m.Releases {
		if r.Files == nil {
			r.Files = []unit.FileDigest{}
		}
		if r.Types == nil {
			r.Types = map[string][]string{}
		}
		releases[i] = r
	}
	m.Releases = releases
	return m
}
