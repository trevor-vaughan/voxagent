package ral

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

var _ Provider = Sysctl{}

// sysctlOver is the providers of a case: one Sysctl over the run.
func sysctlOver(runner sysctlRunner) func(*augeasRun) []Provider {
	return func(run *augeasRun) []Provider {
		return []Provider{Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}}
	}
}

func sysctlRes(title string, params map[string]any) catalog.Resource {
	return raltest.AugRes("Sysctl", title, params)
}

// eTitles is `-e` and the titles.
func eTitles(titles ...string) []string { return append([]string{"-e"}, titles...) }

const sysctlConf = "/etc/sysctl.conf"

// wantMessages fails unless the results' change messages are want, by
// resource.
func wantMessages(t *testing.T, got []caseResult, want ...[]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d results, want %d", len(got), len(want))
	}
	for i, r := range got {
		if !slices.Equal(r.messages(), want[i]) {
			t.Errorf("%s: messages %q, want %q", r.ref, r.messages(), want[i])
		}
	}
}

// wantNoErrors fails if any result carries an error or a failed change.
func wantNoErrors(t *testing.T, got []caseResult) {
	t.Helper()
	for _, r := range got {
		if r.err != nil {
			t.Errorf("%s: err %v", r.ref, r.err)
		}
		for _, ch := range r.changes {
			if ch.Err != nil {
				t.Errorf("%s: change err %v", r.ref, ch.Err)
			}
		}
	}
}

// M1: a key new to an absent file, live 10.
func TestSysctlReplays01a(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles("vm.swappiness"), out: "vm.swappiness = 10\n"},
		sysctlCall{args: []string{"-n", "vm.swappiness"}, out: "10\n"},
		sysctlCall{args: []string{"-w", "vm.swappiness=20"}, out: "vm.swappiness = 20\n"},
	)
	got := runAugeasCase(t, raltest.LoadTrace(t, "01a"), nil, sysctlOver(runner),
		[]catalog.Resource{sysctlRes("vm.swappiness", map[string]any{"value": "20"})}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, []string{"changed configuration value from '' to '20' and live value from '10' to '20'"})
	ch := got[0].changes[0]
	if ch.Property != "value" || ch.Was != nil || ch.Is != "20" || ch.Name != "" {
		t.Errorf("change = %+v", ch)
	}
}

// M3: three keys in the file, each branch of change_to_s that persists.
func TestSysctlReplays01c(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	titles := []string{"vm.swappiness", "net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter"}
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles(titles...), out: "vm.swappiness = 5\nnet.ipv4.ip_forward = 0\nnet.ipv4.conf.all.rp_filter = 1\n"},
		sysctlCall{args: []string{"-n", "vm.swappiness"}, out: "5\n"},
		sysctlCall{args: []string{"-w", "vm.swappiness=20"}, out: "vm.swappiness = 20\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.ip_forward"}, out: "0\n"},
		sysctlCall{args: []string{"-w", "net.ipv4.ip_forward=1"}, out: "net.ipv4.ip_forward = 1\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.conf.all.rp_filter"}, out: "1\n"},
		sysctlCall{args: []string{"-w", "net.ipv4.conf.all.rp_filter=1"}, out: "net.ipv4.conf.all.rp_filter = 1\n"},
	)
	got := runAugeasCase(t, raltest.LoadTrace(t, "01c"), []string{sysctlConf}, sysctlOver(runner), []catalog.Resource{
		sysctlRes(titles[0], map[string]any{"value": "20"}),
		sysctlRes(titles[1], map[string]any{"value": "1"}),
		sysctlRes(titles[2], map[string]any{"value": "1"}),
	}, false)
	wantNoErrors(t, got)
	wantMessages(t, got,
		[]string{"changed configuration value from '5' to '20' and live value from '5' to '20'"},
		[]string{"changed live value from '0' to '1'"},
		[]string{"changed configuration value from '0' to '1'"},
	)
	if ch := got[0].changes[0]; ch.Was != "5" || ch.Is != "20" {
		t.Errorf("change = %+v", ch)
	}
}

// M5: the real binary, every -w refused; silent, invalid and apply => false.
func TestSysctlReplays01e(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles("net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter", "foo.bar", "foo.qux", "vm.swappiness"),
			out: "net.ipv4.ip_forward = 0\nnet.ipv4.conf.all.rp_filter = 0\nvm.swappiness = 60\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.ip_forward"}, out: "0\n"},
		sysctlCall{args: []string{"-w", "net.ipv4.ip_forward=1"}, code: 1, out: "sysctl: permission denied on key \"net.ipv4.ip_forward\"\n"},
		sysctlCall{args: []string{"-w", "net.ipv4.conf.all.rp_filter=1"}, code: 1, out: "sysctl: permission denied on key \"net.ipv4.conf.all.rp_filter\"\n"},
	)
	got := runAugeasCase(t, raltest.LoadTrace(t, "01e"), []string{sysctlConf}, sysctlOver(runner), []catalog.Resource{
		sysctlRes("net.ipv4.ip_forward", map[string]any{"value": "1", "silent": false}),
		sysctlRes("net.ipv4.conf.all.rp_filter", map[string]any{"value": "1", "silent": true}),
		sysctlRes("foo.bar", map[string]any{"value": "1", "silent": false}),
		sysctlRes("foo.qux", map[string]any{"value": "1", "silent": true}),
		sysctlRes("vm.swappiness", map[string]any{"value": "20", "apply": false}),
	}, false)
	wantMessages(t, got,
		[]string{"changed configuration value from '' to '1' and live value from '0' to '1'"},
		[]string{"changed configuration value from '' to '1'"},
		nil, nil,
		[]string{"changed configuration value from '' to '20'"},
	)

	var fe *FlushError
	if !errors.As(got[0].err, &fe) ||
		fe.Error() != `Execution of '/usr/sbin/sysctl -w net.ipv4.ip_forward=1' returned 1: sysctl: permission denied on key "net.ipv4.ip_forward"` {
		t.Errorf("ip_forward err = %#v", got[0].err)
	}

	rp := got[1]
	if rp.err != nil {
		t.Errorf("rp_filter err = %v", rp.err)
	}
	if d := rp.debugAt("Sysctl[net.ipv4.conf.all.rp_filter](provider=augeas)"); !slices.Equal(d,
		[]string{"augeasproviders_sysctl not setting live value for net.ipv4.conf.all.rp_filter due to :silent mode"}) {
		t.Errorf("rp_filter provider debug = %q", d)
	}
	if d := rp.debugAt("Puppet::Type::Sysctl::ProviderAugeas"); !slices.Equal(d,
		[]string{"augeasprovider_sysctl ignoring failed attempt to set net.ipv4.conf.all.rp_filter due to :silent mode"}) {
		t.Errorf("rp_filter class debug = %q", d)
	}

	bar := got[2]
	if bar.err == nil || errors.As(bar.err, &fe) || bar.err.Error() != "Error: `foo.bar` is not a valid sysctl key" {
		t.Errorf("foo.bar err = %#v", bar.err)
	}

	qux := got[3]
	if qux.err != nil {
		t.Errorf("foo.qux err = %v", qux.err)
	}
	if d := qux.debugAt("Sysctl[foo.qux](provider=augeas)"); !slices.Equal(d,
		[]string{"augeasproviders_sysctl: `foo.qux` is not a valid sysctl key"}) {
		t.Errorf("foo.qux provider debug = %q", d)
	}
	if d := qux.debugAt("/Stage[main]/Main/Sysctl[foo.qux]/value"); !slices.Equal(d,
		[]string{"augeasproviders_sysctl: skipping insync? due to invalid resource `foo.qux`"}) {
		t.Errorf("foo.qux value debug = %q", d)
	}
	if got[4].err != nil {
		t.Errorf("vm.swappiness err = %v", got[4].err)
	}
}

// M8: the shim refuses kernel.hostname only; silent kernel.domainname's -w
// succeeds.
func TestSysctlReplays01h(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles("kernel.hostname", "kernel.domainname", "vm.swappiness"),
			out: "kernel.hostname = spine.example\nkernel.domainname = (none)\nvm.swappiness = 10\n"},
		sysctlCall{args: []string{"-n", "kernel.hostname"}, out: "spine.example\n"},
		sysctlCall{args: []string{"-w", "kernel.hostname=zz"}, code: 1, out: "sysctl: permission denied on key \"kernel.hostname\"\n"},
		sysctlCall{args: []string{"-w", "kernel.domainname=zz.example"}, out: "kernel.domainname = zz.example\n"},
		sysctlCall{args: []string{"-n", "vm.swappiness"}, out: "10\n"},
		sysctlCall{args: []string{"-w", "vm.swappiness=20"}, out: "vm.swappiness = 20\n"},
	)
	got := runAugeasCase(t, raltest.LoadTrace(t, "01h"), []string{sysctlConf}, sysctlOver(runner), []catalog.Resource{
		sysctlRes("kernel.hostname", map[string]any{"value": "zz", "silent": false}),
		sysctlRes("kernel.domainname", map[string]any{"value": "zz.example", "silent": true}),
		sysctlRes("vm.swappiness", map[string]any{"value": "20"}),
	}, false)
	wantMessages(t, got,
		[]string{"changed configuration value from '' to 'zz' and live value from 'spine.example' to 'zz'"},
		[]string{"changed configuration value from '' to 'zz.example'"},
		[]string{"changed configuration value from '' to '20' and live value from '10' to '20'"},
	)
	var fe *FlushError
	if !errors.As(got[0].err, &fe) ||
		fe.Error() != `Execution of '/usr/sbin/sysctl -w kernel.hostname=zz' returned 1: sysctl: permission denied on key "kernel.hostname"` {
		t.Errorf("kernel.hostname err = %#v", got[0].err)
	}
	wantNoErrors(t, got[1:])
	if d := got[1].debugAt("Puppet::Type::Sysctl::ProviderAugeas"); d != nil {
		t.Errorf("kernel.domainname's -w succeeded, yet class debug = %q", d)
	}
}

// M10: two vm.swappiness lines; flush removes the second and sets the first.
func TestSysctlReplays01j(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	runner := newScriptSysctl(t, sysctlCall{args: eTitles("vm.swappiness"), out: "vm.swappiness = 60\n"})
	got := runAugeasCase(t, raltest.LoadTrace(t, "01j"), []string{sysctlConf}, sysctlOver(runner),
		[]catalog.Resource{sysctlRes("vm.swappiness", map[string]any{"value": "20", "apply": false})}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, []string{"changed configuration value from '' to '20'"})
}

// S14: under noop, -e and -n still run; nothing is set or saved.
func TestSysctlNoopAppendsNoop(t *testing.T) {
	titles := []string{"vm.swappiness", "net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter"}
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles(titles...), out: "vm.swappiness = 5\nnet.ipv4.ip_forward = 0\nnet.ipv4.conf.all.rp_filter = 1\n"},
		sysctlCall{args: []string{"-n", "vm.swappiness"}, out: "5\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.ip_forward"}, out: "0\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.conf.all.rp_filter"}, out: "1\n"},
	)
	// 01c's prefetch and each resource's retrieve, then the run's close.
	calls := raltest.TraceLines(t, "01c", [2]int{4, 28}, [2]int{49, 55}, [2]int{73, 79}, [2]int{97, 97})
	got := runAugeasCase(t, calls, []string{sysctlConf}, sysctlOver(runner), []catalog.Resource{
		sysctlRes(titles[0], map[string]any{"value": "20"}),
		sysctlRes(titles[1], map[string]any{"value": "1"}),
		sysctlRes(titles[2], map[string]any{"value": "1"}),
	}, true)
	wantNoErrors(t, got)
	wantMessages(t, got,
		[]string{"changed configuration value from '5' to '20' and live value from '5' to '20' (noop)"},
		[]string{"changed live value from '0' to '1' (noop)"},
		[]string{"changed configuration value from '0' to '1' (noop)"},
	)
}

// sysctlScript is a hand-written trace for /etc/sysctl.conf, present and
// already loaded after the first block: lines are joined as they are given,
// and "augopen <name>" expands to augopen's calls on the loaded file with
// $resource bound to $target/<name> (defvar's count is resourceCount).
func sysctlScript(t *testing.T, resourceCount string, lines ...string) []raltest.TraceCall {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		name, ok := strings.CutPrefix(l, "augopen ")
		if !ok {
			b.WriteString(l + "\n")
			continue
		}
		b.WriteString(`h1 aug_match "/augeas/load/Sysctl" => 1 "/augeas/load/Sysctl"` + "\n" +
			`h1 aug_match "/augeas/load/Sysctl/incl[.='/etc/sysctl.conf']" => 1 "/augeas/load/Sysctl/incl"` + "\n" +
			`h1 aug_match "/files/etc/sysctl.conf" => 1 "/files/etc/sysctl.conf"` + "\n" +
			`h1 aug_set "/augeas/context" "/files/etc/sysctl.conf" => 0` + "\n" +
			`h1 aug_defnode "target" "/files/etc/sysctl.conf" NULL NULL => 1` + "\n" +
			`h1 aug_defvar "resource" "$target/` + name + `" => ` + resourceCount + "\n")
	}
	return raltest.ParseTrace(t, t.Name(), b.String())
}

// sysctlFirstLoad is the class handle's open and first load with $resource
// bound to $target/<first>.
func sysctlFirstLoad(first string) []string {
	return []string{
		"h1 aug_init NULL NULL 64",
		`h1 aug_match "/augeas/load/Sysctl" => 0`,
		`h1 aug_set "/augeas/load/Sysctl/lens" "Sysctl.lns" => 0`,
		`h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.conf" => 0`,
		"h1 aug_load => 0",
		`h1 aug_match "/files/etc/sysctl.conf" => 1 "/files/etc/sysctl.conf"`,
		`h1 aug_set "/augeas/context" "/files/etc/sysctl.conf" => 0`,
		`h1 aug_defnode "target" "/files/etc/sysctl.conf" NULL NULL => 1`,
		`h1 aug_defvar "resource" "$target/` + first + `" => 1`,
	}
}

// sysctlFound is prefetch's reads of a title the file holds once.
func sysctlFound(name, value string) []string {
	return []string{
		`h1 aug_match "$target/` + name + `" => 1 "/files/etc/sysctl.conf/` + name + `"`,
		`h1 aug_get "/files/etc/sysctl.conf/` + name + `" => 1 "` + value + `"`,
		`h1 aug_match "$target/#comment[following-sibling::*[1][self::` + name + `]]" => 0`,
	}
}

// I-36 (spec §8): an existing key's -w fails, so its flush stops before the
// save, but its set stays in the shared tree and the next Sysctl's save, the
// only one, writes it.
func TestSysctlFailedLiveApplyOnExistingKeyStaysPending(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	lines := sysctlFirstLoad("vm.swappiness")
	lines = append(lines, sysctlFound("vm.swappiness", "5")...)
	lines = append(lines, sysctlFound("net.ipv4.ip_forward", "0")...)
	lines = append(lines,
		"augopen vm.swappiness", `h1 aug_get "$resource" => 1 "5"`,
		"augopen vm.swappiness", `h1 aug_set "$resource" "20" => 0`,
		"augopen net.ipv4.ip_forward", `h1 aug_get "$resource" => 1 "0"`,
		"augopen net.ipv4.ip_forward", `h1 aug_set "$resource" "1" => 0`,
		"augopen net.ipv4.ip_forward",
		`h1 aug_match "$target/net.ipv4.ip_forward" => 1 "/files/etc/sysctl.conf/net.ipv4.ip_forward"`,
		"h1 aug_save => 0",
		"h1 aug_load => 0",
		"h2 aug_init NULL NULL 64",
		`h2 aug_get "/augeas/version" => 1 "1.15.0"`,
		"h2 aug_close",
		"h1 aug_load => 0",
		"h1 aug_close",
	)
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles("vm.swappiness", "net.ipv4.ip_forward"), out: "vm.swappiness = 5\nnet.ipv4.ip_forward = 0\n"},
		sysctlCall{args: []string{"-n", "vm.swappiness"}, out: "5\n"},
		sysctlCall{args: []string{"-w", "vm.swappiness=20"}, code: 1, out: "sysctl: permission denied on key \"vm.swappiness\"\n"},
		sysctlCall{args: []string{"-n", "net.ipv4.ip_forward"}, out: "0\n"},
		sysctlCall{args: []string{"-w", "net.ipv4.ip_forward=1"}, out: "net.ipv4.ip_forward = 1\n"},
	)
	got := runAugeasCase(t, sysctlScript(t, "1", lines...), []string{sysctlConf}, sysctlOver(runner), []catalog.Resource{
		sysctlRes("vm.swappiness", map[string]any{"value": "20"}),
		sysctlRes("net.ipv4.ip_forward", map[string]any{"value": "1"}),
	}, false)
	wantMessages(t, got,
		[]string{"changed configuration value from '5' to '20' and live value from '5' to '20'"},
		[]string{"changed configuration value from '0' to '1' and live value from '0' to '1'"},
	)
	var fe *FlushError
	if !errors.As(got[0].err, &fe) {
		t.Errorf("vm.swappiness err = %#v, want a FlushError", got[0].err)
	}
	wantNoErrors(t, got[1:])
}

// A key the file holds but the kernel does not: -n fails the property, and
// nothing is synced or flushed.
func TestSysctlLiveReadFailureFailsProperty(t *testing.T) {
	lines := sysctlFirstLoad("foo.bar")
	lines = append(lines, sysctlFound("foo.bar", "1")...)
	lines = append(lines, "augopen foo.bar", `h1 aug_get "$resource" => 1 "1"`, "h1 aug_close")
	for _, noop := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "noop"}[noop], func(t *testing.T) {
			runner := newScriptSysctl(t,
				sysctlCall{args: eTitles("foo.bar")},
				sysctlCall{args: []string{"-n", "foo.bar"}, code: 255, out: "sysctl: cannot stat /proc/sys/foo/bar: No such file or directory\n"},
			)
			got := runAugeasCase(t, sysctlScript(t, "1", lines...), []string{sysctlConf}, sysctlOver(runner),
				[]catalog.Resource{sysctlRes("foo.bar", map[string]any{"value": "2"})}, noop)
			if got[0].err != nil || len(got[0].changes) != 1 {
				t.Fatalf("result = %+v", got[0])
			}
			ch := got[0].changes[0]
			want := "Execution of '/usr/sbin/sysctl -n foo.bar' returned 255: sysctl: cannot stat /proc/sys/foo/bar: No such file or directory"
			if ch.Property != "value" || ch.Was != "1" || ch.Is != "2" || ch.Err == nil || ch.Err.Error() != want {
				t.Errorf("change = %+v", ch)
			}
		})
	}
}

// Ruby's equal collapses runs of ASCII whitespace: a live value with tabs is
// in sync with spaces, so nothing beyond the retrieve happens.
func TestSysctlEqualCollapsesAsciiWhitespace(t *testing.T) {
	const name = "net.ipv4.tcp_rmem"
	lines := sysctlFirstLoad(name)
	lines = append(lines, sysctlFound(name, "4096 87380 6291456")...)
	lines = append(lines, "augopen "+name, `h1 aug_get "$resource" => 1 "4096 87380 6291456"`, "h1 aug_close")
	runner := newScriptSysctl(t,
		sysctlCall{args: eTitles(name), out: name + " = 4096\t87380\t6291456\n"},
		sysctlCall{args: []string{"-n", name}, out: "4096\t87380\t6291456\n"},
	)
	got := runAugeasCase(t, sysctlScript(t, "1", lines...), []string{sysctlConf}, sysctlOver(runner),
		[]catalog.Resource{sysctlRes(name, map[string]any{"value": "4096 87380 6291456"})}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, nil)

	// Out of sync, the message shows the live value as read.
	cur := "4096 87380 4194304"
	if got := sysctlChangeToS(&cur, "4096\t87380\t6291456", cur, true, true); got != "changed live value from '4096\t87380\t6291456' to '4096 87380 4194304'" {
		t.Errorf("message = %q", got)
	}
}

// SYT:37-50, every branch.
func TestSysctlChangeToS(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name           string
		cur            *string
		live, should   string
		apply, persist bool
		want           string
	}{
		{"apply false", s("5"), "", "20", false, true, "changed configuration value from '5' to '20'"},
		{"apply false, absent", nil, "", "20", false, true, "changed configuration value from '' to '20'"},
		{"file equal", s("1"), "0", "1", true, true, "changed live value from '0' to '1'"},
		{"file equal after collapse", s("1  2"), "0", "1 2", true, true, "changed live value from '0' to '1 2'"},
		{"live equal", s("0"), "1", "1", true, true, "changed configuration value from '0' to '1'"},
		{"absent, live equal", nil, "1", "1", true, true, "changed configuration value from '' to '1'"},
		{"both differ", s("5"), "5", "20", true, true, "changed configuration value from '5' to '20' and live value from '5' to '20'"},
		{"both differ, absent", nil, "10", "20", true, true, "changed configuration value from '' to '20' and live value from '10' to '20'"},
		{"both differ, no persist", s("5"), "5", "20", true, false, "changed live value from '5' to '20'"},
	} {
		if got := sysctlChangeToS(tc.cur, tc.live, tc.should, tc.apply, tc.persist); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSysctlRefreshIsNothing(t *testing.T) {
	ok, err := Sysctl{}.Refresh(t.Context(), sysctlRes("vm.swappiness", map[string]any{"value": "1"}), false)
	if ok || err != nil {
		t.Errorf("Refresh = %v, %v", ok, err)
	}
}

// sysctlSaveAndClose is CORE's flush after a successful save, the first per
// type, then the run's close.
var sysctlSaveAndClose = []string{
	"h1 aug_save => 0",
	"h1 aug_load => 0",
	"h2 aug_init NULL NULL 64",
	`h2 aug_get "/augeas/version" => 1 "1.15.0"`,
	"h2 aug_close",
	"h1 aug_load => 0",
	"h1 aug_close",
}

// SYP:225-271: a duplicate's managed comment goes with it; a trailing comment
// that is not the key's stays.
func TestSysctlCleanupDuplicatesRemovesManagedComment(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	const f = "/files/etc/sysctl.conf/"
	lines := sysctlFirstLoad("vm.swappiness")
	lines = append(lines,
		`h1 aug_match "$target/vm.swappiness" => 2 "`+f+`vm.swappiness[1]" "`+f+`vm.swappiness[2]"`,
		`h1 aug_get "`+f+`vm.swappiness[1]" => 1 "5"`,
		`h1 aug_match "$target/#comment[following-sibling::*[1][self::vm.swappiness]]" => 0`,
		"augopen vm.swappiness", `h1 aug_get "$resource" => -1 NULL`,
		"augopen vm.swappiness", `h1 aug_set "$resource" "20" => -1`,
		"augopen vm.swappiness",
		`h1 aug_match "$target/vm.swappiness" => 2 "`+f+`vm.swappiness[1]" "`+f+`vm.swappiness[2]"`,
		`h1 aug_match "$target/*" => 4 "`+f+`vm.swappiness[1]" "`+f+`#comment[1]" "`+f+`vm.swappiness[2]" "`+f+`#comment[2]"`,
		`h1 aug_get "`+f+`#comment[1]" => 1 "vm.swappiness: note"`,
		`h1 aug_rm "`+f+`#comment[1]" => 1`,
		`h1 aug_rm "`+f+`vm.swappiness[2]" => 1`,
		`h1 aug_match "$target/*" => 2 "`+f+`vm.swappiness" "`+f+`#comment"`,
		`h1 aug_get "`+f+`#comment" => 1 "kernel.x: vm.swappiness: not first"`,
		`h1 aug_set "$target/vm.swappiness" "20" => 0`,
	)
	lines = append(lines, sysctlSaveAndClose...)
	runner := newScriptSysctl(t, sysctlCall{args: eTitles("vm.swappiness"), out: "vm.swappiness = 60\n"})
	got := runAugeasCase(t, sysctlScript(t, "2", lines...), []string{sysctlConf}, sysctlOver(runner),
		[]catalog.Resource{sysctlRes("vm.swappiness", map[string]any{"value": "20", "apply": false})}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, []string{"changed configuration value from '' to '20'"})
}

// SYP:198-212: a key new to the file goes after its commented-out line.
func TestSysctlCreateInsertsAfterCommentedLine(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	lines := sysctlFirstLoad("vm.swappiness")
	lines = append(lines,
		`h1 aug_match "$target/vm.swappiness" => 0`,
		"augopen vm.swappiness", `h1 aug_get "$resource" => 0 NULL`,
		"augopen vm.swappiness", `h1 aug_set "$resource" "20" => -1`,
		"augopen vm.swappiness", `h1 aug_match "$target/vm.swappiness" => 0`,
		"augopen vm.swappiness",
		`h1 aug_match "$target/#comment[.=~regexp('vm.swappiness([^a-z.].*)?')]" => 1 "/files/etc/sysctl.conf/#comment"`,
		`h1 aug_insert "/files/etc/sysctl.conf/#comment" "vm.swappiness" 0 => 0`,
		`h1 aug_set "$target/vm.swappiness" "20" => 0`,
		`h1 aug_set "/augeas/context" "/files/etc/sysctl.conf" => 0`,
		`h1 aug_defnode "target" "/files/etc/sysctl.conf" NULL NULL => 1`,
		`h1 aug_defvar "resource" "$target/vm.swappiness" => 1`,
	)
	lines = append(lines, sysctlSaveAndClose...)
	runner := newScriptSysctl(t, sysctlCall{args: eTitles("vm.swappiness"), out: "vm.swappiness = 60\n"})
	got := runAugeasCase(t, sysctlScript(t, "0", lines...), []string{sysctlConf}, sysctlOver(runner),
		[]catalog.Resource{sysctlRes("vm.swappiness", map[string]any{"value": "20", "apply": false})}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, []string{"changed configuration value from '' to '20'"})
}

// A failed save after the change is a FlushError carrying CORE's message.
func TestSysctlSaveFailureIsFlushError(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sysctl": true})
	lines := sysctlFirstLoad("vm.swappiness")
	lines = append(lines, sysctlFound("vm.swappiness", "5")...)
	lines = append(lines,
		"augopen vm.swappiness", `h1 aug_get "$resource" => 1 "5"`,
		"augopen vm.swappiness", `h1 aug_set "$resource" "20" => 0`,
		"augopen vm.swappiness", `h1 aug_match "$target/vm.swappiness" => 1 "/files/etc/sysctl.conf/vm.swappiness"`,
		"h1 aug_save => -1",
		`h1 aug_match "/augeas//error" => 0`,
		"h1 aug_load => 0",
		"h1 aug_load => 0",
		"h1 aug_close",
	)
	runner := newScriptSysctl(t, sysctlCall{args: eTitles("vm.swappiness"), out: "vm.swappiness = 5\n"})
	got := runAugeasCase(t, sysctlScript(t, "1", lines...), []string{sysctlConf}, sysctlOver(runner),
		[]catalog.Resource{sysctlRes("vm.swappiness", map[string]any{"value": "20", "apply": false})}, false)
	wantMessages(t, got, []string{"changed configuration value from '5' to '20'"})
	var fe *FlushError
	if !errors.As(got[0].err, &fe) || fe.Error() != "Failed to save Augeas tree to file. See debug logs for details." {
		t.Errorf("err = %#v", got[0].err)
	}
}
