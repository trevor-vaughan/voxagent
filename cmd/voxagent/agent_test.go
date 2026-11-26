package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const agentCertname = "node1.example.test"

// fakeServer is an OpenVox server as `voxagent agent` meets it: a CA that
// autosigns, the node endpoint, empty plugin mounts, a compiled catalog with
// one notify and one File whose content is puppet:///modules/m/x, and the
// report endpoint.
type fakeServer struct {
	t       *testing.T
	caKey   *ecdsa.PrivateKey
	ca      *x509.Certificate
	port    int
	serial  int64
	catalog []byte

	mu            sync.Mutex
	csr           []byte
	requests      []string
	nodeQuery     url.Values
	catalogForm   url.Values
	catalogStatus int    // non-200 answers the catalog request with this status
	reportStatus  int    // non-200 answers the report PUT with this status
	reportError   string // that answer's message; "" is "report processor failed"
	// holdContent, when set, is closed on the first file_content request, which
	// is then left unanswered until the client gives up, as is any retry.
	holdContent chan struct{}
	holdOnce    sync.Once
	report      []byte
}

func newFakeServer(t *testing.T, target string) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t}
	var err error
	if f.caKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	f.ca = f.issue(&x509.Certificate{
		Subject: pkix.Name{CommonName: "Puppet CA: test"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, &f.caKey.PublicKey)

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvCert := f.issue(&x509.Certificate{
		Subject: pkix.Name{CommonName: "puppet"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &srvKey.PublicKey)

	f.catalog, err = json.Marshal(map[string]any{
		"catalog_format": 2, "name": agentCertname, "environment": "production", "version": 1,
		"catalog_uuid": "00000000-0000-4000-8000-000000000007",
		"resources": []any{
			map[string]any{"type": "Notify", "title": "hello", "tags": []any{}, "parameters": map[string]any{"message": "hello"}},
			map[string]any{"type": "File", "title": target, "tags": []any{},
				"parameters": map[string]any{"ensure": "file", "source": "puppet:///modules/m/x"}},
		},
		"edges": []any{}, "tags": []any{}, "classes": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{srvCert.Raw}, PrivateKey: srvKey}}}
	s.StartTLS()
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	if f.port, err = strconv.Atoi(u.Port()); err != nil {
		t.Fatal(err)
	}
	return f
}

// issue signs tmpl with the CA key, self-signed while there is no CA yet.
func (f *fakeServer) issue(tmpl *x509.Certificate, pub any) *x509.Certificate {
	f.t.Helper()
	f.serial++
	tmpl.SerialNumber = big.NewInt(f.serial)
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	parent := f.ca
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, f.caKey)
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fakeServer) crl() []byte {
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: time.Now().Add(-time.Hour), NextUpdate: time.Now().Add(24 * time.Hour),
	}, f.ca, f.caKey)
	if err != nil {
		f.t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// secretSourceMount is a mount path the fake serves like modules/m/x, for the
// leak test's marked puppet: source.
const secretSourceMount = "modules/m/D8SECRET-File-puppet-source"

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
	}
	if f.holdContent != nil && strings.HasPrefix(r.URL.Path, "/puppet/v3/file_content/") {
		f.holdOnce.Do(func() { close(f.holdContent) })
		<-r.Context().Done()
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	content := "served\n"
	sum := sha256.Sum256([]byte(content))
	switch p := r.URL.Path; {
	case p == "/puppet-ca/v1/certificate/ca":
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.Raw}))
	case p == "/puppet-ca/v1/certificate_revocation_list/ca":
		w.Write(f.crl())
	case p == "/puppet-ca/v1/certificate_request/"+agentCertname && r.Method == http.MethodPut:
		f.csr = body
	case p == "/puppet-ca/v1/certificate/"+agentCertname:
		block, _ := pem.Decode(f.csr)
		if block == nil {
			http.Error(w, `{"message":"Not Found: Could not find certificate"}`, http.StatusNotFound)
			return
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			f.t.Error(err)
			return
		}
		c := f.issue(&x509.Certificate{
			Subject: pkix.Name{CommonName: agentCertname}, BasicConstraintsValid: true,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}, csr.PublicKey)
		w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	case p == "/puppet/v3/node/"+agentCertname:
		f.nodeQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"name":"` + agentCertname + `","environment":"production"}`))
	case strings.HasPrefix(p, "/puppet/v3/file_metadatas/plugin"):
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"path":"/srv","relative_path":".","links":"manage","owner":0,"group":0,"mode":493,"type":"directory","destination":null,"checksum":{"type":"ctime","value":"{ctime}2026-11-16 02:34:01 UTC"}}]`))
	case p == "/puppet/v3/catalog/"+agentCertname && r.Method == http.MethodPost:
		f.catalogForm, err = url.ParseQuery(string(body))
		if err != nil {
			f.t.Error(err)
		}
		if f.catalogStatus != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.catalogStatus)
			w.Write([]byte(`{"message":"Server Error: Evaluation Error: Unknown variable: '$nope'","issue_kind":"RUNTIME_ERROR"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.puppet.rich+json")
		w.Header().Set("X-Puppet-Compiler-Name", "puppet")
		w.Write(f.catalog)
	case p == "/puppet/v3/file_metadata/modules/m/x", p == "/puppet/v3/file_metadata/"+secretSourceMount:
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"path":"/srv/x","relative_path":null,"links":"manage","owner":0,"group":0,"mode":420,"type":"file","destination":null,"checksum":{"type":"sha256","value":"{sha256}` + hex.EncodeToString(sum[:]) + `"}}`))
	case p == "/puppet/v3/file_content/modules/m/x", p == "/puppet/v3/file_content/"+secretSourceMount:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(content))
	case p == "/puppet/v3/report/"+agentCertname && r.Method == http.MethodPut:
		f.report = body
		if f.reportStatus != 0 {
			msg := f.reportError
			if msg == "" {
				msg = "report processor failed"
			}
			b, err := json.Marshal(map[string]string{"message": msg})
			if err != nil {
				f.t.Error(err)
			}
			http.Error(w, string(b), f.reportStatus)
			return
		}
		w.Write([]byte(`[]`))
	default:
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (f *fakeServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// agentScratch is a directory whose every ancestor is owned by the running
// user or root and not group- or world-writable, as `agent` requires of
// libdir, pluginfactdest, vardir and facter; t.TempDir
// under /tmp is world-writable. The user cache dir holds the Go build cache,
// so it exists wherever tests run.
func agentScratch(t *testing.T) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache dir: %v", err)
	}
	d, err := os.MkdirTemp(cache, "voxagent-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// agentArgs points every directory and the facter binary into base and the
// server flags at f.
func agentArgs(t *testing.T, base string, f *fakeServer) []string {
	t.Helper()
	vardir := filepath.Join(base, "cache")
	if err := os.Mkdir(vardir, 0o755); err != nil {
		t.Fatal(err)
	}
	facter := filepath.Join(base, "facter")
	if err := os.WriteFile(facter, []byte("#!/bin/sh\necho '{\"kernel\":\"Linux\",\"secret_fact\":\"s3cr3t-value\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(facter, 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"agent",
		"--server", "127.0.0.1", "--serverport", strconv.Itoa(f.port), "--certname", agentCertname,
		"--ssldir", filepath.Join(base, "ssl"), "--vardir", vardir,
		"--libdir", filepath.Join(vardir, "lib"), "--pluginfactdest", filepath.Join(vardir, "facts.d"),
		"--facter", facter, "--no-ruby", "--http-timeout", "30s",
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// assertNoSourcesDir fails if a voxagent-sources-* directory outlived the run.
func assertNoSourcesDir(t *testing.T, base string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(base, "cache", "voxagent-sources-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("sources directory left behind: %v", left)
	}
}

func TestAgentFirstRunAppliesAndReports(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	target := filepath.Join(base, "managed.txt")
	f := newFakeServer(t, target)

	stdout, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--detailed-exitcodes", "--verbose")...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (changes)\n%s", code, stderr)
	}
	if got := lastLine(stderr); got != "voxagent: outcome=changed" {
		t.Errorf("last stderr line = %q\n%s", got, stderr)
	}
	b, err := os.ReadFile(target)
	if err != nil || string(b) != "served\n" {
		t.Errorf("target = %q, %v; want the puppet:/// source's content", b, err)
	}
	if !slices.Contains(f.seen(), "PUT /puppet-ca/v1/certificate_request/"+agentCertname) {
		t.Error("a fresh node submitted no CSR")
	}
	for _, line := range []string{"voxagent: Using environment 'production'", "voxagent: Retrieving pluginfacts and plugins", "voxagent: Loading facts", "voxagent: Retrieving catalog"} {
		if !strings.Contains(stderr, line+"\n") {
			t.Errorf("stderr lacks %q:\n%s", line, stderr)
		}
	}
	if strings.Contains(stderr, "s3cr3t-value") {
		t.Errorf("stderr carries a fact value:\n%s", stderr)
	}
	assertNoSourcesDir(t, base)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.report == nil {
		t.Fatal("no report was submitted")
	}
	var rep map[string]any
	if err := json.Unmarshal(f.report, &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	tx := f.catalogForm.Get("transaction_uuid")
	if tx == "" || f.nodeQuery.Get("transaction_uuid") != tx {
		t.Errorf("transaction_uuid: node %q, catalog %q; want equal and set", f.nodeQuery.Get("transaction_uuid"), tx)
	}
	for k, want := range map[string]any{
		"host": agentCertname, "environment": "production", "transaction_uuid": tx,
		"status": "changed", "catalog_uuid": "00000000-0000-4000-8000-000000000007",
	} {
		if rep[k] != want {
			t.Errorf("report %s = %v, want %v", k, rep[k], want)
		}
	}
	// configurer.rb:336 sets server_used only when it picked a server from
	// server_list, which voxagent does not implement.
	if v, present := rep["server_used"]; present {
		t.Errorf("report server_used = %v, want absent as in Ruby without server_list", v)
	}

	// The phases configurer.rb times on an agent run, beside the --catalog
	// path's three.
	metrics, _ := rep["metrics"].(map[string]any)
	timeMetric, _ := metrics["time"].(map[string]any)
	values, _ := timeMetric["values"].([]any)
	timed := map[string]float64{}
	for _, v := range values {
		triple, _ := v.([]any)
		if len(triple) == 3 {
			name, _ := triple[0].(string)
			timed[name], _ = triple[2].(float64)
		}
	}
	for _, name := range []string{"startup_time", "node_retrieval", "plugin_sync", "fact_generation",
		"config_retrieval", "convert_catalog", "transaction_evaluation", "catalog_application", "total"} {
		if timed[name] <= 0 {
			t.Errorf("time metric %s = %v, want a measured duration (metrics %v)", name, timed[name], values)
		}
	}
	if timed["total"] < timed["startup_time"]+timed["catalog_application"] {
		t.Errorf("total %v is less than startup_time + catalog_application; want it to span the run", timed["total"])
	}

	// Puppet's pre-apply lines open the report, then the transaction's
	// (configurer.rb, transaction.rb:106); no "Caching catalog": none is cached.
	requesting := "Requesting catalog from 127.0.0.1:" + strconv.Itoa(f.port) + " (127.0.0.1)"
	wantLogs := []string{
		"info Puppet Using environment 'production'",
		"info Puppet Retrieving pluginfacts",
		"info Puppet Retrieving plugin",
		"info Puppet Loading facts",
		"notice Puppet " + requesting,
		"notice Puppet Catalog compiled by puppet",
		"info Puppet Applying configuration version '1'",
	}
	logs := reportLogs(t, rep)
	if len(logs) < len(wantLogs) || !slices.Equal(logs[:len(wantLogs)], wantLogs) {
		t.Errorf("report logs:\n%s\nwant them to start with:\n%s", strings.Join(logs, "\n"), strings.Join(wantLogs, "\n"))
	}
	// The console prints the report's lines as Puppet's console does.
	for _, line := range []string{
		"Info: Using environment 'production'", "Info: Retrieving pluginfacts", "Info: Retrieving plugin",
		"Info: Loading facts", "Notice: " + requesting, "Notice: Catalog compiled by puppet",
		"Info: Applying configuration version '1'", "Notice: hello",
		"Notice: /Notify[hello]/message: defined 'message' as 'hello'",
	} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
}

// reportLogs renders each report log as "<level> <source> <message>".
func reportLogs(t *testing.T, rep map[string]any) []string {
	t.Helper()
	entries, ok := rep["logs"].([]any)
	if !ok {
		t.Fatalf("report logs = %v", rep["logs"])
	}
	var out []string
	for _, e := range entries {
		m := e.(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v", m["level"], m["source"], m["message"]))
	}
	return out
}

// Without --verbose the level is Puppet's default, notice: no info line
// reaches the console or the report.
func TestAgentDefaultLevelIsNotice(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))

	stdout, stderr, code := run(t, bin, agentArgs(t, base, f)...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	if strings.Contains(stdout, "Info: ") {
		t.Errorf("stdout has info lines without --verbose:\n%s", stdout)
	}
	for _, line := range []string{"Notice: Catalog compiled by puppet", "Notice: hello"} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var rep map[string]any
	if err := json.Unmarshal(f.report, &rep); err != nil {
		t.Fatalf("report: %v", err)
	}
	for _, l := range reportLogs(t, rep) {
		if strings.HasPrefix(l, "info ") {
			t.Errorf("report holds an info log without --verbose: %s", l)
		}
	}
}

// A compile error is a server 500; nothing is applied
// and no report is sent.
func TestAgentCompileErrorAppliesNothing(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	target := filepath.Join(base, "managed.txt")
	f := newFakeServer(t, target)
	f.catalogStatus = http.StatusInternalServerError

	_, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--detailed-exitcodes")...)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (could not run)\n%s", code, stderr)
	}
	if got := lastLine(stderr); got != "voxagent: outcome=could-not-run" {
		t.Errorf("last stderr line = %q\n%s", got, stderr)
	}
	if !strings.Contains(stderr, "Unknown variable: '$nope'") {
		t.Errorf("stderr does not carry the server's message:\n%s", stderr)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the File was applied after a compile error")
	}
	for _, r := range f.seen() {
		if strings.Contains(r, "/report/") || strings.Contains(r, "/file_metadata/") {
			t.Errorf("request after a compile error: %s", r)
		}
	}
	assertNoSourcesDir(t, base)
}

// Puppet logs "Could not send report" and keeps the run's status
// (configurer.rb send_report); the outcome line stays last.
func TestAgentReportFailureKeepsExit(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	target := filepath.Join(base, "managed.txt")
	f := newFakeServer(t, target)
	f.reportStatus = http.StatusInternalServerError

	_, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--detailed-exitcodes")...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (changes)\n%s", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) < 2 || lines[len(lines)-1] != "voxagent: outcome=changed" ||
		!strings.HasPrefix(lines[len(lines)-2], "voxagent: Could not send report: ") ||
		!strings.Contains(lines[len(lines)-2], "report processor failed") {
		t.Errorf("stderr does not end with the report failure then the outcome:\n%s", stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the catalog was not applied: %v", err)
	}
	assertNoSourcesDir(t, base)
}

// The server's answer to the report PUT is printed after the catalog is
// decoded, so a marked value it echoes is scrubbed.
func TestAgentReportSendFailureIsScrubbed(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))
	var cat map[string]any
	if err := json.Unmarshal(f.catalog, &cat); err != nil {
		t.Fatal(err)
	}
	notify := cat["resources"].([]any)[0].(map[string]any)
	notify["parameters"] = map[string]any{"message": "SINKSECRET-3"}
	notify["sensitive_parameters"] = []any{"message"}
	var err error
	if f.catalog, err = json.Marshal(cat); err != nil {
		t.Fatal(err)
	}
	f.reportStatus, f.reportError = http.StatusInternalServerError, "processor saw SINKSECRET-3"

	_, stderr, _ := run(t, bin, agentArgs(t, base, f)...)
	if !strings.Contains(stderr, "processor saw [redacted]") || strings.Contains(stderr, "SINKSECRET-3") {
		t.Errorf("stderr lacks the scrubbed report failure or holds SINKSECRET-3:\n%s", stderr)
	}
}

// serveResources makes f's catalog hold resources instead of its own.
func serveResources(t *testing.T, f *fakeServer, resources ...map[string]any) {
	t.Helper()
	var cat map[string]any
	if err := json.Unmarshal(f.catalog, &cat); err != nil {
		t.Fatal(err)
	}
	cat["resources"] = resources
	var err error
	if f.catalog, err = json.Marshal(cat); err != nil {
		t.Fatal(err)
	}
}

// A source fetch fails after the catalog is decoded: the failure line names no
// marked value, whether the File marks its source or another resource marks
// the same text.
func TestAgentSourceFetchFailureIsScrubbed(t *testing.T) {
	bin := buildBinary(t)
	const source = "puppet:///modules/m/D8FAILSRC" // the fake answers 400 for it
	for _, c := range []struct {
		name   string
		marker map[string]any // the resource that marks source
		want   string
	}{
		{name: "File marks it", want: "voxagent: source [redacted]: "},
		{name: "Notify marks it", marker: map[string]any{"type": "Notify", "title": "m", "tags": []any{},
			"parameters": map[string]any{"message": source}, "sensitive_parameters": []any{"message"}},
			want: "voxagent: source [redacted]: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := agentScratch(t)
			f := newFakeServer(t, filepath.Join(base, "managed.txt"))
			file := map[string]any{"type": "File", "title": filepath.Join(base, "managed.txt"), "tags": []any{},
				"parameters": map[string]any{"ensure": "file", "source": source}}
			resources := []map[string]any{file}
			if c.marker == nil {
				file["sensitive_parameters"] = []any{"source"}
			} else {
				resources = append(resources, c.marker)
			}
			serveResources(t, f, resources...)
			stdout, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--debug", "--detailed-exitcodes")...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (could not run)\n%s", code, stderr)
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr lacks %q:\n%s", c.want, stderr)
			}
			for where, text := range map[string]string{"stdout": stdout, "stderr": stderr} {
				if c.marker == nil && strings.Contains(text, "D8FAILSRC") || strings.Contains(text, source) {
					t.Errorf("%s holds the marked source:\n%s", where, text)
				}
			}
		})
	}
}

// The lines logged between the catalog's decoding and applying it (the source
// fetch's debug lines) reach the console scrubbed.
func TestAgentPreApplyConsoleLinesAreScrubbed(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))
	serveResources(t, f,
		map[string]any{"type": "Notify", "title": "m", "tags": []any{},
			"parameters": map[string]any{"message": "puppet:///modules/m/x"}, "sensitive_parameters": []any{"message"}},
		map[string]any{"type": "File", "title": filepath.Join(base, "managed.txt"), "tags": []any{},
			"parameters": map[string]any{"ensure": "file", "source": "puppet:///modules/m/x"}})
	stdout, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--debug")...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Debug: voxagent: source [redacted]: ") || strings.Contains(stdout, "puppet:///modules/m/x") {
		t.Errorf("stdout lacks the scrubbed source line or holds the marked value:\n%s", stdout)
	}
}

// A --report-dir write failure after applying is could-not-run, as for
// `apply`, but the machine was modified, so the server still gets the report.
func TestAgentReportDirFailureStillSendsReport(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	target := filepath.Join(base, "managed.txt")
	f := newFakeServer(t, target)
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--detailed-exitcodes",
		"--report-dir", filepath.Join(blocker, "reports"))...)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (could not run)\n%s", code, stderr)
	}
	if got := lastLine(stderr); got != "voxagent: outcome=could-not-run" {
		t.Errorf("last stderr line = %q\n%s", got, stderr)
	}
	if !strings.Contains(stderr, "WAS modified") {
		t.Errorf("stderr does not say the machine was modified:\n%s", stderr)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the catalog was not applied, so this tests the wrong branch: %v", err)
	}
	f.mu.Lock()
	body := f.report
	f.mu.Unlock()
	var rep map[string]any
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("no report reached the server: %v\n%s", err, stderr)
	}
	if rep["status"] != "changed" || rep["transaction_completed"] != true {
		t.Errorf("report status = %v, transaction_completed = %v; want changed, true",
			rep["status"], rep["transaction_completed"])
	}
	assertNoSourcesDir(t, base)
}

// A signal while the run holds its sources directory still removes it: the
// run ends could-not-run with the outcome line last.
func TestAgentSignalCleansUpAndReportsOutcome(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			bin := buildBinary(t)
			base := agentScratch(t)
			target := filepath.Join(base, "managed.txt")
			f := newFakeServer(t, target)
			f.holdContent = make(chan struct{})

			cmd := exec.Command(bin, agentArgs(t, base, f)...)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-f.holdContent:
			case err := <-done:
				t.Fatalf("exited before the sources fetch: %v\n%s", err, stderr.String())
			case <-time.After(30 * time.Second):
				cmd.Process.Kill()
				t.Fatal("the run never reached the sources fetch")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != 2 {
					t.Errorf("exit = %v, want 2 (could not run, plain)\n%s", err, stderr.String())
				}
			case <-time.After(30 * time.Second):
				cmd.Process.Kill()
				t.Fatalf("still running after %v\n%s", sig, stderr.String())
			}
			if got := lastLine(stderr.String()); got != "voxagent: outcome=could-not-run" {
				t.Errorf("last stderr line = %q\n%s", got, stderr.String())
			}
			assertNoSourcesDir(t, base)
			if _, err := os.Stat(target); err == nil {
				t.Error("the catalog was applied after the signal")
			}
		})
	}
}

// A signal that lands after Prepare has returned but before the catalog is
// applied still ends the run could-not-run with nothing applied. A real signal
// reaches the run's context through goroutines the main one can outrun, so no
// subprocess can be made to hit this window reliably; the run is made in
// process and afterPrepare cancels its context as a delivered signal would.
// TestAgentSignalCleansUpAndReportsOutcome covers the signal wiring itself.
func TestAgentSignalBeforeApplyAppliesNothing(t *testing.T) {
	base := agentScratch(t)
	target := filepath.Join(base, "managed.txt")
	f := newFakeServer(t, target)
	var err error
	f.catalog, err = json.Marshal(map[string]any{
		"catalog_format": 2, "name": agentCertname, "environment": "production", "version": 1,
		"catalog_uuid": "00000000-0000-4000-8000-000000000008",
		"resources": []any{map[string]any{"type": "File", "title": target, "tags": []any{},
			"parameters": map[string]any{"ensure": "file", "content": "x\n"}}},
		"edges": []any{}, "tags": []any{}, "classes": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := agentArgs(t, base, f)[1:]
	afterPrepare = func(ctx context.Context) context.Context {
		ctx, cancel := context.WithCancelCause(ctx)
		cancel(errors.New("terminated signal received"))
		return ctx
	}
	t.Cleanup(func() { afterPrepare = nil })
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Drained as it is written, so a run that logs more than the pipe holds
	// does not block.
	read := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(rd)
		read <- string(out)
	}()
	saved := os.Stderr
	os.Stderr = wr
	code := agentCommand(args)
	os.Stderr = saved
	wr.Close()
	stderr := <-read
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not run, plain)\n%s", code, stderr)
	}
	if got := lastLine(stderr); got != "voxagent: outcome=could-not-run" {
		t.Errorf("last stderr line = %q\n%s", got, stderr)
	}
	if !strings.Contains(stderr, "voxagent: terminated signal received; nothing applied\n") {
		t.Errorf("the run did not stop at the check after Prepare:\n%s", stderr)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Errorf("the catalog was applied after the signal\n%s", stderr)
	}
	assertNoSourcesDir(t, base)
}

func TestAgentRejectsStrayArgs(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))
	args := agentArgs(t, base, f)
	args = slices.Insert(args, 1, "junk")

	_, stderr, code := run(t, bin, append(args, "--noop")...)
	if code != 2 {
		t.Errorf("exit = %d, want 2 (could not run, plain)\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `unexpected argument "junk"`) || !strings.Contains(stderr, "usage: voxagent agent") {
		t.Errorf("stderr does not refuse the argument with the usage:\n%s", stderr)
	}
	if got := lastLine(stderr); got != "voxagent: outcome=could-not-run" {
		t.Errorf("last stderr line = %q", got)
	}
	if got := f.seen(); len(got) != 0 {
		t.Errorf("requests = %v, want none", got)
	}
}

// The integration gate's guard calls `voxagent agent --help`.
func TestAgentHelpExitsZero(t *testing.T) {
	bin := buildBinary(t)
	_, stderr, code := run(t, bin, "agent", "--help")
	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "-ca-fingerprint") || !strings.Contains(stderr, "not Puppet's ca_fingerprint") {
		t.Errorf("help does not describe --ca-fingerprint:\n%s", stderr)
	}
}

// The agent prints the same warning in Ruby's console form, "Warning: <source>:
// <message>" on stderr, and keeps it out of the report.
func TestAgentWarnsOnAnUnmarkableSensitiveParameter(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))
	var err error
	f.catalog, err = json.Marshal(map[string]any{
		"catalog_format": 2, "name": agentCertname, "environment": "production", "version": 1,
		"catalog_uuid": "00000000-0000-4000-8000-000000000009",
		"resources": []any{map[string]any{"type": "Exec", "title": "e", "tags": []any{},
			"sensitive_parameters": []any{"cwd"},
			"parameters":           map[string]any{"command": "/bin/true", "cwd": "/"}}},
		"edges": []any{}, "tags": []any{}, "classes": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, stderr, code := run(t, bin, agentArgs(t, base, f)...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	want := "\nWarning: /Exec[e]: Unable to mark 'cwd' as sensitive: cwd is a parameter and not a property, and cannot be automatically redacted.\n"
	if !strings.Contains("\n"+stderr, want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(string(f.report), "Unable to mark") {
		t.Errorf("the warning is in the report: %s", f.report)
	}
}

// --debug implies --verbose, shows voxagent's own Debug line, and prints each
// line once: the pre-apply lines are not replayed when the report is
// observed.
func TestAgentDebugShowsDebugLinesOnce(t *testing.T) {
	bin := buildBinary(t)
	base := agentScratch(t)
	f := newFakeServer(t, filepath.Join(base, "managed.txt"))

	stdout, stderr, code := run(t, bin, append(agentArgs(t, base, f), "--debug")...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Debug: voxagent: voxagent ") || !strings.Contains(stdout, " agent noop=false level=debug; catalog ") {
		t.Errorf("stdout lacks the run-start debug line:\n%s", stdout)
	}
	for _, line := range []string{"Info: Using environment 'production'", "Info: Loading facts", "Notice: Catalog compiled by puppet", "Info: Applying configuration version '1'"} {
		if n := strings.Count(stdout, line+"\n"); n != 1 {
			t.Errorf("stdout has %q %d times, want once:\n%s", line, n, stdout)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var rep map[string]any
	if err := json.Unmarshal(f.report, &rep); err != nil {
		t.Fatalf("report: %v", err)
	}
	var debug int
	for _, l := range reportLogs(t, rep) {
		if strings.HasPrefix(l, "debug voxagent voxagent ") {
			debug++
		}
	}
	if debug != 1 {
		t.Errorf("report holds %d run-start debug lines, want 1:\n%s", debug, strings.Join(reportLogs(t, rep), "\n"))
	}
}
