package main

import (
	"fmt"
	"io"

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
	msg := e.Message
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
	fmt.Fprintf(w, "%s: %s\n", name, msg)
}
