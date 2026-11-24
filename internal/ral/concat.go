package ral

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// ConcatVersion is the puppetlabs-concat release whose type files the run's
// plugin tree matches (spec §0 row 2). The two differ in tag lists and in
// the generated content's sensitive mark.
type ConcatVersion uint8

const (
	Concat910 ConcatVersion = iota + 1
	Concat1001
)

// ConcatFile is the native provider for puppetlabs-concat's concat_file type.
// Attrs lists every parameter its type file declares, Validate parses them
// (parseConcatFile); generation and apply are in later files.
type ConcatFile struct {
	Version ConcatVersion
	// Catalog is the run's catalog, read when the type generates its content
	// (EvalGenerate).
	Catalog *catalog.Catalog
}

// ConcatFragment is the native provider for concat_fragment, whose only
// consumer is its concat_file's generator. It applies nothing itself.
type ConcatFragment struct {
	Version ConcatVersion
}

// concatFileParams is a Concat_file munged as type/concat_file.rb munges it.
type concatFileParams struct {
	Path   string
	Absent bool   // ensure => absent
	Tag    any    // nil | string | []string; Ruby's false reads as nil
	Order  string // "alpha" | "numeric"

	EnsureNewline, CreateEmptyFile, Replace bool

	// Copied are the attributes the generated File takes (spec §4), those the
	// catalog sets: owner, group, mode, backup, selinux_ignore_defaults,
	// selrange, selrole, seltype, seluser, validate_cmd and show_diff as
	// given, with the Booleans coerced; replace is always present.
	Copied map[string]any
}

// concatFragmentParams is a Concat_fragment munged as type/concat_fragment.rb
// munges it.
type concatFragmentParams struct {
	Name    string
	Content string
	Target  any    // nil | string
	Tag     any    // nil | string | []string
	Order   string // Ruby's to_s of the String or Integer
}

// concatBool reads a Puppet::Parameter::Boolean value. The value collection
// matches case-sensitively, so "TRUE" is refused though Puppet::Coercion.boolean
// would read it (M17o). ok is false for any other value, however shaped.
func concatBool(v any) (b, ok bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch t {
		case "true", "yes":
			return true, true
		case "false", "no":
			return false, true
		}
	}
	return false, false
}

// holdsHash reports whether v is, or nests, a Hash, which rubyInspectValue
// does not render as Ruby does.
func holdsHash(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return true
	case []any:
		for _, e := range t {
			if holdsHash(e) {
				return true
			}
		}
	}
	return false
}

// concatFormats are concat_file's format values; only plain is native.
var concatFormats = []string{"plain", "yaml", "json", "json-array", "json-pretty", "json-array-pretty"}

// windowsAbsolute matches the paths Puppet::Util.absolute_path?(_, :windows)
// takes that do not start with "/": a drive or a UNC path.
var windowsAbsolute = regexp.MustCompile(`^(?:[A-Za-z]:[/\\]|\\\\)`)

// orderForbidden is concat_fragment's order check, unanchored as Ruby's.
var orderForbidden = regexp.MustCompile(`[:\n/]`)

// concatRich refuses, as an unimplemented value, a resource carrying a rich
// value anywhere in its parameters (spec §0 row 1 (f)).
func concatRich(r catalog.Resource) error {
	if rich := catalog.RichValues(r); len(rich) > 0 {
		return &unimplementedValue{r.Ref(), rich[0].Param, rich[0].PType}
	}
	return nil
}

// concatTag reads tag: Ruby accepts any value, native a String or an Array
// of Strings. false is falsy in Ruby, so it reads as unset (M22).
func concatTag(r catalog.Resource) (any, error) {
	switch v := r.Parameters["tag"].(type) {
	case nil:
		return nil, nil
	case bool:
		if !v {
			return nil, nil
		}
	case string:
		return v, nil
	case []any:
		tags := make([]string, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, &unimplementedValue{r.Ref(), "tag", shownValue(r, "tag", v)}
			}
			tags[i] = s
		}
		return tags, nil
	}
	return nil, &unimplementedValue{r.Ref(), "tag", shownValue(r, "tag", r.Parameters["tag"])}
}

// concatPathClean reports whether p, which starts with "/", is in the clean
// form §0 row 1 (e) lets native handle.
func concatPathClean(p string) bool {
	if p == "/" || strings.HasSuffix(p, "/") || strings.Contains(p, "//") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Validate implements Validating. It reports the first failure in §3.1's
// order, which is Ruby's: path, ensure, then the parameters in definition order.
func (ConcatFile) Validate(r catalog.Resource) error {
	_, err := parseConcatFile(r)
	return err
}

func parseConcatFile(r catalog.Resource) (concatFileParams, error) {
	p := concatFileParams{Order: "numeric", CreateEmptyFile: true, Replace: true, Copied: map[string]any{}}
	if err := concatRich(r); err != nil {
		return p, err
	}
	shown := func(name string) string { return shownValue(r, name, r.Parameters[name]) }
	marked := func(name string) bool { return slices.Contains(r.SensitiveParameters, name) }

	p.Path = r.Title
	if v, set := r.Parameters["path"]; set && v != nil {
		s, ok := v.(string)
		if !ok {
			return p, &unimplementedValue{r.Ref(), "path", shownValue(r, "path", v)}
		}
		p.Path = s
	}
	switch {
	case strings.HasPrefix(p.Path, "/"):
		if !concatPathClean(p.Path) {
			return p, &unimplementedValue{r.Ref(), "path", shownValue(r, "path", p.Path)}
		}
	case windowsAbsolute.MatchString(p.Path):
		return p, &unimplementedValue{r.Ref(), "path", shownValue(r, "path", p.Path)}
	default:
		return p, paramFailure(r, "path", "File paths must be fully qualified, not '"+redactIf(marked("path"), "%s", p.Path)+"'")
	}

	switch v := r.Parameters["ensure"]; v {
	case nil, "present":
	case "absent":
		p.Absent = true
	default:
		if holdsHash(v) {
			return p, &unimplementedValue{r.Ref(), "ensure", shown("ensure")}
		}
		return p, paramFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", shown("ensure")))
	}

	var err error
	if p.Tag, err = concatTag(r); err != nil {
		return p, err
	}

	// owner and group accept any value in Ruby; native copies a scalar.
	for _, name := range []string{"owner", "group"} {
		v := r.Parameters[name]
		switch v.(type) {
		case nil:
			continue
		case []any, map[string]any:
			return p, &unimplementedValue{r.Ref(), name, shownValue(r, name, v)}
		}
		// type/file/group.rb:20 refuses a false or empty group; owner has no check.
		if name == "group" && (v == false || v == "") {
			return p, paramFailure(r, name, "Invalid group name '"+redactIf(marked(name), "%s", rubyInspectValue(v))+"'")
		}
		p.Copied[name] = v
	}

	if v := param(r, "mode"); v != nil {
		if _, err := parseDeclaredMode(v, marked("mode")); err != nil {
			return p, paramFailure(r, "mode", err.Error())
		}
		p.Copied["mode"] = v
	}

	switch v := r.Parameters["order"]; v {
	case nil, "numeric":
	case "alpha":
		p.Order = "alpha"
	default:
		if holdsHash(v) {
			return p, &unimplementedValue{r.Ref(), "order", shown("order")}
		}
		return p, paramFailure(r, "order", fmt.Sprintf("Invalid value %s. Valid values are alpha, numeric.", shown("order")))
	}

	switch v := r.Parameters["backup"].(type) {
	case nil, string, bool:
		if v == true {
			return p, &unimplementedValue{r.Ref(), "backup", "true"}
		}
		if v != nil {
			p.Copied["backup"] = v
		}
	default:
		return p, paramFailure(r, "backup", "Backup must be a Boolean or String")
	}

	// boolean reads one Puppet::Parameter::Boolean into dst, when the catalog
	// sets it; copied adds it, coerced, to what the generated File takes.
	boolean := func(name string, dst *bool, copied bool) error {
		v, set := r.Parameters[name]
		if !set || v == nil {
			return nil
		}
		b, ok := concatBool(v)
		if !ok {
			if holdsHash(v) {
				// Ruby refuses it, but inspect of a Hash is not ported.
				return &unimplementedValue{r.Ref(), name, shownValue(r, name, v)}
			}
			return paramFailure(r, name, fmt.Sprintf("Invalid value %s. Valid values are true, false, yes, no.", shown(name)))
		}
		if dst != nil {
			*dst = b
		}
		if copied {
			p.Copied[name] = b
		}
		return nil
	}
	text := func(name, label string) error {
		switch v := r.Parameters[name].(type) {
		case nil:
		case string:
			p.Copied[name] = v
		default:
			return paramFailure(r, name, label+" must be a String")
		}
		return nil
	}

	steps := []func() error{
		func() error { return boolean("replace", &p.Replace, false) },
		func() error { return text("validate_cmd", "Validate_cmd") },
		func() error { return boolean("ensure_newline", &p.EnsureNewline, false) },
		func() error {
			switch v := r.Parameters["format"]; v {
			case nil, "plain":
				return nil
			case "yaml", "json", "json-array", "json-pretty", "json-array-pretty":
				return &unimplementedValue{r.Ref(), "format", shownValue(r, "format", v)}
			default:
				if holdsHash(v) {
					return &unimplementedValue{r.Ref(), "format", shown("format")}
				}
				return paramFailure(r, "format", fmt.Sprintf("Invalid value %s. Valid values are %s.", shown("format"), strings.Join(concatFormats, ", ")))
			}
		},
		func() error { return boolean("force", nil, false) },
		func() error { return boolean("selinux_ignore_defaults", nil, true) },
		func() error { return text("selrange", "Selrange") },
		func() error { return text("selrole", "Selrole") },
		func() error { return text("seltype", "Seltype") },
		func() error { return text("seluser", "Seluser") },
		func() error { return boolean("show_diff", nil, true) },
		func() error { return boolean("create_empty_file", &p.CreateEmptyFile, false) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return p, err
		}
	}
	p.Copied["replace"] = p.Replace
	return p, nil
}

// Validate implements Validating: the parameters in definition order, then the
// type-level validate block (FR9:87-96).
func (ConcatFragment) Validate(r catalog.Resource) error {
	_, err := parseConcatFragment(r)
	return err
}

func parseConcatFragment(r catalog.Resource) (concatFragmentParams, error) {
	p := concatFragmentParams{Name: r.Title, Order: "10"}
	if err := concatRich(r); err != nil {
		return p, err
	}
	if v := r.Parameters["name"]; v != nil {
		s, ok := v.(string)
		if !ok {
			return p, &unimplementedValue{r.Ref(), "name", shownValue(r, "name", v)}
		}
		p.Name = s
	}

	switch v := r.Parameters["target"].(type) {
	case nil:
	case string:
		p.Target = v
	default:
		return p, paramFailure(r, "target", "Target must be a String")
	}

	hasContent := false
	switch v := r.Parameters["content"].(type) {
	case nil:
	case string:
		p.Content, hasContent = v, true
	default:
		return p, paramFailure(r, "content", "Content must be a String")
	}

	// source is read through Puppet::FileServing, which native does not
	// implement (spec §0 row 1 (c)); its own refusals come from the host.
	if v := r.Parameters["source"]; v != nil {
		return p, &unimplementedValue{r.Ref(), "source", shownValue(r, "source", v)}
	}

	switch v := param(r, "order").(type) {
	case nil:
	case string:
		p.Order = v
	case float64:
		switch {
		case v != math.Trunc(v):
			return p, paramFailure(r, "order", "$order is not a string or integer.")
		case math.Abs(v) >= 1<<53:
			return p, &unimplementedValue{r.Ref(), "order", shownValue(r, "order", v)}
		}
		p.Order = strconv.FormatInt(int64(v), 10)
	default:
		return p, paramFailure(r, "order", "$order is not a string or integer.")
	}
	if orderForbidden.MatchString(p.Order) {
		return p, paramFailure(r, "order", "Order cannot contain '/', ':', or '\\n'.")
	}

	var err error
	if p.Tag, err = concatTag(r); err != nil {
		return p, err
	}

	if p.Target == nil && p.Tag == nil {
		return p, fmt.Errorf("Validation of %s failed: No 'target' or 'tag' set", r.Ref())
	}
	if !hasContent {
		return p, fmt.Errorf("Validation of %s failed: Set either 'source' or 'content'", r.Ref())
	}
	return p, nil
}

// Type implements Provider.
func (ConcatFile) Type() string { return "concat_file" }

// Type implements Provider.
func (ConcatFragment) Type() string { return "concat_fragment" }

// ProviderUsed implements Provider: the types have no providers.
func (ConcatFile) ProviderUsed() *string { return nil }

// ProviderUsed implements Provider: the types have no providers.
func (ConcatFragment) ProviderUsed() *string { return nil }

// Attrs implements Provider: every parameter type/concat_file.rb declares.
func (ConcatFile) Attrs() Attrs {
	return NewAttrs("path", "ensure", "tag", "owner", "group", "mode", "order", "backup", "replace", "validate_cmd",
		"ensure_newline", "format", "force", "selinux_ignore_defaults", "selrange", "selrole", "seltype", "seluser",
		"show_diff", "create_empty_file")
}

// Attrs implements Provider: every parameter type/concat_fragment.rb declares.
func (ConcatFragment) Attrs() Attrs {
	return NewAttrs("name", "target", "content", "source", "order", "tag")
}

// Properties implements PropertyDeclaring: ensure, and owner, group and mode,
// which the type declares as Puppet::Property subclasses (spec §3 sensitive
// marks), so a marked one is redacted without a warning.
func (ConcatFile) Properties() Attrs { return NewAttrs("ensure", "owner", "group", "mode") }

// Properties implements PropertyDeclaring: the type has none.
func (ConcatFragment) Properties() Attrs { return NewAttrs() }

// SilentlyMarked implements SilentlyMarking: concat_fragment's
// set_sensitive_parameters override marks content, a parameter, without the
// warning the generic rule gives (FR9:98-105).
func (ConcatFragment) SilentlyMarked() Attrs { return NewAttrs("content") }

// Apply implements Provider. A Concat_file's work is done by the File its
// generator builds, and a fragment is only read by that generator: neither
// has a state of its own to bring about, so there is nothing to change.
func (ConcatFile) Apply(context.Context, catalog.Resource, bool) ([]Change, []Warning, error) {
	return nil, nil, nil
}

// Apply implements Provider; see ConcatFile.Apply.
func (ConcatFragment) Apply(context.Context, catalog.Resource, bool) ([]Change, []Warning, error) {
	return nil, nil, nil
}

// Prefetch implements Provider.
func (c ConcatFile) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return c, nil }

// Prefetch implements Provider.
func (c ConcatFragment) Prefetch(context.Context, []catalog.Resource) (Provider, error) {
	return c, nil
}

// Refresh implements Provider.
func (ConcatFile) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// Refresh implements Provider.
func (ConcatFragment) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}
