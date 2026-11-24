package raltest

import (
	"maps"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/nameservice"
)

// FakeNSS is an in-memory nameservice.NameService; Groups keeps enumeration order; AllUsers is by name.
type FakeNSS struct {
	Users    map[string]nameservice.PasswdEntry
	Groups   []nameservice.GroupEntry
	Grouplst map[string][]int64
}

func (f FakeNSS) User(name string) (nameservice.PasswdEntry, bool, error) {
	u, ok := f.Users[name]
	return u, ok, nil
}

func (f FakeNSS) UserByID(uid int64) (nameservice.PasswdEntry, bool, error) {
	for _, u := range f.sortedUsers() {
		if u.UID == uid {
			return u, true, nil
		}
	}
	return nameservice.PasswdEntry{}, false, nil
}

func (f FakeNSS) AllUsers() ([]nameservice.PasswdEntry, error) { return f.sortedUsers(), nil }

// sortedUsers gives the name-keyed Users map a stable enumeration order.
func (f FakeNSS) sortedUsers() []nameservice.PasswdEntry {
	all := slices.Collect(maps.Values(f.Users))
	slices.SortFunc(all, func(a, b nameservice.PasswdEntry) int { return strings.Compare(a.Name, b.Name) })
	return all
}

func (f FakeNSS) Shadow(string) (nameservice.ShadowEntry, bool, error) {
	return nameservice.ShadowEntry{}, false, nil
}

func (f FakeNSS) Group(name string) (nameservice.GroupEntry, bool, error) {
	for _, g := range f.Groups {
		if g.Name == name {
			return g, true, nil
		}
	}
	return nameservice.GroupEntry{}, false, nil
}

func (f FakeNSS) GroupByID(gid int64) (nameservice.GroupEntry, bool, error) {
	for _, g := range f.Groups {
		if g.GID == gid {
			return g, true, nil
		}
	}
	return nameservice.GroupEntry{}, false, nil
}

func (f FakeNSS) GroupList(user string, _ int64) ([]int64, error) { return f.Grouplst[user], nil }
func (f FakeNSS) AllGroups() ([]nameservice.GroupEntry, error)    { return f.Groups, nil }
