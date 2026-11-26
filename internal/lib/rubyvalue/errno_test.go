package rubyvalue

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
	"testing"
)

// TestErrno: wants measured with ruby 3.4.9, e.g.
//
//	ruby -e 'File.read("/nonexistent")'  # No such file or directory @ rb_sysopen - /nonexistent
//	ruby -e 'Dir.mkdir("/proc/x")'       # No such file or directory @ dir_s_mkdir - /proc/x
func TestErrno(t *testing.T) {
	wrapped := &fs.PathError{Op: "open", Path: "/nonexistent", Err: syscall.ENOENT}
	for _, tt := range []struct {
		name     string
		err      error
		fn, path string
		want     string
	}{
		{"with fn", syscall.ENOENT, "rb_sysopen", "/nonexistent", "No such file or directory @ rb_sysopen - /nonexistent"},
		{"wrapped errno", wrapped, "dir_s_mkdir", "/proc/x", "No such file or directory @ dir_s_mkdir - /proc/x"},
		{"no fn", syscall.EACCES, "", "/ignored", "Permission denied"},
		{"not an errno", errors.New("plain failure"), "rb_sysopen", "/x", "plain failure"},
		{"wrapped non-errno", fmt.Errorf("ctx: %w", errors.New("inner")), "rb_sysopen", "/x", "ctx: inner"},
	} {
		if got := Errno(tt.err, tt.fn, tt.path); got != tt.want {
			t.Errorf("%s: Errno = %q; want %q", tt.name, got, tt.want)
		}
	}
}
