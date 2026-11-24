package fleet

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type pyVerdict struct {
	Exit    int
	Lines   []string
	Message string
}

// verdictOf decodes a golden file's "python" field: a verdict, or nil for
// "could-not-run".
func verdictOf(t *testing.T, raw json.RawMessage) *pyVerdict {
	t.Helper()
	var v pyVerdict
	if json.Unmarshal(raw, &v) != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s != "could-not-run" {
			t.Fatalf("golden \"python\" field %s is neither a verdict nor \"could-not-run\"", raw)
		}
		return nil
	}
	return &v
}

func readGolden(t *testing.T) (g struct {
	Axes []struct {
		Name, Catalog string
		Want          [3]string
		Python        json.RawMessage
	}
	Compliance []struct {
		Name, Catalog string
		Python        json.RawMessage
	}
	Facts []struct {
		Name, Facts string
		Python      json.RawMessage
	}
}) {
	t.Helper()
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Axes) == 0 || len(g.Compliance) == 0 || len(g.Facts) == 0 {
		t.Fatal("testdata/golden.json is missing a table")
	}
	return g
}

// refused fails the test unless err is a refusal of the input rather than a
// missing fixture, which would otherwise pass as one.
func refused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("answered, want a refusal: the Python check could not run")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fixture missing: %v", err)
	}
}

func TestAxesReachesThePythonVerdicts(t *testing.T) {
	for _, c := range readGolden(t).Axes {
		t.Run(c.Name, func(t *testing.T) {
			want := verdictOf(t, c.Python)
			var lines []string
			catalog, err := LoadCatalog(filepath.Join("testdata", c.Catalog))
			if err == nil {
				lines, err = Axes(catalog, AxesWant{c.Want[0], c.Want[1], c.Want[2]})
			}
			if want == nil {
				refused(t, err)
				return
			}
			if err != nil {
				t.Fatalf("could not check: %v; Python exited %d", err, want.Exit)
			}
			if (len(lines) > 0) != (want.Exit == 1) || strings.Join(lines, "\n") != strings.Join(want.Lines, "\n") {
				t.Errorf("lines\n  got:    %q\n  Python: exit %d, %q", lines, want.Exit, want.Lines)
			}
		})
	}
}

func TestComplianceReachesThePythonVerdicts(t *testing.T) {
	for _, c := range readGolden(t).Compliance {
		t.Run(c.Name, func(t *testing.T) {
			want := verdictOf(t, c.Python)
			var outcome ComplianceOutcome
			var lines []string
			catalog, err := LoadCatalog(filepath.Join("testdata", c.Catalog))
			if err == nil {
				outcome, lines, err = Compliance(catalog, "enforced-by-disa_stig")
			}
			if want == nil {
				refused(t, err)
				return
			}
			if err != nil {
				t.Fatalf("could not check: %v; Python exited %d", err, want.Exit)
			}
			exit := map[ComplianceOutcome]int{Enforced: 0, NotEnforced: 1, MarkerAbsent: 2}[outcome]
			if exit != want.Exit || strings.Join(lines, "\n") != strings.Join(want.Lines, "\n") {
				t.Errorf("got exit %d, %q\nPython: exit %d, %q", exit, lines, want.Exit, want.Lines)
			}
		})
	}
}

func TestCheckFactsReachesThePythonVerdicts(t *testing.T) {
	for _, c := range readGolden(t).Facts {
		t.Run(c.Name, func(t *testing.T) {
			want := verdictOf(t, c.Python)
			problem, err := CheckFacts(filepath.Join("testdata", c.Facts), "node-el")
			if want == nil {
				refused(t, err)
				return
			}
			if err != nil {
				t.Fatalf("could not check: %v; Python exited %d", err, want.Exit)
			}
			if (problem != "") != (want.Exit == 1) || problem != want.Message {
				t.Errorf("got %q, Python exited %d with %q", problem, want.Exit, want.Message)
			}
		})
	}
}

// Shapes the Python check handled without crashing but a capture cannot have:
// refused, so a non-object `values` cannot pass by an accident of Python's
// `in` working on a list or a string.
func TestCheckFactsRefusesValuesThatAreNotAnObject(t *testing.T) {
	for _, doc := range []string{`{"values": ["networking", "os", "fleet_tier"]}`, `{"values": "networking os"}`, `[]`} {
		path := filepath.Join(t.TempDir(), "facts.json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if problem, err := CheckFacts(path, "node-el"); err == nil {
			t.Errorf("CheckFacts(%s) = %q, nil; want a refusal", doc, problem)
		}
	}
}
