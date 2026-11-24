package nameservice

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

// fakeNSS is an in-memory NameService; groups keeps enumeration order.
type fakeNSS struct {
	users    map[string]PasswdEntry
	groups   []GroupEntry
	grouplst map[string][]int64
}

func (f fakeNSS) User(name string) (PasswdEntry, bool, error) {
	u, ok := f.users[name]
	return u, ok, nil
}

func (f fakeNSS) Shadow(string) (ShadowEntry, bool, error) { return ShadowEntry{}, false, nil }

func (f fakeNSS) Group(name string) (GroupEntry, bool, error) {
	for _, g := range f.groups {
		if g.Name == name {
			return g, true, nil
		}
	}
	return GroupEntry{}, false, nil
}

func (f fakeNSS) GroupByID(gid int64) (GroupEntry, bool, error) {
	for _, g := range f.groups {
		if g.GID == gid {
			return g, true, nil
		}
	}
	return GroupEntry{}, false, nil
}

func (f fakeNSS) UserByID(uid int64) (PasswdEntry, bool, error) {
	for _, u := range f.sortedUsers() {
		if u.UID == uid {
			return u, true, nil
		}
	}
	return PasswdEntry{}, false, nil
}

func (f fakeNSS) AllUsers() ([]PasswdEntry, error) { return f.sortedUsers(), nil }

// sortedUsers gives the name-keyed map a stable enumeration order.
func (f fakeNSS) sortedUsers() []PasswdEntry {
	all := slices.Collect(maps.Values(f.users))
	slices.SortFunc(all, func(a, b PasswdEntry) int { return strings.Compare(a.Name, b.Name) })
	return all
}

func (f fakeNSS) GroupList(user string, _ int64) ([]int64, error) { return f.grouplst[user], nil }
func (f fakeNSS) AllGroups() ([]GroupEntry, error)                { return f.groups, nil }

func debugCtx() (context.Context, *[]string) {
	var msgs []string
	l := runlog.New("debug", func(e report.LogEntry) { msgs = append(msgs, e.Message) }, nil)
	return runlog.WithLogger(context.Background(), l), &msgs
}

func mustGID(t *testing.T, ctx context.Context, ns NameService, v any) (int64, bool) {
	t.Helper()
	gid, ok, err := PosixGID(ctx, ns, v, false)
	if err != nil {
		t.Fatalf("PosixGID(%v): %v", v, err)
	}
	return gid, ok
}

func TestPosixGIDName(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := fakeNSS{groups: []GroupEntry{{Name: "g4", GID: 4}}}
	if gid, ok := mustGID(t, ctx, ns, "g4"); !ok || gid != 4 {
		t.Fatalf("got %d, %v", gid, ok)
	}
	if len(*msgs) != 0 {
		t.Errorf("unexpected debug: %v", *msgs)
	}
	if _, ok := mustGID(t, ctx, ns, "nope"); ok {
		t.Error("unknown name resolved")
	}
}

func TestPosixGIDIntegerWithoutGroupIsUnresolved(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{groups: []GroupEntry{{Name: "g4", GID: 4}}}
	for _, v := range []any{int64(1234), int64(-1), int64(1 << 40)} {
		if gid, ok := mustGID(t, ctx, ns, v); ok {
			t.Errorf("%v resolved to %d", v, gid)
		}
	}
	if gid, ok := mustGID(t, ctx, ns, int64(4)); !ok || gid != 4 {
		t.Errorf("int64(4): got %d, %v", gid, ok)
	}
}

func TestPosixGIDNumericStringIsInteger(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{groups: []GroupEntry{{Name: "g74", GID: 74}}}
	if gid, ok := mustGID(t, ctx, ns, "74"); !ok || gid != 74 {
		t.Fatalf("got %d, %v", gid, ok)
	}
	if _, ok := mustGID(t, ctx, ns, "75"); ok {
		t.Error("75 resolved")
	}
}

func TestPosixGIDHexNameIsNumber(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{groups: []GroupEntry{{Name: "g31", GID: 31}}}
	if gid, ok := mustGID(t, ctx, ns, "0x1f"); !ok || gid != 31 {
		t.Fatalf("got %d, %v", gid, ok)
	}
}

func TestPosixGIDMultipleEntries(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := fakeNSS{groups: []GroupEntry{{Name: "a", GID: 5}, {Name: "b", GID: 5}}}
	if gid, ok := mustGID(t, ctx, ns, "b"); !ok || gid != 5 {
		t.Fatalf("got %d, %v", gid, ok)
	}
	want := []string{"Multiple entries found for resource: 'group' with gid: 5"}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}

func TestPosixGIDSearchesAllEntries(t *testing.T) {
	// "b" -> gid 5 -> getgrgid(5) is "a", whose own gid is 6, not 5.
	ctx, msgs := debugCtx()
	ns := byIDFirst{fakeNSS{groups: []GroupEntry{{Name: "a", GID: 6}, {Name: "b", GID: 5}}}}
	gid, ok := mustGID(t, ctx, ns, "b")
	if !ok || gid != 5 {
		t.Fatalf("got %d, %v", gid, ok)
	}
	want := []string{"The value retrieved: 'a' is different than the required state: 'b', searching in all entries"}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}

// byIDFirst makes groupByID answer with a fixed alias, as an NSS source that
// disagrees with getgrnam would.
type byIDFirst struct{ fakeNSS }

func (b byIDFirst) GroupByID(gid int64) (GroupEntry, bool, error) {
	if gid == 5 {
		return GroupEntry{Name: "a", GID: 6}, true, nil
	}
	return b.fakeNSS.GroupByID(gid)
}

func TestPosixGIDSearchFindsNothing(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := noEnum{byIDFirst{fakeNSS{groups: []GroupEntry{{Name: "b", GID: 5}}}}}
	if gid, ok := mustGID(t, ctx, ns, "b"); ok {
		t.Fatalf("resolved to %d", gid)
	}
	if len(*msgs) != 1 {
		t.Errorf("debug = %q", *msgs)
	}
}

type noEnum struct{ byIDFirst }

func (noEnum) AllGroups() ([]GroupEntry, error) { return nil, nil }

func TestPosixGIDPropagatesErrors(t *testing.T) {
	ctx, _ := debugCtx()
	if _, _, err := PosixGID(ctx, failingNSS{}, "x", false); err == nil {
		t.Error("error swallowed")
	}
}

type failingNSS struct{ fakeNSS }

func (failingNSS) Group(string) (GroupEntry, bool, error) {
	return GroupEntry{}, false, fmt.Errorf("boom")
}

func mustUID(t *testing.T, ctx context.Context, ns NameService, v any) (int64, bool) {
	t.Helper()
	uid, ok, err := PosixUID(ctx, ns, v, false)
	if err != nil {
		t.Fatalf("PosixUID(%v): %v", v, err)
	}
	return uid, ok
}

func TestPosixUIDName(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := fakeNSS{users: map[string]PasswdEntry{"u4": {Name: "u4", UID: 4}}}
	if uid, ok := mustUID(t, ctx, ns, "u4"); !ok || uid != 4 {
		t.Fatalf("got %d, %v", uid, ok)
	}
	if len(*msgs) != 0 {
		t.Errorf("unexpected debug: %v", *msgs)
	}
	if _, ok := mustUID(t, ctx, ns, "nope"); ok {
		t.Error("unknown name resolved")
	}
}

func TestPosixUIDIntegerWithoutUserIsUnresolved(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{users: map[string]PasswdEntry{"u4": {Name: "u4", UID: 4}}}
	for _, v := range []any{int64(1234), int64(-1), int64(1 << 40), int64(4294967291)} {
		if uid, ok := mustUID(t, ctx, ns, v); ok {
			t.Errorf("%v resolved to %d", v, uid)
		}
	}
	if uid, ok := mustUID(t, ctx, ns, int64(4)); !ok || uid != 4 {
		t.Errorf("int64(4): got %d, %v", uid, ok)
	}
}

func TestPosixUIDNumericStringIsInteger(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{users: map[string]PasswdEntry{"u74": {Name: "u74", UID: 74}}}
	if uid, ok := mustUID(t, ctx, ns, "74"); !ok || uid != 74 {
		t.Fatalf("got %d, %v", uid, ok)
	}
	if _, ok := mustUID(t, ctx, ns, "75"); ok {
		t.Error("75 resolved")
	}
}

func TestPosixUIDHexNameIsNumber(t *testing.T) {
	ctx, _ := debugCtx()
	ns := fakeNSS{users: map[string]PasswdEntry{"u31": {Name: "u31", UID: 31}}}
	if uid, ok := mustUID(t, ctx, ns, "0x1f"); !ok || uid != 31 {
		t.Fatalf("got %d, %v", uid, ok)
	}
}

func TestPosixUIDMultipleEntries(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := fakeNSS{users: map[string]PasswdEntry{"a": {Name: "a", UID: 5}, "b": {Name: "b", UID: 5}}}
	if uid, ok := mustUID(t, ctx, ns, "b"); !ok || uid != 5 {
		t.Fatalf("got %d, %v", uid, ok)
	}
	want := []string{"Multiple entries found for resource: 'passwd' with uid: 5"}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}

// uidByIDFirst makes UserByID answer with a fixed alias, as an NSS source that
// disagrees with getpwnam would.
type uidByIDFirst struct{ fakeNSS }

func (b uidByIDFirst) UserByID(uid int64) (PasswdEntry, bool, error) {
	if uid == 5 {
		return PasswdEntry{Name: "a", UID: 6}, true, nil
	}
	return b.fakeNSS.UserByID(uid)
}

func TestPosixUIDSearchesAllEntries(t *testing.T) {
	// "b" -> uid 5 -> getpwuid(5) is "a", whose own uid is 6, not 5.
	ctx, msgs := debugCtx()
	ns := uidByIDFirst{fakeNSS{users: map[string]PasswdEntry{"a": {Name: "a", UID: 6}, "b": {Name: "b", UID: 5}}}}
	uid, ok := mustUID(t, ctx, ns, "b")
	if !ok || uid != 5 {
		t.Fatalf("got %d, %v", uid, ok)
	}
	want := []string{"The value retrieved: 'a' is different than the required state: 'b', searching in all entries"}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}

func TestPosixUIDSearchFindsNothing(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := noUserEnum{uidByIDFirst{fakeNSS{users: map[string]PasswdEntry{"b": {Name: "b", UID: 5}}}}}
	if uid, ok := mustUID(t, ctx, ns, "b"); ok {
		t.Fatalf("resolved to %d", uid)
	}
	if len(*msgs) != 1 {
		t.Errorf("debug = %q", *msgs)
	}
}

type noUserEnum struct{ uidByIDFirst }

func (noUserEnum) AllUsers() ([]PasswdEntry, error) { return nil, nil }

func TestPosixUIDRedactsSensitiveValue(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := uidByIDFirst{fakeNSS{users: map[string]PasswdEntry{"a": {Name: "a", UID: 6}, "b": {Name: "b", UID: 5}}}}
	if _, ok, err := PosixUID(ctx, ns, "b", true); err != nil || !ok {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	for _, m := range *msgs {
		if strings.Contains(m, "'a'") || strings.Contains(m, "'b'") {
			t.Errorf("sensitive value leaked: %q", m)
		}
	}
}

func TestPosixUIDPropagatesErrors(t *testing.T) {
	ctx, _ := debugCtx()
	known := fakeNSS{users: map[string]PasswdEntry{"x": {Name: "x", UID: 1}}}
	for name, ns := range map[string]NameService{"User": failingUserNSS{known}, "UserByID": failingUserByIDNSS{known}} {
		for _, v := range []any{"x", int64(1)} {
			if _, _, err := PosixUID(ctx, ns, v, false); err == nil {
				t.Errorf("%s failing, v=%v: error swallowed", name, v)
			}
		}
	}
	ns := failingAllUsersNSS{uidByIDFirst{fakeNSS{users: map[string]PasswdEntry{"b": {Name: "b", UID: 5}}}}}
	if _, _, err := PosixUID(ctx, ns, "b", false); err == nil {
		t.Error("AllUsers error swallowed")
	}
}

// countingNSS counts every lookup that reaches it.
type countingNSS struct {
	fakeNSS
	calls *int
}

func (c countingNSS) User(name string) (PasswdEntry, bool, error) {
	*c.calls++
	return c.fakeNSS.User(name)
}

func (c countingNSS) UserByID(uid int64) (PasswdEntry, bool, error) {
	*c.calls++
	return c.fakeNSS.UserByID(uid)
}

func (c countingNSS) AllUsers() ([]PasswdEntry, error) {
	*c.calls++
	return c.fakeNSS.AllUsers()
}

func TestPosixUIDOutOfRangeMakesNoLookup(t *testing.T) {
	ctx, _ := debugCtx()
	var calls int
	ns := countingNSS{fakeNSS{users: map[string]PasswdEntry{"u": {Name: "u", UID: 4}}}, &calls}
	for _, v := range []any{int64(-1), int64(1 << 40), int64(4294967291), "-1", "4294967291"} {
		if uid, ok, err := PosixUID(ctx, ns, v, false); ok || err != nil {
			t.Errorf("PosixUID(%v) = %d, %v, %v", v, uid, ok, err)
		}
	}
	if calls != 0 {
		t.Errorf("%d lookups for out-of-range ids", calls)
	}
}

// userWithoutName finds uid 7 by id but its name has no entry by name, as a
// source whose getpwuid and getpwnam disagree would.
type userWithoutName struct{ fakeNSS }

func (userWithoutName) UserByID(uid int64) (PasswdEntry, bool, error) {
	if uid == 7 {
		return PasswdEntry{Name: "ghost", UID: 7}, true, nil
	}
	return PasswdEntry{}, false, nil
}

func TestPosixUIDIDFoundButNameLookupMissing(t *testing.T) {
	ctx, msgs := debugCtx()
	if uid, ok := mustUID(t, ctx, userWithoutName{}, int64(7)); ok {
		t.Fatalf("resolved to %d", uid)
	}
	want := []string{"Multiple entries found for resource: 'passwd' with uid: "}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}

type failingUserNSS struct{ fakeNSS }

func (failingUserNSS) User(string) (PasswdEntry, bool, error) {
	return PasswdEntry{}, false, fmt.Errorf("boom")
}

type failingUserByIDNSS struct{ fakeNSS }

func (failingUserByIDNSS) UserByID(int64) (PasswdEntry, bool, error) {
	return PasswdEntry{}, false, fmt.Errorf("boom")
}

type failingAllUsersNSS struct{ uidByIDFirst }

func (failingAllUsersNSS) AllUsers() ([]PasswdEntry, error) { return nil, fmt.Errorf("boom") }

func TestGroupsOfKeepsGrouplistOrder(t *testing.T) {
	ns := fakeNSS{
		users: map[string]PasswdEntry{"u": {Name: "u", GID: 100}},
		groups: []GroupEntry{
			{Name: "adm", GID: 4, Members: []string{"u"}},
			{Name: "wheel", GID: 10, Members: []string{"u", "x"}},
			{Name: "users", GID: 100},
			{Name: "other", GID: 7, Members: []string{"x"}},
		},
		grouplst: map[string][]int64{"u": {100, 10, 4, 10, 7}},
	}
	got, err := GroupsOf(ns, "u")
	if err != nil || !slices.Equal(got, []string{"wheel", "adm"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	ns.groups[2].Members = []string{"u"}
	got, _ = GroupsOf(ns, "u")
	if !slices.Equal(got, []string{"users", "wheel", "adm"}) {
		t.Errorf("primary listing the user: got %v", got)
	}
}

func TestGroupsOfFallbackScanForMissingUser(t *testing.T) {
	ns := fakeNSS{groups: []GroupEntry{
		{Name: "b", GID: 2, Members: []string{"ghost"}},
		{Name: "a", GID: 1, Members: []string{"ghost", "x"}},
		{Name: "c", GID: 3, Members: []string{"x"}},
		{Name: "b", GID: 9, Members: []string{"ghost"}},
	}}
	got, err := GroupsOf(ns, "ghost")
	if err != nil || !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestGrowNSSBufferDoublesOnERANGE(t *testing.T) {
	var sizes []int
	found, err := growNSSBuffer("x", 4096, nil, func(size int) (syscall.Errno, bool) {
		sizes = append(sizes, size)
		if size < 16384 {
			return syscall.ERANGE, false
		}
		return 0, true
	})
	if err != nil || !found || !slices.Equal(sizes, []int{4096, 8192, 16384}) {
		t.Fatalf("found %v, err %v, sizes %v", found, err, sizes)
	}
}

func TestGrowNSSBufferDefaultsTo4KiB(t *testing.T) {
	var first int
	_, _ = growNSSBuffer("x", -1, nil, func(size int) (syscall.Errno, bool) { first = size; return 0, false })
	if first != 4096 {
		t.Errorf("first size = %d", first)
	}
}

func TestGrowNSSBufferFailsPastOneMiB(t *testing.T) {
	var last int
	found, err := growNSSBuffer("getgrnam_r big", 4096, nil, func(size int) (syscall.Errno, bool) {
		last = size
		return syscall.ERANGE, false
	})
	if found || err == nil || err.Error() != "getgrnam_r big: entry larger than 1 MiB" || last != 1<<20 {
		t.Fatalf("found %v, err %v, last size %d", found, err, last)
	}
}

func TestGrowNSSBufferErrnos(t *testing.T) {
	found, err := growNSSBuffer("x", 0, []syscall.Errno{syscall.ENOENT}, func(int) (syscall.Errno, bool) { return syscall.ENOENT, false })
	if found || err != nil {
		t.Errorf("not-found errno: %v, %v", found, err)
	}
	_, err = growNSSBuffer("x", 0, []syscall.Errno{syscall.ENOENT}, func(int) (syscall.Errno, bool) { return syscall.EIO, false })
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("other errno: %v", err)
	}
}

func TestGrowGroupList(t *testing.T) {
	var offered []int
	got, err := growGroupList("u", func(n int) ([]int64, int) {
		offered = append(offered, n)
		switch {
		case n < 600:
			return nil, 600 // libc reports the size it needs
		}
		return make([]int64, 600), 600
	})
	if err != nil || len(got) != 600 || !slices.Equal(offered, []int{256, 600}) {
		t.Fatalf("len %d, err %v, offered %v", len(got), err, offered)
	}
	// libc that does not report a larger count: double.
	offered = nil
	_, _ = growGroupList("u", func(n int) ([]int64, int) {
		offered = append(offered, n)
		if n < 1024 {
			return nil, n
		}
		return []int64{}, 0
	})
	if !slices.Equal(offered, []int{256, 512, 1024}) {
		t.Errorf("offered %v", offered)
	}
	if _, err := growGroupList("u", func(n int) ([]int64, int) { return nil, n }); err == nil {
		t.Error("unbounded growth accepted")
	}
}

func TestShadowCommandSearchesRubysPathInOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "groupadd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(first, "groupmod"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"groupadd", "groupmod"} {
		if err := os.WriteFile(filepath.Join(second, c), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := ShadowPath
	ShadowPath = []string{first, second}
	t.Cleanup(func() { ShadowPath = saved })
	for _, c := range []string{"groupadd", "groupmod"} {
		if got, err := ShadowCommand(c); err != nil || got != filepath.Join(second, c) {
			t.Errorf("ShadowCommand(%s) = %q, %v; want the executable file in %s", c, got, err, second)
		}
	}
	if _, err := ShadowCommand("groupdel"); err == nil {
		t.Error("missing command resolved")
	}
	if !reflect.DeepEqual(saved, []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}) {
		t.Errorf("default search order = %q", saved)
	}
}

// D-78: a marked gid prints as [redacted] in the "required state" debug line,
// and so does the value retrieved, which may be derived from it. An unmarked
// gid prints as before (TestPosixGIDSearchesAllEntries).
func TestPosixGIDRedactsMarkedValue(t *testing.T) {
	ctx, msgs := debugCtx()
	ns := byIDFirst{fakeNSS{groups: []GroupEntry{{Name: "a", GID: 6}, {Name: "b", GID: 5}}}}
	gid, ok, err := PosixGID(ctx, ns, "b", true)
	if err != nil || !ok || gid != 5 {
		t.Fatalf("got %d, %v, %v", gid, ok, err)
	}
	want := []string{"The value retrieved: [redacted] is different than the required state: [redacted], searching in all entries"}
	if !reflect.DeepEqual(*msgs, want) {
		t.Errorf("debug = %q; want %q", *msgs, want)
	}
}
