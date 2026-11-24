package nameservice

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/runlog"
)

// PasswdEntry is the getpwnam_r fields the user provider reads.
type PasswdEntry struct {
	Name       string
	UID, GID   int64
	Gecos, Dir string
	Shell      string
}

// ShadowEntry is getspnam_r's record. libc's -1 for an unset field is kept as -1.
type ShadowEntry struct {
	Hash                   string
	Min, Max, Warn, Expire int64
}

// GroupEntry is a group(5) record; Members is gr_mem in NSS order.
type GroupEntry struct {
	Name    string
	GID     int64
	Members []string
}

// NameService is the seam over libc's reentrant NSS calls. Unit tests supply a
// fake; Host is the real one. "false" means the entry does not exist; an
// error means the lookup itself failed and must not be read as absence.
type NameService interface {
	User(name string) (PasswdEntry, bool, error)       // getpwnam_r
	Shadow(name string) (ShadowEntry, bool, error)     // getspnam_r; EACCES/ENOENT = false, nil
	Group(name string) (GroupEntry, bool, error)       // getgrnam_r
	GroupByID(gid int64) (GroupEntry, bool, error)     // getgrgid_r
	GroupList(user string, gid int64) ([]int64, error) // getgrouplist, grown until it fits
	AllGroups() ([]GroupEntry, error)                  // setgrent/getgrent_r/endgrent under nssEnumMu
	UserByID(uid int64) (PasswdEntry, bool, error)     // getpwuid_r
	AllUsers() ([]PasswdEntry, error)                  // setpwent/getpwent_r/endpwent under nssEnumMu
}

// nssEnumMu serialises setgrent..endgrent and setpwent..endpwent: libc keeps one
// enumeration cursor per process for each database.
var nssEnumMu sync.Mutex

var Host NameService = cNSS{}

// maxPosixID bounds what getgrgid_r and getpwuid_r can be asked for. Ruby
// refuses ids above Puppet[:maximum_uid] (4294967290) with an error log
// (util/posix.rb:79-83); anything outside that range, or negative, is
// unresolved here rather than truncated into a different gid_t or uid_t.
const maxPosixID = 4294967290

// posixEntry is the part of a group or passwd record get_posix_value reads.
type posixEntry struct {
	Name string
	ID   int64
}

// posixDB is what differs between Puppet::Util.gid and Puppet::Util.uid: the
// lookups, and the words the debug lines print.
type posixDB struct {
	resource, idField string // "group"/"gid" or "passwd"/"uid", as the debug lines print them
	byName            func(name string) (posixEntry, bool, error)
	byID              func(id int64) (posixEntry, bool, error)
	all               func() ([]posixEntry, error)
}

// PosixGID is Puppet::Util.gid, i.e. get_posix_value(:group, :gid, v)
// (util/posix.rb:172-205). v is an int64 or a string. ok=false is Ruby's nil: no
// such group. marked says v is a Sensitive value, which the debug line that
// repeats it prints as [redacted] (D-78).
func PosixGID(ctx context.Context, ns NameService, v any, marked bool) (gid int64, ok bool, err error) {
	return posixValue(ctx, posixDB{
		resource: "group", idField: "gid",
		byName: func(name string) (posixEntry, bool, error) {
			g, found, err := ns.Group(name)
			return posixEntry{g.Name, g.GID}, found, err
		},
		byID: func(id int64) (posixEntry, bool, error) {
			g, found, err := ns.GroupByID(id)
			return posixEntry{g.Name, g.GID}, found, err
		},
		all: func() ([]posixEntry, error) {
			groups, err := ns.AllGroups()
			out := make([]posixEntry, len(groups))
			for i, g := range groups {
				out[i] = posixEntry{g.Name, g.GID}
			}
			return out, err
		},
	}, v, marked)
}

// PosixUID is Puppet::Util.uid, i.e. get_posix_value(:passwd, :uid, v)
// (util/posix.rb:172-205); see PosixGID for v, ok and marked.
func PosixUID(ctx context.Context, ns NameService, v any, marked bool) (uid int64, ok bool, err error) {
	return posixValue(ctx, posixDB{
		resource: "passwd", idField: "uid",
		byName: func(name string) (posixEntry, bool, error) {
			u, found, err := ns.User(name)
			return posixEntry{u.Name, u.UID}, found, err
		},
		byID: func(id int64) (posixEntry, bool, error) {
			u, found, err := ns.UserByID(id)
			return posixEntry{u.Name, u.UID}, found, err
		},
		all: func() ([]posixEntry, error) {
			users, err := ns.AllUsers()
			out := make([]posixEntry, len(users))
			for i, u := range users {
				out[i] = posixEntry{u.Name, u.UID}
			}
			return out, err
		},
	}, v, marked)
}

// posixValue is get_posix_value over one database.
func posixValue(ctx context.Context, db posixDB, v any, marked bool) (id int64, ok bool, err error) {
	// posix.rb:173-177: Integer(field) rescuing ArgumentError, so a string that
	// is not an integer literal stays a name.
	var field any = v
	if s, isString := v.(string); isString {
		if n, convErr := rubyvalue.Integer(s); convErr == nil {
			field = n
		}
	}

	var check any // check_value: a name or an id, nil when the lookup found none
	if n, isInt := field.(int64); isInt {
		if n < 0 || n > maxPosixID {
			return 0, false, nil
		}
		byID, found, err := db.byID(n)
		if err != nil || !found {
			return 0, false, err
		}
		byName, found, err := db.byName(byID.Name)
		if err != nil {
			return 0, false, err
		}
		if !found {
			// get_posix_field returns nil, so id == check_value_id == nil: Ruby
			// logs "Multiple entries" with an empty id and returns nil.
			runlog.Debugf(ctx, "Puppet", "Multiple entries found for resource: '%s' with %s: ", db.resource, db.idField)
			return 0, false, nil
		}
		id, check = byName.ID, byName.ID
	} else {
		byName, found, err := db.byName(field.(string))
		if err != nil || !found {
			return 0, false, err
		}
		id = byName.ID
		byID, found, err := db.byID(id)
		if err != nil {
			return 0, false, err
		}
		if found {
			check = byID.Name
		}
	}

	if check == field {
		return id, true, nil
	}

	// posix.rb:192-201. check_value_id is only looked up when check_value is set.
	var checkID int64
	haveCheckID := false
	switch c := check.(type) {
	case int64:
		e, found, err := db.byID(c)
		if err != nil {
			return 0, false, err
		}
		checkID, haveCheckID = e.ID, found
	case string:
		e, found, err := db.byName(c)
		if err != nil {
			return 0, false, err
		}
		checkID, haveCheckID = e.ID, found
	}
	if haveCheckID && checkID == id {
		runlog.Debugf(ctx, "Puppet", "Multiple entries found for resource: '%s' with %s: %d", db.resource, db.idField, id)
		return id, true, nil
	}
	shown := ""
	if check != nil {
		shown = fmt.Sprint(check)
	}
	runlog.Debugf(ctx, "Puppet", "The value retrieved: %s is different than the required state: %s, searching in all entries",
		rubyvalue.RedactIf(marked, "'%s'", shown), rubyvalue.RedactIf(marked, "'%v'", field))

	// search_posix_field (posix.rb:100-126): first entry whose id equals an
	// integer input, or whose name equals the input.
	all, err := db.all()
	if err != nil {
		return 0, false, err
	}
	for _, e := range all {
		if n, isInt := field.(int64); isInt && e.ID == n || e.Name == field {
			return e.ID, true, nil
		}
	}
	return 0, false, nil
}

// GroupsOf is Puppet::Util::POSIX.groups_of (util/posix.rb:15-34): the groups
// getgrouplist reports for user, in its order, keeping those whose gr_mem lists
// the user (so the primary group is omitted unless listed), duplicates dropped.
// A user with no passwd entry (Etc.getpwnam raises, posix.rb:41) or a gid
// without a group (Etc.getgrgid raises, posix.rb:55) takes Ruby's fallback: a
// scan of every group for the user, in enumeration order.
func GroupsOf(ns NameService, user string) ([]string, error) {
	groups, complete, err := groupsFromList(ns, user)
	if err != nil {
		return nil, err
	}
	if !complete {
		all, err := ns.AllGroups()
		if err != nil {
			return nil, err
		}
		groups = nil
		for _, g := range all {
			if slices.Contains(g.Members, user) {
				groups = append(groups, g.Name)
			}
		}
	}
	var unique []string
	for _, name := range groups {
		if !slices.Contains(unique, name) {
			unique = append(unique, name)
		}
	}
	return unique, nil
}

// groupsFromList is get_groups_list (posix.rb:38-68). complete=false means Ruby
// would raise and fall back to the full scan.
func groupsFromList(ns NameService, user string) (groups []string, complete bool, err error) {
	pw, found, err := ns.User(user)
	if err != nil || !found {
		return nil, false, err
	}
	gids, err := ns.GroupList(user, pw.GID)
	if err != nil {
		return nil, false, err
	}
	for _, gid := range gids {
		g, found, err := ns.GroupByID(gid)
		if err != nil || !found {
			return nil, false, err
		}
		if slices.Contains(g.Members, user) {
			groups = append(groups, g.Name)
		}
	}
	return groups, true, nil
}

// maxNSSBuffer is the largest libc buffer a lookup may grow to (spec §4).
const maxNSSBuffer = 1 << 20

// growNSSBuffer drives a reentrant libc lookup: attempt(size) runs it with a
// size-byte buffer and returns the errno and whether an entry came back. size
// starts at initial (4 KiB if initial is not positive) and doubles on ERANGE
// up to maxNSSBuffer; ERANGE at that size is an error. Errnos in notFound mean
// "no such entry"; any other failure is an error naming what, never a silent
// absence. The same call is retried each time, so an enumeration such as
// getgrent_r stays at its position.
func growNSSBuffer(what string, initial int, notFound []syscall.Errno, attempt func(size int) (syscall.Errno, bool)) (bool, error) {
	size := initial
	if size <= 0 {
		size = 4096
	}
	for {
		errno, found := attempt(size)
		switch {
		case errno == 0:
			return found, nil
		case errno == syscall.ERANGE:
			if size >= maxNSSBuffer {
				return false, fmt.Errorf("%s: entry larger than 1 MiB", what)
			}
			size = min(size*2, maxNSSBuffer)
		case slices.Contains(notFound, errno):
			return false, nil
		default:
			return false, fmt.Errorf("%s: %w", what, errno)
		}
	}
}

// maxGroupList bounds the getgrouplist retry loop.
const maxGroupList = 1 << 20

// growGroupList drives getgrouplist (util/posix.rb:42-65): attempt(n) offers
// room for n groups and returns the groups and libc's count; a nil result
// means libc said -1 (too small). It starts at Ruby's 256 and, as Ruby does,
// takes libc's count, doubling when that did not grow.
func growGroupList(user string, attempt func(n int) (groups []int64, count int)) ([]int64, error) {
	n := 256
	for {
		groups, count := attempt(n)
		if groups != nil {
			return groups, nil
		}
		next := count
		if next <= n {
			next = n * 2
		}
		if next > maxGroupList {
			return nil, fmt.Errorf("getgrouplist %s: more than %d groups", user, maxGroupList)
		}
		n = next
	}
}

// FeatureMissing is type.rb:790's debug line for a dropped attribute.
const FeatureMissing = "Provider %s does not support features %s; not managing attribute %s"

// ShadowPath is the search order of Ruby's default PATH, where Puppet's
// command() finds the shadow-utils commands; tests point it elsewhere.
// shadowDirs is its colon-separated source, a string so that a test building
// the voxagent binary can link a directory of fake commands in its place
// (-ldflags -X, cmd/voxagent's TestNoSensitiveMarkerLeaks).
var (
	shadowDirs = "/usr/sbin:/usr/bin:/sbin:/bin"
	ShadowPath = strings.Split(shadowDirs, ":")
)

// ShadowCommand is Puppet::Util.which over ShadowPath: the absolute path of
// the first executable regular file called name.
func ShadowCommand(name string) (string, error) {
	for _, dir := range ShadowPath {
		path := filepath.Join(dir, name)
		if command.ExecutableFile(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, strings.Join(ShadowPath, ":"))
}
