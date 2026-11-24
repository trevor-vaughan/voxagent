package ral

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
)

// softMaxFiles is Fileset's soft limit (file_serving/fileset.rb:64): a walk
// with more entries than this warns, and still returns every entry.
const softMaxFiles = 1000

// childExcluded are the parameters newchild never passes to a child
// (type/file.rb:595-598). Only ensure, recurse and source can be present --
// the rest are pre-flight refusals -- but the list is Puppet's whole rule.
var childExcluded = map[string]struct{}{
	"parent": {}, "ensure": {}, "recurse": {}, "recurselimit": {},
	"max_files": {}, "target": {}, "alias": {}, "source": {},
}

// Generate implements Generator: Puppet's eval_generate for file
// (type/file.rb:533-537), which is recurse (:631-646).
//
// Children come from a breadth-first walk of the directory itself and, when
// the source resolves to a directory, of that directory too; a child the
// source has gets "<source dir>/<relative path>" as its own source
// (recurse_remote, :712-726) -- for a downloaded puppet: source,
// "<source URI>/<relative path>" (:750), as Puppet names it. With purge, every child without a source is
// marked ensure => absent (mark_children_for_purging, :573-579). A child at
// or below another catalog File is that File's to manage and is dropped
// (remove_less_specific_files, :648-673). Each child inherits the parent's
// parameters except the ones newchild withholds (:583-603), so one that the
// source does not describe and purge does not remove has no ensure and
// manages only mode, owner and group of what is there.
//
// A source that is missing, a symlink or not a directory contributes no
// children: the parent's own Apply then reports why, as Puppet's does.
//
// A prefetched provider also looks up the children's SELinux defaults here,
// in batches, rather than one process per child in Apply
// (prefetchChildDefaults).
func (f File) Generate(ctx context.Context, r catalog.Resource, managed func(string) bool) ([]catalog.Resource, []Warning, error) {
	if paramString(r, "recurse", "false") != "true" {
		return nil, nil, nil
	}
	parent := FilePath(r)
	local, warnings, err := walkTree(parent, false)
	if err != nil {
		return nil, nil, err
	}
	sources := map[string]string{}
	if raw, ok := r.Parameters["source"]; ok {
		// chooseSource's own error is the parent's to report, from Apply.
		// follow is false because Validate refuses links => follow with
		// recurse, so a recursed source is always read with lstat.
		if spec, fi, found, err := chooseSource(osFS{}, sourceSpecs(raw, f.Sources), false); err == nil && found && fi.IsDir() {
			remote, remoteWarnings, err := walkTree(spec.path, sourceMarked(r))
			if err != nil {
				return nil, nil, err
			}
			warnings = append(warnings, remoteWarnings...)
			for _, rel := range remote {
				if spec.mapped {
					sources[rel] = spec.shown + "/" + filepath.ToSlash(rel)
				} else {
					sources[rel] = filepath.Join(spec.path, rel)
				}
			}
		}
	}

	all := make(map[string]struct{}, len(local)+len(sources))
	for _, rel := range local {
		all[rel] = struct{}{}
	}
	for rel := range sources {
		all[rel] = struct{}{}
	}
	purge := paramString(r, "purge", "false") == "true"
	from := r
	children := make([]catalog.Resource, 0, len(all))
	for rel := range all {
		path := filepath.Join(parent, rel)
		if belowManaged(path, parent, r.Type, managed) {
			continue
		}
		params := make(map[string]any, len(r.Parameters)+1)
		for k, v := range r.Parameters {
			// .compact drops nil values as well (:592).
			if _, withheld := childExcluded[k]; withheld || v == nil {
				continue
			}
			params[k] = v
		}
		if src, ok := sources[rel]; ok {
			params["source"] = src
		} else if purge {
			params["ensure"] = "absent"
		}
		children = append(children, catalog.Resource{
			Type: r.Type, Title: path, Tags: slices.Clone(r.Tags), Kind: r.Kind,
			Parameters: params, GeneratedFrom: &from,
		})
	}
	// recurse sorts by path (:643): Ruby's String#<=> is bytewise, as Go's
	// string comparison is.
	sort.Slice(children, func(i, j int) bool { return children[i].Title < children[j].Title })
	if f.defaults != nil {
		prefetchChildDefaults(ctx, f.selinux(), parent, children, f.defaults.answers)
	}
	return children, warnings, nil
}

// beforeWalkOpen, when set by a test, runs before walkTree opens a directory
// to read it: the window in which a user can swap the directory for a fifo.
var beforeWalkOpen func(path string)

// walkTree lists every entry below dir, breadth first, as paths relative to
// dir: Fileset#perform_recursion (file_serving/fileset.rb:162-177) with
// links => manage, the default.
//
// A dir that is missing, cannot be stat'd, is a symlink or is not a directory
// has no entries: direct_file_server.rb:10,16 answers nil before any Fileset
// exists, and Fileset's directory? is lstat-based (:153-158). Every entry is
// listed, dotfiles included. An entry is descended only when it Lstats as a
// directory, so a symlink is an entry and never a way in; one whose Lstat
// fails with ENOENT or EACCES is a leaf, the two errors directory? rescues. A
// directory that cannot be read fails the walk, as Dir.entries raises there
// (FileSetEntry#children).
//
// The walk runs on an os.Root opened on dir (openDirNoFollow), so no entry
// can lead outside it.
//
// redact is set for a marked source's directory, which no error or warning
// may name (D-78: Ruby leaks it).
func walkTree(dir string, redact bool) ([]string, []Warning, error) {
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil, nil, nil
	}
	root, err := openDirNoFollow(dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()

	// fail is "<op> <path>: <err>", with dir taken out of it for a marked source.
	fail := func(op, path string, err error) error {
		if redact {
			return fmt.Errorf("%s [redacted]: %s", op, strings.ReplaceAll(err.Error(), dir, "[redacted]"))
		}
		return fmt.Errorf("%s %s: %w", op, path, err)
	}

	var rels []string
	queue := []string{"."}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		if beforeWalkOpen != nil {
			beforeWalkOpen(filepath.Join(dir, at))
		}
		// O_NONBLOCK|O_DIRECTORY: a fifo swapped in after the Lstat that
		// queued this directory fails with ENOTDIR instead of blocking.
		f, err := root.OpenFile(at, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
		if err != nil {
			return nil, nil, fail("read directory", filepath.Join(dir, at), err)
		}
		f, err = withName(f, filepath.Join(dir, at))
		if err != nil {
			return nil, nil, fail("read directory", filepath.Join(dir, at), err)
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err != nil {
			return nil, nil, fail("read directory", filepath.Join(dir, at), err)
		}
		for _, e := range entries {
			rel := filepath.Join(at, e.Name())
			rels = append(rels, rel)
			fi, err := root.Lstat(rel)
			switch {
			case errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission):
				continue
			case err != nil:
				return nil, nil, fail("lstat", filepath.Join(dir, rel), err)
			case fi.IsDir():
				queue = append(queue, rel)
			}
		}
	}

	var warnings []Warning
	if len(rels) > softMaxFiles {
		// fileset.rb:72-73, verbatim: Puppet.warning, so sourced "Puppet",
		// once per walk; entries does not count the walk's own ".".
		warnings = append(warnings, Warning{Source: "Puppet", Message: fmt.Sprintf(
			"The directory %s contains %d entries, which exceeds the default soft limit %d and may cause excessive resource consumption and degraded performance. To remove this warning set a value for `max_files` parameter or consider using an alternate method to manage large directory trees",
			redactIf(redact, "'%s'", dir), len(rels), softMaxFiles)})
	}
	return rels, warnings, nil
}

// belowManaged reports whether path is at or below a catalog File other than
// the recursed one, which remove_less_specific_files (type/file.rb:648-673)
// leaves to that File. Ascending from path to parent compares whole
// components, so /d/ab is not below a managed /d/a.
func belowManaged(path, parent, typ string, managed func(string) bool) bool {
	for p := path; p != parent && p != filepath.Dir(p); p = filepath.Dir(p) {
		if managed(typ + "[" + p + "]") {
			return true
		}
	}
	return false
}

// childSourceFS opens the filesystem a generated child's source is read
// through: an os.Root on the source directory the parent's walk listed, so a
// directory in that tree swapped for a symlink after the walk cannot make the
// child copy a file from outside the tree. Puppet reads the child's source by
// path and would follow such a link; refusing it is the safe failure, and
// only a race can reach it, because the walk never descends a link.
//
// The source directory is not re-resolved from the parent, whose first
// existing candidate could have changed since the walk: Generate gave the
// child "<source dir>/<rel>" as its source and "<parent>/<rel>" as its title,
// so it is the source with the title's rel trimmed off.
//
// A child of a downloaded puppet: source has "<source URI>/<rel>" as its
// source. Its directory is the download the parent's source value maps to in
// sources, matched by that text, not by existence, so it is no
// re-resolution either; the returned map takes the child's URI to its file in
// that download, for resolveSource. It is nil for any other child.
//
// A walked source entry that is itself a symlink fails the child here, with
// resolveSource's D-26 message. Through the os.Root, chooseSource's existence
// check cannot follow an absolute link, so it would otherwise report such an
// entry as missing.
func childSourceFS(r catalog.Resource, sources map[string]string) (rootFS, map[string]string, error) {
	src, _ := r.Parameters["source"].(string)
	rel, err := filepath.Rel(FilePath(*r.GeneratedFrom), FilePath(r))
	var dir, path string
	var found bool
	var childSources map[string]string
	if err == nil {
		dir, found = mappedSourceDir(r.GeneratedFrom.Parameters["source"], src, rel, sources)
	}
	if found {
		path = filepath.Join(dir, rel)
		childSources = map[string]string{src: path}
	} else {
		path = filepath.Clean(src)
		dir, found = strings.CutSuffix(path, string(filepath.Separator)+rel)
	}
	// A marked parent's source is shown as [redacted] (D-78: Ruby leaks it).
	redact := sourceMarked(r)
	if err != nil || !found || !filepath.IsAbs(dir) {
		return rootFS{}, nil, fmt.Errorf("%s: source %s does not name %s inside a source directory",
			r.Ref(), redactIf(redact, "%v", r.Parameters["source"]), rel)
	}
	root, err := openDirNoFollow(dir)
	if err != nil {
		return rootFS{}, nil, err
	}
	rfs := rootFS{root: root, dir: dir}
	if fi, err := rfs.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = root.Close()
		shown := sourceSpecs(path, nil)[0].shown
		if childSources != nil {
			shown = src
		}
		return rootFS{}, nil, fmt.Errorf("source %s is a symlink; ensure => link is not implemented", redactIf(redact, "%s", shown))
	}
	return rfs, childSources, nil
}

// mappedSourceDir returns the download that the element of a parent's source
// value parentRaw naming src's directory maps to in sources: the element is
// mapped, and src is it, normalised as sourceSpecs does, joined with rel.
func mappedSourceDir(parentRaw any, src, rel string, sources map[string]string) (string, bool) {
	values, ok := parentRaw.([]any)
	if !ok {
		values = []any{parentRaw}
	}
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if local, ok := sources[s]; ok && strings.TrimRight(s, "/")+"/"+filepath.ToSlash(rel) == src {
			return local, true
		}
	}
	return "", false
}
