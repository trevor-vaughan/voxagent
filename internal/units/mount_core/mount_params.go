package mountcore

import (
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

// defaultTarget is the parsed provider's default_target (mp:4-14).
const defaultTarget = "/etc/fstab"

// The type's patterns. Ruby's ^ and $ are line anchors, so every one is (?m)
// (spec §4); \s is rubyregexp.Space because RE2's lacks \v.
var (
	whitespace = regexp.MustCompile(`(?m)` + rubyregexp.Space) // mt:135, 162, 172, 187
	// trailingSlashes is the name's munge (mt:258-260), also the prefetch hook's
	// (mp:229): the name up to its trailing slashes, per line.
	trailingSlashes = regexp.MustCompile(`(?m)^(.+?)/*$`)
)

// ensureValues are the ensure property's accepted values in Ruby's order, which
// is the order a refusal lists them (mt:26-124; `present` is an alias of
// `defined` and is not listed).
var ensureValues = []string{"defined", "unmounted", "absent", "mounted"}

// mountParams is a Mount resource munged as type/mount.rb munges it. Name is
// the munged name (trailing slashes stripped), Title the title as given, which
// every message keeps. Ensure is "" when the resource manages nothing, else
// one of ensureValues. Should holds exactly the properties that have a should
// value, the defaults included, every one a Str: device, fstype, options,
// blockdevice and target as given, pass and dump as Ruby's to_s of the value
// (Integers lists the attributes whose Ruby value is an Integer, which the
// report shows as a number), atboot as "yes" or "no" (Ruby's :yes and :no).
// atboot and blockdevice are in Should only so that they make the resource
// managed: they are written nowhere on Linux and never diffed (spec §4.4), and
// Task 21's apply must skip them. Remounts is the remounts parameter.
type mountParams struct {
	Name, Title, Ensure string
	Should              map[string]parsedfile.Value
	Integers            map[string]bool
	Remounts            bool
}

// Validate implements ral.Validating: the first failure of parseMount.
func (m Mount) Validate(r catalog.Resource) error {
	_, err := parseMount(r, m.release, m.encoding)
	return err
}

// parseMount munges r and reports the first failure in Ruby's allattrs order
// (type.rb:2441-2463): name, provider, then mountProperties, then remounts. A
// value shape the native parser does not implement is a
// *rubyvalue.UnimplementedValue (spec §2 condition 6), found at its place in
// that order. encoding quotes values in refusal texts (spec §4). release is
// accepted for the Manifest's two releases, whose type files do not differ in
// behaviour.
func parseMount(r catalog.Resource, _, encoding string) (mountParams, error) {
	m := mountParser{r: r, encoding: encoding}
	p := mountParams{Title: r.Title, Should: map[string]parsedfile.Value{}, Integers: map[string]bool{}}

	if err := m.name(&p); err != nil {
		return p, err
	}
	for _, step := range []func(*mountParams) error{
		m.ensure, m.device, m.blockdevice,
		m.noSpaceString("fstype"), m.noSpaceString("options"),
		m.free("pass"), m.atboot, m.dump,
	} {
		if err := step(&p); err != nil {
			return p, err
		}
	}
	targetSet, err := m.target(&p)
	if err != nil {
		return p, err
	}
	if p.Remounts, err = m.remounts(); err != nil {
		return p, err
	}

	// Defaults, as Puppet::Type#set_parameters applies them after every given
	// attribute is set: pass and dump are 0 (an Integer) when any given
	// property has a should value (Type#managed?, mt:194-202, 229-231); the
	// target's default follows them and does not count.
	if len(p.Should) > 0 || p.Ensure != "" {
		for _, attr := range []string{"pass", "dump"} {
			if _, ok := p.Should[attr]; !ok {
				p.Should[attr] = parsedfile.Value{Kind: parsedfile.Str, S: "0"}
				p.Integers[attr] = true
			}
		}
	}
	if !targetSet {
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: defaultTarget}
	}
	return p, nil
}

// mountParser holds what every attribute's checks need.
type mountParser struct {
	r        catalog.Resource
	encoding string
}

// elements is an attribute's value as the list Puppet::Property#should= makes
// of it; set is false for an unset or undef attribute.
func (m mountParser) elements(attr string) (elems []any, set bool) {
	switch v := m.r.Parameters[attr].(type) {
	case nil:
		return nil, false
	case []any:
		return v, true
	default:
		return []any{v}, true
	}
}

// inspect is Ruby's inspect of a String under m.encoding, [redacted] for a
// Sensitive attribute (D-78).
func (m mountParser) inspect(attr, s string) string {
	return rubyvalue.RedactIf(slices.Contains(m.r.SensitiveParameters, attr), "%s", rubyvalue.InspectIn(s, m.encoding))
}

// raw is a value interpolated into a message as it is, [redacted] for a
// Sensitive attribute (D-78).
func (m mountParser) raw(attr, s string) string {
	return rubyvalue.RedactIf(slices.Contains(m.r.SensitiveParameters, attr), "%s", s)
}

func (m mountParser) unimplemented(attr string, v any) error {
	return &rubyvalue.UnimplementedValue{Ref: m.r.Ref(), Attr: attr, Shown: rubyvalue.ShownValue(m.r, attr, v)}
}

// strings is every element of attr as a String; a non-String, nil included, is
// delegated: Ruby's validate or munge calls a String method on it and the
// NoMethodError text is Ruby-version-specific (spec §2 condition 6).
func (m mountParser) strings(attr string) (values []string, set bool, err error) {
	elems, set := m.elements(attr)
	for _, v := range elems {
		s, ok := v.(string)
		if !ok {
			return nil, set, m.unimplemented(attr, v)
		}
		values = append(values, s)
	}
	return values, set, nil
}

// texts is every element of attr as the text Ruby's to_s gives it: a String
// as is, an Integer in decimal. isInteger reports whether the first element
// is an Integer. Any other shape (a Float, a boolean, a Hash) is delegated:
// the catalog decodes every number to a float64, and only a whole one is
// taken for an Integer.
func (m mountParser) texts(attr string) (values []string, isInteger, set bool, err error) {
	elems, set := m.elements(attr)
	for i, v := range elems {
		switch t := v.(type) {
		case string:
			values = append(values, t)
		case int:
			values = append(values, strconv.Itoa(t))
			isInteger = isInteger || i == 0
		case int64:
			values = append(values, strconv.FormatInt(t, 10))
			isInteger = isInteger || i == 0
		case float64:
			if t != math.Trunc(t) || math.Abs(t) >= 1<<53 {
				return nil, false, set, m.unimplemented(attr, v)
			}
			values = append(values, strconv.FormatInt(int64(t), 10))
			isInteger = isInteger || i == 0
		default:
			return nil, false, set, m.unimplemented(attr, v)
		}
	}
	return values, isInteger, set, nil
}

// name is the name namevar (mt:247-261): the explicit name parameter, else the
// title. Any String is valid on Linux; the munge strips trailing slashes from
// each line.
func (m mountParser) name(p *mountParams) error {
	p.Name = m.r.Title
	if v := m.r.Parameters["name"]; v != nil {
		s, ok := v.(string)
		if !ok {
			return m.unimplemented("name", v)
		}
		p.Name = s
	}
	p.Name = trailingSlashes.ReplaceAllString(p.Name, "${1}")
	return nil
}

// ensure is the ensure property (mt:26-124): one of ensureValues or the alias
// present, which munges to defined. A value that is not a String is delegated.
func (m mountParser) ensure(p *mountParams) error {
	values, _, err := m.strings("ensure")
	if err != nil {
		return err
	}
	for i, s := range values {
		if s == "present" {
			s = "defined"
		}
		if !slices.Contains(ensureValues, s) {
			return rubyvalue.ParamFailure(m.r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are %s.",
				m.inspect("ensure", values[i]), strings.Join(ensureValues, ", ")))
		}
		if i == 0 {
			p.Ensure = s
		}
	}
	return nil
}

// device is the device property (mt:133-137): any String on Linux.
func (m mountParser) device(p *mountParams) error {
	values, _, err := m.strings("device")
	if err != nil {
		return err
	}
	if len(values) > 0 {
		p.Should["device"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
	}
	return nil
}

// blockdevice is the blockdevice property (mt:161-163): a String without
// whitespace. Its Solaris default is nil here. Nothing writes it on Linux
// (spec §4.4), so it is kept only to make the resource managed.
func (m mountParser) blockdevice(p *mountParams) error {
	values, _, err := m.strings("blockdevice")
	if err != nil {
		return err
	}
	for i, s := range values {
		if whitespace.MatchString(s) {
			return rubyvalue.ParamFailure(m.r, "blockdevice", "blockdevice must not contain whitespace: "+m.raw("blockdevice", s))
		}
		if i == 0 {
			p.Should["blockdevice"] = parsedfile.Value{Kind: parsedfile.Str, S: s}
		}
	}
	return nil
}

// noSpaceString is the fstype and options property (mt:170-173, 185-188): a
// String that holds no whitespace and is not empty.
func (m mountParser) noSpaceString(attr string) func(*mountParams) error {
	return func(p *mountParams) error {
		values, _, err := m.strings(attr)
		if err != nil {
			return err
		}
		for i, s := range values {
			if whitespace.MatchString(s) {
				return rubyvalue.ParamFailure(m.r, attr, attr+" must not contain whitespace: "+m.raw(attr, s))
			}
			if s == "" {
				return rubyvalue.ParamFailure(m.r, attr, attr+" must not be an empty string")
			}
			if i == 0 {
				p.Should[attr] = parsedfile.Value{Kind: parsedfile.Str, S: s}
			}
		}
		return nil
	}
}

// free is a property that takes any value (pass, mt:194-202): a String or an
// Integer, written with Ruby's to_s.
func (m mountParser) free(attr string) func(*mountParams) error {
	return func(p *mountParams) error {
		values, isInteger, _, err := m.texts(attr)
		if err != nil {
			return err
		}
		if len(values) > 0 {
			p.Should[attr] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
			p.Integers[attr] = isInteger
		}
		return nil
	}
}

// atboot is the atboot property (mt:205-217), a Puppet::Property::Boolean
// munged to :yes or :no. It has no value collection: Puppet::Coercion.boolean
// reads a boolean or a String that downcases to true, yes, false or no, and
// refuses anything else as a munge failure, per element. It is written nowhere
// on Linux and never out of sync (spec §4.4).
func (m mountParser) atboot(p *mountParams) error {
	elems, _ := m.elements("atboot")
	for i, v := range elems {
		var yes bool
		switch t := v.(type) {
		case bool:
			yes = t
		case string:
			switch strings.ToLower(t) {
			case "true", "yes":
				yes = true
			case "false", "no":
			default:
				return m.notBoolean(v)
			}
		default:
			return m.notBoolean(v)
		}
		if i == 0 {
			text := "no"
			if yes {
				text = "yes"
			}
			p.Should["atboot"] = parsedfile.Value{Kind: parsedfile.Str, S: text}
		}
	}
	return nil
}

// notBoolean is Puppet::Parameter#munge's wrapper around Coercion.boolean's
// refusal (parameter.rb:449); a Hash is delegated, as its inspect is not
// ported.
func (m mountParser) notBoolean(v any) error {
	if rubyvalue.HoldsHash(v) {
		return m.unimplemented("atboot", v)
	}
	shown := rubyvalue.ShownValue(m.r, "atboot", v)
	if s, ok := v.(string); ok {
		shown = m.inspect("atboot", s)
	}
	return rubyvalue.ParamFailure(m.r, "atboot", fmt.Sprintf("Munging failed for value %s in class atboot: expected a boolean value", shown))
}

// invalidBoolean refuses v for a parameter whose value collection lists valid
// (remounts); a Hash is delegated, as its inspect is not ported.
func (m mountParser) invalidBoolean(attr string, v any, valid string) error {
	if rubyvalue.HoldsHash(v) {
		return m.unimplemented(attr, v)
	}
	shown := rubyvalue.ShownValue(m.r, attr, v)
	if s, ok := v.(string); ok {
		shown = m.inspect(attr, s)
	}
	return rubyvalue.ParamFailure(m.r, attr, fmt.Sprintf("Invalid value %s. Valid values are %s.", shown, valid))
}

// dump is the dump property (mt:219-232): newvalue(%r{(0|1)}), unanchored, so
// any value whose to_s holds a 0 or a 1 is valid (`10`, `x1y`).
func (m mountParser) dump(p *mountParams) error {
	values, isInteger, _, err := m.texts("dump")
	if err != nil {
		return err
	}
	elems, _ := m.elements("dump")
	for i, s := range values {
		if !strings.ContainsAny(s, "01") {
			shown := rubyvalue.ShownValue(m.r, "dump", elems[i])
			if str, ok := elems[i].(string); ok {
				shown = m.inspect("dump", str)
			}
			return rubyvalue.ParamFailure(m.r, "dump", fmt.Sprintf("Invalid value %s. Valid values match /(0|1)/.", shown))
		}
	}
	if len(values) > 0 {
		p.Should["dump"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
		p.Integers["dump"] = isInteger
	}
	return nil
}

// target is the target property (mt:234-245): a String path, the first of a
// list. set reports whether the catalog gave one, which stops the default. A
// non-String is delegated.
func (m mountParser) target(p *mountParams) (set bool, err error) {
	values, set, err := m.strings("target")
	if err != nil {
		return set, err
	}
	if len(values) > 0 {
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
	}
	return set, nil
}

// remounts is the remounts parameter (mt:263-285): `true` or `false`, a
// boolean or the String, checked as one value (a parameter, not a property, so
// a list is refused whole); the default on Linux is true.
func (m mountParser) remounts() (bool, error) {
	switch t := m.r.Parameters["remounts"].(type) {
	case nil:
		return true, nil
	case bool:
		return t, nil
	case string:
		switch t {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, m.invalidBoolean("remounts", m.r.Parameters["remounts"], "true, false")
}
