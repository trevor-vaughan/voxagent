package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
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
	execPath   = regexp.MustCompile(`(?m)^(/\S+)`)
	execQuoted = regexp.MustCompile(`(?m)^"([^"]+)"`)
)

// AutoRequires implements Provider.
//
// Measured against Puppet 8.10.0, type/exec.rb autorequire(:file): cwd if set,
// plus every absolute path the two patterns above find in command, onlyif and
// unless. Only managed paths become edges.
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
	for _, param := range []string{"command", "onlyif", "unless"} {
		text, ok := r.Parameters[param].(string)
		if !ok {
			continue
		}
		for _, m := range execQuoted.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
		for _, m := range execPath.FindAllStringSubmatch(text, -1) {
			add(m[1])
		}
	}
	return out
}

// Attrs implements Provider.
func (Exec) Attrs() Attrs {
	return NewAttrs("command", "path", "cwd", "returns", "refreshonly", "creates", "onlyif", "unless")
}

// quotedExe is provider/exec.rb extractexe's pattern. Ruby's ^ matches at every
// line start, so (?m) is what makes a quoted path opening a later line of the
// command the executable, as it is in Ruby.
var quotedExe = regexp.MustCompile(`(?m)^"([^"]+)"|^'([^']+)'`)

// Validate implements Validating.
//
// Read from Puppet 8.10.0 source, not measured. Before this check a relative
// cwd ran the command via /bin/sh in voxagent's own working directory -- the
// same defect File.Validate closed for paths. Puppet refuses three things
// while converting the catalog, so the whole run fails:
//
//   - cwd (type/exec.rb:266) and creates (:420) are Parameter::Path, whose
//     validate_path (parameter/path.rb) refuses an array where one path is
//     allowed and any path not starting with "/"; type.rb:692 wraps that as
//     "Parameter <name> failed on <ref>: ...".
//   - the command (:589) goes through provider/exec.rb validatecmd, which
//     refuses an executable not starting with "/" when no path is set;
//     type.rb:2360 wraps that as "Validation of <ref> failed: ...".
//
// The first failure is returned, in Puppet's order: parameters are set in
// allattrs order, where cwd is declared before creates, and the type-level
// validate that checks the command runs after every parameter is set.
func (Exec) Validate(r catalog.Resource) error {
	if cwd, ok := r.Parameters["cwd"]; ok {
		if _, isArray := cwd.([]any); isArray {
			return fmt.Errorf("Parameter cwd failed on %s: cwd only accepts a single path, not an array of paths", r.Ref())
		}
		if !absolutePaths(cwd) {
			return fmt.Errorf("Parameter cwd failed on %s: cwd must be a fully qualified path", r.Ref())
		}
	}
	if creates, ok := r.Parameters["creates"]; ok && !absolutePaths(creates) {
		return fmt.Errorf("Parameter creates failed on %s: creates must be a fully qualified path", r.Ref())
	}
	// The command is the namevar, so absent the parameter the title is it.
	var command any = r.Title
	if v, ok := r.Parameters["command"]; ok {
		command = v
	}
	if path, ok := r.Parameters["path"]; ok && path != nil {
		return nil
	}
	var exe string
	switch c := command.(type) {
	case string:
		if m := quotedExe.FindStringSubmatch(c); m != nil {
			exe = m[1] + m[2]
		} else {
			exe, _, _ = strings.Cut(c, " ")
		}
	case []any:
		if len(c) > 0 {
			exe, _ = c[0].(string)
		}
	default:
		// Not a command shape validatecmd can read; the apply-time type check
		// in run reports it.
		return nil
	}
	if strings.HasPrefix(exe, "/") {
		return nil
	}
	return fmt.Errorf("Validation of %s failed: '%s' is not qualified and no path was specified. Please qualify the command or specify a path.", r.Ref(), exe)
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
		return nil, nil, nil
	}
	changes, err := e.run(ctx, r, noop)
	return changes, nil, err
}

// Refresh implements Provider. Every exec is refreshable, refreshonly or not --
// that is Puppet's behaviour.
//
// A guard that skips the command (creates/onlyif/unless) makes run return
// (nil, nil), so len(changes) == 0 here and Refresh correctly reports false:
// a refresh whose guard skipped never happened, and must not be reported as
// one. This is deliberately pinned by TestExecRefreshSkippedByGuardReportsNoRefresh
// -- returning true unconditionally would make the transaction set Restarted
// and write a "triggered refresh" event for a command that never ran.
func (e Exec) Refresh(ctx context.Context, r catalog.Resource, noop bool) (bool, error) {
	changes, err := e.run(ctx, r, noop)
	if err != nil {
		return false, err
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
			return false, ch.Err
		}
	}
	return len(changes) > 0, nil
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
	commandVal, hasCommand := r.Parameters["command"]
	if !hasCommand {
		return nil, fmt.Errorf("exec has no command")
	}
	// command must be a string. Puppet's command is a string, or an argv array
	// which is out of scope for this bounded attribute set; refuse an array
	// (or any other type) clearly rather than flattening it with %v, which
	// measured as `sh -c "[/bin/echo hi]"` -- exit 127 with no indication of
	// why.
	command, ok := commandVal.(string)
	if !ok {
		return nil, fmt.Errorf("command is %T, not a string (argv-array commands are not supported)", commandVal)
	}
	if command == "" {
		return nil, fmt.Errorf("exec has no command")
	}

	creates, err := stringListParam(r, "creates")
	if err != nil {
		return nil, err
	}
	for _, path := range creates {
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		} else if !os.IsNotExist(err) {
			// "we could not check" is not "the path is missing". Treating an
			// EACCES (or any other stat failure) as missing would re-run the
			// command every time, silently, on every agent run.
			return nil, fmt.Errorf("stat creates path %s: %w", path, err)
		}
	}

	onlyifs, err := stringListParam(r, "onlyif")
	if err != nil {
		return nil, err
	}
	for _, guard := range onlyifs {
		ok, err := e.guardPasses(ctx, r, guard)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}

	unlesses, err := stringListParam(r, "unless")
	if err != nil {
		return nil, err
	}
	for _, guard := range unlesses {
		ok, err := e.guardPasses(ctx, r, guard)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, nil
		}
	}

	// noop is evaluated AFTER the guards above, deliberately: noop cannot
	// honestly report "no change" without first knowing whether a guard would
	// have skipped the command anyway. This means guard commands DO execute
	// under --noop -- that is correct, not an oversight: noop promises not to
	// change state, and running a read-only check is not a change. Pinned by
	// TestExecNoopEvaluatesGuardsBeforeReporting.
	if noop {
		// Is is left empty rather than "0": the command has not run, so there
		// is no exit status to report, and "0" would assert a result that
		// never happened -- the exact class of defect this codebase treats as
		// a real bug. The message says "would execute" for the same reason.
		//
		// KNOWN DIVERGENCE, measured under --noop: Ruby's noop event is
		// {"previous_value"=>"notrun", "desired_value"=>["0"],
		// "message"=>"executed successfully (noop)", "name"=>"executed_command",
		// "status"=>"noop"} -- desired_value is the accepted list, exactly as on
		// a real run, because it is the property's should value and owes nothing
		// to whether the command ran. So Ruby's noop event says more than this
		// one does, and its message is the success wording with a "(noop)"
		// suffix. Left as it is on purpose: closing it is a report-shape
		// decision (voxagent has no "(noop)" suffix anywhere), not a bug in this
		// branch. G5 now compares exec's real-run events against Ruby, but it
		// applies no --noop run, so this branch is still reached by no gate.
		return []Change{{Property: "returns", Name: "executed_command",
			Was: "notrun", Is: "",
			Message: fmt.Sprintf("would execute '%s'", command)}}, nil
	}

	// The output is deliberately discarded. Ruby does not put it in the event
	// message -- measured, `command => '/bin/sh -c "echo hello; exit 7"'` yields
	// the message `change from 'notrun' to ['0'] failed: '/bin/sh -c "echo
	// hello; exit 7"' returned 7 instead of one of [0]` and logs "hello" as a
	// separate notice at the returns property, which type/exec.rb sync emits
	// from its logoutput branch before failing. logoutput is not in this
	// provider's bounded attribute set, and Change.Notice logs at source
	// "Puppet" rather than at the property, so that line cannot be reproduced
	// here. Appending the output to the detail instead, as this once did, put
	// text in the message that Ruby's does not hold.
	code, _, err := e.execute(ctx, r, command)
	if err != nil {
		return nil, err
	}
	accepted, err := acceptableList(r.Parameters["returns"])
	if err != nil {
		return nil, fmt.Errorf("exec returns: %w", err)
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
	if slices.Contains(accepted, code) {
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
	change.Err = fmt.Errorf("'%s' returned %d instead of one of [%s]",
		command, code, strings.Join(desired, ","))
	return []Change{change}, nil
}

// guardPasses runs an onlyif or unless command and reports whether it exited 0.
// A guard that cannot be started at all is an error, not a false: "we could not
// check" is not the same claim as "the condition does not hold", and treating
// the first as the second would silently skip the command.
func (e Exec) guardPasses(ctx context.Context, r catalog.Resource, command string) (bool, error) {
	code, _, err := e.execute(ctx, r, command)
	if err != nil {
		return false, fmt.Errorf("guard '%s': %w", command, err)
	}
	return code == 0, nil
}

// execute runs a command and returns its exit status. A non-zero status is a
// value, not an error; only a failure to start or to wait is an error.
func (e Exec) execute(ctx context.Context, r catalog.Resource, command string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	// CommandContext alone kills only the shell. Whether that stops the work
	// depends on which shell /bin/sh is -- see exec_unix.go for the measurement.
	setupProcessGroup(cmd)
	if cwdVal, ok := r.Parameters["cwd"]; ok {
		cwd, ok := cwdVal.(string)
		if !ok {
			return 0, "", fmt.Errorf("cwd is %T, not a string", cwdVal)
		}
		cmd.Dir = cwd
	}
	path, err := pathParam(r)
	if err != nil {
		return 0, "", err
	}
	if path != "" {
		cmd.Env = append(os.Environ(), "PATH="+path)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), string(out), nil
		}
		return 0, string(out), fmt.Errorf("run '%s': %w", command, err)
	}
	return 0, string(out), nil
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

// stringListParam reads creates/onlyif/unless, accepting both the scalar and
// array shapes a real catalog produces. A missing key returns (nil, nil); the
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
// provider must not invent.
func acceptableList(returns any) ([]int, error) {
	switch v := returns.(type) {
	case nil:
		return []int{0}, nil
	case []any:
		if len(v) == 0 {
			return nil, fmt.Errorf("returns is an empty array")
		}
		out := make([]int, 0, len(v))
		for _, item := range v {
			n, err := toInt(item)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, nil
	default:
		n, err := toInt(v)
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
func toInt(v any) (int, error) {
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
			return 0, fmt.Errorf("returns value %q is not an integer", n)
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
