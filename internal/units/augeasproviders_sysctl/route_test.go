package augeasproviderssysctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// libdir is a temp libdir holding this unit's testdata and augeasproviders_core's,
// which the unit's files list.
func libdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, lib := range []string{"../augeasproviders_core/testdata/lib", "testdata/lib"} {
		if err := os.CopyFS(dir, os.DirFS(lib)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var sysctlHost = raltest.FakePlatform{Exec: map[string]bool{"/usr/sbin/sysctl": true}, Release: raltest.CentOS10}

// route is Unit.Route over dir with the platform p, PATH path, environment
// vars and probe, for a catalog holding rs (a native Sysctl when none).
func route(t *testing.T, dir string, p ral.Platform, path string, vars map[string]string, probe augeas.Probe, rs ...catalog.Resource) unit.Decision {
	t.Helper()
	if len(rs) == 0 {
		rs = []catalog.Resource{raltest.AugRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"})}
	}
	env := func(k string) string {
		if k == "PATH" {
			return path
		}
		return vars[k]
	}
	return Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Libdir: dir, Tree: plugintree.LibdirTree(dir), Platform: p,
		Environ: env, Command: "apply", Augeas: augeas.NewSession(probe)})
}

// A native Sysctl carries the resolved command, the argv runner and a run over
// the probed library; the type goes undelegated.
func TestRouteProvider(t *testing.T) {
	dir := libdir(t)
	d := route(t, dir, sysctlHost, "", nil, &raltest.ProbeFake{T: t})
	if len(d.Providers) != 1 || len(d.Delegated) != 0 {
		t.Fatalf("providers %v, delegated %v", d.Providers, d.Delegated)
	}
	p, ok := d.Providers[0].(Sysctl)
	if !ok || p.sysctl != "/usr/sbin/sysctl" || p.run == nil {
		t.Fatalf("provider %#v", d.Providers[0])
	}
	if _, ok := p.runner.(argvSysctl); !ok {
		t.Errorf("runner %T, want argvSysctl", p.runner)
	}
}

// Only the types the catalog holds are routed: with no Sysctl nothing is read,
// probed or delegated.
func TestRouteNoSysctl(t *testing.T) {
	probe := &raltest.ProbeFake{T: t}
	d := route(t, t.TempDir(), sysctlHost, "", nil, probe, raltest.AugRes("Notify", "n", nil))
	if len(d.Providers) != 0 || len(d.Delegated) != 0 || probe.Calls != 0 {
		t.Errorf("providers %v, delegated %v, probe calls %d", d.Providers, d.Delegated, probe.Calls)
	}
	// "ſysctl" (U+017F) is EqualFold-equal to "sysctl" but not case-mapped to it.
	d = route(t, t.TempDir(), sysctlHost, "", nil, probe, raltest.AugRes("ſysctl", "x", nil))
	if len(d.Providers) != 0 || len(d.Delegated) != 0 || probe.Calls != 0 {
		t.Errorf("ſysctl: providers %v, delegated %v, probe calls %d", d.Providers, d.Delegated, probe.Calls)
	}
}

// §2 (g): the measured platform, and sysctl resolved over PATH as
// Puppet::Util.which does, then /usr/sbin and /sbin.
func TestRoutePlatform(t *testing.T) {
	dir := libdir(t)
	exec := map[string]bool{"/usr/sbin/sysctl": true}
	cases := []struct {
		name    string
		p       raltest.FakePlatform
		path    string
		command string // "" when delegated
	}{
		{"CentOS Stream 10", sysctlHost, "", "/usr/sbin/sysctl"},
		{"PATH first", raltest.FakePlatform{Exec: map[string]bool{"/usr/sbin/sysctl": true, "/opt/x/sysctl": true}, Release: raltest.CentOS10}, "/opt/x:/usr/bin", "/opt/x/sysctl"},
		{"/sbin appended", raltest.FakePlatform{Exec: map[string]bool{"/sbin/sysctl": true}, Release: raltest.CentOS10}, "/usr/bin", "/sbin/sysctl"},
		{"Fedora", raltest.FakePlatform{Exec: exec, Release: "ID=fedora\nID_LIKE=\"rhel\"\nVERSION_ID=41\n"}, "", ""},
		{"RHEL 9", raltest.FakePlatform{Exec: exec, Release: "ID=rhel\nVERSION_ID=\"9.4\"\n"}, "", ""},
		{"os-release unreadable", raltest.FakePlatform{Exec: exec, Err: os.ErrNotExist}, "", ""},
		{"no sysctl", raltest.FakePlatform{Release: raltest.CentOS10}, "/usr/bin", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := route(t, dir, c.p, c.path, nil, &raltest.ProbeFake{T: t})
			if c.command == "" {
				if len(d.Providers) != 0 || d.Delegated["sysctl"] == "" {
					t.Errorf("providers %v, reasons %v; want delegated with a reason", d.Providers, d.Delegated)
				}
				return
			}
			if len(d.Providers) != 1 || d.Providers[0].(Sysctl).sysctl != c.command {
				t.Errorf("providers %v, want one with command %q", d.Providers, c.command)
			}
		})
	}
}

// The first failing check gives the reason, in spec §2's order: files, provider
// directory, environment, libdir, probe, unimplemented value, release, command.
func TestRouteFirstReason(t *testing.T) {
	const probeWhy = "libaugeas: dlopen: no such file"
	unimplemented := raltest.AugRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"val": "1"})
	// each case breaks its own condition and every later one, so the reason
	// names exactly the earliest.
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		libdir func(dir string) string
		vars   map[string]string
		probe  *raltest.ProbeFake
		p      raltest.FakePlatform
		path   string
		rs     []catalog.Resource
		want   string
	}{
		{"core file", func(t *testing.T, dir string) { edit(t, dir, "puppet/type/augeasprovider.rb") }, nil, map[string]string{"AUGEAS_ROOT": "/x"},
			&raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, "puppet/type/augeasprovider.rb has sha256"},
		{"type file", func(t *testing.T, dir string) { edit(t, dir, "puppet/type/sysctl.rb") }, nil, map[string]string{"AUGEAS_ROOT": "/x"},
			&raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, "puppet/type/sysctl.rb has sha256"},
		{"provider dir", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "puppet/provider/sysctl/parsed.rb"), []byte("# x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, nil, map[string]string{"AUGEAS_ROOT": "/x"}, &raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, "parsed.rb besides augeas.rb"},
		{"environment", nil, nil, map[string]string{"AUGEAS_ROOT": "/x"}, &raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, "AUGEAS_ROOT is set"},
		{"libdir", nil, func(dir string) string { return dir + "/" }, nil, &raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, "is not absolute and clean"},
		{"probe", nil, nil, nil, &raltest.ProbeFake{Reason: probeWhy}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, probeWhy},
		{"unimplemented value", nil, nil, nil, &raltest.ProbeFake{}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", []catalog.Resource{unimplemented}, unimplemented.Ref()},
		{"release", nil, nil, nil, &raltest.ProbeFake{}, raltest.FakePlatform{Release: "ID=rhel\nVERSION_ID=\"9.4\"\n"}, "/none", nil, "the platform is not RHEL family 10 or later (Fedora excluded)"},
		{"command", nil, nil, nil, &raltest.ProbeFake{}, raltest.FakePlatform{Release: raltest.CentOS10}, "/none", nil, "no executable sysctl on PATH, /usr/sbin or /sbin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := libdir(t)
			if c.setup != nil {
				c.setup(t, dir)
			}
			ld := dir
			if c.libdir != nil {
				ld = c.libdir(dir)
			}
			c.probe.T = t
			rs := c.rs
			if rs == nil {
				rs = []catalog.Resource{raltest.AugRes("Sysctl", "net.ipv4.ip_forward", map[string]any{"value": "1"})}
			}
			env := func(k string) string {
				if k == "PATH" {
					return c.path
				}
				return c.vars[k]
			}
			d := Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Libdir: ld, Tree: plugintree.LibdirTree(dir), Platform: c.p,
				Environ: env, Command: "apply", Augeas: augeas.NewSession(c.probe)})
			if len(d.Providers) != 0 || !strings.Contains(d.Delegated["sysctl"], c.want) {
				t.Errorf("providers %v, reason %q, want one containing %q", d.Providers, d.Delegated["sysctl"], c.want)
			}
		})
	}
}

// edit changes one byte of dir/rel.
func edit(t *testing.T, dir, rel string) {
	t.Helper()
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

// argvSysctl is runArgvWith: Ruby's locale, combined output.
func TestArgvSysctlRunsWithRubyLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	code, out, err := argvSysctl{}.run(context.Background(), "/bin/sh", "-c", `echo "$LANG $LC_ALL"; echo e >&2`)
	if err != nil || code != 0 || out != "C C\ne\n" {
		t.Errorf("got %d %q %v", code, out, err)
	}
}
