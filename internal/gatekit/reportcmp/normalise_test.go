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
