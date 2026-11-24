package main

import (
	"bytes"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
)

// The console writes each line with Ruby's puts (util/log/destinations.rb:164),
// which adds no newline to a message that already ends in one (M15's
// "chpasswd said ...\n").
func TestConsoleLineEndsWithOneNewline(t *testing.T) {
	var stderr bytes.Buffer
	c := &puppetConsole{level: "notice", stdout: &bytes.Buffer{}, stderr: &stderr}
	c.print(report.LogEntry{Level: "err", Source: "Puppet", Message: "chpasswd said a\nb\n"})
	c.print(report.LogEntry{Level: "err", Source: "Puppet", Message: "plain"})
	if got, want := stderr.String(), "Error: chpasswd said a\nb\nError: plain\n"; got != want {
		t.Errorf("console = %q, want %q", got, want)
	}
}
