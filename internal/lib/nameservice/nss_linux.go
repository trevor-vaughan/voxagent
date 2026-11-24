package nameservice

/*
#define _GNU_SOURCE
#include <errno.h>
#include <grp.h>
#include <pwd.h>
#include <shadow.h>
#include <stdlib.h>
#include <unistd.h>

// Each shim makes one reentrant call and reports whether an entry came back, so
// Go owns the buffer sizing and the conversion. They are the only C in voxagent.
static int nss_getpwnam(const char *n, struct passwd *e, char *b, size_t l, int *found) {
	struct passwd *r = NULL;
	int rc = getpwnam_r(n, e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getpwuid(uid_t u, struct passwd *e, char *b, size_t l, int *found) {
	struct passwd *r = NULL;
	int rc = getpwuid_r(u, e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getpwent(struct passwd *e, char *b, size_t l, int *found) {
	struct passwd *r = NULL;
	int rc = getpwent_r(e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getgrnam(const char *n, struct group *e, char *b, size_t l, int *found) {
	struct group *r = NULL;
	int rc = getgrnam_r(n, e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getgrgid(gid_t g, struct group *e, char *b, size_t l, int *found) {
	struct group *r = NULL;
	int rc = getgrgid_r(g, e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getspnam(const char *n, struct spwd *e, char *b, size_t l, int *found) {
	struct spwd *r = NULL;
	int rc = getspnam_r(n, e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static int nss_getgrent(struct group *e, char *b, size_t l, int *found) {
	struct group *r = NULL;
	int rc = getgrent_r(e, b, l, &r);
	*found = rc == 0 && r != NULL;
	return rc;
}
static char *nss_member(char **mem, int i) { return mem[i]; }
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// cNSS is the NameService backed by libc, so lookups see every NSS source the
// host configures (files, sssd, ...) exactly as Ruby's Etc does.
type cNSS struct{}

// notFoundPW is getpwnam_r(3)'s list of errnos that mean "no such entry".
var notFoundPW = []syscall.Errno{syscall.ENOENT, syscall.ESRCH, syscall.EBADF, syscall.EPERM}

// notFoundSP is what getspnam_r returns for a missing entry (ENOENT) or, for a
// caller that may not read /etc/shadow, EACCES (Ruby's nil, spec §4).
var notFoundSP = []syscall.Errno{syscall.ENOENT, syscall.ESRCH, syscall.EACCES}

// nssLookup runs call against a C buffer sized by growNSSBuffer, starting at
// sysconf(sizeConf) (4 KiB if unknown). call must copy what it needs out of the
// buffer before returning, because the buffer is freed afterwards.
func nssLookup(what string, sizeConf C.int, notFound []syscall.Errno, call func(buf *C.char, size C.size_t) (rc C.int, found bool)) (bool, error) {
	return growNSSBuffer(what, int(C.sysconf(sizeConf)), notFound, func(size int) (syscall.Errno, bool) {
		buf := (*C.char)(C.malloc(C.size_t(size)))
		defer C.free(unsafe.Pointer(buf))
		rc, found := call(buf, C.size_t(size))
		return syscall.Errno(rc), found
	})
}

func goPasswd(p *C.struct_passwd) PasswdEntry {
	return PasswdEntry{
		Name: C.GoString(p.pw_name), UID: int64(p.pw_uid), GID: int64(p.pw_gid),
		Gecos: C.GoString(p.pw_gecos), Dir: C.GoString(p.pw_dir), Shell: C.GoString(p.pw_shell),
	}
}

func goGroup(g *C.struct_group) GroupEntry {
	e := GroupEntry{Name: C.GoString(g.gr_name), GID: int64(g.gr_gid)}
	for i := C.int(0); ; i++ {
		m := C.nss_member(g.gr_mem, i)
		if m == nil {
			break
		}
		e.Members = append(e.Members, C.GoString(m))
	}
	return e
}

func (cNSS) User(name string) (PasswdEntry, bool, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var out PasswdEntry
	var pw C.struct_passwd
	found, err := nssLookup("getpwnam_r "+name, C._SC_GETPW_R_SIZE_MAX, notFoundPW, func(buf *C.char, size C.size_t) (C.int, bool) {
		var ok C.int
		rc := C.nss_getpwnam(cname, &pw, buf, size, &ok)
		if rc == 0 && ok != 0 {
			out = goPasswd(&pw)
		}
		return rc, ok != 0
	})
	return out, found, err
}

// UserByID treats an id outside uid_t as absent rather than truncating it into
// a different user.
func (cNSS) UserByID(uid int64) (PasswdEntry, bool, error) {
	if uid < 0 || uid > 1<<32-1 {
		return PasswdEntry{}, false, nil
	}
	var out PasswdEntry
	var pw C.struct_passwd
	found, err := nssLookup(fmt.Sprintf("getpwuid_r %d", uid), C._SC_GETPW_R_SIZE_MAX, notFoundPW, func(buf *C.char, size C.size_t) (C.int, bool) {
		var ok C.int
		rc := C.nss_getpwuid(C.uid_t(uid), &pw, buf, size, &ok)
		if rc == 0 && ok != 0 {
			out = goPasswd(&pw)
		}
		return rc, ok != 0
	})
	return out, found, err
}

func (cNSS) Shadow(name string) (ShadowEntry, bool, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var out ShadowEntry
	var sp C.struct_spwd
	found, err := nssLookup("getspnam_r "+name, C._SC_GETPW_R_SIZE_MAX, notFoundSP, func(buf *C.char, size C.size_t) (C.int, bool) {
		var ok C.int
		rc := C.nss_getspnam(cname, &sp, buf, size, &ok)
		if rc == 0 && ok != 0 {
			out = ShadowEntry{
				Hash: C.GoString(sp.sp_pwdp), Min: int64(sp.sp_min), Max: int64(sp.sp_max),
				Warn: int64(sp.sp_warn), Expire: int64(sp.sp_expire),
			}
		}
		return rc, ok != 0
	})
	return out, found, err
}

func (cNSS) Group(name string) (GroupEntry, bool, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var out GroupEntry
	var gr C.struct_group
	found, err := nssLookup("getgrnam_r "+name, C._SC_GETGR_R_SIZE_MAX, notFoundPW, func(buf *C.char, size C.size_t) (C.int, bool) {
		var ok C.int
		rc := C.nss_getgrnam(cname, &gr, buf, size, &ok)
		if rc == 0 && ok != 0 {
			out = goGroup(&gr)
		}
		return rc, ok != 0
	})
	return out, found, err
}

// groupByID treats an id outside gid_t as absent rather than truncating it into
// a different group.
func (cNSS) GroupByID(gid int64) (GroupEntry, bool, error) {
	if gid < 0 || gid > 1<<32-1 {
		return GroupEntry{}, false, nil
	}
	var out GroupEntry
	var gr C.struct_group
	found, err := nssLookup(fmt.Sprintf("getgrgid_r %d", gid), C._SC_GETGR_R_SIZE_MAX, notFoundPW, func(buf *C.char, size C.size_t) (C.int, bool) {
		var ok C.int
		rc := C.nss_getgrgid(C.gid_t(gid), &gr, buf, size, &ok)
		if rc == 0 && ok != 0 {
			out = goGroup(&gr)
		}
		return rc, ok != 0
	})
	return out, found, err
}

func (cNSS) GroupList(user string, gid int64) ([]int64, error) {
	if gid < 0 || gid > 1<<32-1 {
		return nil, fmt.Errorf("getgrouplist %s: gid %d out of range", user, gid)
	}
	cuser := C.CString(user)
	defer C.free(unsafe.Pointer(cuser))
	return growGroupList(user, func(n int) ([]int64, int) {
		groups := make([]C.gid_t, n)
		count := C.int(n)
		if C.getgrouplist(cuser, C.gid_t(gid), &groups[0], &count) != -1 {
			out := make([]int64, count)
			for i := range out {
				out[i] = int64(groups[i])
			}
			return out, int(count)
		}
		return nil, int(count)
	})
}

func (cNSS) AllGroups() ([]GroupEntry, error) {
	nssEnumMu.Lock()
	defer nssEnumMu.Unlock()
	C.setgrent()
	defer C.endgrent()
	var all []GroupEntry
	for {
		var gr C.struct_group
		var entry GroupEntry
		// getgrent_r signals the end with ENOENT, which nssLookup reports as
		// "no entry"; ERANGE retries the same position with a bigger buffer.
		found, err := nssLookup("getgrent_r", C._SC_GETGR_R_SIZE_MAX, []syscall.Errno{syscall.ENOENT}, func(buf *C.char, size C.size_t) (C.int, bool) {
			var ok C.int
			rc := C.nss_getgrent(&gr, buf, size, &ok)
			if rc == 0 && ok != 0 {
				entry = goGroup(&gr)
			}
			return rc, ok != 0
		})
		if err != nil {
			return nil, err
		}
		if !found {
			return all, nil
		}
		all = append(all, entry)
	}
}

func (cNSS) AllUsers() ([]PasswdEntry, error) {
	nssEnumMu.Lock()
	defer nssEnumMu.Unlock()
	C.setpwent()
	defer C.endpwent()
	var all []PasswdEntry
	for {
		var pw C.struct_passwd
		var entry PasswdEntry
		// getpwent_r signals the end with ENOENT, which nssLookup reports as
		// "no entry"; ERANGE retries the same position with a bigger buffer.
		found, err := nssLookup("getpwent_r", C._SC_GETPW_R_SIZE_MAX, []syscall.Errno{syscall.ENOENT}, func(buf *C.char, size C.size_t) (C.int, bool) {
			var ok C.int
			rc := C.nss_getpwent(&pw, buf, size, &ok)
			if rc == 0 && ok != 0 {
				entry = goPasswd(&pw)
			}
			return rc, ok != 0
		})
		if err != nil {
			return nil, err
		}
		if !found {
			return all, nil
		}
		all = append(all, entry)
	}
}
