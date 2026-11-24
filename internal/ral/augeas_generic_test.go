package ral

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

var (
	_ Provider   = Augeas{}
	_ Refreshing = Augeas{}
)

// blacklistConf is the kmod cases' file; blacklistPath its tree.
const (
	blacklistConf = "/etc/modprobe.d/blacklist.conf"
	blacklistPath = "/files" + blacklistConf
)

// resolvChanges is 06d's changes string, SIMP's resolv.conf shape.
const resolvChanges = "rm #comment\nrm nameserver[.!=\"10.0.0.53\"]\nset nameserver[1] 10.0.0.53\n" +
	"rm search/domain[.!=\"example.com\"]\nset search/domain[.=\"example.com\"] example.com\n" +
	"rm options/debug\nrm options/inet6\nrm options/no-check-names\ndefnode opts options \"\"\n" +
	"set $opts/attempts 2\nset $opts/ndots 1\nset $opts/rotate \"\"\nset $opts/timeout 2\nrm options[count(*)=0]\n"

// kmodRes is an Augeas resource of the kmod shape (incl and lens).
func kmodRes(title string, changes any) catalog.Resource {
	return raltest.AugRes("Augeas", title, map[string]any{"incl": blacklistConf, "lens": "Modprobe.lns", "changes": changes})
}

func resolvRes() catalog.Resource {
	return raltest.AugRes("Augeas", "/etc/resolv.conf", map[string]any{"context": "/files/etc/resolv.conf", "changes": resolvChanges})
}

// augeasOver builds Augeas over the run's library with lens path "", the
// recordings' (puppet apply) value, recording each .augnew it unlinks.
func augeasOver(unlinked *[]string) func(*augeasRun) []Provider {
	return func(run *augeasRun) []Provider {
		a := newAugeas(run.lib, "")
		a.unlink = func(p string) error {
			*unlinked = append(*unlinked, p)
			return nil
		}
		return []Provider{a}
	}
}

// wantReturns checks that r succeeded with the one returns change, message
// msg (T:188-190).
func wantReturns(t *testing.T, r caseResult, msg string) {
	t.Helper()
	want := []Change{{Property: "returns", Was: "need_to_run", Is: 0, Message: msg}}
	if r.err != nil || !reflect.DeepEqual(r.changes, want) {
		t.Errorf("%s: changes %+v err %v, want %+v", r.ref, r.changes, r.err, want)
	}
}

// wantInSync checks that r succeeded with no change.
func wantInSync(t *testing.T, r caseResult) {
	t.Helper()
	if r.err != nil || len(r.changes) != 0 {
		t.Errorf("%s: changes %+v err %v, want in sync", r.ref, r.changes, r.err)
	}
}

// wantFailure checks that r failed in retrieve with msg (Could not evaluate).
func wantFailure(t *testing.T, r caseResult, msg string) {
	t.Helper()
	if r.err == nil || r.err.Error() != msg || len(r.changes) != 0 {
		t.Errorf("%s: changes %+v err %v, want failure %q", r.ref, r.changes, r.err, msg)
	}
}

// providerSource is the source of r's provider lines (spec §6).
func providerSource(r caseResult) string { return r.ref + "(provider=augeas)" }

// noopCalls is a live trace's retrieve half under noop (spec §4 step 7): per
// handle, every call through the dry run's saved-files lookup, then the close.
func noopCalls(calls []raltest.TraceCall) []raltest.TraceCall {
	var out []raltest.TraceCall
	done := map[augeas.Handle]bool{}
	for i := 0; i < len(calls); i++ {
		c := calls[i]
		if done[c.Handle] {
			continue
		}
		out = append(out, c)
		if c.Fn == "aug_match" && *c.Args[0] == "/augeas/events/saved" {
			out = append(out, calls[i+1:i+1+len(c.Matches)]...)
			i += len(c.Matches)
			out = append(out, raltest.TraceCall{Src: c.Src + " (noop close)", Handle: c.Handle, Fn: "aug_close"})
			done[c.Handle] = true
		}
	}
	return out
}

// M42: two blacklist entries added, each in its own handle, dry run then sync.
func TestAugeasReplays06a(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06a"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod::setting blacklist cramfs", []any{"set blacklist[. = 'cramfs'] cramfs"}),
		kmodRes("kmod::setting blacklist udf", []any{"set blacklist[. = 'udf'] udf"}),
	}, false)
	for _, r := range got {
		wantReturns(t, r, "executed successfully")
	}
	if want := []string{blacklistConf + ".augnew", blacklistConf + ".augnew"}; !reflect.DeepEqual(unlinked, want) {
		t.Errorf("unlinked %q, want %q", unlinked, want)
	}
	set := `sending command 'set' with params ["/files/etc/modprobe.d/blacklist.conf/blacklist[. = 'cramfs']", "cramfs"]`
	want := []string{
		"Opening augeas with root /, lens path , flags 64",
		"Augeas version 1.15.0 is installed",
		"Will attempt to save and only run if files changed",
		set,
		"Files changed, should execute",
		set,
		"Closed the augeas connection",
	}
	if lines := got[0].debugAt(providerSource(got[0])); !reflect.DeepEqual(lines, want) {
		t.Errorf("debug lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// M42 noop.txt: the dry run still runs and deletes the .augnew, then the
// handle closes and sync is skipped.
func TestAugeasReplays06aNoop(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, noopCalls(raltest.LoadTrace(t, "06a")), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod::setting blacklist cramfs", []any{"set blacklist[. = 'cramfs'] cramfs"}),
		kmodRes("kmod::setting blacklist udf", []any{"set blacklist[. = 'udf'] udf"}),
	}, true)
	for _, r := range got {
		wantReturns(t, r, "executed successfully (noop)")
	}
	if len(unlinked) != 2 {
		t.Errorf("unlinked %q, want both .augnew files", unlinked)
	}
	lines := got[1].debugAt(providerSource(got[1]))
	if n := len(lines); n < 2 || lines[n-2] != "Files changed, should execute" || lines[n-1] != "Closed the augeas connection" {
		t.Errorf("debug lines end %q", lines)
	}
}

// M43: the rerun's dry run saves nothing, so both resources are in sync.
func TestAugeasReplays06b(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06b"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod::setting blacklist cramfs", []any{"set blacklist[. = 'cramfs'] cramfs"}),
		kmodRes("kmod::setting blacklist udf", []any{"set blacklist[. = 'udf'] udf"}),
	}, false)
	for _, r := range got {
		wantInSync(t, r)
		lines := r.debugAt(providerSource(r))
		if n := len(lines); n < 2 || lines[n-2] != "Skipping because no files were changed" {
			t.Errorf("%s: debug lines %q", r.ref, lines)
		}
	}
	if len(unlinked) != 0 {
		t.Errorf("unlinked %q", unlinked)
	}
}

// M44: the dry run's save fails; the put errors are printed and the resource
// fails in retrieve. Its dependent never runs, so it has no handle.
func TestAugeasReplays06c(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06c"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod option", []any{"set blacklist[. = 'cramfs'] cramfs", "set options[. = 'tipc']/foo bar"}),
	}, false)
	wantFailure(t, got[0], "Save failed, see debug output for details")
	lines := got[0].debugAt(providerSource(got[0]))
	errNode := "/augeas/files/etc/modprobe.d/blacklist.conf/error"
	for _, want := range []string{
		"Put failed on one or more files, output from /augeas//error:",
		errNode + " = put_failed",
		errNode + "/path = /files/etc/modprobe.d/blacklist.conf/",
		errNode + "/lens = /opt/puppetlabs/puppet/share/augeas/lenses/dist/modprobe.aug:103.10-.32:",
		"Closed the augeas connection",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("no debug line %q in %q", want, lines)
		}
	}
}

// M45: SIMP's resolv.conf shape: the context restricts the load, and
// defnode and $opts paths reach libaugeas verbatim.
func TestAugeasReplays06d(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06d"), nil, augeasOver(&unlinked), []catalog.Resource{resolvRes()}, false)
	wantReturns(t, got[0], "executed successfully")
	if want := []string{"/etc/resolv.conf.augnew"}; !reflect.DeepEqual(unlinked, want) {
		t.Errorf("unlinked %q, want %q", unlinked, want)
	}
	lines := got[0].debugAt(providerSource(got[0]))
	if !slices.Contains(lines, "Opening augeas with root /, lens path , flags 32") ||
		!slices.Contains(lines, `sending command 'defnode' with params ["opts", "/files/etc/resolv.conf/options", ""]`) {
		t.Errorf("debug lines %q", lines)
	}
}

// M45 noop.txt.
func TestAugeasReplays06dNoop(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, noopCalls(raltest.LoadTrace(t, "06d")), nil, augeasOver(&unlinked), []catalog.Resource{resolvRes()}, true)
	wantReturns(t, got[0], "executed successfully (noop)")
	if len(unlinked) != 1 {
		t.Errorf("unlinked %q", unlinked)
	}
}

// M46.
func TestAugeasReplays06e(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06e"), nil, augeasOver(&unlinked), []catalog.Resource{resolvRes()}, false)
	wantInSync(t, got[0])
}

// M50: an array, a string, a string with blank lines and a nested array all
// send the same two commands.
func TestAugeasReplays06i(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06i"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("changes as array", []any{"set blacklist[. = 'a1'] a1", "set blacklist[. = 'a2'] a2"}),
		kmodRes("changes as string", "set blacklist[. = 's1'] s1\nset blacklist[. = 's2'] s2"),
		kmodRes("changes as one string with CRLF-free blank lines", "\n  set blacklist[. = 'b1'] b1\n\n  set blacklist[. = 'b2'] b2\n"),
		kmodRes("changes as nested array", []any{[]any{"set blacklist[. = 'n1'] n1"}, "set blacklist[. = 'n2'] n2"}),
	}, false)
	for _, r := range got {
		wantReturns(t, r, "executed successfully")
	}
}

// M52.
func TestAugeasReplays06k(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06k"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod option", []any{"set options[. = 'tipc'] tipc", "set options[. = 'tipc']/foo bar"}),
		kmodRes("kmod rm", "rm blacklist[. = 'udf']"),
	}, false)
	for _, r := range got {
		wantReturns(t, r, "executed successfully")
	}
}

// M59: the dry run succeeds, the real save fails on the mount: a property
// sync failure, not a retrieve failure.
func TestAugeasReplays06r(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "06r"), nil, augeasOver(&unlinked), []catalog.Resource{resolvRes()}, false)
	r := got[0]
	if r.err != nil || len(r.changes) != 1 {
		t.Fatalf("changes %+v err %v", r.changes, r.err)
	}
	ch := r.changes[0]
	if ch.Property != "returns" || ch.Was != "need_to_run" || ch.Is != 0 || ch.Err == nil ||
		ch.Err.Error() != "Save failed, see debug output for details" {
		t.Errorf("change %+v", ch)
	}
	lines := r.debugAt(providerSource(r))
	if lines[len(lines)-1] != "Closed the augeas connection" {
		t.Errorf("debug lines end %q", lines)
	}
}

// 06h's run-time errors (M49), recorded natively shaped: failed rm, insert
// and mv are doubled, a bad where is reported before any call, a set that
// fails and an rm that matches nothing are silent, and a bad lens warns.
func TestAugeasReplaysRuntimeErrors(t *testing.T) {
	var unlinked []string
	res := func(title, change string) catalog.Resource { return kmodRes(title, []any{change}) }
	bad := raltest.AugRes("Augeas", "e12 bad lens name", map[string]any{"incl": blacklistConf, "lens": "Nonesuch.lns", "changes": []any{"set foo bar"}})
	got := runAugeasCase(t, raltest.LoadTrace(t, "augeas-runtime-errors"), nil, augeasOver(&unlinked), []catalog.Resource{
		res("e2 rm bad path", "rm blacklist[. = "),
		res("e6 one token", "rm"),
		res("e7 insert bad where", "insert foo sideways blacklist"),
		res("e8 rm no match ok", "rm nothere"),
		res("e9 set multi-match", "set blacklist 'x'"),
		res("e10 context-less abs bad path", "rm /files/etc/modprobe.d/blacklist.conf/blacklist[position() = ]"),
		bad,
		res("e13 mv bad", "mv nothere other"),
		res("e14 insert no match", "insert foo after nothere"),
		res("e15 rm unmatched [", "rm blacklist["),
	}, false)
	doubled := func(cmd, params string) string {
		s := "Error sending command '" + cmd + "' with params " + params
		return s + "/" + s
	}
	wantFailure(t, got[0], doubled("rm", `["/files/etc/modprobe.d/blacklist.conf/blacklist[. ="]`))
	wantFailure(t, got[1], doubled("rm", `["/files/etc/modprobe.d/blacklist.conf/"]`))
	wantFailure(t, got[2], `Error sending command 'insert' with params ["foo", "sideways", "/files/etc/modprobe.d/blacklist.conf/blacklist"]/Invalid value 'sideways' for where param`)
	wantInSync(t, got[3])
	wantInSync(t, got[4])
	wantFailure(t, got[5], doubled("rm", `["/files/etc/modprobe.d/blacklist.conf/blacklist[position() = ]"]`))
	wantFailure(t, got[6], "Save failed, see debug output for details")
	wantFailure(t, got[7], doubled("mv", `["/files/etc/modprobe.d/blacklist.conf/nothere", "/files/etc/modprobe.d/blacklist.conf/other"]`))
	wantFailure(t, got[8], doubled("insert", `["foo", "after", "/files/etc/modprobe.d/blacklist.conf/nothere"]`))
	wantFailure(t, got[9], doubled("rm", `["/files/etc/modprobe.d/blacklist.conf/blacklist["]`))

	wantWarn := []Warning{{Source: "Augeas[e12 bad lens name](provider=augeas)",
		Message: "Loading failed for one or more files, see debug for /augeas//error output"}}
	if !reflect.DeepEqual(got[6].warnings, wantWarn) {
		t.Errorf("e12 warnings %+v, want %+v", got[6].warnings, wantWarn)
	}
	if !slices.Contains(got[6].debugAt(providerSource(got[6])), "/augeas/load/Xfm/error = Can not find lens Nonesuch.lns") {
		t.Errorf("e12 debug lines %q", got[6].debugAt(providerSource(got[6])))
	}
	for i, r := range got {
		if i != 6 && len(r.warnings) != 0 {
			t.Errorf("%s: warnings %+v", r.ref, r.warnings)
		}
	}
}

// A notify only refreshes; Augeas has no refresh, and the rerun is in sync.
func TestAugeasReplaysNotified(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "augeas-notified"), nil, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("blacklist cramfs", []any{"set blacklist[. = 'cramfs'] cramfs"}),
	}, false)
	wantInSync(t, got[0])
}

// puppet apply passes "" whatever the libdir's lenses (spec §0 row 4).
func TestAugeasReplaysApplyIgnoresLibdirLens(t *testing.T) {
	var unlinked []string
	got := runAugeasCase(t, raltest.LoadTrace(t, "augeas-apply-ignores-libdir-lens"), []string{augeasCaseLibdir + "/augeas/lenses"}, augeasOver(&unlinked), []catalog.Resource{
		kmodRes("kmod::setting blacklist cramfs", []any{"set blacklist[. = 'cramfs'] cramfs"}),
	}, false)
	wantInSync(t, got[0])
}

// kmodOpen is the kmod shape's open and dry-run prefix on h1, up to the
// dry run's save mode.
const kmodOpen = `h1 aug_init "/" "" 64
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_match "/augeas/version/pathx/functions/glob" => 1 "/augeas/version/pathx/functions/glob"
h1 aug_set "/augeas/load/Xfm/lens" "Modprobe.lns" => 0
h1 aug_set "/augeas/load/Xfm/incl" "/etc/modprobe.d/blacklist.conf" => 0
h1 aug_load => 0
h1 aug_match "/augeas//error" => 0
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_set "/augeas/save" "newfile" => 0
`

// inSyncTail ends a dry run that saved nothing.
const inSyncTail = `h1 aug_save => 0
h1 aug_match "/augeas/events/saved" => 0
h1 aug_close
`

// applyScript applies one resource over a hand-written script with lens path
// lensPath and the real unlink, returning its result.
func applyScript(t *testing.T, script, lensPath string, r catalog.Resource, noop bool) caseResult {
	t.Helper()
	lib := raltest.NewScriptLib(t, raltest.ParseTrace(t, t.Name(), script))
	return runAugeasCase(t, nil, nil, func(*augeasRun) []Provider {
		return []Provider{newAugeas(lib, lensPath)}
	}, []catalog.Resource{r}, noop)[0]
}

// ruby-augeas raises "Failed to initialize Augeas" for a NULL aug_init, and
// there is no handle to close. The lens path is passed as given.
func TestAugeasInitFailure(t *testing.T) {
	r := applyScript(t, `h0 aug_init "/" "/opt/lenses" 64`+"\n", "/opt/lenses", kmodRes("x", "set a b"), false)
	wantFailure(t, r, "unknown error - Failed to initialize Augeas")
	if lines := r.debugAt(providerSource(r)); !reflect.DeepEqual(lines, []string{"Opening augeas with root /, lens path /opt/lenses, flags 64"}) {
		t.Errorf("debug lines %q", lines)
	}
}

// A failing saved-files lookup raises ruby-augeas' match error, unwrapped.
func TestAugeasSavedFilesMatchFailure(t *testing.T) {
	script := kmodOpen + `h1 aug_set "/files/etc/modprobe.d/blacklist.conf/a" "b" => 0
h1 aug_save => 0
h1 aug_match "/augeas/events/saved" => -1
h1 aug_close
`
	r := applyScript(t, script, "", kmodRes("x", "set a b"), false)
	wantFailure(t, r, "unknown error - Matching path expression '/augeas/events/saved' failed")
}

// savedScript is a dry run that saved file, then closes (the delete failed).
func savedScript(file string) string {
	return kmodOpen + `h1 aug_set "/files/etc/modprobe.d/blacklist.conf/a" "b" => 0
h1 aug_save => 0
h1 aug_match "/augeas/events/saved" => 1 "/augeas/events/saved"
h1 aug_get "/augeas/events/saved" => 1 "/files` + file + `"
h1 aug_close
`
}

// File.delete's Errno text is libc's strerror, then " @ apply2files - ".
func TestAugeasAugnewDeleteFailures(t *testing.T) {
	dir := t.TempDir()
	busy := filepath.Join(dir, "busy")
	if err := os.MkdirAll(filepath.Join(busy+".augnew", "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := applyScript(t, savedScript(busy), "", kmodRes("x", "set a b"), false)
	wantFailure(t, r, "Is a directory @ apply2files - "+busy+".augnew")

	missing := filepath.Join(dir, "missing")
	r = applyScript(t, savedScript(missing), "", kmodRes("y", "set a b"), false)
	wantFailure(t, r, "No such file or directory @ apply2files - "+missing+".augnew")
}

// The unmeasured commands' calls and failures (GC §c7).
func TestAugeasCommands(t *testing.T) {
	bp := blacklistPath
	doubled := func(cmd, params string) string {
		s := "Error sending command '" + cmd + "' with params " + params
		return s + "/" + s
	}
	for _, c := range []struct {
		name, change, calls, err, debug string
	}{
		{"touch missing", "touch foo",
			`h1 aug_match "` + bp + `/foo" => 0
h1 aug_set "` + bp + `/foo" NULL => 0
` + inSyncTail, "", `sending command 'touch' (match, set) with params ["` + bp + `/foo"]`},
		{"touch present", "touch foo",
			`h1 aug_match "` + bp + `/foo" => 1 "` + bp + `/foo"
` + inSyncTail, "", ""},
		{"touch bad path", "touch foo[",
			`h1 aug_match "` + bp + `/foo[" => -1
h1 aug_close
`, `Error sending command 'touch' with params ["` + bp + `/foo["]/unknown error - Matching path expression '` + bp + `/foo[' failed`, ""},
		{"touch clear fails", "touch foo",
			`h1 aug_match "` + bp + `/foo" => 0
h1 aug_set "` + bp + `/foo" NULL => -1
h1 aug_close
`, doubled("touch", `["`+bp+`/foo"]`), ""},
		{"clear fails", "clear foo",
			`h1 aug_set "` + bp + `/foo" NULL => -1
h1 aug_close
`, doubled("clear", `["`+bp+`/foo"]`), `sending command 'clear' with params ["` + bp + `/foo"]`},
		{"clear", "clear foo",
			`h1 aug_set "` + bp + `/foo" NULL => 0
` + inSyncTail, "", ""},
		{"defvar fails", "defvar v foo",
			`h1 aug_defvar "v" "` + bp + `/foo" => -1
h1 aug_close
`, doubled("defvar", `["v", "`+bp+`/foo"]`), ""},
		{"defvar", "defvar v foo",
			`h1 aug_defvar "v" "` + bp + `/foo" => 0
` + inSyncTail, "", ""},
		{"defnode fails", "defnode n foo bar",
			`h1 aug_defnode "n" "` + bp + `/foo" "bar" NULL => -1
h1 aug_close
`, doubled("defnode", `["n", "`+bp+`/foo", "bar"]`), ""},
		{"defnode creates none", "defnode n foo bar",
			`h1 aug_defnode "n" "` + bp + `/foo" "bar" NULL => 0
` + inSyncTail, "", ""},
		{"remove logs as typed", "remove foo",
			`h1 aug_rm "` + bp + `/foo" => 0
` + inSyncTail, "", `sending command 'remove' with params ["` + bp + `/foo"]`},
		{"move fails", "move foo bar",
			`h1 aug_mv "` + bp + `/foo" "` + bp + `/bar" => -1
h1 aug_close
`, doubled("move", `["`+bp+`/foo", "`+bp+`/bar"]`), ""},
		{"ins before", "ins foo before bar",
			`h1 aug_insert "` + bp + `/bar" "foo" 1 => 0
` + inSyncTail, "", `sending command 'ins' with params ["foo", "before", "` + bp + `/bar"]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := applyScript(t, kmodOpen+c.calls, "", kmodRes(c.name, c.change), false)
			if c.err == "" {
				wantInSync(t, r)
			} else {
				wantFailure(t, r, c.err)
			}
			if c.debug != "" && !slices.Contains(r.debugAt(providerSource(r)), c.debug) {
				t.Errorf("no debug line %q in %q", c.debug, r.debugAt(providerSource(r)))
			}
		})
	}
}

// mvSync is a dry run of "mv foo bar" that saved the file, then sync, whose
// mv returns syncRC.
func mvSync(syncRC string) string {
	bp := blacklistPath
	return kmodOpen + `h1 aug_mv "` + bp + `/foo" "` + bp + `/bar" => 0
h1 aug_save => 0
h1 aug_match "/augeas/events/saved" => 1 "/augeas/events/saved"
h1 aug_get "/augeas/events/saved" => 1 "` + bp + `"
h1 aug_match "/augeas/events/saved" => 1 "/augeas/events/saved"
h1 aug_get "/augeas/events/saved" => 1 "` + bp + `"
h1 aug_rm "/augeas` + bp + `/mtime" => 1
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_set "/augeas/save" "overwrite" => 0
h1 aug_load => 0
h1 aug_mv "` + bp + `/foo" "` + bp + `/bar" => ` + syncRC + `
`
}

// mv succeeds through sync; a command failing in sync is the returns
// property's failure, and the handle is still closed once.
func TestAugeasMvSync(t *testing.T) {
	ok := mvSync("0") + "h1 aug_save => 0\nh1 aug_close\n"
	lib := raltest.NewScriptLib(t, raltest.ParseTrace(t, t.Name(), ok))
	var unlinked []string
	a := newAugeas(lib, "")
	a.unlink = func(p string) error { unlinked = append(unlinked, p); return nil }
	r := runAugeasCase(t, nil, nil, func(*augeasRun) []Provider { return []Provider{a} },
		[]catalog.Resource{kmodRes("mv", "mv foo bar")}, false)[0]
	wantReturns(t, r, "executed successfully")
	if want := []string{blacklistConf + ".augnew"}; !reflect.DeepEqual(unlinked, want) {
		t.Errorf("unlinked %q, want %q", unlinked, want)
	}

	fails := mvSync("-1") + "h1 aug_close\n"
	lib = raltest.NewScriptLib(t, raltest.ParseTrace(t, t.Name()+"/fails", fails))
	a = newAugeas(lib, "")
	a.unlink = func(string) error { return nil }
	r = runAugeasCase(t, nil, nil, func(*augeasRun) []Provider { return []Provider{a} },
		[]catalog.Resource{kmodRes("mv", "mv foo bar")}, false)[0]
	s := `Error sending command 'mv' with params ["` + blacklistPath + `/foo", "` + blacklistPath + `/bar"]`
	if r.err != nil || len(r.changes) != 1 || r.changes[0].Err == nil || r.changes[0].Err.Error() != s+"/"+s ||
		r.changes[0].Was != "need_to_run" || r.changes[0].Is != 0 {
		t.Errorf("changes %+v err %v", r.changes, r.err)
	}
}

// Without incl, a context no glob narrows is not optimised, and load errors
// without restricted metadata are debug lines; a valueless child prints
// empty.
func TestAugeasUnoptimisedContextAndLoadErrors(t *testing.T) {
	restricted := `/augeas/load/*['/etc/x/' !~ glob(incl) + regexp('/.*')]`
	script := `h1 aug_init "/" "" 32
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_match "/augeas/version/pathx/functions/glob" => 1 "/augeas/version/pathx/functions/glob"
h1 aug_match "` + restricted + `" => 2 "/augeas/load/A" "/augeas/load/B"
h1 aug_match "/augeas/load/*" => 2 "/augeas/load/A" "/augeas/load/B"
h1 aug_load => 0
h1 aug_match "/augeas//error" => 1 "/augeas/files/etc/x/error"
h1 aug_get "/augeas/files/etc/x/error" => 1 "parse_failed"
h1 aug_match "/augeas/files/etc/x/error/*" => 1 "/augeas/files/etc/x/error/message"
h1 aug_get "/augeas/files/etc/x/error/message" => 1 NULL
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_set "/augeas/save" "newfile" => 0
h1 aug_set "/files/etc/x/a" "b" => 0
` + inSyncTail
	r := applyScript(t, script, "", raltest.AugRes("Augeas", "ctx", map[string]any{"context": "/files/etc/x/", "changes": "set a b"}), false)
	wantInSync(t, r)
	if len(r.warnings) != 0 {
		t.Errorf("warnings %+v", r.warnings)
	}
	want := []string{
		"Opening augeas with root /, lens path , flags 32",
		"Augeas version 1.15.0 is installed",
		"Unable to optimize files loaded by context path, no glob matches",
		"Loading failed for one or more files, output from /augeas//error:",
		"/augeas/files/etc/x/error = parse_failed",
		"/augeas/files/etc/x/error/message = ",
		"Will attempt to save and only run if files changed",
		`sending command 'set' with params ["/files/etc/x/a", "b"]`,
		"Skipping because no files were changed",
		"Closed the augeas connection",
	}
	if lines := r.debugAt(providerSource(r)); !reflect.DeepEqual(lines, want) {
		t.Errorf("debug lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// An explicit empty incl is still set (Ruby tests it for truth), so the flags
// are NO_MODL_AUTOLOAD and Xfm gets the empty strings.
func TestAugeasEmptyInclIsSet(t *testing.T) {
	script := `h1 aug_init "/" "" 64
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_match "/augeas/version/pathx/functions/glob" => 1 "/augeas/version/pathx/functions/glob"
h1 aug_set "/augeas/load/Xfm/lens" "" => 0
h1 aug_set "/augeas/load/Xfm/incl" "" => 0
h1 aug_load => 0
h1 aug_match "/augeas//error" => 0
h1 aug_get "/augeas/version" => 1 "1.15.0"
h1 aug_set "/augeas/save" "newfile" => 0
h1 aug_set "/files/a" "b" => 0
` + inSyncTail
	r := applyScript(t, script, "", raltest.AugRes("Augeas", "e", map[string]any{"incl": "", "lens": "", "changes": "set a b"}), false)
	wantInSync(t, r)
}

func TestAugeasTypeSurface(t *testing.T) {
	a := Augeas{}
	if a.Type() != "augeas" || *a.ProviderUsed() != "augeas" {
		t.Errorf("Type %q ProviderUsed %q", a.Type(), *a.ProviderUsed())
	}
	if got := a.AutoRequires(kmodRes("x", "set a b"), func(string) bool { return true }); len(got) != 0 {
		t.Errorf("AutoRequires %q", got)
	}
	want := []string{"changes", "context", "force", "incl", "lens", "load_path", "name", "onlyif", "provider", "returns", "root", "show_diff", "type_check"}
	if got := a.Attrs().Sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("Attrs %q, want %q", got, want)
	}
	if a.Refreshable(catalog.Resource{}) {
		t.Error("Refreshable is true")
	}
}
