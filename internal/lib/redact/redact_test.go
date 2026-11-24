package redact

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

type vectors struct {
	Match []struct {
		Name    string   `json:"name"`
		Forms   []string `json:"forms"`
		Text    string   `json:"text"`
		TextHex string   `json:"text_hex"`
		Want    string   `json:"want"`
		WantHex string   `json:"want_hex"`
	} `json:"match"`
	Forms []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
		Forms []string        `json:"forms"`
	} `json:"forms"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// bytesOf returns the hex-encoded bytes when hexed is set, else the plain text.
func bytesOf(t *testing.T, plain, hexed string) string {
	t.Helper()
	if hexed == "" {
		return plain
	}
	b, err := hex.DecodeString(hexed)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScrubMatchesSharedVectors(t *testing.T) {
	for _, c := range loadVectors(t).Match {
		t.Run(c.Name, func(t *testing.T) {
			text := bytesOf(t, c.Text, c.TextHex)
			want := bytesOf(t, c.Want, c.WantHex)
			if got := fromForms(c.Forms).Scrub(text); got != want {
				t.Errorf("Scrub(%q) = %q, want %q", text, got, want)
			}
		})
	}
}

func TestFormsMatchSharedVectors(t *testing.T) {
	for _, c := range loadVectors(t).Forms {
		t.Run(c.Name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal(c.Value, &value); err != nil {
				t.Fatal(err)
			}
			got := forms(value)
			slices.Sort(got)
			want := slices.Clone(c.Forms)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("forms(%s) = %q, want %q", c.Value, got, want)
			}
		})
	}
}

func TestNewCollectsMarkedParametersOnly(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Notify", Title: "a", Parameters: map[string]any{"message": "topsecret"}, SensitiveParameters: []string{"message"}},
		{Type: "Notify", Title: "b", Parameters: map[string]any{"message": "visible"}},
		{Type: "Notify", Title: "c", Parameters: map[string]any{"message": "visible"}, SensitiveParameters: []string{"nosuch"}},
	}}
	if got, want := New(cat).Scrub("topsecret visible"), "[redacted] visible"; got != want {
		t.Errorf("Scrub = %q, want %q", got, want)
	}
}

func TestScrubUnmarkedReturnsInput(t *testing.T) {
	const text = "nothing sensitive here"
	unmatched := New(&catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Notify", Title: "a", Parameters: map[string]any{"message": "topsecret"}, SensitiveParameters: []string{"message"}},
	}})
	for name, s := range map[string]*Scrubber{"empty catalog": New(&catalog.Catalog{}), "nil": nil, "no match": unmatched} {
		var got string
		if allocs := testing.AllocsPerRun(100, func() { got = s.Scrub(text) }); allocs != 0 {
			t.Errorf("%s: %v allocs, want 0", name, allocs)
		}
		if got != text {
			t.Errorf("%s: Scrub = %q, want input", name, got)
		}
	}
}
