package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension/extensiontest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/units"
	"github.com/voxagent/voxagent/internal/units/concat"
)

// canaryBase is one minimal valid resource of typ, its paths under dir. A
// type with no base returns an empty title: every unit's type needs one.
func canaryBase(typ, dir string) (title string, params map[string]any) {
	switch typ {
	case "augeas":
		return "canary-augeas", map[string]any{"incl": filepath.Join(dir, "augeas.conf"), "lens": "Shellvars.lns", "changes": "set VOXCANARY yes"}
	case "ssh_config":
		return "VoxCanary", map[string]any{"value": "yes", "target": filepath.Join(dir, "ssh_config")}
	case "sshd_config":
		return "VoxCanary", map[string]any{"value": "yes", "target": filepath.Join(dir, "sshd_config")}
	case "sshd_config_subsystem":
		return "voxcanary", map[string]any{"command": "/bin/true", "target": filepath.Join(dir, "sshd_config")}
	case "sysctl":
		return "voxcanary.key", map[string]any{"value": "1", "target": filepath.Join(dir, "sysctl.conf")}
	case "concat_file":
		return filepath.Join(dir, "canary-concat_file"), map[string]any{}
	case "concat_fragment":
		return "canary-concat_fragment", map[string]any{"target": filepath.Join(dir, "canary-concat-target"), "content": "x\n"}
	case "cron":
		return "canary-cron", map[string]any{"command": "/bin/true"}
	case "exec":
		return "canary-exec", map[string]any{"command": "/bin/true", "cwd": dir}
	case "file":
		return filepath.Join(dir, "canary-file"), map[string]any{"ensure": "file"}
	case "group":
		return "voxcanary", map[string]any{"ensure": "present"}
	case "host":
		return "voxcanary", map[string]any{"ip": "127.0.0.1", "target": filepath.Join(dir, "hosts")}
	case "ini_section":
		return "voxcanary", map[string]any{"path": filepath.Join(dir, "canary.ini")}
	case "ini_setting":
		return "voxcanary", map[string]any{"path": filepath.Join(dir, "canary.ini"), "section": "s", "setting": "k", "value": "v"}
	case "ini_subsetting":
		return "voxcanary", map[string]any{"path": filepath.Join(dir, "canary.ini"), "section": "s", "setting": "k", "subsetting": "a", "value": "v"}
	case "mount":
		return "/mnt/voxcanary", map[string]any{"device": "/dev/null", "fstype": "ext4", "target": filepath.Join(dir, "fstab")}
	case "notify":
		return "canary-notify", map[string]any{}
	case "package":
		return "voxcanary-none", map[string]any{"ensure": "present"}
	case "service":
		return "voxcanary", map[string]any{"ensure": "running"}
	case "ssh_authorized_key":
		return "voxcanary", map[string]any{"type": "ssh-rsa", "key": "AAAA", "user": "root", "target": filepath.Join(dir, "authorized_keys")}
	case "sshkey":
		return "voxcanary", map[string]any{"type": "rsa", "key": "AAAA", "target": filepath.Join(dir, "known_hosts")}
	case "tidy":
		// An empty directory: the generator adds no File, and the title stays
		// absolute when runCanary appends "-param" to it.
		return filepath.Join(dir, "canary-tidy"), map[string]any{"age": "1d", "recurse": true}
	case "user":
		return "voxcanary", map[string]any{"ensure": "present"}
	case "vox_conf", "vox_die", "vox_titled":
		return "canary-" + typ, map[string]any{"value": "v"}
	case "vox_deep", "vox_evalgen", "vox_gen", "vox_rapi":
		return "canary-" + typ, map[string]any{}
	case "vox_prefetch":
		return "canary-vox_prefetch", map[string]any{"ensure": "present", "provider": "good"}
	case "vox_secret":
		// raw is absent: its munge refuses every value.
		return "canary-vox_secret", map[string]any{"echo": "e"}
	case "vox_selfref":
		return "canary-vox_selfref", map[string]any{"ensure": "present", "value": "v"}
	case "vox_value":
		return "canary-vox_value", map[string]any{"path": filepath.Join(dir, "vox_value"), "value": "v"}
	}
	return "", nil
}

// canaryNamevars is each swept type's namevar, from its Ruby type: nothing
// in voxagent records namevars.
var canaryNamevars = map[string]string{
	"augeas": "name", "ssh_config": "name", "sshd_config": "name", "sshd_config_subsystem": "name", "sysctl": "name",
	"concat_file": "path", "concat_fragment": "name", "cron": "name", "exec": "command", "file": "path", "group": "name", "host": "name", "ini_section": "name", "ini_setting": "name", "ini_subsetting": "name", "mount": "name",
	"notify": "name", "package": "name", "service": "name", "ssh_authorized_key": "name", "sshkey": "name", "tidy": "path", "user": "name",
	"vox_conf": "name", "vox_deep": "name", "vox_die": "name", "vox_evalgen": "name", "vox_gen": "name",
	"vox_prefetch": "name", "vox_rapi": "name", "vox_secret": "name", "vox_selfref": "name", "vox_titled": "name",
	"vox_value": "name",
}

// canaryHostTypes are the extension host's fixture types
// (internal/extension/testdata/libdir) and the attributes each declares: the
// host does not report a type's attributes, so they are listed here.
var canaryHostTypes = map[string][]string{
	"vox_conf":     {"name", "provider", "value"},
	"vox_deep":     {"name"},
	"vox_die":      {"name", "provider", "value"},
	"vox_evalgen":  {"existing", "make", "name"},
	"vox_gen":      {"make", "name"},
	"vox_prefetch": {"ensure", "name", "provider"},
	"vox_rapi":     {"name"},
	"vox_secret":   {"echo", "extra", "name", "provider", "raw", "state", "token"},
	"vox_selfref":  {"ensure", "exists_now", "fail_refresh", "fail_set", "name", "other", "provider", "value"},
	"vox_titled":   {"bootmode", "fail_set", "name", "provider", "value"},
	"vox_value":    {"name", "path", "peer", "provider", "value"},
}

// TestCanaryHostTypesCoverTheFixtures holds canaryHostTypes to the fixture
// type files: every file has a row, and every parameter or property it
// declares, ensure included when it is ensurable, is in that row.
func TestCanaryHostTypesCoverTheFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(extensiontest.Libdir(t), "puppet", "type", "*.rb"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture type files: %v", err)
	}
	declared := regexp.MustCompile(`new(?:param|property)\(\s*:(\w+)|(?m)^\s*(ensurable)\b`)
	for _, f := range files {
		typ := strings.TrimSuffix(filepath.Base(f), ".rb")
		row, ok := canaryHostTypes[typ]
		if !ok {
			t.Errorf("%s has no canaryHostTypes row", f)
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range declared.FindAllSubmatch(src, -1) {
			name := string(m[1])
			if m[2] != nil {
				name = "ensure"
			}
			if !slices.Contains(row, name) {
				t.Errorf("canaryHostTypes[%q] lacks %s, which %s declares", typ, name, f)
			}
		}
	}
}

// canaryRealTypes are the types whose real applies stay under the test's
// temp dir; every other type runs --noop only unless VOXAGENT_CANARY_REAL=1.
var canaryRealTypes = map[string]bool{"file": true, "concat_file": true, "concat_fragment": true, "notify": true, "exec": true}

// canaryLibdir is a libdir holding every module unit's measured files, so
// each type is native wherever the host allows it.
func canaryLibdir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	core := t.TempDir()
	if err := os.CopyFS(core, os.DirFS(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "units", "augeas_core", "testdata", "lib"))); err != nil {
		t.Fatal(err)
	}
	// augeasLibdir already holds this file, byte for byte, and CopyFS
	// refuses to overwrite.
	if err := os.Remove(filepath.Join(core, "puppet", "feature", "augeas.rb")); err != nil {
		t.Fatal(err)
	}
	return augeasLibdir(t, core, concatModuleLib(t, concat.Concat1001), inifileModuleLib(t, "6.6.1"))
}

// TestSensitiveCanarySweep marks every parameter of every native type, one
// resource per parameter holding the canary CANARY-<type>-<param>, and
// requires that no canary reaches stdout, stderr or the report under any mode.
// With VOXAGENT_CANARY_LIBDIR, a libdir whose module type files no longer
// match their fingerprints, every module unit type must route to the
// extension host, and the host's fixture types are swept too.
func TestSensitiveCanarySweep(t *testing.T) {
	bin := os.Getenv("VOXAGENT_BIN")
	if bin == "" {
		bin = buildBinary(t)
	}
	// Each run's working directory is its own temp dir.
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	realApply := os.Getenv("VOXAGENT_CANARY_REAL") == "1"
	runLibdir := os.Getenv("VOXAGENT_CANARY_LIBDIR")
	measured := canaryLibdir(t)

	var types []string
	module := map[string]bool{}
	for _, u := range units.All() {
		m := u.Manifest()
		types = append(types, m.Types...)
		for _, typ := range m.Types {
			module[typ] = len(m.Releases) > 0
		}
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			probeDir := t.TempDir()
			title, params := canaryBase(typ, probeDir)
			if title == "" {
				t.Fatalf("no canaryBase for %s: every unit type needs one", typ)
			}
			// Route a catalog of the base alone: the native provider gives
			// the declared attributes; a delegated type has none to sweep.
			probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: typ, Title: title, Parameters: params}}}
			natives, delegated := nativeProviders(probe, applyOptions{command: "apply", libdir: measured}, ral.HostPlatform)
			if why, ok := delegated[typ]; ok {
				t.Skipf("%s is not native on this host: %s", typ, why)
			}
			var attrs []string
			for _, p := range natives {
				if p.Type() == typ {
					attrs = p.Attrs().Sorted()
				}
			}
			if attrs == nil {
				t.Fatalf("%s is neither native nor delegated", typ)
			}
			if runLibdir != "" && module[typ] {
				// Otherwise the delegated sweep would run the native route again.
				if _, viaHost := nativeProviders(probe, applyOptions{command: "apply", libdir: runLibdir}, ral.HostPlatform); viaHost[typ] == "" {
					t.Fatalf("%s is native with %s: its type file still matches its fingerprint", typ, runLibdir)
				}
			}
			sweepCanaries(t, bin, typ, attrs, realApply || canaryRealTypes[typ], runLibdir, measured)
		})
	}
	if runLibdir == "" {
		return
	}
	for _, typ := range slices.Sorted(maps.Keys(canaryHostTypes)) {
		t.Run(typ, func(t *testing.T) {
			sweepCanaries(t, bin, typ, canaryHostTypes[typ], realApply, runLibdir, measured)
		})
	}
}

// sweepCanaries runs one catalog per attribute of typ, under --noop and
// --noop --debug, and also without --noop when real.
func sweepCanaries(t *testing.T, bin, typ string, attrs []string, real bool, runLibdir, measured string) {
	t.Helper()
	modes := [][]string{{"--noop"}, {"--noop", "--debug"}}
	if real {
		modes = append(modes, nil, []string{"--debug"})
	}
	for _, mode := range modes {
		t.Run(strings.Join(append([]string{"apply"}, mode...), " "), func(t *testing.T) {
			reported := 0
			for _, param := range attrs {
				t.Run(param, func(t *testing.T) {
					if runCanary(t, bin, typ, param, runLibdir, measured, mode) {
						reported++
					}
				})
			}
			// A base every parameter's run refuses sweeps no apply path.
			// Service refuses every resource where PID 1 is not
			// systemd, and voxagent every Resource API type, so only
			// their refusal text is swept.
			if reported == 0 && !(typ == "service" && pid1IsNotSystemd()) && typ != "vox_rapi" {
				t.Errorf("no %s run produced a report: the base resource is refused", typ)
			}
		})
	}
}

// runCanary applies a catalog of one resource: typ's base with param marked
// and set to its canary. It fails for any canary in stdout, stderr or the
// report dir, and returns whether the run wrote a report. One marked resource
// per catalog, because a refused resource refuses the whole catalog.
func runCanary(t *testing.T, bin, typ, param, runLibdir, measured string, mode []string) bool {
	t.Helper()
	dir := t.TempDir()
	title, params := canaryBase(typ, dir)
	var resources []map[string]any
	if typ == "concat_fragment" {
		resources = append(resources, markedResource("Concat_file", params["target"].(string), map[string]any{}))
	}
	canary := "CANARY-" + typ + "-" + param
	p := map[string]any{param: canary}
	for k, v := range params {
		if k != param {
			p[k] = v
		}
	}
	resources = append(resources, markedResource(strings.ToUpper(typ[:1])+typ[1:], title+"-"+param, p, param))
	// As a compiler writes them: the extension host refuses a resource
	// without its kind.
	for _, r := range resources {
		r["kind"], r["exported"] = "compilable_type", false
	}
	cat := writeCatalog(t, dir, resources)
	reports := filepath.Join(dir, "reports")
	args := append([]string{"apply", "--catalog", cat, "--report-dir", reports, "--vardir", filepath.Join(dir, "vardir")}, mode...)
	if runLibdir != "" {
		args = append(args, "--libdir", runLibdir)
	} else {
		args = append(args, "--libdir", measured)
		// A mark can send a native type's resource to the extension host
		// (augeas.Prelude, concat's marked path): only that one may use it.
		raw, err := os.ReadFile(cat)
		if err != nil {
			t.Fatal(err)
		}
		marked, err := catalog.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, delegated := nativeProviders(marked, applyOptions{command: "apply", libdir: measured}, ral.HostPlatform); delegated[typ] == "" {
			args = append(args, "--no-ruby")
		}
	}
	// A relative canary path resolves under dir, never in the test's
	// working directory.
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run: %v", err)
		}
	}
	// Titles and refs are structural and never scrubbed (spec §0 row 2): a
	// namevar canary may stay visible inside a resource ref derived from it,
	// and nowhere else.
	namevar := canaryNamevars[typ] == param
	out, errs := stdout.String(), stderr.String()
	if namevar {
		out, errs = withoutRefs(out, canary), withoutRefs(errs, canary)
	}
	assertNoCanary(t, "stdout", out)
	assertNoCanary(t, "stderr", errs)
	reported := false
	err := filepath.WalkDir(reports, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		reported = true
		text := string(b)
		if namevar {
			text = withoutStructural(t, path, b, canary)
		}
		assertNoCanary(t, path, text)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read reports: %v", err)
	}
	return reported
}

// withoutRefs is text without the resource refs, Type[title] with Puppet's
// type-name spelling, whose title holds canary. Any other bracketed text
// holding it, such as an array literal, stays.
func withoutRefs(text, canary string) string {
	ref := regexp.MustCompile(`[A-Z][A-Za-z0-9_]*(?:::[A-Z][A-Za-z0-9_]*)*\[[^\]\n]*` + regexp.QuoteMeta(canary) + `[^\]\n]*\]`)
	return ref.ReplaceAllString(text, "")
}

// withoutStructural is the report at path re-encoded without the fields that
// may show a namevar canary: every status's key, resource, title and
// containment_path, and every log's source, which are structural (spec §0
// row 2); and the previous, desired and historical values of the events of
// a status whose key holds the canary, a resource generated or retitled
// from it whose values derive from the marked value (spec §6).
func withoutStructural(t *testing.T, path string, b []byte, canary string) string {
	t.Helper()
	var rep map[string]any
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	statuses, _ := rep["resource_statuses"].(map[string]any)
	var kept []any
	for key, v := range statuses {
		st, _ := v.(map[string]any)
		for _, f := range []string{"resource", "title", "containment_path"} {
			delete(st, f)
		}
		if strings.Contains(key, canary) {
			events, _ := st["events"].([]any)
			for _, e := range events {
				ev, _ := e.(map[string]any)
				for _, f := range []string{"previous_value", "desired_value", "historical_value"} {
					delete(ev, f)
				}
			}
		}
		kept = append(kept, st)
	}
	rep["resource_statuses"] = kept
	logs, _ := rep["logs"].([]any)
	for _, l := range logs {
		entry, _ := l.(map[string]any)
		delete(entry, "source")
	}
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return string(out)
}

// assertNoCanary fails for every canary in text, quoting the 80 bytes after it.
func assertNoCanary(t *testing.T, where, text string) {
	t.Helper()
	for i := strings.Index(text, "CANARY-"); i >= 0; i = strings.Index(text, "CANARY-") {
		t.Errorf("%s leaks a canary: %q", where, text[i:min(len(text), i+80)])
		text = text[i+len("CANARY-"):]
	}
}

func TestWithoutRefs(t *testing.T) {
	const canary = "CANARY-vox_deep-name"
	for _, c := range []struct{ text, want string }{
		{"Notice: /Stage[main]/Notify[CANARY-vox_deep-name-1]/message: defined", "Notice: /Stage[main]//message: defined"},
		{"Foo::Bar_baz[x CANARY-vox_deep-name y] ok", " ok"},
		{"defined 'message' as ['CANARY-vox_deep-name']", "defined 'message' as ['CANARY-vox_deep-name']"},
		{"notify[CANARY-vox_deep-name]", "notify[CANARY-vox_deep-name]"},
		{"Notify[other] CANARY-vox_deep-name", "Notify[other] CANARY-vox_deep-name"},
	} {
		if got := withoutRefs(c.text, canary); got != c.want {
			t.Errorf("withoutRefs(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
