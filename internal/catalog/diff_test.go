package catalog

import "testing"

// twoCatalogs returns two independently decoded copies of the fixture, both
// normalized, so a test can mutate one side freely.
func twoCatalogs(t *testing.T) (*Catalog, *Catalog) {
	t.Helper()
	l, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode left: %v", err)
	}
	r, err := Decode(loadFixture(t, "minimal.json"))
	if err != nil {
		t.Fatalf("Decode right: %v", err)
	}
	Normalize(l)
	Normalize(r)
	return l, r
}

func findDiff(r Report, path string) (Difference, bool) {
	for _, d := range r.Differences {
		if d.Path == path {
			return d, true
		}
	}
	return Difference{}, false
}

func TestDiffIdenticalCatalogs(t *testing.T) {
	l, r := twoCatalogs(t)
	rep := Diff(l, r, Options{})
	if !rep.Identical() {
		t.Errorf("Identical() = false, want true; differences: %+v", rep.Differences)
	}
}

func TestDiffDetectsChangedParameter(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[1].Parameters["message"] = "changed"
	rep := Diff(l, r, Options{})
	d, ok := findDiff(rep, "resources[Notify[loop-1]].parameters.message")
	if !ok {
		t.Fatalf("no difference reported for changed parameter; got %+v", rep.Differences)
	}
	if d.Kind != KindChanged {
		t.Errorf("Kind = %q, want %q", d.Kind, KindChanged)
	}
}

func TestDiffDetectsMissingResource(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources = r.Resources[:1]
	rep := Diff(l, r, Options{})
	d, ok := findDiff(rep, "resources[Notify[loop-1]]")
	if !ok {
		t.Fatalf("no difference reported for removed resource; got %+v", rep.Differences)
	}
	if d.Kind != KindOnlyInLeft {
		t.Errorf("Kind = %q, want %q", d.Kind, KindOnlyInLeft)
	}
	// orderOfCommon restricts order comparison to refs present on both sides,
	// so a pure removal must not also be reported as a reorder.
	if _, ok := findDiff(rep, "resources"); ok {
		t.Errorf("removed resource also reported as a reorder; got %+v", rep.Differences)
	}
}

// A pure addition, like a pure removal, must not be reported as a reorder:
// orderOfCommon restricts order comparison to refs present on both sides.
func TestDiffAddedResourceIsNotReportedAsReordering(t *testing.T) {
	l, r := twoCatalogs(t)
	l.Resources = l.Resources[:1]
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "resources[Notify[loop-1]]"); !ok {
		t.Fatalf("no difference reported for added resource; got %+v", rep.Differences)
	}
	if _, ok := findDiff(rep, "resources"); ok {
		t.Errorf("added resource also reported as a reorder; got %+v", rep.Differences)
	}
}

func TestDiffDetectsAddedParameter(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[1].Parameters["loglevel"] = "debug"
	rep := Diff(l, r, Options{})
	d, ok := findDiff(rep, "resources[Notify[loop-1]].parameters.loglevel")
	if !ok {
		t.Fatalf("no difference reported for added parameter; got %+v", rep.Differences)
	}
	if d.Kind != KindOnlyInRight {
		t.Errorf("Kind = %q, want %q", d.Kind, KindOnlyInRight)
	}
}

// Resource order is behavioural, so reordering must be reported.
func TestDiffDetectsReordering(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[0], r.Resources[1] = r.Resources[1], r.Resources[0]
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "resources"); !ok {
		t.Fatalf("reordering not reported; got %+v", rep.Differences)
	}
}

// Tags are a set in Puppet, so tag order alone is not a difference.
func TestDiffIgnoresTagOrder(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[1].Tags = []string{"class", "default", "node", "loop-1", "notify"}
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "resources[Notify[loop-1]].tags"); ok {
		t.Errorf("tag reordering reported as a difference; it is a set")
	}
}

func TestDiffDetectsChangedTagSet(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[1].Tags = []string{"notify", "loop-1"}
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "resources[Notify[loop-1]].tags"); !ok {
		t.Errorf("changed tag set not reported; got %+v", rep.Differences)
	}
}

func TestDiffSourceLocationsRespectOption(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Resources[1].Line = 999

	if _, ok := findDiff(Diff(l, r, Options{}), "resources[Notify[loop-1]].line"); !ok {
		t.Errorf("changed line not reported with default options")
	}
	if _, ok := findDiff(Diff(l, r, Options{IgnoreSourceLocations: true}), "resources[Notify[loop-1]].line"); ok {
		t.Errorf("changed line reported despite IgnoreSourceLocations")
	}
}

func TestDiffDetectsMissingEdge(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Edges = nil
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "edges[Stage[main] -> Class[Settings]]"); !ok {
		t.Fatalf("removed edge not reported; got %+v", rep.Differences)
	}
}

// equalPtrString is the only scalar comparison with pointer-nil handling.
// This covers the combinations that matter: both nil, one nil, both set to
// the same value via different pointers (the case a naive l.CodeID ==
// r.CodeID pointer comparison would get wrong), and both set to different
// values.
func TestDiffCodeIDNilSemantics(t *testing.T) {
	same1, same2 := "abc123", "abc123" // distinct pointers, equal values
	other := "def456"

	tests := []struct {
		name       string
		left       *string
		right      *string
		wantDiffer bool
	}{
		{"both nil", nil, nil, false},
		{"left nil, right set", nil, &same1, true},
		{"left set, right nil", &same1, nil, true},
		{"same value, different pointers", &same1, &same2, false},
		{"different values", &same1, &other, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, r := twoCatalogs(t)
			l.CodeID = tt.left
			r.CodeID = tt.right
			rep := Diff(l, r, Options{})
			_, ok := findDiff(rep, "code_id")
			if ok != tt.wantDiffer {
				t.Errorf("code_id difference reported = %v, want %v; differences: %+v", ok, tt.wantDiffer, rep.Differences)
			}
		})
	}
}

func TestDiffDetectsChangedEnvironment(t *testing.T) {
	l, r := twoCatalogs(t)
	r.Environment = "staging"
	rep := Diff(l, r, Options{})
	if _, ok := findDiff(rep, "environment"); !ok {
		t.Fatalf("changed environment not reported; got %+v", rep.Differences)
	}
}

// Report output must be stable across runs, or the harness cannot be diffed in
// CI. Go map iteration is randomised, so parameter keys must be sorted in both
// of appendOneResourceDiff's loops: the left loop (parameters present on both
// sides, emitting "changed") and the right loop (parameters only on the
// right, emitting "only_in_right"). Both sides below carry four-plus keys so
// that dropping the sort from either loop makes this test flaky.
func TestDiffOutputIsDeterministic(t *testing.T) {
	l, r := twoCatalogs(t)
	l.Resources[1].Parameters = map[string]any{"a": 1.0, "b": 2.0, "c": 3.0, "d": 4.0}
	r.Resources[1].Parameters = map[string]any{
		"a": 9.0, "b": 9.0, "c": 9.0, "d": 9.0, // present on both -> left loop, KindChanged
		"w": 1.0, "x": 2.0, "y": 3.0, "z": 4.0, // right-only      -> right loop, KindOnlyInRight
	}
	first := Diff(l, r, Options{})
	for i := range 20 {
		got := Diff(l, r, Options{})
		if len(got.Differences) != len(first.Differences) {
			t.Fatalf("run %d: differing count", i)
		}
		for j := range got.Differences {
			if got.Differences[j].Path != first.Differences[j].Path {
				t.Fatalf("run %d: path order changed at %d: %q vs %q",
					i, j, got.Differences[j].Path, first.Differences[j].Path)
			}
		}
	}
}
