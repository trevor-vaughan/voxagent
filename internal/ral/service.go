package ral

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Service is Puppet's service type with its systemd provider, and only that
// provider -- the same bounded-surface discipline as package's dnf.
//
// Every built-in command is argv with "--" before the unit name, never a
// shell, so a unit name cannot reach one. The user's own start, stop, status
// and restart, when a string, single-element array or scalar, ARE shell
// strings in Puppet (util/execution.rb:360,391) and run as /bin/sh -c here:
// that is the catalog author's shell, as exec's command is. A multi-element
// array is a direct argv exec in both (see custom).
//
// Every command's output is captured through a temporary file, not a pipe
// (fileOutputRunner), so a custom start that backgrounds a daemon returns when
// the start command exits instead of when the daemon does.
//
// Locale: journalctl runs with LANG=C LC_ALL=C because its text is copied
// verbatim into the failure message an operator reads, and Puppet runs it with
// override_locale (util/execution.rb:372-379). Start, stop, restart and status
// keep the caller's locale, as Puppet's service_execute does
// (provider/service/service.rb:49-50). The state words parsed from
// systemctl are not localised.
type Service struct {
	run      command.Runner
	initComm string // read to confirm systemd is PID 1; "" means /proc/1/comm
}

var systemdProvider = "systemd"

// Type implements Provider.
func (Service) Type() string { return "service" }

// ProviderUsed implements Provider.
func (Service) ProviderUsed() *string { return &systemdProvider }

// AutoRequires implements Provider. Puppet's service type declares none.
func (Service) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs implements Provider.
func (Service) Attrs() Attrs {
	return NewAttrs("ensure", "enable", "hasstatus", "hasrestart",
		"start", "stop", "status", "restart", "name", "provider")
}

// Properties implements PropertyDeclaring. type/service.rb has two properties,
// ensure and enable; the rest of Attrs are parameters.
func (Service) Properties() Attrs { return NewAttrs("ensure", "enable") }

// SupportsValue implements ValueBounded.
//
// enable => mask is refused as out of scope; manual and delayed fail Puppet's
// own feature check for systemd (property.rb:589-597). hasstatus and
// hasrestart are accepted and have no effect: the systemd provider always has
// a status and a restart command (systemd.rb:188,201).
func (Service) SupportsValue(attr, value string) bool {
	switch attr {
	case "ensure":
		switch value {
		case "running", "stopped", "true", "false":
			return true
		}
		return false
	case "enable", "hasstatus", "hasrestart":
		return value == "true" || value == "false"
	case "provider":
		return value == "systemd"
	}
	return true
}

func (s Service) runner() command.Runner {
	if s.run == nil {
		return command.FileOutputRunner{}
	}
	return s.run
}

// serviceName is the namevar: name if given, else the title. Puppet never
// appends .service (systemd.rb:10-12); systemctl does.
func serviceName(r catalog.Resource) string { return rubyvalue.ParamString(r, "name", r.Title) }

// wantedEnsure normalises ensure, whose aliases are true => running and
// false => stopped (type/service.rb:111-112). Unset means unmanaged.
func wantedEnsure(r catalog.Resource) (string, bool) {
	if _, ok := r.Parameters["ensure"]; !ok {
		return "", false
	}
	switch v := rubyvalue.ParamString(r, "ensure", ""); v {
	case "true":
		return "running", true
	case "false":
		return "stopped", true
	default:
		return v, true
	}
}

// custom returns the argv of the user's command for an operation, if one was
// given. Puppet builds cmd=[c] and execute flattens it (provider/service/service.rb:59-61,
// util/execution.rb:157-159): a string, a single-element array or a scalar is
// one string, which Ruby runs through a shell; an array of several elements is
// a direct argv exec with no shell.
func custom(r catalog.Resource, op string) ([]string, bool) {
	v, ok := r.Parameters[op]
	if !ok || v == nil {
		return nil, false
	}
	if list, isList := v.([]any); isList && len(list) > 1 {
		argv := make([]string, len(list))
		for i, e := range list {
			argv[i] = fmt.Sprint(e)
		}
		return argv, true
	} else if isList && len(list) == 1 {
		v = list[0]
	}
	return []string{"/bin/sh", "-c", fmt.Sprint(v)}, true
}

// status is base.rb's status: the custom status command or is-active, exit 0
// meaning running and anything else stopped -- never an error for a non-zero
// exit. A command that cannot start at all is an error.
func (s Service) status(ctx context.Context, r catalog.Resource, name string) (string, error) {
	var code int
	var err error
	if c, ok := custom(r, "status"); ok {
		code, _, err = s.runner().Run(ctx, c[0], c[1:]...)
	} else {
		code, _, err = s.runner().Run(ctx, "systemctl", "is-active", "--", name)
	}
	if err != nil {
		return "", err
	}
	if code == 0 {
		return "running", nil
	}
	return "stopped", nil
}

// enabledState is what is-enabled said: its output decides the in-sync
// special cases, its exit code the value.
type enabledState struct {
	out  string
	code int
}

func (s Service) isEnabled(ctx context.Context, name string) (enabledState, error) {
	code, out, err := s.runner().Run(ctx, "systemctl", "is-enabled", "--", name)
	if err != nil {
		return enabledState{}, err
	}
	return enabledState{out: strings.TrimSpace(out), code: code}, nil
}

// value is systemd.rb:105-127 without the mask and Debian branches: indirect
// is false, otherwise exit 0 is true.
func (e enabledState) value() string {
	if e.out != "indirect" && e.code == 0 {
		return "true"
	}
	return "false"
}

// inSync is enabled_insync? (systemd.rb:51-67): static and indirect units
// cannot be enabled or disabled, so either is always in sync for true/false.
func (e enabledState) inSync(want string) bool {
	if e.out == "static" || e.out == "indirect" {
		return true
	}
	return e.value() == want
}

// inSyncLogged is inSync, and says why a static unit is in sync as Puppet's
// enabled_insync? does (systemd.rb:58), from Puppet because the call is on the
// class. enable => mask, which would compare, is out of scope (SupportsValue).
func (e enabledState) inSyncLogged(ctx context.Context, name, want string) bool {
	if e.out == "static" {
		runlog.Debugf(ctx, "Puppet", "Unable to enable or disable static service %s", name)
	}
	return e.inSync(want)
}

// Apply implements Provider.
//
// ensure is synced first. When it changes, an out-of-sync enable is synced
// inside that change and gets no event of its own (type/service.rb:120-136,
// resource_harness.rb:84-99); under noop it is not reported at all. When
// ensure is in sync or unmanaged, enable is its own change.
func (s Service) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	name := serviceName(r)
	wantEnsure, manageEnsure := wantedEnsure(r)
	_, manageEnable := r.Parameters["enable"]
	wantEnable := rubyvalue.ParamString(r, "enable", "")

	var current string
	if manageEnsure {
		var err error
		if current, err = s.status(ctx, r, name); err != nil {
			return nil, nil, err
		}
	}
	var enabled enabledState
	if manageEnable {
		var err error
		if enabled, err = s.isEnabled(ctx, name); err != nil {
			return nil, nil, err
		}
	}

	if manageEnsure && current != wantEnsure {
		ch := Change{
			Property: "ensure", Was: current, Is: wantEnsure,
			Message: fmt.Sprintf("ensure changed '%s' to '%s'", current, wantEnsure),
		}
		if wantEnsure == "running" {
			ch.Name = "service_started"
			ch.InvalidateRefreshes = true
		} else {
			ch.Name = "service_stopped"
		}
		if noop {
			return []Change{ch}, nil, nil
		}
		var err error
		if wantEnsure == "running" {
			err = s.start(ctx, r, name)
			if err == nil && manageEnable {
				if _, customStart := custom(r, "start"); !customStart {
					// The built-in start ran unmask, which empties Puppet's
					// is-enabled cache (systemd.rb:74-80), so ensure's sync
					// reads the enable state afresh.
					enabled, err = s.isEnabled(ctx, name)
				}
			}
		} else {
			err = s.stop(ctx, r, name)
		}
		if err == nil && manageEnable && !enabled.inSyncLogged(ctx, name, wantEnable) {
			err = s.setEnable(ctx, name, wantEnable)
		}
		ch.Err = err
		return []Change{ch}, nil, nil
	}

	if manageEnable && !enabled.inSyncLogged(ctx, name, wantEnable) {
		ch := Change{
			Property: "enable", Was: enabled.value(), Is: wantEnable,
			Message: fmt.Sprintf("enable changed '%s' to '%s'", enabled.value(), wantEnable),
			Name:    "service_disabled",
		}
		if wantEnable == "true" {
			ch.Name = "service_enabled"
		}
		if !noop {
			ch.Err = s.setEnable(ctx, name, wantEnable)
		}
		return []Change{ch}, nil, nil
	}
	return nil, nil, nil
}

// daemonReload is daemon_reload? (systemd.rb:160-167): reload systemd when the
// unit says it needs it. Both commands' failures are ignored, as Puppet's
// failonfail => false ignores them.
//
// Puppet takes whatever follows the last '=' of show's stdout. voxagent reads
// stdout and stderr combined (D-20), so it finds the NeedDaemonReload= line
// instead; on show's clean one-line output the two agree.
func (s Service) daemonReload(ctx context.Context, name string) {
	_, out, err := s.runner().Run(ctx, "systemctl", "show", "--property=NeedDaemonReload", "--", name)
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "NeedDaemonReload="); ok && v == "yes" {
			_, _, _ = s.runner().Run(ctx, "systemctl", "daemon-reload")
			return
		}
	}
}

// changeEnable is systemctl_change_enable (systemd.rb:74-80). Its message
// interpolates a variable that is always nil inside the rescue, so the
// command's output never reaches it; the trailing space is Puppet's.
func (s Service) changeEnable(ctx context.Context, action, name string) error {
	code, _, err := s.runner().Run(ctx, "systemctl", action, "--", name)
	if err != nil || code != 0 {
		return fmt.Errorf("Could not %s %s: ", action, name)
	}
	return nil
}

func (s Service) setEnable(ctx context.Context, name, want string) error {
	if want == "true" {
		if err := s.changeEnable(ctx, "unmask", name); err != nil {
			return err
		}
		return s.changeEnable(ctx, "enable", name)
	}
	return s.changeEnable(ctx, "disable", name)
}

// operate runs the user's command for op (shell or direct argv, as custom
// decides) or the built-in systemctl op, and on failure returns
// prepare_error_message's text (systemd.rb:225-231), which replaces whatever
// the command said.
func (s Service) operate(ctx context.Context, r catalog.Resource, name, op string) error {
	var code int
	var err error
	if c, ok := custom(r, op); ok {
		code, _, err = s.runner().Run(ctx, c[0], c[1:]...)
	} else {
		code, _, err = s.runner().Run(ctx, "systemctl", op, "--", name)
	}
	if err != nil || code != 0 {
		return s.failure(ctx, name, op)
	}
	return nil
}

// failure builds Puppet's failure text. Puppet runs the journalctl line
// through a shell with the unit name unescaped (systemd.rb:227); voxagent runs
// it as argv, so the text is the same for any name without shell
// metacharacters and a name with them cannot reach a shell. Recorded in the
// divergence register.
//
// journalctl runs under env with LANG=C LC_ALL=C: Puppet's override_locale
// deletes the other LC_* variables and LANGUAGE and sets both of these
// (util/execution.rb:372-379), and LC_ALL=C overrides every remaining one.
// Puppet runs it with failonfail, so a journalctl that fails replaces the
// whole message with ExecutionFailure's text (util/execution.rb:311-312).
func (s Service) failure(ctx context.Context, name, op string) error {
	code, out, err := s.runner().Run(ctx, "env", "LANG=C", "LC_ALL=C", "journalctl",
		"-n", "50", "--since", "5 minutes ago", "-u", name, "--no-pager")
	line := fmt.Sprintf("journalctl -n 50 --since '5 minutes ago' -u %s --no-pager", name)
	if err != nil {
		return fmt.Errorf("Execution of '%s' failed: %w", line, err)
	}
	if code != 0 {
		return fmt.Errorf("Execution of '%s' returned %d: %s", line, code, strings.TrimSpace(out))
	}
	return fmt.Errorf("Systemd %s for %s failed!\njournalctl log for %s:\n%s", op, name, name, out)
}

// start is systemd.rb's start: daemon-reload check, then the built-in startcmd
// runs unmask first (systemd.rb:192-195); a custom start replaces startcmd and
// so skips it. An unmask failure surfaces as the start failure, as Puppet's
// rescue in start turns it into one.
func (s Service) start(ctx context.Context, r catalog.Resource, name string) error {
	s.daemonReload(ctx, name)
	if _, ok := custom(r, "start"); !ok {
		if err := s.changeEnable(ctx, "unmask", name); err != nil {
			return s.failure(ctx, name, "start")
		}
	}
	return s.operate(ctx, r, name, "start")
}

func (s Service) stop(ctx context.Context, r catalog.Resource, name string) error {
	return s.operate(ctx, r, name, "stop")
}

// Refresh implements Provider. Puppet restarts only a running service
// (type/service.rb:291-298); a stopped one is skipped, and the refresh is still
// reported, because process_callback logs "Triggered 'refresh'" whenever the
// callback does not raise (event_manager.rb:149-156). Under noop Puppet never
// calls refresh at all, so nothing runs here.
func (s Service) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	if noop {
		return true, nil
	}
	name := serviceName(r)
	current, err := s.status(ctx, r, name)
	if err != nil {
		return false, err
	}
	if current != "running" {
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Skipping restart; service is not running")
		return true, nil
	}
	s.daemonReload(ctx, name)
	if err := s.operate(ctx, r, name, "restart"); err != nil {
		return false, err
	}
	return true, nil
}

// Validate implements Validating. Puppet's provider is suitable only when
// /proc/1/comm names systemd (systemd.rb:16) and would otherwise select a
// different provider; driving systemctl on such a host would apply the resource
// with code Puppet would not run. Refusing here, rather than in Prefetch, stops
// the whole catalog before anything is applied.
func (s Service) Validate(r catalog.Resource) error {
	path := s.initComm
	if path == "" {
		path = "/proc/1/comm"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: cannot tell whether systemd is the init system: %w", r.Ref(), err)
	}
	comm := strings.TrimSpace(string(b))
	if !strings.Contains(comm, "systemd") {
		return fmt.Errorf("%s: systemd is not the init system here (PID 1 is %q); "+
			"Puppet would not select the systemd provider either", r.Ref(), comm)
	}
	return nil
}

// Prefetch implements Provider. The systemd provider has no prefetch.
func (s Service) Prefetch(context.Context, []catalog.Resource) (Provider, error) {
	return s, nil
}
