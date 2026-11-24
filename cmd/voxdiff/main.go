// Command voxdiff compares Puppet catalogs and flags constructs that break
// compile determinism.
//
//	voxdiff compare -node NAME [flags]   compile twice, report divergence
//	voxdiff scan PATH...                 report instability findings
//
// compare exits 0 when the catalogs match, 1 when they diverge, and 2 on
// operational failure. That distinction matters in CI: a divergence is a
// result, a failure to compile is not.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/compile"
	"github.com/voxagent/voxagent/internal/instability"
)

const (
	exitIdentical = 0
	exitDiverged  = 1
	exitFailure   = 2
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitFailure)
	}
	switch os.Args[1] {
	case "compare":
		os.Exit(runCompare(os.Args[2:]))
	case "scan":
		os.Exit(runScan(os.Args[2:], os.Stdout, os.Stderr))
	default:
		usage()
		os.Exit(exitFailure)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `voxdiff — Puppet catalog differential harness

  voxdiff compare -node NAME [flags]   compile a node twice and report divergence
  voxdiff scan PATH...                 report constructs that break determinism

Run "voxdiff compare -h" for compare flags.
`)
}

func runCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	node := fs.String("node", "", "node name to compile (required)")
	leftBin := fs.String("left", "puppet", "puppet binary for the left compile")
	rightBin := fs.String("right", "puppet", "puppet binary for the right compile")
	envPath := fs.String("environmentpath", "", "path passed as --environmentpath")
	env := fs.String("environment", "production", "environment name")
	confDir := fs.String("confdir", "", "path passed as --confdir")
	varDir := fs.String("vardir", "", "path passed as --vardir")
	codeDir := fs.String("codedir", "", "path passed as --codedir")
	ignoreLoc := fs.Bool("ignore-source-locations", false, "ignore resource file and line differences")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	_ = fs.Parse(args)

	if *node == "" {
		fmt.Fprintln(os.Stderr, "voxdiff compare: -node is required")
		return exitFailure
	}

	mk := func(bin string) compile.Runner {
		return compile.Runner{
			PuppetBin:       bin,
			EnvironmentPath: *envPath,
			Environment:     *env,
			ConfDir:         *confDir,
			VarDir:          *varDir,
			CodeDir:         *codeDir,
		}
	}

	ctx := context.Background()
	left, err := compileAndDecode(ctx, mk(*leftBin), *node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxdiff compare: left: %v\n", err)
		return exitFailure
	}
	right, err := compileAndDecode(ctx, mk(*rightBin), *node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxdiff compare: right: %v\n", err)
		return exitFailure
	}

	rep := catalog.Diff(left, right, catalog.Options{IgnoreSourceLocations: *ignoreLoc})

	if *asJSON {
		if err := renderJSON(os.Stdout, rep); err != nil {
			fmt.Fprintf(os.Stderr, "voxdiff compare: %v\n", err)
			return exitFailure
		}
	} else {
		renderText(os.Stdout, rep)
	}
	return exitCodeFor(rep)
}

func compileAndDecode(ctx context.Context, r compile.Runner, node string) (*catalog.Catalog, error) {
	raw, err := r.Compile(ctx, node)
	if err != nil {
		return nil, err
	}
	c, err := catalog.Decode(raw)
	if err != nil {
		return nil, err
	}
	catalog.Normalize(c)
	return c, nil
}

// runScan walks each root in args, scanning .pp manifests and .rb functions
// for instability findings.
//
// A file that cannot be read or cannot be fully scanned does not abort the
// walk: its error is accumulated and the walk continues, so one bad file in a
// fleet-sized tree does not throw away findings already gathered from every
// other file. Findings are printed to stdout; accumulated errors are printed
// to stderr afterward, keeping the two streams separable for tooling that
// parses findings. If any error occurred, runScan returns exitFailure even
// when findings were also produced: an error means the scan was incomplete,
// and reporting exitDiverged ("findings, scan complete") would overstate
// confidence in a result that is supposed to be exhaustive.
func runScan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "voxdiff scan: at least one path is required")
		return exitFailure
	}
	var findings []instability.Finding
	var scanErrs []error
	for _, root := range args {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				scanErrs = append(scanErrs, err)
				return nil
			}
			if d.IsDir() {
				return nil
			}
			var scanFn func(string, []byte) ([]instability.Finding, error)
			switch {
			case strings.HasSuffix(path, ".pp"):
				scanFn = instability.ScanManifest
			case strings.HasSuffix(path, ".rb") && strings.Contains(path, "/lib/puppet/functions/"):
				scanFn = instability.ScanRubyFunction
			default:
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				scanErrs = append(scanErrs, err)
				return nil
			}
			results, err := scanFn(path, src)
			if err != nil {
				scanErrs = append(scanErrs, err)
				return nil
			}
			findings = append(findings, results...)
			return nil
		})
		if err != nil {
			scanErrs = append(scanErrs, err)
		}
	}
	for _, f := range findings {
		fmt.Fprintf(stdout, "%s:%d: %s: %s\n", f.File, f.Line, f.Kind, f.Excerpt)
	}
	fmt.Fprintf(stdout, "\n%d finding(s)\n", len(findings))
	for _, e := range scanErrs {
		fmt.Fprintf(stderr, "voxdiff scan: %v\n", e)
	}
	if len(scanErrs) > 0 {
		return exitFailure
	}
	if len(findings) > 0 {
		return exitDiverged
	}
	return exitIdentical
}

func renderText(w io.Writer, rep catalog.Report) {
	fmt.Fprintf(w, "ignored fields: %s\n", strings.Join(catalog.AllowlistedFields, ", "))
	if rep.Identical() {
		fmt.Fprintln(w, "IDENTICAL")
		return
	}
	fmt.Fprintf(w, "%d difference(s)\n\n", len(rep.Differences))
	for _, d := range rep.Differences {
		fmt.Fprintf(w, "  %s\n    %s: %v -> %v\n", d.Path, d.Kind, d.Left, d.Right)
	}
}

func renderJSON(w io.Writer, rep catalog.Report) error {
	payload := struct {
		Identical     bool                 `json:"identical"`
		IgnoredFields []string             `json:"ignored_fields"`
		Differences   []catalog.Difference `json:"differences"`
	}{
		Identical:     rep.Identical(),
		IgnoredFields: catalog.AllowlistedFields,
		Differences:   rep.Differences,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func exitCodeFor(rep catalog.Report) int {
	if rep.Identical() {
		return exitIdentical
	}
	return exitDiverged
}
