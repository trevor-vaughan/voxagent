// Package graph builds the directed acyclic graph a catalog implies and walks
// it in a deterministic order.
//
// It takes reference strings and edges rather than a catalog, and imports
// nothing else from this repository. That is deliberate: it means the graph can
// be fuzzed against pathological topologies -- cycles, self-edges, deep chains,
// wide fans -- without first constructing a valid catalog to carry them.
package graph

import (
	"container/heap"
	"fmt"
	"maps"
	"sort"
	"strings"
)

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
		return fmt.Sprintf("%s could not be ordered: %s",
			plural(len(e.Stuck), "resource"), namedList(e.Stuck))
	}
	msg := fmt.Sprintf("dependency cycle: %s", strings.Join(e.Cycle, " -> "))
	others := e.others()
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

// others returns the stuck references that are not in the reported cycle. They
// may be downstream of it, or in a different cycle entirely -- this function
// does not distinguish, and the message must not either.
func (e *CycleError) others() []string {
	inCycle := make(map[string]struct{}, len(e.Cycle))
	for _, r := range e.Cycle {
		inCycle[r] = struct{}{}
	}
	var out []string
	for _, r := range e.Stuck {
		if _, ok := inCycle[r]; !ok {
			out = append(out, r)
		}
	}
	return out
}

// Graph is a DAG over resource references.
type Graph struct {
	refs     []string            // sorted
	out      map[string][]string // deduplicated, sorted
	refresh  map[string][]string // deduplicated, sorted
	indegree map[string]int
	order    []string
}

// New builds a graph and returns an error if any edge names an undeclared
// resource or if the result is cyclic.
//
// Both are could-not-run conditions for the caller: a catalog that cannot be
// ordered cannot be applied at all, and applying the part that happens to sort
// first would leave a node in a state no agent would produce.
func New(refs []string, edges []Edge) (*Graph, error) {
	g := &Graph{
		refs:     append([]string(nil), refs...),
		out:      make(map[string][]string, len(refs)),
		refresh:  make(map[string][]string),
		indegree: make(map[string]int, len(refs)),
	}
	sort.Strings(g.refs)

	// Duplicates are refused, not deduplicated.
	//
	// refs is a slice and the bookkeeping below is maps, so a repeat used to
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
		g.indegree[e.Target]++
	}
	for k := range g.out {
		sort.Strings(g.out[k])
	}
	for k := range g.refresh {
		sort.Strings(g.refresh[k])
	}

	order, err := g.topological()
	if err != nil {
		return nil, err
	}
	g.order = order
	return g, nil
}

// readyQueue is a min-heap of references, so the ready set can be kept ordered
// without re-sorting it on every step.
type readyQueue []string

func (q readyQueue) Len() int           { return len(q) }
func (q readyQueue) Less(i, j int) bool { return q[i] < q[j] }
func (q readyQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *readyQueue) Push(x any)        { *q = append(*q, x.(string)) }
func (q *readyQueue) Pop() any {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[:n-1]
	return x
}

// topological runs Kahn's algorithm, taking the lexicographically smallest
// ready reference at each step.
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
func (g *Graph) topological() ([]string, error) {
	indegree := make(map[string]int, len(g.indegree))
	maps.Copy(indegree, g.indegree)

	ready := &readyQueue{}
	for _, r := range g.refs {
		if indegree[r] == 0 {
			*ready = append(*ready, r)
		}
	}
	heap.Init(ready)

	order := make([]string, 0, len(g.refs))
	for ready.Len() > 0 {
		next := heap.Pop(ready).(string)
		order = append(order, next)
		for _, t := range g.out[next] {
			indegree[t]--
			if indegree[t] == 0 {
				heap.Push(ready, t)
			}
		}
	}

	if len(order) != len(g.refs) {
		placed := make(map[string]struct{}, len(order))
		for _, r := range order {
			placed[r] = struct{}{}
		}
		var stuck []string
		for _, r := range g.refs {
			if _, ok := placed[r]; !ok {
				stuck = append(stuck, r)
			}
		}
		return nil, &CycleError{Cycle: g.traceCycle(stuck), Stuck: stuck}
	}
	return order, nil
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

// Order returns the deterministic topological order.
func (g *Graph) Order() []string {
	return append([]string(nil), g.order...)
}

// RefreshTargets returns the references a refresh from ref propagates to.
func (g *Graph) RefreshTargets(ref string) []string {
	return append([]string(nil), g.refresh[ref]...)
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
