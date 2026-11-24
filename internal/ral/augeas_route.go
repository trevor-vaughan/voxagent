package ral

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// AugeasTypes is NativeAugeas' answer: which of the four augeasproviders
// types, and the generic augeas type of augeas_core, are applied natively. Reasons holds, for each type the catalog holds
// that goes to the extension host, the first condition that sent it there
// (spec §2's debug line).
type AugeasTypes struct {
	Sysctl, SshConfig, SshdConfig, SshdConfigSubsystem, Augeas bool
	Reasons                                                    map[string]string

	lib      augLib // the probed library, when any type is native
	libdir   string
	lensPath string           // the generic augeas type's lens path: "" or <libdir>/augeas/lenses
	sysctl   string           // the sysctl command, when Sysctl is native
	cat      *catalog.Catalog // the run's catalog, which SshdConfig's autorequire reads
}

// Environ is os.Getenv: the value of key in voxagent's environment, "" when
// unset. It is a function so tests do not depend on the agent's environment.
type Environ func(key string) string

// augeasFingerprints are the sha256 digests of the plugin-tree files whose
// code the native types reimplement (spec §0 row 4): augeasproviders_core
// 4.2.0's three, then each type's type and provider file, the
// sshd_config_match type, whose key attributes decide the names sshd_config's
// autorequire can match (sshdMatchNames), and augeas_core 2.1.0's type,
// provider and parser. puppet/feature/augeas.rb is byte-identical in both
// modules, so one digest serves both.
var augeasFingerprints = map[string]string{
	"puppet/provider/augeasprovider/default.rb":       "3900c7e789d651b9f5271087ef3a3b3a9b08cfef2b725d30b559bfff280af42a",
	"puppet/type/augeasprovider.rb":                   "0cd37a04489790a03adcae071cf3ee62a81ba238c4ce4959314092d1ab69e795",
	"puppet/feature/augeas.rb":                        "d98e8ef39433632b9e835bbb3e85a75f90a8d17a33c2540f2e14f851ca2791b3",
	"puppet/type/sysctl.rb":                           "e3e75816ce52ca14acbf78252e2dc0ae98867e5d3b81637a5d4e29fe2803d863",
	"puppet/provider/sysctl/augeas.rb":                "25ca6b921df6408d69107cba0dcc807c0ff996f8d2e6f2d7b381fa0ef285031d",
	"puppet/type/ssh_config.rb":                       "d1486d319d5b8f036d5d0f308503f2e5cb46ae45527371a7637585a58b582884",
	"puppet/provider/ssh_config/augeas.rb":            "9df8e00affdfe326090333b578eb91235e4a011c72d4cd3547601fd9d15c7221",
	"puppet/type/sshd_config.rb":                      "5b7cf51863c22995039d6db35e67f43c7e273c886a668ec1365428ed5baa1350",
	"puppet/provider/sshd_config/augeas.rb":           "322463ac709f044adcbfd6ff5b20216490c928e31d26a8622fc9266bafd4374f",
	"puppet/type/sshd_config_subsystem.rb":            "e5d4c56466d580bdf5943ae89d312916b8d8de3fc473a8a3012bb8240e5fc58b",
	"puppet/provider/sshd_config_subsystem/augeas.rb": "237148d020f7c3b53d8414863afc7c76dc85f4e3241b685d04af1cf791f13c9c",
	"puppet/type/sshd_config_match.rb":                "dedfb3f7e0cf6250fe1454c247c5b3d7968c80b673f6e704b5314da34a33ccd7",
	"puppet/type/augeas.rb":                           "1b8fe183607c21efb9a3a6431a8644d1dbb7e061a83b03bdd6744c1d9549b071",
	"puppet/provider/augeas/augeas.rb":                "4eedaf2d530bc89909b97f36f552cf820bfbe9808040e6c9caa748a10df31c93",
	"puppet_x/augeas/util/parser.rb":                  "586daa7ddf5a741286f805c90c5e9fc464bb91f184c0c746231bbc5ca0374a7a",
}

// augeasprovidersCore are the fingerprinted files every augeasproviders type
// depends on; the generic augeas type needs augeas_core's files instead.
var augeasprovidersCore = []string{
	"puppet/provider/augeasprovider/default.rb",
	"puppet/type/augeasprovider.rb",
	"puppet/feature/augeas.rb",
}

// augeasRouted are the five types in routing order, each with its parser,
// whose *unimplementedValue is §2 (d)-(f), and the fingerprinted files it
// depends on: its module's shared files (augeasprovidersCore for the four
// augeasproviders types, augeas_core's parser and feature for augeas), then,
// besides its own type and provider, any extra. sshd_config's extra is the
// sshd_config_match type, needed whether or not the catalog holds a match,
// since load-time generation can add one.
var augeasRouted = []struct {
	typ    string
	parse  func(catalog.Resource) error
	shared []string
	extra  []string
}{
	{"sysctl", func(r catalog.Resource) error { _, err := parseSysctl(r); return err }, augeasprovidersCore, nil},
	{"ssh_config", func(r catalog.Resource) error { _, err := parseSshConfig(r); return err }, augeasprovidersCore, nil},
	{"sshd_config", func(r catalog.Resource) error { _, err := parseSshdConfig(r); return err }, augeasprovidersCore, []string{"puppet/type/sshd_config_match.rb"}},
	{"sshd_config_subsystem", func(r catalog.Resource) error { _, err := parseSubsystem(r); return err }, augeasprovidersCore, nil},
	{"augeas", func(r catalog.Resource) error { _, err := parseAugeas(r); return err }, []string{"puppet_x/augeas/util/parser.rb", "puppet/feature/augeas.rb"}, nil},
}

// augeasEnvironment are the variables that send every type to the host
// when set and non-empty (spec §2 (c)): libaugeas reads the AUGEAS_* ones,
// and either LD_* one could make voxagent load other code than Ruby does.
var augeasEnvironment = []string{"AUGEAS_ROOT", "AUGEAS_LENS_LIB", "AUGEAS_DEBUG", "AUGEAS_DEBUG_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH"}

// NativeAugeas says which of sysctl, ssh_config, sshd_config,
// sshd_config_subsystem and augeas are applied natively for cat; each other
// type the catalog holds goes to the extension host, which is the oracle for
// everything not measured (spec §2). Only the types cat holds are routed, and
// with none of them nothing is read or probed.
//
// The conditions run in spec §2's order, each type decided on its own: (a)
// t's files match augeasFingerprints (the type, its provider, its module's
// shared files, and for sshd_config the sshd_config_match type; for augeas,
// augeas_core's parser and feature, not augeasproviders_core) and the type's
// provider directory holds no .rb but augeas.rb; (c) no variable of augeasEnvironment
// is set in env and libdir is absolute and clean; (b) probe accepts the
// library, probed only when a type is left and only after (c), so dlopen never
// runs under an AUGEAS_* or LD_* override; (d)-(f) no resource of the type has
// a shape the native code does not implement (Ruby's own refusals stay
// native); (g) for sysctl only, p is the measured platform and sysctl
// resolves over env's PATH as Puppet::Util.which does.
//
// command is "agent" or "apply". Only an agent run gives the augeas type a
// lens path: <libdir>/augeas/lenses, when that exists, as pluginsync'd lenses
// are loaded by the agent alone (provider/augeas/augeas.rb:372-375); otherwise "".
func NativeAugeas(t PluginTree, p Platform, probe AugeasProbe, env Environ, libdir, command string, cat *catalog.Catalog) AugeasTypes {
	a := AugeasTypes{Reasons: map[string]string{}, libdir: libdir}
	held := map[string]bool{}
	for _, r := range cat.Resources {
		held[strings.ToLower(r.Type)] = true
	}
	candidate := map[string]bool{}
	for _, rt := range augeasRouted {
		if held[rt.typ] {
			candidate[rt.typ] = true
		}
	}
	if len(candidate) == 0 {
		return a
	}
	delegate := func(typ, why string) {
		if candidate[typ] {
			delete(candidate, typ)
			a.Reasons[typ] = why
		}
	}
	delegateAll := func(why string) {
		for _, rt := range augeasRouted {
			delegate(rt.typ, why)
		}
	}

	// (a)
	for _, rt := range augeasRouted {
		if !candidate[rt.typ] {
			continue
		}
		rels := slices.Concat(rt.shared, []string{"puppet/type/" + rt.typ + ".rb", "puppet/provider/" + rt.typ + "/augeas.rb"}, rt.extra)
		if why := augeasFingerprint(t, rels...); why != "" {
			delegate(rt.typ, why)
		} else if why := augeasProviderDir(t, rt.typ); why != "" {
			delegate(rt.typ, why)
		}
	}

	// (c)
	for _, v := range augeasEnvironment {
		if env(v) != "" {
			delegateAll(v + " is set")
		}
	}
	if !strings.HasPrefix(libdir, "/") || !concatPathClean(libdir) {
		delegateAll(fmt.Sprintf("--libdir %q is not absolute and clean", libdir))
	}

	// (b)
	if len(candidate) > 0 {
		lib, why, ok := probe.Probe()
		if !ok {
			delegateAll(why)
		}
		a.lib = lib
	}

	// (d)-(f)
	var uv *unimplementedValue
	for _, r := range cat.Resources {
		for _, rt := range augeasRouted {
			if candidate[rt.typ] && strings.EqualFold(r.Type, rt.typ) {
				if err := rt.parse(r); errors.As(err, &uv) {
					delegate(rt.typ, err.Error())
				}
			}
		}
	}

	// (g)
	if candidate["sysctl"] {
		if !measuredRelease(p) {
			delegate("sysctl", "the platform is not RHEL family 10 or later (Fedora excluded)")
		} else if cmd, ok := resolveSysctl(env("PATH"), p.Executable); !ok {
			delegate("sysctl", "no executable sysctl on PATH, /usr/sbin or /sbin")
		} else {
			a.sysctl = cmd
		}
	}

	a.Sysctl, a.SshConfig = candidate["sysctl"], candidate["ssh_config"]
	a.SshdConfig, a.SshdConfigSubsystem = candidate["sshd_config"], candidate["sshd_config_subsystem"]
	if a.Augeas = candidate["augeas"]; a.Augeas && command == "agent" {
		lenses := filepath.Join(libdir, "augeas", "lenses")
		if _, err := os.Stat(lenses); err == nil {
			a.lensPath = lenses
		}
	}
	if a.SshdConfig {
		a.cat = cat
	}
	return a
}

// sshdMatchNames lists, in catalog order, every name a Sshd_config_match of
// cat is known by (none for a nil cat): its title, and each string of its alias metaparameter,
// which Puppet's catalog adds as an alias (catalog.rb:165-170). Its uniqueness
// key, [name, condition, target], is no single-key alias (host.rb aliases_of).
// A non-string alias never names a condition: a number or boolean holds no
// space, and a hash's text holds quotes, which no condition value does.
func sshdMatchNames(cat *catalog.Catalog) []string {
	if cat == nil {
		return nil
	}
	var out []string
	var aliases func(v any)
	aliases = func(v any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, e := range v {
				aliases(e)
			}
		}
	}
	for _, r := range cat.Resources {
		if strings.EqualFold(r.Type, "sshd_config_match") {
			out = append(out, r.Title)
			aliases(r.Parameters["alias"])
		}
	}
	return out
}

// augeasFingerprint is "" when every file rel of t has its digest in
// augeasFingerprints, else the reason the first one does not.
func augeasFingerprint(t PluginTree, rels ...string) string {
	for _, rel := range rels {
		b, err := t.Read(rel)
		if err != nil {
			return fmt.Sprintf("%s: %v", rel, err)
		}
		sum := sha256.Sum256(b)
		if got, want := hex.EncodeToString(sum[:]), augeasFingerprints[rel]; got != want {
			return fmt.Sprintf("%s has sha256 %s, not the measured %s", rel, got, want)
		}
	}
	return ""
}

// augeasProviderDir is "" when typ's provider directory in t holds no .rb but
// augeas.rb, else the reason: a second provider changes the one Ruby picks.
func augeasProviderDir(t PluginTree, typ string) string {
	dir := "puppet/provider/" + typ
	names, err := t.List(dir)
	if err != nil {
		return fmt.Sprintf("%s: %v", dir, err)
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "augeas.rb" {
			return fmt.Sprintf("%s holds %s besides augeas.rb", dir, n)
		}
	}
	return ""
}

// Providers returns a provider for each native type, all sharing one new
// per-run state over the probed library: call it once per apply. Nothing is
// opened until a provider's Prefetch or Apply, and every provider's EndRun
// closes what the run opened.
func (a AugeasTypes) Providers() []Provider {
	if !a.Sysctl && !a.SshConfig && !a.SshdConfig && !a.SshdConfigSubsystem && !a.Augeas {
		return nil
	}
	run := newAugeasRun(a.lib, a.libdir)
	var ps []Provider
	if a.Sysctl {
		ps = append(ps, Sysctl{run: run, sysctl: a.sysctl, runner: argvSysctl{}})
	}
	if a.SshConfig {
		ps = append(ps, SshConfig{run: run})
	}
	if a.SshdConfig {
		ps = append(ps, SshdConfig{run: run, cat: a.cat})
	}
	if a.SshdConfigSubsystem {
		ps = append(ps, SshdConfigSubsystem{run: run})
	}
	if a.Augeas {
		ps = append(ps, newAugeas(a.lib, a.lensPath))
	}
	return ps
}
