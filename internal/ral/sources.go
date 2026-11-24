package ral

import "context"

// Sources names the resource a provider call is working on, in the forms
// Ruby's log lines use for it. The transaction puts them on the context before
// it calls Apply, Refresh or an Evaluator, so a provider never rebuilds them.
// A call that has none carries the zero Sources: Prefetch is type-wide, with no
// resource of its own to name, so it runs with the zero Sources.
type Sources struct {
	// Resource is the containment path, e.g. "/Stage[main]/Main/Exec[x]". A
	// parameter or property path appends "/<name>" to it.
	Resource string
	// Provider is the provider form, e.g. "Exec[x](provider=posix)". It is Ref
	// for a type that has no provider (ProviderUsed returns nil).
	Provider string
	// Ref is the resource reference, e.g. "Exec[x]".
	Ref string
}

type sourcesKey struct{}

// WithSources returns ctx carrying s.
func WithSources(ctx context.Context, s Sources) context.Context {
	return context.WithValue(ctx, sourcesKey{}, s)
}

// SourcesFrom returns the Sources on ctx, or the zero Sources.
func SourcesFrom(ctx context.Context) Sources {
	s, _ := ctx.Value(sourcesKey{}).(Sources)
	return s
}
