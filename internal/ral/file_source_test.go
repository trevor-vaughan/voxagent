package ral

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// source is read locally: a path or a file: URI. puppet:, http: and https:
// need a transport voxagent does not have, so they are porting
// gaps. recurse and purge are booleans; recurse => remote and Puppet's other
// boolean spellings are refused rather than guessed at.
func TestFileSupportsValueBoundsSourceRecursePurge(t *testing.T) {
	for _, tc := range []struct {
		attr, value string
		want        bool
	}{
		{"source", "/srv/app.conf", true},
		{"source", "file:///srv/app.conf", true},
		{"source", "file:/srv/app.conf", true},
		// Invalid in Puppet too, so Validate refuses it in Puppet's words
		// rather than pre-flight calling it a porting gap.
		{"source", "srv/app.conf", true},
		{"source", "puppet:///modules/app/app.conf", false},
		{"source", "http://example.com/app.conf", false},
		{"source", "https://example.com/app.conf", false},
		{"source", "ftp://example.com/app.conf", false},
		{"recurse", "true", true},
		{"recurse", "false", true},
		{"recurse", "remote", false},
		{"recurse", "yes", false},
		{"recurse", "2", false},
		{"purge", "true", true},
		{"purge", "false", true},
		{"purge", "yes", false},
	} {
		if got := (File{}).SupportsValue(tc.attr, tc.value); got != tc.want {
			t.Errorf("SupportsValue(%q, %q) = %v, want %v", tc.attr, tc.value, got, tc.want)
		}
	}
	if got := (File{}).ListAttrs().Sorted(); !slices.Equal(got, []string{"source"}) {
		t.Errorf("ListAttrs = %v, want [source]", got)
	}
}

// type/file/source.rb:86-103 through type.rb:692's parameter wrapper, the
// creator rule of type/file.rb:428-435 through type.rb:2360's type wrapper,
// and voxagent's own two: a file: URI with a host, and content with recurse.
func TestFileValidateSourceAndCreators(t *testing.T) {
	const ref = "File[/etc/app.conf]"
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"absolute path", map[string]any{"source": "/srv/a"}, ""},
		{"file URI", map[string]any{"source": "file:///srv/a"}, ""},
		{"list of both", map[string]any{"source": []any{"/srv/a", "file:/srv/b"}}, ""},
		{"relative", map[string]any{"source": "srv/a"},
			"Parameter source failed on " + ref + ": Cannot use relative URLs 'srv/a'"},
		{"opaque", map[string]any{"source": "file:srv/a"},
			"Parameter source failed on " + ref + ": Cannot use opaque URLs 'file:srv/a'"},
		{"host", map[string]any{"source": "file://fs1/srv/a"},
			"Parameter source failed on " + ref + ": file: URI 'file://fs1/srv/a' names host 'fs1'; only a local file: URI is implemented"},
		{"bad element in a list", map[string]any{"source": []any{"/srv/a", "srv/b"}},
			"Parameter source failed on " + ref + ": Cannot use relative URLs 'srv/b'"},
		{"unparseable", map[string]any{"source": "file:///srv/\x01"},
			"Parameter source failed on " + ref + ": Could not understand source file:///srv/\x01: " +
				`parse "file:///srv/\x01": net/url: invalid control character in URL`},
		// Puppet escapes % before parsing (Request#set_uri_key), so an
		// invalid escape is a literal path, not a parse error.
		{"percent is literal", map[string]any{"source": "file:///srv/%zz"}, ""},
		{"content and source", map[string]any{"content": "x", "source": "/srv/a"},
			"Validation of " + ref + " failed: You cannot specify more than one of content, source, target"},
		{"content and recurse", map[string]any{"content": "x", "recurse": true},
			ref + ": content with recurse => true is not implemented: every file in the tree would get the same content"},
		{"follow and recurse", map[string]any{"links": "follow", "recurse": true, "source": "/srv/a"},
			ref + ": links => follow with recurse => true is not implemented: Puppet's walk follows a linked directory and copies its tree"},
		{"follow without recurse", map[string]any{"links": "follow", "source": "/srv/a"}, ""},
		{"manage and recurse", map[string]any{"links": "manage", "recurse": true, "source": "/srv/a"}, ""},
		{"content and recurse false", map[string]any{"content": "x", "recurse": false}, ""},
	} {
		err := File{}.Validate(catalog.Resource{Type: "File", Title: "/etc/app.conf", Parameters: tc.params})
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%s: error = %v\n want %q", tc.name, err, tc.want)
		}
	}
}

// source.rb:107-128: trailing separators stripped, an absolute path shown as
// a file URI and unescaped again (measured: file:///abs, three slashes), a
// file: URI shown as given and read at its decoded path.
func TestSourceSpecsNormalise(t *testing.T) {
	got := sourceSpecs([]any{"/srv/a/", "/srv/a b", "file:///srv/a%20b/", "file:/srv/c", "file:///srv/%zz"}, nil)
	want := []sourceSpec{
		{path: "/srv/a", shown: "file:///srv/a"},
		{path: "/srv/a b", shown: "file:///srv/a b"},
		{path: "/srv/a%20b", shown: "file:///srv/a%20b"},
		{path: "/srv/c", shown: "file:/srv/c"},
		{path: "/srv/%zz", shown: "file:///srv/%zz"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("sourceSpecs = %+v\nwant %+v", got, want)
	}
	if got := sourceSpecs("/srv/one", nil); !slices.Equal(got, []sourceSpec{{path: "/srv/one", shown: "file:///srv/one"}}) {
		t.Errorf("sourceSpecs(string) = %+v", got)
	}
}

// source.rb:191-204: the first candidate that exists wins, and existence is
// File.exist? (indirector/direct_file_server.rb:10), which follows links, so
// a dangling symlink is skipped like a missing path.
func TestResolveSourceTakesTheFirstThatExists(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	r := fileRes("/dst", map[string]any{"source": []any{filepath.Join(dir, "missing"), dangling, real}})
	src, err := resolveSource(osFS{}, r, "production", nil)
	if err != nil {
		t.Fatalf("resolveSource: %v", err)
	}
	if src.spec.path != real || src.isDir || src.sum != checksum([]byte("alpha\n")) {
		t.Errorf("resolved = %+v, want the regular file with its checksum", src)
	}
}

// source.rb:208, verbatim, with every normalised candidate joined by ", ".
func TestResolveSourceMissingEverywhereNamesEveryCandidate(t *testing.T) {
	dir := t.TempDir()
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	r := fileRes("/dst", map[string]any{"source": []any{filepath.Join(dir, "missing") + "/", dangling}})
	_, err := resolveSource(osFS{}, r, "production", nil)
	want := "Could not retrieve information from environment production source(s) file://" +
		filepath.Join(dir, "missing") + ", file://" + dangling
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %q", err, want)
	}
}

func TestResolveSourceDirectory(t *testing.T) {
	dir := t.TempDir()
	src, err := resolveSource(osFS{}, fileRes("/dst", map[string]any{"source": dir}), "production", nil)
	if err != nil || !src.isDir || src.sum != "" {
		t.Errorf("resolveSource = %+v, %v; want a directory with no checksum", src, err)
	}
}

// D-26: a source that is not a regular file, a directory or a symlink (a
// FIFO here) fails the resource.
func TestResolveSourceRefusesASpecialFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]string{
		fifo: "source file://" + fifo + " is not a regular file or a directory; only those can be a source",
	} {
		_, err := resolveSource(osFS{}, fileRes("/dst", map[string]any{"source": source}), "production", nil)
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", source, err, want)
		}
	}
}

// source.rb:206: an existing source that cannot be read is a metadata error.
func TestResolveSourceUnreadableFileIsAMetadataError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a 0200 file")
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("x"), 0o200); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	_, err := resolveSource(osFS{}, fileRes("/dst", map[string]any{"source": path}), "production", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Could not retrieve file metadata for file://"+path+": ") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want a metadata error naming the source", err)
	}
}

// Measured on Puppet 8.10.0: a file created from a source reports
// file_created with "defined content as '{sha256}...'", and mode is not
// copied (source_permissions defaults to ignore, source.rb:158), so the new
// file is 0644 whatever the source's mode.
func TestFileSourceCreatesAFileWithTheSourcesContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	changes, _, err := File{}.Apply(context.Background(), fileRes(dst, map[string]any{"source": src}), false)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = %+v, %v; want one change", changes, err)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "file_created" || ch.Is != "file" ||
		ch.Message != "defined content as '"+checksum([]byte("alpha\n"))+"'" {
		t.Errorf("change = %+v", ch)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "alpha\n" {
		t.Errorf("content = %q (err %v)", b, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %04o, want 0644: the source's mode is not copied", got)
	}
}

// Measured: a replaced file reports content changed with both checksums, and
// the second run is quiet.
func TestFileSourceReplacesChangedContent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o640); err != nil {
		t.Fatal(err)
	}
	r := fileRes(dst, map[string]any{"source": src})
	changes, _, err := File{}.Apply(context.Background(), r, false)
	wantMsg := "content changed '" + checksum([]byte("old\n")) + "' to '" + checksum([]byte("new\n")) + "'"
	if err != nil || len(changes) != 1 || changes[0].Property != "content" || changes[0].Message != wantMsg {
		t.Fatalf("Apply = %+v, %v; want one content change %q", changes, err, wantMsg)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new\n" {
		t.Errorf("content = %q (err %v)", b, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %04o, want the existing 0640 kept", got)
	}
	if again, _, err := (File{}).Apply(context.Background(), r, false); err != nil || len(again) != 0 {
		t.Errorf("second Apply = %+v, %v; want converged", again, err)
	}
}

// source.rb:171 sets ensure from the source's type, replacing any other
// value. Measured: ensure => file with a directory source creates a directory.
func TestFileDirectorySourceOverridesEnsure(t *testing.T) {
	for _, ensure := range []string{"file", "present"} {
		srcDir := t.TempDir()
		dst := filepath.Join(t.TempDir(), "d")
		changes, _, err := File{}.Apply(context.Background(),
			fileRes(dst, map[string]any{"ensure": ensure, "source": srcDir}), false)
		if err != nil || len(changes) != 1 || changes[0].Name != "directory_created" {
			t.Errorf("ensure => %s: Apply = %+v, %v; want directory_created", ensure, changes, err)
		}
		if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
			t.Errorf("ensure => %s: %s is not a directory (err %v)", ensure, dst, err)
		}
	}
}

// type/file.rb:842-844 fetches the source before ensure is evaluated, so a
// missing source fails even ensure => absent (measured), and nothing is
// removed.
func TestFileMissingSourceFailsEvenEnsureAbsent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	_, _, err := File{Environment: "production"}.Apply(context.Background(),
		fileRes(dst, map[string]any{"ensure": "absent", "source": missing}), false)
	want := "Could not retrieve information from environment production source(s) file://" + missing
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("the destination is gone: %v", err)
	}
}

func TestFileSourceUnderNoopWritesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	changes, _, err := File{}.Apply(context.Background(), fileRes(dst, map[string]any{"source": src}), true)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = %+v, %v; want the one would-be create", changes, err)
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("noop created the destination: %v", err)
	}
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(dst, map[string]any{"source": filepath.Join(dir, "missing")}), true); err == nil {
		t.Error("a missing source passed under noop; Puppet fails it")
	}
}

// mismatched is content whose write produces bytes other than the ones its
// checksum describes: a source that changed after it was measured.
func mismatched(desired, actual string) content {
	return content{staged: true, sum: checksum([]byte(desired)), write: func(w io.Writer) (string, error) {
		_, err := w.Write([]byte(actual))
		return checksum([]byte(actual)), err
	}}
}

// type/file.rb:1076-1081: the bytes written are checksummed and compared with
// the checksum taken when the source was resolved, and a mismatch discards
// the write. Review Focus 3.
func TestReplaceFileDiscardsAMismatchedWrite(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "conf")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := replaceFile(context.Background(), osFS{}, dst, mismatched("new\n", "changed\n"))
	var mismatch *checksumMismatchError
	want := "File written to disk did not match desired checksum; discarding changes (" +
		checksum([]byte("changed\n")) + " vs " + checksum([]byte("new\n")) + ")"
	if !errors.As(err, &mismatch) || err.Error() != want {
		t.Fatalf("err = %v\nwant %q", err, want)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "old\n" {
		t.Errorf("content = %q (err %v), want the untouched old content", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("directory holds %v (err %v), want only the managed file", entries, err)
	}
}

func TestCreateFileDiscardsAMismatchedWrite(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "new")
	err := createFile(context.Background(), osFS{}, dst, mismatched("new\n", "changed\n"), 0o644)
	var mismatch *checksumMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(dst)); err != nil || len(entries) != 0 {
		t.Errorf("directory holds %v (err %v), want nothing", entries, err)
	}
}

// fail_if_checksum_is_wrong raises through self.fail, a Puppet::Error, so
// property.rb:490 does not wrap it: a create that mismatches fails ensure
// with the bare message.
func TestFileSourceMismatchOnCreateIsAttributedToEnsureUnwrapped(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "new")
	c := mismatched("new\n", "changed\n")
	r := fileRes(dst, nil)
	r.File, r.Line = "/etc/site.pp", 3
	changes, _, err := applyFile(context.Background(), osFS{}, dst, r, false, false, &c, nil, nil)
	if err != nil || len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err == nil {
		t.Fatalf("applyFile = %+v, %v; want one failed ensure change", changes, err)
	}
	if got := changes[0].Err.Error(); !strings.HasPrefix(got, "File written to disk did not match desired checksum;") {
		t.Errorf("Err = %q, want the unwrapped mismatch", got)
	}
}

// The race the check exists for: the source changes between resolution, where
// Puppet's metadata checksum is taken, and the copy.
func TestFileSourceChangedAfterResolutionIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSource(osFS{}, fileRes(dst, map[string]any{"source": src}), "production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = replaceFile(context.Background(), osFS{}, dst, sourceContent(resolved))
	want := "File written to disk did not match desired checksum; discarding changes (" +
		checksum([]byte("v2\n")) + " vs " + checksum([]byte("v1\n")) + ")"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %q", err, want)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "old\n" {
		t.Errorf("content = %q (err %v), want the untouched old content", b, err)
	}
}

// source passes pre-flight now that it is implemented, and each list element
// is bounded on its own.
func TestFileSourceIsBoundedAtPreflight(t *testing.T) {
	reg := NewRegistry(File{})
	for _, tc := range []struct {
		source any
		gaps   []string
	}{
		{"/srv/a", nil},
		{"file:///srv/a", nil},
		{"puppet:///modules/a", []string{"puppet:///modules/a"}},
		{[]any{"/srv/a", "http://h/b"}, []string{"http://h/b"}},
	} {
		var got []string
		for _, g := range Preflight(reg, &catalog.Catalog{Resources: []catalog.Resource{
			{Type: "File", Title: "/dst", Parameters: map[string]any{"source": tc.source}},
		}}) {
			if g.Attribute != "source" {
				t.Errorf("source => %v: unexpected gap %v", tc.source, g)
			}
			got = append(got, g.Value)
		}
		if !slices.Equal(got, tc.gaps) {
			t.Errorf("source => %v: gap values = %v, want %v", tc.source, got, tc.gaps)
		}
	}
}

// Measured on Puppet 8.10.0: source => [] is NOT the same as source unset. The
// parameter exists, so it counts as a creator, and with nothing to choose from
// the resource fails with the missing-source message and an empty list, even
// with ensure => absent. An earlier assumption that it was a no-op was
// wrong.
func TestFileEmptySourceListIsASourceThatIsMissingEverywhere(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst")
	for _, params := range []map[string]any{
		{"source": []any{}},
		{"ensure": "absent", "source": []any{}},
	} {
		_, _, err := File{Environment: "production"}.Apply(context.Background(), fileRes(dst, params), false)
		want := "Could not retrieve information from environment production source(s) "
		if err == nil || err.Error() != want {
			t.Errorf("%v: err = %v, want %q", params, err, want)
		}
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Errorf("the destination was created: %v", err)
	}
	if err := (File{}).Validate(fileRes(dst, map[string]any{"content": "x", "source": []any{}})); err == nil {
		t.Error("content with source => [] passed Validate; Puppet counts it as two creators")
	}
}

// Request#set_uri_key escapes % (uri_encode) before uri_to_path unescapes once,
// so a file: URI's path is read literally. Measured on Puppet 8.10.0 with both
// "a b" and "a%20b" present: file:///.../a%20b/f copied the literal a%20b/f.
func TestResolveSourceReadsAFileURIPathLiterally(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"a b": "space\n", "a%20b": "percent\n"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "f"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := resolveSource(osFS{}, fileRes("/dst", map[string]any{"source": "file://" + dir + "/a%20b/f"}), "production", nil)
	if err != nil || src.sum != checksum([]byte("percent\n")) {
		t.Errorf("resolveSource = %+v, %v; want the literal a%%20b/f", src, err)
	}
}

// source removed between resolution and the create: Puppet writes a sourced
// file through replace_file (type/file.rb:944-962), so nothing is left at the
// path and ensure fails.
func TestFileSourceVanishingBeforeCreateLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSource(osFS{}, fileRes("/dst", map[string]any{"source": src}), "production", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	c := sourceContent(resolved)
	changes, _, err := applyFile(context.Background(), osFS{}, dst, fileRes(dst, nil), false, false, &c, nil, nil)
	if err != nil || len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err == nil {
		t.Fatalf("applyFile = %+v, %v; want one failed ensure change", changes, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("directory holds %v (err %v), want nothing: no destination, no temp file", entries, err)
	}
}

// failRenameFS fails the final rename, the last step of a staged write.
type failRenameFS struct{ osFS }

func (failRenameFS) Rename(string, string) error { return syscall.EIO }

// Puppet writes any file with a content property through replace_file, empty
// content included (type/file.rb:1104-1107: Property#length is 0, which is
// truthy in Ruby), so a create whose write fails leaves nothing at the path.
func TestCreateFileWithContentLeavesNothingWhenTheWriteFails(t *testing.T) {
	for _, body := range []string{"x", ""} {
		dir := t.TempDir()
		dst := filepath.Join(dir, "new")
		if err := createFile(context.Background(), failRenameFS{}, dst, literalContent([]byte(body)), 0o644); err == nil {
			t.Fatalf("content %q: createFile succeeded despite the failed rename", body)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("content %q: directory holds %v (err %v), want nothing", body, entries, err)
		}
	}
}

// symlinkSource lays out dir/real ("z\n") and dir/lnk -> real.
func symlinkSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "real"), "z\n")
	if err := os.Symlink("real", filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// source.rb:170-176, file_serving/metadata.rb:118-124: under links => manage
// a link source makes the resource a link to the source's raw readlink.
func TestFileSymlinkSourceUnderManageMakesALink(t *testing.T) {
	dir := symlinkSource(t)
	d := filepath.Join(dir, "d")
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(d, map[string]any{"source": filepath.Join(dir, "lnk")}), false)
	if err != nil || len(changes) != 1 || changes[0].Name != "link_created" || changes[0].Message != "created" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if got, _ := os.Readlink(d); got != "real" {
		t.Errorf("readlink = %q; want the source's raw target", got)
	}
}

func TestFileSymlinkSourceUnderManageReplacesAFile(t *testing.T) {
	dir := symlinkSource(t)
	d := filepath.Join(dir, "d")
	mustWrite(t, d, "old\n")
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(d, map[string]any{"source": filepath.Join(dir, "lnk")}), false)
	if err != nil || len(changes) != 1 || changes[0].Name != "link_created" ||
		changes[0].Message != "ensure changed 'file' to 'link'" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if got, _ := os.Readlink(d); got != "real" {
		t.Errorf("readlink = %q", got)
	}
}

// A second run finds the link in place: nothing to do.
func TestFileSymlinkSourceUnderManageIsIdempotent(t *testing.T) {
	dir := symlinkSource(t)
	r := fileRes(filepath.Join(dir, "d"), map[string]any{"source": filepath.Join(dir, "lnk")})
	if _, _, err := (File{}).Apply(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	changes, _, err := (File{}).Apply(context.Background(), r, false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("second Apply = %+v, %v; want no change", changes, err)
	}
}

// noop reports the change and creates nothing.
func TestFileSymlinkSourceUnderManageNoopCreatesNothing(t *testing.T) {
	dir := symlinkSource(t)
	d := filepath.Join(dir, "d")
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(d, map[string]any{"source": filepath.Join(dir, "lnk")}), true)
	if err != nil || len(changes) != 1 || changes[0].Name != "link_created" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if _, err := os.Lstat(d); !os.IsNotExist(err) {
		t.Errorf("noop created %s", d)
	}
}

// A dangling link source does not exist (File.exist? follows links), so it is
// a missing source, not a link to copy.
func TestFileDanglingSymlinkSourceIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("nowhere", filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}
	_, _, err := (File{}).Apply(context.Background(),
		fileRes(filepath.Join(dir, "d"), map[string]any{"source": filepath.Join(dir, "lnk")}), false)
	if err == nil || !strings.Contains(err.Error(), "Could not retrieve information from environment") {
		t.Fatalf("err = %v; want the missing-source failure", err)
	}
}

func TestFileSymlinkSourceUnderFollowCopiesTheTarget(t *testing.T) {
	dir := symlinkSource(t)
	d := filepath.Join(dir, "d")
	changes, _, err := (File{}).Apply(context.Background(),
		fileRes(d, map[string]any{"source": "file://" + filepath.Join(dir, "lnk"), "links": "follow"}), false)
	if err != nil || len(changes) != 1 || changes[0].Name != "file_created" ||
		changes[0].Message != "defined content as '"+checksum([]byte("z\n"))+"'" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if fi, _ := os.Lstat(d); !fi.Mode().IsRegular() {
		t.Error("not a regular file")
	}
}

// ensure => absent keeps the destination absent whatever the source is.
func TestFileSymlinkSourceWithEnsureAbsentCreatesNoLink(t *testing.T) {
	dir := symlinkSource(t)
	d := filepath.Join(dir, "d")
	if _, _, err := (File{}).Apply(context.Background(),
		fileRes(d, map[string]any{"source": filepath.Join(dir, "lnk"), "ensure": "absent"}), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(d); !os.IsNotExist(err) {
		t.Errorf("%s exists", d)
	}
}

// swappedOpenFS lets resolution see the real source, then hands the read a
// directory descriptor, as if the path had been retargeted in between.
type swappedOpenFS struct {
	osFS
	dir string
}

func (s swappedOpenFS) OpenFile(string, int, os.FileMode) (*os.File, error) {
	return os.Open(s.dir)
}

// Under follow the open follows links, so the guard is the Fstat of the
// descriptor actually read: a non-regular file there is refused unread.
func TestOpenSourceFollowRefusesANonRegularDescriptor(t *testing.T) {
	dir := symlinkSource(t)
	fsys := swappedOpenFS{dir: dir}
	_, err := openSource(fsys, filepath.Join(dir, "lnk"), true)
	if err == nil || !strings.Contains(err.Error(), "no longer a regular file") {
		t.Errorf("openSource = %v; want a refusal", err)
	}
	r := fileRes(filepath.Join(dir, "d"), map[string]any{"source": filepath.Join(dir, "lnk"), "links": "follow"})
	if _, err := resolveSource(fsys, r, "production", nil); err == nil {
		t.Error("resolveSource accepted a source whose descriptor is not a regular file")
	}
}

// The agent downloads each puppet: source and maps the URI, exactly as the
// catalog wrote it, to the local copy (File.Sources). A mapped URI is
// supported, with or without the server's host, and passes Validate as
// Puppet's source.rb:86-103 does.
func TestFileSupportsMappedPuppetSource(t *testing.T) {
	f := File{Sources: map[string]string{
		"puppet:///modules/m/f":             "/run/dl/0",
		"puppet://srv.example/modules/m/g/": "/run/dl/1",
	}}
	for _, v := range []string{"puppet:///modules/m/f", "puppet://srv.example/modules/m/g/"} {
		if !f.SupportsValue("source", v) {
			t.Errorf("SupportsValue(source, %q) = false, want true", v)
		}
		if err := f.Validate(fileRes("/dst", map[string]any{"source": v})); err != nil {
			t.Errorf("Validate(source => %q) = %v", v, err)
		}
	}
	gaps := Preflight(NewRegistry(f), &catalog.Catalog{Resources: []catalog.Resource{
		fileRes("/dst", map[string]any{"source": []any{"puppet:///modules/m/f", "/srv/a"}}),
	}})
	if len(gaps) != 0 {
		t.Errorf("gaps = %v, want none", gaps)
	}
}

// A puppet: URI the agent did not map -- and every puppet: URI under apply,
// whose Sources is nil -- keeps the pre-flight refusal it always had. The key
// is the URI as written: a trailing separator is another key.
func TestFileRefusesUnmappedPuppetSource(t *testing.T) {
	mapped := File{Sources: map[string]string{"puppet:///modules/m/f": "/run/dl/0"}}
	for _, tc := range []struct {
		f     File
		value string
	}{
		{File{}, "puppet:///modules/m/f"},
		{mapped, "puppet:///modules/m/other"},
		{mapped, "puppet:///modules/m/f/"},
		{mapped, "puppet://srv.example/modules/m/f"},
		{mapped, "http://h/modules/m/f"},
	} {
		if tc.f.SupportsValue("source", tc.value) {
			t.Errorf("Sources %v: SupportsValue(source, %q) = true, want false", tc.f.Sources, tc.value)
		}
		gaps := Preflight(NewRegistry(tc.f), &catalog.Catalog{Resources: []catalog.Resource{
			fileRes("/dst", map[string]any{"source": tc.value}),
		}})
		want := `File[/dst]: source => "` + tc.value + `" is not implemented`
		if len(gaps) != 1 || !strings.HasPrefix(gaps[0].String(), want) {
			t.Errorf("Sources %v, source %q: gaps = %v, want one starting %q", tc.f.Sources, tc.value, gaps, want)
		}
	}
}

// source.rb:107-128 and :198: a mapped source is read from its download but
// named as the catalog wrote it, normalised as any source is; an unmapped
// value is normalised as before.
func TestSourceShownKeepsPuppetURI(t *testing.T) {
	got := sourceSpecs([]any{"puppet:///modules/m/f/", "/srv/a/", "puppet://srv.example/modules/m/g"},
		map[string]string{"puppet:///modules/m/f/": "/run/dl/0", "puppet://srv.example/modules/m/g": "/run/dl/1"})
	want := []sourceSpec{
		{path: "/run/dl/0", shown: "puppet:///modules/m/f", mapped: true},
		{path: "/srv/a", shown: "file:///srv/a"},
		{path: "/run/dl/1", shown: "puppet://srv.example/modules/m/g", mapped: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("sourceSpecs = %+v\nwant %+v", got, want)
	}

	dl := filepath.Join(t.TempDir(), "0")
	if err := os.WriteFile(dl, []byte("served\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "f")
	f := File{Environment: "production", Sources: map[string]string{"puppet:///modules/m/f": dl}}
	if _, _, err := f.Apply(context.Background(), fileRes(dst, map[string]any{"source": "puppet:///modules/m/f"}), false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "served\n" {
		t.Errorf("content = %q (err %v), want the downloaded content", b, err)
	}
}

// A mapped source's failure names its puppet: URI, as Puppet's
// does, and never the run-private path of the download.
func TestMappedSourceErrorNamesURINotDownload(t *testing.T) {
	dl := filepath.Join(t.TempDir(), "voxagent-sources-1", "0")
	if err := os.MkdirAll(filepath.Dir(dl), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dl, []byte("served\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const uri = "puppet:///modules/m/f"
	sources := map[string]string{uri: dl}
	r := fileRes(filepath.Join(t.TempDir(), "d"), map[string]any{"source": uri})
	swapped := swappedOpenFS{dir: t.TempDir()}
	check := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), uri) || strings.Contains(err.Error(), filepath.Dir(dl)) {
			t.Errorf("%s: err = %v; want it to name %s and not the download", what, err, uri)
		}
	}

	_, err := resolveSource(swapped, r, "production", sources)
	check("metadata", err)
	if want := "Could not retrieve file metadata for " + uri + ": " + uri + " is no longer a regular file"; err == nil || err.Error() != want {
		t.Errorf("metadata: err = %v\nwant %q", err, want)
	}

	src, err := resolveSource(osFS{}, r, "production", sources)
	if err != nil {
		t.Fatal(err)
	}
	src.fsys = swapped
	c := sourceContent(src)
	_, err = c.write(io.Discard)
	check("content", err)
}

// recurse_remote (type/file.rb:712-726, :750): a child of a recursed source
// gets "<source>/<relative path>" as its source, so a mapped puppet: source's
// child is named by its puppet: URI and read from the download's tree.
func TestMappedPuppetSourceRecurseChildShown(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "dl/a", "dl/sub/b", "dst/")
	dl, dst := filepath.Join(base, "dl"), filepath.Join(base, "dst")
	if err := os.WriteFile(filepath.Join(dl, "sub/b"), []byte("bee\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dl, "a"), filepath.Join(dl, "link")); err != nil {
		t.Fatal(err)
	}
	const uri = "puppet:///modules/m/tree/"
	f := File{Environment: "production", Sources: map[string]string{uri: dl}}
	parent := fileRes(dst, map[string]any{"recurse": true, "source": uri})
	children, _, err := f.Generate(context.Background(), parent, noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]any{}
	for _, c := range children {
		sources[strings.TrimPrefix(c.Title, dst+"/")] = c.Parameters["source"]
	}
	want := map[string]any{
		"a":     "puppet:///modules/m/tree/a",
		"link":  "puppet:///modules/m/tree/link",
		"sub":   "puppet:///modules/m/tree/sub",
		"sub/b": "puppet:///modules/m/tree/sub/b",
	}
	if !reflect.DeepEqual(sources, want) {
		t.Fatalf("child sources = %v\nwant %v", sources, want)
	}

	for _, c := range children {
		if c.Title == filepath.Join(dst, "link") {
			_, _, err := f.Apply(context.Background(), c, false)
			if want := "source puppet:///modules/m/tree/link is a symlink; ensure => link is not implemented"; err == nil || err.Error() != want {
				t.Errorf("link child: err = %v, want %q", err, want)
			}
			continue
		}
		if _, _, err := f.Apply(context.Background(), c, false); err != nil {
			t.Fatalf("Apply %s: %v", c.Title, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sub/b")); err != nil || string(b) != "bee\n" {
		t.Errorf("sub/b = %q (err %v), want the downloaded content", b, err)
	}

	// A child whose puppet: source is not the parent's source joined with
	// its own relative path names nothing inside the download.
	stray := childOf(parent, filepath.Join(dst, "a"), map[string]any{"source": "puppet:///modules/m/other/a"})
	_, _, err = f.Apply(context.Background(), stray, false)
	if want := "File[" + dst + "/a]: source puppet:///modules/m/other/a does not name a inside a source directory"; err == nil || err.Error() != want {
		t.Errorf("stray child: err = %v, want %q", err, want)
	}
}

// The agent maps a source the server answered 404 for to a path that does not
// exist, so source.rb:208's message names the puppet: URI as Puppet's does.
// Measured on Puppet 8.10.0 for file: (TestResolveSourceMissingEverywhere...);
// the text is the same for any scheme.
func TestMappedMissingPuppetSourceMessage(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	f := File{Environment: "production", Sources: map[string]string{"puppet:///modules/m/missing": absent}}
	_, _, err := f.Apply(context.Background(),
		fileRes(filepath.Join(t.TempDir(), "f"), map[string]any{"source": []any{"puppet:///modules/m/missing", absent + "/"}}), false)
	want := "Could not retrieve information from environment production source(s) puppet:///modules/m/missing, file://" + absent
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %q", err, want)
	}
}
