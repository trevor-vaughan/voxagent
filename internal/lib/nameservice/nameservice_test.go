package nameservice

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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

func (f fakeNSS) GroupList(user string, _ int64) ([]int64, error) { return f.grouplst[user], nil }
func (f fakeNSS) AllGroups() ([]GroupEntry, error)                { return f.groups, nil }

func debugCtx() (context.Context, *[]string) {
	var msgs []string
	l := runlog.New("debug", func(e report.LogEntry) { msgs = append(msgs, e.Message) }, nil)
	return runlog.WithLogger(context.Background(), l), &msgs
}

func mustGID(t *testing.T, ctx context.Context, ns NameService, v any) (int64, bool) {
	t.Helper()
	gid, ok, err := PosixGID(ctx, ns, v)
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
	if _, _, err := PosixGID(ctx, failingNSS{}, "x"); err == nil {
		t.Error("error swallowed")
	}
}

type failingNSS struct{ fakeNSS }

func (failingNSS) Group(string) (GroupEntry, bool, error) {
	return GroupEntry{}, false, fmt.Errorf("boom")
}

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
