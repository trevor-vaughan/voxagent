package ral

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRealPlatformOSRelease(t *testing.T) {
	if _, err := (realPlatform{}).OSRelease(); err != nil {
		t.Skipf("no /etc/os-release: %v", err)
	}
}

// FileExists is Puppet::FileSystem.exist?: os.Stat succeeds, following symlinks.
func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	p := realPlatform{}
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"regular file", file, true},
		{"missing", filepath.Join(dir, "missing"), false},
		{"dangling symlink", dangling, false},
		{"symlink to file", link, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.FileExists(tc.path); got != tc.want {
				t.Errorf("FileExists(%q) = %t, want %t", tc.path, got, tc.want)
			}
		})
	}

	t.Run("unreadable parent", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		if p.FileExists(filepath.Join(locked, "inside")) {
			t.Errorf("FileExists under a mode 0 directory = true, want false")
		}
	})
}
