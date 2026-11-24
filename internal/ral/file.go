package ral

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// File is the Tier 1 file provider, bounded to ensure (link and a link target
// included), content, mode, owner, group, source (a local path or file: URI),
// recurse, purge, path, replace, force, links, show_diff, target, the SELinux
// contexts (seluser, selrole, seltype, selrange), selinux_ignore_defaults and
// backup (a suffix, or the default bucket; file_backup.go).
//
// Everything else in Puppet's file type -- checksum among it -- is
// absent, and absent means a pre-flight refusal rather than a
// silent omission. Puppet's file type is 2,691 lines and most of that
// difficulty is exactly those attributes, so an unbounded file provider would
// be the whole spine.
//
// A symlink at the path is handled as Puppet's links parameter says
// (type/file.rb:895-903). Under manage, the default, the path is lstat'ed: a
// link is replaced by ensure => file or directory, satisfies present, never
// has its mode managed and is lchown'ed. Under follow it is stat'ed, so the
// link's target decides the type and mode, and chmod and chown go through the
// link. Nothing else ever does: a content write replaces the link by renaming
// a temp file over it, and a removal unlinks it.
type File struct {
	// Environment is the catalog's environment, which Puppet names in a
	// missing source's message (type/file/source.rb:208).
	Environment string

	// Sources maps a puppet: source URI exactly as written in the catalog to
	// the local path its content was downloaded to. nil in `apply`, where
	// every puppet: source is a pre-flight gap.
	Sources map[string]string

	// Vardir is Puppet's vardir: the default filebucket backup => 'puppet'
	// names is <Vardir>/clientbucket (Puppet[:clientbucketdir]).
	Vardir string

	// se is the host's SELinux; nil means defaultSELinux.
	se selinuxOps

	// defaults holds the SELinux defaults looked up ahead, in batches, for
	// the run this provider was prefetched for (prefetchChildDefaults); nil
	// for a provider that was not prefetched.
	defaults *runDefaults
}

// runDefaults is one run's SELinux defaults, looked up ahead.
type runDefaults struct {
	answers map[defaultQuery]defaultAnswer
}

// defaultSELinux is the SELinux of a File built without its own se: the
// host's. The package's tests replace it (TestMain), so none reaches the
// host's SELinux unless it asks to.
var defaultSELinux selinuxOps = hostSELinux{run: execRunner{}, handle: &handleProbe{}}

// selinux is f.se, or defaultSELinux when the provider was built without one.
// A prefetched provider answers from its batched defaults first.
func (f File) selinux() selinuxOps {
	se := f.se
	if se == nil {
		se = defaultSELinux
	}
	if f.defaults != nil {
		return prefetchedSELinux{selinuxOps: se, defaults: f.defaults.answers}
	}
	return se
}

// Type implements Provider.
func (File) Type() string { return "file" }

// Measured from a live report: file resolves to the posix provider on Linux.
func (File) ProviderUsed() *string { return &posix }

// AutoRequires implements Provider.
//
// Measured against Puppet 8.10.0, type/file.rb autorequire(:file): ascend from
// the path's PARENT and take the first ancestor the catalog manages --
// `parents.find { |p| catalog.resource(:file, p.to_s) }`. One edge, not one per
// ancestor: transitively the ordering is identical, and the extra edges would
// only make the graph harder to read.
//
// Starting at the parent is what stops a file autorequiring itself. Puppet's
// `unless path.root?` guard is the loop bound here.
//
// A link also requires its target when the catalog manages it
// (type/file.rb:398-399), so the target is created first. Only an absolute
// target can name a managed file; it is cleaned as FilePath cleans a path, so
// the reference matches the File[<path>] alias build.go resolves.
func (File) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	var reqs []string
	if ref, ok := managedAncestor(FilePath(r), managed); ok {
		reqs = append(reqs, ref)
	}
	if t, ok := linkTarget(r); ok && filepath.IsAbs(t) {
		// A link to its own parent directory is one edge, not two.
		if ref := "File[" + filepath.Clean(t) + "]"; managed(ref) && !slices.Contains(reqs, ref) {
			reqs = append(reqs, ref)
		}
	}
	// type/file.rb:403-418: the owner and group, autorequired as User and Group
	// when given by name. shouldorig[0] is the first value, and an Integer or an
	// all-digits string is an id, which names no resource.
	for _, rel := range [...]struct{ param, typ string }{{"owner", "User"}, {"group", "Group"}} {
		v := r.Parameters[rel.param]
		if list, ok := v.([]any); ok && len(list) > 0 {
			v = list[0]
		}
		name, ok := v.(string)
		if !ok || name == "" || isAllDigits(name) {
			continue
		}
		if ref := rel.typ + "[" + name + "]"; managed(ref) {
			reqs = append(reqs, ref)
		}
	}
	return reqs
}

// isAllDigits is Ruby's /^\d+$/ for a single-line value.
func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// managedAncestor is the first ancestor of path, from its parent up, that the
// catalog manages.
func managedAncestor(path string, managed func(string) bool) (string, bool) {
	// Puppet's `unless path.root?`. filepath.Dir("/") is "/", so without this
	// the root directory would find itself managed and autorequire itself.
	if filepath.Dir(path) == path {
		return "", false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		ref := "File[" + dir + "]"
		if managed(ref) {
			return ref, true
		}
		if parent := filepath.Dir(dir); parent == dir {
			// filepath.Dir("/") is "/" and filepath.Dir(".") is ".", so this is
			// the top of the ascent for both absolute and relative paths.
			return "", false
		}
	}
}

// Properties implements PropertyDeclaring: the Attrs that are properties of
// Puppet's file type (type/file.rb newproperty: ensure, content, mode, owner,
// group, target, and the four SELinux context properties). source, recurse
// and the rest are parameters.
func (File) Properties() Attrs {
	return NewAttrs("ensure", "content", "mode", "owner", "group", "target",
		"seluser", "selrole", "seltype", "selrange")
}

// Attrs implements Provider.
// show_diff is accepted but has no effect: Puppet shows a diff only when the
// global Puppet[:show_diff] is true (defaults to false), and voxagent has no
// such setting.
func (File) Attrs() Attrs {
	return NewAttrs("ensure", "content", "mode", "owner", "group", "source", "recurse", "purge", "path", "replace", "force", "links", "show_diff", "target",
		"seluser", "selrole", "seltype", "selrange", "selinux_ignore_defaults", "backup",
		"validate_cmd", "validate_replacement")
}

// SupportsValue implements ValueBounded.
//
// ensure is not bounded: every string is a keyword or a link target
// (type/file/ensure.rb:109-120). A value that is not a string is Validate's.
//
// source is bounded per element (see ListAttrs): a path or a file: URI is read
// locally, a puppet: URI only when it is a key of f.Sources, and every other
// scheme is a porting gap. recurse and purge are
// booleans, which catalog JSON carries as true/false and which arrive here as
// "true"/"false". recurse => remote, and every other spelling Puppet's boolean
// coercion accepts (yes, no, ...), is refused rather than guessed at.
func (f File) SupportsValue(attr, value string) bool {
	switch attr {
	case "source":
		return supportedSource(value, f.Sources)
	case "recurse", "purge":
		return value == "true" || value == "false"
	}
	return true
}

// ListAttrs implements ListValued: source may be a list, the first element
// that exists winning (type/file/source.rb:191-204).
func (File) ListAttrs() Attrs { return NewAttrs("source") }

// Validate implements Validating.
//
// Measured against Puppet 8.10.0: a relative path fails the whole run with
// "Could not run: Parameter path failed on File[rel/f1]: File paths must be
// fully qualified, not 'rel/f1'". Before this check voxagent wrote the file
// relative to its own working directory -- for an agent running as root, a
// write target chosen by wherever the process happened to start. The path is
// the path parameter when the catalog carries one, else the title (FilePath).
//
// The message is Puppet's verbatim, capital letter included.
//
// source, the creator rule, content with recurse and links => follow with
// recurse are checked here for the
// same reason: Puppet refuses the first two while converting the catalog
// (source.rb:86-103, type/file.rb:428-435), and the third is voxagent's own
// refusal of a combination it does not implement. So are a link with recurse
// and an ensure or target that is not a string.
//
// On an SELinux-enabled host a resource that wants default contexts is
// refused when selabel_lookup is not installed: applying it would leave
// contexts Puppet relabels. A resource that looks no default up is exempt:
// one with selinux_ignore_defaults => true or all four sel* declared
// (defaultsWanted), and a removal, which syncs no SELinux property
// (syncsSELinux).
func (f File) Validate(r catalog.Resource) error {
	given := r.Title
	if p, ok := r.Parameters["path"].(string); ok {
		given = p
	}
	if !filepath.IsAbs(given) {
		return fmt.Errorf("Parameter path failed on %s: File paths must be fully qualified, not '%s'", r.Ref(), given)
	}
	if raw, ok := r.Parameters["mode"]; ok {
		// mode.rb:78-96 validates while the catalog becomes a RAL, so an
		// invalid mode aborts the run before anything applies (measured).
		if _, err := parseDeclaredMode(raw, isMarked(f, r, "mode")); err != nil {
			return fmt.Errorf("Parameter mode failed on %s: %w", r.Ref(), err)
		}
	}
	if _, err := fileBoolParam(r, "replace", true); err != nil {
		return err
	}
	if _, err := fileBoolParam(r, "force", false); err != nil {
		return err
	}
	if _, err := backupValue(r); err != nil {
		return err
	}
	ignore, hasIgnore := r.Parameters["selinux_ignore_defaults"]
	if hasIgnore && ignore != true && ignore != false && ignore != "true" && ignore != "false" {
		// newvalues(:true, :false) (type/file/selcontext.rb:94), so unlike
		// replace and force it takes no yes or no; measured wording.
		return fmt.Errorf("Parameter selinux_ignore_defaults failed on %s: Invalid value \"%v\". Valid values are true, false.", r.Ref(), ignore)
	}
	for _, prop := range selProps {
		if v, ok := r.Parameters[prop]; ok {
			if _, isString := v.(string); !isString {
				// Ours: Puppet takes any value and joins it into the context.
				return fmt.Errorf("%s: %s => %v (%T) is not implemented: an SELinux context field is a string",
					r.Ref(), prop, redactIf(isMarked(f, r, prop), "%v", v), v)
			}
		}
	}
	if se := f.selinux(); defaultsWanted(r) && syncsSELinux(r) && se.enabled() {
		if !se.lookupAvailable() {
			return fmt.Errorf("%s: SELinux is enabled, but selabel_lookup (package libselinux-utils) is not installed, "+
				"so default SELinux contexts cannot be computed as Puppet computes them", r.Ref())
		}
		// Puppet computes the defaults while it instantiates the resources
		// (selcontext.rb:42-56, type.rb:2343), so a handle that does not
		// open fails the run before anything is applied (measured, D-35),
		// on a filesystem that carries labels (util/selinux.rb:67-74).
		if se.labelSupported(FilePath(r)) {
			if err := se.handleOpens(context.Background()); err != nil {
				return fmt.Errorf("%s: %w", r.Ref(), err)
			}
		}
	}
	if v, ok := r.Parameters["links"]; ok && v != "manage" && v != "follow" {
		// type/file.rb:272-281, newvalues(:follow, :manage); measured wording.
		return fmt.Errorf("Parameter links failed on %s: Invalid value \"%v\". Valid values are follow, manage.", r.Ref(), v)
	}
	raw, hasSource := r.Parameters["source"]
	if hasSource {
		if err := validateSource(r.Ref(), raw, sourceMarked(r)); err != nil {
			return err
		}
	}
	if v, ok := r.Parameters["ensure"]; ok {
		if _, isString := v.(string); !isString && v != false {
			// Ours. Puppet aliases false to absent (type/file/ensure.rb:62)
			// and makes anything else a link target, which fails as it
			// cannot be a path (measured: "FileSystem implementation
			// expected Pathname, got: 'TrueClass'"), or, for a list, links
			// to its last element (measured).
			return fmt.Errorf("%s: ensure => %v (%T) is not implemented: only a string or false is an ensure value or a link target",
				r.Ref(), redactIf(isMarked(f, r, "ensure"), "%v", v), v)
		}
	}
	if v, ok := r.Parameters["target"]; ok {
		if _, isString := v.(string); !isString {
			return fmt.Errorf("%s: target => %v (%T) is not implemented: a link target is a string",
				r.Ref(), redactIf(isMarked(f, r, "target"), "%v", v), v)
		}
	}
	_, hasContent := r.Parameters["content"]
	_, hasTarget := linkTarget(r)
	creators := 0
	for _, has := range []bool{hasContent, hasSource, hasTarget} {
		if has {
			creators++
		}
	}
	if creators > 1 {
		// Puppet names all three creators whichever two collide; a target
		// ensure supplies counts (ensure.rb:117 sets it while munging).
		return fmt.Errorf("Validation of %s failed: You cannot specify more than one of content, source, target", r.Ref())
	}
	if paramString(r, "recurse", "false") == "true" {
		// Ours: Puppet mirrors a target's tree as links (recurse_link,
		// type/file.rb:634-636, 680-700) and never retargets a recursed
		// link (target.rb:64), none of which is implemented.
		switch {
		case hasTarget:
			return fmt.Errorf("%s: target with recurse => true is not implemented: Puppet mirrors the target's tree as links", r.Ref())
		case paramString(r, "ensure", "") == "link":
			return fmt.Errorf("%s: ensure => link with recurse => true is not implemented: Puppet mirrors the target's tree as links", r.Ref())
		}
	}
	if paramString(r, "recurse", "false") == "true" && linksFollow(r) {
		// Ours: measured on Puppet 8.10.0, source => '<dir>/dl' with recurse
		// => true and links => follow, where dl links to a directory, creates
		// the destination and copies the linked tree; under manage it makes
		// the destination a link and yields no children. The walk
		// (walkTree) never follows a link, and doing so inside its os.Root
		// confinement is not implemented.
		return fmt.Errorf("%s: links => follow with recurse => true is not implemented: Puppet's walk follows a linked directory and copies its tree", r.Ref())
	}
	if hasContent && paramString(r, "recurse", "false") == "true" {
		// Ours: Puppet accepts it, and every generated child inherits the
		// content (newchild, type/file.rb:583-603), writing the same bytes
		// into every file of the tree.
		return fmt.Errorf("%s: content with recurse => true is not implemented: every file in the tree would get the same content", r.Ref())
	}
	return validateValidateCmd(r)
}

// Apply implements Provider.
func (f File) Apply(ctx context.Context, r catalog.Resource, noop bool) (changes []Change, warnings []Warning, err error) {
	if raw, ok := r.Parameters["mode"]; ok {
		// Validate refuses an invalid mode; a caller that skipped it still
		// gets a bare error, and nothing is touched.
		if _, err := parseDeclaredMode(raw, isMarked(f, r, "mode")); err != nil {
			return nil, nil, err
		}
	}
	bk, err := newBackup(r, f.Vardir)
	if err != nil {
		return nil, nil, err
	}
	// Backing up raises Puppet-sourced warnings of its own; they are the
	// run's, whatever the apply returns.
	defer func() {
		if bk != nil {
			warnings = append(warnings, bk.warnings...)
		}
	}()
	var fsys fileSystem = osFS{}
	if recursionChild(r) {
		// A generated child is applied through an os.Root on the directory it
		// was generated under, so nothing done to it can leave that tree
		// (rootFS). Under --noop a parent this run would have created does not
		// exist yet; noop writes nothing, so the child's reads go to the plain
		// filesystem, where nothing exists below the absent directory, as in
		// Puppet's noop run.
		dir := FilePath(*r.GeneratedFrom)
		root, err := openDirNoFollow(dir)
		switch {
		case err == nil:
			defer root.Close()
			fsys = rootFS{root: root, dir: dir}
		case noop && errors.Is(err, fs.ErrNotExist):
			// osFS stays: reads only.
		default:
			return nil, nil, err
		}
	}
	// Puppet munges the path parameter, which defaults to the title, before
	// anything reads the filesystem (type/file.rb:66-73); the clean path is
	// also what keeps lstat("/d/link/") from following the link under
	// links => manage. Titles and report refs stay as given.
	path := FilePath(r)
	var sel *selinuxPlan
	if syncsSELinux(r) {
		var err error
		if sel, err = planSELinux(ctx, f.selinux(), fsys, path, r); err != nil {
			return nil, nil, err
		}
	}
	if _, hasSource := r.Parameters["source"]; hasSource {
		// Resolved before anything else, whatever ensure says: Puppet fetches
		// the metadata in retrieve (type/file.rb:842-844), before ensure is
		// evaluated, so a missing source fails even ensure => absent
		// (measured) and fails under --noop too. source => [] is a source
		// too (measured): it is missing everywhere.
		var srcFS fileSystem = osFS{}
		sources := f.Sources
		if recursionChild(r) {
			rfs, childSources, err := childSourceFS(r, f.Sources)
			if err != nil {
				return nil, nil, err
			}
			defer rfs.root.Close()
			srcFS, sources = rfs, childSources
		}
		src, err := resolveSource(srcFS, r, f.Environment, sources)
		if err != nil {
			return nil, nil, err
		}
		if fi, kept := keptByReplaceFalse(fsys, path, r); kept {
			return propertiesAfterEnsure(ctx, fsys, path, r, noop, fi, sel, bk)
		}
		switch {
		case paramString(r, "ensure", "") == "absent" || paramString(r, "ensure", "") == "false":
			// copy_source_values leaves an explicit absent alone
			// (type/file/source.rb:168), and false is its alias.
			changes, err := applyAbsent(ctx, fsys, path, wantedType(r, "absent"), noop, forceTrue(r), linksFollow(r), bk)
			return changes, nil, err
		case src.isDir:
			// The source's type replaces any other ensure (source.rb:171).
			// Measured: ensure => file with a directory source creates a
			// directory.
			return applyDirectory(ctx, fsys, path, r, noop, sel, bk)
		case src.linkTarget != "":
			// A link source sets ensure and target (source.rb:173-176),
			// replacing any the catalog gave.
			lr := r
			lr.Parameters = maps.Clone(r.Parameters)
			lr.Parameters["ensure"] = "link"
			lr.Parameters["target"] = src.linkTarget
			return applyLink(ctx, fsys, path, lr, noop, sel, bk)
		default:
			c := sourceContent(src)
			return applyFile(ctx, fsys, path, r, noop, false, &c, sel, bk)
		}
	}
	if fi, kept := keptByReplaceFalse(fsys, path, r); kept {
		return propertiesAfterEnsure(ctx, fsys, path, r, noop, fi, sel, bk)
	}
	if _, managed := r.Parameters["ensure"]; !managed {
		// type/file.rb:561-567 infers ensure => link from target, else
		// ensure => file from content; source is the third inference, above.
		if _, ok := linkTarget(r); ok {
			return applyLink(ctx, fsys, path, r, noop, sel, bk)
		}
		// With none of them ensure stays unmanaged: the resource reconciles
		// mode, owner and group of whatever is at the path, and nothing at
		// all when nothing is. Measured on Puppet 8.10.0: file { '/x': mode
		// => '0600' } on an absent path changes and reports nothing, and on
		// an existing 0644 file reports only "mode changed '0644' to
		// '0600'". voxagent used to default ensure to file and created the
		// path.
		if _, hasContent := r.Parameters["content"]; hasContent {
			return applyFile(ctx, fsys, path, r, noop, false, nil, sel, bk)
		}
		return applyMetadataOnly(ctx, fsys, path, r, noop, sel)
	}
	ensure := paramString(r, "ensure", "")

	switch ensure {
	case "absent", "false":
		// false is an alias of absent (type/file/ensure.rb:62); a JSON false
		// reaches here as "false" through paramString.
		// No warnings from this branch: a removal reconciles no ownership, so
		// nothing on it can reach applyOwnership's root check.
		changes, err := applyAbsent(ctx, fsys, path, wantedType(r, "absent"), noop, forceTrue(r), linksFollow(r), bk)
		return changes, nil, err
	case "file":
		return applyFile(ctx, fsys, path, r, noop, false, nil, sel, bk)
	case "present":
		// "present" is looser than "file": measured against Puppet 8.10.0,
		// ensure => present over an existing path of ANY type -- including a
		// directory -- is a clean no-op, whereas ensure => file over the same
		// directory errors (fix #5). applyFile's tolerant flag is what
		// distinguishes the two.
		return applyFile(ctx, fsys, path, r, noop, true, nil, sel, bk)
	case "directory":
		return applyDirectory(ctx, fsys, path, r, noop, sel, bk)
	default:
		// link, or a link target (linkTarget).
		return applyLink(ctx, fsys, path, r, noop, sel, bk)
	}
}

// Refresh implements Provider. A file has nothing to refresh.
func (File) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Refreshable implements Refreshing. type/file.rb defines no refresh, so
// Puppet queues no event for a notified File.
func (File) Refreshable(catalog.Resource) bool { return false }

// Prefetch implements Provider. A file reads its own state in Apply; the
// provider it returns serves one run, so it gets that run's store of batched
// SELinux defaults.
func (p File) Prefetch(context.Context, []catalog.Resource) (Provider, error) {
	p.defaults = &runDefaults{answers: map[defaultQuery]defaultAnswer{}}
	return p, nil
}

// linksFollow is links => follow; manage is the default (type/file.rb:272-281).
func linksFollow(r catalog.Resource) bool {
	return paramString(r, "links", "manage") == "follow"
}

// syncsSELinux reports whether the resource can sync its SELinux properties,
// and so needs a plan and possibly a default lookup. A removal syncs no
// other property; under replace => false an existing path is kept instead
// and its properties sync (keptByReplaceFalse). Validate and Apply must
// agree on it.
func syncsSELinux(r catalog.Resource) bool {
	e := paramString(r, "ensure", "")
	return (e != "absent" && e != "false") || replaceFalse(r)
}

// keptByReplaceFalse reports whether replace => false keeps what is at path,
// and returns it as read. type/file/ensure.rb:162-165: unless the path is
// absent or replace is true, ensure is in sync, whatever the path's type,
// whatever ensure asks for and whatever force says -- so nothing is removed,
// replaced or written, and a purge removes nothing, as a purged child
// inherits replace (measured on Puppet 8.10.0). Mode, owner, group, target
// and the SELinux properties still sync as ordinary properties
// (propertiesAfterEnsure).
//
// The path is read as ensure's retrieve reads it (ensure.rb:174-183,
// type/file.rb:895-917): through statFor, so under links => follow a
// dangling link is absent and is created through as usual. A read that
// fails for any other reason keeps nothing, and the branch that would
// create reports it.
func keptByReplaceFalse(fsys fileSystem, path string, r catalog.Resource) (os.FileInfo, bool) {
	if !replaceFalse(r) {
		return nil, false
	}
	fi, err := statFor(fsys, path, linksFollow(r))
	return fi, err == nil
}

// statFor reads path the way Puppet's stat does (type/file.rb:895-903): stat,
// which follows a final symlink, under links => follow, and lstat otherwise.
// Everything ensure, content and mode compare against comes from it.
func statFor(fsys fileSystem, path string, follow bool) (os.FileInfo, error) {
	if follow {
		return fsys.Stat(path)
	}
	return fsys.Lstat(path)
}

// applyAbsent removes whatever is at path. force is Puppet's parameter of the
// same name: without it a directory is left in place (removeExisting). follow
// is links => follow, under which a link's target decides what is in the way.
func applyAbsent(ctx context.Context, fsys fileSystem, path string, wanted string, noop, force, follow bool, bk *backup) ([]Change, error) {
	fi, err := statFor(fsys, path, follow)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}

	// previous_value is the path's actual TYPE, not the constant "present".
	// Measured 2026-09-18: a regular file reports "file", a symlink reports
	// "link" and, measured 2026-10-06, a directory reports "directory", all
	// with name "file_removed". Ruby gets this from the ensure property's
	// retrieve, which answers what the path currently is. Under links =>
	// manage that is Lstat, which makes the symlink case answer "link" rather
	// than the type of its target. Under follow it is Stat, so a link to a
	// directory answers "directory" and, without force, is noticed and left
	// (measured). Either way what is removed is the link: Remove and
	// RemoveAll never follow one.
	change := Change{Property: "ensure", Was: ftype(fi), Is: "absent",
		Name: "file_removed", Message: "removed"}
	if noop {
		// Under noop ensure is never synced, so remove_existing never runs
		// and there is nothing to notice.
		return []Change{change}, nil
	}
	// A directory without force is noticed and left in place, yet ensure's
	// sync still returns :file_removed (ensure.rb:185-192), so the event says
	// it was removed -- every run, because the directory stays (I-1, measured
	// on Puppet 8.10.0). With force the whole tree goes. A backup comes
	// first, and a failed one removes nothing (type/file.rb:799-805).
	pathWarnings, err := bk.existing(fsys, path, fi)
	change.PathWarnings = pathWarnings
	if err == nil {
		_, change.PathNotices, err = removeExisting(ctx, fsys, path, fi, force, wanted)
	}
	if err != nil {
		// Attributed to ensure, and deliberately NOT wrapped. Measured
		// 2026-09-18, ensure => absent on a file whose parent directory is
		// read-only: one event, property "ensure", name "file_removed", message
		// "change from 'file' to 'absent' failed: Permission denied @
		// apply2files - <path>" -- no "Could not set 'absent' on ensure:" and no
		// (file:, line:) suffix, even though Errno::EACCES is not a
		// Puppet::Error and so would be wrapped by property.rb's set.
		//
		// Read type/file/ensure.rb:185: sync calls @resource.remove_existing and
		// returns :file_removed BEFORE delegating to super, so an absent never
		// reaches set at all and nothing can wrap it.
		//
		// err is used as-is: os.Remove returns a *PathError that already reads
		// "remove <path>: permission denied", which names the operation and the
		// path exactly as Ruby's errno text does. Wrapping it in another
		// "remove %s" would print the path twice.
		change.Err = err
	}
	return []Change{change}, nil
}

// removeExisting is Puppet's remove_existing (type/file.rb:1022-1049): it
// clears path so something else can take its place. fi is path's Lstat.
//
// A directory is removed, tree and all, only with force; without it the
// notice is returned and nothing is touched, which is not an error --
// callers decide what a refusal means. A file, link, fifo or socket is
// unlinked (type/file.rb:814-815); a link is never followed. Any other type
// -- block and character devices -- fails as Puppet does (:816-818); that
// raise happens in ensure's sync before set, so callers do not wrap it.
//
// Before it removes, it says what it removes and what replaces it, as Ruby's
// remove_directory and remove_file do (:1035, :1048), at the resource's path.
// wanted is the replacing type as the line shows it: see wantedType.
func removeExisting(ctx context.Context, fsys fileSystem, path string, fi os.FileInfo, force bool, wanted string) (removed bool, notices []string, err error) {
	if fi.IsDir() {
		if !force {
			return false, []string{"Not removing directory; use 'force' to override"}, nil
		}
		runlog.Debugf(ctx, SourcesFrom(ctx).Resource, "Removing existing directory for replacement with %s", wanted)
		err = fsys.RemoveAll(path)
		return err == nil, nil, err
	}
	switch current := ftype(fi); current {
	case "file", "link", "fifo", "socket":
		runlog.Debugf(ctx, SourcesFrom(ctx).Resource, "Removing existing %s for replacement with %s", current, wanted)
		err = fsys.Remove(path)
		return err == nil, nil, err
	}
	return false, nil, fmt.Errorf("Could not remove files of type %s", ftype(fi))
}

// wantedType is should.to_s in remove_existing's debug line: kind, or
// [redacted] when the catalog marks ensure (D-78: Ruby leaks it).
func wantedType(r catalog.Resource, kind string) string {
	return redactIf(isMarked(File{}, r, "ensure"), "%s", kind)
}

// ftype names what fi is as Ruby's File.lstat(path).ftype does, which is the
// previous_value Puppet reports for ensure.
func ftype(fi os.FileInfo) string {
	switch m := fi.Mode(); {
	case m.IsRegular():
		return "file"
	case m.IsDir():
		return "directory"
	case m&os.ModeSymlink != 0:
		return "link"
	case m&os.ModeNamedPipe != 0:
		return "fifo"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeCharDevice != 0:
		return "characterSpecial"
	case m&os.ModeDevice != 0:
		return "blockSpecial"
	}
	return "unknown"
}

// applyFile applies ensure => file (tolerant == false) or ensure => present
// (tolerant == true). src is the content a source supplies; nil means the
// catalog's own content parameter, if it has one.
func applyFile(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop, tolerant bool, src *content, sel *selinuxPlan, bk *backup) ([]Change, []Warning, error) {
	var changes []Change
	var warnings []Warning

	// ensureValue is the value Ruby's set was handed, which is both what its
	// wrapper interpolates and what the event carries as desired_value. Measured
	// 2026-09-18, a create that cannot succeed: ensure => file reports
	// desired_value "file" and "Could not set 'file' on ensure: ...", while
	// ensure => present reports desired_value "present" and "Could not set
	// 'present' on ensure: ...". tolerant IS that distinction.
	ensureValue := "file"
	if tolerant {
		ensureValue = "present"
	}

	// content must be a string. Measured: content of float64(1e21) wrote the
	// literal bytes "1e+21"; an explicit JSON null wrote the four bytes
	// "<nil>". Puppet refuses both rather than serialising whatever %v
	// produces, so voxagent does too (fix #8).
	want := src
	if raw, ok := r.Parameters["content"]; ok && want == nil {
		s, ok := raw.(string)
		if !ok {
			return changes, nil, fmt.Errorf("content is %T, not a string", raw)
		}
		c := literalContent([]byte(s))
		want = &c
	}
	hasContent := want != nil
	if hasContent {
		val, err := validationOf(r, isMarked(File{}, r, "content"))
		if err != nil {
			return changes, nil, err
		}
		validated := *want
		validated.validate = val
		want = &validated
	}

	follow := linksFollow(r)
	existing, statErr := statFor(fsys, path, follow)
	created := statErr != nil && os.IsNotExist(statErr)
	// ensure => file compares the path's type, so anything but a regular
	// file is replaced (type/file.rb:1022-1049): a directory when force
	// allows, a link, fifo or socket always (measured; a link's target is
	// untouched), and a device fails in removeExisting. Only links =>
	// manage can see a link here; follow sees its target. present is
	// satisfied by any type, below.
	notFile := statErr == nil && !existing.Mode().IsRegular()
	replacing := notFile && !tolerant
	needsWrite := false
	// ensureIdx and contentIdx locate the Change a later failure has to be
	// attributed to, because the operation that can fail happens after the
	// Change describing it has been appended.
	ensureIdx, contentIdx := -1, -1
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return changes, nil, fmt.Errorf("stat %s: %w", path, statErr)
	case created || replacing:
		wantMode, modeErr := createMode(r, false)
		was := "absent"
		if replacing {
			was = ftype(existing)
		}
		// Puppet reports the CONTENT checksum on a file it created with managed
		// content -- type/file/content.rb change_to_s, whose absent branch is
		// "defined content as '...'". A bare "created" is what it says only when
		// no content is managed.
		ensureChange := Change{Property: "ensure", Was: was, Is: ensureValue,
			Name: "file_created", Message: ensureChangeMessage(was, "file")}
		// A file with neither content nor source is written in place
		// (type/file.rb:964-966), so the empty body is not staged.
		body := literalContent(nil)
		body.staged = false
		if hasContent {
			ensureChange.Message = fmt.Sprintf("defined content as '%s'", want.sum)
			body = *want
		}
		if !noop {
			ready := true
			if replacing {
				pathWarnings, err := bk.existing(fsys, path, existing)
				ensureChange.PathWarnings = pathWarnings
				removed := false
				var notices []string
				if err == nil {
					removed, notices, err = removeExisting(ctx, fsys, path, existing, forceTrue(r), wantedType(r, ensureValue))
				}
				if err == nil && !removed {
					// remove_existing declined and runs a second time on the
					// way to the create (I-3), so the notice is logged twice,
					// and so is a backup's warning; the create then meets the
					// directory and opens it as a file. The errno text is
					// masked by G1 (D-9).
					ensureChange.PathWarnings = append(pathWarnings, pathWarnings...)
					ensureChange.PathNotices = append(notices, notices...)
					err = wrapEnsureError(ensureValue, r,
						&fs.PathError{Op: "open", Path: path, Err: syscall.EISDIR})
				}
				// A failed removal is not wrapped, like applyAbsent's: it
				// happens in ensure's sync before set runs.
				if err != nil {
					ensureChange.Err = err
					ready = false
				}
			}
			if ready && modeErr != nil {
				// write computes the mode after remove_existing and before
				// the create (type/file.rb:930-933); its Puppet::Error passes
				// property.rb:490 unwrapped. Measured on an absent path:
				// "change from 'absent' to 'file' failed: X only works ...",
				// nothing created. On a replaced path the removal comes first,
				// so the old path is lost and nothing replaces it (I-23): read
				// (type/file/ensure.rb:185-186 removes before set, and write's
				// own remove_existing precedes the compute, type/file.rb:929-932),
				// not measured.
				ensureChange.Err = modeErr
			} else if ready {
				if err := createFile(ctx, fsys, path, body, wantMode); err != nil {
					// Wrapped. The failure is an Errno from the write itself, and
					// type/file/ensure.rb declares :file and :present with blocks that
					// Puppet::Property.newvalue turns into set_file and set_present
					// methods (read property.rb:168-175), so property.rb:483 set takes
					// its `method` branch: :490 re-raises a Puppet::Error untouched and
					// :492 wraps everything else. Measured 2026-09-18: "change from
					// 'absent' to 'file' failed: Could not set 'file' on ensure:
					// Permission denied @ dir_s_mkdir - <tmp>.lock (file: <manifest>,
					// line: 1)".
					// A checksum mismatch is an exception: fail_if_checksum_is_wrong
					// raises through self.fail, a Puppet::Error, which :490
					// re-raises untouched. So is a validate_cmd's
					// Puppet::ExecutionFailure (measurement c02c).
					var mismatch *checksumMismatchError
					var failed *executionFailure
					if errors.As(err, &mismatch) || errors.As(err, &failed) {
						ensureChange.Err = err
					} else {
						ensureChange.Err = wrapEnsureError(ensureValue, r, err)
					}
				}
			}
		}
		ensureIdx = len(changes)
		changes = append(changes, ensureChange)
	case notFile:
		// ensure => present: measured, Puppet 8.10.0, an existing directory,
		// link or fifo satisfies present outright. content is a file-specific
		// property and is not reconciled against any of them
		// (data_sync.rb:26-35, should_be_file?); fall through to
		// mode/owner/group reconciliation only. Puppet also warns that no
		// content will be synced when content is declared (D-34).
	case hasContent:
		// replace => false never reaches here: Apply keeps an existing path
		// (keptByReplaceFalse), which also puts content in sync
		// (data_sync.rb:38).
		// Non-blocking and type-checked, as a source is read: a regular file
		// swapped for a fifo since the stat must not hang the agent.
		f, err := openSource(fsys, path, follow)
		if err != nil {
			return changes, nil, fmt.Errorf("read %s: %w", path, err)
		}
		current, err := checksumOf(f)
		_ = f.Close()
		if err != nil {
			return changes, nil, fmt.Errorf("read %s: %w", path, err)
		}
		if current != want.sum {
			// Checksums, not content. Puppet prints them precisely so a report
			// is not a copy of every managed file -- previous_value and
			// desired_value travel to PuppetDB and into dashboards. Compared as
			// checksums too: a source may be larger than memory, so neither
			// side is read whole.
			contentIdx = len(changes)
			changes = append(changes, Change{Property: "content",
				Was: current, Is: want.sum,
				Message: fmt.Sprintf("content changed '%s' to '%s'", current, want.sum)})
			needsWrite = true
		}
	}

	if needsWrite && !noop {
		// write's remove_existing backs the file up first (type/file.rb:930);
		// a failed backup is content's failure, unwrapped like the write's,
		// and nothing is written.
		pathWarnings, err := bk.existing(fsys, path, existing)
		changes[contentIdx].PathWarnings = pathWarnings
		if err == nil {
			err = replaceFile(ctx, fsys, path, *want)
		}
		if err != nil {
			// Attributed to content, and NOT wrapped. ensure is already in sync
			// on this path, so resource_harness.rb:86's ensure sync is NO_ACTION
			// and content syncs as an ordinary property at :94 -- nothing puts it
			// inside the ensure setter, so property.rb's wrapper never sees it.
			// Measured 2026-09-18, content => 'new' over an existing file whose
			// parent is read-only: "change from '{sha256}...' to '{sha256}...'
			// failed: Permission denied @ dir_s_mkdir - <tmp>.lock", with no
			// "Could not set" and no (file:, line:) suffix.
			//
			// The walk continues rather than returning: sync_if_needed's rescue
			// is commented "Execution will continue on StandardErrors", so
			// mode, owner and group are still reconciled after a failed write.
			changes[contentIdx].Err = err
		} else if follow {
			// Under links => follow the write replaced a link, and replace_file
			// gave the new file the LINK's lstat mode, uid and gid
			// (util.rb:623-626; I-5), not those of the target that was
			// compared. write's property_fix (type/file.rb:970, :1112-1121)
			// then applies the declared mode, owner and group to the new file
			// against a fresh stat, unreported, inside content's sync -- so a
			// failure there fails content, unwrapped. Without follow the
			// replaced file was the path itself, replace_file carried its own
			// metadata across, and the reported pass below sees what
			// property_fix would.
			fixed, fixWarnings, fixErr := applyMetadata(ctx, fsys, path, r, false, nil, sel)
			changes[contentIdx].Err = propertyFixErr(fixed, fixErr)
			warnings = fixWarnings
		} else {
			// The SELinux label is the one thing replace_file does not carry
			// across: the renamed file has the label it was created with. So
			// property_fix relabels it here, whatever retrieve read
			// (type/file.rb:970).
			fixed, fixWarnings, fixErr := applySELinux(sel, path, false, true)
			changes[contentIdx].Err = propertyFixErr(fixed, fixErr)
			warnings = fixWarnings
		}
	}

	// Mode, owner and group are reported against existing, the state Puppet
	// retrieved before anything synced: under follow that is the target the
	// content write replaced, not the new file at the path.
	changes, metaWarnings, err := recordMetadata(ctx, fsys, path, r, noop, ensureValue, changes, ensureIdx, existing, sel, bk)
	return changes, append(warnings, metaWarnings...), err
}

func applyDirectory(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, sel *selinuxPlan, bk *backup) ([]Change, []Warning, error) {
	var changes []Change

	// Under links => manage a link is not a directory, so it is replaced
	// (measured: "ensure changed 'link' to 'directory'"); under follow a link
	// to a directory is one, and is in sync (measured).
	existing, statErr := statFor(fsys, path, linksFollow(r))
	created := statErr != nil && os.IsNotExist(statErr)
	ensureIdx := -1
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return changes, nil, fmt.Errorf("stat %s: %w", path, statErr)
	case created || !existing.IsDir():
		wantMode, modeErr := createMode(r, true)
		was := "absent"
		if !created {
			// Whatever is in the way is replaced; removeExisting decides
			// which types it can remove.
			was = ftype(existing)
		}
		ensureChange := Change{Property: "ensure", Was: was, Is: "directory",
			Name: "directory_created", Message: ensureChangeMessage(was, "directory")}
		if !noop {
			// The parent check is Puppet's own, reproduced rather than inferred
			// from mkdir's ENOENT. type/file/ensure.rb's :directory block tests
			// the parent BEFORE Dir.mkdir and raises Puppet::Error, which
			// property.rb:490 re-raises untouched -- so this sentence reaches the
			// report with no "Could not set 'directory' on ensure:" wrapper and no
			// (file:, line:) suffix. Measured 2026-09-18, from a resource that had
			// both a file and a line: "change from 'absent' to 'directory' failed:
			// Cannot create <path>; parent directory <parent> does not exist".
			//
			// Ruby's test is Puppet::FileSystem.exist?(parent), i.e. File.exist?,
			// which is false for ANY stat failure -- including EACCES on a parent
			// that exists but is unsearchable -- so a bare `err != nil` here is
			// the same test rather than a looser one.
			parent := filepath.Dir(path)
			// remove_existing comes before the create; only a directory needs
			// force, and a directory is not in the way here. Its failure is unwrapped, as applyAbsent's is.
			var removeErr error
			if !created {
				ensureChange.PathWarnings, removeErr = bk.existing(fsys, path, existing)
				if removeErr == nil {
					_, _, removeErr = removeExisting(ctx, fsys, path, existing, false, wantedType(r, "directory"))
				}
			}
			if removeErr != nil {
				ensureChange.Err = removeErr
			} else if _, err := fsys.Stat(parent); err != nil {
				ensureChange.Err = fmt.Errorf(
					"Cannot create %s; parent directory %s does not exist", path, parent)
			} else if modeErr != nil {
				// The :directory block computes the mode after the parent
				// check, as Dir.mkdir's argument (type/file/ensure.rb:89-92);
				// its Puppet::Error is unwrapped, and nothing is created. A
				// replaced path was already removed above, by ensure's sync
				// (type/file/ensure.rb:185-186; read, not measured; I-23).
				ensureChange.Err = modeErr
			} else if err := fsys.Mkdir(path, wantMode); err != nil {
				ensureChange.Err = wrapEnsureError("directory", r, err)
			} else if err := fsys.Chmod(path, wantMode); err != nil {
				// See the matching comment in createFile: Mkdir's mode argument
				// passes through the umask too.
				ensureChange.Err = wrapEnsureError("directory", r, err)
			}
		}
		ensureIdx = len(changes)
		changes = append(changes, ensureChange)
	}

	return recordMetadata(ctx, fsys, path, r, noop, "directory", changes, ensureIdx, existing, sel, bk)
}

// createMode returns the mode a file (isDir false) or directory this resource
// creates is given. Shared by applyFile's and applyDirectory's creation paths,
// which need the final mode before the path exists (fix #7: create at the
// target mode directly rather than writing at a hard-coded mode and chmod'ing
// after, which left a permission window and fabricated a mode-transition event
// from a value the path never actually had).
//
// A symbolic mode is computed from Puppet's base, 0644 for a file
// (type/file.rb:932-933) and 0755 for a directory (type/file/ensure.rb:89-92),
// whose X always applies. An octal directory mode is masked here: Puppet
// mkdirs with the declared number and chmods to the masked one in
// property_fix (mode.rb:154-159), unreported, so the end state and the report
// are the same. The error is the computation's, X under the wrong operator.
func createMode(r catalog.Resource, isDir bool) (os.FileMode, error) {
	base := uint32(0o644)
	if isDir {
		base = 0o755
	}
	raw, ok := r.Parameters["mode"]
	if !ok {
		return bitsToFileMode(base), nil
	}
	m, err := parseDeclaredMode(raw, isMarked(File{}, r, "mode"))
	if err != nil {
		return 0, err
	}
	n, err := m.apply(base, isDir)
	if err != nil {
		return 0, err
	}
	if isDir && m.octal {
		n = dirmask(n)
	}
	return bitsToFileMode(n), nil
}

// recordMetadata reconciles owner, group and mode and files the result the way
// Puppet's transaction files it, which is not the same on a path that already
// existed and a path this run created.
//
// On an existing path they are ordinary properties, after target
// (propertiesAfterEnsure). resource_harness.rb:86 syncs ensure first, gets
// NO_ACTION because ensure is in sync, and :93 then syncs every property in
// turn through sync_if_needed -- so each records its own event.
//
// On a CREATED path ensure's sync returned an action, so :91's `if ensure_event
// == NO_ACTION` is false and the harness never touches another property at all.
// The metadata is applied instead by type/file.rb:1112 property_fix, which calls
// thing.sync DIRECTLY rather than sync_if_needed, so nothing it does is
// recorded. Measured 2026-09-18: an absent path with owner => 'nosuchuser' emits
// exactly ONE event -- property "ensure", name "file_created" -- and no owner
// event; the same manifest under --noop, with mode, owner and group all out of
// sync, also emits exactly one event.
//
// So on the created path applyMetadata still RUNS -- the metadata really is
// applied, silent does not mean unapplied -- and only its reporting is dropped.
// Its first failure is collapsed onto the ensure change and wrapped as an ensure
// failure, because property_fix runs inside the ensure setter and property.rb
// wraps it at :492 -- :490 spares a Puppet::Error, and property_fix raises
// whatever the property raised, which for owner is a bare RuntimeError.
//
// ensureIdx locates that ensure Change, and is -1 exactly when the path already
// existed. One parameter rather than a separate created flag: two values that
// must agree can disagree.
//
// before is the path's state as read before this run changed it, which an
// existing path's properties are compared against (applyMetadata). A created
// path ignores it: what was there before is gone.
func recordMetadata(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, ensureValue string,
	changes []Change, ensureIdx int, before os.FileInfo, sel *selinuxPlan, bk *backup) ([]Change, []Warning, error) {
	created := ensureIdx >= 0

	if created && changes[ensureIdx].Err != nil {
		// Precedence: an ensure failure WINS, and the metadata is not even
		// attempted. Ruby cannot reach property_fix when the create raises --
		// type/file.rb calls it at :970, after the write, and ensure.rb's
		// :directory block calls it after Dir.mkdir -- so there is no metadata
		// failure to collapse. Letting one overwrite this Err would blame a
		// property for a path that was never created, and chmod'ing or chown'ing
		// that path would be a second bug.
		//
		// No warning either: Ruby cannot reach property_fix when the create
		// raised, so owner.insync? -- where the warnonce lives -- is never
		// called on this path.
		return changes, nil, nil
	}

	if !created {
		more, warnings, err := propertiesAfterEnsure(ctx, fsys, path, r, noop, before, sel, bk)
		return append(changes, more...), warnings, err
	}
	metaChanges, metaWarnings, metaErr := applyMetadata(ctx, fsys, path, r, noop, nil, sel)

	// Under noop ensure is never synced, so neither write nor property_fix
	// runs and nothing can fail: in particular a symbolic mode is never
	// computed against a path that does not exist (read: resource_harness.rb
	// :137-138 records a noop event in place of sync). applyMetadata changed
	// nothing either.
	if noop {
		return changes, metaWarnings, nil
	}
	if collapsed := propertyFixErr(metaChanges, metaErr); collapsed != nil {
		changes[ensureIdx].Err = wrapEnsureError(ensureValue, r, collapsed)
	}
	// Warnings SURVIVE the collapse, even though the events do not. The events
	// are dropped because property_fix calls thing.sync directly and nothing
	// records what it did; the warning is not one of those recordings. Read
	// type/file.rb:1119: property_fix calls `thing.safe_insync?(currentvalue)`,
	// and insync? is exactly where owner.rb:36's warnonce fires -- so a created
	// path whose declared owner cannot be applied warns in Ruby too.
	return changes, metaWarnings, nil
}

// propertyFixErr is the one failure property_fix raises (type/file.rb:1112-1121)
// given what applyMetadata returned for the same pass.
//
// It exists because property_fix's failures do not travel as an error return:
// applyMetadata reports them per-property, and the callers have to collapse
// them back into the single failure Puppet reports. property_fix syncs owner,
// group and mode in declaration order -- `properties.each` filtered by
// include? (:1113-1114), so the filter list's mode-first order decides nothing
// -- and raises out of the first that fails. An attributed failure therefore
// wins over err, which applyMetadata can only have hit later -- mode's
// retrieve after a chown already failed. property_fix has
// no rescue of its own, so a failure it could not attribute to a property --
// Ruby's `currentvalue = thing.retrieve` raising -- is raised all the same.
func propertyFixErr(changes []Change, err error) error {
	for _, ch := range changes {
		if ch.Err != nil {
			return ch.Err
		}
	}
	return err
}

// applyMetadataOnly reconciles owner, group and mode on a path whose ensure is
// unmanaged. An absent path is in sync: there is nothing for the properties
// to describe, and creating one would be managing ensure after all.
//
// A symlink under links => manage keeps its mode (type/file/mode.rb:133), is
// lchown'ed (provider/file/posix.rb:99-144) and is relabelled itself
// (applySELinux); under follow its target is chmod'ed and chown'ed through
// it, as Puppet does.
//
// A symlink found by a recursion walk is treated the same way, as Puppet's
// newchild makes it a File like any other (type/file.rb:583-602). Its links is
// always manage: the parent's is inherited, and Validate refuses follow with
// recurse (D-32). Its fsys is the tree's rootFS, whose Lchown and O_PATH
// label handle act on the link itself and stay in the tree; even under follow
// rootFS would refuse to chmod or chown through the link (D-31).
func applyMetadataOnly(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, sel *selinuxPlan) ([]Change, []Warning, error) {
	fi, err := statFor(fsys, path, linksFollow(r))
	if err != nil {
		if os.IsNotExist(err) {
			// resource_harness.rb:97, at the resource's path.
			runlog.Debugf(ctx, SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("lstat %s: %w", path, err)
	}
	return applyMetadata(ctx, fsys, path, r, noop, fi, sel)
}

// dirmask is type/file/mode.rb:111-121: on a directory, a numeric mode's read
// bit for each class brings that class's search bit with it, so mode => '0644'
// -- the natural mode for the files of a recursed tree -- leaves the
// directories themselves traversable as 0755. Puppet applies it only to an
// all-digit mode (:115), so a symbolic mode, which computes its own search
// bits through X, is never masked.
func dirmask(m uint32) uint32 {
	if m&0o400 != 0 {
		m |= 0o100
	}
	if m&0o040 != 0 {
		m |= 0o010
	}
	if m&0o004 != 0 {
		m |= 0o001
	}
	return m
}

// applyMetadata reconciles owner, group and mode, then the SELinux
// properties, against an existing path -- the order type/file.rb:1127-1139
// declares them in, which is the order the harness syncs and reports them
// (measured as root: owner_changed, group_changed, mode_changed on one file)
// and the order property_fix walks them (:1113-1114).
//
// before is the state to compare against, read by the caller before it wrote
// anything; nil reads the path now (retrieved). A link under links => manage
// gets no mode change (type/file/mode.rb:133, which only logs at debug).
//
// A created path passes nil, as property_fix re-reads before each property
// (type/file.rb:1117). applyFile/applyDirectory created it at its final mode
// (createMode), so mode is normally in sync -- but a chown clears S_ISUID
// and S_ISGID on a non-directory, even as root, and mode then chmods them
// back, as Puppet's does (measured: a created 4755 file owned by nobody ends
// 4755). recordMetadata drops the created path's events, so no mode
// transition is reported for a path that did not exist (fix #7).
//
// A symbolic mode is recomputed here from the created mode, so one that does
// not converge moves the path a second time: u+x,g+X creates 0744 and leaves
// 0754, because X now sees an x bit (I-24). Read, not measured: property_fix
// syncs whatever is not insync? (type/file.rb:1112-1121), and insync? computes
// from the current mode (mode.rb:141-147).
func applyMetadata(ctx context.Context, fsys fileSystem, path string, r catalog.Resource, noop bool, before os.FileInfo, sel *selinuxPlan) ([]Change, []Warning, error) {
	follow := linksFollow(r)

	// The declared mode is parsed before anything syncs, so a mode that
	// cannot be parsed fails the resource with no property moved. It stays
	// resource-level (bare error, no Change): nothing was attempted, so
	// nothing can be blamed.
	raw, manageMode := r.Parameters["mode"]
	var declared declaredMode
	if manageMode {
		var err error
		if declared, err = parseDeclaredMode(raw, isMarked(File{}, r, "mode")); err != nil {
			return nil, nil, err
		}
	}

	// applyOwnership reports a failed chown as a Change.Err rather than an
	// error return, so its own error return is resource-level only -- a stat
	// it could not read.
	changes, warnings, err := applyOwnership(fsys, path, r, noop, before)
	if err != nil {
		return changes, warnings, err
	}

	// Mode is reconciled whether or not ownership succeeded.
	// resource_harness.rb:147's rescue is commented "Execution will continue on
	// StandardErrors", and perform_changes' resource.properties.each moves to
	// the next property.
	if manageMode {
		// A retrieved failure stays resource-level, as a parse failure does: it
		// never got the "was" side of a comparison. Compare os.Chmod below,
		// which DID attempt a sync and so is attributed to the Change. A nil
		// before is read after the chowns, as property_fix's :needs_stat
		// (type/file.rb:1117) is.
		fi, err := retrieved(fsys, path, follow, before)
		if err != nil {
			return changes, warnings, err
		}
		// An absent path reads as mode 0. A link (links => manage) is in sync
		// whatever its mode, and nothing is computed for it (mode.rb:133-135),
		// which Ruby says at the mode property's path.
		var current uint32
		isLink, isDir := false, false
		if fi != nil {
			current = modeBits(fi.Mode())
			isLink = fi.Mode()&os.ModeSymlink != 0
			isDir = fi.IsDir()
		}
		if isLink {
			runlog.Debugf(ctx, SourcesFrom(ctx).Resource+"/mode", "Not managing symlink mode")
		} else if ch, changed := modeChange(fsys, path, noop, declared, current, isDir); changed {
			changes = append(changes, ch)
		}
	}

	// The SELinux properties come last. A nil before is a fresh read, which
	// is property_fix's: see applySELinux.
	selChanges, selWarnings, err := applySELinux(sel, path, noop, before == nil)
	return append(changes, selChanges...), append(warnings, selWarnings...), err
}

// modeChange is the mode property's sync against a path whose mode is current
// (mode.rb:141-190): the change it reports, and false when the path is in
// sync. A failing X fails the change whether or not the path is otherwise in
// sync (measured). The computation runs under noop
// too, as insync? does (read, not measured: resource_harness.rb:129 calls
// safe_insync? before :137 checks noop).
//
// On a directory an octal mode is masked before the comparison, so the event's
// desired value and its message carry the masked mode (mode.rb:154-159;
// measured: "mode changed '0700' to '0750'" for mode => '0640'). A symbolic
// event's desired value is the mode as written, and its message names it.
func modeChange(fsys fileSystem, path string, noop bool, m declaredMode, current uint32, isDir bool) (Change, bool) {
	was := fmt.Sprintf("%04o", current)
	want, err := m.apply(current, isDir)
	if err != nil {
		return Change{Property: "mode", Was: was, Is: m.raw, IsDisplay: m.failureDisplay(), Err: err}, true
	}
	is := m.desired()
	if isDir && m.octal {
		want = dirmask(want)
		is = fmt.Sprintf("%04o", want)
	}
	if want == current {
		return Change{}, false
	}
	ch := Change{Property: "mode", Was: was, Is: is,
		Message: fmt.Sprintf("mode changed '%s' to '%04o'", was, want)}
	if !m.octal {
		ch.Message += " (" + m.raw + ")"
	}
	if !noop {
		if err := fsys.Chmod(path, bitsToFileMode(want)); err != nil {
			ch.Err = fmt.Errorf("chmod %s: %w", path, err)
		}
	}
	return ch, true
}

// retrieved is what mode, owner and group compare against: before when the
// caller read it before writing, else path read now as links says. nil with no
// error means the path does not exist.
func retrieved(fsys fileSystem, path string, follow bool, before os.FileInfo) (os.FileInfo, error) {
	if before != nil {
		return before, nil
	}
	fi, err := statFor(fsys, path, follow)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	return fi, nil
}

// bitsToFileMode maps a traditional octal mode (e.g. 0o4755) onto
// os.FileMode: its top three bits become os.ModeSetuid, os.ModeSetgid and
// os.ModeSticky, which os.Chmod honours on Unix (fix #4): `& os.ModePerm`
// alone drops those bits from both the wanted and current mode, so mode =>
// '4755' silently applied 0755 and was stably idempotent there, and mode =>
// '1777' on a directory silently became 0777 -- a security downgrade letting
// any user delete another user's files in that directory.
func bitsToFileMode(n uint32) os.FileMode {
	mode := os.FileMode(n) & os.ModePerm
	if n&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if n&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if n&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

// modeBits converts fm's permission and special bits back to the single
// traditional octal number (e.g. 0o4755) a manifest would spell -- the
// inverse of bitsToFileMode, and the current mode a symbolic mode is
// computed from. os.FileMode's own bit layout for ModeSetuid/ModeSetgid/
// ModeSticky does not land on octal digits, so formatting fm directly with
// %o would not print anything a Puppet user would recognise.
func modeBits(fm os.FileMode) uint32 {
	n := uint32(fm.Perm())
	if fm&os.ModeSetuid != 0 {
		n |= 0o4000
	}
	if fm&os.ModeSetgid != 0 {
		n |= 0o2000
	}
	if fm&os.ModeSticky != 0 {
		n |= 0o1000
	}
	return n
}

// ownershipNotRootWarning is type/file/owner.rb:36's warnonce text, verbatim.
// The transaction compares this string to dedupe it and the report differential
// compares it to Ruby's, so the wording is a value rather than a description.
const ownershipNotRootWarning = "Cannot manage ownership unless running as root"

// applyOwnership reconciles owner and group as two independent properties.
//
// Two chowns, not one. provider/file/posix.rb sets them separately -- owner=
// is File.chown(should, nil, path) at :108 and group= is
// File.chown(nil, should, path) at :140 -- each with its own rescue and its
// own message. A single combined os.Chown fails both properties as one unit,
// which cannot be attributed to either, and hides the case where one succeeds
// and the other does not. os.Chown passes -1 through to the chown(2) uid/gid
// argument, which POSIX defines as "leave this id unchanged".
//
// Current ownership is read BEFORE the names are resolved, because attributing
// a resolution failure needs the value it was changing FROM. Puppet gets this
// for free: current_values is retrieved before sync_if_needed runs, so
// owner.insync?'s "Could not find user" raise (type/file/owner.rb:29) lands in
// the rescue with current_value already in hand.
//
// owner and group are NOT symmetric about privilege, and the asymmetry is
// Puppet's rather than an oversight here: type/file/owner.rb:35 suppresses an
// out-of-sync owner when the process is not root and warns instead, while
// type/file/group.rb's insync? (:23) is `@should.include?(current)` and nothing
// else, so an unprivileged group change is attempted and fails. Both are
// reproduced below.
//
// Was/Is stay the raw uid/gid -- that is what Puppet's create_change_event
// puts in previous_value/desired_value (measured: previous_value 0,
// desired_value 65534 for an owner change to "nobody"). WasDisplay/IsDisplay
// and the message text are the only things that render names;
// type/file/owner.rb and group.rb override is_to_s/should_to_s, not the
// event's stored value.
//
// Under links => manage the chown is lchown, so a link's own ownership changes
// and its target's never does (provider/file/posix.rb:99-144); under follow it
// goes through the link. before is as applyMetadata's.
func applyOwnership(fsys fileSystem, path string, r catalog.Resource, noop bool, before os.FileInfo) ([]Change, []Warning, error) {
	rawOwner, hasOwner := r.Parameters["owner"]
	rawGroup, hasGroup := r.Parameters["group"]
	if !hasOwner && !hasGroup {
		return nil, nil, nil
	}

	// A marked owner or group is shown as [redacted] in the errors below (D-78:
	// Ruby leaks it).
	ownerMarked, groupMarked := isMarked(File{}, r, "owner"), isMarked(File{}, r, "group")

	follow := linksFollow(r)
	fi, err := retrieved(fsys, path, follow, before)
	if err != nil {
		// A stat that is not IsNotExist blames no property: the run could not
		// read the state it was about to reconcile.
		return nil, nil, err
	}
	curUID, curGID, err := currentOwnership(path, fi)
	if err != nil {
		return nil, nil, err
	}
	chown := fsys.Lchown
	if follow {
		chown = fsys.Chown
	}

	var changes []Change
	var warnings []Warning

	if hasOwner {
		wantName := fmt.Sprintf("%v", rawOwner)
		wantUID, lookupErr := lookupUID(wantName, ownerMarked)
		switch {
		case lookupErr != nil:
			// Is is the DECLARED name, not a resolved uid. Measured:
			// desired_value is the string "nosuchuser" because @should.map!
			// raised before replacing it. No IsDisplay is needed -- the raw
			// value is already the name, and the transaction quotes a string
			// when rendering it into the message.
			ch := Change{Property: "owner",
				Was: curUID, WasDisplay: uidName(curUID), Is: wantName}
			// Under noop this is NOT a failure. type/file/owner.rb:26 is
			// `elsif provider.resource.noop? then return false` -- insync?
			// reports "not in sync" instead of raising, so the resource is out
			// of sync with a noop event and the run still exits 0. Attributing
			// a failure here reported failed=true and exit 4 under
			// --detailed-exitcodes where Puppet reports neither.
			if noop {
				ch.Message = fmt.Sprintf("owner changed '%s' to '%s'",
					uidName(curUID), wantName)
			} else {
				ch.Err = lookupErr
			}
			changes = append(changes, ch)
		case wantUID != curUID && os.Getuid() != 0:
			// Out of sync, and this process cannot chown: report NOTHING and warn
			// instead. type/file/owner.rb:35, verbatim:
			//
			//	unless Puppet.features.root?
			//	  warnonce "Cannot manage ownership unless running as root"
			//	  return true
			//	end
			//
			// insync? answering true means the harness sees the property as in
			// sync: no event, no sync call, no chown. Measured 2026-09-18,
			// unprivileged, a file whose declared owner was out of sync: no owner
			// event in the report and one warning line. voxagent reported a failed
			// owner event, so an unprivileged run exited 4 where Puppet exits 0.
			//
			// This case sits AFTER lookupErr and BEFORE the chown case, and both
			// positions are Ruby's. @should.map! raises at :29, above the root
			// check, so an unresolvable name still fails unprivileged; and
			// `return true if @should.include?(current)` is at :33, also above it,
			// so an owner that already matches warns about nothing -- which is
			// what keeps a converged unprivileged host quiet. An in-sync owner
			// matches no case here and falls through to no change and no warning.
			//
			// Not gated on noop either. Measured 2026-09-19, unprivileged,
			// `puppet apply --noop --detailed-exitcodes` over an out-of-sync
			// owner: exit 0, one warning, and no owner notice -- identical output
			// to the same manifest applied for real. insync? runs ahead of
			// resource_harness.rb's noop branch, so the noop path never gets a
			// would-be change to report. Note that the two branches around this
			// one DO consult noop, so the absence here is deliberate.
			//
			// os.Getuid, not Geteuid: util/suidmanager.rb:52's root? is
			// `Process.uid == 0`, the REAL uid.
			//
			// type/file/owner.rb:36 reaches this through warnonce, which is
			// maybe_log keyed on the calling class -- the owner property -- and
			// the message. Puppet.warning underneath it means source "Puppet".
			warnings = append(warnings, Warning{
				Source:      "Puppet",
				Message:     ownershipNotRootWarning,
				DedupeClass: "Puppet::Type::File::Owner",
			})
		case wantUID != curUID:
			was, is := uidName(curUID), uidName(wantUID)
			ch := Change{Property: "owner", Was: curUID, Is: wantUID,
				WasDisplay: was, IsDisplay: is,
				Message: fmt.Sprintf("owner changed '%s' to '%s'", was, is)}
			if !noop {
				if cerr := chown(path, wantUID, -1); cerr != nil {
					// posix.rb:110's wording.
					ch.Err = fmt.Errorf("Failed to set owner to %s: %w", redactIf(ownerMarked, "'%d'", wantUID), cerr)
				}
			}
			changes = append(changes, ch)
		}
	}

	// group is reconciled whether or not owner succeeded. sync_if_needed's
	// rescue is commented "Execution will continue on StandardErrors", and
	// resource.properties.each keeps going.
	if hasGroup {
		wantName := fmt.Sprintf("%v", rawGroup)
		wantGID, lookupErr := lookupGID(wantName, groupMarked)
		switch {
		case lookupErr != nil:
			// See the owner case: type/file/group.rb:31 has the same
			// `elsif provider.resource.noop? then return false` escape, so an
			// unresolvable group under noop is out of sync rather than failed.
			ch := Change{Property: "group",
				Was: curGID, WasDisplay: gidName(curGID), Is: wantName}
			if noop {
				ch.Message = fmt.Sprintf("group changed '%s' to '%s'",
					gidName(curGID), wantName)
			} else {
				ch.Err = lookupErr
			}
			changes = append(changes, ch)
		case wantGID != curGID:
			was, is := gidName(curGID), gidName(wantGID)
			ch := Change{Property: "group", Was: curGID, Is: wantGID,
				WasDisplay: was, IsDisplay: is,
				Message: fmt.Sprintf("group changed '%s' to '%s'", was, is)}
			if !noop {
				if cerr := chown(path, -1, wantGID); cerr != nil {
					// posix.rb:142's wording.
					ch.Err = fmt.Errorf("Failed to set group to %s: %w", redactIf(groupMarked, "'%d'", wantGID), cerr)
				}
			}
			changes = append(changes, ch)
		}
	}

	return changes, warnings, nil
}

// uidName renders a uid the way Puppet's owner property renders one --
// type/file/owner.rb's is_to_s is `super(provider.uid2name(v) || v)`, so a uid
// with no passwd entry falls back to its decimal form.
func uidName(uid int) string {
	if uid < 0 {
		return "absent"
	}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// gidName is uidName for groups. type/file/group.rb overrides the same two
// methods with gid2name.
func gidName(gid int) string {
	if gid < 0 {
		return "absent"
	}
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
		return g.Name
	}
	return strconv.Itoa(gid)
}

func lookupUID(name string, redact bool) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		// type/file/owner.rb:29's wording, and the underlying lookup error is
		// deliberately not wrapped: Ruby raises a bare "Could not find user
		// nosuchuser", and this message reaches the report as the owner
		// event's failure detail, where the differential compares it. Go's
		// "user: unknown user nosuchuser" adds no fact the sentence does not
		// already state.
		return 0, fmt.Errorf("Could not find user %s", redactIf(redact, "%s", name))
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string, redact bool) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		// type/file/group.rb:34's wording. See lookupUID on why the underlying
		// error is not wrapped.
		return 0, fmt.Errorf("Could not find group %s", redactIf(redact, "%s", name))
	}
	return strconv.Atoi(g.Gid)
}

// FilePath is the filesystem path a File resource manages: its path parameter
// (the namevar, which a catalog carries only when it differs from the title,
// resource.rb:101-102) or else its title, cleaned as Puppet munges it
// (type/file.rb:66-73). Titles and refs stay as given.
func FilePath(r catalog.Resource) string {
	if p, ok := r.Parameters["path"].(string); ok {
		return filepath.Clean(p)
	}
	return filepath.Clean(r.Title)
}

// recursionChild reports whether r is a child File.Generate made, which is
// confined to its parent's directory. A File generated from any other type
// (an extension host's eval_generate) is an ordinary File, as in Puppet.
func recursionChild(r catalog.Resource) bool {
	return r.GeneratedFrom != nil && strings.EqualFold(r.GeneratedFrom.Type, "file")
}

func paramString(r catalog.Resource, name, fallback string) string {
	if v, ok := r.Parameters[name]; ok {
		return fmt.Sprintf("%v", v)
	}
	return fallback
}

// fileBoolParam accepts a Go bool or the strings "true", "false", "yes", "no",
// and returns the parsed value or an error with Puppet's wording.
// def is the value returned when the parameter is absent.
func fileBoolParam(r catalog.Resource, name string, def bool) (bool, error) {
	raw, ok := r.Parameters[name]
	if !ok {
		return def, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch v {
		case "true", "yes":
			return true, nil
		case "false", "no":
			return false, nil
		}
	}
	// Puppet prints the value's inspect (measured): a string is quoted, a
	// number is not ("Invalid value 1.").
	shown := fmt.Sprintf("%v", raw)
	if _, isString := raw.(string); isString {
		shown = `"` + shown + `"`
	}
	return false, fmt.Errorf("Parameter %s failed on %s: Invalid value %s. Valid values are true, false, yes, no.",
		name, r.Ref(), shown)
}

// ensureChangeMessage is the ensure event's message for a path that was was and is
// now is: a bare "created" when nothing was there, else Puppet's
// change_to_s for the transition.
func ensureChangeMessage(was, is string) string {
	if was == "absent" {
		return "created"
	}
	return fmt.Sprintf("ensure changed '%s' to '%s'", was, is)
}

// forceTrue returns the force parameter. Validation has already rejected bad
// values, so it ignores the error.
func forceTrue(r catalog.Resource) bool {
	val, _ := fileBoolParam(r, "force", false)
	return val
}

// replaceFalse returns true when the replace parameter is false.
// Validation has already rejected bad values, so it ignores the error.
func replaceFalse(r catalog.Resource) bool {
	val, _ := fileBoolParam(r, "replace", true)
	return !val
}

// checksum renders content the way Puppet's report does.
//
// Measured: Ruby reports "{sha256}<hex>" for a file's content, and sha256 is
// Puppet 8's default digest_algorithm. The prefix is part of the value, not
// decoration -- a consumer reads it to know which algorithm produced the digest.
func checksum(b []byte) string {
	return fmt.Sprintf("{sha256}%x", sha256.Sum256(b))
}

// content is the bytes a file must hold: a catalog content string, or a source
// file read at write time.
type content struct {
	// sum is the desired bytes' checksum: the content's own, or the one taken
	// when a source was resolved (type/file.rb:1085-1100).
	sum string
	// write copies the desired bytes to w and returns the checksum of what it
	// copied, which differs from sum when a source changed after it was
	// measured.
	write func(w io.Writer) (string, error)
	// staged is true for content from the catalog or a source: it is written
	// through a temp file and verified before it appears at the path, even
	// for a new file (type/file.rb:1104-1107).
	staged bool
	// validate, when set, is the validate_cmd the staged temp file must pass
	// before it is renamed over the path. Unstaged content is never validated
	// (type/file.rb:935, 962-964).
	validate *validation
}

// literalContent is content held in memory: what it writes is what it is.
// It is staged: Puppet writes any file with a content property through
// replace_file, empty content included (type/file.rb:1104-1107).
func literalContent(b []byte) content {
	sum := checksum(b)
	return content{sum: sum, staged: true, write: func(w io.Writer) (string, error) {
		_, err := w.Write(b)
		return sum, err
	}}
}

// checksumOf reads r to its end and renders its sha256 as checksum does,
// without holding the bytes.
func checksumOf(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return fmt.Sprintf("{sha256}%x", h.Sum(nil)), nil
}

// checksumMismatchError is type/file.rb:1076-1081's fail_if_checksum_is_wrong:
// the bytes written are not the bytes that were measured, so the write is
// discarded.
type checksumMismatchError struct{ written, desired string }

func (e *checksumMismatchError) Error() string {
	return fmt.Sprintf("File written to disk did not match desired checksum; discarding changes (%s vs %s)",
		e.written, e.desired)
}

// createFile writes a brand new file at path, at exactly mode.
//
// Content (c.staged) goes through writeViaTemp, as Puppet writes any file with
// a content or source property through replace_file even when it is new
// (type/file.rb:944-962, 1104-1107): a source that vanishes or changes after
// resolution, or a write that fails, must not leave an empty or wrong file at
// the path. The final rename replaces whatever appeared at path since the
// caller measured it absent, as replace_file does; it replaces a symlink
// rather than following it.
//
// A file with neither is opened O_EXCL, because the caller has just measured
// the path absent: a path that appeared in between is something this run never
// inspected, and creating through it would write somewhere the catalog was not
// reconciled against. Puppet writes it in place too (:964-966), so its errors
// name the path itself.
//
// Every error it returns is an *os.PathError, a checksumMismatchError or a
// source read error. A PathError reads "open <path>: permission denied" -- the
// operation and the path, which is what Ruby's errno text carries too -- so
// the caller hands it to wrapEnsureError unchanged rather than prefixing the
// path a second time.
func createFile(ctx context.Context, fsys fileSystem, path string, c content, mode os.FileMode) error {
	if c.staged {
		return writeViaTemp(ctx, fsys, path, c, mode, nil)
	}
	f, err := fsys.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := c.write(f)
	closeErr := f.Close()
	// The write's error is reported ahead of the close's: a failed write is the
	// cause, and closing a file whose write failed commonly fails too.
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	// OpenFile's mode argument passes through the umask, so force the exact bits
	// afterward: a declared mode => '0777' on a host with umask 022 would
	// otherwise silently end up 0755, and setuid, setgid, and sticky bits are not
	// reliably applied by O_CREATE's mode argument at all.
	return fsys.Chmod(path, mode)
}

// replaceFile writes content over an existing path atomically.
//
// Measured against Puppet 8.10.0, file_system/file_impl.rb replace_file: lstat
// the target for its uid, gid and mode, write a temp file in the SAME directory,
// flush, fsync, chown, chmod, then rename over the path.
//
// Same directory because rename is only atomic within a filesystem; a temp file
// in /tmp would become a copy, which is the thing being avoided.
//
// os.WriteFile truncates and rewrites in place, which was what this did before.
// Two consequences, both real for the class of file Puppet exists to manage: a
// reader can observe a half-written config, and a failure part way through
// leaves the file destroyed rather than unchanged. It was also a measured
// divergence -- in a container Ruby cannot manage /etc/hosts at all, because
// rename cannot cross a bind mount, while this silently succeeded.
//
// The existing uid and gid are carried across deliberately. The temp file
// belongs to whoever is running, so renaming it into place without the chown
// would silently reassign every managed file whose owner the catalog does not
// declare.
//
// The Lstat is deliberate under links => follow too, where path may be a link:
// util.rb:623-626 copies the stat of Puppet::FileSystem.lstat(file), so the
// new file takes the link's own mode and owner, not its target's (I-5). The
// rename replaces the link; nothing is written through it.
func replaceFile(ctx context.Context, fsys fileSystem, path string, c content) error {
	info, err := fsys.Lstat(path)
	if err != nil {
		return err
	}
	st, _ := info.Sys().(*syscall.Stat_t)
	return writeViaTemp(ctx, fsys, path, c, info.Mode().Perm(), st)
}

// writeViaTemp is the sequence replaceFile and createFile share: write c to a
// temp file in path's directory, verify its checksum, chown it to owner (nil
// for a new file, which stays the running user's), chmod it to exactly mode,
// run c's validate_cmd against it, then rename it over path. Any failure
// removes the temp file and leaves path as it was.
//
// A validated write names its temp file as Puppet does, because the validator
// is handed the path and every failure message carries it.
func writeViaTemp(ctx context.Context, fsys fileSystem, path string, c content, mode os.FileMode, owner *syscall.Stat_t) error {
	var tmp *os.File
	var tmpPath string
	var err error
	if c.validate != nil {
		tmp, tmpPath, err = c.validate.createTemp(fsys, path)
	} else {
		tmp, tmpPath, err = fsys.CreateTemp(filepath.Dir(path), filepath.Base(path)+".voxagent")
	}
	if err != nil {
		return err
	}

	// discard removes the temp file after a failure. Its own error is dropped on
	// purpose: the caller is already being handed the error that matters, and
	// replacing it with a cleanup failure would hide the cause.
	discard := func(cause error) error {
		_ = tmp.Close()
		_ = fsys.Remove(tmpPath)
		return cause
	}

	written, err := c.write(tmp)
	if err != nil {
		return discard(err)
	}
	// fsync before the rename, as Puppet does. Without it the rename can reach
	// the disk before the contents, so a crash leaves the file present, in place,
	// and empty.
	if err := tmp.Sync(); err != nil {
		return discard(err)
	}
	if err := tmp.Close(); err != nil {
		_ = fsys.Remove(tmpPath)
		return err
	}
	if written != c.sum {
		// type/file.rb:1076-1081, checked once the bytes are on disk, as
		// Puppet checks inside replace_file's block: the temp file goes and
		// the path keeps what it had.
		_ = fsys.Remove(tmpPath)
		return &checksumMismatchError{written: written, desired: c.sum}
	}

	if owner != nil {
		if err := fsys.Chown(tmpPath, int(owner.Uid), int(owner.Gid)); err != nil {
			_ = fsys.Remove(tmpPath)
			return err
		}
	}
	// The mode is set on the temp file rather than after the rename, so the
	// content is never visible at the path under the wrong permissions. Chmod
	// forces the exact bits: CreateTemp's 0600 is not the umask-filtered mode a
	// declared mode => '0777' needs, and setuid, setgid and sticky bits are not
	// reliably applied at creation.
	if err := fsys.Chmod(tmpPath, mode); err != nil {
		_ = fsys.Remove(tmpPath)
		return err
	}
	// The validator sees the file complete, closed and with the owner and mode
	// it will have (util.rb:621-660), so a check of its permissions -- visudo's
	// -- sees what the target would get.
	if c.validate != nil {
		if err := c.validate.run(ctx, tmpPath); err != nil {
			_ = fsys.Remove(tmpPath)
			return err
		}
	}
	if err := fsys.Rename(tmpPath, path); err != nil {
		_ = fsys.Remove(tmpPath)
		return err
	}
	return nil
}

// wrapEnsureError renders the message Puppet's property.rb:483 set produces
// when a value's setter raises something that is NOT a Puppet::Error.
//
// Read from property.rb:493, an ensure => file whose parent directory is
// missing: "Could not set 'file' on ensure: No such file or directory - ...
// (file: /etc/site.pp, line: 6)".
//
// This lives in the provider, not the transaction, because only the provider
// knows which layer raised. property.rb:490 re-raises a Puppet::Error
// untouched and wraps only other StandardErrors at :492 -- so an Errno from
// writing the file IS wrapped, while ensure => directory's own
// "Cannot create X; parent directory Y does not exist" (type/file/ensure.rb,
// a raise Puppet::Error) is NOT. A wrapper keyed on the property name cannot
// tell those apart.
func wrapEnsureError(value string, r catalog.Resource, err error) error {
	return fmt.Errorf("Could not set %s on ensure: %s%s",
		redactIf(isMarked(File{}, r, "ensure"), "'%s'", value), err, errorLocation(r.File, r.Line))
}

// errorLocation reproduces Puppet::Util::Errors.error_location's branches,
// including its empty result when neither file nor line is known.
//
// Read from util/errors.rb: an empty string counts as absent, so a catalog
// resource with no source metadata gets NO suffix -- not "(file: , line: 0)",
// which voxagent emitted. The leading space is error_location_with_space's,
// which error.rb:45 uses to compose an ExternalFileError's message and which
// adds the space only when the location itself is non-empty.
//
// The column branches are omitted: a resource status carries no column, so
// they are unreachable here.
func errorLocation(file string, line int) string {
	switch {
	case file != "" && line != 0:
		return fmt.Sprintf(" (file: %s, line: %d)", file, line)
	case line != 0:
		return fmt.Sprintf(" (line: %d)", line)
	case file != "":
		return fmt.Sprintf(" (file: %s)", file)
	default:
		return ""
	}
}
