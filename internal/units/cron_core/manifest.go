package croncore

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/group"
)

// cronFiles are the files whose digests identify a release (spec §0).
var cronFiles = []string{
	"puppet/type/cron.rb",
	"puppet/provider/cron/crontab.rb",
	"puppet/provider/cron/filetype.rb",
}

// Manifest describes the cron_core unit: puppetlabs-cron_core 1.3.0, the
// release openvox 8.33.0 vendors, and 2.1.0, the release SIMP pins. It
// requires group because it routes with group.ForeignProvider.
// Platforms is the list its parity was measured on.
var Manifest = unit.Manifest{
	Name:      "cron_core",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppetlabs-cron_core", Version: ""},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:     []string{"cron"},
	Requires:  []string{"group"},
	Platforms: []ral.OS{{ID: "rhel", Like: true, MinMajor: 10}},
	Releases: []unit.Release{
		{
			Version: "1.3.0",
			Files: []unit.FileDigest{
				{Path: cronFiles[0], SHA256: "6d53e6173f6fdc2279145885ddd7a952973e2a66a01af9e8d0d05c95464d160b"},
				{Path: cronFiles[1], SHA256: "b90b1118f0b3f71a2006ef0784e3f1cea4e77df49e1e36b088b54a47c61d9d0f"},
				{Path: cronFiles[2], SHA256: "a92c80ee65a9771baf130778ccca0a6a531cffe984b0bb6ce4f4a09fee0a08cc"},
			},
			Types: map[string][]string{"cron": cronFiles},
		},
		{
			Version: "2.1.0",
			Files: []unit.FileDigest{
				{Path: cronFiles[0], SHA256: "a5f7712895d0370077b24e8cfcd47ab16f57e4df9016257258fa7b01168a903c"},
				{Path: cronFiles[1], SHA256: "e823f00e5155c87d2f2acaedf109be12c0b13b3781e6bc05d7285f1cdc4dba4a"},
				{Path: cronFiles[2], SHA256: "f82a87886c2ff0b72a0e43ebd0bb6edecda0362ddcbc672bc51245f2b88266ce"},
			},
			Types: map[string][]string{"cron": cronFiles},
		},
	},
}

// Unit is the cron_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit (spec §2, D-94). Cron is native when the catalog
// holds one and, in this order: the platform is the measured one; crontab
// resolves over PATH as Puppet::Util.which does; no Cron names another
// provider; the plugin tree holds no cron_core file (openvox's vendored
// 1.3.0) or exactly one measured release and no second cron provider; no
// Resources resource purges cron; and no Cron has a value shape the native
// parser does not implement. The first condition that fails is the reason the
// type goes to the extension host. A catalog with no Cron gets an empty
// Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var crons []catalog.Resource
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, "cron") {
			crons = append(crons, r)
		}
	}
	if len(crons) == 0 {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{"cron": why}} }

	if !ral.OnPlatform(env.Platform, Manifest.Platforms) {
		return delegate("not the measured platform")
	}
	crontab, ok := command.RubyWhich("crontab", env.Environ("PATH"), env.Platform.Executable)
	if !ok {
		return delegate("crontab not found on PATH")
	}
	for _, r := range crons {
		if why := group.ForeignProvider(r, "crontab"); why != "" {
			return delegate(why)
		}
	}
	release, why := pluginRelease(env.Tree)
	if why != "" {
		return delegate(why)
	}
	if unit.ResourcesPurge(env.Catalog, "cron") {
		return delegate("resources purge of cron")
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range crons {
		_, err := parseCron(r, release.Version, encoding, func() (string, error) { return "", nil })
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
	}
	return unit.Decision{Providers: []ral.Provider{
		newCron(release.Version, crontab, encoding, env.LookupEnv, command.HostShadowRunner{}, nameservice.Host, env.Stderr),
	}}
}

// pluginRelease is the cron_core release the plugin tree selects (spec §2
// condition 4), or the reason none is measured. A tree with none of the
// pinned files leaves Ruby the vendored 1.3.0; a tree with any must match a
// release in full. Either way the provider directory may hold no .rb beyond
// crontab.rb and filetype.rb: a second provider changes Ruby's choice.
func pluginRelease(tree ral.PluginTree) (unit.Release, string) {
	release := Manifest.Releases[0]
	held := false
	for _, f := range cronFiles {
		if _, err := tree.Read(f); !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
	}
	if held {
		var why string
		var ok bool
		if release, why, ok = unit.MatchFiles(tree, Manifest, "cron", nil); !ok {
			return unit.Release{}, why
		}
	}
	names, err := tree.List("puppet/provider/cron")
	if err != nil && (held || !errors.Is(err, fs.ErrNotExist)) {
		return unit.Release{}, "puppet/provider/cron: " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "crontab.rb" && n != "filetype.rb" {
			return unit.Release{}, "another cron provider in libdir"
		}
	}
	return release, ""
}
