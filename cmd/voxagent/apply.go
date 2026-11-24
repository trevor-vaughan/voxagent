package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/voxagent/voxagent/internal/agent"
	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/plugintree"
	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/node"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/transaction"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units"
)

// applyOptions is everything applyCatalog needs beyond the catalog bytes.
type applyOptions struct {
	noop, noRuby                    bool
	reportDir, ruby, libdir, vardir string
	timeouts                        extension.Timeouts
	certname                        string            // "" -> node.Certname()
	transactionUUID                 string            // "" -> the one report.New generates
	sources                         map[string]string // file.File.Sources: puppet: source -> local copy
	runLogs                         []report.LogEntry // the agent's pre-apply logs, first in the report
	command                         string            // "apply" or "agent", for the run-start debug line
	level                           string            // "notice", "info" or "debug": what the console and report keep
	logger                          *runlog.Logger    // nil -> a notice logger with no console
	start                           time.Time         // an agent run's start; zero for apply, which times no agent phases
	phases                          []agent.Phase     // an agent run's phases timed before applying
	console                         *puppetConsole    // prints Puppet console lines; nil prints none
	providers                       []ral.Provider    // joins the native providers; tests only
	augeasProbe                     augeas.Probe      // nil -> augeas.HostProbe; tests only
	environ                         ral.Environ       // nil -> os.Getenv; tests only; LookupEnv then reads it, empty meaning unset
	stderr                          io.Writer         // set by applyCatalog to the scrubbed stderr; a caller of nativeProviders alone gets os.Stderr
}

// scrubbedStderr writes to the process's stderr through the run's current
// scrubber, so a native provider that copies a command's output there cannot
// show a Sensitive value.
type scrubbedStderr struct {
	scrubber *atomic.Pointer[redact.Scrubber]
}

func (w scrubbedStderr) Write(b []byte) (int, error) {
	if _, err := io.WriteString(os.Stderr, w.scrubber.Load().Scrub(string(b))); err != nil {
		return 0, err
	}
	return len(b), nil
}

// nativeProviders lists the providers voxagent applies itself for cat: each
// unit's providers (units.All), over one libaugeas probe the augeas units
// share. delegated holds, per type the catalog holds that goes to the
// extension host, why.
func nativeProviders(cat *catalog.Catalog, o applyOptions, p ral.Platform) (natives []ral.Provider, delegated map[string]string) {
	environ := o.environ
	if environ == nil {
		environ = os.Getenv
	}
	lookup := os.LookupEnv
	if o.environ != nil {
		lookup = func(key string) (string, bool) { v := o.environ(key); return v, v != "" }
	}
	tree, session := plugintree.LibdirTree(o.libdir), augeas.NewSession(o.augeasProbe)
	env := &unit.Env{Catalog: cat, Libdir: o.libdir, Tree: tree, Platform: p, Environ: environ, Command: o.command,
		Vardir: o.vardir, Sources: o.sources, Augeas: session, LookupEnv: lookup, Stderr: o.stderr}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	decision := unit.RouteAll(env, units.All())
	return append(decision.Providers, o.providers...), decision.Delegated
}

// applyCatalog applies raw (exact catalog bytes) and returns the run's status
// and, once the transaction has started, its finalized report -- even when the
// run is could-not-run, because the machine may have been modified and an
// agent run must still send what it knows. It is nil when the run was refused
// before applying anything. The caller prints the outcome line with finish,
// after anything else it has to say.
func applyCatalog(raw []byte, o applyOptions) (report.Status, *report.Report) {
	converting := time.Now()
	var err error
	cat, err := catalog.Decode(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v\n", err)
		return report.Status{CouldNotRun: true}, nil
	}
	// Every line from here on may carry a catalog value. rescrub rebuilds the
	// scrubber once the host or a native generator has added resources;
	// current hands each rebuild to the host's stderr, which another
	// goroutine copies.
	var sc *redact.Scrubber
	var current atomic.Pointer[redact.Scrubber]
	rescrub := func() {
		sc = redact.New(cat)
		current.Store(sc)
		if o.console != nil {
			o.console.scrub = sc
		}
	}
	rescrub()
	// Before the host starts: loading the catalog, its resolve_and_replace
	// would resolve or defer a Deferred value, which voxagent refuses (D-51).
	if gaps := ral.DeferredGaps(cat); len(gaps) > 0 {
		for _, g := range gaps {
			stderrf(sc, "voxagent: %s\n", g)
		}
		stderrf(sc, "voxagent: %d Deferred value(s); nothing applied\n", len(gaps))
		return report.Status{CouldNotRun: true}, nil
	}

	o.stderr = scrubbedStderr{&current}
	natives, delegated := nativeProviders(cat, o, ral.HostPlatform)
	ext, errMsg := ingestExtensions(extensionConfig{ruby: o.ruby, libdir: o.libdir, vardir: o.vardir, level: o.level, disabled: o.noRuby,
		timeouts: o.timeouts, scrubber: &current}, raw, cat, natives)
	rescrub()
	// done closes the host before the caller's outcome line, which must stay
	// the last stderr line, so every return from here on goes through it.
	done := func(st report.Status) report.Status {
		if ext.host != nil {
			if err := ext.host.Close(); err != nil {
				stderrf(sc, "voxagent: extension host: %v\n", err)
			}
		}
		return st
	}
	if errMsg != "" {
		stderrf(sc, "%s\n", errMsg)
		return done(report.Status{CouldNotRun: true}), nil
	}
	registry := ral.NewRegistry(append(natives, ext.providers...)...)

	// Pre-flight: refuse the whole catalog rather than applying part of it. A
	// partially applied node produces state-diff divergences that are artifacts
	// of coverage rather than defects, which would poison the oracle.
	if gaps := ral.Preflight(registry, cat); len(gaps) > 0 {
		resources := map[string]struct{}{}
		for _, g := range gaps {
			stderrf(sc, "voxagent: %s\n", g)
			resources[g.Ref] = struct{}{}
		}
		stderrf(sc, "voxagent: %d gap(s) across %d resource(s); nothing applied\n",
			len(gaps), len(resources))
		return done(report.Status{CouldNotRun: true}), nil
	}

	// Validation: refuse what Puppet itself refuses while converting the
	// catalog -- a relative file path fails Ruby's whole run with "Could not
	// run". It runs after pre-flight so an operator sees coverage gaps, the
	// actionable fact, first.
	invalid := ral.Validate(registry, cat)
	named := make(map[string]struct{}, len(invalid))
	for _, i := range invalid {
		named[i.Ref] = struct{}{}
	}
	for _, i := range ext.invalid {
		if _, dup := named[i.Ref]; !dup {
			invalid = append(invalid, ral.Invalid{Ref: i.Ref, Err: fmt.Errorf("%s: %s", i.Ref, i.Message)})
		}
	}
	sort.SliceStable(invalid, func(a, b int) bool { return invalid[a].Ref < invalid[b].Ref })
	if len(invalid) > 0 {
		for _, i := range invalid {
			stderrf(sc, "voxagent: %s\n", i)
		}
		stderrf(sc, "voxagent: %d invalid resource(s); nothing applied\n", len(invalid))
		return done(report.Status{CouldNotRun: true}), nil
	}

	// Load-time generation the host did not run (spec §0): Ruby's generate ran
	// on every vertex when the host loaded the catalog, so only without it do
	// native generators build their resources, judged like the catalog's own.
	if !ext.hostLoaded {
		if loadGenerate(cat, registry, sc) {
			return done(report.Status{CouldNotRun: true}), nil
		}
		rescrub()
	}

	// configurer.rb's convert_catalog: Puppet's to_ral validates types and
	// providers, as decoding, the extension host's load, pre-flight and
	// validation do here.
	converted := time.Since(converting)

	// Puppet logs a mark it cannot apply while converting the catalog, to the
	// console only -- the lines are not in its report.
	if o.console != nil {
		for _, r := range cat.Resources {
			if ral.IsContainerResource(r) {
				continue
			}
			p, ok := registry.Provider(r.Type)
			if !ok {
				continue
			}
			_, logs := ral.MarkSensitive(p, r)
			for _, e := range logs {
				o.console.print(e)
			}
		}
	}

	// The report's host is the certname of the machine that applied the catalog,
	// not the node name the catalog was compiled for. Measured: Ruby does the
	// same, reporting Puppet[:certname] even when handed another node's catalog.
	// Using cat.Name instead would be more descriptive of the catalog and would
	// file voxagent's reports under a different key than Ruby's for the same
	// machine, which is the kind of "more informative" that breaks a drop-in.
	certname := o.certname
	if certname == "" {
		certname, err = node.Certname()
		if err != nil {
			stderrf(sc, "voxagent: cannot determine this node's certname: %v\n", err)
			return done(report.Status{CouldNotRun: true}), nil
		}
	}

	rep := report.New(certname, cat.Environment, o.noop)
	rep.SetScrubber(sc.Scrub)
	rep.CatalogUUID = cat.CatalogUUID
	rep.ConfigurationVersion = cat.Version
	if cat.CodeID != nil && *cat.CodeID != "" {
		rep.CodeID = cat.CodeID
	}
	if o.transactionUUID != "" {
		rep.TransactionUUID = o.transactionUUID
	}
	if !o.start.IsZero() {
		rep.StartedAt(o.start)
		for _, ph := range o.phases {
			rep.AddTime(ph.Name, ph.Duration)
		}
		rep.AddTime("convert_catalog", converted)
	}
	rep.AddLog(o.runLogs...)
	// The console prints what the report records from here on; the agent's
	// pre-apply lines in o.runLogs were printed as they were logged.
	if o.console != nil {
		rep.Observe(o.console.print)
	}
	logger := o.logger
	if logger == nil {
		logger = runlog.New("notice", nil, nil)
	}
	logger.Redirect(func(e report.LogEntry) { rep.AddLog(e) })
	ctx := runlog.WithLogger(context.Background(), logger)
	codeID := "none"
	if rep.CodeID != nil {
		codeID = *rep.CodeID
	}
	runlog.Debugf(ctx, "voxagent", "voxagent %s (%s) %s noop=%t level=%s; catalog %d resources, code_id %s",
		moduleVersion(), runtime.Version(), o.command, o.noop, o.level, len(cat.Resources), codeID)
	if runlog.From(ctx).Enabled("debug") {
		// Pre-flight and the host's start ran before the logger had a report to
		// record into, so what they decided is logged here.
		for _, r := range cat.Resources {
			if ral.IsContainer(r.Type) {
				runlog.Debugf(ctx, "voxagent", "%s: container (type %s)", r.Ref(), r.Type)
			} else if ral.IsContainerResource(r) {
				runlog.Debugf(ctx, "voxagent", "%s: container (kind %s)", r.Ref(), r.Kind)
			}
		}
		for _, typ := range slices.Sorted(maps.Keys(delegated)) {
			runlog.Debugf(ctx, "voxagent", "%s applied via extension host: %s", typ, delegated[typ])
		}
		if ext.host != nil {
			runlog.Debugf(ctx, "voxagent", "extension host: started in %s", ext.host.Stats().Startup.Round(time.Millisecond))
			runlog.Debugf(ctx, "voxagent", "extension host: load in %s", ext.loadTook.Round(time.Millisecond))
		}
	}

	// Puppet drops an attribute whose provider feature is missing while it
	// converts the catalog, and says so at the resource's path (type.rb:790).
	for _, r := range cat.Resources {
		p, ok := registry.Provider(r.Type)
		if !ok {
			continue
		}
		if cd, ok := p.(ral.ConversionDebugging); ok {
			for _, line := range cd.ConversionDebug(r) {
				runlog.Debugf(ctx, "/"+r.Ref(), "%s", line)
			}
		}
	}

	// Puppet generates inside Transaction#evaluate, before any resource is
	// evaluated (transaction.rb:99-100), so these lines open the report's log.
	if ext.host != nil {
		runlog.AddLogs(ctx, rep, ral.ConvertLogs(ext.load.GenerateLogs)...)
	}
	// transaction.rb:106, at info: only a verbose run's report holds it, as
	// Puppet's report is a log destination at the run's level.
	if o.level != "notice" && cat.Version != nil {
		rep.Log("info", "Puppet", fmt.Sprintf("Applying configuration version '%s'", versionString(cat.Version)))
	}

	// Built here, after the version line, because Ruby logs its graph lines
	// from Transaction#evaluate, once the transaction has begun.
	g, err := transaction.BuildGraph(ctx, cat, registry)
	if err != nil {
		stderrf(sc, "voxagent: %v; nothing applied\n", err)
		return done(report.Status{CouldNotRun: true}), nil
	}
	tx := &transaction.Transaction{Registry: registry, Noop: o.noop}
	if err := tx.Run(ctx, cat, g, rep); err != nil {
		stderrf(sc, "voxagent: %v\n", err)
		rep.FinalizeIncomplete()
		return done(report.Status{CouldNotRun: true}), rep
	}
	if ext.host != nil {
		// post_resource_eval runs in the transaction's teardown, so Ruby's
		// report holds Finish's lines. A failed generate fails the run
		// (transaction.rb:197).
		rep.FailedToGenerate = rep.FailedToGenerate || ext.load.FailedToGenerate
		finishing := time.Now()
		logs, err := ext.host.Finish()
		runlog.Debugf(ctx, "voxagent", "extension host: finish in %s", time.Since(finishing).Round(time.Millisecond))
		runlog.AddLogs(ctx, rep, ral.ConvertLogs(logs)...)
		if err != nil {
			stderrf(sc, "voxagent: extension host: finish failed: %v\n", err)
		}
		selfReport(ext.host)
	}
	rep.Finalize()

	if o.reportDir != "" {
		if err := writeReport(o.reportDir, rep); err != nil {
			// The run applied changes and cannot say what it did. The claim a
			// run makes is its report, so with no report there is no claim --
			// but the message must say the machine was modified, because
			// "could not run" would otherwise imply nothing happened.
			stderrf(sc,
				"voxagent: the catalog was applied but the report could not be written: %v\n", err)
			stderrf(sc, "voxagent: the machine WAS modified; this run cannot say how\n")
			return done(report.Status{CouldNotRun: true}), rep
		}
	}

	return done(rep.RunStatus()), rep
}

// moduleVersion is the module version stamped into the binary, "(devel)" when
// the build recorded none.
func moduleVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// versionString renders a catalog version as Ruby's to_s does: a string as
// it is, a JSON number without exponent or trailing zeros.
func versionString(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
