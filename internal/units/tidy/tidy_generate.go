package tidy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyfnmatch"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/units/file"
)

// now is the clock age compares against, read once per Tidy; tests replace it.
var now = time.Now

// tidyRelationships are Puppet::Type.relationship_params, the metaparameters
// add_generated_directed_dependency reads for an existing edge (ARG:172).
var tidyRelationships = []string{"before", "notify", "require", "subscribe"}

// tidyMetaparams are the metaparameters copy_metaparams (type.rb:365-370)
// can find on a Tidy that reaches generation: every other one but alias is a
// pre-flight gap, and loglevel is "notice", which the File is given anyway.
var tidyMetaparams = []string{"before", "notify", "require", "subscribe", "tag"}

// LoadGenerate implements LoadGenerator: Tidy's generate (TY:276-324) as
// additional_resource_generator.rb applies a depth-first generator (ARG:21-54,
// :137-212), against the filesystem as the run finds it (spec §4).
//
// Every read is an lstat or a readdir. Below the Tidy's path they go through
// an os.Root opened on that path without following a link (D-104); the path
// itself is Lstat'd by name, which never follows its final component.
// Messages name absolute paths, as Ruby's do.
func (Tidy) LoadGenerate(c *catalog.Catalog, r catalog.Resource) (ral.LoadGeneration, error) {
	p, err := parseParams(r)
	if err != nil {
		return ral.LoadGeneration{}, err
	}
	// What catalog.resource(:file, path) can find: each File by its title
	// and, as its alias, by its path.
	managed := map[string]struct{}{}
	for _, x := range c.Resources {
		if x.Type == "File" {
			managed[x.Title] = struct{}{}
			managed[file.FilePath(x)] = struct{}{}
		}
	}
	t := &tidyRun{r: r, p: p, managed: managed}
	defer t.close()
	g, err := t.generate(c)
	if err != nil {
		// ARG:24-29: logged against the Tidy; nothing it built is added.
		t.log("err", "Failed to generate additional resources using 'generate': "+err.Error())
		return ral.LoadGeneration{Logs: t.logs, Failed: true, DepthFirst: true}, nil
	}
	return ral.LoadGeneration{Generation: g, Logs: t.logs, DepthFirst: true}, nil
}

// tidyRun is one Tidy's generation. Every error its methods return is one
// Ruby raises out of generate, so it fails the generation, not the run.
type tidyRun struct {
	r    catalog.Resource
	p    params
	root *os.Root // opened on p.Path once the walk reads it
	logs []report.LogEntry
	// managed holds the title and cleaned path of every File in the catalog
	// generation began with, which includes those earlier generators merged.
	managed map[string]struct{}
}

func (t *tidyRun) close() {
	if t.root != nil {
		_ = t.root.Close()
	}
}

// log is a line from the Tidy itself (Puppet::Util::Logging on the resource);
// loadGenerate rewrites Source to its containment path.
func (t *tidyRun) log(level, msg string) {
	t.logs = append(t.logs, report.LogEntry{Level: level, Source: t.r.Ref(), Message: msg, File: t.r.File, Line: t.r.Line})
}

// abs is the absolute path of rel, a path below p.Path or "." for p.Path
// (File.join, TY:291).
func (t *tidyRun) abs(rel string) string {
	if rel == "." {
		return t.p.Path
	}
	return filepath.Join(t.p.Path, rel)
}

func (t *tidyRun) generate(c *catalog.Catalog) (catalog.Generation, error) {
	var g catalog.Generation
	// TY:277.
	top, err := t.stat(".")
	if err != nil || top == nil {
		return g, err
	}
	candidates := []string{"."}
	// TY:279-292: Fileset is built only for a recursing Tidy.
	if t.p.Recurse != nil {
		walked, err := t.fileset(top.IsDir())
		if err != nil {
			return g, err
		}
		candidates = append(candidates, walked...)
	}

	// TY:296, :342-370.
	at := now().Unix()
	var found []string
	for _, rel := range candidates {
		ok, err := t.tidy(rel, at)
		if err != nil {
			return g, err
		}
		if ok {
			found = append(found, t.abs(rel))
		}
	}
	if len(found) == 0 {
		return g, nil
	}
	// TY:297-301.
	for _, path := range found {
		t.log("debug", "Tidying File["+path+"]")
	}
	t.log("notice", fmt.Sprintf("Tidying %d files", len(found)))
	// Ruby's String#<=> is bytewise, as strings.Compare.
	slices.SortFunc(found, func(a, b string) int { return strings.Compare(b, a) })

	tidyRef := t.r.Ref()
	files := make([]catalog.Resource, len(found))
	index := make(map[string]int, len(found))
	for i, path := range found {
		files[i] = t.mkfile(path)
		index[path] = i
	}
	// TY:305-321: each directory requires the generated paths directly in it,
	// visited descending, which found already is.
	if t.p.RmDirs {
		for _, path := range found {
			i, ok := index[filepath.Dir(path)]
			if !ok {
				continue
			}
			req := "File[" + path + "]"
			switch old := files[i].Parameters["require"].(type) {
			case nil:
				files[i].Parameters["require"] = []any{req}
			case []any:
				files[i].Parameters["require"] = append(old, req)
			default:
				files[i].Parameters["require"] = []any{old, req}
			}
		}
	}

	// ARG:48-53, :137-155: catalog.container_of(parent) is the source of its
	// containment edge.
	container := ""
	for _, e := range c.Edges {
		if e.Target == tidyRef {
			container = e.Source
			break
		}
	}
	g.Resources = files
	g.Generators = make(map[string]string, len(files))
	if container != "" {
		g.Containers = make(map[string]string, len(files))
	}
	for _, f := range files {
		ref := f.Ref()
		g.Generators[ref] = tidyRef
		if container != "" {
			g.Containers[ref] = container
		}
		// ARG:160-211: the File before the Tidy unless a relationship already
		// links the two, compared by ref as written.
		linked := slices.ContainsFunc(tidyRelationships, func(rel string) bool {
			return slices.Contains(tidyRefs(t.r.Parameters[rel]), ref) ||
				slices.Contains(tidyRefs(f.Parameters[rel]), tidyRef)
		})
		if !linked {
			g.Before = append(g.Before, catalog.Edge{Source: ref, Target: tidyRef})
		}
	}
	return g, nil
}

// stat is TY:372-381 on rel: nil without an error when the path is missing
// (debug) or cannot be stat'd (warning); any other errno is raised.
func (t *tidyRun) stat(rel string) (os.FileInfo, error) {
	var fi os.FileInfo
	var err error
	if rel == "." {
		fi, err = os.Lstat(t.p.Path)
	} else {
		fi, err = t.root.Lstat(rel)
	}
	switch {
	case err == nil:
		return fi, nil
	case errors.Is(err, syscall.ENOENT):
		t.log("debug", "File does not exist")
		return nil, nil
	case errors.Is(err, syscall.EACCES):
		t.log("warning", "Could not stat; permission denied")
		return nil, nil
	}
	return nil, errors.New(rubyvalue.Errno(err, "rb_file_s_lstat", t.abs(rel)))
}

// fileset is Fileset.new(path, recurse: true, recurselimit:, max_files:).files
// (FS:29-84) less its leading ".": breadth-first from p.Path, each
// directory's entries in readdir order, descending only into what lstat says
// is a directory (links => :manage), to depth p.Recurse. isDir is the Tidy
// path's own lstat.
func (t *tidyRun) fileset(isDir bool) ([]string, error) {
	type entry struct {
		depth int
		rel   string
	}
	var walked []string
	queue := []entry{{0, "."}}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		// FS:168, :186-189.
		if e.depth+1 > *t.p.Recurse {
			continue
		}
		names, err := t.children(e.rel, isDir)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			rel := name
			if e.rel != "." {
				rel = e.rel + "/" + name
			}
			walked = append(walked, rel)
			queue = append(queue, entry{e.depth + 1, rel})
		}
	}

	// FS:68-74. A String max_files is Ruby's `'5' > 0`.
	switch limit := t.p.MaxFiles.(type) {
	case string:
		return nil, errors.New("comparison of String with 0 failed")
	case int64:
		n := int64(len(walked))
		switch {
		case limit > 0 && n > limit:
			//nolint:staticcheck // ST1005: Ruby's text (FS:68-74), which the report must carry verbatim.
			return nil, fmt.Errorf("The directory '%s' contains %d entries, which exceeds the limit of %d specified by the max_files parameter for this resource. The limit may be increased, but be aware that large number of file resources can result in excessive resource consumption and degraded performance. Consider using an alternate method to manage large directory trees", t.p.Path, n, limit)
		case limit == 0 && n > 1000:
			// Puppet.warning: the source is Puppet, not the Tidy.
			t.logs = append(t.logs, report.LogEntry{Level: "warning", Source: "Puppet", Message: fmt.Sprintf("The directory '%s' contains %d entries, which exceeds the default soft limit 1000 and may cause excessive resource consumption and degraded performance. To remove this warning set a value for `max_files` parameter or consider using an alternate method to manage large directory trees", t.p.Path, n)})
		}
	}
	// FS:78: the regexp sub over each path raises on a name Dir.entries
	// tagged UTF-8 but is not.
	for _, rel := range walked {
		if !utf8.ValidString(rel) {
			return nil, errors.New("invalid byte sequence in UTF-8")
		}
	}
	return walked, nil
}

// children is FileSetEntry#children (FS:138-157) on rel: nothing unless lstat
// says directory (ENOENT and EACCES mean no), else Dir.entries less . and ..,
// whose failure is raised. Directories open with opendir's flags, so one
// swapped for a fifo fails instead of blocking; Readdirnames, unlike ReadDir
// in a Root, stats nothing.
func (t *tidyRun) children(rel string, topIsDir bool) ([]string, error) {
	const flags = os.O_RDONLY | syscall.O_DIRECTORY | syscall.O_NONBLOCK
	var d *os.File
	var err error
	if rel == "." {
		if !topIsDir {
			return nil, nil
		}
		// The path is opened without following a link, and the root is made
		// from that handle through /proc, so it is the directory listed. The
		// handle is listed itself: reaching "." through the root would need
		// search permission on the path, which Ruby's opendir does not.
		if d, err = os.OpenFile(t.p.Path, flags|syscall.O_NOFOLLOW, 0); err != nil {
			return nil, errors.New(rubyvalue.Errno(err, "dir_initialize", t.p.Path))
		}
		root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", d.Fd()))
		if err != nil {
			_ = d.Close()
			return nil, fmt.Errorf("open %s through /proc/self/fd (is /proc mounted?): %v", t.p.Path, err)
		}
		t.root = root
	} else {
		fi, err := t.root.Lstat(rel)
		switch {
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.EACCES):
			return nil, nil
		case err != nil:
			return nil, errors.New(rubyvalue.Errno(err, "rb_file_s_lstat", t.abs(rel)))
		case !fi.IsDir():
			return nil, nil
		}
		if d, err = t.root.OpenFile(rel, flags, 0); err != nil {
			return nil, errors.New(rubyvalue.Errno(err, "dir_initialize", t.abs(rel)))
		}
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, errors.New(rubyvalue.Errno(err, "dir_initialize", t.abs(rel)))
	}
	return names, nil
}

// tidy is TY:342-370 on rel, with at the run's now in Unix seconds.
func (t *tidyRun) tidy(rel string, at int64) (bool, error) {
	path := t.abs(rel)
	// catalog.resource(:file, path): the File titled path, or aliased to it
	// by its path. This includes Files an earlier generator added.
	if _, ok := t.managed[path]; ok {
		return false, nil
	}
	fi, err := t.stat(rel)
	if err != nil || fi == nil {
		return false, err
	}
	if fi.IsDir() && !t.p.RmDirs {
		return false, nil
	}
	// TY:112-116: File.fnmatch on the basename, stopping at the first match.
	if t.p.Matches != nil {
		matched := false
		for _, pattern := range t.p.Matches {
			ok, err := rubyfnmatch.Match(pattern, filepath.Base(path))
			if err != nil {
				return false, err
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	if t.p.Age == nil && t.p.Size == nil {
		return true, nil
	}
	// TY:150-153: Time.now.to_i - stat.<type>.to_i >= age.
	if t.p.Age != nil {
		var then int64
		st := fi.Sys().(*syscall.Stat_t)
		switch t.p.TimeField {
		case "atime":
			then = st.Atim.Sec
		case "mtime":
			then = fi.ModTime().Unix()
		case "ctime":
			then = st.Ctim.Sec
		}
		if at-then >= *t.p.Age {
			return true, nil
		}
	}
	// TY:192-194.
	return t.p.Size != nil && fi.Size() >= *t.p.Size, nil
}

// mkfile is TY:248-259 with ARG:139's merge_tags_from: a File removing path,
// with the Tidy's metaparameters, tagged file, then the tag metaparameter's
// tags (Tagging#tag), then the Tidy's. A catalog omits a namevar equal to
// the title, so path is the title alone.
func (t *tidyRun) mkfile(path string) catalog.Resource {
	params := map[string]any{"ensure": "absent", "force": true, "backup": false, "loglevel": "notice"}
	for _, name := range tidyMetaparams {
		switch v := t.r.Parameters[name].(type) {
		case nil:
		case []any:
			params[name] = slices.Clone(v)
		default:
			params[name] = v
		}
	}
	tags := []string{"file"}
	add := func(tag string) {
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	// util/tagging.rb:12-33: downcased, and a qualified tag adds its segments.
	for _, v := range flatten(t.r.Parameters["tag"]) {
		var tag string
		switch s := v.(type) {
		case nil:
			continue
		case string:
			tag = s
		default:
			tag = rubyvalue.InspectValue(s)
		}
		tag = strings.ToLower(tag)
		if slices.Contains(tags, tag) {
			continue
		}
		add(tag)
		if strings.Contains(tag, "::") {
			// String#split drops trailing empty fields.
			segs := strings.Split(tag, "::")
			for len(segs) > 0 && segs[len(segs)-1] == "" {
				segs = segs[:len(segs)-1]
			}
			for _, seg := range segs {
				add(seg)
			}
		}
	}
	for _, tag := range t.r.Tags {
		add(tag)
	}
	return catalog.Resource{Type: "File", Title: path, Tags: tags, Parameters: params}
}

// tidyRefs reads a relationship metaparameter's references: one, or an Array
// of them.
func tidyRefs(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
