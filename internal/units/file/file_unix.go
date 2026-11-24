//go:build unix

package file

import (
	"fmt"
	"os"
	"syscall"
)

// currentOwnership returns the uid and gid in fi, or (-1, -1) when fi is nil
// because the path does not exist yet -- which makes any declared owner read
// as a change, the correct answer for a file this run is about to create.
func currentOwnership(path string, fi os.FileInfo) (int, int, error) {
	if fi == nil {
		return -1, -1, nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: unexpected stat type %T", path, fi.Sys())
	}
	return int(st.Uid), int(st.Gid), nil
}
