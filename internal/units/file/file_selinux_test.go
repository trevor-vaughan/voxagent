package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
)

func TestParseContextSplitsTheRangeAfterTheThirdColon(t *testing.T) {
	c, ok := parseContext("system_u:object_r:shadow_t:s0-s0:c0.c1023")
	if !ok || c.user != "system_u" || c.role != "object_r" || c.typ != "shadow_t" || c.rng != "s0-s0:c0.c1023" {
		t.Errorf("parseContext = %+v, %v", c, ok)
	}
	if c.String() != "system_u:object_r:shadow_t:s0-s0:c0.c1023" {
		t.Errorf("String() = %q", c.String())
	}
	if _, ok := parseContext("unlabeled"); ok {
		t.Error("unlabeled parsed as a context")
	}
}

// util/selinux.rb:92 makes the range optional and forbids whitespace in the
// first three fields but not in the range.
func TestParseContextFollowsPuppetsPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"system_u:object_r:etc_t":       true,
		"system_u:object_r:etc_t:s0 s1": true,
		"system_u:object_r":             false,
		"system_u:object_r:etc t:s0":    false,
		"system_u:object_r:etc_t:":      false,
		"system_u:object_r:etc_t:s0/x":  false,
		"system_u::etc_t:s0":            false,
		"":                              false,
		"system_u:object_r:etc_t:s0\tx": true,
	} {
		if _, ok := parseContext(s); ok != want {
			t.Errorf("parseContext(%q) ok = %v; want %v", s, ok, want)
		}
	}
	c, _ := parseContext("system_u:object_r:etc_t")
	if c.rng != "" || c.String() != "system_u:object_r:etc_t" {
		t.Errorf("rangeless context = %+v, %q", c, c.String())
	}
	// Ruby's ^ and $ are line anchors, so the first line that matches wins.
	c, ok := parseContext("garbage line\nsystem_u:object_r:etc_t:s0")
	if !ok || c.String() != "system_u:object_r:etc_t:s0" {
		t.Errorf("multi-line context = %+v, %v", c, ok)
	}
}

func TestContextFieldAndWithField(t *testing.T) {
	c, _ := parseContext("system_u:object_r:etc_t:s0")
	for prop, want := range map[string]string{"seluser": "system_u", "selrole": "object_r", "seltype": "etc_t", "selrange": "s0"} {
		if got := c.field(prop); got != want {
			t.Errorf("field(%q) = %q; want %q", prop, got, want)
		}
	}
	if got := withField(c.String(), "seltype", "shadow_t"); got != "system_u:object_r:shadow_t:s0" {
		t.Errorf("withField(seltype) = %q", got)
	}
	if got := withField(c.String(), "seluser", "unconfined_u"); got != "unconfined_u:object_r:etc_t:s0" {
		t.Errorf("withField(seluser) = %q", got)
	}
}

// set_selinux_context splits the whole raw context on every colon and replaces
// one element (util/selinux.rb:131-144), so a range that itself has a colon
// keeps everything after it (I-7), a context without a range gains one, and
// what it read is never parsed.
func TestWithFieldSplitsTheRawContextAsRubyDoes(t *testing.T) {
	for _, tc := range []struct{ raw, prop, v, want string }{
		{"system_u:object_r:etc_t:s0-s0:c0.c1023", "selrange", "s0", "system_u:object_r:etc_t:s0:c0.c1023"},
		{"system_u:object_r:etc_t:s0-s0:c0.c1023", "seltype", "shadow_t", "system_u:object_r:shadow_t:s0-s0:c0.c1023"},
		{"system_u:object_r:etc_t", "selrange", "s0", "system_u:object_r:etc_t:s0"},
		{"system_u:object_r:etc_t:s0", "selrange", "", "system_u:object_r:etc_t:"},
		{"unlabeled", "seltype", "etc_t", "unlabeled::etc_t"},
		{"u:r:t:::", "seluser", "x", "x:r:t"},
	} {
		if got := withField(tc.raw, tc.prop, tc.v); got != tc.want {
			t.Errorf("withField(%q, %s, %q) = %q; want %q", tc.raw, tc.prop, tc.v, got, tc.want)
		}
	}
}

func TestModeForEnsure(t *testing.T) {
	for ensure, want := range map[string]uint32{"file": syscall.S_IFREG, "present": syscall.S_IFREG,
		"directory": syscall.S_IFDIR, "link": syscall.S_IFLNK, "absent": 0, "false": 0, "": 0} {
		if got := modeForEnsure(ensure); got != want {
			t.Errorf("modeForEnsure(%q) = %o; want %o", ensure, got, want)
		}
	}
}

func TestFirstSelinuxfsMountScansProcMounts(t *testing.T) {
	for file, want := range map[string]string{"mounts-selinuxfs": "/sys/fs/selinux", "mounts-no-selinuxfs": ""} {
		f, err := os.Open(filepath.Join("testdata/selinux", file))
		if err != nil {
			t.Fatal(err)
		}
		got := firstSelinuxfsMount(f)
		f.Close()
		if got != want {
			t.Errorf("%s: firstSelinuxfsMount = %q; want %q", file, got, want)
		}
	}
	// init.c:111-117 gives up on the whole file at a line without two spaces.
	if got := firstSelinuxfsMount(strings.NewReader("garbage\nselinuxfs /x selinuxfs rw 0 0\n")); got != "" {
		t.Errorf("a malformed line before the mount: got %q; want \"\"", got)
	}
	// init.c:118 compares the type exactly: selinuxfs2 is not selinuxfs.
	if got := firstSelinuxfsMount(strings.NewReader("x /a selinuxfs2 rw 0 0\nselinuxfs /b selinuxfs rw 0 0")); got != "/b" {
		t.Errorf("got %q; want /b", got)
	}
	// The mount point is returned as /proc/mounts escapes it (init.c:119-126).
	if got := firstSelinuxfsMount(strings.NewReader(`selinuxfs /mnt/se\040x selinuxfs rw 0 0`)); got != `/mnt/se\040x` {
		t.Errorf("escaped mount point: got %q", got)
	}
}

// fakeStatfs answers statfs from a table of path -> (type, flags); any other
// path is the plain xfs root.
type fakeStatfs map[string][2]int64

func (f fakeStatfs) statfs(path string, buf *syscall.Statfs_t) error {
	v, ok := f[path]
	if !ok {
		v = [2]int64{0x58465342, 0}
	}
	buf.Type, buf.Flags = v[0], v[1]
	return nil
}

func TestSelinuxMountFollowsInitSelinuxmnt(t *testing.T) {
	rw := [2]int64{selinuxMagic, 0x20}
	ro := [2]int64{selinuxMagic, 0x20 | stRdonly}
	elsewhere := "selinuxfs /mnt/se selinuxfs rw 0 0\n"
	for name, tc := range map[string]struct {
		fs     fakeStatfs
		mounts string
		want   string
	}{
		"read-write at the default":              {fakeStatfs{"/sys/fs/selinux": rw}, "", "/sys/fs/selinux"},
		"read-only at the default stops looking": {fakeStatfs{"/sys/fs/selinux": ro, "/selinux": rw, "/mnt/se": rw}, elsewhere, ""},
		"the old mount point":                    {fakeStatfs{"/selinux": rw}, "", "/selinux"},
		"found through /proc/mounts":             {fakeStatfs{"/mnt/se": rw}, elsewhere, "/mnt/se"},
		"read-only through /proc/mounts":         {fakeStatfs{"/mnt/se": ro}, elsewhere, ""},
		"listed but not selinuxfs on statfs":     {fakeStatfs{}, elsewhere, ""},
		"nowhere":                                {fakeStatfs{}, "", ""},
	} {
		open := func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.mounts)), nil }
		if got := selinuxMount(tc.fs.statfs, open); got != tc.want {
			t.Errorf("%s: selinuxMount = %q; want %q", name, got, tc.want)
		}
	}
	unreadable := func() (io.ReadCloser, error) { return nil, os.ErrPermission }
	if got := selinuxMount(fakeStatfs{}.statfs, unreadable); got != "" {
		t.Errorf("unreadable /proc/mounts: selinuxMount = %q; want \"\"", got)
	}
}

// libselinux's own selinuxenabled is the oracle for this host's state. Here
// that is the disabled state; the SELinux VM gate covers the enabled one.
func TestSELinuxEnabledAgreesWithSelinuxenabled(t *testing.T) {
	if _, err := exec.LookPath("selinuxenabled"); err != nil {
		t.Skip("selinuxenabled (libselinux-utils) is not installed")
	}
	err := exec.Command("selinuxenabled").Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if got, want := (hostSELinux{}).enabled(), err == nil; got != want {
		t.Errorf("enabled() = %v; selinuxenabled says %v", got, want)
	}
}

func TestLabelSupportedUsesTheLongestMountPrefix(t *testing.T) {
	b, err := os.ReadFile("testdata/selinux/mounts-mixed")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"/etc/x": true, "/mnt/nfs/x": false, "/mnt/nfs": false, "/mnt/nfsx": true,
		"/tmp/x": true, "/proc/1": false, "/": true,
		"/mnt/nfs/../../etc": true, "/tmp//x/": true, "/proc/../tmp/x": true,
	} {
		if got := labelSupportedIn(string(b), path); got != want {
			t.Errorf("labelSupportedIn(%q) = %v; want %v", path, got, want)
		}
	}
}

func TestLabelSupportedInEdgeCases(t *testing.T) {
	for name, tc := range map[string]struct {
		mounts, path string
		want         bool
	}{
		"rootfs alone is skipped":       {"rootfs / rootfs rw 0 0\n", "/etc/x", false},
		"a later mount at a point wins": {"/dev/sda1 /srv ext4 rw 0 0\nnfs:/x /srv nfs rw 0 0\n", "/srv/x", false},
		"no mount covers the path":      {"tmpfs /tmp tmpfs rw 0 0\n", "/etc/x", false},
		"a line with no type":           {"/dev/sda1 / xfs rw 0 0\nx /srv\n", "/srv/a", false},
		"every listed type":             {"a /a ext2 rw\nb /b ext3 rw\nc /c ext4 rw\nd /d gfs rw\ne /e gfs2 rw\nf /f jfs rw\ng /g btrfs rw\nh /h zfs rw\n", "/h/x", true},
		"no mounts at all":              {"", "/etc", false},
		// read_mounts never unescapes, so a path under a mount point with a
		// space falls through to the parent mount (I-8).
		"an escaped mount point is not the path": {"/dev/sda1 / xfs rw 0 0\nnfs:/x /mnt/my\\040disk nfs rw 0 0\n", "/mnt/my disk/x", true},
		"the escaped spelling matches":           {"/dev/sda1 / xfs rw 0 0\nnfs:/x /mnt/my\\040disk nfs rw 0 0\n", `/mnt/my\040disk/x`, false},
	} {
		if got := labelSupportedIn(tc.mounts, tc.path); got != tc.want {
			t.Errorf("%s: labelSupportedIn(%q) = %v; want %v", name, tc.path, got, tc.want)
		}
	}
}

func TestDefaultForParsesSelabelLookup(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/selabel-lookup-shadow.txt")
	if err != nil {
		t.Fatal(err)
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		"selabel_lookup -b file -k /etc/shadow -t 32768": {Out: string(out)},
	}}
	c, ok, err := hostSELinux{run: fr}.defaultFor(context.Background(), "/etc/shadow", syscall.S_IFREG)
	if err != nil || !ok || c.String() != "system_u:object_r:shadow_t:s0" {
		t.Errorf("defaultFor = %v, %v, %v", c, ok, err)
	}
	want := []string{"selabel_lookup", "-b", "file", "-k", "/etc/shadow", "-t", "32768"}
	if got := fr.OnlyCall(t); !slices.Equal(got, want) {
		t.Errorf("args = %q; want %q", got, want)
	}
}

func TestDefaultForWithNoMatchHasNoDefault(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/selabel-lookup-none.txt")
	if err != nil {
		t.Fatal(err)
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		"selabel_lookup -b file -k /proc/1 -t 32768": {Exit: 255, Out: string(out)},
	}}
	_, ok, err := hostSELinux{run: fr}.defaultFor(context.Background(), "/proc/1", syscall.S_IFREG)
	if err != nil || ok {
		t.Errorf("ok=%v err=%v; want no default and no error", ok, err)
	}
}

// selabel_lookup also exits 255 when it cannot open the file_contexts handle
// (selabel_lookup.c:91-97). Puppet then has a nil handle and raises
// (util/selinux.rb:73), so this is an error, not "no default".
func TestDefaultForWithNoHandleIsPuppetsError(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/selabel-lookup-open-failed.txt")
	if err != nil {
		t.Fatal(err)
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		"selabel_lookup -b file -k /etc/shadow -t 32768": {Exit: 255, Out: string(out)},
	}}
	_, ok, err := hostSELinux{run: fr}.defaultFor(context.Background(), "/etc/shadow", syscall.S_IFREG)
	if ok || err == nil || err.Error() != "Cannot get default context with nil handle" {
		t.Errorf("ok=%v err=%v; want Puppet's nil-handle error", ok, err)
	}
}

// The handle is probed once per provider value however many Files ask, and
// a probe that opens no handle is Puppet's nil-handle error.
func TestHandleOpensProbesOnceAndReportsANilHandle(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/selabel-lookup-open-failed.txt")
	if err != nil {
		t.Fatal(err)
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		"selabel_lookup -b file -k / -t 0": {Exit: 255, Out: string(out)},
	}}
	h := hostSELinux{run: fr, handle: &handleProbe{}}
	for range 3 {
		if err := h.handleOpens(context.Background()); !errors.Is(err, errNilHandle) {
			t.Fatalf("handleOpens = %v; want Puppet's nil-handle error", err)
		}
	}
	if len(fr.Calls) != 1 {
		t.Errorf("ran %v; want one probe", fr.Calls)
	}
	ok := hostSELinux{run: &raltest.FakeRunner{}, handle: &handleProbe{}}
	if err := ok.handleOpens(context.Background()); err != nil {
		t.Errorf("an opening handle: handleOpens = %v", err)
	}
}

func TestDefaultForErrors(t *testing.T) {
	key := "selabel_lookup -b file -k /etc/x -t 0"
	for name, tc := range map[string]struct {
		reply   raltest.FakeReply
		wantErr string
	}{
		"failure to start":    {raltest.FakeReply{Err: errors.New("exec: not found")}, "exec: not found"},
		"a malformed default": {raltest.FakeReply{Out: "Default context: bogus\n"}, "Invalid context to parse: bogus"},
	} {
		fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{key: tc.reply}}
		_, ok, err := hostSELinux{run: fr}.defaultFor(context.Background(), "/etc/x", 0)
		if ok || err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: ok=%v err=%v; want error %q", name, ok, err, tc.wantErr)
		}
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{key: {Out: "something else\n"}}}
	if _, ok, err := (hostSELinux{run: fr}).defaultFor(context.Background(), "/etc/x", 0); ok || err != nil {
		t.Errorf("exit 0 without the line: ok=%v err=%v; want no default and no error", ok, err)
	}
}

// The xattr helpers are exercised through the user namespace, which an
// unprivileged process can write, so the ERANGE retry and the NUL handling are
// covered without SELinux.
func TestXattrHelpersRetryOnERANGEAndKeepTheNUL(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	mustWrite(t, f, "x")
	long := strings.Repeat("a", 1000)
	if err := setfileconRaw(osFS{}, f, "user.voxagent", long); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skipf("user xattrs are not supported on %s", filepath.Dir(f))
		}
		t.Fatal(err)
	}
	raw := make([]byte, 2000)
	n, err := syscall.Getxattr(f, "user.voxagent", raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[:n]) != long+"\x00" {
		t.Errorf("stored %d bytes; want the value plus a NUL (%d)", n, len(long)+1)
	}
	got, err := getfileconRaw(osFS{}, f, "user.voxagent")
	if err != nil || got != long {
		t.Errorf("getfileconRaw = %d bytes, %v; want %d bytes", len(got), err, len(long))
	}
	if _, err := getfileconRaw(osFS{}, f, "user.absent"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("missing attribute: err = %v; want ENODATA", err)
	}
	if _, err := getfileconRaw(osFS{}, filepath.Join(t.TempDir(), "nope"), "user.voxagent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v; want ENOENT", err)
	}
}

// The O_PATH|O_NOFOLLOW handle is the link itself, as lgetfilecon's name is:
// a user xattr on the target is not seen through the link (user xattrs are
// refused on links, so the link reads as having none).
func TestXattrHelpersActOnALinkItself(t *testing.T) {
	dir := t.TempDir()
	f, l := filepath.Join(dir, "f"), filepath.Join(dir, "l")
	mustWrite(t, f, "x")
	if err := os.Symlink("f", l); err != nil {
		t.Fatal(err)
	}
	if err := setfileconRaw(osFS{}, f, "user.voxagent", "v"); err != nil {
		t.Skipf("user xattrs are not supported here (%v)", err)
	}
	if got, err := getfileconRaw(osFS{}, l, "user.voxagent"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("through the link: %q, %v; want the link's own ENODATA", got, err)
	}
	if err := setfileconRaw(osFS{}, l, "user.voxagent", "w"); err == nil {
		t.Error("set through the link succeeded; it must address the link, which refuses user xattrs")
	}
	if got, _ := getfileconRaw(osFS{}, f, "user.voxagent"); got != "v" {
		t.Errorf("target = %q, want it untouched", got)
	}
}

// A generated child's label is read and written through its tree's os.Root:
// a parent swapped for a link out of the tree after the walk is refused, and
// nothing outside is read or changed.
func TestXattrHelpersStayInsideTheRoot(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	mustMkdir(t, filepath.Join(tree, "sub"))
	mustWrite(t, filepath.Join(tree, "sub", "f"), "in")
	mustWrite(t, filepath.Join(outside, "f"), "out")
	if err := setfileconRaw(osFS{}, filepath.Join(outside, "f"), "user.voxagent", "outside"); err != nil {
		t.Skipf("user xattrs are not supported here (%v)", err)
	}
	root, err := openDirNoFollow(tree)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rfs := rootFS{root: root, dir: tree}
	child := filepath.Join(tree, "sub", "f")
	if err := setfileconRaw(rfs, child, "user.voxagent", "inside"); err != nil {
		t.Fatalf("in the tree: %v", err)
	}
	if got, err := getfileconRaw(rfs, child, "user.voxagent"); err != nil || got != "inside" {
		t.Fatalf("in the tree: %q, %v", got, err)
	}
	if err := os.RemoveAll(filepath.Join(tree, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "sub")); err != nil {
		t.Fatal(err)
	}
	if got, err := getfileconRaw(rfs, child, "user.voxagent"); err == nil {
		t.Errorf("read through the swapped parent: %q", got)
	}
	if err := setfileconRaw(rfs, child, "user.voxagent", "relabelled"); err == nil {
		t.Error("write through the swapped parent succeeded")
	}
	if got, _ := getfileconRaw(osFS{}, filepath.Join(outside, "f"), "user.voxagent"); got != "outside" {
		t.Errorf("outside file = %q, want it untouched", got)
	}
}

func TestCurrentReadsTheHostLabel(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	mustWrite(t, f, "x")
	raw, ok := hostSELinux{}.current(osFS{}, f)
	if !ok {
		t.Skip("this filesystem carries no security.selinux label")
	}
	if c, ok, err := puppetContext(raw); err != nil || !ok || c.typ == "" {
		t.Errorf("current = %q: %+v, %v, %v", raw, c, ok, err)
	}
}

func TestCurrentReadsTheXattrWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	f, l := filepath.Join(dir, "f"), filepath.Join(dir, "l")
	mustWrite(t, f, "x")
	if err := os.Symlink(f, l); err != nil {
		t.Fatal(err)
	}
	se := hostSELinux{}
	before, ok := se.current(osFS{}, l)
	if !ok {
		t.Skip("no readable label on the link")
	}
	want := "system_u:object_r:etc_t:s0"
	if before == want {
		t.Skip("the link already carries the test label")
	}
	if err := se.set(osFS{}, f, want); err != nil {
		t.Skipf("the kernel refuses security.selinux writes here (%v); covered by the SELinux VM gate", err)
	}
	if got, ok := se.current(osFS{}, f); !ok || got != want {
		t.Errorf("current(f) = %q, %v; want %q", got, ok, want)
	}
	lc, ok := se.current(osFS{}, l)
	if !ok {
		t.Fatal("the link's label is no longer readable")
	}
	if lc == want {
		t.Error("current(link) returned the target's label; it must read the link's own")
	}
}

func TestMain(m *testing.M) {
	// No test reaches the host's SELinux unless it asks to: File{} gets a
	// disabled one.
	defaultSELinux = &fakeSE{}
	os.Exit(m.Run())
}

// fakeSE is an SELinux-enabled host. Labels, raw strings, belong to inodes,
// as the kernel's do: a path relabelled and then replaced by a rename reads as
// fresh, the label a new file gets on creation. A path that does not exist
// has no label. Paths are resolved through fsys, so a generated child's stay
// confined as hostSELinux's do.
type fakeSE struct {
	on, supported, tool bool
	cur                 map[string]string
	fresh               string
	def                 seContext
	defErr              error
	handleErr           error
	setErr              error
	sets                []string
	lookups             []uint32
	batches             [][]defaultQuery
	reads               int
	ino                 map[string]uint64
}

func inode(fsys fileSystem, p string) (uint64, bool) {
	fi, err := fsys.Lstat(p)
	if err != nil {
		return 0, false
	}
	return fi.Sys().(*syscall.Stat_t).Ino, true
}

func (f *fakeSE) enabled() bool              { return f.on }
func (f *fakeSE) labelSupported(string) bool { return f.supported }
func (f *fakeSE) lookupAvailable() bool      { return f.tool }
func (f *fakeSE) handleOpens(context.Context) error {
	return f.handleErr
}
func (f *fakeSE) current(fsys fileSystem, p string) (string, bool) {
	f.reads++
	ino, exists := inode(fsys, p)
	if !exists {
		return "", false
	}
	if seen, ok := f.ino[p]; !ok {
		f.ino[p] = ino
	} else if seen != ino {
		delete(f.cur, p)
		f.ino[p] = ino
	}
	if c, ok := f.cur[p]; ok {
		return c, true
	}
	return f.fresh, f.fresh != ""
}
func (f *fakeSE) set(fsys fileSystem, p, c string) error {
	f.sets = append(f.sets, p+"="+c)
	if f.setErr == nil {
		f.cur[p] = c
		if ino, ok := inode(fsys, p); ok {
			f.ino[p] = ino
		}
	}
	return f.setErr
}
func (f *fakeSE) defaultFor(_ context.Context, _ string, mode uint32) (seContext, bool, error) {
	f.lookups = append(f.lookups, mode)
	if f.defErr != nil {
		return seContext{}, false, f.defErr
	}
	return f.def, f.def != (seContext{}), nil
}

// defaultsFor answers every query with def, as one batch.
func (f *fakeSE) defaultsFor(_ context.Context, qs []defaultQuery) map[defaultQuery]defaultAnswer {
	f.batches = append(f.batches, qs)
	answers := map[defaultQuery]defaultAnswer{}
	for _, q := range qs {
		answers[q] = defaultAnswer{ctx: f.def, ok: f.def != (seContext{})}
	}
	return answers
}

var (
	tmpT = seContext{"system_u", "object_r", "tmp_t", "s0"}
	etcT = seContext{"system_u", "object_r", "etc_t", "s0"}
)

// enabledSE labels p with cur, unless cur is the zero context. The label is
// pinned to p's inode now, when p already exists, so a later replacement of p
// reads as fresh.
func enabledSE(p string, cur seContext) *fakeSE {
	se := &fakeSE{on: true, supported: true, tool: true,
		cur: map[string]string{}, def: etcT, ino: map[string]uint64{}}
	if cur != (seContext{}) {
		se.cur[p] = cur.String()
	}
	if ino, ok := inode(osFS{}, p); ok {
		se.ino[p] = ino
	}
	return se
}

func TestSELinuxRelabelsToTheDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	changes, warnings, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false)
	if err != nil || len(warnings) != 0 || len(changes) != 1 || changes[0].Property != "seltype" ||
		changes[0].Was != "tmp_t" || changes[0].Is != "etc_t" || changes[0].Err != nil ||
		changes[0].Message != "seltype changed 'tmp_t' to 'etc_t'" {
		t.Fatalf("Apply = %+v, %v, %v", changes, warnings, err)
	}
	if want := []string{p + "=system_u:object_r:etc_t:s0"}; !slices.Equal(se.sets, want) {
		t.Errorf("sets = %v, want %v", se.sets, want)
	}
	if len(se.lookups) != 1 || se.lookups[0]&syscall.S_IFMT != syscall.S_IFREG {
		t.Errorf("lookups = %o, want one with the file's own st_mode", se.lookups)
	}
}

func TestSELinuxDeclaredValueOverridesTheDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, seContext{"unconfined_u", "object_r", "tmp_t", "s0"})
	changes, _, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "bin_t"}), false)
	var got []string
	for _, c := range changes {
		got = append(got, c.Message)
	}
	want := []string{"seluser changed 'unconfined_u' to 'system_u'", "seltype changed 'tmp_t' to 'bin_t'"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("Apply = %q, %v; want %q", got, err, want)
	}
	// Each sync re-reads the context and replaces its own field.
	wantSets := []string{p + "=system_u:object_r:tmp_t:s0", p + "=system_u:object_r:bin_t:s0"}
	if !slices.Equal(se.sets, wantSets) {
		t.Errorf("sets = %v, want %v", se.sets, wantSets)
	}
}

func TestSELinuxNoDefaultLookupWhenAllFourAreDeclared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	_, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file",
		"seluser": "system_u", "selrole": "object_r", "seltype": "tmp_t", "selrange": "s0"}), false)
	if err != nil || len(se.lookups) != 0 || len(se.sets) != 0 {
		t.Fatalf("err = %v, lookups = %v, sets = %v; want none", err, se.lookups, se.sets)
	}
}

func TestSELinuxIgnoreDefaultsManagesOnlyDeclared(t *testing.T) {
	for _, ignore := range []any{true, "true"} {
		p := filepath.Join(t.TempDir(), "f")
		mustWrite(t, p, "x")
		se := enabledSE(p, seContext{"unconfined_u", "object_r", "tmp_t", "s0"})
		changes, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file",
			"selinux_ignore_defaults": ignore, "selrole": "system_r"}), false)
		if err != nil || len(changes) != 1 || changes[0].Message != "selrole changed 'object_r' to 'system_r'" {
			t.Fatalf("%v: Apply = %+v, %v", ignore, changes, err)
		}
		if len(se.lookups) != 0 {
			t.Errorf("%v: lookups = %v, want none", ignore, se.lookups)
		}
	}
}

func TestSELinuxDisabledIsANoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.on = false
	r := raltest.FileRes(p, map[string]any{"ensure": "file", "seluser": "a_u", "selrole": "b_r",
		"seltype": "c_t", "selrange": "s1", "selinux_ignore_defaults": false})
	if err := (File{se: se}).Validate(r); err != nil {
		t.Fatal(err)
	}
	changes, warnings, err := File{se: se}.Apply(context.Background(), r, false)
	if err != nil || len(changes) != 0 || len(warnings) != 0 {
		t.Fatalf("Apply = %+v, %v, %v; want nothing", changes, warnings, err)
	}
	if len(se.sets) != 0 || len(se.lookups) != 0 || se.reads != 0 {
		t.Errorf("sets %v, lookups %v, reads %d; want none", se.sets, se.lookups, se.reads)
	}
}

func TestSELinuxUnsupportedFilesystemIsANoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.supported = false
	changes, _, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "bin_t"}), false)
	if err != nil || len(changes) != 0 || len(se.sets) != 0 || len(se.lookups) != 0 || se.reads != 0 {
		t.Fatalf("Apply = %+v, %v; sets %v lookups %v reads %d", changes, err, se.sets, se.lookups, se.reads)
	}
}

func TestSELinuxCreatedFileIsLabelledSilently(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	se := enabledSE(p, seContext{})
	se.fresh = tmpT.String()
	changes, warnings, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false)
	if err != nil || len(warnings) != 0 || len(changes) != 1 || changes[0].Property != "ensure" || changes[0].Err != nil {
		t.Fatalf("Apply = %+v, %v, %v", changes, warnings, err)
	}
	if want := []string{p + "=system_u:object_r:etc_t:s0"}; !slices.Equal(se.sets, want) {
		t.Errorf("sets = %v, want %v", se.sets, want)
	}
	// get_create_mode (util/selinux.rb:231-242): the path did not exist.
	if !slices.Equal(se.lookups, []uint32{syscall.S_IFREG}) {
		t.Errorf("lookups = %o, want [S_IFREG]", se.lookups)
	}
}

func TestSELinuxCreatedDirectoryAndLinkUseTheirEnsuresMode(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		mode   uint32
	}{
		{map[string]any{"ensure": "directory"}, syscall.S_IFDIR},
		{map[string]any{"ensure": "link", "target": "x"}, syscall.S_IFLNK},
		{map[string]any{"ensure": "x"}, syscall.S_IFLNK},
		{map[string]any{"target": "x"}, 0},  // ensure is inferred after defaults (type/file.rb:553-567)
		{map[string]any{"content": "x"}, 0}, // likewise
	} {
		p := filepath.Join(t.TempDir(), "p")
		se := enabledSE(p, seContext{})
		se.fresh = tmpT.String()
		changes, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, tc.params), false)
		if err != nil || len(changes) != 1 || changes[0].Err != nil {
			t.Fatalf("%v: Apply = %+v, %v", tc.params, changes, err)
		}
		if !slices.Equal(se.lookups, []uint32{tc.mode}) {
			t.Errorf("%v: lookups = %o, want [%o]", tc.params, se.lookups, tc.mode)
		}
		if want := []string{p + "=system_u:object_r:etc_t:s0"}; !slices.Equal(se.sets, want) {
			t.Errorf("%v: sets = %v, want %v", tc.params, se.sets, want)
		}
	}
}

// The default is looked up when Puppet instantiates the resource, before
// anything is changed, so a path that ensure replaces is looked up with its
// old type (util/selinux.rb:247-258).
func TestSELinuxLookupUsesThePathsTypeBeforeTheRun(t *testing.T) {
	dir := t.TempDir()
	l := filepath.Join(dir, "l")
	if err := os.Symlink("nowhere", l); err != nil {
		t.Fatal(err)
	}
	se := enabledSE(l, tmpT)
	_, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(l, map[string]any{"ensure": "file"}), false)
	if err != nil || len(se.lookups) != 1 || se.lookups[0]&syscall.S_IFMT != syscall.S_IFLNK {
		t.Fatalf("err = %v, lookups = %o; want one S_IFLNK", err, se.lookups)
	}
}

// Under links => manage the link itself is compared and relabelled.
func TestSELinuxLabelsTheLinkItself(t *testing.T) {
	dir := t.TempDir()
	f, l := filepath.Join(dir, "f"), filepath.Join(dir, "l")
	mustWrite(t, f, "x")
	if err := os.Symlink("f", l); err != nil {
		t.Fatal(err)
	}
	se := enabledSE(l, tmpT)
	se.cur[f] = etcT.String()
	changes, _, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(l, map[string]any{"ensure": "link", "target": "f"}), false)
	if err != nil || len(changes) != 1 || changes[0].Message != "seltype changed 'tmp_t' to 'etc_t'" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if want := []string{l + "=system_u:object_r:etc_t:s0"}; !slices.Equal(se.sets, want) {
		t.Errorf("sets = %v, want %v", se.sets, want)
	}
}

// D-28 retired. Measured by the SELinux gate: Puppet relabels a symlink found
// by a recursion walk (seltype changed 'tmp_t' to 'etc_t'), looked up as a
// link, and never its target, whether that is outside the tree or inside it.
func TestSELinuxRelabelsAWalkedLinkItself(t *testing.T) {
	base := t.TempDir()
	dir, links := walkedLinks(t, base)
	parent := raltest.FileRes(dir, map[string]any{"recurse": true})
	for link, target := range links {
		se := enabledSE(link, tmpT)
		if target != "" {
			se.cur[target] = tmpT.String()
		}
		changes, _, err := File{se: se}.Apply(context.Background(), childOf(parent, link, nil), false)
		if err != nil || len(changes) != 1 || changes[0].Message != "seltype changed 'tmp_t' to 'etc_t'" || changes[0].Err != nil {
			t.Errorf("%s: Apply = %+v, %v", link, changes, err)
		}
		if want := []string{link + "=system_u:object_r:etc_t:s0"}; !slices.Equal(se.sets, want) {
			t.Errorf("%s: sets = %v, want %v", link, se.sets, want)
		}
		if !slices.Equal(se.lookups, []uint32{syscall.S_IFLNK | 0o777}) {
			t.Errorf("%s: lookups = %o, want the link's own mode", link, se.lookups)
		}
	}
}

// The real helpers on a generated child: opened through the tree's os.Root
// with O_PATH|O_NOFOLLOW, a walked link is the object read and written, so an
// xattr set on it addresses the link (which refuses user xattrs) and never
// its target inside the tree.
func TestXattrHelpersActOnALinkItselfThroughTheRoot(t *testing.T) {
	dir := t.TempDir()
	f, l := filepath.Join(dir, "f"), filepath.Join(dir, "l")
	mustWrite(t, f, "x")
	if err := os.Symlink("f", l); err != nil {
		t.Fatal(err)
	}
	if err := setfileconRaw(osFS{}, f, "user.voxagent", "v"); err != nil {
		t.Skipf("user xattrs are not supported here (%v)", err)
	}
	root, err := openDirNoFollow(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rfs := rootFS{root: root, dir: dir}
	if got, err := getfileconRaw(rfs, l, "user.voxagent"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("through the link: %q, %v; want the link's own ENODATA", got, err)
	}
	if err := setfileconRaw(rfs, l, "user.voxagent", "w"); err == nil {
		t.Error("set through the link succeeded; it must address the link, which refuses user xattrs")
	}
	if got, _ := getfileconRaw(osFS{}, f, "user.voxagent"); got != "v" {
		t.Errorf("target = %q, want it untouched", got)
	}
}

func TestSELinuxSetFailureWarnsAndStillReports(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.setErr = syscall.EINVAL
	changes, warnings, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false)
	if err != nil || len(changes) != 1 || changes[0].Err != nil || changes[0].Message != "seltype changed 'tmp_t' to 'etc_t'" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	want := []ral.Warning{{Source: "Puppet", Message: "Failed to set SELinux context system_u:object_r:etc_t:s0 on " + p}}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %+v, want %+v", warnings, want)
	}
}

func TestSELinuxCreatedSetFailureOnlyWarns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	se := enabledSE(p, seContext{})
	se.fresh = tmpT.String()
	se.setErr = syscall.EINVAL
	changes, warnings, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false)
	if err != nil || len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	want := []ral.Warning{{Source: "Puppet", Message: "Failed to set SELinux context system_u:object_r:etc_t:s0 on " + p}}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %+v, want %+v", warnings, want)
	}
}

// An existing file with no readable context: Puppet's retrieve maps the
// failed lgetfilecon to nil (util/selinux.rb:39-42), which displays as
// nothing, and the sync's own lgetfilecon returns the bare -1 that
// set_selinux_context indexes and splits (:123-131).
func TestSELinuxUnlabeledFileFailsAsPuppetDoes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, seContext{})
	changes, warnings, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "etc_t", "selinux_ignore_defaults": true}), false)
	if err != nil || len(warnings) != 0 || len(changes) != 1 {
		t.Fatalf("Apply = %+v, %v, %v", changes, warnings, err)
	}
	ch := changes[0]
	if ch.Was != nil || ch.Message != "seltype changed  to 'etc_t'" ||
		ch.Err == nil || ch.Err.Error() != "undefined method 'split' for an instance of Integer" {
		t.Errorf("change = %+v", ch)
	}
	if len(se.sets) != 0 {
		t.Errorf("sets = %v, want none", se.sets)
	}
}

// property_fix's failure on a created path is ensure's, wrapped
// (type/file.rb:1112-1121, property.rb:492).
func TestSELinuxCreatedUnlabeledFileFailsEnsure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	se := enabledSE(p, seContext{})
	changes, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false)
	if err != nil || len(changes) != 1 || changes[0].Err == nil ||
		changes[0].Err.Error() != "Could not set 'file' on ensure: undefined method 'split' for an instance of Integer" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
}

func TestSELinuxNoopSetsNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	changes, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), true)
	if err != nil || len(changes) != 1 || changes[0].Message != "seltype changed 'tmp_t' to 'etc_t' (noop)" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	q := filepath.Join(t.TempDir(), "absent")
	se2 := enabledSE(q, seContext{})
	if _, _, err := (File{se: se2}).Apply(context.Background(), raltest.FileRes(q, map[string]any{"ensure": "file"}), true); err != nil {
		t.Fatal(err)
	}
	if len(se.sets) != 0 || len(se2.sets) != 0 {
		t.Errorf("sets = %v, %v; want none", se.sets, se2.sets)
	}
}

func TestSELinuxComesAfterModeOwnerAndGroup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	changes, _, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "mode": "0600", "selrange": "s0:c1"}), false)
	var got []string
	for _, c := range changes {
		got = append(got, c.Property)
	}
	if err != nil || !slices.Equal(got, []string{"mode", "seltype", "selrange"}) {
		t.Fatalf("properties = %v, %v", got, err)
	}
}

// A content write replaces the file, and the new inode does not carry the old
// label; write's property_fix (type/file.rb:970, :1112-1121) relabels it
// silently even though the retrieved label was already in sync.
func TestSELinuxContentRewriteIsRelabelledSilently(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "old")
	se := enabledSE(p, etcT)
	se.fresh = tmpT.String()
	changes, _, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "content": "new"}), false)
	if err != nil || len(changes) != 1 || changes[0].Property != "content" || changes[0].Err != nil {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if c, _ := se.current(osFS{}, p); c != etcT.String() {
		t.Errorf("label after the write = %v, want %v", c, etcT)
	}
}

func TestSELinuxDefaultLookupFailureFailsTheResource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.defErr = errors.New("Cannot get default context with nil handle")
	if _, _, err := (File{se: se}).Apply(context.Background(), raltest.FileRes(p, map[string]any{"ensure": "file"}), false); err == nil ||
		err.Error() != "Cannot get default context with nil handle" {
		t.Fatalf("err = %v", err)
	}
}

func TestSELinuxUnmanagedEnsureRelabelsAnExistingPathOnly(t *testing.T) {
	dir := t.TempDir()
	p, q := filepath.Join(dir, "f"), filepath.Join(dir, "absent")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	changes, _, err := File{se: se}.Apply(context.Background(), raltest.FileRes(p, map[string]any{"seltype": "etc_t"}), false)
	if err != nil || len(changes) != 1 || changes[0].Property != "seltype" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	changes, _, err = File{se: se}.Apply(context.Background(), raltest.FileRes(q, map[string]any{"seltype": "etc_t"}), false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("absent path: Apply = %+v, %v", changes, err)
	}
}

func TestFileValidateRefusesWhenSELinuxIsEnabledWithoutTheLookupTool(t *testing.T) {
	se := &fakeSE{on: true, supported: true}
	err := File{se: se}.Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"}))
	want := "File[/etc/x]: SELinux is enabled, but selabel_lookup (package libselinux-utils) is not installed, " +
		"so default SELinux contexts cannot be computed as Puppet computes them"
	if err == nil || err.Error() != want {
		t.Fatalf("Validate = %v\nwant %s", err, want)
	}
	se.tool = true
	if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"})); err != nil {
		t.Errorf("with the tool: Validate = %v", err)
	}
	se.on, se.tool = false, false
	if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"})); err != nil {
		t.Errorf("disabled: Validate = %v", err)
	}
}

// Only a resource that will look a default up needs the tool: a removal syncs
// no SELinux property, and a resource declaring all four wants no default.
// replace => false keeps an existing path and syncs its properties, so its
// removal does look one up.
func TestFileValidateRefusesAMissingToolOnlyWhereALookupIsNeeded(t *testing.T) {
	se := &fakeSE{on: true, supported: true}
	all := map[string]any{"ensure": "file", "seluser": "system_u", "selrole": "object_r", "seltype": "etc_t", "selrange": "s0"}
	for _, params := range []map[string]any{{"ensure": "absent"}, {"ensure": false}, all} {
		if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", params)); err != nil {
			t.Errorf("%v: Validate = %v; want no refusal", params, err)
		}
	}
	if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "absent", "replace": false})); err == nil {
		t.Error("absent with replace => false: Validate accepted it; its kept path's defaults need the tool")
	}
}

// Measured on the SELinux gate's VM (openvox 8.29.0) with file_contexts moved
// aside: Puppet fails the whole run before applying anything, "Could not
// prepare for execution: Could not create resources for managing ...'s files
// and directories in sections [:main, :agent, :ssl]: Cannot get default
// context with nil handle", and writes no report. So a File that would look
// a default up refuses the catalog; one that would not is unaffected.
func TestFileValidateRefusesTheCatalogWhenNoHandleOpens(t *testing.T) {
	se := &fakeSE{on: true, supported: true, tool: true, handleErr: errNilHandle}
	err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"}))
	want := "File[/etc/x]: Cannot get default context with nil handle"
	if err == nil || err.Error() != want {
		t.Fatalf("Validate = %v; want %q", err, want)
	}
	all := map[string]any{"seluser": "system_u", "selrole": "object_r", "seltype": "etc_t", "selrange": "s0"}
	for _, params := range []map[string]any{{"ensure": "absent"}, {"selinux_ignore_defaults": true}, all} {
		if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", params)); err != nil {
			t.Errorf("%v: Validate = %v; want no refusal, it looks nothing up", params, err)
		}
	}
	se.supported = false
	if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"})); err != nil {
		t.Errorf("no label support: Validate = %v; Puppet looks nothing up there", err)
	}
	se.supported, se.handleErr = true, nil
	if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"ensure": "file"})); err != nil {
		t.Errorf("an opening handle: Validate = %v", err)
	}
}

func TestFileValidateAllowsAMissingToolWhenDefaultsAreIgnored(t *testing.T) {
	se := &fakeSE{on: true, supported: true}
	for _, v := range []any{true, "true"} {
		if err := (File{se: se}).Validate(raltest.FileRes("/etc/x", map[string]any{"selinux_ignore_defaults": v})); err != nil {
			t.Errorf("%v: Validate = %v", v, err)
		}
	}
}

func TestFileValidateSELinuxValues(t *testing.T) {
	se := &fakeSE{}
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		// newvalues(:true, :false) (type/file/selcontext.rb:94); measured.
		{map[string]any{"selinux_ignore_defaults": "yes"},
			`Parameter selinux_ignore_defaults failed on File[/etc/x]: Invalid value "yes". Valid values are true, false.`},
		{map[string]any{"selinux_ignore_defaults": 1.0},
			`Parameter selinux_ignore_defaults failed on File[/etc/x]: Invalid value "1". Valid values are true, false.`},
		{map[string]any{"seltype": 5.0},
			"File[/etc/x]: seltype => 5 (float64) is not implemented: an SELinux context field is a string"},
		{map[string]any{"seltype": "etc_t", "selinux_ignore_defaults": false}, ""},
	} {
		err := File{se: se}.Validate(raltest.FileRes("/etc/x", tc.params))
		if got := fmt.Sprint(err); (tc.want == "" && err != nil) || (tc.want != "" && got != tc.want) {
			t.Errorf("%v: Validate = %v\nwant %s", tc.params, err, tc.want)
		}
	}
}

// get_selinux_current_context maps every lgetfilecon failure to no context
// (util/selinux.rb:39-42), not only a missing label.
func TestCurrentMapsEveryReadFailureToNoContext(t *testing.T) {
	if c, ok := (hostSELinux{}).current(osFS{}, filepath.Join(t.TempDir(), "nope")); ok {
		t.Errorf("missing file: current = %q, true; want no context", c)
	}
}

func TestFileGenerateChildrenInheritTheSELinuxParameters(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a"), "x")
	params := map[string]any{"recurse": true, "seluser": "system_u", "selrole": "object_r",
		"seltype": "etc_t", "selrange": "s0", "selinux_ignore_defaults": true}
	children, _, err := File{}.Generate(context.Background(), raltest.FileRes(dir, params), noneManaged)
	if err != nil || len(children) != 1 {
		t.Fatalf("Generate = %v, %v", children, err)
	}
	for _, k := range []string{"seluser", "selrole", "seltype", "selrange", "selinux_ignore_defaults"} {
		if children[0].Parameters[k] != params[k] {
			t.Errorf("child %s = %v, want %v", k, children[0].Parameters[k], params[k])
		}
	}
}

// userXattrSE is an enabled SELinux whose labels are the user.voxagent
// extended attribute, read and written by the real helpers: the provider's
// own confinement is exercised without security.selinux writes.
type userXattrSE struct{ fakeSE }

// current counts the reads that found a label in reads.
func (se *userXattrSE) current(fsys fileSystem, p string) (string, bool) {
	s, err := getfileconRaw(fsys, p, "user.voxagent")
	if err == nil {
		se.reads++
	}
	return s, err == nil
}
func (se *userXattrSE) set(fsys fileSystem, p, c string) error {
	se.sets = append(se.sets, p+"="+c)
	return setfileconRaw(fsys, p, "user.voxagent", c)
}

func TestSELinuxGeneratedChildIsLabelledOnlyInsideItsTree(t *testing.T) {
	tree, outside := t.TempDir(), t.TempDir()
	mustMkdir(t, filepath.Join(tree, "sub"))
	child, out := filepath.Join(tree, "sub", "f"), filepath.Join(outside, "f")
	mustWrite(t, child, "in")
	mustWrite(t, out, "out")
	for _, p := range []string{child, out} {
		if err := setfileconRaw(osFS{}, p, "user.voxagent", tmpT.String()); err != nil {
			t.Skipf("user xattrs are not supported here (%v)", err)
		}
	}
	se := &userXattrSE{fakeSE{on: true, supported: true, tool: true}}
	parent := raltest.FileRes(tree, map[string]any{"recurse": true, "seltype": "etc_t", "selinux_ignore_defaults": true})
	r := childOf(parent, child, map[string]any{"seltype": "etc_t", "selinux_ignore_defaults": true})

	changes, _, err := File{se: se}.Apply(context.Background(), r, false)
	if err != nil || len(changes) != 1 || changes[0].Message != "seltype changed 'tmp_t' to 'etc_t'" {
		t.Fatalf("in the tree: Apply = %+v, %v", changes, err)
	}
	if got, _ := getfileconRaw(osFS{}, child, "user.voxagent"); got != etcT.String() {
		t.Fatalf("child label = %q, want %q", got, etcT)
	}

	// The walk saw sub as a directory; it is now a link out of the tree.
	if err := os.RemoveAll(filepath.Join(tree, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "sub")); err != nil {
		t.Fatal(err)
	}
	se.sets, se.reads = nil, 0
	changes, _, err = File{se: se}.Apply(context.Background(), r, false)
	if len(changes) != 0 || len(se.sets) != 0 || se.reads != 0 {
		t.Errorf("swapped parent: Apply = %+v, %v; sets %v, %d labels read; want nothing read or applied",
			changes, err, se.sets, se.reads)
	}
	if got, _ := getfileconRaw(osFS{}, out, "user.voxagent"); got != tmpT.String() {
		t.Errorf("outside file label = %q, want it untouched", got)
	}
}

// Puppet's sync reads the raw context and splits it unparsed
// (util/selinux.rb:123-144): "unlabeled" retrieves as nothing but is edited
// into "unlabeled::etc_t", which the kernel refuses, so the change warns and
// succeeds.
func TestSELinuxUnlabeledContextIsEditedAsARawString(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, seContext{})
	se.cur[p] = "unlabeled"
	se.setErr = syscall.EINVAL
	changes, warnings, err := File{se: se}.Apply(context.Background(),
		raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "etc_t", "selinux_ignore_defaults": true}), false)
	if err != nil || len(changes) != 1 || changes[0].Was != nil || changes[0].Err != nil ||
		changes[0].Message != "seltype changed  to 'etc_t'" {
		t.Fatalf("Apply = %+v, %v", changes, err)
	}
	if want := []string{p + "=unlabeled::etc_t"}; !slices.Equal(se.sets, want) {
		t.Errorf("sets = %v, want %v", se.sets, want)
	}
	want := []ral.Warning{{Source: "Puppet", Message: "Failed to set SELinux context unlabeled::etc_t on " + p}}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %+v, want %+v", warnings, want)
	}
}

// File{} reaches defaultSELinux, which TestMain makes a disabled fake: the
// package's tests never call the host's SELinux by accident.
func TestPackageTestsUseADisabledSELinux(t *testing.T) {
	if se, ok := (File{}).selinux().(*fakeSE); !ok || se.enabled() {
		t.Fatalf("File{}.selinux() = %#v; want TestMain's disabled fake", (File{}).selinux())
	}
}

// matchpathconFixture is a captured `matchpathcon -n -m f -- <paths>`
// (libselinux-utils 3.11, selinux-policy-targeted 42.1.27): one line per
// path, in argv order, "<<none>>" for the miss.
var matchpathconFixturePaths = []string{"/etc/shadow", "/proc/1", "/var/www/html/sp ace/i", "/etc/tab\there"}

func TestDefaultsForBatchesThroughMatchpathcon(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/matchpathcon-batch.txt")
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"matchpathcon", "-n", "-m", "f", "--"}, matchpathconFixturePaths...)
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{strings.Join(argv, " "): {Out: string(out)}}}
	var qs []defaultQuery
	for i, p := range matchpathconFixturePaths {
		// Permission bits differ; only the file type picks -m.
		qs = append(qs, defaultQuery{path: p, mode: syscall.S_IFREG | uint32(0o600+i)})
	}
	answers := hostSELinux{run: fr}.defaultsFor(context.Background(), qs)
	if got := fr.OnlyCall(t); !slices.Equal(got, argv) {
		t.Errorf("args = %q; want %q", got, argv)
	}
	want := []defaultAnswer{
		{ctx: seContext{"system_u", "object_r", "shadow_t", "s0"}, ok: true},
		{},
		{ctx: seContext{"system_u", "object_r", "httpd_sys_content_t", "s0"}, ok: true},
		{ctx: etcT, ok: true},
	}
	if len(answers) != len(qs) {
		t.Fatalf("answers = %v; want one per query", answers)
	}
	for i, q := range qs {
		if a, ok := answers[q]; !ok || a != want[i] {
			t.Errorf("%q: answer = %+v, %v; want %+v", q.path, a, ok, want[i])
		}
	}
}

// matchpathcon exits 255 when selabel_open fails (utils/matchpathcon.c:134-140),
// on the same handle selabel_lookup opens, so every path gets Puppet's
// nil-handle error, as its own selabel_lookup would.
func TestDefaultsForWithNoHandleIsPuppetsError(t *testing.T) {
	out, err := os.ReadFile("testdata/selinux/matchpathcon-open-failed.txt")
	if err != nil {
		t.Fatal(err)
	}
	fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
		"matchpathcon -n -m f -- /etc/shadow /etc/passwd": {Exit: 255, Out: string(out)},
	}}
	qs := []defaultQuery{{"/etc/shadow", syscall.S_IFREG}, {"/etc/passwd", syscall.S_IFREG}}
	answers := hostSELinux{run: fr}.defaultsFor(context.Background(), qs)
	for _, q := range qs {
		a, ok := answers[q]
		if !ok || a.ok || a.err == nil || a.err.Error() != "Cannot get default context with nil handle" {
			t.Errorf("%s: answer = %+v, %v; want Puppet's nil-handle error", q.path, a, ok)
		}
	}
}

// One matchpathcon per file type, each chunked under matchpathconArgBytes. A
// mode with no file type is never batched: -m cannot force mode 0, and
// without -m matchpathcon lstats the path itself (matchpathcon.c:149-152).
func TestDefaultsForGroupsByTypeAndChunks(t *testing.T) {
	fr := &raltest.FakeRunner{}
	types := map[uint32]string{
		syscall.S_IFREG: "f", syscall.S_IFDIR: "d", syscall.S_IFLNK: "l", syscall.S_IFIFO: "p",
		syscall.S_IFSOCK: "s", syscall.S_IFCHR: "c", syscall.S_IFBLK: "b",
	}
	var qs []defaultQuery
	for mode, letter := range types {
		qs = append(qs, defaultQuery{path: "/t/" + letter, mode: mode | 0o644})
	}
	qs = append(qs, defaultQuery{path: "/t/zero", mode: 0})
	hostSELinux{run: fr}.defaultsFor(context.Background(), qs)
	var letters []string
	for _, call := range fr.Calls {
		if len(call) != 6 || call[0] != "matchpathcon" || call[1] != "-n" || call[2] != "-m" || call[4] != "--" || call[5] != "/t/"+call[3] {
			t.Errorf("call = %q; want matchpathcon -n -m <t> -- /t/<t>", call)
			continue
		}
		letters = append(letters, call[3])
	}
	if want := []string{"b", "c", "d", "f", "l", "p", "s"}; !slices.Equal(letters, want) {
		t.Errorf("types = %q; want one call each, in order %q", letters, want)
	}

	fr = &raltest.FakeRunner{}
	var long []defaultQuery
	var wantPaths []string
	for i := range 3000 {
		p := fmt.Sprintf("/%s/%04d", strings.Repeat("x", 90), i)
		long = append(long, defaultQuery{path: p, mode: syscall.S_IFREG})
		wantPaths = append(wantPaths, p)
	}
	hostSELinux{run: fr}.defaultsFor(context.Background(), long)
	if len(fr.Calls) < 2 {
		t.Fatalf("%d calls for %d paths; want them chunked", len(fr.Calls), len(long))
	}
	var gotPaths []string
	for _, call := range fr.Calls {
		size := 0
		for _, a := range call {
			size += len(a) + 1 + 8
		}
		if size > matchpathconArgBytes {
			t.Errorf("a call's argv is %d bytes; want at most %d", size, matchpathconArgBytes)
		}
		gotPaths = append(gotPaths, call[5:]...)
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Errorf("the chunks do not carry every path once, in order")
	}
}

// Output that cannot be mapped back to its paths line by line is not
// answered, and those paths go to selabel_lookup one by one.
func TestDefaultsForLeavesWhatItCannotParseToSelabelLookup(t *testing.T) {
	good := "matchpathcon -n -m f -- /a/f"
	bad := "matchpathcon -n -m d -- /a/d1 /a/d2"
	for name, reply := range map[string]raltest.FakeReply{
		"a failed lookup":      {Exit: 1, Out: "selabel_lookup(/a/d1) failed: Invalid argument\nsystem_u:object_r:etc_t:s0\n"},
		"a line short":         {Out: "system_u:object_r:etc_t:s0\n"},
		"an extra stderr line": {Out: "warning\nsystem_u:object_r:etc_t:s0\nsystem_u:object_r:etc_t:s0\n"},
		"no final newline":     {Out: "system_u:object_r:etc_t:s0\nsystem_u:object_r:etc_t:s0"},
		"failure to start":     {Err: errors.New("exec: not found")},
		"an unknown exit":      {Exit: 2, Out: "system_u:object_r:etc_t:s0\nsystem_u:object_r:etc_t:s0\n"},
	} {
		fr := &raltest.FakeRunner{Replies: map[string]raltest.FakeReply{
			good: {Out: "system_u:object_r:etc_t:s0\n"},
			bad:  reply,
			"selabel_lookup -b file -k /a/d2 -t 16384": {Out: "Default context: system_u:object_r:bin_t:s0\n"},
		}}
		qs := []defaultQuery{{"/a/f", syscall.S_IFREG}, {"/a/d1", syscall.S_IFDIR}, {"/a/d2", syscall.S_IFDIR}}
		answers := hostSELinux{run: fr}.defaultsFor(context.Background(), qs)
		if _, ok := answers[qs[0]]; !ok || len(answers) != 1 {
			t.Errorf("%s: answers = %v; want only /a/f", name, answers)
		}
		se := prefetchedSELinux{selinuxOps: hostSELinux{run: fr}, defaults: answers}
		fr.Calls = nil
		if c, ok, err := se.defaultFor(context.Background(), "/a/f", syscall.S_IFREG); err != nil || !ok || c != etcT || len(fr.Calls) != 0 {
			t.Errorf("%s: /a/f = %v, %v, %v after %q; want the batch's answer and no command", name, c, ok, err, fr.Calls)
		}
		c, ok, err := se.defaultFor(context.Background(), "/a/d2", syscall.S_IFDIR)
		want := []string{"selabel_lookup", "-b", "file", "-k", "/a/d2", "-t", "16384"}
		if err != nil || !ok || c.typ != "bin_t" || len(fr.Calls) != 1 || !slices.Equal(fr.Calls[0], want) {
			t.Errorf("%s: /a/d2 = %v, %v, %v after %q; want selabel_lookup's answer", name, c, ok, err, fr.Calls)
		}
	}
}

// An answer is used once and only for the exact query: a path whose type
// changed since the batch is looked up again with its new mode.
func TestPrefetchedSELinuxAnswersOnlyTheQueryItWasGiven(t *testing.T) {
	base := &fakeSE{def: tmpT}
	q := defaultQuery{"/a/f", syscall.S_IFREG | 0o644}
	se := prefetchedSELinux{selinuxOps: base, defaults: map[defaultQuery]defaultAnswer{q: {ctx: etcT, ok: true}}}
	ctx := context.Background()
	if c, _, _ := se.defaultFor(ctx, "/a/f", syscall.S_IFDIR|0o755); c != tmpT {
		t.Errorf("another mode = %v; want a fresh lookup", c)
	}
	if c, _, _ := se.defaultFor(ctx, "/a/f", q.mode); c != etcT {
		t.Errorf("the query = %v; want the batch's answer", c)
	}
	if c, _, _ := se.defaultFor(ctx, "/a/f", q.mode); c != tmpT {
		t.Errorf("the query again = %v; want a fresh lookup", c)
	}
	if !slices.Equal(base.lookups, []uint32{syscall.S_IFDIR | 0o755, q.mode}) {
		t.Errorf("fresh lookups = %o", base.lookups)
	}
}

func lstatMode(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Mode
}

// A recursion's children get their defaults from one batch, made when
// Generate creates them -- Puppet's instantiation of the children -- with each
// child's own pre-run type, and Apply then runs no lookup of its own.
func TestSELinuxRecursionLooksDefaultsUpInOneBatch(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a"), "x")
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "d", "b"), "x")
	if err := os.Symlink("a", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	se := enabledSE(dir, seContext{})
	se.fresh = tmpT.String()
	prov, err := File{se: se}.Prefetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f := prov.(File)
	children, _, err := f.Generate(context.Background(), raltest.FileRes(dir, map[string]any{"recurse": true}), noneManaged)
	if err != nil {
		t.Fatal(err)
	}
	var want []defaultQuery
	for _, c := range children {
		want = append(want, defaultQuery{c.Title, lstatMode(t, c.Title)})
	}
	if len(se.batches) != 1 || !slices.Equal(se.batches[0], want) {
		t.Fatalf("batches = %v; want one with %v", se.batches, want)
	}
	for _, c := range children {
		if _, _, err := f.Apply(context.Background(), c, false); err != nil {
			t.Fatalf("Apply %s: %v", c.Title, err)
		}
	}
	if len(se.lookups) != 0 {
		t.Errorf("per-path lookups = %o; want none", se.lookups)
	}
	if !slices.Contains(se.sets, filepath.Join(dir, "a")+"="+etcT.String()) {
		t.Errorf("sets = %v; want the batch's default applied", se.sets)
	}
}

// A child whose type changed between Generate and its Apply is looked up
// again with the type Apply sees, as before batching.
func TestSELinuxRecursionChildChangedAfterTheBatchIsLookedUpAgain(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	mustWrite(t, a, "x")
	se := enabledSE(dir, seContext{})
	prov, _ := File{se: se}.Prefetch(context.Background(), nil)
	f := prov.(File)
	children, _, err := f.Generate(context.Background(), raltest.FileRes(dir, map[string]any{"recurse": true}), noneManaged)
	if err != nil || len(children) != 1 {
		t.Fatalf("Generate = %v, %v", titles(children), err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Apply(context.Background(), children[0], false); err != nil {
		t.Fatal(err)
	}
	if len(se.lookups) != 1 || se.lookups[0]&syscall.S_IFMT != syscall.S_IFDIR {
		t.Errorf("per-path lookups = %o; want one with the directory's mode", se.lookups)
	}
}

// No batch is made where Apply would look nothing up, nor by a provider that
// was not prefetched for a run.
func TestSELinuxRecursionBatchesOnlyWhenDefaultsAreWanted(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a"), "x")
	for name, tc := range map[string]struct {
		se         *fakeSE
		params     map[string]any
		noPrefetch bool
	}{
		"SELinux disabled":         {se: &fakeSE{}, params: map[string]any{}},
		"an unlabelled filesystem": {se: &fakeSE{on: true}, params: map[string]any{}},
		"defaults ignored":         {se: enabledSE(dir, seContext{}), params: map[string]any{"selinux_ignore_defaults": "true"}},
		"all four declared": {se: enabledSE(dir, seContext{}), params: map[string]any{
			"seluser": "u", "selrole": "r", "seltype": "t", "selrange": "s0"}},
		"not prefetched": {se: enabledSE(dir, seContext{}), params: map[string]any{}, noPrefetch: true},
	} {
		tc.params["recurse"] = true
		var f = File{se: tc.se}
		if !tc.noPrefetch {
			prov, _ := f.Prefetch(context.Background(), nil)
			f = prov.(File)
		}
		children, _, err := f.Generate(context.Background(), raltest.FileRes(dir, tc.params), noneManaged)
		if err != nil || len(children) != 1 {
			t.Fatalf("%s: Generate = %v, %v", name, titles(children), err)
		}
		if len(tc.se.batches) != 0 {
			t.Errorf("%s: batches = %v; want none", name, tc.se.batches)
		}
	}
}

// A purged child is removed, which looks nothing up, so it is not batched.
func TestSELinuxRecursionDoesNotBatchAPurgedChild(t *testing.T) {
	dir, src := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(dir, "gone"), "x")
	mustWrite(t, filepath.Join(dir, "kept"), "x")
	mustWrite(t, filepath.Join(src, "kept"), "y")
	se := enabledSE(dir, seContext{})
	prov, _ := File{se: se}.Prefetch(context.Background(), nil)
	children, _, err := prov.(File).Generate(context.Background(),
		raltest.FileRes(dir, map[string]any{"recurse": true, "purge": true, "source": src}), noneManaged)
	if err != nil || len(children) != 2 {
		t.Fatalf("Generate = %v, %v", titles(children), err)
	}
	kept := filepath.Join(dir, "kept")
	if want := []defaultQuery{{kept, lstatMode(t, kept)}}; len(se.batches) != 1 || !slices.Equal(se.batches[0], want) {
		t.Errorf("batches = %v; want only %v", se.batches, want)
	}
}

// type/file/selcontext.rb:54-63: a default found for an undeclared property is
// logged at that property's path, once per property; a declared property, and
// selinux_ignore_defaults, look nothing up.
func TestSELinuxLogsTheDefaultsItFinds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   map[string]string
	}{
		{"all four", map[string]any{"ensure": "file"}, map[string]string{
			"seluser": "system_u", "selrole": "object_r", "seltype": "etc_t", "selrange": "s0"}},
		{"a declared property is not defaulted", map[string]any{"ensure": "file", "seltype": "bin_t"}, map[string]string{
			"seluser": "system_u", "selrole": "object_r", "selrange": "s0"}},
		{"ignore defaults", map[string]any{"ensure": "file", "selinux_ignore_defaults": true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			mustWrite(t, p, "x")
			src := fileSources(p)

			ctx, got := raltest.DebugContext("debug")
			if _, _, err := (File{se: enabledSE(p, tmpT)}).Apply(sources.WithSources(ctx, src), raltest.FileRes(p, tc.params), false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			for _, prop := range selProps {
				var want []string
				if v, ok := tc.want[prop]; ok {
					want = []string{fmt.Sprintf("Found %s default '%s' for %s", prop, v, p)}
				}
				if lines := raltest.LinesAt(*got, src.Resource+"/"+prop); !slices.Equal(lines, want) {
					t.Errorf("debug lines at %s/%s = %q, want %q", src.Resource, prop, lines, want)
				}
			}
		})
	}

	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	ctx, got := raltest.DebugContext("notice")
	if _, _, err := (File{se: enabledSE(p, tmpT)}).Apply(sources.WithSources(ctx, fileSources(p)),
		raltest.FileRes(p, map[string]any{"ensure": "file"}), false); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(*got) != 0 {
		t.Errorf("entries = %+v, want none at notice", *got)
	}
}

// type/file/selcontext.rb:58-63: with SELinux unavailable, each declared
// property says it is ignored. An undeclared one has no default to look up and
// no value, so it says nothing.
func TestSELinuxLogsWhyADeclaredPropertyIsIgnored(t *testing.T) {
	for _, tc := range []struct {
		name string
		se   *fakeSE
		want string
	}{
		{"no SELinux", &fakeSE{}, "SELinux bindings not found. Ignoring parameter."},
		{"unlabelled filesystem", &fakeSE{on: true}, "SELinux not available for this filesystem. Ignoring parameter."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f")
			mustWrite(t, p, "x")
			src := fileSources(p)

			ctx, got := raltest.DebugContext("debug")
			r := raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "bin_t", "selrole": "object_r"})
			if _, _, err := (File{se: tc.se}).Apply(sources.WithSources(ctx, src), r, false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			for _, prop := range selProps {
				var want []string
				if prop == "seltype" || prop == "selrole" {
					want = []string{tc.want}
				}
				if lines := raltest.LinesAt(*got, src.Resource+"/"+prop); !slices.Equal(lines, want) {
					t.Errorf("debug lines at %s/%s = %q, want %q", src.Resource, prop, lines, want)
				}
			}

			ctx, got = raltest.DebugContext("notice")
			if _, _, err := (File{se: tc.se}).Apply(sources.WithSources(ctx, src), r, false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			if len(*got) != 0 {
				t.Errorf("entries = %+v, want none at notice", *got)
			}
		})
	}
}

// D-78: the context a failed set warns about carries every field, so a marked
// sel* property prints it as [redacted], where Ruby prints it.
func TestSELinuxSetFailureWarningRedactsMarkedContext(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	mustWrite(t, p, "x")
	se := enabledSE(p, tmpT)
	se.setErr = syscall.EINVAL
	r := raltest.FileRes(p, map[string]any{"ensure": "file", "seltype": "etc_t"})
	r.SensitiveParameters = []string{"seltype"}
	_, warnings, err := File{se: se}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []ral.Warning{{Source: "Puppet", Message: "Failed to set SELinux context [redacted] on " + p}}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %+v, want %+v", warnings, want)
	}
}
