package catalog

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestNormalizeZeroesAllowlistedFields(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	Normalize(c)
	if c.Version != nil {
		t.Errorf("Version = %v, want nil after Normalize", c.Version)
	}
	if c.CatalogUUID != "" {
		t.Errorf("CatalogUUID = %q, want empty after Normalize", c.CatalogUUID)
	}
}

func TestNormalizePreservesEverythingElse(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	Normalize(c)
	if c.Name != "testnode.example.com" {
		t.Errorf("Name = %q, want it preserved", c.Name)
	}
	if c.Environment != "production" {
		t.Errorf("Environment = %q, want it preserved", c.Environment)
	}
	if len(c.Resources) != 2 {
		t.Errorf("len(Resources) = %d, want 2", len(c.Resources))
	}
}

// Resource order encodes apply-order tie-breaks, so Normalize must not reorder.
func TestNormalizeDoesNotReorderResources(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	before := []string{c.Resources[0].Ref(), c.Resources[1].Ref()}
	Normalize(c)
	after := []string{c.Resources[0].Ref(), c.Resources[1].Ref()}
	if !slices.Equal(before, after) {
		t.Errorf("resource order changed: %v -> %v", before, after)
	}
}

func TestAllowlistedFieldsIsExactlyTwo(t *testing.T) {
	want := []string{"version", "catalog_uuid"}
	if !slices.Equal(AllowlistedFields, want) {
		t.Errorf("AllowlistedFields = %v, want %v", AllowlistedFields, want)
	}
}

// TestNormalizeZeroesEveryAllowlistedField ties AllowlistedFields to Normalize
// structurally rather than by field name: it walks the JSON produced after
// Normalize and asserts every allowlisted field is either absent or the JSON
// zero value. If a field is ever added to AllowlistedFields without a matching
// change in Normalize, this test fails without needing its own update — unlike
// TestNormalizeZeroesAllowlistedFields, which only knows about Version and
// CatalogUUID by name.
func TestNormalizeZeroesEveryAllowlistedField(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	Normalize(c)

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}

	for _, name := range AllowlistedFields {
		value, present := fields[name]
		if !present {
			continue // absent is fine: an omitempty field zeroed to its default
		}
		if !jsonIsZero(value) {
			t.Errorf("field %q = %#v after Normalize, want absent or a zero value", name, value)
		}
	}
}

// jsonIsZero reports whether a value decoded from JSON (via encoding/json into
// any) is that type's zero value: nil, "", or 0.
func jsonIsZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case float64:
		return x == 0
	default:
		return false
	}
}
