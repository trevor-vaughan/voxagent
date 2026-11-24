package sshkeyscore

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/file"
	"github.com/voxagent/voxagent/internal/units/group"
	"github.com/voxagent/voxagent/internal/units/user"
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
// hooks and user for ssh_authorized_key's purge_ssh_keys condition.
var Manifest = unit.Manifest{
	Name:     "sshkeys_core",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppetlabs-sshkeys_core", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:    []string{"sshkey", "ssh_authorized_key"},
	Requires: []string{"group", "file", "user"},
	Releases: []unit.Release{{
		Version: "2.5.0",
		Files: []unit.FileDigest{
			{Path: sshkeyFiles[0], SHA256: "1583904b917788b8f8a80304089c8971f572b851a2a557a0e1fa8736e57dedf8"},
			{Path: sshkeyFiles[1], SHA256: "2f8d4fe1dc69c2c5f362cfbe044e5bd477c4700d079a817557ca7e08d660e1cf"},
			{Path: authKeyFiles[0], SHA256: "694d7468678ebe894fa022b7f4f38f9c64031a5235bc462d4af41f6ad758de5a"},
			{Path: authKeyFiles[1], SHA256: "86f01f5ed68cb89e193b70291712bd2417df7b6a7a9e382cfeecfb4ea1810334"},
		},
		Types: map[string][]string{"sshkey": sshkeyFiles, "ssh_authorized_key": authKeyFiles},
	}, {
		Version: "3.1.0",
		Files: []unit.FileDigest{
			{Path: sshkeyFiles[0], SHA256: "a16318375affd95b2e368d69b5ede6cae8d8639fcad85277bdabca0d0e2ed8da"},
			{Path: sshkeyFiles[1], SHA256: "2f8d4fe1dc69c2c5f362cfbe044e5bd477c4700d079a817557ca7e08d660e1cf"},
			{Path: authKeyFiles[0], SHA256: "48dd2f61019e1e163e7d354c4618e7466dfd93784c38b9b9afe2180002ea1a06"},
			{Path: authKeyFiles[1], SHA256: "86f01f5ed68cb89e193b70291712bd2417df7b6a7a9e382cfeecfb4ea1810334"},
		},
		Types: map[string][]string{"sshkey": sshkeyFiles, "ssh_authorized_key": authKeyFiles},
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

// Route implements unit.Unit (spec §2.1). Each type the catalog holds is routed
// on its own: see routeSshkeyType and routeSakType. A catalog with neither gets an
// empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var keys, auths []catalog.Resource
	for _, r := range env.Catalog.Resources {
		switch {
		case strings.EqualFold(r.Type, "sshkey"):
			keys = append(keys, r)
		case strings.EqualFold(r.Type, "ssh_authorized_key"):
			auths = append(auths, r)
		}
	}
	d := unit.Decision{}
	if len(keys) > 0 {
		routeType(&d, "sshkey", routeSshkeyType(env, keys))
	}
	if len(auths) > 0 {
		routeType(&d, "ssh_authorized_key", routeSakType(env, auths))
	}
	return d
}

// routeType adds one type's outcome to d: its provider, or its delegation reason.
func routeType(d *unit.Decision, typ string, p routed) {
	if p.why != "" {
		if d.Delegated == nil {
			d.Delegated = map[string]string{}
		}
		d.Delegated[typ] = p.why
		return
	}
	d.Providers = append(d.Providers, p.provider)
}

// routed is one type's outcome: a native provider, or the reason it is not.
type routed struct {
	provider ral.Provider
	why      string
}

// routeSshkeyType applies the conditions to Sshkey: the platform is the measured
// one (the type has no confine, condition 2); no Sshkey names a provider other
// than parsed; the plugin tree holds no sshkey file (openvox's vendored 2.5.0)
// or a measured release and no second sshkey provider; no Resources resource
// purges sshkey; and no Sshkey has a value shape the native parser does not
// implement under the release found. The first condition that fails is the
// reason.
func routeSshkeyType(env *unit.Env, keys []catalog.Resource) routed {
	if !ral.MeasuredRelease(env.Platform) {
		return routed{why: "not the measured platform"}
	}
	for _, r := range keys {
		if why := group.ForeignProvider(r, "parsed"); why != "" {
			return routed{why: why}
		}
	}
	release, why := typeRelease(env.Tree, "sshkey", sshkeyFiles)
	if why != "" {
		return routed{why: why}
	}
	if unit.ResourcesPurge(env.Catalog, "sshkey") {
		return routed{why: "resources purge of sshkey"}
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range keys {
		_, err := parseSshkey(r, release, encoding)
		if errors.As(err, &uv) {
			return routed{why: err.Error()}
		}
	}
	return routed{provider: newSshkey(release, encoding, file.BucketBackup(env.Vardir), file.RelabelHook)}
}

// routeSakType applies the same conditions to Ssh_authorized_key, with condition
// 5 widened: a User whose purge_ssh_keys generates Ssh_authorized_key
// resources also delegates, because the host would edit the same files as the
// native provider in one run.
func routeSakType(env *unit.Env, auths []catalog.Resource) routed {
	if !ral.MeasuredRelease(env.Platform) {
		return routed{why: "not the measured platform"}
	}
	for _, r := range auths {
		if why := group.ForeignProvider(r, "parsed"); why != "" {
			return routed{why: why}
		}
	}
	release, why := typeRelease(env.Tree, "ssh_authorized_key", authKeyFiles)
	if why != "" {
		return routed{why: why}
	}
	if unit.ResourcesPurge(env.Catalog, "ssh_authorized_key") {
		return routed{why: "resources purge of ssh_authorized_key"}
	}
	for _, r := range env.Catalog.Resources {
		if strings.EqualFold(r.Type, "user") && !user.PurgeKeepsNative(r) {
			return routed{why: "purge_ssh_keys generates Ssh_authorized_key resources"}
		}
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range auths {
		_, err := parseSak(r, release, encoding)
		if errors.As(err, &uv) {
			return routed{why: err.Error()}
		}
	}
	return routed{provider: newSak(release, encoding, nameservice.Host, file.BucketBackup(env.Vardir), file.RelabelHook,
		asuser.ProcStarter{}, os.Geteuid)}
}

// typeRelease is the sshkeys_core release the plugin tree runs typ under, or
// the reason it is not measured (spec §2.1 condition 4). A tree with none of
// the type's pinned files leaves Ruby the vendored release; a tree with any
// must match a release in full. Either way the provider directory may hold no
// .rb beyond parsed.rb: a second provider changes Ruby's choice.
func typeRelease(tree ral.PluginTree, typ string, files []string) (release, why string) {
	release = vendoredRelease
	held := false
	for _, f := range files {
		if _, err := tree.Read(f); !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
	}
	if held {
		rel, why, ok := unit.MatchFiles(tree, Manifest, typ, nil)
		if !ok {
			return "", why
		}
		release = rel.Version
	}
	dir := "puppet/provider/" + typ
	names, err := tree.List(dir)
	if err != nil && (held || !errors.Is(err, fs.ErrNotExist)) {
		return "", dir + ": " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "parsed.rb" {
			return "", "another " + typ + " provider in libdir"
		}
	}
	return release, ""
}
