//go:build augeaslib

// These tests run the binding against openvox's real libaugeas, so they need
// /opt/puppetlabs/puppet: `task dev:augeas-lib` builds them on the host and
// runs them as root inside localhost/augeas-measure-el (spec §10). They fail,
// never skip, when the library is missing.
package augeas

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// openvoxRubyRoot is where openvox 8.33.0 installs ruby-augeas (spec §0 row 1).
const openvoxRubyRoot = "/opt/puppetlabs/puppet/lib/ruby"

func realLib(t *testing.T) Lib {
	t.Helper()
	if _, err := os.Stat(augeasLibPath); err != nil {
		t.Fatalf("libaugeas is required here: %v", err)
	}
	lib, reason, ok := probeAugeas(openvoxRubyRoot)
	if !ok {
		t.Fatalf("probeAugeas: not available: %s", reason)
	}
	return lib
}

// openReal opens a handle as ruby-augeas does for parsed_as? and the version
// check: aug_init(NULL, NULL, AUG_NO_MODL_AUTOLOAD).
func openReal(t *testing.T, lib Lib) Handle {
	t.Helper()
	h, ok := lib.Init(nil, nil, NoModlAutoload)
	if !ok {
		t.Fatal("aug_init returned NULL")
	}
	t.Cleanup(func() { lib.Close(h) })
	return h
}

// loadSysctl loads file through the stock Sysctl lens, as augopen does for
// the first resource of the class (spec §4.2 step 2).
func loadSysctl(t *testing.T, lib Lib, h Handle, file string) {
	t.Helper()
	lens, incl := "Sysctl.lns", file
	if rc := lib.Set(h, "/augeas/load/Sysctl/lens", &lens); rc != 0 {
		t.Fatalf("set lens = %d", rc)
	}
	if rc := lib.Set(h, "/augeas/load/Sysctl/incl[last()+1]", &incl); rc != 0 {
		t.Fatalf("set incl = %d", rc)
	}
	if rc := lib.Load(h); rc != 0 {
		t.Fatalf("aug_load = %d", rc)
	}
}

func TestProbeRealLibrary(t *testing.T) {
	lib, reason, ok := probeAugeas(openvoxRubyRoot)
	if !ok || reason != "" || lib == nil {
		t.Fatalf("probeAugeas = %v, %q, %v; want a library, \"\", true", lib, reason, ok)
	}
	again, reason, ok := probeAugeas(openvoxRubyRoot)
	if !ok || reason != "" || again != lib {
		t.Fatalf("second probeAugeas = %v, %q, %v; want the same library", again, reason, ok)
	}
	h := openReal(t, lib)
	if v, rc := lib.Get(h, "/augeas/version"); rc != 1 || v == nil || *v != "1.15.0" {
		t.Fatalf("/augeas/version = %v, %d", v, rc)
	}
}

// A tampered ruby-augeas refuses the library even though it loads fine.
func TestProbeRealLibraryRefusesChangedRubyAugeas(t *testing.T) {
	realLib(t)
	root := t.TempDir()
	for _, rel := range rubyAugeasFiles {
		b, err := os.ReadFile(filepath.Join(openvoxRubyRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if rel == rubyAugeasFiles[1] {
			b = append(b, '\n')
		}
		at := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, reason, ok := probeAugeas(root)
	if ok || lib != nil || reason == "" {
		t.Fatalf("probeAugeas = %v, %q, %v; want refused", lib, reason, ok)
	}
}

// AP: a set through a variable that matched nothing fails and creates nothing,
// which is why M5's new key stays unwritten.
func TestRealSetOnEmptyVariableFails(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	if rc := lib.Defvar(h, "resource", "/files/tmp/x/vm.swappiness"); rc != 0 {
		t.Fatalf("defvar = %d, want 0 (no match)", rc)
	}
	v := "20"
	if rc := lib.Set(h, "$resource", &v); rc != -1 {
		t.Fatalf("set $resource = %d, want -1", rc)
	}
	if paths, rc := lib.Match(h, "/files/tmp//*"); rc != 0 || len(paths) != 0 {
		t.Fatalf("tree changed: %v, %d", paths, rc)
	}
}

// Mv moves a node to a new path, and fails with -1 for a source that matches
// no node (augeas.h 1.15.0 aug_mv).
func TestRealMvMovesNodeAndFailsOnMissingSource(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	v := "x"
	if rc := lib.Set(h, "/test/a", &v); rc != 0 {
		t.Fatalf("set /test/a = %d", rc)
	}
	if rc := lib.Mv(h, "/test/a", "/test/b"); rc != 0 {
		t.Fatalf("mv /test/a /test/b = %d, want 0", rc)
	}
	if paths, rc := lib.Match(h, "/test/b"); rc != 1 || !slices.Equal(paths, []string{"/test/b"}) {
		t.Fatalf("match /test/b = %v, %d", paths, rc)
	}
	if paths, rc := lib.Match(h, "/test/a"); rc != 0 || len(paths) != 0 {
		t.Fatalf("match /test/a = %v, %d; want the source gone", paths, rc)
	}
	if rc := lib.Mv(h, "/files/nothere", "/files/there"); rc != -1 {
		t.Fatalf("mv of a missing source = %d, want -1", rc)
	}
	if rc := lib.Mv(h, "/test/b", "/test/c\x00d"); rc != -1 {
		t.Fatalf("mv with an embedded NUL = %d, want -1", rc)
	}
}

// AP: an empty file loads as an empty node, so augopen's "didn't load" check
// passes for it.
func TestRealEmptyFileLoadsAsNode(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	file := filepath.Join(t.TempDir(), "sysctl.conf")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	loadSysctl(t, lib, h, file)
	if paths, rc := lib.Match(h, "/files"+file); rc != 1 || !slices.Equal(paths, []string{"/files" + file}) {
		t.Fatalf("match /files%s = %v, %d", file, paths, rc)
	}
	if paths, rc := lib.Match(h, "/files"+file+"/*"); rc != 0 || len(paths) != 0 {
		t.Fatalf("empty file has children: %v, %d", paths, rc)
	}
}

// Match frees the array also when it is empty, and leaves it alone on -1;
// a double free or a free of a dangling pointer aborts the process. The
// runner sets GODEBUG=cgocheck=1.
func TestRealMatchFreesZeroAndMany(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	for _, n := range []string{"a", "b", "c"} {
		v := n
		if rc := lib.Set(h, "/test/"+n, &v); rc != 0 {
			t.Fatalf("set /test/%s = %d", n, rc)
		}
	}
	for range 1000 {
		if paths, rc := lib.Match(h, "/nothing/here"); rc != 0 || paths == nil || len(paths) != 0 {
			t.Fatalf("match none = %#v, %d; want an empty non-nil slice, 0", paths, rc)
		}
		if paths, rc := lib.Match(h, "/test/*"); rc != 3 || !slices.Equal(paths, []string{"/test/a", "/test/b", "/test/c"}) {
			t.Fatalf("match /test/* = %v, %d", paths, rc)
		}
		if paths, rc := lib.Match(h, "/test["); rc != -1 || paths != nil {
			t.Fatalf("match bad path = %v, %d; want nil, -1", paths, rc)
		}
	}
}

// parsedAs is CORE:571-593 over the raw binding (spec §4.3).
func parsedAs(t *testing.T, lib Lib, lens, label string) bool {
	t.Helper()
	h := openReal(t, lib)
	in := label + " FOO\n"
	if rc := lib.Set(h, "/input", &in); rc != 0 {
		t.Fatalf("set /input = %d", rc)
	}
	if lib.TextStore(h, lens, "/input", "/parsed") != 0 {
		return false
	}
	paths, rc := lib.Match(h, "/parsed/"+label+"/1")
	return rc > 0 && len(paths) > 0
}

func TestRealParsedAsCiphersTrueCompressionFalse(t *testing.T) {
	lib := realLib(t)
	for _, lens := range []string{"Ssh.lns", "Sshd.lns"} {
		if !parsedAs(t, lib, lens, "Ciphers") {
			t.Errorf("%s: Ciphers not parsed as an array", lens)
		}
		if parsedAs(t, lib, lens, "Compression") {
			t.Errorf("%s: Compression parsed as an array", lens)
		}
	}
}

func TestRealGetCopiesBorrowedValue(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	first := "one"
	lib.Set(h, "/v", &first)
	got, rc := lib.Get(h, "/v")
	if rc != 1 || got == nil || *got != "one" {
		t.Fatalf("get /v = %v, %d", got, rc)
	}
	second := "two-and-longer"
	lib.Set(h, "/v", &second)
	if *got != "one" {
		t.Fatalf("value from the first get changed to %q", *got)
	}
	lib.Set(h, "/novalue", nil)
	if v, rc := lib.Get(h, "/novalue"); rc != 1 || v != nil {
		t.Fatalf("get valueless node = %v, %d; want nil, 1", v, rc)
	}
	if v, rc := lib.Get(h, "/missing"); rc != 0 || v != nil {
		t.Fatalf("get missing = %v, %d; want nil, 0", v, rc)
	}
}

// A Go string with a NUL cannot become a C string without truncation, which
// would address a different node. The binding refuses the call with the C
// function's failure result and never calls libaugeas.
func TestRealRefusesEmbeddedNUL(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	v := "x"
	nulValue := "x\x00y"
	if rc := lib.Set(h, "/a\x00b", &v); rc != -1 {
		t.Errorf("set NUL path = %d, want -1", rc)
	}
	if rc := lib.Set(h, "/a", &nulValue); rc != -1 {
		t.Errorf("set NUL value = %d, want -1", rc)
	}
	if paths, rc := lib.Match(h, "/a"); rc != 0 || len(paths) != 0 {
		t.Fatalf("a refused set changed the tree: %v, %d", paths, rc)
	}
	lib.Set(h, "/a", &v)
	if got, rc := lib.Get(h, "/a\x00b"); rc != -1 || got != nil {
		t.Errorf("get NUL = %v, %d; want nil, -1", got, rc)
	}
	if paths, rc := lib.Match(h, "/a\x00b"); rc != -1 || paths != nil {
		t.Errorf("match NUL = %v, %d; want nil, -1", paths, rc)
	}
	if rc := lib.Rm(h, "/a\x00b"); rc != -1 {
		t.Errorf("rm NUL = %d, want -1", rc)
	}
	if rc := lib.Insert(h, "/a", "b\x00c", true); rc != -1 {
		t.Errorf("insert NUL = %d, want -1", rc)
	}
	if rc := lib.Defvar(h, "x\x00", "/a"); rc != -1 {
		t.Errorf("defvar NUL = %d, want -1", rc)
	}
	if rc := lib.Defnode(h, "n", "/a\x00b", &v); rc != -1 {
		t.Errorf("defnode NUL = %d, want -1", rc)
	}
	if rc := lib.TextStore(h, "Ssh.lns", "/a", "/p\x00"); rc != -1 {
		t.Errorf("text_store NUL = %d, want -1", rc)
	}
	if paths, rc := lib.Match(h, "/*"); rc < 0 || slices.ContainsFunc(paths, func(p string) bool { return p != "/a" && p != "/augeas" && p != "/files" }) {
		t.Fatalf("a refused call changed the tree: %v, %d", paths, rc)
	}
	if got, rc := lib.Get(h, "/a"); rc != 1 || got == nil || *got != "x" {
		t.Fatalf("/a = %v, %d after refused calls", got, rc)
	}
	nulLoadpath := "/tmp\x00/x"
	if h2, ok := lib.Init(nil, &nulLoadpath, NoModlAutoload); ok || h2 != 0 {
		t.Errorf("init NUL loadpath = %v, %v; want 0, false", h2, ok)
	}
}

// libaugeas writes <file>.augnew and renames it over the file; the new file
// must keep the old one's mode and owner (spec §11 Files).
func TestRealSavePreservesModeAndOwner(t *testing.T) {
	lib := realLib(t)
	h := openReal(t, lib)
	file := filepath.Join(t.TempDir(), "sysctl.conf")
	if err := os.WriteFile(file, []byte("vm.swappiness = 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(file, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	loadSysctl(t, lib, h, file)
	v := "20"
	if rc := lib.Set(h, "/files"+file+"/vm.swappiness", &v); rc != 0 {
		t.Fatalf("set = %d", rc)
	}
	if rc := lib.Save(h); rc != 0 {
		t.Fatalf("aug_save = %d", rc)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "vm.swappiness = 20\n" {
		t.Fatalf("file = %q, %v", b, err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != 0o600 || st.Uid != 65534 || st.Gid != 65534 {
		t.Fatalf("after save: mode %o uid %d gid %d; want 600 65534 65534", fi.Mode().Perm(), st.Uid, st.Gid)
	}
}
