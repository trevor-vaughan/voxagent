package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
)

func svcRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Service", Title: title, Parameters: params}
}

func argv(s string) []string { return strings.Fields(s) }

const (
	isActiveA  = "systemctl is-active -- a"
	isEnabledA = "systemctl is-enabled -- a"
	showA      = "systemctl show --property=NeedDaemonReload -- a"
	journalA   = "env LANG=C LC_ALL=C journalctl -n 50 --since 5 minutes ago -u a --no-pager"
)

func TestServiceStartsAndEnablesInOneEvent(t *testing.T) {
	f := &raltest.FakeRunner{
		Replies: map[string]raltest.FakeReply{
			isActiveA: {Exit: 3, Out: "inactive\n"},
			showA:     {Out: "NeedDaemonReload=no\n"},
		},
		Sequences: map[string][]raltest.FakeReply{
			isEnabledA: {{Exit: 1, Out: "disabled\n"}, {Exit: 1, Out: "disabled\n"}},
		},
	}
	changes, _, err := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "running", "enable": "true"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one ensure change (enable is synced inside it)", changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Was != "stopped" || ch.Is != "running" ||
		ch.Name != "service_started" || ch.Message != "ensure changed 'stopped' to 'running'" ||
		!ch.InvalidateRefreshes || ch.Err != nil {
		t.Errorf("change = %+v", ch)
	}
	f.RanExactly(t,
		argv(isActiveA), argv(isEnabledA), argv(showA),
		argv("systemctl unmask -- a"), argv("systemctl start -- a"),
		argv(isEnabledA), // unmask cleared the cache (systemd.rb:74-80)
		argv("systemctl unmask -- a"), argv("systemctl enable -- a"))
}

func TestServiceEnableAloneGetsItsOwnEvent(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isEnabledA: {Exit: 1, Out: "disabled\n"}}}
	changes, _, err := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"enable": true}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	ch := changes[0]
	if ch.Property != "enable" || ch.Was != "false" || ch.Is != "true" ||
		ch.Name != "service_enabled" || ch.Message != "enable changed 'false' to 'true'" ||
		ch.InvalidateRefreshes || ch.Err != nil {
		t.Errorf("change = %+v", ch)
	}
	f.RanExactly(t, argv(isEnabledA), argv("systemctl unmask -- a"), argv("systemctl enable -- a"))
}

func TestServiceDisable(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isEnabledA: {Exit: 0, Out: "enabled\n"}}}
	changes, _, _ := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"enable": "false"}), false)
	if len(changes) != 1 || changes[0].Name != "service_disabled" || changes[0].Was != "true" || changes[0].Is != "false" {
		t.Fatalf("changes = %+v", changes)
	}
	f.RanExactly(t, argv(isEnabledA), argv("systemctl disable -- a"))
}

func TestServiceStop(t *testing.T) {
	f := &raltest.FakeRunner{}
	changes, _, _ := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "stopped"}), false)
	if len(changes) != 1 || changes[0].Name != "service_stopped" || changes[0].Was != "running" ||
		changes[0].InvalidateRefreshes {
		t.Fatalf("changes = %+v", changes)
	}
	f.RanExactly(t, argv(isActiveA), argv("systemctl stop -- a"))
}

// Under noop the ensure change is reported and nothing runs; the enable change
// that would have been synced inside it is not reported at all
// (resource_harness.rb:137-138).
func TestServiceNoopReportsOnlyEnsure(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isActiveA:  {Exit: 3},
		isEnabledA: {Exit: 1, Out: "disabled\n"},
	}}
	changes, _, _ := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "running", "enable": "true"}), true)
	if len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err != nil || !changes[0].InvalidateRefreshes {
		t.Fatalf("changes = %+v", changes)
	}
	f.RanExactly(t, argv(isActiveA), argv(isEnabledA))
}

func TestServiceEnsureAliases(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{{true, "running"}, {"true", "running"}, {false, "stopped"}, {"false", "stopped"}} {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
		if tc.want == "stopped" {
			f.Replies[isActiveA] = raltest.FakeReply{Exit: 0}
		}
		changes, _, _ := Service{run: f}.Apply(context.Background(),
			svcRes("a", map[string]any{"ensure": tc.value}), true)
		if len(changes) != 1 || changes[0].Is != tc.want {
			t.Errorf("ensure => %v: changes = %+v, want Is=%s", tc.value, changes, tc.want)
		}
	}
}

func TestServiceStaticAndIndirectAreInSync(t *testing.T) {
	for _, tc := range []struct {
		out    string
		exit   int
		enable string
	}{{"static", 0, "false"}, {"static", 0, "true"}, {"indirect", 1, "true"}, {"indirect", 1, "false"}} {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isEnabledA: {Exit: tc.exit, Out: tc.out + "\n"}}}
		changes, _, _ := Service{run: f}.Apply(context.Background(),
			svcRes("a", map[string]any{"enable": tc.enable}), false)
		if len(changes) != 0 {
			t.Errorf("%s with enable => %s: changes = %+v, want none", tc.out, tc.enable, changes)
		}
	}
}

// A missing unit: is-enabled exits non-zero with nothing useful, which reads as
// false, so enable => false is in sync (systemd.rb:105-127).
func TestServiceMissingUnitWithEnableFalseIsInSync(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isEnabledA: {Exit: 1, Out: "Failed to get unit file state for a.service: No such file or directory\n"},
	}}
	changes, _, _ := Service{run: f}.Apply(context.Background(), svcRes("a", map[string]any{"enable": "false"}), false)
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none", changes)
	}
}

func TestServiceDaemonReloadRunsWhenNeeded(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isActiveA: {Exit: 3},
		showA:     {Out: "NeedDaemonReload=yes\n"},
	}}
	_, _, _ = Service{run: f}.Apply(context.Background(), svcRes("a", map[string]any{"ensure": "running"}), false)
	f.RanExactly(t, argv(isActiveA), argv(showA), argv("systemctl daemon-reload"),
		argv("systemctl unmask -- a"), argv("systemctl start -- a"))
}

// show's output is read combined with stderr (D-20), so the property is found
// by its line, not by whatever follows the last '=' in the whole output.
func TestServiceDaemonReloadFindsItsLineAmongOtherOutput(t *testing.T) {
	for _, tc := range []struct {
		out    string
		reload bool
	}{
		{"Warning: something=else\nNeedDaemonReload=yes\nnote: done\n", true},
		{"NeedDaemonReload=no\nOther=yes\n", false},
		{"Other=yes\n", false},
	} {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
			isActiveA: {Exit: 3},
			showA:     {Out: tc.out},
		}}
		_, _, _ = Service{run: f}.Apply(context.Background(), svcRes("a", map[string]any{"ensure": "running"}), false)
		ran := false
		for _, c := range f.Calls {
			if strings.Join(c, " ") == "systemctl daemon-reload" {
				ran = true
			}
		}
		if ran != tc.reload {
			t.Errorf("show said %q: daemon-reload ran = %v, want %v", tc.out, ran, tc.reload)
		}
	}
}

// A custom start replaces startcmd, so the implicit unmask does not run; the
// daemon-reload check still does (systemd.rb:212-217). Custom commands are
// shell strings in Puppet (util/execution.rb:360,391).
func TestServiceCustomStartSkipsUnmask(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
	_, _, _ = Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "running", "start": "/opt/start a --now"}), false)
	f.RanExactly(t, argv(isActiveA), argv(showA), []string{"/bin/sh", "-c", "/opt/start a --now"})
}

func TestServiceCustomStatus(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{"/bin/sh -c pgrep a": {Exit: 0}}}
	changes, _, _ := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "running", "status": "pgrep a"}), false)
	if len(changes) != 0 {
		t.Errorf("changes = %+v, want none (custom status says running)", changes)
	}
	f.RanExactly(t, []string{"/bin/sh", "-c", "pgrep a"})
}

func TestServiceNameOverridesTitle(t *testing.T) {
	f := &raltest.FakeRunner{}
	_, _, _ = Service{run: f}.Apply(context.Background(),
		svcRes("web", map[string]any{"name": "httpd.service", "ensure": "stopped"}), false)
	f.RanExactly(t, argv("systemctl is-active -- httpd.service"), argv("systemctl stop -- httpd.service"))
}

// The message is replaced by the journal (systemd.rb:225-231) and is not
// wrapped (property.rb:483-497 re-raises a Puppet::Error as-is). enable is not
// synced after a failed start, because the exception leaves ensure's sync.
func TestServiceStartFailureCarriesTheJournal(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isActiveA:              {Exit: 3},
		isEnabledA:             {Exit: 1, Out: "disabled\n"},
		"systemctl start -- a": {Exit: 1, Out: "Job for a.service failed.\n"},
		journalA:               {Out: "Oct 05 a systemd[1]: Failed to start a.\n"},
	}}
	changes, _, err := Service{run: f}.Apply(context.Background(),
		svcRes("a", map[string]any{"ensure": "running", "enable": "true"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("changes = %+v, want one failed ensure change", changes)
	}
	want := "Systemd start for a failed!\njournalctl log for a:\nOct 05 a systemd[1]: Failed to start a.\n"
	if changes[0].Err.Error() != want {
		t.Errorf("Err = %q, want %q", changes[0].Err.Error(), want)
	}
	for _, c := range f.Calls {
		if strings.Join(c, " ") == "systemctl enable -- a" {
			t.Error("enable ran after a failed start")
		}
	}
	if journal := f.Calls[len(f.Calls)-1]; strings.Join(journal, " ") != journalA || len(journal) != 11 {
		t.Errorf("journalctl argv = %q, want 11 argv elements (no shell)", journal)
	}
}

func TestServiceEnableFailure(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isEnabledA:              {Exit: 1, Out: "disabled\n"},
		"systemctl enable -- a": {Exit: 1, Out: "Failed to enable unit\n"},
	}}
	changes, _, _ := Service{run: f}.Apply(context.Background(), svcRes("a", map[string]any{"enable": "true"}), false)
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != "Could not enable a: " {
		t.Fatalf("changes = %+v, want Err %q (systemd.rb:75-77 drops the output)", changes, "Could not enable a: ")
	}
}

func TestServiceRefresh(t *testing.T) {
	t.Run("running restarts", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 0}}}
		ok, err := Service{run: f}.Refresh(context.Background(), svcRes("a", nil), false)
		if !ok || err != nil {
			t.Fatalf("Refresh = %v, %v", ok, err)
		}
		f.RanExactly(t, argv(isActiveA), argv(showA), argv("systemctl restart -- a"))
	})
	t.Run("stopped skips but still reports a refresh", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
		ok, err := Service{run: f}.Refresh(context.Background(), svcRes("a", nil), false)
		if !ok || err != nil {
			t.Fatalf("Refresh = %v, %v; want true, nil (event_manager.rb:149-156 logs Triggered either way)", ok, err)
		}
		f.RanExactly(t, argv(isActiveA))
	})
	t.Run("noop runs nothing", func(t *testing.T) {
		f := &raltest.FakeRunner{}
		ok, err := Service{run: f}.Refresh(context.Background(), svcRes("a", nil), true)
		if !ok || err != nil || len(f.Calls) != 0 {
			t.Fatalf("Refresh = %v, %v; calls %v", ok, err, f.Calls)
		}
	})
	t.Run("custom restart", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 0}}}
		_, _ = Service{run: f}.Refresh(context.Background(), svcRes("a", map[string]any{"restart": "/opt/reload a"}), false)
		f.RanExactly(t, argv(isActiveA), argv(showA), []string{"/bin/sh", "-c", "/opt/reload a"})
	})
	t.Run("restart failure", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
			isActiveA:                {Exit: 0},
			"systemctl restart -- a": {Exit: 1},
			journalA:                 {Out: "boom\n"},
		}}
		ok, err := Service{run: f}.Refresh(context.Background(), svcRes("a", nil), false)
		if ok || err == nil || err.Error() != "Systemd restart for a failed!\njournalctl log for a:\nboom\n" {
			t.Fatalf("Refresh = %v, %v", ok, err)
		}
	})
}

func TestServiceValidateRequiresSystemdAsInit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := svcRes("a", nil)
	if err := (Service{initComm: write("ok", "systemd\n")}).Validate(r); err != nil {
		t.Errorf("systemd as PID 1: Validate error %v", err)
	}
	err := Service{initComm: write("sleep", "sleep\n")}.Validate(r)
	if err == nil || !strings.HasPrefix(err.Error(), "Service[a]: systemd is not the init system here") ||
		!strings.Contains(err.Error(), `PID 1 is "sleep"`) {
		t.Errorf("sleep as PID 1: Validate error = %v", err)
	}
	err = Service{initComm: filepath.Join(dir, "missing")}.Validate(r)
	if err == nil || !strings.HasPrefix(err.Error(), "Service[a]: cannot tell whether systemd is the init system") {
		t.Errorf("unreadable init comm: Validate error = %v", err)
	}
}

func TestServicePrefetchIsANoOp(t *testing.T) {
	p, err := Service{initComm: "/nonexistent"}.Prefetch(context.Background(), nil)
	if err != nil || p == nil {
		t.Errorf("Prefetch = %v, %v", p, err)
	}
}

// Puppet runs journalctl with failonfail, so its failure replaces the message
// (util/execution.rb:311-312).
func TestServiceJournalctlFailureIsExecutionFailure(t *testing.T) {
	f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		isActiveA:             {Exit: 0},
		"systemctl stop -- a": {Exit: 1},
		journalA:              {Exit: 1, Out: "  no journal\n\n"},
	}}
	changes, _, _ := Service{run: f}.Apply(context.Background(), svcRes("a", map[string]any{"ensure": "stopped"}), false)
	want := "Execution of 'journalctl -n 50 --since '5 minutes ago' -u a --no-pager' returned 1: no journal"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes = %+v, want Err %q", changes, want)
	}
}

func TestServiceCustomCommandShapes(t *testing.T) {
	t.Run("array status is a direct exec", func(t *testing.T) {
		for _, tc := range []struct {
			exit    int
			changes int
		}{{0, 0}, {3, 1}} {
			f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{"/usr/bin/check a": {Exit: tc.exit}}}
			changes, _, _ := Service{run: f}.Apply(context.Background(),
				svcRes("a", map[string]any{"ensure": "running", "status": []any{"/usr/bin/check", "a"}}), true)
			if len(changes) != tc.changes {
				t.Errorf("exit %d: changes = %+v, want %d", tc.exit, changes, tc.changes)
			}
			f.RanExactly(t, []string{"/usr/bin/check", "a"})
		}
	})
	t.Run("array start is a direct exec", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
		_, _, _ = Service{run: f}.Apply(context.Background(),
			svcRes("a", map[string]any{"ensure": "running", "start": []any{"/opt/x", "--now", 5}}), false)
		f.RanExactly(t, argv(isActiveA), argv(showA), []string{"/opt/x", "--now", "5"})
	})
	t.Run("single-element array is a shell string", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
		_, _, _ = Service{run: f}.Apply(context.Background(),
			svcRes("a", map[string]any{"ensure": "running", "start": []any{"/opt/x --now"}}), false)
		f.RanExactly(t, argv(isActiveA), argv(showA), []string{"/bin/sh", "-c", "/opt/x --now"})
	})
	t.Run("number is a shell string", func(t *testing.T) {
		f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: 3}}}
		_, _, _ = Service{run: f}.Apply(context.Background(),
			svcRes("a", map[string]any{"ensure": "running", "start": 42}), false)
		f.RanExactly(t, argv(isActiveA), argv(showA), []string{"/bin/sh", "-c", "42"})
	})
}

func TestServiceSupportsValue(t *testing.T) {
	for _, tc := range []struct {
		attr, value string
		want        bool
	}{
		{"ensure", "running", true}, {"ensure", "stopped", true}, {"ensure", "true", true}, {"ensure", "false", true},
		{"ensure", "paused", false},
		{"enable", "true", true}, {"enable", "false", true},
		{"enable", "mask", false}, {"enable", "manual", false}, {"enable", "delayed", false},
		{"provider", "systemd", true}, {"provider", "redhat", false},
		{"hasstatus", "true", true}, {"hasrestart", "false", true}, {"hasstatus", "maybe", false},
		{"start", "/anything", true},
	} {
		if got := (Service{}).SupportsValue(tc.attr, tc.value); got != tc.want {
			t.Errorf("SupportsValue(%s, %s) = %v, want %v", tc.attr, tc.value, got, tc.want)
		}
	}
}

// type/service.rb:296: a refresh of a service that is not running is skipped,
// and Ruby says so at the resource's path.
func TestServiceRefreshOfAStoppedServiceLogsRubysDebugLine(t *testing.T) {
	src := sources.Sources{Resource: "/Stage[main]/Main/Service[a]", Provider: "Service[a](provider=systemd)", Ref: "Service[a]"}
	for _, tc := range []struct {
		name  string
		exit  int
		noop  bool
		level string
		want  []string
	}{
		{"stopped", 3, false, "debug", []string{"Skipping restart; service is not running"}},
		{"running restarts", 0, false, "debug", nil},
		{"noop never refreshes", 3, true, "debug", nil},
		{"notice drops it", 3, false, "notice", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isActiveA: {Exit: tc.exit}}}
			ctx, got := raltest.DebugContext(tc.level)
			if _, err := (Service{run: f}).Refresh(sources.WithSources(ctx, src), svcRes("a", nil), tc.noop); err != nil {
				t.Fatalf("Refresh returned %v", err)
			}
			if lines := raltest.LinesAt(*got, src.Resource); !slices.Equal(lines, tc.want) {
				t.Errorf("debug lines at %s = %q, want %q", src.Resource, lines, tc.want)
			}
			if tc.level == "notice" && len(*got) != 0 {
				t.Errorf("entries = %+v, want none at notice", *got)
			}
		})
	}
}

// provider/service/systemd.rb:58: enabling or disabling a static unit is not
// possible, so enable is taken as in sync and Ruby says so from Puppet. An
// indirect unit has its own message.
func TestServiceStaticEnableLogsRubysDebugLine(t *testing.T) {
	src := sources.Sources{Resource: "/Stage[main]/Main/Service[a]", Provider: "Service[a](provider=systemd)", Ref: "Service[a]"}
	for _, tc := range []struct {
		name   string
		out    string
		exit   int
		params map[string]any
		level  string
		want   []string
	}{
		{"static, enable true", "static", 0, map[string]any{"enable": "true"}, "debug",
			[]string{"Unable to enable or disable static service a"}},
		{"static, enable false", "static", 0, map[string]any{"enable": "false"}, "debug",
			[]string{"Unable to enable or disable static service a"}},
		{"enabled is compared as usual", "enabled", 0, map[string]any{"enable": "true"}, "debug", nil},
		{"indirect is not a listed row", "indirect", 1, map[string]any{"enable": "true"}, "debug", nil},
		{"notice drops it", "static", 0, map[string]any{"enable": "true"}, "notice", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{isEnabledA: {Exit: tc.exit, Out: tc.out + "\n"}}}
			ctx, got := raltest.DebugContext(tc.level)
			if _, _, err := (Service{run: f}).Apply(sources.WithSources(ctx, src), svcRes("a", tc.params), false); err != nil {
				t.Fatalf("Apply returned %v", err)
			}
			if lines := raltest.LinesAt(*got, "Puppet"); !slices.Equal(lines, tc.want) {
				t.Errorf("debug lines at Puppet = %q, want %q", lines, tc.want)
			}
			if tc.level == "notice" && len(*got) != 0 {
				t.Errorf("entries = %+v, want none at notice", *got)
			}
		})
	}
}

// Service's commands go through the file runner by default, so a custom start
// that backgrounds a daemon cannot hang the run.
func TestServiceDefaultRunnerUsesAFile(t *testing.T) {
	if _, ok := (Service{}).runner().(command.FileOutputRunner); !ok {
		t.Errorf("default runner is %T, want command.FileOutputRunner", Service{}.runner())
	}
}

// customRunner runs the user's custom commands for real, so the context a
// service passes them reaches the logging runner, and answers systemctl and
// journalctl from f.
type customRunner struct{ f *raltest.FakeRunner }

func (c customRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	if name == "systemctl" || name == "env" {
		return c.f.Run(ctx, name, args...)
	}
	return command.FileOutputRunner{}.Run(ctx, name, args...)
}

// D-78: a listed custom start, stop, status or restart is run and logged as
// '[redacted]', where Ruby prints the command (util/execution.rb:181). The
// unmarked line is Ruby's.
func TestServiceCustomCommandsRedactedWhenMarked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		marked []string
		want   []string // the Executing: lines
	}{
		{"status marked", map[string]any{"ensure": "running", "status": []any{"/bin/true", "status-secret"}}, []string{"status"},
			[]string{"Executing: '[redacted]'"}},
		{"status unmarked", map[string]any{"ensure": "running", "status": []any{"/bin/true", "status-secret"}}, nil,
			[]string{"Executing: '/bin/true status-secret'"}},
		{"start marked", map[string]any{"ensure": "running", "status": []any{"/bin/false", "a"}, "start": []any{"/bin/true", "start-secret"}}, []string{"start"},
			[]string{"Executing: '/bin/false a'", "Executing: '[redacted]'"}},
		{"stop marked", map[string]any{"ensure": "stopped", "status": []any{"/bin/true", "a"}, "stop": []any{"/bin/true", "stop-secret"}}, []string{"stop"},
			[]string{"Executing: '/bin/true a'", "Executing: '[redacted]'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, entries := raltest.DebugContext("debug")
			f := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{showA: {Out: "NeedDaemonReload=no\n"}}}
			r := svcRes("a", tc.params)
			r.SensitiveParameters = tc.marked
			if _, _, err := (Service{run: customRunner{f}}).Apply(ctx, r, false); err != nil {
				t.Fatal(err)
			}
			if got := raltest.LinesAt(*entries, "Puppet"); !slices.Equal(got, tc.want) {
				t.Errorf("Executing lines = %q, want %q", got, tc.want)
			}
		})
	}
}

// D-78: a custom command that cannot start fails without naming the command
// or its path when the command is listed.
func TestServiceCustomCommandStartFailureRedactedWhenMarked(t *testing.T) {
	r := svcRes("a", map[string]any{"ensure": "running", "status": []any{"/nonexistent/status-secret", "arg"}})
	r.SensitiveParameters = []string{"status"}
	_, _, err := (Service{run: customRunner{&raltest.FakeRunner{}}}).Apply(context.Background(), r, false)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v, want one that names no part of the command", err)
	}
}
