package ral

import (
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func userRes(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "User", Title: title, Parameters: params, File: "/etc/site.pp", Line: 3}
}

func TestUserParamsRefusals(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"groups": "a,b"}, "Parameter groups failed on User[u9]: Group names must be provided as an array, not a comma-separated list."},
		{map[string]any{"groups": []any{"ok", "a,b"}}, "Parameter groups failed on User[u9]: Group names must be provided as an array, not a comma-separated list."},
		{map[string]any{"groups": "1000"}, "Parameter groups failed on User[u9]: Group names must be provided, not GID numbers."},
		{map[string]any{"groups": []any{""}}, `Parameter groups failed on User[u9]: Group names must not be empty. If you want to specify "no groups" pass an empty array`},
		{map[string]any{"expiry": "2030/01/01"}, `Parameter expiry failed on User[u9]: Expiry dates must be YYYY-MM-DD or the string "absent"`},
		{map[string]any{"password": "a:b"}, "Parameter password failed on User[u9]: Passwords cannot include ':'"},
		{map[string]any{"uid": "1-2"}, `Parameter uid failed on User[u9]: Munging failed for value "1-2" in class uid: invalid value for Integer(): "1-2"`},
		{map[string]any{"gid": "09"}, `Parameter gid failed on User[u9]: Munging failed for value "09" in class gid: invalid value for Integer(): "09"`},
		{map[string]any{"password_min_age": "x"}, "Parameter password_min_age failed on User[u9]: Password minimum age must be provided as a number."},
		{map[string]any{"password_max_age": "1.5"}, "Parameter password_max_age failed on User[u9]: Password maximum age must be provided as a number."},
		{map[string]any{"password_warn_days": 1.5}, "Parameter password_warn_days failed on User[u9]: Password warning days must be provided as a number."},
		{map[string]any{"password_warn_days": "08"}, `Parameter password_warn_days failed on User[u9]: Munging failed for value "08" in class password_warn_days: invalid value for Integer(): "08"`},
		{map[string]any{"membership": "bogus"}, `Parameter membership failed on User[u9]: Invalid value "bogus". Valid values are inclusive, minimum.`},
		{map[string]any{"key_membership": "bogus"}, `Parameter key_membership failed on User[u9]: Invalid value "bogus". Valid values are inclusive, minimum.`},
		{map[string]any{"ensure": "role"}, "Parameter ensure failed on User[u9]: Provider useradd must have features 'manages_solaris_rbac' to set 'ensure' to 'role'"},
		{map[string]any{"ensure": "gone"}, `Parameter ensure failed on User[u9]: Invalid value "gone". Valid values are present, absent, role.`},
		{map[string]any{"managehome": "maybe"}, `Parameter managehome failed on User[u9]: Munging failed for value "maybe" in class managehome: expected a boolean value`},
		{map[string]any{"system": 1.0}, `Parameter system failed on User[u9]: Munging failed for value 1 in class system: expected a boolean value`},
		{map[string]any{"purge_ssh_keys": true}, "User[u9]: purge_ssh_keys => true is not implemented"},
		{map[string]any{"purge_ssh_keys": []any{"/a"}}, `User[u9]: purge_ssh_keys => ["/a"] is not implemented`},
		{map[string]any{"uid": 1.5}, "User[u9]: uid => 1.5 is not implemented"},
		{map[string]any{"groups": []any{1.0}}, "User[u9]: groups => [1] is not implemented"},
		// The password value is never printed, whatever its shape.
		{map[string]any{"password": 1.5}, "User[u9]: password => [redacted] is not implemented"},
		{map[string]any{"password": true}, "User[u9]: password => [redacted] is not implemented"},
		// First failure in allattrs order: properties in definition order, then parameters.
		{map[string]any{"managehome": "maybe", "expiry": "x", "uid": "1-2"}, `Parameter uid failed on User[u9]: Munging failed for value "1-2" in class uid: invalid value for Integer(): "1-2"`},
		{map[string]any{"membership": "bogus", "managehome": "maybe"}, `Parameter membership failed on User[u9]: Invalid value "bogus". Valid values are inclusive, minimum.`},
		{map[string]any{"managehome": "maybe", "role_membership": "bogus"}, `Parameter managehome failed on User[u9]: Munging failed for value "maybe" in class managehome: expected a boolean value`},
		{map[string]any{"ensure": "role", "groups": "a,b"}, "Parameter ensure failed on User[u9]: Provider useradd must have features 'manages_solaris_rbac' to set 'ensure' to 'role'"},
	} {
		if got := errText(User{}.Validate(userRes("u9", tc.params))); got != tc.want {
			t.Errorf("%v:\n got %q\nwant %q", tc.params, got, tc.want)
		}
	}
}

func TestUserParamsAccepts(t *testing.T) {
	// A Sensitive password is refused without its value in the text.
	r := userRes("u9", map[string]any{"password": "a:b"})
	r.SensitiveParameters = []string{"password"}
	if got := errText(User{}.Validate(r)); got != "Parameter password failed on User[u9]: Passwords cannot include ':'" {
		t.Errorf("sensitive password: %q", got)
	}
	// Dropped for a missing feature before munging: never refused.
	dropped := map[string]any{"roles": "a,b", "auths": 1.0, "profiles": true, "keys": "x", "project": 3.0,
		"attributes": "x", "salt": 1.0, "iterations": "x", "loginclass": "", "forcelocal": "maybe", "ia_load_module": 2.0}
	if err := (User{}).Validate(userRes("u", dropped)); err != nil {
		t.Errorf("dropped attributes: %v", err)
	}
	for _, v := range []any{false, "false", []any{}} {
		if err := (User{}).Validate(userRes("u", map[string]any{"purge_ssh_keys": v})); err != nil {
			t.Errorf("purge_ssh_keys %v: %v", v, err)
		}
	}
}

func TestUserParamsParse(t *testing.T) {
	s := func(v string) *string { return &v }
	n := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name   string
		params map[string]any
		want   userParams
	}{
		{"nothing", nil, userParams{Name: "u"}},
		{"leading zero is octal", map[string]any{"uid": "0010"}, userParams{Name: "u", Ensure: "present", UID: int64(8)}},
		{"non-numeric uid kept", map[string]any{"uid": "abc"}, userParams{Name: "u", Ensure: "present", UID: "abc"}},
		{"numeric string", map[string]any{"uid": "74", "gid": "wheel"}, userParams{Name: "u", Ensure: "present", UID: int64(74), GID: "wheel"}},
		{"number", map[string]any{"uid": 74.0, "gid": 0.0}, userParams{Name: "u", Ensure: "present", UID: int64(74), GID: int64(0)}},
		{"empty groups", map[string]any{"groups": []any{}}, userParams{Name: "u", Ensure: "present", Groups: []string{}}},
		{"groups string", map[string]any{"groups": "wheel"}, userParams{Name: "u", Ensure: "present", Groups: []string{"wheel"}}},
		{"explicit ensure", map[string]any{"ensure": "absent"}, userParams{Name: "u", Ensure: "absent"}},
		{"explicit ensure, only dropped", map[string]any{"ensure": "present", "roles": "r"}, userParams{Name: "u", Ensure: "present"}},
		{"M30: all given attributes dropped", map[string]any{"roles": []any{"r1"}, "auths": []any{"a1"}, "loginclass": "lc", "salt": "abcd", "forcelocal": true, "system": true},
			userParams{Name: "u", System: true}},
		{"name parameter", map[string]any{"name": "real", "home": "/h"}, userParams{Name: "real", Ensure: "present", Home: s("/h")}},
		{"ages", map[string]any{"password_min_age": "7", "password_max_age": 90.0, "password_warn_days": "-1"},
			userParams{Name: "u", Ensure: "present", MinAge: n(7), MaxAge: n(90), WarnDays: n(-1)}},
		{"strings", map[string]any{"comment": "C,x", "shell": "/bin/sh", "password": "$6$h", "expiry": "2030-01-02"},
			userParams{Name: "u", Ensure: "present", Comment: s("C,x"), Shell: s("/bin/sh"), Password: s("$6$h"), Expiry: s("2030-01-02")}},
		{"expiry absent", map[string]any{"expiry": "absent"}, userParams{Name: "u", Ensure: "present", Expiry: s("absent")}},
		// An Integer password is its to_s: insync compares desired.to_s
		// (property.rb:397) and execute and chpasswd's input to_s it.
		{"integer password", map[string]any{"password": 12345.0}, userParams{Name: "u", Ensure: "present", Password: s("12345")}},
		{"booleans", map[string]any{"membership": "inclusive", "system": "Yes", "allowdupe": true, "managehome": "no"},
			userParams{Name: "u", Inclusive: true, System: true, AllowDupe: true}},
	} {
		got, err := parseUserParams(userRes("u", tc.params))
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, %v\nwant %+v", tc.name, got, err, tc.want)
		}
	}
}

func TestUserAttrs(t *testing.T) {
	want := NewAttrs("ensure", "home", "uid", "gid", "comment", "shell", "password", "password_min_age",
		"password_max_age", "password_warn_days", "groups", "expiry", "roles", "auths", "profiles", "keys",
		"project", "attributes", "salt", "iterations", "loginclass", "name", "membership", "system", "allowdupe",
		"managehome", "role_membership", "auth_membership", "profile_membership", "key_membership",
		"ia_load_module", "attribute_membership", "forcelocal", "purge_ssh_keys", "provider")
	if got := (User{}).Attrs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Attrs = %v, want %v", got, want)
	}
}

// M30 User[u10]: the ten lines in the order Ruby logged them.
func TestUserConversionDebug(t *testing.T) {
	var _ ConversionDebugging = User{}
	line := func(feature, attr string) string {
		return "Provider useradd does not support features " + feature + "; not managing attribute " + attr
	}
	forcelocal := line("manages_local_users_and_groups", "forcelocal")
	if got := (User{}).ConversionDebug(userRes("u", nil)); !reflect.DeepEqual(got, []string{forcelocal}) {
		t.Errorf("no attributes: %q", got)
	}
	r := userRes("u10", map[string]any{"roles": []any{"r1"}, "auths": []any{"a1"}, "profiles": []any{"p1"},
		"keys": map[string]any{"k": "v"}, "project": "proj", "loginclass": "lc", "salt": "abcd", "iterations": 5.0,
		"attributes": []any{"x=y"}, "forcelocal": true})
	want := []string{
		line("manages_roles", "roles"),
		line("manages_solaris_rbac", "auths"),
		line("manages_solaris_rbac", "profiles"),
		line("manages_solaris_rbac", "keys"),
		line("manages_solaris_rbac", "project"),
		line("manages_aix_lam", "attributes"),
		line("manages_password_salt", "salt"),
		line("manages_password_salt", "iterations"),
		line("manages_loginclass", "loginclass"),
		forcelocal,
	}
	if got := (User{}).ConversionDebug(r); !reflect.DeepEqual(got, want) {
		t.Errorf("M30:\n got %q\nwant %q", got, want)
	}
	got := (User{}).ConversionDebug(userRes("u", map[string]any{"ia_load_module": "files"}))
	if want := []string{line("manages_aix_lam", "ia_load_module"), forcelocal}; !reflect.DeepEqual(got, want) {
		t.Errorf("ia_load_module: %q", got)
	}
}
