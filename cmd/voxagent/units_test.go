package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestUnitsCommandPrintsManifests(t *testing.T) {
	want, err := os.ReadFile("testdata/units.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := unitsCommand(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if out.String() != string(want) {
		t.Fatalf("stdout %q, want %q", out.String(), want)
	}
}

func TestUnitsCommandRefusesArguments(t *testing.T) {
	var out, errb bytes.Buffer
	if code := unitsCommand([]string{"x"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout %q, want empty", out.String())
	}
	if !strings.Contains(errb.String(), unitsUsage) {
		t.Fatalf("stderr %q lacks usage", errb.String())
	}
}

func TestUnitsCommandReadsNoFile(t *testing.T) {
	want, err := os.ReadFile("testdata/units.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Chdir(dir)
	var out, errb bytes.Buffer
	if code := unitsCommand(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if out.String() != string(want) {
		t.Fatalf("stdout %q, want %q", out.String(), want)
	}
}
