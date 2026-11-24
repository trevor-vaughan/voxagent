package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fleetData = "../../internal/gatekit/fleet/testdata/"

func TestFleetAxesExitCodes(t *testing.T) {
	el := []string{"from RedHat/true/false", "primary",
		"fleet-pkg-base-curl,fleet-pkg-base-openssl,fleet-pkg-extra-tmux,fleet-pkg-extra-vim"}
	for _, tc := range []struct {
		catalog string
		want    int
	}{
		{"axes-el.txt", exitExpected},
		{"axes-message-changed.txt", exitFinding},
		{"axes-no-json-line.txt", exitFailure},
	} {
		code, stdout, stderr := gatekit(append([]string{"fleet-axes", fleetData + "catalogs/" + tc.catalog}, el...)...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d (stdout %q, stderr %q)", tc.catalog, code, tc.want, stdout, stderr)
		}
	}
}

func TestFleetComplianceExitCodes(t *testing.T) {
	for _, tc := range []struct {
		catalog, contains string
		want              int
	}{
		{"compliance-enforced.txt", "compliance_marker = enforced-by-disa_stig", exitExpected},
		{"compliance-unenforced.txt", "unenforced", exitFinding},
		{"compliance-marker-absent.txt", "is absent from the catalog", exitFailure},
	} {
		code, stdout, _ := gatekit("fleet-compliance", fleetData+"catalogs/"+tc.catalog, "enforced-by-disa_stig")
		if code != tc.want || !strings.Contains(stdout, tc.contains) {
			t.Errorf("%s: exit %d, stdout %q; want %d containing %q", tc.catalog, code, stdout, tc.want, tc.contains)
		}
	}
}

func TestFleetFactsExitCodes(t *testing.T) {
	for _, tc := range []struct {
		facts string
		want  int
	}{
		{"full.json", exitExpected},
		{"missing-os.json", exitFinding},
		{"absent.json", exitFailure},
	} {
		if code, _, _ := gatekit("fleet-facts", fleetData+"facts/"+tc.facts, "node-el"); code != tc.want {
			t.Errorf("%s: exit %d, want %d", tc.facts, code, tc.want)
		}
	}
}

func TestPdbExportedExitCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusOK, `[{"title":"t"}]`, exitExpected},
		{http.StatusOK, `[]`, exitFinding},
		{http.StatusServiceUnavailable, `[]`, exitFailure},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		if code, _, _ := gatekit("pdb-exported", srv.URL, "t"); code != tc.want {
			t.Errorf("status %d body %s: exit %d, want %d", tc.status, tc.body, code, tc.want)
		}
		srv.Close()
	}
}

func TestManifestMutateExitCodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "site.pp")
	if err := os.WriteFile(path, []byte("a b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := gatekit("manifest-mutate", path, "a", "c"); code != exitExpected {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := gatekit("manifest-mutate", path, "a", "c"); code != exitFailure || !strings.Contains(stderr, "found 0") {
		t.Errorf("second mutation: exit %d, stderr %q; want 2 naming found 0", code, stderr)
	}
}

func TestFleetCertsCannotRunWithoutCertificates(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")} {
		code, stdout, stderr := gatekit("fleet-certs", dir)
		if code != exitFailure || stdout != "" || !strings.Contains(stderr, "gatekit fleet-certs:") {
			t.Errorf("fleet-certs %s: exit %d, stdout %q, stderr %q; want 2 and an error", dir, code, stdout, stderr)
		}
	}
}
