package augeasproviderscore

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/puppetpath"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// The parameter readers the augeasproviders types share (sysctl, ssh_config,
// sshd_config, sshd_config_subsystem). A value Ruby refuses is an error
// carrying Ruby's text (spec §3); a value Ruby accepts but native does not
// implement (spec §2 (d)-(f)) is an *rubyvalue.UnimplementedValue, which sends
// the type to the extension host.

// SysctlEntry is what prefetch learnt of one title: Valid when the target
// file or `sysctl -e` knows the key, Persist when the file does.
type SysctlEntry struct{ Valid, Persist bool }

// targetSegment is a path segment of a native target.
var targetSegment = regexp.MustCompile(`^[A-Za-z0-9._+@-]+$`)

// Text reports whether s is valid UTF-8 without a control character.
func Text(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// Ensure reads ensure: "" when unset. A scalar that is no ensure value
// is Ruby's refusal; an Array or Hash is validated element-wise by Ruby, which
// native does not port.
func Ensure(r catalog.Resource) (string, error) {
	switch v := r.Parameters["ensure"]; v {
	case nil:
		return "", nil
	case "present", "absent":
		return v.(string), nil
	default:
		switch v.(type) {
		case []any, map[string]any:
			return "", augeas.Unimplemented(r, "ensure")
		}
		return "", rubyvalue.ParamFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent.", rubyvalue.ShownValue(r, "ensure", v)))
	}
}

// Bool reads a newvalues(:true, :false) parameter, which takes only
// true, false and those two strings, case-sensitively. valid is the list the
// refusal names, in the type's order. def is the value when unset.
func Bool(r catalog.Resource, name, valid string, def bool) (bool, error) {
	switch v := r.Parameters[name]; v {
	case nil:
		return def, nil
	case true, "true":
		return true, nil
	case false, "false":
		return false, nil
	default:
		if rubyvalue.HoldsHash(v) {
			return false, augeas.Unimplemented(r, name)
		}
		return false, rubyvalue.ParamFailure(r, name, fmt.Sprintf("Invalid value %s. Valid values are %s.", rubyvalue.ShownValue(r, name, v), valid))
	}
}

// StringParam reads a parameter native takes as a String: set is false when
// unset, any other shape or an unusable string is unimplemented.
func StringParam(r catalog.Resource, name string) (s string, set bool, err error) {
	switch v := r.Parameters[name].(type) {
	case nil:
		return "", false, nil
	case string:
		if Text(v) {
			return v, true, nil
		}
	}
	return "", false, augeas.Unimplemented(r, name)
}

// Scalar is a String or an Integer of magnitude below 2^53 as Ruby's
// to_s renders it; ok is false for any other value or an unusable string.
func Scalar(v any) (s string, ok bool) {
	switch t := v.(type) {
	case string:
		return t, Text(t)
	case int:
		return Scalar(float64(t))
	case int64:
		return Scalar(float64(t))
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return strconv.FormatInt(int64(t), 10), true
		}
	}
	return "", false
}

// Target reads target: "" when unset. Native takes a clean absolute path
// of the characters §2 (d) lists, anything else is unimplemented. For
// ssh_config a String that is not absolute is Ruby's refusal instead.
func Target(r catalog.Resource, refuseRelative bool) (string, error) {
	s, set, err := StringParam(r, "target")
	if err != nil || !set {
		return "", err
	}
	if refuseRelative && !strings.HasPrefix(s, "/") {
		return "", rubyvalue.ParamFailure(r, "target", "target must be an absolute path")
	}
	if strings.HasPrefix(s, "/") && puppetpath.Clean(s) {
		clean := true
		for _, seg := range strings.Split(s[1:], "/") {
			clean = clean && targetSegment.MatchString(seg)
		}
		if clean {
			return s, nil
		}
	}
	return "", augeas.Unimplemented(r, "target")
}

// Comment refuses comment, which native does not manage.
func Comment(r catalog.Resource) error {
	if r.Parameters["comment"] != nil {
		return augeas.Unimplemented(r, "comment")
	}
	return nil
}

// TargetRequire is autorequire(:file) { self[:target] }: the File for a target
// the catalog sets, where it is managed. An unset target means no edge, not
// the default file.
func TargetRequire(target string, managed func(string) bool) []string {
	if ref := "File[" + target + "]"; target != "" && managed(ref) {
		return []string{ref}
	}
	return nil
}
