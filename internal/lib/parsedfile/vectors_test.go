package parsedfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const (
	vectorsPath   = "testdata/vectors.json"
	generatorPath = "testdata/fileparsing_vectors.rb"

	// oracleRubyVersion is the ruby of the oracle image (openvox 8.34.0).
	oracleRubyVersion = "3.2.11"
)

// noMethodWant matches a want that is a NoMethodError's text.
var noMethodWant = regexp.MustCompile(`"error":\s*"undefined method`)

// Ruby fileparsing.rb digests the parity test accepts: openvox 8.33.0 (the
// version the wants are generated on) and the puppet 8.10.0 gem, which differs
// from it only by cosmetic `self.` prefixes (fp:106-112).
var acceptedFileparsingDigests = map[string]string{
	"e2ce11acbb51693a3030f26a6716964a7eebbfb90f4ea9477bae13a945fb26dc": "openvox 8.33.0",
	"7ac78cd3d64e8b91eea3cd1b92006e41550166f674201cbe9243afe287582ab7": "puppet 8.10.0",
}

// vecValue is a Value in the vectors' encoding: null, {"absent":true},
// {"s":"x"} or {"list":[...]} whose elements are strings or {"absent":true}.
// The engine's Value has no JSON form; this one lives only in the tests.
type vecValue Value

func (v vecValue) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case Nil:
		return []byte("null"), nil
	case Absent:
		return []byte(`{"absent":true}`), nil
	case Str:
		return json.Marshal(map[string]string{"s": v.S})
	case List:
		l := v.L
		if l == nil {
			l = []string{}
		}
		return json.Marshal(map[string][]string{"list": l})
	}
	return nil, fmt.Errorf("unknown kind %d", v.Kind)
}

func (v *vecValue) UnmarshalJSON(b []byte) error {
	var raw struct {
		Absent bool              `json:"absent"`
		S      *string           `json:"s"`
		List   []json.RawMessage `json:"list"`
	}
	if string(b) == "null" {
		*v = vecValue{Kind: Nil}
		return nil
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch {
	case raw.Absent:
		*v = vecValue{Kind: Absent}
	case raw.S != nil:
		*v = vecValue{Kind: Str, S: *raw.S}
	default:
		// Ruby's [:absent] is Kind Absent; the model has no other list holding
		// the symbol, so any other list with that element is a bad vector.
		l := []string{}
		for _, e := range raw.List {
			var s string
			if err := json.Unmarshal(e, &s); err != nil {
				if len(raw.List) == 1 {
					*v = vecValue{Kind: Absent}
					return nil
				}
				return fmt.Errorf("list element %s is not a string: only a lone {\"absent\":true} is supported", e)
			}
			l = append(l, s)
		}
		*v = vecValue{Kind: List, L: l}
	}
	return nil
}

type vecRecord struct {
	Type   string              `json:"type"`
	Line   string              `json:"line"`
	Fields map[string]vecValue `json:"fields"`
}

func (r vecRecord) record() Record {
	rec := Record{Type: r.Type, Line: r.Line, Fields: map[string]Value{}}
	for k, v := range r.Fields {
		rec.Fields[k] = Value(v)
	}
	return rec
}

func vecRecordOf(r *Record) vecRecord {
	out := vecRecord{Type: r.Type, Line: r.Line, Fields: map[string]vecValue{}}
	for k, v := range r.Fields {
		out.Fields[k] = vecValue(v)
	}
	return out
}

type vecWant struct {
	Lines   *[]string    `json:"lines,omitempty"`
	Records *[]vecRecord `json:"records,omitempty"`
	Line    *string      `json:"line,omitempty"`
	Text    *string      `json:"text,omitempty"`
	Error   *string      `json:"error,omitempty"`
}

type vecCase struct {
	Name    string          `json:"name"`
	Grammar string          `json:"grammar"`
	Op      string          `json:"op"`
	Text    string          `json:"text"`
	Record  *vecRecord      `json:"record"`
	Records []vecRecord     `json:"records"`
	Want    json.RawMessage `json:"want"`
}

func readVectors(t *testing.T) []vecCase {
	t.Helper()
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []vecCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases in " + vectorsPath)
	}
	return v.Cases
}

// canonical decodes JSON to plain values so two encodings compare by content.
func canonical(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

// rubyStr is Ruby's interpolation of a Value ("#{v}"): nil is "", :absent is
// "absent".
func rubyStr(v Value) string {
	switch v.Kind {
	case Nil:
		return ""
	case Absent:
		return "absent"
	}
	return v.S
}

func genericGrammars(t *testing.T) map[string]*Grammar {
	t.Helper()
	sp := regexp.MustCompile(Space + `+`)
	re := func(p string) Matcher { return RE2(regexp.MustCompile(p)) }
	text := func(name, p string) LineType { return LineType{Name: name, Text: true, Match: re(p)} }
	defs := map[string][]LineType{
		"split": {
			text("comment", `^#`),
			{Name: "r", Fields: []string{"a", "b", "c"}, Optional: []string{"b", "c"}, Separator: sp, RTS: regexp.MustCompile(Space + `+$`)},
		},
		"colon":   {{Name: "c", Fields: []string{"x", "y"}, Separator: regexp.MustCompile(`:`)}},
		"tabjoin": {{Name: "t", Fields: []string{"a", "b", "c"}, Optional: []string{"b", "c"}, Separator: regexp.MustCompile(`\t`), Joiner: "\t"}},
		"match":   {{Name: "m", Fields: []string{"x", "y", "z"}, Match: re(`^(\w+) ?(\w+)?$`)}},
		"matchabs": {{Name: "m", Fields: []string{"x", "y"}, Match: re(`^(` + NotSpace + `*) (` + NotSpace + `*)$`),
			Absent: "*", Optional: []string{"x", "y"}}},
		"matchempty": {{Name: "m", Fields: []string{"x", "y"}, Match: re(`^(` + NotSpace + `*) (` + NotSpace + `*)$`),
			Optional: []string{"x", "y"}}},
		"lead": {{Name: "l", Fields: []string{"a", "b"}, Optional: []string{"a"}, Separator: sp, RTS: regexp.MustCompile(`^` + Space + `+`)}},
		"order": {
			text("c1", `a`), text("c2", `a`), text("c3", `b`),
		},
		"pregen": {{Name: "t", Text: true, Match: re(`.`), PreGen: func(r *Record) { r.Line = "X" + r.Line }}},
		"hook": {{Name: "h", Fields: []string{"a", "b"}, Separator: sp, RTS: regexp.MustCompile(Space + `+$`),
			PostParse: func(r *Record) error { r.Fields["seen"] = Value{Kind: Str, S: "yes"}; return nil },
			ToLine: func(r Record) (string, error) {
				return "<" + rubyStr(r.Fields["a"]) + "|" + rubyStr(r.Fields["b"]) + ">  ", nil
			}}},
	}
	out := map[string]*Grammar{}
	for name, types := range defs {
		g, err := NewGrammar(types...)
		if err != nil {
			t.Fatalf("grammar %s: %v", name, err)
		}
		out[name] = g
	}
	return out
}

// TestGrammarMatchesSharedVectors runs every generic-grammar case of
// testdata/vectors.json (wants produced by the real fileparsing.rb). Cases of
// the "cron", "host" and "sshkey" grammars are read by cron_core's, host_core's
// and sshkeys_core's tests, which own those grammars.
func TestGrammarMatchesSharedVectors(t *testing.T) {
	grammars := genericGrammars(t)
	ran := 0
	for _, c := range readVectors(t) {
		g, ok := grammars[c.Grammar]
		if !ok {
			if c.Grammar != "cron" && c.Grammar != "host" && c.Grammar != "sshkey" {
				t.Errorf("%s: unknown grammar %q", c.Name, c.Grammar)
			}
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			var got vecWant
			switch c.Op {
			case "lines":
				l := g.Lines(c.Text)
				got.Lines = &l
			case "parse":
				rs, err := g.Parse(c.Text)
				if err != nil {
					e := err.Error()
					got.Error = &e
					break
				}
				out := []vecRecord{}
				for _, r := range rs {
					out = append(out, vecRecordOf(r))
				}
				got.Records = &out
			case "to_line":
				l, err := g.ToLine(c.Record.record())
				if err != nil {
					e := err.Error()
					got.Error = &e
					break
				}
				got.Line = &l
			case "to_file":
				var rs []*Record
				for _, r := range c.Records {
					rec := r.record()
					rs = append(rs, &rec)
				}
				s, err := g.ToFile(rs)
				if err != nil {
					e := err.Error()
					got.Error = &e
					break
				}
				got.Text = &s
			default:
				t.Fatalf("unknown op %q", c.Op)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := canonical(t, b), canonical(t, c.Want); !reflect.DeepEqual(g, w) {
				t.Errorf("got  %s\nwant %s", b, c.Want)
			}
		})
	}
	if ran == 0 {
		t.Error("no generic-grammar case ran")
	}
}

// rubyOrSkip returns ruby's path, or skips naming the unmet condition: ruby on
// PATH, puppet loadable, and a fileparsing.rb with an accepted digest.
func rubyOrSkip(t *testing.T) string {
	t.Helper()
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby is not on PATH")
	}
	var stderr bytes.Buffer
	cmd := exec.Command(ruby, "-e", `require 'puppet'; require 'puppet/util/fileparsing'; `+
		`puts $LOADED_FEATURES.grep(/puppet\/util\/fileparsing\.rb\z/).first`)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("require 'puppet' fails: %v\n%s", err, stderr.String())
	}
	path := strings.TrimSpace(string(out))
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read the loaded fileparsing.rb %q: %v", path, err)
	}
	sum := sha256.Sum256(src)
	if _, ok := acceptedFileparsingDigests[hex.EncodeToString(sum[:])]; !ok {
		t.Skipf("fileparsing.rb %s has digest %x, not an accepted one", path, sum)
	}
	return ruby
}

// TestRubyMatchesSharedVectors re-runs the generator and fails on any
// difference from the committed wants, so the file cannot drift from Ruby.
func TestRubyMatchesSharedVectors(t *testing.T) {
	ruby := rubyOrSkip(t)
	var stderr bytes.Buffer
	cmd := exec.Command(ruby, generatorPath, vectorsPath)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ruby: %v\n%s", err, stderr.String())
	}
	var got struct {
		Cases []struct {
			Name string          `json:"name"`
			Want json.RawMessage `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("ruby output: %v", err)
	}
	cases := readVectors(t)
	if len(got.Cases) != len(cases) {
		t.Fatalf("ruby returned %d cases, want %d", len(got.Cases), len(cases))
	}
	version, err := exec.Command(ruby, "-e", "print RUBY_VERSION").Output()
	if err != nil {
		t.Fatalf("ruby version: %v", err)
	}
	for i, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// A NoMethodError's text is the Ruby version's: the committed wants
			// carry the oracle's (3.2.11), so another Ruby cannot confirm them.
			if string(version) != oracleRubyVersion && noMethodWant.Match(c.Want) {
				t.Skipf("NoMethodError text of ruby %s differs from the oracle's %s", version, oracleRubyVersion)
			}
			if got.Cases[i].Name != c.Name {
				t.Fatalf("case %d is %q, want %q", i, got.Cases[i].Name, c.Name)
			}
			if g, w := canonical(t, got.Cases[i].Want), canonical(t, c.Want); !reflect.DeepEqual(g, w) {
				t.Errorf("ruby gives %s\nfile has  %s", got.Cases[i].Want, c.Want)
			}
		})
	}
}
