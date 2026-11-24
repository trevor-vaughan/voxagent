// Package graph builds the directed acyclic graph a catalog implies and walks
// it in a deterministic order.
//
// It takes vertices and edges rather than a catalog, and imports
// nothing else from this repository. That is deliberate: it means the graph can
// be fuzzed against pathological topologies -- cycles, self-edges, deep chains,
// wide fans -- without first constructing a valid catalog to carry them.
package graph

import (
	"container/heap"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
)

// Vertex is one vertex of the walk.
type Vertex struct {
	Ref string
	// Key is Puppet's priority (graph/sequential_prioritizer.rb:17-24): the
	// resource's index in the catalog's resources array. The walk takes the
	// smallest Key among ready vertices; equal Keys, which a valid graph never
	// makes ready together, break by Ref.
	Key int
	// Whit marks a container's admissible or completed anchor: walked, never applied.
	Whit bool
	// Label is how messages name the vertex; empty means Ref.
	Label string
}

// Edge is a directed ordering constraint: Source is applied before Target.
//
// Refresh marks an edge that also propagates a refresh event -- Puppet's `~>`,
// which reaches the catalog as a notify or subscribe metaparameter on a
// resource rather than as an entry in the catalog's own edges array.
type Edge struct {
	Source  string
	Target  string
	Refresh bool
}

// CycleError reports a dependency cycle, carrying the members so the message
// can name them. A cycle is a fact about the catalog, not about the graph
// package, and the operator can only act on it if told which resources are
// involved.
type CycleError struct {
	// Cycle is one genuine closed loop, with its first member repeated last so
	// the message reads as a loop. Every reference in it really is in a cycle.
	Cycle []string
	// Stuck is every reference that could not be ordered. It is a superset of
	// Cycle: a resource that merely depends on a cycle is unorderable too, but
	// it is not itself cyclic and must not be named as though it were.
	Stuck []string
	// labels names the vertices that carry a Label. Puppet prints a whit as
	// its container (type/whit.rb:26), so a message names a container, not
	// either of its whits.
	labels map[string]string
}

// maxNamed caps how many references a message enumerates.
//
// The count stays exact -- that is the load-bearing fact -- but the list does
// not. Measured without the cap, on a two-resource cycle in a base class with
// 20,000 resources downstream of it: a single 440 KB line on stderr. A 60,000
// resource graph produced 780 KB. Nobody reads that, and it buries the cycle
// itself, which is the one actionable part.
const maxNamed = 20

func (e *CycleError) Error() string {
	if len(e.Cycle) == 0 {
		// Reachable only if the stuck set contains no cycle, which cannot
		// happen: every reference Kahn's algorithm leaves behind retains an
		// in-edge from another leftover, so the leftover subgraph has minimum
		// in-degree 1 and must contain a loop.
		//
		// It WAS reachable, and this branch is why the bug was survivable
		// rather than loud. A duplicate entry in refs desynchronised the
		// slice-based length check from the map-based bookkeeping, so an
		// acyclic graph fell in here and reported "0 resources could not be
		// ordered: " -- a refusal that named nothing. New now rejects
		// duplicates, which is what makes this genuinely unreachable.
		stuck := e.named(e.Stuck, nil)
		return fmt.Sprintf("%s could not be ordered: %s",
			plural(len(stuck), "resource"), namedList(stuck))
	}
	// A container's two whits are often adjacent in a loop, and both print
	// as the container: it is named once. A self-edge still reads X -> X.
	var cycle []string
	for i, r := range e.Cycle {
		l := e.label(r)
		if i > 0 && e.Cycle[i-1] != r && cycle[len(cycle)-1] == l {
			continue
		}
		cycle = append(cycle, l)
	}
	msg := fmt.Sprintf("dependency cycle: %s", strings.Join(cycle, " -> "))
	others := e.named(e.Stuck, cycle)
	if len(others) == 0 {
		return msg
	}
	// Deliberately NOT "blocking these": that would assert a causal link this
	// code has not established. Two disjoint cycles produce exactly this shape
	// -- a -> b -> a reported, with c and d left over -- and c and d are not
	// blocked by the reported cycle at all, they are a second cycle. Telling an
	// operator they are blocked would send them to fix a, re-run, and meet a
	// cycle they had been told was merely downstream.
	//
	// The first version of this message said "blocking N further resource(s)",
	// which was the same defect it replaced: text asserting more than the code
	// knows.
	return fmt.Sprintf("%s; %s could not be ordered either: %s",
		msg, plural(len(others), "other resource"), namedList(others))
}

// plural renders a count with its noun, so messages do not say "1 resources".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// namedList joins references, capping the enumeration at maxNamed. The count
// alongside it is always exact; only the listing is truncated.
func namedList(refs []string) string {
	if len(refs) <= maxNamed {
		return strings.Join(refs, ", ")
	}
	return fmt.Sprintf("%s ... and %d more",
		strings.Join(refs[:maxNamed], ", "), len(refs)-maxNamed)
}

// label is how the message names ref.
func (e *CycleError) label(ref string) string {
	if l, ok := e.labels[ref]; ok {
		return l
	}
	return ref
}

// named returns the labels of refs, each once and in order, leaving out those
// in exclude. Called with the reported cycle, it is the stuck vertices that
// are not in it. They may be downstream of it, or in a different cycle
// entirely -- this function does not distinguish, and the message must not
// either.
func (e *CycleError) named(refs, exclude []string) []string {
	seen := make(map[string]struct{}, len(exclude))
	for _, l := range exclude {
		seen[l] = struct{}{}
	}
	var out []string
	for _, r := range refs {
		l := e.label(r)
		if _, dup := seen[l]; !dup {
			seen[l] = struct{}{}
			out = append(out, l)
		}
	}
	return out
}

// Graph is a DAG over resource references.
type Graph struct {
	refs     []string // sorted
	vertex   map[string]Vertex
	out      map[string][]string // deduplicated, sorted
	in       map[string][]string // deduplicated, sorted
	refresh  map[string][]string // deduplicated, sorted
	indegree map[string]int
}

// New builds the graph. vertices keep their Keys; the walk does not sort by name.
//
// It returns an error if a Ref repeats, if any edge names an undeclared
// resource, or if the result is cyclic.
//
// All three are could-not-run conditions for the caller: a catalog that cannot be
// ordered cannot be applied at all, and applying the part that happens to sort
// first would leave a node in a state no agent would produce.
func New(vertices []Vertex, edges []Edge) (*Graph, error) {
	g := &Graph{
		refs:     make([]string, 0, len(vertices)),
		vertex:   make(map[string]Vertex, len(vertices)),
		out:      make(map[string][]string, len(vertices)),
		in:       make(map[string][]string, len(vertices)),
		refresh:  make(map[string][]string),
		indegree: make(map[string]int, len(vertices)),
	}
	for _, v := range vertices {
		g.refs = append(g.refs, v.Ref)
		g.vertex[v.Ref] = v
	}
	sort.Strings(g.refs)

	// Duplicates are refused, not deduplicated.
	//
	// The refs are a slice and the bookkeeping below is maps, so a repeat used to
	// desynchronise the two. Measured before this check existed:
	//
	//	New(["A","A"], nil)        -> Order() == ["A","A"], no error
	//	New(["A","B","B"], [A->B]) -> "0 resources could not be ordered: "
	//
	// The first applies a resource twice. The second is worse: an ACYCLIC graph
	// refused as unorderable, with a message naming nothing and asserting that
	// zero resources failed -- because len(order) was compared against a list
	// containing duplicates, and the stuck set was then computed as a set
	// difference that came out empty.
	//
	// Silent deduplication would be the wrong fix. A catalog with two resources
	// at the same reference is malformed, and catalog.Decode does not enforce
	// uniqueness, so this is where a malformed or hostile catalog is caught.
	// Saying so is more useful than quietly applying one of them.
	known := make(map[string]struct{}, len(g.refs))
	for _, r := range g.refs {
		if _, dup := known[r]; dup {
			return nil, fmt.Errorf("duplicate resource reference %s", r)
		}
		known[r] = struct{}{}
		g.indegree[r] = 0
	}

	seen := make(map[[2]string]bool, len(edges))
	seenRefresh := make(map[[2]string]bool)
	for _, e := range edges {
		if _, ok := known[e.Source]; !ok {
			return nil, fmt.Errorf("edge names undeclared resource %s", e.Source)
		}
		if _, ok := known[e.Target]; !ok {
			return nil, fmt.Errorf("edge names undeclared resource %s", e.Target)
		}
		key := [2]string{e.Source, e.Target}
		if e.Refresh && !seenRefresh[key] {
			seenRefresh[key] = true
			g.refresh[e.Source] = append(g.refresh[e.Source], e.Target)
		}
		// A catalog can express the same ordering twice -- `before` on one side
		// and `require` on the other -- so a repeat is not an error. It must
		// still only count once towards the in-degree, or the walk stalls.
		if seen[key] {
			continue
		}
		seen[key] = true
		g.out[e.Source] = append(g.out[e.Source], e.Target)
		g.in[e.Target] = append(g.in[e.Target], e.Source)
		g.indegree[e.Target]++
	}
	for k := range g.out {
		sort.Strings(g.out[k])
	}
	for k := range g.in {
		sort.Strings(g.in[k])
	}
	for k := range g.refresh {
		sort.Strings(g.refresh[k])
	}

	if err := g.topological(); err != nil {
		return nil, err
	}
	return g, nil
}

// readyQueue is a min-heap of vertices on (Key, Ref), so the ready set can be
// kept ordered without re-sorting it on every step.
type readyQueue []Vertex

func (q readyQueue) Len() int { return len(q) }
func (q readyQueue) Less(i, j int) bool {
	if q[i].Key != q[j].Key {
		return q[i].Key < q[j].Key
	}
	return q[i].Ref < q[j].Ref
}
func (q readyQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *readyQueue) Push(x any)   { *q = append(*q, x.(Vertex)) }
func (q *readyQueue) Pop() any {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[:n-1]
	return x
}

// Walker is one traversal (relationship_graph.rb:114-145): Kahn's algorithm
// over a min-heap on (Key, Ref), Puppet's priority order, not lexicographic.
// At each step it takes the ready vertex with the smallest Key -- the earliest
// in the catalog's resources array -- and re-evaluates after every vertex, so
// a vertex released late with a small Key overtakes one that has been ready
// longer (relationship_graph.rb:61-99).
//
// The caller may set a vertex aside (Defer), as traverse sets aside a
// resource that is not suitable?. When the ready set drains, the deferred
// vertices are offered again if anything was placed since the last drain, and
// otherwise handed back overly deferred, for the caller to fail; Puppet's
// overly_deferred_resource_handler.
//
// Ordering the ready set is what makes the walk deterministic rather than
// merely stable: without it the order would depend on map iteration, and two
// runs of the same catalog could apply independent resources in different
// orders. That would not be wrong -- any topological order is valid -- but it
// would mean the state-diff compares two different experiments.
//
// A heap rather than re-sorting the slice each step. Re-sorting is O(n^2 log n)
// in the width of the ready set, which is exactly the shape of a real catalog:
// mostly independent resources, so nearly everything is ready at once. Measured
// on the sort-every-step version: 225 ms at 10,000 resources, 719 ms at 20,000,
// and 3.4 s at 40,000. The heap gives the identical order for the same cost
// class as the walk itself.
type Walker struct {
	g         *Graph
	remaining map[string]int // predecessors not yet Done
	ready     readyQueue
	// deferred is what the caller set aside since the last drain, in
	// deferral order.
	deferred []string
	// overly is what a drain without progress hands back; failing holds it
	// until Done, which then does not count as progress.
	overly   []string
	failing  map[string]struct{}
	progress bool
}

// Walk starts a traversal of g.
func (g *Graph) Walk() *Walker {
	w := &Walker{g: g, remaining: maps.Clone(g.indegree), failing: map[string]struct{}{}}
	for _, r := range g.refs {
		if w.remaining[r] == 0 {
			w.ready = append(w.ready, g.vertex[r])
		}
	}
	heap.Init(&w.ready)
	return w
}

// Next returns the next vertex. overlyDeferred marks a vertex that was
// deferred and that a whole drain could not place: the caller fails it and
// calls Done. ok is false when the walk is complete.
//
// Every vertex a drain hands back comes before anything their Done releases:
// traverse finishes the whole deferred list before it takes the next ready
// resource (relationship_graph.rb:130-139).
func (w *Walker) Next() (ref string, overlyDeferred, ok bool) {
	if w.ready.Len() == 0 && len(w.overly) == 0 && len(w.deferred) > 0 {
		if w.progress {
			for _, r := range w.deferred {
				heap.Push(&w.ready, w.g.vertex[r])
			}
		} else {
			w.overly = w.deferred
			for _, r := range w.overly {
				w.failing[r] = struct{}{}
			}
		}
		w.deferred = nil
		w.progress = false
	}
	if len(w.overly) > 0 {
		ref, w.overly = w.overly[0], w.overly[1:]
		return ref, true, true
	}
	if w.ready.Len() == 0 {
		return "", false, false
	}
	return heap.Pop(&w.ready).(Vertex).Ref, false, true
}

// Done finishes ref and releases each dependent whose last predecessor it was.
func (w *Walker) Done(ref string) {
	if _, failed := w.failing[ref]; failed {
		delete(w.failing, ref)
	} else {
		w.progress = true
	}
	for _, t := range w.g.out[ref] {
		w.remaining[t]--
		if w.remaining[t] == 0 {
			heap.Push(&w.ready, w.g.vertex[t])
		}
	}
}

// Defer sets ref aside until the ready set drains.
func (w *Walker) Defer(ref string) {
	w.deferred = append(w.deferred, ref)
}

// topological refuses a cyclic graph: the walk with nothing deferred places
// every vertex unless the graph has a cycle.
func (g *Graph) topological() error {
	w := g.Walk()
	placed := make(map[string]struct{}, len(g.refs))
	for {
		next, _, ok := w.Next()
		if !ok {
			break
		}
		placed[next] = struct{}{}
		w.Done(next)
	}

	if len(placed) != len(g.refs) {
		var stuck []string
		for _, r := range g.refs {
			if _, ok := placed[r]; !ok {
				stuck = append(stuck, r)
			}
		}
		labels := map[string]string{}
		for _, r := range stuck {
			if v := g.vertex[r]; v.Label != "" {
				labels[r] = v.Label
			}
		}
		return &CycleError{Cycle: g.traceCycle(stuck), Stuck: stuck, labels: labels}
	}
	return nil
}

// traceCycle finds one genuine closed loop among the unplaced references.
//
// Every reference it returns really is in a cycle. That is the whole point, and
// the first implementation got it wrong: it followed edges from the
// alphabetically first stuck reference and returned whatever path it walked,
// so a resource that merely *depends* on a cycle was reported as being in one.
// Measured -- given File[b] <-> File[c] and File[b] -> File[a], it reported
// "dependency cycle: File[a]", naming the one resource with no outgoing edges
// at all. An operator sent to look at File[a] would find nothing wrong with it.
//
// This is a depth-first search for a back edge, restricted to the stuck
// subgraph. A grey node is on the current path, so reaching one closes a loop
// and the slice from that node onward is the cycle. A black node is fully
// explored and reaches no cycle, so it is never revisited -- which is what
// makes this linear rather than exponential on a wide stuck set.
//
// Reporting one loop rather than all of them is deliberate: a catalog with a
// cycle usually has one, and enumerating every elementary cycle is exponential
// in the worst case. CycleError.Stuck still carries everything that could not
// be ordered, so nothing is hidden.
func (g *Graph) traceCycle(stuck []string) []string {
	const (
		white = iota // unvisited
		grey         // on the current path
		black        // fully explored, reaches no cycle
	)
	inStuck := make(map[string]struct{}, len(stuck))
	for _, r := range stuck {
		inStuck[r] = struct{}{}
	}
	colour := make(map[string]int, len(stuck))

	var path []string
	var found []string

	var visit func(string) bool
	visit = func(n string) bool {
		colour[n] = grey
		path = append(path, n)
		for _, t := range g.out[n] {
			if _, ok := inStuck[t]; !ok {
				continue
			}
			if colour[t] == grey {
				for i, p := range path {
					if p == t {
						found = append(append([]string{}, path[i:]...), t)
						break
					}
				}
				return true
			}
			if colour[t] == white && visit(t) {
				return true
			}
		}
		path = path[:len(path)-1]
		colour[n] = black
		return false
	}

	// stuck is sorted, so the loop reported for a given catalog is the same
	// every run -- an error message that reordered itself between runs would be
	// indistinguishable from one that changed.
	for _, r := range stuck {
		if colour[r] == white && visit(r) {
			return found
		}
	}
	return nil
}

// IsWhit reports whether ref is a container's whit (Vertex.Whit).
func (g *Graph) IsWhit(ref string) bool {
	return g.vertex[ref].Whit
}

// Label is how messages name ref: its Vertex.Label, or ref itself.
func (g *Graph) Label(ref string) string {
	if l := g.vertex[ref].Label; l != "" {
		return l
	}
	return ref
}

// RefreshTargets returns the references a refresh from ref propagates to.
func (g *Graph) RefreshTargets(ref string) []string {
	return append([]string(nil), g.refresh[ref]...)
}

// Predecessors returns the source of every edge into ref, sorted.
func (g *Graph) Predecessors(ref string) []string {
	return append([]string(nil), g.in[ref]...)
}

// HasEdge reports whether the graph has an edge from source to target.
func (g *Graph) HasEdge(source, target string) bool {
	_, found := slices.BinarySearch(g.out[source], target)
	return found
}

// Dependents returns every reference reachable from ref, transitively.
//
// The transaction uses this to skip the dependents of a failed resource, which
// is Puppet's semantics: a resource whose prerequisite failed has not been
// evaluated, and applying it anyway would be acting on an assumption the run
// just disproved.
func (g *Graph) Dependents(ref string) map[string]struct{} {
	out := make(map[string]struct{})
	var walk func(string)
	walk = func(r string) {
		for _, t := range g.out[r] {
			if _, seen := out[t]; seen {
				continue
			}
			out[t] = struct{}{}
			walk(t)
		}
	}
	walk(ref)
	return out
}
