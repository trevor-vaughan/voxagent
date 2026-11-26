package inifile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// mungeReleases are the releases with a munge corpus (testdata/gen/gen.sh).
var mungeReleases = []string{"6.5.0", "6.6.0", "6.6.1"}

const (
	oracleOpenvox = "8.34.0"
	oracleRuby    = "3.2.11"
)

// tagged is the corpus's encoding of a Ruby value: {"t": <class>, "v": ...}.
type tagged struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v,omitempty"`
}

type mungeCase struct {
	Type      string            `json:"type"`
	Given     map[string]tagged `json:"given"`
	Sensitive []string          `json:"sensitive"`
	Result    struct {
		Attrs map[string]tagged `json:"attrs"`
		Error string            `json:"error"`
	} `json:"result"`
}

type mungeFile struct {
	Openvox string      `json:"openvox"`
	Ruby    string      `json:"ruby"`
	Cases   []mungeCase `json:"cases"`
}

func readMunge(t *testing.T, release string) (mungeFile, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "munge-"+release+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f mungeFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("munge-%s.json: %v", release, err)
	}
	return f, raw
}

// value decodes a tagged value as a catalog carries it: a decoded number is a
// float64, an Integer and a Float alike (the catalog's whole-number rule).
func (v tagged) value(t *testing.T) any {
	t.Helper()
	switch v.T {
	case "Nil":
		return nil
	case "String":
		var s string
		mustUnmarshal(t, v.V, &s)
		return s
	case "Integer", "Float":
		var f float64
		mustUnmarshal(t, v.V, &f)
		return f
	case "Boolean":
		var b bool
		mustUnmarshal(t, v.V, &b)
		return b
	case "Array":
		var elems []tagged
		mustUnmarshal(t, v.V, &elems)
		out := make([]any, len(elems))
		for i, e := range elems {
			out[i] = e.value(t)
		}
		return out
	}
	t.Fatalf("unknown tag %q", v.T)
	return nil
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
}

func (v tagged) str(t *testing.T) string {
	t.Helper()
	var s string
	mustUnmarshal(t, v.V, &s)
	return s
}

func (c mungeCase) resource(t *testing.T) catalog.Resource {
	t.Helper()
	params := map[string]any{}
	required := map[string]map[string]any{
		"ini_setting":    {"path": "/x", "setting": "s"},
		"ini_subsetting": {"path": "/x", "setting": "s", "subsetting": "ss"},
		"ini_section":    {"path": "/x"},
	}
	maps := required[c.Type]
	for k, v := range maps {
		params[k] = v
	}
	for k, v := range c.Given {
		params[k] = v.value(t)
	}
	return catalog.Resource{
		Type: strings.ToUpper(c.Type[:1]) + c.Type[1:], Title: "n",
		Parameters: params, SensitiveParameters: c.Sensitive,
	}
}

// allowedShapes lists, per attribute, the value tags the native parser models;
// any other shape of an attribute Ruby accepts is a *rubyvalue.UnimplementedValue
// (spec §2 condition 6), as is any value whose Ruby failure is a version-specific
// NoMethodError text.
func allowedShapes(typ, attr string) []string {
	switch attr {
	case "force_new_section_creation", "refreshonly", "use_exact_match", "delete_if_empty":
		return []string{"Boolean", "String"}
	case "show_diff":
		return []string{"Boolean", "String"}
	case "ensure":
		return []string{"String", "Array"}
	case "indent_width":
		return []string{"String", "Integer", "Float"}
	case "value":
		if typ == "ini_setting" {
			return []string{"String", "Integer", "Float", "Boolean", "Array"}
		}
	}
	return []string{"String"}
}

// unmodelled reports whether the case gives a shape the parser refuses though
// Ruby accepts it.
func (c mungeCase) unmodelled() bool {
	for attr, v := range c.Given {
		if !slices.Contains(allowedShapes(c.Type, attr), v.T) {
			return true
		}
	}
	return false
}

// redact is Ruby's error text with each Sensitive value replaced as the native
// parser does (D-78): its inspect and its plain text both become [redacted].
func (c mungeCase) redact(t *testing.T, text string) string {
	for _, attr := range c.Sensitive {
		v, ok := c.Given[attr]
		if !ok {
			continue
		}
		raw := v.value(t)
		if s, isString := raw.(string); isString {
			text = strings.ReplaceAll(text, rubyvalue.InspectValue(s), "[redacted]")
			text = strings.ReplaceAll(text, "'"+s+"'", "'[redacted]'")
		} else {
			text = strings.ReplaceAll(text, rubyvalue.InspectValue(raw), "[redacted]")
		}
	}
	return text
}

func TestMungeIdenticalAcrossReleases(t *testing.T) {
	var shared [][]mungeCase
	for _, rel := range mungeReleases {
		f, _ := readMunge(t, rel)
		if f.Openvox != oracleOpenvox || f.Ruby != oracleRuby {
			t.Errorf("munge-%s.json: generated on openvox %s ruby %s, want %s and %s", rel, f.Openvox, f.Ruby, oracleOpenvox, oracleRuby)
		}
		var cases []mungeCase
		sections := 0
		for _, c := range f.Cases {
			if c.Type == "ini_section" {
				sections++
				continue
			}
			cases = append(cases, c)
		}
		if (rel == "6.5.0") != (sections == 0) {
			t.Errorf("munge-%s.json has %d ini_section cases; only 6.6.x has the type", rel, sections)
		}
		shared = append(shared, cases)
	}
	for i := 1; i < len(shared); i++ {
		if !reflect.DeepEqual(shared[0], shared[i]) {
			t.Errorf("munge-%s.json differs from munge-%s.json in the ini_setting/ini_subsetting cases", mungeReleases[i], mungeReleases[0])
		}
	}
	f6, _ := readMunge(t, "6.6.0")
	f61, _ := readMunge(t, "6.6.1")
	if !reflect.DeepEqual(f6.Cases, f61.Cases) {
		t.Error("munge-6.6.0.json and munge-6.6.1.json differ")
	}
}

func TestParamsMatchRuby(t *testing.T) {
	for _, rel := range mungeReleases {
		f, _ := readMunge(t, rel)
		for i, c := range f.Cases {
			t.Run(fmt.Sprintf("%s/%03d-%s", rel, i, c.Type), func(t *testing.T) {
				r := c.resource(t)
				var got any
				var err error
				switch c.Type {
				case "ini_setting":
					got, err = parseSetting(r)
				case "ini_subsetting":
					got, err = parseSubsetting(r)
				case "ini_section":
					got, err = parseSection(r)
				}
				checkCase(t, c, got, err)
			})
		}
	}
}

func checkCase(t *testing.T, c mungeCase, got any, err error) {
	t.Helper()
	given, _ := json.Marshal(c.Given)
	var unimplemented *rubyvalue.UnimplementedValue
	switch {
	case c.Result.Error == "" && c.unmodelled():
		if !errors.As(err, &unimplemented) {
			t.Fatalf("given %s: err = %v, want an UnimplementedValue", given, err)
		}
	case c.Result.Error == "":
		if err != nil {
			t.Fatalf("given %s: %v", given, err)
		}
		compareAttrs(t, c, got)
	case strings.Contains(c.Result.Error, "undefined method"):
		// The NoMethodError text is the Ruby version's: delegate (cron's rule).
		if !errors.As(err, &unimplemented) {
			t.Fatalf("given %s: err = %v, want an UnimplementedValue for %q", given, err, c.Result.Error)
		}
	default:
		if err == nil {
			t.Fatalf("given %s: no error, want %q", given, c.Result.Error)
		}
		if want := c.redact(t, c.Result.Error); err.Error() != want {
			t.Fatalf("given %s:\n got %q\nwant %q", given, err.Error(), want)
		}
	}
	if unimplemented != nil && strings.Contains(unimplemented.Shown, "s3cret") {
		t.Errorf("refusal shows a Sensitive value: %v", unimplemented)
	}
}

// compareAttrs checks every attribute Ruby recorded that the params carry.
func compareAttrs(t *testing.T, c mungeCase, got any) {
	t.Helper()
	want := c.Result.Attrs
	check := func(attr string, have any) {
		t.Helper()
		w, ok := want[attr]
		if !ok {
			t.Fatalf("corpus has no %s", attr)
		}
		if attr == "ensure" && have == "" {
			have = nil // ensure => [] leaves the resource without an ensure
		}
		if w.T == "Nil" && have == false {
			have = nil // Ruby's `defaultto false` sets nothing
		}
		if exp := w.expect(t, attr); !reflect.DeepEqual(plain(have), exp) {
			t.Errorf("%s = %#v, want %#v (Ruby %s)", attr, have, exp, w.V)
		}
	}
	// indent_width is the type's value, whose to_i the IniFile applies
	// (Ruby: ini_file.rb:17); the params hold the to_i.
	checkIndent := func(have *int) {
		t.Helper()
		w := want["indent_width"]
		var exp any
		switch w.T {
		case "Integer", "Float":
			var f float64
			mustUnmarshal(t, w.V, &f)
			exp = int(math.Trunc(f))
		case "String":
			exp = int(rubyvalue.ToI(w.str(t)))
		}
		if !reflect.DeepEqual(plain(have), exp) {
			t.Errorf("indent_width = %v, want %v (Ruby %s)", plain(have), exp, w.V)
		}
	}
	switch p := got.(type) {
	case settingParams:
		check("ensure", p.Ensure)
		check("value", p.Value)
		check("section", p.Section)
		check("setting", p.Setting)
		check("force_new_section_creation", p.ForceNewSectionCreation)
		check("path", p.Path)
		check("key_val_separator", p.KeyValSeparator)
		check("section_prefix", p.SectionPrefix)
		check("section_suffix", p.SectionSuffix)
		check("indent_char", p.IndentChar)
		checkIndent(p.IndentWidth)
		check("refreshonly", p.Refreshonly)
	case subsettingParams:
		check("ensure", p.Ensure)
		check("section", p.Section)
		check("setting", p.Setting)
		check("subsetting", p.Subsetting)
		check("subsetting_separator", p.SubsettingSeparator)
		check("subsetting_key_val_separator", p.SubsettingKeyValSeparator)
		check("path", p.Path)
		check("key_val_separator", p.KeyValSeparator)
		check("quote_char", p.QuoteChar)
		check("use_exact_match", p.UseExactMatch)
		check("insert_type", p.InsertType)
		check("insert_value", p.InsertValue)
		check("delete_if_empty", p.DeleteIfEmpty)
		check("value", p.Value)
	case sectionParams:
		check("ensure", p.Ensure)
		check("section", p.Section)
		check("path", p.Path)
		check("section_prefix", p.SectionPrefix)
		check("section_suffix", p.SectionSuffix)
	default:
		t.Fatalf("unexpected params %T", got)
	}
}

// plain turns a params field into the form expect gives: a nil pointer or
// slice is nil, a pointer its target.
func plain(have any) any {
	switch h := have.(type) {
	case *string:
		if h == nil {
			return nil
		}
		return *h
	case *int:
		if h == nil {
			return nil
		}
		return *h
	case []string:
		if h == nil {
			return nil
		}
	}
	return have
}

// expect is the value of the params field for Ruby's munged value: a String or
// Symbol a string (use_exact_match and delete_if_empty keep the Symbol :true
// or :false as a bool), a Boolean a bool, an Array of Strings a []string, a
// Nil nil.
func (v tagged) expect(t *testing.T, attr string) any {
	t.Helper()
	switch v.T {
	case "Nil":
		return nil
	case "Symbol":
		if attr == "use_exact_match" || attr == "delete_if_empty" {
			return v.str(t) == "true"
		}
		return v.str(t)
	case "String":
		return v.str(t)
	case "Boolean":
		var b bool
		mustUnmarshal(t, v.V, &b)
		return b
	case "Array":
		var elems []tagged
		mustUnmarshal(t, v.V, &elems)
		out := []string{}
		for _, e := range elems {
			out = append(out, e.str(t))
		}
		return out
	}
	t.Fatalf("unknown tag %q", v.T)
	return nil
}

// TestIndentWidthIsToI pins the to_i of Integer, Float and String values.
func TestIndentWidthIsToI(t *testing.T) {
	for given, want := range map[any]int{"2": 2, "x": 0, " 3": 3, "-1": -1, "1_0": 10, "07": 7, "0x1": 0, "1e2": 1, "+3": 3, 2.0: 2, 1.5: 1, -1.0: -1} {
		r := catalog.Resource{Type: "Ini_setting", Title: "n", Parameters: map[string]any{"path": "/x", "setting": "s", "indent_width": given}}
		p, err := parseSetting(r)
		if err != nil || p.IndentWidth == nil || *p.IndentWidth != want {
			t.Errorf("indent_width %#v: %v, %v; want %d", given, p.IndentWidth, err, want)
		}
	}
}

// TestSensitiveValueRedactedInError: a refused Sensitive value is shown as
// [redacted] (rubyvalue.ShownValue, D-78), where Ruby prints it.
func TestSensitiveValueRedactedInError(t *testing.T) {
	const secret = "s3cret"
	for _, tc := range []struct {
		name   string
		typ    string
		params map[string]any
		attr   string
		want   string
	}{
		{"path", "Ini_setting", map[string]any{"path": "rel-" + secret}, "path",
			"Parameter path failed on Ini_setting[n]: File paths must be fully qualified, not '[redacted]'"},
		{"setting", "Ini_setting", map[string]any{"setting": " " + secret}, "setting",
			"Parameter setting failed on Ini_setting[n]: Munging failed for value [redacted] in class setting: private method `warn' called for Puppet:Module"},
		{"show_diff", "Ini_subsetting", map[string]any{"show_diff": secret}, "show_diff",
			"Parameter show_diff failed on Ini_subsetting[n]: Invalid value [redacted]. Valid values are true, md5, false."},
		{"quote_char", "Ini_subsetting", map[string]any{"quote_char": secret}, "quote_char",
			"Parameter quote_char failed on Ini_subsetting[n]: :quote_char valid values are '', '\"' and \"'\""},
		{"ensure", "Ini_section", map[string]any{"ensure": secret}, "ensure",
			"Parameter ensure failed on Ini_section[n]: Invalid value [redacted]. Valid values are present, absent."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"path": "/x", "setting": "s", "subsetting": "ss"}
			for k, v := range tc.params {
				params[k] = v
			}
			r := catalog.Resource{Type: tc.typ, Title: "n", Parameters: params, SensitiveParameters: []string{tc.attr}}
			var err error
			switch tc.typ {
			case "Ini_setting":
				_, err = parseSetting(r)
			case "Ini_subsetting":
				_, err = parseSubsetting(r)
			default:
				_, err = parseSection(r)
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("value-refusal", func(t *testing.T) {
		r := catalog.Resource{Type: "Ini_setting", Title: "n", SensitiveParameters: []string{"value"},
			Parameters: map[string]any{"path": "/x", "setting": "s", "value": []any{[]any{secret}}}}
		_, err := parseSetting(r)
		var u *rubyvalue.UnimplementedValue
		if !errors.As(err, &u) || strings.Contains(err.Error(), secret) {
			t.Fatalf("err = %v, want an UnimplementedValue without the secret", err)
		}
	})
}

// TestHashValueRefused: inspect of a Hash is not ported (D-53), so a Hash in
// any attribute goes to the extension host.
func TestHashValueRefused(t *testing.T) {
	for _, attr := range []string{"path", "section", "value", "ensure", "show_diff", "refreshonly", "indent_width"} {
		r := catalog.Resource{Type: "Ini_setting", Title: "n",
			Parameters: map[string]any{"path": "/x", "setting": "s", attr: map[string]any{"a": "b"}}}
		_, err := parseSetting(r)
		var u *rubyvalue.UnimplementedValue
		if !errors.As(err, &u) || u.Attr != attr {
			t.Errorf("%s: err = %v, want an UnimplementedValue for it", attr, err)
		}
	}
}

// TestRubyMatchesMungeCorpus re-runs the generator and fails on any difference
// from the committed corpus (as parsedfile's TestRubyMatchesSharedVectors). It
// needs the oracle's puppet and ruby; `gen.sh` runs them in the oracle image.
func TestRubyMatchesMungeCorpus(t *testing.T) {
	ruby := oracleRubyPath(t)
	for _, rel := range mungeReleases {
		t.Run(rel, func(t *testing.T) {
			lib, err := filepath.Abs(filepath.Join("testdata", rel, "lib"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(ruby, filepath.Join("testdata", "gen", "munge.rb"))
			cmd.Env = append(os.Environ(), "RUBYLIB="+lib)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("ruby: %v\n%s", err, stderr.String())
			}
			_, committed := readMunge(t, rel)
			if !bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(committed)) {
				t.Errorf("munge-%s.json differs from the generator's output; rerun testdata/gen/gen.sh %s munge", rel, rel)
			}
		})
	}
}
