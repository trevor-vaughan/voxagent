package hostcore

import (
	"context"
	"fmt"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// The parsed provider's run (spec 6, 6.1). pf: is Puppet's
// provider/parsedfile.rb, list: and olist: property/list.rb and
// property/ordered_list.rb; resource_harness.rb and property.rb are Puppet's
// transaction/resource_harness.rb and property.rb (openvox 8.34.0).

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
	st, err := ral.PrefetchParsedFile(ctx, class, resources)
	if err != nil {
		return h, err
	}
	h.state = st
	return h, nil
}

// hostEnsurable is the host type for ral.ParsedFileRun.Apply. Its create
// copies host_aliases as OrderedList#should gives it, joined (storeField), and
// does not check a missing ip: the flush's to_line refuses it (spec 5.1).
var hostEnsurable = ral.ParsedFileEnsurable{
	Name:       "host",
	Properties: hostProperties[1:],
	Current:    current,
	Change:     propertyChange,
	Store:      storeField,
}

// Apply implements ral.Provider through ral.ParsedFileRun.Apply; the name is
// the key attribute the flush fills in (pf:448-462). A Sensitive value is
// passed raw: the transaction redacts the marked properties.
func (h Host) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseHost(r, h.encoding)
	if err != nil {
		return nil, nil, err
	}
	return h.state.Apply(ctx, &hostEnsurable, ral.ParsedFileShould{
		Resource: hostResource{p},
		Ensure:   p.Ensure,
		Should:   p.Should,
		Keys:     map[string]parsedfile.Value{"name": {Kind: parsedfile.Str, S: p.Name}},
	}, noop)
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
