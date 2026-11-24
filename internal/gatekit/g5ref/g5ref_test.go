package g5ref

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

func TestCheckReachesThePythonVerdicts(t *testing.T) {
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Fixture [4]string
		Cases   []struct {
			Name, Report, Catalog string
			Python                json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("testdata/golden.json holds no cases")
	}
	fixture := Fixture{golden.Fixture[0], golden.Fixture[1], golden.Fixture[2], golden.Fixture[3]}
	for _, c := range golden.Cases {
		t.Run(c.Name, func(t *testing.T) {
			report, err := pycompat.DecodeFile(filepath.Join("testdata", c.Report))
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := pycompat.DecodeFile(filepath.Join("testdata", c.Catalog))
			if err != nil {
				t.Fatal(err)
			}
			complaints, err := Check(report, catalog, fixture)

			var answered struct {
				Exit       int
				Complaints []string
			}
			if json.Unmarshal(c.Python, &answered) != nil {
				if err == nil {
					t.Fatalf("checked (%q), want a refusal: Python crashed", complaints)
				}
				return
			}
			if err != nil {
				t.Fatalf("could not check: %v; Python answered exit %d", err, answered.Exit)
			}
			if strings.Join(complaints, "\n") != strings.Join(answered.Complaints, "\n") {
				t.Errorf("complaints\n  got:    %q\n  Python: %q", complaints, answered.Complaints)
			}
		})
	}
}
