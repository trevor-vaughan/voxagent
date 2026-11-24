package augeasprovidersssh

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
	"github.com/voxagent/voxagent/internal/units/augeasproviders_core/augeastest"
)

var _ ral.Provider = SshConfig{}

const sshConfigFile = "/etc/ssh/ssh_config"

// gateLoadpath is the SIMP libdir's lens directory of the 07* recordings.
const gateLoadpath = augeastest.CaseLibdir + "/augeas/lenses"

func sshConfigOver(run *core.Run) []ral.Provider { return []ral.Provider{SshConfig{run: run}} }

// sshConfigRes is a measured Ssh_config: the resource default's target, key
// and host "*", and the given value and ensure (nil leaves them unset).
func sshConfigRes(key string, value any, ensure any) catalog.Resource {
	params := map[string]any{"target": sshConfigFile, "key": key, "host": "*"}
	if value != nil {
		params["value"] = value
	}
	if ensure != nil {
		params["ensure"] = ensure
	}
	return raltest.AugRes("Ssh_config", "zzzz98_st____"+key, params)
}

func wantEnsureChange(t *testing.T, r augeastest.CaseResult, was, is, name, message string) {
	t.Helper()
	if len(r.Changes) != 1 {
		t.Fatalf("%s: changes %+v", r.Ref, r.Changes)
	}
	ch := r.Changes[0]
	if ch.Property != "ensure" || ch.Was != was || ch.Is != is || ch.Name != name || ch.Message != message || ch.Err != nil {
		t.Errorf("%s: change = %+v", r.Ref, ch)
	}
}

func wantValueChange(t *testing.T, r augeastest.CaseResult, was, is []any, message string) {
	t.Helper()
	if len(r.Changes) != 1 {
		t.Fatalf("%s: changes %+v", r.Ref, r.Changes)
	}
	ch := r.Changes[0]
	if ch.Property != "value" || !reflect.DeepEqual(ch.Was, was) || !reflect.DeepEqual(ch.Is, is) || ch.Name != "" || ch.Message != message || ch.Err != nil {
		t.Errorf("%s: change = %+v", r.Ref, ch)
	}
}

// M11: three keys new to the stock file are created under Host *.
func TestSshConfigReplays02a(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "02a"), []string{sshConfigFile}, sshConfigOver, []catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("GSSAPIAuthentication", "yes", nil),
		sshConfigRes("ServerAliveInterval", "300", nil),
	}, false)
	augeastest.WantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "ssh_config_created", "created")
	}
}

func sshConfig02b() []catalog.Resource {
	return []catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("ForwardX11", "yes", nil),
		sshConfigRes("GSSAPIAuthentication", "yes", nil),
	}
}

// M12: a changed key, one already in sync, one created.
func TestSshConfigReplays02b(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "02b"), []string{sshConfigFile}, sshConfigOver, sshConfig02b(), false)
	augeastest.WantNoErrors(t, got)
	augeastest.WantMessages(t, got, []string{"value changed ['yes'] to ['no']"}, nil, []string{"created"})
	wantValueChange(t, got[0], []any{"yes"}, []any{"no"}, "value changed ['yes'] to ['no']")
	wantEnsureChange(t, got[2], "absent", "present", "ssh_config_created", "created")
}

// 02b under noop: the retrieves only, no value=, create or flush.
func TestSshConfigReplays02bNoop(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := raltest.TraceLines(t, "02b", [2]int{4, 22}, [2]int{43, 65}, [2]int{86, 86})
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, sshConfig02b(), true)
	augeastest.WantNoErrors(t, got)
	augeastest.WantMessages(t, got, []string{"value changed ['yes'] to ['no'] (noop)"}, nil, []string{"created (noop)"})
	wantValueChange(t, got[0], []any{"yes"}, []any{"no"}, "value changed ['yes'] to ['no'] (noop)")
	wantEnsureChange(t, got[2], "absent", "present", "ssh_config_created", "created (noop)")
}

var arrayChanges = [][]string{
	{"value changed ['aes128-ctr'] to ['aes256-ctr', 'aes128-ctr']"},
	{"value changed ['hmac-sha2-512', 'hmac-sha1', 'hmac-md5'] to ['hmac-sha2-512', 'hmac-sha2-256']"},
	{"value changed ['LANG'] to ['LANG', 'LC_*']"},
}

func sshConfigArrays() []catalog.Resource {
	return []catalog.Resource{
		sshConfigRes("Ciphers", []any{"aes256-ctr", "aes128-ctr"}, nil),
		sshConfigRes("MACs", []any{"hmac-sha2-512", "hmac-sha2-256"}, nil),
		sshConfigRes("SendEnv", []any{"LANG", "LC_*"}, nil),
	}
}

// M15: array keys (parsed_as? true) are rewritten in place.
func TestSshConfigReplays02e(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "02e"), []string{sshConfigFile}, sshConfigOver, sshConfigArrays(), false)
	augeastest.WantNoErrors(t, got)
	augeastest.WantMessages(t, got, arrayChanges...)
	wantValueChange(t, got[1], []any{"hmac-sha2-512", "hmac-sha1", "hmac-md5"}, []any{"hmac-sha2-512", "hmac-sha2-256"}, arrayChanges[1][0])
}

// M16, M17: ensure absent removes a key with or without a value; an absent
// key has nothing to manage.
func TestSshConfigReplays02f(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "02f"), []string{sshConfigFile}, sshConfigOver, []catalog.Resource{
		sshConfigRes("CompressionLevel", "6", "absent"),
		sshConfigRes("ForwardX11", nil, "absent"),
		sshConfigRes("Nothere", nil, "absent"),
	}, false)
	augeastest.WantNoErrors(t, got)
	wantEnsureChange(t, got[0], "present", "absent", "ssh_config_removed", "removed")
	wantEnsureChange(t, got[1], "present", "absent", "ssh_config_removed", "removed")
	if len(got[2].Changes) != 0 {
		t.Errorf("Nothere: changes %+v", got[2].Changes)
	}
	src := "/Stage[main]/Main/" + got[2].Ref
	if d := got[2].DebugAt(src); !slices.Equal(d, []string{"Nothing to manage: no ensure and the resource doesn't exist"}) {
		t.Errorf("Nothere debug = %q", d)
	}
	if d := got[0].DebugAt("/Stage[main]/Main/" + got[0].Ref); len(d) != 0 {
		t.Errorf("CompressionLevel debug = %q", d)
	}
}

// M61 (07a): with the stock lens only, a change and an array key created.
// The trace's sshd_config and subsystem calls are Tasks 11-12's.
func TestSshConfigReplays07a(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := raltest.TraceLines(t, "07a", [2]int{4, 68}, [2]int{145, 145})
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, []catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("Ciphers", []any{"aes256-ctr", "aes128-ctr"}, nil),
	}, false)
	augeastest.WantNoErrors(t, got)
	augeastest.WantMessages(t, got, []string{"value changed ['yes'] to ['no']"}, []string{"created"})
	wantEnsureChange(t, got[1], "absent", "present", "ssh_config_created", "created")
}

// M62 (07b): SIMP's ssh.aug cannot parse the file, so every resource fails
// its exists? with the load error and changes nothing.
func TestSshConfigReplays07b(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := raltest.TraceLines(t, "07b", [2]int{4, 20}, [2]int{97, 97})
	got := augeastest.RunAugeasCase(t, calls, []string{gateLoadpath, sshConfigFile}, sshConfigOver, []catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("Ciphers", []any{"aes256-ctr", "aes128-ctr"}, nil),
	}, false)
	want := "Augeas didn't load /etc/ssh/ssh_config with Ssh.lns from /tmp/gatelib/augeas/lenses: Get did not match entire input (line:3, character:0)"
	for _, r := range got {
		if r.Err == nil || r.Err.Error() != want {
			t.Errorf("%s: err = %v\nwant %s", r.Ref, r.Err, want)
		}
		var fe *ral.FlushError
		if errors.As(r.Err, &fe) {
			t.Errorf("%s: err is a FlushError", r.Ref)
		}
		if len(r.Changes) != 0 {
			t.Errorf("%s: changes %+v", r.Ref, r.Changes)
		}
	}
}

// 07f: SIMP's lens loads the stock file; plain and array keys are created,
// parsed_as? still asking the stock lens.
func TestSshConfigReplays07f(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	rs := append([]catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("GSSAPIAuthentication", "yes", nil),
		sshConfigRes("ServerAliveInterval", "300", nil),
	}, sshConfigArrays()...)
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "07f"), []string{gateLoadpath, sshConfigFile}, sshConfigOver, rs, false)
	augeastest.WantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "ssh_config_created", "created")
	}
}

// 07h: SIMP's lens, the array changes of 02e.
func TestSshConfigReplays07h(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	got := augeastest.RunAugeasCase(t, raltest.LoadTrace(t, "07h"), []string{gateLoadpath, sshConfigFile}, sshConfigOver, sshConfigArrays(), false)
	augeastest.WantNoErrors(t, got)
	augeastest.WantMessages(t, got, arrayChanges...)
}

// A raise inside value= (here parsed_as?'s aug_init) fails the property
// with Ruby's is_to_s/should_to_s, and nothing is flushed.
func TestSshConfigValueSyncFailure(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := append(raltest.TraceLines(t, "02b", [2]int{4, 28}), raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\nh1 aug_close\n")...)
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, sshConfig02b()[:1], false)
	if got[0].Err != nil || len(got[0].Changes) != 1 {
		t.Fatalf("result = %+v", got[0])
	}
	ch := got[0].Changes[0]
	if ch.Err == nil || ch.Err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Errorf("change err = %v", ch.Err)
	}
	if !ch.DisplayVerbatim || ch.WasDisplay != "['yes']" || ch.IsDisplay != "['no']" {
		t.Errorf("display = %v %q %q", ch.DisplayVerbatim, ch.WasDisplay, ch.IsDisplay)
	}
}

// A raise inside create fails ensure, and nothing is flushed.
func TestSshConfigCreateFailure(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := append(raltest.TraceLines(t, "02a", [2]int{4, 20}), raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\nh1 aug_close\n")...)
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, []catalog.Resource{sshConfigRes("Compression", "no", nil)}, false)
	if got[0].Err != nil || len(got[0].Changes) != 1 {
		t.Fatalf("result = %+v", got[0])
	}
	ch := got[0].Changes[0]
	if ch.Property != "ensure" || ch.Err == nil || ch.Err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Errorf("change = %+v", ch)
	}
}

// A failed save is the flush's FlushError beside the success change (S1).
func TestSshConfigFlushFailure(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{"Ssh_config": true})
	calls := append(raltest.TraceLines(t, "02f", [2]int{4, 29}), raltest.ParseTrace(t, "script", `h1 aug_save => -1
h1 aug_match "/augeas//error" => 0
h1 aug_load => 0
h1 aug_load => 0
h1 aug_close
`)...)
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, []catalog.Resource{sshConfigRes("CompressionLevel", "6", "absent")}, false)
	var fe *ral.FlushError
	if !errors.As(got[0].Err, &fe) || fe.Error() != "Failed to save Augeas tree to file. See debug logs for details." {
		t.Errorf("err = %v", got[0].Err)
	}
	wantEnsureChange(t, got[0], "present", "absent", "ssh_config_removed", "removed")
}

// A key node without a value (RemoteForward's) reads as nil, shown undef,
// and never matches a should.
func TestSshConfigNilValue(t *testing.T) {
	augeastest.WithVersionMemo(t, map[string]bool{})
	calls := append(raltest.TraceLines(t, "02b", [2]int{4, 20}), raltest.ParseTrace(t, "script", `h1 aug_match "/files/etc/ssh/ssh_config/Host[1]/Compression/*[label()=~regexp('[0-9]*')]" => 0
h1 aug_get "/files/etc/ssh/ssh_config/Host[1]/Compression" => 1 NULL
h1 aug_close
`)...)
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, sshConfig02b()[:1], true)
	augeastest.WantNoErrors(t, got)
	wantValueChange(t, got[0], []any{nil}, []any{"no"}, "value changed [undef] to ['no'] (noop)")
}

// An ensure-less resource without a value manages nothing: absent, a debug
// line; present, no change.
func TestSshConfigNoEnsureNoValue(t *testing.T) {
	calls := append(raltest.TraceLines(t, "02f", [2]int{4, 13}), raltest.ParseTrace(t, "script", `h1 aug_match "/augeas/load/Ssh" => 1 "/augeas/load/Ssh"
h1 aug_match "/augeas/load/Ssh/incl[.='/etc/ssh/ssh_config']" => 1 "/augeas/load/Ssh/incl"
h1 aug_match "/files/etc/ssh/ssh_config" => 1 "/files/etc/ssh/ssh_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/ssh_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/ssh_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/Host[.='*']/*[label()=~regexp('Nothere', 'i')]" => 0
h1 aug_match "$resource" => 0
h1 aug_close
`)...)
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, []catalog.Resource{
		sshConfigRes("CompressionLevel", nil, nil),
		sshConfigRes("Nothere", nil, nil),
	}, false)
	augeastest.WantNoErrors(t, got)
	if len(got[0].Changes)+len(got[1].Changes) != 0 || len(got[0].Logs) != 0 {
		t.Errorf("results = %+v", got)
	}
	if d := got[1].DebugAt("/Stage[main]/Main/" + got[1].Ref); !slices.Equal(d, []string{"Nothing to manage: no ensure and the resource doesn't exist"}) {
		t.Errorf("debug = %q", d)
	}
}

// The default target is /etc/ssh/ssh_config and host defaults to "*".
func TestSshConfigDefaults(t *testing.T) {
	calls := raltest.TraceLines(t, "02f", [2]int{4, 22}, [2]int{60, 60})
	got := augeastest.RunAugeasCase(t, calls, []string{sshConfigFile}, sshConfigOver, []catalog.Resource{
		raltest.AugRes("Ssh_config", "CompressionLevel", map[string]any{"value": "6"}),
	}, false)
	augeastest.WantNoErrors(t, got)
	if len(got[0].Changes) != 0 {
		t.Errorf("changes = %+v", got[0].Changes)
	}
}

func TestSshConfigRefreshAndPrefetch(t *testing.T) {
	s := SshConfig{}
	if ok, err := s.Refresh(t.Context(), catalog.Resource{}, false); ok || err != nil {
		t.Errorf("Refresh = %v, %v", ok, err)
	}
	p, err := s.Prefetch(t.Context(), nil)
	if err != nil || p != ral.Provider(s) {
		t.Errorf("Prefetch = %v, %v", p, err)
	}
}
