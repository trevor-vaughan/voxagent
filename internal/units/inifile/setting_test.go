package inifile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

const (
	// msgValueChanged is NOTES M1's value event message: ini_setting and
	// ini_subsetting redact both sides whatever the value (spec §3).
	msgValueChanged = "value changed [redacted sensitive information] to [redacted sensitive information]"
	// msgNoMethodNil is NOTES M2 7.9's refresh failure text.
	msgNoMethodNil = "undefined method `existing_setting?' for nil:NilClass"
)

func settingRes(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "ini_setting", Title: "s", File: "/site.pp", Line: 3, Parameters: params}
}

func writeIni(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.ini")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func applySetting(t *testing.T, p ral.Provider, r catalog.Resource, noop bool) []ral.Change {
	t.Helper()
	changes, warnings, err := p.Apply(context.Background(), r, noop)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Apply: warnings %v err %v", warnings, err)
	}
	return changes
}

func wantChanges(t *testing.T, got, want []ral.Change) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes\n got %#v\nwant %#v", got, want)
	}
}

func ensureChange(name, was, is, msg string) ral.Change {
	return ral.Change{Property: "ensure", Name: name, Was: was, Is: is, Message: msg}
}

func wantValue(was, is []string, msg string) ral.Change {
	return ral.Change{Property: "value", Was: was, Is: is, Message: msg,
		WasDisplay: redactedDisplay, IsDisplay: redactedDisplay, DisplayVerbatim: true}
}

func TestSettingCreateNewFile(t *testing.T) { // M1 create, setting-create-new-file state
	path := filepath.Join(t.TempDir(), "m.ini")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "v"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false),
		[]ral.Change{ensureChange("ini_setting_created", "absent", "present", "created")})
	if got := readFile(t, path); got != "[a]\nk = v\n" {
		t.Fatalf("file %q", got)
	}
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
}

func TestSettingUpdateValue(t *testing.T) { // M1 value change: arrays, redacted message
	path := writeIni(t, "[a]\nk = old\nz = 1\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "new"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false),
		[]ral.Change{wantValue([]string{"old"}, []string{"new"}, msgValueChanged)})
	if got := readFile(t, path); got != "[a]\nk = new\nz = 1\n" {
		t.Fatalf("file %q", got)
	}
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
}

func TestSettingAbsent(t *testing.T) { // M1 removal
	path := writeIni(t, "[a]\nk = v\nz = 1\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false),
		[]ral.Change{ensureChange("ini_setting_removed", "present", "absent", "removed")})
	if got := readFile(t, path); got != "[a]\nz = 1\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSettingSectionOnly(t *testing.T) { // setting-section-only: [new] appended, then in sync
	path := writeIni(t, "")
	r := settingRes(map[string]any{"path": path, "section": "new"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false),
		[]ral.Change{ensureChange("ini_setting_created", "absent", "present", "created")})
	if got := readFile(t, path); got != "[new]\n" {
		t.Fatalf("file %q", got)
	}
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
}

func TestSettingSectionOnlyAbsentNeverConverges(t *testing.T) { // spec 7.10, NOTES extra
	path := writeIni(t, "[a]\nk = 1")
	r := settingRes(map[string]any{"path": path, "section": "a", "ensure": "absent"})
	want := []ral.Change{ensureChange("ini_setting_removed", "present", "absent", "removed")}
	for run := 0; run < 2; run++ {
		wantChanges(t, applySetting(t, IniSetting{}, r, false), want)
	}
	// Kept: destroy edits nothing yet rewrites the file, adding the newline (7.2).
	if got := readFile(t, path); got != "[a]\nk = 1\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSettingEnsureBeatsValue(t *testing.T) { // Puppet's ensure precedence: value is not synced in the same pass
	path := writeIni(t, "[a]\nk = old\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "new", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false),
		[]ral.Change{ensureChange("ini_setting_removed", "present", "absent", "removed")})
	if got := readFile(t, path); got != "[a]\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSettingRefreshonlyInSync(t *testing.T) { // M2: refreshonly with own value out of sync and no refresh
	path := writeIni(t, "[a]\nk = old\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "new", "refreshonly": true})
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
	if got := readFile(t, path); got != "[a]\nk = old\n" {
		t.Fatalf("file %q", got)
	}
	missing := filepath.Join(t.TempDir(), "none.ini")
	r = settingRes(map[string]any{"path": missing, "section": "a", "setting": "k", "value": "v", "refreshonly": true})
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("refreshonly created the file: %v", err)
	}
}

func TestSettingRefreshSetsValue(t *testing.T) { // M2: refresh writes the value, ts:144-149
	path := writeIni(t, "[a]\nk = old\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "new", "refreshonly": true})
	p := IniSetting{}
	if !p.Refreshable(r) {
		t.Fatal("ini_setting is refreshable")
	}
	if ok, err := p.Refresh(context.Background(), r, false); !ok || err != nil {
		t.Fatalf("Refresh = %v, %v", ok, err)
	}
	if got := readFile(t, path); got != "[a]\nk = new\n" {
		t.Fatalf("file %q", got)
	}

	// Without refreshonly, refresh does nothing (ts:149).
	r = settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "other"})
	if ok, err := p.Refresh(context.Background(), r, false); !ok || err != nil {
		t.Fatalf("Refresh = %v, %v", ok, err)
	}
	if got := readFile(t, path); got != "[a]\nk = new\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSettingRefreshAbsentRemoves(t *testing.T) { // M2 refreshonly-absent-present
	path := writeIni(t, "[a]\nk = v\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "ensure": "absent", "refreshonly": true})
	if _, err := (IniSetting{}).Refresh(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "[a]\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSettingRefreshNoopDoesNothing(t *testing.T) { // G5 refreshonly-*-notifier noop; Ruby's process_callback? is false under noop
	cases := map[string]map[string]any{
		"value":          {"value": "new", "refreshonly": true},
		"absent present": {"ensure": "absent", "refreshonly": true},
		"absent missing": {"ensure": "absent", "refreshonly": true, "section": "none"},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeIni(t, "[a]\nk = old\n")
			params["path"], params["setting"] = path, "k"
			if _, ok := params["section"]; !ok {
				params["section"] = "a"
			}
			ok, err := (IniSetting{}).Refresh(context.Background(), settingRes(params), true)
			if ok || err != nil {
				t.Fatalf("Refresh under noop = %v, %v; want false, nil (even where a live refresh fails)", ok, err)
			}
			if got := readFile(t, path); got != "[a]\nk = old\n" {
				t.Fatalf("noop refresh wrote %q", got)
			}
		})
	}
}

func TestSettingRefreshAbsentMissingSection(t *testing.T) { // spec 7.9, M2 text; both cases leave no file
	for name, content := range map[string]*string{"missing section": ptr("[b]\nz = 1\n"), "missing file": nil} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "m.ini")
			if content != nil {
				if err := os.WriteFile(path, []byte(*content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "ensure": "absent", "refreshonly": true})
			_, err := (IniSetting{}).Refresh(context.Background(), r, false)
			if err == nil || err.Error() != msgNoMethodNil {
				t.Fatalf("err = %v", err)
			}
			if content == nil {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("file created: %v", err)
				}
			} else if got := readFile(t, path); got != *content {
				t.Fatalf("file %q", got)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestForceNewSectionCreationFalse(t *testing.T) { // M7 and spec 7.4
	path := writeIni(t, "[a]\nx = 1\n")
	missing := settingRes(map[string]any{"path": path, "section": "missing", "setting": "k", "value": "v",
		"force_new_section_creation": false})
	p := IniSetting{}.prefetched(t)
	wantChanges(t, applySetting(t, p, missing, false), nil)
	if got := readFile(t, path); got != "[a]\nx = 1\n" {
		t.Fatalf("file %q", got)
	}
	// The sibling with its section present is created normally.
	present := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "v",
		"force_new_section_creation": false})
	wantChanges(t, applySetting(t, p, present, false),
		[]ral.Change{ensureChange("ini_setting_created", "absent", "present", "created")})
	if got := readFile(t, path); got != "[a]\nx = 1\nk = v\n" {
		t.Fatalf("file %q", got)
	}
}

func TestForceNewSectionCreationFalseMutatesEnsureForRefresh(t *testing.T) { // spec 7.4: the mutated ensure is what refresh reads (ts:145)
	path := writeIni(t, "[a]\nx = 1\n")
	r := settingRes(map[string]any{"path": path, "section": "missing", "setting": "k", "value": "v",
		"force_new_section_creation": false, "refreshonly": true})
	p := IniSetting{}.prefetched(t)
	wantChanges(t, applySetting(t, p, r, false), nil)
	_, err := p.Refresh(context.Background(), r, false)
	if err == nil || err.Error() != msgNoMethodNil {
		t.Fatalf("err = %v", err)
	}
}

func TestTwoSettingsSameFileBothLand(t *testing.T) { // G5 two-settings-one-file, review focus 2
	path := writeIni(t, "[a]\n")
	p := IniSetting{}.prefetched(t)
	for _, k := range []string{"k1", "k2"} {
		r := settingRes(map[string]any{"path": path, "section": "a", "setting": k, "value": "v" + k[1:]})
		r.Title = k
		applySetting(t, p, r, false)
	}
	if got := readFile(t, path); got != "[a]\nk1 = v1\nk2 = v2\n" {
		t.Fatalf("file %q", got)
	}
}

func TestNoopDoesNotWrite(t *testing.T) { // review focus 3; M1 noop messages
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name   string
		params map[string]any
		want   ral.Change
	}{
		{"create", map[string]any{"setting": "n", "value": "v"},
			ensureChange("ini_setting_created", "absent", "present", "created (noop)")},
		{"value", map[string]any{"setting": "k", "value": "new"},
			wantValue([]string{"old"}, []string{"new"}, msgValueChanged+" (noop)")},
		{"remove", map[string]any{"setting": "k", "ensure": "absent"},
			ensureChange("ini_setting_removed", "present", "absent", "removed (noop)")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeIni(t, "[a]\nk = old\n")
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			c.params["path"], c.params["section"] = path, "a"
			wantChanges(t, applySetting(t, IniSetting{}, settingRes(c.params), true), []ral.Change{c.want})
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(old) || readFile(t, path) != "[a]\nk = old\n" {
				t.Fatalf("noop wrote: mtime %v", info.ModTime())
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "none.ini")
	applySetting(t, IniSetting{}, settingRes(map[string]any{"path": missing, "section": "a", "setting": "k", "value": "v"}), true)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("noop created the file: %v", err)
	}
}

func TestAbsentMissingFileInSync(t *testing.T) { // review focus 4
	path := filepath.Join(t.TempDir(), "none.ini")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created: %v", err)
	}
}

func TestAbsentMissingSectionInSync(t *testing.T) { // review focus 4
	path := writeIni(t, "[b]\nz = 1\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSetting{}, r, false), nil)
	if got := readFile(t, path); got != "[b]\nz = 1\n" {
		t.Fatalf("file %q", got)
	}
}

func TestAutoRequiresParent(t *testing.T) { // M5 table, soft edge (managed gates it)
	table := map[string]string{"/x": "File[/]", "/a/b/": "File[/a]", "//x": "File[//]", "/": "File[/]",
		"/a/b/m.ini": "File[/a/b]", "/a/./b": "File[/a]", "/a/..": "File[/a/../..]"}
	for _, p := range []ral.Provider{IniSetting{}} {
		for path, want := range table {
			r := catalog.Resource{Type: p.Type(), Title: "s", Parameters: map[string]any{"path": path}}
			got := p.AutoRequires(r, func(ref string) bool { return ref == want })
			if !reflect.DeepEqual(got, []string{want}) {
				t.Errorf("%s %q: %v, want [%s]", p.Type(), path, got, want)
			}
			if got := p.AutoRequires(r, func(string) bool { return false }); len(got) != 0 {
				t.Errorf("%s %q: unmanaged parent gave %v", p.Type(), path, got)
			}
		}
	}
}

func TestSensitiveValueRedacted(t *testing.T) { // M1 Sensitive; spec 7.13, 8.2
	const secret = "hunter2-secret"
	path := writeIni(t, "[a]\nk = oldsecret\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": secret})
	r.SensitiveParameters = []string{"value"}
	if !ral.IsMarked(IniSetting{}, r, "value") || ral.IsMarked(IniSetting{}, r, "ensure") {
		t.Fatal("value, and only value among the declared properties, must be markable")
	}
	for _, noop := range []bool{true, false} {
		for _, ch := range applySetting(t, IniSetting{}, r, noop) {
			if shown := ch.Message + ch.WasDisplay + ch.IsDisplay; strings.Contains(shown, secret) || strings.Contains(shown, "oldsecret") {
				t.Fatalf("change leaks a value: %#v", ch)
			}
		}
		// Re-seed so the second pass sees an out-of-sync value again.
		if err := os.WriteFile(path, []byte("[a]\nk = oldsecret\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A failed write names the path, never the value.
	r.Parameters["path"] = filepath.Join(t.TempDir(), "no", "dir", "m.ini")
	changes := applySetting(t, IniSetting{}, r, false)
	if len(changes) != 1 || changes[0].Err == nil || strings.Contains(changes[0].Err.Error(), secret) {
		t.Fatalf("failure change: %#v", changes)
	}
}

func TestSettingCreateFailureIsWrapped(t *testing.T) { // M3 ensure create: "Could not set" wrapper
	path := filepath.Join(t.TempDir(), "nodir", "m.ini")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "v"})
	changes := applySetting(t, IniSetting{}, r, false)
	want := "Could not set 'present' on ensure: No such file or directory @ rb_sysopen - " + path + " (file: /site.pp, line: 3)"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want ||
		changes[0].Property != "ensure" || changes[0].Name != "ini_setting_created" || changes[0].Was != "absent" || changes[0].Is != "present" {
		t.Fatalf("changes %#v", changes)
	}
}

func TestSettingDirectoryAtPathReadsEmpty(t *testing.T) { // M3: File.file? false -> create path -> "Is a directory"
	dir := t.TempDir()
	r := settingRes(map[string]any{"path": dir, "section": "a", "setting": "k", "value": "v"})
	changes := applySetting(t, IniSetting{}, r, false)
	want := "Could not set 'present' on ensure: Is a directory @ rb_sysopen - " + dir + " (file: /site.pp, line: 3)"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes %#v", changes)
	}
}

func TestSettingValueFailureIsBare(t *testing.T) { // M3 value=: no wrapper, redacted display
	path := writeIni(t, "[a]\nk = old\n")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "v"})
	changes := applySetting(t, IniSetting{}, r, false)
	want := wantValue([]string{"old"}, []string{"v"}, msgValueChanged)
	if os.Geteuid() == 0 { // root writes through mode 0444 (AGENTS.md): the change succeeds
		wantChanges(t, changes, []ral.Change{want})
		if got := readFile(t, path); got != "[a]\nk = v\n" {
			t.Fatalf("file %q", got)
		}
		return
	}
	want.Err = os.ErrPermission
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != "Permission denied @ rb_sysopen - "+path {
		t.Fatalf("changes %#v", changes)
	}
	changes[0].Err = os.ErrPermission
	wantChanges(t, changes, []ral.Change{want})
}

func TestSettingReadFailureFailsEvaluation(t *testing.T) { // M6: Apply's error is "Could not evaluate:"
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
	}
	path := writeIni(t, "[a]\n# caf\xc3\xa9\n")
	r := settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": "v"})
	_, _, err := IniSetting{}.Apply(context.Background(), r, false)
	if err == nil || err.Error() != "invalid byte sequence in US-ASCII" {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("LANG", "C.UTF-8")
	if _, _, err := (IniSetting{}).Apply(context.Background(), r, false); err != nil {
		t.Fatalf("valid UTF-8 under a UTF-8 locale: %v", err)
	}
}

func TestSettingInvalidParamsFail(t *testing.T) {
	r := settingRes(map[string]any{"path": "relative", "section": "a"})
	if _, _, err := (IniSetting{}).Apply(context.Background(), r, false); err == nil ||
		!strings.Contains(err.Error(), "File paths must be fully qualified, not 'relative'") {
		t.Fatalf("err = %v", err)
	}
}

func TestSettingNoPathIsTypeError(t *testing.T) { // File.file?(nil)
	_, _, err := IniSetting{}.Apply(context.Background(), settingRes(map[string]any{"section": "a"}), false)
	if err == nil || err.Error() != "no implicit conversion of nil into String" {
		t.Fatalf("err = %v", err)
	}
}

var typeParam = regexp.MustCompile(`new(?:param|property)\(:(\w+)`)

// typeAttrs are the names a type file declares, plus ensure (from `ensurable`)
// and provider (which Puppet adds to every type with providers).
func typeAttrs(t *testing.T, typeFile string) ral.Attrs {
	t.Helper()
	src := readFile(t, filepath.Join("testdata", "6.6.1", "lib", "puppet", "type", typeFile))
	names := []string{"ensure", "provider"}
	for _, m := range typeParam.FindAllStringSubmatch(src, -1) {
		names = append(names, m[1])
	}
	return ral.NewAttrs(names...)
}

func TestAttrsCoverTypeFiles(t *testing.T) {
	for p, file := range map[ral.Provider]string{IniSetting{}: "ini_setting.rb", IniSubsetting{}: "ini_subsetting.rb", IniSection{}: "ini_section.rb"} {
		if got, want := p.Attrs(), typeAttrs(t, file); !reflect.DeepEqual(got, want) {
			t.Errorf("%s Attrs %v, want %v", p.Type(), got.Sorted(), want.Sorted())
		}
	}
}

func TestProviderUsedIsRuby(t *testing.T) { // M8
	for _, p := range []ral.Provider{IniSetting{}, IniSubsetting{}, IniSection{}} {
		if got := p.ProviderUsed(); got == nil || *got != "ruby" {
			t.Errorf("%s ProviderUsed = %v", p.Type(), got)
		}
	}
}

// prefetched is Prefetch's result, the provider the walk applies with.
func (s IniSetting) prefetched(t *testing.T) ral.Provider {
	t.Helper()
	p, err := s.Prefetch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSubsettingHasNoAutorequire(t *testing.T) { // tss: declares none, in any release
	r := catalog.Resource{Type: "ini_subsetting", Title: "s", Parameters: map[string]any{"path": "/a/b/m.ini"}}
	if got := (IniSubsetting{}).AutoRequires(r, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSettingArrayValueIsComparedWhole(t *testing.T) { // array_matching all: same elements, same order, same length
	r := func(path string) catalog.Resource {
		return settingRes(map[string]any{"path": path, "section": "a", "setting": "k", "value": []any{"a", "b"}})
	}
	path := writeIni(t, "[a]\nk = a\nk = b\n")
	wantChanges(t, applySetting(t, IniSetting{}, r(path), false), nil)
	for name, c := range map[string]struct {
		file string
		was  []string
	}{
		"shorter": {"[a]\nk = a\n", []string{"a"}},
		"longer":  {"[a]\nk = a\nk = b\nk = c\n", []string{"a", "b", "c"}},
		"order":   {"[a]\nk = b\nk = a\n", []string{"b", "a"}},
	} {
		path := writeIni(t, c.file)
		wantChanges(t, applySetting(t, IniSetting{}, r(path), false),
			[]ral.Change{wantValue(c.was, []string{"a", "b"}, msgValueChanged)})
		if got := readFile(t, path); got != "[a]\nk = a\nk = b\n" {
			t.Errorf("%s: file %q", name, got)
		}
	}
}
