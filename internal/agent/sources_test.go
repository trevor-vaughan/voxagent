package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/units/file"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/puppetapi"
)

const testServer = "puppet.example"

// sourceServer adds file_metadata (singular) to fakeFileServer, which serves
// file_metadatas and file_content: what a File resource's puppet: source asks
// for.
type sourceServer struct {
	*fakeFileServer
	single map[string]map[string]any // mount path -> metadata
}

func newSourceServer() *sourceServer {
	f := newFakeFileServer()
	f.mounts = map[string][]map[string]any{}
	return &sourceServer{fakeFileServer: f, single: map[string]map[string]any{}}
}

// file serves body at mountPath as a single file.
func (s *sourceServer) file(mountPath, body string) {
	sum := sha256.Sum256([]byte(body))
	s.single[mountPath] = map[string]any{
		"path": "/srv/" + mountPath, "relative_path": nil, "links": "manage", "owner": 0, "group": 0,
		"mode": 0o644, "type": "file", "destination": nil,
		"checksum": metaChecksum("sha256", "{sha256}"+hex.EncodeToString(sum[:])),
	}
	s.content[mountPath] = []byte(body)
}

// tree makes mountPath a directory, served recursively by file_metadatas.
func (s *sourceServer) tree(mountPath string) {
	s.mounts[mountPath] = []map[string]any{}
}

func (s *sourceServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := strings.CutPrefix(r.URL.Path, "/puppet/v3/file_metadata/")
	var many bool
	if !ok {
		// file_metadatas of a single file lists the file as the root.
		p, many = strings.CutPrefix(r.URL.Path, "/puppet/v3/file_metadatas/")
		if _, isTree := s.mounts[p]; !many || isTree {
			s.fakeFileServer.ServeHTTP(w, r)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
	m, ok := s.single[p]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found: Could not find file_metadata ` + p + `","issue_kind":"RESOURCE_NOT_FOUND"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json;charset=utf-8")
	var body any = m
	if many {
		root := maps.Clone(m)
		root["relative_path"] = "."
		body = []map[string]any{root}
	}
	json.NewEncoder(w).Encode(body)
}

func (s *sourceServer) requestList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// startSources serves s over TLS and returns a client trusting its
// certificate.
func startSources(t *testing.T, s *sourceServer) *puppetapi.Client {
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

func fileResource(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "File", Title: title, Parameters: params}
}

func catalogOf(rs ...catalog.Resource) *catalog.Catalog {
	return &catalog.Catalog{Environment: "production", Resources: rs}
}

// sourcesDir is the run-private directory the caller makes with
// os.MkdirTemp, inside a parent that holds nothing else.
func sourcesDir(t *testing.T) (parent, dir string) {
	t.Helper()
	parent = t.TempDir()
	dir, err := os.MkdirTemp(parent, "sources-")
	if err != nil {
		t.Fatal(err)
	}
	return parent, dir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assertInside fails unless p is strictly below dir.
func assertInside(t *testing.T, p, dir string) {
	t.Helper()
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		t.Errorf("%s is not inside %s", p, dir)
	}
}

func TestFetchSourcesFile(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(
		fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f"}),
		fileResource("/etc/f2", map[string]any{"source": "puppet:///modules/m/f", "links": "manage"}),
		fileResource("/etc/local", map[string]any{"source": "/srv/local"}),
		catalog.Resource{Type: "Notify", Title: "x", Parameters: map[string]any{"source": "puppet:///modules/m/nope"}},
	)
	m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("map = %v, want only the puppet: source of a File", m)
	}
	local := m["puppet:///modules/m/f"]
	assertInside(t, local, dir)
	if got := readFile(t, local); got != "served\n" {
		t.Errorf("content = %q", got)
	}
	want := []string{
		"/puppet/v3/file_metadata/modules/m/f?checksum_type=sha256&environment=production&links=manage&source_permissions=ignore",
		"/puppet/v3/file_content/modules/m/f?environment=production",
	}
	if got := s.requestList(); !reflect.DeepEqual(got, want) {
		t.Errorf("requests = %q\nwant %q", got, want)
	}
}

// links => follow is passed through: the server resolves the link.
func TestFetchSourcesFileLinksFollow(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f", "links": "follow"}))
	if _, err := FetchSources(context.Background(), c, cat, "production", testServer, dir); err != nil {
		t.Fatal(err)
	}
	if got := s.requestList(); len(got) == 0 || !strings.Contains(got[0], "links=follow") {
		t.Errorf("requests = %q, want links=follow on the metadata request", got)
	}
}

func TestFetchSourcesRecurse(t *testing.T) {
	s := newSourceServer()
	const mount = "modules/m/tree"
	s.tree(mount)
	s.fakeFileServer.file(mount, "a", 0o644, "A\n")
	s.fakeFileServer.dir(mount, "sub")
	s.fakeFileServer.file(mount, "sub/b", 0o755, "B\n")
	s.fakeFileServer.dir(mount, "empty")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	const uri = "puppet:///modules/m/tree"
	cat := catalogOf(
		fileResource("/etc/tree", map[string]any{"source": uri, "recurse": true}),
		// The same source without recurse is served from the same tree.
		fileResource("/etc/tree2", map[string]any{"source": uri}),
	)
	m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	local := m[uri]
	assertInside(t, local, dir)
	if got := readFile(t, filepath.Join(local, "a")); got != "A\n" {
		t.Errorf("a = %q", got)
	}
	if got := readFile(t, filepath.Join(local, "sub/b")); got != "B\n" {
		t.Errorf("sub/b = %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(local, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty: %v, %v; want a directory", fi, err)
	}
	want := []string{
		"/puppet/v3/file_metadatas/modules/m/tree?checksum_type=sha256&environment=production&links=manage&max_files=0&recurse=true&source_permissions=ignore",
		"/puppet/v3/file_content/modules/m/tree/a?environment=production",
		"/puppet/v3/file_content/modules/m/tree/sub/b?environment=production",
	}
	if got := s.requestList(); !reflect.DeepEqual(got, want) {
		t.Errorf("requests = %q\nwant %q", got, want)
	}
}

// A recursed source that is a file is fetched as that file.
func TestFetchSourcesRecurseOnFile(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f", "recurse": true}))
	m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, m["puppet:///modules/m/f"]); got != "served\n" {
		t.Errorf("content = %q", got)
	}
}

func TestFetchSourcesOtherHostRefused(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	for _, uri := range []string{
		"puppet://other.example/modules/m/f",
		"puppet://" + testServer + ":8140/modules/m/f",
		"puppet://user@" + testServer + "/modules/m/f",
	} {
		_, dir := sourcesDir(t)
		cat := catalogOf(fileResource("/etc/f", map[string]any{"source": []any{"puppet:///modules/m/f", uri}}))
		_, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
		if err == nil || !strings.Contains(err.Error(), uri) {
			t.Errorf("%s: err = %v, want a refusal naming the source", uri, err)
		}
	}
	if got := s.requestList(); len(got) != 0 {
		t.Errorf("requests = %q, want none: every source is checked before any fetch", got)
	}

	// The server's own name, in any case, is the server.
	for _, uri := range []string{"puppet://" + testServer + "/modules/m/f", "puppet://PUPPET.example/modules/m/f"} {
		_, dir := sourcesDir(t)
		cat := catalogOf(fileResource("/etc/f", map[string]any{"source": uri}))
		m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if got := readFile(t, m[uri]); got != "served\n" {
			t.Errorf("%s: content = %q", uri, got)
		}
	}
}

func TestFetchSourcesMissingIsMappedToAbsent(t *testing.T) {
	s := newSourceServer()
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(
		fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/missing"}),
		fileResource("/etc/tree", map[string]any{"source": "puppet:///modules/m/missingtree", "recurse": true}),
	)
	m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("map = %v, want both sources mapped", m)
	}
	for uri, local := range m {
		assertInside(t, local, dir)
		if _, err := os.Lstat(local); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s -> %s: Lstat err = %v, want not-exist", uri, local, err)
		}
	}
}

// A non-404 failure is an error, not a missing source.
func TestFetchSourcesServerErrorFails(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	s.status["modules/m/f"] = http.StatusInternalServerError
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f"}))
	if _, err := FetchSources(context.Background(), c, cat, "production", testServer, dir); err == nil {
		t.Fatal("err = nil, want the content failure")
	}
}

func TestFetchSourcesTraversalRefused(t *testing.T) {
	for _, rel := range []string{"../escape", "a/../../escape", "/abs", "a\\b", "a//b"} {
		s := newSourceServer()
		s.tree("modules/m/tree")
		s.fakeFileServer.file("modules/m/tree", rel, 0o644, "evil\n")
		c := startSources(t, s)
		parent, dir := sourcesDir(t)
		cat := catalogOf(fileResource("/etc/tree", map[string]any{"source": "puppet:///modules/m/tree", "recurse": true}))
		_, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
		if err == nil {
			t.Errorf("%q: err = nil, want a refusal", rel)
		}
		if des, _ := os.ReadDir(parent); len(des) != 1 {
			t.Errorf("%q: %s holds %d entries, want only the sources dir", rel, parent, len(des))
		}
		if des, _ := os.ReadDir(dir); len(des) != 0 {
			t.Errorf("%q: sources dir holds %v, want nothing written", rel, des)
		}
	}

	// The source URI's own path is a mount path and is checked the same way.
	s := newSourceServer()
	c := startSources(t, s)
	for _, uri := range []string{"puppet:///modules/../../etc/shadow", "puppet:///", "puppet:///modules//m"} {
		_, dir := sourcesDir(t)
		cat := catalogOf(fileResource("/etc/f", map[string]any{"source": uri}))
		if _, err := FetchSources(context.Background(), c, cat, "production", testServer, dir); err == nil || !strings.Contains(err.Error(), uri) {
			t.Errorf("%s: err = %v, want a refusal naming the source", uri, err)
		}
	}
	if got := s.requestList(); len(got) != 0 {
		t.Errorf("requests = %q, want none", got)
	}
}

func TestFetchSourcesWritesNoSymlinks(t *testing.T) {
	link := map[string]any{
		"path": "/srv/x", "relative_path": "ln", "links": "manage", "owner": 0, "group": 0,
		"mode": 0o777, "type": "link", "destination": "/etc/shadow",
		"checksum": metaChecksum("sha256", "{sha256}"+strings.Repeat("0", 64)),
	}

	// A link inside a recursed tree.
	s := newSourceServer()
	s.tree("modules/m/tree")
	s.fakeFileServer.file("modules/m/tree", "a", 0o644, "A\n")
	s.mounts["modules/m/tree"] = append(s.mounts["modules/m/tree"], link)
	// A single source that is a link.
	s.single["modules/m/ln"] = map[string]any{
		"path": "/srv/ln", "relative_path": nil, "links": "manage", "owner": 0, "group": 0,
		"mode": 0o777, "type": "link", "destination": "/etc/shadow",
		"checksum": metaChecksum("sha256", "{sha256}"+strings.Repeat("0", 64)),
	}
	c := startSources(t, s)
	for _, r := range []catalog.Resource{
		fileResource("/etc/tree", map[string]any{"source": "puppet:///modules/m/tree", "recurse": true}),
		fileResource("/etc/ln", map[string]any{"source": "puppet:///modules/m/ln"}),
	} {
		_, dir := sourcesDir(t)
		_, err := FetchSources(context.Background(), c, catalogOf(r), "production", testServer, dir)
		if err == nil || !strings.Contains(err.Error(), "link") {
			t.Errorf("%s: err = %v, want a link refusal", r.Ref(), err)
		}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type()&fs.ModeSymlink != 0 {
				t.Errorf("%s: %s is a symlink", r.Ref(), p)
			}
			return nil
		})
	}
}

// Keys are the catalog's strings, element by element: ral looks a source up
// by its exact text, so a trailing "/" is kept in the key though the request
// drops it.
func TestFetchSourcesKeysAreRawCatalogStrings(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "F\n")
	s.file("modules/m/g", "G\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	cat := catalogOf(fileResource("/etc/f", map[string]any{
		"source": []any{"puppet:///modules/m/f", "puppet:///modules/m/g/"},
	}))
	m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"puppet:///modules/m/f", "puppet:///modules/m/g/"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}
	if got := readFile(t, m["puppet:///modules/m/g/"]); got != "G\n" {
		t.Errorf("g/ content = %q", got)
	}
	if m["puppet:///modules/m/f"] == m["puppet:///modules/m/g/"] {
		t.Error("two sources share one local path")
	}
}

func TestFetchSourcesChecksumMismatch(t *testing.T) {
	for name, checksum := range map[string]map[string]any{
		"wrong digest": metaChecksum("sha256", "{sha256}"+strings.Repeat("ab", 32)),
		"md5 refused":  metaChecksum("md5", "{md5}d41d8cd98f00b204e9800998ecf8427e"),
		"mtime":        metaChecksum("mtime", "{mtime}2026-11-16 02:34:01 UTC"),
	} {
		s := newSourceServer()
		s.file("modules/m/f", "served\n")
		s.single["modules/m/f"]["checksum"] = checksum
		c := startSources(t, s)
		_, dir := sourcesDir(t)
		cat := catalogOf(fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f"}))
		if _, err := FetchSources(context.Background(), c, cat, "production", testServer, dir); err == nil {
			t.Errorf("%s: err = nil, want a refusal", name)
		}
		if des, _ := os.ReadDir(dir); len(des) != 0 {
			t.Errorf("%s: sources dir holds %v, want the rejected content removed", name, des)
		}
	}
}

// Modes are set, not left to the umask: 0777 would leave Mkdir and OpenFile
// with no permission bits at all.
func TestFetchSourcesModes(t *testing.T) {
	for _, mask := range []int{0, 0o777} {
		s := newSourceServer()
		s.file("modules/m/f", "F\n")
		s.tree("modules/m/tree")
		s.fakeFileServer.dir("modules/m/tree", "sub")
		s.fakeFileServer.file("modules/m/tree", "sub/b", 0o4777, "B\n")
		c := startSources(t, s)
		_, dir := sourcesDir(t)
		cat := catalogOf(
			fileResource("/etc/f", map[string]any{"source": "puppet:///modules/m/f"}),
			fileResource("/etc/tree", map[string]any{"source": "puppet:///modules/m/tree", "recurse": true}),
		)
		old := syscall.Umask(mask)
		m, err := FetchSources(context.Background(), c, cat, "production", testServer, dir)
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("umask %04o: %v", mask, err)
		}
		tree := m["puppet:///modules/m/tree"]
		for p, want := range map[string]os.FileMode{
			m["puppet:///modules/m/f"]:      0o600,
			tree:                            fs.ModeDir | 0o700,
			filepath.Join(tree, "sub"):      fs.ModeDir | 0o700,
			filepath.Join(tree, "sub", "b"): 0o600,
		} {
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode() & (fs.ModeType | fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != want {
				t.Errorf("umask %04o: %s: mode %v, want %v", mask, p, got, want)
			}
		}
	}
}

// End to end: the map FetchSources returns is what file.File reads, and the
// provider's messages name the source as the catalog wrote it.
func TestFetchSourcesAppliedByRal(t *testing.T) {
	s := newSourceServer()
	s.file("modules/m/f", "served\n")
	c := startSources(t, s)
	_, dir := sourcesDir(t)
	target := filepath.Join(t.TempDir(), "f")
	missingTarget := filepath.Join(t.TempDir(), "g")
	present := fileResource(target, map[string]any{"source": "puppet:///modules/m/f"})
	missing := fileResource(missingTarget, map[string]any{"source": "puppet:///modules/m/missing"})
	m, err := FetchSources(context.Background(), c, catalogOf(present, missing), "production", testServer, dir)
	if err != nil {
		t.Fatal(err)
	}
	f := file.File{Environment: "production", Sources: m}
	for _, r := range []catalog.Resource{present, missing} {
		if err := f.Validate(r); err != nil {
			t.Fatalf("Validate %s: %v", r.Ref(), err)
		}
	}
	changes, _, err := f.Apply(context.Background(), present, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readFile(t, target); got != "served\n" {
		t.Errorf("content = %q", got)
	}
	if len(changes) != 1 || changes[0].Name != "file_created" {
		t.Errorf("changes = %+v, want one file_created", changes)
	}
	_, _, err = f.Apply(context.Background(), missing, false)
	want := "Could not retrieve information from environment production source(s) puppet:///modules/m/missing"
	if err == nil || err.Error() != want {
		t.Errorf("missing: err = %v\nwant %q", err, want)
	}
}

// Every refusal and failure of a marked source names it [redacted], and no
// part of it shows; an unmarked one is named as written (D-78).
func TestFetchSourcesErrorsRedactMarkedSource(t *testing.T) {
	for _, c := range []struct {
		name, uri string
		params    map[string]any
		other     *catalog.Resource // a second File sharing the source
	}{
		{name: "fetch fails", uri: "puppet:///modules/m/D8SECRET"},
		{name: "escaped fetch fails", uri: "puppet:///modules/m/D8 SECRET"},
		{name: "tree fetch fails", uri: "puppet:///modules/m/D8SECRET", params: map[string]any{"recurse": true}},
		{name: "other host", uri: "puppet://D8SECRET.example/modules/m/f"},
		{name: "not a mount path", uri: "puppet:///modules/../D8SECRET"},
		{name: "links", uri: "puppet:///modules/m/D8SECRET", params: map[string]any{"links": "ignore"}},
		{name: "links differ", uri: "puppet:///modules/m/D8SECRET", params: map[string]any{"links": "follow"},
			other: &catalog.Resource{Type: "File", Title: "/etc/g", Parameters: map[string]any{"source": "puppet:///modules/m/D8SECRET"}}},
	} {
		for _, marked := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/marked=%v", c.name, marked), func(t *testing.T) {
				s := newSourceServer()
				s.file("modules/m/D8SECRET", "served\n")
				s.status["modules/m/D8SECRET"] = http.StatusInternalServerError
				// Its content is missing: the server's 404 message names the path.
				s.file("modules/m/D8 SECRET", "served\n")
				delete(s.content, "modules/m/D8 SECRET")
				params := map[string]any{"source": c.uri}
				maps.Copy(params, c.params)
				r := fileResource("/etc/f", params)
				if marked {
					r.SensitiveParameters = []string{"source"}
				}
				rs := []catalog.Resource{r}
				if c.other != nil {
					rs = append([]catalog.Resource{*c.other}, rs...)
				}
				_, dir := sourcesDir(t)
				_, err := FetchSources(context.Background(), startSources(t, s), catalogOf(rs...), "production", testServer, dir)
				if err == nil {
					t.Fatal("err = nil, want a failure")
				}
				if marked {
					if strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "source [redacted]") {
						t.Errorf("err = %v, want the source [redacted] and no part of it", err)
					}
				} else if !strings.Contains(err.Error(), "source "+c.uri) {
					t.Errorf("err = %v, want the source named", err)
				}
			})
		}
	}
}

// An empty mount path has nothing to hide, and the refusal keeps its words.
func TestFetchSourcesMarkedEmptyMountPath(t *testing.T) {
	r := fileResource("/etc/f", map[string]any{"source": "puppet:///"})
	r.SensitiveParameters = []string{"source"}
	_, dir := sourcesDir(t)
	_, err := FetchSources(context.Background(), startSources(t, newSourceServer()), catalogOf(r), "production", testServer, dir)
	want := `File[/etc/f]: source [redacted]: not a mount path: relative_path "" has an empty, "." or ".." segment`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}
