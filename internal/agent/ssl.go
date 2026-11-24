package agent

import (
	"bytes"
	"context"
	"crypto"
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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/voxagent/voxagent/internal/puppetapi"
)

const (
	// caRefreshInterval and crlRefreshInterval are Puppet's defaults
	// (ca_refresh_interval, crl_refresh_interval); the file mtime is the
	// last-update time, as in Puppet's cert_provider.rb.
	caRefreshInterval  = 24 * time.Hour
	crlRefreshInterval = 24 * time.Hour
	// keyBits is Puppet's default keylength.
	keyBits = 4096
	// maxSSLFile bounds what is read from the ssldir; the server's PEM
	// responses are capped at the same size by puppetapi.
	maxSSLFile = 1 << 20
)

// certnameRe is stricter than Puppet's own check (printable ASCII, lower
// case) because the certname becomes a file name and a URL path segment.
var certnameRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

var (
	oidCommonName = asn1.ObjectIdentifier{2, 5, 4, 3}
	// oidAutoRenew is pp_auth_auto_renew, the one CSR attribute Puppet sends.
	oidAutoRenew     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 34380, 1, 3, 2}
	oidSHA256WithRSA = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
)

// SSLConfig configures Bootstrap. CAFingerprint is "" or the SHA-256 of the
// DER of the CA bundle's self-signed root, hex, colons optional. WaitForCert
// is the poll interval while the certificate is unsigned; 0 exits as Puppet
// does. Timeout bounds each HTTP request of the run, on every client built
// here; 0 means no limit. Now defaults to time.Now and Log to io.Discard.
type SSLConfig struct {
	Server        string
	Port          int
	SSLDir        string
	Certname      string
	WaitForCert   time.Duration
	CAFingerprint string
	Timeout       time.Duration
	Now           func() time.Time
	Log           io.Writer
}

// sslFiles are the ssldir paths Puppet uses.
type sslFiles struct {
	dir, certs, privateKeys, requests string
	ca, crl, key, cert, csr           string
}

// Bootstrap returns a ready mTLS client, doing whatever of Puppet's SSL state
// machine is still needed: fetch or refresh the CA bundle and the
// CRL bundle, generate the key, submit the CSR and fetch the certificate. Each
// step uses its own TLS context: only the first CA fetch is
// unverified.
func Bootstrap(ctx context.Context, c SSLConfig) (*puppetapi.Client, error) {
	if !certnameRe.MatchString(c.Certname) || strings.Contains(c.Certname, "..") {
		return nil, fmt.Errorf("certname %q must match %s and not contain \"..\"", c.Certname, certnameRe)
	}
	pin, err := parseFingerprint(c.CAFingerprint)
	if err != nil {
		return nil, err
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = io.Discard
	}
	f := sslFiles{
		dir:         c.SSLDir,
		certs:       filepath.Join(c.SSLDir, "certs"),
		privateKeys: filepath.Join(c.SSLDir, "private_keys"),
		requests:    filepath.Join(c.SSLDir, "certificate_requests"),
	}
	f.ca = filepath.Join(f.certs, "ca.pem")
	f.crl = filepath.Join(c.SSLDir, "crl.pem")
	f.key = filepath.Join(f.privateKeys, c.Certname+".pem")
	f.cert = filepath.Join(f.certs, c.Certname+".pem")
	f.csr = filepath.Join(f.requests, c.Certname+".pem")
	if err := makeSSLDirs(f); err != nil {
		return nil, err
	}

	caPEM, caChanged, err := ensureCA(ctx, c, f, pin)
	if err != nil {
		return nil, err
	}
	crlPEM, err := ensureCRL(ctx, c, f, caPEM, caChanged)
	if err != nil {
		return nil, err
	}
	key, err := ensureKey(c, f)
	if err != nil {
		return nil, err
	}

	certPEM, _, err := readTrusted(f.cert, false)
	if err == nil {
		client, err := nodeClient(c, caPEM, crlPEM, certPEM, key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.cert, err)
		}
		return client, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return requestCert(ctx, c, f, caPEM, crlPEM, key)
}

// makeSSLDirs creates the ssldir tree with Puppet's modes, set explicitly
// rather than through the umask. An existing directory must
// be a real directory (not a symlink) owned by root or the running uid with no
// group or world write bit beyond Puppet's mode for it; a
// safe one whose mode differs is set to Puppet's mode.
func makeSSLDirs(f sslFiles) error {
	if err := os.MkdirAll(filepath.Dir(f.dir), 0o755); err != nil {
		return err
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{f.dir, 0o771}, {f.certs, 0o755}, {f.privateKeys, 0o750}, {f.requests, 0o755}} {
		err := os.Mkdir(d.path, d.mode)
		if err == nil {
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := os.Lstat(d.path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", d.path)
		}
		if err := checkOwner(d.path, info); err != nil {
			return err
		}
		// Only write bits beyond Puppet's own mode are refused: the ssldir's
		// 0771 is group-writable by design.
		perm := info.Mode().Perm()
		if perm&^d.mode&0o022 != 0 {
			return fmt.Errorf("%s is writable beyond mode %04o (mode %04o)", d.path, d.mode, perm)
		}
		if info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != d.mode {
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureCA returns the trusted CA bundle and whether it changed in this call,
// which forces a CRL refresh as in Puppet: a new CA needs its CRL.
func ensureCA(ctx context.Context, c SSLConfig, f sslFiles, pin []byte) ([]byte, bool, error) {
	caPEM, info, err := readTrusted(f.ca, false)
	if errors.Is(err, fs.ErrNotExist) {
		caPEM, err = puppetapi.FetchCA(ctx, c.Server, c.Port, puppetapi.Timeout(c.Timeout))
		if err != nil {
			return nil, false, fmt.Errorf("fetch CA bundle: %w", err)
		}
		fp, err := trustRoot(caPEM, pin)
		if err != nil {
			return nil, false, fmt.Errorf("CA bundle from %s: %w", c.Server, err)
		}
		fmt.Fprintf(c.Log, "Fetched CA bundle; root CA SHA-256 fingerprint %s\n", fp)
		// ca.pem is saved before the CRL is fetched, as Puppet does: the CRL
		// request verifies the server against it.
		if err := writeFileAtomic(f.ca, caPEM, 0o644); err != nil {
			return nil, false, err
		}
		return caPEM, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	// The pin is checked on load too: a pre-existing ca.pem must not
	// silently override an operator's --ca-fingerprint.
	if _, err := trustRoot(caPEM, pin); err != nil {
		return nil, false, fmt.Errorf("%s: %w", f.ca, err)
	}
	if c.Now().Sub(info.ModTime()) <= caRefreshInterval {
		return caPEM, false, nil
	}

	client, err := puppetapi.New(c.Server, c.Port, caPEM, nil, nil, puppetapi.Timeout(c.Timeout))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", f.ca, err)
	}
	fresh, changed, err := client.CA(ctx, info.ModTime())
	if err != nil {
		fmt.Fprintf(c.Log, "Warning: failed to refresh the CA bundle, using the existing one: %v\n", err)
		return caPEM, false, nil
	}
	if !changed {
		touch(c, f.ca)
		return caPEM, false, nil
	}
	if err := checkRefreshedCA(caPEM, fresh); err != nil {
		fmt.Fprintf(c.Log, "Warning: refused the refreshed CA bundle, using the existing one: %v\n", err)
		return caPEM, false, nil
	}
	// A pin mismatch saves nothing and fails the run.
	fp, err := trustRoot(fresh, pin)
	if err != nil {
		return nil, false, fmt.Errorf("refreshed CA bundle: %w", err)
	}
	fmt.Fprintf(c.Log, "Refreshed CA bundle; root CA SHA-256 fingerprint %s\n", fp)
	if err := writeFileAtomic(f.ca, fresh, 0o644); err != nil {
		return nil, false, err
	}
	return fresh, true, nil
}

// touch resets a file's refresh clock after a 304, as Puppet does. Failing to
// only means the next run asks again, so it warns.
func touch(c SSLConfig, path string) {
	now := c.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		fmt.Fprintf(c.Log, "Warning: could not reset the refresh time of %s: %v\n", path, err)
	}
}

// checkRefreshedCA accepts a refreshed bundle only if it still holds the
// trusted root and every certificate in it is a CA.
func checkRefreshedCA(current, fresh []byte) error {
	cur, err := parseCABundle(current)
	if err != nil {
		return err
	}
	next, err := parseCABundle(fresh)
	if err != nil {
		return err
	}
	if !bytes.Equal(cur.Raw, next.Raw) {
		return fmt.Errorf("it does not contain the trusted root %q", cur.Subject)
	}
	return nil
}

// ensureCRL returns the CRL bundle, fetching it when absent and refreshing it
// when stale or when force is set (the CA bundle changed). A failed refresh
// keeps the existing file and warns.
func ensureCRL(ctx context.Context, c SSLConfig, f sslFiles, caPEM []byte, force bool) ([]byte, error) {
	// CRL requests verify the server against the CA bundle without checking
	// revocation: the CRL being fetched is what revocation needs (Puppet's
	// root context).
	client, err := puppetapi.New(c.Server, c.Port, caPEM, nil, nil, puppetapi.Timeout(c.Timeout))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.ca, err)
	}
	crlPEM, info, err := readTrusted(f.crl, false)
	if errors.Is(err, fs.ErrNotExist) {
		crlPEM, changed, err := client.CRL(ctx, time.Time{})
		if err == nil && !changed {
			err = errors.New("HTTP 304 to an unconditional request")
		}
		if err != nil {
			return nil, fmt.Errorf("fetch CRL bundle: %w", err)
		}
		if err := checkCRLBundle(crlPEM, caPEM, nil, c.Now()); err != nil {
			return nil, fmt.Errorf("CRL bundle from %s: %w", c.Server, err)
		}
		if err := writeFileAtomic(f.crl, crlPEM, 0o644); err != nil {
			return nil, err
		}
		return crlPEM, nil
	}
	if err != nil {
		return nil, err
	}
	if !force && c.Now().Sub(info.ModTime()) <= crlRefreshInterval {
		return crlPEM, nil
	}

	fresh, changed, err := client.CRL(ctx, info.ModTime())
	if err != nil {
		fmt.Fprintf(c.Log, "Warning: failed to refresh the CRL bundle, using the existing one: %v\n", err)
		return crlPEM, nil
	}
	if !changed {
		touch(c, f.crl)
		return crlPEM, nil
	}
	if err := checkCRLBundle(fresh, caPEM, crlPEM, c.Now()); err != nil {
		fmt.Fprintf(c.Log, "Warning: refused the refreshed CRL bundle, using the existing one: %v\n", err)
		return crlPEM, nil
	}
	// The whole file is replaced, never appended to: a stale duplicate CRL
	// for the same CA would fail the revocation check closed.
	if err := writeFileAtomic(f.crl, fresh, 0o644); err != nil {
		return nil, err
	}
	return fresh, nil
}

// checkCRLBundle checks that crlPEM holds exactly one CRL per CA in caPEM,
// each signed by that CA, and, against the previous bundle old (nil on first
// fetch), that no CA's CRL number went down. A CRL past
// its nextUpdate is refused: it would fail every connection.
func checkCRLBundle(crlPEM, caPEM, old []byte, now time.Time) error {
	cas, err := parseCerts(caPEM)
	if err != nil {
		return err
	}
	crls, err := parseCRLs(crlPEM)
	if err != nil {
		return err
	}
	var prev []*x509.RevocationList
	if old != nil {
		if prev, err = parseCRLs(old); err != nil {
			return fmt.Errorf("existing CRL bundle: %w", err)
		}
	}
	covered := make([]bool, len(cas))
	for _, crl := range crls {
		found := false
		for i, ca := range cas {
			if !bytes.Equal(crl.RawIssuer, ca.RawSubject) || crl.CheckSignatureFrom(ca) != nil {
				continue
			}
			if covered[i] {
				return fmt.Errorf("more than one CRL for %q", ca.Subject)
			}
			covered[i], found = true, true
			break
		}
		if !found {
			return fmt.Errorf("CRL from %q is not signed by a CA in the bundle", crl.Issuer)
		}
		if crl.NextUpdate.IsZero() || now.After(crl.NextUpdate) {
			return fmt.Errorf("CRL from %q expired at %s", crl.Issuer, crl.NextUpdate.Format(time.RFC3339))
		}
		for _, p := range prev {
			if bytes.Equal(p.RawIssuer, crl.RawIssuer) && p.Number != nil && (crl.Number == nil || crl.Number.Cmp(p.Number) < 0) {
				return fmt.Errorf("CRL number for %q went down from %s to %v", crl.Issuer, p.Number, crl.Number)
			}
		}
	}
	for i, ok := range covered {
		if !ok {
			return fmt.Errorf("no CRL for %q", cas[i].Subject)
		}
	}
	return nil
}

func parseCRLs(data []byte) ([]*x509.RevocationList, error) {
	ders, err := decodePEM(data, "X509 CRL")
	if err != nil {
		return nil, err
	}
	out := make([]*x509.RevocationList, 0, len(ders))
	for _, der := range ders {
		crl, err := x509.ParseRevocationList(der)
		if err != nil {
			return nil, err
		}
		out = append(out, crl)
	}
	return out, nil
}

// ensureKey loads the node's private key or generates and saves a new one.
// Errors never carry key material.
func ensureKey(c SSLConfig, f sslFiles) (*rsa.PrivateKey, error) {
	data, _, err := readTrusted(f.key, true)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(c.Log, "Creating a new RSA SSL key for %s\n", c.Certname)
		key, err := rsa.GenerateKey(rand.Reader, keyBits)
		if err != nil {
			return nil, err
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := writeFileAtomic(f.key, keyPEM, 0o640); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PKCS#1 RSA private key", f.key)
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.key, err)
	}
	return key, nil
}

// requestCert submits the CSR and fetches the certificate, polling every
// c.WaitForCert while it is unsigned, then saves it once it passes every
// check, and removes the CSR as Puppet does.
func requestCert(ctx context.Context, c SSLConfig, f sslFiles, caPEM, crlPEM []byte, key *rsa.PrivateKey) (*puppetapi.Client, error) {
	csrPEM, err := createCSR(c.Certname, key)
	if err != nil {
		return nil, err
	}
	// CSR and certificate requests verify the server and check revocation,
	// but present no client certificate: the node has none yet.
	ca, err := puppetapi.New(c.Server, c.Port, caPEM, crlPEM, nil, puppetapi.Timeout(c.Timeout))
	if err != nil {
		return nil, err
	}
	for {
		// A 400 means the server already holds a CSR for this name; Puppet
		// goes on to fetch the certificate (state_machine.rb NeedSubmitCSR).
		err := ca.SubmitCSR(ctx, c.Certname, csrPEM)
		var he *puppetapi.HTTPError
		switch {
		case err == nil:
			if err := writeFileAtomic(f.csr, csrPEM, 0o644); err != nil {
				return nil, err
			}
		case errors.As(err, &he) && he.Status == 400:
		default:
			return nil, fmt.Errorf("submit CSR: %w", err)
		}

		certPEM, found, err := ca.Certificate(ctx, c.Certname)
		if err != nil {
			return nil, fmt.Errorf("fetch certificate: %w", err)
		}
		if found {
			client, err := nodeClient(c, caPEM, crlPEM, certPEM, key)
			if err != nil {
				return nil, fmt.Errorf("certificate from %s: %w", c.Server, err)
			}
			if err := writeFileAtomic(f.cert, certPEM, 0o644); err != nil {
				return nil, err
			}
			if err := os.Remove(f.csr); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			return client, nil
		}

		msg := fmt.Sprintf("Couldn't fetch certificate from CA server; you might still need to sign this agent's certificate (%s).", c.Certname)
		if c.WaitForCert <= 0 {
			return nil, errors.New(msg + " Exiting now because the waitforcert setting is set to 0.")
		}
		fmt.Fprintf(c.Log, "%s\nWill try again in %s.\n", msg, c.WaitForCert)
		t := time.NewTimer(c.WaitForCert)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// nodeClient checks the node's certificate and builds the
// full mTLS client: CN = certname, not a CA, inside its validity window and
// matching the private key here; chained to ca.pem and not revoked in
// puppetapi.New.
func nodeClient(c SSLConfig, caPEM, crlPEM, certPEM []byte, key *rsa.PrivateKey) (*puppetapi.Client, error) {
	certs, err := parseCerts(certPEM)
	if err != nil {
		return nil, err
	}
	if len(certs) != 1 {
		return nil, fmt.Errorf("want one certificate, got %d", len(certs))
	}
	leaf := certs[0]
	if leaf.Subject.CommonName != c.Certname {
		return nil, fmt.Errorf("certificate is for CN %q, not %q", leaf.Subject.CommonName, c.Certname)
	}
	if leaf.IsCA {
		return nil, errors.New("certificate is a CA certificate")
	}
	if now := c.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("certificate is valid only from %s to %s", leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	if pub, ok := leaf.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("certificate's public key does not match the private key")
	}
	return puppetapi.New(c.Server, c.Port, caPEM, crlPEM, &tls.Certificate{
		Certificate: [][]byte{leaf.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, puppetapi.Timeout(c.Timeout))
}

// createCSR builds the CSR Puppet sends: version 0, subject
// CN=<certname> as a UTF8String, and one attribute, pp_auth_auto_renew =
// PrintableString "true", signed SHA-256 with RSA. crypto/x509 cannot emit
// that attribute (its Attributes field encodes a SET of AttributeTypeAndValue
// sequences), so the request is assembled with encoding/asn1.
func createCSR(certname string, key *rsa.PrivateKey) ([]byte, error) {
	subject, err := asn1.Marshal(pkix.RDNSequence{{{
		Type:  oidCommonName,
		Value: asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(certname)},
	}}})
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	value, err := asn1.MarshalWithParams("true", "printable")
	if err != nil {
		return nil, err
	}
	attr, err := asn1.Marshal(struct {
		Type   asn1.ObjectIdentifier
		Values []asn1.RawValue `asn1:"set"`
	}{oidAutoRenew, []asn1.RawValue{{FullBytes: value}}})
	if err != nil {
		return nil, err
	}
	tbs, err := asn1.Marshal(struct {
		Version    int
		Subject    asn1.RawValue
		PublicKey  asn1.RawValue
		Attributes []asn1.RawValue `asn1:"tag:0"`
	}{0, asn1.RawValue{FullBytes: subject}, asn1.RawValue{FullBytes: spki}, []asn1.RawValue{{FullBytes: attr}}})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(tbs)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return nil, err
	}
	der, err := asn1.Marshal(struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}{
		asn1.RawValue{FullBytes: tbs},
		pkix.AlgorithmIdentifier{Algorithm: oidSHA256WithRSA, Parameters: asn1.NullRawValue},
		asn1.BitString{Bytes: sig, BitLength: 8 * len(sig)},
	})
	if err != nil {
		return nil, err
	}
	// Puppet verifies its own CSR before sending it; so does this.
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, fmt.Errorf("built an unparseable CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature does not verify: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// parseCABundle requires every certificate to be a CA and exactly one to be
// a self-signed root, which it returns: that root is what a fingerprint pins.
func parseCABundle(data []byte) (*x509.Certificate, error) {
	cas, err := parseCerts(data)
	if err != nil {
		return nil, err
	}
	var root *x509.Certificate
	for _, cert := range cas {
		if !cert.IsCA {
			return nil, fmt.Errorf("%q is not a CA certificate", cert.Subject)
		}
		if !bytes.Equal(cert.RawSubject, cert.RawIssuer) || cert.CheckSignatureFrom(cert) != nil {
			continue
		}
		if root != nil {
			return nil, errors.New("more than one self-signed root")
		}
		root = cert
	}
	if root == nil {
		return nil, errors.New("no self-signed root")
	}
	return root, nil
}

func parseCerts(data []byte) ([]*x509.Certificate, error) {
	ders, err := decodePEM(data, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	out := make([]*x509.Certificate, 0, len(ders))
	for _, der := range ders {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	return out, nil
}

// trustRoot checks a CA bundle and, when pin is set, that its root's SHA-256
// matches. It returns the root's fingerprint for the log.
func trustRoot(data, pin []byte) (string, error) {
	root, err := parseCABundle(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(root.Raw)
	fp := colonHex(sum[:])
	if pin != nil && !bytes.Equal(sum[:], pin) {
		return "", fmt.Errorf("root CA %q has SHA-256 fingerprint %s, not the expected %s", root.Subject, fp, colonHex(pin))
	}
	return fp, nil
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02X", x)
	}
	return strings.Join(parts, ":")
}

// parseFingerprint decodes a SHA-256 fingerprint, hex with optional colons;
// "" means no pin.
func parseFingerprint(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("CA fingerprint %q is not a SHA-256 in hex", s)
	}
	return b, nil
}

// decodePEM returns the DER of every PEM block in data, all of which must be
// of blockType. Data with no such block, or trailing non-PEM text, is an
// error.
func decodePEM(data []byte, blockType string) ([][]byte, error) {
	var out [][]byte
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
		out = append(out, block.Bytes)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("trailing data after the PEM blocks")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no %s block", blockType)
	}
	return out, nil
}

// readTrusted reads an ssldir file only if it is a regular file (not a
// symlink), owned by root or the running uid, and not group- or
// world-writable; a key must also not be world-readable.
// A missing file is an error wrapping fs.ErrNotExist.
//
// O_NONBLOCK keeps the open from waiting for a writer when the path is a FIFO,
// which the regular-file check then refuses; reads of a regular file ignore
// the flag.
func readTrusted(path string, isKey bool) ([]byte, fs.FileInfo, error) {
	fh, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, nil, fmt.Errorf("%s is a symbolic link", path)
	}
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	if err := checkOwner(path, info); err != nil {
		return nil, nil, err
	}
	perm := info.Mode().Perm()
	if perm&0o022 != 0 {
		return nil, nil, fmt.Errorf("%s is group- or world-writable (mode %04o)", path, perm)
	}
	if isKey && perm&0o004 != 0 {
		return nil, nil, fmt.Errorf("%s is world-readable (mode %04o)", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(fh, maxSSLFile+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxSSLFile {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", path, maxSSLFile)
	}
	return data, info, nil
}

// checkOwner refuses a file or directory not owned by root or the running uid.
func checkOwner(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", path)
	}
	if st.Uid != 0 && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not root or the running user", path, st.Uid)
	}
	return nil
}

// writeFileAtomic writes data to a temporary file in path's directory, sets
// mode explicitly, and renames it over path, so a reader never sees a partial
// file and the umask never decides the mode.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	done = true
	return nil
}
