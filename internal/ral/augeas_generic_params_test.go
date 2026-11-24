package ral

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// simpResolv is SIMP's Augeas resolv.conf resource (.test-output/ranking/simp.json).
const simpResolv = "rm #comment\nrm nameserver[.!=\"10.0.0.53\"]\nset nameserver[1] 10.0.0.53\nrm search/domain[.!=\"example.com\"]\nset search/domain[.=\"example.com\"] example.com\nrm options/debug\nrm options/inet6\nrm options/no-check-names\ndefnode opts options \"\"\nset $opts/attempts 2\nset $opts/ndots 1\nset $opts/rotate \"\"\nset $opts/timeout 2\nrm options[count(*)=0]\n"

func cmd(name string, args ...string) augeasCommand { return augeasCommand{name, args} }

func TestParseAugeasChanges(t *testing.T) {
	const x = "/files/etc/x"
	const resolv = "/files/etc/resolv.conf"
	cases := []struct {
		name    string
		changes any
		context string
		want    []augeasCommand
		wantErr string
	}{
		{"modprobe", "set blacklist[. = 'cramfs'] cramfs", x,
			[]augeasCommand{cmd("set", "/files/etc/x/blacklist[. = 'cramfs']", "cramfs")}, ""},
		{"simp resolv", simpResolv, resolv, []augeasCommand{
			cmd("rm", resolv+"/#comment"),
			cmd("rm", resolv+`/nameserver[.!="10.0.0.53"]`),
			cmd("set", resolv+"/nameserver[1]", "10.0.0.53"),
			cmd("rm", resolv+`/search/domain[.!="example.com"]`),
			cmd("set", resolv+`/search/domain[.="example.com"]`, "example.com"),
			cmd("rm", resolv+"/options/debug"),
			cmd("rm", resolv+"/options/inet6"),
			cmd("rm", resolv+"/options/no-check-names"),
			cmd("defnode", "opts", resolv+"/options", ""),
			cmd("set", "$opts/attempts", "2"),
			cmd("set", "$opts/ndots", "1"),
			cmd("set", "$opts/rotate", ""),
			cmd("set", "$opts/timeout", "2"),
			cmd("rm", resolv+"/options[count(*)=0]"),
		}, ""},
		{"array", []any{"set a 1", "rm b"}, x, []augeasCommand{cmd("set", x+"/a", "1"), cmd("rm", x+"/b")}, ""},
		{"nested array", []any{"set a 1", []any{[]any{"rm b"}, nil}}, x, []augeasCommand{cmd("set", x+"/a", "1"), cmd("rm", x+"/b")}, ""},
		{"blank and CR lines", "set a 1\r\n\n  \r\nrm b\r\n", x, []augeasCommand{cmd("set", x+"/a", "1"), cmd("rm", x+"/b")}, ""},
		{"array member keeps interior newline", []any{"set a 1\nrm b"}, x, []augeasCommand{cmd("set", x+"/a", "1")}, ""},
		{"abs path chomped", "rm /abs/path/", x, []augeasCommand{cmd("rm", "/abs/path")}, ""},
		{"quoted path", "rm 'quoted path'", x, []augeasCommand{cmd("rm", x+"/quoted path")}, ""},
		{"quoted abs path", `rm "/q"`, x, []augeasCommand{cmd("rm", "/q")}, ""},
		{"context with slash", "rm a", x + "/", []augeasCommand{cmd("rm", x+"/a")}, ""},
		{"rm slash", "rm /", x, []augeasCommand{cmd("rm", x+"/")}, ""},
		{"rm alone", "rm", x, []augeasCommand{cmd("rm", x+"/")}, ""},
		{"unterminated quote value", "set a 'bc", x, []augeasCommand{cmd("set", x+"/a", "bc")}, ""},
		{"escaped delimiter", `set a "x\"y"`, x, []augeasCommand{cmd("set", x+"/a", `x"y`)}, ""},
		{"other backslash kept", `set a "x\\q"`, x, []augeasCommand{cmd("set", x+"/a", `x\\q`)}, ""},
		{"text after closing quote", "set a 'it''s'", x, []augeasCommand{cmd("set", x+"/a", "it")}, ""},
		{"excess tokens", "set a b c", x, []augeasCommand{cmd("set", x+"/a", "b")}, ""},
		{"empty context", "rm a", "", []augeasCommand{cmd("rm", "a")}, ""},
		{"space in brackets", "rm a[b c] d", x, []augeasCommand{cmd("rm", x+"/a[b c]")}, ""},
		{"backslash escape in path", `rm a\ b`, x, []augeasCommand{cmd("rm", x+`/a\ b`)}, ""},
		{"insert", "ins x before /a/b", x, []augeasCommand{cmd("ins", "x", "before", "/a/b")}, ""},
		{"remove stays", "remove a", x, []augeasCommand{cmd("remove", x+"/a")}, ""},
		{"get", "get a == b", x, []augeasCommand{cmd("get", x+"/a", "==", "b")}, ""},
		{"values", "values a [ 'x' ]", x, []augeasCommand{cmd("values", x+"/a", "[ 'x' ]")}, ""},
		{"size", "size == 007", x, []augeasCommand{cmd("size", "==", "7")}, ""},
		{"size no digits", "size > abc", x, []augeasCommand{cmd("size", ">", "0")}, ""},
		{"== command", "== a b", x, []augeasCommand{cmd("==", "a b")}, ""},
		{"empty", "", x, nil, ""},

		{"unknown", "frob x", x, nil, "Unknown command frob"},
		{"comment", "# x", x, nil, "Unknown command "},
		{"case", "SET a b", x, nil, "Unknown command SET"},
		{"stray bracket", "set a] v", x, nil, "unmatched ["},
		{"missing value", "set blacklist", x, nil, "missing string argument 2 for set"},
		{"open bracket swallows", "set a[ v", x, nil, "missing string argument 2 for set"},
		{"bad comparator", "get a<3", x, nil, "invalid comparator for command get"},
		{"lone tick path", "rm '", x, nil, "undefined method `chomp!' for nil:NilClass"},
		{"nil changes", nil, x, nil, "changes nil is not a string or an array"},
		{"number member", []any{"rm a", float64(1)}, x, nil, "changes member 1 is not a string"},
		{"non-ASCII", "set a ü", x, nil, "non-ASCII byte in changes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAugeasChanges(tc.changes, tc.context)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestParseAugeasShape(t *testing.T) {
	const ctx = "/files/etc/x"
	with := func(extra map[string]any) map[string]any {
		p := map[string]any{"changes": "set a b", "context": ctx}
		for k, v := range extra {
			if v == nil {
				delete(p, k)
			} else {
				p[k] = v
			}
		}
		return p
	}
	refused := []struct {
		name   string
		params map[string]any
		attr   string
	}{
		{"onlyif", with(map[string]any{"onlyif": "get x == y"}), "onlyif"},
		{"onlyif array", with(map[string]any{"onlyif": []any{}}), "onlyif"},
		{"force true", with(map[string]any{"force": true}), "force"},
		{"force string false", with(map[string]any{"force": "false"}), "force"},
		{"root", with(map[string]any{"root": "/tmp"}), "root"},
		{"load_path", with(map[string]any{"load_path": "/x"}), "load_path"},
		{"type_check true", with(map[string]any{"type_check": true}), "type_check"},
		{"returns 1", with(map[string]any{"returns": float64(1)}), "returns"},
		{"lens without incl", with(map[string]any{"lens": "Foo.lns"}), "incl"},
		{"incl without lens", with(map[string]any{"incl": "/etc/x"}), "lens"},
		{"context number", with(map[string]any{"context": float64(5)}), "context"},
		{"context newline", with(map[string]any{"context": "/files/a\nb"}), "context"},
		{"show_diff", with(map[string]any{"show_diff": "maybe"}), "show_diff"},
		{"provider", with(map[string]any{"provider": "x"}), "provider"},
		{"changes absent", with(map[string]any{"changes": nil}), "changes"},
		{"changes empty array", with(map[string]any{"changes": []any{}}), "changes"},
		{"changes blank", with(map[string]any{"changes": "  \n"}), "changes"},
		{"changes number", with(map[string]any{"changes": float64(1)}), "changes"},
		{"setm", with(map[string]any{"changes": "setm a b c"}), "changes"},
		{"rename", with(map[string]any{"changes": "rename a b"}), "changes"},
		{"get", with(map[string]any{"changes": "get a == b"}), "changes"},
		{"parse error", with(map[string]any{"changes": "frob"}), "changes"},
		{"non-ASCII arg", with(map[string]any{"changes": "set a ü"}), "changes"},
		{"control byte arg", with(map[string]any{"changes": "set a b\x01"}), "changes"},
		{"non-ASCII incl", with(map[string]any{"incl": "/etc/ü", "lens": "L.lns"}), "incl"},
		{"non-ASCII lens", with(map[string]any{"incl": "/etc/x", "lens": "ü.lns"}), "lens"},
		{"non-ASCII context", with(map[string]any{"context": "/files/ü"}), "context"},
	}
	for _, tc := range refused {
		t.Run("refused "+tc.name, func(t *testing.T) {
			_, err := parseAugeas(raltest.AugRes("Augeas", "t", tc.params))
			var uv *rubyvalue.UnimplementedValue
			if !errors.As(err, &uv) {
				t.Fatalf("error = %v, want *rubyvalue.UnimplementedValue", err)
			}
			if uv.Attr != tc.attr {
				t.Fatalf("attr = %q, want %q", uv.Attr, tc.attr)
			}
		})
	}

	t.Run("sensitive", func(t *testing.T) {
		_, err := parseAugeas(raltest.AugRes("Augeas", "t", with(nil), "changes"))
		var uv *rubyvalue.UnimplementedValue
		if !errors.As(err, &uv) {
			t.Fatalf("error = %v, want *rubyvalue.UnimplementedValue", err)
		}
	})

	accepted := []struct {
		name    string
		params  map[string]any
		context string
		incl    string
	}{
		{"type_check false string", with(map[string]any{"type_check": "false"}), ctx, ""},
		{"type_check false", with(map[string]any{"type_check": false}), ctx, ""},
		{"force false", with(map[string]any{"force": false}), ctx, ""},
		{"returns 0", with(map[string]any{"returns": float64(0)}), ctx, ""},
		{"show_diff no", with(map[string]any{"show_diff": "no"}), ctx, ""},
		{"onlyif empty root slash", with(map[string]any{"onlyif": "", "root": "/", "load_path": ""}), ctx, ""},
		{"provider augeas", with(map[string]any{"provider": "augeas"}), ctx, ""},
		{"empty context with incl", with(map[string]any{"context": "", "incl": "/etc/x", "lens": "X.lns"}), "", "/etc/x"},
		{"incl without context", with(map[string]any{"context": nil, "incl": "/etc/x", "lens": "X.lns"}), "/files/etc/x", "/etc/x"},
		{"trailing slash context kept raw", with(map[string]any{"context": ctx + "/"}), ctx + "/", ""},
		{"no context", with(map[string]any{"context": nil}), "", ""},
	}
	for _, tc := range accepted {
		t.Run("accepted "+tc.name, func(t *testing.T) {
			p, err := parseAugeas(raltest.AugRes("Augeas", "t", tc.params))
			if err != nil {
				t.Fatal(err)
			}
			if p.Context != tc.context || p.Incl != tc.incl || p.Title != "t" || len(p.Commands) != 1 {
				t.Fatalf("got %+v", p)
			}
		})
	}

	t.Run("simp resolv", func(t *testing.T) {
		p, err := parseAugeas(raltest.AugRes("Augeas", "r", map[string]any{"context": "/files/etc/resolv.conf", "changes": simpResolv, "require": "File[/etc/resolv.conf]"}))
		if err != nil || len(p.Commands) != 14 {
			t.Fatalf("got %d commands, err %v", len(p.Commands), err)
		}
	})
	t.Run("simp modprobe", func(t *testing.T) {
		for _, m := range strings.Fields("bluetooth cramfs dccp dccp_ipv4 dccp_ipv6 freevxfs hfs hfsplus ieee1394 jffs2 net-pf-31 rds sctp squashfs tipc udf usb-storage") {
			p, err := parseAugeas(raltest.AugRes("Augeas", m, map[string]any{
				"incl": "/etc/modprobe.d/blacklist.conf", "lens": "Modprobe.lns",
				"changes": []any{"set blacklist[. = '" + m + "'] " + m}, "require": "File[/etc/modprobe.d/blacklist.conf]"}))
			want := cmd("set", "/files/etc/modprobe.d/blacklist.conf/blacklist[. = '"+m+"']", m)
			if err != nil || !reflect.DeepEqual(p.Commands, []augeasCommand{want}) {
				t.Fatalf("%s: %+v, %v", m, p, err)
			}
		}
	})
}

func TestRubyInspectStrings(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"a", "b"}, `["a", "b"]`},
		{[]string{`x"y`}, `["x\"y"]`},
		{[]string{`x\y`}, `["x\\y"]`},
		{[]string{"a#{b"}, `["a\#{b"]`},
		{[]string{"a#$b"}, `["a\#$b"]`},
		{[]string{"a#@b"}, `["a\#@b"]`},
		{[]string{"a#b", "#", ""}, `["a#b", "#", ""]`},
		{nil, `[]`},
	}
	for _, tc := range cases {
		if got := rubyInspectStrings(tc.in); got != tc.want {
			t.Errorf("%q: got %s, want %s", tc.in, got, tc.want)
		}
	}
}
