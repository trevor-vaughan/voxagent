package catalog

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestDecodeMinimalCatalog(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	if c.Name != "testnode.example.com" {
		t.Errorf("Name = %q, want %q", c.Name, "testnode.example.com")
	}
	if c.CatalogFormat != 2 {
		t.Errorf("CatalogFormat = %d, want 2", c.CatalogFormat)
	}
	if c.CodeID != nil {
		t.Errorf("CodeID = %v, want nil", c.CodeID)
	}
	if got, want := len(c.Resources), 2; got != want {
		t.Fatalf("len(Resources) = %d, want %d", got, want)
	}
	if got, want := len(c.Edges), 1; got != want {
		t.Errorf("len(Edges) = %d, want %d", got, want)
	}
}

func TestDecodeCapturesResourceDetail(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	r := c.Resources[1]
	if got, want := r.Ref(), "Notify[loop-1]"; got != want {
		t.Errorf("Ref() = %q, want %q", got, want)
	}
	if r.Line != 35 {
		t.Errorf("Line = %d, want 35", r.Line)
	}
	if got, want := r.Parameters["message"], "0001"; got != want {
		t.Errorf("Parameters[message] = %v, want %v", got, want)
	}
}

// Stage[main] is not declared in a manifest, so it carries no file or line -
// the case documented on Resource.File and Resource.Line.
func TestDecodeStageResourceHasNoFileOrLine(t *testing.T) {
	c, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode returned error: %v", err)
	}
	r := c.Resources[0]
	if got, want := r.Ref(), "Stage[main]"; got != want {
		t.Fatalf("Resources[0].Ref() = %q, want %q", got, want)
	}
	if r.File != "" {
		t.Errorf("File = %q, want empty for a resource not declared in a manifest", r.File)
	}
	if r.Line != 0 {
		t.Errorf("Line = %d, want 0 for a resource not declared in a manifest", r.Line)
	}
}

func TestDecodeRejectsNonCatalogJSON(t *testing.T) {
	if _, err := Decode([]byte(`{"hello":"world"}`)); err == nil {
		t.Fatal("Decode accepted JSON with no catalog_format; want error")
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := Decode([]byte(`{not json`)); err == nil {
		t.Fatal("Decode accepted malformed JSON; want error")
	}
}

// Decode keeps a static catalog's inlined metadata so validation can refuse
// it; json.Unmarshal would otherwise drop the unknown key without a trace.
func TestDecodeKeepsInlinedMetadata(t *testing.T) {
	c, err := Decode([]byte(`{"catalog_format":2,"resources":[],"metadata":{"/x":{}}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(c.Metadata) != `{"/x":{}}` {
		t.Errorf("Metadata = %q, want the raw object", c.Metadata)
	}
	if c.RecursiveMetadata != nil {
		t.Errorf("RecursiveMetadata = %q, want nil for an absent key", c.RecursiveMetadata)
	}
}

// NormaliseNumbers turns a json.Number, however deep, into the float64 Decode
// produces, and leaves everything else as it is.
func TestNormaliseNumbersConvertsRecursively(t *testing.T) {
	in := map[string]any{"mode": json.Number("420"), "s": "x", "b": true, "nil": nil,
		"l": []any{json.Number("1"), "y"}, "m": map[string]any{"k": json.Number("2.5")}, "nm": map[string]any(nil), "nl": []any(nil)}
	got, err := NormaliseNumbers(in)
	if err != nil {
		t.Fatalf("NormaliseNumbers: %v", err)
	}
	want := map[string]any{"mode": float64(420), "s": "x", "b": true, "nil": nil,
		"l": []any{float64(1), "y"}, "m": map[string]any{"k": 2.5}, "nm": map[string]any(nil), "nl": []any(nil)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// A number no float64 can hold is an error, never a silent string: a
// provider handed the string would act on a different value than the
// catalog declared.
func TestNormaliseNumbersRefusesAnUnrepresentableNumber(t *testing.T) {
	for _, in := range []any{json.Number("1e400"), []any{json.Number("1e400")},
		map[string]any{"k": map[string]any{"deep": json.Number("-1e400")}}} {
		if _, err := NormaliseNumbers(in); err == nil || !strings.Contains(err.Error(), "e400") {
			t.Errorf("NormaliseNumbers(%v) err = %v, want one naming the number", in, err)
		}
	}
}

func TestDecodeSensitiveParameters(t *testing.T) {
	const head = `{"catalog_format":2,"resources":[{"type":"File","title":"/f","tags":[],"exported":false,`
	decode := func(tail string) (*Catalog, error) { return Decode([]byte(head + tail + `}]}`)) }

	c, err := Decode(loadFixture(t, "sensitive.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for i, want := range [][]string{{"content"}, {"mode"}} {
		if got := c.Resources[i].SensitiveParameters; !reflect.DeepEqual(got, want) {
			t.Errorf("resource %d SensitiveParameters = %v, want %v", i, got, want)
		}
	}

	c, err = decode(`"parameters":{"content":"x"}`)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Resources[0].SensitiveParameters != nil {
		t.Errorf("absent key decoded to %v, want nil", c.Resources[0].SensitiveParameters)
	}

	for _, bad := range []string{`"sensitive_parameters":"content"`, `"sensitive_parameters":[1]`} {
		if _, err := decode(bad); err == nil {
			t.Errorf("Decode accepted %s", bad)
		}
	}
}
