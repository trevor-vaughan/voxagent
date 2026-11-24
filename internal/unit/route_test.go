package unit

import (
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/ral"
)

type fakeProvider struct {
	ral.Provider
	typ string
}

type fakeUnit struct {
	name      string
	calls     *[]string
	providers []ral.Provider
	delegated map[string]string
}

func (u fakeUnit) Manifest() Manifest { return Manifest{Name: u.name} }
func (u fakeUnit) Route(*Env) Decision {
	*u.calls = append(*u.calls, u.name)
	return Decision{Providers: u.providers, Delegated: u.delegated}
}

func TestRouteAllCallsEveryUnitInNameOrder(t *testing.T) {
	var calls []string
	pa, pb, pc := &fakeProvider{typ: "a"}, &fakeProvider{typ: "b"}, &fakeProvider{typ: "c"}
	all := []Unit{
		fakeUnit{name: "c", calls: &calls, providers: []ral.Provider{pc}},
		fakeUnit{name: "a", calls: &calls, providers: []ral.Provider{pa}},
		fakeUnit{name: "b", calls: &calls, providers: []ral.Provider{pb}},
	}
	d := RouteAll(&Env{}, all)
	if strings.Join(calls, ",") != "a,b,c" {
		t.Fatalf("calls %v", calls)
	}
	if len(d.Providers) != 3 || d.Providers[0] != ral.Provider(pa) || d.Providers[1] != ral.Provider(pb) || d.Providers[2] != ral.Provider(pc) {
		t.Fatalf("providers %v", d.Providers)
	}
	if all[0].Manifest().Name != "c" {
		t.Fatal("RouteAll reordered the caller's slice")
	}
}

func TestRouteAllMergesDelegated(t *testing.T) {
	var calls []string
	d := RouteAll(&Env{}, []Unit{
		fakeUnit{name: "a", calls: &calls, delegated: map[string]string{"x": "why x"}},
		fakeUnit{name: "b", calls: &calls, delegated: map[string]string{"y": "why y"}},
	})
	if len(d.Delegated) != 2 || d.Delegated["x"] != "why x" || d.Delegated["y"] != "why y" {
		t.Fatalf("delegated %v", d.Delegated)
	}
}

func TestRouteAllPanicsOnDoubleDelegation(t *testing.T) {
	var calls []string
	defer func() {
		want := `unit: type "x" delegated by both a and b`
		if r := recover(); r != want {
			t.Fatalf("panic %v, want %q", r, want)
		}
	}()
	RouteAll(&Env{}, []Unit{
		fakeUnit{name: "b", calls: &calls, delegated: map[string]string{"x": "r"}},
		fakeUnit{name: "a", calls: &calls, delegated: map[string]string{"x": "r"}},
	})
}
