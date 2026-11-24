package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const testCertname = "agent1.example.test"

var testSerial atomic.Int64

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func issue(t *testing.T, tmpl, parent *x509.Certificate, pub any, priv *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(testSerial.Add(1) + 1)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func caTmpl(cn string) *x509.Certificate {
	return &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
}

func leafTmpl(cn string) *x509.Certificate {
	return &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              []string{cn},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
}

func pemCerts(certs ...*x509.Certificate) []byte {
	var b []byte
	for _, c := range certs {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return b
}

func crlFor(t *testing.T, issuer *x509.Certificate, key *ecdsa.PrivateKey, number int64) []byte {
	t.Helper()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(number),
		ThisUpdate: time.Now().Add(-time.Hour),
		NextUpdate: time.Now().Add(24 * time.Hour),
	}, issuer, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

// fakeCA is a Puppet CA as OpenVox serves it: a root and an intermediate, a
// CRL for each, and an autosigner that signs whatever CSR was last submitted.
type fakeCA struct {
	t                  *testing.T
	root, inter        *x509.Certificate
	rootKey, interKey  *ecdsa.PrivateKey
	caPEM, crlPEM      []byte
	port               int
	mu                 sync.Mutex
	requests           []string          // "METHOD path"
	ims                map[string]string // path -> If-Modified-Since
	csrPEM             []byte
	caServed, crlServe []byte       // what a refresh returns; nil means 304
	unsignedGets       int          // certificate GETs answered 404 before signing
	badCert            string       // "key", "cn", "ca", "expired": what is wrong with the issued certificate
	other              http.Handler // serves what is not a CA request; nil refuses it
}

func newFakeCA(t *testing.T) *fakeCA {
	t.Helper()
	f := &fakeCA{t: t, ims: map[string]string{}}
	f.rootKey, f.interKey = testKey(t), testKey(t)
	f.root = issue(t, caTmpl("Puppet Root CA: test"), caTmpl("Puppet Root CA: test"), &f.rootKey.PublicKey, f.rootKey)
	f.inter = issue(t, caTmpl("Puppet CA generated on puppet"), f.root, &f.interKey.PublicKey, f.rootKey)
	f.caPEM = pemCerts(f.root, f.inter)
	f.crlPEM = append(crlFor(t, f.root, f.rootKey, 5), crlFor(t, f.inter, f.interKey, 5)...)

	srvKey := testKey(t)
	st := leafTmpl("puppet")
	st.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	srv := issue(t, st, f.inter, &srvKey.PublicKey, f.interKey)
	s := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{srv.Raw, f.inter.Raw}, PrivateKey: srvKey}}}
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

func (f *fakeCA) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		f.ims[r.URL.Path] = ims
	}
	refresh := r.Header.Get("If-Modified-Since") != ""
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/puppet-ca/v1/certificate/ca":
		f.conditional(w, refresh, f.caPEM, f.caServed)
	case r.Method == http.MethodGet && r.URL.Path == "/puppet-ca/v1/certificate_revocation_list/ca":
		f.conditional(w, refresh, f.crlPEM, f.crlServe)
	case r.Method == http.MethodPut && r.URL.Path == "/puppet-ca/v1/certificate_request/"+testCertname:
		if f.csrPEM != nil {
			// OpenVox refuses a second CSR for a name with one pending.
			http.Error(w, `{"message":"agent1.example.test already has a requested certificate"}`, http.StatusBadRequest)
			return
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(r.Body); err != nil {
			f.t.Error(err)
		}
		f.csrPEM = body.Bytes()
	case r.Method == http.MethodGet && r.URL.Path == "/puppet-ca/v1/certificate/"+testCertname:
		if f.csrPEM == nil || f.unsignedGets > 0 {
			f.unsignedGets--
			http.Error(w, `{"message":"Not Found: Could not find certificate"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write(f.sign())
	case f.other != nil:
		f.other.ServeHTTP(w, r)
	default:
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func (f *fakeCA) conditional(w http.ResponseWriter, refresh bool, initial, refreshed []byte) {
	switch {
	case !refresh:
		_, _ = w.Write(initial)
	case refreshed == nil:
		w.WriteHeader(http.StatusNotModified)
	default:
		_, _ = w.Write(refreshed)
	}
}

func (f *fakeCA) sign() []byte {
	block, _ := pem.Decode(f.csrPEM)
	if block == nil {
		f.t.Fatal("CSR is not PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		f.t.Fatal(err)
	}
	tmpl := leafTmpl(testCertname)
	pub := csr.PublicKey
	switch f.badCert {
	case "key":
		pub = &testKey(f.t).PublicKey
	case "cn":
		tmpl.Subject.CommonName = "other.example.test"
	case "ca":
		tmpl.IsCA = true
	case "expired":
		tmpl.NotAfter = time.Now().Add(-time.Minute)
	}
	return pemCerts(issue(f.t, tmpl, f.inter, pub, f.interKey))
}

func (f *fakeCA) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeCA) config(ssldir string) SSLConfig {
	return SSLConfig{Server: "127.0.0.1", Port: f.port, SSLDir: ssldir, Certname: testCertname, Log: new(bytes.Buffer)}
}

// seed writes the ssldir a completed bootstrap leaves behind, with a 2048-bit
// key so the tests do not each pay for a 4096-bit one.
func (f *fakeCA) seed(t *testing.T, ssldir string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := issue(t, leafTmpl(testCertname), f.inter, &key.PublicKey, f.interKey)
	files := []struct {
		path string
		mode os.FileMode
		data []byte
	}{
		{"certs/ca.pem", 0o644, f.caPEM},
		{"crl.pem", 0o644, f.crlPEM},
		{"private_keys/" + testCertname + ".pem", 0o640, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})},
		{"certs/" + testCertname + ".pem", 0o644, pemCerts(cert)},
	}
	for _, d := range []string{"", "certs", "private_keys", "certificate_requests"} {
		if err := os.MkdirAll(filepath.Join(ssldir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, fl := range files {
		p := filepath.Join(ssldir, fl.path)
		if err := os.WriteFile(p, fl.data, fl.mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, fl.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (err %v), want absent", path, err)
	}
}

func assertRequests(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestBootstrapFreshNode(t *testing.T) {
	f := newFakeCA(t)
	ssldir := filepath.Join(t.TempDir(), "ssl")
	cfg := f.config(ssldir)
	c, err := Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("Bootstrap returned a nil client")
	}
	assertRequests(t, f.seen(),
		"GET /puppet-ca/v1/certificate/ca",
		"GET /puppet-ca/v1/certificate_revocation_list/ca",
		"PUT /puppet-ca/v1/certificate_request/"+testCertname,
		"GET /puppet-ca/v1/certificate/"+testCertname)

	for path, mode := range map[string]os.FileMode{
		"":                                      0o771,
		"certs":                                 0o755,
		"private_keys":                          0o750,
		"certificate_requests":                  0o755,
		"certs/ca.pem":                          0o644,
		"crl.pem":                               0o644,
		"private_keys/" + testCertname + ".pem": 0o640,
		"certs/" + testCertname + ".pem":        0o644,
	} {
		assertMode(t, filepath.Join(ssldir, path), mode)
	}
	// Puppet deletes the CSR once the certificate is saved (state_machine.rb).
	assertAbsent(t, filepath.Join(ssldir, "certificate_requests", testCertname+".pem"))

	keyPEM, err := os.ReadFile(filepath.Join(ssldir, "private_keys", testCertname+".pem"))
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "RSA PRIVATE KEY" {
		t.Fatalf("key is not a PKCS#1 PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if key.N.BitLen() != 4096 {
		t.Errorf("key is %d bits, want 4096", key.N.BitLen())
	}

	cb, _ := pem.Decode(f.csrPEM)
	if cb == nil || cb.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("CSR is not a CERTIFICATE REQUEST PEM block: %q", f.csrPEM)
	}
	csr, err := x509.ParseCertificateRequest(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Errorf("CSR signature: %v", err)
	}
	if csr.SignatureAlgorithm != x509.SHA256WithRSA {
		t.Errorf("CSR signature algorithm = %v", csr.SignatureAlgorithm)
	}
	if pub, ok := csr.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Error("CSR public key is not the saved key")
	}
	// The TBS shape measured from puppet agent's CSR:
	// version 0, CN as UTF8String, and exactly
	// one attribute, pp_auth_auto_renew = PrintableString "true".
	var tbs struct {
		Version    int
		Subject    asn1.RawValue
		PublicKey  asn1.RawValue
		Attributes asn1.RawValue
	}
	if rest, err := asn1.Unmarshal(csr.RawTBSCertificateRequest, &tbs); err != nil || len(rest) != 0 {
		t.Fatalf("TBS: %v, %d trailing bytes", err, len(rest))
	}
	if tbs.Version != 0 {
		t.Errorf("CSR version = %d", tbs.Version)
	}
	wantSubject := "301e311c301a06035504030c13" + hex.EncodeToString([]byte(testCertname))
	if got := hex.EncodeToString(tbs.Subject.FullBytes); got != wantSubject {
		t.Errorf("subject DER = %s, want %s", got, wantSubject)
	}
	const wantAttrs = "a017" + "3015" + "060b2b06010401828c4c010302" + "3106" + "130474727565"
	if got := hex.EncodeToString(tbs.Attributes.FullBytes); got != wantAttrs {
		t.Errorf("attributes DER = %s, want %s", got, wantAttrs)
	}

	log := cfg.Log.(*bytes.Buffer).String()
	sum := sha256.Sum256(f.root.Raw)
	if !strings.Contains(strings.ToLower(strings.ReplaceAll(log, ":", "")), hex.EncodeToString(sum[:])) {
		t.Errorf("log does not name the root CA fingerprint:\n%s", log)
	}
	if strings.Contains(log, "PRIVATE KEY") || strings.Contains(log, "CERTIFICATE REQUEST") {
		t.Errorf("log carries key or CSR material:\n%s", log)
	}
}

func TestBootstrapSteadyStateNoRequestsButNone(t *testing.T) {
	f := newFakeCA(t)
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	c, err := Bootstrap(context.Background(), f.config(ssldir))
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("Bootstrap returned a nil client")
	}
	assertRequests(t, f.seen())
}

func TestBootstrapRefreshesStaleCA(t *testing.T) {
	f := newFakeCA(t)
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	stale := time.Now().Add(-25 * time.Hour).Truncate(time.Second)
	for _, p := range []string{"certs/ca.pem", "crl.pem"} {
		if err := os.Chtimes(filepath.Join(ssldir, p), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Bootstrap(context.Background(), f.config(ssldir)); err != nil {
		t.Fatal(err)
	}
	assertRequests(t, f.seen(),
		"GET /puppet-ca/v1/certificate/ca",
		"GET /puppet-ca/v1/certificate_revocation_list/ca")
	want := stale.UTC().Format(http.TimeFormat)
	for _, p := range []string{"/puppet-ca/v1/certificate/ca", "/puppet-ca/v1/certificate_revocation_list/ca"} {
		if got := f.ims[p]; got != want {
			t.Errorf("%s If-Modified-Since = %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string][]byte{"certs/ca.pem": f.caPEM, "crl.pem": f.crlPEM} {
		path := filepath.Join(ssldir, p)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed on 304", p)
		}
		// A 304 resets the refresh clock, as Puppet touches the file.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(info.ModTime()) > time.Hour {
			t.Errorf("%s mtime not reset on 304: %s", p, info.ModTime())
		}
	}
}

func TestBootstrapRefreshReplacesCRLWholesale(t *testing.T) {
	f := newFakeCA(t)
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	stale := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(ssldir, "crl.pem"), stale, stale); err != nil {
		t.Fatal(err)
	}
	f.crlServe = append(crlFor(t, f.root, f.rootKey, 6), crlFor(t, f.inter, f.interKey, 7)...)
	if _, err := Bootstrap(context.Background(), f.config(ssldir)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(ssldir, "crl.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.crlServe) {
		t.Errorf("crl.pem is not exactly the refreshed bundle:\n%s", got)
	}
	assertMode(t, filepath.Join(ssldir, "crl.pem"), 0o644)
}

// A refresh that fails a check keeps the existing file and warns, and nothing
// about it stops the run.
func TestBootstrapRefreshKeepsFileOnBadRefresh(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fakeCA) (file string){
		"CA bundle without the trusted root": func(t *testing.T, f *fakeCA) string {
			k := testKey(t)
			other := issue(t, caTmpl("Other Root"), caTmpl("Other Root"), &k.PublicKey, k)
			f.caServed = pemCerts(other)
			return "certs/ca.pem"
		},
		"CA bundle with a leaf": func(t *testing.T, f *fakeCA) string {
			k := testKey(t)
			f.caServed = append(pemCerts(f.root, f.inter), pemCerts(issue(t, leafTmpl("leaf"), f.inter, &k.PublicKey, f.interKey))...)
			return "certs/ca.pem"
		},
		"CRL number went down": func(t *testing.T, f *fakeCA) string {
			f.crlServe = append(crlFor(t, f.root, f.rootKey, 5), crlFor(t, f.inter, f.interKey, 4)...)
			return "crl.pem"
		},
		"CRL signed by a foreign CA": func(t *testing.T, f *fakeCA) string {
			k := testKey(t)
			other := issue(t, caTmpl("Puppet CA generated on puppet"), caTmpl("Puppet CA generated on puppet"), &k.PublicKey, k)
			f.crlServe = append(crlFor(t, f.root, f.rootKey, 6), crlFor(t, other, k, 6)...)
			return "crl.pem"
		},
		"CRL past its nextUpdate": func(t *testing.T, f *fakeCA) string {
			der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
				Number:     big.NewInt(6),
				ThisUpdate: time.Now().Add(-2 * time.Hour),
				NextUpdate: time.Now().Add(-time.Hour),
			}, f.inter, f.interKey)
			if err != nil {
				t.Fatal(err)
			}
			f.crlServe = append(crlFor(t, f.root, f.rootKey, 6), pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})...)
			return "crl.pem"
		},
		"CRL bundle missing a CA": func(t *testing.T, f *fakeCA) string {
			f.crlServe = crlFor(t, f.root, f.rootKey, 6)
			return "crl.pem"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeCA(t)
			ssldir := t.TempDir()
			f.seed(t, ssldir)
			file := setup(t, f)
			path := filepath.Join(ssldir, file)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			stale := time.Now().Add(-25 * time.Hour)
			if err := os.Chtimes(path, stale, stale); err != nil {
				t.Fatal(err)
			}
			cfg := f.config(ssldir)
			if _, err := Bootstrap(context.Background(), cfg); err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("%s replaced by a refresh that failed its checks", file)
			}
			if log := cfg.Log.(*bytes.Buffer).String(); !strings.Contains(log, "Warning") {
				t.Errorf("no warning logged:\n%s", log)
			}
		})
	}
}

func TestBootstrapRejectsMismatchedCert(t *testing.T) {
	for _, bad := range []string{"key", "cn", "ca", "expired"} {
		t.Run(bad, func(t *testing.T) {
			f := newFakeCA(t)
			f.badCert = bad
			ssldir := t.TempDir()
			f.seed(t, ssldir)
			certPath := filepath.Join(ssldir, "certs", testCertname+".pem")
			if err := os.Remove(certPath); err != nil {
				t.Fatal(err)
			}
			if _, err := Bootstrap(context.Background(), f.config(ssldir)); err == nil {
				t.Fatal("Bootstrap accepted a bad certificate")
			}
			assertAbsent(t, certPath)
		})
	}
}

func TestBootstrapWaitforcertZero(t *testing.T) {
	f := newFakeCA(t)
	f.unsignedGets = 1
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	if err := os.Remove(filepath.Join(ssldir, "certs", testCertname+".pem")); err != nil {
		t.Fatal(err)
	}
	_, err := Bootstrap(context.Background(), f.config(ssldir))
	const want = "Couldn't fetch certificate from CA server; you might still need to sign this agent's certificate (agent1.example.test). Exiting now because the waitforcert setting is set to 0."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	assertAbsent(t, filepath.Join(ssldir, "certs", testCertname+".pem"))
	// The CSR stays, as Puppet keeps it until the certificate arrives.
	assertMode(t, filepath.Join(ssldir, "certificate_requests", testCertname+".pem"), 0o644)
}

func TestBootstrapWaitforcertPolls(t *testing.T) {
	f := newFakeCA(t)
	f.unsignedGets = 2
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	if err := os.Remove(filepath.Join(ssldir, "certs", testCertname+".pem")); err != nil {
		t.Fatal(err)
	}
	cfg := f.config(ssldir)
	cfg.WaitForCert = 10 * time.Millisecond
	if _, err := Bootstrap(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cert := "GET /puppet-ca/v1/certificate/" + testCertname
	csr := "PUT /puppet-ca/v1/certificate_request/" + testCertname
	assertRequests(t, f.seen(), csr, cert, csr, cert, csr, cert)
	assertMode(t, filepath.Join(ssldir, "certs", testCertname+".pem"), 0o644)
}

func TestBootstrapWaitforcertHonoursContext(t *testing.T) {
	f := newFakeCA(t)
	f.unsignedGets = 1 << 30
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	if err := os.Remove(filepath.Join(ssldir, "certs", testCertname+".pem")); err != nil {
		t.Fatal(err)
	}
	cfg := f.config(ssldir)
	cfg.WaitForCert = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Bootstrap(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestBootstrapFingerprintPin(t *testing.T) {
	f := newFakeCA(t)
	sum := sha256.Sum256(f.root.Raw)
	good := hex.EncodeToString(sum[:])
	var colons []string
	for i := 0; i < len(good); i += 2 {
		colons = append(colons, strings.ToUpper(good[i:i+2]))
	}

	wrong := f.config(filepath.Join(t.TempDir(), "ssl"))
	wrong.CAFingerprint = strings.Repeat("ab", 32)
	if _, err := Bootstrap(context.Background(), wrong); err == nil {
		t.Fatal("Bootstrap accepted a CA that does not match the pin")
	}
	assertAbsent(t, filepath.Join(wrong.SSLDir, "certs", "ca.pem"))
	assertRequests(t, f.seen(), "GET /puppet-ca/v1/certificate/ca")

	// The intermediate's fingerprint is not the root's.
	isum := sha256.Sum256(f.inter.Raw)
	inter := f.config(filepath.Join(t.TempDir(), "ssl"))
	inter.CAFingerprint = hex.EncodeToString(isum[:])
	if _, err := Bootstrap(context.Background(), inter); err == nil {
		t.Fatal("Bootstrap accepted the intermediate's fingerprint as the root's")
	}

	malformed := f.config(filepath.Join(t.TempDir(), "ssl"))
	malformed.CAFingerprint = "not-hex"
	if _, err := Bootstrap(context.Background(), malformed); err == nil {
		t.Fatal("Bootstrap accepted a malformed fingerprint")
	}

	// A pre-existing ca.pem does not override the pin.
	seeded := f.config(t.TempDir())
	f.seed(t, seeded.SSLDir)
	seeded.CAFingerprint = strings.Repeat("ab", 32)
	if _, err := Bootstrap(context.Background(), seeded); err == nil {
		t.Fatal("Bootstrap loaded a saved ca.pem that does not match the pin")
	}

	right := f.config(t.TempDir())
	f.seed(t, right.SSLDir)
	if err := os.Remove(filepath.Join(right.SSLDir, "certs", "ca.pem")); err != nil {
		t.Fatal(err)
	}
	right.CAFingerprint = strings.Join(colons, ":")
	if _, err := Bootstrap(context.Background(), right); err != nil {
		t.Fatalf("Bootstrap with the right pin (colon form): %v", err)
	}
}

func TestBootstrapRefusesUntrustedFiles(t *testing.T) {
	cases := map[string]struct {
		file string
		mode os.FileMode
	}{
		"group-writable ca.pem":        {"certs/ca.pem", 0o664},
		"world-writable crl.pem":       {"crl.pem", 0o646},
		"world-readable key":           {"private_keys/" + testCertname + ".pem", 0o644},
		"group-writable key":           {"private_keys/" + testCertname + ".pem", 0o660},
		"group-writable cert":          {"certs/" + testCertname + ".pem", 0o664},
		"world-writable certs/":        {"certs", 0o757},
		"world-writable ssldir":        {".", 0o773},
		"world-writable ssldir 0757":   {".", 0o757},
		"group-writable private_keys/": {"private_keys", 0o770},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeCA(t)
			ssldir := t.TempDir()
			f.seed(t, ssldir)
			if err := os.Chmod(filepath.Join(ssldir, tc.file), tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Bootstrap(context.Background(), f.config(ssldir)); err == nil {
				t.Fatal("Bootstrap loaded an untrusted file")
			}
		})
	}
	t.Run("symlinked ca.pem", func(t *testing.T) {
		f := newFakeCA(t)
		ssldir := t.TempDir()
		f.seed(t, ssldir)
		ca := filepath.Join(ssldir, "certs", "ca.pem")
		if err := os.Rename(ca, ca+".real"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ca+".real", ca); err != nil {
			t.Fatal(err)
		}
		if _, err := Bootstrap(context.Background(), f.config(ssldir)); err == nil {
			t.Fatal("Bootstrap followed a symlinked ca.pem")
		}
	})
	t.Run("private_keys owned by another uid", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to chown")
		}
		f := newFakeCA(t)
		ssldir := t.TempDir()
		f.seed(t, ssldir)
		if err := os.Chown(filepath.Join(ssldir, "private_keys"), 4242, 4242); err != nil {
			t.Fatal(err)
		}
		if _, err := Bootstrap(context.Background(), f.config(ssldir)); err == nil {
			t.Fatal("Bootstrap used a private_keys directory owned by another uid")
		}
	})
	t.Run("ca.pem owned by another uid", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("needs root to chown")
		}
		f := newFakeCA(t)
		ssldir := t.TempDir()
		f.seed(t, ssldir)
		if err := os.Chown(filepath.Join(ssldir, "certs", "ca.pem"), 4242, 4242); err != nil {
			t.Fatal(err)
		}
		if _, err := Bootstrap(context.Background(), f.config(ssldir)); err == nil {
			t.Fatal("Bootstrap loaded a ca.pem owned by another uid")
		}
	})
}

// The ssldir Bootstrap creates (0771, group-writable by design) is accepted
// on the next run.
func TestBootstrapTwiceOnItsOwnSSLDir(t *testing.T) {
	f := newFakeCA(t)
	ssldir := filepath.Join(t.TempDir(), "ssl")
	if _, err := Bootstrap(context.Background(), f.config(ssldir)); err != nil {
		t.Fatal(err)
	}
	first := len(f.seen())
	if _, err := Bootstrap(context.Background(), f.config(ssldir)); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	assertRequests(t, f.seen()[first:])
}

// A safe existing directory whose mode differs from Puppet's is set to it.
func TestBootstrapFixesSafeDirModes(t *testing.T) {
	f := newFakeCA(t)
	ssldir := t.TempDir()
	f.seed(t, ssldir)
	for d, mode := range map[string]os.FileMode{".": 0o775, "certs": 0o711, "private_keys": 0o700, "certificate_requests": 0o700} {
		if err := os.Chmod(filepath.Join(ssldir, d), mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Bootstrap(context.Background(), f.config(ssldir)); err != nil {
		t.Fatal(err)
	}
	for d, mode := range map[string]os.FileMode{".": 0o771, "certs": 0o755, "private_keys": 0o750, "certificate_requests": 0o755} {
		assertMode(t, filepath.Join(ssldir, d), mode)
	}
}

func TestCertnameRejected(t *testing.T) {
	for _, name := range []string{"../x", "A", "a/b", "", "a..b", "..", "a b", "a\x00b", "agent1.EXAMPLE.test"} {
		ssldir := filepath.Join(t.TempDir(), "ssl")
		_, err := Bootstrap(context.Background(), SSLConfig{Server: "127.0.0.1", Port: 1, SSLDir: ssldir, Certname: name})
		if err == nil {
			t.Errorf("certname %q accepted", name)
		}
		assertAbsent(t, ssldir)
	}
}

// A FIFO at ca.pem is refused at once: opening it must not wait for a writer.
func TestReadTrustedRefusesFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := readTrusted(p, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		// Unblock the open so the goroutine ends.
		if w, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
		t.Fatal("readTrusted blocked on a FIFO")
	}
}
