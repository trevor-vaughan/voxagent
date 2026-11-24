package main

import (
	"strings"
	"testing"
)

// --no-ruby refuses a Cron the route delegates (here, a provider other than
// crontab) exactly as it refuses any other delegated type, and applies
// nothing.
func TestCronNoRubyRefusesDelegated(t *testing.T) {
	o := applyOptions{noRuby: true, level: "notice", environ: environOf(nil)}
	stderr, couldNot := applyInProcess(t, []map[string]any{
		ext("Cron", "job", map[string]any{"command": "/bin/true", "provider": "other"}),
	}, o)
	if !couldNot || !strings.Contains(stderr, "voxagent: --no-ruby: type(s) cron need the extension host; nothing applied") {
		t.Fatalf("could-not-run %v:\n%s", couldNot, stderr)
	}
}
