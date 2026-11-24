package ral

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestCNSSAgainstRealEntries reads a throwaway account back through libc. It
// edits /etc/passwd, /etc/shadow and /etc/group, so it needs root; run it from
// the built test binary under sudo (never `sudo go test`).
func TestCNSSAgainstRealEntries(t *testing.T) {
	const (
		user  = "voxnsstest"
		group = "voxnssgrp"
		extra = "voxnssextra"
		hash  = "$6$saltsalt$abcdefghijklmnopqrstuvwxyz0123456789"
	)
	ns := cNSS{}

	if os.Geteuid() != 0 {
		// Without root getspnam_r normally fails with EACCES, which is absence,
		// not an error; some hosts make /etc/shadow readable, so found may be true.
		if _, _, err := ns.shadow("root"); err != nil {
			t.Errorf("non-root shadow(root) error = %v; want nil", err)
		}
		t.Skip("needs root: groupadd/useradd edit the account databases")
	}

	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("userdel", user).Run()
		_ = exec.Command("groupdel", group).Run()
		_ = exec.Command("groupdel", extra).Run()
	})
	run("groupadd", group)
	run("groupadd", extra)
	run("useradd", "-g", group, "-G", extra, "-c", "A, B", "-s", "/bin/sh", "-d", "/home/"+user,
		"-e", "2030-01-02", user)
	run("usermod", "-p", hash, user)
	run("chage", "-m", "3", "-M", "40", "-W", "5", user)

	u, found, err := ns.user(user)
	if err != nil || !found {
		t.Fatalf("user: found %v, err %v", found, err)
	}
	g, found, err := ns.group(group)
	if err != nil || !found {
		t.Fatalf("group: found %v, err %v", found, err)
	}
	want := passwdEntry{Name: user, UID: u.UID, GID: g.GID, Gecos: "A, B", Dir: "/home/" + user, Shell: "/bin/sh"}
	if u != want {
		t.Errorf("user = %+v; want %+v", u, want)
	}

	sp, found, err := ns.shadow(user)
	if err != nil || !found {
		t.Fatalf("shadow: found %v, err %v", found, err)
	}
	// 2030-01-02 is day 21916 since the epoch.
	if sp.Hash != hash || sp.Min != 3 || sp.Max != 40 || sp.Warn != 5 || sp.Expire != 21916 {
		t.Errorf("shadow fields = min %d max %d warn %d expire %d, hash match %v", sp.Min, sp.Max, sp.Warn, sp.Expire, sp.Hash == hash)
	}
	if _, found, err := ns.shadow("voxnsstest-missing"); found || err != nil {
		t.Errorf("shadow(missing) = %v, %v", found, err)
	}

	byID, found, err := ns.groupByID(g.GID)
	if err != nil || !found || byID.Name != group {
		t.Errorf("groupByID = %+v, %v, %v", byID, found, err)
	}
	ex, _, _ := ns.group(extra)
	if !slices.Contains(ex.Members, user) {
		t.Errorf("extra group members = %v; want %s", ex.Members, user)
	}
	ids, err := ns.groupList(user, u.GID)
	if err != nil || !slices.Contains(ids, g.GID) || !slices.Contains(ids, ex.GID) {
		t.Errorf("groupList = %v, %v; want %d and %d", ids, err, g.GID, ex.GID)
	}
	all, err := ns.allGroups()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(all, func(e groupEntry) bool { return e.Name == extra && e.GID == ex.GID && slices.Contains(e.Members, user) }) {
		t.Errorf("allGroups lacks %s with member %s", extra, user)
	}
	names, err := groupsOf(ns, user)
	if err != nil || !slices.Equal(names, []string{extra}) {
		t.Errorf("groupsOf = %v, %v; want [%s]", names, err, extra)
	}

	if _, found, err := ns.user("voxnsstest-missing"); found || err != nil {
		t.Errorf("user(missing) = %v, %v", found, err)
	}
	if _, found, err := ns.groupByID(1 << 40); found || err != nil {
		t.Errorf("groupByID(huge) = %v, %v", found, err)
	}
}

func rootOrSkip(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: groupadd/useradd edit the account databases")
	}
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// TestCNSSGroupLargerThanInitialBuffer makes a group whose member list exceeds
// the 4 KiB starting buffer, so getgrnam_r, getgrgid_r and getgrent_r must hit
// ERANGE and retry (spec §4).
func TestCNSSGroupLargerThanInitialBuffer(t *testing.T) {
	rootOrSkip(t)
	const group = "voxnssbig"
	var members []string
	for i := 0; i < 600; i++ {
		members = append(members, fmt.Sprintf("voxnssmember%04d", i))
	}
	t.Cleanup(func() {
		_ = exec.Command("groupdel", group).Run()
		_ = exec.Command("groupdel", group+"after").Run()
	})
	// ~10 KiB of gr_mem, then a small group the enumeration must still reach.
	appendGroupLines(t, fmt.Sprintf("%s:x:61900:%s\n%safter:x:61901:\n", group, strings.Join(members, ","), group))

	ns := cNSS{}
	byName, found, err := ns.group(group)
	if err != nil || !found || !slices.Equal(byName.Members, members) {
		t.Fatalf("group: found %v, err %v, %d members", found, err, len(byName.Members))
	}
	byID, found, err := ns.groupByID(byName.GID)
	if err != nil || !found || !slices.Equal(byID.Members, members) {
		t.Fatalf("groupByID: found %v, err %v, %d members", found, err, len(byID.Members))
	}
	all, err := ns.allGroups()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(e groupEntry) bool { return e.Name == group })
	if i < 0 || !slices.Equal(all[i].Members, members) {
		t.Fatalf("allGroups lost the large group (index %d)", i)
	}
	if i+1 >= len(all) || all[i+1].Name != group+"after" {
		t.Fatal("enumeration did not resume after the retried entry")
	}
}

// TestCNSSUserInMoreThan256Groups needs getgrouplist to grow past Ruby's
// initial 256 slots.
func TestCNSSUserInMoreThan256Groups(t *testing.T) {
	rootOrSkip(t)
	const user = "voxnssmany"
	const count = 300
	t.Cleanup(func() {
		_ = exec.Command("userdel", user).Run()
		for i := 0; i < count; i++ {
			_ = exec.Command("groupdel", fmt.Sprintf("voxnssg%03d", i)).Run()
		}
	})
	var names []string
	for i := 0; i < count; i++ {
		names = append(names, fmt.Sprintf("voxnssg%03d", i))
	}
	var lines strings.Builder
	for i, n := range names {
		fmt.Fprintf(&lines, "%s:x:%d:%s\n", n, 61000+i, user)
	}
	appendGroupLines(t, lines.String())
	mustRun(t, "useradd", "-M", "-N", user)

	ns := cNSS{}
	u, found, err := ns.user(user)
	if err != nil || !found {
		t.Fatalf("user: %v %v", found, err)
	}
	ids, err := ns.groupList(user, u.GID)
	if err != nil || len(ids) < count {
		t.Fatalf("groupList returned %d ids, err %v; want at least %d", len(ids), err, count)
	}
	got, err := groupsOf(ns, user)
	if err != nil || !slices.Equal(got, names) {
		t.Errorf("groupsOf: %d names, err %v", len(got), err)
	}
}

// appendGroupLines adds raw group(5) lines in one write; groupadd per entry is
// too slow for hundreds, and gpasswd needs the members to exist as users.
func appendGroupLines(t *testing.T, lines string) {
	t.Helper()
	f, err := os.OpenFile("/etc/group", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := f.WriteString(lines)
	if cerr := f.Close(); werr != nil || cerr != nil {
		t.Fatalf("append /etc/group: %v %v", werr, cerr)
	}
}
