package ral

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
)

// File is the Tier 1 file provider, bounded to ensure, content, mode, owner,
// and group.
//
// Everything else in Puppet's file type -- source, recurse, purge, backup,
// checksum, links, target, SELinux contexts -- is absent, and absent means a
// pre-flight refusal rather than a silent omission. Puppet's file type is 2,691
// lines and most of that difficulty is exactly those attributes, so an
// unbounded file provider would be the whole spine.
//
// Two deliberate divergences from Puppet live here, both because the agent
// normally runs as root and both documented at their call sites:
//   - links is unimplemented, so every ensure of file/present/directory
//     refuses outright when the path is a symlink, rather than replacing it
//     the way Puppet's default links => manage would. See refuseSymlink.
//   - force is unimplemented, so ensure => absent refuses to remove a
//     directory, rather than silently deleting it. See applyAbsent.
type File struct{}

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
// The symlink target half of Puppet's rule is deliberately absent: `target` is
// outside this provider's bounded attribute set, and a symlinked path is
// refused outright (divergence D-4).
func (File) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	path := filepath.Clean(r.Title)
	// Puppet's `unless path.root?`. filepath.Dir("/") is "/", so without this
	// the root directory would find itself managed and autorequire itself.
	if filepath.Dir(path) == path {
		return nil
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		ref := "File[" + dir + "]"
		if managed(ref) {
			return []string{ref}
		}
		if parent := filepath.Dir(dir); parent == dir {
			// filepath.Dir("/") is "/" and filepath.Dir(".") is ".", so this is
			// the top of the ascent for both absolute and relative paths.
			return nil
		}
	}
}

// Attrs implements Provider.
func (File) Attrs() Attrs { return NewAttrs("ensure", "content", "mode", "owner", "group") }

// SupportsValue implements ValueBounded.
//
// Puppet's file ensure also accepts link, and reads any other string as a link
// target (type/file/ensure.rb). Neither is implemented, and refusing them at
// apply time would leave the rest of the catalog applied -- the README promises
// the opposite. So they are refused here, before anything runs.
func (File) SupportsValue(attr, value string) bool {
	if attr != "ensure" {
		return true
	}
	switch value {
	case "file", "present", "directory", "absent":
		return true
	}
	return false
}

// Validate implements Validating.
//
// Measured against Puppet 8.10.0: a relative path fails the whole run with
// "Could not run: Parameter path failed on File[rel/f1]: File paths must be
// fully qualified, not 'rel/f1'". Before this check voxagent wrote the file
// relative to its own working directory -- for an agent running as root, a
// write target chosen by wherever the process happened to start. The title is
// the path because the path parameter is refused at pre-flight.
//
// The message is Puppet's verbatim, capital letter included.
func (File) Validate(r catalog.Resource) error {
	if filepath.IsAbs(r.Title) {
		return nil
	}
	return fmt.Errorf("Parameter path failed on %s: File paths must be fully qualified, not '%s'", r.Ref(), r.Title)
}

// Apply implements Provider.
func (File) Apply(_ context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	path := r.Title
	ensure := paramString(r, "ensure", "file")

	switch ensure {
	case "absent":
		// No warnings from this branch: a removal reconciles no ownership, so
		// nothing on it can reach applyOwnership's root check.
		changes, err := applyAbsent(path, noop)
		return changes, nil, err
	case "file":
		return applyFile(path, r, noop, false)
	case "present":
		// "present" is looser than "file": measured against Puppet 8.10.0,
		// ensure => present over an existing path of ANY type -- including a
		// directory -- is a clean no-op, whereas ensure => file over the same
		// directory errors (fix #5). applyFile's tolerant flag is what
		// distinguishes the two.
		return applyFile(path, r, noop, true)
	case "directory":
		return applyDirectory(path, r, noop)
	default:
		return nil, nil, fmt.Errorf("ensure => %q is not implemented (file, present, directory, absent)", ensure)
	}
}

// Refresh implements Provider. A file has nothing to refresh.
func (File) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Prefetch implements Provider. A file reads its own state in Apply.
func (p File) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return p, nil }

// refuseSymlink guards every write, chmod, and chown this provider performs
// from being redirected by a planted symlink.
//
// Measured: with the pre-fix code, `file { link: ensure => file, content =>
// 'NEW' }` where link -> target wrote THROUGH the link -- target's content
// changed and the link stayed a link. Measured Puppet 8.10.0 (links defaults
// to manage): the symlink is replaced by a regular file and target is
// untouched. Since the agent normally runs as root, an unprivileged user who
// can plant a symlink at a managed path can redirect a root write, chmod, or
// chown anywhere on the system.
//
// links is outside the bounded attribute set, so this provider does not
// implement Puppet's replace behaviour. It refuses instead -- a deliberate
// divergence from Puppet, made because writing through the link is a
// privilege-escalation primitive and refusing is the safe failure mode.
func refuseSymlink(path string) error {
	lst, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat %s: %w", path, err)
	}
	if lst.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; links is not implemented", path)
	}
	return nil
}

func applyAbsent(path string, noop bool) ([]Change, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lstat %s: %w", path, err)
	}

	// Puppet's force parameter gates directory removal and defaults to false.
	// Measured, Puppet 8.10.0, on an empty directory: "Notice: Not removing
	// directory; use 'force' to override" -- the directory is still present
	// afterwards. (Puppet also logs a misleading ensure: removed event in the
	// same run; that is a Puppet defect and is deliberately NOT copied here.)
	//
	// force is outside the bounded attribute set, so this provider can never
	// legitimately be asked to remove a directory: refuse instead. This is a
	// deliberate divergence from Puppet -- Puppet no-ops, voxagent fails the
	// resource -- because silently leaving a directory the catalog said
	// should be absent, while reporting success, is worse than failing loudly.
	//
	// This check must use the Lstat result above, not a fresh Stat: a symlink
	// is never a directory as far as Lstat is concerned, so a symlink to a
	// directory still falls through to the ordinary removal below, which is
	// correct -- removing a symlink removes the link, not the target.
	if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory; force is not implemented", path)
	}

	// previous_value is the path's actual TYPE, not the constant "present".
	// Measured 2026-09-18: a regular file reports "file" and a symlink reports
	// "link", both with name "file_removed". Ruby gets this from the ensure
	// property's retrieve, which answers what the path currently is.
	//
	// A directory is unreachable here -- the force check above refuses it -- so
	// those two are the whole domain. Lstat, not Stat, is what makes the symlink
	// case answer "link" rather than the type of its target.
	was := "file"
	if fi.Mode()&os.ModeSymlink != 0 {
		was = "link"
	}
	change := Change{Property: "ensure", Was: was, Is: "absent",
		Name: "file_removed", Message: "removed"}
	if noop {
		return []Change{change}, nil
	}
	if err := os.Remove(path); err != nil {
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

// applyFile applies ensure => file (tolerant == false) or ensure => present
// (tolerant == true).
func applyFile(path string, r catalog.Resource, noop, tolerant bool) ([]Change, []Warning, error) {
	var changes []Change

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

	if err := refuseSymlink(path); err != nil {
		return changes, nil, err
	}

	// content must be a string. Measured: content of float64(1e21) wrote the
	// literal bytes "1e+21"; an explicit JSON null wrote the four bytes
	// "<nil>". Puppet refuses both rather than serialising whatever %v
	// produces, so voxagent does too (fix #8).
	want, hasContent := r.Parameters["content"]
	var wantBytes []byte
	if hasContent {
		s, ok := want.(string)
		if !ok {
			return changes, nil, fmt.Errorf("content is %T, not a string", want)
		}
		wantBytes = []byte(s)
	}

	existing, statErr := os.Stat(path)
	created := statErr != nil && os.IsNotExist(statErr)
	needsWrite := false
	// ensureIdx and contentIdx locate the Change a later failure has to be
	// attributed to, because the operation that can fail happens after the
	// Change describing it has been appended.
	ensureIdx, contentIdx := -1, -1
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return changes, nil, fmt.Errorf("stat %s: %w", path, statErr)
	case created:
		wantMode, err := resourceMode(r, 0o644)
		if err != nil {
			return changes, nil, err
		}
		// Puppet reports the CONTENT checksum on a file it created with managed
		// content -- type/file/content.rb change_to_s, whose absent branch is
		// "defined content as '...'". A bare "created" is what it says only when
		// no content is managed.
		ensureChange := Change{Property: "ensure", Was: "absent", Is: ensureValue,
			Name: "file_created", Message: "created"}
		if hasContent {
			ensureChange.Message = fmt.Sprintf("defined content as '%s'", checksum(wantBytes))
		}
		if !noop {
			if err := createFile(path, wantBytes, wantMode); err != nil {
				// Wrapped. The failure is an Errno from the write itself, and
				// type/file/ensure.rb declares :file and :present with blocks that
				// Puppet::Property.newvalue turns into set_file and set_present
				// methods (read property.rb:168-175), so property.rb:483 set takes
				// its `method` branch: :490 re-raises a Puppet::Error untouched and
				// :492 wraps everything else. Measured 2026-09-18: "change from
				// 'absent' to 'file' failed: Could not set 'file' on ensure:
				// Permission denied @ dir_s_mkdir - <tmp>.lock (file: <manifest>,
				// line: 1)".
				ensureChange.Err = wrapEnsureError(ensureValue, r, err)
			}
		}
		ensureIdx = len(changes)
		changes = append(changes, ensureChange)
	case existing.IsDir():
		if !tolerant {
			// Not replaced. Removing a directory the catalog did not ask to
			// remove would destroy data on the strength of a type mismatch.
			return changes, nil, fmt.Errorf("%s is a directory but ensure => file", path)
		}
		// ensure => present: measured, Puppet 8.10.0, an existing directory
		// satisfies present outright. content is a file-specific property and
		// is not reconciled against a directory; fall through to mode/owner/
		// group reconciliation only.
	case hasContent:
		current, err := os.ReadFile(path)
		if err != nil {
			return changes, nil, fmt.Errorf("read %s: %w", path, err)
		}
		if string(current) != string(wantBytes) {
			// Checksums, not content. Puppet prints them precisely so a report
			// is not a copy of every managed file -- previous_value and
			// desired_value travel to PuppetDB and into dashboards.
			contentIdx = len(changes)
			changes = append(changes, Change{Property: "content",
				Was: checksum(current), Is: checksum(wantBytes),
				Message: fmt.Sprintf("content changed '%s' to '%s'",
					checksum(current), checksum(wantBytes))})
			needsWrite = true
		}
	}

	if needsWrite && !noop {
		if err := replaceFile(path, wantBytes); err != nil {
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
		}
	}

	return recordMetadata(path, r, noop, ensureValue, changes, ensureIdx)
}

func applyDirectory(path string, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	var changes []Change

	if err := refuseSymlink(path); err != nil {
		return changes, nil, err
	}

	existing, statErr := os.Stat(path)
	created := statErr != nil && os.IsNotExist(statErr)
	ensureIdx := -1
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return changes, nil, fmt.Errorf("stat %s: %w", path, statErr)
	case created:
		wantMode, err := resourceMode(r, 0o755)
		if err != nil {
			return changes, nil, err
		}
		ensureChange := Change{Property: "ensure", Was: "absent", Is: "directory",
			Name: "directory_created", Message: "created"}
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
			if _, err := os.Stat(parent); err != nil {
				ensureChange.Err = fmt.Errorf(
					"Cannot create %s; parent directory %s does not exist", path, parent)
			} else if err := os.Mkdir(path, wantMode); err != nil {
				ensureChange.Err = wrapEnsureError("directory", r, err)
			} else if err := os.Chmod(path, wantMode); err != nil {
				// See the matching comment in createFile: Mkdir's mode argument
				// passes through the umask too.
				ensureChange.Err = wrapEnsureError("directory", r, err)
			}
		}
		ensureIdx = len(changes)
		changes = append(changes, ensureChange)
	case !existing.IsDir():
		return changes, nil, fmt.Errorf("%s exists and is not a directory but ensure => directory", path)
	}

	return recordMetadata(path, r, noop, "directory", changes, ensureIdx)
}

// resourceMode returns the mode this resource declares, or def when none is
// declared. Shared by applyFile's and applyDirectory's creation paths, which
// need the final mode before the path exists (fix #7: create at the target
// mode directly rather than writing at a hard-coded mode and chmod'ing after,
// which left a permission window and fabricated a mode-transition event from
// a value the path never actually had).
func resourceMode(r catalog.Resource, def os.FileMode) (os.FileMode, error) {
	raw, ok := r.Parameters["mode"]
	if !ok {
		return def, nil
	}
	return parseMode(raw)
}

// recordMetadata reconciles mode, owner and group and files the result the way
// Puppet's transaction files it, which is not the same on a path that already
// existed and a path this run created.
//
// On an existing path they are ordinary properties. resource_harness.rb:86 syncs
// ensure first, gets NO_ACTION because ensure is in sync, and :93 then syncs
// every property in turn through sync_if_needed -- so each records its own
// event.
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
func recordMetadata(path string, r catalog.Resource, noop bool, ensureValue string,
	changes []Change, ensureIdx int) ([]Change, []Warning, error) {
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

	metaChanges, metaWarnings, metaErr := applyMetadata(path, r, noop, created)
	if !created {
		return append(changes, metaChanges...), metaWarnings, metaErr
	}

	collapsed := firstChangeErr(metaChanges)
	if collapsed == nil {
		// property_fix has no rescue of its own, so a failure it could not
		// attribute to a property -- Ruby's `currentvalue = thing.retrieve`
		// raising -- reaches the same wrapper as one it could.
		collapsed = metaErr
	}
	if collapsed != nil {
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

// firstChangeErr returns the first attributed failure in a change list.
//
// It exists because property_fix's failures do not travel as an error return:
// applyMetadata reports them per-property, and the created path has to collapse
// them back into the single ensure failure Puppet reports.
func firstChangeErr(changes []Change) error {
	for _, ch := range changes {
		if ch.Err != nil {
			return ch.Err
		}
	}
	return nil
}

// applyMetadata reconciles mode, owner, and group against an existing path.
//
// created says whether the path did not exist before this Apply call. When
// true, applyFile/applyDirectory already created the path at its exact final
// mode (see resourceMode), so mode is not reconciled again here: doing so
// would mean stat'ing a path this run just created and reporting a
// manufactured "mode changed" transition from a value the path never had.
// Measured against Puppet 8.10.0: it emits exactly one event for a created
// file and never a mode transition for a path that did not exist (fix #7).
func applyMetadata(path string, r catalog.Resource, noop, created bool) ([]Change, []Warning, error) {
	var changes []Change

	if raw, ok := r.Parameters["mode"]; ok && !created {
		// parseMode and currentMode failures stay resource-level (bare error,
		// no Change) rather than landing on a mode Change's Err: neither one is
		// a property that was attempted and failed to sync -- parseMode never
		// got a value to compare, and currentMode never got the "was" side of a
		// comparison. Nothing moved, so nothing can be blamed. Compare os.Chmod
		// below, which DID attempt a sync and so is attributed to the Change.
		want, err := parseMode(raw)
		if err != nil {
			return changes, nil, err
		}
		current, err := currentMode(path)
		if err != nil {
			return changes, nil, err
		}
		if current != want {
			ch := Change{Property: "mode",
				Was:     fmt.Sprintf("%04o", modeBits(current)),
				Is:      fmt.Sprintf("%04o", modeBits(want)),
				Message: fmt.Sprintf("mode changed '%04o' to '%04o'", modeBits(current), modeBits(want))}
			if !noop {
				if err := os.Chmod(path, want); err != nil {
					ch.Err = fmt.Errorf("chmod %s: %w", path, err)
				}
			}
			changes = append(changes, ch)
		}
	}

	// Ownership is reconciled whether or not mode succeeded.
	// resource_harness.rb:147's rescue is commented "Execution will continue on
	// StandardErrors", and perform_changes' resource.properties.each moves to
	// the next property. applyOwnership reports a failed chown as a Change.Err
	// rather than an error return, so its own error return is resource-level
	// only -- a stat it could not read.
	ownerChanges, warnings, err := applyOwnership(path, r, noop)
	changes = append(changes, ownerChanges...)
	return changes, warnings, err
}

// currentMode reads the permission and special (setuid/setgid/sticky) bits
// of an existing path, or 0 when it does not exist.
func currentMode(path string) (os.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky), nil
}

// parseMode accepts a string mode, e.g. "0644" or "4755", and nothing else.
//
// Measured, compiling a real manifest against Puppet 8.10.0: `mode => 0644`
// (a bare integer) compiles to catalog JSON 420, and Puppet refuses with
// "The file mode specification must be a string, not 'Integer'" -- it does
// not reinterpret the decimal digits as octal. The pre-fix parseMode here did
// exactly that reinterpretation for some inputs (silently applying 0420) and
// errored on others (0755 -> 493, "not octal"), which is worse than either
// consistent behaviour: the same idiom either silently mis-applied or errored
// depending on the digits (fix #1). voxagent mirrors Puppet's refusal.
//
// The parsed value is bounded to 0o7777 and its top three bits are mapped
// onto os.ModeSetuid, os.ModeSetgid, and os.ModeSticky, which os.Chmod honours
// on Unix (fix #4): `& os.ModePerm` alone drops those bits from both the
// wanted and current mode, so mode => '4755' silently applied 0755 and was
// stably idempotent there, and mode => '1777' on a directory silently became
// 0777 -- a security downgrade letting any user delete another user's files
// in that directory.
func parseMode(raw any) (os.FileMode, error) {
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("the file mode specification must be a string, not %T", raw)
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal: %w", s, err)
	}
	if n > 0o7777 {
		return 0, fmt.Errorf("mode %q exceeds 07777", s)
	}
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
	return mode, nil
}

// modeBits converts fm's permission and special bits back to the single
// traditional octal number (e.g. 0o4755) a manifest would spell -- the
// inverse of the mapping parseMode performs. Used only for message
// formatting: os.FileMode's own bit layout for ModeSetuid/ModeSetgid/
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
func applyOwnership(path string, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	rawOwner, hasOwner := r.Parameters["owner"]
	rawGroup, hasGroup := r.Parameters["group"]
	if !hasOwner && !hasGroup {
		return nil, nil, nil
	}

	curUID, curGID, err := currentOwnership(path)
	if err != nil {
		// A stat that is not IsNotExist blames no property: the run could not
		// read the state it was about to reconcile.
		return nil, nil, err
	}

	var changes []Change
	var warnings []Warning

	if hasOwner {
		wantName := fmt.Sprintf("%v", rawOwner)
		wantUID, lookupErr := lookupUID(wantName)
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
				if cerr := os.Chown(path, wantUID, -1); cerr != nil {
					// posix.rb:110's wording.
					ch.Err = fmt.Errorf("Failed to set owner to '%d': %w", wantUID, cerr)
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
		wantGID, lookupErr := lookupGID(wantName)
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
				if cerr := os.Chown(path, -1, wantGID); cerr != nil {
					// posix.rb:142's wording.
					ch.Err = fmt.Errorf("Failed to set group to '%d': %w", wantGID, cerr)
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

func lookupUID(name string) (int, error) {
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
		return 0, fmt.Errorf("Could not find user %s", name)
	}
	return strconv.Atoi(u.Uid)
}

func lookupGID(name string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	g, err := user.LookupGroup(name)
	if err != nil {
		// type/file/group.rb:34's wording. See lookupUID on why the underlying
		// error is not wrapped.
		return 0, fmt.Errorf("Could not find group %s", name)
	}
	return strconv.Atoi(g.Gid)
}

func paramString(r catalog.Resource, name, fallback string) string {
	if v, ok := r.Parameters[name]; ok {
		return fmt.Sprintf("%v", v)
	}
	return fallback
}

// checksum renders content the way Puppet's report does.
//
// Measured: Ruby reports "{sha256}<hex>" for a file's content, and sha256 is
// Puppet 8's default digest_algorithm. The prefix is part of the value, not
// decoration -- a consumer reads it to know which algorithm produced the digest.
func checksum(b []byte) string {
	return fmt.Sprintf("{sha256}%x", sha256.Sum256(b))
}

// createFile writes a brand new file at path, at exactly mode.
//
// O_EXCL because the caller has just measured the path absent: a path that
// appeared in between is something this run never inspected, and creating
// through it would write somewhere the catalog was not reconciled against.
//
// Every error it returns is an *os.PathError, which already reads
// "open <path>: permission denied" -- the operation and the path, which is what
// Ruby's errno text carries too. The caller therefore hands it to
// wrapEnsureError unchanged rather than prefixing the path a second time.
func createFile(path string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(content)
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
	return os.Chmod(path, mode)
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
func replaceFile(path string, content []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".voxagent")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	// discard removes the temp file after a failure. Its own error is dropped on
	// purpose: the caller is already being handed the error that matters, and
	// replacing it with a cleanup failure would hide the cause.
	discard := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return cause
	}

	if _, err := tmp.Write(content); err != nil {
		return discard(err)
	}
	// fsync before the rename, as Puppet does. Without it the rename can reach
	// the disk before the contents, so a crash leaves the file present, in place,
	// and empty.
	if err := tmp.Sync(); err != nil {
		return discard(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(tmpPath, int(st.Uid), int(st.Gid)); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
	}
	// The mode is set on the temp file rather than after the rename, so the
	// content is never visible at the path under the wrong permissions.
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
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
	return fmt.Errorf("Could not set '%s' on ensure: %s%s",
		value, err, errorLocation(r.File, r.Line))
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
