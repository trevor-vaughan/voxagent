package parsedfile

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/voxagent/voxagent/internal/runlog"
)

// pf: Puppet 8.33.0 lib/puppet/provider/parsedfile.rb. Ruby keeps this state
// on the provider class, so it lives for the whole agent process; a Run holds
// it for one run only (D-96). Two consequences, both unreachable in a run:
// Ruby re-reads every target any earlier prefetch touched (pf:388), and its
// `record?` lookup (pf:315-318) finds the records of an earlier prefetch. The
// consumer binds a resource that matched no record to a fresh
// &Record{Type: Class.Name, Ensure: "absent"} (pf:464-471).

// Resource is what the engine needs of a catalog resource. A consumer unit
// adapts its own resource type.
type Resource interface {
	Name() string                 // namevar value (Ruby resource[:name])
	Title() string                // Ruby's matchers.delete key (pf:249)
	ShouldTarget() (string, bool) // should(:target) (pf:393)
}

// Class is the per-provider configuration Ruby keeps in class-level state and
// class methods subclasses override.
type Class struct {
	Name, TypeName string   // provider and type names: "crontab", "cron"
	Grammar        *Grammar // must hold a record type named Name (new records get Type = Name)
	DefaultTarget  string   // a real target even when "" (Ruby's '' is truthy)
	// FileType returns the filetype of a target (pf:360-371's target_object). A
	// ReadError it returns is remembered for the target, as one from Read is.
	FileType func(target string) (FileType, error)
	// Header is the text prepended to a generated file (pf:118-122); nil means
	// that method's three lines.
	Header func(now time.Time) string
	// NativeHeader matches a vendor header; the first match is dropped from the
	// generated text (pf:412-424 with drop_native_header true).
	NativeHeader *regexp.Regexp
	// PrefetchHook is pf:307's prefetch_hook, run per target on its records
	// after the engine has set OnDisk, Target and Ensure. An error aborts the
	// whole prefetch and is returned as it is.
	PrefetchHook func(target string, rs []*Record) ([]*Record, error)
	// ResourceForRecord replaces pf:265-270's lookup of the resource named like
	// the record; byName is keyed by Resource.Name. Nil means that lookup.
	ResourceForRecord func(rec *Record, byName map[string]Resource) (Resource, bool)
	// Adopt is Ruby's provider-level `match` (pf:246-252), tried for a record
	// ResourceForRecord did not claim. matchers holds the resources not yet
	// adopted, keyed by Name (Ruby's resources.dup, pf:239); order lists those
	// keys in catalog order. A returned resource is removed by its Title
	// (pf:249), which only removes it when title and name agree. A nil
	// resource is a miss.
	Adopt func(rec *Record, matchers map[string]Resource, order []string) (Resource, bool)
	// Targets extends the list of targets to read (pf:380-401, cron's :303). It
	// takes the context because cron logs while it scans its spool directory; an
	// error aborts the prefetch (cron's Dir.foreach raises) and is returned as it is.
	Targets func(ctx context.Context, base []string) ([]string, error)
	// ToFile post-processes the generated text, after the header (cron's TZ line).
	ToFile func(text string) string
	// Now is the clock for Header; nil means time.Now.
	Now func() time.Time
}

// Run is one run's ParsedFile state: Ruby's @records, @target_objects,
// @failed_prefetch_targets, @modified and @backup_stats.
type Run struct {
	c        *Class
	records  []*Record
	files    map[string]FileType
	failed   map[string]string // target -> remembered ReadError text (pf:292)
	modified map[string]bool
	backedUp map[string]bool // reset with each Prefetch (pf:91-99)
}

// NewRun starts a run with no records and nothing modified.
func NewRun(c *Class) *Run {
	return &Run{c: c, files: map[string]FileType{}, failed: map[string]string{}, modified: map[string]bool{}, backedUp: map[string]bool{}}
}

// targets is pf:380-401: the default target, then each resource's should
// target, first seen wins (Ruby's uniq; "" survives its compact), then the
// class's extension.
func (r *Run) targets(ctx context.Context, rs []Resource) ([]string, error) {
	targets := []string{r.c.DefaultTarget}
	for _, res := range rs {
		if t, ok := res.ShouldTarget(); ok && !slices.Contains(targets, t) {
			targets = append(targets, t)
		}
	}
	if r.c.Targets != nil {
		return r.c.Targets(ctx, targets)
	}
	return targets, nil
}

// fileType is target_object (pf:360-371): one filetype per target per run. A
// failing factory is not cached, as an exception from Ruby's `new` is not.
func (r *Run) fileType(target string) (FileType, error) {
	if ft, ok := r.files[target]; ok {
		return ft, nil
	}
	ft, err := r.c.FileType(target)
	if err != nil {
		return nil, err
	}
	r.files[target] = ft
	return ft, nil
}

// readTarget is prefetch_target and retrieve (pf:281-312, 323-342). A
// ReadError, from the filetype or its factory, is remembered and the target
// reads as empty (raise_prefetch_errors true); the filetype has already logged
// every other error it returns, and a ParseError is the caller's to report.
func (r *Run) readTarget(ctx context.Context, target string) ([]*Record, error) {
	var recs []*Record
	text, err := r.readText(ctx, target)
	var readErr ReadError
	switch {
	case errors.As(err, &readErr):
		r.failed[target] = readErr.Msg
	case err != nil:
		return nil, err
	case text != "":
		if recs, err = r.c.Grammar.Parse(text); err != nil {
			var pe *ParseError
			if errors.As(err, &pe) {
				pe.File = target
			}
			return nil, err
		}
	}
	for _, rec := range recs {
		rec.OnDisk, rec.Target, rec.Ensure = true, target, "present"
	}
	if r.c.PrefetchHook != nil {
		return r.c.PrefetchHook(target, recs)
	}
	return recs, nil
}

func (r *Run) readText(ctx context.Context, target string) (string, error) {
	ft, err := r.fileType(target)
	if err != nil {
		return "", err
	}
	return ft.Read(ctx)
}

// isText is skip_record? (pf:346-348): text records are never matched or listed.
func (r *Run) isText(rec *Record) bool {
	lt, ok := r.c.Grammar.byName[rec.Type]
	return ok && lt.Text
}

// Prefetch reads every target and binds records to rs (pf:224-255):
// the returned map is resource name to the record it matched. The run's
// records are replaced only when every target read; a parse error or any
// error other than a ReadError aborts and is returned.
func (r *Run) Prefetch(ctx context.Context, rs []Resource) (map[string]*Record, error) {
	var records []*Record
	targets, err := r.targets(ctx, rs)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		recs, err := r.readTarget(ctx, target)
		if err != nil {
			return nil, err
		}
		records = append(records, recs...)
	}
	r.records = records
	r.backedUp = map[string]bool{}

	// Ruby keys matchers by name (resources.dup, pf:239) but deletes by title
	// (pf:249), so a resource whose name differs from its title is never
	// removed and may be adopted again, by a later record.
	byName := make(map[string]Resource, len(rs))
	matchers := make(map[string]Resource, len(rs))
	order := make([]string, 0, len(rs))
	for _, res := range rs {
		if _, dup := byName[res.Name()]; !dup {
			order = append(order, res.Name())
		}
		byName[res.Name()] = res
		matchers[res.Name()] = res
	}
	matched := map[string]*Record{}
	for _, rec := range records {
		if r.isText(rec) {
			continue
		}
		if res, ok := r.resourceForRecord(rec, byName); ok {
			matched[res.Name()] = rec
			continue
		}
		if r.c.Adopt == nil {
			continue
		}
		remaining := slices.DeleteFunc(slices.Clone(order), func(name string) bool { _, ok := matchers[name]; return !ok })
		if res, ok := r.c.Adopt(rec, matchers, remaining); ok && res != nil {
			delete(matchers, res.Title())
			rec.Fields["name"] = Value{Kind: Str, S: res.Name()}
			matched[res.Name()] = rec
		}
	}
	return matched, nil
}

// resourceForRecord is pf:265-270, unless the class overrides it.
func (r *Run) resourceForRecord(rec *Record, byName map[string]Resource) (Resource, bool) {
	if r.c.ResourceForRecord != nil {
		return r.c.ResourceForRecord(rec, byName)
	}
	name := rec.Fields["name"]
	if name.Kind != Str {
		return nil, false
	}
	res, ok := byName[name.S]
	return res, ok
}

// Instances reads the class's targets without touching the run's records and
// returns the non-text records (pf:172-178).
func (r *Run) Instances(ctx context.Context) ([]*Record, error) {
	var out []*Record
	targets, err := r.targets(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		recs, err := r.readTarget(ctx, target)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			if !r.isText(rec) {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// targetSet reports whether rec has a target. Ruby tells nil from the empty string (truthy)
// and the Go field cannot, so "" counts as set only for a record read from
// disk: a record that was never read gets its target at Flush. The two differ
// only for a record not on disk whose Ruby target is the empty string while its should
// target differs or is absent. Ruby's create (pf:426-433) copies should(:target)
// into the record, and cron's target always has a should value (a default), so
// the stored empty string equals the should value and both readings give the same
// result: the case is not observable for cron.
func targetSet(rec *Record) bool { return rec.Target != "" || rec.OnDisk }

// MarkModified is mark_target_modified (pf:487-493): it marks shouldTarget
// when that differs from the record's target, and the record's target when it
// has one. Setters call it before storing a value.
func (r *Run) MarkModified(rec *Record, shouldTarget string, hasShould bool) {
	if hasShould && !(targetSet(rec) && shouldTarget == rec.Target) {
		r.modified[shouldTarget] = true
	}
	if targetSet(rec) {
		r.modified[rec.Target] = true
	}
}

// Flush is the provider's flush (pf:448-462) and the class's (pf:64-87): a
// record with no target takes shouldTarget or the default and marks it; one
// not yet on disk joins the run's records at the end and marks its target.
// Then every modified target is written, in sorted order. The attempted
// targets leave the modified set even on failure; the first failure stops the
// writing and is returned, and later targets stay marked.
func (r *Run) Flush(ctx context.Context, rec *Record, shouldTarget string, hasShould bool) error {
	if !targetSet(rec) {
		rec.Target = r.c.DefaultTarget
		if hasShould {
			rec.Target = shouldTarget
		}
		r.modified[rec.Target] = true
	}
	if !rec.OnDisk {
		rec.OnDisk = true
		r.records = append(r.records, rec)
		r.modified[rec.Target] = true
	}
	targets := make([]string, 0, len(r.modified))
	for t := range r.modified {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, target := range targets {
		runlog.Debugf(ctx, "Puppet", "Flushing %s provider target %s", r.c.TypeName, target)
		delete(r.modified, target)
		if err := r.flushTarget(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

// flushTarget is pf:101-114.
func (r *Run) flushTarget(ctx context.Context, target string) error {
	if reason, ok := r.failed[target]; ok {
		return fmt.Errorf("Failed to read %s's records when prefetching them. Reason: %s", target, reason)
	}
	if err := r.BackupTarget(ctx, target); err != nil {
		return err
	}
	var keep []*Record
	for _, rec := range r.records {
		if rec.Target == target && rec.Ensure != "absent" {
			keep = append(keep, rec)
		}
	}
	text, err := r.toFile(keep)
	if err != nil {
		return err
	}
	ft, err := r.fileType(target)
	if err != nil {
		return err
	}
	return ft.Write(ctx, text)
}

// toFile is pf:412-424 with drop_native_header true: the grammar's text, the
// first native header span removed, the header prepended, then Class.ToFile.
func (r *Run) toFile(records []*Record) (string, error) {
	text, err := r.c.Grammar.ToFile(records)
	if err != nil {
		return "", err
	}
	if r.c.NativeHeader != nil {
		if loc := r.c.NativeHeader.FindStringIndex(text); loc != nil {
			text = text[:loc[0]] + text[loc[1]:]
		}
	}
	now := time.Now
	if r.c.Now != nil {
		now = r.c.Now
	}
	if r.c.Header != nil {
		text = r.c.Header(now()) + text
	} else {
		text = defaultHeader(now()) + text
	}
	if r.c.ToFile != nil {
		text = r.c.ToFile(text)
	}
	return text, nil
}

// defaultHeader is pf:118-122; Ruby interpolates Time#to_s of a local time.
func defaultHeader(now time.Time) string {
	return "# HEADER: This file was autogenerated at " + now.Format("2006-01-02 15:04:05 -0700") + "\n" +
		"# HEADER: by puppet.  While it can still be managed manually, it\n" +
		"# HEADER: is definitely not recommended.\n"
}

// BackupTarget backs target up once per prefetch, and only when its filetype
// can (pf:91-99). A failed backup is not marked done.
func (r *Run) BackupTarget(ctx context.Context, target string) error {
	ft, err := r.fileType(target)
	if err != nil {
		return err
	}
	b, ok := ft.(Backuper)
	if !ok || r.backedUp[target] {
		return nil
	}
	if err := b.Backup(ctx); err != nil {
		return err
	}
	r.backedUp[target] = true
	return nil
}
