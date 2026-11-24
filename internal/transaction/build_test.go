package transaction

import (
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

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
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	order := g.Order()
	if order[0] != "Stage[main]" || order[len(order)-1] != "Notify[hi]" {
		t.Errorf("Order() = %v, want Stage[main] first and Notify[hi] last", order)
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
			g, err := BuildGraph(c, testRegistry)
			if err != nil {
				t.Fatalf("BuildGraph returned error: %v", err)
			}
			order := g.Order()
			if order[0] != tt.wantSrc {
				t.Errorf("Order() = %v, want %s first", order, tt.wantSrc)
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
	g, err := BuildGraph(c, testRegistry)
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
	_, err := BuildGraph(c, testRegistry)
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
	if _, err := BuildGraph(c, testRegistry); err == nil {
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
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	order := g.Order()
	if order[0] != "Stage[main]" {
		t.Errorf("Order() = %v, want Stage[main] first (containment)", order)
	}
	pos := map[string]int{}
	for i, r := range order {
		pos[r] = i
	}
	if pos["File[conf]"] > pos["Exec[reload]"] {
		t.Errorf("Order() = %v: the notify relationship was dropped", order)
	}
	if got := g.RefreshTargets("File[conf]"); len(got) != 1 {
		t.Errorf("RefreshTargets(File[conf]) = %v, want one entry", got)
	}
}

// positions maps each reference to its index in the walk order.
func positions(t *testing.T, g interface{ Order() []string }) map[string]int {
	t.Helper()
	pos := map[string]int{}
	for i, r := range g.Order() {
		pos[r] = i
	}
	return pos
}

// A relationship whose endpoint is a container orders that container's CONTENTS,
// not the inert container vertex.
//
// Measured against Puppet 8.10.0, graph/relationship_graph.rb
// replace_containers_with_anchors: every container is replaced by an
// admissible/completed whit pair, each containment edge is rewired onto them,
// and `containers.each { |x| remove_vertex! x }` deletes the container itself.
// The contract that survives is "everything in A precedes everything in B".
//
// The contents are named so that a plain alphabetical tie-break yields the
// WRONG order. Without that, an implementation which drops the relationship
// entirely passes by accident -- which is exactly how this shipped.
func TestBuildGraphSplicesContainerOrdering(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Stage", "main", nil),
			res("Class", "A", map[string]any{"before": []any{"Class[B]"}}),
			res("Class", "B", nil),
			res("Notify", "zzz-inside-A", nil),
			res("Notify", "aaa-inside-B", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Stage[main]", Target: "Class[A]"},
			{Source: "Stage[main]", Target: "Class[B]"},
			{Source: "Class[A]", Target: "Notify[zzz-inside-A]"},
			{Source: "Class[B]", Target: "Notify[aaa-inside-B]"},
		},
	}
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["Notify[zzz-inside-A]"] > pos["Notify[aaa-inside-B]"] {
		t.Errorf("Order() = %v\nClass[A] before => Class[B] did not order their contents",
			g.Order())
	}
}

// A refresh notified to a container reaches the resources inside it.
//
// Puppet carries the edge's own label onto the rewired whit edge
// (`add_edge(completed[e.source], admissible[x], e.label)`), and Whit#refresh is
// a deliberate no-op so as to "not break the chain of propagation" -- so
// `notify => Class[x]` restarts everything in x. Dropping it silently is the
// inverse of the failure the refresh guard exists to prevent: not a service
// restarting every run, but one that never restarts at all.
func TestBuildGraphSplicesContainerRefresh(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("File", "conf", map[string]any{"notify": []any{"Class[B]"}}),
			res("Class", "B", nil),
			res("Exec", "reload", nil),
		},
		Edges: []catalog.Edge{{Source: "Class[B]", Target: "Exec[reload]"}},
	}
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("File[conf]"); len(got) != 1 || got[0] != "Exec[reload]" {
		t.Errorf("RefreshTargets(File[conf]) = %v, want [Exec[reload]]", got)
	}
}

// Containers nest, and Puppet's admissible/completed maps recurse through them,
// so a relationship declared BY an outer class must be honoured by a resource
// held by an inner one.
//
// The outgoing direction is the one that can fail. Containment already orders
// `Class[outer]` before its contents, so "something before the class" is
// transitively before the contents for free, and a test in that direction
// passes whether or not splicing happens. Only "everything inside the class
// before something outside it" requires the splice -- containment supplies
// `outer -> after` and `outer -> contents`, never `contents -> after`.
// `zzz-deep` and `aaa-after` are named so the alphabetical tie-break is wrong
// too.
func TestBuildGraphSplicesThroughNestedContainers(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Class", "outer", map[string]any{"before": []any{"Notify[aaa-after]"}}),
			res("Class", "outer::inner", nil),
			res("Notify", "zzz-deep", nil),
			res("Notify", "aaa-after", nil),
		},
		Edges: []catalog.Edge{
			{Source: "Class[outer]", Target: "Class[outer::inner]"},
			{Source: "Class[outer::inner]", Target: "Notify[zzz-deep]"},
		},
	}
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["Notify[zzz-deep]"] > pos["Notify[aaa-after]"] {
		t.Errorf("Order() = %v\nthe relationship did not reach through the nested class",
			g.Order())
	}
}

// An empty container contributes no edges and is not an error. Puppet emits
// `add_edge(admissible[x], completed[x]) if contents.empty?`, which constrains
// only the whits and so orders no real resource.
//
// Asserted through notify rather than before, so the assertion has something to
// be wrong about: an implementation that does not splice returns the container
// itself as a refresh target, and the transaction would then try to refresh a
// vertex that applies nothing.
func TestBuildGraphAcceptsRelationshipToEmptyContainer(t *testing.T) {
	c := &catalog.Catalog{
		Resources: []catalog.Resource{
			res("Notify", "gate", map[string]any{"notify": []any{"Class[empty]"}}),
			res("Class", "empty", nil),
		},
	}
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.RefreshTargets("Notify[gate]"); len(got) != 0 {
		t.Errorf("RefreshTargets(Notify[gate]) = %v, want none -- a container cannot refresh", got)
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
	got := containmentPaths(c)

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
	g, err := BuildGraph(c, testRegistry)
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
	g, err := BuildGraph(&catalog.Catalog{}, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	if got := g.Order(); len(got) != 0 {
		t.Errorf("Order() = %v, want empty", got)
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
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph returned error: %v", err)
	}
	pos := positions(t, g)
	if pos["File[/opt/spine]"] > pos["File[/opt/spine/conf]"] {
		t.Errorf("Order() = %v, want the directory before the file it holds", g.Order())
	}
}

// An autorequire carries no refresh. Puppet sets `edge.event = :NONE` for one.
func TestBuildGraphAutoRequireDoesNotRefresh(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/opt/spine", map[string]any{"ensure": "directory"}),
		res("File", "/opt/spine/conf", map[string]any{"ensure": "file"}),
	}}
	g, err := BuildGraph(c, testRegistry)
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
	g, err := BuildGraph(c, testRegistry)
	if err != nil {
		t.Fatalf("BuildGraph reported an error, so the autorequire formed a cycle "+
			"with the explicit relationship: %v", err)
	}
	pos := positions(t, g)
	if pos["File[/opt/spine/conf]"] > pos["File[/opt/spine]"] {
		t.Errorf("Order() = %v, want the explicit require to win", g.Order())
	}
}

// Autorequire is SOFT: naming an unmanaged path is no edge, not an error.
// An explicit relationship to an absent resource is could-not-run; these are
// not, and conflating them would refuse most real catalogs.
func TestBuildGraphAutoRequireToUnmanagedPathIsNotAnError(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Exec", "e", map[string]any{"command": "/usr/bin/touch /tmp/x"}),
	}}
	if _, err := BuildGraph(c, testRegistry); err != nil {
		t.Errorf("BuildGraph refused a command naming an unmanaged file: %v", err)
	}
}
