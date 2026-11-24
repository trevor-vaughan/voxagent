package augeascore

import (
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

// augeasCoreFixtures are the committed copies of augeas_core 2.1.0's four
// fingerprinted files.
const augeasCoreFixtures = "testdata/lib"

// The committed fixtures are byte-identical to the release whose digests the
// manifest holds, so the manifest and the fixtures cannot drift.
func TestFixturesMatchManifest(t *testing.T) {
	files := Manifest.Releases[0].Files
	if len(files) != 4 {
		t.Fatalf("%d files, want 4", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(augeasCoreFixtures, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if got := raltest.SHA256Hex(b); got != f.SHA256 {
			t.Errorf("%s: sha256 %s, want %s", f.Path, got, f.SHA256)
		}
	}
}

// augeasRouting is a Route decision as the tests read it: whether the augeas
// type is native (with the lens path its provider carries), why it is not, and
// the providers themselves.
type augeasRouting struct {
	Augeas    bool
	Reasons   map[string]string
	lensPath  string
	providers []ral.Provider
}

func (a augeasRouting) Providers() []ral.Provider { return a.providers }

func envOf(vars map[string]string) ral.Environ { return func(k string) string { return vars[k] } }

// routeAugeasCore routes cat through the unit over a session on probe.
func routeAugeasCore(t ral.PluginTree, probe augeas.Probe, env ral.Environ, libdir, command string, cat *catalog.Catalog) augeasRouting {
	d := Unit.Route(&unit.Env{Catalog: cat, Libdir: libdir, Tree: t, Environ: env, Command: command, Augeas: augeas.NewSession(probe)})
	a := augeasRouting{Reasons: d.Delegated, providers: d.Providers}
	if a.Reasons == nil {
		a.Reasons = map[string]string{}
	}
	for _, pr := range d.Providers {
		if q, ok := pr.(Augeas); ok {
			a.Augeas, a.lensPath = true, q.lensPath
		}
	}
	return a
}

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
	return raltest.AugRes("Augeas", "x", map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"})
}

// routeAugeas routes rs over libdir with env and probe.
func routeAugeas(t *testing.T, libdir, command string, env map[string]string, probe augeas.Probe, rs ...catalog.Resource) augeasRouting {
	t.Helper()
	return routeAugeasCore(plugintree.LibdirTree(libdir), probe, envOf(env), libdir, command, &catalog.Catalog{Resources: rs})
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
		a := routeAugeas(t, augeasCoreTree(t), "apply", nil, &raltest.ProbeFake{T: t}, genericRes())
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
			a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, genericRes())
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
			a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, genericRes())
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
		a := routeAugeas(t, dir, "apply", nil, &raltest.ProbeFake{T: t}, genericRes())
		if a.Augeas || !strings.Contains(a.Reasons["augeas"], "parsed.rb") {
			t.Errorf("augeas %v, reason %q does not name parsed.rb", a.Augeas, a.Reasons["augeas"])
		}
	})
}

func TestNativeAugeasLensPath(t *testing.T) {
	dir := augeasCoreTree(t)
	lenses := filepath.Join(dir, "augeas", "lenses")
	route := func(command string) augeasRouting {
		return routeAugeas(t, dir, command, nil, &raltest.ProbeFake{T: t}, genericRes())
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
	a := routeAugeas(t, dir, "agent", nil, &raltest.ProbeFake{T: t}, genericRes())
	ps := a.Providers()
	if len(ps) != 1 || ps[0].Type() != "augeas" {
		t.Fatalf("providers %v", ps)
	}
	if g, ok := ps[0].(Augeas); !ok || g.lensPath != filepath.Join(dir, "augeas", "lenses") || g.lib == nil {
		t.Errorf("provider %#v", ps[0])
	}
}

// Conditions (b) and (c) apply to augeas, and it is probed only once its files
// match. (With the augeasproviders types too, the probe still runs at most once:
// internal/units' TestNativeAugeasGenericEnvironmentAndProbe.)
func TestNativeAugeasGenericProbeAlone(t *testing.T) {
	t.Run("augeas alone is probed", func(t *testing.T) {
		probe := &raltest.ProbeFake{T: t}
		routeAugeas(t, augeasCoreTree(t), "apply", nil, probe, genericRes())
		if probe.Calls != 1 {
			t.Errorf("probe calls %d, want 1", probe.Calls)
		}
	})
	t.Run("nothing left after (a): not probed", func(t *testing.T) {
		probe := &raltest.ProbeFake{T: t}
		routeAugeas(t, t.TempDir(), "apply", nil, probe, genericRes())
		if probe.Calls != 0 {
			t.Errorf("probe calls %d, want 0", probe.Calls)
		}
	})
}
