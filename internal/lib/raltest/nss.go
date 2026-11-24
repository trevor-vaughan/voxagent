package raltest

import "github.com/voxagent/voxagent/internal/lib/nameservice"

// FakeNSS is an in-memory nameservice.NameService; Groups keeps enumeration order.
type FakeNSS struct {
	Users    map[string]nameservice.PasswdEntry
	Groups   []nameservice.GroupEntry
	Grouplst map[string][]int64
}

func (f FakeNSS) User(name string) (nameservice.PasswdEntry, bool, error) {
	u, ok := f.Users[name]
	return u, ok, nil
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
