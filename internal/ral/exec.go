package ral

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Exec is the Tier 1 exec provider, bounded to command, path, cwd, returns,
// refreshonly, creates, onlyif, and unless.
//
// It is in the spine on engine grounds rather than provider grounds: only a
// refreshable type can exercise the refresh path at all, and that path is where
// failed_to_restart, restarted, and refresh propagation are decided. (An
// earlier comment here called failed_to_restart the "otherwise unreachable"
// second clause of Puppet's 0x4. Measured, it is not: event_manager.rb sets
// failed alongside it, so the two clauses always fire together.)
type Exec struct{}

// Type implements Provider.
func (Exec) Type() string { return "exec" }

// Measured: Puppet's exec resolves to the posix provider on Linux.
func (Exec) ProviderUsed() *string { return &posix }

// execPath matches an absolute path at the start of a line, and execQuoted a
// quoted one. Both are Puppet's own, from type/exec.rb autorequire(:file):
// /^(\/\S+)/ and /^"([^"]+)"/. The (?m) mirrors Ruby, where ^ is
// start-of-line rather than start-of-string.
//
// Anchored deliberately. Scanning anywhere in the string would make every path
// an exec merely PASSES to a command into a dependency, so `/bin/cp /etc/src
// /etc/dst` would claim to depend on files it only reads or writes.
var (
	execPath    = regexp.MustCompile(`(?m)^(/\S+)`)
	execQuoted  = regexp.MustCompile(`(?m)^"([^"]+)"`)
	execNumeric = regexp.MustCompile(`^\d+$`)
)

// AutoRequires implements Provider.
//
// Read from Puppet 8.10.0, type/exec.rb:593-640 autorequire(:file), in its
// order: cwd if set; then the command, or an argv's first element, scanned
// with execPath and then execQuoted; then each onlyif and unless command (a
// scalar is a one-element list), or an argv check's first element, scanned
// with execPath only. A guard's quoted path is no edge (measured).
// Only managed paths become edges. A user that is not a uid then adds its
// User[...] edge last (type/exec.rb:642-648).
func (e Exec) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(path string) {
		if path == "" {
			return
		}
		ref := "File[" + path + "]"
		if _, dup := seen[ref]; dup || !managed(ref) {
			return
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}

	if cwd, ok := r.Parameters["cwd"].(string); ok {
		add(cwd)
	}
	// line is `line = line[0] if line.is_a? Array`. Validate refuses an argv
	// whose first element is not a string, which Ruby cannot scan.
	line := func(v any) (string, bool) {
		if argv, ok := v.([]any); ok && len(argv) > 0 {
			v = argv[0]
		}
		s, ok := v.(string)
		return s, ok
	}
	if text, ok := line(r.Parameters["command"]); ok {
		for _, m := range execPath.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
		for _, m := range execQuoted.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	for _, param := range []string{"onlyif", "unless"} {
		v, ok := r.Parameters[param]
		if !ok {
			continue
		}
		list, isList := v.([]any)
		if !isList {
			list = []any{v}
		}
		for _, check := range list {
			if text, ok := line(check); ok {
				for _, m := range execPath.FindAllStringSubmatch(text, -1) {
					add(m[1])
				}
			}
		}
	}
	// type/exec.rb:642-648: a name, not a uid, autorequires its User.
	if user, ok := r.Parameters["user"].(string); ok && !execNumeric.MatchString(user) {
		if ref := "User[" + user + "]"; managed(ref) {
			out = append(out, ref)
		}
	}
	return out
}

// Properties implements PropertyDeclaring. type/exec.rb has one property,
// returns (newproperty(:returns)); command, unless, onlyif and the rest are
// parameters.
func (Exec) Properties() Attrs { return NewAttrs("returns") }

// Attrs implements Provider.
func (Exec) Attrs() Attrs {
	return NewAttrs("command", "path", "user", "group", "cwd", "logoutput", "refresh", "environment", "umask",
		"timeout", "tries", "try_sleep", "returns", "refreshonly", "creates", "onlyif", "unless")
}

// quotedExe is provider/exec.rb extractexe's pattern. Ruby's ^ matches at every
// line start, so (?m) is what makes a quoted path opening a later line of the
// command the executable, as it is in Ruby.
var quotedExe = regexp.MustCompile(`(?m)^"([^"]+)"|^'([^']+)'`)

// Validate implements Validating.
//
// Read from Puppet 8.10.0 source, not measured. Before this check a relative
// cwd ran the command via /bin/sh in voxagent's own working directory -- the
// same defect File.Validate closed for paths. Puppet refuses these
// while converting the catalog, so the whole run fails:
//
//   - cwd (type/exec.rb:266) and creates (:420) are Parameter::Path, whose
//     validate_path (parameter/path.rb) refuses an array where one path is
//     allowed and any path not starting with "/"; creates calls accept_arrays
//     (:448), so only cwd refuses an array. type.rb:692 wraps that as
//     "Parameter <name> failed on <ref>: ...".
//   - logoutput (:271) takes newvalues(:true, :false, :on_failure), which
//     refuses anything else as "Invalid value ...", wrapped the same way.
//   - unless (:497) and onlyif (:560) run validatecmd on each command and
//     are wrapped the same way.
//   - the command (:589) goes through provider/exec.rb validatecmd, which
//     refuses an executable not starting with "/" when no path is set;
//     type.rb:2360 wraps that as "Validation of <ref> failed: ...".
//
// The first failure is returned, in Puppet's order: parameters are set in
// allattrs order, where the command (the namevar, type/exec.rb:216-220 checks
// its class), cwd, logoutput, creates, unless and onlyif are declared in that
// sequence, and the type-level validate that checks the executable runs after
// every parameter is set.
//
// voxagent also refuses two argv shapes (apply-spine.md D-54): a Hash element,
// which Ruby's to_s would render as Hash#inspect, and, with a path, an argv
// whose first element is not a string, on which Puppet's autorequire raises
// and fails the whole catalog. Without a path validatecmd refuses that one
// first, naming the element as Ruby interpolates it.
func (e Exec) Validate(r catalog.Resource) error {
	// The command is the namevar, so absent the parameter the title is it.
	var command any = r.Title
	if v, ok := r.Parameters["command"]; ok {
		command = v
	}
	argvs := map[string][][]any{}
	switch c := command.(type) {
	case string:
	case []any:
		argvs["command"] = [][]any{c}
	default:
		return fmt.Errorf("Parameter command failed on %s: Command must be a String or Array<String>, got value of class %s",
			r.Ref(), catalog.RubyClassName(command))
	}
	for _, name := range []string{"onlyif", "unless"} {
		list, _ := r.Parameters[name].([]any)
		for _, check := range list {
			if argv, ok := check.([]any); ok {
				argvs[name] = append(argvs[name], argv)
			}
		}
	}
	for _, name := range []string{"command", "unless", "onlyif"} {
		for _, argv := range argvs[name] {
			if hasHashElement(argv) {
				return fmt.Errorf("%s: %s => an argv element of class Hash is not implemented", r.Ref(), name)
			}
		}
	}
	var params execParams
	if err := params.parseIdentity(r); err != nil {
		return err
	}
	if cwd, ok := r.Parameters["cwd"]; ok {
		if _, isArray := cwd.([]any); isArray {
			return fmt.Errorf("Parameter cwd failed on %s: cwd only accepts a single path, not an array of paths", r.Ref())
		}
		if !absolutePaths(cwd) {
			return fmt.Errorf("Parameter cwd failed on %s: cwd must be a fully qualified path", r.Ref())
		}
	}
	if _, err := logoutputMode(r); err != nil {
		return err
	}
	if err := params.parseRun(e, r); err != nil {
		return err
	}
	if creates, ok := r.Parameters["creates"]; ok && !absolutePaths(creates) {
		return fmt.Errorf("Parameter creates failed on %s: creates must be a fully qualified path", r.Ref())
	}
	if path, ok := r.Parameters["path"]; !ok || path == nil {
		redact := commandsMarked(e, r)
		// unless is declared before onlyif (type/exec.rb:459, :522); each runs
		// validatecmd on every command, a string or an array whose elements
		// are strings or argv arrays, wrapped as a parameter failure like cwd.
		for _, name := range []string{"unless", "onlyif"} {
			cmds, ok := r.Parameters[name]
			if !ok {
				continue
			}
			list, isList := cmds.([]any)
			if !isList {
				list = []any{cmds}
			}
			for _, cmd := range list {
				if exe, bad := unqualifiedExe(cmd); bad {
					return fmt.Errorf("Parameter %s failed on %s: %s", name, r.Ref(), unqualifiedMessage(exe, redact))
				}
			}
		}
		if exe, bad := unqualifiedExe(command); bad {
			return fmt.Errorf("Validation of %s failed: %s", r.Ref(), unqualifiedMessage(exe, redact))
		}
	}
	// autorequire's order: the command, then onlyif, then unless.
	for _, name := range []string{"command", "onlyif", "unless"} {
		for _, argv := range argvs[name] {
			if len(argv) == 0 {
				return fmt.Errorf("%s: %s => an empty argv is not implemented", r.Ref(), name)
			}
			if _, ok := argv[0].(string); !ok {
				return fmt.Errorf("%s: %s => an argv whose first element is of class %s is not implemented",
					r.Ref(), name, catalog.RubyClassName(argv[0]))
			}
		}
	}
	return nil
}

// commandsMarked reports whether the catalog marks command, unless or onlyif
// sensitive. Marking one marks every one given (MarkSensitive), so any hit
// redacts the command and the guards alike.
func commandsMarked(e Exec, r catalog.Resource) bool {
	marked, _ := MarkSensitive(e, r)
	for _, name := range []string{"command", "unless", "onlyif"} {
		if _, ok := marked[name]; ok {
			return true
		}
	}
	return false
}

// hasHashElement reports whether a map sits anywhere in argv.
func hasHashElement(argv []any) bool {
	for _, el := range argv {
		switch v := el.(type) {
		case map[string]any:
			return true
		case []any:
			if hasHashElement(v) {
				return true
			}
		}
	}
	return false
}

// unqualifiedMessage is validatecmd's failure text (provider/exec.rb:105).
//
// A marked command or guard is shown as [command redacted], quotes included
// (D-78: Ruby leaks it).
func unqualifiedMessage(exe string, redact bool) string {
	return command.QuoteOrRedact(exe, redact) + " is not qualified and no path was specified. Please qualify the command or specify a path."
}

// unqualifiedExe is validatecmd's test (provider/exec.rb:102): the executable
// extractexe finds must be absolute when no path is given. It returns the
// executable and true when the command is NOT qualified. A shape validatecmd
// cannot read returns false; the apply-time type check reports it.
func unqualifiedExe(command any) (string, bool) {
	exe, ok := extractExe(command)
	if !ok {
		return "", false
	}
	return exe, !strings.HasPrefix(exe, "/")
}

// extractExe is provider/exec.rb extractexe: the executable of a string or an
// argv command. ok is false for a shape it cannot read.
func extractExe(command any) (exe string, ok bool) {
	switch c := command.(type) {
	case string:
		if m := quotedExe.FindStringSubmatch(c); m != nil {
			return m[1] + m[2], true
		}
		exe, _, _ = strings.Cut(c, " ")
		return exe, true
	case []any:
		return argvExe(c), true
	}
	return "", false
}

// absolutePaths is parameter/path.rb validate_path's test: Array(paths).all?
// { absolute_path? }, where a non-string is never absolute.
func absolutePaths(v any) bool {
	paths, ok := v.([]any)
	if !ok {
		paths = []any{v}
	}
	for _, p := range paths {
		s, ok := p.(string)
		if !ok || !strings.HasPrefix(s, "/") {
			return false
		}
	}
	return true
}

// Apply implements Provider. A refreshonly exec does nothing here; its work
// happens in Refresh.
//
// It returns no warnings. Nothing in this provider's bounded attribute set has
// a Ruby warnonce behind it, so there is none to report; run keeps the
// two-value shape because Refresh shares it and a refresh has no run log of its
// own to warn into.
func (e Exec) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	if truthy(r.Parameters["refreshonly"]) {
		// A refreshonly exec fails its refreshonly check on every apply
		// (type/exec.rb:387, check_all_attributes(false)); Refresh skips it.
		debugBlocked(ctx, r.Parameters["command"], commandsMarked(e, r), "refreshonly")
		return nil, nil, nil
	}
	changes, err := e.run(ctx, r, noop)
	return changes, nil, err
}

// Refresh implements Provider. Every exec is refreshable, refreshonly or not --
// that is Puppet's behaviour.
//
// A guard that blocks the command (creates/onlyif/unless) makes run return
// (nil, nil), and Refresh still reports a refresh. type/exec.rb:690-699's
// refresh is `if check_all_attributes(true) ... end`, which returns normally
// either way, and process_callback counts a normal return as a refresh:
// Puppet logs `Triggered 'refresh' from 1 event`, records a refresh event and
// sets restarted for a command it did not run. Pinned by TestExecRefreshBlockedByAGuardStillTriggers.
func (e Exec) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	out, err := e.RefreshLogged(ctx, r, noop)
	return out.Refreshed, err
}

// RefreshLogged implements RefreshLogging. A refresh is returns.sync
// (type/exec.rb:690-698), so its output is logged by the same rule as Apply's,
// at the returns property, before the walk's Triggered or Failed line.
// Under noop the guards run and the command does not, as in Apply.
func (e Exec) RefreshLogged(ctx context.Context, r catalog.Resource, noop bool) (RefreshOutcome, error) {
	var out RefreshOutcome
	if refresh := r.Parameters["refresh"]; refresh != nil {
		if err := e.refreshCommand(ctx, r, refresh, noop); err != nil {
			return out, err
		}
		out.Refreshed = true
		return out, nil
	}
	changes, err := e.run(ctx, r, noop)
	if err != nil {
		return out, err
	}
	for _, ch := range changes {
		for _, n := range ch.PropertyNotices {
			out.PropertyNotices = append(out.PropertyNotices, PropertyNotice{Property: ch.Property, Message: n})
		}
	}
	// A refresh reports a failing command as an ERROR, not as a returns event.
	// Read in type/exec.rb refresh: it calls property(:returns).sync itself, so
	// self.fail raises straight out of refresh rather than reaching
	// resource_harness's per-property event path. event_manager.rb's rescue then
	// sets failed_to_restart and fail_with_event, whose event is a
	// resource_error. Returning true here would report a refresh that happened
	// successfully when the command it ran had failed.
	for _, ch := range changes {
		if ch.Err != nil {
			return out, ch.Err
		}
	}
	out.Refreshed = true
	return out, nil
}

// refreshCommand is type/exec.rb:690-699 with a refresh parameter: when the
// guards pass, provider.run(refresh) runs once instead of the main command (M12).
// Its status is not read, except that 127 raises its output (M20); it logs no
// output and does no tries. A guard that blocks it still reports a refresh, as
// in RefreshLogged. Its executable is checked first (posix.rb checkexe), before
// the Executing line, and a marked command shows redacted as for the main command.
func (e Exec) refreshCommand(ctx context.Context, r catalog.Resource, refresh any, noop bool) error {
	redact := commandsMarked(e, r)
	if passed, err := e.guardsPass(ctx, r, r.Parameters["command"], redact); err != nil || !passed {
		return err
	}
	// A marked refresh is redacted too (D-78: Ruby leaks it), as parseRun does.
	redact = redact || slices.Contains(r.SensitiveParameters, "refresh")
	if noop {
		return nil
	}
	exe, _ := extractExe(refresh)
	path, err := pathParam(r)
	if err != nil {
		return err
	}
	if err := checkExe(exe, path, redact); err != nil {
		return err
	}
	_, _, err = e.execute(ctx, r, refresh, redact, false)
	return err
}

// Prefetch implements Provider. An exec has no state to pre-read.
func (e Exec) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return e, nil }

// guardSemantics documents Puppet's check semantics for the three guard
// attributes, measured against type/exec.rb:657-675 (check_all_attributes).
// Puppet wraps a scalar into a one-element array, so scalar and array behave
// identically:
//
//	onlyif   passes (command runs) only if EVERY entry exits 0
//	unless   passes (command runs) only if EVERY entry exits non-zero
//	creates  passes (command runs) only if EVERY entry is missing
//
// The first failing entry stops evaluation -- the command does not run, and
// no later entry is checked.
func (e Exec) run(ctx context.Context, r catalog.Resource, noop bool) ([]Change, error) {
	commandValue, hasCommand := r.Parameters["command"]
	if !hasCommand {
		return nil, fmt.Errorf("exec has no command")
	}
	// command is a string, run through /bin/sh -c, or an argv, run directly
	// with no shell. Validate
	// refuses any other class with Puppet's text; this is the same check for
	// a caller that did not validate.
	switch c := commandValue.(type) {
	case string:
		if c == "" {
			return nil, fmt.Errorf("exec has no command")
		}
	case []any:
	default:
		return nil, fmt.Errorf("Command must be a String or Array<String>, got value of class %s", catalog.RubyClassName(commandValue))
	}

	// A marked command, unless or onlyif is shown as [command redacted] in
	// every message below, guards included: Puppet's override marks the three
	// together. D-78: Ruby leaks the command in the noop and run errors.
	redact := commandsMarked(e, r)

	logoutput, err := logoutputMode(r)
	if err != nil {
		return nil, err
	}

	if passed, err := e.guardsPass(ctx, r, commandValue, redact); err != nil || !passed {
		return nil, err
	}

	// noop is evaluated AFTER the guards above, deliberately: noop cannot
	// honestly report "no change" without first knowing whether a guard would
	// have skipped the command anyway. This means guard commands DO execute
	// under --noop -- that is correct, not an oversight: noop promises not to
	// change state, and running a read-only check is not a change. Pinned by
	// TestExecNoopEvaluatesGuardsBeforeReporting.
	if noop {
		// resource_harness.rb's noop event (measured, and compared by G5's
		// concat M13 --noop pass): {"previous_value"=>"notrun",
		// "desired_value"=>["0"], "message"=>"executed successfully (noop)",
		// "name"=>"executed_command", "status"=>"noop"}. desired_value is the
		// should value, the accepted list, whether or not the command ran, so
		// no exit status is claimed; the message names no command.
		accepted, err := acceptableList(r.Parameters["returns"], isMarked(e, r, "returns"))
		if err != nil {
			return nil, fmt.Errorf("exec returns: %w", err)
		}
		desired := make([]string, len(accepted))
		for i, want := range accepted {
			desired[i] = strconv.Itoa(want)
		}
		return []Change{{Property: "returns", Name: "executed_command",
			Was: "notrun", Is: desired, Message: "executed successfully (noop)"}}, nil
	}

	// An argv's executable is checked before it runs (posix.rb:30-54, run by
	// provider/exec.rb:46 inside returns#sync), so its ArgumentError fails
	// the returns property exactly as a bad status does. That
	// failure carries the accepted list, so an argv's returns is read first;
	// a string's is read after it ran, as it always was.
	var accepted []int
	// acceptable reads returns. A marked returns is shown as [redacted] in the
	// error that quotes its value (D-78: Ruby leaks it).
	returnsMarked := isMarked(e, r, "returns")
	acceptable := func() ([]int, error) {
		list, err := acceptableList(r.Parameters["returns"], returnsMarked)
		if err != nil {
			return nil, fmt.Errorf("exec returns: %w", err)
		}
		return list, nil
	}
	var exeErr error
	argv, isArgv := commandValue.([]any)
	if isArgv {
		if accepted, err = acceptable(); err != nil {
			return nil, err
		}
		path, err := pathParam(r)
		if err != nil {
			return nil, err
		}
		exeErr = checkExe(argvExe(argv), path, redact)
	}
	// The output never enters the event message. Ruby logs it as separate
	// notices at the returns property, which type/exec.rb sync (:120-190)
	// emits from its logoutput branch before it fails -- measured,
	// `command => '/bin/sh -c "echo hello; exit 7"'` yields the message
	// `change from 'notrun' to ['0'] failed: '/bin/sh -c "echo hello; exit
	// 7"' returned 7 instead of one of [0]` and a separate "hello" notice. So it
	// becomes Change.PropertyNotices when logoutput (:271) is
	// true, or on_failure and the status is not accepted; see outputNotices.
	// Appending it to the detail instead, as this once did, put text in the
	// message that Ruby's does not hold.
	//
	// A timeout or an exit status of 127 fails returns too, with no output
	// logged: type/exec.rb sync rescues Timeout::Error as "Command exceeded
	// timeout" (M1), and provider.run raises the 127 output before sync has
	// any output to log (M20).
	p, err := parseExecParams(e, r)
	if err != nil {
		return nil, err
	}
	// Tries (type/exec.rb:123-164) loops `tries.times`, which a Float does not
	// have (M19): returns fails before anything runs.
	if p.Tries != math.Trunc(p.Tries) {
		exeErr = fmt.Errorf("undefined method `times' for %s:Float", rubyvalue.Float(p.Tries))
	}
	var code int
	var output string
	for try := 1.0; exeErr == nil && try <= p.Tries; try++ {
		if p.Tries > 1 {
			runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource+"/returns", "Exec try %.0f/%.0f", try, p.Tries)
		}
		code, output, err = e.execute(ctx, r, commandValue, redact, false)
		var e127 exit127Error
		switch {
		case errors.Is(err, errExecTimeout):
			exeErr = errors.New("Command exceeded timeout")
			continue
		case errors.As(err, &e127):
			exeErr = err
			continue
		case err != nil:
			return nil, err
		}
		if !isArgv && accepted == nil {
			if accepted, err = acceptable(); err != nil {
				return nil, err
			}
		}
		if slices.Contains(accepted, code) {
			break
		}
		// The sleep follows the last failed try too (M6, I-32).
		if p.TrySleep > 0 && p.Tries > 1 {
			runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource+"/returns", "Sleeping for %s seconds between tries", p.TrySleepShown)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(min(p.TrySleep*float64(time.Second), 1<<62))):
			}
		}
	}
	if !isArgv && accepted == nil {
		if accepted, err = acceptable(); err != nil {
			return nil, err
		}
	}

	// desired_value is the ACCEPTED LIST, as strings -- not the status the
	// command actually returned. type/exec.rb declares
	// `newproperty(:returns, :array_matching => :all, :event =>
	// :executed_command)` with `munge(&:to_s)`, so the property's should value is
	// an array of strings and the event carries it verbatim.
	//
	// Measured, `command => '/bin/sh -c "exit 3"', returns => [0, 3]`:
	// "desired_value"=>["0", "3"] with status success, while the command exited
	// 3. Reporting the status here would make every exec event claim the run's
	// outcome was what the catalog asked for.
	desired := make([]string, len(accepted))
	for i, want := range accepted {
		desired[i] = strconv.Itoa(want)
	}

	// Name is the property's :event option, so it is executed_command on
	// success AND on failure -- measured in both. Without it the transaction
	// falls back to "returns_changed", which no Ruby report contains.
	change := Change{Property: "returns", Name: "executed_command", Was: "notrun", Is: desired}

	if exeErr != nil {
		change.Err = exeErr
		return []Change{change}, nil
	}
	// An output that cannot be decoded fails the change with Ruby's split
	// error in place of the exit-status error, whatever the status.
	succeeded := slices.Contains(accepted, code)
	if change.PropertyNotices, err = outputNotices(logoutput, !succeeded, output, redact); err != nil {
		change.Err = err
		return []Change{change}, nil
	}
	if succeeded {
		// type/exec.rb's change_to_s for returns, read: `_("executed
		// successfully")`. It names neither the command nor the status.
		change.Message = "executed successfully"
		return []Change{change}, nil
	}
	// A status outside the list fails the RETURNS PROPERTY rather than the
	// resource. type/exec.rb sync calls self.fail, which raises a Puppet::Error
	// from inside the property, and property.rb:488's rescue re-raises a
	// Puppet::Error untouched -- only another StandardError gets the "Could not
	// set ... on returns:" wrapper at :491. So the detail below is the whole
	// detail, and the transaction renders the measured
	// `change from 'notrun' to ['0'] failed: <detail>`.
	//
	// The list is spelled twice in one event, two different ways, because Ruby
	// spells it twice: format_value_for_display puts ", " between the elements
	// of desired_value, while this detail is `should.join(",")` -- measured
	// `change from 'notrun' to ['0', '3'] failed: ... instead of one of [0,3]`.
	change.Err = fmt.Errorf("%s returned %d instead of one of [%s]",
		command.QuoteOrRedact(commandDisplay(commandValue), redact), code, strings.Join(desired, ","))
	return []Change{change}, nil
}

// guardsPass is check_all_attributes (type/exec.rb:657-675): whether creates,
// onlyif and unless all let the command run. It logs the blocked line itself.
// A refresh reuses it, so a guard blocks the refresh command as it blocks the
// main one.
func (e Exec) guardsPass(ctx context.Context, r catalog.Resource, command any, redact bool) (bool, error) {
	creates, err := stringListParam(r, "creates")
	if err != nil {
		return false, err
	}
	for _, path := range creates {
		// type/exec.rb:454, source <resource>/creates. creates is a parameter,
		// which Sensitive cannot mark (MarkSensitive), so the path shows as is.
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource+"/creates", "Checking that 'creates' path '%s' exists", path)
		if _, err := os.Stat(path); err == nil {
			debugBlocked(ctx, command, redact, "creates")
			return false, nil
		} else if !os.IsNotExist(err) {
			// "we could not check" is not "the path is missing". Treating an
			// EACCES (or any other stat failure) as missing would re-run the
			// command every time, silently, on every agent run.
			return false, fmt.Errorf("stat creates path %s: %w", path, err)
		}
	}

	onlyifs, err := commandList(r, "onlyif")
	if err != nil {
		return false, err
	}
	for _, guard := range onlyifs {
		ok, err := e.guardPasses(ctx, r, "onlyif", guard, redact)
		if err != nil {
			return false, err
		}
		if !ok {
			debugBlocked(ctx, command, redact, "onlyif")
			return false, nil
		}
	}

	unlesses, err := commandList(r, "unless")
	if err != nil {
		return false, err
	}
	for _, guard := range unlesses {
		ok, err := e.guardPasses(ctx, r, "unless", guard, redact)
		if err != nil {
			return false, err
		}
		if !ok {
			debugBlocked(ctx, command, redact, "unless")
			return false, nil
		}
	}
	return true, nil
}

// debugBlocked logs that check stopped the command (type/exec.rb:672), at the
// resource. Ruby prints @parameters[:command].value, so an argv shows as
// Array#inspect (commandDisplay), and "[command redacted]" for a marked one.
func debugBlocked(ctx context.Context, command any, redact bool, check string) {
	shown := commandDisplay(command)
	if redact {
		shown = "[command redacted]"
	}
	runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "'%s' won't be executed because of failed check '%s'", shown, check)
}

// guardPasses runs the named onlyif or unless command and reports whether it
// lets the command run: an onlyif that exited 0, an unless that did not.
// A guard that cannot be started at all is an error, not a false: "we could not
// check" is not the same claim as "the condition does not hold", and treating
// the first as the second would silently skip the command. An argv check's
// executable fails checkExe the same way, as provider.run(value, true) raises
// it in Puppet.
//
// Its output is logged at debug, one line per line of output at
// <resource>/<name>, or one "[output redacted]" when the command is marked
// (type/exec.rb:511-514, :568-571). The output is split only when debug is on.
func (e Exec) guardPasses(ctx context.Context, r catalog.Resource, name string, commandValue any, redact bool) (bool, error) {
	if argv, ok := commandValue.([]any); ok {
		path, err := pathParam(r)
		if err != nil {
			return false, err
		}
		if err := checkExe(argvExe(argv), path, redact); err != nil {
			return false, fmt.Errorf("guard %s: %w", command.QuoteOrRedact(commandDisplay(commandValue), redact), err)
		}
	}
	code, out, err := e.execute(ctx, r, commandValue, redact, true)
	// A timed-out guard blocks the command, whichever guard it is: type/exec.rb
	// check rescues Timeout::Error, logs it at the guard and returns false (M1,
	// M4). An exit of 127 fails the resource with the output alone (M20).
	if errors.Is(err, errExecTimeout) {
		runlog.From(ctx).Log("err", sources.SourcesFrom(ctx).Resource+"/"+name,
			"Check "+rubyvalue.RedactIf(redact, "%s", rubyvalue.InspectValue(commandValue))+" exceeded timeout")
		return false, nil
	}
	var e127 exit127Error
	if errors.As(err, &e127) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("guard %s: %w", command.QuoteOrRedact(commandDisplay(commandValue), redact), err)
	}
	if l := runlog.From(ctx); l.Enabled("debug") {
		source := sources.SourcesFrom(ctx).Resource + "/" + name
		if redact {
			l.Log("debug", source, "[output redacted]")
		} else {
			for _, line := range command.SplitOutput(out) {
				l.Log("debug", source, line)
			}
		}
	}
	return (code == 0) == (name == "onlyif"), nil
}

// execute runs a command and returns its exit status. A non-zero status is a
// value, not an error; only a failure to start or to wait is an error. Output
// goes through a temporary file, so a command that backgrounds a process
// (`/opt/app/run &`) returns when the shell exits, as under Puppet.
//
// A string runs through /bin/sh -c. An argv runs with no shell, as Puppet's
// child runs Kernel.exec(*argv) after its chdir to cwd (util/execution.rb:
// 359,388-391): argv[0] with a slash is used as given, relative to cwd, and
// one without is found through the resource's path by lookPathIn, whose "."
// and empty entries are cwd too. Validate guarantees the path for that.
// Every way that exec can fail once checkExe has passed (no file found from
// cwd, a cwd that cannot be entered, EACCES on an interpreter, an empty argv)
// is status 1, as the child's rescue calls exit!(1) (:392-396); a file with
// no #! line is retried as a /bin/sh script, as Ruby's exec retries ENOEXEC.
//
// check says the command is a guard: Ruby's provider names it in its debug line
// (provider/exec.rb:48), source the provider form, "Executing check '<cmd>'" or
// "Executing '<cmd>'", [redacted] for a marked command.
func (e Exec) execute(ctx context.Context, r catalog.Resource, commandValue any, redact, check bool) (int, string, error) {
	kind := "Executing"
	if check {
		kind = "Executing check"
	}
	runlog.Debugf(ctx, sources.SourcesFrom(ctx).Provider, "%s '%s'", kind, rubyvalue.RedactIf(redact, "%s", commandDisplay(commandValue)))
	var cwd string
	if cwdVal, ok := r.Parameters["cwd"]; ok {
		s, ok := cwdVal.(string)
		if !ok {
			return 0, "", fmt.Errorf("cwd is %T, not a string", cwdVal)
		}
		cwd = s
	}
	path, err := pathParam(r)
	if err != nil {
		return 0, "", err
	}
	p, err := parseExecParams(e, r)
	if err != nil {
		return 0, "", err
	}
	env := execEnv(ctx, r, p, path, slices.Contains(r.SensitiveParameters, "environment"))
	marked := func(kind string) bool { return slices.Contains(r.SensitiveParameters, kind) }
	argv, isArgv := commandValue.([]any)
	var words []string
	display, _ := commandValue.(string)
	if isArgv {
		words = argvOf(argv)
		display = strings.Join(words, " ")
	}
	// util/execution.rb:167-180 logs this before the child resolves the
	// identity or the program, so an unknown user or group still shows it (M16).
	var as string
	for _, xid := range []struct {
		field, kind string
		value       any
	}{{"uid", "user", p.User}, {"gid", "group", p.Group}} {
		if xid.value == nil {
			continue
		}
		s, ok := xid.value.(string)
		if !ok {
			s = rubyvalue.InspectValue(xid.value)
		}
		as += " " + xid.field + "=" + rubyvalue.RedactIf(marked(xid.kind), "%s", s)
	}
	if as != "" {
		as = " with" + as
	}
	runlog.Debugf(ctx, "Puppet", "Executing%s: '%s'", as, rubyvalue.RedactIf(redact, "%s", display))
	id, err := resolveIdentity(p.User, p.Group, marked)
	var invalid invalidIdentityError
	if errors.As(err, &invalid) {
		// Raised in Ruby's child, before exec: its rescue logs the error to
		// the output and exits 1 (util/execution.rb:393-395, M6, M18).
		return 1, "Error: Could not execute posix command: " + invalid.Error() + "\n", nil
	}
	if err != nil {
		return 0, "", err
	}
	stage := staging{cwd: cwd, umask: p.Umask, id: id}
	start := (*exec.Cmd).Start
	if stage.needed() {
		start = stage.start
	}
	child := func(name string, args ...string) *exec.Cmd {
		// runExec signals the group this makes; see there. Dir stays empty:
		// stage.start enters cwd for the child.
		cmd := exec.Command(name, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Env = env
		return cmd
	}
	// Status 127 is an error whatever returns accepts (provider/exec.rb:80-83).
	// Its text is the output, so a marked command's is "[output redacted]",
	// as its logged output is (D-78: a missing executable's shell error names
	// it).
	run := func(cmd *exec.Cmd, what string) (int, string, error) {
		code, out, err := runExec(ctx, cmd, start, p.Timeout, what)
		if err == nil && code == 127 {
			if redact {
				return code, out, exit127Error{output: "[output redacted]"}
			}
			return code, out, exit127Error{output: out}
		}
		return code, out, err
	}
	if !isArgv {
		return run(child("/bin/sh", "-c", display), command.QuoteOrRedact(display, redact))
	}

	line := command.QuoteOrRedact(commandDisplay(commandValue), redact)
	if len(words) == 0 {
		return 1, "", nil
	}
	prog := words[0]
	if !strings.Contains(prog, "/") {
		hit, found := lookPathIn(prog, path, cwd)
		if !found {
			return 1, "", nil
		}
		prog = hit
	}
	// prog holds a slash, so exec.Command does no LookPath of its own (and
	// raises no ErrDot); a relative prog resolves in the cwd stage.start enters, or in the
	// agent's own when there is none.
	cmd := child(prog, words[1:]...)
	cmd.Args[0] = words[0]
	code, out, err := run(cmd, line)
	var startErr *fs.PathError
	if err == nil || !errors.As(err, &startErr) || startErr.Op != "fork/exec" {
		return code, out, err
	}
	// A cwd that cannot be entered reaches here from stage.start.
	if errors.Is(startErr.Err, syscall.ENOEXEC) {
		// The script path and the argv go to sh as separate positional
		// arguments, no -c: the shell reads the file, never parses the argv.
		retry := child("/bin/sh", append([]string{prog}, words[1:]...)...)
		retry.Args[0] = "sh"
		return run(retry, line)
	}
	return 1, out, nil
}

// pathParam returns path as a colon-joined string, accepting both the scalar
// and array shapes a real catalog produces. Measured: exec.rb:223-228
// documents path as "an array or as a File::PATH_SEPARATOR separated list" --
// i.e. an array joins with ':', matching a plain string already in that form.
func pathParam(r catalog.Resource) (string, error) {
	v, ok := r.Parameters["path"]
	if !ok {
		return "", nil
	}
	switch val := v.(type) {
	case string:
		return val, nil
	case []any:
		parts := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return "", fmt.Errorf("path element is %T, not a string", item)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ":"), nil
	default:
		return "", fmt.Errorf("path is %T, not a string or array", v)
	}
}

// commandList reads onlyif or unless: a list of commands, each a string or an
// argv (a nested array), where a scalar is a one-element list
// (type/exec.rb:493-499,556-562). A flat array is several string commands, not
// one argv. A missing key returns (nil, nil), which is "not declared".
func commandList(r catalog.Resource, name string) ([]any, error) {
	v, ok := r.Parameters[name]
	if !ok {
		return nil, nil
	}
	switch val := v.(type) {
	case string:
		return []any{val}, nil
	case []any:
		for _, item := range val {
			switch item.(type) {
			case string, []any:
			default:
				return nil, fmt.Errorf("%s element is %T, not a string or array", name, item)
			}
		}
		return val, nil
	default:
		return nil, fmt.Errorf("%s is %T, not a string or array", name, v)
	}
}

// stringListParam reads creates, accepting both the scalar and array shapes a
// real catalog produces. A missing key returns (nil, nil); the
// caller's range over a nil slice is a no-op, which is exactly "not declared".
func stringListParam(r catalog.Resource, name string) ([]string, error) {
	v, ok := r.Parameters[name]
	if !ok {
		return nil, nil
	}
	switch val := v.(type) {
	case string:
		return []string{val}, nil
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s element is %T, not a string", name, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s is %T, not a string or array", name, v)
	}
}

// acceptableList returns the declared acceptable exit statuses. Puppet's
// default, when returns is not declared, is [0].
//
// An unparseable entry or an empty array is an error, not a silent fallback
// to 0: a `returns` filter the catalog never declared is a policy this
// provider must not invent. redact shows a quoted value as [redacted].
func acceptableList(returns any, redact bool) ([]int, error) {
	switch v := returns.(type) {
	case nil:
		return []int{0}, nil
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("returns is an empty array")
		}
		out := make([]int, 0, len(v))
		for _, item := range v {
			n, err := toInt(item, redact)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, nil
	default:
		n, err := toInt(v, redact)
		if err != nil {
			return nil, err
		}
		return []int{n}, nil
	}
}

// toInt converts a decoded catalog value to an exit status. JSON decoding
// produces float64 for a bare number; a manifest may also spell returns as a
// string. Anything else -- including an unparseable string -- is an error:
// silently treating it as 0 would invent an acceptable-status policy the
// catalog never declared.
func toInt(v any, redact bool) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("returns value %s is not an integer", rubyvalue.RedactIf(redact, "%q", n))
		}
		return i, nil
	default:
		return 0, fmt.Errorf("returns value is %T, not an integer", v)
	}
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	default:
		return false
	}
}
