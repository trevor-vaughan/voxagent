package ral

import (
	"errors"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

type fakePlatform struct {
	exec    map[string]bool
	release string
	err     error
}

func (f fakePlatform) Executable(path string) bool { return f.exec[path] }
func (f fakePlatform) OSRelease() ([]byte, error) {
	return []byte(f.release), f.err
}

const (
	centos10 = "NAME=\"CentOS Stream\"\nID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"10\"\n"
	libuser  = "/usr/sbin/luseradd"
)

func ugRes(typ string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: typ, Title: "x", Parameters: params}
}

func route(p Platform, rs ...catalog.Resource) (bool, bool) {
	return NativeUserGroup(p, &catalog.Catalog{Resources: rs})
}

func TestNativeUserGroupPlatform(t *testing.T) {
	cases := []struct {
		name, osr string
		want      bool
	}{
		{"centos stream 10", centos10, true},
		{"rhel 10.1", "ID=rhel\nVERSION_ID=\"10.1\"\n", true},
		{"rhel 9.4", "ID=rhel\nVERSION_ID=\"9.4\"\n", false},
		{"rhel 11", "ID=rhel\nVERSION_ID=11\n", true},
		{"fedora 41", "ID=fedora\nID_LIKE=\"rhel\"\nVERSION_ID=41\n", false},
		{"fedora 41 is not rhel", "ID=fedora\nVERSION_ID=41\n", false},
		{"id_like whole word", "ID=x\nID_LIKE=\"rhelish\"\nVERSION_ID=10\n", false},
		{"id_like single quotes", "ID=x\nID_LIKE='fedora rhel'\nVERSION_ID='10'\n", true},
		{"id_like unquoted", "ID=x\nID_LIKE=rhel\nVERSION_ID=10\n", true},
		{"debian", "ID=debian\nVERSION_ID=\"12\"\n", false},
		{"no version", "ID=rhel\n", false},
		{"non-numeric major", "ID=rhel\nVERSION_ID=\"x.1\"\n", false},
		{"empty", "", false},
		{"garbage", "\x00\xff not os-release", false},
		{"comments and blanks", "# c\n\nID=rhel\n  \nVERSION_ID=10\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, g := route(fakePlatform{release: c.osr}, ugRes("User", nil), ugRes("Group", nil))
			if u != c.want || g != c.want {
				t.Fatalf("got user=%v group=%v, want both %v", u, g, c.want)
			}
		})
	}
}

func TestNativeUserGroupOSReleaseUnreadable(t *testing.T) {
	u, g := route(fakePlatform{err: errors.New("no such file")})
	if u || g {
		t.Fatalf("got %v %v, want false false", u, g)
	}
}

func TestNativeUserGroupLibuser(t *testing.T) {
	cases := []struct {
		name string
		exec map[string]bool
		want bool
	}{
		{"neither", nil, true},
		{"only luseradd", map[string]bool{libuser: true}, true},
		{"only lgroupadd", map[string]bool{"/usr/sbin/lgroupadd": true}, true},
		{"both", map[string]bool{libuser: true, "/usr/sbin/lgroupadd": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, g := route(fakePlatform{exec: c.exec, release: centos10})
			if u != c.want || g != c.want {
				t.Fatalf("got %v %v, want %v", u, g, c.want)
			}
		})
	}
}

func TestNativeUserGroupProvider(t *testing.T) {
	p := fakePlatform{release: centos10}
	cases := []struct {
		name         string
		rs           []catalog.Resource
		wantU, wantG bool
	}{
		{"user ldap", []catalog.Resource{ugRes("User", map[string]any{"provider": "ldap"})}, false, true},
		{"group groupadd", []catalog.Resource{ugRes("Group", map[string]any{"provider": "groupadd"})}, true, true},
		{"group useradd", []catalog.Resource{ugRes("Group", map[string]any{"provider": "useradd"})}, true, false},
		{"user groupadd", []catalog.Resource{ugRes("User", map[string]any{"provider": "groupadd"})}, false, true},
		{"user useradd", []catalog.Resource{ugRes("User", map[string]any{"provider": "useradd"})}, true, true},
		{"one of two users", []catalog.Resource{ugRes("User", nil), ugRes("User", map[string]any{"provider": "pw"})}, false, true},
		{"non-string provider", []catalog.Resource{ugRes("User", map[string]any{"provider": 1.0})}, false, true},
		{"group ldap", []catalog.Resource{ugRes("Group", map[string]any{"provider": "ldap"})}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, g := route(p, c.rs...)
			if u != c.wantU || g != c.wantG {
				t.Fatalf("got %v %v, want %v %v", u, g, c.wantU, c.wantG)
			}
		})
	}
}

func TestNativeUserGroupPurgeDelegatesUserOnly(t *testing.T) {
	p := fakePlatform{release: centos10}
	cases := []struct {
		name   string
		v      any
		native bool
	}{
		{"true", true, false},
		{"false", false, true},
		{"string false", "false", true},
		{"empty list", []any{}, true},
		{"string False", "False", false},
		{"empty string", "", false},
		{"string true", "true", false},
		{"path list", []any{"/x"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, g := route(p, ugRes("User", map[string]any{"purge_ssh_keys": c.v}), ugRes("Group", nil))
			if u != c.native || !g {
				t.Fatalf("got user=%v group=%v, want user=%v group=true", u, g, c.native)
			}
		})
	}
}

// A value shape the native parser does not implement sends its type to the
// host, which applied it before user and group were native; a value Ruby
// itself refuses stays native, refused with Ruby's message.
func TestNativeUserGroupUnimplementedValuesDelegate(t *testing.T) {
	p := fakePlatform{release: centos10}
	cases := []struct {
		name         string
		r            catalog.Resource
		wantU, wantG bool
	}{
		{"user gid array", ugRes("User", map[string]any{"gid": []any{"a"}}), false, true},
		{"user uid fraction", ugRes("User", map[string]any{"uid": 1.5}), false, true},
		{"user comment number", ugRes("User", map[string]any{"comment": 1.0}), false, true},
		{"user home bool", ugRes("User", map[string]any{"home": true}), false, true},
		{"user shell array", ugRes("User", map[string]any{"shell": []any{"/bin/sh"}}), false, true},
		{"user password bool", ugRes("User", map[string]any{"password": true}), false, true},
		{"user age array", ugRes("User", map[string]any{"password_min_age": []any{1.0}}), false, true},
		{"user groups element number", ugRes("User", map[string]any{"groups": []any{"a", 1.0}}), false, true},
		{"user groups hash", ugRes("User", map[string]any{"groups": map[string]any{}}), false, true},
		{"user expiry number", ugRes("User", map[string]any{"expiry": 1.0}), false, true},
		{"group gid fraction", ugRes("Group", map[string]any{"gid": 1.5}), true, false},
		{"group gid array", ugRes("Group", map[string]any{"gid": []any{"1"}}), true, false},
		{"group members element number", ugRes("Group", map[string]any{"members": []any{"a", 1.0}}), true, false},
		{"group members hash", ugRes("Group", map[string]any{"members": map[string]any{}}), true, false},
		// Ruby's own refusals stay native.
		{"user groups csv", ugRes("User", map[string]any{"groups": "a,b"}), true, true},
		{"user password colon", ugRes("User", map[string]any{"password": "a:b"}), true, true},
		{"user uid munge", ugRes("User", map[string]any{"uid": "1-2"}), true, true},
		{"user bad ensure", ugRes("User", map[string]any{"ensure": "gone"}), true, true},
		{"user integer password", ugRes("User", map[string]any{"password": 12345.0}), true, true},
		{"group bad gid", ugRes("Group", map[string]any{"gid": "abc"}), true, true},
		{"group bad system", ugRes("Group", map[string]any{"system": "maybe"}), true, true},
		// The first failure in allattrs order decides: Ruby refuses this one
		// at ensure before it reaches gid.
		{"user ruby refusal first", ugRes("User", map[string]any{"ensure": "gone", "gid": []any{"a"}}), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, g := route(p, c.r)
			if u != c.wantU || g != c.wantG {
				t.Fatalf("got %v %v, want %v %v", u, g, c.wantU, c.wantG)
			}
		})
	}
}

// errors.As tells an unimplemented value from a Ruby validation failure.
func TestUnimplementedValueIsTyped(t *testing.T) {
	var uv *unimplementedValue
	_, err := parseUserParams(ugRes("User", map[string]any{"gid": []any{"a"}}))
	if !errors.As(err, &uv) || err.Error() != `User[x]: gid => ["a"] is not implemented` {
		t.Errorf("user gid array: %v", err)
	}
	_, err = parseGroup(ugRes("Group", map[string]any{"members": []any{1.0}}))
	if !errors.As(err, &uv) || err.Error() != "Group[x]: members => [1] is not implemented" {
		t.Errorf("group members: %v", err)
	}
	for _, err := range []error{
		func() error { _, err := parseUserParams(ugRes("User", map[string]any{"groups": "a,b"})); return err }(),
		func() error { _, err := parseGroup(ugRes("Group", map[string]any{"gid": "abc"})); return err }(),
	} {
		if err == nil || errors.As(err, &uv) {
			t.Errorf("Ruby refusal %v is typed as unimplemented", err)
		}
	}
}

func TestNativeUserGroupIgnoresOtherTypes(t *testing.T) {
	u, g := route(fakePlatform{release: centos10}, ugRes("File", map[string]any{"provider": "x", "purge_ssh_keys": true}))
	if !u || !g {
		t.Fatalf("got %v %v, want true true", u, g)
	}
}

func TestRealPlatformOSRelease(t *testing.T) {
	if _, err := (realPlatform{}).OSRelease(); err != nil {
		t.Skipf("no /etc/os-release: %v", err)
	}
}

func TestUserGroupMinimalProviders(t *testing.T) {
	for _, p := range []Provider{User{}} {
		ch, w, err := p.Apply(t.Context(), ugRes(p.Type(), nil), false)
		if ch != nil || w != nil || err != nil {
			t.Errorf("%s: Apply of an attribute-less resource = %v %v %v", p.Type(), ch, w, err)
		}
		if got, err := p.Prefetch(t.Context(), nil); err != nil || got.Type() != p.Type() {
			t.Errorf("%s: Prefetch = %v, %v", p.Type(), got, err)
		}
		if again, err := p.Refresh(t.Context(), ugRes(p.Type(), nil), false); again || err != nil {
			t.Errorf("%s: Refresh = %v, %v", p.Type(), again, err)
		}
	}
	if *(User{}).ProviderUsed() != "useradd" || *(Group{}).ProviderUsed() != "groupadd" {
		t.Error("provider names")
	}
}
