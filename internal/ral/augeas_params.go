package ral

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
)

// The four augeasproviders types (sysctl, ssh_config, sshd_config,
// sshd_config_subsystem) parse their parameters here. A value Ruby refuses is
// an error carrying Ruby's text (spec §3); a value Ruby accepts but native
// does not implement (spec §2 (d)-(f)) is an *unimplementedValue, which sends
// the type to the extension host.

// sysctlParams is a Sysctl as type/sysctl.rb munges it.
type sysctlParams struct {
	Name, Target, Value string
	HasValue            bool
	Apply, Silent       bool
	EnsureSet           bool // ensure => present was declared
}

// sshConfigParams is an Ssh_config as type/ssh_config.rb munges it. Ensure is
// as declared: "" with a Value means present, "" without one manages nothing.
type sshConfigParams struct {
	Name, Key, Host, Target string
	Value                   []string
	Ensure                  string // "", "present", "absent"
}

// sshdConfigParams is an Sshd_config as type/sshd_config.rb munges it; Ensure
// is as for sshConfigParams.
type sshdConfigParams struct {
	Name, Key, Target string
	Value             []string
	Ensure            string // "", "present", "absent"
	ArrayAppend       bool
	Condition         []condPair
}

// condPair is one entry of sshd_config's condition Hash.
type condPair struct{ Key, Value string }

// subsystemParams is an Sshd_config_subsystem as type/sshd_config_subsystem.rb
// munges it; Ensure is as for sshConfigParams.
type subsystemParams struct {
	Name, Target, Command string
	HasCommand            bool
	Ensure                string // "", "present", "absent"
}

var (
	// sysctlTitle is §2 (d): dot-separated segments, none starting with a
	// digit, `-` or `.`.
	sysctlTitle = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z_][A-Za-z0-9_-]*)*$`)
	sshKeyName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	subsysTitle = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	// targetSegment is a path segment of a native target.
	targetSegment = regexp.MustCompile(`^[A-Za-z0-9._+@-]+$`)
)

// rubySplit is String#split with no argument: runs of ASCII space, \t, \n,
// \v, \f and \r separate the fields, and leading ones are ignored.
func rubySplit(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			return true
		}
		return false
	})
}

// augeasText reports whether s is valid UTF-8 without a control character.
func augeasText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// augeasUnimplemented is the refusal to handle parameter name natively, for
// the value the catalog gives it.
func augeasUnimplemented(r catalog.Resource, name string) error {
	return &unimplementedValue{r.Ref(), name, shownValue(r, name, r.Parameters[name])}
}

// augeasPrelude is what every attribute of the four types is subject to: a
// rich value or a Sensitive mark anywhere, a name parameter that is not the
// title, and a provider other than augeas.
func augeasPrelude(r catalog.Resource) error {
	if err := concatRich(r); err != nil {
		return err
	}
	if len(r.SensitiveParameters) > 0 {
		return &unimplementedValue{r.Ref(), r.SensitiveParameters[0], "[redacted]"}
	}
	if v := r.Parameters["name"]; v != nil && v != r.Title {
		return augeasUnimplemented(r, "name")
	}
	if v := r.Parameters["provider"]; v != nil && v != "augeas" {
		return augeasUnimplemented(r, "provider")
	}
	return nil
}

// augeasEnsure reads ensure: "" when unset. A scalar that is no ensure value
// is Ruby's refusal; an Array or Hash is validated element-wise by Ruby, which
// native does not port.
func augeasEnsure(r catalog.Resource) (string, error) {
	switch v := r.Parameters["ensure"]; v {
	case nil:
		return "", nil
	case "present", "absent":
		return v.(string), nil
	default:
		switch v.(type) {
		case []any, map[string]any:
			return "", augeasUnimplemented(r, "ensure")
		}
		return "", paramFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", shownValue(r, "ensure", v)))
	}
}

// augeasBool reads a newvalues(:true, :false) parameter, which takes only
// true, false and those two strings, case-sensitively. valid is the list the
// refusal names, in the type's order. def is the value when unset.
func augeasBool(r catalog.Resource, name, valid string, def bool) (bool, error) {
	switch v := r.Parameters[name]; v {
	case nil:
		return def, nil
	case true, "true":
		return true, nil
	case false, "false":
		return false, nil
	default:
		if holdsHash(v) {
			return false, augeasUnimplemented(r, name)
		}
		return false, paramFailure(r, name, fmt.Sprintf("Invalid value %s. Valid values are %s.", shownValue(r, name, v), valid))
	}
}

// augeasString reads a parameter native takes as a String: set is false when
// unset, any other shape or an unusable string is unimplemented.
func augeasString(r catalog.Resource, name string) (s string, set bool, err error) {
	switch v := r.Parameters[name].(type) {
	case nil:
		return "", false, nil
	case string:
		if augeasText(v) {
			return v, true, nil
		}
	}
	return "", false, augeasUnimplemented(r, name)
}

// augeasScalar is a String or an Integer of magnitude below 2^53 as Ruby's
// to_s renders it; ok is false for any other value or an unusable string.
func augeasScalar(v any) (s string, ok bool) {
	switch t := v.(type) {
	case string:
		return t, augeasText(t)
	case int:
		return augeasScalar(float64(t))
	case int64:
		return augeasScalar(float64(t))
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return strconv.FormatInt(int64(t), 10), true
		}
	}
	return "", false
}

// augeasValues reads a value native takes as a String or a non-empty Array of
// them; integers admits Integers too (sshd_config munges them with to_s). set
// is false when unset.
func augeasValues(r catalog.Resource, name string, integers bool) (vals []string, set bool, err error) {
	one := func(v any) (string, bool) {
		if _, isString := v.(string); !isString && !integers {
			return "", false
		}
		return augeasScalar(v)
	}
	switch v := r.Parameters[name].(type) {
	case nil:
		return nil, false, nil
	case []any:
		if len(v) == 0 {
			break
		}
		vals = make([]string, len(v))
		ok := true
		for i, e := range v {
			var good bool
			if vals[i], good = one(e); !good {
				ok = false
			}
		}
		if ok {
			return vals, true, nil
		}
	default:
		if s, ok := one(v); ok {
			return []string{s}, true, nil
		}
	}
	return nil, false, augeasUnimplemented(r, name)
}

// augeasTarget reads target: "" when unset. Native takes a clean absolute path
// of the characters §2 (d) lists, anything else is unimplemented. For
// ssh_config a String that is not absolute is Ruby's refusal instead.
func augeasTarget(r catalog.Resource, refuseRelative bool) (string, error) {
	s, set, err := augeasString(r, "target")
	if err != nil || !set {
		return "", err
	}
	if refuseRelative && !strings.HasPrefix(s, "/") {
		return "", paramFailure(r, "target", "target must be an absolute path")
	}
	if strings.HasPrefix(s, "/") && concatPathClean(s) {
		clean := true
		for _, seg := range strings.Split(s[1:], "/") {
			clean = clean && targetSegment.MatchString(seg)
		}
		if clean {
			return s, nil
		}
	}
	return "", augeasUnimplemented(r, "target")
}

// augeasComment refuses comment, which native does not manage.
func augeasComment(r catalog.Resource) error {
	if r.Parameters["comment"] != nil {
		return augeasUnimplemented(r, "comment")
	}
	return nil
}

// augeasKey is the setting name of ssh_config and sshd_config: the key
// parameter, else the title.
func augeasKey(r catalog.Resource) (string, error) {
	key, set, err := augeasString(r, "key")
	if err != nil {
		return "", err
	}
	if !set {
		key = r.Title
	}
	if !sshKeyName.MatchString(key) {
		if set {
			return "", augeasUnimplemented(r, "key")
		}
		return "", augeasUnimplemented(r, "name")
	}
	return key, nil
}

func parseSysctl(r catalog.Resource) (sysctlParams, error) {
	p := sysctlParams{Name: r.Title}
	if err := augeasPrelude(r); err != nil {
		return p, err
	}
	if !sysctlTitle.MatchString(r.Title) {
		return p, augeasUnimplemented(r, "name")
	}
	ensure, err := augeasEnsure(r)
	if err != nil {
		return p, err
	}
	if ensure == "absent" {
		return p, augeasUnimplemented(r, "ensure")
	}
	p.EnsureSet = ensure == "present"
	if r.Parameters["val"] != nil {
		return p, augeasUnimplemented(r, "val")
	}
	if v := r.Parameters["value"]; v != nil {
		s, ok := augeasScalar(v)
		if !ok {
			return p, augeasUnimplemented(r, "value")
		}
		p.Value, p.HasValue = s, true
	}
	if err := augeasComment(r); err != nil {
		return p, err
	}
	if p.Target, err = augeasTarget(r, false); err != nil {
		return p, err
	}
	if p.Apply, err = augeasBool(r, "apply", "true, false", true); err != nil {
		return p, err
	}
	persist, err := augeasBool(r, "persist", "true, false", true)
	if err != nil {
		return p, err
	}
	if !persist {
		return p, augeasUnimplemented(r, "persist")
	}
	p.Silent, err = augeasBool(r, "silent", "true, false", false)
	return p, err
}

func parseSshConfig(r catalog.Resource) (sshConfigParams, error) {
	p := sshConfigParams{Name: r.Title, Host: "*"}
	if err := augeasPrelude(r); err != nil {
		return p, err
	}
	var err error
	if p.Ensure, err = augeasEnsure(r); err != nil {
		return p, err
	}
	var hasValue bool
	if p.Value, hasValue, err = augeasValues(r, "value", false); err != nil {
		return p, err
	}
	if p.Ensure == "present" && !hasValue {
		return p, augeasUnimplemented(r, "ensure")
	}
	if err := augeasComment(r); err != nil {
		return p, err
	}
	if p.Key, err = augeasKey(r); err != nil {
		return p, err
	}
	if p.Target, err = augeasTarget(r, true); err != nil {
		return p, err
	}
	if host, set, err := augeasString(r, "host"); err != nil {
		return p, err
	} else if set {
		if strings.ContainsAny(host, `'"\\`) {
			return p, augeasUnimplemented(r, "host")
		}
		p.Host = host
	}
	return p, nil
}

func parseSshdConfig(r catalog.Resource) (sshdConfigParams, error) {
	p := sshdConfigParams{Name: r.Title}
	if err := augeasPrelude(r); err != nil {
		return p, err
	}
	var err error
	if p.Ensure, err = augeasEnsure(r); err != nil {
		return p, err
	}
	var hasValue bool
	if p.Value, hasValue, err = augeasValues(r, "value", true); err != nil {
		return p, err
	}
	if p.Ensure == "present" && !hasValue {
		return p, augeasUnimplemented(r, "ensure")
	}
	if err := augeasComment(r); err != nil {
		return p, err
	}
	if p.Key, err = augeasKey(r); err != nil {
		return p, err
	}
	if p.ArrayAppend, err = augeasBool(r, "array_append", "false, true", false); err != nil {
		return p, err
	}
	if p.Target, err = augeasTarget(r, false); err != nil {
		return p, err
	}
	if r.Parameters["condition"] != nil {
		if p.Condition, err = parseCondition(r); err != nil {
			return p, err
		}
	}
	return p, nil
}

// conditionValue is a condition value: no quote, backslash, bracket, slash or
// control character (§2 (d)).
var conditionValue = regexp.MustCompile(`^[^'"\\\[\]/]+$`)

// parseCondition reads condition as a String of key and value tokens, the
// Hash[*tokens] of type/sshd_config.rb: a repeated key keeps its first position
// and its last value.
func parseCondition(r catalog.Resource) ([]condPair, error) {
	s, ok := r.Parameters["condition"].(string)
	if !ok {
		return nil, augeasUnimplemented(r, "condition")
	}
	tokens := rubySplit(s)
	if len(tokens) == 0 || len(tokens)%2 != 0 {
		return nil, augeasUnimplemented(r, "condition")
	}
	var pairs []condPair
	for i := 0; i < len(tokens); i += 2 {
		k, v := tokens[i], tokens[i+1]
		if !sshKeyName.MatchString(k) || !conditionValue.MatchString(v) || !augeasText(v) {
			return nil, augeasUnimplemented(r, "condition")
		}
		if at := slices.IndexFunc(pairs, func(c condPair) bool { return c.Key == k }); at >= 0 {
			pairs[at].Value = v
		} else {
			pairs = append(pairs, condPair{k, v})
		}
	}
	return pairs, nil
}

func parseSubsystem(r catalog.Resource) (subsystemParams, error) {
	p := subsystemParams{Name: r.Title}
	if err := augeasPrelude(r); err != nil {
		return p, err
	}
	if !subsysTitle.MatchString(r.Title) {
		return p, augeasUnimplemented(r, "name")
	}
	var err error
	if p.Ensure, err = augeasEnsure(r); err != nil {
		return p, err
	}
	if p.Command, p.HasCommand, err = augeasString(r, "command"); err != nil {
		return p, err
	}
	if err := augeasComment(r); err != nil {
		return p, err
	}
	p.Target, err = augeasTarget(r, false)
	return p, err
}
