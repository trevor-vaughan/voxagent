package sshkeyscore

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

// Each type is fingerprinted by its own two files (spec §2.2).
var (
	sshkeyFiles  = []string{"puppet/type/sshkey.rb", "puppet/provider/sshkey/parsed.rb"}
	authKeyFiles = []string{"puppet/type/ssh_authorized_key.rb", "puppet/provider/ssh_authorized_key/parsed.rb"}
)

// Manifest describes the sshkeys_core unit: puppetlabs-sshkeys_core 2.5.0, the
// release openvox 8.34.0 vendors, and 3.1.0, the release SIMP pins. They differ
// only in the two type files; the release found selects sshkey's key
// validation. It requires group for group.ForeignProvider, file for the flat
// hooks and user for ssh_authorized_key's purge_ssh_keys condition. Only
// sshkey is routed so far, so Types lists only it; the ssh_authorized_key
// checkpoint adds that type to Types and to each release's Types map.
var Manifest = unit.Manifest{
	Name:     "sshkeys_core",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppetlabs-sshkeys_core", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:    []string{"sshkey"},
	Requires: []string{"group", "file", "user"},
	Releases: []unit.Release{{
		Version: "2.5.0",
		Files: []unit.FileDigest{
			{Path: sshkeyFiles[0], SHA256: "1583904b917788b8f8a80304089c8971f572b851a2a557a0e1fa8736e57dedf8"},
			{Path: sshkeyFiles[1], SHA256: "2f8d4fe1dc69c2c5f362cfbe044e5bd477c4700d079a817557ca7e08d660e1cf"},
			{Path: authKeyFiles[0], SHA256: "694d7468678ebe894fa022b7f4f38f9c64031a5235bc462d4af41f6ad758de5a"},
			{Path: authKeyFiles[1], SHA256: "86f01f5ed68cb89e193b70291712bd2417df7b6a7a9e382cfeecfb4ea1810334"},
		},
		Types: map[string][]string{"sshkey": sshkeyFiles},
	}, {
		Version: "3.1.0",
		Files: []unit.FileDigest{
			{Path: sshkeyFiles[0], SHA256: "a16318375affd95b2e368d69b5ede6cae8d8639fcad85277bdabca0d0e2ed8da"},
			{Path: sshkeyFiles[1], SHA256: "2f8d4fe1dc69c2c5f362cfbe044e5bd477c4700d079a817557ca7e08d660e1cf"},
			{Path: authKeyFiles[0], SHA256: "48dd2f61019e1e163e7d354c4618e7466dfd93784c38b9b9afe2180002ea1a06"},
			{Path: authKeyFiles[1], SHA256: "86f01f5ed68cb89e193b70291712bd2417df7b6a7a9e382cfeecfb4ea1810334"},
		},
		Types: map[string][]string{"sshkey": sshkeyFiles},
	}},
}

// vendoredRelease is the release Ruby runs when the plugin tree holds none of
// a type's files.
const vendoredRelease = "2.5.0"

// Unit is the sshkeys_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit (spec §2.1). Sshkey is native when the catalog
// holds one and, in this order: the platform is the measured one (the type has
// no confine, condition 2); no Sshkey names a provider other than parsed; the
// plugin tree holds no sshkey file (openvox's vendored 2.5.0) or a measured
// release and no second sshkey provider; no Resources resource purges sshkey;
// and no Sshkey has a value shape the native parser does not implement under
// the release found. The first condition that fails is the reason the type
// goes to the extension host. A catalog with no Sshkey gets an empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var keys []catalog.Resource
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, "sshkey") {
			keys = append(keys, r)
		}
	}
	if len(keys) == 0 {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{"sshkey": why}} }

	if !ral.MeasuredRelease(env.Platform) {
		return delegate("not the measured platform")
	}
	for _, r := range keys {
		if why := group.ForeignProvider(r, "parsed"); why != "" {
			return delegate(why)
		}
	}
	release, why := sshkeyRelease(env.Tree)
	if why != "" {
		return delegate(why)
	}
	if unit.ResourcesPurge(env.Catalog, "sshkey") {
		return delegate("resources purge of sshkey")
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range keys {
		_, err := parseSshkey(r, release, encoding)
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
	}
	return unit.Decision{Providers: []ral.Provider{
		newSshkey(release, encoding, file.BucketBackup(env.Vardir), file.RelabelHook),
	}}
}

// sshkeyRelease is the sshkeys_core release the plugin tree runs sshkey under,
// or the reason it is not measured (spec §2.1 condition 4). A tree with none of
// sshkey's pinned files leaves Ruby the vendored release; a tree with any must
// match a release in full. Either way the provider directory may hold no .rb
// beyond parsed.rb: a second provider changes Ruby's choice.
func sshkeyRelease(tree ral.PluginTree) (release, why string) {
	release = vendoredRelease
	held := false
	for _, f := range sshkeyFiles {
		if _, err := tree.Read(f); !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
	}
	if held {
		rel, why, ok := unit.MatchFiles(tree, Manifest, "sshkey", nil)
		if !ok {
			return "", why
		}
		release = rel.Version
	}
	names, err := tree.List("puppet/provider/sshkey")
	if err != nil && (held || !errors.Is(err, fs.ErrNotExist)) {
		return "", "puppet/provider/sshkey: " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "parsed.rb" {
			return "", "another sshkey provider in libdir"
		}
	}
	return release, ""
}
