package sshkeyscore

import (
	"context"
	"fmt"
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The parsed provider's run (spec 6, 6.2). pf: is Puppet's
// provider/parsedfile.rb; resource_harness.rb and property.rb are Puppet's
// transaction/resource_harness.rb and property.rb (openvox 8.34.0); type:<n> is
// puppet/type/sshkey.rb.

// newFileMode is the mode of a new known_hosts file (sp:27-29), less the umask
// the flat filetype applies.
const newFileMode = 0o644

// sshkeyResource adapts a munged Sshkey resource to parsedfile.Resource. Its
// name is the title: Ruby's Type#name is "<name>@<type>" for this type
// (type:11-15), which is the name Ruby's prefetch keys its resources by and
// so the key of the run's records.
type sshkeyResource struct{ p sshkeyParams }

// Name implements parsedfile.Resource.
func (r sshkeyResource) Name() string { return r.p.Title }

// Title implements parsedfile.Resource.
func (r sshkeyResource) Title() string { return r.p.Title }

// ShouldTarget implements parsedfile.Resource: should(:target), which always
// has a value (the type's default).
func (r sshkeyResource) ShouldTarget() (string, bool) { return r.p.shouldTarget() }

func (p sshkeyParams) shouldTarget() (string, bool) {
	v, ok := p.Should["target"]
	return v.S, ok
}

// PrefetchAtTurn implements ral.TurnPrefetching: every provider of this slice
// prefetches at the first resource of its type the walk reaches (spec 0).
func (Sshkey) PrefetchAtTurn() {}

// Prefetch implements ral.Provider (pf:224-255 through sshkeyClass): every
// target is read through the flat filetype and each resource bound to its
// record. A read or parse error (a line with leading whitespace, spec 5.2) is
// the returned error, which the transaction reports type-wide (spec 5 intro).
func (s Sshkey) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", parsedProvider, s.Type())
	resources := make([]parsedfile.Resource, 0, len(rs))
	for _, r := range rs {
		p, err := parseSshkey(r, s.release, s.encoding)
		if err != nil {
			return s, err
		}
		resources = append(resources, sshkeyResource{p})
	}
	class := sshkeyClass(func(target string) (parsedfile.FileType, error) {
		return parsedfile.NewFlat(target, newFileMode, s.backup, s.relabel, s.encoding)
	}, s.now)
	st, err := ral.PrefetchParsedFile(ctx, class, resources)
	if err != nil {
		return s, err
	}
	s.state = st
	return s, nil
}

// sshkeyEnsurable is the sshkey type for ral.ParsedFileRun.Apply. Its create
// copies host_aliases whole, as the property's should gives it (storeField),
// and does not check a missing type: the flush's join refuses it (spec 6.2).
var sshkeyEnsurable = ral.ParsedFileEnsurable{
	Name:       "sshkey",
	Properties: sshkeyProperties[1:],
	Current:    current,
	Change:     propertyChange,
	Store:      storeField,
}

// Apply implements ral.Provider through ral.ParsedFileRun.Apply; name and type
// are the key attributes the flush fills in (pf:448-462). A resource without a
// type leaves it nil, which the line's join refuses. A Sensitive value is
// passed raw: the transaction redacts the marked properties.
func (s Sshkey) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSshkey(r, s.release, s.encoding)
	if err != nil {
		return nil, nil, err
	}
	keys := map[string]parsedfile.Value{"name": {Kind: parsedfile.Str, S: p.Name}}
	if p.Type != "" {
		keys["type"] = parsedfile.Value{Kind: parsedfile.Str, S: p.Type}
	}
	return s.state.Apply(ctx, &sshkeyEnsurable, ral.ParsedFileShould{Resource: sshkeyResource{p}, Ensure: p.Ensure, Should: p.Should, Keys: keys}, noop)
}

// storeField puts a should value where the getters read it: target is the
// record's Target, a host_aliases list a private copy, key a field.
func storeField(rec *parsedfile.Record, prop string, v parsedfile.Value) {
	switch prop {
	case "target":
		rec.Target = v.S
	case "host_aliases":
		rec.Fields[prop] = parsedfile.Value{Kind: parsedfile.List, L: slices.Clone(v.L)}
	default:
		rec.Fields[prop] = v
	}
}

// current is the property's retrieve: target is the record's Target, the rest
// are the field or :absent (mk_resource_methods).
func current(rec *parsedfile.Record, prop string) parsedfile.Value {
	if prop == "target" {
		return parsedfile.Value{Kind: parsedfile.Str, S: rec.Target}
	}
	if v := rec.Fields[prop]; v.Kind != parsedfile.Nil {
		return v
	}
	return parsedfile.Value{Kind: parsedfile.Absent}
}

// propertyChange is the change of prop from is to should, nil when in sync,
// with property.rb:205-219's messages.
func propertyChange(prop string, is, should parsedfile.Value) *ral.Change {
	if inSync(prop, is, should) {
		return nil
	}
	isShow, shouldShow := is.Display(), should.Display()
	ch := &ral.Change{Property: prop, Was: is.ReportValue(), Is: should.ReportValue(),
		WasDisplay: isShow, IsDisplay: shouldShow, DisplayVerbatim: true}
	if is.Kind == parsedfile.Absent {
		ch.Message = fmt.Sprintf("defined '%s' as %s", prop, shouldShow)
	} else {
		ch.Message = fmt.Sprintf("%s changed %s to %s", prop, isShow, shouldShow)
	}
	return ch
}

// inSync is the property's insync?: host_aliases is the whole list, equal
// element by element and in order (type:74-76); key and target are string
// equality, :absent never in sync (property.rb property_matches?).
func inSync(prop string, is, should parsedfile.Value) bool {
	if prop == "host_aliases" {
		return is.Kind == parsedfile.List && slices.Equal(is.L, should.L)
	}
	return is.Kind == parsedfile.Str && is.S == should.S
}
