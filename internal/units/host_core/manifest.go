package hostcore

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/file"
	"github.com/voxagent/voxagent/internal/units/group"
)

// hostFiles are the files whose digests identify the release (spec §2.2).
var hostFiles = []string{
	"puppet/type/host.rb",
	"puppet/provider/host/parsed.rb",
}

// Manifest describes the host_core unit: puppetlabs-host_core 1.3.0, the
// release openvox 8.34.0 vendors; SIMP's 2.1.0 has the same two files byte for
// byte, so one release covers both. It requires group for
// group.ForeignProvider and file for the flat hooks.
var Manifest = unit.Manifest{
	Name:     "host_core",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppetlabs-host_core", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:    []string{"host"},
	Requires: []string{"group", "file"},
	Releases: []unit.Release{{
		Version: "1.3.0",
		Files: []unit.FileDigest{
			{Path: hostFiles[0], SHA256: "e0d448a5835a12a2565d309b348ed071c0f849aeec87372777c948acc6bf1e35"},
			{Path: hostFiles[1], SHA256: "a396f906038d718f6c69cb7a836f13eed16de5e9b2cf9d39a020eed9e9685f7d"},
		},
		Types: map[string][]string{"host": hostFiles},
	}},
}

// Unit is the host_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit (spec §2.1). Host is native when the catalog
// holds one and, in this order: the platform is the measured one; /etc/hosts
// exists, following symlinks (the provider's confine: without it Ruby fails
// the whole run, M16, and delegating hands that outcome to the host); no Host
// names a provider other than parsed; the plugin tree holds no host_core file
// (openvox's vendored 1.3.0) or the measured release and no second host
// provider; no Resources resource purges host; and no Host has a value shape
// the native parser does not implement. The first condition that fails is the
// reason the type goes to the extension host. A catalog with no Host gets an
// empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var hosts []catalog.Resource
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, "host") {
			hosts = append(hosts, r)
		}
	}
	if len(hosts) == 0 {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{"host": why}} }

	if !ral.MeasuredRelease(env.Platform) {
		return delegate("not the measured platform")
	}
	if !env.Platform.FileExists("/etc/hosts") {
		return delegate("/etc/hosts does not exist")
	}
	for _, r := range hosts {
		if why := group.ForeignProvider(r, "parsed"); why != "" {
			return delegate(why)
		}
	}
	if why := checkTree(env.Tree); why != "" {
		return delegate(why)
	}
	if unit.ResourcesPurge(env.Catalog, "host") {
		return delegate("resources purge of host")
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range hosts {
		_, err := parseHost(r, encoding)
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
	}
	return unit.Decision{Providers: []ral.Provider{
		newHost(encoding, file.BucketBackup(env.Vardir), file.RelabelHook),
	}}
}

// checkTree is the reason the plugin tree is not measured, or "" (spec §2.1
// condition 4). A tree with none of the pinned files leaves Ruby the vendored
// 1.3.0; a tree with any must match the release in full. Either way the
// provider directory may hold no .rb beyond parsed.rb: a second provider
// changes Ruby's choice.
func checkTree(tree ral.PluginTree) string {
	held := false
	for _, f := range hostFiles {
		if _, err := tree.Read(f); !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
	}
	if held {
		if _, why, ok := unit.MatchFiles(tree, Manifest, "host", nil); !ok {
			return why
		}
	}
	names, err := tree.List("puppet/provider/host")
	if err != nil && (held || !errors.Is(err, fs.ErrNotExist)) {
		return "puppet/provider/host: " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "parsed.rb" {
			return "another host provider in libdir"
		}
	}
	return ""
}
