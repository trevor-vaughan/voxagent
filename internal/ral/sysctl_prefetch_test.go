package ral

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

// sysctlCall is one scripted sysctl command: its argv after the binary and
// the exit status and combined output it answers with.
type sysctlCall struct {
	args []string
	code int
	out  string
}

// scriptSysctl is a sysctlRunner answering calls in order; any other command
// fails the test, and calls not made fail it at cleanup.
type scriptSysctl struct {
	t     *testing.T
	bin   string
	calls []sysctlCall
	next  int
}

func newScriptSysctl(t *testing.T, calls ...sysctlCall) *scriptSysctl {
	s := &scriptSysctl{t: t, bin: "/usr/sbin/sysctl", calls: calls}
	t.Cleanup(func() {
		if !t.Failed() && s.next != len(s.calls) {
			t.Errorf("sysctl: %d scripted command(s) not run; next %v", len(s.calls)-s.next, s.calls[s.next].args)
		}
	})
	return s
}

func (s *scriptSysctl) run(_ context.Context, name string, args ...string) (int, string, error) {
	s.t.Helper()
	if s.next == len(s.calls) {
		s.t.Fatalf("sysctl: unexpected command %s %v", name, args)
	}
	c := s.calls[s.next]
	if name != s.bin || !slices.Equal(args, c.args) {
		s.t.Fatalf("sysctl: command %d = %s %q, want %s %q", s.next+1, name, args, s.bin, c.args)
	}
	s.next++
	return c.code, c.out, nil
}

func sysctlResources(titles ...string) []catalog.Resource {
	rs := make([]catalog.Resource, len(titles))
	for i, title := range titles {
		rs[i] = raltest.AugRes("Sysctl", title, map[string]any{"value": "1"})
	}
	return rs
}

func lastTraceCall(t *testing.T, id string) []raltest.TraceCall {
	calls := raltest.LoadTrace(t, id)
	return calls[len(calls)-1:]
}

func titlesN(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("k.n%d", i+1)
	}
	return out
}

func TestSysctlArgSlices(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []int // titles per slice
	}{
		{0, nil},
		{1, []int{1}},
		{30, []int{30}},
		{31, []int{30, 31}},
		{63, []int{30, 60, 63}},
		{65, []int{30, 60, 65}}, // M9
	} {
		titles := titlesN(tc.n)
		got := sysctlArgSlices(titles)
		if len(got) != len(tc.want) {
			t.Fatalf("%d titles: %d slices, want %d", tc.n, len(got), len(tc.want))
		}
		for i, n := range tc.want {
			want := append([]string{"-e"}, titles[:n]...)
			if !slices.Equal(got[i], want) {
				t.Errorf("%d titles: slice %d = %v, want %v", tc.n, i, got[i], want)
			}
		}
	}
}

func TestParseSysctlE(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"value with =", "vm.swappiness = 10\nnet.x = a=b\n", []string{"vm.swappiness", "net.x"}},
		{"high bytes dropped", "k\xc3\xa9.x = 1\n", []string{"k.x"}},
		{"stderr line", "sysctl: cannot stat /proc/sys/foo/bar: No such file or directory\nvm.swappiness = 60\n",
			[]string{"sysctl: cannot stat /proc/sys/foo/bar: No such file or directory", "vm.swappiness"}},
		{"no final newline", "a = 1\nb = 2", []string{"a", "b"}},
		{"cr is not a line end", "a = 1\rb = 2\n", []string{"a"}},
		{"empty", "", nil},
	} {
		if got := parseSysctlE(tc.out); !slices.Equal(got, tc.want) {
			t.Errorf("%s: keys = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestRubyWhitespace(t *testing.T) {
	for in, want := range map[string]string{"a\r\n": "a", "a\n\n": "a\n", "a\r": "a", "a\n\r": "a\n", "a": "a", "": ""} {
		if got := rubyChomp(in); got != want {
			t.Errorf("chomp(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"4096\t87380\v\v6291456": "4096 87380 6291456", "a\u2003b": "a\u2003b", " a ": " a ", "a\x00b": "a\x00b"} {
		if got := rubyCollapseSpace(in); got != want {
			t.Errorf("collapse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveSysctl(t *testing.T) {
	only := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	for _, tc := range []struct {
		name, path string
		exe        func(string) bool
		want       string
		ok         bool
	}{
		{"appended /usr/sbin", "/bin", only("/usr/sbin/sysctl"), "/usr/sbin/sysctl", true},
		{"PATH order", "/bin:/sbin:/usr/sbin", only("/usr/sbin/sysctl", "/sbin/sysctl"), "/sbin/sysctl", true},
		{"/usr/sbin before /sbin", "/bin", only("/usr/sbin/sysctl", "/sbin/sysctl"), "/usr/sbin/sysctl", true},
		{"PATH first", "/opt/x", only("/opt/x/sysctl", "/usr/sbin/sysctl"), "/opt/x/sysctl", true},
		{"empty entry is /", "/bin::/usr/sbin", only("/sysctl", "/usr/sbin/sysctl"), "/sysctl", true},
		{"empty PATH starts with /", "", only("/sysctl", "/usr/sbin/sysctl"), "/sysctl", true},
		{"absent", "/bin", only(), "", false},
		{"~ entry before a hit delegates", "/bin:~/bin:/usr/sbin", only("/usr/sbin/sysctl"), "", false},
		{"bare ~ entry delegates", "~", only("/usr/sbin/sysctl"), "", false},
		{"~ entry after a hit", "/opt/x:~/bin", only("/opt/x/sysctl"), "/opt/x/sysctl", true},
		{"~ inside an entry is literal", "/opt/a~b", only("/opt/a~b/sysctl"), "/opt/a~b/sysctl", true},
	} {
		got, ok := resolveSysctl(tc.path, tc.exe)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: resolveSysctl(%q) = %q, %v; want %q, %v", tc.name, tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

// 01e: no title is in the file; -e names the three live keys.
func TestSysctlPrefetchReplays01e(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	calls := append(raltest.TraceLines(t, "01e", [2]int{4, 17}), lastTraceCall(t, "01e")...)
	run := testAugeasRun(t, calls, t.TempDir(), "/etc/sysctl.conf")
	titles := []string{"net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter", "foo.bar", "foo.qux", "vm.swappiness"}
	runner := newScriptSysctl(t, sysctlCall{args: append([]string{"-e"}, titles...),
		out: "net.ipv4.ip_forward = 0\nnet.ipv4.conf.all.rp_filter = 0\nvm.swappiness = 60\n"})
	ctx, logs := raltest.DebugContext("debug")
	if _, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(ctx, sysctlResources(titles...)); err != nil {
		t.Fatal(err)
	}
	want := map[string]sysctlEntry{
		"net.ipv4.ip_forward":         {Valid: true},
		"net.ipv4.conf.all.rp_filter": {Valid: true},
		"foo.bar":                     {},
		"foo.qux":                     {},
		"vm.swappiness":               {Valid: true},
	}
	if !maps.Equal(run.sysctl, want) {
		t.Errorf("table = %v\nwant %v", run.sysctl, want)
	}
	if got := raltest.LinesAt(*logs, "Puppet"); len(got) == 0 || got[0] != "Prefetching augeas resources for sysctl" {
		t.Errorf("Puppet debug = %q", got)
	}
	run.EndRun()
}

// 01c: every title is in the file, so persist; each found entry's comment is
// looked up.
func TestSysctlPrefetchReplays01c(t *testing.T) {
	calls := append(raltest.TraceLines(t, "01c", [2]int{4, 21}), lastTraceCall(t, "01c")...)
	run := testAugeasRun(t, calls, t.TempDir(), "/etc/sysctl.conf")
	titles := []string{"vm.swappiness", "net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter"}
	runner := newScriptSysctl(t, sysctlCall{args: append([]string{"-e"}, titles...),
		out: "vm.swappiness = 5\nnet.ipv4.ip_forward = 0\nnet.ipv4.conf.all.rp_filter = 1\n"})
	if _, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), sysctlResources(titles...)); err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		if e := run.sysctl[title]; !e.Valid || !e.Persist {
			t.Errorf("%s = %+v, want valid and persist", title, e)
		}
	}
	run.EndRun()
}

// 01j: duplicate entries; the first's value and the comment before it are
// read. A key in the file that sysctl does not know is still valid.
func TestSysctlPrefetchReplays01j(t *testing.T) {
	calls := append(raltest.TraceLines(t, "01j", [2]int{4, 16}), lastTraceCall(t, "01j")...)
	run := testAugeasRun(t, calls, t.TempDir(), "/etc/sysctl.conf")
	runner := newScriptSysctl(t, sysctlCall{args: []string{"-e", "vm.swappiness"}})
	if _, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), sysctlResources("vm.swappiness")); err != nil {
		t.Fatal(err)
	}
	if e := run.sysctl["vm.swappiness"]; e != (sysctlEntry{Valid: true, Persist: true}) {
		t.Errorf("entry = %+v", e)
	}
	run.EndRun()
}

// Every title is looked up in the first resource's target, whatever the
// others set.
func TestSysctlPrefetchReadsEveryTitleInFirstTarget(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_match "/augeas/load/Sysctl" => 0
h1 aug_set "/augeas/load/Sysctl/lens" "Sysctl.lns" => 0
h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.d/50-x.conf" => 0
h1 aug_load => 0
h1 aug_set "/augeas/context" "/files/etc/sysctl.d/50-x.conf" => 0
h1 aug_defnode "target" "/files/etc/sysctl.d/50-x.conf" NULL NULL => 1
h1 aug_defvar "resource" "$target/a.b" => 0
h1 aug_match "$target/a.b" => 0
h1 aug_match "$target/c.d" => 0
h1 aug_close
`)
	run := testAugeasRun(t, calls, t.TempDir())
	rs := []catalog.Resource{
		raltest.AugRes("Sysctl", "a.b", map[string]any{"target": "/etc/sysctl.d/50-x.conf"}),
		raltest.AugRes("Sysctl", "c.d", map[string]any{"target": "/etc/sysctl.conf"}),
	}
	runner := newScriptSysctl(t, sysctlCall{args: []string{"-e", "a.b", "c.d"}, out: "c.d = 1\n"})
	if _, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	want := map[string]sysctlEntry{"a.b": {}, "c.d": {Valid: true}}
	if !maps.Equal(run.sysctl, want) {
		t.Errorf("table = %v, want %v", run.sysctl, want)
	}
	run.EndRun()
}

// The outputs of the slices are one text: a line split across two commands'
// outputs is one line, as Ruby's `+=` makes it.
func TestSysctlPrefetchConcatenatesSliceOutputs(t *testing.T) {
	titles := titlesN(31)
	script := "h1 aug_init NULL NULL 64\n" +
		`h1 aug_match "/augeas/load/Sysctl" => 0
h1 aug_set "/augeas/load/Sysctl/lens" "Sysctl.lns" => 0
h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.conf" => 0
h1 aug_load => 0
h1 aug_set "/augeas/context" "/files/etc/sysctl.conf" => 0
h1 aug_defnode "target" "/files/etc/sysctl.conf" NULL NULL => 1
h1 aug_defvar "resource" "$target/k.n1" => 0
`
	for _, title := range titles {
		script += fmt.Sprintf("h1 aug_match \"$target/%s\" => 0\n", title)
	}
	run := testAugeasRun(t, raltest.ParseTrace(t, "script", script+"h1 aug_close\n"), t.TempDir())
	runner := newScriptSysctl(t,
		sysctlCall{args: append([]string{"-e"}, titles[:30]...), out: "k.n1 = 1\nk.n"},
		sysctlCall{args: append([]string{"-e"}, titles...), out: "2 = 1\n"})
	if _, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), sysctlResources(titles...)); err != nil {
		t.Fatal(err)
	}
	var valid []string
	for title, e := range run.sysctl {
		if e.Valid {
			valid = append(valid, title)
		}
	}
	slices.Sort(valid)
	if !slices.Equal(valid, []string{"k.n1", "k.n2"}) {
		t.Errorf("valid = %v, want [k.n1 k.n2]", valid)
	}
	run.EndRun()
}

// CORE's load error is a prefetch failure, and no sysctl command runs.
func TestSysctlPrefetchLoadErrorFails(t *testing.T) {
	calls := raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_match "/augeas/load/Sysctl" => 0
h1 aug_set "/augeas/load/Sysctl/lens" "Sysctl.lns" => 0
h1 aug_set "/augeas/load/Sysctl/incl[last()+1]" "/etc/sysctl.conf" => 0
h1 aug_load => 0
h1 aug_match "/files/etc/sysctl.conf" => 0
h1 aug_get "/augeas/files/etc/sysctl.conf/error/message" => 1 "Permission denied"
h1 aug_match "/augeas/files/etc/sysctl.conf/error/pos" => 0
h1 aug_close
`)
	run := testAugeasRun(t, calls, t.TempDir(), "/etc/sysctl.conf")
	runner := newScriptSysctl(t)
	_, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), sysctlResources("vm.swappiness"))
	want := "Augeas didn't load /etc/sysctl.conf with Sysctl.lns: Permission denied"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	run.EndRun()
}

// A failing -e is Puppet::ExecutionFailure and a prefetch failure.
func TestSysctlPrefetchSysctlFailureFails(t *testing.T) {
	calls := append(raltest.TraceLines(t, "01e", [2]int{4, 17}), lastTraceCall(t, "01e")...)
	run := testAugeasRun(t, calls, t.TempDir(), "/etc/sysctl.conf")
	titles := []string{"net.ipv4.ip_forward", "net.ipv4.conf.all.rp_filter", "foo.bar", "foo.qux", "vm.swappiness"}
	runner := newScriptSysctl(t, sysctlCall{args: append([]string{"-e"}, titles...), code: 255, out: "sysctl: boom\n"})
	_, err := (Sysctl{run: run, sysctl: "/usr/sbin/sysctl", runner: runner}).Prefetch(context.Background(), sysctlResources(titles...))
	want := "Execution of '/usr/sbin/sysctl -e " + strings.Join(titles, " ") + "' returned 255: sysctl: boom"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	run.EndRun()
}
