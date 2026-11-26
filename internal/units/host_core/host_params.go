package hostcore

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// defaultTarget is the parsed provider's default_target (hp:10).
const defaultTarget = "/etc/hosts"

// The type's patterns. Ruby's ^ and $ are line anchors, so every one is (?m)
// (spec §4); \s is rubyregexp.Space because RE2's lacks \v, and \d and \w are
// ASCII in Ruby, as written here.
var (
	v4Pattern = regexp.MustCompile(`(?m)^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$`) // type:15
	// v6Pattern is type:22, which RE2 takes as is once \s is spelled out and
	// \d is [0-9].
	v6Pattern      = regexp.MustCompile(`(?m)^` + rubyregexp.Space + `*((([0-9A-Fa-f]{1,4}:){7}([0-9A-Fa-f]{1,4}|:))|(([0-9A-Fa-f]{1,4}:){6}(:[0-9A-Fa-f]{1,4}|((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3})|:))|(([0-9A-Fa-f]{1,4}:){5}(((:[0-9A-Fa-f]{1,4}){1,2})|:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3})|:))|(([0-9A-Fa-f]{1,4}:){4}(((:[0-9A-Fa-f]{1,4}){1,3})|((:[0-9A-Fa-f]{1,4})?:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}))|:))|(([0-9A-Fa-f]{1,4}:){3}(((:[0-9A-Fa-f]{1,4}){1,4})|((:[0-9A-Fa-f]{1,4}){0,2}:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}))|:))|(([0-9A-Fa-f]{1,4}:){2}(((:[0-9A-Fa-f]{1,4}){1,5})|((:[0-9A-Fa-f]{1,4}){0,3}:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}))|:))|(([0-9A-Fa-f]{1,4}:){1}(((:[0-9A-Fa-f]{1,4}){1,6})|((:[0-9A-Fa-f]{1,4}){0,4}:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}))|:))|(:(((:[0-9A-Fa-f]{1,4}){1,7})|((:[0-9A-Fa-f]{1,4}){0,5}:((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])(\.(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])){3}))|:)))(%.+)?` + rubyregexp.Space + `*$`)
	whitespace     = regexp.MustCompile(`(?m)` + rubyregexp.Space)         // type:50
	blankLine      = regexp.MustCompile(`(?m)^` + rubyregexp.Space + `*$`) // type:51
	hostnamePartRE = regexp.MustCompile(`(?m)^([\w]+|[\w][\w\-]+[\w])$`)   // type:84
)

// hostParams is a Host resource munged as type/host.rb munges it. Ensure is ""
// when the resource manages nothing. Should holds exactly the properties that
// have a should value, the target default included: ip, comment and target a
// Str, host_aliases a List (empty for `[]`, which deletes every alias).
type hostParams struct {
	Name, Title string
	Ensure      string
	Should      map[string]parsedfile.Value
}

// Validate implements ral.Validating: the first failure of parseHost.
func (h Host) Validate(r catalog.Resource) error {
	_, err := parseHost(r, h.encoding)
	return err
}

// parseHost munges r and reports the first failure in Ruby's allattrs order
// (type.rb:2441-2463): name, provider, then the properties of hostProperties.
// A value shape the native parser does not implement is a
// *rubyvalue.UnimplementedValue (spec §2 condition 6), found at its place in
// that order. encoding quotes values in refusal texts (spec §4).
func parseHost(r catalog.Resource, encoding string) (hostParams, error) {
	h := hostParser{r: r, encoding: encoding}
	p := hostParams{Name: r.Title, Title: r.Title, Should: map[string]parsedfile.Value{}}

	if err := h.name(&p); err != nil {
		return p, err
	}
	ensureSet, err := h.ensure(&p)
	if err != nil {
		return p, err
	}
	for _, step := range []func(*hostParams) error{h.ip, h.hostAliases, h.comment} {
		if err := step(&p); err != nil {
			return p, err
		}
	}
	targetSet, err := h.target(&p)
	if err != nil {
		return p, err
	}

	// Defaults, as Puppet::Type#set_parameters applies them after every given
	// attribute is set. ensure's is decided before target's, so only given
	// properties make the resource managed (Type#managed?).
	if !ensureSet && len(p.Should) > 0 {
		p.Ensure = "present"
	}
	if !targetSet {
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: defaultTarget}
	}
	return p, nil
}

// hostParser holds what every attribute's checks need.
type hostParser struct {
	r        catalog.Resource
	encoding string
}

// elements is an attribute's value as the list Puppet::Property#should= makes
// of it; set is false for an unset or undef attribute.
func (h hostParser) elements(attr string) (elems []any, set bool) {
	switch v := h.r.Parameters[attr].(type) {
	case nil:
		return nil, false
	case []any:
		return v, true
	default:
		return []any{v}, true
	}
}

// inspect is Ruby's inspect of a String under h.encoding, [redacted] for a
// Sensitive attribute (D-78).
func (h hostParser) inspect(attr, s string) string {
	return rubyvalue.RedactIf(h.sensitive(attr), "%s", rubyvalue.InspectIn(s, h.encoding))
}

func (h hostParser) sensitive(attr string) bool {
	for _, a := range h.r.SensitiveParameters {
		if a == attr {
			return true
		}
	}
	return false
}

func (h hostParser) unimplemented(attr string, v any) error {
	return &rubyvalue.UnimplementedValue{Ref: h.r.Ref(), Attr: attr, Shown: rubyvalue.ShownValue(h.r, attr, v)}
}

// strings is every element of attr as a String; a non-String, nil included, is
// delegated: Ruby's validate calls a String method on it and the
// NoMethodError text is Ruby-version-specific (spec §2 condition 6).
func (h hostParser) strings(attr string) (values []string, set bool, err error) {
	elems, set := h.elements(attr)
	for _, v := range elems {
		s, ok := v.(string)
		if !ok {
			return nil, set, h.unimplemented(attr, v)
		}
		values = append(values, s)
	}
	return values, set, nil
}

// name is the namevar (type:77-92): the explicit name parameter, else the title.
// Each part of the name split on '.' must be a word, then the whole may hold
// no line break.
func (h hostParser) name(p *hostParams) error {
	if v := h.r.Parameters["name"]; v != nil {
		s, ok := v.(string)
		if !ok {
			return h.unimplemented("name", v)
		}
		p.Name = s
	}
	for _, part := range splitDots(p.Name) {
		if !hostnamePartRE.MatchString(part) {
			return rubyvalue.ParamFailure(h.r, "name", "Invalid host name")
		}
	}
	if strings.ContainsAny(p.Name, "\n\r") {
		return rubyvalue.ParamFailure(h.r, "name", "Hostname cannot include newline")
	}
	return nil
}

// splitDots is String#split('.'): empty fields are kept except trailing ones,
// so "a." is ["a"], "a..b" is ["a", "", "b"] and "..." is [].
func splitDots(s string) []string {
	parts := strings.Split(s, ".")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// ensure sets p.Ensure from the ensure property (ensurable: present, absent);
// set reports whether the catalog gave one, which stops the default. A value
// that is not a String is delegated, as the text Ruby shows for it is not
// reproduced here.
func (h hostParser) ensure(p *hostParams) (set bool, err error) {
	values, set, err := h.strings("ensure")
	if err != nil {
		return set, err
	}
	for i, s := range values {
		if s != "present" && s != "absent" {
			return set, rubyvalue.ParamFailure(h.r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", h.inspect("ensure", s)))
		}
		if i == 0 {
			p.Ensure = s
		}
	}
	return set, nil
}

// ip is the ip property (type:11-33): a dotted quad with every octet at most
// 255, or an IPv6 address, and no line break in either.
func (h hostParser) ip(p *hostParams) error {
	values, _, err := h.strings("ip")
	if err != nil {
		return err
	}
	for i, s := range values {
		if !validV4(s) && !v6Pattern.MatchString(s) || strings.ContainsAny(s, "\n\r") {
			return rubyvalue.ParamFailure(h.r, "ip", "Invalid IP address "+h.inspect("ip", s))
		}
		if i == 0 {
			p.Should["ip"] = parsedfile.Value{Kind: parsedfile.Str, S: s}
		}
	}
	return nil
}

// validV4 is valid_v4? (type:14-17): the pattern's first match has four
// octets no greater than 255.
func validV4(s string) bool {
	m := v4Pattern.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	for _, octet := range m[1:] {
		if n, _ := strconv.Atoi(octet); n > 255 {
			return false
		}
	}
	return true
}

// hostAliases is the host_aliases property (type:36-53), an OrderedList whose
// every element is one alias: a String given whole is one element, so
// 'a  b' is refused, not split. should is the list of elements, empty for `[]`.
func (h hostParser) hostAliases(p *hostParams) error {
	values, set, err := h.strings("host_aliases")
	if err != nil {
		return err
	}
	for _, s := range values {
		if whitespace.MatchString(s) {
			return rubyvalue.ParamFailure(h.r, "host_aliases", "Host aliases cannot include whitespace")
		}
		if blankLine.MatchString(s) {
			return rubyvalue.ParamFailure(h.r, "host_aliases", "Host aliases cannot be an empty string. Use an empty array to delete all host_aliases ")
		}
	}
	if set {
		p.Should["host_aliases"] = parsedfile.Value{Kind: parsedfile.List, L: values}
	}
	return nil
}

// comment is the comment property (type:55-62): any String without a line break.
func (h hostParser) comment(p *hostParams) error {
	values, _, err := h.strings("comment")
	if err != nil {
		return err
	}
	for i, s := range values {
		if strings.ContainsAny(s, "\n\r") {
			return rubyvalue.ParamFailure(h.r, "comment", "Comment cannot include newline")
		}
		if i == 0 {
			p.Should["comment"] = parsedfile.Value{Kind: parsedfile.Str, S: s}
		}
	}
	return nil
}

// target is the target property (type:64-75): a String path, the first of a
// list. set reports whether the catalog gave one, which stops the default. A
// non-String is delegated.
func (h hostParser) target(p *hostParams) (set bool, err error) {
	values, set, err := h.strings("target")
	if err != nil {
		return set, err
	}
	if len(values) > 0 {
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
	}
	return set, nil
}
