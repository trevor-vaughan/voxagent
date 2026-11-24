// Package augeasprovidersssh is the native ssh_config, sshd_config and
// sshd_config_subsystem types of augeasproviders_ssh.
//
// Changelog: 1.0.0 — moved from internal/ral.
package augeasprovidersssh

import (
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// coreFiles are the files of augeasproviders_core every type of the unit
// depends on, as the manifest of that unit lists them.
var coreFiles = []string{
	"augeasproviders_core:puppet/provider/augeasprovider/default.rb",
	"augeasproviders_core:puppet/type/augeasprovider.rb",
	"augeasproviders_core:puppet/feature/augeas.rb",
}

// typeFiles is the list a release holds for typ: the core files, then typ's
// type and provider, then any extra. sshd_config's extra is the
// sshd_config_match type, which decides the names its autorequire can match
// (sshdMatchNames) and is needed whether or not the catalog holds a match,
// since load-time generation can add one.
func typeFiles(typ string, extra ...string) []string {
	files := append([]string(nil), coreFiles...)
	files = append(files, "puppet/type/"+typ+".rb", "puppet/provider/"+typ+"/augeas.rb")
	return append(files, extra...)
}

// Manifest describes the augeasproviders_ssh unit: the type and provider files
// of augeasproviders_ssh 7.0.0's three types, and its sshd_config_match type
// (spec §0 row 4), with the three files of augeasproviders_core. The digests
// of the core files are in augeasproviders_core's manifest.
var Manifest = unit.Manifest{
	Name:     "augeasproviders_ssh",
	Version:  "1.0.0",
	Source:   unit.Origin{Implementation: "puppet-augeasproviders_ssh", Version: ""},
	Oracle:   unit.Origin{Implementation: "openvox", Version: "8.33.0"},
	Types:    []string{"ssh_config", "sshd_config", "sshd_config_subsystem"},
	Requires: []string{"augeasproviders_core"},
	Releases: []unit.Release{{
		Version: "7.0.0",
		Files: []unit.FileDigest{
			{Path: "puppet/type/ssh_config.rb", SHA256: "d1486d319d5b8f036d5d0f308503f2e5cb46ae45527371a7637585a58b582884"},
			{Path: "puppet/provider/ssh_config/augeas.rb", SHA256: "9df8e00affdfe326090333b578eb91235e4a011c72d4cd3547601fd9d15c7221"},
			{Path: "puppet/type/sshd_config.rb", SHA256: "5b7cf51863c22995039d6db35e67f43c7e273c886a668ec1365428ed5baa1350"},
			{Path: "puppet/provider/sshd_config/augeas.rb", SHA256: "322463ac709f044adcbfd6ff5b20216490c928e31d26a8622fc9266bafd4374f"},
			{Path: "puppet/type/sshd_config_subsystem.rb", SHA256: "e5d4c56466d580bdf5943ae89d312916b8d8de3fc473a8a3012bb8240e5fc58b"},
			{Path: "puppet/provider/sshd_config_subsystem/augeas.rb", SHA256: "237148d020f7c3b53d8414863afc7c76dc85f4e3241b685d04af1cf791f13c9c"},
			{Path: "puppet/type/sshd_config_match.rb", SHA256: "dedfb3f7e0cf6250fe1454c247c5b3d7968c80b673f6e704b5314da34a33ccd7"},
		},
		Types: map[string][]string{
			"ssh_config":            typeFiles("ssh_config"),
			"sshd_config":           typeFiles("sshd_config", "puppet/type/sshd_config_match.rb"),
			"sshd_config_subsystem": typeFiles("sshd_config_subsystem"),
		},
	}},
}

// Unit is the augeasproviders_ssh unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// sshTypes are the types of the unit in routing order, each with its parser,
// whose *rubyvalue.UnimplementedValue is §2 (d)-(f).
var sshTypes = []struct {
	typ   string
	parse func(catalog.Resource) error
}{
	{"ssh_config", func(r catalog.Resource) error { _, err := parseSshConfig(r); return err }},
	{"sshd_config", func(r catalog.Resource) error { _, err := parseSshdConfig(r); return err }},
	{"sshd_config_subsystem", func(r catalog.Resource) error { _, err := parseSubsystem(r); return err }},
}

// Route implements unit.Unit. Each of ssh_config, sshd_config and
// sshd_config_subsystem the catalog holds is applied natively, over the run the
// augeasproviders types of the run share, unless a condition sends it to the
// extension host, which is the oracle for everything not measured (spec §2),
// with the first condition that failed for it as its reason. Only the types the
// catalog holds are routed, and with none of them nothing is read or probed.
//
// The conditions run in spec §2's order, each type decided on its own: (a) the
// files of the manifest match and the type's provider directory holds no .rb
// but augeas.rb; (c) ral.AugeasRunChecks; (b) the run's probe accepts the
// library, made only when a type is left and only after (c); (d)-(f) no
// resource of the type has a shape the native code does not implement (Ruby's
// own refusals stay native).
func (unitImpl) Route(env *unit.Env) unit.Decision {
	var d unit.Decision
	candidate := map[string]bool{}
	for _, st := range sshTypes {
		candidate[st.typ] = ral.HeldAugeas(env.Catalog, st.typ)
	}
	delegate := func(typ, why string) {
		if candidate[typ] {
			candidate[typ] = false
			if d.Delegated == nil {
				d.Delegated = map[string]string{}
			}
			d.Delegated[typ] = why
		}
	}
	delegateAll := func(why string) {
		for _, st := range sshTypes {
			delegate(st.typ, why)
		}
	}
	anyLeft := func() bool {
		for _, left := range candidate {
			if left {
				return true
			}
		}
		return false
	}
	if !anyLeft() {
		return d
	}

	// (a)
	for _, st := range sshTypes {
		if !candidate[st.typ] {
			continue
		}
		if _, why, ok := unit.MatchFiles(env.Tree, Manifest, st.typ, []unit.Manifest{core.Manifest}); !ok {
			delegate(st.typ, why)
		} else if why := ral.AugeasProviderDir(env.Tree, st.typ); why != "" {
			delegate(st.typ, why)
		}
	}

	// (c)
	if why := ral.AugeasRunChecks(env.Environ, env.Libdir); why != "" {
		delegateAll(why)
	}

	// (b)
	if anyLeft() {
		if _, why, ok := env.Augeas.Probe(); !ok {
			delegateAll(why)
		}
	}

	// (d)-(f)
	var uv *rubyvalue.UnimplementedValue
	for _, r := range env.Catalog.Resources {
		for _, st := range sshTypes {
			if candidate[st.typ] && strings.EqualFold(r.Type, st.typ) {
				if err := st.parse(r); errors.As(err, &uv) {
					delegate(st.typ, err.Error())
				}
			}
		}
	}

	if !anyLeft() {
		return d
	}
	run := core.SharedRun(env.Augeas, env.Libdir)
	if candidate["ssh_config"] {
		d.Providers = append(d.Providers, SshConfig{run: run})
	}
	if candidate["sshd_config"] {
		d.Providers = append(d.Providers, SshdConfig{run: run, cat: env.Catalog})
	}
	if candidate["sshd_config_subsystem"] {
		d.Providers = append(d.Providers, SshdConfigSubsystem{run: run})
	}
	return d
}
