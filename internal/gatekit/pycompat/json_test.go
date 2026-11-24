package pycompat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := Decode([]byte(s))
	if err != nil {
		t.Fatalf("Decode(%q): %v", s, err)
	}
	return v
}

// Every expectation below was measured with CPython 3.12.14's json.loads and ==.
func TestEqualIsPythonEquality(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{`1`, `1.0`, true},
		{`true`, `1`, true},
		{`true`, `1.0`, true},
		{`false`, `0`, true},
		{`9007199254740993`, `9007199254740992`, false},
		{`9007199254740992`, `9007199254740992.0`, true},
		{`9007199254740993`, `9007199254740993.0`, false},
		{`1e400`, `1e400`, true},
		{`-0`, `0.0`, true},
		{`1e-400`, `0`, true},
		{`"1"`, `1`, false},
		{`null`, `false`, false},
		{`null`, `null`, true},
		{`"a"`, `"a"`, true},
		{`[1,2]`, `[1.0,2]`, true},
		{`[1,2]`, `[1]`, false},
		{`{"a":1}`, `{"a":true}`, true},
		{`{"a":1}`, `{"b":1}`, false},
		{`[]`, `{}`, false},
	} {
		a, b := mustDecode(t, tc.a), mustDecode(t, tc.b)
		if got := Equal(a, b); got != tc.want {
			t.Errorf("Equal(%s, %s) = %v, Python says %v", tc.a, tc.b, got, tc.want)
		}
		if got := Equal(b, a); got != tc.want {
			t.Errorf("Equal(%s, %s) = %v, Python says %v", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestTruthyIsPythonBool(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{`0`, false}, {`0.0`, false}, {`-0.0`, false}, {`1e-400`, false},
		{`""`, false}, {`[]`, false}, {`{}`, false}, {`null`, false},
		{`false`, false}, {`"a"`, true}, {`[0]`, true}, {`2`, true},
		{`true`, true}, {`{"a":null}`, true},
	} {
		if got := Truthy(mustDecode(t, tc.in)); got != tc.want {
			t.Errorf("Truthy(%s) = %v, Python says %v", tc.in, got, tc.want)
		}
	}
}

func TestLenCountsCharactersAndRefusesScalars(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{`"ab"`, 2}, {`"²é"`, 2}, {`[1,2,3]`, 3}, {`{"a":1}`, 1}, {`[]`, 0},
	} {
		got, err := Len(mustDecode(t, tc.in))
		if err != nil || got != tc.want {
			t.Errorf("Len(%s) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{`1`, `true`, `null`} {
		if _, err := Len(mustDecode(t, in)); err == nil {
			t.Errorf("Len(%s) succeeded; Python raises TypeError", in)
		}
	}
}

func TestDecodeRefusesWhatAReportCannotContain(t *testing.T) {
	for _, in := range []string{
		"\xff{}", "{\"a\": \"\xff\"}", `NaN`, `Infinity`, `[1,]`, `{"a":1} x`, "\ufeff{}", ``,
		`{"a":"\ud800"}`, `{"a":"\udfff"}`, `{"a":"\ud800x"}`, `{"a":"\ud800A"}`,
		`{"a":"\udc00\ud800"}`, `{"a":"\ud800\ud800"}`,
		"[" + strings.Repeat("1", 4301) + "]", "[-" + strings.Repeat("1", 4301) + "]",
	} {
		if v, err := Decode([]byte(in)); err == nil {
			t.Errorf("Decode(%q) = %v, want an error", in, v)
		}
	}
	for _, in := range []string{
		`{"a":"\\ud800"}`, `{"a":"\ud83d\ude00"}`, `{"a":"\\\\\ud83d\ude00"}`,
		"[" + strings.Repeat("1", 4300) + "]", "[-" + strings.Repeat("1", 4300) + "]",
		"[1." + strings.Repeat("1", 5000) + "]", "[1" + strings.Repeat("0", 5000) + "e0]",
		`["` + strings.Repeat("1", 5000) + `"]`,
	} {
		if _, err := Decode([]byte(in)); err != nil {
			t.Errorf("Decode(%.40q) = %v, want it accepted", in, err)
		}
	}
	v := mustDecode(t, " {\"a\":1,\"a\":2}\n\t")
	if !Equal(v, mustDecode(t, `{"a":2}`)) {
		t.Errorf("a duplicate key decoded to %s; Python keeps the last value", Render(v))
	}
}

func TestDecodeFileNamesThePathOnBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeFile(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("DecodeFile error = %v, want it to name %s", err, path)
	}
	if _, err := DecodeFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("DecodeFile on an absent file succeeded")
	}
}

func TestRenderWritesStringsQuotedAndTheRestAsJSON(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"x"`, `"x"`},
		{`null`, `null`},
		{`true`, `true`},
		{`1.50`, `1.50`},
		{`["a", null, 2]`, `["a", null, 2]`},
		{`{"b":1,"a":"x"}`, `{"a": "x", "b": 1}`},
	} {
		if got := Render(mustDecode(t, tc.in)); got != tc.want {
			t.Errorf("Render(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestShapeNamesTheKindNotTheValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"a":1}`, "an object"},
		{`[1]`, "a list"},
		{`"x"`, "a string"},
		{`1`, "a number"},
		{`1.5e3`, "a number"},
		{`true`, "a bool"},
		{`null`, "null"},
	} {
		if got := Shape(mustDecode(t, tc.in)); got != tc.want {
			t.Errorf("Shape(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
