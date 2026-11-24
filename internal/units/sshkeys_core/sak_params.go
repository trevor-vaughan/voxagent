package sshkeyscore

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// sakTypes are the type property's accepted values (kt:67-70), in Ruby's
// order, which is the order the refusal lists them. The aliases of
// typeAliases (kt:72-76, the same five as sshkey's) are accepted too and not
// listed.
var sakTypes = []string{
	"ssh-dss", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "ssh-ed25519",
	"sk-ecdsa-sha2-nistp256@openssh.com", "sk-ssh-ed25519@openssh.com", "ssh-rsa-cert-v01@openssh.com",
	"ssh-ed25519-cert-v01@openssh.com", "ssh-dss-cert-v01@openssh.com", "ecdsa-sha2-nistp256-cert-v01@openssh.com",
	"ecdsa-sha2-nistp384-cert-v01@openssh.com", "ecdsa-sha2-nistp521-cert-v01@openssh.com",
}

// sakOption is an options element's pattern (kt:144). Ruby's ^ and $ are line
// anchors, so a value with a line break passes when any line matches (spec §8
// item 16).
var sakOption = regexp.MustCompile(`(?m)^[-a-z0-9A-Z_]+(?:=".*?")?$`)

// sakParams is a Sak resource munged as kt munges it. Title is the catalog's
// (Type#title, which sak does not override), Name the name parameter or else
// the title. Ensure is ""
// when the resource manages nothing. Should holds the properties that have a
// should value: type and key a Str, target a Str (a Nil for `target => []`,
// which satisfies the type-level check and has no value; unset, the target is
// computed from User when it is asked for, kt:109-120) and options a List,
// Absent unless the catalog gives some. user is not a record field (spec
// §6.3), so User holds it, and UserGiven says it was given: an empty user is a
// user to Ruby (should(:user) is truthy), whose uid lookup then fails.
type sakParams struct {
	Name, Title, Ensure, User string
	UserGiven                 bool
	DropPrivileges            bool
	Should                    map[string]parsedfile.Value
}

// Validate implements ral.Validating: the first failure of parseSak.
func (s Sak) Validate(r catalog.Resource) error {
	_, err := parseSak(r, s.release, s.encoding)
	return err
}

// sakParser holds what every attribute's checks need. It borrows the sshkey
// parser's value readers.
type sakParser struct{ sshkeyParser }

// parseSak munges r and reports the first failure in Ruby's allattrs order
// (type.rb:2441-2463): the namevar name, then the properties of sakProperties,
// then the parameter drop_privileges, and last the type-level validate
// (kt:157-166). release is accepted as Sshkey's is: 2.5.0 and 3.1.0 validate
// every attribute alike. A value shape the native parser does not implement is
// a *rubyvalue.UnimplementedValue (spec §2 condition 6), found at its place in
// that order. encoding quotes values in refusal texts (spec §4).
func parseSak(r catalog.Resource, _, encoding string) (sakParams, error) {
	s := sakParser{sshkeyParser{r: r, encoding: encoding}}
	var err error
	p := sakParams{Title: r.Title, DropPrivileges: true, Should: map[string]parsedfile.Value{}}

	if err := s.nameparam(&p); err != nil {
		return p, err
	}
	var ensureSet bool
	p.Ensure, ensureSet, err = s.ensureValue(s.r)
	if err != nil {
		return p, err
	}

	// managed counts the properties Type#managed? sees when ensure's default is
	// decided: the given ones with a should value. Defaults come after.
	managed := false
	for _, step := range []func(*sakParams) (hasShould bool, err error){s.typ, s.key, s.user, s.target, s.options} {
		hasShould, err := step(&p)
		if err != nil {
			return p, err
		}
		managed = managed || hasShould
	}
	if err := s.dropPrivileges(&p); err != nil {
		return p, err
	}

	// kt:159-163: a given target passes, even `target => []`; else user must be.
	_, targetGiven := p.Should["target"]
	_, userGiven := s.elements("user")
	if !targetGiven && !userGiven {
		return p, fmt.Errorf("Validation of %s failed: Attribute 'user' or 'target' is mandatory", s.r.Ref())
	}
	if !ensureSet && managed {
		p.Ensure = "present"
	}
	if _, ok := p.Should["options"]; !ok {
		p.Should["options"] = parsedfile.Value{Kind: parsedfile.Absent}
	}
	return p, nil
}

// nameparam is the name namevar (kt:44-53): any String, the title when the
// catalog gives none (the default title pattern takes the whole title). Title
// stays the catalog's: refusals name `Ssh_authorized_key[<title>]` even when
// name differs (oracle, 17h-sak-title-vs-name).
func (s sakParser) nameparam(p *sakParams) error {
	p.Name = s.r.Title
	if v := s.r.Parameters["name"]; v != nil {
		str, ok := v.(string)
		if !ok {
			return s.unimplemented("name", v)
		}
		p.Name = str
	}
	return nil
}

// typ is the type property (kt:64-77): one of sakTypes or an alias, which
// munges to its target. Each element is validated, the first is the should.
func (s sakParser) typ(p *sakParams) (hasShould bool, err error) {
	values, _, err := s.strings("type")
	if err != nil {
		return false, err
	}
	for i, v := range values {
		target, isAlias := typeAliases[v]
		if !isAlias && !slices.Contains(sakTypes, v) {
			return false, rubyvalue.ParamFailure(s.r, "type", fmt.Sprintf("Invalid value %s. Valid values are %s.",
				s.inspect("type", v), strings.Join(sakTypes, ", ")))
		}
		if i == 0 {
			if isAlias {
				v = target
			}
			p.Should["type"] = parsedfile.Value{Kind: parsedfile.Str, S: v}
		}
	}
	return len(values) > 0, nil
}

// key is the key property (kt:79-93): any String without whitespace, in both
// releases. The refusal interpolates the value as it is, not inspected.
func (s sakParser) key(p *sakParams) (hasShould bool, err error) {
	values, _, err := s.strings("key")
	if err != nil {
		return false, err
	}
	for i, v := range values {
		if whitespace.MatchString(v) {
			return false, rubyvalue.ParamFailure(s.r, "key", "Key must not contain whitespace: "+s.raw("key", v))
		}
		if i == 0 {
			p.Should["key"] = parsedfile.Value{Kind: parsedfile.Str, S: v}
		}
	}
	return len(values) > 0, nil
}

// user is the user property (kt:95-98): any String, the first of a list.
func (s sakParser) user(p *sakParams) (hasShould bool, err error) {
	values, _, err := s.strings("user")
	if err != nil || len(values) == 0 {
		return false, err
	}
	p.User, p.UserGiven = values[0], true
	return true, nil
}

// target is the target property (kt:100-125): a String path, the first of a
// list. "absent" is a path like any other here: Ruby compares the String with
// the Symbol :absent, which differ. Only an empty list leaves a Nil.
func (s sakParser) target(p *sakParams) (hasShould bool, err error) {
	values, set, err := s.strings("target")
	switch {
	case err != nil || !set:
		return false, err
	case len(values) == 0:
		p.Should["target"] = parsedfile.Value{Kind: parsedfile.Nil}
		return false, nil
	}
	p.Should["target"] = parsedfile.Value{Kind: parsedfile.Str, S: values[0]}
	return true, nil
}

// options is the options property (kt:127-151, array_matching all): every
// element is one option. Given, even as [], it has a should.
func (s sakParser) options(p *sakParams) (hasShould bool, err error) {
	values, set, err := s.strings("options")
	if err != nil || !set {
		return false, err
	}
	for _, v := range values {
		if !sakOption.MatchString(v) {
			return false, rubyvalue.ParamFailure(s.r, "options", fmt.Sprintf("Option %s is not valid. A single option must either be of the form 'option' or 'option=\"value\"'. Multiple options must be provided as an array", s.raw("options", v)))
		}
	}
	p.Should["options"] = parsedfile.Value{Kind: parsedfile.List, L: append([]string{}, values...)}
	return true, nil
}

// dropPrivileges is the drop_privileges parameter (kt:55-62), a
// Puppet::Parameter::Boolean: validated against the four strings (and the
// booleans, which convert to them) before it is coerced; default true.
func (s sakParser) dropPrivileges(p *sakParams) error {
	v := s.r.Parameters["drop_privileges"]
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case bool:
		p.DropPrivileges = t
		return nil
	case string:
		switch t {
		case "true", "yes":
			p.DropPrivileges = true
			return nil
		case "false", "no":
			p.DropPrivileges = false
			return nil
		}
	}
	if rubyvalue.HoldsHash(v) {
		return s.unimplemented("drop_privileges", v)
	}
	shown := rubyvalue.ShownValue(s.r, "drop_privileges", v)
	if str, ok := v.(string); ok {
		shown = s.inspect("drop_privileges", str)
	}
	return rubyvalue.ParamFailure(s.r, "drop_privileges", fmt.Sprintf("Invalid value %s. Valid values are true, false, yes, no.", shown))
}

// raw is a value interpolated into a message as it is, [redacted] for a
// Sensitive attribute (D-78).
func (s sakParser) raw(attr, v string) string {
	return rubyvalue.RedactIf(slices.Contains(s.r.SensitiveParameters, attr), "%s", v)
}
