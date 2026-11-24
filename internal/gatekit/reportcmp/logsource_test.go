package reportcmp

import (
	"strings"
	"testing"
)

// The one source the SIMP gate registers (D-8), and sources that a prefix,
// a sibling property or fnmatch would confuse with it.
const (
	chronyKeys = "/Stage[main]/Chrony::Config/File[/etc/chrony.keys]/ensure"
	// fnmatch reads [main] and [/etc/chrony.keys] as one-character classes,
	// so this source matches chronyKeys read as a pattern.
	chronyKeysAsGlob = "/Stagem/Chrony::Config/File//ensure"
)

func logReport(source, message string) string {
	return `{"logs":[{"source":"` + source + `","message":"` + message + `","level":"notice"}]}`
}

func TestParseAllowlistReadsALogSourceKey(t *testing.T) {
	allow, err := ParseAllowlist(decode(t, `{"cross_implementation":{
		"logs (source: `+chronyKeys+`)": "D-8",
		"puppet_version": "reason"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(allow.LogSources) != 1 || allow.LogSources[0] != chronyKeys {
		t.Errorf("LogSources = %q, want [%q]", allow.LogSources, chronyKeys)
	}
	if len(allow.CrossImplementation) != 1 || allow.CrossImplementation[0] != "puppet_version" {
		t.Errorf("CrossImplementation = %q, want only the path pattern", allow.CrossImplementation)
	}
}

func TestParseAllowlistRefusesAMalformedLogSourceKey(t *testing.T) {
	for _, key := range []string{
		"logs (source: )",
		"logs (source: " + chronyKeys,
	} {
		_, err := ParseAllowlist(decode(t, `{"cross_implementation":{"`+key+`":"r"}}`))
		if err == nil || !strings.Contains(err.Error(), "logs (source: ") {
			t.Errorf("ParseAllowlist(%q) err = %v, want a refusal naming the key form", key, err)
		}
	}
}

func TestALogSourceKeyDropsThatSourcesLogsOnBothSides(t *testing.T) {
	allow := Allowlist{LogSources: []string{chronyKeys}}
	for _, tc := range []struct{ name, ruby, voxagent string }{
		{"differing messages", logReport(chronyKeys, "changed [redacted] to [redacted]"), logReport(chronyKeys, "defined content as x")},
		{"only in ruby", logReport(chronyKeys, "a"), `{"logs":[]}`},
		{"only in voxagent", `{"logs":[]}`, logReport(chronyKeys, "a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Compare(decode(t, tc.ruby), decode(t, tc.voxagent), allow)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Divergences) != 0 {
				t.Errorf("divergences %v, want none: the source is registered", result.Divergences)
			}
		})
	}
}

func TestALogSourceKeyLeavesEveryOtherSourceCompared(t *testing.T) {
	allow := Allowlist{LogSources: []string{chronyKeys}}
	for _, source := range []string{
		"/Stage[main]/Chrony::Config/File[/etc/chrony.keys]/content",
		"/Stage[main]/Chrony::Config/File[/etc/chrony.keys]",
		chronyKeys + "/extra",
		"x" + chronyKeys,
		chronyKeysAsGlob,
		"Puppet",
	} {
		t.Run(source, func(t *testing.T) {
			result, err := Compare(decode(t, logReport(source, "a")), decode(t, logReport(source, "b")), allow)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, d := range result.Divergences {
				paths = append(paths, d.Path)
			}
			if strings.Join(paths, ",") != "logs (only in ruby),logs (only in voxagent)" {
				t.Errorf("divergences %q, want the differing message on %q reported both ways", paths, source)
			}
		})
	}
}
