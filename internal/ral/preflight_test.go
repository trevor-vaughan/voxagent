package ral

import (
	"context"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func testRegistry() *Registry {
	return NewRegistry(
		stub{typ: "file", attrs: NewAttrs("ensure", "content", "mode", "owner", "group")},
		stub{typ: "notify", attrs: NewAttrs("message", "withpath")},
	)
}

func res(typ, title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: typ, Title: title, Parameters: params}
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
		res("Stub", "a", map[string]any{"ensure": "purged"}),
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
		res("Stub", "a", map[string]any{"ensure": "present", "name": "a"}),
		res("Stub", "b", map[string]any{"ensure": "absent"}),
		// A provider that does not implement ValueBounded is unaffected:
		// "directory" is not in the stub's present|absent domain, and file
		// must still accept it.
		res("File", "/tmp/a", map[string]any{"ensure": "directory"}),
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
		res("Halfstub", "a", map[string]any{"ensure": "purged"}),
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
// Providers read parameters through fmt's %v (paramString), so a JSON true
// reaches Apply as "true" and must be judged as "true" here. Skipping
// non-strings let {"ensure": false} through to apply time, where it failed one
// resource after the rest of the catalog had been written.
func TestPreflightBoundsNonStringValuesAsTheProviderReadsThem(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Stub", "a", map[string]any{"ensure": true}),
		res("Stub", "b", map[string]any{"ensure": 42}),
		res("Stub", "c", map[string]any{"ensure": []any{"purged"}}),
		res("Stub", "d", map[string]any{"ensure": false}),
		res("Stub", "e", map[string]any{"ensure": nil}),
		res("Stub", "f", map[string]any{"ensure": map[string]any{"k": "v"}}),
		// name is implemented and not bounded: a non-string there is not a gap.
		res("Stub", "g", map[string]any{"name": 42}),
		res("Stub", "h", map[string]any{"name": []any{"x"}}),
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
		res("Stage", "main", nil),
		res("Class", "main", nil),
		res("File", "/tmp/a", map[string]any{"ensure": "file", "content": "x"}),
		res("Notify", "hi", map[string]any{"message": "yo"}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps in an implementable catalog: %v", gaps)
	}
}

func TestPreflightReportsUnimplementedType(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Service", "nginx", map[string]any{"ensure": "running"}),
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
		res("File", "/etc/app", map[string]any{
			"ensure": "directory", "recurse": true, "source": "puppet:///m/app",
		}),
		res("Service", "nginx", nil),
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
	if attrs[0] != "recurse" || attrs[1] != "source" {
		t.Errorf("attribute gaps = %v, want [recurse source]", attrs)
	}
}

func TestPreflightIgnoresContainersAndEngineAttrs(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Stage", "main", nil),
		res("Class", "Settings", nil),
		res("Node", "default", nil),
		res("File", "/tmp/a", map[string]any{
			"ensure": "file",
			"notify": []any{"Notify[hi]"},
			"tag":    []any{"role::web"},
		}),
		res("Notify", "hi", map[string]any{"message": "yo"}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps: %v", gaps)
	}
}

// A metaparameter the engine does NOT handle must be a gap. Silently ignoring
// `schedule` would mean applying a resource the catalog said not to apply yet.
func TestPreflightRefusesUnhandledMetaparameter(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("Notify", "hi", map[string]any{"message": "yo", "schedule": "daily"}),
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
		res("Class", "profile::web", map[string]any{"port": 8080, "recurse": true}),
	}}
	if gaps := Preflight(testRegistry(), c); len(gaps) != 0 {
		t.Errorf("Preflight found gaps on a parameterised container: %v", gaps)
	}
}

func TestPreflightIsDeterministic(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		res("File", "/z", map[string]any{"zeta": 1, "alpha": 2, "mu": 3}),
		res("File", "/a", map[string]any{"beta": 1}),
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
