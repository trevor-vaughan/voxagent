// Command gatekit holds the parsing and judging the gate scripts need, so that
// the scripts stay shell and the repository needs no second language.
//
//	gatekit compare-reports RUBY VOXAGENT ALLOWLIST   compare two reports (G1, G5)
//	gatekit run-to-run RUN1 RUN2 ALLOWLIST            check the run_to_run allowlist
//	gatekit g5-reference REPORT CATALOG INSTALL REMOVE LATEST MISSING
//	                                                  check G5's reference run exercised its fixture
//	gatekit fleet-axes RAW MESSAGE TIER PACKAGES      check a node's catalog carries its hiera axes
//	gatekit fleet-compliance RAW EXPECTED             check compliance enforcement reached a catalog
//	gatekit fleet-facts FACTS NODE                    check a fact capture is complete
//	gatekit pdb-exported URL TITLE                    ask OpenVoxDB whether TITLE is exported
//	gatekit manifest-mutate MANIFEST ORIGINAL MUTATED replace the one occurrence of ORIGINAL
//	gatekit fleet-certs DIR                           report when the fleet's certificates and CRL expire
//
// Every subcommand exits 0 for the expected outcome, 1 for a finding, and 2
// when it could not run -- bad arguments, an unreadable file, or input that is
// not the shape it judges. A 2 is never a statement about what was judged.
//
// It is a development tool, not part of voxagent: it is built into bin/ by
// `task build` and called only by the gates.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/voxagent/voxagent/internal/gatekit/g5ref"
	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
	"github.com/voxagent/voxagent/internal/gatekit/reportcmp"
)

const (
	exitExpected = 0
	exitFinding  = 1
	exitFailure  = 2
)

const usage = `gatekit — parsing and judging for the gate scripts

  gatekit compare-reports RUBY VOXAGENT ALLOWLIST
  gatekit run-to-run RUN1 RUN2 ALLOWLIST
  gatekit g5-reference REPORT CATALOG INSTALL REMOVE LATEST MISSING
  gatekit fleet-axes RAW MESSAGE TIER PACKAGES
  gatekit fleet-compliance RAW EXPECTED
  gatekit fleet-facts FACTS NODE
  gatekit pdb-exported URL TITLE
  gatekit manifest-mutate MANIFEST ORIGINAL MUTATED
  gatekit fleet-certs DIR

Exit 0 is the expected outcome, 1 a finding, 2 could not run.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// subcommands maps each name to its argument count and implementation.
var subcommands = map[string]struct {
	args int
	run  func(args []string, stdout io.Writer) (int, error)
}{
	"compare-reports":  {3, compareReports},
	"run-to-run":       {3, runToRun},
	"g5-reference":     {6, g5Reference},
	"fleet-axes":       {4, fleetAxes},
	"fleet-compliance": {2, fleetCompliance},
	"fleet-facts":      {2, fleetFacts},
	"pdb-exported":     {2, pdbExported},
	"manifest-mutate":  {3, manifestMutate},
	"fleet-certs":      {1, fleetCerts},
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitFailure
	}
	sub, ok := subcommands[args[0]]
	if !ok || len(args)-1 != sub.args {
		fmt.Fprint(stderr, usage)
		return exitFailure
	}
	code, err := sub.run(args[1:], stdout)
	if err != nil {
		fmt.Fprintf(stderr, "gatekit %s: %v\n", args[0], err)
		return exitFailure
	}
	return code
}

// decodeAll decodes every path, returning the first failure.
func decodeAll(paths ...string) ([]any, error) {
	docs := make([]any, len(paths))
	for i, p := range paths {
		doc, err := pycompat.DecodeFile(p)
		if err != nil {
			return nil, fmt.Errorf("could not read an input: %w", err)
		}
		docs[i] = doc
	}
	return docs, nil
}

func compareReports(args []string, stdout io.Writer) (int, error) {
	docs, err := decodeAll(args...)
	if err != nil {
		return 0, err
	}
	allow, err := reportcmp.ParseAllowlist(docs[2])
	if err != nil {
		return 0, err
	}
	result, err := reportcmp.Compare(docs[0], docs[1], allow)
	if err != nil {
		return 0, err
	}
	result.Write(stdout)
	if len(result.Divergences) > 0 {
		return exitFinding, nil
	}
	return exitExpected, nil
}

func runToRun(args []string, stdout io.Writer) (int, error) {
	docs, err := decodeAll(args...)
	if err != nil {
		return 0, err
	}
	allow, err := reportcmp.ParseAllowlist(docs[2])
	if err != nil {
		return 0, err
	}
	clean, err := reportcmp.RunToRun(docs[0], docs[1], allow, stdout)
	if err != nil {
		return 0, err
	}
	if !clean {
		return exitFinding, nil
	}
	return exitExpected, nil
}

func g5Reference(args []string, stdout io.Writer) (int, error) {
	docs, err := decodeAll(args[0], args[1])
	if err != nil {
		return 0, err
	}
	complaints, err := g5ref.Check(docs[0], docs[1], g5ref.Fixture{
		Install: args[2], Remove: args[3], Latest: args[4], Missing: args[5],
	})
	if err != nil {
		return 0, err
	}
	for _, c := range complaints {
		fmt.Fprintln(stdout, c)
	}
	if len(complaints) > 0 {
		return exitFinding, nil
	}
	return exitExpected, nil
}
