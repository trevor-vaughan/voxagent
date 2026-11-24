package compile

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Real Puppet 8.10.0 prints this Notice, with ANSI escapes, before the JSON.
const noticePrefix = "\x1b[mNotice: Compiled catalog for testnode.example.com " +
	"in environment production in 0.11 seconds\x1b[0m\n"

func TestExtractJSONStripsNoticePrefix(t *testing.T) {
	body := `{"catalog_format":2,"name":"testnode.example.com"}`
	got, err := ExtractJSON([]byte(noticePrefix + body))
	if err != nil {
		t.Fatalf("ExtractJSON returned error: %v", err)
	}
	if string(got) != body {
		t.Errorf("ExtractJSON = %q, want %q", got, body)
	}
}

func TestExtractJSONPassesThroughCleanInput(t *testing.T) {
	body := `{"catalog_format":2}`
	got, err := ExtractJSON([]byte(body))
	if err != nil {
		t.Fatalf("ExtractJSON returned error: %v", err)
	}
	if string(got) != body {
		t.Errorf("ExtractJSON = %q, want %q", got, body)
	}
}

func TestExtractJSONRejectsInputWithNoObject(t *testing.T) {
	if _, err := ExtractJSON([]byte("Error: could not compile\n")); err == nil {
		t.Fatal("ExtractJSON accepted output containing no JSON object; want error")
	}
}

// Guards against a brace inside the Notice text being mistaken for the body.
func TestExtractJSONFindsFirstTopLevelBrace(t *testing.T) {
	got, err := ExtractJSON([]byte("Notice: thing {weird} happened\n{\"catalog_format\":2}"))
	if err != nil {
		t.Fatalf("ExtractJSON returned error: %v", err)
	}
	if !strings.HasPrefix(string(got), `{"catalog_format"`) {
		t.Errorf("ExtractJSON = %q, want it to start at the JSON body", got)
	}
}

// Puppet does not indent its JSON output, but ExtractJSON's line-start match
// only requires the trimmed line to begin with '{' -- it does not require the
// raw line to begin with '{'. This documents that indentation before the
// brace is tolerated and the returned body still starts exactly at '{', not
// at the leading whitespace.
func TestExtractJSONHandlesIndentedJSONLine(t *testing.T) {
	body := `{"catalog_format":2}`
	got, err := ExtractJSON([]byte(noticePrefix + "   " + body))
	if err != nil {
		t.Fatalf("ExtractJSON returned error: %v", err)
	}
	if string(got) != body {
		t.Errorf("ExtractJSON = %q, want %q", got, body)
	}
}

func TestCompileReportsMissingBinary(t *testing.T) {
	r := Runner{PuppetBin: "definitely-not-a-real-binary-voxdiff", Environment: "production"}
	if _, err := r.Compile(context.Background(), "node.example.com"); err == nil {
		t.Fatal("Compile succeeded with a nonexistent binary; want error")
	}
}

// Integration: only runs where a real puppet is installed.
func TestCompileAgainstRealPuppet(t *testing.T) {
	if _, err := exec.LookPath("puppet"); err != nil {
		t.Skip("puppet not installed; skipping integration check")
	}
	r := Runner{
		PuppetBin:       "puppet",
		EnvironmentPath: "testdata/environments",
		Environment:     "production",
		ConfDir:         t.TempDir(),
		VarDir:          t.TempDir(),
		CodeDir:         "testdata",
	}
	b, err := r.Compile(context.Background(), "testnode.example.com")
	if err != nil {
		t.Fatalf("Compile returned error: %v", err)
	}
	if !strings.HasPrefix(string(b), "{") {
		t.Errorf("Compile output does not start with a JSON object: %.60q", b)
	}
}
