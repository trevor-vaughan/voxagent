package ral

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// userEnvVars are the variables Puppet removes from every child
// (util/posix.rb:11's USER_ENV_VARS, applied at util/execution.rb:386).
var userEnvVars = []string{"HOME", "USER", "LOGNAME"}

// withoutUserEnv returns environ without the userEnvVars.
func withoutUserEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(userEnvVars, name) {
			out = append(out, kv)
		}
	}
	return out
}

// envAssignment is provider/exec.rb:20's /^(\w+)=((.|\n)*)$/. Ruby's ^ is
// line-anchored and its value group runs to the end of the string, so the
// first line that starts a setting names the variable and everything after its
// "=" is the value (M9).
var envAssignment = regexp.MustCompile(`(?m)^(\w+)=`)

// execEnv builds a command's environment as provider/exec.rb:9-40 does: the
// agent's environment without the userEnvVars, PATH from path, then each
// environment element, later settings winning. It logs Ruby's warnings for
// the settings it cannot take or that override an earlier one. redact says
// environment is marked Sensitive, which Ruby leaks into those warnings (D-78).
func execEnv(ctx context.Context, r catalog.Resource, p execParams, path string, redact bool) []string {
	env := withoutUserEnv(os.Environ())
	set := map[string]bool{}
	if path != "" {
		env = append(env, "PATH="+path)
		set["PATH"] = true
	}
	for _, setting := range p.Environment {
		loc := envAssignment.FindStringSubmatchIndex(setting)
		if loc == nil {
			runlog.From(ctx).Log("warning", SourcesFrom(ctx).Provider,
				"Cannot understand environment setting "+redactIf(redact, "%s", rubyInspect(setting)))
			continue
		}
		name, value := setting[loc[2]:loc[3]], setting[loc[1]:]
		if set[name] {
			runlog.From(ctx).Log("warning", SourcesFrom(ctx).Provider,
				fmt.Sprintf("Overriding environment setting '%s' with %s", name, redactIf(redact, "'%s'", value)))
		}
		if value == "" {
			warnOnce(ctx, "empty_env_var_"+name, fmt.Sprintf("Empty environment setting '%s'", name), r.File, r.Line)
		}
		set[name] = true
		env = append(env, name+"="+value)
	}
	return env
}
