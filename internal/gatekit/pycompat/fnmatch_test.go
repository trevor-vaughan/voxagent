package pycompat

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFnmatchMatchesCPython(t *testing.T) {
	data, err := os.ReadFile("testdata/fnmatch.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases [][3]string `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("testdata/fnmatch.json holds no cases")
	}
	for _, c := range table.Cases {
		pattern, name, want := c[0], c[1], c[2]
		re, err := CompileFnmatch(pattern)
		if strings.HasPrefix(want, "error:") {
			if err == nil {
				t.Errorf("CompileFnmatch(%q) compiled, CPython raised %s", pattern, want)
			}
			continue
		}
		if err != nil {
			t.Errorf("CompileFnmatch(%q): %v, CPython answered %s", pattern, err, want)
			continue
		}
		if got := re.MatchString(name); got != (want == "match") {
			t.Errorf("fnmatch(%q, %q) = %v, CPython says %s", name, pattern, got, want)
		}
	}
}

// The property the allowlist depends on, asserted directly so that a
// regenerated table which lost these rows would not silently drop it.
func TestFnmatchStarCrossesDotsAndSlashes(t *testing.T) {
	re, err := CompileFnmatch("resource_statuses.*.time")
	if err != nil {
		t.Fatal(err)
	}
	if !re.MatchString("resource_statuses.File[/etc/hosts].time") {
		t.Error("* did not cross the dots and slashes inside a resource reference")
	}
	if re.MatchString("resource_statuses.File[/etc/hosts].time.extra") {
		t.Error("the pattern matched past its end; fnmatch is anchored at both ends")
	}
}
