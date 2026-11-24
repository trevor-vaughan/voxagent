package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
)

// runServer is the v3 node and catalog endpoints on top of sourceServer
// (plugin mounts, file_metadata, file_content), behind a fakeCA.
type runServer struct {
	*sourceServer
	mu            sync.Mutex
	nodeEnv       string // the node response's environment
	catalog       []byte // the catalog response body
	nodeQuery     string // the node request's raw query
	form          map[string]string
	order         []string // node, catalog and every file request, in order
	hang          bool     // the node request never answers
	catalogStatus int      // non-zero answers the catalog request with this status
}

func (s *runServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.order = append(s.order, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	switch {
	case r.URL.Path == "/puppet/v3/node/"+testCertname:
		if s.hang {
			<-r.Context().Done()
			return
		}
		s.mu.Lock()
		s.nodeQuery = r.URL.RawQuery
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"name": testCertname, "environment": s.nodeEnv})
	case r.Method == http.MethodPost && r.URL.Path == "/puppet/v3/catalog/"+testCertname:
		if s.catalogStatus != 0 {
			w.Header().Set("X-Puppet-Compiler-Name", "puppet")
			http.Error(w, `{"message":"Server Error: Evaluation Error"}`, s.catalogStatus)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.form = map[string]string{}
		for k := range r.PostForm {
			s.form[k] = r.PostForm.Get(k)
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.puppet.rich+json")
		w.Header().Set("X-Puppet-Compiler-Name", "puppet")
		w.Write(s.catalog)
	default:
		s.sourceServer.ServeHTTP(w, r)
	}
}

func (s *runServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

// runCatalog is a catalog in environment env with one File whose content
// comes from puppet:///modules/m/x.
func runCatalog(t *testing.T, env string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"catalog_format": 2, "name": testCertname, "environment": env, "version": 1,
		"catalog_uuid": "00000000-0000-4000-8000-000000000001",
		"resources": []any{map[string]any{
			"type": "File", "title": "/tmp/voxagent-run-test", "tags": []any{},
			"parameters": map[string]any{"ensure": "file", "source": "puppet:///modules/m/x"},
		}},
		"edges": []any{}, "tags": []any{}, "classes": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type runFixture struct {
	ca  *fakeCA
	srv *runServer
	cfg RunConfig
	log *bytes.Buffer
}

// newRunFixture serves a node in production with an empty plugin tree and one
// source file, against a seeded ssldir, with every directory Prepare checks
// in a trusted scratch tree. libdir and pluginfactdest do not exist yet.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	base := trustedScratch(t)
	vardir := filepath.Join(base, "cache")
	if err := os.Mkdir(vardir, 0o755); err != nil {
		t.Fatal(err)
	}
	facter := filepath.Join(base, "facter")
	if err := os.WriteFile(facter, []byte("#!/bin/sh\necho '{\"kernel\":\"Linux\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(facter, 0o755); err != nil {
		t.Fatal(err)
	}

	src := newSourceServer()
	src.tree("plugins")
	src.tree("pluginfacts")
	src.file("modules/m/x", "served\n")
	srv := &runServer{sourceServer: src, nodeEnv: "production", catalog: runCatalog(t, "production")}

	ca := newFakeCA(t)
	ca.other = srv
	ssldir := filepath.Join(base, "ssl")
	ca.seed(t, ssldir)
	log := new(bytes.Buffer)
	ssl := ca.config(ssldir)
	ssl.Log = log
	return &runFixture{ca: ca, srv: srv, log: log, cfg: RunConfig{
		SSL:             ssl,
		Environment:     "production",
		LibDir:          filepath.Join(vardir, "lib"),
		PluginFactDest:  filepath.Join(vardir, "facts.d"),
		VarDir:          vardir,
		Facter:          facter,
		TransactionUUID: "11111111-2222-4333-8444-555555555555",
	}}
}

// vardirEntries lists what Prepare left in vardir other than libdir and
// pluginfactdest: the sources directory, if any.
func (f *runFixture) vardirEntries(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(f.cfg.VarDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		if e.Name() != "lib" && e.Name() != "facts.d" {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestPrepareRunsInPuppetsOrder(t *testing.T) {
	f := newRunFixture(t)
	p, err := Prepare(context.Background(), f.cfg)
	if err != nil {
		t.Fatalf("Prepare: %v\nlog:\n%s", err, f.log)
	}

	want := []string{
		"GET /puppet/v3/node/" + testCertname,
		"GET /puppet/v3/file_metadatas/plugins",
		"GET /puppet/v3/file_metadatas/pluginfacts",
		"GET /puppet/v3/file_metadatas/plugins",
		"POST /puppet/v3/catalog/" + testCertname,
		"GET /puppet/v3/file_metadata/modules/m/x",
		"GET /puppet/v3/file_content/modules/m/x",
	}
	if got := f.srv.requests(); !slices.Equal(got, want) {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(f.srv.nodeQuery, "transaction_uuid="+f.cfg.TransactionUUID) {
		t.Errorf("node query %q does not carry the transaction UUID", f.srv.nodeQuery)
	}
	if got := f.srv.form["transaction_uuid"]; got != f.cfg.TransactionUUID {
		t.Errorf("catalog transaction_uuid = %q, want %q", got, f.cfg.TransactionUUID)
	}
	if got := f.srv.form["environment"]; got != "production" {
		t.Errorf("catalog environment = %q, want production", got)
	}
	if !bytes.Equal(p.Catalog, f.srv.catalog) {
		t.Error("Catalog is not the response bytes unchanged")
	}
	if p.Environment != "production" {
		t.Errorf("Environment = %q, want production", p.Environment)
	}

	assertMode(t, f.cfg.LibDir, 0o755)
	assertMode(t, f.cfg.PluginFactDest, 0o755)
	local, ok := p.Sources["puppet:///modules/m/x"]
	if !ok {
		t.Fatalf("Sources = %v, want puppet:///modules/m/x", p.Sources)
	}
	if got := readFile(t, local); got != "served\n" {
		t.Errorf("source content = %q", got)
	}
	dirs := f.vardirEntries(t)
	if len(dirs) != 1 || !strings.HasPrefix(dirs[0], "voxagent-sources-") {
		t.Fatalf("vardir holds %v, want one voxagent-sources-* directory", dirs)
	}
	sources := filepath.Join(f.cfg.VarDir, dirs[0])
	assertInside(t, local, sources)
	assertMode(t, sources, 0o700)

	for _, line := range []string{"Using environment 'production'", "Retrieving pluginfacts and plugins", "Loading facts", "Retrieving catalog"} {
		if !strings.Contains(f.log.String(), line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, f.log)
		}
	}
	if strings.Contains(f.log.String(), "Linux") {
		t.Errorf("log carries a fact value:\n%s", f.log)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertAbsent(t, sources)
}

// Log receives the lines Puppet's agent logs before applying, in its order
// and at its levels (configurer.rb, configurer/downloader.rb,
// indirector/facts/facter.rb, indirector/catalog/rest.rb,
// http/service/compiler.rb). "Caching catalog" is not among them: voxagent
// caches no catalog.
func TestPrepareLogsPuppetsLines(t *testing.T) {
	f := newRunFixture(t)
	var got []string
	f.cfg.Log = func(level, message string) { got = append(got, level+": "+message) }
	p, err := Prepare(context.Background(), f.cfg)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer p.Close()
	want := []string{
		"info: Using environment 'production'",
		"info: Retrieving pluginfacts",
		"info: Retrieving plugin",
		"info: Loading facts",
		"notice: Requesting catalog from 127.0.0.1:" + strconv.Itoa(f.ca.port) + " (127.0.0.1)",
		"notice: Catalog compiled by puppet",
	}
	if !slices.Equal(got, want) {
		t.Errorf("logs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Phases are the steps configurer.rb times with add_times, under its names
// and in its order. startup_time runs from Start to the end of the SSL
// bootstrap, as Puppet::Agent#run waits for certificates before the
// configurer starts; with no Start there is none, as with no :start_time.
func TestPreparePhasesArePuppetsTimings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start time.Time
		want  []string
	}{
		{"with a start", time.Now().Add(-time.Hour),
			[]string{"startup_time", "node_retrieval", "plugin_sync", "fact_generation", "config_retrieval"}},
		{"without one", time.Time{},
			[]string{"node_retrieval", "plugin_sync", "fact_generation", "config_retrieval"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.cfg.Start = tc.start
			p, err := Prepare(context.Background(), f.cfg)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			defer p.Close()
			var names []string
			for _, ph := range p.Phases {
				names = append(names, ph.Name)
				if ph.Duration <= 0 {
					t.Errorf("%s took %v, want a measured duration", ph.Name, ph.Duration)
				}
			}
			if !slices.Equal(names, tc.want) {
				t.Errorf("phases %q, want %q", names, tc.want)
			}
			if !tc.start.IsZero() && p.Phases[0].Duration < time.Hour {
				t.Errorf("startup_time = %v, want at least the hour since Start", p.Phases[0].Duration)
			}
		})
	}
}

// Puppet logs the request and the compiler before it looks at the status, so
// a failed compile still names both.
func TestPrepareLogsTheCatalogRequestOnACompileError(t *testing.T) {
	f := newRunFixture(t)
	f.srv.catalogStatus = http.StatusInternalServerError
	var got []string
	f.cfg.Log = func(level, message string) { got = append(got, level+": "+message) }
	if _, err := Prepare(context.Background(), f.cfg); err == nil {
		t.Fatal("Prepare on a 500 = nil error")
	}
	if len(got) < 2 || !strings.HasPrefix(got[len(got)-2], "notice: Requesting catalog from 127.0.0.1:") ||
		got[len(got)-1] != "notice: Catalog compiled by puppet" {
		t.Errorf("logs = %q, want them to end with the request and the compiler", got)
	}
}

// The catalog must be for the environment pluginsync used,
// or the plugins loaded do not match the catalog's.
func TestPrepareRefusesCatalogFromAnotherEnvironment(t *testing.T) {
	f := newRunFixture(t)
	f.srv.catalog = runCatalog(t, "staging")
	_, err := Prepare(context.Background(), f.cfg)
	if err == nil || !strings.Contains(err.Error(), `"staging"`) || !strings.Contains(err.Error(), `"production"`) {
		t.Fatalf("Prepare = %v, want an environment mismatch naming both", err)
	}
	if slices.Contains(f.srv.requests(), "GET /puppet/v3/file_metadata/modules/m/x") {
		t.Error("sources were fetched for a refused catalog")
	}
	if dirs := f.vardirEntries(t); len(dirs) != 0 {
		t.Errorf("vardir holds %v after a refused catalog", dirs)
	}
}

func TestPrepareRefusesBadEnvironmentNames(t *testing.T) {
	t.Run("from the server", func(t *testing.T) {
		f := newRunFixture(t)
		f.srv.nodeEnv = "../etc"
		_, err := Prepare(context.Background(), f.cfg)
		if err == nil || !strings.Contains(err.Error(), "environment") {
			t.Fatalf("Prepare = %v, want the environment refused", err)
		}
		if got := f.srv.requests(); len(got) != 1 {
			t.Errorf("requests after the node = %v, want none", got[1:])
		}
	})
	t.Run("from the flag", func(t *testing.T) {
		f := newRunFixture(t)
		f.cfg.Environment = "Production"
		if _, err := Prepare(context.Background(), f.cfg); err == nil {
			t.Fatal("Prepare accepted environment \"Production\"")
		}
		if got := f.srv.requests(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})
}

// A directory pluginsync writes Ruby into, or the facter
// binary, under a location another user can write is refused before any
// plugin is fetched.
func TestPrepareRefusesUntrustedLocations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, c *RunConfig)
	}{
		{"libdir parent", func(t *testing.T, c *RunConfig) {
			parent := filepath.Join(c.VarDir, "shared")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, 0o777); err != nil {
				t.Fatal(err)
			}
			c.LibDir = filepath.Join(parent, "lib")
		}},
		{"pluginfactdest", func(t *testing.T, c *RunConfig) {
			if err := os.Mkdir(c.PluginFactDest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(c.PluginFactDest, 0o775); err != nil {
				t.Fatal(err)
			}
		}},
		{"vardir", func(t *testing.T, c *RunConfig) {
			if err := os.Chmod(c.VarDir, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{"facter", func(t *testing.T, c *RunConfig) {
			if err := os.Chmod(c.Facter, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			tc.plant(t, &f.cfg)
			_, err := Prepare(context.Background(), f.cfg)
			if err == nil || !strings.Contains(err.Error(), "untrusted location") {
				t.Fatalf("Prepare = %v, want an untrusted location", err)
			}
			for _, r := range f.srv.requests() {
				if strings.Contains(r, "file_") {
					t.Errorf("pluginsync ran: %s", r)
				}
			}
		})
	}
}

// A source the server fails to send fails the run and leaves no sources
// directory behind.
func TestPrepareSourceFailureRemovesSourcesDir(t *testing.T) {
	f := newRunFixture(t)
	f.srv.status["modules/m/x"] = http.StatusInternalServerError
	_, err := Prepare(context.Background(), f.cfg)
	if err == nil || !strings.Contains(err.Error(), "puppet:///modules/m/x") {
		t.Fatalf("Prepare = %v, want the source failure", err)
	}
	if dirs := f.vardirEntries(t); len(dirs) != 0 {
		t.Errorf("vardir holds %v after a failed source fetch", dirs)
	}
}

// Decoded sees the catalog before its sources are fetched, so the caller can
// scrub what the fetch logs or fails with; it is called on a failed run too.
func TestPrepareHandsOverTheDecodedCatalogFirst(t *testing.T) {
	f := newRunFixture(t)
	f.srv.status["modules/m/x"] = http.StatusInternalServerError
	var got *catalog.Catalog
	var fetchedBefore []string
	f.cfg.Decoded = func(cat *catalog.Catalog) {
		got = cat
		fetchedBefore = slices.DeleteFunc(f.srv.requests(), func(r string) bool { return !strings.Contains(r, "modules/m/x") })
	}
	if _, err := Prepare(context.Background(), f.cfg); err == nil {
		t.Fatal("Prepare = nil, want the source failure")
	}
	if got == nil || len(got.Resources) != 1 || got.Resources[0].Ref() != "File[/tmp/voxagent-run-test]" {
		t.Fatalf("Decoded got %+v, want the served catalog", got)
	}
	if len(fetchedBefore) != 0 {
		t.Errorf("sources fetched before Decoded: %q", fetchedBefore)
	}
}

// SSL.Timeout reaches the client Bootstrap returns, so a server that accepts
// the connection and never answers cannot hold the run.
func TestPrepareTimesOutASilentServer(t *testing.T) {
	f := newRunFixture(t)
	f.srv.hang = true
	f.cfg.SSL.Timeout = 200 * time.Millisecond
	start := time.Now()
	_, err := Prepare(context.Background(), f.cfg)
	if err == nil {
		t.Fatal("Prepare against a silent node endpoint = nil error")
	}
	var nerr interface{ Timeout() bool }
	if !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Errorf("Prepare = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Prepare took %s", d)
	}
}
