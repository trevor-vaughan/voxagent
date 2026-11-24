package augeasprovidersssh

import (
	"regexp"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// The augeasproviders_ssh types (ssh_config, sshd_config,
// sshd_config_subsystem) parse their parameters here. A value Ruby refuses is
// an error carrying Ruby's text (spec §3); a value Ruby accepts but native
// does not implement (spec §2 (d)-(f)) is an *rubyvalue.UnimplementedValue, which sends
// the type to the extension host.

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
	sshKeyName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	subsysTitle = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
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

// augeasValues reads a value native takes as a String or a non-empty Array of
// them; integers admits Integers too (sshd_config munges them with to_s). set
// is false when unset.
func augeasValues(r catalog.Resource, name string, integers bool) (vals []string, set bool, err error) {
	one := func(v any) (string, bool) {
		if _, isString := v.(string); !isString && !integers {
			return "", false
		}
		return core.Scalar(v)
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
	return nil, false, augeas.Unimplemented(r, name)
}

// augeasKey is the setting name of ssh_config and sshd_config: the key
// parameter, else the title.
func augeasKey(r catalog.Resource) (string, error) {
	key, set, err := core.StringParam(r, "key")
	if err != nil {
		return "", err
	}
	if !set {
		key = r.Title
	}
	if !sshKeyName.MatchString(key) {
		if set {
			return "", augeas.Unimplemented(r, "key")
		}
		return "", augeas.Unimplemented(r, "name")
	}
	return key, nil
}

func parseSshConfig(r catalog.Resource) (sshConfigParams, error) {
	p := sshConfigParams{Name: r.Title, Host: "*"}
	if err := augeas.Prelude(r); err != nil {
		return p, err
	}
	var err error
	if p.Ensure, err = core.Ensure(r); err != nil {
		return p, err
	}
	var hasValue bool
	if p.Value, hasValue, err = augeasValues(r, "value", false); err != nil {
		return p, err
	}
	if p.Ensure == "present" && !hasValue {
		return p, augeas.Unimplemented(r, "ensure")
	}
	if err := core.Comment(r); err != nil {
		return p, err
	}
	if p.Key, err = augeasKey(r); err != nil {
		return p, err
	}
	if p.Target, err = core.Target(r, true); err != nil {
		return p, err
	}
	if host, set, err := core.StringParam(r, "host"); err != nil {
		return p, err
	} else if set {
		if strings.ContainsAny(host, `'"\\`) {
			return p, augeas.Unimplemented(r, "host")
		}
		p.Host = host
	}
	return p, nil
}

func parseSshdConfig(r catalog.Resource) (sshdConfigParams, error) {
	p := sshdConfigParams{Name: r.Title}
	if err := augeas.Prelude(r); err != nil {
		return p, err
	}
	var err error
	if p.Ensure, err = core.Ensure(r); err != nil {
		return p, err
	}
	var hasValue bool
	if p.Value, hasValue, err = augeasValues(r, "value", true); err != nil {
		return p, err
	}
	if p.Ensure == "present" && !hasValue {
		return p, augeas.Unimplemented(r, "ensure")
	}
	if err := core.Comment(r); err != nil {
		return p, err
	}
	if p.Key, err = augeasKey(r); err != nil {
		return p, err
	}
	if p.ArrayAppend, err = core.Bool(r, "array_append", "false, true", false); err != nil {
		return p, err
	}
	if p.Target, err = core.Target(r, false); err != nil {
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
		return nil, augeas.Unimplemented(r, "condition")
	}
	tokens := rubySplit(s)
	if len(tokens) == 0 || len(tokens)%2 != 0 {
		return nil, augeas.Unimplemented(r, "condition")
	}
	var pairs []condPair
	for i := 0; i < len(tokens); i += 2 {
		k, v := tokens[i], tokens[i+1]
		if !sshKeyName.MatchString(k) || !conditionValue.MatchString(v) || !core.Text(v) {
			return nil, augeas.Unimplemented(r, "condition")
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
	if err := augeas.Prelude(r); err != nil {
		return p, err
	}
	if !subsysTitle.MatchString(r.Title) {
		return p, augeas.Unimplemented(r, "name")
	}
	var err error
	if p.Ensure, err = core.Ensure(r); err != nil {
		return p, err
	}
	if p.Command, p.HasCommand, err = core.StringParam(r, "command"); err != nil {
		return p, err
	}
	if err := core.Comment(r); err != nil {
		return p, err
	}
	p.Target, err = core.Target(r, false)
	return p, err
}
