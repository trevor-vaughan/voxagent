package mountcore

import (
	"errors"
	"io/fs"
	"regexp"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/file"
	"github.com/voxagent/voxagent/internal/units/group"
)

// mountFiles are the files whose digests identify the release (spec §2.2).
var mountFiles = []string{
	"puppet/type/mount.rb",
	"puppet/provider/mount/parsed.rb",
	"puppet/provider/mount.rb",
}

// Manifest describes the mount_core unit: puppetlabs-mount_core 1.3.0, the
// release openvox 8.34.0 vendors, and 2.1.0, the release SIMP pins. They differ
// only in Ruby 3.1 hash shorthand, so both run the same native code. It
// requires group for group.ForeignProvider and file for the flat hooks.
// Platforms is the list its parity was measured on.
var Manifest = unit.Manifest{
	Name:      "mount_core",
	Version:   "1.0.0",
	Source:    unit.Origin{Implementation: "puppetlabs-mount_core", Version: ""},
	Oracle:    unit.Origin{Implementation: "openvox", Version: "8.34.0"},
	Types:     []string{"mount"},
	Requires:  []string{"group", "file"},
	Platforms: []ral.OS{{ID: "rhel", Like: true, MinMajor: 10}},
	Releases: []unit.Release{{
		Version: "1.3.0",
		Files: []unit.FileDigest{
			{Path: mountFiles[0], SHA256: "0939199693c7c8f3bfe76c4dce93b62623011b1de4e34606df054422d09a5a25"},
			{Path: mountFiles[1], SHA256: "583208bfca8e5afac71efec73cd715b49155ffe6a73af2eb50e8a775f60ec168"},
			{Path: mountFiles[2], SHA256: "9c2ce747e83d5398ac51cd5f5f9abe80860c75c61f7650ddc3fe39bf3c07bea1"},
		},
		Types: map[string][]string{"mount": mountFiles},
	}, {
		Version: "2.1.0",
		Files: []unit.FileDigest{
			{Path: mountFiles[0], SHA256: "ad59cd27fb9611a38407ece68e1d128c05fe6dba6f5be5af979c4c50eda43d7e"},
			{Path: mountFiles[1], SHA256: "9ce69a02f80ad9e04c014bec2d555240dca26e5226e77f2ea0695bc96c293230"},
			{Path: mountFiles[2], SHA256: "9c2ce747e83d5398ac51cd5f5f9abe80860c75c61f7650ddc3fe39bf3c07bea1"},
		},
		Types: map[string][]string{"mount": mountFiles},
	}},
}

// Unit is the mount_core unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// Route implements unit.Unit (spec §2.1). Mount is native when the catalog
// holds one and, in this order: the platform is the measured one; mount and
// umount resolve over PATH as Puppet::Util.which does (the provider's confine:
// without them Ruby fails the whole run, M11, and delegating hands that
// outcome to the host); no Mount names a provider other than parsed; the
// plugin tree holds no mount_core file (openvox's vendored 1.3.0) or a
// measured release and no second mount provider; no Resources resource purges
// mount; no Mount has a value shape the native parser does not implement; and,
// when the catalog holds a File, every Mount name is a pattern RE2 reads as
// Onigmo does. The first condition that fails is the reason the type goes to
// the extension host. A catalog with no Mount gets an empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var mounts []catalog.Resource
	var files []string
	for _, r := range env.Catalog.Resources {
		switch {
		case strings.EqualFold(r.Type, "mount"):
			mounts = append(mounts, r)
		case strings.EqualFold(r.Type, "file"):
			files = append(files, file.FilePath(r))
		}
	}
	if len(mounts) == 0 {
		return unit.Decision{}
	}
	delegate := func(why string) unit.Decision { return unit.Decision{Delegated: map[string]string{"mount": why}} }

	if !ral.OnPlatform(env.Platform, Manifest.Platforms) {
		return delegate("not the measured platform")
	}
	path := env.Environ("PATH")
	mountPath, ok := command.RubyWhich("mount", path, env.Platform.Executable)
	if !ok {
		return delegate("mount not found on PATH")
	}
	umountPath, ok := command.RubyWhich("umount", path, env.Platform.Executable)
	if !ok {
		return delegate("umount not found on PATH")
	}
	for _, r := range mounts {
		if why := group.ForeignProvider(r, "parsed"); why != "" {
			return delegate(why)
		}
	}
	release, why := pluginRelease(env.Tree)
	if why != "" {
		return delegate(why)
	}
	if unit.ResourcesPurge(env.Catalog, "mount") {
		return delegate("resources purge of mount")
	}
	encoding := rubyvalue.ExternalEncoding(env.Environ)
	var uv *rubyvalue.UnimplementedValue
	names := make([]string, 0, len(mounts))
	for _, r := range mounts {
		p, err := parseMount(r, release, encoding)
		if errors.As(err, &uv) {
			return delegate(err.Error())
		}
		// An invalid Mount stays out of condition 7: Ruby refuses it at catalog
		// conversion, before the graph build compiles any autobefore pattern.
		if err == nil {
			names = append(names, p.Name)
		}
	}
	if len(files) > 0 {
		for _, n := range names {
			if !portablePattern(n) {
				return delegate("mount name " + n + " is not a portable regular expression")
			}
		}
	}
	m := newMount(release, encoding, mountPath, umountPath, command.HostShadowRunner{}, file.BucketBackup(env.Vardir), file.RelabelHook)
	m.files = files
	return unit.Decision{Providers: []ral.Provider{m}}
}

// portablePattern reports whether a mount name, which Ruby interpolates raw
// into the autobefore regexp (mt:311), means the same to RE2 as to Onigmo: it
// holds none of the characters where the two differ (`\`, `(?`, `{`, `[`) and
// compiles with (?m) in the form mount_relations.go builds.
func portablePattern(name string) bool {
	if strings.ContainsAny(name, `\{[`) || strings.Contains(name, "(?") {
		return false
	}
	_, err := regexp.Compile(`(?m)^` + name + `/.`)
	return err == nil
}

// pluginRelease is the mount_core release the plugin tree selects (spec §2.1
// condition 4), or the reason none is measured. A tree with none of the pinned
// files leaves Ruby the vendored 1.3.0; a tree with any must match a release
// in full. Either way the provider directory may hold no .rb beyond parsed.rb:
// a second provider changes Ruby's choice.
func pluginRelease(tree ral.PluginTree) (string, string) {
	release := Manifest.Releases[0]
	held := false
	for _, f := range mountFiles {
		if _, err := tree.Read(f); !errors.Is(err, fs.ErrNotExist) {
			held = true
		}
	}
	if held {
		var why string
		var ok bool
		if release, why, ok = unit.MatchFiles(tree, Manifest, "mount", nil); !ok {
			return "", why
		}
	}
	names, err := tree.List("puppet/provider/mount")
	if err != nil && (held || !errors.Is(err, fs.ErrNotExist)) {
		return "", "puppet/provider/mount: " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "parsed.rb" {
			return "", "another mount provider in libdir"
		}
	}
	return release.Version, ""
}
