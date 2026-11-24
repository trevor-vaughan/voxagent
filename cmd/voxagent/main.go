// Command voxagent applies a Puppet catalog: from a file (apply), or fetched
// from an OpenVox server in one agent run (agent, agent.go).
//
// This is the apply spine: it reads a catalog from a file, refuses anything it
// cannot fully implement, applies the rest, writes a report_format 12 report,
// and exits on a contract that matches measured Puppet.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

const usage = "usage: voxagent apply --catalog <file> [--detailed-exitcodes] [--verbose] [--debug] [--noop] [--report-dir <dir>] [--ruby <path>] [--libdir <dir>] [--vardir <dir>] [--no-ruby] [--extension-timeout <d>] [--extension-evaluate-timeout <d>]"

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "apply":
			os.Exit(applyCommand(os.Args[2:]))
		case "agent":
			os.Exit(agentCommand(os.Args[2:]))
		case "units":
			os.Exit(unitsCommand(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	fmt.Fprintln(os.Stderr, usage)
	fmt.Fprintln(os.Stderr, agentUsage)
	fmt.Fprintln(os.Stderr, unitsUsage)
	// No --detailed-exitcodes has been parsed at this point, so this uses
	// the plain convention: 2 is could-not-run.
	fmt.Fprintf(os.Stderr, "voxagent: outcome=%s\n", report.OutcomeCouldNotRun)
	os.Exit(2)
}

func applyCommand(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	catalogPath := fs.String("catalog", "", "path to a catalog JSON file")
	detailed := fs.Bool("detailed-exitcodes", false, "use Puppet's exit bitmask")
	verbose := fs.Bool("verbose", false, "also log info-level messages, to the console and the report")
	debug := fs.Bool("debug", false, "also log debug-level messages: all of them to the console, those within 16 MiB to the report; implies --verbose")
	noop := fs.Bool("noop", false, "report what would change without changing it")
	reportDir := fs.String("report-dir", "", "directory to write the run report into")
	ruby := fs.String("ruby", "/opt/puppetlabs/puppet/bin/ruby", "Ruby interpreter for the extension host")
	libdir := fs.String("libdir", "/opt/puppetlabs/puppet/cache/lib", "plugin directory the extension host loads types from")
	vardir := fs.String("vardir", "/opt/puppetlabs/puppet/cache", "Puppet's vardir, shared with the extension host; backup => 'puppet' writes the clientbucket under it. The default is root's; a non-root run that backs up to 'puppet' (every concat File) needs a writable one")
	noRuby := fs.Bool("no-ruby", false, "refuse catalogs that need the extension host")
	extTimeout := fs.Duration("extension-timeout", 5*time.Minute, "time limit for each extension host request other than evaluate")
	extEvalTimeout := fs.Duration("extension-evaluate-timeout", time.Hour, "time limit for each extension host evaluate request")
	if err := fs.Parse(args); err != nil {
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	// Go's flag package stops at the first non-flag argument and parks the rest
	// in fs.Args(), where nothing would read them. Leaving them unread makes the
	// dropped tokens silent: `apply --catalog c junk --noop` parsed as a live
	// run, applied the catalog, and exited 0. A flag the operator passed and the
	// tool ignored is the one thing a run must never do quietly.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "voxagent: unexpected argument %q; no flag after it was parsed\n",
			fs.Arg(0))
		fmt.Fprintln(os.Stderr, usage)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	if *catalogPath == "" {
		fmt.Fprintln(os.Stderr, "voxagent: --catalog is required")
		fmt.Fprintln(os.Stderr, usage)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	for _, d := range []struct {
		name  string
		value time.Duration
	}{{"--extension-timeout", *extTimeout}, {"--extension-evaluate-timeout", *extEvalTimeout}} {
		if d.value <= 0 {
			fmt.Fprintf(os.Stderr, "voxagent: %s must be positive\n", d.name)
			return finish(report.Status{CouldNotRun: true}, *detailed)
		}
	}

	raw, err := os.ReadFile(*catalogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: cannot read catalog: %v\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	level := logLevel(*verbose, *debug)
	console := &puppetConsole{level: level, stdout: os.Stdout, stderr: os.Stderr}
	st, _ := applyCatalog(raw, applyOptions{
		command: "apply", level: level, logger: runlog.New(level, console.record, console.print),
		noop: *noop, noRuby: *noRuby,
		reportDir: *reportDir, ruby: *ruby, libdir: *libdir, vardir: *vardir,
		timeouts: extension.Timeouts{Evaluate: *extEvalTimeout, Other: *extTimeout},
		console:  console,
	})
	return finish(st, *detailed)
}

// finish prints the outcome line and returns the process exit status.
//
// The outcome line is unconditional because the meaning of the bare integer
// depends on --detailed-exitcodes, and an exit status is read out of band from
// the invocation that produced it -- by a monitoring wrapper, or by a human
// reading a log weeks later.
func finish(s report.Status, detailed bool) int {
	fmt.Fprintf(os.Stderr, "voxagent: outcome=%s\n", s.Outcome())
	return s.Exit(detailed)
}

func writeReport(dir string, rep *report.Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	name := filepath.Join(dir, time.Now().UTC().Format("20060102150405.000000000")+".json")
	if err := os.WriteFile(name, b, 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
