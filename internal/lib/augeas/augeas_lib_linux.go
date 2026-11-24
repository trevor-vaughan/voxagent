package augeas

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// augeas 1.15.0's src/augeas.h prototypes (spec §4.1). The image ships no
// header, so they are declared here; `augeas` is opaque and a handle crosses
// into Go as a uintptr_t.
typedef struct augeas augeas;
typedef augeas *(*aug_init_fn)(const char *, const char *, unsigned int);
typedef void (*aug_close_fn)(augeas *);
typedef int (*aug_get_fn)(const augeas *, const char *, const char **);
typedef int (*aug_set_fn)(augeas *, const char *, const char *);
typedef int (*aug_rm_fn)(augeas *, const char *);
typedef int (*aug_insert_fn)(augeas *, const char *, const char *, int);
typedef int (*aug_mv_fn)(augeas *, const char *, const char *);
typedef int (*aug_match_fn)(const augeas *, const char *, char ***);
typedef int (*aug_void_fn)(augeas *);
typedef int (*aug_defvar_fn)(augeas *, const char *, const char *);
typedef int (*aug_defnode_fn)(augeas *, const char *, const char *, const char *, int *);
typedef int (*aug_text_store_fn)(augeas *, const char *, const char *, const char *);

// dlerror's text is thread-local and the goroutine may change OS thread
// between two cgo calls, so each shim copies it out in the same call.
static void vox_dlerror(char *err, size_t n) {
	const char *e = dlerror();
	snprintf(err, n, "%s", e ? e : "unknown dlerror");
}

// RTLD_NOW: every undefined symbol of libaugeas and its dependencies
// (libfa and libxml2 through its DT_RPATH /opt/puppetlabs/puppet/lib,
// libselinux and libc by the default search) is bound here, so a broken
// install fails the probe rather than a later call mid-run. RTLD_LOCAL: its
// symbols are not made available to anything loaded afterwards.
static void *vox_dlopen(const char *path, char *err, size_t n) {
	void *h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (h == NULL) vox_dlerror(err, n);
	return h;
}
static void *vox_dlsym(void *lib, const char *name, char *err, size_t n) {
	dlerror();
	void *s = dlsym(lib, name);
	if (s == NULL) vox_dlerror(err, n);
	return s;
}

// The trampolines: cgo cannot call a C function pointer itself.
static uintptr_t vox_aug_init(void *f, const char *root, const char *loadpath, unsigned int flags) {
	return (uintptr_t)((aug_init_fn)f)(root, loadpath, flags);
}
static void vox_aug_close(void *f, uintptr_t a) { ((aug_close_fn)f)((augeas *)a); }
static int vox_aug_get(void *f, uintptr_t a, const char *path, const char **value) {
	*value = NULL;
	return ((aug_get_fn)f)((const augeas *)a, path, value);
}
static int vox_aug_set(void *f, uintptr_t a, const char *path, const char *value) {
	return ((aug_set_fn)f)((augeas *)a, path, value);
}
static int vox_aug_rm(void *f, uintptr_t a, const char *path) { return ((aug_rm_fn)f)((augeas *)a, path); }
static int vox_aug_insert(void *f, uintptr_t a, const char *path, const char *label, int before) {
	return ((aug_insert_fn)f)((augeas *)a, path, label, before);
}
static int vox_aug_mv(void *f, uintptr_t a, const char *src, const char *dst) {
	return ((aug_mv_fn)f)((augeas *)a, src, dst);
}
static int vox_aug_match(void *f, uintptr_t a, const char *path, char ***matches) {
	*matches = NULL;
	return ((aug_match_fn)f)((const augeas *)a, path, matches);
}
static char *vox_match_at(char **m, int i) { return m[i]; }
static int vox_aug_void(void *f, uintptr_t a) { return ((aug_void_fn)f)((augeas *)a); }
static int vox_aug_defvar(void *f, uintptr_t a, const char *name, const char *expr) {
	return ((aug_defvar_fn)f)((augeas *)a, name, expr);
}
static int vox_aug_defnode(void *f, uintptr_t a, const char *name, const char *expr, const char *value) {
	return ((aug_defnode_fn)f)((augeas *)a, name, expr, value, NULL);
}
static int vox_aug_text_store(void *f, uintptr_t a, const char *lens, const char *node, const char *path) {
	return ((aug_text_store_fn)f)((augeas *)a, lens, node, path);
}

// strerror's text is copied out in the same call: for an errno it does not
// know, glibc formats into a buffer the next call on the thread may reuse.
static void vox_strerror(int e, char *buf, size_t n) { snprintf(buf, n, "%s", strerror(e)); }
*/
import "C"

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/voxagent/voxagent/internal/lib/plugintree"
)

// augeasLibPath is openvox's bundled libaugeas 1.15.0 (spec §0 row 1). A
// constant: never taken from the environment, the catalog or the libdir.
const augeasLibPath = "/opt/puppetlabs/puppet/lib/libaugeas.so.0"

// augeasVersion is the only /augeas/version the binding accepts.
const augeasVersion = "1.15.0"

// rubyAugeasFiles are openvox 8.33.0's ruby-augeas files under the Ruby lib
// root, whose return-value mapping Aug reimplements; rubyAugeasDigests
// are their measured sha256 values, in the same order (spec §0 row 1).
var rubyAugeasFiles = [3]string{
	"vendor_ruby/augeas.rb",
	"vendor_ruby/augeas/facade.rb",
	"vendor_ruby/3.2.0/x86_64-linux/_augeas.so",
}

var rubyAugeasDigests = [3]string{
	"004aeafc201010bd3ed0ee419874b23446719abffda44386433aa729c585790c",
	"5b042f632619df3e72d8c8020fd312c33cc9fff49e9973966fd57c34c39caed3",
	"c5bd6e8f3536cdfd61a54d649a70c1c9c3bc32459f8b2fcfd261c735b9c54108",
}

// Probe reports whether the native augeas types can run, and over which
// library; reason is §2's text when they cannot. Routing depends on it so its
// tests need no libaugeas.
type Probe interface{ Probe() (Lib, string, bool) }

// rubyAugeasRoot is openvox's Ruby lib root, which rubyAugeasFiles are under.
const rubyAugeasRoot = "/opt/puppetlabs/puppet/lib/ruby"

// HostProbe probes openvox's ruby-augeas and libaugeas on this host
// (probeAugeas). Its caller makes §2's environment checks first.
var HostProbe Probe = hostAugeasProbe{}

type hostAugeasProbe struct{}

func (hostAugeasProbe) Probe() (Lib, string, bool) { return probeAugeas(rubyAugeasRoot) }

// libAugeas implements Lib over the dlopen'd library; one per process.
// Each field is a dlsym'd function pointer, called through a C trampoline.
//
// A Go string holding a NUL byte has no C equivalent: passing it truncated
// would address a different node or store a different value. Every method
// refuses such an argument without calling libaugeas and returns the C
// function's own failure result (-1, nil and -1 for Get and Match, false
// for Init). ruby-augeas raises ArgumentError there instead; routing admits
// no control character (spec §2 (d)), so no routed resource reaches either.
type libAugeas struct {
	init, close, get, set, rm, insert, mv, match, load, save, defvar, defnode, textStore unsafe.Pointer
}

var (
	augeasOnce   sync.Once
	augeasLoaded *libAugeas
	augeasReason string
)

// probeAugeas checks the three ruby-augeas digests under rubyRoot, then
// dlopens augeasLibPath once per process (never dlclosed), resolves the
// thirteen symbols, opens aug_init(NULL, NULL, AUG_NO_MODL_AUTOLOAD), checks
// /augeas/version is 1.15.0 and closes it. The digests come first so the
// library is not loaded for a Ruby that was not measured. reason is §2's
// text when ok is false. Callers must first run §2's environment checks
// (AUGEAS_*, LD_PRELOAD, LD_LIBRARY_PATH send the types to the host), which
// this probe does not make, so that dlopen never runs under them.
func probeAugeas(rubyRoot string) (lib Lib, reason string, ok bool) {
	tree := plugintree.LibdirTree(rubyRoot)
	for i, rel := range rubyAugeasFiles {
		b, err := tree.Read(rel)
		if err != nil {
			return nil, fmt.Sprintf("ruby-augeas %s: %v", rel, err), false
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != rubyAugeasDigests[i] {
			return nil, fmt.Sprintf("ruby-augeas %s has sha256 %s, not the measured %s", rel, got, rubyAugeasDigests[i]), false
		}
	}
	augeasOnce.Do(func() { augeasLoaded, augeasReason = loadAugeas() })
	if augeasLoaded == nil {
		return nil, augeasReason, false
	}
	return augeasLoaded, "", true
}

// loadAugeas is probeAugeas' library half; it runs once per process.
func loadAugeas() (*libAugeas, string) {
	const errLen = 512
	errBuf := (*C.char)(C.malloc(errLen))
	defer C.free(unsafe.Pointer(errBuf))
	cpath := C.CString(augeasLibPath)
	defer C.free(unsafe.Pointer(cpath))
	handle := C.vox_dlopen(cpath, errBuf, errLen)
	if handle == nil {
		return nil, fmt.Sprintf("libaugeas: dlopen: %s", C.GoString(errBuf))
	}
	l := &libAugeas{}
	for _, s := range []struct {
		name string
		to   *unsafe.Pointer
	}{
		{"aug_init", &l.init}, {"aug_close", &l.close}, {"aug_get", &l.get},
		{"aug_set", &l.set}, {"aug_rm", &l.rm}, {"aug_insert", &l.insert}, {"aug_mv", &l.mv},
		{"aug_match", &l.match}, {"aug_load", &l.load}, {"aug_save", &l.save},
		{"aug_defvar", &l.defvar}, {"aug_defnode", &l.defnode}, {"aug_text_store", &l.textStore},
	} {
		cname := C.CString(s.name)
		sym := C.vox_dlsym(handle, cname, errBuf, errLen)
		C.free(unsafe.Pointer(cname))
		if sym == nil {
			return nil, fmt.Sprintf("libaugeas: dlsym %s: %s", s.name, C.GoString(errBuf))
		}
		*s.to = sym
	}
	h, ok := l.Init(nil, nil, NoModlAutoload)
	if !ok {
		return nil, "libaugeas: aug_init(NULL, NULL, AUG_NO_MODL_AUTOLOAD) returned NULL"
	}
	v, rc := l.Get(h, "/augeas/version")
	l.Close(h)
	if rc != 1 || v == nil {
		return nil, fmt.Sprintf("libaugeas: aug_get /augeas/version returned %d", rc)
	}
	if *v != augeasVersion {
		return nil, fmt.Sprintf("libaugeas: version %q, not the measured %s", *v, augeasVersion)
	}
	return l, ""
}

// cStrings makes C strings of args for one call; a nil arg stays NULL. ok is
// false, and nothing is allocated, when an arg holds a NUL byte. free must be
// called once the call has returned.
func cStrings(args ...*string) (cs []*C.char, free func(), ok bool) {
	for _, a := range args {
		if a != nil && strings.IndexByte(*a, 0) >= 0 {
			return nil, nil, false
		}
	}
	cs = make([]*C.char, len(args))
	for i, a := range args {
		if a != nil {
			cs[i] = C.CString(*a)
		}
	}
	return cs, func() {
		for _, c := range cs {
			C.free(unsafe.Pointer(c))
		}
	}, true
}

// Init refuses flags that do not fit aug_init's unsigned int rather than
// truncate them into different flags.
func (l *libAugeas) Init(root, loadpath *string, flags uint) (Handle, bool) {
	if flags > math.MaxUint32 {
		return 0, false
	}
	cs, free, ok := cStrings(root, loadpath)
	if !ok {
		return 0, false
	}
	defer free()
	h := Handle(C.vox_aug_init(l.init, cs[0], cs[1], C.uint(flags)))
	return h, h != 0
}

func (l *libAugeas) Close(h Handle) {
	C.vox_aug_close(l.close, C.uintptr_t(h))
}

// Get copies the value before returning: libaugeas lends it only until the
// node next changes.
func (l *libAugeas) Get(h Handle, path string) (*string, int) {
	cs, free, ok := cStrings(&path)
	if !ok {
		return nil, -1
	}
	defer free()
	var value *C.char
	rc := int(C.vox_aug_get(l.get, C.uintptr_t(h), cs[0], &value))
	if value == nil {
		return nil, rc
	}
	v := C.GoString(value)
	return &v, rc
}

func (l *libAugeas) Set(h Handle, path string, value *string) int {
	cs, free, ok := cStrings(&path, value)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_set(l.set, C.uintptr_t(h), cs[0], cs[1]))
}

func (l *libAugeas) Rm(h Handle, path string) int {
	cs, free, ok := cStrings(&path)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_rm(l.rm, C.uintptr_t(h), cs[0]))
}

func (l *libAugeas) Insert(h Handle, path, label string, before bool) int {
	cs, free, ok := cStrings(&path, &label)
	if !ok {
		return -1
	}
	defer free()
	var b C.int
	if before {
		b = 1
	}
	return int(C.vox_aug_insert(l.insert, C.uintptr_t(h), cs[0], cs[1], b))
}

// Mv is aug_mv (augeas.h 1.15.0: int aug_mv(augeas *aug, const char *src,
// const char *dst)): 0 on success, -1 on failure, e.g. src matching no node.
func (l *libAugeas) Mv(h Handle, src, dst string) int {
	cs, free, ok := cStrings(&src, &dst)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_mv(l.mv, C.uintptr_t(h), cs[0], cs[1]))
}

// Match copies and frees every result string and then the array, which
// libaugeas allocates even for 0 matches (augeas.c:1431). On -1 it touches
// neither: libaugeas has already freed the array without clearing the
// pointer (augeas.c:1450-1458).
func (l *libAugeas) Match(h Handle, path string) ([]string, int) {
	cs, free, ok := cStrings(&path)
	if !ok {
		return nil, -1
	}
	defer free()
	var matches **C.char
	rc := int(C.vox_aug_match(l.match, C.uintptr_t(h), cs[0], &matches))
	if rc < 0 {
		return nil, rc
	}
	paths := make([]string, rc)
	for i := range paths {
		m := C.vox_match_at(matches, C.int(i))
		paths[i] = C.GoString(m)
		C.free(unsafe.Pointer(m))
	}
	C.free(unsafe.Pointer(matches))
	return paths, rc
}

func (l *libAugeas) Load(h Handle) int {
	return int(C.vox_aug_void(l.load, C.uintptr_t(h)))
}

func (l *libAugeas) Save(h Handle) int {
	return int(C.vox_aug_void(l.save, C.uintptr_t(h)))
}

func (l *libAugeas) Defvar(h Handle, name, expr string) int {
	cs, free, ok := cStrings(&name, &expr)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_defvar(l.defvar, C.uintptr_t(h), cs[0], cs[1]))
}

func (l *libAugeas) Defnode(h Handle, name, expr string, value *string) int {
	cs, free, ok := cStrings(&name, &expr, value)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_defnode(l.defnode, C.uintptr_t(h), cs[0], cs[1], cs[2]))
}

func (l *libAugeas) TextStore(h Handle, lens, node, path string) int {
	cs, free, ok := cStrings(&lens, &node, &path)
	if !ok {
		return -1
	}
	defer free()
	return int(C.vox_aug_text_store(l.textStore, C.uintptr_t(h), cs[0], cs[1], cs[2]))
}

// LibcStrerror is libc's strerror(e), the text Ruby's Errno messages carry
// ("No such file or directory"); Go's own errno strings differ in case.
func LibcStrerror(e syscall.Errno) string {
	var buf [256]C.char
	C.vox_strerror(C.int(e), &buf[0], C.size_t(len(buf)))
	return C.GoString(&buf[0])
}
