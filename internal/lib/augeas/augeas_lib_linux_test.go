package augeas

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ruby-augeas digests are checked before the library is opened, so these
// run on a host without libaugeas: a dlopen attempt would report the library,
// not the Ruby file.
func TestProbeAugeasRefusesMissingRubyAugeas(t *testing.T) {
	lib, reason, ok := probeAugeas(t.TempDir())
	if ok || lib != nil || !strings.HasPrefix(reason, "ruby-augeas vendor_ruby/augeas.rb: ") {
		t.Fatalf("probeAugeas = %v, %q, %v; want refused for vendor_ruby/augeas.rb", lib, reason, ok)
	}
}

func TestProbeAugeasRefusesUnmeasuredRubyAugeas(t *testing.T) {
	root := t.TempDir()
	for _, rel := range rubyAugeasFiles {
		at := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, []byte("# not the measured file\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, reason, ok := probeAugeas(root)
	want := "ruby-augeas vendor_ruby/augeas.rb has sha256 "
	if ok || lib != nil || !strings.HasPrefix(reason, want) || !strings.Contains(reason, rubyAugeasDigests[0][0]) {
		t.Fatalf("probeAugeas = %v, %q, %v; want %q... naming the measured digest", lib, reason, ok, want)
	}
}
