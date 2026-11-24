// Package plugintree reads the plugin tree under a libdir.
package plugintree

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxPluginFile bounds a fingerprinted file (spec §0 row 2).
const maxPluginFile = 1 << 20

// LibdirTree is the plugin tree at a libdir path.
type LibdirTree string

func (l LibdirTree) Read(rel string) ([]byte, error) {
	at := filepath.Join(string(l), filepath.FromSlash(rel))
	// O_NONBLOCK: opening a FIFO would otherwise block; the type is checked
	// on the opened file, so nothing can be swapped in after the check.
	f, err := os.OpenFile(at, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", at)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPluginFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPluginFile {
		return nil, fmt.Errorf("%s is over %d bytes", at, maxPluginFile)
	}
	return b, nil
}

func (l LibdirTree) List(rel string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(string(l), filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}
