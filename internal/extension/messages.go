package extension

import (
	"time"

	"github.com/voxagent/voxagent/internal/report"
)

// Protocol is the protocol version this voxagent speaks. A host announcing
// another is refused at the handshake.
const Protocol = 1

// Timeouts bound each call. Evaluate is separate because it includes the
// lazy prefetch of the resource's provider class, which can read a whole
// package database.
type Timeouts struct {
	Evaluate time.Duration
	Other    time.Duration
}

// Hello is the host's first frame.
type Hello struct {
	Protocol      int    `json:"protocol"`
	PuppetVersion string `json:"puppet_version"`
	RubyVersion   string `json:"ruby_version"`
}

// Log is one Puppet log line emitted while the host served a call. File and
// Line are set when Puppet sets them: on lines a resource logs.
type Log struct {
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// TypeInfo is the host's classification of one catalog type.
type TypeInfo struct {
	Class         string `json:"class"`
	Origin        string `json:"origin,omitempty"`
	Refreshable   bool   `json:"refreshable"`
	EvalGenerates bool   `json:"eval_generates"`
	// SelfRefresh is a Tier 3 type that refreshes itself after its own events
	// (type.rb:2186-2189, event_manager.rb:79).
	SelfRefresh bool `json:"self_refresh"`
}

// Refusal names a resource and why it cannot be applied, in Puppet's words.
type Refusal struct {
	Ref     string `json:"ref"`
	Message string `json:"message"`
}

// ResourceInfo is what load reports per resource.
type ResourceInfo struct {
	ProviderUsed *string `json:"provider_used"`
	// Aliases are the other refs the catalog knows the resource by: a title
	// other than its name makes the name an alias (catalog.rb:184-219).
	Aliases []string `json:"aliases,omitempty"`
	// PuppetRef and PuppetTitle are Puppet's own ref and title for a resource
	// whose type overrides title (kernel_parameter.rb:42-48); empty when they
	// equal the catalog's.
	PuppetRef   string `json:"puppet_ref,omitempty"`
	PuppetTitle string `json:"puppet_title,omitempty"`
	// Unresolvable: Puppet's catalog cannot look the resource up by its
	// catalog ref (resource/catalog.rb:378-395).
	Unresolvable bool `json:"unresolvable,omitempty"`
}

// LoadResult is the host's answer to load.
type LoadResult struct {
	Types      map[string]TypeInfo     `json:"types"`
	Metaparams []string                `json:"metaparams"`
	Invalid    []Refusal               `json:"invalid"`
	Generated  Generation              `json:"generated"`
	Resources  map[string]ResourceInfo `json:"resources"`
	// Order is every resource of the host's catalog, catalog and generated
	// refs alike, in Puppet's catalog order after the generation pass
	// (resource/catalog.rb:99-124, additional_resource_generator.rb:36-50).
	Order []string `json:"order"`
	Logs  []Log    `json:"logs"`
	// GenerateLogs are the lines Puppet logs while running generate; they belong
	// in the report, as they happen inside Puppet's transaction.
	GenerateLogs     []Log `json:"generate_logs"`
	FailedToGenerate bool  `json:"failed_to_generate"`
}

// Override is a generator's change to a resource that already existed.
type Override struct {
	Ref        string         `json:"ref"`
	Parameters map[string]any `json:"parameters"`
	Sensitive  []string       `json:"sensitive"`
}

// BeforeEdge is a `before` value a generator appended: Source before Target.
type BeforeEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// GeneratedResource is a resource in catalog JSON shape.
type GeneratedResource struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Tags       []string       `json:"tags"`
	Exported   bool           `json:"exported"`
	Kind       string         `json:"kind"`
	Parameters map[string]any `json:"parameters"`
}

// Generation is what one generator call did to the host's catalog.
type Generation struct {
	Resources  []GeneratedResource `json:"resources"`
	Containers map[string]string   `json:"containers"`
	// Generators names, for a new resource, the resource whose generate or
	// eval_generate made it: add_resource's parent_resource
	// (additional_resource_generator.rb:137).
	Generators map[string]string `json:"generators"`
	Before     []BeforeEdge      `json:"before"`
	Overrides  []Override        `json:"overrides"`
}

// EvalGeneration is the host's answer to eval_generate. Existing are the
// resources the generator returned that the catalog already held.
type EvalGeneration struct {
	Generation
	Existing []string `json:"existing"`
	Failed   bool     `json:"failed"`
	Logs     []Log    `json:"logs"`
}

// Relations are one resource's auto-relations, as refs of the OTHER end.
type Relations struct {
	Require   []string `json:"require"`
	Before    []string `json:"before"`
	Subscribe []string `json:"subscribe"`
	Notify    []string `json:"notify"`
}

// Evaluation is the host's answer to evaluate and skip: the resource
// status's events as Event#to_data_hash writes them, its flags, and the log
// lines the harness emitted.
type Evaluation struct {
	Events  []report.Event `json:"events"`
	Failed  bool           `json:"failed"`
	Skipped bool           `json:"skipped"`
	// Unsuitable is a resource Puppet does not evaluate because it has no
	// suitable provider (relationship_graph.rb:116-137): failed, not skipped,
	// with no events. Puppet logs nothing for it until the walk cannot place
	// it (Host.OverlyDeferred).
	Unsuitable          bool  `json:"unsuitable"`
	InvalidateRefreshes bool  `json:"invalidate_refreshes"`
	Logs                []Log `json:"logs"`
}

// RefreshResult is the host's answer to refresh. Error is the callback's
// exception message; the walk words Puppet's "Failed to call refresh" line.
// Logs are the lines the callback logged; ExceptionLogs are the lines
// resource.log_exception logged for its exception, which Puppet writes after
// the "Failed to call" line (event_manager.rb:160-165), so they travel apart.
type RefreshResult struct {
	Refreshed     bool    `json:"refreshed"`
	Error         *string `json:"error"`
	Logs          []Log   `json:"logs"`
	ExceptionLogs []Log   `json:"exception_logs"`
}

// Stats is the host's contribution to the run's self-report.
type Stats struct {
	Startup   time.Duration
	Spent     time.Duration
	Evaluated int
}

// CallError is a request the host could not serve, because of an exception
// outside Puppet's own rescue. The host stays usable.
type CallError struct {
	Op      string
	Message string
}

func (e *CallError) Error() string {
	return "the extension host could not serve " + e.Op + ": " + e.Message
}
