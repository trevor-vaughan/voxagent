package reportcmp

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// judged lists the packages under test in the report differentials. The judge
// must not import them: a struct shared with the code under test would let a
// bug in that struct agree with itself.
var judged = []string{
	"github.com/voxagent/voxagent/internal/report",
	"github.com/voxagent/voxagent/internal/transaction",
	"github.com/voxagent/voxagent/internal/ral",
}

const modulePath = "github.com/voxagent/voxagent"

// moduleRoot is the repository root relative to this package's directory.
const moduleRoot = "../../.."

// forbiddenImports returns every judged package the Go files in dir reach,
// tests included, directly or through packages of this module. Each entry
// reads as the chain of imports that reaches it.
func forbiddenImports(dir string) ([]string, error) {
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	direct := append(append(append([]string{}, pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
	return reachedJudged(filepath.Base(dir), direct, moduleImports)
}

// moduleImports lists the non-test imports of a package of this module.
func moduleImports(pkg string) ([]string, error) {
	p, err := build.ImportDir(filepath.Join(moduleRoot, strings.TrimPrefix(pkg, modulePath)), 0)
	if err != nil {
		return nil, err
	}
	return p.Imports, nil
}

// reachedJudged follows direct imports through packages of this module,
// asking imports for each one's own imports, and returns a chain for every
// judged package reached. A judged package is reported, not entered; packages
// outside the module are not entered.
func reachedJudged(from string, direct []string, imports func(pkg string) ([]string, error)) ([]string, error) {
	var found []string
	seen := map[string]bool{}
	var visit func(chain string, pkgs []string) error
	visit = func(chain string, pkgs []string) error {
		for _, p := range pkgs {
			if seen[p] || !strings.HasPrefix(p, modulePath+"/") {
				continue
			}
			seen[p] = true
			if slices.Contains(judged, p) {
				found = append(found, chain+" imports "+p)
				continue
			}
			next, err := imports(p)
			if err != nil {
				return err
			}
			if err := visit(chain+" imports "+p, next); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(from, direct); err != nil {
		return nil, err
	}
	return found, nil
}

func TestJudgeImportsNothingItJudges(t *testing.T) {
	found, err := forbiddenImports(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Error(f)
	}
}

// The check above passing proves nothing unless it can fail.
func TestForbiddenImportsSeesAJudgedImport(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nimport _ \"github.com/voxagent/voxagent/internal/report\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := forbiddenImports(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("forbiddenImports = %q, want the one judged import", found)
	}
}

// A judged package reached through an intermediate package of this module is
// as much a shared struct as a direct import.
func TestReachedJudgedFollowsModulePackages(t *testing.T) {
	imports := map[string][]string{
		modulePath + "/internal/mid":  {"fmt", modulePath + "/internal/leaf"},
		modulePath + "/internal/leaf": {modulePath + "/internal/report"},
		modulePath + "/internal/safe": {"fmt"},
	}
	resolve := func(pkg string) ([]string, error) { return imports[pkg], nil }

	found, err := reachedJudged("x", []string{modulePath + "/internal/mid"}, resolve)
	if err != nil {
		t.Fatal(err)
	}
	want := "x imports " + modulePath + "/internal/mid imports " + modulePath + "/internal/leaf imports " + modulePath + "/internal/report"
	if len(found) != 1 || found[0] != want {
		t.Errorf("reachedJudged = %q, want [%q]", found, want)
	}

	found, err = reachedJudged("x", []string{modulePath + "/internal/safe", "fmt"}, resolve)
	if err != nil || len(found) != 0 {
		t.Errorf("reachedJudged over a clean graph = %q, %v; want nothing", found, err)
	}
}
