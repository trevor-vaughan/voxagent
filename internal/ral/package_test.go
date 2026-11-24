package ral

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// fakeRunner replays scripted responses and records what it was asked to run.
//
// A command with no scripted reply succeeds silently, so a test that cares
// about a failure has to say so -- the reverse would make every unasserted
// command look like a failure and hide which one the provider actually ran.
//
// sequences answers a command differently each time it runs -- rpm -q before
// and after an install -- and takes precedence over replies until exhausted.
type fakeRunner struct {
	replies   map[string]fakeReply
	sequences map[string][]fakeReply
	calls     [][]string
}

type fakeReply struct {
	exit int
	out  string
	err  error
}

// run implements commandRunner. Note the result order -- status, output, error
// -- which is the seam's (dnf.go) and Exec.execute's.
func (f *fakeRunner) run(_ context.Context, name string, args ...string) (int, string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	key := strings.Join(append([]string{name}, args...), " ")
	if seq := f.sequences[key]; len(seq) > 0 {
		f.sequences[key] = seq[1:]
		return seq[0].exit, seq[0].out, seq[0].err
	}
	r, ok := f.replies[key]
	if !ok {
		return 0, "", nil
	}
	return r.exit, r.out, r.err
}

// onlyCall returns the single command the provider ran, failing the test when
// it ran a different number of them.
func (f *fakeRunner) onlyCall(t *testing.T) []string {
	t.Helper()
	if len(f.calls) != 1 {
		t.Fatalf("ran %v, want exactly one command", f.calls)
	}
	return f.calls[0]
}

// ranExactly fails the test unless the provider ran exactly want, in order.
func (f *fakeRunner) ranExactly(t *testing.T, want ...[]string) {
	t.Helper()
	if len(f.calls) != len(want) {
		t.Fatalf("ran %v, want %v", f.calls, want)
	}
	for i := range want {
		if !equalArgv(f.calls[i], want[i]) {
			t.Errorf("command %d: ran %v, want %v", i, f.calls[i], want[i])
		}
	}
}

// requery is the argv of the re-query a versioned install runs after dnf
// returns (see verifyVersion), as a fakeRunner reply key and as an argv.
func requery(name string, virtual bool) []string {
	argv := []string{"/usr/bin/rpm", "-q", name, "--qf", rpmQueryFormat}
	if virtual {
		argv = append(argv, "--whatprovides")
	}
	return argv
}

func requeryKey(name string, virtual bool) string {
	return strings.Join(requery(name, virtual), " ")
}

// absentFirst answers the FIRST run of each query for name as not installed,
// leaving replies to answer later runs: a package absent before dnf runs and
// present for the post-install check.
func absentFirst(name string) map[string][]fakeReply {
	seq := map[string][]fakeReply{}
	for key, reply := range notInstalled(nil, name) {
		seq[key] = []fakeReply{reply}
	}
	return seq
}

// notInstalled scripts both queries failing for each name, as real rpm does for
// a package that neither exists nor is provided: the answer every name absent
// from the prefetched set gets before it is treated as purged.
func notInstalled(replies map[string]fakeReply, names ...string) map[string]fakeReply {
	if replies == nil {
		replies = map[string]fakeReply{}
	}
	for _, name := range names {
		replies[requeryKey(name, false)] = fakeReply{exit: 1, out: "package " + name + " is not installed\n"}
		replies[requeryKey(name, true)] = fakeReply{exit: 1, out: "no package provides " + name + "\n"}
	}
	return replies
}

// prefetched builds a Package as Prefetch would leave it: seams primed and the
// installed set known. The command paths are absolute because Prefetch resolves
// them with exec.LookPath -- Ruby's `commands :cmd => "dnf"` resolves the same
// way, and the absolute path is quoted verbatim in the failure message the
// report carries (see the measured string in TestPackageInstallFailure...).
func prefetched(run commandRunner, state map[string]string) Package {
	return Package{run: run, dnfPath: "/usr/bin/dnf", rpmPath: "/usr/bin/rpm", state: state}
}

func pkgRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Package", Title: title, Parameters: params}
}

func TestPackageInstallsAnAbsentPackage(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "tree")}
	p := prefetched(f, map[string]string{})

	changes, warnings, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Property != "ensure" || c.Name != "package_installed" {
		t.Errorf("Property/Name = %q/%q, want ensure/package_installed", c.Property, c.Name)
	}
	// Measured, report-root.json Package[tree]: previous_value "purged" --
	// provider/package.rb's substitute for a package that is not installed --
	// and message "created".
	if c.Was != "purged" || c.Is != "present" {
		t.Errorf("Was/Is = %v/%v, want purged/present", c.Was, c.Is)
	}
	if c.Message != "created" {
		t.Errorf("Message = %q, want created", c.Message)
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil", c.Err)
	}
	f.ranExactly(t, requery("tree", false), requery("tree", true), []string{"/usr/bin/dnf", "-y", "install", "tree"})
}

// ensure => installed is the same sync as present, and its desired_value
// normalises to "present" (AMENDMENT A2's table).
func TestPackageEnsureInstalledNormalisesToPresent(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "tree")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "installed"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Is != "present" || changes[0].Name != "package_installed" {
		t.Errorf("Is/Name = %v/%q, want present/package_installed", changes[0].Is, changes[0].Name)
	}
}

func TestPackageAlreadyInstalledIsANoChange(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"tree": "2.1.1-1.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("got %+v, want no changes", changes)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want nothing -- an in-sync package must not be touched", f.calls)
	}
}

func TestPackageRemovesAnInstalledPackage(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"zsh": "5.9-15.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("zsh", map[string]any{"ensure": "absent"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Name != "package_removed" {
		t.Errorf("Name = %q, want package_removed", c.Name)
	}
	// Measured, report-remove.json Package[zsh]: previous_value is the
	// installed VERSION, not the constant "present", and the message is
	// "removed".
	if c.Was != "5.9-15.el10" || c.Is != "absent" {
		t.Errorf("Was/Is = %v/%v, want 5.9-15.el10/absent", c.Was, c.Is)
	}
	if c.Message != "removed" {
		t.Errorf("Message = %q, want removed", c.Message)
	}
	// rpm -e, not dnf remove: the dnf provider defines no uninstall and
	// inherits rpm.rb:141-174 (AMENDMENT A8).
	if got, want := f.onlyCall(t), []string{"/usr/bin/rpm", "-e", "zsh"}; !equalArgv(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestPackageAbsentAndNotInstalledIsANoChange(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "zsh")}
	p := prefetched(f, map[string]string{"tree": "2.1.1-1.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("zsh", map[string]any{"ensure": "absent"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("got %+v, want no changes -- an absent package is already absent", changes)
	}
	// Only the reads that establish zsh is absent; nothing that changes it.
	if len(f.calls) != 2 || f.calls[0][0] != "/usr/bin/rpm" || f.calls[1][0] != "/usr/bin/rpm" {
		t.Errorf("ran %v, want only the two queries", f.calls)
	}
}

// A failed install is attributed to ensure, never returned bare. Puppet's
// sync_if_needed rescue (resource_harness.rb:147) records a failure event for
// the property that failed.
func TestPackageInstallFailureIsAttributedToEnsure(t *testing.T) {
	// Verbatim from the captured oracle: report-root.json,
	// Package[definitely-not-a-real-package], the event's message with the
	// transaction's "change from 'purged' to 'present' failed: " prefix
	// removed. The trailing newline is dnf's; Ruby's execute strips it
	// (util/execution.rb:312 interpolates output.strip), so the assertion below
	// proves this end strips it too.
	const dnfOutput = "Last metadata expiration check: 0:00:12 ago on Mon Sep 21 18:59:38 2026.\n" +
		"No match for argument: definitely-not-a-real-package\n" +
		"Error: Unable to find a match: definitely-not-a-real-package\n"
	const want = "Execution of '/usr/bin/dnf -y install definitely-not-a-real-package' returned 1: " +
		"Last metadata expiration check: 0:00:12 ago on Mon Sep 21 18:59:38 2026.\n" +
		"No match for argument: definitely-not-a-real-package\n" +
		"Error: Unable to find a match: definitely-not-a-real-package"

	f := &fakeRunner{replies: notInstalled(map[string]fakeReply{
		"/usr/bin/dnf -y install definitely-not-a-real-package": {exit: 1, out: dnfOutput},
	}, "definitely-not-a-real-package")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("definitely-not-a-real-package", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Property != "ensure" {
		t.Errorf("Property = %q, want ensure", c.Property)
	}
	// Was and Is describe the transition that was TRIED: the transaction builds
	// "change from 'purged' to 'present' failed: <Err>" from them.
	if c.Was != "purged" || c.Is != "present" {
		t.Errorf("Was/Is = %v/%v, want purged/present", c.Was, c.Is)
	}
	if c.Name != "package_installed" {
		t.Errorf("Name = %q, want package_installed -- the event name comes from the declared ensure, not from the outcome", c.Name)
	}
	if c.Err == nil {
		t.Fatal("Err = nil, want the install failure attributed to ensure")
	}
	if got := c.Err.Error(); got != want {
		t.Errorf("Err =\n%q\nwant\n%q", got, want)
	}
}

// A command that could not be started at all is still the ensure property's
// failure -- the seam reports that as an error rather than as an exit status.
func TestPackageInstallThatCannotRunIsAttributedToEnsure(t *testing.T) {
	boom := errors.New("fork/exec /usr/bin/dnf: permission denied")
	f := &fakeRunner{replies: notInstalled(map[string]fakeReply{
		"/usr/bin/dnf -y install tree": {err: boom},
	}, "tree")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if !errors.Is(changes[0].Err, boom) {
		t.Errorf("Err = %v, want it to wrap the seam's error", changes[0].Err)
	}
}

func TestPackageNoopReportsTheChangeAndRunsNothing(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "tree")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), true)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Err != nil {
		t.Errorf("Err = %v, want nil -- noop attempted nothing, so nothing failed", changes[0].Err)
	}
	// The queries are reads and run under noop; dnf does not.
	if len(f.calls) != 2 || f.calls[0][0] != "/usr/bin/rpm" || f.calls[1][0] != "/usr/bin/rpm" {
		t.Errorf("noop ran %v, want only the two queries", f.calls)
	}
}

func TestPackageNoopRemovalRunsNothing(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"zsh": "5.9-15.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("zsh", map[string]any{"ensure": "absent"}), true)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if len(f.calls) != 0 {
		t.Errorf("noop ran %v, want nothing", f.calls)
	}
}

// Measured, report-verabsent.json Package[lsof]: previous_value "purged",
// desired_value the declared version, message "created" -- and the part that is
// not obvious, name "package_changed" rather than package_installed, because
// only the literal present/installed newvalue declares that event
// (AMENDMENT A2, type/package.rb:99 against :136's catch-all newvalue(/./)).
func TestPackageInstallsADeclaredVersionOntoAPurgedPackage(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("lsof", false): {out: "lsof 0 4.98.0 7.el10 x86_64\n"},
	}, sequences: absentFirst("lsof")}
	p := prefetched(f, map[string]string{})

	changes, warnings, err := p.Apply(context.Background(),
		pkgRes("lsof", map[string]any{"ensure": "4.98.0-7.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Property != "ensure" || c.Name != "package_changed" {
		t.Errorf("Property/Name = %q/%q, want ensure/package_changed", c.Property, c.Name)
	}
	if c.Was != "purged" || c.Is != "4.98.0-7.el10" {
		t.Errorf("Was/Is = %v/%v, want purged/4.98.0-7.el10", c.Was, c.Is)
	}
	// AMENDMENT A10: the message keys on whether the package was INSTALLED, not
	// on the form of the declared value. It was not, so "created".
	if c.Message != "created" {
		t.Errorf("Message = %q, want created", c.Message)
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil", c.Err)
	}
	// yum.rb:283-284 appends "-<version>" to the package name, and :305 builds
	// [cmd, "-y", install_options, operation, wanted] with install_options
	// compacted away -- this provider does not implement it. The package is not
	// installed, so `current_package = query` is nil (yum.rb:290) and the
	// operation stays :install. yum.rb:313-321 then re-queries, because a
	// version was declared.
	f.ranExactly(t, requery("lsof", false), requery("lsof", true), []string{"/usr/bin/dnf", "-y", "install", "lsof-4.98.0-7.el10"}, requery("lsof", false))
}

// Measured, report-root.json Package[nano]: previous_value "8.1-1.el10",
// desired_value "8.1-3.el10", message "ensure changed '8.1-1.el10' to
// '8.1-3.el10'", name "package_changed".
//
// The operation is `upgrade`: yum.rb:298 selects update_command when the wanted
// version is the greater, and dnf.rb:44-47 overrides update_command to
// 'upgrade'. Run against the container's own util/rpm_compare.rb,
// rpm_compare_evr("8.1-3.el10", "8.1-1.el10") == 1.
func TestPackageUpgradesToANewerDeclaredVersion(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("nano", false): {out: "nano 0 8.1 3.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"nano": "8.1-1.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Name != "package_changed" {
		t.Errorf("Name = %q, want package_changed", c.Name)
	}
	if c.Was != "8.1-1.el10" || c.Is != "8.1-3.el10" {
		t.Errorf("Was/Is = %v/%v, want 8.1-1.el10/8.1-3.el10", c.Was, c.Is)
	}
	if c.Message != "ensure changed '8.1-1.el10' to '8.1-3.el10'" {
		t.Errorf("Message = %q, want the measured transition wording", c.Message)
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil", c.Err)
	}
	f.ranExactly(t, []string{"/usr/bin/dnf", "-y", "upgrade", "nano-8.1-3.el10"}, requery("nano", false))
}

// The mirror image, and the reason the operation is not cosmetic: `dnf -y
// install <older-version>` fails where `downgrade` succeeds, so choosing
// install here would turn a converging resource into a failed one.
// rpm_compare_evr("8.1-1.el10", "8.1-3.el10") == -1 in the container's Ruby,
// which is yum.rb:295's :downgrade.
func TestPackageDowngradesToAnOlderDeclaredVersion(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("nano", false): {out: "nano 0 8.1 1.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"nano": "8.1-3.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-1.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Was != "8.1-3.el10" || c.Is != "8.1-1.el10" {
		t.Errorf("Was/Is = %v/%v, want 8.1-3.el10/8.1-1.el10", c.Was, c.Is)
	}
	if c.Message != "ensure changed '8.1-3.el10' to '8.1-1.el10'" {
		t.Errorf("Message = %q, want the measured transition wording", c.Message)
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil", c.Err)
	}
	f.ranExactly(t, []string{"/usr/bin/dnf", "-y", "downgrade", "nano-8.1-1.el10"}, requery("nano", false))
}

// type/package.rb:199's `when *Array(is)` compares the declared string to the
// stored value before the provider is consulted at all.
func TestPackageAtTheDeclaredVersionIsANoChange(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"nano": "8.1-3.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(f.calls) != 0 {
		t.Errorf("changes = %+v, calls = %v, want neither", changes, f.calls)
	}
}

// PUP-1244, carried by rpm_compare_evr (rpmvercmp.go): a declared version with
// no release compares EQUAL to an installed one that has a release, so a
// release-less declaration is in sync and must not be reinstalled. Verified
// against the container's util/rpm_compare.rb: rpm_compare_evr("8.1",
// "8.1-3.el10") == 0.
func TestPackageDeclaredVersionWithoutAReleaseIsInSync(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"nano": "8.1-3.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(f.calls) != 0 {
		t.Errorf("changes = %+v, calls = %v, want neither -- PUP-1244 makes 8.1 in sync with 8.1-3.el10", changes, f.calls)
	}
}

// A package installed more than once is NEVER in sync with a declared version,
// so it syncs on every run. rpm.rb:191 returns false for any value containing
// the multiversion separator unless install_only is set, and install_only is
// outside this provider's bounded attribute set, so it is always false here.
//
// This is Ruby's behaviour reproduced deliberately, not a bug to be improved
// on: the standing directive is to match the original before optimising, and
// "the wanted version is already installed, so do nothing" is exactly the
// improvement Ruby declines to make.
func TestPackageMultiversionWithADeclaredVersionAlwaysSyncs(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("kernel", false): {out: "kernel 0 6.12.0 1.el10 x86_64\nkernel 0 6.12.0 2.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"kernel": "6.12.0-1.el10; 6.12.0-2.el10"})

	// The declared version IS one of the two installed, and it still syncs.
	changes, _, err := p.Apply(context.Background(),
		pkgRes("kernel", map[string]any{"ensure": "6.12.0-2.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	// The previous value is the JOINED string, which is what rpm.rb:105 puts in
	// the property hash and what the report would carry.
	if c.Was != "6.12.0-1.el10; 6.12.0-2.el10" || c.Is != "6.12.0-2.el10" {
		t.Errorf("Was/Is = %v/%v, want the joined string and the declared version", c.Was, c.Is)
	}
	if c.Message != "ensure changed '6.12.0-1.el10; 6.12.0-2.el10' to '6.12.0-2.el10'" {
		t.Errorf("Message = %q, want the transition from the joined string", c.Message)
	}
	// The operation comes from comparing the declared version against the whole
	// joined string, because that is what yum.rb:295-300 passes to
	// rpm_compare_evr. Measured in the container's Ruby:
	// rpm_compare_evr("6.12.0-2.el10", "6.12.0-1.el10; 6.12.0-2.el10") == 1
	// -- rpm_parse_evr reads the release as "1.el10; 6.12.0-2.el10", and
	// "2.el10" sorts above it -- so the operation is upgrade.
	f.ranExactly(t, []string{"/usr/bin/dnf", "-y", "upgrade", "kernel-6.12.0-2.el10"}, requery("kernel", false))

	// And it FAILS, every run (D-14): the re-query still finds both versions,
	// which the same rule refuses. Measured, out/rerun-ruby.json under
	// .test-output/package-oracle/d14/.
	const want = "Could not update: Failed to update to version 6.12.0-2.el10, " +
		"got version 6.12.0-1.el10; 6.12.0-2.el10 instead"
	if c.Err == nil || c.Err.Error() != want {
		t.Errorf("Err = %v, want %q", c.Err, want)
	}
}

// The one way a multiversion package IS in sync: type/package.rb:199 matches
// the declared string against the whole stored value BEFORE the provider's
// insync? sees it, and Array("a; b") is ["a; b"]. An odd catalog, and the only
// thing that distinguishes the type's equality test from the provider's
// version comparison -- without it that step could be deleted unnoticed.
func TestPackageMultiversionMatchedExactlyIsANoChange(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"kernel": "6.12.0-1.el10; 6.12.0-2.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("kernel", map[string]any{"ensure": "6.12.0-1.el10; 6.12.0-2.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(f.calls) != 0 {
		t.Errorf("changes = %+v, calls = %v, want neither", changes, f.calls)
	}
}

// D-14, measured (.test-output/package-oracle/d14/, out/upgrade-ruby.json):
// kernel-core 6.12.0-266 installed, `ensure => '6.12.0-267.el10'`. dnf
// installs 267 BESIDE 266, because kernel-core is installonly, and exits 0.
// yum.rb:313-321 then re-queries, finds "266; 267", and its insync? refuses any
// joined value -- so Ruby reports a FAILED resource on a machine it did change.
// voxagent reported a successful change until this test.
func TestPackageVersionInstallLeavingTwoVersionsFails(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("kernel-core", false): {out: "kernel-core 0 6.12.0 266.el10 x86_64\nkernel-core 0 6.12.0 267.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"kernel-core": "6.12.0-266.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("kernel-core", map[string]any{"ensure": "6.12.0-267.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Property != "ensure" || c.Was != "6.12.0-266.el10" || c.Is != "6.12.0-267.el10" {
		t.Errorf("Property/Was/Is = %q/%v/%v, want ensure/6.12.0-266.el10/6.12.0-267.el10", c.Property, c.Was, c.Is)
	}
	const want = "Could not update: Failed to update to version 6.12.0-267.el10, " +
		"got version 6.12.0-266.el10; 6.12.0-267.el10 instead"
	if c.Err == nil || c.Err.Error() != want {
		t.Errorf("Err = %v, want %q", c.Err, want)
	}
	f.ranExactly(t, []string{"/usr/bin/dnf", "-y", "upgrade", "kernel-core-6.12.0-267.el10"}, requery("kernel-core", false))
}

// The re-query asks the PROVIDER's insync? (yum.rb:320), not the type's: it
// skips type/package.rb:199's whole-value equality, which is the one step that
// can call a joined value in sync. So a declaration spelled as the joined
// string, which TestPackageMultiversionMatchedExactlyIsANoChange shows is in
// sync BEFORE a sync, still fails the check AFTER one. The scenario assumes dnf
// exits 0 for that argument, which a real dnf refuses unless strict=False; it
// is here to pin which insync? the check calls, not as a likely input.
func TestPackageVersionCheckSkipsTheTypesEqualityStep(t *testing.T) {
	const joined = "6.12.0-1.el10; 6.12.0-2.el10"
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("kernel", false): {out: "kernel 0 6.12.0 1.el10 x86_64\nkernel 0 6.12.0 2.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"kernel": "6.12.0-1.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("kernel", map[string]any{"ensure": joined}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	want := "Could not update: Failed to update to version " + joined + ", got version " + joined + " instead"
	if c := changes[0]; c.Err == nil || c.Err.Error() != want {
		t.Errorf("Err = %v, want %q", c.Err, want)
	}
}

// A release-less declaration is verified the way it is synced: PUP-1244 makes
// rpm_compare_evr("8.1", "8.1-3.el10") == 0, so the check accepts it.
func TestPackageVersionCheckAcceptsAReleaseLessDeclaration(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("nano", false): {out: "nano 0 8.1 3.el10 x86_64\n"},
	}, sequences: absentFirst("nano")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 || changes[0].Err != nil {
		t.Errorf("changes = %+v, want one with no Err", changes)
	}
	// The check RAN and accepted it, rather than being skipped for a
	// release-less value.
	f.ranExactly(t, requery("nano", false), requery("nano", true), []string{"/usr/bin/dnf", "-y", "install", "nano-8.1"}, requery("nano", false))
}

// An rpm that cannot be run at all fails the resource with the seam's own error
// and is not retried with --whatprovides -- what Ruby does here is unmeasured,
// so this keeps execute's position. Above all it must not read as a success.
func TestPackageVersionCheckThatCannotRunFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replies func(boom error) map[string]fakeReply
		want    [][]string
	}{
		{"the first query", func(boom error) map[string]fakeReply {
			return map[string]fakeReply{requeryKey("nano", false): {err: boom}}
		}, [][]string{requery("nano", false)}},
		{"the --whatprovides retry", func(boom error) map[string]fakeReply {
			return map[string]fakeReply{
				requeryKey("nano", false): {exit: 1, out: "package nano is not installed\n"},
				requeryKey("nano", true):  {err: boom},
			}
		}, [][]string{requery("nano", false), requery("nano", true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boom := errors.New("fork/exec /usr/bin/rpm: permission denied")
			f := &fakeRunner{replies: tc.replies(boom), sequences: absentFirst("nano")}
			p := prefetched(f, map[string]string{})

			changes, _, err := p.Apply(context.Background(),
				pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}), false)
			if err != nil {
				t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
			}
			if len(changes) != 1 {
				t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
			}
			if c := changes[0]; !errors.Is(c.Err, boom) || !strings.HasPrefix(c.Err.Error(), couldNotUpdate) {
				t.Errorf("Err = %v, want the seam's error behind %q", c.Err, couldNotUpdate)
			}
			f.ranExactly(t, append([][]string{requery("nano", false), requery("nano", true), {"/usr/bin/dnf", "-y", "install", "nano-8.1-3.el10"}}, tc.want...)...)
		})
	}
}

// dnf can exit 0 having installed something other than what was asked, and the
// check is what catches it -- the case D-14 named beyond multiversion.
func TestPackageVersionInstallLandingOnAnotherVersionFails(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("nano", false): {out: "nano 0 8.1 1.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	const want = "Could not update: Failed to update to version 8.1-3.el10, got version 8.1-1.el10 instead"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Errorf("changes = %+v, want one carrying %q", changes, want)
	}
}

// rpm.rb:91-101: when `rpm -q NAME` fails, allow_virtual -- true by default,
// and dnf.rb/yum.rb/rpm.rb define no defaultto_allow_virtual to change that --
// retries with --whatprovides, and the version checked is the one the
// PROVIDING package carries.
func TestPackageVersionCheckFallsBackToWhatProvides(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("vim", false): {exit: 1, out: "package vim is not installed\n"},
		requeryKey("vim", true):  {out: "vim-enhanced 2 9.1.083 5.el10 x86_64\n"},
	}, sequences: absentFirst("vim")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("vim", map[string]any{"ensure": "2:9.1.083-5.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 || changes[0].Err != nil {
		t.Errorf("changes = %+v, want one with no Err", changes)
	}
	f.ranExactly(t, requery("vim", false), requery("vim", true), []string{"/usr/bin/dnf", "-y", "install", "vim-2:9.1.083-5.el10"},
		requery("vim", false), requery("vim", true))
}

// Both queries failing is yum.rb:315's `Could not find package`, raised inside
// newvalue(/./) and so carrying its "Could not update: " prefix.
func TestPackageVersionCheckFindingNothingFails(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("lsof", false): {exit: 1, out: "package lsof is not installed\n"},
		requeryKey("lsof", true):  {exit: 1, out: "no package provides lsof\n"},
	}}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("lsof", map[string]any{"ensure": "4.98.0-7.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	const want = "Could not update: Could not find package lsof"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Errorf("changes = %+v, want one carrying %q", changes, want)
	}
}

// Several packages providing one virtual name make rpm.rb:249-253's
// nevra_to_multiversion_hash return an Array, and query's
// @property_hash.update(Array) raises a TypeError. Its message, measured on the
// image's own Ruby: {}.update([{a: 1}, {b: 2}]) raises
// "no implicit conversion of Array into Hash".
func TestPackageVersionCheckWithSeveralProvidersFails(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("mta", false): {exit: 1, out: "package mta is not installed\n"},
		requeryKey("mta", true):  {out: "postfix 2 3.8.5 1.el10 x86_64\nsendmail 0 8.18.1 1.el10 x86_64\n"},
	}, sequences: absentFirst("mta")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("mta", map[string]any{"ensure": "2:3.8.5-1.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	const want = "Could not update: no implicit conversion of Array into Hash"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Errorf("changes = %+v, want one carrying %q", changes, want)
	}
}

// A name the prefetched set does not carry is QUERIED before it is called
// purged. provider/package.rb's prefetch hands only matching resources a
// prefetched provider; any other keeps an empty @property_hash, and
// `properties` fills it from `query || { :ensure => :purged }` -- rpm -q NAME,
// then --whatprovides. Measured in the container (scenario V1,
// .test-output/package-oracle/unmeasured/): `vi`, provided by the installed
// vim-minimal, with `ensure => present` is unchanged in Ruby. voxagent used to
// run `dnf install vi`, which exits 0 doing nothing, and report "created".
func TestPackageVirtualNameProvidedByAnInstalledPackageIsInSync(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("vi", false): {exit: 1, out: "package vi is not installed\n"},
		requeryKey("vi", true):  {out: "vim-minimal 2 9.1.083 23.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{"vim-minimal": "2:9.1.083-23.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("vi", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none -- vi is provided, so present is in sync", changes)
	}
	f.ranExactly(t, requery("vi", false), requery("vi", true))
}

// The same query supplies the CURRENT value a declared version is compared
// with. Measured (scenario V2): Ruby reports previous_value
// "2:9.1.083-23.el10" -- vim-minimal's -- and, the declared release being
// older, runs `downgrade`. voxagent reported "purged" and ran `install`.
func TestPackageVirtualNameWithAnOlderVersionDowngrades(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("vi", false): {exit: 1, out: "package vi is not installed\n"},
		requeryKey("vi", true):  {out: "vim-minimal 2 9.1.083 23.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("vi", map[string]any{"ensure": "2:9.1.083-22.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if c := changes[0]; c.Was != "2:9.1.083-23.el10" || c.Message != "ensure changed '2:9.1.083-23.el10' to '2:9.1.083-22.el10'" {
		t.Errorf("Was/Message = %v/%q, want the provider's version as the current value", c.Was, c.Message)
	}
	if len(f.calls) < 3 || !equalArgv(f.calls[2], []string{"/usr/bin/dnf", "-y", "downgrade", "vi-2:9.1.083-22.el10"}) {
		t.Errorf("ran %v, want the two queries then a downgrade", f.calls)
	}
}

// A real absent package costs both queries -- Ruby pays the same, since query
// only returns nil after --whatprovides has failed too -- and is then purged.
func TestPackageAbsentNameIsQueriedThenPurged(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "tree")}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 || changes[0].Was != "purged" {
		t.Fatalf("changes = %+v, want one from purged", changes)
	}
	f.ranExactly(t, requery("tree", false), requery("tree", true), []string{"/usr/bin/dnf", "-y", "install", "tree"})
}

// A name the prefetched set DOES carry is not queried: prefetch handed Ruby's
// resource a provider with a full @property_hash, so `properties` returns it
// without calling query.
func TestPackagePrefetchedNameIsNotQueried(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"tree": "2.1.1-1.el10"})

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(f.calls) != 0 {
		t.Errorf("changes = %+v, calls = %v, want neither", changes, f.calls)
	}
}

// The query is a read, so noop runs it: Ruby's retrieve happens whether or not
// the sync does. Without it a noop run would report a would-be change for a
// provided virtual name that a real run makes no change to.
func TestPackageVirtualNameIsQueriedUnderNoop(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		requeryKey("vi", false): {exit: 1, out: "package vi is not installed\n"},
		requeryKey("vi", true):  {out: "vim-minimal 2 9.1.083 23.el10 x86_64\n"},
	}}
	p := prefetched(f, map[string]string{})

	changes, _, err := p.Apply(context.Background(), pkgRes("vi", map[string]any{"ensure": "present"}), true)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none", changes)
	}
	f.ranExactly(t, requery("vi", false), requery("vi", true))
}

// A query that fails outright is a RESOURCE-level failure, not an ensure one:
// in Ruby it raises from retrieve, before any property is evaluated, which
// resource_harness reports through failed_because -- the shape a bare error
// from Apply produces. Covers the several-providers TypeError, which
// rpm.rb:249-253 and query's @property_hash.update(Array) raise (the message
// measured on the image's Ruby; the image has no name two installed packages
// provide, so the whole path is unmeasured), and an rpm that cannot run.
func TestPackageQueryFailureIsAResourceFailure(t *testing.T) {
	boom := errors.New("fork/exec /usr/bin/rpm: permission denied")
	for _, tc := range []struct {
		name    string
		replies map[string]fakeReply
		want    string
	}{
		{"several providers", map[string]fakeReply{
			requeryKey("mta", false): {exit: 1, out: "package mta is not installed\n"},
			requeryKey("mta", true):  {out: "postfix 2 3.8.5 1.el10 x86_64\nsendmail 0 8.18.1 1.el10 x86_64\n"},
		}, "no implicit conversion of Array into Hash"},
		{"rpm cannot run", map[string]fakeReply{
			requeryKey("mta", false): {err: boom},
		}, boom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{replies: tc.replies}
			p := prefetched(f, map[string]string{})

			changes, _, err := p.Apply(context.Background(), pkgRes("mta", map[string]any{"ensure": "present"}), false)
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if len(changes) != 0 {
				t.Errorf("changes = %+v, want none -- no property was evaluated", changes)
			}
			for _, call := range f.calls {
				if call[0] == "/usr/bin/dnf" {
					t.Errorf("ran %v after the query failed", call)
				}
			}
		})
	}
}

// A dnf that FAILS is not re-queried: execute raises at yum.rb:306, before the
// check at :313 is reached.
func TestPackageFailedVersionInstallIsNotRequeried(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(map[string]fakeReply{
		"/usr/bin/dnf -y install lsof-999-1.el10": {exit: 1, out: "Error: Unable to find a match"},
	}, "lsof")}
	p := prefetched(f, map[string]string{})

	if _, _, err := p.Apply(context.Background(),
		pkgRes("lsof", map[string]any{"ensure": "999-1.el10"}), false); err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	f.ranExactly(t, requery("lsof", false), requery("lsof", true), []string{"/usr/bin/dnf", "-y", "install", "lsof-999-1.el10"})
}

// A failed version sync is attributed to ensure exactly as a failed install is,
// and the quoted command line carries the operation word -- so a wrong
// operation would diverge in the reported message as well as on the node.
//
// The "Could not update: " prefix is part of it. This test originally asserted
// the message WITHOUT it, which was wrong: a version string reaches
// type/package.rb's catch-all newvalue(/./) at :136, whose rescue re-raises as
// "Could not update: <detail>" just as newvalue(:latest)'s does at :118. Only
// present and absent are unwrapped. Measured both ways (AMENDMENT A16).
func TestPackageVersionFailureIsAttributedToEnsure(t *testing.T) {
	const dnfOutput = "Error: Packages for argument nano-8.1-1.el10 available, but not installed.\n"
	const want = "Could not update: " +
		"Execution of '/usr/bin/dnf -y downgrade nano-8.1-1.el10' returned 1: " +
		"Error: Packages for argument nano-8.1-1.el10 available, but not installed."

	f := &fakeRunner{replies: map[string]fakeReply{
		"/usr/bin/dnf -y downgrade nano-8.1-1.el10": {exit: 1, out: dnfOutput},
	}}
	p := prefetched(f, map[string]string{"nano": "8.1-3.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-1.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Property != "ensure" {
		t.Errorf("Property = %q, want ensure", c.Property)
	}
	// property.rb:227-238's fallback runs on the event name before the sync, so
	// a failed version sync is still package_changed.
	if c.Name != "package_changed" {
		t.Errorf("Name = %q, want package_changed", c.Name)
	}
	if c.Err == nil {
		t.Fatal("Err = nil, want the downgrade failure attributed to ensure")
	}
	if got := c.Err.Error(); got != want {
		t.Errorf("Err =\n%q\nwant\n%q", got, want)
	}
}

func TestPackageVersionNoopReportsTheChangeAndRunsNothing(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"nano": "8.1-1.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}), true)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Err != nil {
		t.Errorf("Err = %v, want nil -- noop attempted nothing, so nothing failed", changes[0].Err)
	}
	if len(f.calls) != 0 {
		t.Errorf("noop ran %v, want nothing", f.calls)
	}
}

// The namevar is the name parameter when set, the title otherwise: a resource
// titled for readability must still act on the package it names.
func TestPackageNameParameterOverridesTitle(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "nano")}
	p := prefetched(f, map[string]string{})

	_, _, err := p.Apply(context.Background(),
		pkgRes("the editor", map[string]any{"ensure": "present", "name": "nano"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	f.ranExactly(t, requery("nano", false), requery("nano", true), []string{"/usr/bin/dnf", "-y", "install", "nano"})
}

// The name parameter also selects which prefetched entry answers "is it
// installed?", so a titled resource whose package is present is a no-change.
func TestPackageNameParameterSelectsTheInstalledEntry(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"nano": "8.1-3.el10"})

	changes, _, err := p.Apply(context.Background(),
		pkgRes("the editor", map[string]any{"ensure": "present", "name": "nano"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(f.calls) != 0 {
		t.Errorf("changes = %+v, calls = %v, want neither", changes, f.calls)
	}
}

// A nil state map means Prefetch did not run. It is indistinguishable from "no
// package is installed" by inspection, and guessing the second would install
// every package in the catalog, so it is a resource-level error.
func TestPackageApplyWithoutPrefetchIsAResourceError(t *testing.T) {
	f := &fakeRunner{}
	p := Package{run: f}

	changes, _, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "present"}), false)
	if err == nil {
		t.Fatal("Apply returned no error, want one -- an unprefetched provider knows nothing")
	}
	if !strings.Contains(err.Error(), "prefetch") {
		t.Errorf("err = %v, want it to name prefetch as the missing step", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none -- a bare error discards them", changes)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want nothing", f.calls)
	}
}

// ensure values this provider does not implement must not silently do nothing.
// Pre-flight refuses them first (TestPackageSupportsValue); this is the
// backstop for a caller that did not ask.
//
// purged and held are here for a second reason: with a version string accepted,
// "anything that is not a symbolic value is a version" is the classification,
// and a symbolic name that slipped through it would be installed as one --
// `dnf -y install nano-purged`.
func TestPackageUnimplementedEnsureIsAResourceError(t *testing.T) {
	for _, ensure := range []string{"purged", "held", "disabled", ""} {
		f := &fakeRunner{replies: notInstalled(nil, "nano")}
		p := prefetched(f, map[string]string{})

		changes, _, err := p.Apply(context.Background(), pkgRes("nano", map[string]any{"ensure": ensure}), false)
		if err == nil {
			t.Errorf("ensure => %q returned no error, want one", ensure)
		}
		// Only the reads that establish nano's current value run; the
		// refusal comes after retrieve, as it does in Ruby.
		if len(changes) != 0 || len(f.calls) != 2 || f.calls[0][0] != "/usr/bin/rpm" || f.calls[1][0] != "/usr/bin/rpm" {
			t.Errorf("ensure => %q produced changes %+v and ran %v, want no change and only the two queries", ensure, changes, f.calls)
		}
	}
}

func TestPackageAttrsAreBounded(t *testing.T) {
	a := Package{}.Attrs()
	for _, want := range []string{"ensure", "name", "provider"} {
		if _, ok := a[want]; !ok {
			t.Errorf("Attrs missing %q", want)
		}
	}
	for _, unwanted := range []string{"source", "install_options", "mark", "flavor"} {
		if _, ok := a[unwanted]; ok {
			t.Errorf("Attrs declares %q, which is not implemented -- declaring an attribute is a promise to read it", unwanted)
		}
	}
}

func TestPackageSupportsValue(t *testing.T) {
	p := Package{}
	// Each case is asserted in BOTH directions. Asserting only that "purged" is
	// unsupported would pass against a provider that supports nothing.
	for _, tc := range []struct {
		attr, value string
		want        bool
	}{
		{"ensure", "present", true},
		{"ensure", "installed", true},
		{"ensure", "absent", true},
		// A concrete version, in the three shapes a catalog writes it: with a
		// release, without one, and epoch-qualified. type/package.rb:136's
		// newvalue(/./) accepts any string, so a finite list cannot express
		// this -- the predicate has to be "not one of the symbolic values".
		{"ensure", "8.1-3.el10", true},
		{"ensure", "8.1", true},
		{"ensure", "1:1.14.10-6.el10", true},
		// latest is implemented, so pre-flight claims it.
		{"ensure", "latest", true},
		// purged (:107) is a value Puppet implements and this provider does
		// not; held is not a package ensure value at all, so Ruby's catch-all
		// would read it as a version and let dnf reject `nano-held`. Both are
		// refused by name, which is this provider's bounded surface.
		{"ensure", "purged", false},
		{"ensure", "held", false},
		{"ensure", "disabled", false},
		// Empty is not a version. paramString's fallback means a resource with
		// no ensure reads as "present", but an explicitly empty one is a
		// catalog error, not an install of `nano-`.
		{"ensure", "", false},
		// A JSON boolean reaches the predicate as "true"/"false". Read as a
		// version it would install `nano-false`; it is a catalog error.
		{"ensure", "true", false},
		{"ensure", "false", false},
		{"provider", "dnf", true},
		{"provider", "gem", false},
		{"provider", "yum", false},
		{"name", "anything", true},
	} {
		if got := p.SupportsValue(tc.attr, tc.value); got != tc.want {
			t.Errorf("SupportsValue(%q, %q) = %v, want %v", tc.attr, tc.value, got, tc.want)
		}
	}
}

// Prefetch refuses to run as anyone but root, because provider/package/yum.rb:73
// does: the message is Ruby's, measured in report-nonroot.json as the tail of
// "Could not prefetch package provider 'dnf': ...".
func TestPackagePrefetchIsRootOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the refusal branch is unreachable, and TestPackagePrefetchReadsTheInstalledSet covers the other one")
	}
	f := &fakeRunner{}

	got, err := Package{run: f}.Prefetch(context.Background(), nil)
	if err == nil {
		t.Fatal("Prefetch returned no error as a non-root user, want the yum provider's refusal")
	}
	if err.Error() != "The yum provider can only be used as root" {
		t.Errorf("err = %q, want Ruby's wording verbatim", err.Error())
	}
	if got != nil {
		t.Errorf("provider = %+v, want nil alongside the error", got)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want nothing -- the root check precedes every command", f.calls)
	}
}

// The other half of the root check, which only `task dev:test-privileged` can
// reach. It fakes the seam, so it queries no real rpm database.
func TestPackagePrefetchReadsTheInstalledSet(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root: Prefetch refuses before it reads anything, which TestPackagePrefetchIsRootOnly covers")
	}
	rpmPath := lookPathOrSkip(t, "rpm")
	f := &fakeRunner{replies: map[string]fakeReply{
		rpmPath + " -qa --qf " + rpmQueryFormat: {out: "tree 0 2.1.1 1.el10 x86_64\ndbus 1 1.14.10 6.el10 x86_64\n"},
	}}

	got, err := Package{run: f}.Prefetch(context.Background(), nil)
	if err != nil {
		t.Fatalf("Prefetch returned %v", err)
	}
	p, ok := got.(Package)
	if !ok {
		t.Fatalf("Prefetch returned %T, want a primed Package", got)
	}
	if p.state["tree"] != "2.1.1-1.el10" || p.state["dbus"] != "1:1.14.10-6.el10" {
		t.Errorf("state = %v, want the parsed rpm query", p.state)
	}
	// The returned provider must carry the seams forward, or every Apply after
	// it would shell out for real.
	if p.run != commandRunner(f) {
		t.Errorf("run = %v, want the seam it was primed with", p.run)
	}
	if p.dnfPath == "" || p.rpmPath == "" {
		t.Errorf("dnfPath/rpmPath = %q/%q, want both resolved -- the failure message quotes them", p.dnfPath, p.rpmPath)
	}
}

// A failed rpm query is a Prefetch error, not an empty installed set: an empty
// set would read as "nothing is installed" and install the whole catalog.
func TestPackagePrefetchFailureIsAnError(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root: Prefetch refuses before it reads anything")
	}
	rpmPath := lookPathOrSkip(t, "rpm")
	f := &fakeRunner{replies: map[string]fakeReply{
		rpmPath + " -qa --qf " + rpmQueryFormat: {exit: 1, out: "rpm: unknown option\n"},
	}}

	got, err := Package{run: f}.Prefetch(context.Background(), nil)
	if err == nil {
		t.Fatal("Prefetch returned no error, want the rpm failure")
	}
	if !strings.Contains(err.Error(), "returned 1: rpm: unknown option") {
		t.Errorf("err = %q, want rpm's own output in Ruby's ExecutionFailure form", err.Error())
	}
	if got != nil {
		t.Errorf("provider = %+v, want nil alongside the error", got)
	}
}

// Measured, every package resource_status in the captured reports:
// provider_used is "dnf". type/package.rb declares no autorequire, and
// reinstall_on_refresh is outside the bounded attribute set.
func TestPackageIdentity(t *testing.T) {
	p := Package{}
	if p.Type() != "package" {
		t.Errorf("Type = %q, want package", p.Type())
	}
	used := p.ProviderUsed()
	if used == nil || *used != "dnf" {
		t.Errorf("ProviderUsed = %v, want dnf", used)
	}
	if reqs := p.AutoRequires(pkgRes("tree", nil), func(string) bool { return true }); reqs != nil {
		t.Errorf("AutoRequires = %v, want nil", reqs)
	}
	refreshed, err := p.Refresh(context.Background(), pkgRes("tree", nil), false)
	if refreshed || err != nil {
		t.Errorf("Refresh = %v, %v, want false, nil", refreshed, err)
	}
}

func equalArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// lookPathOrSkip resolves a command the way Prefetch does. The root-only tests
// fake the command's OUTPUT but not its existence, because Prefetch resolves
// dnf and rpm the way Ruby's `commands` declaration does -- both must be on
// PATH for the provider to be usable at all.
func lookPathOrSkip(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH, so Prefetch cannot get as far as the query: %v", name, err)
	}
	return path
}

// A version RANGE is refused at pre-flight, not handed to dnf.
//
// dnf.rb:14 declares :version_ranges, so Ruby resolves `>=8.1` through
// best_version and installs the greatest available version in the range. This
// provider does not implement that. Classified as a concrete version it would
// pass pre-flight and then run `dnf -y install nano->=8.1`, failing mid-run
// with the rest of the catalog already applied -- the half-converged node
// ValueBounded exists to prevent.
func TestPackageRefusesAVersionRange(t *testing.T) {
	p := Package{}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		// Ranges: refused.
		{">=8.1", false},
		{"<9.0", false},
		{">8.1", false},
		{"=8.1", false},
		{">8.1 <9.0", false},
		// NOT a range: the multiversion string rpm.rb:105 joins with "; ".
		// It contains a space, so a predicate keyed on whitespace rather than
		// on a leading operator would refuse it -- and type/package.rb:199
		// matches exactly this value by whole-value equality.
		{"6.12.0-1.el10; 6.12.0-2.el10", true},
		// Concrete versions: still accepted. Without these the test would pass
		// against a predicate that refuses every version.
		{"8.1-3.el10", true},
		{"8.1", true},
		{"1:1.14.10-6.el10", true},
		{"4.98.0-7.el10", true},
	} {
		if got := p.SupportsValue("ensure", tc.value); got != tc.want {
			t.Errorf("SupportsValue(ensure, %q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// THE headline case for the Change.Is / Change.IsDisplay split, reached from a
// second resource type.
//
// resource_harness.rb:188 puts `property.should` into the event's
// desired_value, and for `ensure => latest` that is the literal symbol
// `:latest`. type/package.rb:218-224's should_to_s substitutes the RESOLVED
// version into the message instead, from the @latest it cached during insync?.
// Measured, report-root.json Package[which]:
//
//	desired_value "latest"
//	message       "ensure changed '2.21-42.el10' to '0:2.21-44.el10'"
//
// Putting the resolved version in Is would fix the message by corrupting the
// event field, which is exactly what the split exists to prevent.
func TestPackageLatestKeepsLatestInTheEventAndTheVersionInTheMessage(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"which": "2.21-42.el10"})
	p.updates = map[string]string{"which": "0:2.21-44.el10"}

	changes, warnings, err := p.Apply(context.Background(), pkgRes("which", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none -- check-update succeeded", warnings)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Is != "latest" {
		t.Errorf("Is = %v, want the literal \"latest\" -- that is what desired_value carries", c.Is)
	}
	if c.IsDisplay != "0:2.21-44.el10" {
		t.Errorf("IsDisplay = %q, want the resolved, epoch-qualified version", c.IsDisplay)
	}
	if c.Was != "2.21-42.el10" {
		t.Errorf("Was = %v, want the installed version", c.Was)
	}
	// The epoch prefix is on the NEW value only, and unconditionally -- "0:"
	// included, for a package whose installed version carries no epoch at all
	// (AMENDMENT A3). A shared version formatter for this and the <version>
	// branch would be wrong in one direction or the other.
	if c.Message != "ensure changed '2.21-42.el10' to '0:2.21-44.el10'" {
		t.Errorf("Message = %q, want the measured wording", c.Message)
	}
	// AMENDMENT A2: latest never emits package_installed.
	if c.Name != "package_changed" || c.Property != "ensure" {
		t.Errorf("Name/Property = %q/%q, want package_changed/ensure", c.Name, c.Property)
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil", c.Err)
	}
	// AMENDMENT A15, measured with --debug: an INSTALLED package upgrades.
	// `install` would refuse to move a package dnf considers satisfied.
	if got, want := f.onlyCall(t), []string{"/usr/bin/dnf", "-y", "upgrade", "which"}; !equalArgv(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

// A package that is not installed short-circuits type/package.rb:164 BEFORE
// provider.latest runs, so no version is ever resolved: should_to_s has no
// @latest to substitute and the message rule reports the creation instead.
// Measured, report-root.json Package[lsof]: previous_value "purged",
// desired_value "latest", message "created", name "package_changed".
func TestPackageLatestOnAPurgedPackageIsCreated(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "lsof")}
	p := prefetched(f, map[string]string{})
	// A resolved update for the package exists and must NOT be used: the
	// short-circuit is ahead of the lookup, not a consequence of a miss.
	p.updates = map[string]string{"lsof": "0:4.98.0-7.el10"}

	changes, _, err := p.Apply(context.Background(), pkgRes("lsof", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	c := changes[0]
	if c.Was != "purged" || c.Is != "latest" {
		t.Errorf("Was/Is = %v/%v, want purged/latest", c.Was, c.Is)
	}
	if c.IsDisplay != "" {
		t.Errorf("IsDisplay = %q, want empty -- @latest was never computed, so `latest` renders as itself", c.IsDisplay)
	}
	if c.Message != "created" || c.Name != "package_changed" {
		t.Errorf("Message/Name = %q/%q, want created/package_changed", c.Message, c.Name)
	}
	f.ranExactly(t, requery("lsof", false), requery("lsof", true), []string{"/usr/bin/dnf", "-y", "install", "lsof"})
}

// yum.rb's latest falls back to the installed version when dnf reported no
// update for the package ("Yum didn't find updates, pretend the current version
// is the latest"), so insync? compares the value against itself and succeeds.
func TestPackageLatestWithNoUpdateAvailableIsANoChange(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"tree": "2.1.1-1.el10"})
	p.updates = map[string]string{"glibc": "0:2.39-141.el10"}

	changes, warnings, err := p.Apply(context.Background(), pkgRes("tree", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 0 || len(warnings) != 0 {
		t.Errorf("got changes %+v and warnings %+v, want neither", changes, warnings)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want nothing", f.calls)
	}
}

// `latest` compares the resolved version for EQUALITY
// (type/package.rb:181-184), never through rpmCompareEVR.
//
// rpm_compare.rb's no-release early return carries the comment "This should NOT
// be triggered if we're trying to ensure latest", and routing `latest` through
// the comparison is how it would be: any pair rpm calls equal but that differ
// as strings would be reported in sync forever, and the package would never be
// upgraded again.
//
// The pair below is chosen to separate the two predicates rather than to model
// dnf's output: rpmvercmp compares numeric segments as NUMBERS, so a release of
// `044.el10` and one of `44.el10` compare equal while the strings differ.
// Every other difference a real capture can produce happens to make the two
// predicates agree, so a realistic pair would not detect the mistake at all.
func TestPackageLatestComparesByEqualityNotByRpmVersion(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"which": "0:2.21-044.el10"})
	p.updates = map[string]string{"which": "0:2.21-44.el10"}
	if rpmCompareEVR("0:2.21-44.el10", "0:2.21-044.el10") != 0 {
		t.Fatal("the fixture no longer separates the two predicates; pick another pair")
	}

	changes, _, err := p.Apply(context.Background(), pkgRes("which", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1 -- the strings differ, so latest is out of sync: %+v", len(changes), changes)
	}
	if changes[0].IsDisplay != "0:2.21-44.el10" {
		t.Errorf("IsDisplay = %q, want the resolved version", changes[0].IsDisplay)
	}
}

// AMENDMENT A16: newvalue(:latest) rescues the provider's failure and re-raises
// it as "Could not update: <detail>", which newvalue(:present) does not do.
// Measured, a purged `latest` against an unreachable repo:
//
//	change from 'purged' to 'latest' failed: Could not update: Execution of
//	'/usr/bin/dnf -y install nano' returned 1: ...
func TestPackageLatestFailureIsPrefixedAndAttributedToEnsure(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		"/usr/bin/dnf -y upgrade which": {exit: 1, out: "Error: Failed to download metadata\n"},
	}}
	p := prefetched(f, map[string]string{"which": "2.21-42.el10"})
	p.updates = map[string]string{"which": "0:2.21-44.el10"}

	changes, _, err := p.Apply(context.Background(), pkgRes("which", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v, want the failure carried on the change instead", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].Err == nil {
		t.Fatal("Err = nil, want the dnf failure")
	}
	want := "Could not update: Execution of '/usr/bin/dnf -y upgrade which' returned 1: Error: Failed to download metadata"
	if changes[0].Err.Error() != want {
		t.Errorf("Err = %q, want %q", changes[0].Err, want)
	}
	// The change is still reported, and still attributed to ensure, because
	// resource_harness.rb:147 records a failure event for the property it was
	// syncing rather than discarding it.
	if changes[0].Property != "ensure" || changes[0].Is != "latest" {
		t.Errorf("Property/Is = %q/%v, want ensure/latest", changes[0].Property, changes[0].Is)
	}
}

func TestPackageLatestNoopRunsNothing(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"which": "2.21-42.el10"})
	p.updates = map[string]string{"which": "0:2.21-44.el10"}

	changes, _, err := p.Apply(context.Background(), pkgRes("which", map[string]any{"ensure": "latest"}), true)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %+v", len(changes), changes)
	}
	if changes[0].IsDisplay != "0:2.21-44.el10" || changes[0].Err != nil {
		t.Errorf("change = %+v, want the would-be upgrade with no error", changes[0])
	}
	if len(f.calls) != 0 {
		t.Errorf("noop ran %v, want nothing", f.calls)
	}
}

// A failed check-update is NOT a resource failure. yum.rb:129 warns and
// check_updates returns an empty hash, so latest falls back to the installed
// version and every `latest` resource goes in sync. Measured: the report's
// status is "unchanged" with an EMPTY resource_statuses -- an in-sync resource
// records no status at all.
func TestPackageCheckUpdateFailureWarnsAndLeavesTheResourceInSync(t *testing.T) {
	f := &fakeRunner{}
	p := prefetched(f, map[string]string{"which": "2.21-42.el10"})
	p.checkUpdateFailure = "Could not check for updates, '/usr/bin/dnf check-update' exited with 1"

	changes, warnings, err := p.Apply(context.Background(), pkgRes("which", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v, want the warning instead of a failure", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none -- the fallback compares the version against itself", changes)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v, want nothing", f.calls)
	}
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(warnings), warnings)
	}
	w := warnings[0]
	// Measured verbatim: the source is
	// the provider CLASS -- not "Puppet" and not the resource path -- and the
	// command in the message is absolute and carries no flag.
	if w.Source != "Puppet::Type::Package::ProviderDnf" {
		t.Errorf("Source = %q, want the provider class rendering", w.Source)
	}
	if w.Message != "Could not check for updates, '/usr/bin/dnf check-update' exited with 1" {
		t.Errorf("Message = %q, want yum.rb:129's wording with the absolute path", w.Message)
	}
	// AMENDMENT A13: Ruby emits this once per RUN, via memoization reached from
	// the first resource's insync?. voxagent resolves once in Prefetch and
	// dedupes the log, which matches both observable counts.
	if w.DedupeClass != "Puppet::Type::Package::ProviderDnf" {
		t.Errorf("DedupeClass = %q, want the provider class -- N latest resources log ONE warning", w.DedupeClass)
	}
}

// A purged `latest` resource must not carry the warning, because in Ruby it
// never reaches the code that raises it: type/package.rb:164 short-circuits
// ahead of provider.latest.
func TestPackageLatestOnAPurgedPackageRaisesNoCheckUpdateWarning(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(nil, "lsof")}
	p := prefetched(f, map[string]string{})
	p.checkUpdateFailure = "Could not check for updates, '/usr/bin/dnf check-update' exited with 1"

	_, warnings, err := p.Apply(context.Background(), pkgRes("lsof", map[string]any{"ensure": "latest"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none -- a purged resource never asks for the latest version", warnings)
	}
}

// AMENDMENT A13: one `dnf check-update` for the whole run, for no particular
// package, no matter how many resources declare `latest`. Ruby reaches that
// through yum.rb's class-level @latest_versions memo; this reaches it by
// resolving in Prefetch, which is the only place that sees the whole list.
func TestPackageResolveUpdatesRunsOnceForTheWholeRun(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		"/usr/bin/dnf -y check-update": {exit: 100, out: "\nwhich.x86_64  2.21-44.el10  baseos\n"},
	}}
	p := prefetched(f, map[string]string{"which": "2.21-42.el10", "nano": "8.1-3.el10", "lsof": "4.98.0-7.el10"})

	updates, failure, err := p.resolveUpdates(context.Background(), []catalog.Resource{
		pkgRes("which", map[string]any{"ensure": "latest"}),
		pkgRes("nano", map[string]any{"ensure": "latest"}),
		pkgRes("lsof", map[string]any{"ensure": "latest"}),
	})
	if err != nil {
		t.Fatalf("resolveUpdates returned %v", err)
	}
	if failure != "" {
		t.Errorf("failure = %q, want empty", failure)
	}
	if updates["which"] != "0:2.21-44.el10" {
		t.Errorf("updates = %v, want which resolved with its epoch", updates)
	}
	// AMENDMENT A14, measured with --debug: Ruby's argv is `-y check-update`
	// with NO package name -- it asks for every update at once and indexes the
	// answer.
	if got, want := f.onlyCall(t), []string{"/usr/bin/dnf", "-y", "check-update"}; !equalArgv(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

// Measured: a catalog whose `latest` resources are all purged emits
// ZERO warnings and runs check-update zero times, because type/package.rb:164
// short-circuits before provider.latest.
func TestPackageResolveUpdatesSkipsWhenNoInstalledResourceAsks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     map[string]string
		resources []catalog.Resource
	}{
		{"every latest resource is purged", map[string]string{"which": "2.21-42.el10"}, []catalog.Resource{
			pkgRes("nano", map[string]any{"ensure": "latest"}),
			pkgRes("lsof", map[string]any{"ensure": "latest"}),
		}},
		{"no resource declares latest", map[string]string{"which": "2.21-42.el10"}, []catalog.Resource{
			pkgRes("which", map[string]any{"ensure": "present"}),
			pkgRes("nano", map[string]any{"ensure": "8.1-3.el10"}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{replies: map[string]fakeReply{
				"/usr/bin/dnf -y check-update": {exit: 1, out: "boom"},
			}}
			p := prefetched(f, tc.state)

			updates, failure, err := p.resolveUpdates(context.Background(), tc.resources)
			if err != nil {
				t.Fatalf("resolveUpdates returned %v", err)
			}
			if len(updates) != 0 || failure != "" {
				t.Errorf("updates/failure = %v/%q, want neither", updates, failure)
			}
			if len(f.calls) != 0 {
				t.Errorf("ran %v, want nothing", f.calls)
			}
		})
	}
}

// The three exits yum.rb:121-130 distinguishes. 100 is the only one that names
// packages, 0 means none are available, and everything else warns -- treating
// 100 as a failure would turn the single most informative answer dnf gives into
// a warning and leave every latest resource permanently in sync.
func TestPackageResolveUpdatesReadsTheExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reply       fakeReply
		wantUpdates bool
		wantFailure string
	}{
		{"100 names the updates", fakeReply{exit: 100, out: "\nwhich.x86_64  2.21-44.el10  baseos\n"}, true, ""},
		{"0 means none are available", fakeReply{exit: 0}, false, ""},
		{"1 warns", fakeReply{exit: 1, out: "Error: Failed to download metadata"}, false,
			"Could not check for updates, '/usr/bin/dnf check-update' exited with 1"},
		{"127 warns with its own status", fakeReply{exit: 127}, false,
			"Could not check for updates, '/usr/bin/dnf check-update' exited with 127"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{replies: map[string]fakeReply{"/usr/bin/dnf -y check-update": tc.reply}}
			p := prefetched(f, map[string]string{"which": "2.21-42.el10"})

			updates, failure, err := p.resolveUpdates(context.Background(),
				[]catalog.Resource{pkgRes("which", map[string]any{"ensure": "latest"})})
			if err != nil {
				t.Fatalf("resolveUpdates returned %v", err)
			}
			if got := len(updates) > 0; got != tc.wantUpdates {
				t.Errorf("updates = %v, want any = %v", updates, tc.wantUpdates)
			}
			if failure != tc.wantFailure {
				t.Errorf("failure = %q, want %q", failure, tc.wantFailure)
			}
		})
	}
}

// Parsed from a REAL capture, not a hand-written line: leading blank line,
// NAME.ARCH in the first column, space-padded columns and trailing whitespace
// on some rows. The file's last line records the exit status the capture
// produced.
func TestPackageParsesCapturedCheckUpdateOutput(t *testing.T) {
	out, exit := readCheckUpdateCapture(t, "testdata/dnf-check-update-updates.txt")
	if exit != 100 {
		t.Fatalf("capture recorded exit %d, want 100 -- the fixture is the wrong one", exit)
	}
	updates := parseCheckUpdate(out)

	for name, want := range map[string]string{
		// The epoch is absent from every row of this capture and is defaulted
		// to "0" (yum.rb's update_to_hash), which is what makes the resolved
		// value epoch-qualified unconditionally -- AMENDMENT A3.
		"glibc":        "0:2.39-141.el10",
		"glibc-common": "0:2.39-141.el10",
		"libsemanage":  "0:3.11-2.el10",
		// Split at the LAST dot, not the first: a name that contains one must
		// survive, or every dotted package resolves to nothing.
		"gdb-gdbserver": "0:17.2-3.1.el10",
		// Indexed under NAME.ARCH as well, because yum considers the two
		// interchangeable (yum.rb's parse_updates writes both keys).
		"glibc.x86_64":         "0:2.39-141.el10",
		"gdb-gdbserver.x86_64": "0:17.2-3.1.el10",
	} {
		if got := updates[name]; got != want {
			t.Errorf("updates[%q] = %q, want %q", name, got, want)
		}
	}
	// Seven update rows, each indexed twice, and nothing else: the leading
	// blank line and the trailing EXIT marker must not become entries.
	if len(updates) != 14 {
		t.Errorf("parsed %d keys, want 14 (seven updates under two keys each): %v", len(updates), updates)
	}

	out, exit = readCheckUpdateCapture(t, "testdata/dnf-check-update.txt")
	if exit != 0 {
		t.Fatalf("capture recorded exit %d, want 0", exit)
	}
	if got := parseCheckUpdate(out); len(got) != 0 {
		t.Errorf("parsed %v from the no-updates capture, want nothing", got)
	}
}

// Repo download progress and the metadata-expiry header land on dnf's STDOUT
// when it is run Ruby's way (`-y`, not `-q`). Ruby does not suppress them; its
// parser ignores them, because they sit in their own blank-line-separated chunk
// and no three-field slice of them both contains a dot and parses as a version.
// Reproducing the filter is what makes the argv safe to match -- AMENDMENT A14.
func TestPackageCheckUpdateIgnoresProgressAndTrailingSections(t *testing.T) {
	updates := parseCheckUpdate("CentOS Stream 10 - BaseOS                3.4 MB/s | 6.8 MB     00:01    \n" +
		"CentOS Stream 10 - AppStream             602 kB/s | 3.3 MB     00:05    \n" +
		"\n" +
		"glibc.x86_64                             2.39-141.el10               baseos   \n" +
		// A package name that CONTAINS a dot, which the captured fixture has
		// none of. The arch is what follows the LAST dot, so this is
		// python3.12 on x86_64; cutting at the first one would resolve
		// "python3" and leave every `ensure => latest` on a dotted package
		// permanently in sync.
		"python3.12.x86_64                        3.12.11-2.el10              appstream\n" +
		"\n" +
		"Obsoleting Packages\n" +
		"gone.x86_64                              9.9-9.el10                  baseos   \n")

	if updates["glibc"] != "0:2.39-141.el10" {
		t.Errorf("updates = %v, want glibc resolved past the progress lines", updates)
	}
	if updates["python3.12"] != "0:3.12.11-2.el10" {
		t.Errorf("updates[python3.12] = %q, want the name split at its LAST dot", updates["python3.12"])
	}
	if _, ok := updates["python3"]; ok {
		t.Errorf("updates = %v, want no python3 entry -- that is a first-dot split", updates)
	}
	// Everything from the Obsoleting/Security:/Update marker onwards is
	// partitioned away, so an obsoleted package is not an available update.
	if _, ok := updates["gone"]; ok {
		t.Errorf("updates = %v, want nothing from the Obsoleting section", updates)
	}
	if len(updates) != 4 {
		t.Errorf("parsed %d keys, want 4: %v", len(updates), updates)
	}
}

// readCheckUpdateCapture splits a captured `dnf check-update` fixture into the
// output dnf wrote and the exit status the capture recorded on its last line.
func readCheckUpdateCapture(t *testing.T, path string) (string, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body, marker, found := strings.Cut(string(raw), "EXIT=")
	if !found {
		t.Fatalf("%s has no EXIT= marker; the capture convention changed", path)
	}
	exit, err := strconv.Atoi(strings.TrimSpace(marker))
	if err != nil {
		t.Fatalf("%s records exit %q, which is not a number: %v", path, marker, err)
	}
	return body, exit
}

// A failed VERSION sync carries the same "Could not update: " prefix a failed
// `latest` does, and a failed `present` does not.
//
// type/package.rb wraps two newvalue blocks in a rescue that re-raises as
// "Could not update: <detail>": newvalue(:latest) at :118 and the catch-all
// newvalue(/./) at :136, which is the one a version string reaches.
// newvalue(:present) at :99 and newvalue(:absent) at :103 have no such rescue.
// Measured both ways against the oracle (AMENDMENT A16).
func TestPackageVersionFailureCarriesTheCouldNotUpdatePrefix(t *testing.T) {
	const argv = "/usr/bin/dnf -y install lsof-999-1.el10"
	f := &fakeRunner{replies: notInstalled(map[string]fakeReply{
		argv: {exit: 1, out: "Error: Unable to find a match: lsof-999-1.el10"},
	}, "lsof")}
	p := Package{run: f, dnfPath: "/usr/bin/dnf", rpmPath: "/usr/bin/rpm", state: map[string]string{}}

	changes, _, err := p.Apply(context.Background(),
		pkgRes("lsof", map[string]any{"ensure": "999-1.el10"}), false)
	if err != nil {
		t.Fatalf("Apply returned a bare error %v -- the failure names a property", err)
	}
	if len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("changes = %+v, want one carrying Err", changes)
	}
	if !strings.HasPrefix(changes[0].Err.Error(), "Could not update: ") {
		t.Errorf("Err = %q, want it to start with %q", changes[0].Err, "Could not update: ")
	}
	// The prefix goes AHEAD of the ExecutionFailure wording, not instead of it.
	if !strings.Contains(changes[0].Err.Error(), "Execution of '"+argv+"' returned 1:") {
		t.Errorf("Err = %q, want it to keep the execution detail", changes[0].Err)
	}
}

// ...and a failed `present` still does NOT carry it. Without this, the test
// above passes against a provider that prefixes every failure.
func TestPackagePresentFailureHasNoCouldNotUpdatePrefix(t *testing.T) {
	f := &fakeRunner{replies: notInstalled(map[string]fakeReply{
		"/usr/bin/dnf -y install nope": {exit: 1, out: "No match for argument: nope"},
	}, "nope")}
	p := Package{run: f, dnfPath: "/usr/bin/dnf", rpmPath: "/usr/bin/rpm", state: map[string]string{}}

	changes, _, err := p.Apply(context.Background(),
		pkgRes("nope", map[string]any{"ensure": "present"}), false)
	if err != nil {
		t.Fatalf("Apply returned %v", err)
	}
	if len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("changes = %+v, want one carrying Err", changes)
	}
	if strings.Contains(changes[0].Err.Error(), "Could not update") {
		t.Errorf("Err = %q, want no Could not update prefix -- newvalue(:present) has no rescue", changes[0].Err)
	}
}

// rpmQueryFormat is rpm.rb:27's NEVRA_FORMAT byte for byte. Every other test
// builds its expected argv FROM the constant, so without this one any edit to
// it passes. Ruby's single-quoted '...\\n' is the two characters backslash and
// n, which rpm itself turns into a newline -- the same two characters a Go raw
// string carries.
func TestRpmQueryFormatIsRubysNevraFormat(t *testing.T) {
	const want = `%{NAME} %|EPOCH?{%{EPOCH}}:{0}| %{VERSION} %{RELEASE} %{ARCH}\n`
	if rpmQueryFormat != want {
		t.Errorf("rpmQueryFormat = %q, want rpm.rb's NEVRA_FORMAT %q", rpmQueryFormat, want)
	}
}
