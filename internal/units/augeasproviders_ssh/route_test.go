package augeasprovidersssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/raltest"
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

// three holds one resource of each type of the unit, in a shape that stays
// native.
func three() []catalog.Resource {
	return []catalog.Resource{
		raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr"}),
		raltest.AugRes("Sshd_config", "X11Forwarding", map[string]any{"value": "no"}),
		raltest.AugRes("Sshd_config_subsystem", "sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
	}
}

// route is Unit.Route over dir (as the plugin tree) with libdir ld, environment
// vars and probe, for a catalog holding rs.
func route(t *testing.T, dir, ld string, vars map[string]string, probe augeas.Probe, rs ...catalog.Resource) unit.Decision {
	t.Helper()
	return Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Libdir: ld, Tree: plugintree.LibdirTree(dir),
		Environ: func(k string) string { return vars[k] }, Command: "apply", Augeas: augeas.NewSession(probe)})
}

// The providers are built in routing order, all over the one run of the
// session, and SshdConfig reads the run's catalog.
func TestRouteProviders(t *testing.T) {
	dir := libdir(t)
	d := route(t, dir, dir, nil, &raltest.ProbeFake{T: t}, three()...)
	if len(d.Delegated) != 0 || len(d.Providers) != 3 {
		t.Fatalf("providers %v, delegated %v", d.Providers, d.Delegated)
	}
	a, ok1 := d.Providers[0].(SshConfig)
	b, ok2 := d.Providers[1].(SshdConfig)
	c, ok3 := d.Providers[2].(SshdConfigSubsystem)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("providers %T, %T, %T; want ssh_config, sshd_config, sshd_config_subsystem", d.Providers[0], d.Providers[1], d.Providers[2])
	}
	if a.run == nil || a.run != b.run || b.run != c.run {
		t.Error("the providers do not share one run")
	}
	if b.cat == nil {
		t.Error("sshd_config has no catalog")
	}
}

// Only the types the catalog holds are routed, and with none of them nothing is
// read or probed.
func TestRouteOnlyTypesPresent(t *testing.T) {
	dir := libdir(t)
	probe := &raltest.ProbeFake{T: t}
	d := route(t, dir, dir, nil, probe, raltest.AugRes("Notify", "n", nil))
	if len(d.Providers) != 0 || len(d.Delegated) != 0 || probe.Calls != 0 {
		t.Errorf("no ssh type: providers %v, delegated %v, probe calls %d", d.Providers, d.Delegated, probe.Calls)
	}
	d = route(t, dir, dir, nil, probe, three()[0])
	if len(d.Providers) != 1 || len(d.Delegated) != 0 || probe.Calls != 1 {
		t.Errorf("ssh_config only: providers %v, delegated %v, probe calls %d", d.Providers, d.Delegated, probe.Calls)
	}
}

// Each type is decided on its own, and the first failing check gives its reason,
// in spec §2's order: files, provider directory, environment, libdir, probe,
// unimplemented value.
func TestRouteFirstReason(t *testing.T) {
	const probeWhy = "libaugeas: dlopen: no such file"
	changed := func(t *testing.T, dir, rel string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unimplemented := raltest.AugRes("Ssh_config", "Ciphers", map[string]any{"comment": "c"})
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string)
		libdir func(dir string) string
		vars   map[string]string
		probe  *raltest.ProbeFake
		want   string
	}{
		{"core file", func(t *testing.T, dir string) { changed(t, dir, "puppet/type/augeasprovider.rb") }, nil, map[string]string{"AUGEAS_ROOT": "/x"},
			&raltest.ProbeFake{Reason: probeWhy}, "puppet/type/augeasprovider.rb has sha256"},
		{"type file", func(t *testing.T, dir string) { changed(t, dir, "puppet/type/ssh_config.rb") }, nil, map[string]string{"AUGEAS_ROOT": "/x"},
			&raltest.ProbeFake{Reason: probeWhy}, "puppet/type/ssh_config.rb has sha256"},
		{"provider dir", func(t *testing.T, dir string) { changed(t, dir, "puppet/provider/ssh_config/parsed.rb") }, nil, map[string]string{"AUGEAS_ROOT": "/x"},
			&raltest.ProbeFake{Reason: probeWhy}, "parsed.rb besides augeas.rb"},
		{"environment", nil, nil, map[string]string{"AUGEAS_ROOT": "/x"}, &raltest.ProbeFake{Reason: probeWhy}, "AUGEAS_ROOT is set"},
		{"libdir", nil, func(dir string) string { return dir + "/" }, nil, &raltest.ProbeFake{Reason: probeWhy}, "is not absolute and clean"},
		{"probe", nil, nil, nil, &raltest.ProbeFake{Reason: probeWhy}, probeWhy},
		{"unimplemented value", nil, nil, nil, &raltest.ProbeFake{}, unimplemented.Ref()},
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
			d := route(t, dir, ld, c.vars, c.probe, unimplemented)
			if len(d.Providers) != 0 || !strings.Contains(d.Delegated["ssh_config"], c.want) {
				t.Errorf("providers %v, reason %q, want one containing %q", d.Providers, d.Delegated["ssh_config"], c.want)
			}
		})
	}
	t.Run("types are decided on their own", func(t *testing.T) {
		dir := libdir(t)
		changed(t, dir, "puppet/type/sshd_config_subsystem.rb")
		d := route(t, dir, dir, nil, &raltest.ProbeFake{T: t}, three()...)
		if len(d.Providers) != 2 || !strings.Contains(d.Delegated["sshd_config_subsystem"], "puppet/type/sshd_config_subsystem.rb has sha256") || len(d.Delegated) != 1 {
			t.Errorf("providers %v, delegated %v", d.Providers, d.Delegated)
		}
	})
}
