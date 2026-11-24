package ral

import (
	"context"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// stub is a provider used to test the registry itself. It is not a Tier 1
// provider and ships only in tests.
type stub struct {
	typ   string
	attrs Attrs
}

func (s stub) Type() string                                              { return s.typ }
func (s stub) ProviderUsed() *string                                     { return nil }
func (s stub) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }
func (s stub) Attrs() Attrs                                              { return s.attrs }
func (s stub) Apply(context.Context, catalog.Resource, bool) ([]Change, []Warning, error) {
	return nil, nil, nil
}
func (s stub) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}
func (s stub) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return s, nil }

func TestRegistryLooksUpByLowercaseType(t *testing.T) {
	reg := NewRegistry(stub{typ: "file", attrs: NewAttrs("ensure")})
	// Catalogs spell types in title case ("File"), providers in lowercase.
	// The registry normalises so every caller does not have to.
	for _, name := range []string{"file", "File", "FILE"} {
		if _, ok := reg.Provider(name); !ok {
			t.Errorf("Provider(%q) not found", name)
		}
	}
	if _, ok := reg.Provider("service"); ok {
		t.Error(`Provider("service") found an unregistered type`)
	}
}

func TestNewRegistryRejectsDuplicateType(t *testing.T) {
	defer func() {
		// Assert on the message, not merely that something panicked. Checking
		// only `recover() != nil` would pass for an unrelated panic -- a nil
		// map write, say -- and so would keep passing if the duplicate check
		// were deleted and replaced by a different bug.
		r := recover()
		if r == nil {
			t.Fatal("NewRegistry accepted two providers for one type; want a panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panicked with %T(%v), want a string naming the duplicate", r, r)
		}
		if !strings.Contains(msg, "two providers registered") || !strings.Contains(msg, `"file"`) {
			t.Errorf("panic message %q does not name the duplicate registration", msg)
		}
	}()
	NewRegistry(stub{typ: "file"}, stub{typ: "file"})
}

// A provider whose Type() is not already lowercase must still be reachable.
// Dropping the ToLower in NewRegistry survived, because every stub in the suite
// returns a lowercase type -- and the real symptom is a provider that is
// registered and then permanently unreachable, so pre-flight refuses its type
// as unimplemented while it sits in the registry.
func TestRegistryNormalisesTheRegisteredType(t *testing.T) {
	reg := NewRegistry(stub{typ: "File", attrs: NewAttrs("ensure")})
	for _, name := range []string{"file", "File", "FILE"} {
		if _, ok := reg.Provider(name); !ok {
			t.Errorf("Provider(%q) not found for a provider registered as \"File\"", name)
		}
	}
	if got := reg.Types(); len(got) != 1 || got[0] != "file" {
		t.Errorf("Types() = %v, want [file]", got)
	}
}

func TestRegistryTypesIsSorted(t *testing.T) {
	reg := NewRegistry(stub{typ: "notify"}, stub{typ: "exec"}, stub{typ: "file"})
	got := reg.Types()
	want := []string{"exec", "file", "notify"}
	if len(got) != len(want) {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Types() = %v, want %v", got, want)
		}
	}
}

func TestContainerTypesCoverARealCatalog(t *testing.T) {
	// Measured: a catalog compiled from a one-line manifest already contains
	// Stage[main], Class[Settings], Class[main], and Node[default]. A
	// pre-flight check that did not know these are containers would refuse
	// every catalog ever compiled, so this set is load-bearing.
	for _, typ := range []string{"Stage", "Class", "Node", "stage", "CLASS"} {
		if !IsContainer(typ) {
			t.Errorf("IsContainer(%q) = false, want true", typ)
		}
	}
	// whit and component are compiler-internal too. They are not in the
	// measured one-line catalog above -- Puppet emits them for anchored
	// containment and for defined-type bodies -- so they are asserted here
	// rather than left to that measurement, which does not cover them.
	// Dropping them survived the suite, and the symptom is pre-flight refusing
	// any catalog containing a Whit.
	for _, typ := range []string{"Whit", "whit", "Component", "component"} {
		if !IsContainer(typ) {
			t.Errorf("IsContainer(%q) = false, want true", typ)
		}
	}
	for _, typ := range []string{"File", "Exec", "Notify", "Service"} {
		if IsContainer(typ) {
			t.Errorf("IsContainer(%q) = true, want false", typ)
		}
	}
}

func TestEngineAttrsCoverRelationshipMetaparameters(t *testing.T) {
	// These become graph edges or report data rather than provider work.
	for _, a := range []string{"before", "require", "notify", "subscribe", "tag"} {
		if !IsEngineAttr(a) {
			t.Errorf("IsEngineAttr(%q) = false, want true", a)
		}
	}
	// Case-insensitive, like IsContainer. Dropping strings.ToLower here
	// survived the suite because every name tested was already lowercase --
	// and a title-cased metaparameter would then become a spurious pre-flight
	// gap, refusing a catalog that is perfectly implementable.
	for _, a := range []string{"Before", "REQUIRE", "Notify", "Subscribe", "TAG"} {
		if !IsEngineAttr(a) {
			t.Errorf("IsEngineAttr(%q) = false, want true -- the check must be case-insensitive", a)
		}
	}
	// Anything NOT listed must become a pre-flight gap. Silently ignoring
	// `schedule` would mean applying a resource the catalog said not to apply
	// yet; silently ignoring `audit` would drop an auditing requirement.
	for _, a := range []string{"schedule", "audit", "noop", "alias", "loglevel", "content"} {
		if IsEngineAttr(a) {
			t.Errorf("IsEngineAttr(%q) = true, want false -- unhandled metaparameters must become gaps", a)
		}
	}
}

func TestAttrsSortedIsStable(t *testing.T) {
	a := NewAttrs("mode", "ensure", "content")
	got := a.Sorted()
	want := []string{"content", "ensure", "mode"}
	if len(got) != len(want) {
		t.Fatalf("Sorted() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sorted() = %v, want %v", got, want)
		}
	}
}

// Puppet's to_ral (resource.rb:515-526): in a catalog_format 2 catalog a
// resource whose kind is not compilable_type becomes an inert Component. Stage
// is compilable_type yet a container (relationship_graph.rb:240), so the type
// list still applies. A kind-less resource falls back to the type list: Puppet
// would make it a Component, but only hand-written catalogs omit kind.
func TestIsContainerResource(t *testing.T) {
	for _, tc := range []struct {
		r    catalog.Resource
		want bool
	}{
		{catalog.Resource{Type: "Stage", Title: "main", Kind: "compilable_type"}, true},
		{catalog.Resource{Type: "Class", Title: "main", Kind: "unknown"}, true},
		{catalog.Resource{Type: "Node", Title: "default", Kind: "unknown"}, true},
		{catalog.Resource{Type: "Bench_profile::Site", Title: "s1", Kind: "defined_type"}, true},
		{catalog.Resource{Type: "Concat", Title: "/etc/x", Kind: "defined_type"}, true},
		{catalog.Resource{Type: "Class", Title: "profile", Kind: "class"}, true},
		{catalog.Resource{Type: "File", Title: "/tmp/x", Kind: "compilable_type"}, false},
		{catalog.Resource{Type: "Concat_file", Title: "/etc/x", Kind: "compilable_type"}, false},
		{catalog.Resource{Type: "File", Title: "/tmp/x"}, false},
		{catalog.Resource{Type: "Class", Title: "main"}, true},
		{catalog.Resource{Type: "Foo::Bar", Title: "x"}, false},
	} {
		if got := IsContainerResource(tc.r); got != tc.want {
			t.Errorf("IsContainerResource(%s kind=%q) = %v, want %v", tc.r.Ref(), tc.r.Kind, got, tc.want)
		}
	}
}
