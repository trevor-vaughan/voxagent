package ral

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// execParams is the eight exec parameters beyond the original nine, parsed and
// validated as type/exec.rb's validate and munge blocks do (spec §2).
type execParams struct {
	User, Group   any           // nil when unset; string or float64 as decoded
	Refresh       any           // nil, string or []any (as command)
	Environment   []string      // scalar wrapped; nil when unset
	Umask         *int          // nil when unset; octal value
	Timeout       time.Duration // 300s default; 0 = none
	Tries         float64       // 1 default; non-integer kept (fails at sync, M19)
	TrySleep      float64       // 0 default
	TrySleepShown string        // Ruby rendering: "1" for Integer, Float#to_s otherwise
}

// agentIsRoot is Puppet.features.root?; a variable so tests can stand in for
// either answer.
var agentIsRoot = func() bool { return os.Geteuid() == 0 }

var (
	envSetting  = regexp.MustCompile(`\w+=`)
	umaskSpec   = regexp.MustCompile(`(?m)^0?[0-7]{1,4}$`)
	digitsLine  = regexp.MustCompile(`(?m)^\d+$`)
	numberChars = regexp.MustCompile(`(?m)^[-\d.]+$`)
	// rubyFloatText is the shape Kernel#Float accepts for a decimal string:
	// optional sign, digits with single underscores between them, an optional
	// fraction (leading or trailing dot allowed) and exponent.
	rubyFloatText = regexp.MustCompile(`^[-+]?(\d+(_\d+)*)?(\.(\d+(_\d+)*)?)?([eE][-+]?\d+(_\d+)*)?$`)
	rubyHexText   = regexp.MustCompile(`^[-+]?0[xX][0-9a-fA-F]+(_[0-9a-fA-F]+)*(\.[0-9a-fA-F]+)?([pP][-+]?\d+)?$`)
)

// parseExecParams validates and parses all eight parameters, returning the
// first failure in type/exec.rb's declaration order. Validate calls the two
// passes separately so cwd and logoutput fall between them.
func parseExecParams(e Exec, r catalog.Resource) (execParams, error) {
	var p execParams
	if err := p.parseIdentity(r); err != nil {
		return p, err
	}
	err := p.parseRun(e, r)
	return p, err
}

// noMethod is Ruby's NoMethodError text for op sent to a value that lacks it,
// in the form measured in M18.
func noMethod(r catalog.Resource, name, op string, value any) string {
	return fmt.Sprintf("undefined method `%s' for %s:%s", op, rubyvalue.ShownValue(r, name, value), catalog.RubyClassName(value))
}

// parseIdentity is the user and group part of §2: user, then group (which is
// resolved at run time and has nothing to refuse here).
func (p *execParams) parseIdentity(r catalog.Resource) error {
	p.User, p.Group = rubyvalue.Param(r, "user"), rubyvalue.Param(r, "group")
	// type/exec.rb:251: resource.current_username() != user, so a non-root
	// agent naming its own uid number is refused.
	if p.User != nil && !agentIsRoot() {
		self, err := user.LookupId(strconv.Itoa(os.Getuid()))
		if name, isString := p.User.(string); err != nil || !isString || name != self.Username {
			return rubyvalue.ParamFailure(r, "user", "Only root can execute commands as other users")
		}
	}
	return nil
}

// parseRun is refresh through try_sleep in declaration order.
func (p *execParams) parseRun(e Exec, r catalog.Resource) error {
	p.Timeout, p.Tries, p.TrySleep, p.TrySleepShown = 300*time.Second, 1, 0, "0"

	if v := rubyvalue.Param(r, "refresh"); v != nil {
		p.Refresh = v
		if path := rubyvalue.Param(r, "path"); path == nil {
			if exe, bad := unqualifiedExe(v); bad {
				redact := slices.Contains(r.SensitiveParameters, "refresh") || commandsMarked(e, r)
				return rubyvalue.ParamFailure(r, "refresh", unqualifiedMessage(exe, redact))
			}
		}
	}
	if v := rubyvalue.Param(r, "environment"); v != nil {
		if err := p.parseEnvironment(r, v); err != nil {
			return err
		}
	}
	if v := rubyvalue.Param(r, "umask"); v != nil {
		if err := p.parseUmask(r, v); err != nil {
			return err
		}
	}
	if v := rubyvalue.Param(r, "timeout"); v != nil {
		if err := p.parseTimeout(r, v); err != nil {
			return err
		}
	}
	if v := rubyvalue.Param(r, "tries"); v != nil {
		if err := p.parseTries(r, v); err != nil {
			return err
		}
	}
	if v := rubyvalue.Param(r, "try_sleep"); v != nil {
		if err := p.parseTrySleep(r, v); err != nil {
			return err
		}
	}
	return nil
}

// parseEnvironment is the validate block (type/exec.rb:306): every element must
// contain /\w+=/. An element Ruby's =~ cannot take is a NoMethodError, which
// Parameter#validate reports as a DevError (parameter.rb:485).
func (p *execParams) parseEnvironment(r catalog.Resource, v any) error {
	list, isList := v.([]any)
	if !isList {
		list = []any{v}
	}
	env := make([]string, 0, len(list))
	for _, el := range list {
		s, isString := el.(string)
		if !isString && el != nil {
			return rubyvalue.ParamFailure(r, "environment", "Validate method failed for class environment: "+noMethod(r, "environment", "=~", el))
		}
		if !envSetting.MatchString(s) {
			return rubyvalue.ParamFailure(r, "environment", "Invalid environment setting "+rubyvalue.RedactIf(slices.Contains(r.SensitiveParameters, "environment"), "'%s'", s))
		}
		env = append(env, s)
	}
	p.Environment = env
	return nil
}

// parseUmask is the munge block (type/exec.rb:318). A bad string raises
// Puppet::Error, which munge lets through unwrapped; any other class fails =~.
func (p *execParams) parseUmask(r catalog.Resource, v any) error {
	s, isString := v.(string)
	if !isString {
		return rubyvalue.MungeFailure(r, "umask", v, noMethod(r, "umask", "=~", v))
	}
	if !umaskSpec.MatchString(s) {
		return rubyvalue.ParamFailure(r, "umask", "The umask specification is invalid: "+rubyvalue.ShownValue(r, "umask", s))
	}
	// String#to_i(8): leading whitespace, then the leading octal digits.
	digits := strings.TrimLeft(s, " \t\n\v\f\r")
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '7' {
		end++
	}
	mode, _ := strconv.ParseInt(digits[:end], 8, 32)
	m := int(mode)
	p.Umask = &m
	return nil
}

// parseTimeout is the munge block (type/exec.rb:333): a list gives its first
// element (shifted off, so a failure shows the rest), Float() converts, and a
// negative becomes 0.
func (p *execParams) parseTimeout(r catalog.Resource, v any) error {
	shown, value := v, v
	if list, isList := v.([]any); isList {
		value = nil
		if len(list) > 0 {
			value = list[0]
			list = list[1:]
		}
		shown = list
	}
	var seconds float64
	switch n := value.(type) {
	case float64:
		seconds = n
	case string:
		f, ok := rubyFloatParse(n)
		if !ok {
			return rubyvalue.MungeFailure(r, "timeout", shown, "The timeout must be a number.")
		}
		seconds = f
	default:
		return rubyvalue.MungeFailure(r, "timeout", shown, fmt.Sprintf("can't convert %s into Float", rubyConversionName(value)))
	}
	seconds = math.Max(seconds, 0)
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		p.Timeout = time.Duration(math.MaxInt64)
	} else {
		p.Timeout = time.Duration(seconds * float64(time.Second))
	}
	return nil
}

// rubyConversionName is how Kernel#Float names a value it cannot convert:
// nil, true and false by their literal, anything else by class.
func rubyConversionName(v any) string {
	switch v.(type) {
	case nil, bool:
		return rubyvalue.InspectValue(v)
	default:
		return catalog.RubyClassName(v)
	}
}

// rubyFloatParse is Kernel#Float for a string: surrounding whitespace allowed,
// underscores only between digits, hex accepted, Infinity and NaN refused.
func rubyFloatParse(s string) (float64, bool) {
	s = strings.Trim(s, " \t\n\v\f\r")
	hex := rubyHexText.MatchString(s)
	if !hex && (!rubyFloatText.MatchString(s) || !strings.ContainsAny(s, "0123456789")) {
		return 0, false
	}
	s = strings.ReplaceAll(s, "_", "")
	if hex && !strings.ContainsAny(s, "pP") {
		s += "p0"
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// parseTries is the munge block (type/exec.rb:352): a string must be digits
// and goes through Integer(); anything below 1 is refused. A non-integer
// number passes here and fails at sync (M19).
func (p *execParams) parseTries(r catalog.Resource, v any) error {
	n, isNumber := v.(float64)
	if s, isString := v.(string); isString {
		if !digitsLine.MatchString(s) {
			return rubyvalue.MungeFailure(r, "tries", v, "Tries must be an integer")
		}
		// Integer() reads a leading 0 as octal, as ParseInt's base 0 does.
		i, err := strconv.ParseInt(strings.Trim(s, " \t\n\v\f\r"), 0, 64)
		switch {
		case errors.Is(err, strconv.ErrRange):
			n, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
		case err != nil:
			return rubyvalue.MungeFailure(r, "tries", v, "invalid value for Integer(): "+rubyvalue.ShownValue(r, "tries", s))
		default:
			n = float64(i)
		}
	} else if !isNumber {
		return rubyvalue.MungeFailure(r, "tries", v, noMethod(r, "tries", "<", v))
	}
	if n < 1 {
		return rubyvalue.MungeFailure(r, "tries", v, "Tries must be an integer >= 1")
	}
	p.Tries = n
	return nil
}

// parseTrySleep is the munge block (type/exec.rb:374): a string must look like
// a number and goes through Float(); a negative is refused. The Ruby rendering
// is kept because the "Sleeping for" message prints it (M6).
func (p *execParams) parseTrySleep(r catalog.Resource, v any) error {
	n, isNumber := v.(float64)
	if s, isString := v.(string); isString {
		if !numberChars.MatchString(s) {
			return rubyvalue.MungeFailure(r, "try_sleep", v, "try_sleep must be a number")
		}
		f, ok := rubyFloatParse(s)
		if !ok {
			return rubyvalue.MungeFailure(r, "try_sleep", v, "invalid value for Float(): "+rubyvalue.ShownValue(r, "try_sleep", s))
		}
		n = f
		p.TrySleepShown = rubyvalue.Float(f)
	} else if isNumber {
		p.TrySleepShown = rubyvalue.InspectValue(n)
	} else {
		return rubyvalue.MungeFailure(r, "try_sleep", v, noMethod(r, "try_sleep", "<", v))
	}
	if n < 0 {
		return rubyvalue.MungeFailure(r, "try_sleep", v, "try_sleep cannot be a negative number")
	}
	p.TrySleep = n
	return nil
}
