package mountcore

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// The type's relations (spec 6.4 AutoRequires and AutoRelations): mounts
// higher up first (mt:299-305), and the mount before the catalog's Files under
// it (mt:308-316).

// SelfRefreshes implements ral.SelfRefreshing: the type is declared with
// self_refresh: true (mt:4).
func (Mount) SelfRefreshes(catalog.Resource) bool { return true }

// AutoRequires implements Provider (mt:299-305): Mount[<p>] for each proper
// ancestor of the munged name, root first, when the catalog manages it.
// Validation refused an unparseable resource before the graph is built, so a
// parse error here means no edge.
func (m Mount) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseMount(r, m.release, m.encoding)
	if err != nil {
		return nil
	}
	var refs []string
	for _, parent := range ancestors(p.Name) {
		if ref := "Mount[" + parent + "]"; managed(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// AutoRelations implements ral.AutoRelating (mt:308-316): File[<path>] before
// which the mount applies, for every catalog File (m.files, in catalog order)
// whose expanded path matches (?m)^<name>/. with the name interpolated raw.
// Ruby builds the pattern only inside its loop over the Files, so it is
// compiled only when there is one; Route then delegates a name RE2 cannot
// compile (spec 2.1 condition 7), which makes the compile error unreachable
// here, and a parse error means no edge, as in AutoRequires.
func (m Mount) AutoRelations(r catalog.Resource) []ral.AutoRelation {
	if len(m.files) == 0 {
		return nil
	}
	p, err := parseMount(r, m.release, m.encoding)
	if err != nil {
		return nil
	}
	children, err := regexp.Compile(`(?m)^` + p.Name + `/.`)
	if err != nil {
		return nil
	}
	var rels []ral.AutoRelation
	for _, path := range m.files {
		// File.expand_path: Abs fails only on a relative path whose working
		// directory is gone, and File refuses a relative path.
		expanded, err := filepath.Abs(path)
		if err != nil {
			continue
		}
		if children.MatchString(expanded) {
			rels = append(rels, ral.AutoRelation{Kind: ral.RelBefore, Ref: "File[" + path + "]"})
		}
	}
	return rels
}

// ancestors is Pathname#ascend's paths past the first, root first: the
// path's proper ancestors as Ruby's pathname.rb derives them, by text, so
// "/a/./b" has "/a/." and "a/b" has "a".
func ancestors(path string) []string {
	var out []string
	for {
		pre, _, ok := chopBasename(path)
		if !ok || pre == "" {
			return out
		}
		path = pre
		out = append([]string{delTrailingSeparator(path)}, out...)
	}
}

// chopBasename is pathname.rb's chop_basename: the path before its last
// component, and the component; false when the basename is empty or "/".
func chopBasename(path string) (pre, base string, ok bool) {
	base = rubyBasename(path)
	if base == "" || base == "/" {
		return "", "", false
	}
	return path[:strings.LastIndex(path, base)], base, true
}

// delTrailingSeparator is pathname.rb's del_trailing_separator: the path
// without its trailing slashes, "/" for a path of slashes only.
func delTrailingSeparator(path string) string {
	if pre, base, ok := chopBasename(path); ok {
		return pre + base
	}
	if strings.HasSuffix(path, "/") {
		return "/"
	}
	return path
}

// rubyBasename is File.basename on Linux: the last component after the
// trailing slashes are dropped, "/" for slashes only, "" for "".
func rubyBasename(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		if path == "" {
			return ""
		}
		return "/"
	}
	return trimmed[strings.LastIndex(trimmed, "/")+1:]
}
