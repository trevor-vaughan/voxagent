package catalog

import (
	"reflect"
	"testing"
)

func decodeParams(t *testing.T, params string) (*Catalog, error) {
	t.Helper()
	return Decode([]byte(`{"catalog_format":2,"resources":[{"type":"Notify","title":"n",` +
		`"kind":"compilable_type","parameters":` + params + `}]}`))
}

// An ASCII-only Binary is a
// plain String in every channel measured, at any depth.
func TestDecodeTurnsAnASCIIBinaryIntoAString(t *testing.T) {
	c, err := decodeParams(t, `{"message":{"__ptype":"Binary","__pvalue":"aGVsbG8="},`+
		`"list":["x",{"__ptype":"Binary","__pvalue":"aGk="}],"map":{"b":{"__ptype":"Binary","__pvalue":"Yg=="}}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"message": "hello", "list": []any{"x", "hi"}, "map": map[string]any{"b": "b"}}
	if got := c.Resources[0].Parameters; !reflect.DeepEqual(got, want) {
		t.Errorf("parameters = %#v, want %#v", got, want)
	}
	if rv := RichValues(c.Resources[0]); len(rv) != 0 {
		t.Errorf("RichValues = %v, want none", rv)
	}
}

// Bytes C3 A9 render as 'Ã©' in Ruby's message, so a non-ASCII
// Binary stays rich and pre-flight refuses it on a native resource.
func TestDecodeKeepsANonASCIIBinaryRich(t *testing.T) {
	c, err := decodeParams(t, `{"message":{"__ptype":"Binary","__pvalue":"w6k="}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := RichValues(c.Resources[0]), []RichValue{{Param: "message", PType: "Binary"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("RichValues = %v, want %v", got, want)
	}
}

// Invalid base64 fails as strict_decode64 does.
func TestDecodeRefusesInvalidBase64(t *testing.T) {
	const want = "Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: invalid base64"
	for _, v := range []string{`aGVsbG8`, `aGVs\nbG8=`, `aGVsbG8=!`} {
		_, err := decodeParams(t, `{"message":["ok",{"__ptype":"Binary","__pvalue":"`+v+`"}]}`)
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", v, err, want)
		}
	}
}

// An unresolvable name fails the whole catalog.
func TestDecodeRefusesAnUnknownPType(t *testing.T) {
	_, err := decodeParams(t, `{"message":{"__ptype":"Mymod::Thing","__pvalue":"x"}}`)
	const want = "Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: " +
		"No implementation mapping found for Puppet Type Mymod::Thing"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// Every other kind is kept and named; a __ptype that is
// neither a string nor a map is a plain hash (from_data_converter.rb:159-163).
func TestRichValuesNamesEachKind(t *testing.T) {
	c, err := decodeParams(t, `{"a":{"__ptype":"Timestamp","__pvalue":"2020-01-02T03:04:05.000000000 UTC"},`+
		`"b":{"__ptype":"Regexp","__pvalue":"ab+c"},"c":{"__ptype":"Default"},`+
		`"d":{"__ptype":"Hash","__pvalue":[1,"one"]},"e":{"__ptype":"Pcore::IntegerType","from":1,"to":5},`+
		`"f":{"__ptype":"Deferred","name":"join","arguments":[["a","b"],"-"]},`+
		`"g":{"__ptype":"Sensitive","__pvalue":{"__ptype":"Binary","__pvalue":"w6k="}},`+
		`"h":{"__ptype":5,"x":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []RichValue{{"a", "Timestamp"}, {"b", "Regexp"}, {"c", "Default"}, {"d", "Hash"},
		{"e", "Pcore::IntegerType"}, {"f", "Deferred"}, {"g", "Sensitive"}}
	if got := RichValues(c.Resources[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("RichValues = %v, want %v", got, want)
	}
}

func TestRubyClassName(t *testing.T) {
	for v, want := range map[any]string{float64(755): "Integer", 1.5: "Float", "s": "String",
		true: "TrueClass", false: "FalseClass", nil: "NilClass"} {
		if got := RubyClassName(v); got != want {
			t.Errorf("RubyClassName(%v) = %q, want %q", v, got, want)
		}
	}
	if RubyClassName([]any{}) != "Array" || RubyClassName(map[string]any{}) != "Hash" {
		t.Error("Array/Hash misnamed")
	}
}

// Read, not measured (from_data_converter.rb:212-226, decision D-52).
func TestDecodeRefusesANonStringBinary(t *testing.T) {
	const prefix = "Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: "
	for v, class := range map[string]string{`5`: "Integer", `{"a":1}`: "Hash", `["a"]`: "Array"} {
		_, err := decodeParams(t, `{"message":{"__ptype":"Binary","__pvalue":`+v+`}}`)
		if want := prefix + "Cannot create a Binary from a " + class; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", v, err, want)
		}
	}
}
