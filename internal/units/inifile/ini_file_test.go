package inifile

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/rubyregexp/rubyregexptest"
)

// corpora are the Ruby-generated IniFile corpora and the releases each is
// generated on (testdata/gen/gen.sh). Removal covers the 6.6.x-only
// remove_section.
var corpora = map[string][]string{
	"edits":   {"6.5.0", "6.6.0", "6.6.1"},
	"removal": {"6.6.0", "6.6.1"},
}

type editsOp struct {
	Op     string            `json:"op"`
	Args   []json.RawMessage `json:"args"`
	Result json.RawMessage   `json:"result"`
	Error  *struct {
		Class   string `json:"class"`
		Message string `json:"message"`
	} `json:"error"`
}

type editsCase struct {
	Name          string `json:"name"`
	Encoding      string `json:"encoding"`
	InputB64      string `json:"input_b64"`
	Missing       bool   `json:"missing"`
	Directory     bool   `json:"directory"`
	MissingParent bool   `json:"missing_parent"`
	Ctor          struct {
		KVS         string `json:"kvs"`
		Prefix      string `json:"prefix"`
		Suffix      string `json:"suffix"`
		IndentChar  string `json:"indent_char"`
		IndentWidth *int   `json:"indent_width"`
	} `json:"ctor"`
	Ops      []editsOp `json:"ops"`
	FinalB64 *string   `json:"final_b64"`
}

type editsFile struct {
	Openvox string      `json:"openvox"`
	Ruby    string      `json:"ruby"`
	Cases   []editsCase `json:"cases"`
}

func readCorpus(t *testing.T, corpus, release string) (editsFile, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", corpus+"-"+release+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f editsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s-%s.json: %v", corpus, release, err)
	}
	return f, raw
}

func TestEditsIdenticalAcrossReleases(t *testing.T) {
	for corpus, releases := range corpora {
		f, first := readCorpus(t, corpus, releases[0])
		if f.Openvox != oracleOpenvox || f.Ruby != oracleRuby {
			t.Errorf("%s-%s.json: generated on openvox %s ruby %s, want %s %s", corpus, releases[0], f.Openvox, f.Ruby, oracleOpenvox, oracleRuby)
		}
		if len(f.Cases) == 0 {
			t.Errorf("%s corpus is empty", corpus)
		}
		for _, rel := range releases[1:] {
			if _, other := readCorpus(t, corpus, rel); !bytes.Equal(first, other) {
				t.Errorf("%s-%s.json differs from %s-%s.json: the releases behave differently on a call the providers make", corpus, rel, corpus, releases[0])
			}
		}
	}
}

// jsonArg decodes call argument i into dst.
func (o editsOp) jsonArg(t *testing.T, i int, dst any) {
	t.Helper()
	if i >= len(o.Args) {
		return
	}
	if err := json.Unmarshal(o.Args[i], dst); err != nil {
		t.Fatalf("%s arg %d: %v", o.Op, i, err)
	}
}

// editsResult is what the Go call returned, in the corpus's JSON form (Ruby's
// nil is null, an empty Array is []).
func (c editsCase) call(t *testing.T, ini *IniFile, o editsOp) (any, error) {
	t.Helper()
	var section string
	var setting *string
	o.jsonArg(t, 0, &section)
	o.jsonArg(t, 1, &setting)
	switch o.Op {
	case "section_names":
		return ini.SectionNames(), nil
	case "section?":
		return ini.IsSection(section), nil
	case "get_value":
		return ini.GetValue(section, setting), nil
	case "set_value":
		if len(o.Args) == 1 {
			ini.SetSection(section)
			return nil, nil
		}
		var sep string
		var value []string
		o.jsonArg(t, 2, &sep)
		o.jsonArg(t, 3, &value)
		return nil, ini.SetValue(section, setting, sep, value)
	case "remove_setting":
		return nil, ini.RemoveSetting(section, setting)
	case "remove_section":
		return nil, ini.RemoveSection(section)
	case "save":
		return nil, ini.Save()
	}
	t.Fatalf("unknown op %q", o.Op)
	return nil, nil
}

// runEditsCase drives the Go port with the case's calls, as the generator
// drove Ruby, and compares every result, error and the final bytes.
func runEditsCase(t *testing.T, c editsCase) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.ini")
	switch {
	case c.Directory:
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	case c.MissingParent:
		path = filepath.Join(dir, "nodir", "f.ini")
	case c.Missing:
	default:
		input, err := base64.StdEncoding.DecodeString(c.InputB64)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, input, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var ini *IniFile
	for i, o := range c.Ops {
		if o.Op == "reopen" {
			ini = nil
			continue
		}
		var got any
		var err error
		if ini == nil {
			ini, err = openIniFile(path, c.Ctor.KVS, c.Ctor.Prefix, c.Ctor.Suffix, c.Ctor.IndentChar, c.Ctor.IndentWidth, c.Encoding)
		}
		if err == nil {
			got, err = c.call(t, ini, o)
		}
		checkOp(t, i, o, got, err, path)
	}
	checkFinal(t, c, path)
}

func checkOp(t *testing.T, i int, o editsOp, got any, err error, path string) {
	t.Helper()
	if o.Error != nil {
		if err == nil {
			t.Errorf("op %d %s: no error, Ruby raised %s: %s", i, o.Op, o.Error.Class, o.Error.Message)
			return
		}
		if msg := strings.ReplaceAll(err.Error(), path, "<path>"); msg != o.Error.Message {
			t.Errorf("op %d %s: error %q, Ruby's message %q", i, o.Op, msg, o.Error.Message)
		}
		var re *RubyError
		isRuby := errors.As(err, &re)
		switch {
		case strings.HasPrefix(o.Error.Class, "Errno::"):
			if isRuby {
				t.Errorf("op %d %s: a RubyError of class %s, Ruby raised %s", i, o.Op, re.Class, o.Error.Class)
			}
		case !isRuby:
			t.Errorf("op %d %s: error %T, want a RubyError of class %s", i, o.Op, err, o.Error.Class)
		case re.Class != o.Error.Class:
			t.Errorf("op %d %s: class %s, Ruby's %s", i, o.Op, re.Class, o.Error.Class)
		}
		return
	}
	if err != nil {
		t.Errorf("op %d %s: %v, Ruby returned a result", i, o.Op, err)
		return
	}
	enc, mErr := json.Marshal(got)
	if mErr != nil {
		t.Fatal(mErr)
	}
	var have, want any
	if err := json.Unmarshal(enc, &have); err != nil {
		t.Fatal(err)
	}
	if len(o.Result) > 0 {
		if err := json.Unmarshal(o.Result, &want); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("op %d %s: got %s, Ruby's %s", i, o.Op, enc, o.Result)
	}
}

func checkFinal(t *testing.T, c editsCase, path string) {
	t.Helper()
	info, statErr := os.Stat(path)
	if c.FinalB64 == nil {
		if statErr == nil && info.Mode().IsRegular() {
			t.Errorf("a file exists at the end, Ruby's does not")
		}
		return
	}
	want, err := base64.StdEncoding.DecodeString(*c.FinalB64)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("final file: %v, Ruby's holds %q", err, want)
		return
	}
	if !bytes.Equal(have, want) {
		t.Errorf("final file %q, Ruby's %q", have, want)
	}
}

func TestEditsMatchRuby(t *testing.T) {
	for corpus, releases := range corpora {
		f, _ := readCorpus(t, corpus, releases[0])
		for _, c := range f.Cases {
			t.Run(corpus+"/"+c.Name+"/"+c.Encoding, func(t *testing.T) { runEditsCase(t, c) })
		}
	}
}

// oracleRubyPath is the ruby that can re-run the generators, or skips the test:
// they need the oracle's puppet and ruby; `gen.sh` runs them in the oracle
// image.
func oracleRubyPath(t *testing.T) string {
	t.Helper()
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby is not on PATH")
	}
	versions, err := exec.Command(ruby, "-e", `require 'puppet'; print Puppet.version, " ", RUBY_VERSION`).Output()
	if err != nil {
		t.Skipf("require 'puppet' fails: %v", err)
	}
	if want := oracleOpenvox + " " + oracleRuby; string(versions) != want {
		t.Skipf("puppet and ruby are %q, the corpus is generated on %q", versions, want)
	}
	return ruby
}

// checkRegenerated re-runs testdata/gen/<corpus>.rb against release's lib and
// fails on any difference from the committed corpus.
func checkRegenerated(t *testing.T, ruby, corpus, release string) {
	t.Helper()
	lib, err := filepath.Abs(filepath.Join("testdata", release, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ruby, filepath.Join("testdata", "gen", corpus+".rb"))
	cmd.Env = append(os.Environ(), "RUBYLIB="+lib)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ruby: %v\n%s", err, stderr.String())
	}
	committed, err := os.ReadFile(filepath.Join("testdata", corpus+"-"+release+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(committed)) {
		t.Errorf("%s-%s.json differs from the generator's output; rerun testdata/gen/gen.sh %s %s", corpus, release, release, corpus)
	}
}

// TestRubyMatchesEdits re-runs the generators and fails on any difference from
// the committed corpora (as parsedfile's TestRubyMatchesSharedVectors).
func TestRubyMatchesEdits(t *testing.T) {
	ruby := oracleRubyPath(t)
	for corpus, releases := range corpora {
		for _, rel := range releases {
			t.Run(corpus+"/"+rel, func(t *testing.T) { checkRegenerated(t, ruby, corpus, rel) })
		}
	}
}

func TestNoRubyWhitespaceEscapes(t *testing.T) {
	rubyregexptest.NoRubyWhitespaceEscapes(t)
}
