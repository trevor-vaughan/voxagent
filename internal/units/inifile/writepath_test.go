package inifile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// openDefault opens path with the ini_setting provider's default syntax and
// the encoding of an unset LANG.
func openDefault(t *testing.T, path string) *IniFile {
	t.Helper()
	ini, err := openIniFile(path, " = ", "[", "]", " ", nil, "US-ASCII")
	if err != nil {
		t.Fatal(err)
	}
	return ini
}

func TestSaveNewFileMode(t *testing.T) {
	// Not parallel: the umask is process-wide.
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "new.ini")
	ini := openDefault(t, path)
	x := "x"
	if err := ini.SetValue("a", &x, " = ", []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if err := ini.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o666&^0o027); got != want {
		t.Errorf("mode %v, want 0666 &^ umask = %v", got, want)
	}
}

func TestSaveKeepsExistingFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.ini")
	if err := os.WriteFile(path, []byte("[a]\nx = 1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := openDefault(t, path).Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", info.Mode().Perm())
	}
}

func TestSaveFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.ini")
	link := filepath.Join(dir, "link.ini")
	if err := os.WriteFile(target, []byte("[a]\nx = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	ini := openDefault(t, link)
	x := "x"
	if err := ini.SetValue("a", &x, " = ", []string{"2"}); err != nil {
		t.Fatal(err)
	}
	if err := ini.Save(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v, %v", info, err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "[a]\nx = 2\n" {
		t.Errorf("target = %q, %v; want it rewritten", got, err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the target's inode changed: the write must truncate in place")
	}
}

func TestSaveMissingParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodir", "f.ini")
	err := openDefault(t, path).Save()
	if err == nil {
		t.Fatal("no error")
	}
	if want := "No such file or directory @ rb_sysopen - " + path; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if _, statErr := os.Stat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the parent directory was created")
	}
}

func TestSaveUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through the missing permission")
	}
	path := filepath.Join(t.TempDir(), "ro.ini")
	if err := os.WriteFile(path, []byte("[a]\nx = 1\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	err := openDefault(t, path).Save()
	if err == nil {
		t.Fatal("no error")
	}
	if want := "Permission denied @ rb_sysopen - " + path; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if got, _ := os.ReadFile(path); string(got) != "[a]\nx = 1\n" {
		t.Errorf("file = %q, want it unchanged", got)
	}
}

func TestReadUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through the missing permission")
	}
	path := filepath.Join(t.TempDir(), "secret.ini")
	if err := os.WriteFile(path, []byte("[a]\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	_, err := openIniFile(path, " = ", "[", "]", " ", nil, "US-ASCII")
	if err == nil || err.Error() != "Permission denied @ rb_sysopen - "+path {
		t.Errorf("error %v, want Permission denied @ rb_sysopen", err)
	}
}

func TestReadDirectoryIsEmpty(t *testing.T) {
	// File.file? is false for a directory: no lines, and nothing to read.
	dir := t.TempDir()
	ini := openDefault(t, dir)
	if got := ini.SectionNames(); len(got) != 1 || got[0] != "" {
		t.Errorf("section names %q, want only the global section", got)
	}
	if err := ini.Save(); err == nil || !strings.HasPrefix(err.Error(), "Is a directory @ rb_sysopen - ") {
		t.Errorf("Save on a directory: %v, want Is a directory @ rb_sysopen", err)
	}
}

func TestOpenInvalidEncodingNamesTheEncoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.ini")
	if err := os.WriteFile(path, []byte("[a]\nx = \xc3\xa9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for enc, wantErr := range map[string]bool{"US-ASCII": true, "UTF-8": false} {
		_, err := openIniFile(path, " = ", "[", "]", " ", nil, enc)
		if (err != nil) != wantErr {
			t.Errorf("%s: err = %v, want error %v", enc, err, wantErr)
		}
		var re *RubyError
		if wantErr && (!errors.As(err, &re) || re.Class != "ArgumentError" || re.Message != "invalid byte sequence in US-ASCII") {
			t.Errorf("%s: err = %v, want ArgumentError: invalid byte sequence in US-ASCII", enc, err)
		}
	}
}
