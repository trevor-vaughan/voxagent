package mountcore

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The parsed provider's run (spec 5.4, 6.4). mount's ensure is a plain
// property with its own state machine (mt:26-124), not ensurable, so only the
// record binding of ral.PrefetchParsedFile is shared. pf: is Puppet's
// provider/parsedfile.rb; resource_harness.rb and property.rb are Puppet's
// transaction/resource_harness.rb and property.rb (openvox 8.34.0).

// newMount is the provider for one run, on the local time, which the header's
// offset shows (M4).
func newMount(release, encoding, mountPath, umountPath string, run command.ShadowRunner, backup, relabel func(ctx context.Context, path string) error) Mount {
	return Mount{
		release: release, encoding: encoding, mountPath: mountPath, umountPath: umountPath,
		run: run, backup: backup, relabel: relabel, now: time.Now,
	}
}

// mountResource adapts a munged Mount resource to parsedfile.Resource.
type mountResource struct{ p mountParams }

// Name implements parsedfile.Resource: the munged name.
func (r mountResource) Name() string { return r.p.Name }

// Title implements parsedfile.Resource.
func (r mountResource) Title() string { return r.p.Title }

// ShouldTarget implements parsedfile.Resource: should(:target), which always
// has a value (the type's default).
func (r mountResource) ShouldTarget() (string, bool) {
	v, ok := r.p.Should["target"]
	return v.S, ok
}

// PrefetchAtTurn implements ral.TurnPrefetching: every provider of this slice
// prefetches at the first resource of its type the walk reaches (spec 0).
func (Mount) PrefetchAtTurn() {}

// Prefetch implements ral.Provider over prefetch.
func (m Mount) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	return m.prefetch(ctx, rs)
}

// prefetch is mp:253-272: every target is read through the flat filetype and
// each resource bound to its record (pf:224-255 through mountClass), then the
// mount listing marks the resources of the listed names: absent becomes ghost
// (mounted, not in the fstab) and unmounted mounted. A read error, a failing
// listing and an unparseable listing line are the returned error, which the
// transaction reports type-wide (spec 5 intro, 5.4).
func (m Mount) prefetch(ctx context.Context, rs []catalog.Resource) (Mount, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", parsedProvider, m.Type())
	resources := make([]parsedfile.Resource, 0, len(rs))
	for _, r := range rs {
		p, err := parseMount(r, m.release, m.encoding)
		if err != nil {
			return m, err
		}
		resources = append(resources, mountResource{p})
	}
	class := mountClass(func(target string) (parsedfile.FileType, error) {
		return parsedfile.NewFlat(target, 0, m.backup, m.relabel, m.encoding)
	}, m.now)
	st, err := ral.PrefetchParsedFile(ctx, class, resources)
	if err != nil {
		return m, err
	}
	// The listing must be whole: a cut one would hide mounts, and the walk
	// would mount them again or leave them in place as unmounted.
	code, out, err := m.run.Run(ctx, command.ArgvOpts{FailOnTruncate: true}, m.mountPath)
	if err == nil && code != 0 {
		err = command.ExecFailure([]string{m.mountPath}, false, false, code, out)
	}
	if err != nil {
		return m, err
	}
	names, err := parseMountOutput(out)
	if err != nil {
		return m, err
	}
	for _, name := range names {
		rec, ok := st.Records[name]
		if !ok {
			continue
		}
		switch rec.Ensure {
		case "absent":
			rec.Ensure = "ghost"
		case "unmounted":
			rec.Ensure = "mounted"
		}
	}
	m.state = st
	return m, nil
}

// The listing's patterns (mp:285, 289). A line holds no "\n", so (?m) changes
// nothing; it keeps the catalog-pattern rule of spec 4.
var (
	listedName = regexp.MustCompile(`(?m) on (.*) type `)
	headerRule = regexp.MustCompile(`(?m)^[- \t]*$`)
)

// parseMountOutput is mountinstances (mp:274-303) on Linux: the output split
// as Ruby's split("\n") does; when the second of at least two lines is blank
// or dashes, the first two are a header and dropped; every other line gives
// the greedy capture of ` on (.*) type `, raw (mount lists a space as a space,
// M9). A line that does not match fails the whole listing. Ruby takes the
// capture when it is truthy, which an empty String is, so an empty name is a
// name.
func parseMountOutput(out string) ([]string, error) {
	lines := command.SplitOutput(out)
	if len(lines) >= 2 && headerRule.MatchString(lines[1]) {
		lines = lines[2:]
	}
	var names []string
	for _, line := range lines {
		match := listedName.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("Could not understand line %s from mount output", line)
		}
		names = append(names, match[1])
	}
	return names, nil
}

// syncedProperties are the properties other than ensure that can be out of
// sync, in sync order (spec 6.4). atboot and blockdevice are not Linux fields:
// their getter returns the should, so they are always in sync (spec 4.4).
var syncedProperties = []string{"device", "fstype", "options", "pass", "dump", "target"}

// Apply implements ral.Provider (resource_harness.rb:84-99 perform_changes):
// an out-of-sync ensure is synced alone through the state machine; otherwise,
// when the current ensure is not absent (a ghost is present), every
// out-of-sync property is synced in order. Live, a resource with a change is
// then flushed (resource_harness.rb:25-28), and a flush failure is a
// FlushError after the changes. A failure inside the ensure sync is the
// ensure change's Err, and nothing is flushed (the change does not count).
// Under noop nothing is stored, run or written.
func (m Mount) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseMount(r, m.release, m.encoding)
	if err != nil {
		return nil, nil, err
	}
	res := mountResource{p}
	rec := m.state.Records[p.Name]
	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	// The ensure getter: the record's ensure, :absent when it has none.
	is := rec.Ensure
	if is == "" {
		is = "absent"
	}
	var changes []ral.Change
	switch {
	case p.Ensure != "" && !ensureInSync(p.Ensure, is):
		ch := ensureChange(is, p.Ensure)
		ch.Message += suffix
		if noop {
			return []ral.Change{ch}, nil, nil
		}
		if err := m.syncEnsure(ctx, res, rec, is); err != nil {
			ch.Err = ensureError(r, p.Ensure, ral.IsMarked(m, r, "ensure"), err)
			return []ral.Change{ch}, nil, nil
		}
		changes = []ral.Change{ch}
	case is == "absent":
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		changes = outOfSync(p, rec)
		for i := range changes {
			changes[i].Message += suffix
			if !noop {
				m.store(res, rec, changes[i].Property)
			}
		}
	}
	if noop || len(changes) == 0 {
		return changes, nil, nil
	}
	if err := m.flush(ctx, res, rec); err != nil {
		return changes, nil, &ral.FlushError{Err: err}
	}
	return changes, nil, nil
}

// ensureError is property.rb:483-497's set around a failed value block: a
// Puppet::Error passes unwrapped (a command's ExecutionFailure, mp:217's
// fstype refusal); any other error, the engine's join refusal (fp:110-114) or
// a backup or write errno from a flush, becomes "Could not set '<v>' on
// ensure: <detail> (file: F, line: N)". The transaction's Puppet-sourced log
// of the change's error is then property.rb:496's log_exception line. pf's
// "Failed to read <t>'s records" Puppet::Error cannot reach here: a failed
// read aborts mount's whole prefetch. The value is [redacted] when ensure is
// marked Sensitive (ral.IsMarked).
func ensureError(r catalog.Resource, value string, marked bool, err error) error {
	var execFailure *command.ExecutionFailure
	var refusal *puppetError
	if errors.As(err, &execFailure) || errors.As(err, &refusal) {
		return err
	}
	return fmt.Errorf("Could not set %s on ensure: %s%s",
		rubyvalue.RedactIf(marked, "'%s'", value), err, rubyvalue.ErrorLocation(r.File, r.Line))
}

// ensureInSync is mt:99-105: defined is in sync with mounted and unmounted,
// otherwise the values must be equal.
func ensureInSync(should, is string) bool {
	if should == "defined" && (is == "mounted" || is == "unmounted") {
		return true
	}
	return should == is
}

// ensureChange is the ensure change from is to should: property.rb:205-219's
// message on the quoted symbols and prop:227-239's event, which is the
// value's event option (absent: mount_deleted, mounted: mount_mounted) or
// mount_changed. The value blocks' `return :mount_created` is discarded
// (spec 6.4).
func ensureChange(is, should string) ral.Change {
	name := "mount_changed"
	switch should {
	case "absent":
		name = "mount_deleted"
	case "mounted":
		name = "mount_mounted"
	}
	var msg string
	switch {
	case is == "absent":
		msg = fmt.Sprintf("defined 'ensure' as %s", rubyvalue.PuppetQuote(should))
	case should == "absent":
		msg = fmt.Sprintf("undefined 'ensure' from %s", rubyvalue.PuppetQuote(is))
	default:
		msg = fmt.Sprintf("ensure changed %s to %s", rubyvalue.PuppetQuote(is), rubyvalue.PuppetQuote(should))
	}
	return ral.Change{Property: "ensure", Was: is, Is: should, Message: msg, Name: name}
}

// syncEnsure is the ensure value blocks (mt:40-95) from is, which is not in
// sync. The unmounted block's else branch (mt:66-67) is unreachable: a record's
// ensure is absent, ghost, mounted or unmounted, and unmounted is in sync.
func (m Mount) syncEnsure(ctx context.Context, res mountResource, rec *parsedfile.Record, is string) error {
	switch res.p.Ensure {
	case "defined":
		m.create(res, rec)
	case "unmounted":
		switch is {
		case "ghost":
			m.create(res, rec)
			if err := m.flush(ctx, res, rec); err != nil {
				return err
			}
			return m.unmount(ctx, res, rec)
		case "absent":
			m.create(res, rec)
		case "mounted":
			if err := m.unmount(ctx, res, rec); err != nil {
				return err
			}
			return m.syncOthers(ctx, res, rec)
		}
	case "absent":
		if mounted(rec) {
			if err := m.unmount(ctx, res, rec); err != nil {
				return err
			}
		}
		if is != "ghost" {
			// destroy (pf:437-441) through the ensure setter.
			m.markModified(res, rec)
			rec.Ensure = "absent"
		}
	case "mounted":
		wasMounted := mounted(rec)
		if is == "absent" || is == "ghost" {
			m.create(res, rec)
		}
		if err := m.syncOthers(ctx, res, rec); err != nil {
			return err
		}
		if !wasMounted {
			rec.Flags["needs_mount"] = true
		}
	}
	return nil
}

// create is pf:426-435: every property's should is copied into the record,
// ensure's included, and the target marked. atboot and blockdevice are left
// out: not Linux fields, nothing reads or writes them (spec 4.4).
func (m Mount) create(res mountResource, rec *parsedfile.Record) {
	rec.Ensure = res.p.Ensure
	for _, prop := range syncedProperties {
		if v, ok := res.p.Should[prop]; ok {
			setField(rec, prop, v)
		}
	}
	m.markModified(res, rec)
}

// syncOthers is mt:107-123: every other property out of sync against a fresh
// retrieve is set with no Change, and the record flushed at once if any was.
func (m Mount) syncOthers(ctx context.Context, res mountResource, rec *parsedfile.Record) error {
	oos := outOfSync(res.p, rec)
	for _, ch := range oos {
		m.store(res, rec, ch.Property)
	}
	if len(oos) == 0 {
		return nil
	}
	return m.flush(ctx, res, rec)
}

// flush is mp:305-309 around pf:448-462: the needs_mount flag is taken off
// the record, the name filled in when the record lacks one, the run flushed,
// then the mount run when the flag was set.
func (m Mount) flush(ctx context.Context, res mountResource, rec *parsedfile.Record) error {
	needsMount := rec.Flags["needs_mount"]
	delete(rec.Flags, "needs_mount")
	if rec.Fields["name"].Kind == parsedfile.Nil {
		rec.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: res.p.Name}
	}
	target, ok := res.ShouldTarget()
	if err := m.state.Run.Flush(ctx, rec, target, ok); err != nil {
		return err
	}
	if needsMount {
		return m.mount(ctx, res, rec)
	}
	return nil
}

// mount is mm:7-24 on Linux: `mount <name>`, then the record's ensure follows
// without marking the target.
func (m Mount) mount(ctx context.Context, res mountResource, rec *parsedfile.Record) error {
	if err := m.command(ctx, m.mountPath, res.p.Name); err != nil {
		return err
	}
	switch rec.Ensure {
	case "absent":
		rec.Ensure = "ghost"
	case "unmounted":
		rec.Ensure = "mounted"
	}
	return nil
}

// unmount is mm:62-70: `umount <name>`, then the record's ensure follows
// without marking the target.
func (m Mount) unmount(ctx context.Context, res mountResource, rec *parsedfile.Record) error {
	if err := m.command(ctx, m.umountPath, res.p.Name); err != nil {
		return err
	}
	switch rec.Ensure {
	case "mounted":
		rec.Ensure = "unmounted"
	case "ghost":
		rec.Ensure = "absent"
	}
	return nil
}

// mounted is mm:73-75.
func mounted(rec *parsedfile.Record) bool { return rec.Ensure == "mounted" || rec.Ensure == "ghost" }

// Refresh implements ral.Provider over RefreshLogged.
func (m Mount) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	out, err := m.RefreshLogged(ctx, r, noop)
	return out.Refreshed, err
}

// RefreshLogged implements ral.RefreshLogging (mt:287-290): when the should
// fstype is not swap and the record is mounted at refresh, remount (mm:26-41
// on Linux): info Remounting at the provider's to_s, logged directly so it
// precedes the command's Executing line as in Ruby, then `mount -o remount
// <name>` with remounts true, else unmount then mount. Otherwise the refresh
// does nothing and still happened. A command's failure is the error, which
// the walk logs as "Failed to call refresh: <text>" and then <text>. Under
// noop nothing runs: Ruby never calls refresh then.
func (m Mount) RefreshLogged(ctx context.Context, r catalog.Resource, noop bool) (ral.RefreshOutcome, error) {
	if noop {
		return ral.RefreshOutcome{}, nil
	}
	p, err := parseMount(r, m.release, m.encoding)
	if err != nil {
		return ral.RefreshOutcome{}, err
	}
	res := mountResource{p}
	rec := m.state.Records[p.Name]
	if fstype, ok := p.Should["fstype"]; (ok && fstype.S == "swap") || !mounted(rec) {
		return ral.RefreshOutcome{Refreshed: true}, nil
	}
	runlog.From(ctx).Log("info", sources.SourcesFrom(ctx).Provider, "Remounting")
	if p.Remounts {
		err = m.command(ctx, m.mountPath, "-o", "remount", p.Name)
	} else if err = m.unmount(ctx, res, rec); err == nil {
		err = m.mount(ctx, res, rec)
	}
	if err != nil {
		return ral.RefreshOutcome{}, err
	}
	return ral.RefreshOutcome{Refreshed: true}, nil
}

// command runs path with args as Ruby's `commands` do (spec 6.4 Commands):
// argv only, no `--` added, so a name starting with - is an option to the
// tool as in Ruby (spec 8 item 17). A non-zero exit is the ExecutionFailure
// naming the resolved path (D-82).
func (m Mount) command(ctx context.Context, path string, args ...string) error {
	code, out, err := m.run.Run(ctx, command.ArgvOpts{}, path, args...)
	if err == nil && code != 0 {
		err = command.ExecFailure(append([]string{path}, args...), false, false, code, out)
	}
	return err
}

// markModified is mark_target_modified (pf:487-493) with the resource's
// target should.
func (m Mount) markModified(res mountResource, rec *parsedfile.Record) {
	target, ok := res.ShouldTarget()
	m.state.Run.MarkModified(rec, target, ok)
}

// store is a property setter (pf:181-202): the target is marked, then the
// should stored.
func (m Mount) store(res mountResource, rec *parsedfile.Record, prop string) {
	m.markModified(res, rec)
	setField(rec, prop, res.p.Should[prop])
}

// setField puts v where the getter reads prop: target is the record's Target,
// the rest are fields.
func setField(rec *parsedfile.Record, prop string, v parsedfile.Value) {
	if prop == "target" {
		rec.Target = v.S
		return
	}
	rec.Fields[prop] = v
}

// outOfSync is the change of every property with a should that is out of
// sync, in sync order, every current value retrieved first
// (resource_harness.rb:22, 315).
func outOfSync(p mountParams, rec *parsedfile.Record) []ral.Change {
	type current struct {
		v     parsedfile.Value
		field bool
	}
	currents := map[string]current{}
	for _, prop := range syncedProperties {
		v, field := getter(rec, prop)
		currents[prop] = current{v, field}
	}
	var changes []ral.Change
	for _, prop := range syncedProperties {
		should, ok := p.Should[prop]
		if !ok || !currents[prop].field {
			continue
		}
		if ch := propertyChange(prop, currents[prop].v, should, p.Integers[prop]); ch != nil {
			changes = append(changes, *ch)
		}
	}
	return changes
}

// getter is mk_resource_methods' getter (pf:181-202). A field is the record's
// value or :absent. target is not a field: it is the record's when set,
// otherwise the getter returns the should, which is always in sync (field
// false), as for a ghost's fresh record.
func getter(rec *parsedfile.Record, prop string) (v parsedfile.Value, field bool) {
	if prop == "target" {
		if rec.Target == "" {
			return parsedfile.Value{}, false
		}
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}, true
	}
	if v := rec.Fields[prop]; v.Kind == parsedfile.Str {
		return v, true
	}
	return parsedfile.Value{Kind: parsedfile.Absent}, true
}

// propertyChange is the change of prop from is to should, nil when in sync.
// property_matches? (prop:393-398) compares the current String with the
// should's to_s, so an Integer should is its decimal text; :absent is never in
// sync. Messages are property.rb:205-219's; a String displays through
// PuppetQuote, an Integer bare, :absent as absent, and an Integer is the
// event's desired value as a number (M12). A should is never :absent.
func propertyChange(prop string, is, should parsedfile.Value, integer bool) *ral.Change {
	if is.Kind == parsedfile.Str && is.S == should.S {
		return nil
	}
	var want any = should.S
	shouldShow := rubyvalue.PuppetQuote(should.S)
	if integer {
		if n, err := strconv.Atoi(should.S); err == nil {
			want, shouldShow = n, should.S
		}
	}
	ch := &ral.Change{Property: prop, Was: "absent", Is: want, WasDisplay: "absent", IsDisplay: shouldShow, DisplayVerbatim: true}
	if is.Kind == parsedfile.Absent {
		ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shouldShow)
		return ch
	}
	ch.Was, ch.WasDisplay = is.S, rubyvalue.PuppetQuote(is.S)
	ch.Message = fmt.Sprintf("%s changed %s to %s", prop, ch.WasDisplay, shouldShow)
	return ch
}
