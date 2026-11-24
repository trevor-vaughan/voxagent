package ral

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// augeasCoreOnly is the augeas type's four files and nothing else, the set
// that routes augeas without any augeasproviders file.
var augeasCoreOnly = []string{
	"puppet/type/augeas.rb",
	"puppet/provider/augeas/augeas.rb",
	"puppet_x/augeas/util/parser.rb",
	"puppet/feature/augeas.rb",
}

// genericRes is a resource of the augeas type in a shape that stays native.
func genericRes() catalog.Resource {
	return augRes("Augeas", "x", map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"})
}

// routeAugeas routes rs over libdir on sysctlHost with env and probe.
func routeAugeas(t *testing.T, libdir, command string, env map[string]string, probe AugeasProbe, rs ...catalog.Resource) AugeasTypes {
	t.Helper()
	return NativeAugeas(LibdirTree(libdir), sysctlHost, probe, envOf(env), libdir, command, &catalog.Catalog{Resources: rs})
}

// augeasCoreTree is a libdir holding only augeas_core's fixtures.
func augeasCoreTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(augeasCoreFixtures)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNativeAugeasGenericFiles(t *testing.T) {
	t.Run("measured, augeas_core alone", func(t *testing.T) {
		a := routeAugeas(t, augeasCoreTree(t), "apply", nil, &augeasProbeFake{t: t}, genericRes())
		if !a.Augeas || len(a.Reasons) != 0 {
			t.Errorf("augeas %v, reasons %q", a.Augeas, a.Reasons)
		}
	})
	for _, rel := range augeasCoreOnly {
		t.Run(rel+" missing", func(t *testing.T) {
			dir := augeasCoreTree(t)
			if err := os.Remove(filepath.Join(dir, rel)); err != nil {
				t.Fatal(err)
			}
			a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, genericRes())
			if a.Augeas || !strings.Contains(a.Reasons["augeas"], rel) {
				t.Errorf("augeas %v, reason %q does not name %s", a.Augeas, a.Reasons["augeas"], rel)
			}
		})
		t.Run(rel+" altered", func(t *testing.T) {
			dir := augeasCoreTree(t)
			at := filepath.Join(dir, rel)
			b, err := os.ReadFile(at)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(at, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, genericRes())
			if a.Augeas || !strings.Contains(a.Reasons["augeas"], rel) {
				t.Errorf("augeas %v, reason %q does not name %s", a.Augeas, a.Reasons["augeas"], rel)
			}
		})
	}
	t.Run("extra provider", func(t *testing.T) {
		dir := augeasCoreTree(t)
		if err := os.WriteFile(filepath.Join(dir, "puppet", "provider", "augeas", "parsed.rb"), []byte("# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, genericRes())
		if a.Augeas || !strings.Contains(a.Reasons["augeas"], "parsed.rb") {
			t.Errorf("augeas %v, reason %q does not name parsed.rb", a.Augeas, a.Reasons["augeas"])
		}
	})
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
		a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, rs...)
		if !a.Augeas || a.Sysctl || a.SshConfig || a.SshdConfig || a.SshdConfigSubsystem || len(a.Reasons) != 4 {
			t.Errorf("augeas %v, four types native %v, reasons %q", a.Augeas, nativeSet(a), a.Reasons)
		}
	})
	t.Run("augeas_core absent", func(t *testing.T) {
		dir := augeasTree(t)
		if err := os.Remove(filepath.Join(dir, "puppet", "type", "augeas.rb")); err != nil {
			t.Fatal(err)
		}
		a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, rs...)
		checkRoute(t, a, allAugeasTypes)
		if a.Augeas || a.Reasons["augeas"] == "" {
			t.Errorf("augeas %v, reason %q", a.Augeas, a.Reasons["augeas"])
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
			probe := &augeasProbeFake{t: t}
			a := routeAugeas(t, dir, "apply", map[string]string{v: "x"}, probe, rs...)
			if a.Augeas || !strings.Contains(a.Reasons["augeas"], v) || probe.calls != 0 {
				t.Errorf("augeas %v, reason %q, probe calls %d", a.Augeas, a.Reasons["augeas"], probe.calls)
			}
		})
	}
	t.Run("libdir not clean", func(t *testing.T) {
		probe := &augeasProbeFake{t: t}
		a := routeAugeas(t, dir+"/", "apply", nil, probe, genericRes())
		if a.Augeas || a.Reasons["augeas"] == "" || probe.calls != 0 {
			t.Errorf("augeas %v, reason %q, probe calls %d", a.Augeas, a.Reasons["augeas"], probe.calls)
		}
	})
	t.Run("probe refuses", func(t *testing.T) {
		const why = "libaugeas: dlopen: no such file"
		a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t, reason: why}, rs...)
		if a.Augeas || a.Reasons["augeas"] != why {
			t.Errorf("augeas %v, reason %q, want %q", a.Augeas, a.Reasons["augeas"], why)
		}
	})
	t.Run("probed once for all five", func(t *testing.T) {
		probe := &augeasProbeFake{t: t}
		a := routeAugeas(t, dir, "apply", nil, probe, rs...)
		if !a.Augeas || probe.calls != 1 {
			t.Errorf("augeas %v, probe calls %d", a.Augeas, probe.calls)
		}
	})
	t.Run("augeas alone is probed", func(t *testing.T) {
		probe := &augeasProbeFake{t: t}
		routeAugeas(t, augeasCoreTree(t), "apply", nil, probe, genericRes())
		if probe.calls != 1 {
			t.Errorf("probe calls %d, want 1", probe.calls)
		}
	})
	t.Run("nothing left after (a): not probed", func(t *testing.T) {
		probe := &augeasProbeFake{t: t}
		routeAugeas(t, t.TempDir(), "apply", nil, probe, genericRes())
		if probe.calls != 0 {
			t.Errorf("probe calls %d, want 0", probe.calls)
		}
	})
}

// A resource shape the native code does not implement sends every augeas
// resource to the host, the reason naming the resource; the four types stay.
func TestNativeAugeasGenericShape(t *testing.T) {
	dir := augeasTree(t)
	bad := augRes("Augeas", "bad", map[string]any{"context": "/files/etc/hosts", "onlyif": "match 1 size > 0"})
	rs := append(augeasFour(), genericRes(), bad)
	a := routeAugeas(t, dir, "apply", nil, &augeasProbeFake{t: t}, rs...)
	checkRoute(t, a, allAugeasTypes)
	if a.Augeas || !strings.Contains(a.Reasons["augeas"], bad.Ref()) {
		t.Errorf("augeas %v, reason %q does not name %s", a.Augeas, a.Reasons["augeas"], bad.Ref())
	}
}

func TestNativeAugeasLensPath(t *testing.T) {
	dir := augeasCoreTree(t)
	lenses := filepath.Join(dir, "augeas", "lenses")
	route := func(command string) AugeasTypes {
		return routeAugeas(t, dir, command, nil, &augeasProbeFake{t: t}, genericRes())
	}
	if a := route("agent"); a.lensPath != "" {
		t.Errorf("agent without the directory: %q", a.lensPath)
	}
	if err := os.MkdirAll(lenses, 0o755); err != nil {
		t.Fatal(err)
	}
	if a := route("agent"); a.lensPath != lenses || !a.Augeas {
		t.Errorf("agent with the directory: %q", a.lensPath)
	}
	if a := route("apply"); a.lensPath != "" {
		t.Errorf("apply with the directory: %q", a.lensPath)
	}
	// Any file type satisfies File.exist?.
	if err := os.Remove(lenses); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lenses, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if a := route("agent"); a.lensPath != lenses {
		t.Errorf("agent with a plain file: %q", a.lensPath)
	}
}

func TestAugeasTypesProvidersGeneric(t *testing.T) {
	dir := augeasCoreTree(t)
	if err := os.MkdirAll(filepath.Join(dir, "augeas", "lenses"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := routeAugeas(t, dir, "agent", nil, &augeasProbeFake{t: t}, genericRes())
	ps := a.Providers()
	if len(ps) != 1 || ps[0].Type() != "augeas" {
		t.Fatalf("providers %v", ps)
	}
	if g, ok := ps[0].(Augeas); !ok || g.lensPath != filepath.Join(dir, "augeas", "lenses") || g.lib == nil {
		t.Errorf("provider %#v", ps[0])
	}
}
