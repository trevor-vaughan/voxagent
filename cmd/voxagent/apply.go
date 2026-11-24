package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"time"

	"github.com/voxagent/voxagent/internal/agent"
	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/node"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/transaction"
)

// applyOptions is everything applyCatalog needs beyond the catalog bytes.
type applyOptions struct {
	noop, noRuby                    bool
	reportDir, ruby, libdir, vardir string
	timeouts                        extension.Timeouts
	certname                        string                // "" -> node.Certname()
	transactionUUID                 string                // "" -> the one report.New generates
	sources                         map[string]string     // ral.File.Sources: puppet: source -> local copy
	runLogs                         []report.LogEntry     // the agent's pre-apply logs, first in the report
	command                         string                // "apply" or "agent", for the run-start debug line
	level                           string                // "notice", "info" or "debug": what the console and report keep
	logger                          *runlog.Logger        // nil -> a notice logger with no console
	start                           time.Time             // an agent run's start; zero for apply, which times no agent phases
	phases                          []agent.Phase         // an agent run's phases timed before applying
	console                         func(report.LogEntry) // prints a Puppet console line; nil prints none
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
	// Before the host starts: loading the catalog, its resolve_and_replace
	// would resolve or defer a Deferred value, which voxagent refuses (D-51).
	if gaps := ral.DeferredGaps(cat); len(gaps) > 0 {
		for _, g := range gaps {
			fmt.Fprintf(os.Stderr, "voxagent: %s\n", g)
		}
		fmt.Fprintf(os.Stderr, "voxagent: %d Deferred value(s); nothing applied\n", len(gaps))
		return report.Status{CouldNotRun: true}, nil
	}

	// File names the catalog's environment in a missing source's message
	// (type/file/source.rb:208).
	natives := []ral.Provider{ral.Notify{}, ral.File{Environment: cat.Environment, Sources: o.sources, Vardir: o.vardir}, ral.Exec{}, ral.Package{}, ral.Service{}}
	ext, errMsg := ingestExtensions(extensionConfig{ruby: o.ruby, libdir: o.libdir, vardir: o.vardir, level: o.level, disabled: o.noRuby,
		timeouts: o.timeouts}, raw, cat, natives)
	// done closes the host before the caller's outcome line, which must stay
	// the last stderr line, so every return from here on goes through it.
	done := func(st report.Status) report.Status {
		if ext.host != nil {
			if err := ext.host.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "voxagent: extension host: %v\n", err)
			}
		}
		return st
	}
	if errMsg != "" {
		fmt.Fprintln(os.Stderr, errMsg)
		return done(report.Status{CouldNotRun: true}), nil
	}
	registry := ral.NewRegistry(append(natives, ext.providers...)...)

	// Pre-flight: refuse the whole catalog rather than applying part of it. A
	// partially applied node produces state-diff divergences that are artifacts
	// of coverage rather than defects, which would poison the oracle.
	if gaps := ral.Preflight(registry, cat); len(gaps) > 0 {
		resources := map[string]struct{}{}
		for _, g := range gaps {
			fmt.Fprintf(os.Stderr, "voxagent: %s\n", g)
			resources[g.Ref] = struct{}{}
		}
		fmt.Fprintf(os.Stderr, "voxagent: %d gap(s) across %d resource(s); nothing applied\n",
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
			fmt.Fprintf(os.Stderr, "voxagent: %s\n", i)
		}
		fmt.Fprintf(os.Stderr, "voxagent: %d invalid resource(s); nothing applied\n", len(invalid))
		return done(report.Status{CouldNotRun: true}), nil
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
				o.console(e)
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
			fmt.Fprintf(os.Stderr, "voxagent: cannot determine this node's certname: %v\n", err)
			return done(report.Status{CouldNotRun: true}), nil
		}
	}

	rep := report.New(certname, cat.Environment, o.noop)
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
	rep.Observe(o.console)
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
		if ext.host != nil {
			runlog.Debugf(ctx, "voxagent", "extension host: started in %s", ext.host.Stats().Startup.Round(time.Millisecond))
			runlog.Debugf(ctx, "voxagent", "extension host: load in %s", ext.loadTook.Round(time.Millisecond))
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
		fmt.Fprintf(os.Stderr, "voxagent: %v; nothing applied\n", err)
		return done(report.Status{CouldNotRun: true}), nil
	}
	tx := &transaction.Transaction{Registry: registry, Noop: o.noop}
	if err := tx.Run(ctx, cat, g, rep); err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "voxagent: extension host: finish failed: %v\n", err)
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
			fmt.Fprintf(os.Stderr,
				"voxagent: the catalog was applied but the report could not be written: %v\n", err)
			fmt.Fprintln(os.Stderr,
				"voxagent: the machine WAS modified; this run cannot say how")
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
