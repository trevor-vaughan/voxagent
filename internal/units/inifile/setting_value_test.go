package inifile

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// subsettingReleases are the releases subsetting-<release>.json is generated
// on (testdata/gen/gen.sh); ini_subsetting is the same on all three.
var subsettingReleases = []string{"6.5.0", "6.6.0", "6.6.1"}

type subsettingOp struct {
	Op     string            `json:"op"`
	Args   []json.RawMessage `json:"args"`
	Result json.RawMessage   `json:"result"`
	Error  *rubyErrorWant    `json:"error"`
}

type rubyErrorWant struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

type subsettingCase struct {
	Name string `json:"name"`
	Ctor struct {
		Value *string `json:"value"`
		Sep   string  `json:"sep"`
		Quote string  `json:"quote"`
		KVS   string  `json:"kvs"`
	} `json:"ctor"`
	CtorError *rubyErrorWant `json:"ctor_error"`
	Ops       []subsettingOp `json:"ops"`
}

type subsettingFile struct {
	Openvox string           `json:"openvox"`
	Ruby    string           `json:"ruby"`
	Cases   []subsettingCase `json:"cases"`
}

func readSubsetting(t *testing.T, release string) (subsettingFile, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "subsetting-"+release+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f subsettingFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("subsetting-%s.json: %v", release, err)
	}
	return f, raw
}

func TestSubsettingIdenticalAcrossReleases(t *testing.T) {
	f, first := readSubsetting(t, subsettingReleases[0])
	if f.Openvox != oracleOpenvox || f.Ruby != oracleRuby {
		t.Errorf("subsetting-%s.json: generated on openvox %s ruby %s, want %s %s", subsettingReleases[0], f.Openvox, f.Ruby, oracleOpenvox, oracleRuby)
	}
	if len(f.Cases) == 0 {
		t.Error("subsetting corpus is empty")
	}
	for _, rel := range subsettingReleases[1:] {
		if _, other := readSubsetting(t, rel); !bytes.Equal(first, other) {
			t.Errorf("subsetting-%s.json differs from subsetting-%s.json: the releases behave differently on a call the provider makes", rel, subsettingReleases[0])
		}
	}
}

func svArg[T any](t *testing.T, o subsettingOp, i int) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(o.Args[i], &v); err != nil {
		t.Fatalf("%s arg %d: %v", o.Op, i, err)
	}
	return v
}

// checkRubyError compares err with the exception Ruby raised: class and message.
func checkRubyError(t *testing.T, what string, err error, want rubyErrorWant) {
	t.Helper()
	var re *RubyError
	switch {
	case err == nil:
		t.Errorf("%s: no error, Ruby raised %s: %s", what, want.Class, want.Message)
	case !errors.As(err, &re):
		t.Errorf("%s: error %T %q, want a RubyError of class %s", what, err, err, want.Class)
	case re.Class != want.Class || re.Message != want.Message:
		t.Errorf("%s: %s: %s, Ruby raised %s: %s", what, re.Class, re.Message, want.Class, want.Message)
	}
}

// runSubsettingOp makes one call and returns its result in the corpus's JSON
// form: Ruby's nil is null; get_subsetting_value's `true` (an exact match was
// found) is the non-nil pointer GetSubsettingValue returns for it.
func runSubsettingOp(t *testing.T, sv *SettingValue, o subsettingOp) (any, error) {
	t.Helper()
	switch o.Op {
	case "get_value":
		return sv.GetValue(), nil
	case "get_subsetting_value":
		exact := svArg[bool](t, o, 1)
		got := sv.GetSubsettingValue(svArg[string](t, o, 0), exact)
		switch {
		case got == nil:
			return nil, nil
		case exact:
			return true, nil
		}
		return *got, nil
	case "add_subsetting":
		return nil, sv.AddSubsetting(svArg[string](t, o, 0), svArg[*string](t, o, 1), svArg[bool](t, o, 2), svArg[string](t, o, 3), svArg[*string](t, o, 4))
	case "remove_subsetting":
		sv.RemoveSubsetting(svArg[string](t, o, 0), svArg[bool](t, o, 1))
		return nil, nil
	}
	t.Fatalf("unknown op %q", o.Op)
	return nil, nil
}

func runSubsettingCase(t *testing.T, c subsettingCase) {
	t.Helper()
	sv, err := newSettingValue(c.Ctor.Value, c.Ctor.Sep, c.Ctor.Quote, c.Ctor.KVS)
	if c.CtorError != nil {
		checkRubyError(t, "constructor", err, *c.CtorError)
		return
	}
	if err != nil {
		t.Fatalf("constructor: %v, Ruby built it", err)
	}
	for i, o := range c.Ops {
		got, err := runSubsettingOp(t, sv, o)
		if o.Error != nil {
			checkRubyError(t, o.Op, err, *o.Error)
			continue
		}
		if err != nil {
			t.Errorf("op %d %s: %v, Ruby returned a result", i, o.Op, err)
			continue
		}
		var want any
		if len(o.Result) > 0 {
			if err := json.Unmarshal(o.Result, &want); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("op %d %s %s: got %#v, Ruby's %s", i, o.Op, o.Args, got, o.Result)
		}
	}
}

func TestSubsettingMatchesRuby(t *testing.T) {
	f, _ := readSubsetting(t, subsettingReleases[0])
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) { runSubsettingCase(t, c) })
	}
}

// TestRubyMatchesSubsetting re-runs the generator and fails on any difference
// from the committed corpora.
func TestRubyMatchesSubsetting(t *testing.T) {
	ruby := oracleRubyPath(t)
	for _, rel := range subsettingReleases {
		t.Run(rel, func(t *testing.T) { checkRegenerated(t, ruby, "subsetting", rel) })
	}
}

// The separators and index the corpus cannot hold are left to Ruby: the port
// says so rather than answer differently.
func TestSettingValueUnsupported(t *testing.T) {
	value := "a b"
	for _, sep := range []string{"a", "1", "_", " [", " ]", " -", " &", " ^", " \\", " \xff", "\xff"} {
		if _, err := newSettingValue(&value, sep, "", ""); !errors.Is(err, errUnsupportedSettingValue) {
			t.Errorf("separator %q: error %v, want errUnsupportedSettingValue", sep, err)
		}
	}
	for _, sep := range []string{" ", ",", ", ", ";", ":", "\t", ".", "é", "|", " a", `\`} {
		if _, err := newSettingValue(&value, sep, "", ""); err != nil {
			t.Errorf("separator %q: %v, want it supported", sep, err)
		}
	}
	sv, err := newSettingValue(&value, " ", "", "")
	if err != nil {
		t.Fatal(err)
	}
	min := "-9223372036854775808"
	if err := sv.AddSubsetting("c", nil, false, "index", &min); !errors.Is(err, errUnsupportedSettingValue) {
		t.Errorf("index %s: error %v, want errUnsupportedSettingValue", min, err)
	}
	if got := sv.GetValue(); got != "a b" {
		t.Errorf("value after the refused add: %q", got)
	}
}
