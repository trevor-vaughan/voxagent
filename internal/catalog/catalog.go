// Package catalog decodes Puppet catalogs and compares them structurally.
//
// It never shells out and never invokes Puppet, so it is testable from fixtures
// on a machine with no Ruby installed. Invoking a compiler is internal/compile's
// responsibility.
package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
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

	// Metadata and RecursiveMetadata are a static catalog's inlined file
	// metadata (resource/catalog.rb:455-466), present only when the server
	// computed some (:488-490). Kept raw: voxagent does not read them, and
	// ral.Validate refuses a catalog carrying either.
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	RecursiveMetadata json.RawMessage `json:"recursive_metadata,omitempty"`
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
	// SensitiveParameters names the parameters the compiler marked Sensitive,
	// as the catalog gives them. ral.MarkSensitive judges the names, because
	// only there is the resource's type known.
	SensitiveParameters []string `json:"sensitive_parameters,omitempty"`
	// GeneratedFrom is the resource that generated this one, and nil for
	// every resource the catalog declares: a native provider's children at
	// apply time (ral.Generator), the extension host's eval_generate output at
	// the parent's turn (ral.EvalGenerator), and the host's load-time generate
	// output, which mergeGeneration marks from the host's generators. Never
	// decoded: a catalog cannot claim to be generated.
	GeneratedFrom *Resource `json:"-"`
}

// MarkSensitive adds names to the resource's SensitiveParameters, each once.
// It never writes the slice the resource held: catalogs share backing arrays
// between copies of a resource.
func (r *Resource) MarkSensitive(names []string) {
	for _, n := range names {
		if !slices.Contains(r.SensitiveParameters, n) {
			r.SensitiveParameters = append(slices.Clip(r.SensitiveParameters), n)
		}
	}
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
//
// Every resource's rich values are resolved (ResolveRich): an ASCII-only
// Binary becomes its string, and invalid rich data is a *RichDataError.
func Decode(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode catalog: %w", err)
	}
	if c.CatalogFormat == 0 {
		return nil, fmt.Errorf("decode catalog: no catalog_format field; input is not a Puppet catalog")
	}
	// A *RichDataError is Puppet's own message, so it is not prefixed.
	if err := resolveRichData(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// NormaliseNumbers returns v with every json.Number in it, at any depth of
// lists and maps, replaced by the float64 Decode produces for the same JSON,
// which is what providers read. Values arriving through a decoder that uses
// UseNumber (the extension host's) carry json.Number instead.
//
// A number no float64 can hold is an error: substituting anything else would
// hand a provider a value the catalog did not declare.
func NormaliseNumbers(v any) (any, error) {
	switch v := v.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("the number %s cannot be represented: %w", v, err)
		}
		return f, nil
	case []any:
		if v == nil {
			return v, nil
		}
		out := make([]any, len(v))
		for i, e := range v {
			n, err := NormaliseNumbers(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		if v == nil {
			return v, nil
		}
		out := make(map[string]any, len(v))
		for k, e := range v {
			n, err := NormaliseNumbers(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	default:
		return v, nil
	}
}
