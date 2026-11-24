package extension

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type strictProbe struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Value any    `json:"value"`
}

func TestDecodeStrictAccepts(t *testing.T) {
	var p strictProbe
	if err := decodeStrict([]byte(`{"name":"üé😀","count":3,"value":12345678901234567890}`), &p); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if p.Name != "üé😀" || p.Count != 3 {
		t.Errorf("decoded %+v", p)
	}
	// UseNumber: a large integer survives verbatim into the report.
	if n, ok := p.Value.(json.Number); !ok || n.String() != "12345678901234567890" {
		t.Errorf("value = %#v, want json.Number 12345678901234567890", p.Value)
	}
}

func TestDecodeStrictRefuses(t *testing.T) {
	for _, tt := range []struct {
		name, in, want string
	}{
		{"unknown field", `{"name":"a","extra":1}`, "unknown field"},
		{"invalid UTF-8", "{\"name\":\"\xff\"}", "UTF-8"},
		{"lone high surrogate", `{"name":"\ud800"}`, "surrogate"},
		{"lone low surrogate", `{"name":"\udc00x"}`, "surrogate"},
		{"high surrogate then non-low", `{"name":"\ud800A"}`, "surrogate"},
		{"trailing value", `{"name":"a"} {}`, "trailing"},
		{"wrong type", `{"count":"three"}`, "cannot unmarshal"},
		{"not an object", `[]`, "cannot unmarshal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var p strictProbe
			err := decodeStrict([]byte(tt.in), &p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("decodeStrict(%s) = %v, want an error containing %q", tt.in, err, tt.want)
			}
		})
	}
}

// An escaped backslash before "u" is not an escape: \\ud800 is the text \ud800.
func TestDecodeStrictEscapedBackslashIsNotASurrogate(t *testing.T) {
	var p strictProbe
	if err := decodeStrict([]byte(`{"name":"\\ud800"}`), &p); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if p.Name != `\ud800` {
		t.Errorf("name = %q", p.Name)
	}
}

// Whatever decodeStrict accepts decodes to strings with no replacement
// character the input did not carry literally.
func FuzzDecodeStrict(f *testing.F) {
	f.Add([]byte(`{"name":"a"}`))
	f.Add([]byte(`{"name":"😀"}`))
	f.Add([]byte(`{"name":"\ud800"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		var p strictProbe
		if decodeStrict(in, &p) != nil {
			return
		}
		if strings.ContainsRune(p.Name, '�') && !bytes.Contains(in, []byte("�")) &&
			!bytes.Contains(bytes.ToLower(in), []byte(`�`)) {
			t.Fatalf("accepted %q and produced a replacement character", in)
		}
	})
}

// spy records whether encoding/json reached it, to prove a refusal came
// before the decode allocated anything.
type spy struct{ decoded bool }

func (s *spy) UnmarshalJSON([]byte) error { s.decoded = true; return nil }

func array(n int, elem string) string {
	return "[" + strings.TrimSuffix(strings.Repeat(elem+",", n), ",") + "]"
}

func TestDecodeStrictRefusesTooManyElementsBeforeDecoding(t *testing.T) {
	for _, tt := range []struct{ name, in string }{
		{"array of empty objects", `{"events":` + array(MaxItems+1, "{}") + `}`},
		{"object members", "{" + strings.TrimSuffix(strings.Repeat(`"k":1,`, MaxItems+1), ",") + "}"},
		{"nested inner array", `[[` + strings.TrimPrefix(array(MaxItems+1, "0"), "[") + `]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var s spy
			err := decodeStrict([]byte(tt.in), &s)
			if err == nil || !strings.Contains(err.Error(), "more than 65536 elements") {
				t.Errorf("decodeStrict = %v, want the element limit", err)
			}
			if s.decoded {
				t.Error("encoding/json decoded the value before the limit was enforced")
			}
		})
	}
}

func TestDecodeStrictAcceptsExactlyMaxItems(t *testing.T) {
	var v struct {
		Events []struct{} `json:"events"`
	}
	if err := decodeStrict([]byte(`{"events":`+array(MaxItems, "{}")+`}`), &v); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if len(v.Events) != MaxItems {
		t.Errorf("decoded %d events", len(v.Events))
	}
}

// Each container is counted on its own: MaxItems arrays of MaxItems/… elements
// would overflow a shared counter, and a parent counts a child as one element.
func TestDecodeStrictCountsContainersIndependently(t *testing.T) {
	inner := array(MaxItems, "0")
	in := "[" + inner + "," + inner + "]"
	var v [][]int
	if err := decodeStrict([]byte(in), &v); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if len(v) != 2 || len(v[1]) != MaxItems {
		t.Errorf("decoded %d arrays", len(v))
	}
}

// Brackets, commas, colons and escaped quotes inside strings are text.
func TestDecodeStrictDoesNotCountInsideStrings(t *testing.T) {
	noisy := `"a,b],[{,}\",\\"`
	in := array(MaxItems, noisy)
	var v []string
	if err := decodeStrict([]byte(in), &v); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if len(v) != MaxItems || v[0] != `a,b],[{,}",\` {
		t.Errorf("decoded %d strings, first %q", len(v), v[0])
	}
}

func TestDecodeStrictRefusesDeepNesting(t *testing.T) {
	ok := strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth)
	var v any
	if err := decodeStrict([]byte(ok), &v); err != nil {
		t.Fatalf("decodeStrict at MaxDepth: %v", err)
	}
	deep := "[" + ok + "]"
	var s spy
	err := decodeStrict([]byte(deep), &s)
	if err == nil || !strings.Contains(err.Error(), "nested deeper than") || s.decoded {
		t.Errorf("decodeStrict = %v (decoded %v), want the depth limit before decoding", err, s.decoded)
	}
}
