package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/report"
)

// logLevel is the run's log level: Puppet's default is notice, --verbose
// lowers it to info and --debug to debug, whichever order they were given in.
func logLevel(verbose, debug bool) string {
	switch {
	case debug:
		return "debug"
	case verbose:
		return "info"
	}
	return "notice"
}

// puppetConsole prints the run's log as Puppet's console destination does
// (util/log/destinations.rb): "Notice: <message>" for source Puppet,
// "Notice: <source>: <message>" otherwise, warning and worse on stderr, the
// rest on stdout, no colour. level is the run's level, for the console and the
// report alike, as Puppet's report is a log destination at the run's level.
type puppetConsole struct {
	level          string // "notice", "info" or "debug"
	stdout, stderr io.Writer
	entries        []report.LogEntry // what record kept, for the report once it exists
	scrub          *redact.Scrubber  // the run's, once the catalog is decoded; nil scrubs nothing
}

// record keeps a line logged before the report exists, for the report's first
// entries, and prints it. After the report exists the logger records into the
// report instead, whose observer is print.
func (c *puppetConsole) record(e report.LogEntry) {
	c.entries = append(c.entries, e)
	c.print(e)
}

func (c *puppetConsole) shows(level string) bool {
	switch level {
	case "debug":
		return c.level == "debug"
	case "info":
		return c.level != "notice"
	}
	return true
}

// consoleLevels are Puppet's console names; a level not listed is printed
// under its own name on stderr.
var consoleLevels = map[string]string{
	"debug": "Debug", "info": "Info", "notice": "Notice",
	"warning": "Warning", "err": "Error", "alert": "Alert", "emerg": "Emergency", "crit": "Critical",
}

func (c *puppetConsole) print(e report.LogEntry) {
	if !c.shows(e.Level) {
		return
	}
	msg := c.scrub.Scrub(e.Message)
	if e.Source != "Puppet" {
		msg = e.Source + ": " + msg
	}
	name, known := consoleLevels[e.Level]
	if !known {
		name = e.Level
	}
	w := c.stderr
	if e.Level == "debug" || e.Level == "info" || e.Level == "notice" {
		w = c.stdout
	}
	// Ruby's puts adds the newline only when the line lacks one.
	line := name + ": " + msg
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	io.WriteString(w, line)
}

// stderrf prints a command line that may carry a catalog value to os.Stderr,
// scrubbed by s.
func stderrf(s *redact.Scrubber, format string, a ...any) {
	fmt.Fprint(os.Stderr, s.Scrub(fmt.Sprintf(format, a...)))
}
