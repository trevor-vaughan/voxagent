package user

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

const (
	libuser = "/usr/sbin/luseradd"
)

func ugRes(typ string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: typ, Title: "x", Parameters: params}
}

// route reports whether the user unit serves rs natively on p.
func route(p ral.Platform, rs ...catalog.Resource) bool {
	d := Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Platform: p})
	return len(d.Providers) > 0
}

func TestNativeUserGroupPlatform(t *testing.T) {
	cases := []struct {
		name, osr string
		want      bool
	}{
		{"centos stream 10", raltest.CentOS10, true},
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
			got := route(raltest.FakePlatform{Release: c.osr}, ugRes("User", nil), ugRes("Group", nil))
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNativeUserGroupOSReleaseUnreadable(t *testing.T) {
	if got := route(raltest.FakePlatform{Err: errors.New("no such file")}); got {
		t.Fatalf("got %v, want false", got)
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
			got := route(raltest.FakePlatform{Exec: c.exec, Release: raltest.CentOS10})
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNativeUserGroupProvider(t *testing.T) {
	p := raltest.FakePlatform{Release: raltest.CentOS10}
	cases := []struct {
		name string
		rs   []catalog.Resource
		want bool
	}{
		{"user ldap", []catalog.Resource{ugRes("User", map[string]any{"provider": "ldap"})}, false},
		{"group groupadd", []catalog.Resource{ugRes("Group", map[string]any{"provider": "groupadd"})}, true},
		{"group useradd", []catalog.Resource{ugRes("Group", map[string]any{"provider": "useradd"})}, true},
		{"user groupadd", []catalog.Resource{ugRes("User", map[string]any{"provider": "groupadd"})}, false},
		{"user useradd", []catalog.Resource{ugRes("User", map[string]any{"provider": "useradd"})}, true},
		{"one of two users", []catalog.Resource{ugRes("User", nil), ugRes("User", map[string]any{"provider": "pw"})}, false},
		{"non-string provider", []catalog.Resource{ugRes("User", map[string]any{"provider": 1.0})}, false},
		{"group ldap", []catalog.Resource{ugRes("Group", map[string]any{"provider": "ldap"})}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := route(p, c.rs...)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNativeUserGroupPurgeDelegatesUserOnly(t *testing.T) {
	p := raltest.FakePlatform{Release: raltest.CentOS10}
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
			if got := route(p, ugRes("User", map[string]any{"purge_ssh_keys": c.v}), ugRes("Group", nil)); got != c.native {
				t.Fatalf("got user=%v, want user=%v", got, c.native)
			}
		})
	}
}

// A value shape the native parser does not implement sends its type to the
// host, which applied it before user and group were native; a value Ruby
// itself refuses stays native, refused with Ruby's message.
func TestNativeUserGroupUnimplementedValuesDelegate(t *testing.T) {
	p := raltest.FakePlatform{Release: raltest.CentOS10}
	cases := []struct {
		name string
		r    catalog.Resource
		want bool
	}{
		{"user gid array", ugRes("User", map[string]any{"gid": []any{"a"}}), false},
		{"user uid fraction", ugRes("User", map[string]any{"uid": 1.5}), false},
		{"user comment number", ugRes("User", map[string]any{"comment": 1.0}), false},
		{"user home bool", ugRes("User", map[string]any{"home": true}), false},
		{"user shell array", ugRes("User", map[string]any{"shell": []any{"/bin/sh"}}), false},
		{"user password bool", ugRes("User", map[string]any{"password": true}), false},
		{"user age array", ugRes("User", map[string]any{"password_min_age": []any{1.0}}), false},
		{"user groups element number", ugRes("User", map[string]any{"groups": []any{"a", 1.0}}), false},
		{"user groups hash", ugRes("User", map[string]any{"groups": map[string]any{}}), false},
		{"user expiry number", ugRes("User", map[string]any{"expiry": 1.0}), false},
		{"group gid fraction", ugRes("Group", map[string]any{"gid": 1.5}), true},
		{"group gid array", ugRes("Group", map[string]any{"gid": []any{"1"}}), true},
		{"group members element number", ugRes("Group", map[string]any{"members": []any{"a", 1.0}}), true},
		{"group members hash", ugRes("Group", map[string]any{"members": map[string]any{}}), true},
		// Ruby's own refusals stay native.
		{"user groups csv", ugRes("User", map[string]any{"groups": "a,b"}), true},
		{"user password colon", ugRes("User", map[string]any{"password": "a:b"}), true},
		{"user uid munge", ugRes("User", map[string]any{"uid": "1-2"}), true},
		{"user bad ensure", ugRes("User", map[string]any{"ensure": "gone"}), true},
		{"user integer password", ugRes("User", map[string]any{"password": 12345.0}), true},
		{"group bad gid", ugRes("Group", map[string]any{"gid": "abc"}), true},
		{"group bad system", ugRes("Group", map[string]any{"system": "maybe"}), true},
		// The first failure in allattrs order decides: Ruby refuses this one
		// at ensure before it reaches gid.
		{"user ruby refusal first", ugRes("User", map[string]any{"ensure": "gone", "gid": []any{"a"}}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := route(p, c.r)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// errors.As tells an unimplemented value from a Ruby validation failure.
func TestUnimplementedValueIsTyped(t *testing.T) {
	var uv *rubyvalue.UnimplementedValue
	_, err := parseUserParams(ugRes("User", map[string]any{"gid": []any{"a"}}))
	if !errors.As(err, &uv) || err.Error() != `User[x]: gid => ["a"] is not implemented` {
		t.Errorf("user gid array: %v", err)
	}
	for _, err := range []error{
		func() error { _, err := parseUserParams(ugRes("User", map[string]any{"groups": "a,b"})); return err }(),
	} {
		if err == nil || errors.As(err, &uv) {
			t.Errorf("Ruby refusal %v is typed as unimplemented", err)
		}
	}
}

func TestNativeUserGroupIgnoresOtherTypes(t *testing.T) {
	if got := route(raltest.FakePlatform{Release: raltest.CentOS10}, ugRes("File", map[string]any{"provider": "x", "purge_ssh_keys": true})); !got {
		t.Fatalf("got %v, want true", got)
	}
}

func TestUserGroupMinimalProviders(t *testing.T) {
	for _, p := range []ral.Provider{User{}} {
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
	if *(User{}).ProviderUsed() != "useradd" {
		t.Error("provider names")
	}
}

// delegated returns the reasons the user unit sends its type to the host.
func delegated(p ral.Platform, rs ...catalog.Resource) map[string]string {
	return Unit.Route(&unit.Env{Catalog: &catalog.Catalog{Resources: rs}, Platform: p}).Delegated
}

// Each reason is the first failing condition, in Route's check order, and is
// given only for a type the catalog holds (spec §6).
func TestUserDelegationReasons(t *testing.T) {
	centos := raltest.FakePlatform{Release: raltest.CentOS10}
	libuserHost := raltest.FakePlatform{Exec: map[string]bool{libuser: true, "/usr/sbin/lgroupadd": true}, Release: raltest.CentOS10}
	const purge = "purge_ssh_keys generates Ssh_authorized_key resources"
	cases := []struct {
		name string
		p    ral.Platform
		rs   []catalog.Resource
		want map[string]string
	}{
		{"libuser present", libuserHost, []catalog.Resource{ugRes("User", nil)}, map[string]string{"user": "libuser present"}},
		{"not the measured platform", raltest.FakePlatform{Release: "ID=debian\nVERSION_ID=\"12\"\n"},
			[]catalog.Resource{ugRes("User", nil)}, map[string]string{"user": "not the measured platform"}},
		{"libuser wins over release", raltest.FakePlatform{Exec: map[string]bool{libuser: true, "/usr/sbin/lgroupadd": true}},
			[]catalog.Resource{ugRes("User", nil)}, map[string]string{"user": "libuser present"}},
		{"provider", centos, []catalog.Resource{ugRes("User", map[string]any{"provider": "ldap"})}, map[string]string{"user": "provider ldap"}},
		{"non-string provider", centos, []catalog.Resource{ugRes("User", map[string]any{"provider": 1.0})}, map[string]string{"user": "provider 1"}},
		{"purge_ssh_keys", centos, []catalog.Resource{ugRes("User", map[string]any{"purge_ssh_keys": true})}, map[string]string{"user": purge}},
		{"provider wins over purge", centos, []catalog.Resource{
			ugRes("User", map[string]any{"provider": "pw", "purge_ssh_keys": true})}, map[string]string{"user": "provider pw"}},
		{"purge wins over unimplemented", centos, []catalog.Resource{
			ugRes("User", map[string]any{"purge_ssh_keys": true, "gid": []any{"a"}})}, map[string]string{"user": purge}},
		{"unimplemented value", centos, []catalog.Resource{ugRes("User", map[string]any{"gid": []any{"a"}})},
			map[string]string{"user": `User[x]: gid => ["a"] is not implemented`}},
		{"first failing resource wins", centos, []catalog.Resource{
			ugRes("User", map[string]any{"provider": "ldap"}), ugRes("User", map[string]any{"purge_ssh_keys": true})},
			map[string]string{"user": "provider ldap"}},
		{"no user in the catalog", libuserHost, []catalog.Resource{ugRes("Group", nil)}, nil},
		{"native", centos, []catalog.Resource{ugRes("User", nil)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := delegated(c.p, c.rs...); !maps.Equal(got, c.want) {
				t.Fatalf("Delegated = %v, want %v", got, c.want)
			}
		})
	}
}

// A Sensitive parameter's value never reaches a delegation reason or a
// refusal: the reasons are logged at debug level, the refusals are reported.
func TestUserReasonsRedactSensitiveValues(t *testing.T) {
	centos := raltest.FakePlatform{Release: raltest.CentOS10}
	cases := []struct {
		name   string
		params map[string]any
		attr   string
		want   string
	}{
		{"comment", map[string]any{"comment": []any{"hunter2"}}, "comment", "User[x]: comment => [redacted] is not implemented"},
		{"password_max_age", map[string]any{"password_max_age": []any{"hunter2"}}, "password_max_age", "User[x]: password_max_age => [redacted] is not implemented"},
		{"provider", map[string]any{"provider": "hunter2"}, "provider", "provider [redacted]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ugRes("User", c.params)
			r.SensitiveParameters = []string{c.attr}
			if got := delegated(centos, r)["user"]; got != c.want {
				t.Fatalf("reason = %q, want %q", got, c.want)
			}
		})
	}
	for _, attr := range []string{"ensure", "membership", "role_membership"} {
		t.Run("refusal "+attr, func(t *testing.T) {
			r := ugRes("User", map[string]any{attr: "hunter2"})
			r.SensitiveParameters = []string{attr}
			err := User{}.Validate(r)
			if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("err = %v, want a refusal showing [redacted] only", err)
			}
		})
	}
}
