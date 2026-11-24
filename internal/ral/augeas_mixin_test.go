package ral

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
)

// testAugeasRun is a run over calls whose File.exist? answers true exactly
// for the paths in present.
func testAugeasRun(t *testing.T, calls []raltest.TraceCall, libdir string, present ...string) *augeasRun {
	t.Helper()
	r := newAugeasRun(raltest.NewScriptLib(t, calls), libdir)
	r.exists = func(p string) bool { return slices.Contains(present, p) }
	return r
}

// withAugVersionMemo replaces the per-process version memo for the test.
func withAugVersionMemo(t *testing.T, types map[string]bool) {
	t.Helper()
	augVersionChecked.Lock()
	old := augVersionChecked.types
	augVersionChecked.types = types
	augVersionChecked.Unlock()
	t.Cleanup(func() {
		augVersionChecked.Lock()
		augVersionChecked.types = old
		augVersionChecked.Unlock()
	})
}

// 01a: the class handle opens at the first augopen, loads the file once, and
// a second augopen of the same target only re-runs setvars.
func TestAugopenReplays01aFirstResource(t *testing.T) {
	calls := raltest.TraceLines(t, "01a", [2]int{4, 17}, [2]int{47, 47})
	r := testAugeasRun(t, calls, t.TempDir())
	aug, err := r.augopen(sysctlLens, "/etc/sysctl.conf", "$target/vm.swappiness")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := aug.Match("$target/vm.swappiness"); err != nil || len(m) != 0 {
		t.Fatalf("match = %v, %v", m, err)
	}
	again, err := r.augopen(sysctlLens, "/etc/sysctl.conf", "$target/vm.swappiness")
	if err != nil {
		t.Fatal(err)
	}
	if again != aug {
		t.Fatal("second augopen did not reuse the class handle")
	}
	r.EndRun()
}

// A later resource with a new target adds an incl and reloads.
func TestAugopenAddsInclForSecondTarget(t *testing.T) {
	calls := append(raltest.TraceLines(t, "01a", [2]int{4, 11}), raltest.ParseTrace(t, "script", `h1 aug_match "/augeas/load/Sysctl" => 1 "/augeas/load/Sysctl"
h1 aug_match "/augeas/load/Sysctl/incl[.='/etc/sysctl.d/50-x.conf']" => 0
h1 aug_set "/augeas/load/Sysctl/incl[.='/etc/sysctl.d/50-x.conf']" "/etc/sysctl.d/50-x.conf" => 0
h1 aug_load => 0
h1 aug_set "/augeas/context" "/files/etc/sysctl.d/50-x.conf" => 0
h1 aug_defnode "target" "/files/etc/sysctl.d/50-x.conf" NULL NULL => 1
h1 aug_defvar "resource" "$target/kernel.x" => 0
h1 aug_close
`)...)
	r := testAugeasRun(t, calls, t.TempDir())
	if _, err := r.augopen(sysctlLens, "/etc/sysctl.conf", "$target/vm.swappiness"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.augopen(sysctlLens, "/etc/sysctl.d/50-x.conf/", "$target/kernel.x"); err != nil {
		t.Fatal(err)
	}
	r.EndRun()
}

// 07b (M62): an existing file the lens cannot parse fails every augopen, with
// the loadpath and the error position in the text.
func TestAugopenLoadErrorNamesLoadpath(t *testing.T) {
	calls := raltest.TraceLines(t, "07b", [2]int{4, 20}, [2]int{97, 97})
	r := testAugeasRun(t, calls, "/tmp/gatelib", "/tmp/gatelib/augeas/lenses", "/etc/ssh/ssh_config")
	want := "Augeas didn't load /etc/ssh/ssh_config with Ssh.lns from /tmp/gatelib/augeas/lenses: Get did not match entire input (line:3, character:0)"
	for i := range 2 {
		if _, err := r.augopen(sshConfigLens, "/etc/ssh/ssh_config", "$target/x"); err == nil || err.Error() != want {
			t.Fatalf("augopen %d: err = %v\nwant %s", i+1, err, want)
		}
	}
	r.EndRun()
}

func TestAugopenLoadErrorWithoutLoadpathOrPosition(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_match "/augeas/load/Ssh" => 0
h1 aug_set "/augeas/load/Ssh/lens" "Ssh.lns" => 0
h1 aug_set "/augeas/load/Ssh/incl[last()+1]" "/etc/ssh/ssh_config" => 0
h1 aug_load => 0
h1 aug_match "/files/etc/ssh/ssh_config" => 0
h1 aug_get "/augeas/files/etc/ssh/ssh_config/error/message" => 1 "Permission denied"
h1 aug_match "/augeas/files/etc/ssh/ssh_config/error/pos" => 0
h1 aug_close
`)
	r := testAugeasRun(t, calls, t.TempDir(), "/etc/ssh/ssh_config")
	want := "Augeas didn't load /etc/ssh/ssh_config with Ssh.lns: Permission denied"
	if _, err := r.augopen(sshConfigLens, "/etc/ssh/ssh_config", "$target/x"); err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	r.EndRun()
}

// A failed aug_init is ruby-augeas' error and is retried by the next augopen,
// as `@aug_handler ||=` does not memoize a raise.
func TestAugopenInitFailureRetries(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\nh0 aug_init NULL NULL 64\n")
	r := testAugeasRun(t, calls, t.TempDir())
	for range 2 {
		if _, err := r.augopen(sysctlLens, "/etc/sysctl.conf", "$target/a"); err == nil || err.Error() != "unknown error - Failed to initialize Augeas" {
			t.Fatalf("err = %v", err)
		}
	}
	r.EndRun()
}

func TestAugopenLoadFailureIsAugeasError(t *testing.T) {
	calls := append(raltest.TraceLines(t, "01a", [2]int{4, 7}), raltest.ParseTrace(t, "script", "h1 aug_load => -1\nh1 aug_close\n")...)
	r := testAugeasRun(t, calls, t.TempDir())
	if _, err := r.augopen(sysctlLens, "/etc/sysctl.conf", "$target/a"); err == nil || err.Error() != "Augeas::Error" {
		t.Fatalf("err = %v", err)
	}
	r.EndRun()
}

func TestAugeasRunLoadpath(t *testing.T) {
	libdir := t.TempDir()
	r := newAugeasRun(nil, libdir)
	if lp := r.loadpath(); lp != nil {
		t.Fatalf("no lens dir: loadpath = %q, want nil", *lp)
	}
	if err := os.MkdirAll(filepath.Join(libdir, "augeas"), 0o755); err != nil {
		t.Fatal(err)
	}
	// File.exist? accepts any file type, so a regular file counts.
	if err := os.WriteFile(filepath.Join(libdir, "augeas", "lenses"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(libdir, "augeas", "lenses")
	if lp := r.loadpath(); lp == nil || *lp != want {
		t.Fatalf("loadpath = %v, want %q", lp, want)
	}
}

func TestFlushSaveFailure(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sysctl": true})
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_save => -1
h1 aug_match "/augeas//error" => 1 "/augeas/files/x"
h1 aug_match "/augeas/files/x/*" => 1 "/augeas/files/x/error"
h1 aug_get "/augeas/files/x/error" => 1 "put_failed"
h1 aug_load => 0
h1 aug_load => 0
h1 aug_close
`)
	r := testAugeasRun(t, calls, t.TempDir())
	ctx, logs := raltest.DebugContext("debug")
	err := r.flush(ctx, sysctlLens)
	var fe *FlushError
	if !errors.As(err, &fe) || err.Error() != "Failed to save Augeas tree to file. See debug logs for details." {
		t.Fatalf("err = %#v", err)
	}
	got := raltest.LinesAt(*logs, "Puppet::Type::Sysctl::ProviderAugeas")
	if want := []string{"Save failure details:\n/augeas/files/x/error = put_failed"}; !slices.Equal(got, want) {
		t.Fatalf("debug = %q, want %q", got, want)
	}
	r.EndRun()
}

// The reload in Ruby's ensure clause replaces the save error when it fails,
// and the second load is not reached.
func TestFlushLoadFailureReplacesSaveError(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sysctl": true})
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_save => -1
h1 aug_match "/augeas//error" => 0
h1 aug_load => -1
h1 aug_close
`)
	r := testAugeasRun(t, calls, t.TempDir())
	ctx, logs := raltest.DebugContext("debug")
	err := r.flush(ctx, sysctlLens)
	var fe *FlushError
	if !errors.As(err, &fe) || err.Error() != "Augeas::Error" {
		t.Fatalf("err = %#v", err)
	}
	if got := raltest.LinesAt(*logs, "Puppet::Type::Sysctl::ProviderAugeas"); !slices.Equal(got, []string{"Save failure details:\n"}) {
		t.Fatalf("debug = %q", got)
	}
	r.EndRun()
}

// 02b: the first flush of a type reads the version on its own handle between
// the two loads; the type's second flush answers from the memo.
func TestFlushVersionHandleOncePerType(t *testing.T) {
	withAugVersionMemo(t, nil)
	calls := raltest.TraceLines(t, "02b", [2]int{4, 4}, [2]int{37, 42}, [2]int{83, 86})
	r := testAugeasRun(t, calls, t.TempDir())
	ctx, _ := raltest.DebugContext("debug")
	for i := range 2 {
		if err := r.flush(ctx, sshConfigLens); err != nil {
			t.Fatalf("flush %d: %v", i+1, err)
		}
	}
	r.EndRun()
}

// 01a's flush: save, load, the version handle h2, load.
func TestFlushReplays01a(t *testing.T) {
	withAugVersionMemo(t, nil)
	calls := raltest.TraceLines(t, "01a", [2]int{4, 4}, [2]int{41, 47})
	r := testAugeasRun(t, calls, t.TempDir())
	ctx, _ := raltest.DebugContext("debug")
	if err := r.flush(ctx, sysctlLens); err != nil {
		t.Fatal(err)
	}
	r.EndRun()
}

// A version handle that cannot open fails the flush and leaves the memo
// empty, so the next flush opens one again.
func TestFlushVersionInitFailureNotMemoized(t *testing.T) {
	withAugVersionMemo(t, nil)
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_save => 0
h1 aug_load => 0
h0 aug_init NULL NULL 64
h1 aug_save => 0
h1 aug_load => 0
h2 aug_init NULL NULL 64
h2 aug_get "/augeas/version" => 1 "1.15.0"
h2 aug_close
h1 aug_load => 0
h1 aug_close
`)
	r := testAugeasRun(t, calls, t.TempDir())
	ctx, _ := raltest.DebugContext("debug")
	if err := r.flush(ctx, sysctlLens); err == nil || err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Fatalf("flush 1: err = %v", err)
	}
	if err := r.flush(ctx, sysctlLens); err != nil {
		t.Fatalf("flush 2: %v", err)
	}
	r.EndRun()
}

func TestParsedAsUsesNullLoadpath(t *testing.T) {
	libdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(libdir, "augeas", "lenses"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		from  int
		label string
		want  bool
	}{
		{"02b", 29, "Compression", false},
		{"02e", 30, "Ciphers", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := newAugeasRun(raltest.NewScriptLib(t, raltest.TraceLines(t, tc.id, [2]int{tc.from, tc.from + 4})), libdir)
			got, err := r.parsedAs("Ssh.lns", tc.label)
			if err != nil || got != tc.want {
				t.Fatalf("parsedAs = %v, %v; want %v", got, err, tc.want)
			}
			r.EndRun()
		})
	}
}

func TestParsedAsTextStoreFailureIsFalse(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_set "/input" "Bogus FOO\n" => 0
h1 aug_text_store "Ssh.lns" "/input" "/parsed" => -1
h1 aug_close
`)
	r := newAugeasRun(raltest.NewScriptLib(t, calls), t.TempDir())
	if got, err := r.parsedAs("Ssh.lns", "Bogus"); err != nil || got {
		t.Fatalf("parsedAs = %v, %v", got, err)
	}
	r.EndRun()
}

func TestParsedAsInitFailure(t *testing.T) {
	r := newAugeasRun(raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\n")), t.TempDir())
	if _, err := r.parsedAs("Ssh.lns", "X"); err == nil || err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Fatalf("err = %v", err)
	}
}

// 07b: two classes' handles, the first opened by an augopen that failed;
// EndRun closes each once, in open order, however often it runs.
func TestAugeasRunEndRunClosesEveryHandleOnce(t *testing.T) {
	calls := raltest.TraceLines(t, "07b", [2]int{4, 13}, [2]int{21, 29}, [2]int{97, 98})
	r := testAugeasRun(t, calls, "/tmp/gatelib", "/tmp/gatelib/augeas/lenses", "/etc/ssh/ssh_config", "/etc/ssh/sshd_config")
	if _, err := r.augopen(sshConfigLens, "/etc/ssh/ssh_config", "$target/x"); err == nil {
		t.Fatal("ssh_config augopen succeeded")
	}
	if _, err := r.augopen(sshdConfigLens, "/etc/ssh/sshd_config", "$target/*[label()=~regexp('PermitRootLogin', 'i')]"); err != nil {
		t.Fatal(err)
	}
	r.EndRun()
	r.EndRun()
}
