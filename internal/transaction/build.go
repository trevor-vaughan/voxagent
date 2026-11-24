// Package transaction walks a catalog's graph and applies it.
package transaction

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/graph"
	"github.com/voxagent/voxagent/internal/ral"
)

// relationships maps each relationship metaparameter to its edge direction and
// whether it carries a refresh.
//
// forward means the declaring resource is the source: `before` and `notify`
// order the declarer first. `require` and `subscribe` invert that.
var relationships = []struct {
	param   string
	forward bool
	refresh bool
}{
	{"before", true, false},
	{"notify", true, true},
	{"require", false, false},
	{"subscribe", false, true},
}

// BuildGraph builds the graph a catalog implies.
//
// Two sources, because a catalog carries its two kinds of ordering in two
// different places -- measured against Puppet 8.10.0, not assumed:
//
//   - The catalog's own `edges` array holds CONTAINMENT only:
//     Stage[main] -> Class[main] -> Node[default] -> Resource.
//   - A `~>` relationship appears nowhere in that array. It arrives as a
//     resource parameter, e.g. File[x] carrying notify => ['Exec[y]'].
//
// A graph built from `edges` alone would therefore silently drop every
// dependency and every refresh in the catalog, and would still produce a valid
// topological order -- so nothing downstream would notice.
// The registry is needed for autorelations, which are per-type rules the
// providers own -- a file autorequires its parent directory, an exec the files
// its command names. Deriving them here instead would put type knowledge in the
// engine, which is the boundary this package exists to keep.
func BuildGraph(c *catalog.Catalog, reg *ral.Registry) (*graph.Graph, error) {
	refs := make([]string, 0, len(c.Resources))
	known := make(map[string]struct{}, len(c.Resources))
	for _, r := range c.Resources {
		ref := r.Ref()
		refs = append(refs, ref)
		known[ref] = struct{}{}
	}

	edges := make([]graph.Edge, 0, len(c.Edges))
	for _, e := range c.Edges {
		edges = append(edges, graph.Edge{Source: e.Source, Target: e.Target})
	}

	members := containerMembers(c)

	for _, r := range c.Resources {
		declarer := r.Ref()
		for _, rel := range relationships {
			for _, target := range refList(r.Parameters[rel.param]) {
				if _, ok := known[target]; !ok {
					return nil, fmt.Errorf("%s declares %s => %s, which is not in the catalog",
						declarer, rel.param, target)
				}
				edge := graph.Edge{Source: declarer, Target: target, Refresh: rel.refresh}
				if !rel.forward {
					edge.Source, edge.Target = target, declarer
				}
				edges = append(edges, spliceContainers(edge, members)...)
			}
		}
	}

	return graph.New(refs, append(edges, autoRequireEdges(c, reg, known, edges)...))
}

// autoRequireEdges returns the ordering every type implies but no catalog
// states.
//
// Measured against Puppet 8.10.0, relationship_graph.rb
// build_autorelation_dependencies, whose two guards are the whole of the
// conflict rule:
//
//	next if edge?(edge.source, edge.target)     # already stated -- no duplicate
//	if edge?(edge.target, edge.source)          # stated the other way round
//	  debug "Skipping automatic relationship"   # -- the manual edge wins
//
// So an explicit relationship always beats an autorequire, and the two can
// never form a cycle between the same pair. A longer cycle is still possible,
// and graph.New reports it, which is also what Puppet does.
//
// The edges carry no refresh: type.rb sets `edge.event = :NONE` for an
// autorequire, so a file being rewritten never restarts the service that
// happens to autorequire it.
func autoRequireEdges(c *catalog.Catalog, reg *ral.Registry, known map[string]struct{},
	stated []graph.Edge) []graph.Edge {

	managed := func(ref string) bool {
		_, ok := known[ref]
		return ok
	}
	// Pairs in BOTH directions, because a stated edge either way suppresses the
	// automatic one.
	statedPair := make(map[[2]string]struct{}, len(stated))
	for _, e := range stated {
		statedPair[[2]string{e.Source, e.Target}] = struct{}{}
	}

	var out []graph.Edge
	for _, r := range c.Resources {
		provider, ok := reg.Provider(r.Type)
		if !ok {
			// A container, or a type pre-flight will refuse. Either way it
			// declares no autorelations.
			continue
		}
		ref := r.Ref()
		for _, before := range provider.AutoRequires(r, managed) {
			if before == ref {
				continue
			}
			if _, stated := statedPair[[2]string{before, ref}]; stated {
				continue
			}
			if _, reversed := statedPair[[2]string{ref, before}]; reversed {
				continue
			}
			out = append(out, graph.Edge{Source: before, Target: ref})
		}
	}
	return out
}

// containmentPaths returns, for each resource, the chain of containers holding
// it, ending with the resource itself.
//
// Measured against Puppet 8.10.0, type/component.rb pathbuilder: a component
// whose reference type is Class contributes its TITLE, and everything else
// contributes its full reference. So a file inside the implicit main class
// under a node reads ["Stage[main]", "Main", "Node[default]", "File[...]"] --
// the Stage and the Node keep their brackets, the Class loses them.
//
// A resource nothing contains is a path of one. Puppet's pathbuilder terminates
// the same way, on a resource with no parent.
func containmentPaths(c *catalog.Catalog) map[string][]string {
	parent := make(map[string]string, len(c.Edges))
	for _, e := range c.Edges {
		parent[e.Target] = e.Source
	}
	name := make(map[string]string, len(c.Resources))
	for _, r := range c.Resources {
		if strings.EqualFold(r.Type, "class") {
			name[r.Ref()] = capitaliseClassTitle(r.Title)
			continue
		}
		name[r.Ref()] = r.Ref()
	}

	paths := make(map[string][]string, len(c.Resources))
	for _, r := range c.Resources {
		ref := r.Ref()
		var chain []string
		// The guard is against a containment cycle, which is malformed rather
		// than expected -- but a hang here would be indistinguishable from a
		// slow run, and the graph is the thing that should report it.
		seen := map[string]struct{}{}
		for at := ref; at != ""; at = parent[at] {
			if _, loop := seen[at]; loop {
				break
			}
			seen[at] = struct{}{}
			label, known := name[at]
			if !known {
				label = at
			}
			chain = append(chain, label)
		}
		slices.Reverse(chain)
		paths[ref] = chain
	}
	return paths
}

// capitaliseClassTitle upper-cases each :: segment of a class name.
//
// A compiled catalog already spells user classes this way -- Class[Settings],
// Class[Deep::Nested_thing] -- so for those this is the identity. The implicit
// main class is the exception: the catalog stores Class[main] and Ruby reports
// it as "Main", because a class reference is canonicalised when it is built.
func capitaliseClassTitle(title string) string {
	segments := strings.Split(title, "::")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		segments[i] = strings.ToUpper(segment[:1]) + segment[1:]
	}
	return strings.Join(segments, "::")
}

// containerMembers resolves every container to the applying resources it holds,
// transitively through nested containers.
//
// Containment is what the catalog's own edges array carries, so this reads that
// array and nothing else. A container that holds only other containers resolves
// to their contents; an empty one resolves to nothing.
//
// The visiting set guards against a containment cycle. A catalog carrying one is
// malformed, but a malformed catalog must produce an error from graph.New rather
// than a hang here.
func containerMembers(c *catalog.Catalog) map[string][]string {
	contents := make(map[string][]string, len(c.Edges))
	for _, e := range c.Edges {
		contents[e.Source] = append(contents[e.Source], e.Target)
	}
	isContainer := make(map[string]bool, len(c.Resources))
	for _, r := range c.Resources {
		isContainer[r.Ref()] = ral.IsContainer(r.Type)
	}

	resolved := make(map[string][]string)
	visiting := make(map[string]bool)

	var walk func(string) []string
	walk = func(ref string) []string {
		if done, ok := resolved[ref]; ok {
			return done
		}
		if visiting[ref] {
			return nil
		}
		visiting[ref] = true
		defer delete(visiting, ref)

		seen := make(map[string]struct{})
		var out []string
		for _, child := range contents[ref] {
			var reach []string
			if isContainer[child] {
				reach = walk(child)
			} else {
				reach = []string{child}
			}
			for _, m := range reach {
				if _, dup := seen[m]; dup {
					continue
				}
				seen[m] = struct{}{}
				out = append(out, m)
			}
		}
		sort.Strings(out)
		resolved[ref] = out
		return out
	}

	for ref, container := range isContainer {
		if container {
			walk(ref)
		}
	}
	return resolved
}

// spliceContainers rewrites an edge whose endpoints are containers into edges
// between the resources those containers hold.
//
// Measured against Puppet 8.10.0, graph/relationship_graph.rb
// replace_containers_with_anchors: each container becomes an
// admissible/completed whit pair, every edge touching it is rewired onto them
// carrying its own label, and the container vertex is then deleted. The
// surviving contract is "everything in the source precedes everything in the
// target", with the refresh callback riding along -- Whit#refresh is a
// deliberate no-op so as not to "break the chain of propagation".
//
// Ordering two inert container vertices against each other instead, as this did
// before, discards every class-level relationship silently: the walk still has a
// valid topological order, so nothing downstream notices. That is the
// roles-and-profiles idiom, so it is most of the ordering in real Puppet code.
//
// The original edge is kept as well, but with its refresh STRIPPED. Keeping it
// costs only a graph edge -- both endpoints apply nothing -- and it preserves
// cycle detection for a catalog whose containers are mutually dependent but
// empty, where the spliced set is necessarily empty and the cycle would
// otherwise vanish. Its refresh must not survive, or the transaction would hold
// a container in its pending-refresh set: Puppet's whit is refresh-aware for the
// same reason, and does nothing when refreshed.
func spliceContainers(e graph.Edge, members map[string][]string) []graph.Edge {
	sources, sourceIsContainer := members[e.Source]
	targets, targetIsContainer := members[e.Target]
	if !sourceIsContainer && !targetIsContainer {
		return []graph.Edge{e}
	}
	if !sourceIsContainer {
		sources = []string{e.Source}
	}
	if !targetIsContainer {
		targets = []string{e.Target}
	}

	ordering := e
	ordering.Refresh = false
	out := []graph.Edge{ordering}
	for _, s := range sources {
		for _, t := range targets {
			out = append(out, graph.Edge{Source: s, Target: t, Refresh: e.Refresh})
		}
	}
	return out
}

// refList normalises a relationship metaparameter's value.
//
// BOTH shapes occur in real catalogs, measured against Puppet 8.10.0 by
// compiling all three declaration forms:
//
//	notify => Notify['one']                      -> "Notify[one]"      (string)
//	notify => [Notify['one'], Notify['two']]     -> [...]              (array)
//	Notify['a'] -> Notify['b']                   -> [...]              (array)
//
// So a single-target metaparameter is a scalar and everything else is an array;
// a chaining arrow is an array even with one target. An earlier version of this
// comment claimed every catalog carries an array and the scalar was a schema
// allowance -- which would make the scalar branch look like dead code to the
// next reader, when it is the shape a hand-written `notify => Service[x]`
// produces.
func refList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	case []string:
		return t
	default:
		return []string{fmt.Sprintf("%v", t)}
	}
}
