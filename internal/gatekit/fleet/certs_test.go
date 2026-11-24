package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// now is fixed so every expiry below is an exact distance from it.
var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// mint writes a CA certificate, one leaf certificate and a CRL into a new
// directory, with the given expiries, the way make-certs.sh lays them out.
func mint(t *testing.T, caExpires, leafExpires, crlNextUpdate time.Time) string {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Puppet CA: fleet-ca"},
		NotBefore:             now.AddDate(0, 0, -30),
		NotAfter:              caExpires,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "node-el"},
		NotBefore:    now.AddDate(0, 0, -30),
		NotAfter:     leafExpires,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: now.AddDate(0, 0, -30),
		NextUpdate: crlNextUpdate,
	}, caCert, key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER)
	writePEM(t, filepath.Join(dir, "node-el.crt"), "CERTIFICATE", leafDER)
	writePEM(t, filepath.Join(dir, "ca.crl"), "X509 CRL", crlDER)
	// A private key sits beside them in the real directory and must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "node-el.key"), []byte("not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCertsReportsEachExpiry(t *testing.T) {
	dir := mint(t, now.AddDate(5, 0, 0), now.AddDate(1, 0, 0), now.AddDate(0, 0, 28))
	got, err := CheckCerts(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"ca.crl": 28, "ca.crt": 1826, "node-el.crt": 365}
	if len(got) != len(want) {
		t.Fatalf("CheckCerts returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		if c.DaysLeft != want[c.File] {
			t.Errorf("%s: %d days left, want %d", c.File, c.DaysLeft, want[c.File])
		}
		if c.Expired() || c.Expiring() {
			t.Errorf("%s: expired=%v expiring=%v, want neither", c.File, c.Expired(), c.Expiring())
		}
	}
	if got[0].File != "ca.crl" || got[0].Kind != "CRL" {
		t.Errorf("first entry %+v, want ca.crl as a CRL (entries sort by file name)", got[0])
	}
}

func TestCheckCertsFlagsExpiringAndExpired(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		crlNext              time.Time
		wantExpired, warning bool
	}{
		{"valid beyond the window", now.AddDate(0, 0, 8), false, false},
		{"inside the warning window", now.AddDate(0, 0, 3), false, true},
		{"expires today, later", now.Add(2 * time.Hour), false, true},
		{"expired an hour ago", now.Add(-time.Hour), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := mint(t, now.AddDate(5, 0, 0), now.AddDate(1, 0, 0), tc.crlNext)
			got, err := CheckCerts(dir, now)
			if err != nil {
				t.Fatal(err)
			}
			crl := got[0]
			if crl.Expired() && crl.DaysLeft >= 0 {
				t.Errorf("ca.crl expired but DaysLeft = %d, want negative", crl.DaysLeft)
			}
			if crl.Expired() != tc.wantExpired || crl.Expiring() != tc.warning {
				t.Errorf("ca.crl expired=%v expiring=%v, want %v %v", crl.Expired(), crl.Expiring(), tc.wantExpired, tc.warning)
			}
		})
	}
}

func TestCheckCertsCannotRun(t *testing.T) {
	empty := t.TempDir()
	if _, err := CheckCerts(empty, now); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Errorf("empty directory: err = %v, want one naming no certificates", err)
	}
	if _, err := CheckCerts(filepath.Join(empty, "absent"), now); err == nil {
		t.Error("absent directory: CheckCerts succeeded")
	}
	bad := mint(t, now.AddDate(5, 0, 0), now.AddDate(1, 0, 0), now.AddDate(0, 0, 28))
	if err := os.WriteFile(filepath.Join(bad, "node-el.crt"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCerts(bad, now); err == nil || !strings.Contains(err.Error(), "node-el.crt") {
		t.Errorf("unparsable certificate: err = %v, want one naming node-el.crt", err)
	}
}

func report(t *testing.T, crlNext time.Time) (string, bool) {
	t.Helper()
	entries, err := CheckCerts(mint(t, now.AddDate(5, 0, 0), now.AddDate(1, 0, 0), crlNext), now)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	expired := WriteCertReport(&out, entries)
	return out.String(), expired
}

func TestWriteCertReportListsEveryFile(t *testing.T) {
	out, expired := report(t, now.AddDate(0, 0, 28))
	want := "" +
		"  ca.crl       CRL          expires 2026-11-02 (28 days)\n" +
		"  ca.crt       certificate  expires 2031-10-05 (1826 days)\n" +
		"  node-el.crt  certificate  expires 2027-10-05 (365 days)\n"
	if out != want || expired {
		t.Errorf("report (expired=%v):\n%s\nwant:\n%s", expired, out, want)
	}
}

func TestWriteCertReportWarnsBeforeExpiry(t *testing.T) {
	out, expired := report(t, now.AddDate(0, 0, 3))
	if expired {
		t.Error("expired = true for a CRL with 3 days left")
	}
	for _, want := range []string{
		"WARNING: ca.crl expires in 3 days",
		"re-mint, from the repository root: task fleet:down; rm -f fleet/.certs/ca.crt fleet/.certs/ca.crl; task fleet:certs; task fleet:up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestWriteCertReportFailsOnExpiry(t *testing.T) {
	out, expired := report(t, now.Add(-time.Hour))
	if !expired {
		t.Error("expired = false for a CRL past its next update")
	}
	for _, want := range []string{"  ca.crl       CRL          expires 2026-10-05 (expired)\n", "EXPIRED: ca.crl expired 2026-10-05", "re-mint, from the repository root: "} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("an expired file is not also a warning:\n%s", out)
	}
}

// make-certs.sh writes ca.crt and ca.crl; a directory missing either is not one
// the fleet can use, whatever else it holds.
func TestCheckCertsRequiresTheCAAndItsCRL(t *testing.T) {
	for _, name := range []string{"ca.crt", "ca.crl"} {
		dir := mint(t, now.AddDate(5, 0, 0), now.AddDate(1, 0, 0), now.AddDate(0, 0, 28))
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckCerts(dir, now); err == nil || !strings.Contains(err.Error(), name+" is missing") {
			t.Errorf("without %s: err = %v, want one naming it missing", name, err)
		}
	}
}

func TestWriteCertReportSaysLessThanADay(t *testing.T) {
	out, _ := report(t, now.Add(2*time.Hour))
	for _, want := range []string{"(less than a day)", "WARNING: ca.crl expires in less than a day", "re-mint, from the repository root: "} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
