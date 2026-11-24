package exec

import (
	"os"
	"os/user"
	"strconv"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

// paramsRes is an exec with a qualified command, so only the parameter under
// test can fail Validate.
func paramsRes(params map[string]any) catalog.Resource {
	all := map[string]any{"command": "/bin/true"}
	for k, v := range params {
		all[k] = v
	}
	return raltest.ExecRes("t", all)
}

func setAgentRoot(t *testing.T, root bool) {
	t.Helper()
	old := agentIsRoot
	agentIsRoot = func() bool { return root }
	t.Cleanup(func() { agentIsRoot = old })
}

func TestExecParamsRefusals(t *testing.T) {
	setAgentRoot(t, true)
	const pre = "Parameter "
	tests := []struct {
		name  string
		param string
		value any
		want  string
	}{
		{"timeout not a number", "timeout", "abc", pre + `timeout failed on Exec[t]: Munging failed for value "abc" in class timeout: The timeout must be a number.`},
		{"timeout list shows the rest", "timeout", []any{"abc", 9.0}, pre + `timeout failed on Exec[t]: Munging failed for value [9] in class timeout: The timeout must be a number.`},
		{"timeout empty list", "timeout", []any{}, pre + `timeout failed on Exec[t]: Munging failed for value [] in class timeout: can't convert nil into Float`},
		{"timeout bool", "timeout", true, pre + `timeout failed on Exec[t]: Munging failed for value true in class timeout: can't convert true into Float`},
		{"timeout underscore pair", "timeout", "1__0", pre + `timeout failed on Exec[t]: Munging failed for value "1__0" in class timeout: The timeout must be a number.`},
		{"timeout infinity word", "timeout", "Infinity", pre + `timeout failed on Exec[t]: Munging failed for value "Infinity" in class timeout: The timeout must be a number.`},
		{"tries zero", "tries", int64(0), pre + `tries failed on Exec[t]: Munging failed for value 0 in class tries: Tries must be an integer >= 1`},
		{"tries float below one", "tries", 0.5, pre + `tries failed on Exec[t]: Munging failed for value 0.5 in class tries: Tries must be an integer >= 1`},
		{"tries string", "tries", "2x", pre + `tries failed on Exec[t]: Munging failed for value "2x" in class tries: Tries must be an integer`},
		{"tries string zero", "tries", "0", pre + `tries failed on Exec[t]: Munging failed for value "0" in class tries: Tries must be an integer >= 1`},
		{"tries octal-looking", "tries", "08", pre + `tries failed on Exec[t]: Munging failed for value "08" in class tries: invalid value for Integer(): "08"`},
		{"tries bool", "tries", true, pre + "tries failed on Exec[t]: Munging failed for value true in class tries: undefined method `<' for true:TrueClass"},
		{"try_sleep negative", "try_sleep", int64(-1), pre + `try_sleep failed on Exec[t]: Munging failed for value -1 in class try_sleep: try_sleep cannot be a negative number`},
		{"try_sleep negative string", "try_sleep", "-1", pre + `try_sleep failed on Exec[t]: Munging failed for value "-1" in class try_sleep: try_sleep cannot be a negative number`},
		{"try_sleep word", "try_sleep", "soon", pre + `try_sleep failed on Exec[t]: Munging failed for value "soon" in class try_sleep: try_sleep must be a number`},
		{"try_sleep two dots", "try_sleep", "1.2.3", pre + `try_sleep failed on Exec[t]: Munging failed for value "1.2.3" in class try_sleep: invalid value for Float(): "1.2.3"`},
		{"umask string", "umask", "0999", pre + `umask failed on Exec[t]: The umask specification is invalid: "0999"`},
		{"umask too long", "umask", "017777", pre + `umask failed on Exec[t]: The umask specification is invalid: "017777"`},
		{"umask integer", "umask", int64(22), pre + "umask failed on Exec[t]: Munging failed for value 22 in class umask: undefined method `=~' for 22:Integer"},
		{"umask float", "umask", 2.5, pre + "umask failed on Exec[t]: Munging failed for value 2.5 in class umask: undefined method `=~' for 2.5:Float"},
		{"environment no key", "environment", "=nokey", pre + `environment failed on Exec[t]: Invalid environment setting '=nokey'`},
		{"environment list", "environment", []any{"A=1", "nokey"}, pre + `environment failed on Exec[t]: Invalid environment setting 'nokey'`},
		{"environment non-string", "environment", []any{5.0}, pre + "environment failed on Exec[t]: Validate method failed for class environment: undefined method `=~' for 5:Integer"},
		{"refresh unqualified", "refresh", "relative-cmd", pre + `refresh failed on Exec[t]: 'relative-cmd' is not qualified and no path was specified. Please qualify the command or specify a path.`},
		{"refresh argv unqualified", "refresh", []any{"relative-cmd", "x"}, pre + `refresh failed on Exec[t]: 'relative-cmd' is not qualified and no path was specified. Please qualify the command or specify a path.`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := paramsRes(map[string]any{tt.param: tt.value})
			if _, err := parseExecParams(Exec{}, r); err == nil || err.Error() != tt.want {
				t.Errorf("parseExecParams error = %v, want %q", err, tt.want)
			}
			if err := (Exec{}).Validate(r); err == nil || err.Error() != tt.want {
				t.Errorf("Validate error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExecParamsAccepts(t *testing.T) {
	setAgentRoot(t, true)
	umask := func(v int) *int { return &v }
	tests := []struct {
		name   string
		params map[string]any
		check  func(p execParams) bool
	}{
		{"defaults", nil, func(p execParams) bool {
			return p.User == nil && p.Group == nil && p.Refresh == nil && p.Environment == nil && p.Umask == nil &&
				p.Timeout == 300*time.Second && p.Tries == 1 && p.TrySleep == 0 && p.TrySleepShown == "0"
		}},
		{"nil is unset", map[string]any{"timeout": nil, "tries": nil, "umask": nil, "user": nil}, func(p execParams) bool {
			return p.Timeout == 300*time.Second && p.Tries == 1 && p.Umask == nil && p.User == nil
		}},
		{"timeout list uses first", map[string]any{"timeout": []any{"0.5", 9.0}}, func(p execParams) bool { return p.Timeout == 500*time.Millisecond }},
		{"timeout negative clamps to none", map[string]any{"timeout": -3.0}, func(p execParams) bool { return p.Timeout == 0 }},
		{"timeout int64", map[string]any{"timeout": int64(2)}, func(p execParams) bool { return p.Timeout == 2*time.Second }},
		{"timeout ruby float string", map[string]any{"timeout": " 1_0 "}, func(p execParams) bool { return p.Timeout == 10*time.Second }},
		{"timeout hex string", map[string]any{"timeout": "0x1A"}, func(p execParams) bool { return p.Timeout == 26*time.Second }},
		{"timeout huge saturates", map[string]any{"timeout": 1e300}, func(p execParams) bool { return p.Timeout == time.Duration(1<<63-1) }},
		{"tries string", map[string]any{"tries": "3"}, func(p execParams) bool { return p.Tries == 3 }},
		{"tries int64", map[string]any{"tries": int64(4)}, func(p execParams) bool { return p.Tries == 4 }},
		{"tries non-integer kept", map[string]any{"tries": 2.5}, func(p execParams) bool { return p.Tries == 2.5 }},
		{"try_sleep string is a float", map[string]any{"try_sleep": "1"}, func(p execParams) bool { return p.TrySleep == 1 && p.TrySleepShown == "1.0" }},
		{"try_sleep integer", map[string]any{"try_sleep": int64(1)}, func(p execParams) bool { return p.TrySleep == 1 && p.TrySleepShown == "1" }},
		{"try_sleep float", map[string]any{"try_sleep": 0.5}, func(p execParams) bool { return p.TrySleep == 0.5 && p.TrySleepShown == "0.5" }},
		{"try_sleep decimal string", map[string]any{"try_sleep": ".5"}, func(p execParams) bool { return p.TrySleep == 0.5 && p.TrySleepShown == "0.5" }},
		{"umask 022", map[string]any{"umask": "022"}, func(p execParams) bool { return p.Umask != nil && *p.Umask == 0o22 }},
		{"umask 0777", map[string]any{"umask": "0777"}, func(p execParams) bool { return *p.Umask == *umask(0o777) }},
		{"umask 7", map[string]any{"umask": "7"}, func(p execParams) bool { return *p.Umask == 7 }},
		{"umask 0", map[string]any{"umask": "0"}, func(p execParams) bool { return p.Umask != nil && *p.Umask == 0 }},
		{"environment scalar", map[string]any{"environment": "A=1"}, func(p execParams) bool { return len(p.Environment) == 1 && p.Environment[0] == "A=1" }},
		{"environment list", map[string]any{"environment": []any{"A=1", "x B=2"}}, func(p execParams) bool {
			return len(p.Environment) == 2 && p.Environment[1] == "x B=2"
		}},
		{"environment unanchored", map[string]any{"environment": "oops\nY=z"}, func(p execParams) bool { return len(p.Environment) == 1 }},
		{"user any when root", map[string]any{"user": "nobody", "group": int64(7)}, func(p execParams) bool { return p.User == "nobody" && p.Group == 7.0 }},
		{"user numeric when root", map[string]any{"user": int64(65534)}, func(p execParams) bool { return p.User == 65534.0 }},
		{"refresh kept", map[string]any{"refresh": "/bin/true x"}, func(p execParams) bool { return p.Refresh == "/bin/true x" }},
		{"refresh unqualified with path", map[string]any{"refresh": "relative", "path": "/bin"}, func(p execParams) bool { return p.Refresh == "relative" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := paramsRes(tt.params)
			p, err := parseExecParams(Exec{}, r)
			if err != nil {
				t.Fatalf("parseExecParams: %v", err)
			}
			if !tt.check(p) {
				t.Errorf("unexpected params: %+v", p)
			}
			if err := (Exec{}).Validate(r); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestExecParamsUserNonRoot(t *testing.T) {
	setAgentRoot(t, false)
	self, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatalf("looking up the test's own user: %v", err)
	}
	const want = "Parameter user failed on Exec[t]: Only root can execute commands as other users"
	for _, tt := range []struct {
		name  string
		value any
		ok    bool
	}{
		{"own name", self.Username, true},
		{"other name", self.Username + "x", false},
		{"own uid as number", float64(os.Getuid()), false},
		{"own uid as string", self.Uid, false},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseExecParams(Exec{}, paramsRes(map[string]any{"user": tt.value}))
			switch {
			case tt.ok && err != nil:
				t.Errorf("refused: %v", err)
			case !tt.ok && (err == nil || err.Error() != want):
				t.Errorf("error = %v, want %q", err, want)
			}
		})
	}
}

// Validate returns the first failure in type/exec.rb's declaration order.
func TestExecParamsFirstFailureOrder(t *testing.T) {
	tests := []struct {
		name   string
		root   bool
		params map[string]any
		want   string
	}{
		{"user before timeout", false, map[string]any{"user": "nobody-such", "timeout": "abc"}, "Parameter user failed on Exec[t]: Only root can execute commands as other users"},
		{"umask before tries", true, map[string]any{"umask": "9", "tries": int64(0)}, `Parameter umask failed on Exec[t]: The umask specification is invalid: "9"`},
		{"environment before umask", true, map[string]any{"umask": "9", "environment": "x"}, "Parameter environment failed on Exec[t]: Invalid environment setting 'x'"},
		{"user before cwd", false, map[string]any{"user": "nobody-such", "cwd": "rel"}, "Parameter user failed on Exec[t]: Only root can execute commands as other users"},
		{"cwd before refresh", true, map[string]any{"cwd": "rel", "refresh": "rel"}, "Parameter cwd failed on Exec[t]: cwd must be a fully qualified path"},
		{"logoutput before timeout", true, map[string]any{"logoutput": "maybe", "timeout": "abc"}, "Parameter logoutput failed on Exec[t]: Invalid value \"maybe\". Valid values are true, false, on_failure."},
		{"timeout before creates", true, map[string]any{"timeout": "abc", "creates": "rel"}, `Parameter timeout failed on Exec[t]: Munging failed for value "abc" in class timeout: The timeout must be a number.`},
		{"timeout before unless", true, map[string]any{"timeout": "abc", "unless": "rel"}, `Parameter timeout failed on Exec[t]: Munging failed for value "abc" in class timeout: The timeout must be a number.`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAgentRoot(t, tt.root)
			err := (Exec{}).Validate(paramsRes(tt.params))
			if err == nil || err.Error() != tt.want {
				t.Errorf("Validate error = %v, want %q", err, tt.want)
			}
		})
	}
}

// A parameter the catalog marks Sensitive is shown as [redacted] (D-78).
func TestExecParamsRedactsSensitiveValues(t *testing.T) {
	setAgentRoot(t, true)
	tests := []struct {
		param string
		value any
		want  string
	}{
		{"timeout", "s3cret", "Parameter timeout failed on Exec[t]: Munging failed for value [redacted] in class timeout: The timeout must be a number."},
		{"tries", "s3cret", "Parameter tries failed on Exec[t]: Munging failed for value [redacted] in class tries: Tries must be an integer"},
		{"try_sleep", "1.2.3", "Parameter try_sleep failed on Exec[t]: Munging failed for value [redacted] in class try_sleep: invalid value for Float(): [redacted]"},
		{"umask", "s3cret", "Parameter umask failed on Exec[t]: The umask specification is invalid: [redacted]"},
		{"umask", int64(22), "Parameter umask failed on Exec[t]: Munging failed for value [redacted] in class umask: undefined method `=~' for [redacted]:Integer"},
		{"environment", "s3cret", "Parameter environment failed on Exec[t]: Invalid environment setting [redacted]"},
		{"refresh", "s3cret", "Parameter refresh failed on Exec[t]: [command redacted] is not qualified and no path was specified. Please qualify the command or specify a path."},
	}
	for _, tt := range tests {
		t.Run(tt.param, func(t *testing.T) {
			r := paramsRes(map[string]any{tt.param: tt.value})
			r.SensitiveParameters = []string{tt.param}
			if _, err := parseExecParams(Exec{}, r); err == nil || err.Error() != tt.want {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExecAttrsIncludesEveryParameter(t *testing.T) {
	attrs := Exec{}.Attrs()
	for _, name := range []string{"command", "path", "user", "group", "cwd", "logoutput", "refresh", "environment", "umask",
		"timeout", "tries", "try_sleep", "refreshonly", "creates", "unless", "onlyif", "returns"} {
		if _, ok := attrs[name]; !ok {
			t.Errorf("Attrs() lacks %q", name)
		}
	}
	if len(attrs) != 17 {
		t.Errorf("Attrs() has %d names, want 17", len(attrs))
	}
}
