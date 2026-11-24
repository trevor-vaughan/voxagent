package reportcmp

import (
	"strings"
	"testing"
)

func metricsDoc(triples ...any) map[string]any {
	return map[string]any{"metrics": map[string]any{
		"voxagent": map[string]any{"values": triples},
	}}
}

func TestNormaliseMetricsRefusesARepeatedName(t *testing.T) {
	doc := metricsDoc(
		[]any{"turns", "Turns", "1"},
		[]any{"turns", "Turns", "2"},
	)
	_, err := normaliseMetrics(doc)
	if err == nil || !strings.Contains(err.Error(), "voxagent") || !strings.Contains(err.Error(), `"turns"`) {
		t.Errorf("normaliseMetrics error = %v, want one naming the group and the repeated name", err)
	}
}

func TestNormaliseMetricsRekeysDistinctNames(t *testing.T) {
	out, err := normaliseMetrics(metricsDoc(
		[]any{"turns", "Turns", "1"},
		[]any{"errors", "Errors", "0"},
	))
	if err != nil {
		t.Fatal(err)
	}
	values := out["metrics"].(map[string]any)["voxagent"].(map[string]any)["values"].(map[string]any)
	if len(values) != 2 {
		t.Fatalf("values = %v, want two entries keyed by name", values)
	}
	turns := values["turns"].(map[string]any)
	if turns["label"] != "Turns" || turns["value"] != "1" {
		t.Errorf("values[turns] = %v", turns)
	}
}

func TestMaskValidateTempNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Execution of '/tmp/fail.sh /tmp/t/f20261007-625-1c0wrau' returned 1: <detail>",
			"Execution of '/tmp/fail.sh /tmp/t/f<YYYYMMDD-pid-rand>' returned 1: <detail>"},
		{"change from 'absent' to 'file' failed: Execution of '/usr/sbin/visudo -q -c -f /tmp/t/sudoers20261007-5906-1ewhm9d-2' returned 1: <detail>",
			"change from 'absent' to 'file' failed: Execution of '/usr/sbin/visudo -q -c -f /tmp/t/sudoers<YYYYMMDD-pid-rand>' returned 1: <detail>"},
		{"Execution of '/r.sh /t/a20261007-1-x --x=/t/a20261007-1-x' returned 1: <detail>",
			"Execution of '/r.sh /t/a<YYYYMMDD-pid-rand> --x=/t/a<YYYYMMDD-pid-rand>' returned 1: <detail>"},
		// Another naming is not masked, so it still diverges.
		{"Execution of '/r.sh /t/a.voxagent123' returned 1: <detail>",
			"Execution of '/r.sh /t/a.voxagent123' returned 1: <detail>"},
		// Outside the command nothing is touched.
		{"Could not set 'file' on ensure: /t/a20261007-1-x", "Could not set 'file' on ensure: /t/a20261007-1-x"},
	} {
		if got := maskValidateTempNames(tc.in); got != tc.want {
			t.Errorf("maskValidateTempNames(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
