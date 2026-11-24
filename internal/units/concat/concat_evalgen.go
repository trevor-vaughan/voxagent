package concat

import (
	"context"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/ral"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
)

// concatBelongs is §5.1's test of whether fragment f belongs to the
// Concat_file titled title with parameters file: concat_file.rb's fragments
// (CF9:191-201, CF10:201-208), which concat_fragment.rb's autorequire mirrors
// from the fragment's side. A false tag was read as nil (concatTag), so it
// never matches.
func concatBelongs(v ConcatVersion, file concatFileParams, title string, f concatFragmentParams) bool {
	if f.Target != nil && (f.Target == file.Path || f.Target == title) {
		return true
	}
	switch ft := f.Tag.(type) {
	case string:
		if file.Tag == ft {
			return true
		}
		// 10.0.1 only: a list tag on the Concat_file includes the fragment's.
		list, ok := file.Tag.([]string)
		return v == Concat1001 && ok && slices.Contains(list, ft)
	case []string:
		list, ok := file.Tag.([]string)
		return ok && slices.Equal(list, ft)
	}
	return false
}

// EvalGenerates implements EvalGenerator: every Concat_file fills its File.
func (ConcatFile) EvalGenerates(catalog.Resource) bool { return true }

// EvalGenerate implements EvalGenerator: concat_file.rb's eval_generate
// (CF9:352-360, CF10:365-374; spec §5). It joins the belonging fragments'
// content in order and sets it, with ensure absent for an empty result when
// create_empty_file is false, on the File at the path, which it returns as an
// existing member of the Concat_file's unit.
//
// A raise in Ruby is a failed generation here, logged at the Concat_file's
// path (from the context's Sources), file and line: the incomparable order
// keys (M3c), or no File at the path (M20b), which load-time generation never
// leaves natively.
func (c ConcatFile) EvalGenerate(ctx context.Context, r catalog.Resource) (ral.EvalGeneration, error) {
	p, err := parseConcatFile(r)
	if err != nil {
		return ral.EvalGeneration{}, err
	}
	failed := func(detail string) ral.EvalGeneration {
		return ral.EvalGeneration{Failed: true, Logs: []report.LogEntry{{
			Level: "err", Source: sources.SourcesFrom(ctx).Resource,
			Message: "Failed to generate additional resources using 'eval_generate': " + detail,
			File:    r.File, Line: r.Line,
		}}}
	}

	var frags []concatFragmentParams
	marked := false
	for _, fr := range c.Catalog.Resources {
		if !strings.EqualFold(fr.Type, "concat_fragment") {
			continue
		}
		f, err := parseConcatFragment(fr)
		if err != nil {
			return ral.EvalGeneration{}, err
		}
		if concatBelongs(c.Version, p, r.Title, f) {
			frags = append(frags, f)
			marked = marked || slices.Contains(fr.SensitiveParameters, "content")
		}
	}
	sorted, err := concatSort(frags, p.Order)
	if err != nil {
		return failed(err.Error()), nil
	}
	content := concatContent(sorted, p.EnsureNewline)

	ref, found := concatFileRef(c.Catalog, p.Path)
	if !found {
		return failed("undefined method `[]=' for nil:NilClass"), nil
	}
	params := map[string]any{"content": content}
	if !p.CreateEmptyFile && content == "" {
		params["ensure"] = "absent"
	}
	// 10.0.1 sets the parameter's sensitive flag to @has_sensitive_content_fragments
	// (CF10:221-226, :369): nil until a fragment is read, false with none.
	mark := ral.ContentMarkKeep
	if c.Version == Concat1001 {
		switch {
		case marked:
			mark = ral.ContentMarkSensitive
		case len(frags) == 0:
			mark = ral.ContentMarkNotSensitive
		default:
			mark = ral.ContentMarkUnset
		}
	}
	return ral.EvalGeneration{
		Existing: []string{ref},
		// D-78 under both versions: a validate_cmd can echo a marked
		// fragment, which 9.1.0 leaves unmarked in the content.
		Overrides: []ral.Override{{Ref: ref, Parameters: params, ContentMark: mark, RedactOutput: marked}},
	}, nil
}

// AutoRequires implements Provider: autorequire(:file) { [self[:path]] }.
// Validation refused an unparseable Concat_file before the graph is built.
func (ConcatFile) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseConcatFile(r)
	if err != nil {
		return nil
	}
	if ref := "File[" + p.Path + "]"; managed(ref) {
		return []string{ref}
	}
	return nil
}

// AutoRequires implements Provider: concat_fragment's autorequire(:file)
// block only warns (GraphWarnings) and returns nil.
func (ConcatFragment) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// GraphWarnings implements AutoRelationWarning: concat_fragment's orphan
// warning (FR9:73-85, FR10:73-85; spec §6), when no Concat_file in cat
// collects r. Validation refused an unparseable resource before the graph is
// built.
func (f ConcatFragment) GraphWarnings(r catalog.Resource, cat *catalog.Catalog) []string {
	p, err := parseConcatFragment(r)
	if err != nil {
		return nil
	}
	for _, cr := range cat.Resources {
		if !strings.EqualFold(cr.Type, "concat_file") {
			continue
		}
		if file, err := parseConcatFile(cr); err == nil && concatBelongs(f.Version, file, cr.Title, p) {
			return nil
		}
	}
	// Ruby's string interpolation: nil is empty, a String verbatim, an Array
	// its inspect.
	interpolate := func(name string, v any) string {
		marked := slices.Contains(r.SensitiveParameters, name)
		switch t := v.(type) {
		case nil:
			return ""
		case string:
			return rubyvalue.RedactIf(marked, "%s", t)
		}
		return rubyvalue.RedactIf(marked, "%s", rubyvalue.InspectValue(v))
	}
	tag := ""
	if p.Tag != nil {
		// no closing quote, as Ruby's message has none
		tag = "or tag '" + interpolate("tag", r.Parameters["tag"]) + " "
	}
	return []string{"Target Concat_file with path or title '" + interpolate("target", p.Target) + "' " + tag + "not found in the catalog"}
}
