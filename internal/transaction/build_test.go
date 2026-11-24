package transaction

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/graph"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// walked is the order a Walker places g's vertices in when nothing is
// deferred: each vertex is Done as soon as it is taken, as the transaction
// does for a resource that is suitable.
func walked(g *graph.Graph) []string {
	var order []string
	w := g.Walk()
	for ref, _, ok := w.Next(); ok; ref, _, ok = w.Next() {
		order = append(order, ref)
		w.Done(ref)
	}
	return order
}

func res(typ, title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Parameters: params}
}

// Measured against Puppet 8.10.0: the catalog's own edges array carries
// CONTAINMENT only. A `~>` relationship appears nowhere in it -- it arrives as
// a notify parameter on the notifying resource.
func TestBuildGraphUsesContainmentEdges(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil), res("Node", "default", nil), res("Notify", "hi", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Node[default]"},
			{Source: "Node[default]", Target: "Notify[hi]"},
		},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	order := walked(g)
	at := func(ref string) int { return slices.Index(order, ref) }
	if order[0] != "Whit[admissible_Stage[main]]" ||
		!(at("Whit[admissible_Node[default]]") < at("Notify[hi]") && at("Notify[hi]") < at("Whit[completed_Node[default]]")) {
		t.Errorf("walk = %v, want Stage[main]'s admissible whit first and Notify[hi] inside Node[default]'s whits", order)
	}
}

// Each resource's Key is its index in the
// catalog's resources array, so unrelated resources walk in that order rather
// than by title, and reversing the array reverses the walk.
func TestBuildGraphKeysResourcesByCatalogIndex(t *testing.T) {
	resources := []catalog.Resource{
		res("Notify", "zeta", nil), res("Notify", "alpha", nil), res("Notify", "mid", nil),
		res("File", "/T", nil), res("Notify", "beta", nil),
	}
	want := []string{"Notify[zeta]", "Notify[alpha]", "Notify[mid]", "File[/T]", "Notify[beta]"}
	for _, reversed := range []bool{false, true} {
		rs, w := slices.Clone(resources), slices.Clone(want)
		if reversed {
			slices.Reverse(rs)
			slices.Reverse(w)
		}
		g, err := BuildGraph(context.Background(), &catalog.Catalog{Resources: rs}, testRegistry)
		if err != nil {
			t.Fatalf("BuildGraph returned error: %v", err)
		}
		if got := walked(g); !slices.Equal(got, w) {
			t.Errorf("reversed=%v: walk = %v, want %v", reversed, got, w)
		}
	}
}

func TestBuildGraphRelationshipMetaparameters(t *testing.T) {
	tests := []struct {
		name    string
		param   string
		wantSrc string
		wantDst string
		refresh bool
	}{
		// before on A listing B: A applies first.
		{"before", "before", "File[a]", "Exec[b]", false},
		// require on A listing B: B applies first.
		{"require", "require", "Exec[b]", "File[a]", false},
		// notify is before plus a refresh.
		{"notify", "notify", "File[a]", "Exec[b]", true},
		// subscribe is require plus a refresh.
		{"subscribe", "subscribe", "Exec[b]", "File[a]", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &catalog.Catalog{Resources: []catalog.Resource{
				res("File", "a", map[string]any{tt.param: []any{"Exec[b]"}}),
				res("Exec", "b", nil),
			}}
			g, err := BuildGraph(context.Background(), c, testRegistry)
			if err != nil {
				t.Fatalf("BuildGraph returned error: %v", err)
			}
			order := walked(g)
			if order[0] != tt.wantSrc {
				t.Errorf("walk = %v, want %s first", order, tt.wantSrc)
			}
			got := g.RefreshTargets(tt.wantSrc)
			if tt.refresh {
				if len(got) != 1 || got[0] != tt.wantDst {
					t.Errorf("RefreshTargets(%s) = %v, want [%s]", tt.wantSrc, got, tt.wantDst)
				}
			} else if len(got) != 0 {
				t.Errorf("RefreshTargets(%s) = %v, want empty for %s", tt.wantSrc, got, tt.param)
			}
		})
	}
}

// A single-target metaparameter compiles to a bare string, not a one-element
// array. Measured: `notify => Notify['one']` emits "Notify[one]" while
// `notify => [Notify['one'], Notify['two']]` and the chaining arrow both emit
// arrays. This is the common shape in hand-written manifests rather than an
// exotic one, so reading only the array shape would fail on ordinary code.
func TestBuildGraphAcceptsScalarRelationshipValue(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", map[string]any{"notify": "Exec[b]"}),
		res("Exec", "b", nil),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("File[a]"); len(got) != 1 || got[0] != "Exec[b]" {
		t.Errorf("RefreshTargets(File[a]) = %v, want [Exec[b]]", got)
	}
}

// A relationship naming a resource the catalog does not contain is a
// could-not-run condition: the run cannot be ordered, so no part of it may be
// applied.
func TestBuildGraphRejectsDanglingRelationship(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", map[string]any{"require": []any{"Exec[ghost]"}}),
	}}
	_, err := BuildGraph(context.Background(), c, testRegistry)
	if err == nil {
		t.Fatal("BuildGraph accepted a relationship naming an absent resource")
	}
	if !strings.Contains(err.Error(), "Exec[ghost]") {
		t.Errorf("error %q does not name the missing resource", err.Error())
	}
	if !strings.Contains(err.Error(), "File[a]") {
		t.Errorf("error %q does not name the resource that declared it", err.Error())
	}
}

func TestBuildGraphReportsCycle(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "a", map[string]any{"require": []any{"File[b]"}}),
		res("File", "b", map[string]any{"require": []any{"File[a]"}}),
	}}
	if _, err := BuildGraph(context.Background(), c, testRegistry); err == nil {
		t.Fatal("BuildGraph accepted a cycle")
	}
}

// Both a metaparameter relationship AND a containment edge must reach the
// graph. A build that read only one source would still produce a valid
// topological order, so nothing downstream would notice the other was dropped.
func TestBuildGraphCombinesBothSources(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("File", "conf", map[string]any{"notify": []any{"Exec[reload]"}}),
			res("Exec", "reload", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "File[conf]"},
			{Source: "Stage[main]", Target: "Exec[reload]"},
		},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	order := walked(g)
	if order[0] != "Whit[admissible_Stage[main]]" {
		t.Errorf("walk = %v, want Stage[main]'s admissible whit first (containment)", order)
	}
	pos := map[string]int{}
	for i, r := range order {
		pos[r] = i
	}
	if pos["File[conf]"] > pos["Exec[reload]"] {
		t.Errorf("walk = %v: the notify relationship was dropped", order)
	}
	if got := g.RefreshTargets("File[conf]"); !slices.Contains(got, "Exec[reload]") {
		t.Errorf("RefreshTargets(File[conf]) = %v, want it to include Exec[reload]", got)
	}
}

// positions maps each applying resource to its index in the walk order.
func positions(t *testing.T, g *graph.Graph) map[string]int {
	t.Helper()
	pos := map[string]int{}
	for i, r := range resourceOrder(g) {
		pos[r] = i
	}
	return pos
}

// A relationship whose endpoint is a container orders that container's CONTENTS.
//
// Measured against Puppet 8.10.0, graph/relationship_graph.rb
// replace_containers_with_anchors: every container is replaced by an
// admissible/completed whit pair, each containment edge is rewired onto them,
// and `containers.each { |x| remove_vertex! x }` deletes the container itself.
// The contract that survives is "everything in A precedes everything in B".
//
// The contents are listed so that a plain catalog-order (Key) tie-break
// yields the WRONG order. Without that, an implementation which drops the relationship
// entirely passes by accident -- which is exactly how this shipped.
func TestBuildGraphOrdersContainerContentsThroughWhits(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("Class", "A", map[string]any{"before": []any{"Class[B]"}}),
			res("Class", "B", nil),
			res("Notify", "aaa-inside-B", nil),
			res("Notify", "zzz-inside-A", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[A]"},
			{Source: "Stage[main]", Target: "Class[B]"},
			{Source: "Class[A]", Target: "Notify[zzz-inside-A]"},
			{Source: "Class[B]", Target: "Notify[aaa-inside-B]"},
		},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["Notify[zzz-inside-A]"] > pos["Notify[aaa-inside-B]"] {
		t.Errorf("walk = %v\nClass[A] before => Class[B] did not order their contents",
			walked(g))
	}
}

// A refresh notified to a container reaches the resources inside it, through
// the container's admissible whit.
//
// Puppet carries the edge's own label onto the rewired whit edge
// (`add_edge(completed[e.source], admissible[x], e.label)`), labels every
// containment edge Default_label, and Whit#refresh is a deliberate no-op so as
// to "not break the chain of propagation" -- so `notify => Class[x]` restarts
// everything in x. Dropping it silently is the inverse of the failure the
// refresh guard exists to prevent: not a service restarting every run, but one
// that never restarts at all.
func TestBuildGraphRefreshesAContainerThroughItsWhits(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("File", "conf", map[string]any{"notify": []any{"Class[B]"}}),
			res("Class", "B", nil),
			res("Exec", "reload", nil),
		},
		Edges: []catalog.Edge{{Source: "Class[B]", Target: "Exec[reload]"}},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("File[conf]"); !slices.Equal(got, []string{"Whit[admissible_Class[B]]"}) {
		t.Errorf("RefreshTargets(File[conf]) = %v, want [Whit[admissible_Class[B]]]", got)
	}
	if got := g.RefreshTargets("Whit[admissible_Class[B]]"); !slices.Contains(got, "Exec[reload]") {
		t.Errorf("RefreshTargets(Whit[admissible_Class[B]]) = %v, want it to include Exec[reload]", got)
	}
}

// Containers nest, and Puppet's admissible/completed maps recurse through them,
// so a relationship declared BY an outer class must be honoured by a resource
// held by an inner one.
//
// The outgoing direction is the one that can fail. Containment already orders
// `Class[outer]` before its contents, so "something before the class" is
// transitively before the contents for free, and a test in that direction
// passes whether or not the whits are wired. Only "everything inside the class
// before something outside it" requires the completed whit -- containment
// supplies `outer -> contents`, never `contents -> after`.
// `aaa-after` is listed before `zzz-deep` so the catalog-order (Key) tie-break
// is wrong too.
func TestBuildGraphOrdersNestedContainersThroughWhits(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Class", "outer", map[string]any{"before": []any{"Notify[aaa-after]"}}),
			res("Class", "outer::inner", nil),
			res("Notify", "aaa-after", nil),
			res("Notify", "zzz-deep", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Class[outer]", Target: "Class[outer::inner]"},
			{Source: "Class[outer::inner]", Target: "Notify[zzz-deep]"},
		},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["Notify[zzz-deep]"] > pos["Notify[aaa-after]"] {
		t.Errorf("walk = %v\nthe relationship did not reach through the nested class",
			walked(g))
	}
}

// An empty container is not an error. Puppet emits
// `add_edge(admissible[x], completed[x]) if contents.empty?` with no label, which
// constrains only the whits and so orders no real resource.
//
// Asserted through notify rather than before, so the assertion has something to
// be wrong about: the refresh reaches the admissible whit and stops there.
func TestBuildGraphAcceptsRelationshipToEmptyContainer(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Notify", "gate", map[string]any{"notify": []any{"Class[empty]"}}),
			res("Class", "empty", nil),
		},
	}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("Notify[gate]"); !slices.Equal(got, []string{"Whit[admissible_Class[empty]]"}) {
		t.Errorf("RefreshTargets(Notify[gate]) = %v, want [Whit[admissible_Class[empty]]]", got)
	}
	const admissible = "Whit[admissible_Class[empty]]"
	got := g.Dependents(admissible)
	if _, ok := got["Whit[completed_Class[empty]]"]; len(got) != 1 || !ok {
		t.Errorf("Dependents(%s) = %v, want only the completed whit", admissible, got)
	}
	if got := g.RefreshTargets(admissible); len(got) != 0 {
		t.Errorf("RefreshTargets(%s) = %v, want none (rule 0 carries no label)", admissible, got)
	}
}

// containment_path is the chain of containers holding a resource, and it is a
// compared field in the report differential.
//
// Measured against Puppet 8.10.0, type/component.rb pathbuilder: a Class
// contributes its TITLE and everything else contributes its full reference, so
// a file under a class under a node reads
// ["Stage[main]", "Main", "Node[default]", "File[...]"].
//
// The Class title is capitalised per :: segment. A real catalog already spells
// user classes that way -- Class[Deep::Nested_thing] -- but stores the implicit
// main class as Class[main], and Ruby still reports it as "Main".
func TestContainmentPaths(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("Class", "main", nil),
			res("Node", "default", nil),
			res("Class", "Deep::Nested_thing", nil),
			res("File", "/tmp/a", nil),
			res("Notify", "inside", nil),
			res("Notify", "orphan", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: "Node[default]"},
			{Source: "Node[default]", Target: "File[/tmp/a]"},
			{Source: "Stage[main]", Target: "Class[Deep::Nested_thing]"},
			{Source: "Class[Deep::Nested_thing]", Target: "Notify[inside]"},
		},
	}
	got := containmentPaths(c, nil)

	for ref, want := range map[string][]string{
		"File[/tmp/a]":   {"Stage[main]", "Main", "Node[default]", "File[/tmp/a]"},
		"Notify[inside]": {"Stage[main]", "Deep::Nested_thing", "Notify[inside]"},
		// A resource no edge contains is its own whole path, not an empty one.
		"Notify[orphan]": {"Notify[orphan]"},
	} {
		if !slices.Equal(got[ref], want) {
			t.Errorf("containmentPaths()[%s] = %v, want %v", ref, got[ref], want)
		}
	}
}

// A resource may declare several targets at once.
func TestBuildGraphHandlesMultipleTargets(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "conf", map[string]any{"notify": []any{"Exec[a]", "Exec[b]"}}),
		res("Exec", "a", nil),
		res("Exec", "b", nil),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("File[conf]"); len(got) != 2 {
		t.Errorf("RefreshTargets(File[conf]) = %v, want two entries", got)
	}
}

// An empty catalog builds an empty graph rather than failing. It is unusual but
// not malformed, and refusing it would be a claim the catalog is broken.
func TestBuildGraphAcceptsEmptyCatalog(t *testing.T) {
	g, err := BuildGraph(context.Background(), &catalog.Catalog{}, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := walked(g); len(got) != 0 {
		t.Errorf("walk = %v, want empty", got)
	}
}

// testRegistry holds the real providers, so autorequire behaves in these tests
// the way it behaves in a run. The fixtures above use relative titles like "a",
// which have no managed ancestor, so it adds no edges where none are intended.
var testRegistry = ral.NewRegistry(ral.Notify{}, ral.File{}, ral.Exec{})

// A file is applied after its parent directory with nothing in the catalog
// saying so.
//
// This is Puppet's autorequire, and without it the most ordinary pattern there
// is fails. Measured in a container before the fix: voxagent applied
// File[/opt/spine/conf] first and reported
// "create /opt/spine/conf: no such file or directory", because under any plain
// string order File[/opt/spine/conf] precedes File[/opt/spine] -- '/' is 0x2F
// and ']' is 0x5D.
func TestBuildGraphAutoRequiresParentDirectory(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/opt/spine/conf", map[string]any{"ensure": "file"}),
		res("File", "/opt/spine", map[string]any{"ensure": "directory"}),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["File[/opt/spine]"] > pos["File[/opt/spine/conf]"] {
		t.Errorf("walk = %v, want the directory before the file it holds", walked(g))
	}
}

// An autorequire carries no refresh. Puppet sets `edge.event = :NONE` for one.
func TestBuildGraphAutoRequireDoesNotRefresh(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/opt/spine", map[string]any{"ensure": "directory"}),
		res("File", "/opt/spine/conf", map[string]any{"ensure": "file"}),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("File[/opt/spine]"); len(got) != 0 {
		t.Errorf("RefreshTargets(File[/opt/spine]) = %v, want none", got)
	}
}

// An explicit relationship beats an autorequire, so the two can never form a
// cycle between the same pair.
//
// Measured against Puppet 8.10.0, relationship_graph.rb
// build_autorelation_dependencies:
//
//	next if edge?(edge.source, edge.target)     # already stated -- do not duplicate
//	if edge?(edge.target, edge.source)          # stated the other way round
//	  debug "Skipping automatic relationship"   # -- the manual edge wins
//
// Declaring that the directory requires the file inside it is unusual, but it
// is not an error, and a run that refused it would refuse a catalog Puppet
// applies.
func TestBuildGraphExplicitRelationshipBeatsAutoRequire(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/opt/spine", map[string]any{
			"ensure": "directory", "require": []any{"File[/opt/spine/conf]"},
		}),
		res("File", "/opt/spine/conf", map[string]any{"ensure": "file"}),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph reported an error, so the autorequire formed a cycle "+
			"with the explicit relationship: %v", err)
	}
	pos := positions(t, g)
	if pos["File[/opt/spine/conf]"] > pos["File[/opt/spine]"] {
		t.Errorf("walk = %v, want the explicit require to win", walked(g))
	}
}

// Autorequire is SOFT: naming an unmanaged path is no edge, not an error.
// An explicit relationship to an absent resource is could-not-run; these are
// not, and conflating them would refuse most real catalogs.
func TestBuildGraphAutoRequireToUnmanagedPathIsNotAnError(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "e", map[string]any{"command": "/usr/bin/touch /tmp/x"}),
	}}
	if _, err := BuildGraph(context.Background(), c, testRegistry); err != nil {
		t.Errorf("BuildGraph refused a command naming an unmanaged file: %v", err)
	}
}

func TestBuildGraphResolvesAFilePathAlias(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "nsx", map[string]any{"path": "/x/n", "ensure": "file"}),
		res("File", "/x/after", map[string]any{"ensure": "file", "require": "File[/x/n]"}),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	pos := positions(t, g)
	if pos["File[nsx]"] >= pos["File[/x/after]"] {
		t.Errorf("order %v: File[nsx] must precede File[/x/after]", walked(g))
	}
}

func TestAutoRequireFindsAParentManagedUnderAnotherTitle(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/x/pd/child", map[string]any{"ensure": "file"}),
		res("File", "pd", map[string]any{"path": "/x/pd", "ensure": "directory"}),
	}}
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	pos := positions(t, g)
	if pos["File[pd]"] >= pos["File[/x/pd/child]"] {
		t.Errorf("order %v: File[pd] must precede its child", walked(g))
	}
}

// autoRelatingStub declares one auto-relation of each kind for Vox_value[a].
type autoRelatingStub struct{ evaluatorStub }

func (autoRelatingStub) AutoRelations(r catalog.Resource) []ral.AutoRelation {
	if r.Ref() != "Vox_value[a]" {
		return nil
	}
	return []ral.AutoRelation{{Kind: ral.RelRequire, Ref: "File[/req]"}, {Kind: ral.RelBefore, Ref: "File[/bef]"},
		{Kind: ral.RelSubscribe, Ref: "File[/sub]"}, {Kind: ral.RelNotify, Ref: "File[/not]"},
		{Kind: ral.RelRequire, Ref: "Vox_value[a]"}, {Kind: ral.RelBefore, Ref: "File[/stated]"}}
}

func TestAutoRelationsBecomeTypedEdges(t *testing.T) {
	var calls, applied, refreshed []string
	stub := autoRelatingStub{evaluatorStub{typ: "vox_value", calls: &calls}}
	reg := ral.NewRegistry(stub, recorder{typ: "file", applied: &applied, refreshed: &refreshed})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil), res("File", "/req", nil), res("File", "/bef", nil),
		res("File", "/sub", nil), res("File", "/not", nil),
		// Stated the other way round: the manual edge wins and the autobefore is dropped.
		res("File", "/stated", map[string]any{"before": "Vox_value[a]"}),
	}}
	g, err := BuildGraph(context.Background(), c, reg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	order := walked(g)
	at := func(ref string) int { return slices.Index(order, ref) }
	a := at("Vox_value[a]")
	if !(at("File[/req]") < a && a < at("File[/bef]") && at("File[/sub]") < a && a < at("File[/not]") && at("File[/stated]") < a) {
		t.Errorf("order = %v", order)
	}
	if !slices.Contains(g.RefreshTargets("File[/sub]"), "Vox_value[a]") || !slices.Contains(g.RefreshTargets("Vox_value[a]"), "File[/not]") {
		t.Error("subscribe and notify auto-relations must carry refresh")
	}
	if slices.Contains(g.RefreshTargets("File[/req]"), "Vox_value[a]") || slices.Contains(g.RefreshTargets("Vox_value[a]"), "File[/bef]") {
		t.Error("require and before auto-relations carry no events")
	}
}

// relStub gives each ref its own native autorequires and auto-relations.
type relStub struct {
	evaluatorStub
	reqs map[string][]string
	rels map[string][]ral.AutoRelation
}

func (s relStub) AutoRequires(r catalog.Resource, _ func(string) bool) []string {
	return s.reqs[r.Ref()]
}
func (s relStub) AutoRelations(r catalog.Resource) []ral.AutoRelation { return s.rels[r.Ref()] }

func relGraph(t *testing.T, stub relStub, resources ...catalog.Resource) *graph.Graph {
	t.Helper()
	g, err := BuildGraph(context.Background(), &catalog.Catalog{Resources: resources}, ral.NewRegistry(stub))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return g
}

// relationship_graph.rb:172-212 skips an auto edge when ANY edge already joins
// the pair, so the first of a resource's require/subscribe wins.
func TestAutoRelationsSamePairOnlyFirstKindWins(t *testing.T) {
	var calls []string
	stub := relStub{evaluatorStub: evaluatorStub{typ: "vox_value", calls: &calls}, rels: map[string][]ral.AutoRelation{
		// Listed subscribe-first: Puppet still processes require first.
		"Vox_value[a]": {{Kind: ral.RelSubscribe, Ref: "Vox_value[b]"}, {Kind: ral.RelRequire, Ref: "Vox_value[b]"}}}}
	g := relGraph(t, stub, res("Vox_value", "a", nil), res("Vox_value", "b", nil))
	if slices.Contains(g.RefreshTargets("Vox_value[b]"), "Vox_value[a]") {
		t.Error("the later subscribe edge was added beside the require edge")
	}
	if order := walked(g); slices.Index(order, "Vox_value[b]") > slices.Index(order, "Vox_value[a]") {
		t.Errorf("order = %v", order)
	}
}

func TestAutoRelationsOppositeDirectionIsDropped(t *testing.T) {
	var calls []string
	stub := relStub{evaluatorStub: evaluatorStub{typ: "vox_value", calls: &calls}, rels: map[string][]ral.AutoRelation{
		"Vox_value[a]": {{Kind: ral.RelNotify, Ref: "Vox_value[b]"}, {Kind: ral.RelRequire, Ref: "Vox_value[b]"}}}}
	g := relGraph(t, stub, res("Vox_value", "a", nil), res("Vox_value", "b", nil))
	// require runs before notify: b first, and the notify edge is dropped.
	order := walked(g)
	if slices.Index(order, "Vox_value[b]") > slices.Index(order, "Vox_value[a]") ||
		slices.Contains(g.RefreshTargets("Vox_value[a]"), "Vox_value[b]") {
		t.Errorf("order = %v", order)
	}
}

func TestAutoRelationsMutualAutorequireFirstInCatalogOrderWins(t *testing.T) {
	var calls []string
	stub := relStub{evaluatorStub: evaluatorStub{typ: "vox_value", calls: &calls},
		reqs: map[string][]string{"Vox_value[a]": {"Vox_value[b]"}, "Vox_value[b]": {"Vox_value[a]"}}}
	g := relGraph(t, stub, res("Vox_value", "a", nil), res("Vox_value", "b", nil))
	if order := walked(g); slices.Index(order, "Vox_value[b]") > slices.Index(order, "Vox_value[a]") {
		t.Errorf("order = %v, want a's autorequire of b to win", order)
	}
}

// An auto-relation names a file by its path alias, which build.go resolves to
// the resource's title-based ref.
func TestAutoRelationRefIsCanonicalisedThroughFileAliases(t *testing.T) {
	var calls []string
	stub := relStub{evaluatorStub: evaluatorStub{typ: "vox_value", calls: &calls}, rels: map[string][]ral.AutoRelation{
		"Vox_value[a]": {{Kind: ral.RelRequire, Ref: "File[/etc/real]"}}}}
	g, err := BuildGraph(context.Background(), &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_value", "a", nil), res("File", "named", map[string]any{"path": "/etc/real"})}},
		ral.NewRegistry(stub, recorder{typ: "file"}))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	if order := walked(g); slices.Index(order, "File[named]") > slices.Index(order, "Vox_value[a]") {
		t.Errorf("order = %v, want File[named] first via its alias", order)
	}
}

// aliasingStub is a provider that knows Vox_value[vt] by a second name.
type aliasingStub struct{ evaluatorStub }

func (aliasingStub) Aliases(r catalog.Resource) []string {
	if r.Ref() == "Vox_value[vt]" {
		return []string{"Vox_value[vn]"}
	}
	return nil
}

func TestBuildGraphResolvesAProviderAlias(t *testing.T) {
	var calls, applied, refreshed []string
	reg := ral.NewRegistry(aliasingStub{evaluatorStub{typ: "vox_value", calls: &calls}},
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "after", map[string]any{"require": "Vox_value[vn]"}),
		res("Vox_value", "vt", nil),
	}}
	g, err := BuildGraph(context.Background(), c, reg)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	pos := positions(t, g)
	if pos["Vox_value[vt]"] >= pos["Notify[after]"] {
		t.Errorf("order %v: Vox_value[vt] must precede Notify[after]", walked(g))
	}
}

// titledStub is a provider whose Vox_titled[fips] Puppet names
// Vox_titled[fips:all] and cannot look up by its catalog title, as a type
// that overrides title.
type titledStub struct{ evaluatorStub }

func (titledStub) Unresolvable(r catalog.Resource) bool { return r.Ref() == "Vox_titled[fips]" }

func TestBuildGraphRefusesAnUnresolvableTitle(t *testing.T) {
	var calls, applied, refreshed []string
	reg := ral.NewRegistry(titledStub{evaluatorStub{typ: "vox_titled", calls: &calls,
		retitled: map[string][2]string{"Vox_titled[fips]": {"Vox_titled[fips:all]", "fips:all"}}}},
		recorder{typ: "notify", applied: &applied, refreshed: &refreshed})
	for _, param := range []string{"before", "require", "subscribe", "notify"} {
		c := &catalog.Catalog{Resources: []catalog.Resource{
			res("Vox_titled", "fips", nil),
			res("Notify", "probe", map[string]any{param: "Vox_titled[fips]"}),
		}}
		_, err := BuildGraph(context.Background(), c, reg)
		const want = "Failed to apply catalog: Could not retrieve dependency 'Vox_titled[fips]' of Notify[probe]"
		var unresolvable *UnresolvableError
		if err == nil || err.Error() != want || !errors.As(err, &unresolvable) {
			t.Errorf("%s: BuildGraph error = %v, want %q", param, err, want)
		}
	}

	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_titled", "audit:all", nil),
		res("Notify", "probe", map[string]any{"subscribe": "Vox_titled[audit:all]"}),
	}}
	if _, err := BuildGraph(context.Background(), c, reg); err != nil {
		t.Errorf("a resolvable title: BuildGraph: %v", err)
	}
}

// resourceOrder is g's walk restricted to the catalog's applying resources.
func resourceOrder(g *graph.Graph) []string {
	var out []string
	for _, ref := range walked(g) {
		if !g.IsWhit(ref) {
			out = append(out, ref)
		}
	}
	return out
}

// topScope is a compiled catalog's skeleton (Stage[main], Class[Settings],
// Class[main]) followed by rs, with containment edges as given.
func topScope(rs []catalog.Resource, edges ...[2]string) *catalog.Catalog {
	c := &catalog.Catalog{Resources: append([]catalog.Resource{
		res("Stage", "main", nil), res("Class", "Settings", nil), res("Class", "main", nil)}, rs...)}
	c.Edges = []catalog.Edge{{Source: "Stage[main]", Target: "Class[Settings]"},
		{Source: "Stage[main]", Target: "Class[main]"}}
	for _, e := range edges {
		c.Edges = append(c.Edges, catalog.Edge{Source: e[0], Target: e[1]})
	}
	return c
}

func n(title string, params map[string]any) catalog.Resource { return res("Notify", title, params) }

// The completed whit takes its own turn, so r (index 4)
// runs before d (index 3), which waits for the class's completed whit (5).
func TestWhitIsAQueueEntryNotASplice(t *testing.T) {
	c := topScope([]catalog.Resource{
		n("d", map[string]any{"require": "Class[X]"}), n("r", map[string]any{"require": "Notify[xm]"}),
		res("Class", "X", nil), n("xm", nil)},
		[2]string{"Class[main]", "Notify[d]"}, [2]string{"Class[main]", "Notify[r]"},
		[2]string{"Stage[main]", "Class[X]"}, [2]string{"Class[X]", "Notify[xm]"})
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(resourceOrder(g), " "), "Notify[xm] Notify[r] Notify[d]"; got != want {
		t.Errorf("order = %q, want %q (splicing gives xm d r)", got, want)
	}
}

// inner2 is contain()ed by outer and
// included by Stage[main], so it is under both; inner1 is only included.
func TestWhitsGiveAClassWithTwoContainersBothParents(t *testing.T) {
	c := topScope([]catalog.Resource{
		res("Class", "Outer", nil), res("Class", "Inner1", nil),
		n("i1", map[string]any{"require": "Notify[late]"}), res("Class", "Inner2", nil), n("i2", nil),
		n("outer_n", nil), n("after", map[string]any{"require": "Class[Outer]"}), n("late", nil)},
		[2]string{"Stage[main]", "Class[Outer]"}, [2]string{"Stage[main]", "Class[Inner1]"},
		[2]string{"Class[Inner1]", "Notify[i1]"}, [2]string{"Stage[main]", "Class[Inner2]"},
		[2]string{"Class[Outer]", "Class[Inner2]"}, [2]string{"Class[Inner2]", "Notify[i2]"},
		[2]string{"Class[Outer]", "Notify[outer_n]"}, [2]string{"Class[main]", "Notify[after]"},
		[2]string{"Class[main]", "Notify[late]"})
	g, err := BuildGraph(context.Background(), c, testRegistry)
	if err != nil {
		t.Fatal(err)
	}
	want := "Whit[admissible_Stage[main]] Whit[admissible_Class[Settings]] Whit[completed_Class[Settings]] " +
		"Whit[admissible_Class[main]] Whit[admissible_Class[Outer]] Whit[admissible_Class[Inner1]] " +
		"Whit[admissible_Class[Inner2]] Notify[i2] Whit[completed_Class[Inner2]] Notify[outer_n] " +
		"Whit[completed_Class[Outer]] Notify[after] Notify[late] Whit[completed_Class[main]] Notify[i1] " +
		"Whit[completed_Class[Inner1]] Whit[completed_Stage[main]]"
	if got := strings.Join(walked(g), " "); got != want {
		t.Errorf("order =\n%s\nwant\n%s", got, want)
	}
	if paths := containmentPaths(c, nil); !slices.Equal(paths["Notify[i2]"], []string{"Stage[main]", "Inner2", "Notify[i2]"}) {
		t.Errorf("containment_path of i2 = %v, want the first containment edge's", paths["Notify[i2]"])
	}
}

// The walk order Puppet gives containers, over the applying resources.
func TestWhitOrderMatchesPuppet(t *testing.T) {
	mydef := func(title string) catalog.Resource {
		return catalog.Resource{Type: "Mydef", Title: title, Kind: "defined_type"}
	}
	for _, tt := range []struct {
		name string
		c    *catalog.Catalog
		want string
	}{
		{"3a", topScope([]catalog.Resource{n("free1", nil), res("Class", "Zz", nil), n("zz_b", nil), n("zz_a", nil),
			n("free2", nil)},
			[2]string{"Class[main]", "Notify[free1]"}, [2]string{"Stage[main]", "Class[Zz]"},
			[2]string{"Class[Zz]", "Notify[zz_b]"}, [2]string{"Class[Zz]", "Notify[zz_a]"},
			[2]string{"Class[main]", "Notify[free2]"}),
			"Notify[free1] Notify[zz_b] Notify[zz_a] Notify[free2]"},
		// The define's contents are appended to the array, so after_def and
		// last (5, 7) run before d1_x (8) although d1 (4) was declared first.
		{"3b", topScope([]catalog.Resource{n("before_def", nil), mydef("d1"), n("after_def", nil), mydef("d0"),
			n("last", nil), n("d1_x", nil), n("d1_y", nil), n("d0_x", nil), n("d0_y", nil)},
			[2]string{"Class[main]", "Notify[before_def]"}, [2]string{"Class[main]", "Mydef[d1]"},
			[2]string{"Class[main]", "Notify[after_def]"}, [2]string{"Class[main]", "Mydef[d0]"},
			[2]string{"Class[main]", "Notify[last]"}, [2]string{"Mydef[d1]", "Notify[d1_x]"},
			[2]string{"Mydef[d1]", "Notify[d1_y]"}, [2]string{"Mydef[d0]", "Notify[d0_x]"},
			[2]string{"Mydef[d0]", "Notify[d0_y]"}),
			"Notify[before_def] Notify[after_def] Notify[last] Notify[d1_x] Notify[d1_y] Notify[d0_x] Notify[d0_y]"},
		// A Stage-to-Stage edge orders every stage's contents: e1 (9) before
		// m1 (7), l1 (6) last.
		{"3d", topScope([]catalog.Resource{res("Stage", "pre", map[string]any{"before": "Stage[main]"}),
			res("Stage", "post", map[string]any{"require": "Stage[main]"}), res("Class", "Late", nil), n("l1", nil),
			n("m1", nil), res("Class", "Early", nil), n("e1", nil), n("m2", nil)},
			[2]string{"Stage[post]", "Class[Late]"}, [2]string{"Class[Late]", "Notify[l1]"},
			[2]string{"Class[main]", "Notify[m1]"}, [2]string{"Stage[pre]", "Class[Early]"},
			[2]string{"Class[Early]", "Notify[e1]"}, [2]string{"Class[main]", "Notify[m2]"}),
			"Notify[e1] Notify[m1] Notify[m2] Notify[l1]"},
		{"3h", topScope([]catalog.Resource{n("gate", nil), n("r2", map[string]any{"require": "Notify[gate]"}),
			res("Class", "Y", map[string]any{"require": "Notify[gate]"}), n("ym", nil),
			n("tail", map[string]any{"before": []any{"Notify[gate]"}})},
			[2]string{"Class[main]", "Notify[gate]"}, [2]string{"Class[main]", "Notify[r2]"},
			[2]string{"Stage[main]", "Class[Y]"}, [2]string{"Class[Y]", "Notify[ym]"},
			[2]string{"Class[main]", "Notify[tail]"}),
			"Notify[tail] Notify[gate] Notify[r2] Notify[ym]"},
		// Class[main]'s whits sort last, but its contents are blocked behind
		// its admissible whit, so the order is catalog order.
		{"1c", &catalog.Catalog{
			Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "Settings", nil),
				n("zeta", nil), n("alpha", nil), n("mid", nil), res("File", "/T", nil), n("beta", nil),
				res("Class", "main", nil)},
			Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[Settings]"},
				{Source: "Stage[main]", Target: "Class[main]"}, {Source: "Class[main]", Target: "Notify[zeta]"},
				{Source: "Class[main]", Target: "Notify[alpha]"}, {Source: "Class[main]", Target: "Notify[mid]"},
				{Source: "Class[main]", Target: "File[/T]"}, {Source: "Class[main]", Target: "Notify[beta]"}}},
			"Notify[zeta] Notify[alpha] Notify[mid] File[/T] Notify[beta]"},
	} {
		g, err := BuildGraph(context.Background(), tt.c, testRegistry)
		if err != nil {
			t.Fatalf("case %s: BuildGraph: %v", tt.name, err)
		}
		if got := strings.Join(resourceOrder(g), " "); got != tt.want {
			t.Errorf("case %s: order = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// A contain()ed class has two containment edges, and
// Puppet's pathbuilder follows the first (type.rb:2511-2513,
// graph/simple_graph.rb:349-353).
func TestContainmentPathTakesTheFirstEdge(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "Chrony", nil),
			res("Class", "Chrony::Config", nil), res("File", "/etc/chrony.conf", nil)},
		Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[Chrony]"},
			{Source: "Stage[main]", Target: "Class[Chrony::Config]"},
			{Source: "Class[Chrony]", Target: "Class[Chrony::Config]"},
			{Source: "Class[Chrony::Config]", Target: "File[/etc/chrony.conf]"}},
	}
	want := []string{"Stage[main]", "Chrony::Config", "File[/etc/chrony.conf]"}
	if got := containmentPaths(c, nil)["File[/etc/chrony.conf]"]; !slices.Equal(got, want) {
		t.Errorf("containment_path = %v, want %v", got, want)
	}
}

// A cycle through containers names each by its whit label, as Puppet prints a
// whit as its container, and never by a whit's own ref.
func TestCycleThroughAContainerNamesItsLabel(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Class", "a", map[string]any{"require": "Class[b]"}), n("na", nil),
			res("Class", "b", map[string]any{"require": "Class[a]"}), n("nb", nil)},
		Edges: []catalog.Edge{{Source: "Class[a]", Target: "Notify[na]"}, {Source: "Class[b]", Target: "Notify[nb]"}},
	}
	_, err := BuildGraph(context.Background(), c, testRegistry)
	var ce *graph.CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("BuildGraph error = %v, want a *graph.CycleError", err)
	}
	const want = "dependency cycle: Notify[na] -> Class[A] -> Class[B] -> Notify[nb] -> Class[B] -> Class[A] -> Notify[na]"
	if err.Error() != want || strings.Contains(err.Error(), "Whit[") {
		t.Errorf("error = %q\nwant    %q", err.Error(), want)
	}
}

// A repeated container is refused under its own ref, not a whit's.
func TestBuildGraphNamesARepeatedContainer(t *testing.T) {
	_, err := BuildGraph(context.Background(), &catalog.Catalog{Resources: []catalog.Resource{
		res("Class", "x", nil), res("Class", "x", nil)}}, testRegistry)
	if err == nil || err.Error() != "duplicate resource reference Class[x]" {
		t.Errorf("BuildGraph error = %v, want the repeated Class[x] named", err)
	}
}

// Puppet finds autorelations before it replaces containers with whits
// (relationship_graph.rb:24-33), so a stated relationship to a class does not
// suppress an autorequire between the class's member and the resource that
// required it. Measured with Puppet 8.10.0 `puppet apply`: Found 1 dependency
// cycle: (File[/tmp/t8bapp/conf] => Class[App] => File[/tmp/t8bapp] =>
// File[/tmp/t8bapp/conf]).
func TestAutoRequireIsNotSuppressedByARelationshipToTheContainer(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Class", "app", nil),
			res("File", "/opt/app/conf", map[string]any{"ensure": "file"}),
			res("File", "/opt/app", map[string]any{"ensure": "directory", "require": "Class[app]"})},
		Edges: []catalog.Edge{{Source: "Class[app]", Target: "File[/opt/app/conf]"}},
	}
	_, err := BuildGraph(context.Background(), c, testRegistry)
	var ce *graph.CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("BuildGraph error = %v, want a *graph.CycleError", err)
	}
	const want = "dependency cycle: File[/opt/app/conf] -> Class[App] -> File[/opt/app] -> File[/opt/app/conf]"
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err.Error(), want)
	}
}

// logContext returns a context whose run logger keeps entries at level, and the
// entries it keeps.
func logContext(level string) (context.Context, *[]report.LogEntry) {
	var got []report.LogEntry
	l := runlog.New(level, func(e report.LogEntry) { got = append(got, e) }, nil)
	return runlog.WithLogger(context.Background(), l), &got
}

// debugLines renders the debug entries as "<source>: <message>".
func debugLines(entries []report.LogEntry) []string {
	var out []string
	for _, e := range entries {
		if e.Level == "debug" {
			out = append(out, e.Source+": "+e.Message)
		}
	}
	return out
}

func buildLines(t *testing.T, level string, stub relStub, resources ...catalog.Resource) []string {
	t.Helper()
	ctx, got := logContext(level)
	if _, err := BuildGraph(ctx, &catalog.Catalog{Resources: resources}, ral.NewRegistry(stub)); err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return debugLines(*got)
}

func voxStub(reqs map[string][]string, rels map[string][]ral.AutoRelation) relStub {
	return relStub{evaluatorStub: evaluatorStub{typ: "vox_value", calls: new([]string)}, reqs: reqs, rels: rels}
}

// relationship_graph.rb:182 and :202, at the resource's path.
func TestBuildGraphLogsEachAutorelationKind(t *testing.T) {
	stub := voxStub(map[string][]string{"Vox_value[f]": {"Vox_value[b]"}}, map[string][]ral.AutoRelation{
		"Vox_value[a]": {{Kind: ral.RelNotify, Ref: "Vox_value[e]"}, {Kind: ral.RelBefore, Ref: "Vox_value[d]"},
			{Kind: ral.RelSubscribe, Ref: "Vox_value[c]"}, {Kind: ral.RelRequire, Ref: "Vox_value[b]"}}})
	got := buildLines(t, "debug", stub, res("Vox_value", "a", nil), res("Vox_value", "b", nil),
		res("Vox_value", "c", nil), res("Vox_value", "d", nil), res("Vox_value", "e", nil), res("Vox_value", "f", nil))
	want := []string{
		"/Vox_value[a]: Adding autorequire relationship with Vox_value[b]",
		"/Vox_value[a]: Adding autosubscribe relationship with Vox_value[c]",
		"/Vox_value[a]: Adding autobefore relationship with Vox_value[d]",
		"/Vox_value[a]: Adding autonotify relationship with Vox_value[e]",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("lines = %q, missing %q", got, w)
		}
	}
	// The provider's native autorequires log as autorequire too.
	if !slices.Contains(got, "/Vox_value[f]: Adding autorequire relationship with Vox_value[b]") {
		t.Errorf("lines = %q, missing f's autorequire of b", got)
	}
}

// relationship_graph.rb:180 skips, with a log line, an autorequire or
// autosubscribe whose pair a stated edge already joins the other way round, and
// an autobefore or autonotify whose pair it already joins the same way (:200).
// The opposite direction of each is skipped silently (:177, :197).
func TestBuildGraphLogsSkippedAutorelations(t *testing.T) {
	stub := voxStub(nil, map[string][]ral.AutoRelation{
		"Vox_value[a]": {{Kind: ral.RelRequire, Ref: "Vox_value[b]"}},
		"Vox_value[c]": {{Kind: ral.RelBefore, Ref: "Vox_value[d]"}},
		"Vox_value[e]": {{Kind: ral.RelRequire, Ref: "Vox_value[f]"}},
	})
	got := buildLines(t, "debug", stub,
		// a -> b stated; a's autorequire would be b -> a: reversed, logged.
		res("Vox_value", "a", map[string]any{"before": "Vox_value[b]"}), res("Vox_value", "b", nil),
		// c -> d stated; c's autobefore is c -> d: same direction, logged.
		res("Vox_value", "c", map[string]any{"before": "Vox_value[d]"}), res("Vox_value", "d", nil),
		// f -> e stated; e's autorequire is f -> e: same direction, silent.
		res("Vox_value", "e", nil), res("Vox_value", "f", map[string]any{"before": "Vox_value[e]"}))
	for _, w := range []string{
		"/Vox_value[a]: Skipping automatic relationship with Vox_value[b]",
		"/Vox_value[c]: Skipping automatic relationship with Vox_value[d]",
	} {
		if !slices.Contains(got, w) {
			t.Errorf("lines = %q, missing %q", got, w)
		}
	}
	for _, line := range got {
		if strings.Contains(line, "/Vox_value[e]") || strings.Contains(line, "Adding auto") {
			t.Errorf("line %q: want a silent skip for e and no Adding line for a pair a stated edge joins", line)
		}
	}
}

// type.rb:1551-1561 logs the metaparameter at "<path>/<metaparam>". Its
// "subscribes" and "notifies" branches never match, so those two read plain.
func TestBuildGraphLogsExplicitRelationships(t *testing.T) {
	got := buildLines(t, "debug", voxStub(nil, nil),
		res("Vox_value", "a", map[string]any{
			"require": "Vox_value[b]", "before": []any{"Vox_value[c]"},
			"subscribe": "Vox_value[d]", "notify": "Vox_value[e]"}),
		res("Vox_value", "b", nil), res("Vox_value", "c", nil), res("Vox_value", "d", nil), res("Vox_value", "e", nil))
	for _, w := range []string{
		"/Vox_value[a]/require: require to Vox_value[b]",
		"/Vox_value[a]/before: before to Vox_value[c]",
		"/Vox_value[a]/subscribe: subscribe to Vox_value[d]",
		"/Vox_value[a]/notify: notify to Vox_value[e]",
	} {
		if !slices.Contains(got, w) {
			t.Errorf("lines = %q, missing %q", got, w)
		}
	}
}

// The path is the containment path, not the bare reference.
func TestBuildGraphLogsAtTheContainmentPath(t *testing.T) {
	ctx, got := logContext("debug")
	c := &catalog.Catalog{
		Resources: []catalog.Resource{res("Stage", "main", nil), res("Class", "main", nil),
			res("Vox_value", "a", map[string]any{"require": "Vox_value[b]"}), res("Vox_value", "b", nil)},
		Edges: []catalog.Edge{{Source: "Stage[main]", Target: "Class[main]"},
			{Source: "Class[main]", Target: "Vox_value[a]"}, {Source: "Class[main]", Target: "Vox_value[b]"}},
	}
	if _, err := BuildGraph(ctx, c, ral.NewRegistry(voxStub(nil, nil))); err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	want := "/Stage[main]/Main/Vox_value[a]/require: require to Vox_value[b]"
	if lines := debugLines(*got); !slices.Contains(lines, want) {
		t.Errorf("lines = %q, missing %q", lines, want)
	}
}

func TestBuildGraphLogsNothingBelowDebug(t *testing.T) {
	stub := voxStub(nil, map[string][]ral.AutoRelation{"Vox_value[a]": {{Kind: ral.RelRequire, Ref: "Vox_value[b]"}}})
	for _, level := range []string{"notice", "info"} {
		ctx, got := logContext(level)
		_, err := BuildGraph(ctx, &catalog.Catalog{Resources: []catalog.Resource{
			res("Vox_value", "a", map[string]any{"notify": "Vox_value[c]"}), res("Vox_value", "b", nil), res("Vox_value", "c", nil)}},
			ral.NewRegistry(stub))
		if err != nil {
			t.Fatalf("BuildGraph: %v", err)
		}
		if len(*got) != 0 {
			t.Errorf("level %s: entries = %+v, want none", level, *got)
		}
	}
}

// warnStub raises "w-<title>" at graph build for every resource it is asked
// about, as concat_fragment raises its orphan warning (concat design §6).
type warnStub struct{ evaluatorStub }

func (warnStub) GraphWarnings(r catalog.Resource, _ *catalog.Catalog) []string {
	return []string{"w-" + r.Title}
}

// A warning a type raises while the graph is built is logged at "warning", at
// the resource's containment path, in catalog order, and before anything is
// applied -- with or without --debug.
func TestGraphWarningsLoggedInCatalogOrder(t *testing.T) {
	for _, level := range []string{"notice", "debug"} {
		t.Run(level, func(t *testing.T) {
			ctx, got := logContext(level)
			c := topScope([]catalog.Resource{res("Vox_w", "a", nil), res("Vox_w", "b", nil)},
				[2]string{"Class[main]", "Vox_w[a]"}, [2]string{"Class[main]", "Vox_w[b]"})
			if _, err := BuildGraph(ctx, c, ral.NewRegistry(warnStub{evaluatorStub{typ: "vox_w", calls: new([]string)}})); err != nil {
				t.Fatalf("BuildGraph: %v", err)
			}
			var warnings []string
			for _, e := range *got {
				if e.Level == "warning" {
					warnings = append(warnings, e.Source+": "+e.Message)
				}
			}
			want := []string{"/Stage[main]/Main/Vox_w[a]: w-a", "/Stage[main]/Main/Vox_w[b]: w-b"}
			if !slices.Equal(warnings, want) {
				t.Errorf("warnings = %q; want %q", warnings, want)
			}
		})
	}
}

// A provider without the hook logs no warning.
func TestNoGraphWarningsWithoutTheHook(t *testing.T) {
	ctx, got := logContext("debug")
	c := &catalog.Catalog{Resources: []catalog.Resource{res("Vox_value", "a", nil)}}
	if _, err := BuildGraph(ctx, c, ral.NewRegistry(voxStub(nil, nil))); err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	for _, e := range *got {
		if e.Level == "warning" {
			t.Errorf("unexpected warning %q", e.Message)
		}
	}
}
