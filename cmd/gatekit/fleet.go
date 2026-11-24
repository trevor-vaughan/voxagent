package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/voxagent/voxagent/internal/gatekit/fleet"
)

func printLines(w io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

func fleetAxes(args []string, stdout io.Writer) (int, error) {
	catalog, err := fleet.LoadCatalog(args[0])
	if err != nil {
		return 0, fmt.Errorf("could not read the catalog: %w", err)
	}
	lines, err := fleet.Axes(catalog, fleet.AxesWant{Message: args[1], Tier: args[2], Packages: args[3]})
	if err != nil {
		return 0, fmt.Errorf("could not read the catalog: %w", err)
	}
	printLines(stdout, lines)
	if len(lines) > 0 {
		return exitFinding, nil
	}
	return exitExpected, nil
}

func fleetCompliance(args []string, stdout io.Writer) (int, error) {
	catalog, err := fleet.LoadCatalog(args[0])
	if err != nil {
		return 0, fmt.Errorf("could not read the catalog: %w", err)
	}
	outcome, lines, err := fleet.Compliance(catalog, args[1])
	if err != nil {
		return 0, fmt.Errorf("could not read the catalog: %w", err)
	}
	printLines(stdout, lines)
	switch outcome {
	case fleet.Enforced:
		return exitExpected, nil
	case fleet.NotEnforced:
		return exitFinding, nil
	default:
		return exitFailure, nil
	}
}

func fleetFacts(args []string, stdout io.Writer) (int, error) {
	problem, err := fleet.CheckFacts(args[0], args[1])
	if err != nil {
		return 0, err
	}
	if problem != "" {
		fmt.Fprintln(stdout, problem)
		return exitFinding, nil
	}
	return exitExpected, nil
}

// pdbExported exits 0 when the title is exported, 1 when OpenVoxDB answered
// that it is not, and 2 when the query failed. fleet_wait_for_export polls on
// either non-zero status; the two differ for a person reading the run.
func pdbExported(args []string, _ io.Writer) (int, error) {
	err := fleet.Exported(args[0], args[1])
	switch {
	case err == nil:
		return exitExpected, nil
	case errors.Is(err, fleet.ErrNotExported):
		return exitFinding, nil
	default:
		return 0, err
	}
}

func manifestMutate(args []string, _ io.Writer) (int, error) {
	if err := fleet.MutateManifest(args[0], args[1], args[2]); err != nil {
		return 0, err
	}
	return exitExpected, nil
}

// fleetCerts exits 1 when a certificate or the CRL has expired: the fleet's TLS
// gate cannot pass on it. Expiry within the warning window is reported but is
// not a finding.
func fleetCerts(args []string, stdout io.Writer) (int, error) {
	entries, err := fleet.CheckCerts(args[0], time.Now())
	if err != nil {
		return 0, err
	}
	if fleet.WriteCertReport(stdout, entries) {
		return exitFinding, nil
	}
	return exitExpected, nil
}
