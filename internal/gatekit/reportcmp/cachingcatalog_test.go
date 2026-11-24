package reportcmp

import (
	"strings"
	"testing"
)

// hostReport is a report for host holding one log entry.
func hostReport(host, source, level, message string) string {
	return `{"host":"` + host + `","logs":[{"source":"` + source + `","level":"` + level + `","message":"` + message + `"}]}`
}

func TestCachingCatalogLogIsDroppedFromRubyOnly(t *testing.T) {
	ruby := hostReport("ruby.example.test", "Puppet", "info", "Caching catalog for ruby.example.test")
	result, err := Compare(decode(t, ruby), decode(t, `{"host":"ruby.example.test","logs":[]}`), Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Divergences) != 0 {
		t.Errorf("divergences %v, want none: Ruby's catalog-cache line is D-67", result.Divergences)
	}
}

func TestCachingCatalogDropIsExact(t *testing.T) {
	const host = "ruby.example.test"
	for _, tc := range []struct{ name, ruby, voxagent, want string }{
		{"another host's name",
			hostReport(host, "Puppet", "info", "Caching catalog for vox.example.test"), `{"host":"` + host + `","logs":[]}`,
			"logs (only in ruby)"},
		{"a longer message",
			hostReport(host, "Puppet", "info", "Caching catalog for "+host+" again"), `{"host":"` + host + `","logs":[]}`,
			"logs (only in ruby)"},
		{"another level",
			hostReport(host, "Puppet", "notice", "Caching catalog for "+host), `{"host":"` + host + `","logs":[]}`,
			"logs (only in ruby)"},
		{"another source",
			hostReport(host, "/Stage[main]", "info", "Caching catalog for "+host), `{"host":"` + host + `","logs":[]}`,
			"logs (only in ruby)"},
		{"voxagent's side",
			`{"host":"` + host + `","logs":[]}`, hostReport(host, "Puppet", "info", "Caching catalog for "+host),
			"logs (only in voxagent)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Compare(decode(t, tc.ruby), decode(t, tc.voxagent), Allowlist{})
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, d := range result.Divergences {
				paths = append(paths, d.Path)
			}
			if strings.Join(paths, ",") != tc.want {
				t.Errorf("divergences %q, want only %q", paths, tc.want)
			}
		})
	}
}
