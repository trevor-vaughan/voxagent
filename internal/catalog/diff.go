package catalog

import (
	"maps"
	"reflect"
	"slices"
)

// Kind classifies a single difference.
type Kind string

const (
	KindOnlyInLeft  Kind = "only_in_left"
	KindOnlyInRight Kind = "only_in_right"
	KindChanged     Kind = "changed"
	KindReordered   Kind = "reordered"
)

// Difference is one divergence between two catalogs. Path is a stable,
// human-readable locator such as
// "resources[Notify[loop-1]].parameters.message".
//
// Left and Right are omitted from JSON when nil, but a genuinely nil value —
// Puppet's undef, or a null inside an array or hash parameter — serialises
// identically to an omitted field. Consumers must key off Kind, which is
// always present and unambiguous, rather than inferring meaning from the
// presence or absence of left/right.
type Difference struct {
	Path  string `json:"path"`
	Kind  Kind   `json:"kind"`
	Left  any    `json:"left,omitempty"`
	Right any    `json:"right,omitempty"`
}

// Report is the full result of a comparison.
type Report struct {
	Differences []Difference `json:"differences"`
}

// Identical reports whether the two catalogs matched.
func (r Report) Identical() bool { return len(r.Differences) == 0 }

// Options controls comparison strictness.
type Options struct {
	// IgnoreSourceLocations drops file and line from resource comparison.
	// Off by default: a changed source location is a real difference, and
	// suppressing it by default would hide divergence. Turn it on when
	// deliberately comparing across a manifest reorganisation.
	IgnoreSourceLocations bool
}

// Diff compares two catalogs. Callers must call Normalize on both first, or
// the allowlisted fields will be reported as differences.
//
// The output order is deterministic: differences are emitted in catalog order,
// and map keys are sorted before iteration.
func Diff(left, right *Catalog, opts Options) Report {
	var d []Difference
	d = appendScalarDiffs(d, left, right)
	d = appendResourceDiffs(d, left, right, opts)
	d = appendEdgeDiffs(d, left, right)
	return Report{Differences: d}
}

func appendScalarDiffs(d []Difference, l, r *Catalog) []Difference {
	if l.Name != r.Name {
		d = append(d, Difference{Path: "name", Kind: KindChanged, Left: l.Name, Right: r.Name})
	}
	if l.Environment != r.Environment {
		d = append(d, Difference{Path: "environment", Kind: KindChanged, Left: l.Environment, Right: r.Environment})
	}
	if l.CatalogFormat != r.CatalogFormat {
		d = append(d, Difference{Path: "catalog_format", Kind: KindChanged, Left: l.CatalogFormat, Right: r.CatalogFormat})
	}
	if !equalPtrString(l.CodeID, r.CodeID) {
		d = append(d, Difference{Path: "code_id", Kind: KindChanged, Left: derefString(l.CodeID), Right: derefString(r.CodeID)})
	}
	if !equalStringSets(l.Tags, r.Tags) {
		d = append(d, Difference{Path: "tags", Kind: KindChanged, Left: l.Tags, Right: r.Tags})
	}
	if !equalStringSets(l.Classes, r.Classes) {
		d = append(d, Difference{Path: "classes", Kind: KindChanged, Left: l.Classes, Right: r.Classes})
	}
	return d
}

func appendResourceDiffs(d []Difference, l, r *Catalog, opts Options) []Difference {
	li := indexResources(l.Resources)
	ri := indexResources(r.Resources)

	for _, res := range l.Resources {
		other, ok := ri[res.Ref()]
		if !ok {
			d = append(d, Difference{Path: "resources[" + res.Ref() + "]", Kind: KindOnlyInLeft, Left: res})
			continue
		}
		d = appendOneResourceDiff(d, res, other, opts)
	}
	for _, res := range r.Resources {
		if _, ok := li[res.Ref()]; !ok {
			d = append(d, Difference{Path: "resources[" + res.Ref() + "]", Kind: KindOnlyInRight, Right: res})
		}
	}

	// Order is behavioural: SequentialPrioritizer derives apply-order
	// tie-breaks from catalog position. Compare order over the refs present on
	// both sides, so a pure addition or removal is not also reported as a
	// reorder.
	lo := orderOfCommon(l.Resources, ri)
	ro := orderOfCommon(r.Resources, li)
	if !slices.Equal(lo, ro) {
		d = append(d, Difference{Path: "resources", Kind: KindReordered, Left: lo, Right: ro})
	}
	return d
}

func appendOneResourceDiff(d []Difference, l, r Resource, opts Options) []Difference {
	p := "resources[" + l.Ref() + "]"

	if l.Exported != r.Exported {
		d = append(d, Difference{Path: p + ".exported", Kind: KindChanged, Left: l.Exported, Right: r.Exported})
	}
	if l.Kind != r.Kind {
		d = append(d, Difference{Path: p + ".kind", Kind: KindChanged, Left: l.Kind, Right: r.Kind})
	}
	if !opts.IgnoreSourceLocations {
		if l.File != r.File {
			d = append(d, Difference{Path: p + ".file", Kind: KindChanged, Left: l.File, Right: r.File})
		}
		if l.Line != r.Line {
			d = append(d, Difference{Path: p + ".line", Kind: KindChanged, Left: l.Line, Right: r.Line})
		}
	}
	// Tags are a set in Puppet — `tag ==` collectors and --tags filtering are
	// membership tests — so order carries no behaviour.
	if !equalStringSets(l.Tags, r.Tags) {
		d = append(d, Difference{Path: p + ".tags", Kind: KindChanged, Left: l.Tags, Right: r.Tags})
	}

	for _, k := range slices.Sorted(maps.Keys(l.Parameters)) {
		lv := l.Parameters[k]
		rv, ok := r.Parameters[k]
		if !ok {
			d = append(d, Difference{Path: p + ".parameters." + k, Kind: KindOnlyInLeft, Left: lv})
			continue
		}
		if !reflect.DeepEqual(lv, rv) {
			d = append(d, Difference{Path: p + ".parameters." + k, Kind: KindChanged, Left: lv, Right: rv})
		}
	}
	for _, k := range slices.Sorted(maps.Keys(r.Parameters)) {
		if _, ok := l.Parameters[k]; !ok {
			d = append(d, Difference{Path: p + ".parameters." + k, Kind: KindOnlyInRight, Right: r.Parameters[k]})
		}
	}
	return d
}

func appendEdgeDiffs(d []Difference, l, r *Catalog) []Difference {
	le := indexEdges(l.Edges)
	re := indexEdges(r.Edges)
	for _, e := range l.Edges {
		if _, ok := re[edgeKey(e)]; !ok {
			d = append(d, Difference{Path: "edges[" + edgeKey(e) + "]", Kind: KindOnlyInLeft, Left: e})
		}
	}
	for _, e := range r.Edges {
		if _, ok := le[edgeKey(e)]; !ok {
			d = append(d, Difference{Path: "edges[" + edgeKey(e) + "]", Kind: KindOnlyInRight, Right: e})
		}
	}
	return d
}

func edgeKey(e Edge) string { return e.Source + " -> " + e.Target }

// indexResources keys resources by Ref(). If two resources ever shared a ref,
// this map would silently keep the last one, and the diff would compare a
// catalog it never mentions dropping resources from. Puppet enforces ref
// uniqueness at compile time, so this cannot arise while both sides come from
// Puppet (P0's scope). It becomes a live concern once a second, independent
// implementation is under test in P1, whose uniqueness guarantees are exactly
// what's being verified — revisit then.
func indexResources(rs []Resource) map[string]Resource {
	m := make(map[string]Resource, len(rs))
	for _, r := range rs {
		m[r.Ref()] = r
	}
	return m
}

func indexEdges(es []Edge) map[string]Edge {
	m := make(map[string]Edge, len(es))
	for _, e := range es {
		m[edgeKey(e)] = e
	}
	return m
}

// orderOfCommon returns the refs of rs, in catalog order, restricted to those
// also present in other.
func orderOfCommon(rs []Resource, other map[string]Resource) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if _, ok := other[r.Ref()]; ok {
			out = append(out, r.Ref())
		}
	}
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := slices.Clone(a), slices.Clone(b)
	slices.Sort(as)
	slices.Sort(bs)
	return slices.Equal(as, bs)
}

func equalPtrString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
