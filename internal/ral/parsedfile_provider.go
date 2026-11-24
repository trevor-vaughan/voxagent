package ral

import (
	"context"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The run of an ensurable ParsedFile provider (host, sshkey,
// ssh_authorized_key), shared by their units: an engine piece because no unit
// may import another that does not require it, and a library may not import
// ral. pf: is Puppet's provider/parsedfile.rb; resource_harness.rb and
// property.rb are Puppet's transaction/resource_harness.rb and property.rb
// (openvox 8.34.0).

// ParsedFileRun is one run's state, held by the primed provider and mutated
// only by the walk goroutine (D-96). Records binds each resource's
// parsedfile.Resource Name to its record: the one Prefetch matched, else a
// fresh absent one (pf:464-471), which a create fills and Flush appends.
type ParsedFileRun struct {
	Run     *parsedfile.Run
	Records map[string]*parsedfile.Record
}

// PrefetchParsedFile starts a run of class and binds every resource of rs to
// its record (pf:224-255). An engine error is returned as it is.
func PrefetchParsedFile(ctx context.Context, class *parsedfile.Class, rs []parsedfile.Resource) (*ParsedFileRun, error) {
	st := &ParsedFileRun{Run: parsedfile.NewRun(class), Records: make(map[string]*parsedfile.Record, len(rs))}
	matched, err := st.Run.Prefetch(ctx, rs)
	if err != nil {
		return nil, err
	}
	for _, res := range rs {
		rec, ok := matched[res.Name()]
		if !ok {
			rec = &parsedfile.Record{Type: class.Name, Ensure: "absent", Fields: map[string]parsedfile.Value{}, Flags: map[string]bool{}}
		}
		st.Records[res.Name()] = rec
	}
	return st, nil
}

// ParsedFileEnsurable is what Apply needs of an ensurable ParsedFile type.
type ParsedFileEnsurable struct {
	// Name is the type name; the ensure events are <Name>_created and
	// <Name>_removed.
	Name string
	// Properties are the properties other than ensure, in sync order.
	Properties []string
	// Current is the property's retrieve from the record.
	Current func(rec *parsedfile.Record, prop string) parsedfile.Value
	// Change is the change of prop from is to should, nil when in sync, with
	// property.rb:205-219's message and no noop suffix.
	Change func(prop string, is, should parsedfile.Value) *Change
	// Store puts a should value where Current reads it (the setter of
	// mk_resource_methods).
	Store func(rec *parsedfile.Record, prop string, v parsedfile.Value)
}

// ParsedFileShould is one munged resource as Apply needs it.
type ParsedFileShould struct {
	// Resource gives the record's key (Name) and should(:target).
	Resource parsedfile.Resource
	// Ensure is should(:ensure): "present", "absent", or "" when not given.
	Ensure string
	// Should holds the should value of each property given.
	Should map[string]parsedfile.Value
	// Keys are the key attributes' resource values the flush fills into a
	// record that lacks them (pf:448-462); an unset parameter is left out.
	Keys map[string]parsedfile.Value
	// Flush, when set, is the provider's flush (ssh_authorized_key's,
	// kp:69-103), called instead of Run.Flush with the run and the record,
	// its key attributes filled in. Nil means Run.Flush with the resource's
	// target should.
	Flush func(ctx context.Context, run *parsedfile.Run, rec *parsedfile.Record) error
}

// Apply implements Provider.Apply for t (resource_harness.rb:84-99
// perform_changes): ensure first, and when it changes nothing else is synced;
// otherwise every out-of-sync property in sync order. Live, a resource with a
// change is then flushed (resource_harness.rb:25-28), and a flush failure is
// a FlushError after the changes. Under noop nothing is stored or written.
func (st *ParsedFileRun) Apply(ctx context.Context, t *ParsedFileEnsurable, s ParsedFileShould, noop bool) ([]Change, []Warning, error) {
	rec := st.Records[s.Resource.Name()]
	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	// exists? (pf:443-445).
	exists := rec.Ensure != "absent" && rec.Ensure != ""
	var changes []Change
	switch {
	case s.Ensure == "present" && !exists:
		changes = []Change{{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: t.Name + "_created"}}
		if !noop {
			// create (pf:426-435): every property's should is copied into the
			// record and the target marked.
			rec.Ensure = "present"
			for _, prop := range t.Properties {
				if should, ok := s.Should[prop]; ok {
					t.Store(rec, prop, should)
				}
			}
			st.markModified(rec, s.Resource)
		}
	case s.Ensure == "absent" && exists:
		changes = []Change{{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: t.Name + "_removed"}}
		if !noop {
			// destroy (pf:437-441) through the ensure setter.
			st.markModified(rec, s.Resource)
			rec.Ensure = "absent"
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		changes = st.syncProperties(t, s, rec, noop, suffix)
	}
	if noop || len(changes) == 0 {
		return changes, nil, nil
	}
	// flush (pf:448-462): the key attributes are filled in first.
	for attr, v := range s.Keys {
		if rec.Fields[attr].Kind == parsedfile.Nil {
			rec.Fields[attr] = v
		}
	}
	var err error
	if s.Flush != nil {
		err = s.Flush(ctx, st.Run, rec)
	} else {
		target, hasTarget := s.Resource.ShouldTarget()
		err = st.Run.Flush(ctx, rec, target, hasTarget)
	}
	if err != nil {
		return changes, nil, &FlushError{Err: err}
	}
	return changes, nil, nil
}

// syncProperties syncs each property with a should that is out of sync, in
// sync order; live, each setter marks the targets and then stores
// (mk_resource_methods).
func (st *ParsedFileRun) syncProperties(t *ParsedFileEnsurable, s ParsedFileShould, rec *parsedfile.Record, noop bool, suffix string) []Change {
	// Ruby retrieves every current value before the first sync
	// (resource_harness.rb:22, 315).
	currents := map[string]parsedfile.Value{}
	for _, prop := range t.Properties {
		currents[prop] = t.Current(rec, prop)
	}
	var changes []Change
	for _, prop := range t.Properties {
		should, ok := s.Should[prop]
		if !ok {
			continue
		}
		ch := t.Change(prop, currents[prop], should)
		if ch == nil {
			continue
		}
		ch.Message += suffix
		changes = append(changes, *ch)
		if noop {
			continue
		}
		st.markModified(rec, s.Resource)
		t.Store(rec, prop, should)
	}
	return changes
}

// markModified is mark_target_modified (pf:487-493) with the resource's
// target should.
func (st *ParsedFileRun) markModified(rec *parsedfile.Record, res parsedfile.Resource) {
	target, ok := res.ShouldTarget()
	st.Run.MarkModified(rec, target, ok)
}
