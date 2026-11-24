package ral

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

func testRegistry() *Registry {
	return NewRegistry(
		stub{typ: "file", attrs: NewAttrs("ensure", "content", "mode", "owner", "group")},
		stub{typ: "notify", attrs: NewAttrs("message", "withpath")},
	)
}

// stubValueProvider implements ensure => present|absent and nothing else. It
// embeds stub so it inherits the six provider methods and adds only the value
// predicate, which is the whole of what ValueBounded asks for.
type stubValueProvider struct{ stub }

func (stubValueProvider) SupportsValue(attr, value string) bool {
	if attr != "ensure" {
		return true
	}
	return value == "present" || value == "absent"
}

// Prefetch must be overridden, not inherited. Go binds a promoted method's
// receiver to the EMBEDDED value, so stub's "return the receiver" would hand
// back the inner stub and silently drop SupportsValue -- leaving a fixture that
// still satisfies Provider while no longer being ValueBounded. Nothing calls
// Prefetch yet; the transaction starts calling it on every registered provider
// in a later task, and a test registering this fixture there would then pass
// while exercising the wrong thing.
func (s stubValueProvider) Prefetch(context.Context, []catalog.Resource) (Provider, error) {
	return s, nil
}

// valueRegistry registers a value-bounded provider alongside the plain ones, so
// a test can show that bounding values changes nothing for providers that do
// not bound them.
func valueRegistry() *Registry {
	return NewRegistry(
		stub{typ: "file", attrs: NewAttrs("ensure", "content", "mode", "owner", "group")},
		stubValueProvider{stub{typ: "stub", attrs: NewAttrs("ensure", "name")}},
		// Bounds ensure's values but does not implement the ensure attribute at
		// all -- the state a provider is in partway through a port, where the
		// value predicate is written before the attribute is wired. It is the
		// only shape that can exercise the double-report guard: for any other
		// attribute the predicate answers true, so no second gap could arise
		// whether the guard is there or not.
		stubValueProvider{stub{typ: "halfstub", attrs: NewAttrs("name")}},
	)
}

// An attribute that IS implemented but carries a value that is not must refuse
// at pre-flight, not at apply. A run that applies the rest of the catalog and
// fails one resource leaves the node half-converged, which produces state-diff
// divergences that are artifacts of coverage rather than defects.
func TestPreflightRefusesAnUnimplementedAttributeValue(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Stub", "a", map[string]any{"ensure": "purged"}),
	}}
	gaps := Preflight(valueRegistry(), c)
	if len(gaps) != 1 {
		t.Fatalf("Preflight returned %d gaps, want 1: %v", len(gaps), gaps)
	}
	if gaps[0].Attribute != "ensure" || gaps[0].Value != "purged" {
		t.Errorf("gap = %+v, want Attribute=ensure Value=purged", gaps[0])
	}
	// The message must name the value: "attribute ensure is not implemented"
	// would send an operator porting the attribute, which already exists.
	want := `Stub[a]: ensure => "purged" is not implemented`
	if got := gaps[0].String(); got != want {
		t.Errorf("Gap.String() = %q, want %q", got, want)
	}
}

// An implemented value produces no gap. Without this, the test above passes
// against a Preflight that refuses every value it sees.
func TestPreflightAcceptsAnImplementedAttributeValue(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Stub", "a", map[string]any{"ensure": "present", "name": "a"}),
		raltest.Res("Stub", "b", map[string]any{"ensure": "absent"}),
		// A provider that does not implement ValueBounded is unaffected:
		// "directory" is not in the stub's present|absent domain, and file
		// must still accept it.
		raltest.Res("File", "/tmp/a", map[string]any{"ensure": "directory"}),
	}}
	if gaps := Preflight(valueRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps on implemented values: %v", gaps)
	}
}

// An UNIMPLEMENTED attribute yields exactly one gap, not two. The value check
// must not also fire for an attribute that already failed the attribute check.
func TestPreflightDoesNotDoubleReportAnUnimplementedAttribute(t *testing.T) {
	// Halfstub rejects ensure => "purged" AND does not implement ensure, so a
	// missing guard reports the attribute and the value both.
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Halfstub", "a", map[string]any{"ensure": "purged"}),
	}}
	gaps := Preflight(valueRegistry(), c)
	if len(gaps) != 1 {
		t.Fatalf("Preflight returned %d gaps, want 1: %v", len(gaps), gaps)
	}
	// Double-reporting would inflate the porting histogram this list feeds, and
	// would tell an operator to port a value of an attribute that has no code.
	if gaps[0].Value != "" {
		t.Errorf("gap = %+v, want an attribute gap with no Value", gaps[0])
	}
	want := `Halfstub[a]: attribute "ensure" is not implemented`
	if got := gaps[0].String(); got != want {
		t.Errorf("Gap.String() = %q, want %q", got, want)
	}
}

// A non-string value is bounded as the string the provider will act on.
// Providers read parameters through fmt's %v (rubyvalue.ParamString), so a JSON true
// reaches Apply as "true" and must be judged as "true" here. Skipping
// non-strings let {"ensure": false} through to apply time, where it failed one
// resource after the rest of the catalog had been written.
func TestPreflightBoundsNonStringValuesAsTheProviderReadsThem(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Stub", "a", map[string]any{"ensure": true}),
		raltest.Res("Stub", "b", map[string]any{"ensure": 42}),
		raltest.Res("Stub", "c", map[string]any{"ensure": []any{"purged"}}),
		raltest.Res("Stub", "d", map[string]any{"ensure": false}),
		raltest.Res("Stub", "e", map[string]any{"ensure": nil}),
		raltest.Res("Stub", "f", map[string]any{"ensure": map[string]any{"k": "v"}}),
		// name is implemented and not bounded: a non-string there is not a gap.
		raltest.Res("Stub", "g", map[string]any{"name": 42}),
		raltest.Res("Stub", "h", map[string]any{"name": []any{"x"}}),
	}}
	got := []string{}
	for _, g := range Preflight(valueRegistry(), c) {
		got = append(got, g.Ref+" "+g.Attribute+"="+g.Value)
	}
	want := []string{
		"Stub[a] ensure=true",
		"Stub[b] ensure=42",
		"Stub[c] ensure=[purged]",
		"Stub[d] ensure=false",
		"Stub[e] ensure=<nil>",
		"Stub[f] ensure=map[k:v]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gaps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPreflightAcceptsAnImplementedCatalog(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Stage", "main", nil),
		raltest.Res("Class", "main", nil),
		raltest.Res("File", "/tmp/a", map[string]any{"ensure": "file", "content": "x"}),
		raltest.Res("Notify", "hi", map[string]any{"message": "yo"}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps in an implementable catalog: %v", gaps)
	}
}

func TestPreflightReportsUnimplementedType(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Service", "nginx", map[string]any{"ensure": "running"}),
	}}
	gaps := Preflight(testRegistry(), c)
	if len(gaps) != 1 {
		t.Fatalf("Preflight returned %d gaps, want 1: %v", len(gaps), gaps)
	}
	if gaps[0].Attribute != "" {
		t.Errorf("Gap.Attribute = %q, want empty for an unimplemented type", gaps[0].Attribute)
	}
	// An unimplemented TYPE must not also report every one of its attributes:
	// the type is the single actionable fact, and listing its attributes as
	// separate gaps would inflate the porting histogram by attribute count.
	if !strings.Contains(gaps[0].String(), "Service[nginx]") {
		t.Errorf("Gap.String() = %q, does not name the resource", gaps[0].String())
	}
}

// Every gap, not the first: the list is also the porting roadmap, so stopping
// early would under-report the work.
func TestPreflightReportsEveryGap(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("File", "/etc/app", map[string]any{
			"ensure": "directory", "recurselimit": 2, "links": "follow",
		}),
		raltest.Res("Service", "nginx", nil),
	}}
	gaps := Preflight(testRegistry(), c)
	if len(gaps) != 3 {
		t.Fatalf("Preflight returned %d gaps, want 3: %v", len(gaps), gaps)
	}
	var attrs []string
	for _, g := range gaps {
		if g.Attribute != "" {
			attrs = append(attrs, g.Attribute)
		}
	}
	if len(attrs) != 2 {
		t.Fatalf("got %d attribute gaps, want 2: %v", len(attrs), attrs)
	}
	// Sorted, so the message is stable run to run.
	if attrs[0] != "links" || attrs[1] != "recurselimit" {
		t.Errorf("attribute gaps = %v, want [links recurselimit]", attrs)
	}
}

func TestPreflightIgnoresContainersAndEngineAttrs(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Stage", "main", nil),
		raltest.Res("Class", "Settings", nil),
		raltest.Res("Node", "default", nil),
		raltest.Res("File", "/tmp/a", map[string]any{
			"ensure": "file",
			"notify": []any{"Notify[hi]"},
			"tag":    []any{"role::web"},
		}),
		raltest.Res("Notify", "hi", map[string]any{"message": "yo"}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps: %v", gaps)
	}
}

// A metaparameter the engine does NOT handle must be a gap. Silently ignoring
// `schedule` would mean applying a resource the catalog said not to apply yet.
func TestPreflightRefusesUnhandledMetaparameter(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Notify", "hi", map[string]any{"message": "yo", "schedule": "daily"}),
	}}
	gaps := Preflight(testRegistry(), c)
	if len(gaps) != 1 || gaps[0].Attribute != "schedule" {
		t.Fatalf("Preflight returned %v, want one gap for schedule", gaps)
	}
}

// A container carrying parameters must still be skipped. Class[main] with
// parameters is ordinary in a real catalog, and reporting its parameters as
// gaps would refuse every catalog that uses a parameterised class.
func TestPreflightSkipsContainerParameters(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Class", "profile::web", map[string]any{"port": 8080, "recurse": true}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps on a parameterised container: %v", gaps)
	}
}

func TestPreflightIsDeterministic(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("File", "/z", map[string]any{"zeta": 1, "alpha": 2, "mu": 3}),
		raltest.Res("File", "/a", map[string]any{"beta": 1}),
	}}
	first := Preflight(testRegistry(), c)
	for i := 0; i < 20; i++ {
		again := Preflight(testRegistry(), c)
		if len(again) != len(first) {
			t.Fatalf("gap count varies between runs: %d then %d", len(first), len(again))
		}
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("gap %d varies between runs: %v then %v", j, first[j], again[j])
			}
		}
	}
}

// An empty catalog is not an error. It is a legitimate, if unusual, thing to
// apply, and refusing it would be a claim the catalog is malformed.
func TestPreflightAcceptsAnEmptyCatalog(t *testing.T) {
	if gaps := Preflight(testRegistry(), &catalog.Catalog{}); len(gaps) != 0 {
		t.Errorf("Preflight found gaps in an empty catalog: %v", gaps)
	}
}

// A provider that embeds another to inherit its methods must still prefetch to
// ITSELF. Go binds a promoted method's receiver to the embedded value, so
// inheriting stub's Prefetch would return the inner stub -- a value that still
// satisfies Provider but is no longer ValueBounded, so pre-flight would stop
// refusing values and every test built on this fixture would keep passing.
//
// The assertion is the type, not the value: stub holds an Attrs map, and
// comparing interface values whose dynamic type contains a map panics.
func TestValueBoundedProviderPrefetchesToItself(t *testing.T) {
	var p Provider = stubValueProvider{stub{typ: "stub", attrs: NewAttrs("ensure")}}

	got, err := p.Prefetch(context.Background(), nil)
	if err != nil {
		t.Fatalf("Prefetch returned %v, want nil", err)
	}
	if _, ok := got.(stubValueProvider); !ok {
		t.Fatalf("Prefetch returned %T, want stubValueProvider", got)
	}
	if _, ok := got.(ValueBounded); !ok {
		t.Error("the prefetched provider is no longer ValueBounded")
	}
}

// A defined type's instance applies nothing itself, so neither it nor its
// parameters are provider work. Before the kind rule, every catalog that used
// a defined type was refused whole.
func TestPreflightSkipsDefinedTypeInstances(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Bench_profile::Site", Title: "s1", Kind: "defined_type",
			Parameters: map[string]any{"idx": 1, "port": 1025}},
		{Type: "File", Title: "/tmp/a", Kind: "compilable_type",
			Parameters: map[string]any{"ensure": "file"}},
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight = %v, want no gaps", gaps)
	}
}

// listStub is a value-bounded provider that declares "source" list-valued and
// refuses any puppet: element; ensure keeps stubValueProvider's present|absent.
type listStub struct{ stubValueProvider }

func (listStub) SupportsValue(attr, value string) bool {
	if attr == "source" {
		return !strings.HasPrefix(value, "puppet:")
	}
	return stubValueProvider{}.SupportsValue(attr, value)
}

func (listStub) ListAttrs() Attrs { return NewAttrs("source") }

// See stubValueProvider.Prefetch: a promoted Prefetch would drop ListAttrs.
func (s listStub) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return s, nil }

// A list-valued attribute is bounded element by element, so the gap names the
// element an operator has to change. Every other attribute still judges a list
// whole: ensure => ["present"] passing as "present" would fail at apply, after
// the rest of the catalog had run.
func TestPreflightBoundsEachElementOfAListValuedAttribute(t *testing.T) {
	reg := NewRegistry(listStub{stubValueProvider{stub{typ: "liststub", attrs: NewAttrs("ensure", "source")}}})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Liststub", "a", map[string]any{"source": []any{"/ok", "puppet:///b", "puppet:///a"}}),
		raltest.Res("Liststub", "b", map[string]any{"source": []any{"/ok", "file:///ok"}}),
		raltest.Res("Liststub", "c", map[string]any{"source": "puppet:///c"}),
		raltest.Res("Liststub", "d", map[string]any{"ensure": []any{"present"}}),
	}}
	var got []string
	for _, g := range Preflight(reg, c) {
		got = append(got, g.String())
	}
	want := []string{
		`Liststub[a]: source => "puppet:///a" is not implemented`,
		`Liststub[a]: source => "puppet:///b" is not implemented`,
		`Liststub[c]: source => "puppet:///c" is not implemented`,
		`Liststub[d]: ensure => "[present]" is not implemented`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gaps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// loglevel => notice is Puppet's default and changes nothing; concat copies it
// onto every File it makes. Any other loglevel would change what is logged.
func TestPreflightAcceptsLoglevelNoticeOnly(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("File", "/a", map[string]any{"loglevel": "notice"}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("loglevel => notice is a gap: %v", gaps)
	}
	c = &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("File", "/a", map[string]any{"loglevel": "debug"}),
	}}
	gaps := Preflight(testRegistry(), c)
	if len(gaps) != 1 || gaps[0].Attribute != "loglevel" {
		t.Errorf("gaps = %v, want one loglevel gap", gaps)
	}
}

// ResourceGaps is Preflight's per-resource judgement, extracted so the walk
// can re-judge a resource a generator changed. For a one-resource catalog the
// two must agree exactly, whatever kind of gap the resource has.
func TestResourceGapsMatchesPreflightForOneResource(t *testing.T) {
	reg := NewRegistry(
		stub{typ: "file", attrs: NewAttrs("ensure", "content", "mode")},
		stubValueProvider{stub{typ: "stub", attrs: NewAttrs("ensure", "name")}},
		listStub{stubValueProvider{stub{typ: "liststub", attrs: NewAttrs("source", "ensure")}}},
	)
	for _, r := range []catalog.Resource{
		raltest.Res("File", "/ok", map[string]any{"ensure": "file", "require": "File[/x]", "loglevel": "notice"}),
		raltest.Res("File", "/gaps", map[string]any{"links": "follow", "recurselimit": 2, "loglevel": "info"}),
		raltest.Res("Stub", "v", map[string]any{"ensure": "purged", "name": "n"}),
		raltest.Res("Liststub", "l", map[string]any{"source": []any{"good", "puppet:///bad"}, "ensure": []any{"present"}}),
		raltest.Res("Service", "nginx", map[string]any{"ensure": "running"}),
	} {
		want := Preflight(reg, &catalog.Catalog{Resources: []catalog.Resource{r}})
		p, _ := reg.Provider(r.Type)
		got := ResourceGaps(p, r)
		if len(got) != len(want) {
			t.Errorf("%s: ResourceGaps = %v, Preflight = %v", r.Ref(), got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: ResourceGaps = %v, Preflight = %v", r.Ref(), got, want)
				break
			}
		}
	}
}

// A declared filebucket is refused even when a provider claims the type --
// the extension host can apply one -- because native File's backup knows only
// the default puppet bucket: a File backing up into the declared bucket would
// fail, and a declared 'puppet' bucket would be silently bypassed.
func TestPreflightRefusesADeclaredFilebucket(t *testing.T) {
	reg := NewRegistry(
		stub{typ: "file", attrs: NewAttrs("ensure", "content", "backup")},
		stub{typ: "filebucket", attrs: NewAttrs("path", "server")},
	)
	for _, title := range []string{"main", "puppet"} {
		r := raltest.Res("Filebucket", title, map[string]any{"path": "/x"})
		c := &catalog.Catalog{Resources: []catalog.Resource{
			r, raltest.Res("File", "/f", map[string]any{"content": "new", "backup": title}),
		}}
		gaps := Preflight(reg, c)
		if len(gaps) != 1 || gaps[0].Ref != r.Ref() {
			t.Fatalf("%s: Preflight = %v, want one gap for the filebucket", title, gaps)
		}
		want := r.Ref() + ": resource type Filebucket is not implemented: native File's backup does not implement named filebuckets"
		if got := gaps[0].String(); got != want {
			t.Errorf("%s: gap = %q, want %q", title, got, want)
		}
		p, _ := reg.Provider(r.Type)
		if got := ResourceGaps(p, r); len(got) != 1 || got[0] != gaps[0] {
			t.Errorf("%s: ResourceGaps = %v, want %v", title, got, gaps)
		}
	}
}

// richSamples are three rich values Decode leaves in place, each with the gap
// line a native provider gives it.
var richSamples = []struct {
	value any
	want  string
}{
	{map[string]any{"__ptype": "Regexp", "__pvalue": "ab+c"},
		"Notify[n]: parameter message holds a rich Regexp value, which voxagent does not support"},
	{map[string]any{"__ptype": "Binary", "__pvalue": "w6k="},
		"Notify[n]: parameter message holds a Binary value that is not ASCII, which voxagent does not support on a native resource"},
	{map[string]any{"__ptype": "Sensitive", "__pvalue": "s"},
		"Notify[n]: parameter message holds a rich Sensitive value, which voxagent does not support"},
}

func TestResourceGapsRefuseRichValuesOnNativeResources(t *testing.T) {
	for _, s := range richSamples {
		gaps := ResourceGaps(Notify{}, raltest.Res("Notify", "n", map[string]any{"message": s.value}))
		if len(gaps) != 1 || gaps[0].String() != s.want {
			t.Errorf("gaps = %v, want exactly %q", gaps, s.want)
		}
	}
	// An unimplemented attribute stays the attribute's one gap.
	gaps := ResourceGaps(Notify{}, raltest.Res("Notify", "n", map[string]any{"nope": richSamples[0].value}))
	if len(gaps) != 1 || gaps[0].String() != `Notify[n]: attribute "nope" is not implemented` {
		t.Errorf("unimplemented attribute gaps = %v", gaps)
	}
}

// The host decodes the raw catalog bytes itself, so a Tier 3 resource keeps
// every rich value.
func TestResourceGapsLeaveRichValuesToTheHost(t *testing.T) {
	for _, s := range richSamples {
		params := map[string]any{"value": s.value}
		e := NewExtension("vox_value", &fakeHost{}, load, nil, cat(params))
		if gaps := ResourceGaps(e, raltest.Res("Vox_value", "a", params)); len(gaps) != 0 {
			t.Errorf("%v: gaps = %v, want none", s.value, gaps)
		}
	}
}

func TestDeferredGapsSkipContainers(t *testing.T) {
	deferred := map[string]any{"__ptype": "Deferred", "name": "join", "arguments": []any{[]any{"a", "b"}, "-"}}
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Class", Title: "x", Kind: "class", Parameters: map[string]any{"p": deferred}},
		{Type: "Notify", Title: "n", Kind: "compilable_type", Parameters: map[string]any{"message": deferred,
			"withpath": map[string]any{"__ptype": "Regexp", "__pvalue": "a"}}},
	}}
	gaps := DeferredGaps(c)
	const want = "Notify[n]: parameter message is Deferred; voxagent does not resolve Deferred values"
	if len(gaps) != 1 || gaps[0].String() != want {
		t.Errorf("DeferredGaps = %v, want exactly %q", gaps, want)
	}
}

// Puppet's DeferredResolver finds a Deferred at any depth, so DeferredGaps
// does too: one gap per parameter holding one.
func TestDeferredGapsFindNestedDeferred(t *testing.T) {
	deferred := map[string]any{"__ptype": "Deferred", "name": "join", "arguments": []any{[]any{"a", "b"}, "-"}}
	c := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Vox_value", Title: "a", Kind: "compilable_type",
		Parameters: map[string]any{
			"array":     []any{"x", map[string]any{"k": deferred}},
			"hash":      map[string]any{"__ptype": "Hash", "__pvalue": []any{json.Number("1"), deferred}},
			"object":    map[string]any{"__ptype": map[string]any{"__ptype": "Pcore::IntegerType"}, "from": deferred},
			"plain":     "x",
			"sensitive": map[string]any{"__ptype": "Sensitive", "__pvalue": deferred},
			"twice":     []any{deferred, deferred},
		}}}}
	var got []string
	for _, g := range DeferredGaps(c) {
		got = append(got, g.String())
	}
	var want []string
	for _, p := range []string{"array", "hash", "object", "sensitive", "twice"} {
		want = append(want, "Vox_value[a]: parameter "+p+" is Deferred; voxagent does not resolve Deferred values")
	}
	if !slices.Equal(got, want) {
		t.Errorf("DeferredGaps =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A marked attribute's unsupported value is shown as [redacted] (D-78).
func TestPreflightRedactsAMarkedValue(t *testing.T) {
	r := raltest.Res("Stub", "a", map[string]any{"ensure": "purged"})
	for _, tc := range []struct {
		marked []string
		want   string
	}{
		{[]string{"ensure"}, `Stub[a]: ensure => [redacted] is not implemented`},
		{nil, `Stub[a]: ensure => "purged" is not implemented`},
	} {
		r.SensitiveParameters = tc.marked
		gaps := Preflight(valueRegistry(), &catalog.Catalog{Resources: []catalog.Resource{r}})
		if len(gaps) != 1 || gaps[0].String() != tc.want {
			t.Errorf("marked=%v: gaps = %v, want %q", tc.marked, gaps, tc.want)
		}
	}
}
