package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/transaction"
)

// augeasLibdir returns a temp libdir holding the twelve augeasproviders files
// committed under internal/ral/testdata/augeasproviders (see its README),
// then each extra tree.
func augeasLibdir(t *testing.T, extra ...string) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	lib := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "ral", "testdata", "augeasproviders", "lib")
	dir := t.TempDir()
	for _, src := range append([]string{lib}, extra...) {
		if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakeAugeasLib is libaugeas answering every call with success and nothing
// found; it records which handles were opened and how often each was closed.
type fakeAugeasLib struct {
	next      augeas.Handle
	closed    map[augeas.Handle]int
	loadpaths []string // the loadpath each Init received; "<nil>" for NULL
}

func (f *fakeAugeasLib) Init(root, loadpath *string, flags uint) (augeas.Handle, bool) {
	f.next++
	if loadpath == nil {
		f.loadpaths = append(f.loadpaths, "<nil>")
	} else {
		f.loadpaths = append(f.loadpaths, *loadpath)
	}
	if f.closed == nil {
		f.closed = map[augeas.Handle]int{}
	}
	f.closed[f.next] = 0
	return f.next, true
}
func (f *fakeAugeasLib) Close(h augeas.Handle)                               { f.closed[h]++ }
func (f *fakeAugeasLib) Get(augeas.Handle, string) (*string, int)            { return nil, 0 }
func (f *fakeAugeasLib) Set(augeas.Handle, string, *string) int              { return 0 }
func (f *fakeAugeasLib) Rm(augeas.Handle, string) int                        { return 0 }
func (f *fakeAugeasLib) Insert(augeas.Handle, string, string, bool) int      { return 0 }
func (f *fakeAugeasLib) Mv(augeas.Handle, string, string) int                { return 0 }
func (f *fakeAugeasLib) Match(augeas.Handle, string) ([]string, int)         { return []string{}, 0 }
func (f *fakeAugeasLib) Load(augeas.Handle) int                              { return 0 }
func (f *fakeAugeasLib) Save(augeas.Handle) int                              { return 0 }
func (f *fakeAugeasLib) Defvar(augeas.Handle, string, string) int            { return 0 }
func (f *fakeAugeasLib) Defnode(augeas.Handle, string, string, *string) int  { return 0 }
func (f *fakeAugeasLib) TextStore(augeas.Handle, string, string, string) int { return 0 }

// fakeAugeasProbe answers the library probe with lib, or fails with reason.
type fakeAugeasProbe struct {
	lib    *fakeAugeasLib
	reason string
	calls  int
}

func (p *fakeAugeasProbe) Probe() (augeas.Lib, string, bool) {
	p.calls++
	if p.reason != "" {
		return nil, p.reason, false
	}
	return p.lib, "", true
}

// augeasPlatform is CentOS Stream 10 with sysctl in /usr/sbin.
type augeasPlatform struct{}

func (augeasPlatform) Executable(path string) bool { return path == "/usr/sbin/sysctl" }
func (augeasPlatform) OSRelease() ([]byte, error) {
	return []byte("ID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"10\"\n"), nil
}

func environOf(vars map[string]string) ral.Environ { return func(k string) string { return vars[k] } }

// A type is native only with the measured tree, a clean environment and a
// library the probe accepts; otherwise it needs the host as today, and the
// reason says why.
func TestAugeasIsNativeOnlyWithAMeasuredTree(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Sysctl", Title: "vm.swappiness", Parameters: map[string]any{"value": "10"}},
		{Type: "Ssh_config", Title: "Ciphers", Parameters: map[string]any{"value": "aes256-ctr"}},
	}}
	appended := func(t *testing.T) string {
		dir := augeasLibdir(t)
		at := filepath.Join(dir, "puppet", "type", "sysctl.rb")
		b, err := os.ReadFile(at)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	measured := func(t *testing.T) string { return augeasLibdir(t) }
	for _, tc := range []struct {
		name   string
		libdir func(t *testing.T) string
		env    map[string]string
		reason string
		host   []string
		probed int
		why    string
	}{
		{"measured", measured, nil, "", nil, 1, ""},
		{"empty libdir", func(t *testing.T) string { return t.TempDir() }, nil, "", []string{"ssh_config", "sysctl"}, 0, "puppet/provider/augeasprovider/default.rb"},
		{"sysctl.rb changed", appended, nil, "", []string{"sysctl"}, 1, "puppet/type/sysctl.rb"},
		{"AUGEAS_ROOT set", measured, map[string]string{"AUGEAS_ROOT": "/tmp"}, "", []string{"ssh_config", "sysctl"}, 0, "AUGEAS_ROOT"},
		{"LD_PRELOAD set", measured, map[string]string{"LD_PRELOAD": "x.so"}, "", []string{"ssh_config", "sysctl"}, 0, "LD_PRELOAD"},
		{"probe fails", measured, nil, "libaugeas: dlopen: gone", []string{"ssh_config", "sysctl"}, 1, "libaugeas: dlopen: gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &fakeAugeasProbe{lib: &fakeAugeasLib{}, reason: tc.reason}
			o := applyOptions{libdir: tc.libdir(t), augeasProbe: probe, environ: environOf(tc.env)}
			natives, reasons := nativeProviders(cat, o, augeasPlatform{})
			claimed := map[string]struct{}{}
			for _, p := range natives {
				claimed[strings.ToLower(p.Type())] = struct{}{}
			}
			if got := nonNativeTypes(cat, claimed); !slices.Equal(got, tc.host) {
				t.Errorf("host types %q, want %q (reasons %q)", got, tc.host, reasons)
			}
			if probe.calls != tc.probed {
				t.Errorf("probe called %d time(s), want %d", probe.calls, tc.probed)
			}
			for _, typ := range tc.host {
				if !strings.Contains(reasons[typ], tc.why) {
					t.Errorf("%s reason %q does not contain %q", typ, reasons[typ], tc.why)
				}
			}
			if len(probe.lib.closed) != 0 {
				t.Errorf("routing opened %d handle(s)", len(probe.lib.closed))
			}
		})
	}
}

// --no-ruby refuses a delegated augeas type exactly as any other host type,
// and applies a native one without Ruby.
func TestAugeasNoRuby(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		refused bool
	}{{"native", nil, false}, {"delegated", map[string]string{"LD_LIBRARY_PATH": "/x"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			o := applyOptions{noRuby: true, libdir: augeasLibdir(t), level: "notice",
				augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(tc.env)}
			stderr, couldNot := applyInProcess(t, []map[string]any{
				ext("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr", "target": filepath.Join(dir, "ssh_config")}),
			}, o)
			refusal := strings.Contains(stderr, "voxagent: --no-ruby: type(s) ssh_config need the extension host; nothing applied")
			if couldNot != tc.refused || refusal != tc.refused {
				t.Errorf("could-not-run %v, refusal %v; want %v:\n%s", couldNot, refusal, tc.refused, stderr)
			}
		})
	}
}

// The run's teardown closes every handle the native types opened, once:
// the per-run state nativeProviders built is the providers' RunEnding.
func TestAugeasRunEndedAfterApply(t *testing.T) {
	dir := t.TempDir()
	lib := &fakeAugeasLib{}
	o := applyOptions{noRuby: true, libdir: augeasLibdir(t), level: "notice",
		augeasProbe: &fakeAugeasProbe{lib: lib}, environ: environOf(nil)}
	stderr, couldNot := applyInProcess(t, []map[string]any{
		ext("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr", "target": filepath.Join(dir, "ssh_config")}),
		ext("Sshd_config", "X11Forwarding", map[string]any{"value": "no", "target": filepath.Join(dir, "sshd_config")}),
		ext("Sshd_config_subsystem", "sftp", map[string]any{"command": "/bin/sftp", "target": filepath.Join(dir, "sshd_config")}),
	}, o)
	if couldNot {
		t.Fatalf("could not run:\n%s", stderr)
	}
	if len(lib.closed) < 3 {
		t.Fatalf("%d handle(s) opened, want one per type at least", len(lib.closed))
	}
	for h, n := range lib.closed {
		if n != 1 {
			t.Errorf("handle %d closed %d time(s)", h, n)
		}
	}
}

// Delegating a catalog the host loads is logged at debug with its reason.
func TestAugeasDelegationIsLoggedAtDebug(t *testing.T) {
	ruby := extensiontest.Ruby(t)
	dir := t.TempDir()
	raw, err := os.ReadFile(writeCatalog(t, dir, []map[string]any{
		ext("Vox_value", "v", map[string]any{"path": filepath.Join(dir, "v"), "value": "x"}),
		ext("Ssh_config", "Ciphers", map[string]any{"value": "aes256-ctr", "target": filepath.Join(dir, "ssh_config")}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	o := applyOptions{noop: true, ruby: ruby, libdir: augeasLibdir(t, extensiontest.Libdir(t)), vardir: t.TempDir(),
		level: "debug", logger: runlog.New("debug", nil, nil), certname: "n.example",
		timeouts:    extension.Timeouts{Evaluate: time.Minute, Other: time.Minute},
		augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(map[string]string{"AUGEAS_DEBUG": "1"})}
	_, rep := applyCatalog(raw, o)
	if rep == nil {
		t.Fatal("the run was refused")
	}
	want := "ssh_config applied via extension host: AUGEAS_DEBUG is set"
	if !slices.ContainsFunc(rep.Logs, func(e report.LogEntry) bool {
		return e.Level == "debug" && e.Source == "voxagent" && e.Message == want
	}) {
		t.Errorf("report logs lack the debug line %q", want)
	}
}

// matchStub stands in for the host's sshd_config_match: it knows a resource
// by each string of its alias metaparameter, as the host reports Puppet's
// explicit aliases.
type matchStub struct{}

func (matchStub) Type() string                                              { return "sshd_config_match" }
func (matchStub) ProviderUsed() *string                                     { return nil }
func (matchStub) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (matchStub) Attrs() ral.Attrs                                          { return ral.NewAttrs("name", "condition", "target", "alias") }
func (matchStub) Apply(context.Context, catalog.Resource, bool) ([]ral.Change, []ral.Warning, error) {
	return nil, nil, nil
}
func (m matchStub) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) { return m, nil }
func (matchStub) Refresh(context.Context, catalog.Resource, bool) (bool, error)        { return false, nil }
func (matchStub) Aliases(r catalog.Resource) []string {
	if a, ok := r.Parameters["alias"].(string); ok {
		return []string{"Sshd_config_match[" + a + "]"}
	}
	return nil
}

// A native Sshd_config requires the catalog's Sshd_config_match whose title
// or alias names its condition with the keys in another order, and no other.
func TestSshdConfigRequiresItsMatchByTitleOrAlias(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Sshd_config", Title: "PermitRootLogin", Parameters: map[string]any{"value": "no", "condition": "Host a User b Group c"}},
		{Type: "Sshd_config_match", Title: "User b Group c Host a"},
		{Type: "Sshd_config_match", Title: "aliased", Parameters: map[string]any{"alias": "Group c Host a User b in "}},
		{Type: "Sshd_config_match", Title: "Host a User b"},
		{Type: "Sshd_config_match", Title: "Host a  User b Group c"},
	}}
	o := applyOptions{libdir: augeasLibdir(t), augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(nil)}
	natives, reasons := nativeProviders(cat, o, augeasPlatform{})
	if len(reasons) != 0 {
		t.Fatalf("sshd_config delegated: %q", reasons)
	}
	g, err := transaction.BuildGraph(t.Context(), cat, ral.NewRegistry(append(natives, matchStub{})...))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	got := g.Predecessors("Sshd_config[PermitRootLogin]")
	// Predecessors are sorted; the edges' order is TestAugeasAutoRequires'.
	want := []string{"Sshd_config_match[User b Group c Host a]", "Sshd_config_match[aliased]"}
	if !slices.Equal(got, want) {
		t.Errorf("Sshd_config[PermitRootLogin] requires %q, want %q", got, want)
	}
}

// A Sshd_config_match the host generates after routing, as a purging
// resources does, still gets the native Sshd_config's edge: BuildGraph runs
// on the merged catalog, and so does the autorequire.
func TestSshdConfigRequiresAGeneratedMatch(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Resources", Title: "sshd_config_match", Parameters: map[string]any{"purge": true}},
		{Type: "Sshd_config", Title: "PermitRootLogin", Parameters: map[string]any{"value": "no", "condition": "Host a", "target": "/etc/ssh/sshd_config"}},
	}}
	o := applyOptions{libdir: augeasLibdir(t), augeasProbe: &fakeAugeasProbe{lib: &fakeAugeasLib{}}, environ: environOf(nil)}
	natives, reasons := nativeProviders(cat, o, augeasPlatform{})
	if len(reasons) != 0 {
		t.Fatalf("sshd_config delegated: %q", reasons)
	}
	const generated = "Sshd_config_match[Host a in /etc/ssh/sshd_config]"
	if err := catalog.MergeGeneration(cat, catalog.Generation{
		Resources:  []catalog.Resource{{Type: "Sshd_config_match", Title: "Host a in /etc/ssh/sshd_config", Parameters: map[string]any{"ensure": "absent"}}},
		Generators: map[string]string{generated: "Resources[sshd_config_match]"},
	}, catalog.PlaceAppend); err != nil {
		t.Fatal(err)
	}
	g, err := transaction.BuildGraph(t.Context(), cat, ral.NewRegistry(append(natives, matchStub{})...))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if !g.HasEdge(generated, "Sshd_config[PermitRootLogin]") {
		t.Errorf("no edge from %s to Sshd_config[PermitRootLogin]; predecessors %q", generated, g.Predecessors("Sshd_config[PermitRootLogin]"))
	}
}

// The generic augeas type is routed on augeas_core's files alone, takes the
// run's command for its lens path, and a catalog shape the native code does
// not implement sends it to the host like any other delegated type.
func TestAugeasGenericRouting(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	libdir := t.TempDir()
	core := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "ral", "testdata", "augeas_core", "lib")
	if err := os.CopyFS(libdir, os.DirFS(core)); err != nil {
		t.Fatal(err)
	}
	native := ext("Augeas", "hosts", map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"})
	refused := ext("Augeas", "hosts", map[string]any{"context": "/files/etc/hosts", "onlyif": "match 1 size > 0"})
	for _, tc := range []struct {
		name string
		res  map[string]any
		host bool
	}{{"supported shape", native, false}, {"onlyif", refused, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cat := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Augeas", Title: "hosts", Parameters: tc.res["parameters"].(map[string]any)}}}
			probe := &fakeAugeasProbe{lib: &fakeAugeasLib{}}
			o := applyOptions{command: "apply", libdir: libdir, augeasProbe: probe, environ: environOf(nil)}
			natives, reasons := nativeProviders(cat, o, augeasPlatform{})
			claimed := map[string]struct{}{}
			for _, p := range natives {
				claimed[strings.ToLower(p.Type())] = struct{}{}
			}
			if _, has := claimed["augeas"]; has == tc.host {
				t.Errorf("augeas claimed %v, want %v (reasons %q)", has, !tc.host, reasons)
			}
			if _, has := reasons["augeas"]; has != tc.host {
				t.Errorf("augeas reason present %v, want %v", has, tc.host)
			}
		})
	}
}

// The run command reaches the native augeas provider: under agent it opens its
// handle with <libdir>/augeas/lenses, under apply with "" (provider/augeas/
// augeas.rb:362-387), so a provider fed a constant instead of o.command fails
// one of the two rows.
func TestAugeasGenericLensPathFollowsTheRunCommand(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	core := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "ral", "testdata", "augeas_core", "lib")
	for _, tc := range []struct {
		command string
		want    func(libdir string) string
	}{
		{"agent", func(libdir string) string { return filepath.Join(libdir, "augeas", "lenses") }},
		{"apply", func(string) string { return "" }},
	} {
		t.Run(tc.command, func(t *testing.T) {
			libdir := t.TempDir()
			if err := os.CopyFS(libdir, os.DirFS(core)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(libdir, "augeas", "lenses"), 0o755); err != nil {
				t.Fatal(err)
			}
			res := catalog.Resource{Type: "Augeas", Title: "hosts", Parameters: map[string]any{"context": "/files/etc/hosts", "changes": "set 1/ipaddr 1.2.3.4"}}
			lib := &fakeAugeasLib{}
			o := applyOptions{command: tc.command, libdir: libdir, augeasProbe: &fakeAugeasProbe{lib: lib}, environ: environOf(nil)}
			natives, reasons := nativeProviders(&catalog.Catalog{Resources: []catalog.Resource{res}}, o, augeasPlatform{})
			i := slices.IndexFunc(natives, func(p ral.Provider) bool { return strings.EqualFold(p.Type(), "augeas") })
			if i < 0 {
				t.Fatalf("augeas not native: %q", reasons)
			}
			prov, err := natives[i].Prefetch(context.Background(), []catalog.Resource{res})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := prov.Apply(context.Background(), res, false); err != nil {
				t.Fatal(err)
			}
			if want := tc.want(libdir); !slices.Equal(lib.loadpaths, []string{want}) {
				t.Errorf("aug_init loadpaths %q, want [%q]", lib.loadpaths, want)
			}
		})
	}
}
