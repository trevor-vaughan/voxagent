package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/agent"
	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/extension"
	"github.com/voxagent/voxagent/internal/lib/redact"
	"github.com/voxagent/voxagent/internal/node"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

const agentUsage = "usage: voxagent agent [--server <host>] [--serverport <port>] [--certname <name>] [--environment <env>] [--ssldir <dir>] [--waitforcert <d>] [--ca-fingerprint <sha256>] [--http-timeout <d>] [--facter <path>] [--pluginfactdest <dir>] [--detailed-exitcodes] [--verbose] [--debug] [--noop] [--report-dir <dir>] [--ruby <path>] [--libdir <dir>] [--vardir <dir>] [--no-ruby] [--extension-timeout <d>] [--extension-evaluate-timeout <d>]"

// agentCommand is one `puppet agent --onetime --no-daemonize` run against an
// OpenVox server.
// Defaults are Puppet's compiled-in settings; puppet.conf is not read.
func agentCommand(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	server := fs.String("server", "puppet", "Puppet server host name; the server certificate must name it")
	port := fs.Int("serverport", 8140, "Puppet server port")
	certname := fs.String("certname", "", "this node's certificate name (default: the FQDN, lower case, as Puppet's)")
	environment := fs.String("environment", "production", "environment to request; the server's node classification may override it")
	ssldir := fs.String("ssldir", "/etc/puppetlabs/puppet/ssl", "directory holding the CA bundle, CRL, key and certificate")
	var waitForCert puppetDurationFlag
	fs.Var(&waitForCert, "waitforcert", "poll interval while the certificate is unsigned, in Puppet's syntax (seconds, or a number with s, m, h, d or y); 0 exits instead")
	caFingerprint := fs.String("ca-fingerprint", "", "SHA-256 of the CA root certificate's DER (hex, colons optional); not Puppet's ca_fingerprint, which digests the PEM bundle")
	httpTimeout := fs.Duration("http-timeout", 2*time.Minute, "time limit for each HTTP request to the server, including reading its response")
	facter := fs.String("facter", "/opt/puppetlabs/puppet/bin/facter", "facter binary (absolute path) that collects the facts sent with the catalog request")
	pluginfactdest := fs.String("pluginfactdest", "/opt/puppetlabs/puppet/cache/facts.d", "directory pluginsync fills with external facts")
	detailed := fs.Bool("detailed-exitcodes", false, "use Puppet's exit bitmask")
	verbose := fs.Bool("verbose", false, "also log info-level messages, to the console and the report")
	debug := fs.Bool("debug", false, "also log debug-level messages: all of them to the console, those within 16 MiB to the report; implies --verbose")
	noop := fs.Bool("noop", false, "report what would change without changing it")
	reportDir := fs.String("report-dir", "", "directory to also write the run report into")
	ruby := fs.String("ruby", "/opt/puppetlabs/puppet/bin/ruby", "Ruby interpreter for the extension host")
	libdir := fs.String("libdir", "/opt/puppetlabs/puppet/cache/lib", "directory pluginsync fills and the extension host loads types from")
	vardir := fs.String("vardir", "/opt/puppetlabs/puppet/cache", "Puppet's vardir: holds the run's downloaded puppet: sources and the clientbucket")
	noRuby := fs.Bool("no-ruby", false, "refuse catalogs that need the extension host")
	extTimeout := fs.Duration("extension-timeout", 5*time.Minute, "time limit for each extension host request other than evaluate")
	extEvalTimeout := fs.Duration("extension-evaluate-timeout", time.Hour, "time limit for each extension host evaluate request")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	// As in apply: an argument after which no flag was parsed is refused, not
	// ignored.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "voxagent: unexpected argument %q; no flag after it was parsed\n", fs.Arg(0))
		fmt.Fprintln(os.Stderr, agentUsage)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{{"--http-timeout", *httpTimeout}, {"--extension-timeout", *extTimeout}, {"--extension-evaluate-timeout", *extEvalTimeout}} {
		if d.value <= 0 {
			fmt.Fprintf(os.Stderr, "voxagent: %s must be positive\n", d.name)
			return finish(report.Status{CouldNotRun: true}, *detailed)
		}
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "voxagent: --serverport %d is not a TCP port\n", *port)
		return finish(report.Status{CouldNotRun: true}, *detailed)
	}
	if *certname == "" {
		name, err := node.Certname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "voxagent: cannot determine this node's certname: %v\n", err)
			return finish(report.Status{CouldNotRun: true}, *detailed)
		}
		*certname = name
	}

	txUUID := report.NewUUID()
	level := logLevel(*verbose, *debug)
	console := &puppetConsole{level: level, stdout: os.Stdout, stderr: os.Stderr}
	st := runAgent(console, agent.RunConfig{
		SSL: agent.SSLConfig{
			Server: *server, Port: *port, SSLDir: *ssldir, Certname: *certname,
			WaitForCert: time.Duration(waitForCert), CAFingerprint: *caFingerprint, Timeout: *httpTimeout,
			Log: &linePrefixer{w: os.Stderr},
		},
		Environment: *environment, LibDir: *libdir, PluginFactDest: *pluginfactdest, VarDir: *vardir,
		Facter: *facter, Noop: *noop, TransactionUUID: txUUID,
	}, applyOptions{
		command: "agent", level: level, logger: runlog.New(level, console.record, console.print),
		noop: *noop, noRuby: *noRuby,
		reportDir: *reportDir, ruby: *ruby, libdir: *libdir, vardir: *vardir,
		timeouts: extension.Timeouts{Evaluate: *extEvalTimeout, Other: *extTimeout},
		certname: *certname, transactionUUID: txUUID,
	})
	return finish(st, *detailed)
}

// processStart stands for Puppet's :start_time (application/agent.rb:43), set
// as the process initialises: startup_time runs from it, and so does total.
var processStart = time.Now()

// runAgent prepares the run, applies the catalog and submits the report. It
// returns before the caller prints the outcome line, so the sources
// directory's removal and a failed report submission are reported above it.
//
// SIGINT and SIGTERM cancel the run's context, so the deferred removal of the
// sources directory runs on that exit path too. A signal
// received before applyCatalog starts -- during Prepare or after it returns --
// ends the run could-not-run with nothing applied and no report. Applying is
// not interruptible (applyCatalog is shared with `apply`, which keeps its
// default signal behaviour): a signal received once it has started is held
// until the transaction ends, the report of what was applied is still sent,
// and a second signal kills the process.
func runAgent(console *puppetConsole, rc agent.RunConfig, o applyOptions) report.Status {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)
	ctx = runlog.WithLogger(ctx, o.logger)
	rc.Log = func(level, message string) { o.logger.Log(level, "Puppet", message) }
	rc.Start = processStart
	// The source fetch's lines and errors may carry a catalog value; applyCatalog
	// rebuilds the scrubber from the same catalog.
	rc.Decoded = func(cat *catalog.Catalog) { console.scrub = redact.New(cat) }
	p, err := agent.Prepare(ctx, rc)
	if err != nil {
		stderrf(console.scrub, "voxagent: %v\n", err)
		return report.Status{CouldNotRun: true}
	}
	defer func() {
		if err := p.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "voxagent: could not remove the run's sources directory: %v\n", err)
		}
	}()

	if err := ctx.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "voxagent: %v; nothing applied\n", context.Cause(ctx))
		return report.Status{CouldNotRun: true}
	}
	o.sources = p.Sources
	o.console = console
	o.runLogs = console.entries
	o.start, o.phases = processStart, p.Phases
	st, rep := applyCatalog(p.Catalog, o)
	if rep == nil {
		return st
	}
	// The encoding writeReport uses, so the server stores what --report-dir
	// would hold.
	body, err := json.MarshalIndent(rep, "", "  ")
	if err == nil {
		err = p.Client.Report(context.WithoutCancel(ctx), rep.Host, rep.Environment, body)
	}
	if err != nil {
		// Puppet logs this and keeps the run's status (configurer.rb
		// send_report): the catalog was applied either way.
		stderrf(console.scrub, "voxagent: Could not send report: %v\n", err)
	}
	return st
}

// linePrefixer starts every line written through it with "voxagent: ", so the
// agent package's progress lines read like the command's own.
type linePrefixer struct {
	w   io.Writer
	mid bool // the last byte written was not a newline
}

func (p *linePrefixer) Write(b []byte) (int, error) {
	out := make([]byte, 0, len(b)+len("voxagent: "))
	for _, c := range b {
		if !p.mid {
			out = append(out, "voxagent: "...)
		}
		out = append(out, c)
		p.mid = c != '\n'
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}
