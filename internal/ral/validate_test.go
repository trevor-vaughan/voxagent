package ral

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// fileValidator stands in for file's path check, the one Validating provider
// the engine tests need: its message is type/file.rb's verbatim.
type fileValidator struct{ stub }

func (fileValidator) Validate(r catalog.Resource) error {
	if !strings.HasPrefix(r.Title, "/") {
		return fmt.Errorf("Parameter path failed on %s: File paths must be fully qualified, not '%s'", r.Ref(), r.Title)
	}
	return nil
}

var fileValidating = fileValidator{stub{typ: "file", attrs: NewAttrs("path")}}

func TestValidateCollectsEveryInvalidResourceSorted(t *testing.T) {
	reg := NewRegistry(fileValidating, stub{typ: "notify", attrs: NewAttrs("message")})
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
	reg := NewRegistry(fileValidating, stub{typ: "notify", attrs: NewAttrs("message")})
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "File", Title: "/ok"},
		{Type: "Notify", Title: "hello"},
	}}
	if got := Validate(reg, c); len(got) != 0 {
		t.Errorf("Validate = %v, want none", got)
	}
}

// A static catalog inlines file metadata the server computed, and Puppet's
// source reads it in place of the file (type/file/source.rb:186). voxagent
// reads every source itself, so a catalog carrying either key is refused
// before anything runs.
func TestValidateRefusesInlinedFileMetadata(t *testing.T) {
	reg := NewRegistry(fileValidating)
	c := &catalog.Catalog{
		Resources:         []catalog.Resource{{Type: "File", Title: "/ok"}},
		Metadata:          json.RawMessage(`{"/ok":{"checksum":{"type":"sha256","value":"x"}}}`),
		RecursiveMetadata: json.RawMessage(`{}`),
	}
	var got []string
	for _, i := range Validate(reg, c) {
		got = append(got, i.String())
	}
	want := []string{
		"the catalog carries metadata (a static catalog's inlined file metadata), which is not implemented",
		"the catalog carries recursive_metadata (a static catalog's inlined file metadata), which is not implemented",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Validate = %q, want %q", got, want)
	}
	c.Metadata, c.RecursiveMetadata = nil, json.RawMessage("null")
	if got := Validate(reg, c); len(got) != 0 {
		t.Errorf("Validate = %v for absent and null metadata, want none", got)
	}
}
