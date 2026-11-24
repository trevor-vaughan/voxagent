package puppetapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testCA is a certificate authority minted for one test. Puppet's CA is a
// two-level chain (a self-signed root and an intermediate that signs every
// node and server certificate), so the fixtures are too.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

var serialCounter atomic.Int64

func nextSerial() *big.Int { return big.NewInt(serialCounter.Add(1) + 1) }

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, priv *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
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

func caTemplate(cn string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
}

func newRootCA(t *testing.T, cn string) *testCA {
	t.Helper()
	k := newKey(t)
	tmpl := caTemplate(cn)
	return &testCA{cert: sign(t, tmpl, tmpl, &k.PublicKey, k), key: k}
}

func (ca *testCA) newIntermediate(t *testing.T, cn string) *testCA {
	t.Helper()
	k := newKey(t)
	return &testCA{cert: sign(t, caTemplate(cn), ca.cert, &k.PublicKey, ca.key), key: k}
}

// leaf issues an end-entity certificate usable for both server and client
// authentication, as Puppet's CA issues them.
func (ca *testCA) leaf(t *testing.T, cn string, dns []string, ips []net.IP) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	return sign(t, tmpl, ca.cert, &k.PublicKey, ca.key), k
}

// crl returns a PEM CRL signed by ca revoking serials, valid from thisUpdate to
// nextUpdate.
func (ca *testCA) crl(t *testing.T, thisUpdate, nextUpdate time.Time, serials ...*big.Int) []byte {
	t.Helper()
	return signedCRL(t, ca.cert, ca.key, thisUpdate, nextUpdate, serials...)
}

func signedCRL(t *testing.T, issuer *x509.Certificate, key *ecdsa.PrivateKey, thisUpdate, nextUpdate time.Time, serials ...*big.Int) []byte {
	t.Helper()
	var entries []x509.RevocationListEntry
	for _, s := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: s, RevocationTime: thisUpdate})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:                    big.NewInt(1),
		ThisUpdate:                thisUpdate,
		NextUpdate:                nextUpdate,
		RevokedCertificateEntries: entries,
	}, issuer, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
}

func certPEM(certs ...*x509.Certificate) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

func tlsCert(key *ecdsa.PrivateKey, chain ...*x509.Certificate) tls.Certificate {
	tc := tls.Certificate{PrivateKey: key, Leaf: chain[0]}
	for _, c := range chain {
		tc.Certificate = append(tc.Certificate, c.Raw)
	}
	return tc
}

// pki is the default fixture: root -> intermediate -> server and node leaves,
// with an empty CRL per CA.
type pki struct {
	root, inter    *testCA
	server, node   *x509.Certificate
	serverKey      *ecdsa.PrivateKey
	nodeKey        *ecdsa.PrivateKey
	caPEM, crlPEM  []byte
	serverTLSChain []*x509.Certificate
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{root: newRootCA(t, "Puppet Root CA: test")}
	p.inter = p.root.newIntermediate(t, "Puppet CA generated on puppet")
	p.server, p.serverKey = p.inter.leaf(t, "puppet", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	p.node, p.nodeKey = p.inter.leaf(t, "agent1.example.test", []string{"agent1.example.test"}, nil)
	p.caPEM = certPEM(p.root.cert, p.inter.cert)
	now := time.Now()
	p.crlPEM = append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour)), p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour))...)
	p.serverTLSChain = []*x509.Certificate{p.server, p.inter.cert}
	return p
}

func (p *pki) nodeCert() *tls.Certificate {
	c := tlsCert(p.nodeKey, p.node, p.inter.cert)
	return &c
}

// startTLS serves h over TLS with the given server chain and records the
// client certificate the peer presented, if any.
func startTLS(t *testing.T, h http.Handler, key *ecdsa.PrivateKey, chain []*x509.Certificate, cfg func(*tls.Config)) (*httptest.Server, int) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{tlsCert(key, chain...)},
		ClientAuth:   tls.RequestClientCert,
	}
	if cfg != nil {
		cfg(srv.TLS)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return srv, port
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"agent1.example.test","environment":"production"}`))
	})
}

// nodeRequest makes one mTLS request through c.
func nodeRequest(c *Client) error {
	_, err := c.Node(context.Background(), "agent1.example.test", "production", "uuid")
	return err
}

func TestAcceptsServerFromBundle(t *testing.T) {
	p := newPKI(t)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err != nil {
		t.Fatalf("request against a valid server failed: %v", err)
	}
}

// Puppet's server may send only its leaf; the intermediate comes from the
// agent's own CA bundle, and its revocation must still be checked.
func TestServerSendingOnlyLeafIsVerifiedThroughBundle(t *testing.T) {
	p := newPKI(t)
	_, port := startTLS(t, okHandler(), p.serverKey, []*x509.Certificate{p.server}, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err != nil {
		t.Fatalf("leaf-only server rejected: %v", err)
	}
	now := time.Now()
	revoked := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour), p.inter.cert.SerialNumber),
		p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour))...)
	c, err = New("127.0.0.1", port, p.caPEM, revoked, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err == nil {
		t.Fatal("revoked intermediate taken from the bundle was accepted")
	}
}

func TestRejectsServerFromOtherCA(t *testing.T) {
	p := newPKI(t)
	other := newRootCA(t, "Other Root").newIntermediate(t, "Other CA")
	srvCert, srvKey := other.leaf(t, "puppet", []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	_, port := startTLS(t, okHandler(), srvKey, []*x509.Certificate{srvCert, other.cert}, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err == nil {
		t.Fatal("server certificate from a foreign CA was accepted")
	}
}

func TestRejectsWrongHostname(t *testing.T) {
	p := newPKI(t)
	srvCert, srvKey := p.inter.leaf(t, "elsewhere", []string{"elsewhere.example.test"}, nil)
	_, port := startTLS(t, okHandler(), srvKey, []*x509.Certificate{srvCert, p.inter.cert}, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) {
		t.Fatalf("want a hostname error, got %v", err)
	}
}

func TestRejectsExpiredServerCert(t *testing.T) {
	p := newPKI(t)
	k := newKey(t)
	expired := sign(t, &x509.Certificate{
		SerialNumber: nextSerial(),
		Subject:      pkix.Name{CommonName: "puppet"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}, p.inter.cert, &k.PublicKey, p.inter.key)
	_, port := startTLS(t, okHandler(), k, []*x509.Certificate{expired, p.inter.cert}, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("want an expired-certificate error, got %v", err)
	}
}

func TestRejectsRevokedServerCert(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	crls := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour)),
		p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour), p.server.SerialNumber)...)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, crls, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("want a revocation error, got %v", err)
	}
}

func TestRevokedIntermediateRejected(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	crls := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour), p.inter.cert.SerialNumber),
		p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour))...)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, crls, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("want a revocation error for the intermediate, got %v", err)
	}
}

func TestRejectsExpiredCRL(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	crls := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour)),
		p.inter.crl(t, now.Add(-2*time.Hour), now.Add(-time.Hour))...)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, crls, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expired-CRL error, got %v", err)
	}
}

// Revocation mode chain: every CA in the chain must have a CRL, so a bundle
// holding only the intermediate's CRL fails closed.
func TestMissingCRLForChainCAFails(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	onlyInter := p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour))
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, onlyInter, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	if err == nil || !strings.Contains(err.Error(), "no CRL") {
		t.Fatalf("want a missing-CRL error, got %v", err)
	}
}

// A CRL that names the intermediate as issuer but was signed by another key
// must not be trusted, even though it revokes nothing.
func TestBadlySignedCRLFails(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	forged := signedCRL(t, p.inter.cert, newKey(t), now.Add(-time.Hour), now.Add(time.Hour))
	crls := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour)), forged...)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, crls, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = nodeRequest(c)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("want a CRL signature error, got %v", err)
	}
}

func TestNewRequiresCRLWithClientCert(t *testing.T) {
	p := newPKI(t)
	if _, err := New("puppet", 8140, p.caPEM, nil, p.nodeCert()); err == nil {
		t.Fatal("New accepted a client certificate without a CRL bundle")
	}
	if _, err := New("puppet", 8140, p.caPEM, nil, nil); err != nil {
		t.Fatalf("bootstrap client (no CRL, no cert) refused: %v", err)
	}
}

func TestNewRejectsBadMaterial(t *testing.T) {
	p := newPKI(t)
	cases := map[string]struct {
		ca, crl []byte
	}{
		"empty CA bundle":         {nil, p.crlPEM},
		"garbage CA bundle":       {[]byte("not pem"), p.crlPEM},
		"bundle without a root":   {certPEM(p.inter.cert), p.crlPEM},
		"non-CA cert in bundle":   {certPEM(p.root.cert, p.inter.cert, p.server), p.crlPEM},
		"CRL bundle without CRLs": {p.caPEM, []byte("not pem")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New("puppet", 8140, tc.ca, tc.crl, nil); err == nil {
				t.Fatal("New accepted bad TLS material")
			}
		})
	}
}

// The node's own chain is checked against the CRLs too.
func TestNewRejectsRevokedClientCert(t *testing.T) {
	p := newPKI(t)
	now := time.Now()
	crls := append(p.root.crl(t, now.Add(-time.Hour), now.Add(time.Hour)),
		p.inter.crl(t, now.Add(-time.Hour), now.Add(time.Hour), p.node.SerialNumber)...)
	_, err := New("puppet", 8140, p.caPEM, crls, p.nodeCert())
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("want a revocation error for the node certificate, got %v", err)
	}
	other := newRootCA(t, "Other Root").newIntermediate(t, "Other CA")
	foreign, foreignKey := other.leaf(t, "agent1.example.test", nil, nil)
	fc := tlsCert(foreignKey, foreign, other.cert)
	if _, err := New("puppet", 8140, p.caPEM, p.crlPEM, &fc); err == nil {
		t.Fatal("New accepted a node certificate from a foreign CA")
	}
}

func TestPresentsClientCert(t *testing.T) {
	p := newPKI(t)
	var got atomic.Value
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			got.Store(r.TLS.PeerCertificates[0].Subject.CommonName)
		}
		okHandler().ServeHTTP(w, r)
	})
	_, port := startTLS(t, h, p.serverKey, p.serverTLSChain, nil)
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err != nil {
		t.Fatal(err)
	}
	if got.Load() != "agent1.example.test" {
		t.Fatalf("server saw client cert CN %v, want agent1.example.test", got.Load())
	}
}

func TestRejectsTLSBelow12(t *testing.T) {
	p := newPKI(t)
	_, port := startTLS(t, okHandler(), p.serverKey, p.serverTLSChain, func(c *tls.Config) {
		c.MinVersion = tls.VersionTLS10
		c.MaxVersion = tls.VersionTLS11
	})
	c, err := New("127.0.0.1", port, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeRequest(c); err == nil {
		t.Fatal("TLS 1.1 server was accepted")
	}
}

// FetchCA is the only unverified request: it accepts any
// server certificate and never presents a client certificate.
func TestFetchCAIsUnverifiedAndAnonymous(t *testing.T) {
	other := newRootCA(t, "Unknown Root")
	srvCert, srvKey := other.leaf(t, "puppet", nil, []net.IP{net.ParseIP("127.0.0.1")})
	var sawClientCert atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClientCert.Store(len(r.TLS.PeerCertificates) > 0)
		_, _ = w.Write([]byte("PEM"))
	})
	_, port := startTLS(t, h, srvKey, []*x509.Certificate{srvCert}, nil)
	body, err := FetchCA(context.Background(), "127.0.0.1", port)
	if err != nil || string(body) != "PEM" {
		t.Fatalf("FetchCA = %q, %v", body, err)
	}
	if sawClientCert.Load() {
		t.Fatal("FetchCA presented a client certificate")
	}

	p := newPKI(t)
	c, err := New("127.0.0.1", port, p.caPEM, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.CRL(context.Background(), time.Time{}); err == nil {
		t.Fatal("the verified client accepted a server from an unknown CA")
	}
}

// Proxy environment variables are ignored, on both the
// verified client and the first-contact CA fetch. Go's environment proxy
// skips loopback targets, so the requests go to a name under .invalid (RFC
// 6761): honouring the proxy would show up as a CONNECT at the fake proxy;
// ignoring it is a direct dial that fails to resolve.
func TestNoProxyFromEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	for _, v := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(v, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const host = "puppet.voxagent-test.invalid"
	p := newPKI(t)
	c, err := New(host, 8140, p.caPEM, p.crlPEM, p.nodeCert())
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.hc.Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("Transport.Proxy is set")
	}
	if _, err := c.Node(ctx, "agent1.example.test", "production", "uuid"); err == nil {
		t.Fatal("request to an unresolvable host succeeded")
	}
	if _, err := FetchCA(ctx, host, 8140); err == nil {
		t.Fatal("FetchCA to an unresolvable host succeeded")
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("%d requests went through the environment's proxy", n)
	}
}
