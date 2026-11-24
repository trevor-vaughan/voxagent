package user

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// userParams is a User resource munged as type/user.rb munges it. Ensure is ""
// when the resource manages nothing (M30); a nil pointer or slice is an
// attribute the catalog does not set.
type userParams struct {
	Name     string
	Ensure   string // "", "present", "absent"
	UID, GID any    // nil | int64 | string: a string that is not an integer is kept as written
	Home     *string
	Comment  *string
	Shell    *string
	Password *string // as written; never shown in a message
	MinAge   *int64
	MaxAge   *int64
	WarnDays *int64
	Groups   []string // nil = unset; non-nil empty = []
	Expiry   *string  // "absent" or YYYY-MM-DD

	Inclusive, System, AllowDupe, ManageHome bool
}

// The patterns are Ruby's, whose ^ and $ match at every line: (?m).
var (
	idPattern   = regexp.MustCompile(`(?m)^[-0-9]+$`)
	agePattern  = regexp.MustCompile(`(?m)^-?\d+$`)
	gidNumber   = regexp.MustCompile(`(?m)^\d+$`)
	datePattern = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}$`)
)

// userManagedProperties are the properties useradd has the features for, other
// than ensure, in allattrs order (type.rb:125-127). Setting one makes the
// resource managed, which defaults ensure to present (type/user.rb:85-91).
var userManagedProperties = []string{"home", "uid", "gid", "comment", "shell", "password",
	"password_min_age", "password_max_age", "password_warn_days", "groups", "expiry"}

// userDropped are the attributes useradd lacks the feature for, in allattrs
// order, with the feature each needs (M30). forcelocal is last: it is a
// parameter with a default, so it is always dropped.
var userDropped = []struct{ attr, feature string }{
	{"roles", "manages_roles"},
	{"auths", "manages_solaris_rbac"},
	{"profiles", "manages_solaris_rbac"},
	{"keys", "manages_solaris_rbac"},
	{"project", "manages_solaris_rbac"},
	{"attributes", "manages_aix_lam"},
	{"salt", "manages_password_salt"},
	{"iterations", "manages_password_salt"},
	{"loginclass", "manages_loginclass"},
	{"ia_load_module", "manages_aix_lam"},
	{"forcelocal", "manages_local_users_and_groups"},
}

// Validate implements Validating. It reports the first failure in Ruby's
// allattrs order (type.rb:2441-2463): the properties in definition order,
// then the parameters in definition order. Attributes in userDropped are
// dropped before they are munged, so they are never refused.
func (User) Validate(r catalog.Resource) error {
	_, err := parseUserParams(r)
	return err
}

func parseUserParams(r catalog.Resource) (userParams, error) {
	p := userParams{Name: r.Title}
	if n, ok := r.Parameters["name"].(string); ok {
		p.Name = n
	}
	unsupported := func(name string, v any) error {
		return &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: name, Shown: rubyvalue.ShownValue(r, name, v)}
	}

	switch v := r.Parameters["ensure"]; v {
	case nil:
	case "present", "absent":
		p.Ensure = v.(string)
	case "role":
		return p, rubyvalue.ParamFailure(r, "ensure", "Provider useradd must have features 'manages_solaris_rbac' to set 'ensure' to 'role'")
	default:
		return p, rubyvalue.ParamFailure(r, "ensure", fmt.Sprintf("Invalid value %s. Valid values are present, absent, role.", rubyvalue.ShownValue(r, "ensure", v)))
	}

	text := func(name string, dst **string) error {
		switch v := r.Parameters[name].(type) {
		case nil:
		case string:
			*dst = &v
		default:
			return unsupported(name, v)
		}
		return nil
	}
	// id is the uid and gid munge: a string of digits and dashes is read as
	// Ruby's Integer(); any other string is kept.
	id := func(name string, dst *any) error {
		switch v := rubyvalue.Param(r, name).(type) {
		case nil:
		case string:
			if !idPattern.MatchString(v) {
				*dst = v
				return nil
			}
			n, err := rubyvalue.Integer(v)
			if err != nil {
				return rubyvalue.MungeFailure(r, name, v, err.Error())
			}
			*dst = n
		case float64:
			if v != math.Trunc(v) || math.Abs(v) >= 1<<53 {
				return unsupported(name, v)
			}
			*dst = int64(v)
		default:
			return unsupported(name, v)
		}
		return nil
	}
	// age validates on to_s, then munges a String with Integer().
	age := func(name, label string, dst **int64) error {
		var text string
		switch v := rubyvalue.Param(r, name).(type) {
		case nil:
			return nil
		case string:
			text = v
		case float64:
			if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
				text = strconv.FormatInt(int64(v), 10)
			} else {
				text = rubyvalue.InspectValue(v)
			}
		case bool:
			text = strconv.FormatBool(v)
		default:
			return unsupported(name, v)
		}
		if !agePattern.MatchString(text) {
			return rubyvalue.ParamFailure(r, name, label+" must be provided as a number.")
		}
		n, err := rubyvalue.Integer(text)
		if err != nil {
			return rubyvalue.MungeFailure(r, name, r.Parameters[name], err.Error())
		}
		*dst = &n
		return nil
	}

	if err := text("home", &p.Home); err != nil {
		return p, err
	}
	if err := id("uid", &p.UID); err != nil {
		return p, err
	}
	if err := id("gid", &p.GID); err != nil {
		return p, err
	}
	if err := text("comment", &p.Comment); err != nil {
		return p, err
	}
	if err := text("shell", &p.Shell); err != nil {
		return p, err
	}
	// An Integer password is its to_s: insync compares desired.to_s
	// (property.rb:397) and execute and chpasswd's input to_s it. Any other
	// shape is refused without the value, which is never printed.
	switch v := r.Parameters["password"].(type) {
	case nil:
	case string:
		p.Password = &v
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			s := strconv.FormatInt(int64(v), 10)
			p.Password = &s
		}
	}
	if r.Parameters["password"] != nil && p.Password == nil {
		return p, &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: "password", Shown: "[redacted]"}
	}
	if p.Password != nil && strings.Contains(*p.Password, ":") {
		return p, rubyvalue.ParamFailure(r, "password", "Passwords cannot include ':'")
	}
	if err := age("password_min_age", "Password minimum age", &p.MinAge); err != nil {
		return p, err
	}
	if err := age("password_max_age", "Password maximum age", &p.MaxAge); err != nil {
		return p, err
	}
	if err := age("password_warn_days", "Password warning days", &p.WarnDays); err != nil {
		return p, err
	}

	switch v := r.Parameters["groups"].(type) {
	case nil:
	case string:
		p.Groups = []string{v}
	case []any:
		p.Groups = make([]string, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return p, unsupported("groups", v)
			}
			p.Groups[i] = s
		}
	default:
		return p, unsupported("groups", v)
	}
	for _, g := range p.Groups {
		switch {
		case gidNumber.MatchString(g):
			return p, rubyvalue.ParamFailure(r, "groups", "Group names must be provided, not GID numbers.")
		case strings.Contains(g, ","):
			return p, rubyvalue.ParamFailure(r, "groups", "Group names must be provided as an array, not a comma-separated list.")
		case g == "":
			return p, rubyvalue.ParamFailure(r, "groups", `Group names must not be empty. If you want to specify "no groups" pass an empty array`)
		}
	}

	switch v := r.Parameters["expiry"].(type) {
	case nil:
	case string:
		if v != "absent" && !datePattern.MatchString(v) {
			return p, rubyvalue.ParamFailure(r, "expiry", `Expiry dates must be YYYY-MM-DD or the string "absent"`)
		}
		p.Expiry = &v
	default:
		return p, unsupported("expiry", v)
	}

	boolParam := func(name string, dst *bool) error {
		v := r.Parameters[name]
		if v == nil {
			return nil
		}
		val, err := rubyvalue.Bool(v)
		if err != nil {
			return rubyvalue.MungeFailure(r, name, v, err.Error())
		}
		*dst = val
		return nil
	}
	membership := func(name string, inclusive *bool) error {
		switch v := r.Parameters[name]; v {
		case nil, "minimum":
		case "inclusive":
			if inclusive != nil {
				*inclusive = true
			}
		default:
			return rubyvalue.ParamFailure(r, name, fmt.Sprintf("Invalid value %s. Valid values are inclusive, minimum.", rubyvalue.ShownValue(r, name, v)))
		}
		return nil
	}
	if err := membership("membership", &p.Inclusive); err != nil {
		return p, err
	}
	if err := boolParam("system", &p.System); err != nil {
		return p, err
	}
	if err := boolParam("allowdupe", &p.AllowDupe); err != nil {
		return p, err
	}
	if err := boolParam("managehome", &p.ManageHome); err != nil {
		return p, err
	}
	for _, name := range []string{"role_membership", "auth_membership", "profile_membership", "key_membership", "attribute_membership"} {
		if err := membership(name, nil); err != nil {
			return p, err
		}
	}
	// Only a value that purges nothing reaches native (routing, spec §0), and
	// it purges nothing.
	switch v := r.Parameters["purge_ssh_keys"].(type) {
	case nil:
	case bool, string:
		if v != false && v != "false" {
			return p, unsupported("purge_ssh_keys", v)
		}
	case []any:
		if len(v) != 0 {
			return p, unsupported("purge_ssh_keys", v)
		}
	default:
		return p, unsupported("purge_ssh_keys", v)
	}

	if p.Ensure == "" {
		for _, name := range userManagedProperties {
			if r.Parameters[name] != nil {
				p.Ensure = "present"
				break
			}
		}
	}
	return p, nil
}

// ConversionDebug implements ConversionDebugging: the attributes useradd lacks
// the feature for, given ones in allattrs order and then forcelocal, which has
// a default and so is always dropped (M30).
func (User) ConversionDebug(r catalog.Resource) []string {
	var lines []string
	for _, d := range userDropped {
		if d.attr == "forcelocal" || r.Parameters[d.attr] != nil {
			lines = append(lines, fmt.Sprintf(nameservice.FeatureMissing, useraddProvider, d.feature, d.attr))
		}
	}
	return lines
}
