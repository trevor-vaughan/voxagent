package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
)

// validRelPath checks a relative_path from server metadata: valid UTF-8, no
// NUL or backslash, and every "/"-separated segment non-empty and neither "."
// nor "..". A leading or trailing "/" makes an empty segment, so absolute paths
// are refused too. The mount root's "." is not a relative path in this sense;
// callers handle it before calling.
//
// Writes still go through an os.Root on the target directory, which is what
// confines them; this check refuses names Puppet itself would never produce.
func validRelPath(rel string) error {
	if !utf8.ValidString(rel) {
		return fmt.Errorf("relative_path %q is not valid UTF-8", rel)
	}
	if strings.ContainsAny(rel, "\x00\\") {
		return fmt.Errorf("relative_path %q contains NUL or a backslash", rel)
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("relative_path %q has an empty, \".\" or \"..\" segment", rel)
		}
	}
	return nil
}

// ensureDir makes rel inside root a real directory with mode 0755. A symlink
// or other non-directory there is removed, not followed. The parent of rel
// must already be a real directory.
func ensureDir(root *os.Root, rel string) error {
	fi, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case fi.IsDir():
		if fi.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == 0o755 {
			return nil
		}
		return root.Chmod(rel, 0o755)
	default:
		if err := root.Remove(rel); err != nil {
			return err
		}
	}
	if err := root.Mkdir(rel, 0o755); err != nil {
		return err
	}
	// Mkdir's mode is filtered by the umask.
	return root.Chmod(rel, 0o755)
}

// checkTrusted refuses path unless every component from / to it is owned by
// root or the running uid and is not group- or world-writable. path must be
// absolute and exist. A symlink component is checked for its owner (its own
// mode bits mean nothing) and the fully resolved path is then checked the same
// way, so both the link and what it points at are trusted.
func checkTrusted(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("untrusted location %q: not an absolute path", path)
	}
	path = filepath.Clean(path)
	sawLink, err := checkComponents(path)
	if err != nil {
		return err
	}
	if !sawLink {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("untrusted location %q: %w", path, err)
	}
	if _, err := checkComponents(resolved); err != nil {
		return fmt.Errorf("%s (resolved from %s)", err, path)
	}
	return nil
}

// checkComponents checks /, then each prefix of the clean absolute path p,
// without following symlinks. It reports whether any component is a symlink.
func checkComponents(p string) (sawLink bool, err error) {
	uid := uint32(os.Getuid())
	comps := []string{"/"}
	for i := 1; i < len(p); i++ {
		if p[i] == '/' {
			comps = append(comps, p[:i])
		}
	}
	if p != "/" {
		comps = append(comps, p)
	}
	for _, c := range comps {
		fi, err := os.Lstat(c)
		if err != nil {
			return false, fmt.Errorf("untrusted location: %w", err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return false, fmt.Errorf("untrusted location %s: no owner information", c)
		}
		if st.Uid != 0 && st.Uid != uid {
			return false, fmt.Errorf("untrusted location %s: owned by uid %d, not root or %d", c, st.Uid, uid)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			sawLink = true
			continue
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return false, fmt.Errorf("untrusted location %s: group- or world-writable (mode %04o)", c, st.Mode&0o7777)
		}
	}
	return sawLink, nil
}
