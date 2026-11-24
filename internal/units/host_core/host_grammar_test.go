package hostcore

import (
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/parsedfile/vectorstest"
)

func testClass() *parsedfile.Class {
	return hostClass(func(string) (parsedfile.FileType, error) { return nil, nil }, time.Now)
}

func TestHostClassIdentity(t *testing.T) {
	c := testClass()
	if c.Name != "parsed" || c.TypeName != "host" || c.DefaultTarget != "/etc/hosts" {
		t.Errorf("class = %q/%q default %q, want parsed/host /etc/hosts", c.Name, c.TypeName, c.DefaultTarget)
	}
	if c.Grammar == nil || c.FileType == nil || c.Now == nil {
		t.Errorf("Grammar, FileType and Now must be set: %+v", c)
	}
}

// ---- the shared vectors -------------------------------------------------

// TestHostGrammarMatchesSharedVectors runs every "host" case of vectors.json,
// whose wants fileparsing_vectors.rb takes from the real engine and the
// provider's own definitions (hp:21-48).
func TestHostGrammarMatchesSharedVectors(t *testing.T) {
	g := testClass().Grammar
	for _, c := range vectorstest.Read(t, "host") {
		t.Run(c.Name+"/"+c.Op, func(t *testing.T) { c.Check(t, g) })
	}
}

// ---- edges the vectors cannot hold --------------------------------------

func hostRecord(fields map[string]parsedfile.Value) parsedfile.Record {
	return parsedfile.Record{Type: "parsed", Fields: fields}
}

func TestHostToLine(t *testing.T) {
	absent := parsedfile.Value{Kind: parsedfile.Absent}
	g := testClass().Grammar
	for _, tc := range []struct {
		name   string
		fields map[string]parsedfile.Value
		want   string
		err    string
	}{
		{"aliases empty string is written", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "name": str("h1"), "host_aliases": str(""), "comment": str("")}, "1.2.3.4\th1\t", ""},
		{"aliases empty and comment", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "name": str("h1"), "host_aliases": str(""), "comment": str("c")}, "1.2.3.4\th1\t\t# c", ""},
		{"empty comment writes no marker", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "name": str("h1"), "host_aliases": str("a"), "comment": str("")}, "1.2.3.4\th1\ta", ""},
		{"aliases list joins with a space", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "name": str("h1"), "host_aliases": {Kind: parsedfile.List, L: []string{"a", "b"}}}, "1.2.3.4\th1\ta b", ""},
		{"absent aliases skipped", map[string]parsedfile.Value{"ip": str("1.2.3.4"), "name": str("h1"), "host_aliases": absent}, "1.2.3.4\th1", ""},
		{"missing ip", map[string]parsedfile.Value{"name": str("h1")}, "", "ip is a required attribute for hosts"},
		{"absent ip", map[string]parsedfile.Value{"ip": absent, "name": str("h1")}, "", "ip is a required attribute for hosts"},
		{"missing name", map[string]parsedfile.Value{"ip": str("1.2.3.4")}, "", "name is a required attribute for hosts"},
		{"both missing reports ip", map[string]parsedfile.Value{}, "", "ip is a required attribute for hosts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := g.ToLine(hostRecord(tc.fields))
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("ToLine = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestHostReserialisesParsedLines(t *testing.T) {
	g := testClass().Grammar
	recs, err := g.Parse("192.0.2.1\th1  a   b   # c")
	if err != nil {
		t.Fatal(err)
	}
	text, err := g.ToFile(recs)
	if err != nil {
		t.Fatal(err)
	}
	if want := "192.0.2.1\th1\ta b\t# c\n"; text != want {
		t.Errorf("ToFile = %q, want %q", text, want)
	}
}

func TestHostIncompleteMatchesEverything(t *testing.T) {
	g := testClass().Grammar
	for _, line := range []string{"192.0.2.3", "face", "\t# c", "  indented", "x\ty", "1.2.3.4 h", "é ", "%$!"} {
		recs, err := g.Parse(line)
		if err != nil {
			t.Errorf("Parse(%q): %v", line, err)
			continue
		}
		if len(recs) != 1 || recs[0].Type != "incomplete" || recs[0].Line != line {
			t.Errorf("Parse(%q) = %+v, want one incomplete record holding the line", line, recs)
		}
	}
	// A direct check of the matcher, not just of the grammar around it.
	caps, ok := incompleteMatcher{}.Match("")
	if !ok || len(caps) != 0 {
		t.Errorf("Match(%q) = %v, %v; want no captures, true", "", caps, ok)
	}
}

func TestHostParseIsLineStrict(t *testing.T) {
	g := testClass().Grammar
	// A name needs two characters, an IP two, and the IP must be lower-case hex.
	for _, line := range []string{"1.2.3.4 h", "f h1", "FE80::1 h1", "1.2.3.4 #c", "1.2.3.4 +x"} {
		recs, _ := g.Parse(line)
		if len(recs) != 1 || recs[0].Type != "incomplete" {
			t.Errorf("Parse(%q) = %+v, want incomplete", line, recs)
		}
	}
}
