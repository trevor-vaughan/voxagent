package suid

import (
	"errors"
	"os"
	"os/user"
	"slices"
	"strconv"
	"testing"
)

func needRoot(t *testing.T, reason string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: " + reason)
	}
}

// D-78: every invalid-identity path of a marked user or group, including the
// ids identityKey refuses before any lookup, shows [redacted].
func TestResolveRedactsMarkedIdentity(t *testing.T) {
	marked := func(string) bool { return true }
	for _, tt := range []struct{ usr, group any }{
		{"secretname", nil},
		{"99999999999999999999", nil},
		{float64(-1), nil},
		{nil, "secretgroup"},
		{"nobody", "99999999999999999999"},
	} {
		_, err := Resolve(tt.usr, tt.group, marked)
		var invalid InvalidIdentityError
		if !errors.As(err, &invalid) || invalid.name != "[redacted]" {
			t.Errorf("user %v group %v: %v, want an invalid identity named [redacted]", tt.usr, tt.group, err)
		}
	}
}

func TestResolveNoIdentityIsNil(t *testing.T) {
	cred, err := Resolve(nil, nil, func(string) bool { return false })
	if cred != nil || err != nil {
		t.Errorf("Resolve(nil, nil) = %v, %v, want nil, nil", cred, err)
	}
}

// M15, suidmanager.rb:121: the agent's own uid keeps its groups, so no initgroups.
func TestResolveSameUIDNoSetGroups(t *testing.T) {
	cred, err := Resolve(strconv.Itoa(os.Getuid()), nil, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if cred == nil || cred.Uid != uint32(os.Getuid()) || !cred.NoSetGroups || cred.Groups != nil {
		t.Errorf("credential = %+v, want the agent's uid with NoSetGroups and no Groups", cred)
	}
}

// suidmanager.rb:161-164: another user gets initgroups(name, primary gid).
func TestResolveRootInitgroups(t *testing.T) {
	needRoot(t, "a user other than the agent's own needs its groups read")
	u, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no nobody user: %v", err)
	}
	ids, err := u.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	want := make([]uint32, len(ids))
	for i, id := range ids {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = uint32(n)
	}
	cred, err := Resolve("nobody", nil, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if cred.NoSetGroups || !slices.Equal(cred.Groups, want) {
		t.Errorf("credential = %+v, want Groups %v and NoSetGroups false", cred, want)
	}
}
