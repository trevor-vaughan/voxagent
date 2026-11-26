package croncore

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// release130 is the cron_core release whose numfix and type guards differ
// from 2.1.0 (spec §4.3); any other release gets 2.1.0's rules.
const release130 = "1.3.0"

// cronParams is a Cron resource munged as type/cron.rb munges it. Ensure is ""
// when the resource manages nothing. Should holds exactly the properties that
// have a should value, the target default included: a time field whose first
// element munged to :absent is a Kind Absent Value, any other time field and
// environment a List (a later :absent element stays the literal "absent", which
// is how Ruby displays and writes it: its display, in-sync result and written
// line all agree with this encoding. Only unnamed-record field matching
// (Adopt) differs, :absent against "absent"; that is unreachable, because
// cronie refuses an absent field at install), command a Str, special a Str or Absent,
// user and target a Str.
type cronParams struct {
	Name, Title string
	Ensure      string
	Should      map[string]parsedfile.Value
}

// The type's patterns. Ruby's ^ and $ are line anchors, so every one is (?m)
// (spec §4.5); \s is rubyregexp.Space because RE2's lacks \v, and \d and \w
// are ASCII in Ruby, as written here.
var (
	stepPattern      = regexp.MustCompile(`(?m)^\*/[0-9]+$`)                                                                                                                // type:156
	rangePattern     = regexp.MustCompile(`(?m)^[0-9]+-[0-9]+$`)                                                                                                            // type:161
	rangeStepPattern = regexp.MustCompile(`(?m)^[0-9]+-[0-9]+/[0-9]+$`)                                                                                                     // type:166
	digitsPattern    = regexp.MustCompile(`(?m)^[0-9]+$`)                                                                                                                   // type:76 numfix
	environmentLine  = regexp.MustCompile(`(?m)^` + rubyregexp.Space + `*([a-zA-Z0-9_]+)` + rubyregexp.Space + `*=` + rubyregexp.Space + `*(.*)` + rubyregexp.Space + `*$`) // type:324
)

// timeSpec is a time property's bounds (self.boundaries) and, for weekday and
// month, its alpha name table (type:272-300). Month's list starts with a
// placeholder so that an index is the month number.
type timeSpec struct {
	lower, upper int64
	names        []string
}

var timeSpecs = map[string]timeSpec{
	"minute":   {0, 59, nil},
	"hour":     {0, 23, nil},
	"weekday":  {0, 7, []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}},
	"month":    {1, 12, []string{"___placeholder", "january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}},
	"monthday": {1, 31, nil},
}

// specials are the special property's accepted values (type:244-247); "absent"
// is munged to :absent.
var specials = []string{"reboot", "yearly", "annually", "monthly", "weekly", "daily", "midnight", "hourly", "absent"}

// Validate implements ral.Validating: the first failure of parseCron. The
// target default is not looked up, so validation never reads the NSS.
func (c Cron) Validate(r catalog.Resource) error {
	_, err := parseCron(r, c.release, c.encoding, func() (string, error) { return "", nil })
	return err
}

// parseCron munges r for the given cron_core release and reports the first
// failure in Ruby's allattrs order (type.rb:2441-2463): name, provider, the
// properties of cronProperties, then the type-level validate (type:412-422).
// A value shape the native parser does not implement is a
// *rubyvalue.UnimplementedValue (spec §2 condition 6), found at its place in
// that order. encoding quotes values in refusal texts (spec §4).
// defaultTarget is the target default when neither user nor target is set:
// the name of getpwuid(getuid()), else "root" (type:395-409); its error is
// returned as is.
func parseCron(r catalog.Resource, release, encoding string, defaultTarget func() (string, error)) (cronParams, error) {
	c := cronParser{r: r, release: release, encoding: encoding}
	p := cronParams{Name: r.Title, Title: r.Title, Should: map[string]parsedfile.Value{}}
	if n, ok := r.Parameters["name"].(string); ok {
		p.Name = n
	}

	ensureSet, err := c.ensure(&p)
	if err != nil {
		return p, err
	}
	steps := []func(*cronParams) error{c.command, c.special}
	for _, field := range cronTimeFields {
		steps = append(steps, func(p *cronParams) error { return c.timeField(p, field) })
	}
	steps = append(steps, c.environment,
		func(p *cronParams) error { return c.text(p, "user") },
		func(p *cronParams) error { return c.text(p, "target") })
	for _, step := range steps {
		if err := step(&p); err != nil {
			return p, err
		}
	}

	// Defaults, as Puppet::Type#set_parameters applies them after every given
	// attribute is set. ensure's is decided before user's and target's, so only
	// given properties make the resource managed (Type#managed?).
	if !ensureSet && len(p.Should) > 0 {
		p.Ensure = "present"
	}
	if _, targetSet := c.elements("target"); !targetSet {
		if user, ok := p.Should["user"]; ok {
			p.Should["target"] = user
		} else {
			target, err := defaultTarget()
			if err != nil {
				return p, err
			}
			p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: target}
		}
	}

	if special, set := p.Should["special"]; set && special.Kind != parsedfile.Absent {
		for _, field := range []string{"minute", "hour", "weekday", "monthday", "month"} {
			if value, set := p.Should[field]; set && value.Kind != parsedfile.Absent {
				return p, fmt.Errorf("Validation of %s failed: %s cannot specify both a special schedule and a value for %s", r.Ref(), r.Ref(), field)
			}
		}
	}
	return p, nil
}

// cronParser holds what every attribute's checks need.
type cronParser struct {
	r        catalog.Resource
	release  string
	encoding string
}

// valueKind is how a catalog value reads as a Ruby object.
type valueKind int

const (
	kindString  valueKind = iota
	kindInteger           // a Ruby Integer (a whole JSON number, D-84)
	// kindPlain is an object this file can show as Ruby does and Ruby's checks
	// refuse on 2.1.0 but fail on with a version-specific NoMethodError on 1.3.0:
	// true, false, nil (inspect "nil", to_s ""), a non-integral Float, and an
	// Array of Strings, Integers and such values (nested to any depth).
	kindPlain
	// kindOpaque is a value whose Ruby text is not reproduced: a Hash, a whole
	// number beyond 2^53 (a Bignum), or an Array holding one.
	kindOpaque
)

// classify reads one catalog value (a decoded number is a float64, as in
// rubyvalue.Param); n is the value of a kindInteger.
func classify(v any) (kind valueKind, n int64) {
	switch t := v.(type) {
	case string:
		return kindString, 0
	case int:
		return kindInteger, int64(t)
	case int64:
		return kindInteger, t
	case float64:
		switch {
		case t != math.Trunc(t):
			return kindPlain, 0
		case math.Abs(t) < 1<<53:
			return kindInteger, int64(t)
		}
	case bool, nil:
		return kindPlain, 0
	case []any:
		for _, e := range t {
			if k, _ := classify(e); k == kindOpaque {
				return kindOpaque, 0
			}
		}
		return kindPlain, 0
	}
	return kindOpaque, 0
}

// elements is an attribute's value as the list Puppet::Property#should= makes
// of it; set is false for an unset or undef attribute.
func (c cronParser) elements(attr string) (elems []any, set bool) {
	switch v := c.r.Parameters[attr].(type) {
	case nil:
		return nil, false
	case []any:
		return v, true
	default:
		return []any{v}, true
	}
}

func (c cronParser) sensitive(attr string) bool {
	return slices.Contains(c.r.SensitiveParameters, attr)
}

// inspect is Ruby's inspect of a value under c.encoding, [redacted] for a
// Sensitive attribute (D-78).
func (c cronParser) inspect(attr string, v any) string {
	if c.sensitive(attr) {
		return "[redacted]"
	}
	return inspectIn(v, c.encoding)
}

func inspectIn(v any, encoding string) string {
	switch t := v.(type) {
	case string:
		return rubyvalue.InspectIn(t, encoding)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = inspectIn(e, encoding)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return rubyvalue.InspectValue(v)
	}
}

// toS is Ruby's to_s of a kindPlain or kindInteger value (an Array's is its
// inspect), [redacted] for a Sensitive attribute.
func (c cronParser) toS(attr string, v any) string {
	if c.sensitive(attr) {
		return "[redacted]"
	}
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		if t != math.Trunc(t) {
			return rubyvalue.Float(t)
		}
	case []any:
		return inspectIn(t, c.encoding)
	}
	return rubyvalue.InspectValue(v)
}

func (c cronParser) unimplemented(attr string, v any) error {
	return &rubyvalue.UnimplementedValue{Ref: c.r.Ref(), Attr: attr, Shown: rubyvalue.ShownValue(c.r, attr, v)}
}

// ensure sets p.Ensure from the ensure property (ensurable: present, absent);
// set reports whether the catalog gave one, which stops the default.
func (c cronParser) ensure(p *cronParams) (set bool, err error) {
	elems, set := c.elements("ensure")
	for i, v := range elems {
		if k, _ := classify(v); k == kindOpaque {
			return set, c.unimplemented("ensure", v)
		}
		s, isString := v.(string)
		if !isString || (s != "present" && s != "absent") {
			return set, rubyvalue.ParamFailure(c.r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", c.inspect("ensure", v)))
		}
		if i == 0 {
			p.Ensure = s
		}
	}
	return set, nil
}

// command is the command property (type:201-233): every element is stripped
// (a non-String has no strip), and should is the first. Every non-String,
// nil included, is delegated: Ruby's NoMethodError text is version-specific
// (spec §2 condition 6; nil reads "undefined method 'strip' for nil" on 3.4).
func (c cronParser) command(p *cronParams) error {
	elems, _ := c.elements("command")
	for i, v := range elems {
		s, ok := v.(string)
		if !ok {
			return c.unimplemented("command", v)
		}
		if i == 0 {
			p.Should["command"] = parsedfile.Value{Kind: parsedfile.Str, S: rubyvalue.Strip(s)}
		}
	}
	return nil
}

// special is the special property (type:244-262): each element validated, the
// first munged.
func (c cronParser) special(p *cronParams) error {
	elems, _ := c.elements("special")
	for i, v := range elems {
		if k, _ := classify(v); k == kindOpaque {
			return c.unimplemented("special", v)
		}
		s, isString := v.(string)
		if !isString || !slices.Contains(specials, s) {
			return rubyvalue.ParamFailure(c.r, "special", "Invalid special schedule "+c.inspect("special", v))
		}
		if i == 0 {
			p.Should["special"] = parsedfile.Value{Kind: parsedfile.Str, S: s}
			if s == "absent" {
				p.Should["special"] = parsedfile.Value{Kind: parsedfile.Absent}
			}
		}
	}
	return nil
}

// timeField munges every element of a time property (type:150-191), then
// applies CronParam#should: the first element :absent makes the should
// :absent.
func (c cronParser) timeField(p *cronParams, field string) error {
	elems, set := c.elements(field)
	if !set {
		return nil
	}
	var munged []string
	firstAbsent := false
	for i, v := range elems {
		s, isAbsent, err := c.mungeTime(field, v)
		if err != nil {
			return err
		}
		if i == 0 {
			firstAbsent = isAbsent
		}
		munged = append(munged, s)
	}
	if firstAbsent {
		p.Should[field] = parsedfile.Value{Kind: parsedfile.Absent}
		return nil
	}
	p.Should[field] = parsedfile.Value{Kind: parsedfile.List, L: munged}
	return nil
}

// mungeTime is the munge block of CronParam (type:150-191) for one element,
// per spec §4.2. An :absent is returned as the string "absent" with isAbsent.
func (c cronParser) mungeTime(field string, v any) (munged string, isAbsent bool, err error) {
	spec := timeSpecs[field]
	refuse := func(tail string) (string, bool, error) {
		return "", false, rubyvalue.ParamFailure(c.r, field, fmt.Sprintf("Munging failed for value %s in class %s: %s is not a valid %s", c.inspect(field, v), field, tail, field))
	}
	kind, n := classify(v)
	switch kind {
	case kindOpaque:
		return "", false, c.unimplemented(field, v)
	case kindPlain:
		// 1.3.0 sends it to String#=~, which it lacks; 2.1.0's numfix and
		// alphacheck refuse a non-String (type:76-79, 91-93).
		if c.release == release130 {
			return "", false, c.unimplemented(field, v)
		}
		return refuse(c.toS(field, v))
	case kindInteger:
		if n >= spec.lower && n <= spec.upper {
			return strconv.FormatInt(n, 10), false, nil
		}
		return refuse(c.toS(field, v))
	}

	s := v.(string)
	switch {
	case s == "absent":
		return "absent", true, nil
	case stepPattern.MatchString(s), rangePattern.MatchString(s), rangeStepPattern.MatchString(s):
		return s, false, nil
	case s == "*":
		return "absent", true, nil
	}
	if c.numfix(s) {
		if n := rubyvalue.ToI(s); n >= spec.lower && n <= spec.upper {
			return s, false, nil
		}
		return refuse(c.rawTail(field, s))
	}
	if spec.names != nil {
		if index, found := alphacheck(s, spec.names); found {
			return strconv.Itoa(index), false, nil
		}
	}
	return refuse(c.rawTail(field, s))
}

// rawTail is the value as the message's tail shows it: bare, so non-ASCII
// characters stay raw (M12), [redacted] when Sensitive.
func (c cronParser) rawTail(attr, s string) string {
	if c.sensitive(attr) {
		return "[redacted]"
	}
	return s
}

// numfix reports whether a String is accepted as a number (type:76-80 on 1.3.0:
// digits on some line; type:76-79 on 2.1.0: that, or any Kernel#Integer
// literal, a Bignum included).
func (c cronParser) numfix(s string) bool {
	if digitsPattern.MatchString(s) {
		return true
	}
	if c.release == release130 {
		return false
	}
	_, err := rubyvalue.Integer(s)
	return err == nil || errors.Is(err, strconv.ErrRange)
}

// alphacheck is type:91-108: s downcased; three characters match the first
// name with that prefix, anything else must be a whole name. Only ASCII is
// downcased, as no other character downcases to one of the names' letters.
func alphacheck(s string, names []string) (index int, found bool) {
	lower := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
	if len([]rune(lower)) == 3 {
		for i, name := range names {
			if name[:3] == lower {
				return i, true
			}
		}
		return 0, false
	}
	index = slices.Index(names, lower)
	return index, index >= 0
}

// environment is the environment property (type:308-336). Its elements are not
// munged: 'absent' stays the string.
func (c cronParser) environment(p *cronParams) error {
	elems, set := c.elements("environment")
	if !set {
		return nil
	}
	var lines []string
	for _, v := range elems {
		kind, _ := classify(v)
		s, isString := v.(string)
		switch {
		case kind == kindOpaque, !isString && c.release == release130:
			return c.unimplemented("environment", v)
		case !isString, s != "absent" && !environmentLine.MatchString(s):
			return rubyvalue.ParamFailure(c.r, "environment", "Invalid environment setting "+c.inspect("environment", v))
		}
		lines = append(lines, s)
	}
	p.Should["environment"] = parsedfile.Value{Kind: parsedfile.List, L: lines}
	return nil
}

// textProperty is the should of a property that takes a String and keeps the
// first of a list (user, target); set is false when it has none, which includes
// a first element of nil (Ruby's should is then nil, measured with the real
// type; the parameter still counts as given, so no default replaces it). A
// later nil is ignored, as Ruby ignores any element after the first.
func textProperty(r catalog.Resource, attr string) (text string, set bool, err error) {
	c := cronParser{r: r}
	elems, _ := c.elements(attr)
	if len(elems) == 0 {
		return "", false, nil
	}
	if elems[0] == nil {
		return "", false, nil
	}
	s, ok := elems[0].(string)
	if !ok {
		return "", false, c.unimplemented(attr, elems[0])
	}
	return s, true, nil
}

// text sets the should of user or target.
func (c cronParser) text(p *cronParams, attr string) error {
	s, set, err := textProperty(c.r, attr)
	if set {
		p.Should[attr] = parsedfile.Value{Kind: parsedfile.Str, S: s}
	}
	return err
}
