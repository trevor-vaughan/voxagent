// Package tidy is the native tidy type (puppet/type/tidy.rb, cited as TY:).
package tidy

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// params is a Tidy munged as type/tidy.rb munges it (spec §3.1).
type params struct {
	Path      string   // cleaned
	Recurse   *int     // nil: no recursion; math.MaxInt for true/inf
	MaxFiles  any      // int64, or the String Ruby kept (fails generation)
	Matches   []string // nil when unset
	Age, Size *int64   // nil when unset; saturated
	TimeField string   // "atime" | "mtime" | "ctime"
	RmDirs    bool
}

var (
	// unitPattern and barePattern are age's and size's patterns (TY:158, :161,
	// :198, :201). Ruby's \w is ASCII here, as Go's (measured: "1é" matches
	// neither). Ruby's ^ and $ are line anchors; a value with a line break is
	// delegated, so the patterns need no (?m).
	unitPattern = regexp.MustCompile(`^([0-9]+)(\w)\w*$`)
	barePattern = regexp.MustCompile(`^([0-9]+)$`)
	// digitsPattern is recurse's and max_files' value regex (TY:39, :70).
	digitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

// ageSeconds is AgeConvertors (TY:133-139).
var ageSeconds = map[string]int64{"s": 1, "m": 60, "h": 60 * 60, "d": 60 * 60 * 24, "w": 60 * 60 * 24 * 7}

// sizePowers is size's unit table, powers of 1024 (TY:182).
var sizePowers = map[string]int{"b": 0, "k": 1, "m": 2, "g": 3, "t": 4}

// tidyParams are the parameters whose values reach a munge or a refusal.
// backup is not among them: TY:244 replaces every catalog value with false.
var tidyParams = []string{"recurse", "max_files", "matches", "age", "size", "type", "rmdirs"}

// delegateReason says why r must be applied by the extension host, or "" when
// it is native: spec §0's "Delegate, not native" conditions, the first that
// holds. A line break is checked before size's NameError, because Ruby's line
// anchors would munge such a size by its first line.
func delegateReason(r catalog.Resource) string {
	ref := r.Ref()
	path := r.Title
	if v, set := r.Parameters["path"]; set && v != nil {
		s, ok := v.(string)
		if !ok {
			return ref + ": path is not a String"
		}
		path = s
	}
	// file's recursionChild (internal/units/file/file.go) reads a Tidy's path
	// by these same rules to confine its children; keep the two in step
	// (TestDelegatedPathsAreUnconfined).
	switch {
	case !strings.HasPrefix(path, "/"):
		// File.expand_path (TY:29) would resolve it against the agent's
		// working directory or $HOME.
		return ref + ": path is relative"
	case strings.HasPrefix(path, "//"):
		// File.expand_path keeps "//x" and "///x"; filepath.Clean does not.
		return ref + ": path starts with //"
	}
	for _, name := range []string{"recurse", "max_files", "age", "size", "type"} {
		if s, ok := r.Parameters[name].(string); ok && strings.Contains(s, "\n") {
			return ref + ": line break in " + name
		}
	}
	if m := r.Parameters["matches"]; m != nil {
		for _, e := range flatten(m) {
			s, ok := e.(string)
			if !ok {
				return ref + ": matches element is not a String"
			}
			if strings.Contains(s, "\n") {
				return ref + ": line break in a matches element"
			}
		}
	}
	if v := r.Parameters["size"]; v != nil {
		// TY:206 names an undefined age, so Ruby's refusal is a NameError whose
		// text embeds object addresses.
		if s, ok := v.(string); !ok || !unitPattern.MatchString(s) && !barePattern.MatchString(s) {
			return ref + ": size Ruby refuses with a NameError"
		}
	}
	if err := rubyvalue.Rich(r); err != nil {
		return err.Error()
	}
	for _, name := range tidyParams {
		if unported(r.Parameters[name]) {
			return ref + ": " + name + " has a value shape not implemented natively"
		}
	}
	return ""
}

// unported reports whether v holds, at any depth, a Hash, whose inspect
// rubyvalue does not port, or a number beyond float64's integer precision,
// which Ruby held as an exact Integer.
func unported(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		return true
	case float64:
		return math.Abs(v) >= 1<<53
	case []any:
		return slices.ContainsFunc(v, unported)
	}
	return false
}

// flatten is [value].flatten (TY:107).
func flatten(v any) []any {
	a, ok := v.([]any)
	if !ok {
		return []any{v}
	}
	out := []any{}
	for _, e := range a {
		out = append(out, flatten(e)...)
	}
	return out
}

// parseParams munges r's parameters in Ruby's order, the declaration order
// (type.rb:2446 walks allattrs): path, recurse, max_files, matches, backup,
// age, size, type, rmdirs. The first refusal is returned with Ruby's text
// (spec §3.3), the value redacted when the catalog marks it Sensitive (D-78).
// A Tidy delegateReason sends to the extension host is an error.
func parseParams(r catalog.Resource) (params, error) {
	p := params{MaxFiles: int64(0), TimeField: "atime"}
	if why := delegateReason(r); why != "" {
		return p, errors.New(why)
	}
	marked := func(name string) bool { return slices.Contains(r.SensitiveParameters, name) }
	shown := func(name string) string { return rubyvalue.ShownValue(r, name, rubyvalue.Param(r, name)) }

	// TY:28-30. File.expand_path refuses a NUL; for any other absolute path not
	// starting "//" it is filepath.Clean.
	p.Path = r.Title
	if s, ok := r.Parameters["path"].(string); ok {
		p.Path = s
	}
	if strings.ContainsRune(p.Path, 0) {
		return p, rubyvalue.MungeFailure(r, "path", p.Path, "path name contains null byte")
	}
	p.Path = filepath.Clean(p.Path)

	if err := parseRecurse(r, &p); err != nil {
		return p, err
	}

	// TY:69-70: validated by the regex against to_s, kept as given.
	switch v := rubyvalue.Param(r, "max_files").(type) {
	case nil:
	case float64:
		if v < 0 || v != math.Trunc(v) {
			return p, rubyvalue.ParamFailure(r, "max_files", fmt.Sprintf("Invalid value %s. Valid values match /^[0-9]+$/.", shown("max_files")))
		}
		p.MaxFiles = int64(v)
	case string:
		if !digitsPattern.MatchString(v) {
			return p, rubyvalue.ParamFailure(r, "max_files", fmt.Sprintf("Invalid value %s. Valid values match /^[0-9]+$/.", shown("max_files")))
		}
		p.MaxFiles = v
	default:
		// A Boolean's or an Array's to_s never matches.
		return p, rubyvalue.ParamFailure(r, "max_files", fmt.Sprintf("Invalid value %s. Valid values match /^[0-9]+$/.", shown("max_files")))
	}

	// TY:104-108: the munged recurse's to_s against /^(0|false|)$/.
	if v := r.Parameters["matches"]; v != nil {
		if p.Recurse == nil || *p.Recurse == 0 {
			return p, rubyvalue.ParamFailure(r, "matches", "Tidy can't use matches with recurse 0, false, or undef")
		}
		p.Matches = []string{}
		for _, e := range flatten(v) {
			p.Matches = append(p.Matches, e.(string))
		}
	}

	var err error
	if p.Age, err = parseQuantity(r, "age", marked("age")); err != nil {
		return p, err
	}
	if p.Size, err = parseQuantity(r, "size", marked("size")); err != nil {
		return p, err
	}

	// TY:216-218: Value#match? compares the String's Symbol exactly.
	switch v := r.Parameters["type"]; v {
	case nil:
	case "atime", "mtime", "ctime":
		p.TimeField = v.(string)
	default:
		return p, rubyvalue.ParamFailure(r, "type", fmt.Sprintf("Invalid value %s. Valid values are atime, mtime, ctime.", shown("type")))
	}

	// TY:221, parameter/boolean.rb: the value collection true, false, yes, no
	// validates case-sensitively before Puppet::Coercion.boolean munges.
	switch v := r.Parameters["rmdirs"]; v {
	case nil, false, "false", "no":
	case true, "true", "yes":
		p.RmDirs = true
	default:
		return p, rubyvalue.ParamFailure(r, "rmdirs", fmt.Sprintf("Invalid value %s. Valid values are true, false, yes, no.", shown("rmdirs")))
	}
	return p, nil
}

// parseRecurse is recurse's munge (TY:39-54). The value collection maps
// "true", "false" and "inf" exactly and passes a digit String or an Integer
// through; Integer() reads the digits, octal on a leading 0. A depth beyond
// int is clamped to math.MaxInt, unlimited in practice.
func parseRecurse(r catalog.Resource, p *params) error {
	unlimited := math.MaxInt
	v := rubyvalue.Param(r, "recurse")
	invalid := func() error {
		return rubyvalue.MungeFailure(r, "recurse", v, "Invalid recurse value "+rubyvalue.ShownValue(r, "recurse", v))
	}
	switch t := v.(type) {
	case nil:
	case bool:
		if t {
			p.Recurse = &unlimited
		}
	case float64:
		if t != math.Trunc(t) {
			return invalid()
		}
		n := int(t)
		p.Recurse = &n
	case string:
		switch {
		case t == "true", t == "inf":
			p.Recurse = &unlimited
		case t == "false":
		case digitsPattern.MatchString(t):
			n, err := rubyvalue.Integer(t)
			switch {
			case errors.Is(err, strconv.ErrRange) || n > math.MaxInt:
				p.Recurse = &unlimited
			case err != nil:
				return rubyvalue.MungeIntegerFailure(r, "recurse", v, err)
			default:
				depth := int(n)
				p.Recurse = &depth
			}
		default:
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

// parseQuantity is age's or size's munge (TY:155-170, :196-210): digits read
// by Integer(), then a unit by the first character after them, downcased; a
// bare number is days or kilobytes. The product saturates at math.MaxInt64,
// which no lstat age or size reaches, as Ruby's Bignum does not (spec §3.1).
// size's NameError branch never reaches here: delegateReason sends it to the
// extension host.
func parseQuantity(r catalog.Resource, name string, marked bool) (*int64, error) {
	v := rubyvalue.Param(r, name)
	if v == nil {
		return nil, nil
	}
	s, _ := v.(string)
	var digits, unit string
	if m := unitPattern.FindStringSubmatch(s); m != nil {
		digits, unit = m[1], strings.ToLower(m[2])
	} else if m := barePattern.FindStringSubmatch(s); m != nil {
		digits, unit = m[1], map[string]string{"age": "d", "size": "k"}[name]
	} else if name == "age" {
		// Only age gets here: delegateReason sends an unmatched size to the host.
		// "%{age}" is to_s: a String raw, anything else as inspect.
		text := s
		if _, ok := v.(string); !ok {
			text = rubyvalue.InspectValue(v)
		}
		return nil, rubyvalue.ParamFailure(r, name, "Invalid tidy age "+rubyvalue.RedactIf(marked, "%s", text))
	}
	multi, err := rubyvalue.Integer(digits)
	if errors.Is(err, strconv.ErrRange) {
		multi = math.MaxInt64
	} else if err != nil {
		return nil, rubyvalue.MungeIntegerFailure(r, name, v, err)
	}
	var n int64
	if name == "age" {
		factor, ok := ageSeconds[unit]
		if !ok {
			return nil, rubyvalue.ParamFailure(r, name, "Invalid age unit "+rubyvalue.RedactIf(marked, "'%s'", unit))
		}
		n = saturatingMul(multi, factor)
	} else {
		power, ok := sizePowers[unit]
		if !ok {
			return nil, rubyvalue.ParamFailure(r, name, "Invalid size unit "+rubyvalue.RedactIf(marked, "'%s'", unit))
		}
		n = multi
		for range power {
			n = saturatingMul(n, 1024)
		}
	}
	return &n, nil
}

// saturatingMul is a*b for non-negative a and positive b, clamped at
// math.MaxInt64.
func saturatingMul(a, b int64) int64 {
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}
