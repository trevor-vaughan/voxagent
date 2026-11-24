package graph

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func vertices(refs ...string) []Vertex {
	out := make([]Vertex, len(refs))
	for i, r := range refs {
		out[i] = Vertex{Ref: r, Key: i}
	}
	return out
}

// walked is the order a Walker places g's vertices in when nothing is
// deferred: each vertex is Done as soon as it is taken, as the transaction
// does for a resource that is suitable.
func walked(g *Graph) []string {
	var order []string
	w := g.Walk()
	for ref, _, ok := w.Next(); ok; ref, _, ok = w.Next() {
		order = append(order, ref)
		w.Done(ref)
	}
	return order
}

func orderOf(t *testing.T, refs []string, edges []Edge) string {
	t.Helper()
	g, err := New(vertices(refs...), edges)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(walked(g), " ")
}

// The smallest catalog index among the ready set, recomputed after every
// vertex.
func TestOrderTakesTheSmallestReadyKey(t *testing.T) {
	e := func(s, t string) Edge { return Edge{Source: s, Target: t} }
	for _, tt := range []struct {
		name  string
		refs  []string
		edges []Edge
		want  string
	}{
		{"1", []string{"zeta", "alpha", "mid", "T", "beta"}, nil, "zeta alpha mid T beta"},
		{"1b", []string{"beta", "T", "mid", "alpha", "zeta"}, nil, "beta T mid alpha zeta"},
		{"2a", []string{"a", "b", "c", "d"}, []Edge{e("c", "a")}, "b c a d"},
		{"2b", []string{"g", "f", "e", "free"}, []Edge{e("e", "f"), e("f", "g")}, "e f g free"},
		{"2c", []string{"p", "q", "r", "h", "s", "t"}, []Edge{e("h", "p"), e("h", "q"), e("h", "s")}, "r h p q s t"},
		{"2d", []string{"n1", "n2", "n3", "n4", "n0", "n5"},
			[]Edge{e("n2", "n0"), e("n4", "n3"), e("n1", "n5")}, "n1 n2 n4 n3 n0 n5"},
	} {
		if got := orderOf(t, tt.refs, tt.edges); got != tt.want {
			t.Errorf("case %s: walk = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Equal Keys never tie at random.
func TestOrderBreaksEqualKeysByRef(t *testing.T) {
	g, err := New([]Vertex{{Ref: "b", Key: 0}, {Ref: "a", Key: 0}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(walked(g), " "); got != "a b" {
		t.Errorf("walk = %q, want %q", got, "a b")
	}
}

func TestNewOrdersDependenciesFirst(t *testing.T) {
	g, err := New(
		vertices("File[a]", "Exec[b]", "Notify[c]"),
		[]Edge{
			{Source: "File[a]", Target: "Exec[b]"},
			{Source: "Exec[b]", Target: "Notify[c]"},
		},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got := walked(g)
	want := []string{"File[a]", "Exec[b]", "Notify[c]"}
	if len(got) != len(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walk = %v, want %v", got, want)
		}
	}
}

// The same vertices must produce the same order however the slice holding them
// is arranged: the walk is decided by Keys, not by the position of a vertex in
// the input. Puppet's own apply order is one topological order among many; the
// walk must pick the SAME one every time or the state-diff compares two
// different experiments.
func TestOrderIsDeterministicForTheSameKeys(t *testing.T) {
	edges := []Edge{{Source: "File[root]", Target: "Notify[z]"}}
	forward := vertices("Notify[z]", "Notify[m]", "Notify[a]", "File[root]")
	reversed := slices.Clone(forward)
	slices.Reverse(reversed)
	a, err := New(forward, edges)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	b, err := New(reversed, edges)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got, again := strings.Join(walked(a), " "), strings.Join(walked(b), " ")
	if got != again {
		t.Errorf("order depends on the arrangement of the input:\n  %v\n  %v", got, again)
	}
	// Notify[z] waits for File[root] (Key 3), then runs
	// last; the others follow their Keys.
	if want := "Notify[m] Notify[a] File[root] Notify[z]"; got != want {
		t.Errorf("walk = %q, want %q", got, want)
	}
}

func TestNewDetectsCycle(t *testing.T) {
	_, err := New(
		vertices("File[a]", "File[b]", "File[c]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]"},
			{Source: "File[b]", Target: "File[c]"},
			{Source: "File[c]", Target: "File[a]"},
		},
	)
	if err == nil {
		t.Fatal("New accepted a cycle; want an error")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %T, want *CycleError", err)
	}
	if len(ce.Cycle) == 0 {
		t.Error("CycleError carries no cycle; the message cannot name what is wrong")
	}
	for _, ref := range []string{"File[a]", "File[b]", "File[c]"} {
		if !strings.Contains(err.Error(), ref) {
			t.Errorf("error %q does not name %q", err.Error(), ref)
		}
	}
}

func TestNewDetectsSelfEdge(t *testing.T) {
	_, err := New(vertices("File[a]"), []Edge{{Source: "File[a]", Target: "File[a]"}})
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError for a self-edge", err)
	}
}

func TestNewRejectsUnknownReference(t *testing.T) {
	_, err := New(vertices("File[a]"), []Edge{{Source: "File[a]", Target: "File[ghost]"}})
	if err == nil {
		t.Fatal("New accepted an edge to an undeclared resource; want an error")
	}
	if !strings.Contains(err.Error(), "File[ghost]") {
		t.Errorf("error %q does not name the missing reference", err.Error())
	}
}

// Duplicate edges are not an error -- a catalog can legitimately express the
// same ordering twice, via `before` on one resource and `require` on the other.
func TestDuplicateEdgesAreAccepted(t *testing.T) {
	g, err := New(
		vertices("File[a]", "File[b]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]"},
			{Source: "File[a]", Target: "File[b]"},
		},
	)
	if err != nil {
		t.Fatalf("New returned error on duplicate edges: %v", err)
	}
	if got := walked(g); got[0] != "File[a]" {
		t.Errorf("walk = %v, want File[a] first", got)
	}
}

func TestRefreshTargets(t *testing.T) {
	g, err := New(
		vertices("File[conf]", "Exec[reload]", "Notify[quiet]"),
		[]Edge{
			{Source: "File[conf]", Target: "Exec[reload]", Refresh: true},
			{Source: "File[conf]", Target: "Notify[quiet]"},
		},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got := g.RefreshTargets("File[conf]")
	if len(got) != 1 || got[0] != "Exec[reload]" {
		t.Errorf("RefreshTargets(File[conf]) = %v, want [Exec[reload]]", got)
	}
	if got := g.RefreshTargets("Exec[reload]"); len(got) != 0 {
		t.Errorf("RefreshTargets(Exec[reload]) = %v, want empty", got)
	}
}

func TestDependentsAreTransitive(t *testing.T) {
	g, err := New(
		vertices("File[a]", "File[b]", "File[c]", "File[loner]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]"},
			{Source: "File[b]", Target: "File[c]"},
		},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got := g.Dependents("File[a]")
	if len(got) != 2 {
		t.Fatalf("Dependents(File[a]) = %v, want 2 entries", got)
	}
	if _, ok := got["File[c]"]; !ok {
		t.Error("Dependents(File[a]) does not include the transitive File[c]")
	}
	if _, ok := got["File[loner]"]; ok {
		t.Error("Dependents(File[a]) includes an unrelated resource")
	}
}

// New must never panic and must never return a graph whose order omits or
// duplicates a reference. Fuzzing it directly is the reason this package does
// not import catalog: these topologies are ones no compiler would emit.
func FuzzNew(f *testing.F) {
	f.Add(3, "0>1,1>2")
	f.Add(2, "0>0")
	f.Add(4, "0>1,1>2,2>3,3>0")
	f.Add(1, "")
	f.Fuzz(func(t *testing.T, n int, spec string) {
		if n < 0 || n > 64 {
			t.Skip()
		}
		refs := make([]string, n)
		for i := range refs {
			refs[i] = fmt.Sprintf("Notify[%d]", i)
		}
		var edges []Edge
		for _, part := range strings.Split(spec, ",") {
			var a, b int
			if _, err := fmt.Sscanf(part, "%d>%d", &a, &b); err != nil {
				continue
			}
			if a < 0 || a >= n || b < 0 || b >= n {
				continue
			}
			edges = append(edges, Edge{
				Source: fmt.Sprintf("Notify[%d]", a),
				Target: fmt.Sprintf("Notify[%d]", b),
			})
		}
		g, err := New(vertices(refs...), edges)
		if err != nil {
			return // a cycle is a legitimate answer, not a failure
		}
		order := walked(g)
		if len(order) != n {
			t.Fatalf("walk has %d entries, want %d", len(order), n)
		}
		pos := make(map[string]int, n)
		for i, r := range order {
			if _, dup := pos[r]; dup {
				t.Fatalf("walk contains %s twice", r)
			}
			pos[r] = i
		}
		for _, e := range edges {
			if pos[e.Source] > pos[e.Target] {
				t.Fatalf("edge %s -> %s is inverted in the order", e.Source, e.Target)
			}
		}
	})
}

// A resource that merely depends on a cycle is unorderable, but it is not
// itself cyclic and must not be named as though it were.
//
// The first traceCycle followed edges from the alphabetically first stuck
// reference and returned whatever path it walked. Measured against this exact
// topology, it reported "dependency cycle: File[a]" -- naming the one resource
// with no outgoing edges at all, while File[b] and File[c], the actual loop,
// went unmentioned. An operator sent to look at File[a] would find nothing
// wrong with it.
func TestCycleErrorNamesOnlyResourcesActuallyInTheCycle(t *testing.T) {
	_, err := New(
		vertices("File[a]", "File[b]", "File[c]"),
		[]Edge{
			{Source: "File[b]", Target: "File[a]"}, // a is downstream, not cyclic
			{Source: "File[b]", Target: "File[c]"},
			{Source: "File[c]", Target: "File[b]"}, // the real cycle
		},
	)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError", err)
	}

	inCycle := map[string]bool{}
	for _, r := range ce.Cycle {
		inCycle[r] = true
	}
	if !inCycle["File[b]"] || !inCycle["File[c]"] {
		t.Errorf("Cycle = %v, want it to contain File[b] and File[c]", ce.Cycle)
	}
	if inCycle["File[a]"] {
		t.Errorf("Cycle = %v, names File[a], which has no outgoing edges and is in no cycle", ce.Cycle)
	}
	// Stuck carries everything unorderable, so nothing is hidden.
	if len(ce.Stuck) != 3 {
		t.Errorf("Stuck = %v, want all three unorderable references", ce.Stuck)
	}
	// The message must mention the leftover without claiming it is cyclic.
	if !strings.Contains(err.Error(), "1 other resource could not be ordered either") {
		t.Errorf("error %q does not mention the unorderable File[a]", err.Error())
	}
	if !strings.Contains(err.Error(), "File[a]") {
		t.Errorf("error %q does not name File[a] at all", err.Error())
	}
}

// Two disjoint cycles: neither is downstream of the other. The message may
// report one loop and mention the rest, but it must not claim the rest are
// BLOCKED by the loop it reported -- they are a second, independent cycle.
//
// The first version of this message said "blocking N further resource(s)",
// which sent an operator to fix the reported cycle, re-run, and meet a cycle
// they had been told was merely downstream of it. That is the same defect class
// -- text asserting more than the code knows -- that the cycle-naming fix was
// itself written to close.
func TestCycleErrorDoesNotClaimDisjointCyclesAreBlocked(t *testing.T) {
	_, err := New(
		vertices("File[a]", "File[b]", "File[c]", "File[d]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]"},
			{Source: "File[b]", Target: "File[a]"},
			{Source: "File[c]", Target: "File[d]"},
			{Source: "File[d]", Target: "File[c]"},
		},
	)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError", err)
	}
	if strings.Contains(err.Error(), "blocking") {
		t.Errorf("error %q claims the second cycle is blocked by the first", err.Error())
	}
	if !strings.Contains(err.Error(), "could not be ordered either") {
		t.Errorf("error %q does not mention the other unorderable resources", err.Error())
	}
	if len(ce.Stuck) != 4 {
		t.Errorf("Stuck = %v, want all four", ce.Stuck)
	}
}

// A self-edge is a cycle of one, reported as [X, X]. The leftover count must be
// zero rather than negative or off by one.
func TestCycleErrorOnSelfEdgeMentionsNoOthers(t *testing.T) {
	_, err := New(vertices("File[a]"), []Edge{{Source: "File[a]", Target: "File[a]"}})
	if strings.Contains(err.Error(), "could not be ordered either") {
		t.Errorf("error %q mentions other resources; there are none", err.Error())
	}
	if !strings.Contains(err.Error(), "File[a] -> File[a]") {
		t.Errorf("error %q does not show the self-edge as a loop", err.Error())
	}
}

// The reported loop must be closed: first member repeated last, and every
// consecutive pair a real edge. Without this, a "cycle" that is merely a path
// would read as a loop.
func TestCycleErrorReportsAClosedLoop(t *testing.T) {
	_, err := New(
		vertices("File[a]", "File[b]", "File[c]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]"},
			{Source: "File[b]", Target: "File[c]"},
			{Source: "File[c]", Target: "File[a]"},
		},
	)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError", err)
	}
	if len(ce.Cycle) < 2 {
		t.Fatalf("Cycle = %v, want at least two entries", ce.Cycle)
	}
	if ce.Cycle[0] != ce.Cycle[len(ce.Cycle)-1] {
		t.Errorf("Cycle = %v, does not close: first and last differ", ce.Cycle)
	}
	for i := 0; i < len(ce.Cycle)-1; i++ {
		from, to := ce.Cycle[i], ce.Cycle[i+1]
		var real bool
		for _, target := range declaredEdges(from) {
			if target == to {
				real = true
			}
		}
		if !real {
			t.Errorf("Cycle = %v, but %s -> %s is not an edge", ce.Cycle, from, to)
		}
	}
}

// declaredEdges re-derives the edge set the test declared, so the assertion
// above checks the reported loop against the input rather than against the
// graph's own internals.
func declaredEdges(from string) []string {
	switch from {
	case "File[a]":
		return []string{"File[b]"}
	case "File[b]":
		return []string{"File[c]"}
	case "File[c]":
		return []string{"File[a]"}
	}
	return nil
}

// A duplicate entry in refs is refused, not deduplicated.
//
// refs is a slice and the bookkeeping is maps, so a repeat desynchronised the
// two. Measured before the check existed:
//
//	New(["A","A"], nil)        -> Order() == ["A","A"], no error
//	New(["A","B","B"], [A->B]) -> "0 resources could not be ordered: "
//
// The second is the sharper one: an ACYCLIC graph refused, with a message
// naming nothing and asserting zero resources failed.
func TestNewRejectsDuplicateReferences(t *testing.T) {
	for _, tt := range []struct {
		name  string
		refs  []string
		edges []Edge
	}{
		{"duplicate alone", []string{"File[a]", "File[a]"}, nil},
		{"duplicate with an edge", []string{"File[a]", "File[a]", "File[b]"},
			[]Edge{{Source: "File[a]", Target: "File[b]"}}},
		{"duplicate of an edge target", []string{"File[a]", "File[b]", "File[b]"},
			[]Edge{{Source: "File[a]", Target: "File[b]"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(vertices(tt.refs...), tt.edges)
			if err == nil {
				t.Fatal("New accepted a duplicate reference; want an error")
			}
			if !strings.Contains(err.Error(), "duplicate resource reference") {
				t.Errorf("error %q does not say the reference is duplicated", err.Error())
			}
			var ce *CycleError
			if errors.As(err, &ce) {
				t.Errorf("duplicate reported as a cycle: %v", err)
			}
		})
	}
}

// The undeclared-reference check must cover the SOURCE as well as the target.
// Deleting the source check survived the suite: the only test covering it used
// an unknown target. With the check gone, New(["N[a]"], [ghost->N[a]]) returns
// "1 resource could not be ordered: N[a]" -- a false cycle report for what is
// really a dangling edge.
func TestNewRejectsUnknownSourceReference(t *testing.T) {
	_, err := New(vertices("File[a]"), []Edge{{Source: "File[ghost]", Target: "File[a]"}})
	if err == nil {
		t.Fatal("New accepted an edge FROM an undeclared resource; want an error")
	}
	if !strings.Contains(err.Error(), "File[ghost]") {
		t.Errorf("error %q does not name the missing source", err.Error())
	}
	var ce *CycleError
	if errors.As(err, &ce) {
		t.Errorf("dangling source reported as a cycle: %v", err)
	}
}

// The documented rule is "smallest ready Key at each step", which is stronger
// than "deterministic". Dropping the ordering of the ready set leaves the walk
// deterministic -- so the determinism test cannot see it -- while silently
// changing apply order, which would make the state-diff compare two different
// experiments between versions.
func TestOrderTakesTheSmallestReadyReference(t *testing.T) {
	// m has no dependencies; z and a become ready only after m is applied.
	// Adjacency is sorted by Ref, so a FIFO ready set, like a heap on Ref,
	// yields [m a z]; the smallest ready Key yields [m z a].
	g, err := New(
		vertices("N[m]", "N[z]", "N[a]"),
		[]Edge{{Source: "N[m]", Target: "N[z]"}, {Source: "N[m]", Target: "N[a]"}},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	got := walked(g)
	want := []string{"N[m]", "N[z]", "N[a]"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walk = %v, want %v -- the ready set is not ordered", got, want)
		}
	}
}

// Stuck must be sorted, so a cycle message is byte-identical between runs on
// the same catalog whatever order the references arrived in. Dropping the sort
// of refs left every other test passing.
func TestCycleErrorStuckIsSorted(t *testing.T) {
	edges := []Edge{{Source: "File[y]", Target: "File[z]"}, {Source: "File[z]", Target: "File[y]"}}
	forward, _ := New(vertices("File[y]", "File[z]"), edges)
	_ = forward
	a, errA := New(vertices("File[y]", "File[z]"), edges)
	b, errB := New(vertices("File[z]", "File[y]"), edges)
	if a != nil || b != nil {
		t.Fatal("New returned a graph for a cyclic input")
	}
	if errA.Error() != errB.Error() {
		t.Errorf("message depends on input order:\n  %v\n  %v", errA, errB)
	}
}

// A refresh edge declared twice must fire once. Puppet expresses the same
// relationship from either end, so a catalog carrying both notify on one side
// and subscribe on the other is ordinary -- and refreshing a service twice per
// event is a visible, wrong side effect.
func TestRefreshTargetsAreDeduplicated(t *testing.T) {
	g, err := New(
		vertices("File[conf]", "Exec[reload]"),
		[]Edge{
			{Source: "File[conf]", Target: "Exec[reload]", Refresh: true},
			{Source: "File[conf]", Target: "Exec[reload]", Refresh: true},
		},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if got := g.RefreshTargets("File[conf]"); len(got) != 1 {
		t.Errorf("RefreshTargets = %v, want exactly one entry", got)
	}
}

// Order and RefreshTargets return slices backed by the graph's own state unless
// they copy. A caller reordering the returned slice in place would silently
// change the graph for every later caller.
func TestAccessorsReturnCopies(t *testing.T) {
	g, err := New(
		vertices("File[a]", "File[b]", "File[c]"),
		[]Edge{
			{Source: "File[a]", Target: "File[b]", Refresh: true},
			{Source: "File[a]", Target: "File[c]", Refresh: true},
		},
	)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	targets := g.RefreshTargets("File[a]")
	targets[0] = "MUTATED"
	for _, got := range g.RefreshTargets("File[a]") {
		if got == "MUTATED" {
			t.Error("RefreshTargets() returns the graph's own slice")
		}
	}
}

// A message must stay readable when the stuck set is large. Measured before the
// cap: a two-resource cycle with 20,000 resources downstream produced a single
// 440 KB line on stderr, burying the cycle -- the one actionable part.
func TestCycleErrorCapsTheEnumeration(t *testing.T) {
	const n = 500
	refs := []string{"File[cyc-a]", "File[cyc-b]"}
	edges := []Edge{
		{Source: "File[cyc-a]", Target: "File[cyc-b]"},
		{Source: "File[cyc-b]", Target: "File[cyc-a]"},
	}
	for i := 0; i < n; i++ {
		ref := fmt.Sprintf("File[down-%03d]", i)
		refs = append(refs, ref)
		edges = append(edges, Edge{Source: "File[cyc-a]", Target: ref})
	}
	_, err := New(vertices(refs...), edges)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError", err)
	}
	msg := err.Error()
	if len(msg) > 2000 {
		t.Errorf("message is %d bytes; the enumeration is not capped", len(msg))
	}
	// The COUNT must stay exact even though the list is truncated -- that is
	// the load-bearing fact.
	if !strings.Contains(msg, fmt.Sprintf("%d other resources", n)) {
		t.Errorf("message %q does not report the exact count of %d", msg, n)
	}
	if !strings.Contains(msg, "and 480 more") {
		t.Errorf("message %q does not say how many were elided", msg)
	}
	if !strings.Contains(msg, "File[cyc-a]") {
		t.Errorf("message %q no longer names the cycle", msg)
	}
}

// Independent resources must come out in Key order: unrelated
// resources apply in catalog array order, not by title.
//
// TestOrderTakesTheSmallestReadyReference does not catch a broken ready-set
// ordering on its own: pushes from a single node arrive in adjacency order and
// an unordered heap can pop them back in the same order. With several
// resources ready at once and no edges to impose an order, an unordered ready
// set scrambles them visibly.
func TestOrderFollowsKeysForIndependentResources(t *testing.T) {
	got, err := New(vertices("N[e]", "N[d]", "N[c]", "N[b]", "N[a]"), nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	want := []string{"N[e]", "N[d]", "N[c]", "N[b]", "N[a]"}
	order := walked(got)
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("walk = %v, want %v -- the ready set is not ordered", order, want)
		}
	}
}

// traceCycle must not re-explore references it has already proved acyclic.
//
// Marking a fully explored node black is what keeps the search linear. Without
// it, a stuck subgraph containing a diamond ladder is walked once per distinct
// path through it -- 2^k for k diamonds -- before the cycle is reached.
//
// The topology below puts the cycle FIRST in the reference ordering's tail, so
// the DFS starts at the ladder, exhausts it finding nothing, and only then
// reaches the loop. With black-marking this is a few hundred steps; without it,
// 2^28 and the test does not finish. The wall-clock bound is deliberately loose
// -- it exists to catch an algorithmic regression, not to measure performance.
func TestTraceCycleDoesNotRevisitExploredReferences(t *testing.T) {
	const diamonds = 28

	// Zc1 <-> Zc2 is the cycle; Zc1 -> L00 makes the whole ladder unorderable.
	refs := []string{"Zc1", "Zc2"}
	edges := []Edge{
		{Source: "Zc1", Target: "Zc2"},
		{Source: "Zc2", Target: "Zc1"},
		{Source: "Zc1", Target: "L00"},
	}
	for i := 0; i <= diamonds; i++ {
		refs = append(refs, fmt.Sprintf("L%02d", i))
	}
	for i := 0; i < diamonds; i++ {
		a := fmt.Sprintf("M%02da", i)
		b := fmt.Sprintf("M%02db", i)
		refs = append(refs, a, b)
		edges = append(edges,
			Edge{Source: fmt.Sprintf("L%02d", i), Target: a},
			Edge{Source: fmt.Sprintf("L%02d", i), Target: b},
			Edge{Source: a, Target: fmt.Sprintf("L%02d", i+1)},
			Edge{Source: b, Target: fmt.Sprintf("L%02d", i+1)},
		)
	}

	done := make(chan error, 1)
	go func() {
		_, err := New(vertices(refs...), edges)
		done <- err
	}()

	select {
	case err := <-done:
		var ce *CycleError
		if !errors.As(err, &ce) {
			t.Fatalf("New returned %v, want *CycleError", err)
		}
		if len(ce.Cycle) == 0 {
			t.Error("no cycle reported for a graph that has one")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("New did not finish in 10s: traceCycle is re-exploring references it has already cleared")
	}
}

// walkTrace walks g, deferring every offer of a ref for which deferFor returns
// true, and returns what happened at each step.
func walkTrace(g *Graph, deferFor func(ref string, offer int) bool) string {
	w := g.Walk()
	offers := map[string]int{}
	var trace []string
	for {
		ref, overly, ok := w.Next()
		if !ok {
			break
		}
		offers[ref]++
		switch {
		case overly:
			trace = append(trace, "fail:"+ref)
			w.Done(ref)
		case deferFor(ref, offers[ref]):
			trace = append(trace, "defer:"+ref)
			w.Defer(ref)
		default:
			trace = append(trace, ref)
			w.Done(ref)
		}
	}
	return strings.Join(trace, " ")
}

// A deferred vertex is re-offered once the ready set drains after
// progress, and handed back overly deferred when a drain makes none; its
// dependents wait for it.
func TestWalkerDefersToTheEndOfADrain(t *testing.T) {
	g, err := New(vertices("a", "pkg", "b", "c"), []Edge{{Source: "pkg", Target: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	w := g.Walk()
	var trace []string
	for {
		ref, overly, ok := w.Next()
		if !ok {
			break
		}
		switch {
		case overly:
			trace = append(trace, "fail:"+ref)
			w.Done(ref)
		case ref == "pkg":
			trace = append(trace, "defer:"+ref)
			w.Defer(ref)
		default:
			trace = append(trace, ref)
			w.Done(ref)
		}
	}
	if got, want := strings.Join(trace, " "), "a defer:pkg c defer:pkg fail:pkg b"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// A deferred vertex that is placed on its second offer is an ordinary vertex
// from then on: it counts as progress, and its dependent follows it.
func TestWalkerReofferedVertexThatBecomesSuitable(t *testing.T) {
	g, err := New(vertices("a", "pkg", "b", "c"), []Edge{{Source: "pkg", Target: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	got := walkTrace(g, func(ref string, offer int) bool { return ref == "pkg" && offer == 1 })
	if want := "a defer:pkg c pkg b"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// Every overly deferred vertex is handed back, in deferral order, before
// anything their Done released: relationship_graph.rb:136-139 finishes the
// whole deferred list before it takes the next ready vertex.
func TestWalkerHandsBackEveryOverlyDeferredVertexFirst(t *testing.T) {
	g, err := New(vertices("q", "p", "a", "x"), []Edge{{Source: "q", Target: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	got := walkTrace(g, func(ref string, _ int) bool { return ref == "p" || ref == "q" })
	if want := "defer:q defer:p a defer:q defer:p fail:q fail:p x"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// Nothing has progressed before the first vertex is placed, so a vertex
// deferred while it is the only one ready is failed at once, as Puppet's
// made_progress starts unset.
func TestWalkerFailsAtOnceWithoutProgress(t *testing.T) {
	g, err := New(vertices("pkg", "b"), []Edge{{Source: "pkg", Target: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	got := walkTrace(g, func(ref string, _ int) bool { return ref == "pkg" })
	if want := "defer:pkg fail:pkg b"; got != want {
		t.Errorf("trace = %q, want %q", got, want)
	}
}

// A whit is walked like any vertex; IsWhit and Label let the caller tell it
// apart and name it.
func TestWhitVerticesAreWalkedAndNamedByLabel(t *testing.T) {
	g, err := New([]Vertex{
		{Ref: "Whit[admissible_Class[x]]", Key: 0, Whit: true, Label: "Class[X]"},
		{Ref: "Notify[m]", Key: 1},
		{Ref: "Whit[completed_Class[x]]", Key: 0, Whit: true, Label: "Class[X]"},
	}, []Edge{
		{Source: "Whit[admissible_Class[x]]", Target: "Notify[m]"},
		{Source: "Notify[m]", Target: "Whit[completed_Class[x]]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(walked(g), " "),
		"Whit[admissible_Class[x]] Notify[m] Whit[completed_Class[x]]"; got != want {
		t.Errorf("walk = %q, want %q", got, want)
	}
	for ref, want := range map[string]bool{"Whit[admissible_Class[x]]": true, "Notify[m]": false, "absent": false} {
		if got := g.IsWhit(ref); got != want {
			t.Errorf("IsWhit(%s) = %v, want %v", ref, got, want)
		}
	}
	for ref, want := range map[string]string{"Whit[completed_Class[x]]": "Class[X]", "Notify[m]": "Notify[m]"} {
		if got := g.Label(ref); got != want {
			t.Errorf("Label(%s) = %q, want %q", ref, got, want)
		}
	}
}

// Puppet prints a whit as its container, so a cycle through one names the
// container by its label, once where its two whits are adjacent, and the
// leftovers count each container once.
func TestCycleErrorNamesVerticesByLabel(t *testing.T) {
	_, err := New([]Vertex{
		{Ref: "Whit[admissible_Class[a]]", Key: 0, Whit: true, Label: "Class[A]"},
		{Ref: "Whit[completed_Class[a]]", Key: 0, Whit: true, Label: "Class[A]"},
		{Ref: "Notify[n]", Key: 1},
		{Ref: "Whit[admissible_Class[b]]", Key: 2, Whit: true, Label: "Class[B]"},
		{Ref: "Whit[completed_Class[b]]", Key: 2, Whit: true, Label: "Class[B]"},
	}, []Edge{
		{Source: "Whit[admissible_Class[a]]", Target: "Whit[completed_Class[a]]"},
		{Source: "Whit[completed_Class[a]]", Target: "Notify[n]"},
		{Source: "Notify[n]", Target: "Whit[admissible_Class[a]]"},
		{Source: "Notify[n]", Target: "Whit[admissible_Class[b]]"},
		{Source: "Whit[admissible_Class[b]]", Target: "Whit[completed_Class[b]]"},
	})
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("New returned %v, want *CycleError", err)
	}
	const want = "dependency cycle: Notify[n] -> Class[A] -> Notify[n]; " +
		"1 other resource could not be ordered either: Class[B]"
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err.Error(), want)
	}
	if !slices.Contains(ce.Cycle, "Whit[admissible_Class[a]]") {
		t.Errorf("Cycle = %v, want the vertices' refs; only the message uses labels", ce.Cycle)
	}
}
