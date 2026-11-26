package rubyregexptest

import (
	"reflect"
	"testing"
)

func TestWhitespaceEscapeScanner(t *testing.T) {
	const src = "package p\n" +
		"var a = regexp.MustCompile(`^\\s+`)\n" +
		"var b = regexp.MustCompile(Space + `\\S`)\n" +
		"var c = regexp.MustCompile(\"[^\\\\s]\")\n" +
		"var d = regexp.MustCompile(`\\\\s`)\n" +
		"var e = regexp.MustCompile(`^\\w+` + Space)\n" +
		"const f = `\\s`\n" +
		"var g = other.Call(\"\\\\S\")\n"
	got := whitespaceEscapes(t, "x.go", src)
	want := []string{"`^\\s+`", "`\\S`", "\"[^\\\\s]\"", "`\\s`", "\"\\\\S\""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flagged %q, want %q", got, want)
	}
}
