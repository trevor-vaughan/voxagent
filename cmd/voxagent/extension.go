package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/ral"
)

// extensionConfig is the command line's say over the extension host.
type extensionConfig struct {
	ruby, libdir string
	// vardir is the run's Puppet vardir, which the host shares.
	vardir   string
	level    string // the run's level, which the host's Puppet logs at
	disabled bool
	timeouts extension.Timeouts
}

// extensionIngest is what the extension host contributed to a run. A zero
// value (host == nil) means the catalog needed no host.
type extensionIngest struct {
	host       *extension.Host
	load       extension.LoadResult
	loadTook   time.Duration // how long host.Load took
	hostLoaded bool          // host.Load returned: Ruby's generate already ran on every vertex
	providers  []ral.Provider
	// invalid are Puppet's own validation refusals, for the validation block.
	invalid []extension.Refusal
}

// ingestExtensions classifies the catalog's non-native types and, only when
// some exist, starts the host, loads the catalog into it and builds a provider
// for each legacy (tier 3) type. A non-empty errMsg means the run cannot
// start; it is the stderr line to print. The caller must Close a non-nil host.
func ingestExtensions(cfg extensionConfig, raw []byte, cat *catalog.Catalog,
	natives []ral.Provider) (in extensionIngest, errMsg string) {
	claimed := make(map[string]struct{}, len(natives))
	for _, p := range natives {
		claimed[strings.ToLower(p.Type())] = struct{}{}
	}
	var refs []string
	for _, r := range cat.Resources {
		refs = append(refs, r.Ref())
	}
	needed := nonNativeTypes(cat, claimed)
	if len(needed) == 0 {
		return in, ""
	}

	if cfg.disabled {
		return in, fmt.Sprintf("voxagent: --no-ruby: type(s) %s need the extension host; nothing applied",
			strings.Join(needed, ", "))
	}
	host, err := extension.Launch(extension.Config{Ruby: cfg.ruby, Timeouts: cfg.timeouts}, os.Environ())
	if err != nil {
		return in, fmt.Sprintf("voxagent: the extension host is unavailable: %v; nothing applied", err)
	}
	in.host = host
	loading := time.Now()
	load, err := host.Load(raw, refs, cfg.libdir, cfg.vardir, cfg.level)
	in.loadTook = time.Since(loading)
	if err != nil {
		return in, fmt.Sprintf("voxagent: the extension host could not load the catalog: %v; nothing applied", err)
	}
	in.load = load
	in.hostLoaded = true
	generated, err := convertGeneration(load.Generated)
	if err == nil {
		err = catalog.MergeGeneration(cat, generated, catalog.PlaceAppend)
	}
	if err != nil {
		return in, fmt.Sprintf("voxagent: the extension host's generated resources could not be merged: %v; nothing applied", err)
	}
	// A refused load's order omits what Puppet refused, and its run never starts.
	if len(load.Invalid) == 0 {
		if err := catalog.ApplyOrder(cat, load.Order); err != nil {
			return in, fmt.Sprintf("voxagent: the extension host's catalog order could not be applied: %v; nothing applied", err)
		}
	}
	// Generation may add types the catalog did not name.
	needed = nonNativeTypes(cat, claimed)
	// Puppet logs these during conversion, before the report is a log destination.
	for _, l := range load.Logs {
		fmt.Fprintf(os.Stderr, "voxagent: extension host: %s: %s\n", l.Level, l.Message)
	}

	tier3 := map[string]struct{}{}
	var tier3Refs []string
	for _, typ := range needed {
		info := load.Types[typ]
		switch info.Class {
		case "tier3":
			tier3[typ] = struct{}{}
			n := 0
			for _, r := range cat.Resources {
				if strings.EqualFold(r.Type, typ) {
					tier3Refs = append(tier3Refs, r.Ref())
					n++
				}
			}
			fmt.Fprintf(os.Stderr, "voxagent: %d %s resource(s) applied via extension host (%s)\n",
				n, typ, info.Origin)
		case "tier2":
			fmt.Fprintf(os.Stderr, "voxagent: %s is a Resource API type; Resource API types are not implemented\n", typ)
		case "missing":
			// Pre-flight already reports a type no provider claims.
		}
	}
	if len(tier3) == 0 {
		return in, ""
	}
	in.invalid = load.Invalid
	// A refused catalog never runs, and Puppet has no relationships for the
	// resources it refused, so asking could only mask the refusal lines.
	var relations map[string]extension.Relations
	if len(in.invalid) == 0 {
		var err error
		if relations, err = host.Relationships(tier3Refs); err != nil {
			return in, fmt.Sprintf("voxagent: the extension host could not read relationships: %v; nothing applied", err)
		}
	}
	for _, typ := range needed {
		if _, ok := tier3[typ]; !ok {
			continue
		}
		in.providers = append(in.providers, ral.NewExtension(typ, host, load, relations, cat))
	}
	return in, ""
}

// nonNativeTypes returns, sorted and lowercase, the types of cat's resources
// that neither a native provider claims nor the engine treats as a container.
// Filebucket is left out: pre-flight refuses it whatever the host could do
// (ral.Filebucket), so the host must neither start for it nor claim it.
func nonNativeTypes(cat *catalog.Catalog, claimed map[string]struct{}) []string {
	seen := map[string]struct{}{}
	for _, r := range cat.Resources {
		typ := strings.ToLower(r.Type)
		if _, native := claimed[typ]; !native && typ != ral.Filebucket && !ral.IsContainerResource(r) {
			seen[typ] = struct{}{}
		}
	}
	types := make([]string, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Strings(types)
	return types
}

// selfReport prints the host's own account of its cost, after the run.
func selfReport(host *extension.Host) {
	s := host.Stats()
	fmt.Fprintf(os.Stderr, "voxagent: extension host: started in %s, evaluated %d resource(s), %s spent in the host\n",
		s.Startup.Round(time.Millisecond), s.Evaluated, s.Spent.Round(time.Millisecond))
}
