package mountcore

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/transaction"
)

var (
	_ ral.Provider       = Mount{}
	_ ral.SelfRefreshing = Mount{}
	_ ral.RefreshLogging = Mount{}
	_ ral.AutoRelating   = Mount{}
)

// remountX is the refresh's remount of /mnt/x with remounts true.
const remountX = "Executing: '/usr/bin/mount -o remount /mnt/x'"

// refreshAt is ctx carrying the sources the walk gives r's refresh, and a
// debug logger whose entries it returns.
func refreshAt(r catalog.Resource) (context.Context, *[]report.LogEntry) {
	ctx, logs := raltest.DebugContext("debug")
	ref := r.Ref()
	return sources.WithSources(ctx, sources.Sources{Resource: "/Stage[main]/Main/" + ref, Provider: ref + "(provider=parsed)", Ref: ref}), logs
}

// TestMountRefresh is mt:287-290 and mm:26-41 on Linux: when the should fstype
// is not swap and the record is mounted (mounted or ghost) at refresh, info
// Remounting at the provider's to_s, then `mount -o remount <name>` with
// remounts true, else umount then mount; otherwise nothing is logged or run.
// Either way the refresh happened. Under noop nothing runs (Ruby never calls
// refresh then).
func TestMountRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, current string
		params        map[string]any
		noop          bool
		remounting    bool
		trace         []string
		ensureAfter   string
	}{
		{"mounted", "mounted", nil, false, true, []string{remountX}, "mounted"},
		{"ghost", "ghost", nil, false, true, []string{remountX}, "ghost"},
		{"remounts-false", "mounted", map[string]any{"remounts": false}, false, true, []string{umountX, mountX}, "mounted"},
		{"remounts-false-string/ghost", "ghost", map[string]any{"remounts": "false"}, false, true, []string{umountX, mountX}, "ghost"},
		{"swap", "mounted", map[string]any{"fstype": "swap"}, false, false, nil, "mounted"},
		{"unmounted", "unmounted", nil, false, false, nil, "unmounted"},
		{"absent", "absent", nil, false, false, nil, "absent"},
		{"noop", "mounted", nil, true, false, nil, "mounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed, listed := seedState(tc.current)
			run := &fakeMount{listing: listed}
			mh := newMountHarness(run)
			r := mountAt("/mnt/x", seedFstab(t, seed), tc.params)
			primed, err := mh.m.prefetch(context.Background(), []catalog.Resource{r})
			if err != nil {
				t.Fatal(err)
			}
			ctx, logs := refreshAt(r)
			out, err := primed.RefreshLogged(ctx, r, tc.noop)
			if err != nil || out.Refreshed != !tc.noop || len(out.Logs) != 0 || len(out.ExceptionLogs) != 0 || len(out.PropertyNotices) != 0 {
				t.Fatalf("RefreshLogged = %+v, %v", out, err)
			}
			if got := commandTrace(*logs); !reflect.DeepEqual(got, tc.trace) {
				t.Errorf("commands = %q, want %q", got, tc.trace)
			}
			var infos []string
			for _, e := range *logs {
				if e.Level != "debug" {
					infos = append(infos, e.Level+" "+e.Source+": "+e.Message)
				}
			}
			var want []string
			if tc.remounting {
				want = []string{"info Mount[/mnt/x](provider=parsed): Remounting"}
			}
			if !reflect.DeepEqual(infos, want) {
				t.Errorf("logs = %q, want %q", infos, want)
			}
			if got := primed.state.Records["/mnt/x"].Ensure; got != tc.ensureAfter && !(tc.ensureAfter == "absent" && got == "") {
				t.Errorf("record ensure = %q, want %q", got, tc.ensureAfter)
			}
			refreshed, err := primed.Refresh(ctx, r, tc.noop)
			if err != nil || refreshed != !tc.noop {
				t.Errorf("Refresh = %v, %v", refreshed, err)
			}
			if !primed.SelfRefreshes(r) {
				t.Error("SelfRefreshes = false")
			}
		})
	}
}

// remountDenied is util-linux's text for a refused remount (17f-*).
const remountDenied = "mount: /mnt/x: permission denied.\n       dmesg(1) may have more information after failed mount system call."

// TestMountRefreshFailure is a failing remount: Remounting is logged before
// the command, and the error is the command's ExecutionFailure.
func TestMountRefreshFailure(t *testing.T) {
	seed, listed := seedState("mounted")
	run := &fakeMount{listing: listed, fail: map[string]cmdResult{mountBin + " -o remount /mnt/x": {32, remountDenied + "\n"}}}
	r := mountAt("/mnt/x", seedFstab(t, seed), nil)
	primed, err := newMountHarness(run).m.prefetch(context.Background(), []catalog.Resource{r})
	if err != nil {
		t.Fatal(err)
	}
	ctx, logs := refreshAt(r)
	out, err := primed.RefreshLogged(ctx, r, false)
	want := "Execution of '/usr/bin/mount -o remount /mnt/x' returned 32: " + remountDenied
	if raltest.ErrText(err) != want || out.Refreshed {
		t.Fatalf("RefreshLogged = %+v, %q; want %q", out, raltest.ErrText(err), want)
	}
	if got := allLines(*logs); !reflect.DeepEqual(got[len(got)-2:], []string{
		"info Mount[/mnt/x](provider=parsed): Remounting", "debug Puppet: " + remountX}) {
		t.Errorf("logs = %q", got)
	}
}

// allLines is every log entry as "<level> <source>: <message>".
func allLines(entries []report.LogEntry) []string {
	var out []string
	for _, l := range entries {
		out = append(out, l.Level+" "+l.Source+": "+l.Message)
	}
	return out
}

// subsequence reports whether want appears in got in order.
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestMountRefreshThroughWalk is the self-refresh in the transaction (M11,
// 17f-*): absent -> mounted mounts, then remounts in the same step, change
// count 2 (the ensure change and the refresh); a failing remount after an
// options change logs Remounting, the command, then the walk's two errs, and
// the resource fails to restart with the options event kept.
func TestMountRefreshThroughWalk(t *testing.T) {
	t.Run("absent-to-mounted", func(t *testing.T) {
		run := &fakeMount{}
		r := mountAt("/mnt/x", seedFstab(t, keepLine), map[string]any{"ensure": "mounted", "device": "tmpfs", "fstype": "tmpfs", "options": "defaults"})
		rep := runMounts(t, newMountHarness(run).m, r)
		st, ok := rep.ResourceStatuses["Mount[/mnt/x]"]
		if !ok || st.Failed || !st.Restarted || st.ChangeCount != 2 {
			t.Fatalf("status = %+v", st)
		}
		if got := commandTrace(rep.Logs); len(got) != 4 || got[0] != listing || !strings.HasPrefix(got[1], flushing) || got[2] != mountX || got[3] != remountX {
			t.Errorf("commands = %q", got)
		}
		if !subsequence(allLines(rep.Logs), []string{
			"debug Puppet: " + mountX,
			"info Mount[/mnt/x](provider=parsed): Remounting",
			"debug Puppet: " + remountX,
			"notice /Mount[/mnt/x]: Triggered 'refresh' from 1 event",
		}) {
			t.Errorf("logs = %q", allLines(rep.Logs))
		}
	})
	t.Run("failed-remount", func(t *testing.T) {
		seed, listed := seedState("mounted")
		run := &fakeMount{listing: listed, fail: map[string]cmdResult{mountBin + " -o remount /mnt/x": {32, remountDenied + "\n"}}}
		r := mountAt("/mnt/x", seedFstab(t, seed), map[string]any{"options": "noatime"})
		rep := runMounts(t, newMountHarness(run).m, r)
		text := "Execution of '/usr/bin/mount -o remount /mnt/x' returned 32: " + remountDenied
		if !subsequence(allLines(rep.Logs), []string{
			"notice /Mount[/mnt/x]/options: options changed 'defaults' to 'noatime'",
			"info Mount[/mnt/x](provider=parsed): Remounting",
			"debug Puppet: " + remountX,
			"err /Mount[/mnt/x]: Failed to call refresh: " + text,
			"err /Mount[/mnt/x]: " + text,
		}) {
			t.Errorf("logs = %q", allLines(rep.Logs))
		}
		st, ok := rep.ResourceStatuses["Mount[/mnt/x]"]
		if !ok || !st.Failed || !st.FailedToRestart || st.ChangeCount != 1 || len(st.Events) != 2 {
			t.Fatalf("status = %+v", st)
		}
		if ev := st.Events[0]; ev.Status != "success" || ev.Name == nil || *ev.Name != "options_changed" {
			t.Errorf("event 0 = %+v", ev)
		}
		if ev := st.Events[1]; ev.Status != "failure" || ev.Name == nil || *ev.Name != "resource_error" || ev.Property != nil ||
			ev.Message != "Failed to call refresh: "+text {
			t.Errorf("event 1 = %+v", ev)
		}
	})
}

// TestMountAutoRequires is mt:299-305: Mount[<p>] for each proper ancestor of
// the munged name by Pathname#ascend, root first, when managed. The ancestor
// rows are Ruby 3.4's Pathname#ascend on the same strings.
func TestMountAutoRequires(t *testing.T) {
	all := func(string) bool { return true }
	for _, tc := range []struct {
		title string
		want  []string
	}{
		{"/a/b/c", []string{"/", "/a", "/a/b"}},
		{"/", nil},
		{"/mnt/t/", []string{"/", "/mnt"}},
		{"a/b", []string{"a"}},
		{"a", nil},
		{"/a//b", []string{"/", "/a"}},
		{"//a", []string{"/"}},
		{"/a/./b", []string{"/", "/a", "/a/."}},
		{"/a/../b", []string{"/", "/a", "/a/.."}},
		{"./a", []string{"."}},
		{"../x/y", []string{"..", "../x"}},
	} {
		var want []string
		for _, p := range tc.want {
			want = append(want, "Mount["+p+"]")
		}
		if got := (Mount{}).AutoRequires(mountAt(tc.title, "/etc/fstab", nil), all); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: AutoRequires = %q, want %q", tc.title, got, want)
		}
	}
	managed := func(ref string) bool { return ref == "Mount[/]" || ref == "Mount[/a/b]" }
	if got, want := (Mount{}).AutoRequires(mountAt("/a/b/c", "/etc/fstab", nil), managed), []string{"Mount[/]", "Mount[/a/b]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("managed only: %q, want %q", got, want)
	}
	named := mountAt("t", "/etc/fstab", map[string]any{"name": "/srv/d/"})
	if got, want := (Mount{}).AutoRequires(named, all), []string{"Mount[/]", "Mount[/srv]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("by name: %q, want %q", got, want)
	}
}

// TestMountAutoBefore is mt:308-316: File[<path>] for every catalog File whose
// expanded path matches (?m)^<name>/., in catalog order (M11).
func TestMountAutoBefore(t *testing.T) {
	for _, tc := range []struct {
		title string
		files []string
		want  []string
	}{
		{"/mnt/x", []string{"/mnt/x", "/mnt/x/f", "/mnt/xz/h", "/mnt/x/y/g"}, []string{"/mnt/x/f", "/mnt/x/y/g"}},
		{"/mnt", []string{"/mnt/x", "/mnt"}, []string{"/mnt/x"}},
		{"/mnt/a.b", []string{"/mnt/aXb/f", "/mnt/a.b/f"}, []string{"/mnt/aXb/f", "/mnt/a.b/f"}},
		{"/mnt/c+d", []string{"/mnt/ccd/f", "/mnt/c+d/f"}, []string{"/mnt/ccd/f"}},
		{"/", []string{"/x", "/"}, nil},
		{"/mnt/t/", []string{"/mnt/t/f", "/mnt/t"}, []string{"/mnt/t/f"}},
		{"/mnt/x", []string{"/mnt/x/../x/f", "/mnt/x/./../y"}, []string{"/mnt/x/../x/f"}},
	} {
		var want []ral.AutoRelation
		for _, p := range tc.want {
			want = append(want, ral.AutoRelation{Kind: ral.RelBefore, Ref: "File[" + p + "]"})
		}
		m := Mount{files: tc.files}
		if got := m.AutoRelations(mountAt(tc.title, "/etc/fstab", nil)); !reflect.DeepEqual(got, want) {
			t.Errorf("%q over %q: AutoRelations = %+v, want %+v", tc.title, tc.files, got, want)
		}
	}
}

// TestMountAutoBeforeSkipsPatternWithoutFiles is spec 2.1 condition 7: Ruby
// builds the pattern only inside its loop over Files, so a name RE2 cannot
// compile is irrelevant when the catalog holds none.
func TestMountAutoBeforeSkipsPatternWithoutFiles(t *testing.T) {
	if got := (Mount{}).AutoRelations(mountAt("/mnt/a(b", "/etc/fstab", nil)); got != nil {
		t.Errorf("AutoRelations = %+v, want nil", got)
	}
}

// TestMountRelationsInGraph is M11's relationship debug lines: autorequires
// root first, only declared parents, then the autobefore, at the resource.
func TestMountRelationsInGraph(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		mountAt("/", "/etc/fstab", nil),
		mountAt("/mnt/x/y", "/etc/fstab", nil),
		mountAt("/mnt", "/etc/fstab", nil),
		raltest.FileRes("/mnt/x/y/f", nil),
	}}
	ctx, logs := raltest.DebugContext("debug")
	if _, err := transaction.BuildGraph(ctx, cat, ral.NewRegistry(Mount{files: []string{"/mnt/x/y/f"}})); err != nil {
		t.Fatal(err)
	}
	if got, want := raltest.LinesAt(*logs, "/Mount[/mnt/x/y]"), []string{
		"Adding autorequire relationship with Mount[/]",
		"Adding autorequire relationship with Mount[/mnt]",
		"Adding autobefore relationship with File[/mnt/x/y/f]",
	}; !subsequence(got, want) || len(raltest.LinesAt(*logs, "/Mount[/]")) != 0 {
		t.Errorf("lines = %q / Mount[/] %q", got, raltest.LinesAt(*logs, "/Mount[/]"))
	}
}

// TestMountEnsureErrorRedactsMarked is the ensure value redacted in a wrapped
// set failure when ral.IsMarked marks ensure, a generator's flag included.
func TestMountEnsureErrorRedactsMarked(t *testing.T) {
	const leadingWS = "  tmpfs\t/mnt/w\ttmpfs\tdefaults\t0\t0\n"
	r := mountAt("/mnt/x", seedFstab(t, leadingWS), map[string]any{"ensure": "unmounted", "device": "tmpfs", "fstype": "tmpfs"})
	r.File, r.Line = "/case/site.pp", 1
	r.SetSensitivity = map[string]bool{"ensure": true}
	_, out := newMountHarness(&fakeMount{listing: listedLine}).apply(t, context.Background(), false, r)
	want := "Could not set [redacted] on ensure: Field 'device' is required (file: /case/site.pp, line: 1)"
	if len(out[0].changes) != 1 || raltest.ErrText(out[0].changes[0].Err) != want {
		t.Errorf("outcome = %+v, want %q", out[0], want)
	}
}
