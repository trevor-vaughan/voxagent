package catalog

import (
	"os"
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
