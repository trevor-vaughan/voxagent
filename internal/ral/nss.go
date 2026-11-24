package ral

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"syscall"

	"github.com/voxagent/voxagent/internal/runlog"
)

// passwdEntry is the getpwnam_r fields the user provider reads.
type passwdEntry struct {
	Name       string
	UID, GID   int64
	Gecos, Dir string
	Shell      string
}

// shadowEntry is getspnam_r's record. libc's -1 for an unset field is kept as -1.
type shadowEntry struct {
	Hash                   string
	Min, Max, Warn, Expire int64
}

// groupEntry is a group(5) record; Members is gr_mem in NSS order.
type groupEntry struct {
	Name    string
	GID     int64
	Members []string
}

// nameService is the seam over libc's reentrant NSS calls. Unit tests supply a
// fake; hostNSS is the real one. "false" means the entry does not exist; an
// error means the lookup itself failed and must not be read as absence.
type nameService interface {
	user(name string) (passwdEntry, bool, error)       // getpwnam_r
	shadow(name string) (shadowEntry, bool, error)     // getspnam_r; EACCES/ENOENT = false, nil
	group(name string) (groupEntry, bool, error)       // getgrnam_r
	groupByID(gid int64) (groupEntry, bool, error)     // getgrgid_r
	groupList(user string, gid int64) ([]int64, error) // getgrouplist, grown until it fits
	allGroups() ([]groupEntry, error)                  // setgrent/getgrent_r/endgrent under nssEnumMu
}

// nssEnumMu serialises setgrent..endgrent: libc keeps one enumeration cursor
// per process.
var nssEnumMu sync.Mutex

var hostNSS nameService = cNSS{}

// maxGID bounds what getgrgid_r can be asked for. Ruby refuses ids above
// Puppet[:maximum_uid] (4294967290) with an error log (util/posix.rb:79-83);
// anything outside that range, or negative, is unresolved here rather than
// truncated into a different gid_t.
const maxGID = 4294967290

// posixGID is Puppet::Util.gid, i.e. get_posix_value(:group, :gid, v)
// (util/posix.rb:172-205). v is an int64 or a string. ok=false is Ruby's nil: no
// such group.
func posixGID(ctx context.Context, ns nameService, v any) (gid int64, ok bool, err error) {
	// posix.rb:173-177: Integer(field) rescuing ArgumentError, so a string that
	// is not an integer literal stays a name.
	var field any = v
	if s, isString := v.(string); isString {
		if n, convErr := rubyInteger(s); convErr == nil {
			field = n
		}
	}

	var id int64
	var check any // check_value: a name or an id, nil when the lookup found none
	if n, isInt := field.(int64); isInt {
		if n < 0 || n > maxGID {
			return 0, false, nil
		}
		byID, found, err := ns.groupByID(n)
		if err != nil || !found {
			return 0, false, err
		}
		byName, found, err := ns.group(byID.Name)
		if err != nil {
			return 0, false, err
		}
		if !found {
			// get_posix_field returns nil, so id == check_value_id == nil: Ruby
			// logs "Multiple entries" with an empty id and returns nil.
			runlog.Debugf(ctx, "Puppet", "Multiple entries found for resource: 'group' with gid: ")
			return 0, false, nil
		}
		id, check = byName.GID, byName.GID
	} else {
		byName, found, err := ns.group(field.(string))
		if err != nil || !found {
			return 0, false, err
		}
		id = byName.GID
		byID, found, err := ns.groupByID(id)
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
		e, found, err := ns.groupByID(c)
		if err != nil {
			return 0, false, err
		}
		checkID, haveCheckID = e.GID, found
	case string:
		e, found, err := ns.group(c)
		if err != nil {
			return 0, false, err
		}
		checkID, haveCheckID = e.GID, found
	}
	if haveCheckID && checkID == id {
		runlog.Debugf(ctx, "Puppet", "Multiple entries found for resource: 'group' with gid: %d", id)
		return id, true, nil
	}
	shown := ""
	if check != nil {
		shown = fmt.Sprint(check)
	}
	runlog.Debugf(ctx, "Puppet", "The value retrieved: '%s' is different than the required state: '%v', searching in all entries", shown, field)

	// search_posix_field (posix.rb:100-126): first entry whose gid equals an
	// integer input, or whose name equals the input.
	all, err := ns.allGroups()
	if err != nil {
		return 0, false, err
	}
	for _, g := range all {
		if n, isInt := field.(int64); isInt && g.GID == n || g.Name == field {
			return g.GID, true, nil
		}
	}
	return 0, false, nil
}

// groupsOf is Puppet::Util::POSIX.groups_of (util/posix.rb:15-34): the groups
// getgrouplist reports for user, in its order, keeping those whose gr_mem lists
// the user (so the primary group is omitted unless listed), duplicates dropped.
// A user with no passwd entry (Etc.getpwnam raises, posix.rb:41) or a gid
// without a group (Etc.getgrgid raises, posix.rb:55) takes Ruby's fallback: a
// scan of every group for the user, in enumeration order.
func groupsOf(ns nameService, user string) ([]string, error) {
	groups, complete, err := groupsFromList(ns, user)
	if err != nil {
		return nil, err
	}
	if !complete {
		all, err := ns.allGroups()
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
func groupsFromList(ns nameService, user string) (groups []string, complete bool, err error) {
	pw, found, err := ns.user(user)
	if err != nil || !found {
		return nil, false, err
	}
	gids, err := ns.groupList(user, pw.GID)
	if err != nil {
		return nil, false, err
	}
	for _, gid := range gids {
		g, found, err := ns.groupByID(gid)
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
