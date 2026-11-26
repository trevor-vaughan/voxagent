package inifile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

func subsettingRes(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "ini_subsetting", Title: "s", File: "/site.pp", Line: 3, Parameters: params}
}

func strValueChange(was, is, msg string) ral.Change {
	return ral.Change{Property: "value", Was: was, Is: is, Message: msg,
		WasDisplay: redactedDisplay, IsDisplay: redactedDisplay, DisplayVerbatim: true}
}

func TestSubsettingCreate(t *testing.T) { // M1 subsetting: ini_subsetting_created, strings
	path := writeIni(t, "[a]\no = -Xfoo\n")
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "-Xmx", "value": "512m"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false),
		[]ral.Change{ensureChange("ini_subsetting_created", "absent", "present", "created")})
	if got := readFile(t, path); got != "[a]\no = -Xfoo -Xmx512m\n" {
		t.Fatalf("file %q", got)
	}
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false), nil)
}

func TestSubsettingChangeAndRemove(t *testing.T) { // M1: value_changed with strings, then ini_subsetting_removed
	path := writeIni(t, "[a]\no = -Xms256m -Xfoo\n")
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "-Xms", "value": "1g"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false),
		[]ral.Change{strValueChange("256m", "1g", msgValueChanged)})
	if got := readFile(t, path); got != "[a]\no = -Xms1g -Xfoo\n" {
		t.Fatalf("file %q", got)
	}
	r.Parameters["ensure"] = "absent"
	wantChanges(t, applySetting(t, IniSubsetting{}, r, true),
		[]ral.Change{ensureChange("ini_subsetting_removed", "present", "absent", "removed (noop)")})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false),
		[]ral.Change{ensureChange("ini_subsetting_removed", "present", "absent", "removed")})
	if got := readFile(t, path); got != "[a]\no = -Xfoo\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSubsettingEmptyValueExists(t *testing.T) { // spec 7.5: '' is truthy
	path := writeIni(t, "[a]\no = -v\n")
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "-v", "value": ""})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false), nil)
	if got := readFile(t, path); got != "[a]\no = -v\n" {
		t.Fatalf("file %q", got)
	}
	// The same through exact matching, which returns Ruby's true.
	r.Parameters["use_exact_match"] = "true"
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false), nil)
}

func TestSubsettingDeleteIfEmpty(t *testing.T) { // spec 7.11: effectively always on
	for name, params := range map[string]map[string]any{
		"unset":    {},
		"true":     {"delete_if_empty": "true"},
		"explicit": {"delete_if_empty": "false"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeIni(t, "[a]\no = A\np = B\n")
			params["path"], params["section"], params["setting"], params["subsetting"], params["ensure"] = path, "a", "o", "A", "absent"
			wantChanges(t, applySetting(t, IniSubsetting{}, subsettingRes(params), false),
				[]ral.Change{ensureChange("ini_subsetting_removed", "present", "absent", "removed")})
			if got := readFile(t, path); got != "[a]\np = B\n" {
				t.Fatalf("file %q", got)
			}
		})
	}
}

func TestSubsettingIgnoresCustomSectionSyntax(t *testing.T) { // spec 7.6: its IniFile has the default [ ]
	path := writeIni(t, "<a>\no = X\n")
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "Y", "value": "1"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false),
		[]ral.Change{ensureChange("ini_subsetting_created", "absent", "present", "created")})
	if got := readFile(t, path); got != "<a>\no = X\n\n[a]\no = Y1\n" {
		t.Fatalf("file %q", got)
	}
}

func TestSubsettingAbsentMissingFileInSync(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.ini")
	r := subsettingRes(map[string]any{"path": missing, "section": "a", "setting": "o", "subsetting": "Y", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false), nil)
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("file created: %v", err)
	}
}

func TestSubsettingAbsentMissingSectionInSync(t *testing.T) {
	r := subsettingRes(map[string]any{"path": writeIni(t, "[b]\n"), "section": "a", "setting": "o", "subsetting": "Y", "ensure": "absent"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, false), nil)
}

func TestSubsettingRefusedSeparatorSurfaces(t *testing.T) { // ruling R8: Apply surfaces the port's refusal
	path := writeIni(t, "[a]\no = AdB\n")
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "A", "subsetting_separator": "d"})
	_, _, err := IniSubsetting{}.Apply(context.Background(), r, false)
	if want := `subsetting_separator "d": not reproduced natively: ruby regexp or bignum semantics`; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestSubsettingCreateFailureIsWrapped(t *testing.T) { // M3 wrapper on ensure
	missing := t.TempDir() + "/nodir/m.ini"
	r := subsettingRes(map[string]any{"path": missing, "section": "a", "setting": "o", "subsetting": "Y", "value": "1"})
	changes := applySetting(t, IniSubsetting{}, r, false)
	want := "Could not set 'present' on ensure: No such file or directory @ rb_sysopen - " + missing + " (file: /site.pp, line: 3)"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes %#v", changes)
	}
}

func TestSubsettingNoopDoesNotWrite(t *testing.T) {
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	path := writeIni(t, "[a]\no = -Xms256m\n")
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	r := subsettingRes(map[string]any{"path": path, "section": "a", "setting": "o", "subsetting": "-Xms", "value": "1g"})
	wantChanges(t, applySetting(t, IniSubsetting{}, r, true),
		[]ral.Change{strValueChange("256m", "1g", msgValueChanged+" (noop)")})
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(old) || readFile(t, path) != "[a]\no = -Xms256m\n" {
		t.Fatalf("noop wrote: %v %v", info, err)
	}
}

func TestSubsettingNotRefreshable(t *testing.T) {
	if (IniSubsetting{}).Refreshable(subsettingRes(nil)) {
		t.Fatal("ini_subsetting defines no refresh")
	}
}
