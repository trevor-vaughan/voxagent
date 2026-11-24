package extension

import "strings"

// hostEnvKeys is the whole environment the host receives: the
// command path, HOME (which the agent process keeps; Puppet strips it only
// from the commands it runs, util/execution.rb:387), the locale, and Ruby's
// gem location, without which a gem-managed Ruby cannot load Puppet
// (measured). Nothing that changes what Ruby loads -- RUBYLIB, RUBYOPT,
// LD_PRELOAD -- passes.
var hostEnvKeys = map[string]struct{}{
	"PATH": {}, "HOME": {}, "LANG": {}, "LC_ALL": {}, "GEM_HOME": {}, "GEM_PATH": {},
}

// HostEnv filters environ, in order, down to the host's environment.
func HostEnv(environ []string) []string {
	out := []string{} // never nil: exec.Cmd would read a nil Env as "inherit everything"
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if _, keep := hostEnvKeys[key]; ok && keep {
			out = append(out, kv)
		}
	}
	return out
}
