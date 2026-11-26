package tidy

import (
	"context"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// Tidy is the native provider for the tidy type. Tidy manages nothing itself:
// its work is the File resources its generator adds (a later file), so Apply
// has no state to bring about.
type Tidy struct{}

// Type implements Provider.
func (Tidy) Type() string { return "tidy" }

// ProviderUsed implements Provider: the type has no providers.
func (Tidy) ProviderUsed() *string { return nil }

// AutoRequires implements Provider. type/tidy.rb declares no autorelation.
func (Tidy) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs implements Provider: every parameter type/tidy.rb declares (TY:24-221).
func (Tidy) Attrs() ral.Attrs {
	return ral.NewAttrs("path", "recurse", "max_files", "matches", "backup", "age", "size", "type", "rmdirs")
}

// Properties implements PropertyDeclaring: the type has none.
func (Tidy) Properties() ral.Attrs { return ral.NewAttrs() }

// Validate implements Validating: Ruby's refusal of the parameters (TY:24-221).
func (Tidy) Validate(r catalog.Resource) error {
	_, err := parseParams(r)
	return err
}

// Apply implements Provider; see Tidy.
func (Tidy) Apply(context.Context, catalog.Resource, bool) ([]ral.Change, []ral.Warning, error) {
	return nil, nil, nil
}

// Prefetch implements Provider.
func (t Tidy) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) { return t, nil }

// Refresh implements Provider.
func (Tidy) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }
