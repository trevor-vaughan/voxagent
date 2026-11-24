package sshkeyscore

import (
	"errors"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/parsedfile/vectorstest"
)

func testClass() *parsedfile.Class {
	return sshkeyClass(func(string) (parsedfile.FileType, error) { return nil, nil }, time.Now)
}

func TestSshkeyClassIdentity(t *testing.T) {
	c := testClass()
	if c.Name != "parsed" || c.TypeName != "sshkey" || c.DefaultTarget != "/etc/ssh/ssh_known_hosts" {
		t.Errorf("class = %q/%q default %q, want parsed/sshkey /etc/ssh/ssh_known_hosts", c.Name, c.TypeName, c.DefaultTarget)
	}
	if c.Grammar == nil || c.FileType == nil || c.Now == nil || c.ResourceForRecord == nil {
		t.Errorf("Grammar, FileType, Now and ResourceForRecord must be set: %+v", c)
	}
}

// TestSshkeyGrammarMatchesSharedVectors runs every "sshkey" case of
// vectors.json, whose wants fileparsing_vectors.rb takes from the real engine
// and the provider's own definitions (sp:10-24).
func TestSshkeyGrammarMatchesSharedVectors(t *testing.T) {
	g := testClass().Grammar
	for _, c := range vectorstest.Read(t, "sshkey") {
		t.Run(c.Name+"/"+c.Op, func(t *testing.T) { c.Check(t, g) })
	}
}

// ---- edges the vectors cannot hold --------------------------------------

// TestSshkeyLeadingWhitespaceAbortsParse: the split's leading empty field
// leaves name absent, sp:15 raises NoMethodError, and that is not a line the
// grammar rejects: Parse returns the error as is (spec 5.2, M7 07e).
func TestSshkeyLeadingWhitespaceAbortsParse(t *testing.T) {
	g := testClass().Grammar
	const want = "undefined method `split' for :absent:Symbol"
	for _, text := range []string{" lead ssh-rsa LLLL", "good ssh-rsa GGGG\n lead ssh-rsa LLLL", "\tlead ssh-rsa LLLL"} {
		recs, err := g.Parse(text)
		var pe *parsedfile.ParseError
		if err == nil || err.Error() != want || errors.As(err, &pe) || errors.Is(err, parsedfile.ErrNoRecord) {
			t.Errorf("Parse(%q) = %v, %v; want the plain error %q", text, recs, err, want)
		}
	}
}

func TestSshkeyAliasesKeepEmptyElements(t *testing.T) {
	g := testClass().Grammar
	recs, err := g.Parse("a,,b, ssh-rsa K")
	if err != nil || len(recs) != 1 {
		t.Fatalf("Parse = %v, %v", recs, err)
	}
	r := recs[0]
	if got := r.Fields["name"]; got.Kind != parsedfile.Str || got.S != "a" {
		t.Errorf("name = %+v, want a", got)
	}
	if got := r.Fields["host_aliases"]; got.Kind != parsedfile.List || len(got.L) != 3 || got.L[0] != "" || got.L[1] != "b" || got.L[2] != "" {
		t.Errorf("host_aliases = %+v, want [\"\", \"b\", \"\"]", got)
	}
}

func TestSshkeyPreGenJoinsNameAndAliases(t *testing.T) {
	g := testClass().Grammar
	absent := parsedfile.Value{Kind: parsedfile.Absent}
	for _, tc := range []struct {
		name    string
		aliases *parsedfile.Value // nil: the field is missing
		want    string
	}{
		{"aliases joined", &parsedfile.Value{Kind: parsedfile.List, L: []string{"a", "b"}}, "n,a,b ssh-rsa K"},
		{"empty list is the bare name", &parsedfile.Value{Kind: parsedfile.List, L: []string{}}, "n ssh-rsa K"},
		{"empty element kept", &parsedfile.Value{Kind: parsedfile.List, L: []string{""}}, "n, ssh-rsa K"},
		{"missing field", nil, "n ssh-rsa K"},
		{"nil", &parsedfile.Value{Kind: parsedfile.Nil}, "n ssh-rsa K"},
		{"absent is the symbol's text", &absent, "n,absent ssh-rsa K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]parsedfile.Value{"name": str("n"), "type": str("ssh-rsa"), "key": str("K")}
			if tc.aliases != nil {
				fields["host_aliases"] = *tc.aliases
			}
			rec := parsedfile.Record{Type: "parsed", Fields: fields}
			got, err := g.ToLine(rec)
			if err != nil || got != tc.want {
				t.Errorf("ToLine = %q, %v; want %q", got, err, tc.want)
			}
			// PreGen works on a copy: the caller's record keeps its fields.
			if tc.aliases != nil && rec.Fields["name"].S != "n" {
				t.Errorf("PreGen changed the caller's name to %+v", rec.Fields["name"])
			}
			if _, ok := rec.Fields["host_aliases"]; ok != (tc.aliases != nil) {
				t.Errorf("PreGen changed the caller's host_aliases presence")
			}
		})
	}
}

func TestSshkeyRequiredFields(t *testing.T) {
	g := testClass().Grammar
	for _, tc := range []struct{ missing, want string }{
		{"type", "Field 'type' is required"},
		{"key", "Field 'key' is required"},
	} {
		fields := map[string]parsedfile.Value{"name": str("n"), "type": str("ssh-rsa"), "key": str("K"), "host_aliases": list()}
		delete(fields, tc.missing)
		if _, err := g.ToLine(parsedfile.Record{Type: "parsed", Fields: fields}); err == nil || err.Error() != tc.want {
			t.Errorf("without %s: error = %v, want %q", tc.missing, err, tc.want)
		}
	}
}

func TestSshkeyReserialisesTabsAsSpaces(t *testing.T) {
	g := testClass().Grammar
	recs, err := g.Parse("# c\ntab.example\tssh-rsa\tTTTT\nbaz,al1 ssh-rsa CCCC # x\n")
	if err != nil {
		t.Fatal(err)
	}
	text, err := g.ToFile(recs)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# c\ntab.example ssh-rsa TTTT\nbaz,al1 ssh-rsa CCCC # x\n"; text != want {
		t.Errorf("ToFile = %q, want %q", text, want)
	}
}

// fakeResource is a catalog resource as ParsedFile sees it.
type fakeResource struct{ name string }

func (f fakeResource) Name() string                 { return f.name }
func (f fakeResource) Title() string                { return f.name }
func (f fakeResource) ShouldTarget() (string, bool) { return "", false }

func TestSshkeyResourceForRecordBindsByNameAtType(t *testing.T) {
	c := testClass()
	byName := map[string]parsedfile.Resource{
		"h.example@ssh-rsa":     fakeResource{"h.example@ssh-rsa"},
		"h.example@ssh-ed25519": fakeResource{"h.example@ssh-ed25519"},
		"h.example":             fakeResource{"h.example"},
	}
	rec := func(name, typ string) *parsedfile.Record {
		return &parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{"name": str(name), "type": str(typ)}}
	}
	for _, tc := range []struct {
		rec  *parsedfile.Record
		want string // "" is a miss
	}{
		{rec("h.example", "ssh-rsa"), "h.example@ssh-rsa"},
		{rec("h.example", "ssh-ed25519"), "h.example@ssh-ed25519"},
		{rec("h.example", "ssh-dss"), ""},
		{rec("other", "ssh-rsa"), ""},
		{&parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{"name": str("h.example"), "type": {Kind: parsedfile.Absent}}}, ""},
	} {
		res, ok := c.ResourceForRecord(tc.rec, byName)
		if tc.want == "" {
			if ok || res != nil {
				t.Errorf("%+v bound %v, want a miss", tc.rec.Fields, res)
			}
			continue
		}
		if !ok || res.Name() != tc.want {
			t.Errorf("%+v bound %v, %v; want %s", tc.rec.Fields, res, ok, tc.want)
		}
	}
}
