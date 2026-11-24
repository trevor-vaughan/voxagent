// Package extensiontest finds the Ruby and the fixture plugin tree that tests
// of the extension host need.
package extensiontest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Ruby returns an absolute path to a Ruby that can require 'puppet':
// $VOXAGENT_TEST_RUBY, else `ruby` on PATH. Without one the test is skipped,
// unless VOXAGENT_REQUIRE_RUBY is set -- scripts/run-tests.sh sets it, so
// `task test` cannot pass by skipping the host's tests.
func Ruby(t *testing.T) string {
	t.Helper()
	ruby := os.Getenv("VOXAGENT_TEST_RUBY")
	if ruby == "" {
		found, err := exec.LookPath("ruby")
		if err != nil {
			unavailable(t, "no ruby on PATH")
			return ""
		}
		ruby = found
	}
	abs, err := filepath.Abs(ruby)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(abs, "-e", "require 'puppet'").CombinedOutput(); err != nil {
		unavailable(t, abs+" cannot require puppet: "+string(out))
	}
	return abs
}

func unavailable(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("VOXAGENT_REQUIRE_RUBY") != "" {
		t.Fatalf("VOXAGENT_REQUIRE_RUBY is set and %s", why)
	}
	t.Skip(why)
}

// Libdir is the fixture plugin tree: internal/extension/testdata/libdir.
func Libdir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate extensiontest")
	}
	return filepath.Join(filepath.Dir(filepath.Dir(here)), "testdata", "libdir")
}
