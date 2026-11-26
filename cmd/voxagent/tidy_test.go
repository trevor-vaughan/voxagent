package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A Tidy applies natively with no extension host: its load-time generation
// logs the notice that opens the report, each generated File removes its
// path with one event, and the Tidy itself changes nothing.
func TestTidyAppliesNativelyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	for _, d := range []string{tree, filepath.Join(tree, "s")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var gone []string
	for _, f := range []string{"a.log", "s/b.log"} {
		gone = append(gone, filepath.Join(tree, f))
	}
	kept := filepath.Join(tree, "keep.txt")
	for _, f := range append(gone, kept) {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stage := map[string]any{"type": "Stage", "title": "main", "tags": []any{}, "parameters": map[string]any{}}
	mainClass := map[string]any{"type": "Class", "title": "main", "tags": []any{}, "kind": "class",
		"parameters": map[string]any{"name": "main"}}
	tidy := ext("Tidy", tree, map[string]any{"recurse": true, "matches": "*.log"})
	raw, err := os.ReadFile(writeCatalog(t, dir, []map[string]any{stage, mainClass, tidy}))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	tidyRef := "Tidy[" + tree + "]"
	body["edges"] = []map[string]string{{"source": "Stage[main]", "target": "Class[main]"},
		{"source": "Class[main]", "target": tidyRef}}
	if raw, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	st, rep := applyCatalog(raw, applyOptions{noRuby: true, level: "notice", reportDir: filepath.Join(dir, "reports")})
	if rep == nil || st.CouldNotRun {
		t.Fatalf("no run: status %+v", st)
	}

	for _, f := range gone {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s survived: %v", f, err)
		}
	}
	if _, err := os.Lstat(kept); err != nil {
		t.Errorf("%s was removed: %v", kept, err)
	}
	if len(rep.Logs) == 0 || rep.Logs[0].Level != "notice" || rep.Logs[0].Message != "Tidying 2 files" ||
		rep.Logs[0].Source != "/Stage[main]/Main/"+tidyRef {
		t.Errorf("first log = %+v, want the Tidy's notice", rep.Logs)
	}
	for _, f := range gone {
		rs, ok := rep.ResourceStatuses["File["+f+"]"]
		if !ok {
			t.Errorf("no status for File[%s]", f)
			continue
		}
		if len(rs.Events) != 1 || rs.Events[0].Message != "removed" || rs.Events[0].Status != "success" {
			t.Errorf("File[%s] events = %+v, want one removed", f, rs.Events)
		}
	}
	if rs, ok := rep.ResourceStatuses[tidyRef]; !ok || len(rs.Events) != 0 || rs.Failed {
		t.Errorf("%s status = %+v (present %v), want one with no events", tidyRef, rs, ok)
	}
	if rep.Status != "changed" {
		t.Errorf("report status = %q, want changed", rep.Status)
	}
}
