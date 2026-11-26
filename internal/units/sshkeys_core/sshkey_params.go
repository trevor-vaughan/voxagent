package sshkeyscore

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// defaultTarget is the parsed provider's default_target (sp:49-50).
const defaultTarget = "/etc/ssh/ssh_known_hosts"

// release31 is the sshkeys_core release whose key property validates (3.1.0
// type:63-65).
const release31 = "3.1.0"

// The type's patterns. Ruby's ^ and $ are line anchors, so every one is (?m)
// (spec §4); \s is rubyregexp.Space because RE2's lacks \v.
var (
	whitespace = regexp.MustCompile(`(?m)` + rubyregexp.Space) // type:63-65, 85, 100
	// The title patterns of type:21-37; the first that matches names the
	// title's parameters.
	titleNameType = regexp.MustCompile(`(?m)^(.*?)@(.*)$`)
	titleName     = regexp.MustCompile(`(?m)^([^@]+)$`)
)

// sshkeyTypes are type's accepted values (type:44-45), in Ruby's order, which
// is the order the refusal lists them.
var sshkeyTypes = []string{
	"ssh-dss", "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	"sk-ecdsa-sha2-nistp256@openssh.com", "sk-ssh-ed25519@openssh.com",
}

// typeAliases are the values aliasvalue maps to a target (type:47-51).
var typeAliases = map[string]string{
	"dsa":        "ssh-dss",
	"ed25519":    "ssh-ed25519",
	"rsa":        "ssh-rsa",
	"ecdsa-sk":   "sk-ecdsa-sha2-nistp256@openssh.com",
	"ed25519-sk": "sk-ssh-ed25519@openssh.com",
}

// sshkeyParams is a Sshkey resource munged as type/sshkey.rb munges it. Type
// is the munged type, "" when the resource has none, and Title Ruby's
// "<name>@<type>" (type:11-15). Ensure is "" when the resource manages
// nothing. Should holds exactly the properties that have a should value, the
// target default included: key and target a Str, host_aliases a List (empty
// for `[]`, which deletes every alias).
type sshkeyParams struct {
	Name, Type, Title, Ensure string
	Should                    map[string]parsedfile.Value
}

// Validate implements ral.Validating: the first failure of parseSshkey.
func (s Sshkey) Validate(r catalog.Resource) error {
	_, err := parseSshkey(r, s.release, s.encoding)
	return err
}

// parseSshkey munges r and reports the first failure in Ruby's allattrs order
// (type.rb:2441-2463): the namevars type then name, provider, then the
// properties of sshkeyProperties. release is the sshkeys_core release, "2.5.0"
// or "3.1.0". A value shape the native parser does not implement is a
// *rubyvalue.UnimplementedValue (spec §2 condition 6), found at its place in
// that order. encoding quotes values in refusal texts (spec §4).
//
// A refusal names the resource as Ruby has built it so far (M7): the name and
// type that are already set, the others empty.
func parseSshkey(r catalog.Resource, release, encoding string) (sshkeyParams, error) {
	s := sshkeyParser{r: r, release: release, encoding: encoding}
	p := sshkeyParams{Should: map[string]parsedfile.Value{}}

	if err := s.typ(&p); err != nil {
		return p, err
	}
	if err := s.name(&p); err != nil {
		return p, err
	}
	ensureSet, err := s.ensure(&p)
	if err != nil {
		return p, err
	}
	for _, step := range []func(*sshkeyParams) error{s.key, s.hostAliases} {
		if err := step(&p); err != nil {
			return p, err
		}
	}
	targetSet, err := s.target(&p)
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
	p.Title = p.Name + "@" + p.Type
	return p, nil
}

// sshkeyParser holds what every attribute's checks need.
type sshkeyParser struct {
	r                 catalog.Resource
	release, encoding string
}

// failure is ParamFailure naming the resource as built so far in p.
func (s sshkeyParser) failure(p *sshkeyParams, attr, text string) error {
	built := s.r
	built.Title = p.Name + "@" + p.Type
	return rubyvalue.ParamFailure(built, attr, text)
}

// elements is an attribute's value as the list Puppet::Property#should= makes
// of it; set is false for an unset or undef attribute.
func (s sshkeyParser) elements(attr string) (elems []any, set bool) {
	switch v := s.r.Parameters[attr].(type) {
	case nil:
		return nil, false
	case []any:
		return v, true
	default:
		return []any{v}, true
	}
}

// inspect is Ruby's inspect of a String under s.encoding, [redacted] for a
// Sensitive attribute (D-78).
func (s sshkeyParser) inspect(attr, v string) string {
	return rubyvalue.RedactIf(slices.Contains(s.r.SensitiveParameters, attr), "%s", rubyvalue.InspectIn(v, s.encoding))
}

func (s sshkeyParser) unimplemented(attr string, v any) error {
	return &rubyvalue.UnimplementedValue{Ref: s.r.Ref(), Attr: attr, Shown: rubyvalue.ShownValue(s.r, attr, v)}
}

// strings is every element of attr as a String; a non-String, nil included, is
// delegated: Ruby's validate calls a String method on it and the
// NoMethodError text is Ruby-version-specific (spec §2 condition 6).
func (s sshkeyParser) strings(attr string) (values []string, set bool, err error) {
	elems, set := s.elements(attr)
	for _, v := range elems {
		str, ok := v.(string)
		if !ok {
			return nil, set, s.unimplemented(attr, v)
		}
		values = append(values, str)
	}
	return values, set, nil
}

// namevar is what the namevar attr is given: its parameter, else the part of
// the title the title patterns assign it (Puppet::Resource#to_hash merges the
// parameters over the parsed title). set is false when neither gives one.
func (s sshkeyParser) namevar(attr string) (value string, set bool, err error) {
	if v := s.r.Parameters[attr]; v != nil {
		str, ok := v.(string)
		if !ok {
			return "", true, s.unimplemented(attr, v)
		}
		return str, true, nil
	}
	if m := titleNameType.FindStringSubmatch(s.r.Title); m != nil {
		return map[string]string{"name": m[1], "type": m[2]}[attr], true, nil
	}
	if m := titleName.FindStringSubmatch(s.r.Title); m != nil && attr == "name" {
		return m[1], true, nil
	}
	return "", false, nil
}

// typ is the type namevar (type:39-52): one of sshkeyTypes or an alias, which
// munges to its target. A resource without one is not refused (M7).
func (s sshkeyParser) typ(p *sshkeyParams) error {
	v, set, err := s.namevar("type")
	if err != nil || !set {
		return err
	}
	if target, ok := typeAliases[v]; ok {
		v = target
	}
	if !slices.Contains(sshkeyTypes, v) {
		return s.failure(p, "type", fmt.Sprintf("Invalid value %s. Valid values are %s.",
			s.inspect("type", v), strings.Join(sshkeyTypes, ", ")))
	}
	p.Type = v
	return nil
}

// name is the name namevar (type:94-103): no whitespace, no comma.
func (s sshkeyParser) name(p *sshkeyParams) error {
	v, _, err := s.namevar("name")
	if err != nil {
		return err
	}
	if whitespace.MatchString(v) {
		return s.failure(p, "name", "Resourcename cannot include whitespaces")
	}
	if strings.Contains(v, ",") {
		return s.failure(p, "name", "No comma in resourcename allowed. If you want to specify aliases use the host_aliases property")
	}
	p.Name = v
	return nil
}

// ensure sets p.Ensure from the ensure property (ensurable: present, absent);
// set reports whether the catalog gave one, which stops the default.
func (s sshkeyParser) ensure(p *sshkeyParams) (set bool, err error) {
	built := s.r
	built.Title = p.Name + "@" + p.Type
	p.Ensure, set, err = s.ensureValue(built)
	return set, err
}

// ensureValue is ensurable's value check, shared with ssh_authorized_key:
// the first element of the property, set when the catalog gave one. built
// names the resource in a refusal. A value that is not a String is delegated,
// as the text Ruby shows for it is not reproduced here.
func (s sshkeyParser) ensureValue(built catalog.Resource) (first string, set bool, err error) {
	values, set, err := s.strings("ensure")
	if err != nil {
		return "", set, err
	}
	for i, v := range values {
		if v != "present" && v != "absent" {
			return "", set, rubyvalue.ParamFailure(built, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", s.inspect("ensure", v)))
		}
		if i == 0 {
			first = v
		}
	}
	return first, set, nil
}

// key is the key property (type:54-64): any String, and from release 3.1.0 one
// without whitespace.
func (s sshkeyParser) key(p *sshkeyParams) error {
	values, _, err := s.strings("key")
	if err != nil {
		return err
	}
	for i, v := range values {
		if s.release == release31 && whitespace.MatchString(v) {
			return s.failure(p, "key", "Key must not contain whitespace: "+s.inspect("key", v))
		}
		if i == 0 {
			p.Should["key"] = parsedfile.Value{Kind: parsedfile.Str, S: v}
		}
	}
	return nil
}

// hostAliases is the host_aliases property (type:66-92): every element is one
// alias, so a String given whole is one element and 'a b' is refused, not
// split. should is the whole list, empty for `[]`.
func (s sshkeyParser) hostAliases(p *sshkeyParams) error {
	values, set, err := s.strings("host_aliases")
	if err != nil {
		return err
	}
	for _, v := range values {
		if whitespace.MatchString(v) {
			return s.failure(p, "host_aliases", "Aliases cannot include whitespace")
		}
		if strings.Contains(v, ",") {
			return s.failure(p, "host_aliases", "Aliases must be provided as an array, not a comma-separated list")
		}
	}
	if set {
		p.Should["host_aliases"] = parsedfile.Value{Kind: parsedfile.List, L: values}
	}
	return nil
}

// target is the target property (type:105-116): a String path, the first of a
// list. set reports whether the catalog gave one, which stops the default. A
// non-String is delegated.
func (s sshkeyParser) target(p *sshkeyParams) (set bool, err error) {
	values, set, err := s.strings("target")
	if err != nil {
		return set, err
	}
	if len(values) > 0 {
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
	}
	return set, nil
}
