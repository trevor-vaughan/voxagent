package ral

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

func concatFileRes(title string, params map[string]any, marked ...string) catalog.Resource {
	return catalog.Resource{Type: "Concat_file", Title: title, Parameters: params, SensitiveParameters: marked}
}

func concatFragRes(title string, params map[string]any, marked ...string) catalog.Resource {
	return catalog.Resource{Type: "Concat_fragment", Title: title, Parameters: params, SensitiveParameters: marked}
}

func TestConcatFileParams(t *testing.T) {
	const c17 = "/etc/c17.conf"
	cases := []struct {
		name   string
		title  string
		params map[string]any
		marked []string
		want   string // "" = accepted
	}{
		{"relative path", "relative.conf", nil, nil, "Parameter path failed on Concat_file[relative.conf]: File paths must be fully qualified, not 'relative.conf'"},
		{"relative path param", "/etc/t", map[string]any{"path": "rel"}, nil, "Parameter path failed on Concat_file[/etc/t]: File paths must be fully qualified, not 'rel'"},
		{"marked relative path", "rel", nil, []string{"path"}, "Parameter path failed on Concat_file[rel]: File paths must be fully qualified, not '[redacted]'"},
		{"ensure", c17, map[string]any{"ensure": "x"}, nil, `Parameter ensure failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are present, absent.`},
		{"ensure marked", c17, map[string]any{"ensure": "x"}, []string{"ensure"}, `Parameter ensure failed on Concat_file[/etc/c17.conf]: Invalid value [redacted]. Valid values are present, absent.`},
		{"ensure file is not concat's", c17, map[string]any{"ensure": "file"}, nil, `Parameter ensure failed on Concat_file[/etc/c17.conf]: Invalid value "file". Valid values are present, absent.`},
		{"format", "/etc/c17f1.conf", map[string]any{"format": "toml"}, nil, `Parameter format failed on Concat_file[/etc/c17f1.conf]: Invalid value "toml". Valid values are plain, yaml, json, json-array, json-pretty, json-array-pretty.`},
		{"validate_cmd", c17, map[string]any{"validate_cmd": float64(1)}, nil, "Parameter validate_cmd failed on Concat_file[/etc/c17.conf]: Validate_cmd must be a String"},
		{"backup number", c17, map[string]any{"backup": float64(1)}, nil, "Parameter backup failed on Concat_file[/etc/c17.conf]: Backup must be a Boolean or String"},
		{"backup array", c17, map[string]any{"backup": []any{"a"}}, nil, "Parameter backup failed on Concat_file[/etc/c17.conf]: Backup must be a Boolean or String"},
		{"order", c17, map[string]any{"order": "sideways"}, nil, `Parameter order failed on Concat_file[/etc/c17.conf]: Invalid value "sideways". Valid values are alpha, numeric.`},
		{"order case", c17, map[string]any{"order": "Alpha"}, nil, `Parameter order failed on Concat_file[/etc/c17.conf]: Invalid value "Alpha". Valid values are alpha, numeric.`},
		{"replace", c17, map[string]any{"replace": "maybe"}, nil, `Parameter replace failed on Concat_file[/etc/c17.conf]: Invalid value "maybe". Valid values are true, false, yes, no.`},
		{"replace TRUE", c17, map[string]any{"replace": "TRUE"}, nil, `Parameter replace failed on Concat_file[/etc/c17.conf]: Invalid value "TRUE". Valid values are true, false, yes, no.`},
		{"replace number", c17, map[string]any{"replace": float64(1)}, nil, `Parameter replace failed on Concat_file[/etc/c17.conf]: Invalid value 1. Valid values are true, false, yes, no.`},
		{"ensure_newline", c17, map[string]any{"ensure_newline": "x"}, nil, `Parameter ensure_newline failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are true, false, yes, no.`},
		{"force", c17, map[string]any{"force": "x"}, nil, `Parameter force failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are true, false, yes, no.`},
		{"selinux_ignore_defaults", c17, map[string]any{"selinux_ignore_defaults": "x"}, nil, `Parameter selinux_ignore_defaults failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are true, false, yes, no.`},
		{"show_diff", c17, map[string]any{"show_diff": "x"}, nil, `Parameter show_diff failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are true, false, yes, no.`},
		{"create_empty_file", c17, map[string]any{"create_empty_file": "x"}, nil, `Parameter create_empty_file failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are true, false, yes, no.`},
		{"replace array", c17, map[string]any{"replace": []any{true}}, nil, `Parameter replace failed on Concat_file[/etc/c17.conf]: Invalid value [true]. Valid values are true, false, yes, no.`},
		{"force array", c17, map[string]any{"force": []any{"a", float64(1)}}, nil, `Parameter force failed on Concat_file[/etc/c17.conf]: Invalid value ["a", 1]. Valid values are true, false, yes, no.`},
		{"group false", "/etc/c23.conf", map[string]any{"group": false}, nil, "Parameter group failed on Concat_file[/etc/c23.conf]: Invalid group name 'false'"},
		{"group empty", "/etc/c23.conf", map[string]any{"group": ""}, nil, `Parameter group failed on Concat_file[/etc/c23.conf]: Invalid group name '""'`},
		{"group marked", "/etc/c23.conf", map[string]any{"group": false}, []string{"group"}, "Parameter group failed on Concat_file[/etc/c23.conf]: Invalid group name '[redacted]'"},
		{"mode class", c17, map[string]any{"mode": float64(644)}, nil, "Parameter mode failed on Concat_file[/etc/c17.conf]: The file mode specification must be a string, not 'Integer'"},
		{"mode invalid", c17, map[string]any{"mode": "rw"}, nil, `Parameter mode failed on Concat_file[/etc/c17.conf]: The file mode specification is invalid: "rw"`},
		{"mode marked", c17, map[string]any{"mode": "rw"}, []string{"mode"}, "Parameter mode failed on Concat_file[/etc/c17.conf]: The file mode specification is invalid: [redacted]"},
		{"selrange", c17, map[string]any{"selrange": true}, nil, "Parameter selrange failed on Concat_file[/etc/c17.conf]: Selrange must be a String"},
		{"selrole", c17, map[string]any{"selrole": true}, nil, "Parameter selrole failed on Concat_file[/etc/c17.conf]: Selrole must be a String"},
		{"seltype", c17, map[string]any{"seltype": true}, nil, "Parameter seltype failed on Concat_file[/etc/c17.conf]: Seltype must be a String"},
		{"seluser", c17, map[string]any{"seluser": true}, nil, "Parameter seluser failed on Concat_file[/etc/c17.conf]: Seluser must be a String"},
		// First failure follows §3.1's order: path, ensure, tag, owner, group, mode, order, backup, replace, ...
		{"path before ensure", "rel", map[string]any{"ensure": "x"}, nil, "Parameter path failed on Concat_file[rel]: File paths must be fully qualified, not 'rel'"},
		{"ensure before group", c17, map[string]any{"ensure": "x", "group": ""}, nil, `Parameter ensure failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are present, absent.`},
		{"group before order", c17, map[string]any{"group": "", "order": "x"}, nil, `Parameter group failed on Concat_file[/etc/c17.conf]: Invalid group name '""'`},
		{"order before backup", c17, map[string]any{"order": "x", "backup": float64(1)}, nil, `Parameter order failed on Concat_file[/etc/c17.conf]: Invalid value "x". Valid values are alpha, numeric.`},
		{"validate_cmd before format", c17, map[string]any{"validate_cmd": float64(1), "format": "toml"}, nil, "Parameter validate_cmd failed on Concat_file[/etc/c17.conf]: Validate_cmd must be a String"},
		{"format before seluser", c17, map[string]any{"format": "toml", "seluser": true}, nil, `Parameter format failed on Concat_file[/etc/c17.conf]: Invalid value "toml". Valid values are plain, yaml, json, json-array, json-pretty, json-array-pretty.`},
		// Accepted: Ruby takes these.
		{"all strings and booleans", c17, map[string]any{"ensure": "absent", "order": "alpha", "replace": "yes", "backup": ".bak", "group": "g", "owner": float64(0), "mode": "0644", "tag": "t", "format": "plain", "show_diff": "no"}, nil, ""},
		{"group as uid number", c17, map[string]any{"group": float64(0)}, nil, ""},
		{"backup false", c17, map[string]any{"backup": false}, nil, ""},
		{"tag false", c17, map[string]any{"tag": false}, nil, ""},
		{"tag list", c17, map[string]any{"tag": []any{"a", "b"}}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := concatFileRes(tc.title, tc.params, tc.marked...)
			_, err := parseConcatFile(r)
			verr := ConcatFile{}.Validate(r)
			if tc.want == "" {
				if err != nil || verr != nil {
					t.Fatalf("parse: %v, Validate: %v; want accepted", err, verr)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("parse error = %v\nwant %s", err, tc.want)
			}
			if verr == nil || verr.Error() != tc.want {
				t.Fatalf("Validate error = %v\nwant %s", verr, tc.want)
			}
			var uv *rubyvalue.UnimplementedValue
			if errors.As(err, &uv) {
				t.Fatalf("a Ruby refusal must not read as an unimplemented value: %v", err)
			}
		})
	}
}

func TestConcatFileParamsValues(t *testing.T) {
	got, err := parseConcatFile(concatFileRes("/etc/a.conf", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := concatFileParams{Path: "/etc/a.conf", Order: "numeric", CreateEmptyFile: true, Replace: true,
		Copied: map[string]any{"replace": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v\nwant %#v", got, want)
	}

	got, err = parseConcatFile(concatFileRes("/etc/title", map[string]any{
		"path": "/etc/b.conf", "ensure": "absent", "tag": []any{"x", "y"}, "order": "alpha",
		"ensure_newline": "yes", "create_empty_file": "false", "replace": "no", "force": true,
		"owner": "root", "group": "wheel", "mode": "0640", "backup": ".bak", "validate_cmd": "/bin/true %",
		"selinux_ignore_defaults": "true", "show_diff": false, "selrange": "s0", "selrole": "r", "seltype": "t", "seluser": "u",
		"format": "plain",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want = concatFileParams{Path: "/etc/b.conf", Absent: true, Tag: []string{"x", "y"}, Order: "alpha",
		EnsureNewline: true, CreateEmptyFile: false, Replace: false,
		Copied: map[string]any{"owner": "root", "group": "wheel", "mode": "0640", "backup": ".bak",
			"validate_cmd": "/bin/true %", "selinux_ignore_defaults": true, "show_diff": false,
			"selrange": "s0", "selrole": "r", "seltype": "t", "seluser": "u", "replace": false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit = %#v\nwant %#v", got, want)
	}

	if got, _ = parseConcatFile(concatFileRes("/etc/a.conf", map[string]any{"tag": "t"})); got.Tag != "t" {
		t.Errorf("string tag = %#v", got.Tag)
	}
	if got, _ = parseConcatFile(concatFileRes("/etc/a.conf", map[string]any{"tag": false})); got.Tag != nil {
		t.Errorf("tag false must read as unset (M22), got %#v", got.Tag)
	}
}

// TestConcatFileUnimplemented: shapes Ruby accepts and native does not
// implement come back as *rubyvalue.UnimplementedValue (spec §0 (b), (d), (e), (f)).
func TestConcatFileUnimplemented(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		params map[string]any
		attr   string
	}{
		{"format yaml", "/etc/a", map[string]any{"format": "yaml"}, "format"},
		{"format json-array-pretty", "/etc/a", map[string]any{"format": "json-array-pretty"}, "format"},
		{"backup true", "/etc/a", map[string]any{"backup": true}, "backup"},
		{"trailing slash", "/etc/a/", nil, "path"},
		{"double slash", "/etc//a", nil, "path"},
		{"dot segment", "/etc/./a", nil, "path"},
		{"dotdot segment", "/etc/../a", nil, "path"},
		{"root", "/", nil, "path"},
		{"windows drive", `C:/x`, nil, "path"},
		{"windows drive backslash", `C:\x`, nil, "path"},
		{"windows unc", `\\host\share`, nil, "path"},
		{"path not a string", "/etc/a", map[string]any{"path": float64(1)}, "path"},
		{"rich value", "/etc/a", map[string]any{"owner": map[string]any{"__ptype": "Sensitive", "__pvalue": "x"}}, "owner"},
		{"tag number", "/etc/a", map[string]any{"tag": float64(1)}, "tag"},
		{"tag true", "/etc/a", map[string]any{"tag": true}, "tag"},
		{"ensure hash", "/etc/a", map[string]any{"ensure": map[string]any{"a": float64(1)}}, "ensure"},
		{"order hash", "/etc/a", map[string]any{"order": map[string]any{"a": float64(1)}}, "order"},
		{"format hash", "/etc/a", map[string]any{"format": map[string]any{"a": float64(1)}}, "format"},
		{"replace hash", "/etc/a", map[string]any{"replace": map[string]any{"a": float64(1)}}, "replace"},
		{"show_diff hash", "/etc/a", map[string]any{"show_diff": map[string]any{"a": float64(1)}}, "show_diff"},
		{"create_empty_file array holding a hash", "/etc/a", map[string]any{"create_empty_file": []any{map[string]any{"a": "b"}}}, "create_empty_file"},
		{"tag list of numbers", "/etc/a", map[string]any{"tag": []any{"a", float64(1)}}, "tag"},
		{"tag hash", "/etc/a", map[string]any{"tag": map[string]any{"a": "b"}}, "tag"},
		{"owner list", "/etc/a", map[string]any{"owner": []any{"a"}}, "owner"},
		{"group list", "/etc/a", map[string]any{"group": []any{"a"}}, "group"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConcatFile(concatFileRes(tc.title, tc.params))
			var uv *rubyvalue.UnimplementedValue
			if !errors.As(err, &uv) {
				t.Fatalf("error = %v, want *rubyvalue.UnimplementedValue", err)
			}
			if uv.Attr != tc.attr {
				t.Errorf("attr = %q, want %q (%v)", uv.Attr, tc.attr, err)
			}
		})
	}
}

func TestConcatFragmentParams(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		params map[string]any
		want   string
	}{
		{"target", "x", map[string]any{"target": float64(1), "content": "c"}, "Parameter target failed on Concat_fragment[x]: Target must be a String"},
		{"content", "y", map[string]any{"target": "/t", "content": float64(1)}, "Parameter content failed on Concat_fragment[y]: Content must be a String"},
		{"order float", "z", map[string]any{"target": "/t", "content": "c", "order": 1.5}, "Parameter order failed on Concat_fragment[z]: $order is not a string or integer."},
		{"order true", "z", map[string]any{"target": "/t", "content": "c", "order": true}, "Parameter order failed on Concat_fragment[z]: $order is not a string or integer."},
		{"order slash", "x", map[string]any{"target": "/t", "content": "c", "order": "a/b"}, "Parameter order failed on Concat_fragment[x]: Order cannot contain '/', ':', or '\\n'."},
		{"order colon", "x", map[string]any{"target": "/t", "content": "c", "order": "a:b"}, "Parameter order failed on Concat_fragment[x]: Order cannot contain '/', ':', or '\\n'."},
		{"order newline", "x", map[string]any{"target": "/t", "content": "c", "order": "a\nb"}, "Parameter order failed on Concat_fragment[x]: Order cannot contain '/', ':', or '\\n'."},
		{"no target or tag", "x", map[string]any{"content": "c"}, "Validation of Concat_fragment[x] failed: No 'target' or 'tag' set"},
		{"tag false, no target", "x", map[string]any{"content": "c", "tag": false}, "Validation of Concat_fragment[x] failed: No 'target' or 'tag' set"},
		{"neither source nor content", "x", map[string]any{"target": "/t"}, "Validation of Concat_fragment[x] failed: Set either 'source' or 'content'"},
		{"target before content", "x", map[string]any{"target": true, "content": true}, "Parameter target failed on Concat_fragment[x]: Target must be a String"},
		{"content before order", "x", map[string]any{"target": "/t", "content": true, "order": "a/b"}, "Parameter content failed on Concat_fragment[x]: Content must be a String"},
		{"order before validation", "x", map[string]any{"order": "a/b"}, "Parameter order failed on Concat_fragment[x]: Order cannot contain '/', ':', or '\\n'."},
		{"validation: target-or-tag before content", "x", map[string]any{}, "Validation of Concat_fragment[x] failed: No 'target' or 'tag' set"},
		{"tag alone is enough", "x", map[string]any{"tag": "t"}, "Validation of Concat_fragment[x] failed: Set either 'source' or 'content'"},
		{"accepted", "x", map[string]any{"target": "/t", "content": "c", "order": "05", "tag": "t"}, ""},
		{"tag only with content", "x", map[string]any{"tag": []any{"a"}, "content": ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := concatFragRes(tc.title, tc.params)
			_, err := parseConcatFragment(r)
			verr := ConcatFragment{}.Validate(r)
			if tc.want == "" {
				if err != nil || verr != nil {
					t.Fatalf("parse: %v, Validate: %v; want accepted", err, verr)
				}
				return
			}
			if err == nil || err.Error() != tc.want || verr == nil || verr.Error() != tc.want {
				t.Fatalf("parse = %v, Validate = %v\nwant %s", err, verr, tc.want)
			}
		})
	}
}

func TestConcatFragmentParamsValues(t *testing.T) {
	got, err := parseConcatFragment(concatFragRes("x", map[string]any{"target": "/t", "content": "c"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (concatFragmentParams{Name: "x", Content: "c", Target: "/t", Order: "10"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %#v, want %#v", got, want)
	}
	for _, o := range []struct {
		in   any
		want string
	}{{float64(2), "2"}, {2.0, "2"}, {float64(-3), "-3"}, {"2.0", "2.0"}, {"", ""}} {
		got, err := parseConcatFragment(concatFragRes("x", map[string]any{"target": "/t", "content": "c", "order": o.in}))
		if err != nil || got.Order != o.want {
			t.Errorf("order %#v = %q, %v; want %q", o.in, got.Order, err, o.want)
		}
	}
	got, _ = parseConcatFragment(concatFragRes("n", map[string]any{"tag": []any{"a", "b"}, "content": "c"}))
	if !reflect.DeepEqual(got.Tag, []string{"a", "b"}) || got.Target != nil {
		t.Errorf("tag list = %#v, target = %#v", got.Tag, got.Target)
	}
	got, _ = parseConcatFragment(concatFragRes("n", map[string]any{"tag": false, "target": "/t", "content": "c"}))
	if got.Tag != nil {
		t.Errorf("tag false must read as unset, got %#v", got.Tag)
	}
}

func TestConcatFragmentUnimplemented(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"source", map[string]any{"target": "/t", "source": "puppet:///x"}, "source"},
		{"source list", map[string]any{"target": "/t", "source": []any{"a"}}, "source"},
		{"order at 2^53", map[string]any{"target": "/t", "content": "c", "order": float64(1 << 53)}, "order"},
		{"order below -2^53", map[string]any{"target": "/t", "content": "c", "order": -float64(1 << 53)}, "order"},
		{"tag number", map[string]any{"target": "/t", "content": "c", "tag": float64(1)}, "tag"},
		{"rich", map[string]any{"target": "/t", "content": map[string]any{"__ptype": "Sensitive", "__pvalue": "x"}}, "content"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConcatFragment(concatFragRes("x", tc.params))
			var uv *rubyvalue.UnimplementedValue
			if !errors.As(err, &uv) || uv.Attr != tc.attr {
				t.Fatalf("error = %v, want *rubyvalue.UnimplementedValue for %s", err, tc.attr)
			}
		})
	}
	// A refusal that comes first wins over a later unimplemented shape.
	_, err := parseConcatFragment(concatFragRes("x", map[string]any{"target": true, "source": "s"}))
	var uv *rubyvalue.UnimplementedValue
	if err == nil || errors.As(err, &uv) {
		t.Errorf("target refusal should come before source: %v", err)
	}
}

func TestConcatSensitiveMarks(t *testing.T) {
	file := concatFileRes("/etc/a.conf", map[string]any{"ensure": "present", "owner": "o", "group": "g", "mode": "0600"},
		"ensure", "owner", "group", "mode")
	marked, logs := MarkSensitive(ConcatFile{}, file)
	if !reflect.DeepEqual(marked, NewAttrs("ensure", "owner", "group", "mode")) || len(logs) != 0 {
		t.Errorf("Concat_file marks = %v, logs = %v", marked, logs)
	}

	file = concatFileRes("/etc/a.conf", map[string]any{"path": "/etc/a.conf", "show_diff": true}, "path", "show_diff")
	marked, logs = MarkSensitive(ConcatFile{}, file)
	if len(marked) != 0 || len(logs) != 2 {
		t.Fatalf("parameters must warn: marks = %v, logs = %v", marked, logs)
	}
	if logs[0].Message != "Unable to mark 'path' as sensitive: path is a parameter and not a property, and cannot be automatically redacted." ||
		logs[0].Level != "warning" || logs[0].Source != "/Concat_file[/etc/a.conf]" {
		t.Errorf("path warning = %#v", logs[0])
	}

	frag := concatFragRes("f", map[string]any{"target": "/t", "content": "secret", "order": "1"}, "content")
	marked, logs = MarkSensitive(ConcatFragment{}, frag)
	if !reflect.DeepEqual(marked, NewAttrs("content")) || len(logs) != 0 {
		t.Errorf("Concat_fragment content marks = %v, logs = %v", marked, logs)
	}

	frag = concatFragRes("f", map[string]any{"target": "/t", "content": "c", "order": "1", "tag": "t"}, "target", "order", "tag")
	marked, logs = MarkSensitive(ConcatFragment{}, frag)
	if len(marked) != 0 || len(logs) != 3 {
		t.Errorf("fragment parameters must warn: marks = %v, logs = %v", marked, logs)
	}
}

func TestConcatAttrs(t *testing.T) {
	wantFile := NewAttrs("path", "ensure", "tag", "owner", "group", "mode", "order", "backup", "replace", "validate_cmd",
		"ensure_newline", "format", "force", "selinux_ignore_defaults", "selrange", "selrole", "seltype", "seluser",
		"show_diff", "create_empty_file")
	if got := (ConcatFile{}).Attrs(); !reflect.DeepEqual(got, wantFile) {
		t.Errorf("Concat_file Attrs = %v", got)
	}
	if got, want := (ConcatFile{}).Properties(), NewAttrs("ensure", "owner", "group", "mode"); !reflect.DeepEqual(got, want) {
		t.Errorf("Concat_file Properties = %v", got)
	}
	if got, want := (ConcatFragment{}).Attrs(), NewAttrs("name", "target", "content", "source", "order", "tag"); !reflect.DeepEqual(got, want) {
		t.Errorf("Concat_fragment Attrs = %v", got)
	}
	if got := (ConcatFragment{}).Properties(); len(got) != 0 {
		t.Errorf("Concat_fragment Properties = %v, want none", got)
	}
	if (ConcatFile{}).Type() != "concat_file" || (ConcatFragment{}).Type() != "concat_fragment" {
		t.Error("Type")
	}
	if (ConcatFile{}).ProviderUsed() != nil || (ConcatFragment{}).ProviderUsed() != nil {
		t.Error("a type with no provider reports provider_used null")
	}
}

// TestConcatValidateNeverPanics gives every attribute an array and a hash:
// Validate answers (accept, refuse or delegate) and never panics.
func TestConcatValidateNeverPanics(t *testing.T) {
	shapes := map[string]any{"array": []any{true}, "nested": []any{[]any{"a"}, map[string]any{"a": "b"}}, "hash": map[string]any{"a": float64(1)}}
	for typ, p := range map[string]interface {
		Validate(catalog.Resource) error
		Attrs() Attrs
	}{"Concat_file": ConcatFile{}, "Concat_fragment": ConcatFragment{}} {
		for attr := range p.Attrs() {
			for shape, v := range shapes {
				t.Run(typ+"/"+attr+"/"+shape, func(t *testing.T) {
					r := catalog.Resource{Type: typ, Title: "/etc/a", Parameters: map[string]any{attr: v}}
					_ = p.Validate(r)
				})
			}
		}
	}
}

// TestConcatDelegationRedactsMarkedValues: the text of a delegation, which
// reaches the operator, holds no part of a marked attribute's value.
func TestConcatDelegationRedactsMarkedValues(t *testing.T) {
	secret := map[string]any{"secretkey": "secretval"}
	for attr := range (ConcatFile{}).Attrs() {
		t.Run(attr, func(t *testing.T) {
			r := concatFileRes("/etc/a", map[string]any{attr: secret}, attr)
			_, err := parseConcatFile(r)
			var uv *rubyvalue.UnimplementedValue
			if !errors.As(err, &uv) {
				return // a refusal that shows no value
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("delegation text leaks the marked value: %v", err)
			}
		})
	}
	r := concatFileRes("/etc/a", map[string]any{"tag": []any{"secretval", float64(1)}}, "tag")
	if _, err := parseConcatFile(r); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("marked tag list: %v", err)
	}
	r = concatFileRes("title", map[string]any{"path": "/etc/secretdir/"}, "path")
	if _, err := parseConcatFile(r); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("marked unclean path: %v", err)
	}
}
