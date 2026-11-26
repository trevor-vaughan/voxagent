package inifile

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// settingParams is an ini_setting resource as type/ini_setting.rb munges it
// (ts:). Ensure is "" only for `ensure => []`, which leaves Ruby without an
// ensure value; Route sends such a resource to the host (ruling R15). IndentWidth is the to_i that IniFile#initialize applies to the
// given value (if:17), which no later step can tell from the value itself.
type settingParams struct {
	Ensure                  string
	Section                 string
	Setting                 *string
	Value                   []string
	Path                    string
	KeyValSeparator         string
	SectionPrefix           string
	SectionSuffix           string
	IndentChar              string
	IndentWidth             *int
	ForceNewSectionCreation bool
	Refreshonly             bool
}

// subsettingParams is an ini_subsetting resource as type/ini_subsetting.rb
// munges it (tss:).
type subsettingParams struct {
	Ensure                    string
	Section                   string
	Setting                   string
	Subsetting                string
	SubsettingSeparator       string
	SubsettingKeyValSeparator string
	Path                      string
	KeyValSeparator           string
	QuoteChar                 string
	InsertType                string
	Value                     *string
	InsertValue               *string
	UseExactMatch             bool
	DeleteIfEmpty             bool
}

// sectionParams is an ini_section resource as type/ini_section.rb munges it
// (tsc:).
type sectionParams struct {
	Ensure        string
	Section       string
	Path          string
	SectionPrefix string
	SectionSuffix string
}

var (
	// ts:50: the setting munge's whitespace test. Ruby's ^ and $ are line
	// anchors, so (?m); \s is rubyregexp.Space.
	settingPadded = regexp.MustCompile(`(?m)(^` + rubyregexp.Space + `|` + rubyregexp.Space + `$)`)
	// tss:78: quote_char's check, line-anchored as well.
	quoteCharPattern = regexp.MustCompile(`(?m)^["']?$`)
	// Puppet::Util.absolute_path? on POSIX (util.rb:252): ^/ is line-anchored.
	absolutePathPosix = regexp.MustCompile(`(?m)^/`)
)

// parseSetting munges r as type/ini_setting.rb does and reports the first
// failure in Ruby's allattrs order (type.rb:2441-2463): the properties ensure
// and value, then the parameters as defined. A value shape the native port
// does not implement is a *rubyvalue.UnimplementedValue (spec §2 condition 6),
// found at its place in that order. The error text is Ruby's as Puppet wraps
// it (Parameter <p> failed on <Type>[<title>]: <message>).
func parseSetting(r catalog.Resource) (settingParams, error) {
	p := iniParser{r}
	s := settingParams{Section: "", KeyValSeparator: " = ", SectionPrefix: "[", SectionSuffix: "]", IndentChar: " ", ForceNewSectionCreation: true}
	if err := rubyvalue.Rich(r); err != nil {
		return s, err
	}
	steps := []func() error{
		func() (err error) { s.Ensure, err = p.ensure(); return },
		func() (err error) { s.Value, err = p.settingValue(); return },
		func() (err error) { s.Section, err = p.text("section", s.Section); return },
		func() (err error) { s.Setting, err = p.settingName(); return },
		func() (err error) {
			s.ForceNewSectionCreation, err = p.boolean("force_new_section_creation", true)
			return
		},
		func() (err error) { s.Path, err = p.path(); return },
		p.showDiff,
		func() (err error) { s.KeyValSeparator, err = p.text("key_val_separator", s.KeyValSeparator); return },
		func() (err error) { s.SectionPrefix, err = p.text("section_prefix", s.SectionPrefix); return },
		func() (err error) { s.SectionSuffix, err = p.text("section_suffix", s.SectionSuffix); return },
		func() (err error) { s.IndentChar, err = p.text("indent_char", s.IndentChar); return },
		func() (err error) { s.IndentWidth, err = p.indentWidth(); return },
		func() (err error) { s.Refreshonly, err = p.boolean("refreshonly", false); return },
	}
	return s, runSteps(steps)
}

// parseSubsetting munges r as type/ini_subsetting.rb does (see parseSetting).
func parseSubsetting(r catalog.Resource) (subsettingParams, error) {
	p := iniParser{r}
	s := subsettingParams{SubsettingSeparator: " ", KeyValSeparator: " = ", InsertType: "end"}
	if err := rubyvalue.Rich(r); err != nil {
		return s, err
	}
	steps := []func() error{
		func() (err error) { s.Ensure, err = p.ensure(); return },
		func() (err error) { s.Value, err = p.optionalText("value"); return },
		func() (err error) { s.Section, err = p.text("section", s.Section); return },
		func() (err error) { s.Setting, err = p.text("setting", s.Setting); return },
		func() (err error) { s.Subsetting, err = p.text("subsetting", s.Subsetting); return },
		func() (err error) {
			s.SubsettingSeparator, err = p.text("subsetting_separator", s.SubsettingSeparator)
			return
		},
		func() (err error) {
			s.SubsettingKeyValSeparator, err = p.text("subsetting_key_val_separator", s.SubsettingKeyValSeparator)
			return
		},
		func() (err error) { s.Path, err = p.path(); return },
		p.showDiff,
		func() (err error) { s.KeyValSeparator, err = p.text("key_val_separator", s.KeyValSeparator); return },
		func() (err error) { s.QuoteChar, err = p.quoteChar(); return },
		func() (err error) { s.UseExactMatch, err = p.trueFalse("use_exact_match"); return },
		func() (err error) {
			s.InsertType, err = p.enum("insert_type", []string{"start", "end", "before", "after", "index"}, s.InsertType)
			return
		},
		func() (err error) { s.InsertValue, err = p.optionalText("insert_value"); return },
		func() (err error) { s.DeleteIfEmpty, err = p.trueFalse("delete_if_empty"); return },
	}
	return s, runSteps(steps)
}

// parseSection munges r as type/ini_section.rb does (see parseSetting).
func parseSection(r catalog.Resource) (sectionParams, error) {
	p := iniParser{r}
	s := sectionParams{SectionPrefix: "[", SectionSuffix: "]"}
	if err := rubyvalue.Rich(r); err != nil {
		return s, err
	}
	steps := []func() error{
		func() (err error) { s.Ensure, err = p.ensure(); return },
		func() (err error) { s.Section, err = p.sectionName(); return },
		func() (err error) { s.Path, err = p.path(); return },
		func() (err error) { s.SectionPrefix, err = p.text("section_prefix", s.SectionPrefix); return },
		func() (err error) { s.SectionSuffix, err = p.text("section_suffix", s.SectionSuffix); return },
	}
	return s, runSteps(steps)
}

func runSteps(steps []func() error) error {
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// iniParser reads one resource's attributes.
type iniParser struct{ r catalog.Resource }

func (p iniParser) sensitive(attr string) bool {
	return slices.Contains(p.r.SensitiveParameters, attr)
}

// unimplemented refuses a value shape the port does not implement.
func (p iniParser) unimplemented(attr string, v any) error {
	return &rubyvalue.UnimplementedValue{Ref: p.r.Ref(), Attr: attr, Shown: rubyvalue.ShownValue(p.r, attr, v)}
}

// get is an attribute's catalog value, nil when unset or undef. A Hash is
// refused: inspect of one is not ported (D-53).
func (p iniParser) get(attr string) (any, error) {
	v := rubyvalue.Param(p.r, attr)
	if rubyvalue.HoldsHash(v) {
		return nil, p.unimplemented(attr, v)
	}
	return v, nil
}

// elements is an attribute's value as the list Puppet::Property#should= makes
// of it; set is false for an unset or undef attribute.
func (p iniParser) elements(attr string) (elems []any, set bool, err error) {
	v, err := p.get(attr)
	if err != nil || v == nil {
		return nil, false, err
	}
	if list, ok := v.([]any); ok {
		return list, true, nil
	}
	return []any{v}, true, nil
}

// ensure is the ensure property (ts:8-23, tss:7-11, tsc:5-20): every element is
// validated against present and absent, the first is the value.
func (p iniParser) ensure() (string, error) {
	elems, set, err := p.elements("ensure")
	if err != nil {
		return "", err
	}
	if !set {
		return "present", nil
	}
	ensure := ""
	for i, v := range elems {
		s, ok := v.(string)
		if !ok || (s != "present" && s != "absent") {
			return "", rubyvalue.ParamFailure(p.r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", rubyvalue.ShownValue(p.r, "ensure", v)))
		}
		if i == 0 {
			ensure = s
		}
	}
	return ensure, nil
}

// text is a parameter Ruby takes as given: a String, or the default when
// unset. Any other value is refused, since later steps of Ruby's providers
// treat it dynamically.
func (p iniParser) text(attr, def string) (string, error) {
	v, err := p.get(attr)
	if err != nil {
		return "", err
	}
	switch t := v.(type) {
	case nil:
		return def, nil
	case string:
		return t, nil
	}
	return "", p.unimplemented(attr, v)
}

// optionalText is text with no default: nil when unset. For the subsetting's
// value property (tss:88) only the first element counts, and any other shape
// than a String is refused.
func (p iniParser) optionalText(attr string) (*string, error) {
	v, err := p.get(attr)
	if err != nil || v == nil {
		return nil, err
	}
	s, ok := v.(string)
	if !ok {
		return nil, p.unimplemented(attr, v)
	}
	return &s, nil
}

// path is a path parameter's validate (ts:60-64): Puppet::Util.absolute_path?
// on POSIX. A non-String is not absolute, whose message prints its to_s, an
// Array's being its inspect (Ruby also logs a warning, which is not ported).
func (p iniParser) path() (string, error) {
	v, err := p.get("path")
	if err != nil || v == nil {
		return "", err
	}
	// Kept from Ruby (util.rb:252): the POSIX pattern is ^/, which a later line
	// of a multi-line path also satisfies.
	if s, ok := v.(string); ok && absolutePathPosix.MatchString(s) {
		return s, nil
	}
	shown := rubyvalue.InspectValue(v)
	if s, ok := v.(string); ok {
		shown = s
	}
	return "", rubyvalue.ParamFailure(p.r, "path", "File paths must be fully qualified, not '"+rubyvalue.RedactIf(p.sensitive("path"), "%s", shown)+"'")
}

// settingName is ini_setting's setting munge (ts:49-52).
func (p iniParser) settingName() (*string, error) {
	v, err := p.get("setting")
	if err != nil || v == nil {
		return nil, err
	}
	s, ok := v.(string)
	if !ok {
		// match? has no receiver to run on; Ruby's NoMethodError text is the
		// version's (3.2: "undefined method `match?' for 1:Integer").
		return nil, p.unimplemented("setting", v)
	}
	if settingPadded.MatchString(s) {
		// Kept from Ruby (ts:50): Puppet.warn is private, so the warning for a
		// padded setting raises NoMethodError and the resource fails (S1). The
		// text is Ruby 3.2's, the oracle's.
		return nil, rubyvalue.MungeFailure(p.r, "setting", s, "private method `warn' called for Puppet:Module")
	}
	stripped := rubyvalue.Strip(s)
	return &stripped, nil
}

// settingValue is ini_setting's value property (ts:84-93): each element is
// munged, true, false and Numeric to their to_s, a String stripped. Any other
// element has no strip, and Ruby's NoMethodError text is the version's, so
// it is refused. nil is returned only for an unset value.
func (p iniParser) settingValue() ([]string, error) {
	elems, set, err := p.elements("value")
	if err != nil || !set {
		return nil, err
	}
	out := make([]string, 0, len(elems))
	for _, v := range elems {
		switch t := v.(type) {
		case string:
			out = append(out, rubyvalue.Strip(t))
		case bool:
			out = append(out, strconv.FormatBool(t))
		case float64:
			switch {
			case t != math.Trunc(t):
				out = append(out, rubyvalue.Float(t))
			case math.Abs(t) < 1<<53:
				out = append(out, strconv.FormatInt(int64(t), 10))
			default:
				return nil, p.unimplemented("value", v) // a Bignum
			}
		default:
			return nil, p.unimplemented("value", v)
		}
	}
	return out, nil
}

// showDiff validates show_diff (ts:67-77): it accepts true, false and md5 and
// has no effect on voxagent's output (spec §3).
func (p iniParser) showDiff() error {
	_, err := p.enum("show_diff", []string{"true", "md5", "false"}, "true")
	return err
}

// enum is a parameter declared with newvalues: a String or boolean whose text
// is one of allowed. def is the result when unset.
func (p iniParser) enum(attr string, allowed []string, def string) (string, error) {
	v, err := p.get(attr)
	if err != nil || v == nil {
		return def, err
	}
	s := ""
	switch t := v.(type) {
	case string:
		s = t
	case bool:
		s = strconv.FormatBool(t)
	}
	if !slices.Contains(allowed, s) {
		valid := ""
		for i, a := range allowed {
			if i > 0 {
				valid += ", "
			}
			valid += a
		}
		return "", rubyvalue.ParamFailure(p.r, attr, fmt.Sprintf("Invalid value %s. Valid values are %s.", rubyvalue.ShownValue(p.r, attr, v), valid))
	}
	return s, nil
}

// trueFalse is an attribute declared newvalues(:true, :false), default false.
func (p iniParser) trueFalse(attr string) (bool, error) {
	s, err := p.enum(attr, []string{"true", "false"}, "false")
	return s == "true", err
}

// boolean is a Puppet::Parameter::Boolean (ts:55, ts:139): true, false and
// the Strings true, false, yes and no, case-sensitively, validated before
// Puppet::Coercion.boolean munges them.
func (p iniParser) boolean(attr string, def bool) (bool, error) {
	v, err := p.get(attr)
	if err != nil || v == nil {
		return def, err
	}
	switch v {
	case true, "true", "yes":
		return true, nil
	case false, "false", "no":
		return false, nil
	}
	return false, rubyvalue.ParamFailure(p.r, attr, fmt.Sprintf("Invalid value %s. Valid values are true, false, yes, no.", rubyvalue.ShownValue(p.r, attr, v)))
}

// indentWidth is ini_setting's indent_width (ts:135), which Ruby keeps as
// given until IniFile#initialize takes its to_i (if:17).
func (p iniParser) indentWidth() (*int, error) {
	v, err := p.get("indent_width")
	if err != nil || v == nil {
		return nil, err
	}
	var n int64
	switch t := v.(type) {
	case string:
		n = rubyvalue.ToI(t)
	case float64:
		if math.Abs(t) >= 1<<53 {
			return nil, p.unimplemented("indent_width", v)
		}
		n = int64(t) // Integer and Float#to_i alike truncate
	default:
		return nil, p.unimplemented("indent_width", v)
	}
	if n != int64(int(n)) {
		return nil, p.unimplemented("indent_width", v)
	}
	w := int(n)
	return &w, nil
}

// quoteChar is ini_subsetting's quote_char (tss:73-80): empty, ' or ", where
// Ruby's anchors are line anchors.
func (p iniParser) quoteChar() (string, error) {
	s, err := p.text("quote_char", "")
	if err != nil {
		return "", err
	}
	// Kept from Ruby (tss:78): ^["']?$ matches any line, so a value with a
	// line that is empty, ' or " passes.
	if !quoteCharPattern.MatchString(s) {
		return "", rubyvalue.ParamFailure(p.r, "quote_char", `:quote_char valid values are '', '"' and "'"`)
	}
	return s, nil
}

// sectionName is ini_section's section (tsc:27-36): it defaults to the name,
// and a section whose strip is empty is refused. Ruby validates the default as
// well; its failure has no Parameter prefix (measured with puppet apply).
func (p iniParser) sectionName() (string, error) {
	v, err := p.get("section")
	if err != nil {
		return "", err
	}
	given := v != nil
	if !given {
		if v = rubyvalue.Param(p.r, "name"); v == nil {
			v = p.r.Title
		}
	}
	s, ok := v.(string)
	if !ok {
		if given {
			return "", p.unimplemented("section", v)
		}
		return "", p.unimplemented("name", v)
	}
	if rubyvalue.Strip(s) == "" {
		if given {
			return "", rubyvalue.ParamFailure(p.r, "section", "Section names must not be empty")
		}
		//nolint:staticcheck // ST1005: Ruby's text (tsc:34), which the report must carry verbatim.
		return "", fmt.Errorf("Section names must not be empty")
	}
	return s, nil
}
