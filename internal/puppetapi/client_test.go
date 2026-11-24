package puppetapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// recorded is what the test server saw of one request.
type recorded struct {
	method, path, rawPath, rawQuery string
	header                          http.Header
	body                            []byte
}

// apiServer starts a verified mTLS server running h and returns a client for
// it plus the request it last received.
func apiServer(t *testing.T, h http.HandlerFunc) (*Client, func() recorded) {
	t.Helper()
	p := newPKI(t)
	var mu sync.Mutex
	var last recorded
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		mu.Lock()
		last = recorded{r.Method, r.URL.Path, r.URL.RawPath, r.URL.RawQuery, r.Header.Clone(), body}
		mu.Unlock()
		h(w, r)
	})
	_, port := startTLS(t, wrapped, p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	return c, func() recorded {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func assertHeaders(t *testing.T, r recorded, accept string) {
	t.Helper()
	if got := r.header.Get("Accept"); got != accept {
		t.Errorf("Accept = %q, want %q", got, accept)
	}
	if got := r.header.Get("X-Puppet-Version"); got != report.PuppetVersion {
		t.Errorf("X-Puppet-Version = %q, want %q", got, report.PuppetVersion)
	}
}

func query(t *testing.T, r recorded) url.Values {
	t.Helper()
	q, err := url.ParseQuery(r.rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The wire form Puppet sends, except static_catalog, which voxagent sends as
// false.
func TestCatalogFormMatchesPuppet(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.puppet.rich+json; charset=utf-8")
		_, _ = w.Write([]byte(`{"catalog_format":2}`))
	})
	facts := []byte(`{"name":"n","values":{"k":"a b&c"}}`)
	got, err := c.Catalog(context.Background(), "agent1.example.test", CatalogForm{
		Environment: "production", TransactionUUID: "43656189-4237-4b81-90f7-4122120981fd", Facts: facts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != `{"catalog_format":2}` {
		t.Fatalf("catalog bytes = %q", got)
	}
	r := last()
	if r.method != http.MethodPost || r.path != "/puppet/v3/catalog/agent1.example.test" || r.rawQuery != "environment=production" {
		t.Fatalf("request = %s %s?%s", r.method, r.path, r.rawQuery)
	}
	assertHeaders(t, r, "application/vnd.puppet.rich+json, application/json")
	if ct := r.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	want := "facts_format=application%2Fjson" +
		"&facts=%257B%2522name%2522%253A%2522n%2522%252C%2522values%2522%253A%257B%2522k%2522%253A%2522a%2520b%2526c%2522%257D%257D" +
		"&environment=production&configured_environment=production&check_environment=true" +
		"&transaction_uuid=43656189-4237-4b81-90f7-4122120981fd&job_uuid=&static_catalog=false" +
		"&checksum_type=sha256.sha384.sha512.sha224.md5"
	if string(r.body) != want {
		t.Fatalf("form body\n got %s\nwant %s", r.body, want)
	}
	// What the server recovers: form-decode, then percent-decode once more.
	form, err := url.ParseQuery(string(r.body))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := url.PathUnescape(form.Get("facts"))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != string(facts) {
		t.Fatalf("server-side facts = %q, want %q", decoded, facts)
	}
}

func TestCatalogBodyLimit(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for range 257 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	_, err := c.Catalog(context.Background(), "agent1.example.test", CatalogForm{Environment: "production", Facts: []byte("{}")})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want a size-limit error, got %v", err)
	}
}

// The smaller caps.
func TestSmallBodyLimits(t *testing.T) {
	big := func(n int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("a"), n))
		}
	}
	c, _ := apiServer(t, big(1<<20+1))
	if _, _, err := c.CRL(context.Background(), time.Time{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("CRL: want a size-limit error, got %v", err)
	}
	if _, _, err := c.Certificate(context.Background(), "agent1.example.test"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Certificate: want a size-limit error, got %v", err)
	}
	c, _ = apiServer(t, big(64<<10+1))
	if _, err := c.Node(context.Background(), "agent1.example.test", "production", "u"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Node: want a size-limit error, got %v", err)
	}
	if _, err := c.FileMetadata(context.Background(), "modules/m/x", "production", "manage"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("FileMetadata: want a size-limit error, got %v", err)
	}
}

// The caps apply after decompression: a gzip body far under the PEM cap on
// the wire that inflates past it is refused.
func TestBodyLimitAppliesAfterDecompression(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		var gzBody bytes.Buffer
		gz := gzip.NewWriter(&gzBody)
		_, _ = gz.Write(bytes.Repeat([]byte("a"), maxPEM+1))
		_ = gz.Close()
		if gzBody.Len() >= maxPEM/100 {
			t.Errorf("compressed body is %d bytes; the test needs it far under the cap", gzBody.Len())
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(gzBody.Len()))
		_, _ = w.Write(gzBody.Bytes())
	})
	if _, _, err := c.CRL(context.Background(), time.Time{}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want a size-limit error, got %v", err)
	}
}

func TestCAConditional(t *testing.T) {
	since := time.Date(2026, 11, 15, 2, 37, 51, 0, time.FixedZone("x", 3600))
	var gotIMS string
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotIMS = r.Header.Get("If-Modified-Since")
		if gotIMS != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("PEM"))
	})
	body, changed, err := c.CA(context.Background(), since)
	if err != nil || changed || body != nil {
		t.Fatalf("CA on 304 = %q, %v, %v; want nil, false, nil", body, changed, err)
	}
	if gotIMS != "Sun, 15 Nov 2026 01:37:51 GMT" {
		t.Fatalf("If-Modified-Since = %q", gotIMS)
	}
	r := last()
	if r.method != http.MethodGet || r.path != "/puppet-ca/v1/certificate/ca" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
	assertHeaders(t, r, "text/plain")
	body, changed, err = c.CA(context.Background(), time.Time{})
	if err != nil || !changed || string(body) != "PEM" {
		t.Fatalf("unconditional CA = %q, %v, %v", body, changed, err)
	}
	if gotIMS != "" {
		t.Fatalf("unconditional fetch sent If-Modified-Since %q", gotIMS)
	}
}

// A CA refresh is verified against the saved bundle, unlike
// FetchCA: a server with a certificate from another CA is refused.
func TestCARefreshRejectsServerFromOtherCA(t *testing.T) {
	p := newPKI(t)
	other := newRootCA(t, "Unknown Root")
	srvCert, srvKey := other.leaf(t, "puppet", nil, []net.IP{net.ParseIP("127.0.0.1")})
	var served atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Store(true)
		_, _ = w.Write(p.caPEM)
	})
	_, port := startTLS(t, h, srvKey, []*x509.Certificate{srvCert}, nil)
	c, err := New("127.0.0.1", port, p.caPEM, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CA(context.Background(), time.Time{}); err == nil {
		t.Fatal("CA refresh from a server with a certificate from another CA succeeded")
	}
	if served.Load() {
		t.Fatal("request reached the handler over an unverified connection")
	}
}

// First contact is unconditional, so a 304 to it is a protocol error, not an
// empty bundle.
func TestFetchCA304IsAnError(t *testing.T) {
	var gotIMS string
	p := newPKI(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIMS = r.Header.Get("If-Modified-Since")
		w.WriteHeader(http.StatusNotModified)
	})
	_, port := startTLS(t, h, p.serverKey, p.serverTLSChain, nil)
	if body, err := FetchCA(context.Background(), "127.0.0.1", port); err == nil {
		t.Fatalf("FetchCA on 304 = %q, nil; want an error", body)
	}
	if gotIMS != "" {
		t.Fatalf("FetchCA sent If-Modified-Since %q", gotIMS)
	}
}

// Error strings carry the escaped request path, so a server-supplied relative
// path cannot put raw control bytes into a log line.
func TestErrorPathIsEscaped(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.FileMetadata(context.Background(), "plugins/a\x1b[31m\nb", "production", "manage")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if he.Path != "/puppet/v3/file_metadata/plugins/a%1B%5B31m%0Ab" {
		t.Errorf("Path = %q", he.Path)
	}
	if strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Errorf("error has raw control bytes: %q", err.Error())
	}
}

func TestCRLConditional(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("CRL"))
	})
	body, changed, err := c.CRL(context.Background(), time.Now())
	if err != nil || changed || body != nil {
		t.Fatalf("CRL on 304 = %q, %v, %v", body, changed, err)
	}
	r := last()
	if r.path != "/puppet-ca/v1/certificate_revocation_list/ca" {
		t.Fatalf("path = %s", r.path)
	}
	assertHeaders(t, r, "text/plain")
	body, changed, err = c.CRL(context.Background(), time.Time{})
	if err != nil || !changed || string(body) != "CRL" {
		t.Fatalf("CRL = %q, %v, %v", body, changed, err)
	}
}

func TestSubmitCSR(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := c.SubmitCSR(context.Background(), "agent1.example.test", []byte("CSR PEM")); err != nil {
		t.Fatal(err)
	}
	r := last()
	if r.method != http.MethodPut || r.path != "/puppet-ca/v1/certificate_request/agent1.example.test" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
	assertHeaders(t, r, "text/plain")
	if ct := r.header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q", ct)
	}
	if string(r.body) != "CSR PEM" {
		t.Errorf("body = %q", r.body)
	}
}

func TestCertificate404IsNotFound(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.Error(w, "Not Found: Could not find certificate missing", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("CERT"))
	})
	body, found, err := c.Certificate(context.Background(), "missing")
	if err != nil || found || body != nil {
		t.Fatalf("Certificate(missing) = %q, %v, %v; want nil, false, nil", body, found, err)
	}
	body, found, err = c.Certificate(context.Background(), "agent1.example.test")
	if err != nil || !found || string(body) != "CERT" {
		t.Fatalf("Certificate = %q, %v, %v", body, found, err)
	}
	r := last()
	if r.path != "/puppet-ca/v1/certificate/agent1.example.test" {
		t.Fatalf("path = %s", r.path)
	}
	assertHeaders(t, r, "text/plain")
}

func TestNode(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"agent1.example.test","environment":"staging"}`))
	})
	env, err := c.Node(context.Background(), "agent1.example.test", "production", "uuid-1")
	if err != nil || env != "staging" {
		t.Fatalf("Node = %q, %v", env, err)
	}
	r := last()
	if r.method != http.MethodGet || r.path != "/puppet/v3/node/agent1.example.test" {
		t.Fatalf("request = %s %s", r.method, r.path)
	}
	assertHeaders(t, r, "application/json")
	want := url.Values{"environment": {"production"}, "configured_environment": {"production"}, "transaction_uuid": {"uuid-1"}}
	if q := query(t, r); !reflect.DeepEqual(q, want) {
		t.Fatalf("query = %v, want %v", q, want)
	}

	c, _ = apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"agent1.example.test"}`))
	})
	if _, err := c.Node(context.Background(), "agent1.example.test", "production", "u"); err == nil {
		t.Fatal("node response without an environment was accepted")
	}
}

func TestHTTPErrorCarriesServerMessage(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Evaluation Error: boom","issue_kind":"RUNTIME_ERROR"}`))
	})
	_, err := c.Catalog(context.Background(), "agent1.example.test", CatalogForm{Environment: "production", Facts: []byte("{}")})
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("want *HTTPError, got %T %v", err, err)
	}
	if he.Message != "Evaluation Error: boom" || he.Status != 500 || he.Method != http.MethodPost || he.Path != "/puppet/v3/catalog/agent1.example.test" {
		t.Fatalf("HTTPError = %+v", he)
	}
	if !strings.Contains(err.Error(), "Evaluation Error: boom") {
		t.Fatalf("Error() = %q does not carry the server message", err.Error())
	}

	// The CA answers errors in text/plain.
	c, _ = apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "requires an environment", http.StatusBadRequest)
	})
	err = c.SubmitCSR(context.Background(), "agent1.example.test", []byte("x"))
	if !errors.As(err, &he) || he.Message != "requires an environment" || he.Status != 400 {
		t.Fatalf("text/plain error = %#v", err)
	}
}

// Server text is truncated to 4 KiB and control
// characters other than \n are escaped before anyone prints it.
func TestServerMessageSanitised(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"newline kept":         {"line1\nline2", "line1\nline2"},
		"escape sequence":      {"red \x1b[31mX\x1b[0m", `red \x1b[31mX\x1b[0m`},
		"carriage return, tab": {"a\rb\tc", `a\rb\tc`},
		"NUL and DEL":          {"a\x00b\x7f", `a\x00b\x7f`},
		"C1 control":           {"a\u009bb", `a\u009bb`},
		"bidi override":        {"a\u202eb", `a\u202eb`},
		"invalid UTF-8":        {"a\xffb", `a\xffb`},
		"printable unicode":    {"Évaluation ✓", "Évaluation ✓"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Sanitise(tc.in); got != tc.want {
				t.Fatalf("Sanitise(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	long := strings.Repeat("é", 3000) // 6000 bytes
	got := Sanitise(long)
	if len(got) > 4096 || !strings.HasSuffix(got, "[truncated]") || !strings.HasPrefix(got, "éé") {
		t.Fatalf("long message: %d bytes, suffix %q", len(got), got[max(0, len(got)-20):])
	}
	controls := strings.Repeat("\x1b", 4096)
	if got := Sanitise(controls); len(got) > 4096 || strings.ContainsRune(got, 0x1b) {
		t.Fatalf("escaped control run: %d bytes, raw ESC present: %v", len(got), strings.ContainsRune(got, 0x1b))
	}

	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom\u001b[2J"}`))
	})
	_, err := c.Node(context.Background(), "agent1.example.test", "production", "u")
	var he *HTTPError
	if !errors.As(err, &he) || he.Message != `boom\x1b[2J` {
		t.Fatalf("HTTPError = %#v", err)
	}
}

func TestFileMetadatasQuery(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"path":"/etc/puppetlabs/code/environments/production/modules/m/lib","relative_path":".","links":"follow","owner":64604,"group":0,"mode":420,"checksum":{"type":"ctime","value":"{ctime}2026-11-16 02:34:01 UTC"},"type":"directory","destination":null},` +
			`{"path":"/p","relative_path":"puppet/type/x.rb","owner":1,"group":2,"mode":493,"checksum":{"type":"sha256","value":"{sha256}78"},"type":"file","destination":null}]`))
	})
	got, err := c.FileMetadatas(context.Background(), "pluginfacts", "production", MetadatasParams{
		Recurse: true, MaxFiles: -1, Links: "follow", SourcePermissions: "use", Ignore: []string{".svn", "CVS", ".git", ".hg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := last()
	if r.path != "/puppet/v3/file_metadatas/pluginfacts" {
		t.Fatalf("path = %s", r.path)
	}
	assertHeaders(t, r, "application/json")
	want := url.Values{
		"recurse": {"true"}, "max_files": {"-1"}, "ignore": {".svn", "CVS", ".git", ".hg"}, "links": {"follow"},
		"checksum_type": {"sha256"}, "source_permissions": {"use"}, "environment": {"production"},
	}
	if q := query(t, r); !reflect.DeepEqual(q, want) {
		t.Fatalf("query = %v\nwant %v", q, want)
	}
	if len(got) != 2 {
		t.Fatalf("got %d metadata, want 2", len(got))
	}
	second := got[1]
	if second.RelativePath != "puppet/type/x.rb" || second.Type != "file" || second.Mode != 493 || second.Owner != 1 || second.Group != 2 ||
		second.Checksum.Type != "sha256" || second.Checksum.Value != "{sha256}78" || second.Path != "/p" || second.Destination != "" {
		t.Fatalf("metadata = %+v", second)
	}

	// Non-recursive probe: no max_files, no ignore.
	if _, err := c.FileMetadatas(context.Background(), "plugins", "production", MetadatasParams{Links: "manage", SourcePermissions: "ignore"}); err != nil {
		t.Fatal(err)
	}
	want = url.Values{"recurse": {"false"}, "links": {"manage"}, "checksum_type": {"sha256"}, "source_permissions": {"ignore"}, "environment": {"production"}}
	if q := query(t, last()); !reflect.DeepEqual(q, want) {
		t.Fatalf("non-recursive query = %v\nwant %v", q, want)
	}
}

func TestFileMetadataQuery(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"path":"/x","relative_path":null,"links":"manage","owner":64604,"group":0,"mode":420,"checksum":{"type":"sha256","value":"{sha256}cb"},"type":"file","destination":null}`))
	})
	m, err := c.FileMetadata(context.Background(), "modules/vox_probe/x", "production", "manage")
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != "/x" || m.RelativePath != "" || m.Checksum.Value != "{sha256}cb" || m.Mode != 420 || m.Owner != 64604 {
		t.Fatalf("metadata = %+v", m)
	}
	r := last()
	if r.path != "/puppet/v3/file_metadata/modules/vox_probe/x" {
		t.Fatalf("path = %s", r.path)
	}
	assertHeaders(t, r, "application/json")
	want := url.Values{"links": {"manage"}, "checksum_type": {"sha256"}, "source_permissions": {"ignore"}, "environment": {"production"}}
	if q := query(t, r); !reflect.DeepEqual(q, want) {
		t.Fatalf("query = %v\nwant %v", q, want)
	}
}

// Names are path-escaped, and a segment that would change
// the URL's path structure is refused before any request is made.
func TestNamesArePathEscaped(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"environment":"production"}`))
	})
	if _, err := c.Node(context.Background(), "a/b?c#d", "production", "u"); err != nil {
		t.Fatal(err)
	}
	if r := last(); r.rawPath != "/puppet/v3/node/a%2Fb%3Fc%23d" {
		t.Fatalf("raw path = %q (path %q)", r.rawPath, r.path)
	}
	if _, err := c.FileMetadata(context.Background(), "modules/m/a b%", "production", "manage"); err != nil {
		t.Fatal(err)
	}
	if r := last(); r.path != "/puppet/v3/file_metadata/modules/m/a b%" {
		t.Fatalf("path = %q", r.path)
	}
	for _, name := range []string{"", ".", ".."} {
		if _, err := c.Node(context.Background(), name, "production", "u"); err == nil {
			t.Errorf("certname %q was accepted", name)
		}
	}
	for _, mount := range []string{"", "/plugins", "plugins/", "modules/../x", "modules/./x", "modules//x"} {
		if err := c.FileContent(context.Background(), mount, "production", io.Discard); err == nil {
			t.Errorf("mount path %q was accepted", mount)
		}
	}
}

func TestFileContentGzip(t *testing.T) {
	content := bytes.Repeat([]byte("vox_probe_external=from_pluginfacts\n"), 1000)
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("Accept-Encoding = %q, want gzip offered", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(content)
		_ = gz.Close()
	})
	var buf bytes.Buffer
	if err := c.FileContent(context.Background(), "pluginfacts/vox_probe.txt", "production", &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("content not decoded: %d bytes, want %d", buf.Len(), len(content))
	}
	r := last()
	if r.path != "/puppet/v3/file_content/pluginfacts/vox_probe.txt" || r.rawQuery != "environment=production" {
		t.Fatalf("request = %s?%s", r.path, r.rawQuery)
	}
	assertHeaders(t, r, "application/octet-stream")
}

// file_content is streamed, but capped as file_metadatas is: a body past the
// cap is an error, at the cap it is not.
func TestFileContentLimit(t *testing.T) {
	old := maxFileContent
	maxFileContent = 1000
	t.Cleanup(func() { maxFileContent = old })
	for _, tc := range []struct {
		n        int
		chunked  bool
		wantFail bool
	}{
		{1000, true, false},
		{1001, true, true},
		{1001, false, true},
	} {
		c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			if tc.chunked {
				w.(http.Flusher).Flush() // no Content-Length: the cap must hold mid-stream
			} else {
				w.Header().Set("Content-Length", strconv.Itoa(tc.n))
			}
			_, _ = w.Write(bytes.Repeat([]byte("a"), tc.n))
		})
		err := c.FileContent(context.Background(), "modules/m/x", "production", io.Discard)
		if tc.wantFail != (err != nil && strings.Contains(err.Error(), "exceeds")) {
			t.Errorf("%d bytes (chunked %v): err = %v", tc.n, tc.chunked, err)
		}
	}
}

func TestFileContentErrorWritesNothing(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found: Could not find file_content", http.StatusNotFound)
	})
	var buf bytes.Buffer
	err := c.FileContent(context.Background(), "modules/m/x", "production", &buf)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 || buf.Len() != 0 {
		t.Fatalf("err = %v, wrote %d bytes", err, buf.Len())
	}
}

func TestReportPut(t *testing.T) {
	c, last := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		_, _ = w.Write([]byte(`["log"]`))
	})
	body := []byte(`{"host":"agent1.example.test","report_format":12}`)
	if err := c.Report(context.Background(), "agent1.example.test", "production", body); err != nil {
		t.Fatal(err)
	}
	r := last()
	if r.method != http.MethodPut || r.path != "/puppet/v3/report/agent1.example.test" || r.rawQuery != "environment=production" {
		t.Fatalf("request = %s %s?%s", r.method, r.path, r.rawQuery)
	}
	assertHeaders(t, r, "application/json")
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !bytes.Equal(r.body, body) {
		t.Errorf("body = %q", r.body)
	}
}

// A redirect could move a POST carrying facts to another URL; it is reported,
// not followed.
func TestRedirectNotFollowed(t *testing.T) {
	var hits atomic.Int32
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusTemporaryRedirect)
	})
	_, err := c.Catalog(context.Background(), "agent1.example.test", CatalogForm{Environment: "production", Facts: []byte("{}")})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTemporaryRedirect || hits.Load() != 1 {
		t.Fatalf("err = %v, hits = %d", err, hits.Load())
	}
}

// A server that never answers must not hang the run: Timeout bounds each
// request, on the verified client and on the first-contact CA fetch alike.
func TestTimeoutBoundsEachRequest(t *testing.T) {
	p := newPKI(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	_, port := startTLS(t, h, p.serverKey, p.serverTLSChain, nil)

	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert(), Timeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.Node(context.Background(), "agent1", "production", "u"); err == nil {
		t.Fatal("Node against a silent server = nil error, want a timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Node took %s, want about 200ms", d)
	}

	start = time.Now()
	if _, err := FetchCA(context.Background(), "127.0.0.1", port, Timeout(200*time.Millisecond)); err == nil {
		t.Fatal("FetchCA against a silent server = nil error, want a timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("FetchCA took %s, want about 200ms", d)
	}
}

// The compiler's name and the connection's address are what Puppet logs
// around the catalog request; both are known on an error status too.
func TestCatalogReportsCompilerAndPeer(t *testing.T) {
	status, compiler := http.StatusOK, "compiler1"
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Puppet-Compiler-Name", compiler)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"catalog_format":2}`))
	})
	got, err := c.Catalog(context.Background(), "agent1.example.test", CatalogForm{Environment: "production", Facts: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Compiler != "compiler1" || got.Peer != "127.0.0.1" {
		t.Errorf("Compiler, Peer = %q, %q; want compiler1, 127.0.0.1", got.Compiler, got.Peer)
	}

	// A server-supplied name is printed and reported: capped like a message.
	status, compiler = http.StatusInternalServerError, strings.Repeat("c", 5000)
	got, err = c.Catalog(context.Background(), "agent1.example.test", CatalogForm{Environment: "production", Facts: []byte("{}")})
	if err == nil || got.Body != nil {
		t.Fatalf("Catalog on 500 = %q, %v; want no body and an error", got.Body, err)
	}
	if !strings.HasSuffix(got.Compiler, " [truncated]") || len(got.Compiler) > 4<<10 || got.Peer != "127.0.0.1" {
		t.Errorf("on 500: Compiler (%d bytes), Peer = %q; want a truncated name and 127.0.0.1", len(got.Compiler), got.Peer)
	}
}

func TestRequestLogsMethodPathStatusDuration(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Secret-Header", "hunter2")
		_, _ = w.Write([]byte(`{"name":"agent1.example.test","environment":"staging","secret":"body-text"}`))
	})
	var got []report.LogEntry
	ctx := runlog.WithLogger(context.Background(), runlog.New("debug", func(e report.LogEntry) { got = append(got, e) }, nil))
	if _, err := c.Node(ctx, "agent1.example.test", "production", "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("entries = %v, want one", got)
	}
	e := got[0]
	re := regexp.MustCompile(`^GET /puppet/v3/node/agent1\.example\.test -> 200 in \d+(\.\d+)?m?s$`)
	if e.Level != "debug" || e.Source != "voxagent" || !re.MatchString(e.Message) {
		t.Errorf("entry = %+v, want a debug voxagent line matching %v", e, re)
	}
	for _, leak := range []string{"?", "uuid-1", "environment", "hunter2", "body-text", "Accept"} {
		if strings.Contains(e.Message, leak) {
			t.Errorf("line %q carries %q", e.Message, leak)
		}
	}
}

func TestRequestLogsErrorStatus(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	var got []report.LogEntry
	ctx := runlog.WithLogger(context.Background(), runlog.New("debug", func(e report.LogEntry) { got = append(got, e) }, nil))
	if _, err := c.Node(ctx, "agent1.example.test", "production", "u"); err == nil {
		t.Fatal("404 was accepted")
	}
	if len(got) != 1 || !strings.Contains(got[0].Message, " -> 404 in ") {
		t.Errorf("entries = %v, want one 404 line", got)
	}
}

func TestRequestLogsNothingBelowDebug(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"a","environment":"e"}`))
	})
	var got []report.LogEntry
	ctx := runlog.WithLogger(context.Background(), runlog.New("info", func(e report.LogEntry) { got = append(got, e) }, nil))
	if _, err := c.Node(ctx, "a", "production", "u"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("entries at info = %v, want none", got)
	}
}

// A redacted request names no mount path in its HTTP line, whichever endpoint
// carries it; an unmarked request still does.
func TestRequestLogRedactsMountPath(t *testing.T) {
	c, _ := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/puppet/v3/file_metadatas/") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"directory"}`))
	})
	cases := []struct {
		name string
		call func(ctx context.Context) error
		want string
	}{
		{"file_metadata", func(ctx context.Context) error {
			_, err := c.FileMetadata(ctx, "modules/m/SECRET", "production", "manage")
			return err
		}, "GET /puppet/v3/file_metadata/[redacted] -> 200 in "},
		{"file_metadatas", func(ctx context.Context) error {
			_, err := c.FileMetadatas(ctx, "modules/m/SECRET", "production", MetadatasParams{Recurse: true, Links: "manage", SourcePermissions: "ignore"})
			return err
		}, "GET /puppet/v3/file_metadatas/[redacted] -> 200 in "},
		{"file_content", func(ctx context.Context) error {
			return c.FileContent(ctx, "modules/m/SECRET", "production", io.Discard)
		}, "GET /puppet/v3/file_content/[redacted] -> 200 in "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []report.LogEntry
			ctx := runlog.WithLogger(context.Background(), runlog.New("debug", func(e report.LogEntry) { got = append(got, e) }, nil))
			if err := tc.call(RedactPath(ctx)); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !strings.HasPrefix(got[0].Message, tc.want) || strings.Contains(got[0].Message, "SECRET") {
				t.Errorf("redacted entries = %v, want one starting %q", got, tc.want)
			}
			got = nil
			if err := tc.call(ctx); err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !strings.Contains(got[0].Message, "/modules/m/SECRET -> 200") {
				t.Errorf("unredacted entries = %v, want the mount path", got)
			}
		})
	}
}
