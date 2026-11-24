package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/report"
)

func fakeFacter(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type factsWrapper struct {
	Name       string         `json:"name"`
	Values     map[string]any `json:"values"`
	Timestamp  string         `json:"timestamp"`
	Expiration string         `json:"expiration"`
}

func runFacts(t *testing.T, c FactsConfig) factsWrapper {
	t.Helper()
	out, err := Facts(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	var w factsWrapper
	if err := json.Unmarshal(out, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestFactsWrapperShape(t *testing.T) {
	now := time.Date(2026, 11, 16, 2, 37, 51, 948459197, time.UTC)
	w := runFacts(t, FactsConfig{Facter: fakeFacter(t, "facter-basic.sh"), Certname: "a.example.test", Now: func() time.Time { return now }})
	if w.Name != "a.example.test" {
		t.Errorf("name = %q", w.Name)
	}
	if w.Timestamp != "2026-11-16T02:37:51.948459197+00:00" {
		t.Errorf("timestamp = %q", w.Timestamp)
	}
	if w.Expiration != "2026-11-16T03:07:51.948459197+00:00" {
		t.Errorf("expiration = %q", w.Expiration)
	}
	if w.Values["kernel"] != "Linux" {
		t.Errorf("facter values not carried: %v", w.Values)
	}
}

func TestFactsAddsAgentFacts(t *testing.T) {
	for _, noop := range []bool{false, true} {
		w := runFacts(t, FactsConfig{Facter: fakeFacter(t, "facter-basic.sh"), Certname: "a.example.test", Noop: noop})
		want := map[string]any{
			"implementation": "openvox",
			"clientcert":     "a.example.test",
			"clientversion":  report.PuppetVersion,
			"puppetversion":  report.PuppetVersion,
			"clientnoop":     noop,
		}
		for k, v := range want {
			if w.Values[k] != v {
				t.Errorf("noop=%v: %s = %v, want %v", noop, k, w.Values[k], v)
			}
		}
	}
}

func TestFactsPassesPluginDirsOnlyWhenPresent(t *testing.T) {
	argv := func(c FactsConfig) []string {
		c.Facter = fakeFacter(t, "facter-argv.sh")
		return strings.Split(strings.TrimSpace(runFacts(t, c).Values["argv"].(string)), "\n")
	}
	lib, ext := t.TempDir(), t.TempDir()

	if got := argv(FactsConfig{LibDir: lib, PluginFactDest: filepath.Join(ext, "missing")}); len(got) != 1 || got[0] != "--json" {
		t.Errorf("absent dirs: argv = %q", got)
	}
	if err := os.Mkdir(filepath.Join(lib, "facter"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []string{"--json", "--custom-dir", filepath.Join(lib, "facter"), "--external-dir", ext}
	if got := argv(FactsConfig{LibDir: lib, PluginFactDest: ext}); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("present dirs: argv = %q, want %q", got, want)
	}
	// A regular file is not a directory.
	f := filepath.Join(ext, "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := argv(FactsConfig{PluginFactDest: f}); len(got) != 1 {
		t.Errorf("file as dir: argv = %q", got)
	}
}

func TestFactsNonZeroExit(t *testing.T) {
	_, err := Facts(context.Background(), FactsConfig{Facter: fakeFacter(t, "facter-fail.sh")})
	if err == nil || !strings.Contains(err.Error(), "boom detail") {
		t.Fatalf("err = %v, want stderr included", err)
	}
}

func TestFactsNotJSON(t *testing.T) {
	_, err := Facts(context.Background(), FactsConfig{Facter: fakeFacter(t, "facter-notjson.sh")})
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("err = %v", err)
	}
}

// json.Unmarshal of `null` into a map leaves it nil; adding the agent facts
// to it must not panic.
func TestFactsNullRefused(t *testing.T) {
	_, err := Facts(context.Background(), FactsConfig{Facter: fakeFacter(t, "facter-null.sh")})
	if err == nil || !strings.Contains(err.Error(), "not a JSON object") {
		t.Fatalf("err = %v", err)
	}
}

// facter's stderr goes into an error that is printed and logged, so terminal
// escapes in it are escaped.
func TestFactsStderrSanitised(t *testing.T) {
	_, err := Facts(context.Background(), FactsConfig{Facter: fakeFacter(t, "facter-escfail.sh")})
	if err == nil {
		t.Fatal("no error")
	}
	if msg := err.Error(); strings.ContainsAny(msg, "\x1b\x07") || !strings.Contains(msg, `\x1b[31mred`) {
		t.Fatalf("err = %q, want the escapes escaped", msg)
	}
}

func TestFactsRelativeFacterRefused(t *testing.T) {
	for _, f := range []string{"facter", "./testdata/facter-basic.sh", ""} {
		if _, err := Facts(context.Background(), FactsConfig{Facter: f}); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("facter %q: err = %v, want refusal", f, err)
		}
	}
}

func TestFactsCleanEnvironment(t *testing.T) {
	t.Setenv("RUBYLIB", "/tmp/evil")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("FACTERLIB", "/tmp/evil")
	w := runFacts(t, FactsConfig{Facter: fakeFacter(t, "facter-env.sh")})
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(w.Values["env"].(string)), "\n") {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	want := map[string]string{"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "LANG": "C", "HOME": os.Getenv("HOME")}
	if len(got) != len(want) {
		t.Fatalf("env keys = %v, want exactly PATH, LANG, HOME", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestFactsOutputCap(t *testing.T) {
	_, err := Facts(context.Background(), FactsConfig{Facter: fakeFacter(t, "facter-huge.sh")})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want cap error", err)
	}
}

func TestFactsNoisyStderrSucceeds(t *testing.T) {
	w := runFacts(t, FactsConfig{Facter: fakeFacter(t, "facter-noisy.sh")})
	if w.Values["kernel"] != "Linux" {
		t.Errorf("values = %v", w.Values)
	}
}
