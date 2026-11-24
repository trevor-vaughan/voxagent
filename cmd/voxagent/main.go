// Command voxagent applies a Puppet catalog.
//
// This is the apply spine: it reads a catalog from a file, refuses anything it
// cannot fully implement, applies the rest, writes a report_format 12 report,
// and exits on a contract that matches measured Puppet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/node"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/transaction"
)

const usage = "usage: voxagent apply --catalog <file> [--detailed-exitcodes] [--noop] [--report-dir <dir>]"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "apply" {
		fmt.Fprintln(os.Stderr, usage)
		// No --detailed-exitcodes has been parsed at this point, so this uses
		// the plain convention: 2 is could-not-run.
		fmt.Fprintf(os.Stderr, "voxagent: outcome=%s\n", report.OutcomeCouldNotRun)
		os.Exit(2)
	}
	os.Exit(applyCommand(os.Args[2:]))
}

func applyCommand(args []string) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	catalogPath := fs.String("catalog", "", "path to a catalog JSON file")
	detailed := fs.Bool("detailed-exitcodes", false, "use Puppet's exit bitmask")
	noop := fs.Bool("noop", false, "report what would change without changing it")
	reportDir := fs.String("report-dir", "", "directory to write the run report into")
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

	raw, err := os.ReadFile(*catalogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: cannot read catalog: %v\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	cat, err := catalog.Decode(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	registry := ral.NewRegistry(ral.Notify{}, ral.File{}, ral.Exec{}, ral.Package{})

	// Pre-flight: refuse the whole catalog rather than applying part of it. A
	// partially applied node produces state-diff divergences that are artifacts
	// of coverage rather than defects, which would poison the oracle.
	if gaps := ral.Preflight(registry, cat); len(gaps) > 0 {
		resources := map[string]struct{}{}
		for _, g := range gaps {
			fmt.Fprintf(os.Stderr, "voxagent: %s\n", g)
			resources[g.Ref] = struct{}{}
		}
		fmt.Fprintf(os.Stderr, "voxagent: %d gap(s) across %d resource(s); nothing applied\n",
			len(gaps), len(resources))
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	// Validation: refuse what Puppet itself refuses while converting the
	// catalog -- a relative file path fails Ruby's whole run with "Could not
	// run". It runs after pre-flight so an operator sees coverage gaps, the
	// actionable fact, first.
	if invalid := ral.Validate(registry, cat); len(invalid) > 0 {
		for _, i := range invalid {
			fmt.Fprintf(os.Stderr, "voxagent: %s\n", i)
		}
		fmt.Fprintf(os.Stderr, "voxagent: %d invalid resource(s); nothing applied\n", len(invalid))
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	g, err := transaction.BuildGraph(cat, registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v; nothing applied\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	// The report's host is the certname of the machine that applied the catalog,
	// not the node name the catalog was compiled for. Measured: Ruby does the
	// same, reporting Puppet[:certname] even when handed another node's catalog.
	// Using cat.Name instead would be more descriptive of the catalog and would
	// file voxagent's reports under a different key than Ruby's for the same
	// machine, which is the kind of "more informative" that breaks a drop-in.
	certname, err := node.Certname()
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: cannot determine this node's certname: %v\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}

	rep := report.New(certname, cat.Environment, *noop)
	rep.CatalogUUID = cat.CatalogUUID
	rep.ConfigurationVersion = cat.Version

	tx := &transaction.Transaction{Registry: registry, Noop: *noop}
	if err := tx.Run(context.Background(), cat, g, rep); err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v\n", err)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	rep.Finalize()

	if *reportDir != "" {
		if err := writeReport(*reportDir, rep); err != nil {
			// The run applied changes and cannot say what it did. The claim a
			// run makes is its report, so with no report there is no claim --
			// but the message must say the machine was modified, because
			// "could not run" would otherwise imply nothing happened.
			fmt.Fprintf(os.Stderr,
				"voxagent: the catalog was applied but the report could not be written: %v\n", err)
			fmt.Fprintln(os.Stderr,
				"voxagent: the machine WAS modified; this run cannot say how")
			return finish(report.Status{CouldNotRun: true}, *detailed)
		}
	}

	return finish(rep.RunStatus(), *detailed)
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
