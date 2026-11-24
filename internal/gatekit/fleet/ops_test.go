package fleet

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pdb serves one canned answer and records the query it was asked.
func pdb(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pdb/query/v4/resources" {
			http.NotFound(w, r)
			return
		}
		query = r.URL.Query().Get("query")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &query
}

func TestExportedAsksForTheExportedTitle(t *testing.T) {
	srv, query := pdb(t, http.StatusOK, `[{"title":"x"}]`)
	if err := Exported(srv.URL, `fleet-export "q"`); err != nil {
		t.Fatalf("Exported = %v, want nil", err)
	}
	want := `["and",["=","exported",true],["=","title","fleet-export \"q\""]]`
	if *query != want {
		t.Errorf("query = %s, want %s", *query, want)
	}
}

func TestExportedDistinguishesNothingFromFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		none   bool
	}{
		{"empty answer", http.StatusOK, `[]`, true},
		{"server error", http.StatusInternalServerError, `[{"title":"x"}]`, false},
		{"not JSON", http.StatusOK, `<html>`, false},
		{"trailing data", http.StatusOK, `[{"title":"x"}] junk`, false},
	} {
		srv, _ := pdb(t, tc.status, tc.body)
		err := Exported(srv.URL, "t")
		if err == nil {
			t.Errorf("%s: Exported = nil, want an error", tc.name)
			continue
		}
		if errors.Is(err, ErrNotExported) != tc.none {
			t.Errorf("%s: Exported = %v; ErrNotExported = %v, want %v", tc.name, err, errors.Is(err, ErrNotExported), tc.none)
		}
	}
}

func TestExportedFailsOnAnUnreachableServer(t *testing.T) {
	srv, _ := pdb(t, http.StatusOK, `[1]`)
	base := srv.URL
	srv.Close()
	if err := Exported(base, "t"); err == nil || errors.Is(err, ErrNotExported) {
		t.Errorf("Exported on a closed server = %v, want a query failure", err)
	}
}

func TestExportedTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	if err := Exported(srv.URL, "t"); err == nil {
		t.Fatal("Exported on a server that never answers returned nil")
	}
	if elapsed := time.Since(start); elapsed > ExportQueryTimeout+2*time.Second {
		t.Errorf("Exported took %v, want about %v", elapsed, ExportQueryTimeout)
	}
}

func TestMutateManifest(t *testing.T) {
	const manifest = "node default {\n  notify { 'a': }\n}\r\n"
	for _, tc := range []struct {
		name, original, want string
		ok                   bool
	}{
		{"one occurrence", "notify { 'a': }", "node default {\n  notify { 'b': }\n}\r\n", true},
		{"absent", "notify { 'z': }", manifest, false},
		{"twice", "e", manifest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "site.pp")
			if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			err := MutateManifest(path, tc.original, "notify { 'b': }")
			if (err == nil) != tc.ok {
				t.Fatalf("MutateManifest error = %v, want ok=%v", err, tc.ok)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Errorf("file = %q, want %q", got, tc.want)
			}
			after, _ := os.Stat(path)
			if !os.SameFile(before, after) {
				t.Error("the manifest was replaced, not rewritten in place; a bind mount would keep the old file")
			}
		})
	}
	t.Run("count message", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "site.pp")
		if err := os.WriteFile(path, []byte("x x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := MutateManifest(path, "x", "y"); err == nil || !strings.Contains(err.Error(), "found 2") {
			t.Errorf("error = %v, want it to say found 2", err)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if err := MutateManifest(filepath.Join(t.TempDir(), "absent.pp"), "x", "y"); err == nil {
			t.Error("MutateManifest on an absent file succeeded")
		}
	})
}
