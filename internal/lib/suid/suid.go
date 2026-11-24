// Package suid resolves the user and group a child drops to into a
// syscall.Credential, with Ruby's SUIDManager rules (spec §3.3).
package suid

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// InvalidIdentityError is convert_xid's failure (suidmanager.rb:150). Ruby
// raises it in the child, whose rescue logs it to the command's output and
// exits 1 (util/execution.rb:393-395, M6, M18).
type InvalidIdentityError struct{ kind, name string }

func (e InvalidIdentityError) Error() string { return "Invalid " + e.kind + ": " + e.name }

// Resolve turns exec's user and group into the child's credential
// (spec §4.1), with the os/user lookups File uses. A string of digits or an
// Integer is an id that must exist; anything else is a name. The group is
// resolved first, as change_privileges changes the group first.
//
// marked says the catalog marks "user" or "group" Sensitive. No error then
// carries its value (D-78), nor the lookup's own error, whose text names it
// (cgo's "user: lookup username <name>: <errno>"), as lookupUID does not wrap
// its lookup error either.
func Resolve(usr, group any, marked func(kind string) bool) (*syscall.Credential, error) {
	if usr == nil && group == nil {
		return nil, nil
	}
	var gid uint32
	if group != nil {
		key, byID, err := identityKey("group", group, marked("group"))
		if err != nil {
			return nil, err
		}
		var g *user.Group
		if byID {
			g, err = user.LookupGroupId(key)
		} else {
			g, err = user.LookupGroup(key)
		}
		if err != nil {
			return nil, unknownIdentity("group", group, marked("group"), err)
		}
		if gid, err = parseXID(g.Gid); err != nil {
			return nil, err
		}
	}
	// The agent's uid comes from the kernel's 32-bit uid_t.
	agentUID := uint32(os.Getuid())
	if usr == nil {
		// M15: the uid stays and so do the agent's supplementary groups.
		return &syscall.Credential{Uid: agentUID, Gid: gid, NoSetGroups: true}, nil
	}
	key, byID, err := identityKey("user", usr, marked("user"))
	if err != nil {
		return nil, err
	}
	var u *user.User
	if byID {
		u, err = user.LookupId(key)
	} else {
		u, err = user.Lookup(key)
	}
	if err != nil {
		return nil, unknownIdentity("user", usr, marked("user"), err)
	}
	uid, err := parseXID(u.Uid)
	if err != nil {
		return nil, err
	}
	if group == nil {
		if gid, err = parseXID(u.Gid); err != nil {
			return nil, err
		}
	}
	if uid == agentUID {
		// change_user returns at once for the process's own uid
		// (suidmanager.rb:121), so no initgroups: a non-root agent could not.
		return &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true}, nil
	}
	// initgroups(name, primary gid) (suidmanager.rb:161-164).
	ids, err := u.GroupIds()
	if err != nil {
		if marked("user") {
			return nil, errors.New("Could not read the supplementary groups of user [redacted]")
		}
		return nil, fmt.Errorf("Could not read the supplementary groups of user %s: %w", u.Username, err)
	}
	groups := make([]uint32, len(ids))
	for i, id := range ids {
		if groups[i], err = parseXID(id); err != nil {
			return nil, err
		}
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}, nil
}

// identityKey is the lookup key for a user or group value and whether it is
// an id. An id beyond 32 bits names no entry, so it fails as an unknown one.
func identityKey(kind string, v any, redact bool) (key string, byID bool, err error) {
	switch v := v.(type) {
	case string:
		if v == "" || strings.Trim(v, "0123456789") != "" {
			return v, false, nil
		}
		if _, err := strconv.ParseUint(v, 10, 32); err != nil {
			return "", false, InvalidIdentityError{kind, rubyvalue.RedactIf(redact, "%s", v)}
		}
		return v, true, nil
	case float64:
		// Kernel#Integer truncates a Float (posix.rb:174).
		n := math.Trunc(v)
		if n < 0 || n > math.MaxUint32 {
			return "", false, InvalidIdentityError{kind, rubyvalue.RedactIf(redact, "%s", rubyvalue.InspectValue(v))}
		}
		return strconv.FormatUint(uint64(n), 10), true, nil
	default:
		return "", false, fmt.Errorf("%s is %T, not a string or an integer", kind, v)
	}
}

// unknownIdentity is the invalid-identity error for a lookup that found no
// entry; any other lookup failure is wrapped, unless redact withholds it.
func unknownIdentity(kind string, v any, redact bool, err error) error {
	var (
		noUser    user.UnknownUserError
		noUID     user.UnknownUserIdError
		noGroup   user.UnknownGroupError
		noGroupID user.UnknownGroupIdError
	)
	if !errors.As(err, &noUser) && !errors.As(err, &noUID) && !errors.As(err, &noGroup) && !errors.As(err, &noGroupID) {
		if redact {
			return fmt.Errorf("Could not look up %s [redacted]", kind)
		}
		return fmt.Errorf("Could not look up %s: %w", kind, err)
	}
	name, ok := v.(string)
	if !ok {
		name = rubyvalue.InspectValue(v)
	}
	return InvalidIdentityError{kind, rubyvalue.RedactIf(redact, "%s", name)}
}

// parseXID reads an id from the user or group database. A malformed one is
// not quoted: it may belong to a marked user or group.
func parseXID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errors.New("the user or group database holds an id that is not a 32-bit number")
	}
	return uint32(n), nil
}
