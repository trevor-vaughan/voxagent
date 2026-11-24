// Package catalog decodes Puppet catalogs and compares them structurally.
//
// It never shells out and never invokes Puppet, so it is testable from fixtures
// on a machine with no Ruby installed. Invoking a compiler is internal/compile's
// responsibility.
package catalog

import (
	"encoding/json"
	"fmt"
)

// Catalog mirrors the JSON emitted by a Puppet compiler. The field set was
// verified against Puppet 8.10.0 output at catalog_format 2.
type Catalog struct {
	Tags          []string   `json:"tags"`
	Name          string     `json:"name"`
	Version       any        `json:"version"`
	CodeID        *string    `json:"code_id"`
	CatalogUUID   string     `json:"catalog_uuid"`
	CatalogFormat int        `json:"catalog_format"`
	Environment   string     `json:"environment"`
	Resources     []Resource `json:"resources"`
	Edges         []Edge     `json:"edges"`
	Classes       []string   `json:"classes"`
}

// Resource is one catalog resource. File and Line are absent for resources not
// declared in a manifest, such as Stage[main].
type Resource struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Tags       []string       `json:"tags"`
	File       string         `json:"file,omitempty"`
	Line       int            `json:"line,omitempty"`
	Exported   bool           `json:"exported"`
	Kind       string         `json:"kind,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// Ref returns the canonical Type[Title] reference, the same form Puppet uses in
// catalog edges.
func (r Resource) Ref() string {
	return fmt.Sprintf("%s[%s]", r.Type, r.Title)
}

// Edge is a dependency edge between two resource references.
type Edge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Decode parses catalog JSON.
//
// Puppet's CLI prints a "Notice: Compiled catalog for ..." line to stdout ahead
// of the JSON body. Decode does not tolerate that prefix; use compile.Runner,
// which strips it.
func Decode(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if c.CatalogFormat == 0 {
		return nil, fmt.Errorf("decode catalog: no catalog_format field; input is not a Puppet catalog")
	}
	return &c, nil
}
