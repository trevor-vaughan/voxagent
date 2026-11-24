package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidRelPathRejects(t *testing.T) {
	for _, rel := range []string{
		"/etc/x", "../x", "a/../../x", "a/./b", "", ".", "..", "a//b", "a/", "a\\b", "a\x00b", "\xff.rb",
	} {
		if err := validRelPath(rel); err == nil {
			t.Errorf("validRelPath(%q) = nil, want an error", rel)
		}
	}
	for _, rel := range []string{"a", "puppet/type/vox_marker.rb", "..a", "a..", ".hidden", "ünïcode.rb"} {
		if err := validRelPath(rel); err != nil {
			t.Errorf("validRelPath(%q) = %v, want nil", rel, err)
		}
	}
}

// A symlink at a path that must be a directory is removed, even when it
// points somewhere real, and the directory is made in its place.
func TestEnsureDirReplacesParentSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/tmp", filepath.Join(dir, "p")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := ensureDir(root, "p"); err != nil {
		t.Fatal(err)
	}
	assertDir(t, filepath.Join(dir, "p"))
}

// trustedScratch returns a fresh directory whose every ancestor passes
// checkTrusted, so a test can show that the planted component, not /tmp, is
// what fails. The user cache dir holds the Go build cache, so it exists
// wherever tests run.
func trustedScratch(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache dir: %v", err)
	}
	if err := checkTrusted(cache); err != nil {
		t.Skipf("no trusted scratch location: %v", err)
	}
	d, err := os.MkdirTemp(cache, "voxagent-trust-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCheckTrustedAcceptsTrustedTree(t *testing.T) {
	base := trustedScratch(t)
	lib := filepath.Join(base, "cache", "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkTrusted(lib); err != nil {
		t.Errorf("checkTrusted(%s) = %v, want nil", lib, err)
	}
	if err := checkTrusted("/"); err != nil {
		t.Errorf("checkTrusted(/) = %v, want nil", err)
	}
}

func TestCheckTrustedRefusesWritableParent(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o775, 0o1777} {
		t.Run(mode.String(), func(t *testing.T) {
			base := trustedScratch(t)
			parent := filepath.Join(base, "cache")
			lib := filepath.Join(parent, "lib")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, mode); err != nil {
				t.Fatal(err)
			}
			err := checkTrusted(lib)
			if err == nil || !strings.Contains(err.Error(), parent+": group- or world-writable") {
				t.Errorf("checkTrusted = %v, want %s refused as writable", err, parent)
			}
		})
	}
}

// A symlink in the path is checked through to its target.
func TestCheckTrustedFollowsSymlinkToUntrusted(t *testing.T) {
	base := trustedScratch(t)
	bad := filepath.Join(base, "bad")
	if err := os.MkdirAll(filepath.Join(bad, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(bad, link); err != nil {
		t.Fatal(err)
	}
	err := checkTrusted(filepath.Join(link, "lib"))
	if err == nil || !strings.Contains(err.Error(), bad+": group- or world-writable") {
		t.Errorf("checkTrusted via symlink = %v, want %s refused", err, bad)
	}
	good := filepath.Join(base, "good")
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(good, filepath.Join(base, "goodlink")); err != nil {
		t.Fatal(err)
	}
	if err := checkTrusted(filepath.Join(base, "goodlink")); err != nil {
		t.Errorf("checkTrusted via symlink to trusted dir = %v, want nil", err)
	}
}

func TestCheckTrustedRefusesWorldWritableTmp(t *testing.T) {
	d := filepath.Join(t.TempDir(), "ww")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := checkTrusted(d); err == nil {
		t.Errorf("checkTrusted(%s) = nil, want an error", d)
	}
}

func TestCheckTrustedRefusesRelativeAndMissing(t *testing.T) {
	if err := checkTrusted("lib"); err == nil {
		t.Error("relative path accepted")
	}
	if err := checkTrusted(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing path accepted")
	}
}
