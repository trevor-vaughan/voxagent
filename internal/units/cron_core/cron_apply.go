package croncore

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The crontab provider's run (spec 6). prov: is cron_core's
// provider/cron/crontab.rb, pf: Puppet 8.33.0's provider/parsedfile.rb, type:
// type/cron.rb; resource_harness.rb and property.rb are Puppet 8.33.0's
// transaction/resource_harness.rb and property.rb.

// cronRun is one run's state, held by the primed provider and mutated only by
// the walk goroutine (D-96). records binds each resource name to its record:
// the one Prefetch matched, else a fresh absent one (pf:464-471), which a
// create fills and Flush appends.
type cronRun struct {
	run           *parsedfile.Run
	records       map[string]*parsedfile.Record
	targetDefault func() (string, error)
}

// PrefetchAtTurn implements ral.TurnPrefetching: Puppet prefetches cron at the
// first Cron the walk reaches (spec 0 "Prefetch timing", M11).
func (Cron) PrefetchAtTurn() {}

// Prefetch implements ral.Provider (pf:224-255 through the class of
// cron_grammar.go): every target is read and each resource bound to its
// record. The provider's default target is $USER when set, even empty, else
// root (prov:4), read once here. Each target's filetype is built here, so a
// user created earlier in the run has a uid (M11). A parse error, a hook or
// spool error and a failed type target default are the returned error, which
// the transaction reports type-wide (spec 6.4).
func (c Cron) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", crontabProvider, c.Type())
	st := &cronRun{records: make(map[string]*parsedfile.Record, len(rs)), targetDefault: c.typeTargetDefault()}
	resources := make([]parsedfile.Resource, 0, len(rs))
	for _, r := range rs {
		p, err := parseCron(r, c.release, c.encoding, st.targetDefault)
		if err != nil {
			return c, err
		}
		resources = append(resources, cronResource{p})
	}
	providerTarget := "root"
	if user, set := c.environ("USER"); set {
		providerTarget = user
	}
	class := cronClass(c.release, providerTarget, c.spoolDir, func(target string) (parsedfile.FileType, error) {
		return parsedfile.NewCrontab(ctx, target, c.crontab, c.run, c.ns, c.stderr, c.encoding)
	}, c.now)
	st.run = parsedfile.NewRun(class)
	matched, err := st.run.Prefetch(ctx, resources)
	if err != nil {
		return c, err
	}
	for _, res := range resources {
		rec, ok := matched[res.Name()]
		if !ok {
			rec = &parsedfile.Record{Type: crontabProvider, Ensure: "absent", Fields: map[string]parsedfile.Value{}, Flags: map[string]bool{}}
		}
		st.records[res.Name()] = rec
	}
	c.state = st
	return c, nil
}

// typeTargetDefault is the type's target default when neither user nor target
// is given (type:395-409): the invoker's passwd name, looked up once per run
// and only when a resource needs it. Ruby's Etc.getpwuid raises ArgumentError
// for a uid with no entry; voxagent fails the prefetch with that text, and
// with the lookup's error, which Etc cannot tell from a missing entry (spec
// 5.1). An entry always has a name, so Ruby's `|| 'root'` is unreachable.
func (c Cron) typeTargetDefault() func() (string, error) {
	var name string
	var err error
	looked := false
	return func() (string, error) {
		if !looked {
			looked = true
			uid := os.Getuid()
			pw, found, lookupErr := c.ns.UserByID(int64(uid))
			switch {
			case lookupErr != nil:
				err = lookupErr
			case !found:
				err = fmt.Errorf("can't find user for %d", uid)
			default:
				name = pw.Name
			}
		}
		return name, err
	}
}

// Apply implements ral.Provider (resource_harness.rb:84-99 perform_changes):
// ensure first, and when it changes nothing else is synced; otherwise every
// out-of-sync property in type order. Live, a resource with a change is then
// flushed (resource_harness.rb:25-28), and a flush failure is a
// ral.FlushError after the changes. Under noop nothing is stored or written.
// Sensitive values are passed raw: the transaction redacts the marked
// properties (Properties declares them).
func (c Cron) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	st := c.state
	p, err := parseCron(r, c.release, c.encoding, st.targetDefault)
	if err != nil {
		return nil, nil, err
	}
	rec := st.records[p.Name]
	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	// exists? (pf:443-445).
	exists := rec.Ensure != "absent" && rec.Ensure != ""
	var changes []ral.Change
	switch {
	case p.Ensure == "present" && !exists:
		changes = []ral.Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "cron_created"}}
		if !noop {
			c.create(ctx, p, rec)
		}
	case p.Ensure == "absent" && exists:
		changes = []ral.Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "cron_removed"}}
		if !noop {
			// destroy (pf:437-441) through the ensure setter.
			c.markModified(p, rec)
			rec.Ensure = "absent"
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		changes = c.syncProperties(p, rec, noop, suffix)
	}
	if noop || len(changes) == 0 {
		return changes, nil, nil
	}
	// flush (pf:448-462): the name is the key attribute it fills in.
	if rec.Fields["name"].Kind == parsedfile.Nil {
		rec.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: p.Name}
	}
	target, hasTarget := p.shouldText("target")
	if err := st.run.Flush(ctx, rec, target, hasTarget); err != nil {
		return changes, nil, &ral.FlushError{Err: err}
	}
	return changes, nil, nil
}

// create is prov:100-106 around pf:426-435: with a command should (the empty
// string counts), every property's should is copied into the record and the
// target marked; without one, only the err line, and the record stays absent.
func (c Cron) create(ctx context.Context, p cronParams, rec *parsedfile.Record) {
	if _, ok := p.Should["command"]; !ok {
		runlog.From(ctx).Log("err", sources.SourcesFrom(ctx).Resource, "no command specified, cannot create")
		return
	}
	rec.Ensure = "present"
	for _, prop := range cronProperties[1:] {
		if should, ok := p.Should[prop]; ok {
			storeField(rec, prop, should)
		}
	}
	c.markModified(p, rec)
}

// markModified is mark_target_modified (pf:487-493) with the resource's
// target should.
func (c Cron) markModified(p cronParams, rec *parsedfile.Record) {
	target, ok := p.shouldText("target")
	c.state.run.MarkModified(rec, target, ok)
}

// storeField puts a should value where the getters read it: target is the
// record's Target, the rest are fields.
func storeField(rec *parsedfile.Record, prop string, v parsedfile.Value) {
	if prop == "target" {
		rec.Target = v.S
		return
	}
	v.L = slices.Clone(v.L)
	rec.Fields[prop] = v
}

// current is the provider getter for prop (mk_resource_methods: the field, or
// :absent): user reads user, else target (prov:262-264); target is the
// record's Target.
func current(rec *parsedfile.Record, prop string) parsedfile.Value {
	switch prop {
	case "target":
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}
	case "user":
		if u := rec.Fields["user"]; u.Kind == parsedfile.Str {
			return u
		}
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}
	}
	if v := rec.Fields[prop]; v.Kind != parsedfile.Nil {
		return v
	}
	return parsedfile.Value{Kind: parsedfile.Absent}
}

// syncProperties syncs each property with a should that is out of sync, in
// type order (type:57-410), with property.rb:205-219's messages; live, each
// setter marks the targets and then stores (mk_resource_methods), and user=
// also moves the record to the user's crontab (prov:253-260).
func (c Cron) syncProperties(p cronParams, rec *parsedfile.Record, noop bool, suffix string) []ral.Change {
	// Ruby retrieves every current value before the first sync
	// (resource_harness.rb:22, 315), so target is judged by what was read even
	// after user= has moved the record.
	currents := map[string]parsedfile.Value{}
	for _, prop := range cronProperties[1:] {
		currents[prop] = current(rec, prop)
	}
	var changes []ral.Change
	for _, prop := range cronProperties[1:] {
		should, ok := p.Should[prop]
		if !ok {
			continue
		}
		is := currents[prop]
		if inSync(prop, is, should) {
			continue
		}
		isShow, shouldShow := display(is), display(should)
		if prop == "environment" {
			shouldShow = strings.Join(should.L, ",") // type:340-346
		}
		ch := ral.Change{Property: prop, Was: reportValue(is), Is: reportValue(should),
			WasDisplay: isShow, IsDisplay: shouldShow, DisplayVerbatim: true}
		switch {
		case is.Kind == parsedfile.Absent:
			ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shouldShow)
		case should.Kind == parsedfile.Absent:
			ch.Message = fmt.Sprintf("undefined '%s' from %s", prop, isShow)
		default:
			ch.Message = fmt.Sprintf("%s changed %s to %s", prop, isShow, shouldShow)
		}
		ch.Message += suffix
		changes = append(changes, ch)
		if noop {
			continue
		}
		c.markModified(p, rec)
		storeField(rec, prop, should)
		if prop == "user" {
			rec.Target = should.S
		}
	}
	return changes
}

// inSync is spec 4.4. The time fields and command compare their displays
// (type:68-70); special compares its first should (Property#insync?);
// environment compares sorted lists, and a current :absent never matches
// (type:330-336); user and target compare strings (property.rb:393-398).
func inSync(prop string, is, should parsedfile.Value) bool {
	switch prop {
	case "special":
		return is.Kind == should.Kind && is.S == should.S
	case "environment":
		return is.Kind == parsedfile.List && slices.Equal(slices.Sorted(slices.Values(is.L)), slices.Sorted(slices.Values(should.L)))
	case "user", "target":
		return is.S == should.S
	}
	return display(is) == display(should)
}

// display is format_value_for_display (%#p, spec 6.1, M2): a String through
// PuppetQuote, a list as its quoted elements in brackets, :absent bare. The
// CronParam rule that shows a list's first element when it is a Symbol
// (type:110-130) is the Absent kind here, and command is never a list.
func display(v parsedfile.Value) string {
	switch v.Kind {
	case parsedfile.Str:
		return rubyvalue.PuppetQuote(v.S)
	case parsedfile.List:
		quoted := make([]string, len(v.L))
		for i, s := range v.L {
			quoted[i] = rubyvalue.PuppetQuote(s)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	return "absent"
}

// reportValue is an event's previous or desired value: a string, a list
// (never nil, so an empty one is [] in the report) or "absent".
func reportValue(v parsedfile.Value) any {
	switch v.Kind {
	case parsedfile.Str:
		return v.S
	case parsedfile.List:
		return append([]string{}, v.L...)
	}
	return "absent"
}
