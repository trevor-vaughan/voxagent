package puppetapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// New builds an mTLS client for https://server:port.
//
// caPEM is the CA bundle (ssldir/certs/ca.pem: Puppet's root and
// intermediate). crlPEM is the CRL bundle (ssldir/crl.pem, one CRL per CA); it
// may be nil only when cert is nil, which is the bootstrap step that fetches
// the CRL itself. cert is the node's certificate and key, nil before the node
// has one.
//
// The server is verified against the bundle with a hostname check against
// server. When crlPEM is given, revocation is checked in Puppet's default
// mode, chain: every certificate in the server's chain and in
// the node's own chain must be covered by a valid CRL from its issuer. Go's
// crypto/tls does not check CRLs, so this is a VerifyConnection callback.
func New(server string, port int, caPEM, crlPEM []byte, cert *tls.Certificate, opts ...Option) (*Client, error) {
	if crlPEM == nil && cert != nil {
		return nil, errors.New("puppetapi: a client certificate needs a CRL bundle: revocation is checked in chain mode")
	}
	cas, err := parsePEM(caPEM, "CERTIFICATE", func(der []byte) (*x509.Certificate, error) { return x509.ParseCertificate(der) })
	if err != nil {
		return nil, fmt.Errorf("puppetapi: CA bundle: %w", err)
	}
	// bundle is what crypto/tls verifies against: every CA in ca.pem, so a
	// server that sends only its leaf still verifies. roots and intermediates
	// split it again so the revocation check can build the chain up to the
	// self-signed root and cover the intermediate too.
	bundle, roots, intermediates := x509.NewCertPool(), x509.NewCertPool(), x509.NewCertPool()
	haveRoot := false
	for _, c := range cas {
		// crypto/tls accepts any certificate in RootCAs as a trust anchor, so
		// a leaf in the bundle would be trusted to vouch for itself.
		if !c.IsCA {
			return nil, fmt.Errorf("puppetapi: CA bundle: %s is not a CA certificate", c.Subject)
		}
		bundle.AddCert(c)
		if bytes.Equal(c.RawSubject, c.RawIssuer) && c.CheckSignatureFrom(c) == nil {
			roots.AddCert(c)
			haveRoot = true
		} else {
			intermediates.AddCert(c)
		}
	}
	if !haveRoot {
		return nil, errors.New("puppetapi: CA bundle has no self-signed root")
	}

	var crls []*x509.RevocationList
	if crlPEM != nil {
		crls, err = parsePEM(crlPEM, "X509 CRL", x509.ParseRevocationList)
		if err != nil {
			return nil, fmt.Errorf("puppetapi: CRL bundle: %w", err)
		}
	}

	tc := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: server,
		RootCAs:    bundle,
	}
	if cert != nil {
		if err := checkNodeCert(cert, roots, intermediates, crls); err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{*cert}
	}
	if crls != nil {
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			// crypto/tls has already verified the chain and the hostname
			// against bundle; this adds the chain-to-root revocation check.
			if len(cs.PeerCertificates) == 0 {
				return errors.New("puppetapi: server presented no certificate")
			}
			pool := intermediates.Clone()
			for _, c := range cs.PeerCertificates[1:] {
				pool.AddCert(c)
			}
			chains, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: pool,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return fmt.Errorf("puppetapi: server certificate does not chain to the CA root: %w", err)
			}
			if err := checkRevocation(chains, crls, time.Now()); err != nil {
				return fmt.Errorf("puppetapi: server certificate: %w", err)
			}
			return nil
		}
	}
	return &Client{base: baseURL(server, port), hc: newHTTPClient(tc, opts)}, nil
}

// Option configures the HTTP client New or FetchCA builds.
type Option func(*http.Client)

// Timeout bounds each request, from dialing to reading the last byte of the
// response body (http.Client.Timeout). Zero, the default, means no limit.
func Timeout(d time.Duration) Option {
	return func(hc *http.Client) { hc.Timeout = d }
}

// FetchCA fetches the CA bundle from /puppet-ca/v1/certificate/ca without
// verifying the server: this is Puppet's trust-on-first-use step
// (create_insecure_context), the only unverified request, on
// its own client that never presents a client certificate.
// The caller decides whether to trust what comes back. First contact has no
// saved bundle, so the request is unconditional; refreshes use Client.CA.
func FetchCA(ctx context.Context, server string, port int, opts ...Option) (pem []byte, err error) {
	c := &Client{
		base: baseURL(server, port),
		hc: newHTTPClient(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			ServerName:         server,
			InsecureSkipVerify: true,
		}, opts),
	}
	defer c.hc.CloseIdleConnections()
	b, changed, err := c.conditionalGet(ctx, "/puppet-ca/v1/certificate/ca", time.Time{})
	if err != nil {
		return nil, err
	}
	if !changed {
		return nil, errors.New("puppetapi: GET /puppet-ca/v1/certificate/ca: HTTP 304 to an unconditional request")
	}
	return b, nil
}

// newHTTPClient returns a client that ignores proxy settings
// (Transport.Proxy is nil) and does not follow redirects, so a POST
// carrying facts is never resent to a URL the operator did not configure.
func newHTTPClient(tc *tls.Config, opts []Option) *http.Client {
	hc := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     tc,
			TLSHandshakeTimeout: 30 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, o := range opts {
		o(hc)
	}
	return hc
}

// checkNodeCert verifies that the node's own certificate chains to the CA
// root and, in chain mode, that no certificate in that chain is revoked.
func checkNodeCert(cert *tls.Certificate, roots, intermediates *x509.CertPool, crls []*x509.RevocationList) error {
	if len(cert.Certificate) == 0 {
		return errors.New("puppetapi: client certificate is empty")
	}
	leaf := cert.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return fmt.Errorf("puppetapi: client certificate: %w", err)
		}
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return fmt.Errorf("puppetapi: client certificate does not chain to the CA root: %w", err)
	}
	if err := checkRevocation(chains, crls, time.Now()); err != nil {
		return fmt.Errorf("puppetapi: client certificate: %w", err)
	}
	return nil
}

// checkRevocation applies revocation mode chain to every verified chain: each
// certificate, the self-signed root included, must be covered by a CRL from its
// issuer that is correctly signed and current, and must not be listed in it.
// Anything missing fails closed.
func checkRevocation(chains [][]*x509.Certificate, crls []*x509.RevocationList, now time.Time) error {
	for _, chain := range chains {
		for i, c := range chain {
			issuer := c
			if i+1 < len(chain) {
				issuer = chain[i+1]
			}
			if err := checkRevoked(c, issuer, crls, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRevoked(c, issuer *x509.Certificate, crls []*x509.RevocationList, now time.Time) error {
	covered := false
	for _, crl := range crls {
		if !bytes.Equal(crl.RawIssuer, issuer.RawSubject) {
			continue
		}
		if len(crl.AuthorityKeyId) > 0 && len(issuer.SubjectKeyId) > 0 && !bytes.Equal(crl.AuthorityKeyId, issuer.SubjectKeyId) {
			continue
		}
		if err := crl.CheckSignatureFrom(issuer); err != nil {
			return fmt.Errorf("CRL for %s has a bad signature: %w", issuer.Subject, err)
		}
		if crl.NextUpdate.IsZero() || now.After(crl.NextUpdate) {
			return fmt.Errorf("CRL for %s expired at %s", issuer.Subject, crl.NextUpdate.Format(time.RFC3339))
		}
		if crl.ThisUpdate.After(now) {
			return fmt.Errorf("CRL for %s is not valid until %s", issuer.Subject, crl.ThisUpdate.Format(time.RFC3339))
		}
		for _, e := range crl.RevokedCertificateEntries {
			if e.SerialNumber.Cmp(c.SerialNumber) == 0 {
				return fmt.Errorf("%s (serial %s) is revoked by %s", c.Subject, c.SerialNumber, issuer.Subject)
			}
		}
		covered = true
	}
	if !covered {
		return fmt.Errorf("no CRL for %s, which issued %s", issuer.Subject, c.Subject)
	}
	return nil
}

// parsePEM decodes every PEM block in data, all of which must be of
// blockType. Data with no such block is an error.
func parsePEM[T any](data []byte, blockType string, parse func([]byte) (T, error)) ([]T, error) {
	var out []T
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != blockType {
			return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
		}
		v, err := parse(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s block", blockType)
	}
	return out, nil
}
