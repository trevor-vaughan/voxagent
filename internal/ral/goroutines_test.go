package ral

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §4.2 item 8: staging changes the agent's cwd and umask while it starts a
// child, which is safe only because nothing else runs then. The one goroutine
// these packages (and the units) start is runExec's waiter, which calls nothing but Wait.
func TestOnlyTheExecWaiterStartsAGoroutine(t *testing.T) {
	var found []string
	fset := token.NewFileSet()
	libs, err := filepath.Glob("../lib/*")
	if err != nil {
		t.Fatal(err)
	}
	units, err := filepath.Glob("../units/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range append(append([]string{".", "../transaction"}, libs...), units...) {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(fset, name, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if g, ok := n.(*ast.GoStmt); ok {
					found = append(found, fset.Position(g.Pos()).String())
				}
				return true
			})
		}
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], "../units/exec/exec_run.go:") {
		t.Errorf("go statements at %v, want exactly one, in units/exec/exec_run.go", found)
	}
}
