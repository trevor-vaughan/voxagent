package ral

import (
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/report"
)

func TestMarkSensitive(t *testing.T) {
	const (
		cwdWarn = "Unable to mark 'cwd' as sensitive: cwd is a parameter and not a property, and cannot be automatically redacted."
		tagWarn = "Unable to mark 'tag' as sensitive: tag is a parameter and not a property, and cannot be automatically redacted."
		unset   = "Unable to mark 'enable' as sensitive: the property itself was not assigned a value."
		undef   = "Unable to mark 'nosuch' as sensitive: the property itself is not defined on Package."
	)
	cases := []struct {
		name     string
		p        Provider
		typ      string
		params   map[string]any
		marked   []string
		want     Attrs
		wantLogs []report.LogEntry
	}{
		{"file content also marks ensure", File{}, "File", map[string]any{"content": "x"}, []string{"content"},
			NewAttrs("content", "ensure"), nil},
		{"file content not given", File{}, "File", map[string]any{"ensure": "file"}, []string{"content"},
			NewAttrs("ensure"), []report.LogEntry{{Level: "warning", Message: "Unable to mark 'content' as sensitive: the property itself was not assigned a value."}}},
		{"file ensure listed after content, ensure not given", File{}, "File", map[string]any{"content": "x"}, []string{"content", "ensure"},
			NewAttrs("content", "ensure"), nil},
		{"file source and content", File{}, "File", map[string]any{"source": "/s", "content": "x"}, []string{"source", "content"},
			NewAttrs("content", "ensure"), nil},
		{"file mode only", File{}, "File", map[string]any{"content": "x", "mode": "0600"}, []string{"mode"},
			NewAttrs("mode"), nil},
		{"file source marks content and ensure", File{}, "File", map[string]any{"source": "/s"}, []string{"source"},
			NewAttrs("content", "ensure"), nil},
		{"file target on a link", File{}, "File", map[string]any{"ensure": "link", "target": "/t"}, []string{"target"},
			NewAttrs("target"), nil},
		{"exec unless marks the given guards", Exec{}, "Exec", map[string]any{"command": "c", "unless": "u"}, []string{"unless"},
			NewAttrs("command", "unless"), nil},
		{"exec two guards marked, no stray warning", Exec{}, "Exec", map[string]any{"command": "c", "unless": "u", "onlyif": "o"}, []string{"command", "unless"},
			NewAttrs("command", "unless", "onlyif"), nil},
		{"exec cwd is a parameter", Exec{}, "Exec", map[string]any{"command": "c", "cwd": "/"}, []string{"cwd"},
			NewAttrs(), []report.LogEntry{{Level: "warning", Message: cwdWarn}}},
		{"notify message", Notify{}, "Notify", map[string]any{"message": "m"}, []string{"message"},
			NewAttrs("message"), nil},
		{"service enable unset", Service{}, "Service", map[string]any{"ensure": "running"}, []string{"enable"},
			NewAttrs(), []report.LogEntry{{Level: "warning", Message: unset}}},
		{"package unknown name", Package{}, "Package", map[string]any{"ensure": "present"}, []string{"nosuch"},
			NewAttrs(), []report.LogEntry{{Level: "err", Message: undef}}},
		{"engine metaparameter is a parameter", Notify{}, "Notify", map[string]any{"message": "m", "tag": "t"}, []string{"tag"},
			NewAttrs(), []report.LogEntry{{Level: "warning", Message: tagWarn}}},
		{"duplicate name", File{}, "File", map[string]any{"content": "x"}, []string{"content", "content"},
			NewAttrs("content", "ensure"), nil},
		{"extension provider", Extension{typ: "x"}, "X", map[string]any{"a": "b"}, []string{"a"}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := catalog.Resource{Type: tc.typ, Title: "/tmp/x", Parameters: tc.params, SensitiveParameters: tc.marked}
			got, logs := MarkSensitive(tc.p, r)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("set = %v, want %v", got, tc.want)
			}
			for i := range tc.wantLogs {
				tc.wantLogs[i].Source = "/" + r.Ref()
			}
			if !reflect.DeepEqual(logs, tc.wantLogs) {
				t.Errorf("logs = %+v, want %+v", logs, tc.wantLogs)
			}
		})
	}
}

// A directly set content flag marks content alone: the File cascade belongs
// to the catalog's sensitive_parameters (design §5.4).
func TestMarkSensitiveHonoursSetSensitivityWithoutCascade(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]bool
		want []string
	}{
		{"set true marks content only", map[string]bool{"content": true}, []string{"content"}},
		{"set false marks nothing", map[string]bool{"content": false}, nil},
		{"no record marks nothing", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fileRes("/f", map[string]any{"ensure": "file", "content": "x"})
			r.SetSensitivity = tc.set
			marked, logs := MarkSensitive(File{}, r)
			if len(logs) != 0 || len(marked) != len(tc.want) {
				t.Fatalf("marked %v logs %v, want %v", marked, logs, tc.want)
			}
			for _, n := range tc.want {
				if _, ok := marked[n]; !ok {
					t.Errorf("%s not marked: %v", n, marked)
				}
			}
		})
	}
}
