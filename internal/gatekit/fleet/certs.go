package fleet

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CertWarningWindow is how far ahead an expiry is announced. The fleet CA's CRL
// lives 30 days, so a week leaves time to re-mint before the TLS gate fails.
const CertWarningWindow = 7 * 24 * time.Hour

// CertExpiry is when one certificate or CRL in the fleet's certificate
// directory stops being accepted.
type CertExpiry struct {
	File     string // base name, e.g. "ca.crl"
	Kind     string // "certificate" or "CRL"
	Expires  time.Time
	DaysLeft int // whole days from now; negative once expired
	now      time.Time
}

// Expired reports whether the file is no longer accepted.
func (c CertExpiry) Expired() bool { return !c.now.Before(c.Expires) }

// Expiring reports whether the file is still accepted but stops being so
// within CertWarningWindow.
func (c CertExpiry) Expiring() bool {
	return !c.Expired() && c.Expires.Before(c.now.Add(CertWarningWindow))
}

// CheckCerts reads every certificate (*.crt) and CRL (*.crl) in dir -- the
// layout fleet/bin/make-certs.sh writes -- and returns when each expires,
// sorted by file name. A certificate expires at NotAfter; a CRL at NextUpdate,
// after which Puppet refuses it ("The CRL ... has expired"), which is how the
// fleet's TLS gate first failed, 30 days after minting. Private keys are not
// read. An error means the directory could not be checked.
func CheckCerts(dir string, now time.Time) ([]CertExpiry, error) {
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}
	var files []string
	for _, pattern := range []string{"*.crt", "*.crl"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no certificates or CRLs in %s; run 'task fleet:certs'", dir)
	}
	// Every node and OpenVoxDB trusts these two; a directory without either
	// fails TLS as opaquely as an expired one.
	for _, name := range []string{"ca.crt", "ca.crl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return nil, fmt.Errorf("%s is missing from %s; re-mint with 'task fleet:certs'", name, dir)
		}
	}
	sort.Strings(files)

	out := make([]CertExpiry, 0, len(files))
	for _, path := range files {
		expires, kind, err := expiryOf(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		out = append(out, CertExpiry{
			File:     filepath.Base(path),
			Kind:     kind,
			Expires:  expires,
			DaysLeft: int(math.Floor(expires.Sub(now).Hours() / 24)),
			now:      now,
		})
	}
	return out, nil
}

// expiryOf reads the first PEM block of path.
func expiryOf(path string) (time.Time, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, "", errors.New("no PEM block")
	}
	switch block.Type {
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, "", err
		}
		return cert.NotAfter, "certificate", nil
	case "X509 CRL":
		crl, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return time.Time{}, "", err
		}
		return crl.NextUpdate, "CRL", nil
	}
	return time.Time{}, "", fmt.Errorf("a %q PEM block, not a certificate or CRL", block.Type)
}

// WriteCertReport prints one line per entry, then a WARNING line for each
// entry inside CertWarningWindow, an EXPIRED line for each entry past its
// expiry, and, when there is either, the steps that re-mint the directory. It
// returns whether anything has expired.
func WriteCertReport(w io.Writer, entries []CertExpiry) bool {
	width := 0
	for _, c := range entries {
		width = max(width, len(c.File))
	}
	var notes []string
	expired := false
	for _, c := range entries {
		left := days(c.DaysLeft)
		if c.Expired() {
			left = "expired"
		}
		fmt.Fprintf(w, "  %-*s  %-11s  expires %s (%s)\n", width, c.File, c.Kind, c.Expires.Format("2006-01-02"), left)
		switch {
		case c.Expired():
			expired = true
			notes = append(notes, fmt.Sprintf("EXPIRED: %s expired %s", c.File, c.Expires.Format("2006-01-02")))
		case c.Expiring():
			notes = append(notes, fmt.Sprintf("WARNING: %s expires in %s", c.File, days(c.DaysLeft)))
		}
	}
	for _, n := range notes {
		fmt.Fprintln(w, n)
	}
	if len(notes) > 0 {
		fmt.Fprintln(w, "re-mint, from the repository root: task fleet:down; rm -f fleet/.certs/ca.crt fleet/.certs/ca.crl; task fleet:certs; task fleet:up")
	}
	return expired
}

func days(n int) string {
	if n == 0 {
		return "less than a day"
	}
	if n == 1 {
		return fmt.Sprintf("%d day", n)
	}
	return fmt.Sprintf("%d days", n)
}
