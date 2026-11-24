package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/voxagent/voxagent/internal/ral"
)

// MatchFiles returns the first release of m whose files for typ are all in
// tree with the digests the manifest measured, and "", true. Otherwise it
// returns the reason the last release tried failed, and false. Every file of
// one release must match: a tree mixing files of two releases matches neither.
func MatchFiles(tree ral.PluginTree, m Manifest, typ string, requires []Manifest) (Release, string, bool) {
	reason := typ + ": no measured release lists it"
	for _, rel := range m.Releases {
		paths, listed := rel.Types[typ]
		if !listed {
			continue
		}
		reason = ""
		for _, p := range paths {
			if reason = checkFile(tree, m, rel, p, requires); reason != "" {
				break
			}
		}
		if reason == "" {
			return rel, "", true
		}
	}
	return Release{}, reason, false
}

// checkFile is "" when the file ref of rel reads from tree with its measured
// digest, else the reason it does not. ref is a path of rel.Files, or
// "<unit>:<path>" resolved in the first required manifest of that name that
// lists the path.
func checkFile(tree ral.PluginTree, m Manifest, rel Release, ref string, requires []Manifest) string {
	path, owner, files := ref, m.Name, rel.Files
	if unit, p, found := strings.Cut(ref, ":"); found {
		path, owner, files = p, unit, nil
		for _, r := range requires {
			if r.Name == unit {
				if files = digestsListing(r, p); files != nil {
					break
				}
			}
		}
	}
	want, ok := digestOf(files, path)
	if !ok {
		return fmt.Sprintf("%s: not listed in the %s manifest", path, owner)
	}
	b, err := tree.Read(path)
	if err != nil {
		return fmt.Sprintf("%s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Sprintf("%s has sha256 %s, not the measured %s", path, got, want)
	}
	return ""
}

// digestsListing is the Files of the first release of r that lists path, or
// nil.
func digestsListing(r Manifest, path string) []FileDigest {
	for _, rel := range r.Releases {
		if _, ok := digestOf(rel.Files, path); ok {
			return rel.Files
		}
	}
	return nil
}

func digestOf(files []FileDigest, path string) (string, bool) {
	for _, f := range files {
		if f.Path == path {
			return f.SHA256, true
		}
	}
	return "", false
}
