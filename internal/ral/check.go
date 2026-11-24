package ral

import (
	"slices"
	"sort"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// ResourceGaps is Preflight's judgement of one resource against one provider:
// every reason p cannot apply r, sorted. A nil p means r's type has no
// provider, which is the one gap reported for it.
//
// Preflight judges the catalog with it before anything is applied, and the
// walk re-judges a resource a generator changed before the resource is
// visited, so the two can never disagree about what is implemented.
func ResourceGaps(p Provider, r catalog.Resource) []Gap {
	if strings.EqualFold(r.Type, Filebucket) {
		return []Gap{{Ref: r.Ref(), Type: r.Type,
			Reason: "native File's backup does not implement named filebuckets"}}
	}
	if p == nil {
		// The type is the single actionable fact. Listing its attributes
		// as further gaps would inflate the porting histogram by attribute
		// count and bury the one thing an operator can act on.
		return []Gap{{Ref: r.Ref(), Type: r.Type}}
	}
	var gaps []Gap
	implemented := p.Attrs()
	// A provider that does not bound its values leaves them to apply time.
	// Resolve the assertion once per resource rather than once per
	// attribute.
	bounded, boundsValues := p.(ValueBounded)
	// The extension host decodes the raw catalog itself, so an Evaluator's
	// resources keep their rich values; a native provider can take none.
	rich := map[string][]string{}
	if _, evaluated := p.(Evaluator); !evaluated {
		for _, rv := range catalog.RichValues(r) {
			rich[rv.Param] = append(rich[rv.Param], rv.PType)
		}
	}
	var listed Attrs
	if lv, ok := p.(ListValued); ok {
		listed = lv.ListAttrs()
	}
	names := make([]string, 0, len(r.Parameters))
	for name := range r.Parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if IsEngineAttr(name) {
			continue
		}
		// Puppet's default level; concat copies it onto every File it makes.
		if name == "loglevel" && r.Parameters[name] == "notice" {
			continue
		}
		if _, ok := implemented[name]; !ok {
			// The attribute has no code at all, so its value is not a
			// separate fact to report. Reporting both would double-count
			// one attribute in the porting histogram this list feeds, and
			// would point an operator at a value of an attribute that does
			// not exist yet.
			gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name})
			continue
		}
		if kinds := rich[name]; len(kinds) > 0 {
			// A rich value is not a string to bound; the kind is the fact.
			for _, kind := range kinds {
				gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name, Rich: kind})
			}
			continue
		}
		if !boundsValues {
			continue
		}
		_, list := listed[name]
		for _, value := range valuesToBound(r.Parameters[name], list) {
			if bounded.SupportsValue(name, value) {
				continue
			}
			gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name, Value: value,
				Marked: slices.Contains(r.SensitiveParameters, name)})
		}
	}
	sortGaps(gaps)
	return gaps
}

// sortGaps orders gaps by ref, attribute, value and rich kind, so the
// message a refusing run prints is identical between runs on the same
// catalog.
func sortGaps(gaps []Gap) {
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Ref != gaps[j].Ref {
			return gaps[i].Ref < gaps[j].Ref
		}
		if gaps[i].Attribute != gaps[j].Attribute {
			return gaps[i].Attribute < gaps[j].Attribute
		}
		if gaps[i].Value != gaps[j].Value {
			return gaps[i].Value < gaps[j].Value
		}
		return gaps[i].Rich < gaps[j].Rich
	})
}

// DeferredGaps is one gap per parameter that holds a Deferred value, at any
// depth, on a resource that is not a container, sorted. voxagent does not
// resolve Deferred values (decision D-51), and the extension host would
// resolve one on loading the catalog -- Puppet's resolver descends into
// Sensitive and every other value -- so this is judged before the host
// starts. Container parameters are never read, so a Deferred one is accepted.
func DeferredGaps(c *catalog.Catalog) []Gap {
	var gaps []Gap
	for _, r := range c.Resources {
		if IsContainerResource(r) {
			continue
		}
		for name, v := range r.Parameters {
			if holdsDeferred(v) {
				gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name, Rich: "Deferred"})
			}
		}
	}
	sortGaps(gaps)
	return gaps
}

// holdsDeferred reports whether v is, or holds at any depth, a Deferred map.
// Unlike catalog.RichValues it descends into rich maps, a Sensitive's
// __pvalue, a Hash's pairs and an Object's init hash included.
func holdsDeferred(v any) bool {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			if holdsDeferred(e) {
				return true
			}
		}
	case map[string]any:
		if v["__ptype"] == "Deferred" {
			return true
		}
		for _, e := range v {
			if holdsDeferred(e) {
				return true
			}
		}
	}
	return false
}

// MetaparamGaps judges r only by the metaparameters the walk would have to
// honour and does not: every name in metaparams other than an engine
// attribute and Puppet's default loglevel => notice. It is the whole judgement
// for a resource another implementation validates (MetaparamJudging).
func MetaparamGaps(metaparams Attrs, r catalog.Resource) []Gap {
	var gaps []Gap
	for name, v := range r.Parameters {
		if _, meta := metaparams[name]; !meta || IsEngineAttr(name) {
			continue
		}
		if name == "loglevel" && v == "notice" {
			continue
		}
		gaps = append(gaps, Gap{Ref: r.Ref(), Type: r.Type, Attribute: name})
	}
	sortGaps(gaps)
	return gaps
}
