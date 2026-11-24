package parsedfile

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestNewGrammarRefusals: the four definition errors of fp:39-48,301-323,
// 388-398, with Ruby's texts (parsedfile-inventory 5.2).
func TestNewGrammarRefusals(t *testing.T) {
	text := LineType{Name: "t", Text: true, Match: RE2(regexp.MustCompile(`.`))}
	for _, c := range []struct {
		name  string
		types []LineType
		want  string
	}{
		{"duplicate name", []LineType{text, {Name: "t", Fields: []string{"a"}}}, "Line type t is already defined"},
		{"record without fields", []LineType{{Name: "r"}}, "Must include a list of fields"},
		{"text without match", []LineType{{Name: "t", Text: true}}, "You must provide a :match regex for text lines"},
		{"reserved record_type", []LineType{{Name: "r", Fields: []string{"a", "record_type"}}}, "Cannot have fields named record_type"},
		{"reserved target", []LineType{{Name: "r", Fields: []string{"target"}}}, "Cannot have fields named target"},
		{"reserved on_disk", []LineType{{Name: "r", Fields: []string{"on_disk"}}}, "Cannot have fields named on_disk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, err := NewGrammar(c.types...)
			if err == nil || err.Error() != c.want || g != nil {
				t.Errorf("NewGrammar = %v, %v; want nil, %q", g, err, c.want)
			}
		})
	}

	if _, err := NewGrammar(text, LineType{Name: "r", Fields: []string{"a"}}); err != nil {
		t.Errorf("valid grammar refused: %v", err)
	}
}

// TestParseErrorText: ParseError renders Ruby's message, and the target the
// ParsedFile layer adds through rubyvalue.ErrorLocation (fp:259-261).
func TestParseErrorText(t *testing.T) {
	g, err := NewGrammar(LineType{Name: "t", Text: true, Match: RE2(regexp.MustCompile(`^a`))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Parse("a\nb\tc")
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("Parse error = %v, want *ParseError", err)
	}
	if pe.Line != 3 || pe.Text != "b\tc" || pe.File != "" {
		t.Errorf("ParseError = %+v, want Line 3 (physical line 2 plus one), Text b<tab>c", *pe)
	}
	if want := `Could not parse line "b\tc" (line: 3)`; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	pe.File = "/tmp/f"
	if want := `Could not parse line "b\tc" (file: /tmp/f, line: 3)`; pe.Error() != want {
		t.Errorf("Error() with file = %q, want %q", pe.Error(), want)
	}
}

// TestParseNoTypes: fp:268 raises only when there is a line to parse.
func TestParseNoTypes(t *testing.T) {
	g, err := NewGrammar()
	if err != nil {
		t.Fatal(err)
	}
	if rs, err := g.Parse(""); err != nil || len(rs) != 0 {
		t.Errorf("Parse(\"\") = %v, %v; want no records, nil", rs, err)
	}
	if _, err := g.Parse("a"); err == nil || err.Error() != "No record types defined; cannot parse lines" {
		t.Errorf("Parse(\"a\") error = %v", err)
	}
}

// TestToLineCopiesBeforePreGen: PreGen sees a copy (fp:341-344), so the
// caller's record keeps its fields, as Ruby's details.dup does.
func TestToLineCopiesBeforePreGen(t *testing.T) {
	g, err := NewGrammar(LineType{
		Name: "r", Fields: []string{"a"},
		PreGen: func(r *Record) {
			r.Fields["a"] = Value{Kind: Str, S: "changed"}
			r.Flags["seen"] = true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{Type: "r", Fields: map[string]Value{"a": {Kind: Str, S: "orig"}}, Flags: map[string]bool{}}
	got, err := g.ToLine(rec)
	if err != nil || got != "changed" {
		t.Errorf("ToLine = %q, %v; want changed", got, err)
	}
	if rec.Fields["a"].S != "orig" || rec.Flags["seen"] {
		t.Errorf("PreGen mutated the caller's record: %+v", rec)
	}
}

// TestSplitModeAbsentText: a field equal to the type's Absent text is Absent
// in split mode, and the rollup after an absent last field joins the symbol's
// text (fp:213-225: ([:absent] + rest).join).
func TestSplitModeAbsentText(t *testing.T) {
	g, err := NewGrammar(LineType{
		Name: "r", Fields: []string{"a", "b"}, Absent: "-",
		Separator: regexp.MustCompile(`:`), Joiner: "+",
	})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Parse("-:x\nq:-:y:z")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := rs[0].Fields["a"], rs[0].Fields["b"]; a.Kind != Absent || b.Kind != Str || b.S != "x" {
		t.Errorf("record 0 = %+v", rs[0].Fields)
	}
	if rs[1].Fields["b"].S != "absent+y+z" {
		t.Errorf("record 1 b = %+v, want rollup \"absent+y+z\"", rs[1].Fields["b"])
	}
}

// TestParsedfileSimpleRoundTrip is Puppet's parsedfile_spec.rb:207-213 at the
// engine level: with one catch-all text type, parsing then generating a file
// gives it back (the spec's header is the ParsedFile layer's).
func TestParsedfileSimpleRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/parsedfile_simple.txt")
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGrammar(LineType{Name: "text", Text: true, Match: RE2(regexp.MustCompile(`.`))})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != strings.Count(string(raw), "\n") {
		t.Errorf("parsed %d records, want one per line", len(rs))
	}
	got, err := g.ToFile(rs)
	if err != nil || got != string(raw) {
		t.Errorf("ToFile(Parse(text)) = %q, %v; want the text back", got, err)
	}
}

// TestHooksMayWriteFlags: parsed records and the copy PreGen sees carry empty
// maps, so a hook that sets a flag or field does not panic.
func TestHooksMayWriteFlags(t *testing.T) {
	g, err := NewGrammar(
		LineType{Name: "t", Text: true, Match: RE2(regexp.MustCompile(`.`)),
			PostParse: func(r *Record) error { r.Flags["parsed"] = true; return nil },
			PreGen:    func(r *Record) { r.Flags["gen"] = true; r.Fields["f"] = Value{Kind: Str, S: "x"} }},
	)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Parse("a")
	if err != nil || !rs[0].Flags["parsed"] {
		t.Fatalf("Parse = %+v, %v; want PostParse's flag set", rs, err)
	}
	if _, err := g.ToLine(Record{Type: "t", Line: "a"}); err != nil {
		t.Errorf("ToLine on a record with nil maps: %v", err)
	}
}

// TestPostParseErrNoRecordIsParseError: a PostParse that returns ErrNoRecord fails
// the parse for its line, as an error raised there would, without trying the
// types after it.
func TestPostParseErrNoRecordIsParseError(t *testing.T) {
	g, err := NewGrammar(
		LineType{Name: "t", Text: true, Match: RE2(regexp.MustCompile(`^bad`)), PostParse: func(*Record) error { return ErrNoRecord }},
		LineType{Name: "any", Text: true, Match: RE2(regexp.MustCompile(`.`))},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Parse("ok\nbad line")
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Text != "bad line" || pe.Line != 3 {
		t.Errorf("err = %#v; want ParseError for line 3", err)
	}
}

// TestListOfAbsentStringIsAValue: fp:110 tests == [:absent], a symbol, so the
// String list ["absent"] is generated as Ruby's Array#to_s gives it.
func TestListOfAbsentStringIsAValue(t *testing.T) {
	g, err := NewGrammar(LineType{Name: "r", Fields: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.ToLine(Record{Type: "r", Fields: map[string]Value{"a": {Kind: List, L: []string{"absent"}}}})
	if err != nil || got != `["absent"]` {
		t.Errorf("ToLine = %q, %v; want [\"absent\"]", got, err)
	}
}

// TestPostParseErrorAbortsParse: any other PostParse error aborts the parse and
// is returned as is, not as a ParseError.
func TestPostParseErrorAbortsParse(t *testing.T) {
	sentinel := errors.New("boom")
	g, err := NewGrammar(
		LineType{Name: "t", Text: true, Match: RE2(regexp.MustCompile(`^bad`)), PostParse: func(*Record) error { return sentinel }},
		LineType{Name: "any", Text: true, Match: RE2(regexp.MustCompile(`.`))},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Parse("ok\nbad line")
	var pe *ParseError
	if err != sentinel || errors.As(err, &pe) {
		t.Errorf("err = %#v; want the sentinel itself, not a ParseError", err)
	}
}

// TestValueDisplayAndReportValue pins the change formatting cron's tests relied
// on (spec 6.1, M2): quoted strings, bracketed quoted lists, bare absent, and
// report values that are never a nil list.
func TestValueDisplayAndReportValue(t *testing.T) {
	for _, tc := range []struct {
		v       Value
		display string
		report  any
	}{
		{Value{Kind: Str, S: "a b"}, "'a b'", "a b"},
		{Value{Kind: Str, S: ""}, "''", ""},
		{Value{Kind: Str, S: "it's"}, `'it\'s'`, "it's"},
		{Value{Kind: List, L: []string{"a1", "a2"}}, "['a1', 'a2']", []string{"a1", "a2"}},
		{Value{Kind: List}, "[]", []string{}},
		{Value{Kind: Absent}, "absent", "absent"},
		{Value{Kind: Nil}, "absent", "absent"},
	} {
		if got := tc.v.Display(); got != tc.display {
			t.Errorf("Display(%+v) = %q, want %q", tc.v, got, tc.display)
		}
		if got := tc.v.ReportValue(); !reflect.DeepEqual(got, tc.report) {
			t.Errorf("ReportValue(%+v) = %#v, want %#v", tc.v, got, tc.report)
		}
	}
}
