// Package vectorstest reads the shared vectors (parsedfile/testdata/vectors.json)
// for the unit packages' grammar tests and checks a grammar against them. It
// imports parsedfile, so only other packages' tests import it, never
// parsedfile's own.
package vectorstest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
)

// value decodes the vectors' Value encoding: null, {"absent":true},
// {"s":"x"} or {"list":[...]}.
type value parsedfile.Value

func (v *value) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*v = value{Kind: parsedfile.Nil}
		return nil
	}
	var raw struct {
		Absent bool               `json:"absent"`
		S      *string            `json:"s"`
		List   *[]json.RawMessage `json:"list"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch {
	case raw.Absent:
		*v = value{Kind: parsedfile.Absent}
	case raw.S != nil:
		*v = value{Kind: parsedfile.Str, S: *raw.S}
	case raw.List != nil:
		// Ruby's lone [:absent] is the engine's Absent; a list holds strings.
		l := []string{}
		for _, e := range *raw.List {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				if len(*raw.List) == 1 {
					*v = value{Kind: parsedfile.Absent}
					return nil
				}
				return fmt.Errorf("list element %s is not a string", e)
			}
			l = append(l, s)
		}
		*v = value{Kind: parsedfile.List, L: l}
	default:
		return fmt.Errorf("unknown value %s", b)
	}
	return nil
}

// record is a vector's record, in the shape of Ruby's hash.
type record struct {
	Type   string           `json:"type"`
	Line   string           `json:"line"`
	Fields map[string]value `json:"fields"`
}

func (r record) record() parsedfile.Record {
	rec := parsedfile.Record{Type: r.Type, Line: r.Line, Fields: map[string]parsedfile.Value{}}
	for k, v := range r.Fields {
		rec.Fields[k] = parsedfile.Value(v)
	}
	return rec
}

func recordOf(r *parsedfile.Record) record {
	vr := record{Type: r.Type, Line: r.Line, Fields: map[string]value{}}
	for k, v := range r.Fields {
		vr.Fields[k] = value(v)
	}
	return vr
}

// Case is one vector: an operation on a grammar and what Ruby returned.
type Case struct {
	Name    string          `json:"name"`
	Grammar string          `json:"grammar"`
	Op      string          `json:"op"`
	Text    string          `json:"text"`
	Record  *record         `json:"record"`
	Records []record        `json:"records"`
	Want    json.RawMessage `json:"want"`
}

// Read returns the vectors of one grammar, failing the test when there are none.
func Read(t testing.TB, grammar string) []Case {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate vectorstest")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []Case `json:"cases"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	var out []Case
	for _, c := range file.Cases {
		if c.Grammar == grammar {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %q vectors", grammar)
	}
	return out
}

// Check runs the case on g and compares the outcome with Ruby's.
func (c Case) Check(t *testing.T, g *parsedfile.Grammar) {
	t.Helper()
	var want struct {
		Records *[]record `json:"records"`
		Line    *string   `json:"line"`
		Text    *string   `json:"text"`
		Error   *string   `json:"error"`
	}
	if err := json.Unmarshal(c.Want, &want); err != nil {
		t.Fatal(err)
	}
	var records []record
	var text string
	var gotErr error
	switch c.Op {
	case "parse":
		var recs []*parsedfile.Record
		recs, gotErr = g.Parse(c.Text)
		for _, r := range recs {
			records = append(records, recordOf(r))
		}
	case "to_line":
		text, gotErr = g.ToLine(c.Record.record())
	case "to_file":
		var rs []*parsedfile.Record
		for _, r := range c.Records {
			rec := r.record()
			rs = append(rs, &rec)
		}
		text, gotErr = g.ToFile(rs)
	default:
		t.Fatalf("unknown op %q", c.Op)
	}
	switch {
	case want.Error != nil:
		if gotErr == nil || gotErr.Error() != *want.Error {
			t.Errorf("error = %v, want %q", gotErr, *want.Error)
		}
	case gotErr != nil:
		t.Fatalf("unexpected error: %v", gotErr)
	case want.Records != nil:
		if len(records) != len(*want.Records) {
			t.Fatalf("got %d records, want %d", len(records), len(*want.Records))
		}
		for i, w := range *want.Records {
			if !reflect.DeepEqual(records[i], w) {
				t.Errorf("record %d = %+v, want %+v", i, records[i], w)
			}
		}
	case want.Line != nil:
		if text != *want.Line {
			t.Errorf("line = %q, want %q", text, *want.Line)
		}
	case want.Text != nil:
		if text != *want.Text {
			t.Errorf("text = %q, want %q", text, *want.Text)
		}
	default:
		t.Fatal("case has no expectation")
	}
}
