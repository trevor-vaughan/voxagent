package agent

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/puppetapi"
)

// fakeFileServer serves file_metadatas and file_content the way the
// measured OpenVox server does.
type fakeFileServer struct {
	mu       sync.Mutex
	mounts   map[string][]map[string]any // mount -> entries below the root
	content  map[string][]byte           // "mount/rel" -> body served
	status   map[string]int              // "mount/rel" -> non-200 content status
	requests []string                    // path?rawquery, in order
}

func newFakeFileServer() *fakeFileServer {
	return &fakeFileServer{
		mounts:  map[string][]map[string]any{"plugins": nil, "pluginfacts": nil},
		content: map[string][]byte{},
		status:  map[string]int{},
	}
}

func metaChecksum(typ, value string) map[string]any {
	return map[string]any{"type": typ, "value": value}
}

func (s *fakeFileServer) dir(mount, rel string) {
	s.mounts[mount] = append(s.mounts[mount], map[string]any{
		"path": "/srv/" + mount, "relative_path": rel, "links": "follow", "owner": 0, "group": 0,
		"mode": 493, "type": "directory", "destination": nil,
		"checksum": metaChecksum("ctime", "{ctime}2026-11-16 02:34:01 UTC"),
	})
}

func (s *fakeFileServer) file(mount, rel string, mode int, body string) {
	sum := sha256.Sum256([]byte(body))
	s.mounts[mount] = append(s.mounts[mount], map[string]any{
		"path": "/srv/" + mount, "relative_path": rel, "links": "follow", "owner": 64604, "group": 0,
		"mode": mode, "type": "file", "destination": nil,
		"checksum": metaChecksum("sha256", "{sha256}"+hex.EncodeToString(sum[:])),
	})
	s.content[mount+"/"+rel] = []byte(body)
}

func (s *fakeFileServer) contentRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if strings.HasPrefix(r, "/puppet/v3/file_content/") {
			n++
		}
	}
	return n
}

func (s *fakeFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
	notFound := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found: Could not find file_metadatas ` + r.URL.Path + `","issue_kind":"RESOURCE_NOT_FOUND"}`))
	}
	if mount, ok := strings.CutPrefix(r.URL.Path, "/puppet/v3/file_metadatas/"); ok {
		entries, ok := s.mounts[mount]
		if !ok {
			notFound()
			return
		}
		root := map[string]any{
			"path": "/srv/" + mount, "relative_path": ".", "links": r.URL.Query().Get("links"), "owner": 0, "group": 0,
			"mode": 493, "type": "directory", "destination": nil,
			"checksum": metaChecksum("ctime", "{ctime}2026-11-16 02:34:01 UTC"),
		}
		out := []map[string]any{root}
		if r.URL.Query().Get("recurse") == "true" {
			out = append(out, entries...)
		}
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(out)
		return
	}
	if p, ok := strings.CutPrefix(r.URL.Path, "/puppet/v3/file_content/"); ok {
		if st, ok := s.status[p]; ok {
			w.WriteHeader(st)
			return
		}
		body, ok := s.content[p]
		if !ok {
			notFound()
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
		return
	}
	notFound()
}

// start serves s over TLS and returns a client trusting its certificate.
func (s *fakeFileServer) start(t *testing.T) *puppetapi.Client {
	t.Helper()
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := puppetapi.New("127.0.0.1", port, caPEM, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// targets returns libdir and pluginfactdest, plus an "outside" dir next to
// them that no write may touch.
func targets(t *testing.T) (libdir, factdest, outside string) {
	t.Helper()
	base := t.TempDir()
	libdir, factdest, outside = filepath.Join(base, "lib"), filepath.Join(base, "facts.d"), filepath.Join(base, "outside")
	for _, d := range []string{libdir, factdest, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return libdir, factdest, outside
}

func standardTree(s *fakeFileServer) {
	s.file("pluginfacts", "vox_probe.txt", 0o644, "vox_probe_external=from_pluginfacts\n")
	s.file("pluginfacts", "exec.sh", 0o750, "#!/bin/sh\necho a=b\n")
	s.file("pluginfacts", "setuid.sh", 0o4755, "#!/bin/sh\necho c=d\n")
	s.dir("plugins", "puppet")
	s.dir("plugins", "puppet/type")
	s.file("plugins", "puppet/type/vox_marker.rb", 0o755, "Puppet::Type.newtype(:vox_marker) {}\n")
}

func assertFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s: mode %v, want a regular file", p, fi.Mode())
	}
	if got := fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky); got != mode {
		t.Errorf("%s: mode %v, want %v", p, got, mode)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != body {
		t.Errorf("%s: content %q, want %q", p, b, body)
	}
}

func assertDir(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o755 {
		t.Errorf("%s: mode %v, want a 0755 directory", p, fi.Mode())
	}
}

// listTree returns every path under dir with its mode and content, to show
// that a refused sync changed nothing.
func listTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + string(b)
		}
		out[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPluginsyncWritesTreeAndModes(t *testing.T) {
	s := newFakeFileServer()
	standardTree(s)
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(factdest, "vox_probe.txt"), "vox_probe_external=from_pluginfacts\n", 0o644)
	assertFile(t, filepath.Join(factdest, "exec.sh"), "#!/bin/sh\necho a=b\n", 0o755)
	// setuid on the server: the execute bit is kept, the setuid bit never.
	assertFile(t, filepath.Join(factdest, "setuid.sh"), "#!/bin/sh\necho c=d\n", 0o755)
	assertDir(t, filepath.Join(libdir, "puppet"))
	assertDir(t, filepath.Join(libdir, "puppet/type"))
	// plugins: source_permissions=ignore, so 0644 whatever the server says.
	assertFile(t, filepath.Join(libdir, "puppet/type/vox_marker.rb"), "Puppet::Type.newtype(:vox_marker) {}\n", 0o644)
}

// Modes are set explicitly, so the process umask does not change them.
func TestPluginsyncModesIndependentOfUmask(t *testing.T) {
	var trees []map[string]string
	for _, mask := range []int{0, 0o077} {
		s := newFakeFileServer()
		standardTree(s)
		c := s.start(t)
		libdir, factdest, _ := targets(t)
		old := syscall.Umask(mask)
		err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
		syscall.Umask(old)
		if err != nil {
			t.Fatal(err)
		}
		tree := map[string]string{}
		for p, v := range listTree(t, filepath.Dir(libdir)) {
			tree[strings.TrimPrefix(p, filepath.Dir(libdir))] = v
		}
		trees = append(trees, tree)
	}
	if !reflect.DeepEqual(trees[0], trees[1]) {
		t.Errorf("umask 0 tree %v\numask 077 tree %v", trees[0], trees[1])
	}
	if got := trees[1]["/lib/puppet/type"]; !strings.HasPrefix(got, "drwxr-xr-x") {
		t.Errorf("under umask 077 lib/puppet/type is %q, want drwxr-xr-x", got)
	}
}

func TestPluginsyncSkipsUnchanged(t *testing.T) {
	s := newFakeFileServer()
	standardTree(s)
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	first := s.contentRequests()
	if first != 4 {
		t.Fatalf("first run made %d file_content requests, want 4", first)
	}
	// A mode drift alone is repaired without fetching content.
	rb := filepath.Join(libdir, "puppet/type/vox_marker.rb")
	if err := os.Chmod(rb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	if n := s.contentRequests() - first; n != 0 {
		t.Errorf("second run made %d file_content requests, want 0", n)
	}
	assertFile(t, rb, "Puppet::Type.newtype(:vox_marker) {}\n", 0o644)
}

func TestPluginsyncUpdatesChangedContent(t *testing.T) {
	s := newFakeFileServer()
	s.file("plugins", "a.rb", 0o644, "new\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	if err := os.WriteFile(filepath.Join(libdir, "a.rb"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(libdir, "a.rb"), "new\n", 0o644)
}

func TestPluginsyncPurges(t *testing.T) {
	s := newFakeFileServer()
	s.dir("plugins", "keep")
	s.file("plugins", "keep/k.rb", 0o644, "k\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	for _, d := range []string{"keep", "gone/deep"} {
		if err := os.MkdirAll(filepath.Join(libdir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(libdir, "old.rb"), filepath.Join(libdir, "keep/stale.rb"),
		filepath.Join(libdir, "gone/deep/x.rb"), filepath.Join(factdest, "stale.txt")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(libdir, "keep/k.rb"), "k\n", 0o644)
	for _, p := range []string{"old.rb", "keep/stale.rb", "gone"} {
		assertAbsent(t, filepath.Join(libdir, p))
	}
	assertAbsent(t, filepath.Join(factdest, "stale.txt"))
}

// Puppet's recursion skips pluginsignore names at every level, so purge never
// removes a local .git (or .svn, CVS, .hg) and leaves its contents alone.
func TestPluginsyncPurgeKeepsIgnoredNames(t *testing.T) {
	s := newFakeFileServer()
	s.dir("plugins", "keep")
	s.file("plugins", "keep/k.rb", 0o644, "k\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	for _, d := range []string{".git", "keep/.git"} {
		if err := os.MkdirAll(filepath.Join(libdir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(libdir, d, "HEAD"), []byte("ref\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(libdir, "keep/CVS"), []byte("cvs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libdir, "old.rb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(libdir, ".git/HEAD"), "ref\n", 0o644)
	assertFile(t, filepath.Join(libdir, "keep/.git/HEAD"), "ref\n", 0o644)
	assertFile(t, filepath.Join(libdir, "keep/CVS"), "cvs\n", 0o644)
	assertAbsent(t, filepath.Join(libdir, "old.rb"))
}

// A 404 for a mount's metadata, on the probe or on a recursive request, fails
// closed: an error, no purge, the local trees untouched.
func TestPluginsyncMissingMountFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		mount, recurse string
	}{
		{"plugins", "false"},
		{"pluginfacts", "true"},
	} {
		t.Run(tc.mount, func(t *testing.T) {
			s := newFakeFileServer()
			delete(s.mounts, tc.mount)
			c := s.start(t)
			libdir, factdest, _ := targets(t)
			for _, f := range []string{filepath.Join(libdir, "old.rb"), filepath.Join(factdest, "old.txt")} {
				if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := listTree(t, filepath.Dir(libdir))
			err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
			var he *puppetapi.HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusNotFound {
				t.Fatalf("err = %v, want the 404 as an error", err)
			}
			if after := listTree(t, filepath.Dir(libdir)); !reflect.DeepEqual(before, after) {
				t.Errorf("tree changed: before %v after %v", before, after)
			}
			last, err := url.Parse(s.requests[len(s.requests)-1])
			if err != nil {
				t.Fatal(err)
			}
			if last.Path != "/puppet/v3/file_metadatas/"+tc.mount || last.Query().Get("recurse") != tc.recurse {
				t.Errorf("last request = %q, want the failing %s metadata request (recurse=%s)", last, tc.mount, tc.recurse)
			}
		})
	}
}

func TestPluginsyncChecksumMismatchRefused(t *testing.T) {
	s := newFakeFileServer()
	s.file("plugins", "a.rb", 0o644, "expected\n")
	s.content["plugins/a.rb"] = []byte("tampered\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	if err := os.WriteFile(filepath.Join(libdir, "a.rb"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, libdir)
	err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match the metadata checksum") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if after := listTree(t, libdir); !reflect.DeepEqual(before, after) {
		t.Errorf("libdir changed: before %v after %v", before, after)
	}
}

// Fail closed: a content failure stops the sync before that mount's purge and
// before the next mount.
func TestPluginsyncContentErrorStopsBeforePurge(t *testing.T) {
	s := newFakeFileServer()
	s.file("pluginfacts", "a.txt", 0o644, "a=b\n")
	s.status["pluginfacts/a.txt"] = http.StatusInternalServerError
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	stale := filepath.Join(factdest, "stale.txt")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err == nil {
		t.Fatal("want an error")
	}
	assertFile(t, stale, "x", 0o644)
	for _, r := range s.requests {
		if strings.HasPrefix(r, "/puppet/v3/file_metadatas/plugins?recurse=true") {
			t.Errorf("plugins requested after a pluginfacts failure: %q", r)
		}
	}
}

func TestPluginsyncRefusesTraversal(t *testing.T) {
	for _, rel := range []string{"../../evil", "/etc/evil", "a/../../evil"} {
		t.Run(rel, func(t *testing.T) {
			s := newFakeFileServer()
			s.file("plugins", rel, 0o644, "pwned\n")
			c := s.start(t)
			libdir, factdest, outside := targets(t)
			before := listTree(t, filepath.Dir(libdir))
			err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
			if err == nil || !strings.Contains(err.Error(), "segment") {
				t.Fatalf("err = %v, want a relative_path refusal", err)
			}
			if after := listTree(t, filepath.Dir(libdir)); !reflect.DeepEqual(before, after) {
				t.Errorf("tree changed: before %v after %v", before, after)
			}
			assertAbsent(t, filepath.Join(outside, "evil"))
			assertAbsent(t, "/etc/evil")
		})
	}
}

func TestPluginsyncRefusesLinkMetadata(t *testing.T) {
	s := newFakeFileServer()
	s.mounts["plugins"] = append(s.mounts["plugins"], map[string]any{
		"path": "/srv/plugins", "relative_path": "l.rb", "links": "follow", "owner": 0, "group": 0,
		"mode": 511, "type": "link", "destination": "/etc/shadow",
		"checksum": metaChecksum("sha256", "{sha256}"+strings.Repeat("0", 64)),
	})
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
	if err == nil || !strings.Contains(err.Error(), "link metadata refused") {
		t.Fatalf("err = %v, want link metadata refused", err)
	}
	assertAbsent(t, filepath.Join(libdir, "l.rb"))
}

func TestPluginsyncRefusesWeakChecksums(t *testing.T) {
	for _, ck := range []map[string]any{
		metaChecksum("md5", "{md5}d41d8cd98f00b204e9800998ecf8427e"),
		metaChecksum("mtime", "{mtime}2026-11-16 02:34:01 UTC"),
		metaChecksum("none", "{none}"),
		metaChecksum("sha256", "{sha256}abcd"),
		metaChecksum("sha256", strings.Repeat("0", 64)),
	} {
		t.Run(ck["value"].(string), func(t *testing.T) {
			s := newFakeFileServer()
			s.file("plugins", "a.rb", 0o644, "a\n")
			s.mounts["plugins"][0]["checksum"] = ck
			c := s.start(t)
			libdir, factdest, _ := targets(t)
			if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err == nil {
				t.Fatal("want a checksum refusal")
			}
			assertAbsent(t, filepath.Join(libdir, "a.rb"))
			if n := s.contentRequests(); n != 0 {
				t.Errorf("%d file_content requests, want 0", n)
			}
		})
	}
}

// sha512 is accepted as a type, and its value is enforced.
func TestPluginsyncSHA512(t *testing.T) {
	for _, tc := range []struct {
		name, hashed string
		ok           bool
	}{{"match", "a\n", true}, {"mismatch", "b\n", false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeFileServer()
			s.file("plugins", "a.rb", 0o644, "a\n")
			sum := sha512.Sum512([]byte(tc.hashed))
			s.mounts["plugins"][0]["checksum"] = metaChecksum("sha512", "{sha512}"+hex.EncodeToString(sum[:]))
			c := s.start(t)
			libdir, factdest, _ := targets(t)
			err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				assertFile(t, filepath.Join(libdir, "a.rb"), "a\n", 0o644)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "does not match the metadata checksum") {
				t.Fatalf("err = %v, want a sha512 mismatch", err)
			}
			assertAbsent(t, filepath.Join(libdir, "a.rb"))
		})
	}
}

func TestPluginsyncRefusesUnlistedParent(t *testing.T) {
	s := newFakeFileServer()
	s.file("plugins", "puppet/type/a.rb", 0o644, "a\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil)
	if err == nil || !strings.Contains(err.Error(), "not listed as a directory") {
		t.Fatalf("err = %v, want an unlisted-parent refusal", err)
	}
}

// A symlink planted in libdir where a directory or file is expected is
// replaced, never followed: nothing outside the root changes.
func TestPluginsyncReplacesPlantedSymlinks(t *testing.T) {
	s := newFakeFileServer()
	s.dir("plugins", "puppet")
	s.file("plugins", "puppet/x.rb", 0o644, "x\n")
	s.file("plugins", "a.rb", 0o644, "a\n")
	c := s.start(t)
	libdir, factdest, outside := targets(t)
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(libdir, "puppet")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(libdir, "a.rb")); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, outside)
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	if after := listTree(t, outside); !reflect.DeepEqual(before, after) {
		t.Errorf("outside changed: before %v after %v", before, after)
	}
	assertDir(t, filepath.Join(libdir, "puppet"))
	assertFile(t, filepath.Join(libdir, "puppet/x.rb"), "x\n", 0o644)
	assertFile(t, filepath.Join(libdir, "a.rb"), "a\n", 0o644)
}

// Purge removes a planted symlink itself and never descends through it.
func TestPluginsyncPurgeDoesNotFollowSymlinks(t *testing.T) {
	s := newFakeFileServer()
	c := s.start(t)
	libdir, factdest, outside := targets(t)
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(libdir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "victim"), filepath.Join(libdir, "filelink")); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, outside)
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	if after := listTree(t, outside); !reflect.DeepEqual(before, after) {
		t.Errorf("outside changed: before %v after %v", before, after)
	}
	assertAbsent(t, filepath.Join(libdir, "dirlink"))
	assertAbsent(t, filepath.Join(libdir, "filelink"))
}

// Puppet's order and query parameters.
func TestPluginsyncRequestOrder(t *testing.T) {
	s := newFakeFileServer()
	s.file("pluginfacts", "f.txt", 0o644, "f=1\n")
	s.file("plugins", "p.rb", 0o644, "p\n")
	c := s.start(t)
	libdir, factdest, _ := targets(t)
	if err := Pluginsync(context.Background(), c, "production", libdir, factdest, nil); err != nil {
		t.Fatal(err)
	}
	recursive := func(mount, perms string) string {
		return "/puppet/v3/file_metadatas/" + mount + "?" + url.Values{
			"recurse": {"true"}, "max_files": {"-1"}, "ignore": {".svn", "CVS", ".git", ".hg"},
			"links": {"follow"}, "checksum_type": {"sha256"}, "source_permissions": {perms}, "environment": {"production"},
		}.Encode()
	}
	want := []string{
		"/puppet/v3/file_metadatas/plugins?" + url.Values{
			"recurse": {"false"}, "links": {"manage"}, "checksum_type": {"sha256"},
			"source_permissions": {"ignore"}, "environment": {"production"},
		}.Encode(),
		recursive("pluginfacts", "use"),
		"/puppet/v3/file_content/pluginfacts/f.txt?environment=production",
		recursive("plugins", "ignore"),
		"/puppet/v3/file_content/plugins/p.rb?environment=production",
	}
	if len(s.requests) != len(want) {
		t.Fatalf("requests = %q, want %q", s.requests, want)
	}
	for i := range want {
		got, err := url.Parse(s.requests[i])
		if err != nil {
			t.Fatal(err)
		}
		w, _ := url.Parse(want[i])
		if got.Path != w.Path || !reflect.DeepEqual(got.Query(), w.Query()) {
			t.Errorf("request %d = %q, want %q", i, s.requests[i], want[i])
		}
	}
}
