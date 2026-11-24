package units_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units"
	augeasproviderscore "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// The augeas routing tests live here, not in a unit, because they span the
// augeas units (one probe, one run).

// augeasUnitLibs are the testdata/lib trees of the three augeasproviders units,
// which together hold the twelve fingerprinted files of the augeasproviders
// releases; augeasCoreFixtures is augeas_core 2.1.0's four, one of them also in
// augeasproviders_core. Both are relative to this package's directory.
var augeasUnitLibs = []string{
	"augeasproviders_core/testdata/lib",
	"augeasproviders_sysctl/testdata/lib",
	"augeasproviders_ssh/testdata/lib",
}

const augeasCoreFixtures = "augeas_core/testdata/lib"

// The committed fixtures are byte-identical to the releases whose digests the
// manifests hold, so the manifests and the fixtures cannot drift.
func TestAugeasFixturesMatchManifests(t *testing.T) {
	byName := map[string]unit.Manifest{}
	for _, u := range units.All() {
		byName[u.Manifest().Name] = u.Manifest()
	}
	files := 0
	for _, name := range []string{"augeasproviders_core", "augeasproviders_sysctl", "augeasproviders_ssh"} {
		for _, rel := range byName[name].Releases {
			for _, f := range rel.Files {
				files++
				b, err := os.ReadFile(filepath.Join(name, "testdata", "lib", f.Path))
				if err != nil {
					t.Fatal(err)
				}
				if got := raltest.SHA256Hex(b); got != f.SHA256 {
					t.Errorf("%s %s: sha256 %s, want %s", name, f.Path, got, f.SHA256)
				}
			}
		}
	}
	if files != 12 {
		t.Fatalf("%d fingerprinted files in the three manifests, want 12", files)
	}
}

// augeasTree copies the unit fixture trees and augeas_core's into a temp
// libdir, which a test may edit. puppet/feature/augeas.rb is in both, byte for
// byte.
func augeasTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, lib := range append(slices.Clone(augeasUnitLibs), augeasCoreFixtures) {
		err := copyMissing(dir, lib)
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// augeasTypes are the five types of the two augeas modules.
var augeasTypes = []string{"augeas", "ssh_config", "sshd_config", "sshd_config_subsystem", "sysctl"}

// augeasRouting is a routing as the tests read it: the augeas providers (and
// so which types are native), and why each other held type is not.
type augeasRouting struct {
	Reasons   map[string]string
	providers []ral.Provider
}

func (a augeasRouting) Providers() []ral.Provider { return a.providers }

// has reports whether the routing holds a provider of typ.
func (a augeasRouting) has(typ string) bool {
	return slices.ContainsFunc(a.providers, func(p ral.Provider) bool { return p.Type() == typ })
}

// nativeAugeas routes cat as cmd/voxagent's nativeProviders does for the augeas
// types: every unit over one session on probe. Only the augeas types' providers and reasons are kept.
func nativeAugeas(t ral.PluginTree, p ral.Platform, probe augeas.Probe, env ral.Environ, libdir, command string, cat *catalog.Catalog) augeasRouting {
	s := augeas.NewSession(probe)
	d := unit.RouteAll(&unit.Env{Catalog: cat, Libdir: libdir, Tree: t, Platform: p, Environ: env, Command: command, Augeas: s}, units.All())
	a := augeasRouting{Reasons: map[string]string{}}
	for _, pr := range d.Providers {
		if slices.Contains(augeasTypes, pr.Type()) {
			a.providers = append(a.providers, pr)
		}
	}
	for typ, why := range d.Delegated {
		if slices.Contains(augeasTypes, typ) {
			a.Reasons[typ] = why
		}
	}
	return a
}

func envOf(vars map[string]string) ral.Environ { return func(k string) string { return vars[k] } }

// genericRes is a resource of the augeas type in a shape that stays native.
func genericRes() catalog.Resource {
	return raltest.AugRes("Augeas", "x", map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"})
}

// sysctlHost is the measured platform with sysctl in /usr/sbin.
var sysctlHost = raltest.FakePlatform{Exec: map[string]bool{"/usr/sbin/sysctl": true}, Release: raltest.CentOS10}

// augeasFour holds one resource of each type, in a shape that stays native.
func augeasFour() []catalog.Resource {
	return []catalog.Resource{
		raltest.AugRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"}),
		raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr"}),
		raltest.AugRes("Sshd_config", "X11Forwarding", map[string]any{"value": "no"}),
		raltest.AugRes("Sshd_config_subsystem", "sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
	}
}

// augeasRoute routes rs over libdir on sysctlHost, a clean environment and a
// successful probe.
func augeasRoute(t *testing.T, libdir string, rs ...catalog.Resource) augeasRouting {
	t.Helper()
	return nativeAugeas(plugintree.LibdirTree(libdir), sysctlHost, &raltest.ProbeFake{T: t}, envOf(nil), libdir, "apply", &catalog.Catalog{Resources: rs})
}

// nativeSet is a's answer as the set of native type names.
func nativeSet(a augeasRouting) []string {
	var out []string
	for _, typ := range allAugeasTypes {
		if a.has(typ) {
			out = append(out, typ)
		}
	}
	slices.Sort(out)
	return out
}

var allAugeasTypes = []string{"ssh_config", "sshd_config", "sshd_config_subsystem", "sysctl"}

// without is allAugeasTypes less the named ones.
func without(types ...string) []string {
	return slices.DeleteFunc(slices.Clone(allAugeasTypes), func(s string) bool { return slices.Contains(types, s) })
}

// checkRoute asserts the native set and that every delegated type has a
// reason and no native one does.
func checkRoute(t *testing.T, a augeasRouting, want []string) {
	t.Helper()
	if got := nativeSet(a); !slices.Equal(got, want) {
		t.Errorf("native %q, want %q (reasons %q)", got, want, a.Reasons)
	}
	for _, typ := range allAugeasTypes {
		_, has := a.Reasons[typ]
		if native := slices.Contains(want, typ); has == native {
			t.Errorf("%s: reason %q with native %v", typ, a.Reasons[typ], native)
		}
	}
}

func TestNativeAugeasFingerprints(t *testing.T) {
	type edit func(t *testing.T, dir, rel string)
	changeByte := func(t *testing.T, dir, rel string) {
		at := filepath.Join(dir, rel)
		b, err := os.ReadFile(at)
		if err != nil {
			t.Fatal(err)
		}
		b[len(b)/2] ^= 1
		if err := os.WriteFile(at, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, dir, rel string) {
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
	}
	directory := func(t *testing.T, dir, rel string) {
		remove(t, dir, rel)
		if err := os.Mkdir(filepath.Join(dir, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oversized := func(t *testing.T, dir, rel string) {
		at := filepath.Join(dir, rel)
		b, err := os.ReadFile(at)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, append(make([]byte, 1<<20), b...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	edits := map[string]edit{"one byte changed": changeByte, "missing": remove, "unreadable": directory, "over 1 MiB": oversized}

	t.Run("measured", func(t *testing.T) { checkRoute(t, augeasRoute(t, augeasTree(t), augeasFour()...), allAugeasTypes) })
	t.Run("symlinked libdir", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "lib")
		if err := os.Symlink(augeasTree(t), link); err != nil {
			t.Fatal(err)
		}
		checkRoute(t, augeasRoute(t, link, augeasFour()...), allAugeasTypes)
	})
	// sshd_config_match's type is sshd_config's alone, catalog match or not.
	for name, e := range edits {
		t.Run("puppet/type/sshd_config_match.rb "+name, func(t *testing.T) {
			dir := augeasTree(t)
			e(t, dir, "puppet/type/sshd_config_match.rb")
			a := augeasRoute(t, dir, augeasFour()...)
			checkRoute(t, a, without("sshd_config"))
			if !strings.Contains(a.Reasons["sshd_config"], "puppet/type/sshd_config_match.rb") {
				t.Errorf("sshd_config reason %q does not name sshd_config_match.rb", a.Reasons["sshd_config"])
			}
		})
	}
	for _, typ := range allAugeasTypes {
		for _, rel := range []string{"puppet/type/" + typ + ".rb", "puppet/provider/" + typ + "/augeas.rb"} {
			for name, e := range edits {
				t.Run(rel+" "+name, func(t *testing.T) {
					dir := augeasTree(t)
					e(t, dir, rel)
					checkRoute(t, augeasRoute(t, dir, augeasFour()...), without(typ))
				})
			}
		}
		t.Run(typ+" extra provider", func(t *testing.T) {
			dir := augeasTree(t)
			if err := os.WriteFile(filepath.Join(dir, "puppet", "provider", typ, "parsed.rb"), []byte("# x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			a := augeasRoute(t, dir, augeasFour()...)
			checkRoute(t, a, without(typ))
			if !strings.Contains(a.Reasons[typ], "parsed.rb") {
				t.Errorf("reason %q does not name parsed.rb", a.Reasons[typ])
			}
		})
	}
	t.Run("a file that is not Ruby beside augeas.rb", func(t *testing.T) {
		dir := augeasTree(t)
		if err := os.WriteFile(filepath.Join(dir, "puppet", "provider", "sysctl", "README"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		checkRoute(t, augeasRoute(t, dir, augeasFour()...), allAugeasTypes)
	})
	for _, rel := range []string{"puppet/provider/augeasprovider/default.rb", "puppet/type/augeasprovider.rb", "puppet/feature/augeas.rb"} {
		t.Run("core "+rel, func(t *testing.T) {
			dir := augeasTree(t)
			changeByte(t, dir, rel)
			checkRoute(t, augeasRoute(t, dir, augeasFour()...), nil)
		})
	}
	t.Run("empty libdir", func(t *testing.T) { checkRoute(t, augeasRoute(t, t.TempDir(), augeasFour()...), nil) })
	t.Run("FIFO", func(t *testing.T) {
		dir := augeasTree(t)
		rel := "puppet/type/sysctl.rb"
		remove(t, dir, rel)
		if err := syscall.Mkfifo(filepath.Join(dir, rel), 0o644); err != nil {
			t.Fatal(err)
		}
		checkRoute(t, augeasRoute(t, dir, augeasFour()...), without("sysctl"))
	})
}

// Only the types the catalog holds are routed, and with none of them the
// probe never runs.
func TestNativeAugeasOnlyTypesPresent(t *testing.T) {
	dir := augeasTree(t)
	probe := &raltest.ProbeFake{T: t}
	a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply",
		&catalog.Catalog{Resources: []catalog.Resource{raltest.AugRes("Notify", "n", nil)}})
	if got := nativeSet(a); got != nil || len(a.Reasons) != 0 || probe.Calls != 0 {
		t.Errorf("no augeas type: native %q, reasons %q, probe calls %d", got, a.Reasons, probe.Calls)
	}
	a = nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply",
		&catalog.Catalog{Resources: augeasFour()[1:2]})
	if got := nativeSet(a); !slices.Equal(got, []string{"ssh_config"}) || len(a.Reasons) != 0 || probe.Calls != 1 {
		t.Errorf("ssh_config only: native %q, reasons %q, probe calls %d", got, a.Reasons, probe.Calls)
	}
}

func TestNativeAugeasEnvironment(t *testing.T) {
	for _, v := range []string{"AUGEAS_ROOT", "AUGEAS_LENS_LIB", "AUGEAS_DEBUG", "AUGEAS_DEBUG_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		t.Run(v, func(t *testing.T) {
			dir := augeasTree(t)
			probe := &raltest.ProbeFake{T: t}
			a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{v: "x"}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, nil)
			if probe.Calls != 0 {
				t.Errorf("probe called %d time(s) with %s set", probe.Calls, v)
			}
			if !strings.Contains(a.Reasons["sysctl"], v) {
				t.Errorf("reason %q does not name %s", a.Reasons["sysctl"], v)
			}
			probe = &raltest.ProbeFake{T: t}
			a = nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{v: ""}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, allAugeasTypes)
			if probe.Calls != 1 {
				t.Errorf("probe called %d time(s) with %s empty, want 1", probe.Calls, v)
			}
		})
	}
}

func TestNativeAugeasLibdirClean(t *testing.T) {
	tree := plugintree.LibdirTree(augeasTree(t))
	for _, libdir := range []string{"/x/", "/x//y", "/x/./y", "/x/../y", "/", "x/y", "", "./x"} {
		t.Run(libdir, func(t *testing.T) {
			probe := &raltest.ProbeFake{T: t}
			a := nativeAugeas(tree, sysctlHost, probe, envOf(nil), libdir, "apply", &catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, nil)
			if probe.Calls != 0 {
				t.Errorf("probe called %d time(s)", probe.Calls)
			}
		})
	}
}

func TestNativeAugeasProbe(t *testing.T) {
	dir := augeasTree(t)
	const why = "libaugeas: dlopen: no such file"
	probe := &raltest.ProbeFake{T: t, Reason: why}
	a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply", &catalog.Catalog{Resources: augeasFour()})
	checkRoute(t, a, nil)
	for _, typ := range allAugeasTypes {
		if a.Reasons[typ] != why {
			t.Errorf("%s reason %q, want %q", typ, a.Reasons[typ], why)
		}
	}

	// No type left after the fingerprints: the library is not probed.
	probe = &raltest.ProbeFake{T: t}
	empty := t.TempDir()
	nativeAugeas(plugintree.LibdirTree(empty), sysctlHost, probe, envOf(nil), empty, "apply", &catalog.Catalog{Resources: augeasFour()})
	if probe.Calls != 0 {
		t.Errorf("probe called %d time(s) with no fingerprint matching", probe.Calls)
	}
}

// shapeCase is one resource and whether its type stays native.
type shapeCase struct {
	name   string
	r      catalog.Resource
	native bool
}

func runShapes(t *testing.T, cases []shapeCase) {
	dir := augeasTree(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			typ := strings.ToLower(c.r.Type)
			// A native resource of the type first, so only c.r decides.
			var rs []catalog.Resource
			for _, r := range augeasFour() {
				if strings.ToLower(r.Type) == typ && r.Title != c.r.Title {
					rs = append(rs, r)
				}
			}
			rs = append(rs, c.r)
			for _, r := range augeasFour() {
				if strings.ToLower(r.Type) != typ {
					rs = append(rs, r)
				}
			}
			want := allAugeasTypes
			if !c.native {
				want = without(typ)
			}
			a := augeasRoute(t, dir, rs...)
			checkRoute(t, a, want)
			if !c.native && !strings.Contains(a.Reasons[typ], c.r.Ref()) {
				t.Errorf("reason %q does not name %s", a.Reasons[typ], c.r.Ref())
			}
		})
	}
}

func TestNativeAugeasNameShapes(t *testing.T) {
	v := map[string]any{"value": "1"}
	sv := map[string]any{"value": "yes"}
	cmd := map[string]any{"command": "/bin/x"}
	runShapes(t, []shapeCase{
		{"sysctl 1x.y", raltest.AugRes("Sysctl", "1x.y", v), false},
		{"sysctl .a", raltest.AugRes("Sysctl", ".a", v), false},
		{"sysctl -a", raltest.AugRes("Sysctl", "-a", v), false},
		{"sysctl a..b", raltest.AugRes("Sysctl", "a..b", v), false},
		{"sysctl net.ipv4.ip_forward", raltest.AugRes("Sysctl", "net.ipv4.ip_forward", v), true},
		{"sysctl kernel.core_pattern", raltest.AugRes("Sysctl", "kernel.core_pattern", v), true},
		{"ssh_config Ciphers", raltest.AugRes("Ssh_config", "Ciphers", sv), true},
		{"ssh_config Foo-Bar", raltest.AugRes("Ssh_config", "Foo-Bar", sv), false},
		{"ssh_config key Foo-Bar", raltest.AugRes("Ssh_config", "x", map[string]any{"value": "yes", "key": "Foo-Bar"}), false},
		{"sshd_config Foo-Bar", raltest.AugRes("Sshd_config", "Foo-Bar", sv), false},
		{"ssh_config host *", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "host": "*"}), true},
		{"ssh_config host a'b", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "host": "a'b"}), false},
		{"subsystem sftp", raltest.AugRes("Sshd_config_subsystem", "sftp", cmd), true},
		{"subsystem _x", raltest.AugRes("Sshd_config_subsystem", "_x", cmd), true},
		{"subsystem x-y", raltest.AugRes("Sshd_config_subsystem", "x-y", cmd), true},
		{"subsystem .", raltest.AugRes("Sshd_config_subsystem", ".", cmd), false},
		{"subsystem ..", raltest.AugRes("Sshd_config_subsystem", "..", cmd), false},
		{"subsystem 1", raltest.AugRes("Sshd_config_subsystem", "1", cmd), false},
		{"subsystem 1x", raltest.AugRes("Sshd_config_subsystem", "1x", cmd), false},
		{"subsystem a.b", raltest.AugRes("Sshd_config_subsystem", "a.b", cmd), false},
		{"condition pairs", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"}), true},
		{"condition odd", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host"}), false},
		{"condition Hash", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": map[string]any{"Host": "x"}}), false},
		{"target clean", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/sysctl.d/99-x.conf"}), true},
		{"target trailing /", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/x/"}), false},
		{"target //", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc//x"}), false},
		{"target ..", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/../x"}), false},
		{"target space", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/a b"}), false},
		{"target quote", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "target": "/etc/a'b"}), false},
		{"ssh_config relative target is Ruby's refusal", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "target": "etc/x"}), true},
		{"value control character", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "a\nb"}), false},
	})
}

func TestNativeAugeasShapes(t *testing.T) {
	const max = float64(1<<53 - 1)
	runShapes(t, []shapeCase{
		{"comment", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "comment": "c"}), false},
		{"ssh_config comment", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "x", "comment": "c"}), false},
		{"val", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"val": "1"}), false},
		{"persist => false", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "persist": false}), false},
		{"sysctl ensure => absent", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"ensure": "absent"}), false},
		{"ssh_config ensure => absent stays", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"ensure": "absent"}), true},
		{"provider => parsed", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "provider": "parsed"}), false},
		{"provider => augeas", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "provider": "augeas"}), true},
		{"name differs from title", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "name": "vm.dirty_ratio"}), false},
		{"Sensitive", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no"}, "value"), false},
		{"ssh_config present without value", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"ensure": "present"}), false},
		{"sshd_config present without value", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"ensure": "present"}), false},
		{"ssh_config Integer value", raltest.AugRes("Ssh_config", "Port", map[string]any{"value": float64(22)}), false},
		{"sshd_config Integer value", raltest.AugRes("Sshd_config", "Port", map[string]any{"value": float64(22)}), true},
		{"sysctl 2^53", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": max + 1}), false},
		{"sysctl 2^53 - 1", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": max}), true},
		{"ssh_config []", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": []any{}}), false},
		{"sshd_config []", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": []any{}}), false},
		{"ssh_config array", raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}}), true},
		{"subsystem Integer command", raltest.AugRes("Sshd_config_subsystem", "sftp", map[string]any{"command": float64(1)}), false},
		{"Hash value", raltest.AugRes("Sshd_config", "PermitRootLogin", map[string]any{"value": map[string]any{"a": "b"}}), false},
		// Ruby's refusals stay native: validation prints them.
		{"apply => yes", raltest.AugRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "apply": "yes"}), true},
	})
}

func TestNativeAugeasSysctlPlatform(t *testing.T) {
	dir := augeasTree(t)
	exec := map[string]bool{"/usr/sbin/sysctl": true}
	cases := []struct {
		name   string
		p      raltest.FakePlatform
		path   string
		native bool
	}{
		{"CentOS Stream 10", sysctlHost, "", true},
		{"libuser is not a condition", raltest.FakePlatform{Exec: map[string]bool{"/usr/sbin/sysctl": true,
			"/usr/sbin/luseradd": true, "/usr/sbin/lgroupadd": true}, Release: raltest.CentOS10}, "", true},
		{"PATH first", raltest.FakePlatform{Exec: map[string]bool{"/usr/sbin/sysctl": true, "/opt/x/sysctl": true},
			Release: raltest.CentOS10}, "/opt/x:/usr/bin", true},
		{"/sbin appended", raltest.FakePlatform{Exec: map[string]bool{"/sbin/sysctl": true}, Release: raltest.CentOS10}, "/usr/bin", true},
		{"Fedora", raltest.FakePlatform{Exec: exec, Release: "ID=fedora\nID_LIKE=\"rhel\"\nVERSION_ID=41\n"}, "", false},
		{"RHEL 9", raltest.FakePlatform{Exec: exec, Release: "ID=rhel\nVERSION_ID=\"9.4\"\n"}, "", false},
		{"os-release unreadable", raltest.FakePlatform{Exec: exec, Err: os.ErrNotExist}, "", false},
		{"no sysctl", raltest.FakePlatform{Release: raltest.CentOS10}, "/usr/bin", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := nativeAugeas(plugintree.LibdirTree(dir), c.p, &raltest.ProbeFake{T: t}, envOf(map[string]string{"PATH": c.path}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			want := allAugeasTypes
			if !c.native {
				want = without("sysctl")
			}
			checkRoute(t, a, want)
		})
	}
}

func TestLibdirTreeList(t *testing.T) {
	dir := augeasTree(t)
	got, err := plugintree.LibdirTree(dir).List("puppet/provider")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if w := []string{"augeas", "augeasprovider", "ssh_config", "sshd_config", "sshd_config_subsystem", "sysctl"}; !slices.Equal(got, w) {
		t.Errorf("List = %q, want %q", got, w)
	}
	if _, err := plugintree.LibdirTree(dir).List("puppet/none"); err == nil {
		t.Error("missing directory listed without an error")
	}
}

func TestAugeasSplitProbesOnceAcrossGroups(t *testing.T) {
	dir := augeasTree(t)
	probe := &raltest.ProbeFake{T: t}
	rs := []catalog.Resource{augeasFour()[0], augeasFour()[1], genericRes()}
	a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply", &catalog.Catalog{Resources: rs})
	ps, reasons := a.providers, a.Reasons
	if len(ps) != 3 || len(reasons) != 0 || probe.Calls != 1 {
		t.Errorf("%d providers, reasons %q, probe calls %d; want 3, none, 1", len(ps), reasons, probe.Calls)
	}
}

func TestAugeasSplitNoProbeWhenNoneSurvive(t *testing.T) {
	dir := augeasTree(t)
	probe := &raltest.ProbeFake{T: t}
	rs := append(augeasFour(), genericRes())
	a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{"AUGEAS_ROOT": "/x"}), dir, "apply", &catalog.Catalog{Resources: rs})
	ps, reasons := a.providers, a.Reasons
	if len(ps) != 0 || probe.Calls != 0 {
		t.Errorf("%d providers, probe calls %d; want 0, 0", len(ps), probe.Calls)
	}
	for _, typ := range append(slices.Clone(allAugeasTypes), "augeas") {
		if reasons[typ] != "AUGEAS_ROOT is set" {
			t.Errorf("%s reason %q", typ, reasons[typ])
		}
	}
	if len(reasons) != 5 {
		t.Errorf("reasons %q, want exactly the five held types", reasons)
	}
}

func TestAugeasSplitMixedFirstReasons(t *testing.T) {
	dir := augeasTree(t)
	if err := os.WriteFile(filepath.Join(dir, "puppet/type/sysctl.rb"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := &raltest.ProbeFake{T: t}
	reasons := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{"AUGEAS_DEBUG": "1"}), dir, "apply",
		&catalog.Catalog{Resources: augeasFour()[:2]}).Reasons
	if !strings.Contains(reasons["sysctl"], "puppet/type/sysctl.rb has sha256") {
		t.Errorf("sysctl reason %q, want the fingerprint text", reasons["sysctl"])
	}
	if reasons["ssh_config"] != "AUGEAS_DEBUG is set" {
		t.Errorf("ssh_config reason %q", reasons["ssh_config"])
	}
}

// "ſysctl" (U+017F) is EqualFold-equal to "sysctl" but not case-mapped to it:
// a catalog holding only that type holds no augeas type.
func TestAugeasSplitUnicodeFoldedTypeIsNotHeld(t *testing.T) {
	dir := augeasTree(t)
	probe := &raltest.ProbeFake{T: t}
	a := nativeAugeas(plugintree.LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply",
		&catalog.Catalog{Resources: []catalog.Resource{raltest.AugRes("ſysctl", "x", nil)}})
	if probe.Calls != 0 || len(a.Reasons) != 0 || len(a.providers) != 0 {
		t.Errorf("probe calls %d, reasons %q, %d provider(s); want none", probe.Calls, a.Reasons, len(a.providers))
	}
}

// The four providers hold the one run of the session, which any of them ends:
// EndRun on each, alone, closes the handle opened through the session's run,
// once. A second routing (a fresh session) has a run of its own.
func TestAugeasProvidersShareOneRun(t *testing.T) {
	dir := augeasTree(t)
	route := func(lib augeas.Lib) (*augeas.Session, []ral.Provider) {
		s := augeas.NewSession(libProbe{lib})
		d := unit.RouteAll(&unit.Env{Catalog: &catalog.Catalog{Resources: augeasFour()}, Libdir: dir, Tree: plugintree.LibdirTree(dir),
			Platform: sysctlHost, Environ: envOf(nil), Command: "apply", Augeas: s}, units.All())
		return s, d.Providers
	}
	for _, typ := range allAugeasTypes {
		t.Run(typ, func(t *testing.T) {
			lib := raltest.NewScriptLib(t, raltest.ParseTrace(t, "end", "h1 aug_init NULL NULL 64\nh1 aug_close\n"))
			s, ps := route(lib)
			var p ral.Provider
			for _, q := range ps {
				if q.Type() == typ {
					p = q
				}
			}
			if p == nil {
				t.Fatalf("no %s provider among %d", typ, len(ps))
			}
			if _, err := augeasproviderscore.SharedRun(s, dir).Open(nil); err != nil {
				t.Fatal(err)
			}
			p.(ral.RunEnding).EndRun()
		})
	}
	s1, _ := route(raltest.NewScriptLib(t, nil))
	s2, _ := route(raltest.NewScriptLib(t, nil))
	if augeasproviderscore.SharedRun(s1, dir) == augeasproviderscore.SharedRun(s2, dir) {
		t.Error("a second routing reuses the first run")
	}
}

// augeasEnvironment are the variables that send every augeas type to the host.
var augeasEnvironment = []string{"AUGEAS_ROOT", "AUGEAS_LENS_LIB", "AUGEAS_DEBUG", "AUGEAS_DEBUG_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH"}

// routeAugeas routes rs over libdir on sysctlHost with env and probe.
func routeAugeas(t *testing.T, libdir, command string, env map[string]string, probe augeas.Probe, rs ...catalog.Resource) augeasRouting {
	t.Helper()
	return nativeAugeas(plugintree.LibdirTree(libdir), sysctlHost, probe, envOf(env), libdir, command, &catalog.Catalog{Resources: rs})
}

// The two modules are independent: augeasproviders_core's files do not decide
// augeas, nor augeas_core's the four types.
func TestNativeAugeasGenericAndProvidersAreIndependent(t *testing.T) {
	rs := append(augeasFour(), genericRes())
	t.Run("augeasproviders absent", func(t *testing.T) {
		dir := augeasTree(t)
		for _, rel := range []string{"puppet/provider/augeasprovider/default.rb", "puppet/type/augeasprovider.rb"} {
			if err := os.Remove(filepath.Join(dir, rel)); err != nil {
				t.Fatal(err)
			}
		}
		a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, rs...)
		if !a.has("augeas") || len(nativeSet(a)) != 0 || len(a.Reasons) != 4 {
			t.Errorf("augeas %v, four types native %v, reasons %q", a.has("augeas"), nativeSet(a), a.Reasons)
		}
	})
	t.Run("augeas_core absent", func(t *testing.T) {
		dir := augeasTree(t)
		if err := os.Remove(filepath.Join(dir, "puppet", "type", "augeas.rb")); err != nil {
			t.Fatal(err)
		}
		a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, rs...)
		checkRoute(t, a, allAugeasTypes)
		if a.has("augeas") || a.Reasons["augeas"] == "" {
			t.Errorf("augeas %v, reason %q", a.has("augeas"), a.Reasons["augeas"])
		}
	})
}

// Conditions (b) and (c) apply to augeas as to the four types, and the probe
// still runs at most once.
func TestNativeAugeasGenericEnvironmentAndProbe(t *testing.T) {
	dir := augeasTree(t)
	rs := append(augeasFour(), genericRes())
	for _, v := range augeasEnvironment {
		t.Run(v, func(t *testing.T) {
			probe := &raltest.ProbeFake{T: t}
			a := routeAugeas(t, dir, "apply", map[string]string{v: "x"}, probe, rs...)
			if a.has("augeas") || !strings.Contains(a.Reasons["augeas"], v) || probe.Calls != 0 {
				t.Errorf("augeas %v, reason %q, probe calls %d", a.has("augeas"), a.Reasons["augeas"], probe.Calls)
			}
		})
	}
	t.Run("libdir not clean", func(t *testing.T) {
		probe := &raltest.ProbeFake{T: t}
		a := routeAugeas(t, dir+"/", "apply", nil, probe, genericRes())
		if a.has("augeas") || a.Reasons["augeas"] == "" || probe.Calls != 0 {
			t.Errorf("augeas %v, reason %q, probe calls %d", a.has("augeas"), a.Reasons["augeas"], probe.Calls)
		}
	})
	t.Run("probe refuses", func(t *testing.T) {
		const why = "libaugeas: dlopen: no such file"
		a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t, Reason: why}, rs...)
		if a.has("augeas") || a.Reasons["augeas"] != why {
			t.Errorf("augeas %v, reason %q, want %q", a.has("augeas"), a.Reasons["augeas"], why)
		}
	})
	t.Run("probed once for all five", func(t *testing.T) {
		probe := &raltest.ProbeFake{T: t}
		a := routeAugeas(t, dir, "apply", nil, probe, rs...)
		if !a.has("augeas") || probe.Calls != 1 {
			t.Errorf("augeas %v, probe calls %d", a.has("augeas"), probe.Calls)
		}
	})
}

// A resource shape the native code does not implement sends every augeas
// resource to the host, the reason naming the resource; the four types stay.
func TestNativeAugeasGenericShape(t *testing.T) {
	dir := augeasTree(t)
	bad := raltest.AugRes("Augeas", "bad", map[string]any{"context": "/files/etc/hosts", "onlyif": "match 1 size > 0"})
	rs := append(augeasFour(), genericRes(), bad)
	a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, rs...)
	checkRoute(t, a, allAugeasTypes)
	if a.has("augeas") || !strings.Contains(a.Reasons["augeas"], bad.Ref()) {
		t.Errorf("augeas %v, reason %q does not name %s", a.has("augeas"), a.Reasons["augeas"], bad.Ref())
	}
}
