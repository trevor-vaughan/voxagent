package reportcmp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

type golden struct {
	Compare []struct {
		Name, Ruby, Voxagent, Allowlist string
		Python                          json.RawMessage
		Corrected                       json.RawMessage
		GoRefuses                       bool `json:"go_refuses"`
	} `json:"compare"`
	RunToRun []struct {
		First, Second, Allowlist string
		Exit                     int
		Lines                    []string
	} `json:"run_to_run"`
}

func readGolden(t *testing.T) golden {
	t.Helper()
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Compare) == 0 || len(g.RunToRun) == 0 {
		t.Fatal("testdata/golden.json is missing its compare or run_to_run cases")
	}
	return g
}

// load decodes the three inputs of one case, returning the first error.
func load(paths ...string) ([]any, error) {
	docs := make([]any, len(paths))
	for i, p := range paths {
		doc, err := pycompat.DecodeFile(filepath.Join("testdata", p))
		if err != nil {
			return nil, err
		}
		docs[i] = doc
	}
	return docs, nil
}

// correctedVerdict decodes a golden "corrected" field into a fresh value, so a
// field it omits cannot silently keep Python's. Every field is required (paths
// may be an empty list), an unknown key is an error, and why must say something.
func correctedVerdict(raw json.RawMessage) (exit, compared int, paths []string, err error) {
	var v struct {
		Exit     *int      `json:"exit"`
		Compared *int      `json:"compared"`
		Paths    *[]string `json:"paths"`
		Why      *string   `json:"why"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&v); err != nil {
		return 0, 0, nil, err
	}
	switch {
	case v.Exit == nil, v.Compared == nil, v.Paths == nil, v.Why == nil:
		return 0, 0, nil, errors.New("exit, compared, paths and why are all required")
	case *v.Why == "":
		return 0, 0, nil, errors.New("why is empty")
	}
	return *v.Exit, *v.Compared, *v.Paths, nil
}

func TestCompareReachesThePythonVerdicts(t *testing.T) {
	for _, c := range readGolden(t).Compare {
		t.Run(c.Name, func(t *testing.T) {
			var answered struct {
				Exit     int
				Compared int
				Paths    []string
			}
			pythonAnswered := json.Unmarshal(c.Python, &answered) == nil
			// A case where Go deliberately departs from Python carries the
			// verdict it must reach instead; Python's stays as history.
			source := "Python"
			if len(c.Corrected) > 0 {
				var err error
				if answered.Exit, answered.Compared, answered.Paths, err = correctedVerdict(c.Corrected); err != nil {
					t.Fatalf("golden corrected field %s: %v", c.Corrected, err)
				}
				pythonAnswered, source = true, "the corrected verdict"
			}

			var result Result
			docs, err := load(c.Ruby, c.Voxagent, c.Allowlist)
			if err == nil {
				var allow Allowlist
				if allow, err = ParseAllowlist(docs[2]); err == nil {
					result, err = Compare(docs[0], docs[1], allow)
				}
			}

			if !pythonAnswered || c.GoRefuses {
				if err == nil {
					t.Fatalf("compared (%d divergent), want a refusal: Python gave %s, go_refuses=%v",
						len(result.Divergences), c.Python, c.GoRefuses)
				}
				if errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a fixture is missing, which is not a refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("could not compare: %v; %s is exit %d", err, source, answered.Exit)
			}
			if result.Compared != answered.Compared {
				t.Errorf("compared %d leaves, %s compared %d", result.Compared, source, answered.Compared)
			}
			var paths []string
			for _, d := range result.Divergences {
				paths = append(paths, d.Path)
			}
			if strings.Join(paths, "\n") != strings.Join(answered.Paths, "\n") {
				t.Errorf("divergent paths\n  got: %q\n  %s: %q", paths, source, answered.Paths)
			}
			if wantExit := map[bool]int{true: 1, false: 0}[len(paths) > 0]; wantExit != answered.Exit {
				t.Errorf("golden is inconsistent: %d paths but %s exits %d", len(answered.Paths), source, answered.Exit)
			}
		})
	}
}

var (
	alsoSuppresses = regexp.MustCompile(`^(\s+also suppresses: .*?) = .*$`)
	runValue       = regexp.MustCompile(`^(    run[12]:) .*$`)
)

func TestRunToRunReachesThePythonVerdicts(t *testing.T) {
	for _, c := range readGolden(t).RunToRun {
		t.Run(c.First+"~"+c.Second, func(t *testing.T) {
			docs, err := load(c.First, c.Second, c.Allowlist)
			if err != nil {
				t.Fatal(err)
			}
			allow, err := ParseAllowlist(docs[2])
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			clean, err := RunToRun(docs[0], docs[1], allow, &out)
			exit := map[bool]int{true: 0, false: 1}[clean]
			if err != nil {
				exit = 2
			}
			if exit != c.Exit {
				t.Fatalf("exit %d (err %v), Python exited %d", exit, err, c.Exit)
			}
			var lines []string
			if out.Len() > 0 {
				for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
					l = alsoSuppresses.ReplaceAllString(l, "$1 = …")
					lines = append(lines, runValue.ReplaceAllString(l, "$1 …"))
				}
			}
			if strings.Join(lines, "\n") != strings.Join(c.Lines, "\n") {
				t.Errorf("output differs\n--- got\n%s\n--- Python\n%s", strings.Join(lines, "\n"), strings.Join(c.Lines, "\n"))
			}
		})
	}
}

func TestCorrectedVerdictRejectsMalformed(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"null", `null`},
		{"empty object", `{}`},
		{"missing exit", `{"compared":1,"paths":["a"],"why":"x"}`},
		{"missing compared", `{"exit":1,"paths":["a"],"why":"x"}`},
		{"missing paths", `{"exit":1,"compared":1,"why":"x"}`},
		{"missing why", `{"exit":1,"compared":1,"paths":["a"]}`},
		{"empty why", `{"exit":1,"compared":1,"paths":["a"],"why":""}`},
		{"unknown key", `{"exit":1,"compared":1,"pahts":["a"],"paths":["a"],"why":"x"}`},
		{"not an object", `[1]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := correctedVerdict(json.RawMessage(tc.raw)); err == nil {
				t.Errorf("correctedVerdict(%s) accepted a malformed verdict", tc.raw)
			}
		})
	}

	exit, compared, paths, err := correctedVerdict(json.RawMessage(`{"exit":1,"compared":1,"paths":["a"],"why":"x"}`))
	if err != nil || exit != 1 || compared != 1 || strings.Join(paths, ",") != "a" {
		t.Errorf("well-formed verdict: got (%d, %d, %q, %v)", exit, compared, paths, err)
	}
	if _, _, paths, err = correctedVerdict(json.RawMessage(`{"exit":0,"compared":0,"paths":[],"why":"x"}`)); err != nil || len(paths) != 0 {
		t.Errorf("empty paths list must be accepted: got (%q, %v)", paths, err)
	}
}
