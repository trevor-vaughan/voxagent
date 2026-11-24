package reportcmp

import (
	"reflect"
	"strings"
	"testing"
)

// orderReport builds a report with one status per ref (its containment path is
// Stage[main], Main, ref) and one log per source, in the order given.
func orderReport(refs []string, sources ...string) string {
	var statuses, logs []string
	for _, r := range refs {
		statuses = append(statuses, `"`+r+`":{"containment_path":["Stage[main]","Main","`+r+`"]}`)
	}
	for _, s := range sources {
		logs = append(logs, `{"source":"`+s+`","message":"m","level":"notice"}`)
	}
	return `{"resource_statuses":{` + strings.Join(statuses, ",") + `},"logs":[` + strings.Join(logs, ",") + `]}`
}

func under(ref string) string { return "/Stage[main]/Main/" + ref + "/ensure" }

func TestWalkOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refs        []string
		sources     []string
		want        []string
		wantLogging int
	}{
		{"first log orders, repeats ignored", []string{"A", "B", "C"},
			[]string{under("B"), under("A"), under("B"), under("C")}, []string{"B", "A", "C"}, 3},
		{"longest prefix wins", []string{"File[/x]", "File[/x/y]"},
			[]string{"/Stage[main]/Main/File[/x]/content", "/Stage[main]/Main/File[/x/y]/content"},
			[]string{"File[/x]", "File[/x/y]"}, 2},
		{"a prefix is not a path component", []string{"File[/x]"},
			[]string{"/Stage[main]/Main/File[/x]sibling/content"}, nil, 0},
		{"the status itself is a source", []string{"A"}, []string{"/Stage[main]/Main/A"}, []string{"A"}, 1},
		{"unowned logs are ignored", []string{"A"},
			[]string{"Puppet", "/Stage[main]/Whit[x]", under("A")}, []string{"A"}, 1},
		{"a status that never logged is absent", []string{"A", "B"}, []string{under("B")}, []string{"B"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, logging, err := WalkOrder(decode(t, orderReport(tc.refs, tc.sources...)))
			if err != nil || !reflect.DeepEqual(got, tc.want) || logging != tc.wantLogging {
				t.Errorf("WalkOrder = %v, %d, %v; want %v, %d", got, logging, err, tc.want, tc.wantLogging)
			}
		})
	}
}

func TestWalkOrderRefusesMalformedReports(t *testing.T) {
	for name, report := range map[string]string{
		"not an object":        `[]`,
		"no statuses":          `{"logs":[]}`,
		"status not an object": `{"resource_statuses":{"A":1}}`,
		"no containment path":  `{"resource_statuses":{"A":{}}}`,
		"empty path":           `{"resource_statuses":{"A":{"containment_path":[]}}}`,
		"path element number":  `{"resource_statuses":{"A":{"containment_path":[1]}}}`,
		"logs not a list":      `{"resource_statuses":{},"logs":{}}`,
		"log not an object":    `{"resource_statuses":{},"logs":[1]}`,
		"source not a string":  `{"resource_statuses":{},"logs":[{"source":1}]}`,
	} {
		if _, _, err := WalkOrder(decode(t, report)); err == nil {
			t.Errorf("%s: WalkOrder accepted %s", name, report)
		}
	}
}

func compareOrders(t *testing.T, ruby, vox string) (bool, int, string, error) {
	t.Helper()
	return CompareOrder(decode(t, ruby), decode(t, vox))
}

func abc(order ...string) string {
	sources := make([]string, len(order))
	for i, r := range order {
		sources[i] = under(r)
	}
	return orderReport([]string{"a", "b", "c"}, sources...)
}

func TestCompareOrderAgrees(t *testing.T) {
	agree, compared, detail, err := compareOrders(t, abc("a", "b", "c"), abc("a", "b", "c"))
	if !agree || compared != 3 || detail != "" || err != nil {
		t.Errorf("= %v, %d, %q, %v; want agreement over 3", agree, compared, detail, err)
	}
}

func TestCompareOrderDiverges(t *testing.T) {
	agree, compared, detail, err := compareOrders(t, abc("a", "b", "c"), abc("a", "c", "b"))
	if agree || err != nil || compared != 3 {
		t.Fatalf("= %v, %d, %q, %v; want a divergence", agree, compared, detail, err)
	}
	if want := "walk order diverges at 1: ruby b, voxagent c\n  ruby before: a\n  voxagent before: a"; detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

func TestCompareOrderShowsThreeRefsBefore(t *testing.T) {
	refs := []string{"a", "b", "c", "d", "e", "f"}
	order := func(last ...string) string {
		sources := []string{}
		for _, r := range append([]string{"a", "b", "c", "d"}, last...) {
			sources = append(sources, under(r))
		}
		return orderReport(refs, sources...)
	}
	_, _, detail, _ := compareOrders(t, order("e", "f"), order("f", "e"))
	if !strings.Contains(detail, "diverges at 4: ruby e, voxagent f") || !strings.Contains(detail, "ruby before: b, c, d\n") {
		t.Errorf("detail = %q", detail)
	}
}

// Statuses agree on both sides, and both are attributed by the same rule, so
// a ref only one side logged for is a log divergence, not an attribution
// failure. Only refs both logged for are ordered, so the one-sided ref is
// named as such rather than shifting the rest into a false walk divergence.
func TestCompareOrderLoggingOnOneSideIsADivergence(t *testing.T) {
	agree, compared, detail, err := compareOrders(t, abc("a", "b", "c"), abc("a", "c"))
	if agree || compared != 2 || err != nil || detail != "b logged only by ruby" {
		t.Errorf("= %v, %d, %q, %v; want an exit-1 divergence over 2", agree, compared, detail, err)
	}
	agree, compared, detail, err = compareOrders(t, abc("a", "c"), abc("a", "b", "c"))
	if agree || compared != 2 || err != nil || detail != "b logged only by voxagent" {
		t.Errorf("= %v, %d, %q, %v; want an exit-1 divergence over 2", agree, compared, detail, err)
	}
}

// The first one-sided ref is named in its side's walk order.
func TestCompareOrderNamesTheFirstOneSidedRefInWalkOrder(t *testing.T) {
	_, _, detail, err := compareOrders(t, abc("c", "b", "a"), abc("a"))
	if err != nil || detail != "c logged only by ruby" {
		t.Errorf("detail = %q, err = %v; want c, the first ruby walked", detail, err)
	}
}

// A source both sides leave unattributed ("Puppet") explains no missing log,
// so it does not turn a one-sided log into a thin comparison.
func TestCompareOrderSharedUnattributedSourcesAreNotThin(t *testing.T) {
	ruby := orderReport([]string{"a", "b"}, "Puppet", under("a"), under("b"))
	vox := orderReport([]string{"a", "b"}, "Puppet", under("a"))
	agree, _, detail, err := compareOrders(t, ruby, vox)
	if agree || err != nil || detail != "b logged only by ruby" {
		t.Errorf("= %v, %q, %v; want an exit-1 divergence", agree, detail, err)
	}
}

// A source only the side lacking a log leaves unattributed may be that log,
// so the comparison is thin: attribution failed there.
func TestCompareOrderOneSidedUnattributedSourceIsThin(t *testing.T) {
	ruby := orderReport([]string{"a", "b"}, under("a"), "/elsewhere/b")
	vox := orderReport([]string{"a", "b"}, under("a"), under("b"))
	agree, compared, detail, err := compareOrders(t, ruby, vox)
	if agree || compared != 1 || detail != "" || err == nil || !strings.Contains(err.Error(), "compared 1 of 2 logging resources; attribution failed") {
		t.Errorf("= %v, %d, %q, %v; want a thin comparison error", agree, compared, detail, err)
	}
}

// A one-sided log the other side cannot explain is reported even when
// attribution failed for a different ref, so thinness never hides it.
func TestCompareOrderOneSidedLoggingBeatsThinness(t *testing.T) {
	refs := []string{"a", "b", "c"}
	ruby := orderReport(refs, under("a"), under("b"))
	vox := orderReport(refs, under("a"), under("c"), "/elsewhere/b")
	agree, _, detail, err := compareOrders(t, ruby, vox)
	if agree || err != nil || detail != "c logged only by voxagent" {
		t.Errorf("= %v, %q, %v; want an exit-1 divergence", agree, detail, err)
	}
}

func TestCompareOrderStatusOnOneSideIsADivergence(t *testing.T) {
	ruby := orderReport([]string{"a", "b"}, under("a"), under("b"))
	vox := orderReport([]string{"a"}, under("a"))
	agree, _, detail, err := compareOrders(t, ruby, vox)
	if agree || err != nil || detail != "status b reported only by ruby" {
		t.Errorf("= %v, %q, %v; want exit-1 divergence", agree, detail, err)
	}
	agree, _, detail, err = compareOrders(t, vox, ruby)
	if agree || err != nil || detail != "status b reported only by voxagent" {
		t.Errorf("= %v, %q, %v; want exit-1 divergence", agree, detail, err)
	}
}

// A real divergence must be reported even when attribution also failed on a
// status, so thinness never hides it.
func TestCompareOrderDivergenceBeatsThinness(t *testing.T) {
	ruby := orderReport([]string{"a", "b", "c"}, under("a"), under("b"), under("c"))
	vox := orderReport([]string{"a", "b", "c"}, under("b"), under("a"), "/elsewhere/c")
	agree, _, detail, err := compareOrders(t, ruby, vox)
	if agree || err != nil || !strings.HasPrefix(detail, "walk order diverges at 0") {
		t.Errorf("= %v, %q, %v; want a divergence", agree, detail, err)
	}
}

func TestCompareOrderUnattributableLogsAreThin(t *testing.T) {
	ruby := abc("a", "b", "c")
	vox := orderReport([]string{"a", "b", "c"}, "/elsewhere/a", "/elsewhere/b")
	agree, compared, _, err := compareOrders(t, ruby, vox)
	if agree || compared != 0 || err == nil || !strings.Contains(err.Error(), "compared 0 of 3 logging resources; attribution failed") {
		t.Errorf("= %v, %d, %v; want a thin comparison error", agree, compared, err)
	}
}

func TestCompareOrderRefusesMalformedReports(t *testing.T) {
	if _, _, _, err := compareOrders(t, `[]`, abc()); err == nil || !strings.Contains(err.Error(), "ruby report") {
		t.Errorf("ruby: %v", err)
	}
	if _, _, _, err := compareOrders(t, abc(), `[]`); err == nil || !strings.Contains(err.Error(), "voxagent report") {
		t.Errorf("voxagent: %v", err)
	}
}

func TestCompareOrderWithNothingToCompareIsThin(t *testing.T) {
	refs := []string{"a", "b"}
	for name, report := range map[string]string{
		"no logs":           orderReport(refs),
		"unmatched sources": orderReport(refs, "/elsewhere/a", "Puppet"),
		"no statuses":       orderReport(nil),
	} {
		agree, compared, _, err := compareOrders(t, report, report)
		if agree || compared != 0 || err == nil || !strings.Contains(err.Error(), "compared 0 of 0 logging resources; nothing to compare") {
			t.Errorf("%s: = %v, %d, %v; want a nothing-to-compare error", name, agree, compared, err)
		}
	}
}

func TestWalkOrderRefusesDuplicateContainmentPath(t *testing.T) {
	report := `{"resource_statuses":{` +
		`"File[/b]":{"containment_path":["Stage[main]","X"]},` +
		`"File[/a]":{"containment_path":["Stage[main]","X"]}}}`
	_, _, err := WalkOrder(decode(t, report))
	if err == nil || !strings.Contains(err.Error(), "File[/a]") || !strings.Contains(err.Error(), "File[/b]") {
		t.Errorf("err = %v; want a refusal naming both statuses", err)
	}
}
