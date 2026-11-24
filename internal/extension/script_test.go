package extension

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The constant is the reviewed script's hash. Editing host.rb without
// updating it fails here, so the binary can never exec a script nobody pinned.
func TestHostScriptHashIsPinned(t *testing.T) {
	sum := sha256.Sum256(hostScript)
	if got := hex.EncodeToString(sum[:]); got != HostScriptSHA256 {
		t.Fatalf("sha256(host.rb) = %s, HostScriptSHA256 = %s", got, HostScriptSHA256)
	}
}

func TestStageScriptWritesAPrivateVerifiedCopy(t *testing.T) {
	dir, path, err := stageScript(hostScript, HostScriptSHA256)
	if err != nil {
		t.Fatalf("stageScript: %v", err)
	}
	defer os.RemoveAll(dir)
	for p, want := range map[string]os.FileMode{dir: 0o700 | os.ModeDir, filepath.Join(dir, "state"): 0o700 | os.ModeDir, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != want {
			t.Errorf("%s mode = %v, want %v", p, fi.Mode(), want)
		}
	}
}

func TestStageScriptRefusesAMismatchAndCleansUp(t *testing.T) {
	_, _, err := stageScript([]byte("puts 1\n"), HostScriptSHA256)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stageScript = %v, want a hash mismatch", err)
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "voxagent-host-*"))
	for _, m := range matches {
		if _, err := os.Stat(filepath.Join(m, "host.rb")); err == nil {
			if b, _ := os.ReadFile(filepath.Join(m, "host.rb")); string(b) == "puts 1\n" {
				t.Errorf("the refused script was left at %s", m)
			}
		}
	}
}

func TestLaunchRefusesARelativeRuby(t *testing.T) {
	_, err := Launch(Config{Ruby: "ruby", Timeouts: quick()}, nil)
	if err == nil || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("Launch = %v, want a refusal of a relative path", err)
	}
}
