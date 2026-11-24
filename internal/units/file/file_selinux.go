package file

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/puppetpath"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// This file holds the SELinux primitives the file provider needs to give
// file's seluser, selrole, seltype and selrange Puppet's behaviour. Puppet
// reaches libselinux through its Ruby bindings; voxagent has no cgo, so it
// reproduces the few libselinux functions Puppet calls from their source
// (libselinux 3.11, the EL10 source RPM, cited as libselinux/src/<file>:<line>)
// and gets defaults from libselinux's own selabel_lookup CLI, so path
// matching is never reimplemented.

// seContext is an SELinux context split into Puppet's four properties
// (util/selinux.rb:87-109). The range is everything after the third colon, and
// is empty when the context has none.
type seContext struct{ user, role, typ, rng string }

// contextPattern is util/selinux.rb:92's regexp. Ruby's ^ and $ are line
// anchors, hence (?m); Ruby's \s also matches \v, which Go's does not, hence
// the spelled-out class.
var contextPattern = regexp.MustCompile(
	`(?m)^([^\t\n\v\f\r :]+):([^\t\n\v\f\r :]+):([^\t\n\v\f\r :]+)(?::([\t\n\v\f\r a-zA-Z0-9:,._-]+))?$`)

// parseContext splits s as parse_selinux_context does (util/selinux.rb:87-95).
// "unlabeled", which Puppet maps to nil, and anything the pattern rejects, on
// which Puppet raises, both report false; puppetContext tells them apart.
func parseContext(s string) (seContext, bool) {
	if s == "unlabeled" {
		return seContext{}, false
	}
	m := contextPattern.FindStringSubmatch(s)
	if m == nil {
		return seContext{}, false
	}
	return seContext{user: m[1], role: m[2], typ: m[3], rng: m[4]}, true
}

// puppetContext is parse_selinux_context's whole contract
// (util/selinux.rb:87-95): "unlabeled" is no context, and a string the
// pattern rejects is Puppet's "Invalid context to parse" error.
func puppetContext(s string) (seContext, bool, error) {
	c, ok := parseContext(s)
	if !ok && s != "unlabeled" {
		return seContext{}, false, fmt.Errorf("Invalid context to parse: %s", s)
	}
	return c, ok, nil
}

// String renders the context as user:role:type, with :range when it has one.
func (c seContext) String() string {
	s := c.user + ":" + c.role + ":" + c.typ
	if c.rng != "" {
		s += ":" + c.rng
	}
	return s
}

// selProps are the four properties in context order, so a property's index
// is its colon-separated position (util/selinux.rb:97-105, 132-140).
var selProps = []string{"seluser", "selrole", "seltype", "selrange"}

// selPropIndex returns prop's position in a context. Any other name is a
// caller bug, as it is for Puppet (util/selinux.rb:107, 142).
func selPropIndex(prop string) int {
	i := slices.Index(selProps, prop)
	if i < 0 {
		panic("SELinux property must be one of seluser, selrole, seltype or selrange, not " + prop)
	}
	return i
}

// field is the value of prop (seluser/selrole/seltype/selrange) in c.
func (c seContext) field(prop string) string {
	return [...]string{c.user, c.role, c.typ, c.rng}[selPropIndex(prop)]
}

// withField is the context set_selinux_context writes to change prop to v
// (util/selinux.rb:121-144): raw, the context lgetfilecon just read, is split
// on every colon, the property's element is replaced, and the elements are
// joined back. raw is not parsed, so a context Puppet's pattern would reject,
// or the kernel's "unlabeled", is edited all the same.
//
// So a range with its own colon keeps what follows its first colon:
// selrange => s0 over s0-s0:c0.c1023 gives s0:c0.c1023 (I-7). Ruby's split
// drops trailing empty elements and its array assignment pads with nil, which
// join renders as "": seltype => etc_t over "unlabeled" gives
// "unlabeled::etc_t".
func withField(raw, prop, v string) string {
	parts := strings.Split(raw, ":")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	i := selPropIndex(prop)
	for len(parts) <= i {
		parts = append(parts, "")
	}
	parts[i] = v
	return strings.Join(parts, ":")
}

// selinuxOps is everything the file provider needs from SELinux, behind an
// interface so tests can fake an enabled host.
type selinuxOps interface {
	enabled() bool
	labelSupported(path string) bool
	// current is path's raw context, false when it cannot be read; set
	// writes one. Both act on path itself, resolved by fsys (fileconFD).
	current(fsys fileSystem, path string) (string, bool)
	set(fsys fileSystem, path, context string) error
	defaultFor(ctx context.Context, path string, mode uint32) (seContext, bool, error)
	// defaultsFor answers many defaultFor queries at once. A query it
	// cannot answer exactly as defaultFor would is left out, for defaultFor.
	defaultsFor(ctx context.Context, qs []defaultQuery) map[defaultQuery]defaultAnswer
	lookupAvailable() bool
	// handleOpens is nil when selabel_open can open the file-contexts
	// handle, errNilHandle when it cannot, and another error when that
	// could not be checked.
	handleOpens(ctx context.Context) error
}

// defaultQuery is one default lookup: the path and the mode passed for it.
type defaultQuery struct {
	path string
	mode uint32
}

// defaultAnswer is defaultFor's result for one defaultQuery.
type defaultAnswer struct {
	ctx seContext
	ok  bool
	err error
}

// hostSELinux is the real selinuxOps. run must be set for defaultFor and
// handleOpens; the other methods do not use it. handle memoizes handleOpens
// for the process, which is one run; nil probes every time.
type hostSELinux struct {
	run    command.Runner
	handle *handleProbe
}

// handleProbe is handleOpens' answer, found once.
type handleProbe struct {
	once sync.Once
	err  error
}

var _ selinuxOps = hostSELinux{}

// libselinux's constants: libselinux/src/policy.h:11,15,18,21,24-25 and
// libselinux/src/selinux_internal.h:88-89. stRdonly is ST_RDONLY from
// <bits/statvfs.h>:84, absent from Go's syscall package.
const (
	xattrNameSELinux = "security.selinux"
	initContextLen   = 255
	selinuxfsName    = "selinuxfs"
	selinuxMagic     = 0xf97cff8c
	selinuxMnt       = "/sys/fs/selinux"
	oldSelinuxMnt    = "/selinux"
	selinuxConfig    = "/etc/selinux/config"
	stRdonly         = 1
)

// enabled is Puppet's selinux_support? (util/selinux.rb:21-28), which is
// libselinux's is_selinux_enabled() == 1. That is NOT just "selinuxfs is
// mounted" (libselinux/src/enabled.c:11-21): it is selinux_mnt being set AND
// /etc/selinux/config existing (has_selinux_config, libselinux/src/init.c:152,
// access(F_OK)). selinux_mnt is set only for a READ-WRITE selinuxfs
// (init.c:31-53), which is why a container that sees the host's selinuxfs
// read-only reads as disabled.
//
// Both are computed once, when libselinux is loaded (init_lib, a constructor,
// init.c:145-152), so the answer is cached for the process as Puppet's is.
// That also keeps the statfs calls and the /proc/mounts read out of every
// File's Validate and Apply.
func (hostSELinux) enabled() bool { return hostSELinuxEnabled() }

var hostSELinuxEnabled = sync.OnceValue(func() bool {
	statfs := func(p string, buf *syscall.Statfs_t) error {
		for {
			// init.c:36-38 retries statfs on EINTR.
			if err := syscall.Statfs(p, buf); !errors.Is(err, syscall.EINTR) {
				return err
			}
		}
	}
	open := func() (io.ReadCloser, error) { return os.Open("/proc/mounts") }
	return selinuxMount(statfs, open) != "" && syscall.Access(selinuxConfig, syscall.F_OK) == nil
})

// selinuxMount is init_selinuxmnt (libselinux/src/init.c:82-132): the mount
// libselinux records as selinux_mnt, or "" for none.
//
// The default and old mount points are tried in turn, and the first that is
// an selinuxfs at all ends the search, recorded only when it is read-write
// (init.c:92-96). Otherwise the first selinuxfs listed in /proc/mounts is
// verified the same way (init.c:104-126).
//
// init.c:99 first asks /proc/filesystems whether selinuxfs is registered and
// stops if not. That is skipped: an unregistered filesystem cannot be listed
// in /proc/mounts, so the answer is the same, and an unreadable
// /proc/filesystems makes libselinux go on to /proc/mounts anyway (init.c:65).
func selinuxMount(statfs func(string, *syscall.Statfs_t) error, openMounts func() (io.ReadCloser, error)) string {
	for _, m := range []string{selinuxMnt, oldSelinuxMnt} {
		if found, rw := verifySelinuxMnt(statfs, m); found {
			if rw {
				return m
			}
			return ""
		}
	}
	f, err := openMounts()
	if err != nil {
		return ""
	}
	defer f.Close()
	m := firstSelinuxfsMount(f)
	if m == "" {
		return ""
	}
	if found, rw := verifySelinuxMnt(statfs, m); found && rw {
		return m
	}
	return ""
}

// verifySelinuxMnt is verify_selinuxmnt (libselinux/src/init.c:31-53): found
// when mnt is an selinuxfs (statfs f_type is SELINUX_MAGIC), rw when it is
// also not read-only. libselinux reads the read-only bit through statvfs,
// whose f_flag glibc takes from the same statfs call's f_flags, so one call
// answers both.
func verifySelinuxMnt(statfs func(string, *syscall.Statfs_t) error, mnt string) (found, rw bool) {
	var st syscall.Statfs_t
	if statfs(mnt, &st) != nil || uint32(st.Type) != selinuxMagic {
		return false, false
	}
	return true, st.Flags&stRdonly == 0
}

// firstSelinuxfsMount is the /proc/mounts scan of init.c:104-122: the mount
// point (second field, still octal-escaped, as libselinux passes it on) of
// the first line whose third field is exactly selinuxfs. A line without two
// spaces ends the scan with nothing found, and so does a read error, which
// getline reports as end of file.
func firstSelinuxfsMount(r io.Reader) string {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			first := strings.IndexByte(line, ' ')
			if first < 0 {
				return ""
			}
			rest := line[first+1:]
			second := strings.IndexByte(rest, ' ')
			if second < 0 {
				return ""
			}
			if strings.HasPrefix(rest[second+1:], selinuxfsName+" ") {
				return rest[:second]
			}
		}
		if err != nil {
			return ""
		}
	}
}

// labelFilesystems are the filesystem types Puppet trusts to hold labels
// (util/selinux.rb:224).
var labelFilesystems = []string{"ext2", "ext3", "ext4", "gfs", "gfs2", "xfs", "jfs", "btrfs", "tmpfs", "zfs"}

// labelSupported is selinux_label_support? (util/selinux.rb:220-226) over the
// live /proc/mounts. An unreadable /proc/mounts is unsupported, as Puppet's
// read_mounts returns nil (util/selinux.rb:278-279).
func (hostSELinux) labelSupported(p string) bool {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return labelSupportedIn(string(b), p)
}

// labelSupportedIn is selinux_label_support? over the text of /proc/mounts.
//
// read_mounts (util/selinux.rb:284-295) maps each line's second field to its
// third, skipping rootfs, so a later mount at the same point wins. It never
// unescapes, so the escaped form of a mount point (\040 for a space) is what
// is matched, and a path under a mount point with a space falls through to the
// parent mount (I-8). A line with a mount point but no type maps to nil, which
// is unsupported.
//
// find_fs (util/selinux.rb:300-320) cleans the path lexically and walks up
// from it, so the deepest mount point that is a whole-segment prefix decides.
// Paths reach here absolute (file's path is validated), so the walk ends at
// "/"; find_fs's final mounts['/'] is then only reached with no "/" mount.
func labelSupportedIn(mounts, p string) bool {
	isSpace := func(r rune) bool { return strings.ContainsRune(" \t\n\v\f\r", r) } // Ruby's split(' ')
	fs := map[string]string{}
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.FieldsFunc(line, isSpace)
		if len(f) < 2 {
			continue
		}
		typ := ""
		if len(f) > 2 {
			typ = f[2]
		}
		if typ == "rootfs" {
			continue
		}
		fs[f[1]] = typ
	}
	for p = path.Clean(p); ; p = path.Dir(p) {
		if typ, ok := fs[p]; ok {
			return slices.Contains(labelFilesystems, typ)
		}
		if p == "/" || p == "." {
			break
		}
	}
	return slices.Contains(labelFilesystems, fs["/"])
}

// current is the file's own raw context, as lgetfilecon reads it, never
// through a link. false is any failure: get_selinux_current_context maps
// every one to nil (util/selinux.rb:39-42) -- ENODATA, no label at all, but
// equally ENOENT, EACCES or ENOTSUP -- and set_selinux_context's own read
// fails as a whole (:123, splitOnInteger). retrievedContext parses it.
//
// Puppet's bindings call the translating lgetfilecon; this reads the raw
// context. They differ only under mcstrans.
func (hostSELinux) current(fsys fileSystem, p string) (string, bool) {
	s, err := getfileconRaw(fsys, p, xattrNameSELinux)
	return s, err == nil
}

// set writes context to the file itself, never through a link, as
// lsetfilecon does.
func (hostSELinux) set(fsys fileSystem, p, context string) error {
	return setfileconRaw(fsys, p, xattrNameSELinux, context)
}

// fileconFD opens p itself through fsys -- O_PATH|O_NOFOLLOW, so a final link
// is the link -- and returns the handle with its /proc/self/fd path. This is
// how libselinux reaches an O_PATH descriptor's label
// (libselinux/src/fgetfilecon.c:11-35, fsetfilecon.c:11-35) and how
// selinux_restorecon labels what it opened (selinux_restorecon.c:110-140,
// 1093, 1257): getxattr and setxattr on that path act on the opened object,
// a link included, and never resolve its name again.
//
// It is what keeps a generated child's label inside its tree. The name is
// resolved by fsys, which for a child is the os.Root on the recursed
// directory (rootFS): a parent directory swapped for a link out of the tree
// is refused there, where lgetxattr and lsetxattr on the name would follow it.
func fileconFD(fsys fileSystem, p string) (*os.File, string, error) {
	f, err := fsys.OpenFile(p, puppetpath.OPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	return f, "/proc/self/fd/" + strconv.Itoa(int(f.Fd())), nil
}

// getfileconRaw is fgetfilecon_raw (libselinux/src/fgetfilecon.c:38-73) on p
// opened by fileconFD, for the extended attribute name, which is
// security.selinux outside tests.
//
// The first read uses a 255-byte buffer; on ERANGE the size is asked for and
// the read repeated once. An empty value is ENOTSUP (fgetfilecon.c:64-68).
// The value is a C string to libselinux, so it ends at its first NUL -- the
// kernel stores the terminating NUL with the context.
func getfileconRaw(fsys fileSystem, p, name string) (string, error) {
	f, proc, err := fileconFD(fsys, p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return getxattrString(proc, p, name)
}

// getxattrString is the read itself, on proc; errors name p.
func getxattrString(proc, p, name string) (string, error) {
	pathp, err := syscall.BytePtrFromString(proc)
	if err != nil {
		return "", err
	}
	namep, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	get := func(buf []byte) (int, syscall.Errno) {
		var bufp unsafe.Pointer
		if len(buf) > 0 {
			bufp = unsafe.Pointer(&buf[0])
		}
		n, _, errno := syscall.Syscall6(syscall.SYS_GETXATTR, uintptr(unsafe.Pointer(pathp)),
			uintptr(unsafe.Pointer(namep)), uintptr(bufp), uintptr(len(buf)), 0, 0)
		return int(n), errno
	}
	buf := make([]byte, initContextLen)
	n, errno := get(buf)
	if errno == syscall.ERANGE {
		size, serr := get(nil)
		if serr != 0 {
			return "", &os.PathError{Op: "getxattr", Path: p, Err: serr}
		}
		buf = make([]byte, size)
		n, errno = get(buf)
	}
	if errno != 0 {
		return "", &os.PathError{Op: "getxattr", Path: p, Err: errno}
	}
	if n == 0 {
		return "", &os.PathError{Op: "getxattr", Path: p, Err: syscall.ENOTSUP}
	}
	b := buf[:n]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b), nil
}

// setfileconRaw is fsetfilecon_raw (libselinux/src/fsetfilecon.c:36-51) on p
// opened by fileconFD, for the extended attribute name: the value is written
// with its terminating NUL (strlen + 1), and ENOTSUP is success when the file
// already carries exactly that value. lsetfilecon_raw, which Puppet calls,
// does the same (lsetfilecon.c:10-26).
func setfileconRaw(fsys fileSystem, p, name, value string) error {
	f, proc, err := fileconFD(fsys, p)
	if err != nil {
		return err
	}
	defer f.Close()
	pathp, err := syscall.BytePtrFromString(proc)
	if err != nil {
		return err
	}
	namep, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	valp, err := syscall.BytePtrFromString(value)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_SETXATTR, uintptr(unsafe.Pointer(pathp)),
		uintptr(unsafe.Pointer(namep)), uintptr(unsafe.Pointer(valp)), uintptr(len(value)+1), 0, 0)
	if errno == syscall.ENOTSUP {
		if cur, err := getxattrString(proc, p, name); err == nil && cur == value {
			return nil
		}
	}
	if errno != 0 {
		return &os.PathError{Op: "setxattr", Path: p, Err: errno}
	}
	return nil
}

// defaultFor is the policy's default context for path, which Puppet gets from
// selabel_lookup on a handle opened with no options
// (util/selinux.rb:67-82, provider/file/posix.rb:22). The CLI opens its handle
// with a NULL path and NULL validate option, which are the defaults, and calls
// the same non-raw selabel_lookup (libselinux/utils/selabel_lookup.c:88-105).
//
// mode is the file's st_mode, or modeForEnsure's for a path not there yet.
// -t is parsed with atoi (selabel_lookup.c:80-81), so it is passed in decimal.
//
// Success prints "Default context: <ctx>" (selabel_lookup.c:125). A failed
// lookup exits non-zero with a message on stderr (selabel_lookup.c:109-123);
// that, or no such line, is no default -- Puppet's `retval == -1 ? nil`.
//
// The CLI also exits 255 when selabel_open fails, printing "ERROR:
// selabel_open - Could not obtain handle" (selabel_lookup.c:91-97). Puppet's
// handle is then nil and it raises "Cannot get default context with nil
// handle" (util/selinux.rb:73-74), so that is the error here: a host with
// missing or broken file contexts fails rather than leaving defaults unmanaged.
// Failing to run the command at all is an error.
func (h hostSELinux) defaultFor(ctx context.Context, p string, mode uint32) (seContext, bool, error) {
	code, out, err := h.run.Run(ctx, "selabel_lookup", "-b", "file", "-k", p, "-t", strconv.FormatUint(uint64(mode), 10))
	if err != nil {
		return seContext{}, false, err
	}
	if code != 0 {
		if strings.Contains(out, "ERROR: selabel_open - Could not obtain handle") {
			return seContext{}, false, errNilHandle
		}
		return seContext{}, false, nil
	}
	for _, line := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(line, "Default context: "); ok {
			return puppetContext(s)
		}
	}
	return seContext{}, false, nil
}

// errNilHandle is Puppet's error for a default lookup without a file_contexts
// handle (util/selinux.rb:73-74).
var errNilHandle = errors.New("Cannot get default context with nil handle")

// matchpathconArgBytes bounds one matchpathcon's argv, each argument counted
// with its NUL and its pointer. It is half of ARG_MAX, 128 KiB of arguments
// and environment for exec (<linux/limits.h>:8), which leaves the rest for
// the environment.
const matchpathconArgBytes = 64 << 10

// defaultsFor is defaultFor for many paths, through libselinux-utils'
// matchpathcon: one process per file type and argv chunk instead of one
// selabel_lookup per path.
//
// It gives selabel_lookup's answers. matchpathcon opens the same handle, a
// file backend with no path and no validation (utils/matchpathcon.c:82,134;
// label.c:110-119), and, without -N, calls the same non-raw selabel_lookup
// (matchpathcon.c:27-31). -m forces the lookup's mode to the bare S_IF* bits
// (:50-71, 151-152), and the backend compares only those bits
// (label_file.c:2099-2122). A miss, ENOENT, prints "<<none>>" (:32-34): a
// file_contexts "<<none>>" entry is itself a miss (label_file.c:1860-1864),
// so the string is never a context. Checked by running both CLIs over 1,214
// paths with 10 modes each in an EL10 container, with no difference.
//
// -n prints only the context, one line per path in argv order (:41-44), so a
// path with any byte in it maps back by position; "--" ends getopt's options
// (:87), so no path is read as one. matchpathcon cannot force mode 0 (-m takes
// a type letter; without it the path's own lstat mode is used, :149-152), so
// a query whose mode has no file type is not answered.
//
// The output is combined with stderr. Exit 0 with exactly one line per path
// is the answer. selabel_open failing exits 255 with "Error while opening
// file contexts database" (:134-140), selabel_lookup's open failure, so each
// path gets defaultFor's nil-handle error. Anything else -- a lookup error
// (:35-38, exit 1, its line missing), a warning on stderr, a command that
// cannot run -- leaves the chunk unanswered, for defaultFor.
func (h hostSELinux) defaultsFor(ctx context.Context, qs []defaultQuery) map[defaultQuery]defaultAnswer {
	byType := map[string][]defaultQuery{}
	for _, q := range qs {
		if t := matchpathconType(q.mode); t != "" {
			byType[t] = append(byType[t], q)
		}
	}
	answers := map[defaultQuery]defaultAnswer{}
	for _, t := range slices.Sorted(maps.Keys(byType)) {
		fixed := []string{"matchpathcon", "-n", "-m", t, "--"}
		base := 0
		for _, a := range fixed {
			base += len(a) + 1 + 8
		}
		group := byType[t]
		for len(group) > 0 {
			n, size := 0, base
			for n < len(group) && (n == 0 || size+len(group[n].path)+1+8 <= matchpathconArgBytes) {
				size += len(group[n].path) + 1 + 8
				n++
			}
			h.matchpathcon(ctx, fixed, group[:n], answers)
			group = group[n:]
		}
	}
	return answers
}

// matchpathcon runs one defaultsFor chunk and records what it answers.
func (h hostSELinux) matchpathcon(ctx context.Context, fixed []string, chunk []defaultQuery, answers map[defaultQuery]defaultAnswer) {
	args := slices.Clone(fixed[1:])
	for _, q := range chunk {
		args = append(args, q.path)
	}
	code, out, err := h.run.Run(ctx, fixed[0], args...)
	switch {
	case err != nil:
		return
	case code != 0:
		if strings.Contains(out, "Error while opening file contexts database") {
			for _, q := range chunk {
				answers[q] = defaultAnswer{err: errNilHandle}
			}
		}
		return
	}
	lines, found := strings.CutSuffix(out, "\n")
	split := strings.Split(lines, "\n")
	if !found || len(split) != len(chunk) {
		return
	}
	for i, q := range chunk {
		if split[i] == "<<none>>" {
			answers[q] = defaultAnswer{}
			continue
		}
		c, ok, err := puppetContext(split[i])
		answers[q] = defaultAnswer{ctx: c, ok: ok, err: err}
	}
}

// matchpathconType is matchpathcon's -m letter for mode's file type
// (utils/matchpathcon.c:50-71), "" for a mode with none.
func matchpathconType(mode uint32) string {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFBLK:
		return "b"
	case syscall.S_IFCHR:
		return "c"
	case syscall.S_IFDIR:
		return "d"
	case syscall.S_IFIFO:
		return "p"
	case syscall.S_IFLNK:
		return "l"
	case syscall.S_IFSOCK:
		return "s"
	case syscall.S_IFREG:
		return "f"
	}
	return ""
}

// prefetchedSELinux is ops with some defaults already looked up, in a batch,
// for a run: a query it holds is answered once from defaults, and anything
// else, a query with another mode included, goes to ops.
type prefetchedSELinux struct {
	selinuxOps
	defaults map[defaultQuery]defaultAnswer
}

func (s prefetchedSELinux) defaultFor(ctx context.Context, p string, mode uint32) (seContext, bool, error) {
	q := defaultQuery{p, mode}
	if a, ok := s.defaults[q]; ok {
		delete(s.defaults, q)
		return a.ctx, a.ok, a.err
	}
	return s.selinuxOps.defaultFor(ctx, p, mode)
}

// prefetchChildDefaults looks up, in one defaultsFor, the defaults the
// children of the recursed File at parent will ask planSELinux for, and adds
// them to into. Each query is the one the child's Apply would make, its mode
// read through the same os.Root, and an answer is used only for that exact
// query (prefetchedSELinux), so batching changes no answer: a child it skips,
// or one whose type changes before its Apply, is looked up there as before.
// Its lstat is when
// Puppet makes it, as the children are instantiated (type/file.rb:583-603,
// type.rb:2343). A failure here is left for the child's Apply to report.
//
// Whether labels are supported is asked once, for parent, not per child: it
// reads /proc/mounts each time (labelSupported), 0.2 ms a call. A child on
// another filesystem than parent is at worst a batched lookup its Apply never
// uses, or one its Apply makes itself.
func prefetchChildDefaults(ctx context.Context, se selinuxOps, parent string, children []catalog.Resource, into map[defaultQuery]defaultAnswer) {
	if len(children) == 0 || !se.enabled() || !se.labelSupported(parent) {
		return
	}
	root, err := openDirNoFollow(parent)
	if err != nil {
		return
	}
	defer root.Close()
	fsys := rootFS{root: root, dir: parent}
	var qs []defaultQuery
	for _, c := range children {
		path := FilePath(c)
		if e := rubyvalue.ParamString(c, "ensure", ""); e == "absent" || e == "false" || !defaultsWanted(c) {
			continue
		}
		if mode, err := lookupMode(fsys, path, c); err == nil {
			qs = append(qs, defaultQuery{path, mode})
		}
	}
	if len(qs) > 0 {
		maps.Copy(into, se.defaultsFor(ctx, qs))
	}
}

// defaultsWanted reports whether planSELinux looks r's default up: some sel*
// property is undeclared and selinux_ignore_defaults is not true.
func defaultsWanted(r catalog.Resource) bool {
	if ignore := r.Parameters["selinux_ignore_defaults"]; ignore == true || ignore == "true" {
		return false
	}
	for _, prop := range selProps {
		if _, ok := r.Parameters[prop].(string); !ok {
			return true
		}
	}
	return false
}

// handleOpens asks whether the file-contexts handle opens, which Puppet
// opens once (provider/file/posix.rb:22) and which no path or mode affects:
// a lookup of "/" with mode 0 answers it through defaultFor, whose
// errNilHandle is selabel_lookup's open failure.
func (h hostSELinux) handleOpens(ctx context.Context) error {
	probe := func() error {
		_, _, err := h.defaultFor(ctx, "/", 0)
		return err
	}
	if h.handle == nil {
		return probe()
	}
	h.handle.once.Do(func() { h.handle.err = probe() })
	return h.handle.err
}

// lookupAvailable reports whether selabel_lookup (libselinux-utils) is on PATH.
func (hostSELinux) lookupAvailable() bool {
	_, err := exec.LookPath("selabel_lookup")
	return err == nil
}

// modeForEnsure is get_create_mode (util/selinux.rb:231-242): the file-type
// bits Puppet passes to selabel_lookup for a path that does not exist yet.
// ensure is the munged value, so ensure => <target> arrives as "link"
// (type/file/ensure.rb:114-121); anything else, absent included, is 0.
func modeForEnsure(ensure string) uint32 {
	switch ensure {
	case "present", "file":
		return syscall.S_IFREG
	case "directory":
		return syscall.S_IFDIR
	case "link":
		return syscall.S_IFLNK
	}
	return 0
}

// selinuxPlan is a file resource's seluser, selrole, seltype and selrange as
// Puppet instantiates them, and the context they are compared against. nil
// means there is nothing to manage: SELinux is disabled, the path's
// filesystem carries no labels, or no property has a value.
type selinuxPlan struct {
	ops  selinuxOps
	fsys fileSystem        // resolves path for ops: a generated child's stays in its tree
	want map[string]string // by property; a property with no value is absent
	// redact is set when any sel* property is marked Sensitive: a context
	// names every field, so the warning that repeats one is [redacted] (D-78).
	redact bool

	// cur is the context retrieve_resource read before anything was synced;
	// curOK is false when there was none.
	cur   seContext
	curOK bool
}

// planSELinux reads what the resource's sel* properties want before Apply
// changes anything, because that is when Puppet reads it: their defaults are
// computed while the catalog's resources are instantiated
// (type/file/selcontext.rb:42-56, type.rb:2343), and their current values by
// retrieve_resource, before ensure syncs (selcontext.rb:30-40).
//
// Disabled SELinux makes every property nil or in sync
// (util/selinux.rb:68, selcontext.rb:58-61), as does a filesystem that
// carries no labels (:71, :62-64), so neither reads or looks anything up.
// Otherwise a declared value is wanted as given, and an undeclared one is the
// default's field unless selinux_ignore_defaults is true; a field the default
// lacks leaves the property without a value (type.rb:861-865).
//
// The default is looked up once rather than once per property. The mode
// passed is the path's lstat st_mode, 0 when lstat is refused, and, for a
// path that does not exist, get_create_mode of the declared ensure
// (util/selinux.rb:231-258) -- declared, because ensure is inferred from
// target or content only after the defaults are set (type/file.rb:553-567).
// Any other lstat failure raises in Puppet, and is an error here.
func planSELinux(ctx context.Context, se selinuxOps, fsys fileSystem, path string, r catalog.Resource) (*selinuxPlan, error) {
	// Ruby's insync? says why each declared property is ignored
	// (type/file/selcontext.rb:58-64), at the property's path.
	reason := ""
	switch {
	case !se.enabled():
		reason = "SELinux bindings not found. Ignoring parameter."
	case !se.labelSupported(path):
		reason = "SELinux not available for this filesystem. Ignoring parameter."
	}
	if reason != "" {
		for _, prop := range selProps {
			if _, declared := r.Parameters[prop].(string); declared {
				runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource+"/"+prop, "%s", reason)
			}
		}
		return nil, nil
	}
	p := &selinuxPlan{ops: se, fsys: fsys, want: map[string]string{},
		redact: slices.ContainsFunc(selProps, func(prop string) bool { return ral.IsMarked(File{}, r, prop) })}
	var undeclared []string
	for _, prop := range selProps {
		if v, ok := r.Parameters[prop].(string); ok {
			p.want[prop] = v
		} else {
			undeclared = append(undeclared, prop)
		}
	}
	if defaultsWanted(r) {
		mode, err := lookupMode(fsys, path, r)
		if err != nil {
			return nil, err
		}
		def, ok, err := se.defaultFor(ctx, path, mode)
		if err != nil {
			return nil, err
		}
		for _, prop := range undeclared {
			if v := def.field(prop); ok && v != "" {
				p.want[prop] = v
				runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource+"/"+prop, "Found %s default '%s' for %s", prop, v, path)
			}
		}
	}
	if len(p.want) == 0 {
		return nil, nil
	}
	var err error
	if p.cur, p.curOK, err = retrievedContext(se, fsys, path); err != nil {
		return nil, err
	}
	return p, nil
}

// retrievedContext is what the sel* properties' retrieve reads
// (selcontext.rb:33-34): the current context, parsed. An unreadable or
// "unlabeled" context is no context; one Puppet's pattern rejects is its
// "Invalid context to parse" error.
func retrievedContext(se selinuxOps, fsys fileSystem, path string) (seContext, bool, error) {
	raw, ok := se.current(fsys, path)
	if !ok {
		return seContext{}, false, nil
	}
	return puppetContext(raw)
}

// lookupMode is file_mode (util/selinux.rb:247-258) for planSELinux.
func lookupMode(fsys fileSystem, path string, r catalog.Resource) (uint32, error) {
	fi, err := fsys.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// ensure is munged before the defaults are set: a non-keyword is a
		// link target (type/file/ensure.rb:114-121).
		ensure, _ := r.Parameters["ensure"].(string)
		switch ensure {
		case "", "present", "file", "directory", "link", "absent", "false":
		default:
			ensure = "link"
		}
		return modeForEnsure(ensure), nil
	case errors.Is(err, fs.ErrPermission):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("lstat %s: %w", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("lstat %s: no stat_t", path)
	}
	return st.Mode, nil
}

// splitOnInteger is the NoMethodError set_selinux_context raises when it
// cannot read the context it is about to change one field of. On failure the
// SWIG binding returns the bare -1, with no context appended
// (libselinux/src/selinuxswig_ruby.i:30-35); util/selinux.rb:123 takes its
// [1], which is Integer#[]'s bit 1 of -1, so the `== -1` guard at :124 and its
// warning never fire, and :131 calls split on 1. Measured, Ruby 3.4.9:
// `(-1[1]).split(":")` raises exactly this message.
var splitOnInteger = errors.New("undefined method 'split' for an instance of Integer")

// applySELinux syncs seluser, selrole, seltype and selrange, in that order
// (type/file.rb:1125-1139), on the path itself: lgetfilecon and lsetfilecon
// never follow a link, whatever links says.
//
// fix false is the properties as the transaction syncs them on a path whose
// ensure was in sync: each is compared with the context retrieved before the
// run (plan.cur), and one that differs is a Change, "<prop> changed '<cur>'
// to '<want>'". A missing current value is Ruby's nil, which displays as
// nothing (property.rb:211).
//
// fix true is property_fix (type/file.rb:1112-1121), after ensure, a content
// write or a link: each property is compared with a fresh read, and its
// Changes only carry failures for the caller to collapse (propertyFixErr).
// Under noop nothing was written, so there is nothing to fix.
//
// A sync is set_selinux_context's (util/selinux.rb:118-156): re-read the raw
// context, replace the one field (withField, I-7) and write it back. A
// failed write is only a warning from Puppet.warning, and the property's
// event still succeeds (I-4); a raw "unlabeled" becomes "unlabeled::<type>",
// which the kernel refuses, so it warns too. A failed re-read fails the
// property (splitOnInteger, I-9).
func applySELinux(plan *selinuxPlan, path string, noop, fix bool) ([]ral.Change, []ral.Warning, error) {
	if plan == nil || (fix && noop) {
		return nil, nil, nil
	}
	var changes []ral.Change
	var warnings []ral.Warning
	for _, prop := range selProps {
		want, ok := plan.want[prop]
		if !ok {
			continue
		}
		cur, curOK := plan.cur, plan.curOK
		if fix {
			var err error
			if cur, curOK, err = retrievedContext(plan.ops, plan.fsys, path); err != nil {
				return changes, warnings, err
			}
		}
		var was any
		shown := ""
		if v := cur.field(prop); curOK && v != "" {
			was, shown = v, "'"+v+"'"
		}
		if was == want {
			continue
		}
		ch := ral.Change{Property: prop, Was: was, Is: want,
			Message: fmt.Sprintf("%s changed %s to '%s'", prop, shown, want)}
		if !noop {
			if raw, ok := plan.ops.current(plan.fsys, path); !ok {
				ch.Err = splitOnInteger
			} else {
				next := withField(raw, prop, want)
				if plan.ops.set(plan.fsys, path, next) != nil {
					warnings = append(warnings, ral.Warning{Source: "Puppet",
						Message: fmt.Sprintf("Failed to set SELinux context %s on %s", rubyvalue.RedactIf(plan.redact, "%s", next), path)})
				}
			}
		}
		changes = append(changes, ch)
	}
	return changes, warnings, nil
}
