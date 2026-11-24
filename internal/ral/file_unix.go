//go:build unix

package ral

import (
	"fmt"
	"os"
	"syscall"
)

// currentOwnership returns the uid and gid of path, or (-1, -1) when it does
// not exist yet -- which makes any declared owner read as a change, the correct
// answer for a file this run is about to create.
func currentOwnership(path string) (int, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return -1, -1, nil
		}
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("stat %s: unexpected stat type %T", path, fi.Sys())
	}
	return int(st.Uid), int(st.Gid), nil
}
