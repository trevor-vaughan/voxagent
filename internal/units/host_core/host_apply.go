package hostcore

import (
	"context"
	"fmt"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The parsed provider's run (spec 6, 6.1). pf: is Puppet's
// provider/parsedfile.rb, list: and olist: property/list.rb and
// property/ordered_list.rb; resource_harness.rb and property.rb are Puppet's
// transaction/resource_harness.rb and property.rb (openvox 8.34.0).

// hostRun is one run's state, held by the primed provider and mutated only by
// the walk goroutine (D-96). records binds each resource name to its record:
// the one Prefetch matched, else a fresh absent one (pf:464-471), which a
// create fills and Flush appends.
type hostRun struct {
	run     *parsedfile.Run
	records map[string]*parsedfile.Record
}

// hostResource adapts a munged Host resource to parsedfile.Resource.
type hostResource struct{ p hostParams }

// Name implements parsedfile.Resource.
func (r hostResource) Name() string { return r.p.Name }

// Title implements parsedfile.Resource.
func (r hostResource) Title() string { return r.p.Title }

// ShouldTarget implements parsedfile.Resource: should(:target), which always
// has a value (the type's default).
func (r hostResource) ShouldTarget() (string, bool) { return r.p.shouldTarget() }

func (p hostParams) shouldTarget() (string, bool) {
	v, ok := p.Should["target"]
	return v.S, ok
}

// PrefetchAtTurn implements ral.TurnPrefetching: every provider of this slice
// prefetches at the first resource of its type the walk reaches (spec 0).
func (Host) PrefetchAtTurn() {}

// Prefetch implements ral.Provider (pf:224-255 through hostClass): every
// target is read through the flat filetype and each resource bound to its
// record. A read error is the returned error, which the transaction reports
// type-wide (spec 5 intro); host files never fail to parse (spec 5.1).
func (h Host) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", parsedProvider, h.Type())
	st := &hostRun{records: make(map[string]*parsedfile.Record, len(rs))}
	resources := make([]parsedfile.Resource, 0, len(rs))
	for _, r := range rs {
		p, err := parseHost(r, h.encoding)
		if err != nil {
			return h, err
		}
		resources = append(resources, hostResource{p})
	}
	class := hostClass(func(target string) (parsedfile.FileType, error) {
		return parsedfile.NewFlat(target, 0, h.backup, h.relabel, h.encoding)
	}, h.now)
	st.run = parsedfile.NewRun(class)
	matched, err := st.run.Prefetch(ctx, resources)
	if err != nil {
		return h, err
	}
	for _, res := range resources {
		rec, ok := matched[res.Name()]
		if !ok {
			rec = &parsedfile.Record{Type: parsedProvider, Ensure: "absent", Fields: map[string]parsedfile.Value{}, Flags: map[string]bool{}}
		}
		st.records[res.Name()] = rec
	}
	h.state = st
	return h, nil
}

// Apply implements ral.Provider (resource_harness.rb:84-99 perform_changes):
// ensure first, and when it changes nothing else is synced; otherwise every
// out-of-sync property in sync order. Live, a resource with a change is then
// flushed (resource_harness.rb:25-28), and a flush failure is a
// ral.FlushError after the changes. Under noop nothing is stored or written.
// A Sensitive value is passed raw: the transaction redacts the marked
// properties.
func (h Host) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	st := h.state
	p, err := parseHost(r, h.encoding)
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
		changes = []ral.Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "host_created"}}
		if !noop {
			h.create(p, rec)
		}
	case p.Ensure == "absent" && exists:
		changes = []ral.Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "host_removed"}}
		if !noop {
			// destroy (pf:437-441) through the ensure setter.
			h.markModified(p, rec)
			rec.Ensure = "absent"
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		changes = h.syncProperties(p, rec, noop, suffix)
	}
	if noop || len(changes) == 0 {
		return changes, nil, nil
	}
	// flush (pf:448-462): the name is the key attribute it fills in.
	if rec.Fields["name"].Kind == parsedfile.Nil {
		rec.Fields["name"] = parsedfile.Value{Kind: parsedfile.Str, S: p.Name}
	}
	target, hasTarget := p.shouldTarget()
	if err := st.run.Flush(ctx, rec, target, hasTarget); err != nil {
		return changes, nil, &ral.FlushError{Err: err}
	}
	return changes, nil, nil
}

// create is pf:426-435: every property's should is copied into the record
// (host_aliases as OrderedList#should gives it, joined) and the target marked.
// A missing ip is not checked here: the flush's to_line refuses it (spec 5.1).
func (h Host) create(p hostParams, rec *parsedfile.Record) {
	rec.Ensure = "present"
	for _, prop := range hostProperties[1:] {
		if should, ok := p.Should[prop]; ok {
			storeField(rec, prop, should)
		}
	}
	h.markModified(p, rec)
}

// markModified is mark_target_modified (pf:487-493) with the resource's
// target should.
func (h Host) markModified(p hostParams, rec *parsedfile.Record) {
	target, ok := p.shouldTarget()
	h.state.run.MarkModified(rec, target, ok)
}

// storeField puts a should value where the getters read it: target is the
// record's Target, host_aliases the joined string (olist:15-27 dearrayify),
// the rest are fields.
func storeField(rec *parsedfile.Record, prop string, v parsedfile.Value) {
	switch prop {
	case "target":
		rec.Target = v.S
	case "host_aliases":
		rec.Fields[prop] = parsedfile.Value{Kind: parsedfile.Str, S: strings.Join(v.L, " ")}
	default:
		rec.Fields[prop] = v
	}
}

// current is the property's retrieve: target is the record's Target;
// host_aliases is List#retrieve (list:47-55), the record's string split on
// ' ' (Ruby's awk split: ASCII whitespace runs, leading and trailing dropped)
// or :absent; the rest are the field, or :absent (mk_resource_methods).
func current(rec *parsedfile.Record, prop string) parsedfile.Value {
	v := rec.Fields[prop]
	switch {
	case prop == "target":
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}
	case v.Kind == parsedfile.Nil || v.Kind == parsedfile.Absent:
		return parsedfile.Value{Kind: parsedfile.Absent}
	case prop == "host_aliases" && v.Kind == parsedfile.Str:
		return parsedfile.Value{Kind: parsedfile.List, L: strings.FieldsFunc(v.S, func(r rune) bool {
			return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r'
		})}
	}
	return v
}

// syncProperties syncs each property with a should that is out of sync, in
// sync order, with property.rb:205-219's messages; live, each setter marks
// the targets and then stores (mk_resource_methods).
func (h Host) syncProperties(p hostParams, rec *parsedfile.Record, noop bool, suffix string) []ral.Change {
	// Ruby retrieves every current value before the first sync
	// (resource_harness.rb:22, 315).
	currents := map[string]parsedfile.Value{}
	for _, prop := range hostProperties[1:] {
		currents[prop] = current(rec, prop)
	}
	var changes []ral.Change
	for _, prop := range hostProperties[1:] {
		should, ok := p.Should[prop]
		if !ok {
			continue
		}
		is := currents[prop]
		ch := propertyChange(prop, is, should)
		if ch == nil {
			continue
		}
		ch.Message += suffix
		changes = append(changes, *ch)
		if noop {
			continue
		}
		h.markModified(p, rec)
		storeField(rec, prop, should)
	}
	return changes
}

// propertyChange is the change of prop from is to should, nil when in sync.
// host_aliases (list:11-67, olist:15-27, inclusive membership): should is the
// list joined with ' ', in sync when the current list (:absent as []) joined
// equals it; is_to_s joins the current list bare, should_to_s quotes the
// joined string, and the event carries the list and the string. The others
// compare strings, :absent never in sync (property.rb property_matches?), and
// display through PuppetQuote (format_value_for_display). A should is never
// :absent (hostParams), so property.rb's `undefined` form cannot occur.
func propertyChange(prop string, is, should parsedfile.Value) *ral.Change {
	var was, want any
	var isShow, shouldShow string
	if prop == "host_aliases" {
		joined := strings.Join(should.L, " ")
		if strings.Join(is.L, " ") == joined {
			return nil
		}
		was, want = "absent", joined
		isShow, shouldShow = "absent", rubyvalue.PuppetQuote(joined)
		if is.Kind == parsedfile.List {
			was, isShow = append([]string{}, is.L...), strings.Join(is.L, " ")
		}
	} else {
		if is.Kind == parsedfile.Str && is.S == should.S {
			return nil
		}
		was, want = "absent", should.S
		isShow, shouldShow = "absent", rubyvalue.PuppetQuote(should.S)
		if is.Kind == parsedfile.Str {
			was, isShow = is.S, rubyvalue.PuppetQuote(is.S)
		}
	}
	ch := &ral.Change{Property: prop, Was: was, Is: want, WasDisplay: isShow, IsDisplay: shouldShow, DisplayVerbatim: true}
	if is.Kind == parsedfile.Absent {
		ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shouldShow)
	} else {
		ch.Message = fmt.Sprintf("%s changed %s to %s", prop, isShow, shouldShow)
	}
	return ch
}
