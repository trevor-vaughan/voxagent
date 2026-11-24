package extension

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/report"
)

// Host is one running extension host. It is not safe for concurrent use: the
// protocol is strictly one request, then one response.
type Host struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	dir      string
	timeouts Timeouts
	hello    Hello

	known   map[string]struct{} // refs the loaded catalog holds
	aliases map[string]struct{} // refs the loaded catalog's resources are also known by
	// dead is the first failure. Once set, the process has been killed and every
	// call returns it: Puppet's prefetch cannot be replayed, so there is no
	// restart.
	dead   error
	closed bool
	stats  Stats
}

// Start runs argv with env, reads the hello and refuses a protocol other than
// Protocol. dir, when non-empty, is removed by Close. A failed handshake closes
// the host, so dir is already removed when Start returns an error; a failure
// before the process starts leaves dir for the caller.
func Start(argv, env []string, dir string, t Timeouts) (*Host, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if env == nil {
		env = []string{} // a nil Env would make the host inherit the agent's whole environment
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("extension host: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("extension host: %w", err)
	}
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the extension host %s: %w", argv[0], err)
	}
	h := &Host{cmd: cmd, stdin: stdin, stdout: stdout, dir: dir, timeouts: t}
	raw, err := h.roundTrip("hello", nil, t.Other)
	if err == nil {
		var env struct {
			Hello *Hello `json:"hello"`
		}
		if derr := decodeStrict(raw, &env); derr != nil || env.Hello == nil {
			err = h.fail(fmt.Errorf("the hello frame is malformed: %v", derr))
		} else if env.Hello.Protocol != Protocol {
			err = h.fail(fmt.Errorf("the extension host speaks protocol %d; voxagent speaks %d",
				env.Hello.Protocol, Protocol))
		} else {
			h.hello = *env.Hello
		}
	}
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	h.stats.Startup = time.Since(started)
	return h, nil
}

// Hello is the handshake the host sent.
func (h *Host) Hello() Hello { return h.hello }

// Stats reports start-up time, time spent in calls, and how many resources the
// host evaluated.
func (h *Host) Stats() Stats { return h.stats }

// fail records the first failure, kills the process, closes the parent's ends
// of its pipes, and returns the stored error. Closing the pipes is what
// unblocks a pending read or write: a descendant of the host may hold the
// other ends open past the kill.
func (h *Host) fail(cause error) error {
	if h.dead == nil {
		h.dead = fmt.Errorf("extension host failed: %w", cause)
		_ = h.cmd.Process.Kill()
		_ = h.stdin.Close()
		_ = h.stdout.Close()
	}
	return h.dead
}

// roundTrip writes req (nil writes nothing, for the hello) and reads one
// frame, within timeout. On a timeout or an I/O error the host is killed;
// fail closes the pipes, which unblocks the goroutine, and it is waited for
// so none outlives the call.
func (h *Host) roundTrip(op string, req []byte, timeout time.Duration) ([]byte, error) {
	if h.dead != nil {
		return nil, h.dead
	}
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if req != nil {
			if err := WriteFrame(h.stdin, req); err != nil {
				done <- result{err: err}
				return
			}
		}
		body, err := ReadFrame(h.stdout)
		done <- result{body, err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, h.fail(fmt.Errorf("%s: %w", op, r.err))
		}
		return r.body, nil
	case <-timer.C:
		err := h.fail(fmt.Errorf("the host did not answer %s within %s", op, timeout))
		<-done
		return nil, err
	}
}

// call sends {op: payload} and decodes the reply's body into out.
func (h *Host) call(op string, payload any, timeout time.Duration, out any) error {
	if h.dead != nil {
		return h.dead
	}
	req, err := json.Marshal(map[string]any{op: payload})
	if err != nil {
		return fmt.Errorf("extension host: encode %s: %w", op, err)
	}
	started := time.Now()
	defer func() { h.stats.Spent += time.Since(started) }()
	raw, err := h.roundTrip(op, req, timeout)
	if err != nil {
		return err
	}
	var env map[string]json.RawMessage
	if err := decodeStrict(raw, &env); err != nil {
		return h.fail(fmt.Errorf("%s: %w", op, err))
	}
	if len(env) != 1 {
		return h.fail(fmt.Errorf("%s: a reply has exactly one key, got %d", op, len(env)))
	}
	if body, ok := env["error"]; ok {
		var e struct {
			Message string `json:"message"`
		}
		if err := decodeStrict(body, &e); err != nil || e.Message == "" {
			return h.fail(fmt.Errorf("%s: malformed error reply", op))
		}
		return &CallError{Op: op, Message: e.Message}
	}
	body, ok := env[op]
	if !ok {
		for other := range env {
			return h.fail(fmt.Errorf("the host answered %s to %s", other, op))
		}
	}
	// A null body would decode as the zero reply: an evaluation that changed
	// nothing, or a refresh that did nothing. The host always sends an object.
	if string(body) == "null" {
		return h.fail(fmt.Errorf("%s: a null reply body", op))
	}
	if err := decodeStrict(body, out); err != nil {
		return h.fail(fmt.Errorf("%s: %w", op, err))
	}
	return nil
}

// Load sends the catalog. refs are every resource reference in it. vardir
// becomes the host's Puppet[:vardir]: the run's own, which Tier 3 type code
// writes its state under. logLevel is the run's level ("notice", "info" or
// "debug"); the host refuses any other.
func (h *Host) Load(catalog []byte, refs []string, libdir, vardir, logLevel string) (LoadResult, error) {
	known := make(map[string]struct{}, len(refs))
	types := map[string]struct{}{}
	for _, ref := range refs {
		known[ref] = struct{}{}
		if typ, _, ok := strings.Cut(ref, "["); ok {
			types[strings.ToLower(typ)] = struct{}{}
		}
	}
	var res LoadResult
	payload := map[string]any{"catalog": json.RawMessage(catalog), "libdir": libdir, "vardir": vardir,
		"log_level": logLevel}
	if err := h.call("load", payload, h.timeouts.Other, &res); err != nil {
		return LoadResult{}, err
	}
	h.known = known
	if err := h.checkLoad(res, refs, types); err != nil {
		h.known = nil
		return LoadResult{}, h.fail(fmt.Errorf("load: %w", err))
	}
	return res, nil
}

func (h *Host) checkLoad(res LoadResult, refs []string, types map[string]struct{}) error {
	if err := h.checkGeneration(res.Generated); err != nil {
		return err
	}
	// The generation's refs join the known set only now that it is valid; a
	// refused load kills the host and discards h.known.
	for _, r := range res.Generated.Resources {
		types[strings.ToLower(r.Type)] = struct{}{}
	}
	h.addGenerated(res.Generated)
	for typ, info := range res.Types {
		if _, ok := types[typ]; !ok {
			return fmt.Errorf("type %q is not in the catalog", typ)
		}
		switch {
		case info.Class == "missing" && info.Origin == "":
		case (info.Class == "tier3" || info.Class == "tier2") && (info.Origin == "core" || info.Origin == "libdir"):
		default:
			return fmt.Errorf("type %q has class %q and origin %q", typ, info.Class, info.Origin)
		}
		// Only Puppet::Type.newtype takes self_refresh; register_type never
		// passes it, and a missing type has no class to carry it.
		if info.SelfRefresh && info.Class != "tier3" {
			return fmt.Errorf("type %q has class %q and self_refresh", typ, info.Class)
		}
	}
	aliased := map[string]string{}
	retitled := map[string]string{}
	h.aliases = map[string]struct{}{}
	for ref, info := range res.Resources {
		if err := h.checkRef(ref); err != nil {
			return err
		}
		if err := h.checkAliases(ref, info.Aliases, aliased); err != nil {
			return err
		}
		if err := h.checkPuppetRef(ref, info, retitled); err != nil {
			return err
		}
	}
	// After the loop, as res.Resources is visited in no order: a ref that is
	// both an alias and a Puppet ref would name two resources.
	for puppetRef, ref := range retitled {
		if other, taken := aliased[puppetRef]; taken {
			return fmt.Errorf("%s has the Puppet ref %s, which %s has as an alias", ref, puppetRef, other)
		}
	}
	for _, r := range res.Invalid {
		if err := h.checkRef(r.Ref); err != nil {
			return err
		}
	}
	if err := h.checkOrder(res.Order, refs, len(res.Invalid) == 0); err != nil {
		return err
	}
	if err := checkLogs(res.Logs); err != nil {
		return err
	}
	return checkLogs(res.GenerateLogs)
}

// checkOrder requires order to name known refs, generated ones included, each
// once, and the catalog's own refs in refs' order: Puppet inserts generated
// resources beside their parents and never moves another
// (resource/catalog.rb:99-124). Complete, it must name every known ref. A
// refused load's order is not complete, as the host's catalog holds no
// resource whose conversion failed, and a refused run never starts.
func (h *Host) checkOrder(order, refs []string, complete bool) error {
	position := make(map[string]int, len(refs))
	for i, ref := range refs {
		position[ref] = i
	}
	seen := make(map[string]struct{}, len(order))
	last, lastRef := -1, ""
	for _, ref := range order {
		if _, ok := h.known[ref]; !ok {
			return fmt.Errorf("the order names %s, which is not in the catalog", ref)
		}
		if _, dup := seen[ref]; dup {
			return fmt.Errorf("the order names %s twice", ref)
		}
		seen[ref] = struct{}{}
		i, catalogRef := position[ref]
		if !catalogRef {
			continue
		}
		if i < last {
			return fmt.Errorf("the order names %s after %s, which the catalog has after it", ref, lastRef)
		}
		last, lastRef = i, ref
	}
	if !complete || len(seen) == len(h.known) {
		return nil
	}
	var missing []string
	for ref := range h.known {
		if _, ok := seen[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	slices.Sort(missing)
	return fmt.Errorf("the order omits %s", strings.Join(missing, ", "))
}

// splitRef splits a well-formed "Type[title]" reference.
func splitRef(ref string) (typ, title string, ok bool) {
	typ, rest, found := strings.Cut(ref, "[")
	title, closed := strings.CutSuffix(rest, "]")
	return typ, title, found && closed && typ != "" && title != ""
}

// checkAliases requires each alias of ref to be a well-formed ref of ref's own
// type, neither a known ref nor another resource's alias. aliased maps the
// aliases seen so far to their resource.
func (h *Host) checkAliases(ref string, aliases []string, aliased map[string]string) error {
	refType, _, _ := splitRef(ref)
	for _, alias := range aliases {
		typ, _, ok := splitRef(alias)
		if !ok || typ != refType {
			return fmt.Errorf("%s has the alias %q, which is not a reference of its type", ref, alias)
		}
		if _, known := h.known[alias]; known {
			return fmt.Errorf("%s has the alias %s, which is a resource", ref, alias)
		}
		if other, taken := aliased[alias]; taken {
			return fmt.Errorf("%s has the alias %s, which %s has too", ref, alias, other)
		}
		aliased[alias] = ref
		h.aliases[alias] = struct{}{}
	}
	return nil
}

// checkPuppetRef requires Puppet's own ref for ref, when the host names one,
// to be a well-formed ref of ref's own type titled by its PuppetTitle, other
// than ref, neither a known ref nor another resource's Puppet ref. Only an
// override makes a resource unresolvable. retitled maps the Puppet refs seen
// so far to their resource.
func (h *Host) checkPuppetRef(ref string, info ResourceInfo, retitled map[string]string) error {
	if info.PuppetRef == "" {
		switch {
		case info.PuppetTitle != "":
			return fmt.Errorf("%s has the Puppet title %q and no Puppet ref", ref, info.PuppetTitle)
		case info.Unresolvable:
			return fmt.Errorf("%s is unresolvable without a Puppet ref", ref)
		}
		return nil
	}
	refType, _, _ := splitRef(ref)
	typ, title, ok := splitRef(info.PuppetRef)
	if !ok || typ != refType {
		return fmt.Errorf("%s has the Puppet ref %q, which is not a reference of its type", ref, info.PuppetRef)
	}
	if title != info.PuppetTitle {
		return fmt.Errorf("%s has the Puppet ref %s and the Puppet title %q", ref, info.PuppetRef, info.PuppetTitle)
	}
	if info.PuppetRef == ref {
		return fmt.Errorf("%s has its own catalog ref as its Puppet ref", ref)
	}
	if _, known := h.known[info.PuppetRef]; known {
		return fmt.Errorf("%s has the Puppet ref %s, which is a resource", ref, info.PuppetRef)
	}
	if other, taken := retitled[info.PuppetRef]; taken {
		return fmt.Errorf("%s has the Puppet ref %s, which %s has too", ref, info.PuppetRef, other)
	}
	retitled[info.PuppetRef] = ref
	return nil
}

// checkGeneration validates a generation against the known refs. Every ref it
// names must be known or added by the generation itself, never looked up.
func (h *Host) checkGeneration(g Generation) error {
	added := make(map[string]struct{}, len(g.Resources))
	for _, r := range g.Resources {
		ref := r.Type + "[" + r.Title + "]"
		switch {
		case r.Type == "" || r.Title == "":
			return fmt.Errorf("a generated resource has no type or no title: %+v", r)
		case strings.ContainsAny(r.Type, "[]"):
			return fmt.Errorf("a generated resource has the type %q", r.Type)
		case !canonicalType(r.Type):
			return fmt.Errorf("a generated resource has the type %q, which is not capitalised as Puppet renders a type", r.Type)
		case r.Kind != "compilable_type" || r.Exported:
			return fmt.Errorf("generated %s has kind %q and exported %v", ref, r.Kind, r.Exported)
		}
		if _, known := h.known[ref]; known {
			return fmt.Errorf("generated %s is already in the catalog", ref)
		}
		if _, dup := added[ref]; dup {
			return fmt.Errorf("%s is generated twice", ref)
		}
		if _, aliased := h.aliases[ref]; aliased {
			return fmt.Errorf("generated %s is an alias of a resource in the catalog", ref)
		}
		added[ref] = struct{}{}
	}
	named := func(ref string) bool {
		if _, ok := h.known[ref]; ok {
			return true
		}
		_, ok := added[ref]
		return ok
	}
	for ref, container := range g.Containers {
		if _, ok := added[ref]; !ok {
			return fmt.Errorf("%s has a container but was not generated", ref)
		}
		if !named(container) {
			return fmt.Errorf("%s is contained by %s, which is not in the catalog", ref, container)
		}
	}
	for ref, generator := range g.Generators {
		if _, ok := added[ref]; !ok {
			return fmt.Errorf("%s has a generator but was not generated", ref)
		}
		if !named(generator) {
			return fmt.Errorf("%s was generated by %s, which is not in the catalog", ref, generator)
		}
	}
	for _, edge := range g.Before {
		for _, ref := range []string{edge.Source, edge.Target} {
			if !named(ref) {
				return fmt.Errorf("a generated before edge names %s, which is not in the catalog", ref)
			}
		}
	}
	for _, o := range g.Overrides {
		if _, ok := h.known[o.Ref]; !ok {
			return fmt.Errorf("an override names %s, which is not in the catalog", o.Ref)
		}
		for _, name := range o.Sensitive {
			if _, ok := o.Parameters[name]; !ok {
				return fmt.Errorf("the override of %s marks %q sensitive but does not change it", o.Ref, name)
			}
		}
	}
	return nil
}

// canonicalType reports whether typ is capitalised as Puppet::Resource renders
// a type in a ref: each "::" segment non-empty, its first character upper case
// and the rest lower case (String#capitalize). Refs are compared byte for byte,
// so a generated file[/x] would otherwise pass beside the catalog's File[/x].
func canonicalType(typ string) bool {
	for seg := range strings.SplitSeq(typ, "::") {
		first, size := utf8.DecodeRuneInString(seg)
		if seg == "" || first != unicode.ToUpper(first) || seg[size:] != strings.ToLower(seg[size:]) {
			return false
		}
	}
	return true
}

func (h *Host) addGenerated(g Generation) {
	for _, r := range g.Resources {
		h.known[r.Type+"["+r.Title+"]"] = struct{}{}
	}
}

// EvalGenerate runs the resource's eval_generate and reports what it added.
func (h *Host) EvalGenerate(ref string) (EvalGeneration, error) {
	if err := h.checkRef(ref); err != nil {
		return EvalGeneration{}, err
	}
	var res EvalGeneration
	if err := h.call("eval_generate", map[string]any{"ref": ref}, h.timeouts.Evaluate, &res); err != nil {
		return EvalGeneration{}, err
	}
	if err := h.checkGeneration(res.Generation); err != nil {
		return EvalGeneration{}, h.fail(fmt.Errorf("eval_generate %s: %w", ref, err))
	}
	for _, existing := range res.Existing {
		if err := h.checkRef(existing); err != nil {
			return EvalGeneration{}, h.fail(fmt.Errorf("eval_generate %s: %w", ref, err))
		}
	}
	if err := checkLogs(res.Logs); err != nil {
		return EvalGeneration{}, h.fail(fmt.Errorf("eval_generate %s: %w", ref, err))
	}
	h.addGenerated(res.Generation)
	return res, nil
}

func (h *Host) checkRef(ref string) error {
	if _, ok := h.known[ref]; !ok {
		return fmt.Errorf("%s is not in the catalog", ref)
	}
	return nil
}

var puppetLevels = map[string]struct{}{
	"debug": {}, "info": {}, "notice": {}, "warning": {}, "err": {}, "alert": {}, "emerg": {}, "crit": {},
}

func checkLogs(logs []Log) error {
	for _, l := range logs {
		if _, ok := puppetLevels[l.Level]; !ok {
			return fmt.Errorf("log level %q", l.Level)
		}
		if l.Line < 0 {
			return fmt.Errorf("log line %d", l.Line)
		}
	}
	return nil
}

var eventStatuses = map[string]struct{}{"success": {}, "failure": {}, "noop": {}, "audit": {}}

func checkEvents(events []report.Event) error {
	for _, e := range events {
		if _, ok := eventStatuses[e.Status]; !ok {
			return fmt.Errorf("event status %q", e.Status)
		}
		if _, ok := e.Property.(string); e.Property != nil && !ok {
			return fmt.Errorf("event property %v is not a string", e.Property)
		}
		if _, ok := e.Redacted.(bool); e.Redacted != nil && !ok {
			return fmt.Errorf("event redacted %v is not a bool", e.Redacted)
		}
		if e.Name == nil || e.Time == "" {
			return errors.New("an event has no name or no time")
		}
	}
	return nil
}

// Relationships asks for the auto-relations of refs.
func (h *Host) Relationships(refs []string) (map[string]Relations, error) {
	for _, ref := range refs {
		if err := h.checkRef(ref); err != nil {
			return nil, err
		}
	}
	var res struct {
		Relations map[string]Relations `json:"relations"`
		Logs      []Log                `json:"logs"`
	}
	if err := h.call("relationships", map[string]any{"refs": refs}, h.timeouts.Other, &res); err != nil {
		return nil, err
	}
	if len(res.Relations) != len(refs) {
		return nil, h.fail(fmt.Errorf("relationships: asked for %d refs, answered %d", len(refs), len(res.Relations)))
	}
	for _, ref := range refs {
		rel, ok := res.Relations[ref]
		if !ok {
			return nil, h.fail(fmt.Errorf("relationships: no answer for %s", ref))
		}
		for _, list := range [][]string{rel.Require, rel.Before, rel.Subscribe, rel.Notify} {
			for _, target := range list {
				if err := h.checkRef(target); err != nil {
					return nil, h.fail(fmt.Errorf("relationships of %s: %w", ref, err))
				}
			}
		}
	}
	if err := checkLogs(res.Logs); err != nil {
		return nil, h.fail(fmt.Errorf("relationships: %w", err))
	}
	return res.Relations, nil
}

// Evaluate evaluates one resource with Puppet's ResourceHarness.
func (h *Host) Evaluate(ref string, noop bool) (Evaluation, error) {
	ev, err := h.evaluation("evaluate", map[string]any{"ref": ref, "noop": noop}, ref, h.timeouts.Evaluate)
	if err == nil {
		h.stats.Evaluated++
	}
	return ev, err
}

// Skip runs what Puppet still runs for a resource skipped for failed
// dependencies: its provider class's prefetch.
func (h *Host) Skip(ref string) (Evaluation, error) {
	return h.evaluation("skip", map[string]any{"ref": ref}, ref, h.timeouts.Evaluate)
}

func (h *Host) evaluation(op string, payload any, ref string, timeout time.Duration) (Evaluation, error) {
	if err := h.checkRef(ref); err != nil {
		return Evaluation{}, err
	}
	var ev Evaluation
	if err := h.call(op, payload, timeout, &ev); err != nil {
		return Evaluation{}, err
	}
	if ev.Unsuitable && (!ev.Failed || ev.Skipped || len(ev.Events) > 0) {
		return Evaluation{}, h.fail(fmt.Errorf("%s %s: an unsuitable resource is failed, not skipped, with no events", op, ref))
	}
	if err := checkEvents(ev.Events); err != nil {
		return Evaluation{}, h.fail(fmt.Errorf("%s %s: %w", op, ref, err))
	}
	if err := checkLogs(ev.Logs); err != nil {
		return Evaluation{}, h.fail(fmt.Errorf("%s %s: %w", op, ref, err))
	}
	return ev, nil
}

// OverlyDeferred is Puppet's handling of a resource with no suitable provider
// that the walk could not place (transaction.rb:124-136), and returns the
// lines it logged.
func (h *Host) OverlyDeferred(ref string) ([]Log, error) {
	if err := h.checkRef(ref); err != nil {
		return nil, err
	}
	var res struct {
		Logs []Log `json:"logs"`
	}
	if err := h.call("overly_deferred", map[string]any{"ref": ref}, h.timeouts.Other, &res); err != nil {
		return nil, err
	}
	if err := checkLogs(res.Logs); err != nil {
		return nil, h.fail(fmt.Errorf("overly_deferred %s: %w", ref, err))
	}
	return res.Logs, nil
}

// Refresh calls the resource's refresh callback.
func (h *Host) Refresh(ref string) (RefreshResult, error) {
	if err := h.checkRef(ref); err != nil {
		return RefreshResult{}, err
	}
	var res RefreshResult
	if err := h.call("refresh", map[string]any{"ref": ref}, h.timeouts.Evaluate, &res); err != nil {
		return RefreshResult{}, err
	}
	switch {
	case !res.Refreshed && res.Error == nil:
		return RefreshResult{}, h.fail(fmt.Errorf("refresh %s: the reply neither refreshed nor failed", ref))
	case res.Refreshed && res.Error != nil:
		return RefreshResult{}, h.fail(fmt.Errorf("refresh %s: the reply both refreshed and failed", ref))
	case res.Error == nil && len(res.ExceptionLogs) > 0:
		return RefreshResult{}, h.fail(fmt.Errorf("refresh %s: exception lines without an error", ref))
	}
	for _, logs := range [][]Log{res.Logs, res.ExceptionLogs} {
		if err := checkLogs(logs); err != nil {
			return RefreshResult{}, h.fail(fmt.Errorf("refresh %s: %w", ref, err))
		}
	}
	return res, nil
}

// Finish runs post_resource_eval for the provider classes the run touched.
func (h *Host) Finish() ([]Log, error) {
	var res struct {
		Logs []Log `json:"logs"`
	}
	if err := h.call("finish", struct{}{}, h.timeouts.Other, &res); err != nil {
		return nil, err
	}
	if err := checkLogs(res.Logs); err != nil {
		return nil, h.fail(fmt.Errorf("finish: %w", err))
	}
	return res.Logs, nil
}

// Close shuts the host down: shutdown when it is healthy, a kill otherwise,
// then wait, then remove its directory. It is idempotent.
func (h *Host) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	var errs []error
	if h.dead == nil {
		var res struct {
			Logs []Log `json:"logs"`
		}
		if err := h.call("shutdown", struct{}{}, h.timeouts.Other, &res); err != nil {
			errs = append(errs, h.fail(err))
		}
	}
	_ = h.stdin.Close()
	waited := make(chan error, 1)
	go func() { waited <- h.cmd.Wait() }()
	timer := time.NewTimer(h.timeouts.Other)
	defer timer.Stop()
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-timer.C:
		errs = append(errs, h.fail(fmt.Errorf("the host did not exit within %s of shutdown", h.timeouts.Other)))
		waitErr = <-waited
	}
	// A killed host's exit status is the kill, already reported as h.dead.
	if h.dead == nil && waitErr != nil {
		errs = append(errs, fmt.Errorf("extension host exit: %w", waitErr))
	}
	if h.dir != "" {
		if err := os.RemoveAll(h.dir); err != nil {
			errs = append(errs, fmt.Errorf("remove the extension host directory: %w", err))
		}
	}
	return errors.Join(errs...)
}
