package concat

import (
	"strings"

	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// Manifest describes the concat unit: puppetlabs-concat's two type files,
// measured at the two releases the unit reproduces (spec §0 row 2). Both type
// files of a release must match its digests.
var Manifest = unit.Manifest{
	Name:      "concat",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppetlabs-concat", Version: ""},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:     []string{"concat_file", "concat_fragment"},
	Requires:  []string{"file"},
	Platforms: unit.AnyPlatform,
	Releases: []unit.Release{
		{
			Version: "9.1.0",
			Files: []unit.FileDigest{
				{Path: concatTypeFiles[0], SHA256: "7872760598bf35c8e3b285d89daecc4fa29c727a456e47118626a2c58f9c0d6f"},
				{Path: concatTypeFiles[1], SHA256: "ee003b5d10550f6104fde62ad46f5296ce9361a6a4c1e5ef6d973333f30585ef"},
			},
			Types: map[string][]string{"concat_file": concatTypeFiles, "concat_fragment": concatTypeFiles},
		},
		{
			Version: "10.0.1",
			Files: []unit.FileDigest{
				{Path: concatTypeFiles[0], SHA256: "e38f130ed5ccb6bc0bb3c7a047ead0ace40ebcbb8d2f408acf46f10b8b9c59c6"},
				{Path: concatTypeFiles[1], SHA256: "1a53e85c6a873be95eb4b410153e3632658d01c0c65b5f2e0f2886a6f0cb067d"},
			},
			Types: map[string][]string{"concat_file": concatTypeFiles, "concat_fragment": concatTypeFiles},
		},
	},
}

// Unit is the concat unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit. concat_file and concat_fragment are applied
// natively where the plugin tree holds a measured release; otherwise both go to
// the extension host, which is the oracle for everything not measured (spec §0
// rows 1-2). ConcatFile reads the catalog when it generates its content, after
// load-time generation has merged File[path] into this same catalog.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	v, reason, ok := nativeConcat(env.Tree, env.Catalog, Manifest)
	if ok {
		return unit.Decision{Providers: []ral.Provider{ConcatFile{Version: v, Catalog: env.Catalog}, ConcatFragment{Version: v}}}
	}
	var d unit.Decision
	for _, r := range env.Catalog.Resources {
		if typ := strings.ToLower(r.Type); typ == "concat_file" || typ == "concat_fragment" {
			if d.Delegated == nil {
				d.Delegated = map[string]string{}
			}
			d.Delegated[typ] = reason
		}
	}
	return d
}
