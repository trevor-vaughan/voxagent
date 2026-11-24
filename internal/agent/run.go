package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/puppetapi"
)

// environmentRe restricts an environment, which is a query value here and a
// directory name on the server.
var environmentRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// RunConfig configures Prepare. SSL.Log receives Bootstrap's lines and the
// run's progress lines, which never carry fact values;
// SSL.Timeout bounds every request of the run. Environment is the requested
// environment (--environment); the server's node answer may change it.
// TransactionUUID is sent with the node and catalog requests; the report must
// carry the same one. Log, which may be nil, receives the lines Puppet's agent
// logs before applying, at Puppet's levels, for the report and the console.
// Start, when set, is when the command began; it opens startup_time.
type RunConfig struct {
	SSL                                    SSLConfig
	Environment                            string
	LibDir, PluginFactDest, VarDir, Facter string
	Noop                                   bool
	TransactionUUID                        string
	Log                                    LogFunc
	Start                                  time.Time
}

// Phase is one step of the run Puppet's agent times and reports under Name
// (configurer.rb's report.add_times).
type Phase struct {
	Name     string
	Duration time.Duration
}

// Prepared is a run ready to apply. Catalog is the catalog response's exact
// bytes; Environment is the one pluginsync and the catalog request used;
// Sources maps each puppet: source to its local copy, for file.File.Sources.
// Phases are the steps timed so far, in Puppet's order. Close removes the
// local copies.
type Prepared struct {
	Client      *puppetapi.Client
	Environment string
	Catalog     []byte
	Sources     map[string]string
	Phases      []Phase
	sourcesDir  string
}

// Close removes the run's sources directory.
func (p *Prepared) Close() error {
	return os.RemoveAll(p.sourcesDir)
}

// Prepare does everything a Puppet agent run does before applying, in
// Puppet's order: SSL bootstrap, the node request for the
// environment, pluginsync, facts, the catalog request, then the catalog's
// puppet: sources into a run-private directory under VarDir. Any error means
// nothing may be applied; nothing is left in VarDir but what
// pluginsync wrote.
//
// Before pluginsync, libdir, pluginfactdest, vardir and the facter binary must
// be trusted locations; a missing libdir or pluginfactdest
// is created 0755 under a trusted parent. A catalog for another environment
// than pluginsync's is refused: Puppet would pluginsync again for the
// new environment and retry; voxagent stops instead.
//
// Each step Puppet's agent times is timed the same way: startup_time up to the
// end of the bootstrap (Puppet::Agent#run waits for certificates before the
// configurer starts), then node_retrieval, plugin_sync, fact_generation and
// config_retrieval, which covers the request and decoding the catalog as
// Puppet's indirector find does. Fetching the puppet: sources is not a phase
// of Puppet's: it fetches them while applying.
func Prepare(ctx context.Context, c RunConfig) (*Prepared, error) {
	log := c.SSL.Log
	if log == nil {
		log = io.Discard
	}
	plog := c.Log
	if plog == nil {
		plog = func(string, string) {}
	}
	if !environmentRe.MatchString(c.Environment) {
		return nil, fmt.Errorf("environment %q must match %s", c.Environment, environmentRe)
	}
	client, err := Bootstrap(ctx, c.SSL)
	if err != nil {
		return nil, err
	}
	var phases []Phase
	mark := time.Now()
	if !c.Start.IsZero() {
		phases = append(phases, Phase{"startup_time", mark.Sub(c.Start)})
	}
	// timed records the phase that ran since mark and starts the next.
	timed := func(name string) {
		now := time.Now()
		phases = append(phases, Phase{name, now.Sub(mark)})
		mark = now
	}
	env, err := client.Node(ctx, c.SSL.Certname, c.Environment, c.TransactionUUID)
	if err != nil {
		return nil, err
	}
	timed("node_retrieval")
	if !environmentRe.MatchString(env) {
		return nil, fmt.Errorf("the server put %s in environment %q, which does not match %s", c.SSL.Certname, env, environmentRe)
	}
	fmt.Fprintf(log, "Using environment '%s'\n", env)
	plog("info", fmt.Sprintf("Using environment '%s'", env))

	for _, d := range []string{c.LibDir, c.PluginFactDest} {
		if err := ensureTrustedDir(d); err != nil {
			return nil, err
		}
	}
	for _, p := range []string{c.VarDir, c.Facter} {
		if err := checkTrusted(p); err != nil {
			return nil, err
		}
	}
	fmt.Fprintln(log, "Retrieving pluginfacts and plugins")
	mark = time.Now()
	if err := Pluginsync(ctx, client, env, c.LibDir, c.PluginFactDest, plog); err != nil {
		return nil, err
	}
	timed("plugin_sync")
	fmt.Fprintln(log, "Loading facts")
	plog("info", "Loading facts")
	facts, err := Facts(ctx, FactsConfig{
		Facter: c.Facter, Certname: c.SSL.Certname, LibDir: c.LibDir, PluginFactDest: c.PluginFactDest, Noop: c.Noop,
	})
	if err != nil {
		return nil, err
	}
	timed("fact_generation")
	fmt.Fprintln(log, "Retrieving catalog")
	resp, err := client.Catalog(ctx, c.SSL.Certname, puppetapi.CatalogForm{
		Environment: env, TransactionUUID: c.TransactionUUID, Facts: facts,
	})
	// Puppet logs both before it looks at the status (indirector/catalog/
	// rest.rb, http/service/compiler.rb).
	requesting := fmt.Sprintf("Requesting catalog from %s:%d", c.SSL.Server, c.SSL.Port)
	if resp.Peer != "" {
		requesting += " (" + resp.Peer + ")"
	}
	plog("notice", requesting)
	if resp.Compiler != "" {
		plog("notice", "Catalog compiled by "+resp.Compiler)
	}
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	cat, err := catalog.Decode(raw)
	if err != nil {
		return nil, err
	}
	timed("config_retrieval")
	if cat.Environment != env {
		return nil, fmt.Errorf("the server sent a catalog for environment %q, but plugins were synced for %q; nothing applied", cat.Environment, env)
	}

	dir, err := os.MkdirTemp(c.VarDir, "voxagent-sources-")
	if err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	sources, err := FetchSources(ctx, client, cat, env, c.SSL.Server, dir)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	return &Prepared{Client: client, Environment: env, Catalog: raw, Sources: sources, Phases: phases, sourcesDir: dir}, nil
}

// ensureTrustedDir makes dir exist as a directory in a trusted location: an
// existing dir must pass checkTrusted; a missing one is created 0755,
// set explicitly, once its parent passes checkTrusted.
func ensureTrustedDir(dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := checkTrusted(filepath.Dir(dir)); err != nil {
			return err
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return checkTrusted(dir)
}
