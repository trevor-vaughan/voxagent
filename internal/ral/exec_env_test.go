package ral

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
)

// envWarnings runs execEnv on an Exec[env] resource carrying environment and
// returns the environment it built and the warnings it logged as "source: text".
func envWarnings(t *testing.T, environment any, path string, marked bool) ([]string, []string) {
	t.Helper()
	r := catalog.Resource{Type: "Exec", Title: "env", File: "/site.pp", Line: 3,
		Parameters: map[string]any{"command": "true", "environment": environment}}
	var p execParams
	switch v := environment.(type) {
	case string:
		p.Environment = []string{v}
	case []string:
		p.Environment = v
	}
	ctx, entries := raltest.DebugContext("warning")
	ctx = sources.WithSources(WithUniqueWarnings(ctx), sources.Sources{Provider: "Exec[env](provider=posix)"})
	env := execEnv(ctx, r, p, path, marked)
	var got []string
	for _, e := range *entries {
		if e.Level == "warning" {
			got = append(got, e.Source+": "+e.Message)
		}
	}
	return env, got
}

// tail is the environment settings execEnv added after the agent's own.
func tail(env []string, n int) []string { return env[len(env)-n:] }

func TestExecEnvironmentUnparsableIsSkippedWithWarning(t *testing.T) {
	env, warnings := envWarnings(t, []string{"x y", "A-B=c", "B=v"}, "", false)
	want := []string{
		`Exec[env](provider=posix): Cannot understand environment setting "x y"`,
		`Exec[env](provider=posix): Cannot understand environment setting "A-B=c"`,
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
	if got := tail(env, 1); got[0] != "B=v" {
		t.Errorf("tail = %q, want B=v", got)
	}
}

func TestExecEnvironmentRepeatedVariableWarnsAndLastWins(t *testing.T) {
	env, warnings := envWarnings(t, []string{"A=one", "A=two", "A=three"}, "", false)
	want := []string{
		"Exec[env](provider=posix): Overriding environment setting 'A' with 'two'",
		"Exec[env](provider=posix): Overriding environment setting 'A' with 'three'",
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
	if got := tail(env, 1); got[0] != "A=three" {
		t.Errorf("last = %q, want A=three", got)
	}
}

func TestExecEnvironmentPathOverrideWarns(t *testing.T) {
	env, warnings := envWarnings(t, []string{"PATH=/x"}, "/usr/bin:/bin", false)
	want := []string{"Exec[env](provider=posix): Overriding environment setting 'PATH' with '/x'"}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
	if got := tail(env, 2); !slices.Equal(got, []string{"PATH=/usr/bin:/bin", "PATH=/x"}) {
		t.Errorf("tail = %q", got)
	}
	if _, warnings := envWarnings(t, []string{"PATH=/x"}, "", false); len(warnings) != 0 {
		t.Errorf("PATH without path warned: %q", warnings)
	}
}

func TestExecEnvironmentEmptyValueWarnsOncePerVariable(t *testing.T) {
	_, warnings := envWarnings(t, []string{"E=", "E=", "F="}, "", false)
	want := []string{
		"Puppet: Empty environment setting 'E'\n   (file: /site.pp, line: 3)",
		"Exec[env](provider=posix): Overriding environment setting 'E' with ''",
		"Puppet: Empty environment setting 'F'\n   (file: /site.pp, line: 3)",
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
}

// warn_once keys the empty-setting warning on empty_env_var_<var> in
// $unique_warnings (util/logging.rb:181-201), which the whole run shares: a
// guard, the command and a second exec that all set E empty warn once. The
// next run's set is fresh.
func TestExecEmptyEnvironmentWarnsOncePerRun(t *testing.T) {
	ctx, entries := raltest.DebugContext("warning")
	run := WithUniqueWarnings(ctx)
	apply := func(ctx context.Context, title string) {
		t.Helper()
		r := catalog.Resource{Type: "Exec", Title: title, File: "/site.pp", Line: 3,
			Parameters: map[string]any{"command": "/bin/true", "onlyif": "/bin/true", "environment": []any{"E="}}}
		changes, _, err := Exec{}.Apply(sources.WithSources(ctx, sources.Sources{Provider: "Exec[" + title + "](provider=posix)"}), r, false)
		if err != nil || len(changes) != 1 || changes[0].Err != nil {
			t.Fatalf("Apply %s = (%+v, %v)", title, changes, err)
		}
	}
	empties := func() int {
		n := 0
		for _, e := range *entries {
			if e.Level == "warning" && strings.HasPrefix(e.Message, "Empty environment setting") {
				n++
				if want := "Empty environment setting 'E'\n   (file: /site.pp, line: 3)"; e.Source != "Puppet" || e.Message != want {
					t.Errorf("warning = %s: %q, want Puppet: %q", e.Source, e.Message, want)
				}
			}
		}
		return n
	}
	apply(run, "a")
	apply(run, "b")
	if n := empties(); n != 1 {
		t.Errorf("one run: %d empty-setting warnings, want 1", n)
	}
	apply(WithUniqueWarnings(ctx), "c")
	if n := empties(); n != 2 {
		t.Errorf("after a second run: %d empty-setting warnings, want 2", n)
	}
}

// util/logging.rb:193-194: with neither file nor line, warn_once says so.
func TestExecEmptyEnvironmentWarningWithoutLocation(t *testing.T) {
	ctx, entries := raltest.DebugContext("warning")
	r := catalog.Resource{Type: "Exec", Title: "env", Parameters: map[string]any{"command": "true"}}
	execEnv(WithUniqueWarnings(ctx), r, execParams{Environment: []string{"E="}}, "", false)
	if len(*entries) != 1 {
		t.Fatalf("entries = %+v, want one warning", *entries)
	}
	if got, want := (*entries)[0].Message, "Empty environment setting 'E'\n   (file & line not available)"; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
}

func TestExecEnvironmentValueShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		element any
		want    string
	}{
		{"multi-line value keeps both lines", []string{"A=line1\nline2"}, "A=line1\nline2"},
		{"line-anchored: junk line skipped", []string{"junk\nB=v"}, "B=v"},
		{"first equals splits", []string{"A=b=c"}, "A=b=c"},
		{"scalar", "S=1", "S=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, warnings := envWarnings(t, tc.element, "", false)
			if got := tail(env, 1); got[0] != tc.want {
				t.Errorf("env tail = %q, want %q", got, tc.want)
			}
			if len(warnings) != 0 {
				t.Errorf("unexpected warnings %q", warnings)
			}
		})
	}
}

// The junk-line case: the variable is B and the value is everything after
// "B=", so a trailing second assignment stays inside the value.
func TestExecEnvironmentValueRunsToEndOfElement(t *testing.T) {
	env, _ := envWarnings(t, []string{"junk\nB=v\nC=w"}, "", false)
	if got := tail(env, 1); got[0] != "B=v\nC=w" {
		t.Errorf("tail = %q, want B=v\\nC=w", got)
	}
}

func TestExecEnvironmentMarkedIsRedactedInWarnings(t *testing.T) {
	_, warnings := envWarnings(t, []string{"A=one", "A=secret", "bad secret"}, "", true)
	want := []string{
		"Exec[env](provider=posix): Overriding environment setting 'A' with [redacted]",
		"Exec[env](provider=posix): Cannot understand environment setting [redacted]",
	}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
	for _, w := range warnings {
		if strings.Contains(w, "secret") {
			t.Errorf("warning leaks the value: %q", w)
		}
	}
}

func TestExecEnvironmentWithoutPathLeavesAgentPath(t *testing.T) {
	env, _ := envWarnings(t, []string{"A=1"}, "", false)
	if slices.Contains(env, "PATH=") {
		t.Errorf("an empty path set PATH: %q", env)
	}
}

func hasUserEnv(env []string) bool {
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(command.UserEnvVars, name) {
			return true
		}
	}
	return false
}

func TestWithoutUserEnv(t *testing.T) {
	got := command.WithoutUserEnv([]string{"HOME=/h", "A=1", "USER=u", "LOGNAME=l", "HOMEX=2", "B=HOME=3"})
	if want := []string{"A=1", "HOMEX=2", "B=HOME=3"}; !slices.Equal(got, want) {
		t.Errorf("withoutUserEnv = %q, want %q", got, want)
	}
}

// D-23 retired: no child gets HOME, USER or LOGNAME, whichever path starts it.
func TestExecChildHasNoUserEnv(t *testing.T) {
	setLocale(t, "C")
	t.Setenv("HOME", "/nonexistent-home")
	t.Setenv("USER", "someone")
	t.Setenv("LOGNAME", "someone")
	guardOut := filepath.Join(t.TempDir(), "guard.env")

	ch := execChange(t, raltest.ExecRes("env", map[string]any{
		"command":   "env",
		"path":      "/usr/bin:/bin",
		"logoutput": true,
		"onlyif":    "env > " + guardOut,
	}))
	if ch.Err != nil {
		t.Fatalf("exec failed: %v", ch.Err)
	}
	if out := strings.Join(ch.PropertyNotices, "\n"); hasUserEnv(strings.Split(out, "\n")) || !strings.Contains(out, "PATH=") {
		t.Errorf("command env = %q, want PATH and none of %v", out, command.UserEnvVars)
	}
	guard, err := os.ReadFile(guardOut)
	if err != nil {
		t.Fatal(err)
	}
	if hasUserEnv(strings.Split(string(guard), "\n")) {
		t.Errorf("guard env has a user variable: %q", guard)
	}

	_, out, err := command.FileOutputRunner{}.Run(context.Background(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if hasUserEnv(strings.Split(out, "\n")) || !strings.Contains(out, "PATH=") {
		t.Errorf("runArgv env = %q, want PATH and none of %v", out, command.UserEnvVars)
	}
}
