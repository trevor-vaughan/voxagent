package ral

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// augeasFixtures and augeasCoreFixtures are the committed copies of the
// fingerprinted files of the augeasproviders releases (twelve) and of
// augeas_core 2.1.0 (four, one of them also in augeasproviders_core).
const (
	augeasFixtures     = "testdata/augeasproviders/lib"
	augeasCoreFixtures = "testdata/augeas_core/lib"
)

// The committed fixtures are byte-identical to the releases whose digests
// augeasFingerprints holds, so the constants and the fixtures cannot drift.
func TestAugeasFixturesMatchFingerprints(t *testing.T) {
	if len(augeasFingerprints) != 15 {
		t.Fatalf("%d fingerprints, want 15", len(augeasFingerprints))
	}
	for rel, want := range augeasFingerprints {
		b, err := os.ReadFile(filepath.Join(augeasFixtures, rel))
		if errors.Is(err, fs.ErrNotExist) {
			b, err = os.ReadFile(filepath.Join(augeasCoreFixtures, rel))
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := sha256Hex(b); got != want {
			t.Errorf("%s: sha256 %s, want %s", rel, got, want)
		}
	}
}

// augeasTree copies both fixture trees into a temp libdir, which a test may
// edit. puppet/feature/augeas.rb is in both, byte for byte.
func augeasTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(augeasFixtures)); err != nil {
		t.Fatal(err)
	}
	err := fs.WalkDir(os.DirFS(augeasCoreFixtures), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		at := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(at, 0o755)
		}
		if _, err := os.Stat(at); err == nil {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(augeasCoreFixtures, rel))
		if err != nil {
			return err
		}
		return os.WriteFile(at, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// augeasProbeFake answers the library probe and counts its calls. A
// successful probe's library is a script of no calls: routing must not touch
// it (handles open lazily, in Prefetch and Apply).
type augeasProbeFake struct {
	t      *testing.T
	reason string // "" probes successfully
	calls  int
}

func (p *augeasProbeFake) Probe() (augLib, string, bool) {
	p.calls++
	if p.reason != "" {
		return nil, p.reason, false
	}
	return newScriptLib(p.t, nil), "", true
}

func envOf(vars map[string]string) Environ { return func(k string) string { return vars[k] } }

// sysctlHost is the measured platform with sysctl in /usr/sbin.
var sysctlHost = fakePlatform{exec: map[string]bool{"/usr/sbin/sysctl": true}, release: centos10}

// augeasFour holds one resource of each type, in a shape that stays native.
func augeasFour() []catalog.Resource {
	return []catalog.Resource{
		augRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"}),
		augRes("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr"}),
		augRes("Sshd_config", "X11Forwarding", map[string]any{"value": "no"}),
		augRes("Sshd_config_subsystem", "sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
	}
}

// augeasRoute routes rs over libdir on sysctlHost, a clean environment and a
// successful probe.
func augeasRoute(t *testing.T, libdir string, rs ...catalog.Resource) AugeasTypes {
	t.Helper()
	return NativeAugeas(LibdirTree(libdir), sysctlHost, &augeasProbeFake{t: t}, envOf(nil), libdir, "apply", &catalog.Catalog{Resources: rs})
}

// nativeSet is a's answer as the set of native type names.
func nativeSet(a AugeasTypes) []string {
	var out []string
	for typ, native := range map[string]bool{"sysctl": a.Sysctl, "ssh_config": a.SshConfig,
		"sshd_config": a.SshdConfig, "sshd_config_subsystem": a.SshdConfigSubsystem} {
		if native {
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
func checkRoute(t *testing.T, a AugeasTypes, want []string) {
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
	probe := &augeasProbeFake{t: t}
	a := NativeAugeas(LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply",
		&catalog.Catalog{Resources: []catalog.Resource{augRes("Notify", "n", nil)}})
	if got := nativeSet(a); got != nil || len(a.Reasons) != 0 || probe.calls != 0 {
		t.Errorf("no augeas type: native %q, reasons %q, probe calls %d", got, a.Reasons, probe.calls)
	}
	a = NativeAugeas(LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply",
		&catalog.Catalog{Resources: augeasFour()[1:2]})
	if got := nativeSet(a); !slices.Equal(got, []string{"ssh_config"}) || len(a.Reasons) != 0 || probe.calls != 1 {
		t.Errorf("ssh_config only: native %q, reasons %q, probe calls %d", got, a.Reasons, probe.calls)
	}
}

func TestNativeAugeasEnvironment(t *testing.T) {
	for _, v := range []string{"AUGEAS_ROOT", "AUGEAS_LENS_LIB", "AUGEAS_DEBUG", "AUGEAS_DEBUG_DIR", "LD_PRELOAD", "LD_LIBRARY_PATH"} {
		t.Run(v, func(t *testing.T) {
			dir := augeasTree(t)
			probe := &augeasProbeFake{t: t}
			a := NativeAugeas(LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{v: "x"}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, nil)
			if probe.calls != 0 {
				t.Errorf("probe called %d time(s) with %s set", probe.calls, v)
			}
			if !strings.Contains(a.Reasons["sysctl"], v) {
				t.Errorf("reason %q does not name %s", a.Reasons["sysctl"], v)
			}
			probe = &augeasProbeFake{t: t}
			a = NativeAugeas(LibdirTree(dir), sysctlHost, probe, envOf(map[string]string{v: ""}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, allAugeasTypes)
			if probe.calls != 1 {
				t.Errorf("probe called %d time(s) with %s empty, want 1", probe.calls, v)
			}
		})
	}
}

func TestNativeAugeasLibdirClean(t *testing.T) {
	tree := LibdirTree(augeasTree(t))
	for _, libdir := range []string{"/x/", "/x//y", "/x/./y", "/x/../y", "/", "x/y", "", "./x"} {
		t.Run(libdir, func(t *testing.T) {
			probe := &augeasProbeFake{t: t}
			a := NativeAugeas(tree, sysctlHost, probe, envOf(nil), libdir, "apply", &catalog.Catalog{Resources: augeasFour()})
			checkRoute(t, a, nil)
			if probe.calls != 0 {
				t.Errorf("probe called %d time(s)", probe.calls)
			}
		})
	}
}

func TestNativeAugeasProbe(t *testing.T) {
	dir := augeasTree(t)
	const why = "libaugeas: dlopen: no such file"
	probe := &augeasProbeFake{t: t, reason: why}
	a := NativeAugeas(LibdirTree(dir), sysctlHost, probe, envOf(nil), dir, "apply", &catalog.Catalog{Resources: augeasFour()})
	checkRoute(t, a, nil)
	for _, typ := range allAugeasTypes {
		if a.Reasons[typ] != why {
			t.Errorf("%s reason %q, want %q", typ, a.Reasons[typ], why)
		}
	}

	// No type left after the fingerprints: the library is not probed.
	probe = &augeasProbeFake{t: t}
	empty := t.TempDir()
	NativeAugeas(LibdirTree(empty), sysctlHost, probe, envOf(nil), empty, "apply", &catalog.Catalog{Resources: augeasFour()})
	if probe.calls != 0 {
		t.Errorf("probe called %d time(s) with no fingerprint matching", probe.calls)
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
		{"sysctl 1x.y", augRes("Sysctl", "1x.y", v), false},
		{"sysctl .a", augRes("Sysctl", ".a", v), false},
		{"sysctl -a", augRes("Sysctl", "-a", v), false},
		{"sysctl a..b", augRes("Sysctl", "a..b", v), false},
		{"sysctl net.ipv4.ip_forward", augRes("Sysctl", "net.ipv4.ip_forward", v), true},
		{"sysctl kernel.core_pattern", augRes("Sysctl", "kernel.core_pattern", v), true},
		{"ssh_config Ciphers", augRes("Ssh_config", "Ciphers", sv), true},
		{"ssh_config Foo-Bar", augRes("Ssh_config", "Foo-Bar", sv), false},
		{"ssh_config key Foo-Bar", augRes("Ssh_config", "x", map[string]any{"value": "yes", "key": "Foo-Bar"}), false},
		{"sshd_config Foo-Bar", augRes("Sshd_config", "Foo-Bar", sv), false},
		{"ssh_config host *", augRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "host": "*"}), true},
		{"ssh_config host a'b", augRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "host": "a'b"}), false},
		{"subsystem sftp", augRes("Sshd_config_subsystem", "sftp", cmd), true},
		{"subsystem _x", augRes("Sshd_config_subsystem", "_x", cmd), true},
		{"subsystem x-y", augRes("Sshd_config_subsystem", "x-y", cmd), true},
		{"subsystem .", augRes("Sshd_config_subsystem", ".", cmd), false},
		{"subsystem ..", augRes("Sshd_config_subsystem", "..", cmd), false},
		{"subsystem 1", augRes("Sshd_config_subsystem", "1", cmd), false},
		{"subsystem 1x", augRes("Sshd_config_subsystem", "1x", cmd), false},
		{"subsystem a.b", augRes("Sshd_config_subsystem", "a.b", cmd), false},
		{"condition pairs", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host example.net User root"}), true},
		{"condition odd", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": "Host"}), false},
		{"condition Hash", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "condition": map[string]any{"Host": "x"}}), false},
		{"target clean", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/sysctl.d/99-x.conf"}), true},
		{"target trailing /", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/x/"}), false},
		{"target //", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc//x"}), false},
		{"target ..", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/../x"}), false},
		{"target space", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "target": "/etc/a b"}), false},
		{"target quote", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no", "target": "/etc/a'b"}), false},
		{"ssh_config relative target is Ruby's refusal", augRes("Ssh_config", "Ciphers", map[string]any{"value": "yes", "target": "etc/x"}), true},
		{"value control character", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "a\nb"}), false},
	})
}

func TestNativeAugeasShapes(t *testing.T) {
	const max = float64(1<<53 - 1)
	runShapes(t, []shapeCase{
		{"comment", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "comment": "c"}), false},
		{"ssh_config comment", augRes("Ssh_config", "Ciphers", map[string]any{"value": "x", "comment": "c"}), false},
		{"val", augRes("Sysctl", "vm.swappiness", map[string]any{"val": "1"}), false},
		{"persist => false", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "persist": false}), false},
		{"sysctl ensure => absent", augRes("Sysctl", "vm.swappiness", map[string]any{"ensure": "absent"}), false},
		{"ssh_config ensure => absent stays", augRes("Ssh_config", "Ciphers", map[string]any{"ensure": "absent"}), true},
		{"provider => parsed", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "provider": "parsed"}), false},
		{"provider => augeas", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "provider": "augeas"}), true},
		{"name differs from title", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "name": "vm.dirty_ratio"}), false},
		{"Sensitive", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": "no"}, "value"), false},
		{"ssh_config present without value", augRes("Ssh_config", "Ciphers", map[string]any{"ensure": "present"}), false},
		{"sshd_config present without value", augRes("Sshd_config", "PermitRootLogin", map[string]any{"ensure": "present"}), false},
		{"ssh_config Integer value", augRes("Ssh_config", "Port", map[string]any{"value": float64(22)}), false},
		{"sshd_config Integer value", augRes("Sshd_config", "Port", map[string]any{"value": float64(22)}), true},
		{"sysctl 2^53", augRes("Sysctl", "vm.swappiness", map[string]any{"value": max + 1}), false},
		{"sysctl 2^53 - 1", augRes("Sysctl", "vm.swappiness", map[string]any{"value": max}), true},
		{"ssh_config []", augRes("Ssh_config", "Ciphers", map[string]any{"value": []any{}}), false},
		{"sshd_config []", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": []any{}}), false},
		{"ssh_config array", augRes("Ssh_config", "Ciphers", map[string]any{"value": []any{"a", "b"}}), true},
		{"subsystem Integer command", augRes("Sshd_config_subsystem", "sftp", map[string]any{"command": float64(1)}), false},
		{"Hash value", augRes("Sshd_config", "PermitRootLogin", map[string]any{"value": map[string]any{"a": "b"}}), false},
		// Ruby's refusals stay native: validation prints them.
		{"apply => yes", augRes("Sysctl", "vm.swappiness", map[string]any{"value": "1", "apply": "yes"}), true},
	})
}

func TestNativeAugeasSysctlPlatform(t *testing.T) {
	dir := augeasTree(t)
	exec := map[string]bool{"/usr/sbin/sysctl": true}
	cases := []struct {
		name    string
		p       fakePlatform
		path    string
		native  bool
		command string
	}{
		{"CentOS Stream 10", sysctlHost, "", true, "/usr/sbin/sysctl"},
		{"libuser is not a condition", fakePlatform{exec: map[string]bool{"/usr/sbin/sysctl": true,
			"/usr/sbin/luseradd": true, "/usr/sbin/lgroupadd": true}, release: centos10}, "", true, "/usr/sbin/sysctl"},
		{"PATH first", fakePlatform{exec: map[string]bool{"/usr/sbin/sysctl": true, "/opt/x/sysctl": true},
			release: centos10}, "/opt/x:/usr/bin", true, "/opt/x/sysctl"},
		{"/sbin appended", fakePlatform{exec: map[string]bool{"/sbin/sysctl": true}, release: centos10}, "/usr/bin", true, "/sbin/sysctl"},
		{"Fedora", fakePlatform{exec: exec, release: "ID=fedora\nID_LIKE=\"rhel\"\nVERSION_ID=41\n"}, "", false, ""},
		{"RHEL 9", fakePlatform{exec: exec, release: "ID=rhel\nVERSION_ID=\"9.4\"\n"}, "", false, ""},
		{"os-release unreadable", fakePlatform{exec: exec, err: os.ErrNotExist}, "", false, ""},
		{"no sysctl", fakePlatform{release: centos10}, "/usr/bin", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := NativeAugeas(LibdirTree(dir), c.p, &augeasProbeFake{t: t}, envOf(map[string]string{"PATH": c.path}), dir, "apply",
				&catalog.Catalog{Resources: augeasFour()})
			want := allAugeasTypes
			if !c.native {
				want = without("sysctl")
			}
			checkRoute(t, a, want)
			if a.sysctl != c.command {
				t.Errorf("sysctl command %q, want %q", a.sysctl, c.command)
			}
		})
	}
}

// Providers builds one provider per native type, all over one run that
// opened nothing; each ends that run.
func TestAugeasTypesProviders(t *testing.T) {
	dir := augeasTree(t)
	rs := augeasFour()
	rs[1].Parameters["comment"] = "c"
	a := augeasRoute(t, dir, rs...)
	ps := a.Providers()
	var types []string
	var runs []*augeasRun
	for _, p := range ps {
		types = append(types, p.Type())
		if _, ok := p.(RunEnding); !ok {
			t.Errorf("%s does not end its run", p.Type())
		}
		switch q := p.(type) {
		case Sysctl:
			runs = append(runs, q.run)
			if q.sysctl != "/usr/sbin/sysctl" {
				t.Errorf("sysctl command %q", q.sysctl)
			}
			if _, ok := q.runner.(argvSysctl); !ok {
				t.Errorf("sysctl runner %T, want argvSysctl", q.runner)
			}
		case SshdConfig:
			runs = append(runs, q.run)
		case SshdConfigSubsystem:
			runs = append(runs, q.run)
		}
	}
	if w := []string{"sysctl", "sshd_config", "sshd_config_subsystem"}; !slices.Equal(types, w) {
		t.Fatalf("providers %q, want %q", types, w)
	}
	if runs[0] == nil || runs[0] != runs[1] || runs[1] != runs[2] {
		t.Fatal("the providers do not share one run")
	}
	if runs[0].libdir != dir || len(runs[0].opened) != 0 {
		t.Errorf("run libdir %q, %d handle(s) opened", runs[0].libdir, len(runs[0].opened))
	}
	if again := a.Providers(); again[0].(Sysctl).run == runs[0] {
		t.Error("a second Providers call reuses the first run")
	}
	if got := (AugeasTypes{}).Providers(); got != nil {
		t.Errorf("no native type: %d provider(s)", len(got))
	}
}

// EndRun on any of the providers closes every handle the run opened, once.
func TestAugeasProvidersEndRun(t *testing.T) {
	lib := newScriptLib(t, parseTrace(t, "end", strings.Join([]string{
		`h1 aug_init NULL NULL 64`,
		`h1 aug_close`,
	}, "\n")+"\n"))
	a := AugeasTypes{SshConfig: true, SshdConfig: true, lib: lib, libdir: "/x"}
	ps := a.Providers()
	run := ps[0].(SshConfig).run
	if _, err := run.open(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		p.(RunEnding).EndRun()
	}
}

// argvSysctl is runArgvWith: Ruby's locale, combined output.
func TestArgvSysctlRunsWithRubyLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	code, out, err := argvSysctl{}.run(context.Background(), "/bin/sh", "-c", `echo "$LANG $LC_ALL"; echo e >&2`)
	if err != nil || code != 0 || out != "C C\ne\n" {
		t.Errorf("got %d %q %v", code, out, err)
	}
}

func TestLibdirTreeList(t *testing.T) {
	dir := augeasTree(t)
	got, err := LibdirTree(dir).List("puppet/provider")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if w := []string{"augeas", "augeasprovider", "ssh_config", "sshd_config", "sshd_config_subsystem", "sysctl"}; !slices.Equal(got, w) {
		t.Errorf("List = %q, want %q", got, w)
	}
	if _, err := LibdirTree(dir).List("puppet/none"); err == nil {
		t.Error("missing directory listed without an error")
	}
}

// sshdMatchNames: each Sshd_config_match's title, then its alias strings,
// flattened as Puppet flattens them; other types and non-strings are not names.
func TestSshdMatchNames(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Sshd_config_match", Title: "Host a", Parameters: map[string]any{"alias": "User b"}},
		{Type: "Sshd_config", Title: "Host z", Parameters: map[string]any{"alias": "User z"}},
		{Type: "sshd_config_match", Title: "m", Parameters: map[string]any{"alias": []any{"Host c", []any{"Host d"}, float64(1), true}}},
		{Type: "Sshd_config_match", Title: "n"},
	}}
	want := []string{"Host a", "User b", "m", "Host c", "Host d", "n"}
	if got := sshdMatchNames(cat); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
