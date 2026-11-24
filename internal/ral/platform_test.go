package ral

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
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

func TestOnPlatform(t *testing.T) {
	rhel10 := []OS{{ID: "rhel", Like: true, MinMajor: 10}}
	const rhelWith = "ID=\"rhel\"\n"
	for _, tc := range []struct {
		name    string
		release string
		err     error
		list    []OS
		want    bool
	}{
		{"RHEL 10.0", rhelWith + "VERSION_ID=\"10.0\"\n", nil, rhel10, true},
		{"RHEL 10.1", rhelWith + "VERSION_ID=\"10.1\"\n", nil, rhel10, true},
		{"RHEL 9.6", rhelWith + "VERSION_ID=\"9.6\"\n", nil, rhel10, false},
		{"Rocky 10", "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"10.0\"\n", nil, rhel10, true},
		{"CentOS Stream 10", raltest.CentOS10, nil, rhel10, true},
		{"AlmaLinux 10", "ID=\"almalinux\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"10.0\"\n", nil, rhel10, true},
		{"Fedora 41", "ID=fedora\nVERSION_ID=41\n", nil, rhel10, false},
		{"Debian 12", "ID=debian\nVERSION_ID=\"12\"\n", nil, rhel10, false},
		{"Ubuntu 24.04", "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", nil, rhel10, false},
		{"empty file", "", nil, rhel10, false},
		{"unreadable", rhelWith + "VERSION_ID=\"10\"\n", errors.New("denied"), rhel10, false},
		{"VERSION_ID missing", "ID=rhel\n", nil, rhel10, false},
		{"VERSION_ID 10", "ID=rhel\nVERSION_ID=\"10\"\n", nil, rhel10, true},
		{"VERSION_ID 10.2", "ID=rhel\nVERSION_ID=\"10.2\"\n", nil, rhel10, true},
		{"VERSION_ID x", "ID=rhel\nVERSION_ID=\"x\"\n", nil, rhel10, false},
		{"VERSION_ID +10", "ID=rhel\nVERSION_ID=\"+10\"\n", nil, rhel10, false},
		{"VERSION_ID leading space", "ID=rhel\nVERSION_ID=\" 10\"\n", nil, rhel10, false},
		{"VERSION_ID overflow", "ID=rhel\nVERSION_ID=\"99999999999\"\n", nil, rhel10, false},
		{"unquoted", "ID=rhel\nVERSION_ID=10\n", nil, rhel10, true},
		{"single-quoted", "ID='rhel'\nVERSION_ID='10'\n", nil, rhel10, true},
		{"any platform, unreadable", "", errors.New("denied"), []OS{{ID: "*"}}, true},
		{"Like false refuses derivative", "ID=rocky\nID_LIKE=\"rhel\"\nVERSION_ID=10\n", nil, []OS{{ID: "rhel", MinMajor: 10}}, false},
		{"Like false refuses Ubuntu", "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n", nil, []OS{{ID: "debian", MinMajor: 12}}, false},
		{"Like false accepts Debian", "ID=debian\nVERSION_ID=\"12\"\n", nil, []OS{{ID: "debian", MinMajor: 12}}, true},
		{"MinMajor 0 without VERSION_ID", "ID=rhel\n", nil, []OS{{ID: "rhel", Like: true}}, true},
		{"empty list", "ID=rhel\nVERSION_ID=10\n", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := raltest.FakePlatform{Release: tc.release, Err: tc.err}
			if got := OnPlatform(p, tc.list); got != tc.want {
				t.Errorf("OnPlatform = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestReadOSReleaseFallback(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first, second := write("first", "one"), write("second", "two")
	missing := filepath.Join(dir, "missing")

	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"first wins", []string{first, second}, "one"},
		{"first missing", []string{missing, second}, "two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readOSRelease(tc.paths...)
			if err != nil || string(got) != tc.want {
				t.Errorf("readOSRelease = %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Run("dangling symlink", func(t *testing.T) {
		dangling := filepath.Join(dir, "dangling")
		if err := os.Symlink(missing, dangling); err != nil {
			t.Fatal(err)
		}
		got, err := readOSRelease(dangling, second)
		if err != nil || string(got) != "two" {
			t.Errorf("readOSRelease = %q, %v; want %q", got, err, "two")
		}
	})

	t.Run("both missing", func(t *testing.T) {
		_, err := readOSRelease(missing, filepath.Join(dir, "missing2"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("unreadable first does not fall through", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file permissions")
		}
		locked := write("locked", "secret")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		got, err := readOSRelease(locked, second)
		if err == nil {
			t.Errorf("readOSRelease = %q, nil; want an error", got)
		}
	})
}
