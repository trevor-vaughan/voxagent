package tidy

import (
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

func routeOf(rs ...catalog.Resource) unit.Decision {
	return Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}})
}

func TestRouteNoTidy(t *testing.T) {
	d := routeOf(raltest.Res("File", "/tmp/x", nil))
	if len(d.Providers) != 0 || len(d.Delegated) != 0 {
		t.Fatalf("Route = %+v, want empty", d)
	}
}

func TestRouteNativeTidy(t *testing.T) {
	d := routeOf(raltest.Res("Tidy", "/var/tmp/a", map[string]any{"age": "1d"}))
	if len(d.Providers) != 1 || len(d.Delegated) != 0 {
		t.Fatalf("Route = %+v, want one native provider", d)
	}
	if d.Providers[0].Type() != "tidy" {
		t.Errorf("provider type = %q", d.Providers[0].Type())
	}
}

func TestRouteDelegatesFirstReason(t *testing.T) {
	d := routeOf(
		raltest.Res("Tidy", "/var/tmp/a", nil),
		raltest.Res("Tidy", "rel/b", nil),
		raltest.Res("Tidy", "rel/c", nil),
	)
	if len(d.Providers) != 0 {
		t.Fatalf("Providers = %v, want none", d.Providers)
	}
	why := d.Delegated["tidy"]
	if !strings.Contains(why, "Tidy[rel/b]") || !strings.Contains(why, "path is relative") {
		t.Errorf("reason = %q, want the first relative Tidy", why)
	}
}

func TestRouteRubyRefusalStaysNative(t *testing.T) {
	d := routeOf(raltest.Res("Tidy", "/var/tmp/a", map[string]any{"age": "10"}))
	if len(d.Providers) != 1 || len(d.Delegated) != 0 {
		t.Fatalf("Route = %+v, want native (Validate refuses)", d)
	}
}

func TestValidateIsParseParamsError(t *testing.T) {
	r := raltest.Res("Tidy", "/var/tmp/a", map[string]any{"age": "10"})
	_, want := parseParams(r)
	if want == nil {
		t.Fatal("fixture should be refused by Ruby")
	}
	if err := (Tidy{}).Validate(r); err == nil || err.Error() != want.Error() {
		t.Errorf("Validate = %v, want %v", err, want)
	}
	if err := (Tidy{}).Validate(raltest.Res("Tidy", "/var/tmp/a", nil)); err != nil {
		t.Errorf("Validate of a good Tidy = %v", err)
	}
}

func TestManifestPins(t *testing.T) {
	m := Manifest
	if m.Name != "tidy" || m.Version != "1.0.0" || len(m.Releases) != 0 ||
		m.Source != (unit.Origin{Implementation: "puppet", Version: "8.10.0"}) ||
		m.Oracle != (unit.Origin{Implementation: "openvox", Version: "8.34.0"}) ||
		len(m.Types) != 1 || m.Types[0] != "tidy" || len(m.Requires) != 1 || m.Requires[0] != "file" {
		t.Errorf("Manifest = %+v", m)
	}
}

func TestPreflightScheduleIsAGap(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		raltest.Res("Tidy", "/var/tmp/a", map[string]any{"schedule": "daily"}),
	}}
	gaps := ral.Preflight(ral.NewRegistry(Tidy{}), c)
	if len(gaps) != 1 || gaps[0].Attribute != "schedule" {
		t.Fatalf("gaps = %v, want one for schedule", gaps)
	}
}
