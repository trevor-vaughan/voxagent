package ral

import (
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func TestFileValidateRequiresAnAbsolutePath(t *testing.T) {
	cases := []struct {
		title string
		want  string // "" means valid
	}{
		{"/tmp/a", ""},
		{"/", ""},
		{"rel/f1", "Parameter path failed on File[rel/f1]: File paths must be fully qualified, not 'rel/f1'"},
		{"./x", "Parameter path failed on File[./x]: File paths must be fully qualified, not './x'"},
		{"", "Parameter path failed on File[]: File paths must be fully qualified, not ''"},
	}
	for _, tc := range cases {
		err := File{}.Validate(catalog.Resource{Type: "File", Title: tc.title})
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.title, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%q: error = %v, want %q", tc.title, err, tc.want)
		}
	}
}

func TestValidateCollectsEveryInvalidResourceSorted(t *testing.T) {
	reg := NewRegistry(File{}, Notify{})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "zz/rel"},
		{Type: "File", Title: "/ok"},
		{Type: "File", Title: "aa/rel"},
		// Only File validates paths: a notify titled like a path is fine.
		{Type: "Notify", Title: "rel/x"},
		// Containers are never validated.
		{Type: "Class", Title: "rel/y"},
	}}
	got := Validate(reg, c)
	if len(got) != 2 {
		t.Fatalf("Validate = %v, want 2 invalid resources", got)
	}
	if got[0].Ref != "File[aa/rel]" || got[1].Ref != "File[zz/rel]" {
		t.Errorf("order = %s, %s; want File[aa/rel], File[zz/rel]", got[0].Ref, got[1].Ref)
	}
	if got[0].String() != got[0].Err.Error() {
		t.Errorf("String() = %q, want the error text %q", got[0].String(), got[0].Err.Error())
	}
}

func TestValidateAcceptsAValidCatalog(t *testing.T) {
	reg := NewRegistry(File{}, Notify{})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/ok"},
		{Type: "Notify", Title: "hello"},
	}}
	if got := Validate(reg, c); len(got) != 0 {
		t.Errorf("Validate = %v, want none", got)
	}
}
