// Package transaction walks a catalog's graph and applies it.
package transaction

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/graph"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
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

// UnresolvableError is Puppet's refusal of a relationship naming a resource
// by a catalog title it cannot look up (type.rb:1516-1524, configurer.rb:507).
type UnresolvableError struct{ Reference, Of string }

func (e *UnresolvableError) Error() string {
	return fmt.Sprintf("Failed to apply catalog: Could not retrieve dependency '%s' of %s", e.Reference, e.Of)
}

// BuildGraph builds the graph a catalog implies, logging Ruby's debug lines for
// the relationships it finds through the run logger on ctx.
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
//
// Containers become whits, as graph/relationship_graph.rb:236-285
// replace_containers_with_anchors makes them: every container X is replaced by
// an admissible and a completed whit, both keyed at X's catalog index. The
// relationships and autorelations are found first, on the catalog's refs
// (populate_from does the same, :24-33), and then rewired onto the whits.
//
// A whit is a vertex of its own that takes its turn in the ready queue. It is
// not equivalent to splicing every edge through to the container's members:
// a measured case gave `xm r d` where the splice gives `xm d r`,
// because a completed whit released with a larger key than a ready resource
// waits behind it, and so do the resources only the whit releases.
func BuildGraph(ctx context.Context, c *catalog.Catalog, reg *ral.Registry) (*graph.Graph, error) {
	vertices := make([]graph.Vertex, 0, len(c.Resources))
	known := make(map[string]struct{}, len(c.Resources))
	containers := make(map[string]struct{})
	for i, r := range c.Resources {
		ref := r.Ref()
		known[ref] = struct{}{}
		if !ral.IsContainerResource(r) {
			vertices = append(vertices, graph.Vertex{Ref: ref, Key: i})
			continue
		}
		// graph.New refuses a repeated ref; checked here so the message
		// names the container rather than one of its whits.
		if _, dup := containers[ref]; dup {
			return nil, fmt.Errorf("duplicate resource reference %s", ref)
		}
		containers[ref] = struct{}{}
		label := ref
		if strings.EqualFold(r.Type, "class") {
			label = "Class[" + capitaliseClassTitle(r.Title) + "]"
		}
		admissible, completed := whitRefs(ref)
		vertices = append(vertices, graph.Vertex{Ref: admissible, Key: i, Whit: true, Label: label},
			graph.Vertex{Ref: completed, Key: i, Whit: true, Label: label})
	}
	// A[v] and C[v]: a container's whits, or v itself.
	admissibleOf := func(ref string) string {
		if _, ok := containers[ref]; ok {
			a, _ := whitRefs(ref)
			return a
		}
		return ref
	}
	completedOf := func(ref string) string {
		if _, ok := containers[ref]; ok {
			_, cmp := whitRefs(ref)
			return cmp
		}
		return ref
	}

	stated := make([]graph.Edge, 0, len(c.Edges))
	for _, e := range c.Edges {
		stated = append(stated, graph.Edge{Source: e.Source, Target: e.Target})
	}
	aliases := resourceAliases(c, reg)
	display, _, unresolvable := puppetNames(c, reg)
	logs := relationshipLog{ctx: ctx, paths: containmentPaths(c, display), display: display}

	var dependencies []graph.Edge
	for _, r := range c.Resources {
		declarer := r.Ref()
		for _, rel := range relationships {
			for _, target := range refList(r.Parameters[rel.param]) {
				// Checked as written: a target named through an alias
				// resolves in Puppet too.
				if _, refused := unresolvable[target]; refused {
					of, ok := display[declarer]
					if !ok {
						of = declarer
					}
					return nil, &UnresolvableError{Reference: target, Of: of}
				}
				if canon, ok := aliases[target]; ok {
					target = canon
				}
				if _, ok := known[target]; !ok {
					return nil, fmt.Errorf("%s declares %s => %s, which is not in the catalog",
						declarer, rel.param, target)
				}
				// type.rb:1551-1561. Ruby's "subscribes" and "notifies" branches
				// compare a Symbol with Strings and never match, so every
				// metaparameter prints under its own name.
				logs.debug(declarer, "/"+rel.param, "%s to %s", rel.param, logs.name(target))
				edge := graph.Edge{Source: declarer, Target: target, Refresh: rel.refresh}
				if !rel.forward {
					edge.Source, edge.Target = target, declarer
				}
				dependencies = append(dependencies, edge)
			}
		}
	}
	dependencies = append(dependencies, autoRequireEdges(c, reg, known, aliases, append(stated, dependencies...), logs)...)

	// relationship_graph.rb:265-283. Every containment edge X -> v gives
	// A_X -> A[v] and C[v] -> C_X with Default_label, which refreshes, so a
	// contain()ed class, which has two containment edges, is under both of
	// its containers. A container with no members gets an
	// unlabelled A_X -> C_X.
	edges := make([]graph.Edge, 0, len(c.Edges)*2+len(dependencies))
	hasMembers := make(map[string]bool, len(containers))
	for _, e := range c.Edges {
		if _, ok := containers[e.Source]; !ok {
			edges = append(edges, graph.Edge{Source: e.Source, Target: admissibleOf(e.Target)})
			continue
		}
		hasMembers[e.Source] = true
		edges = append(edges,
			graph.Edge{Source: admissibleOf(e.Source), Target: admissibleOf(e.Target), Refresh: true},
			graph.Edge{Source: completedOf(e.Target), Target: completedOf(e.Source), Refresh: true})
	}
	for _, r := range c.Resources {
		if _, ok := containers[r.Ref()]; ok && !hasMembers[r.Ref()] {
			admissible, completed := whitRefs(r.Ref())
			edges = append(edges, graph.Edge{Source: admissible, Target: completed})
		}
	}
	for _, e := range dependencies {
		edges = append(edges, graph.Edge{Source: completedOf(e.Source), Target: admissibleOf(e.Target), Refresh: e.Refresh})
	}
	return graph.New(vertices, edges)
}

// relationshipLog writes the debug lines Ruby's graph build emits for a
// resource, at its containment path (the walk's log source) plus a suffix for a
// parameter. paths and display are those containmentPaths and puppetNames
// return.
type relationshipLog struct {
	ctx     context.Context
	paths   map[string][]string
	display map[string]string
}

func (l relationshipLog) debug(ref, suffix, format string, args ...any) {
	if !runlog.From(l.ctx).Enabled("debug") {
		return
	}
	runlog.Debugf(l.ctx, "/"+strings.Join(l.paths[ref], "/")+suffix, format, args...)
}

// name is the reference Ruby prints for ref: Puppet's own where it differs from
// the catalog's.
func (l relationshipLog) name(ref string) string {
	if shown, ok := l.display[ref]; ok {
		return shown
	}
	return ref
}

// whitRefs names container's two whits as Puppet names them
// (relationship_graph.rb:236-285): Whit[admissible_<ref>] and Whit[completed_<ref>].
func whitRefs(container string) (admissible, completed string) {
	return admissibleWhitPrefix + container + "]", completedWhitPrefix + container + "]"
}

const (
	admissibleWhitPrefix = "Whit[admissible_"
	completedWhitPrefix  = "Whit[completed_"
)

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
// The guards see every edge already in the graph, automatic ones included, so
// the first edge joining a pair wins: an explicit relationship beats an
// automatic one, and an earlier automatic one beats a later one. Per resource,
// in catalog order, the kinds run require, subscribe, before, notify. Two
// resources can therefore never form a cycle between the same pair. A longer cycle is still possible,
// and graph.New reports it, which is also what Puppet does.
//
// Autorequire and autobefore edges carry no refresh: type.rb sets
// `edge.event = :NONE` for them, so a file being rewritten never restarts the
// service that happens to autorequire it. Autosubscribe and autonotify edges
// refresh on every event (graph/relationship_graph.rb:172-212).
//
// Each edge is logged as Ruby logs it, at the resource's path. The two guards
// are not symmetric there: an autorequire or autosubscribe skips silently when
// the pair is already joined the same way (:177) and logs "Skipping" when it is
// joined the other way (:180); an autobefore or autonotify does the opposite
// (:197, :200).
func autoRequireEdges(c *catalog.Catalog, reg *ral.Registry, known map[string]struct{}, aliases map[string]string,
	stated []graph.Edge, logs relationshipLog) []graph.Edge {

	managed := func(ref string) bool {
		_, ok := known[ref]
		if !ok {
			_, ok = aliases[ref]
		}
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
		// relationship_graph.rb build_autorelation_dependencies' two guards,
		// for every kind alike: a stated edge either way suppresses the auto one.
		add := func(kind, source, target string, refresh bool) {
			if canon, ok := aliases[source]; ok {
				source = canon
			}
			if canon, ok := aliases[target]; ok {
				target = canon
			}
			if source == target {
				return
			}
			_, same := statedPair[[2]string{source, target}]
			_, reversed := statedPair[[2]string{target, source}]
			other, silent, skipped := source, same, reversed
			if kind == "before" || kind == "notify" {
				other, silent, skipped = target, reversed, same
			}
			if silent {
				return
			}
			if skipped {
				logs.debug(ref, "", "Skipping automatic relationship with %s", logs.name(other))
				return
			}
			logs.debug(ref, "", "Adding auto%s relationship with %s", kind, logs.name(other))
			// Later auto edges, of this resource or another, see this one too.
			statedPair[[2]string{source, target}] = struct{}{}
			out = append(out, graph.Edge{Source: source, Target: target, Refresh: refresh})
		}
		for _, before := range provider.AutoRequires(r, managed) {
			add("require", before, ref, false)
		}
		if ar, ok := provider.(ral.AutoRelating); ok {
			// Puppet's order, whatever order the provider lists them in:
			// [:require, :subscribe] then [:before, :notify].
			rels := ar.AutoRelations(r)
			for _, kind := range [...]ral.RelationKind{ral.RelRequire, ral.RelSubscribe, ral.RelBefore, ral.RelNotify} {
				for _, rel := range rels {
					if rel.Kind != kind {
						continue
					}
					switch kind {
					case ral.RelRequire:
						add("require", rel.Ref, ref, false)
					case ral.RelSubscribe:
						add("subscribe", rel.Ref, ref, true)
					case ral.RelBefore:
						add("before", ref, rel.Ref, false)
					case ral.RelNotify:
						add("notify", ref, rel.Ref, true)
					}
				}
			}
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
//
// display maps a catalog ref to Puppet's own ref for the resource, where they
// differ (puppetNames). The paths stay keyed by catalog ref.
func containmentPaths(c *catalog.Catalog, display map[string]string) map[string][]string {
	// A contain()ed class has two containment edges; the pathbuilder
	// follows the first (type.rb:2511-2513, graph/simple_graph.rb:349-353).
	parent := make(map[string]string, len(c.Edges))
	for _, e := range c.Edges {
		if _, ok := parent[e.Target]; !ok {
			parent[e.Target] = e.Source
		}
	}
	name := make(map[string]string, len(c.Resources))
	for _, r := range c.Resources {
		if strings.EqualFold(r.Type, "class") {
			name[r.Ref()] = capitaliseClassTitle(r.Title)
			continue
		}
		if ref, ok := display[r.Ref()]; ok {
			name[r.Ref()] = ref
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

// fileAliases maps File[<path>] to the resource's own ref for every File
// whose path differs from its title. Puppet's catalog adds that alias on the
// uniqueness key (resource/catalog.rb:176-178), and a compiled catalog keeps
// a relationship that names the path as written (measured: "require":
// "File[/x/n]" for title nsx), so the graph must resolve it.
func fileAliases(c *catalog.Catalog) map[string]string {
	out := map[string]string{}
	for _, r := range c.Resources {
		if r.Type != "File" {
			continue
		}
		if alias := "File[" + ral.FilePath(r) + "]"; alias != r.Ref() {
			out[alias] = r.Ref()
		}
	}
	return out
}

// resourceAliases is fileAliases plus the names a provider reports for its own
// resources, such as a type whose name differs from its title.
func resourceAliases(c *catalog.Catalog, reg *ral.Registry) map[string]string {
	out := fileAliases(c)
	for _, r := range c.Resources {
		p, ok := reg.Provider(r.Type)
		if !ok {
			continue
		}
		if a, ok := p.(ral.Aliasing); ok {
			for _, alias := range a.Aliases(r) {
				out[alias] = r.Ref()
			}
		}
	}
	return out
}

// puppetNames maps the catalog ref of every resource Puppet names by another
// ref (ral.Retitling) to that ref, and to Puppet's title for it. The report
// uses them; the walk, the graph and the host keep the catalog's. unresolvable
// holds the catalog refs Puppet's catalog cannot look a resource up by
// (resource/catalog.rb:378-395).
func puppetNames(c *catalog.Catalog, reg *ral.Registry) (display, titles map[string]string,
	unresolvable map[string]struct{}) {

	display, titles, unresolvable = map[string]string{}, map[string]string{}, map[string]struct{}{}
	for _, r := range c.Resources {
		p, ok := reg.Provider(r.Type)
		if !ok {
			continue
		}
		rt, ok := p.(ral.Retitling)
		if !ok {
			continue
		}
		if ref, title, ok := rt.PuppetRef(r); ok {
			display[r.Ref()], titles[r.Ref()] = ref, title
		}
		if rt.Unresolvable(r) {
			unresolvable[r.Ref()] = struct{}{}
		}
	}
	return display, titles, unresolvable
}
