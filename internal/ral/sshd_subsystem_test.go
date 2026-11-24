package ral

import (
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

var _ Provider = SshdConfigSubsystem{}

func subsystemOver(run *augeasRun) []Provider { return []Provider{SshdConfigSubsystem{run: run}} }

// subsysRes is a measured Sshd_config_subsystem: the default target and the
// given parameters.
func subsysRes(title string, params map[string]any) catalog.Resource {
	return augRes("Sshd_config_subsystem", title, params)
}

// wantCommandChange checks r's single change is a command change.
func wantCommandChange(t *testing.T, r caseResult, was any, is, message string) {
	t.Helper()
	if len(r.changes) != 1 {
		t.Fatalf("%s: changes %+v", r.ref, r.changes)
	}
	ch := r.changes[0]
	if ch.Property != "command" || ch.Was != was || ch.Is != is || ch.Name != "command_changed" || ch.Message != message || ch.Err != nil {
		t.Errorf("%s: change = %+v", r.ref, ch)
	}
}

// M30: a subsystem new to a file without Match blocks is set under a new
// Subsystem entry.
func TestSubsystemReplays04a(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "04a"), []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
	}, false)
	wantNoErrors(t, got)
	wantEnsureChange(t, got[0], "absent", "present", "sshd_config_subsystem_created", "created")
}

// M31: an existing subsystem's command is read and set through $resource.
func TestSubsystemReplays04b(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "04b"), []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"command": "internal-sftp -l INFO"}),
	}, false)
	wantNoErrors(t, got)
	wantCommandChange(t, got[0], "/usr/libexec/openssh/sftp-server", "internal-sftp -l INFO",
		"command changed '/usr/libexec/openssh/sftp-server' to 'internal-sftp -l INFO'")
}

// 04b under noop: exists? and the command getter only, nothing set or saved.
func TestSubsystemReplays04bNoop(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	calls := traceLines(t, "04b", [2]int{4, 20}, [2]int{34, 34})
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"command": "internal-sftp -l INFO"}),
	}, true)
	wantNoErrors(t, got)
	wantCommandChange(t, got[0], "/usr/libexec/openssh/sftp-server", "internal-sftp -l INFO",
		"command changed '/usr/libexec/openssh/sftp-server' to 'internal-sftp -l INFO' (noop)")
}

// A valueless (or ambiguous) $resource reads nil, shown undef; Ruby's quoting
// applies to the should.
func TestSubsystemCommandFromNil(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	calls := append(traceLines(t, "04b", [2]int{4, 19}), parseTrace(t, "script", `h1 aug_get "$resource" => 1 NULL
h1 aug_close
`)...)
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"command": "it's"}),
	}, true)
	wantNoErrors(t, got)
	wantCommandChange(t, got[0], nil, "it's", `command changed undef to 'it\'s' (noop)`)
}

// M33: with Match blocks the Subsystem entry is inserted before the first
// one and its name cleared before the command is set; the second resource
// shares the class handle and the version memo.
func TestSubsystemReplays04d(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "04d"), []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
		subsysRes("other", map[string]any{"command": "/bin/other"}),
	}, false)
	wantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "sshd_config_subsystem_created", "created")
	}
}

// M34: destroy removes every Subsystem entry holding the name.
func TestSubsystemReplays04e(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "04e"), []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{"ensure": "absent"}),
	}, false)
	wantNoErrors(t, got)
	wantEnsureChange(t, got[0], "present", "absent", "sshd_config_subsystem_removed", "removed")
}

// An absent subsystem with no ensure is not managed, with Ruby's debug line.
func TestSubsystemAbsentNotManaged(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, traceLines(t, "04a", [2]int{4, 13}, [2]int{28, 28}), []string{sshdConfigFile}, subsystemOver, []catalog.Resource{
		subsysRes("sftp", map[string]any{}),
	}, false)
	wantNoErrors(t, got)
	if len(got[0].changes) != 0 {
		t.Errorf("changes = %+v", got[0].changes)
	}
	want := []string{"Nothing to manage: no ensure and the resource doesn't exist"}
	if d := got[0].debugAt("/Stage[main]/Main/" + got[0].ref); !slices.Equal(d, want) {
		t.Errorf("debug = %q, want %q", d, want)
	}
}

// relabel is trace text with each line's handle renamed by names (e.g.
// "h1" to "h9"), for a recording replayed inside another run.
func relabel(text string, names map[string]string) string {
	lines := strings.SplitAfter(text, "\n")
	for i, l := range lines {
		h, rest, ok := strings.Cut(l, " ")
		if n, found := names[h]; ok && found {
			lines[i] = n + " " + rest
		}
	}
	return strings.Join(lines, "")
}

// Sshd_config and Sshd_config_subsystem edit one file through two class
// handles (CORE:64-66): each loads the file on its own handle, and each save
// reloads only its own. 07a's PermitRootLogin (h5), 04a's subsystem create
// relabeled onto h9 with its version handle h10, then 07a's AllowUsers back on
// h5, which never sees the subsystem's save.
func TestSubsystemAndSshdConfigUseSeparateHandles(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	sub := strings.SplitAfter(readTraceFile(t, "04a"), "\n")[3:27]
	calls := traceLines(t, "07a", [2]int{69, 100})
	calls = append(calls, parseTrace(t, "04a-relabeled", relabel(strings.Join(sub, ""), map[string]string{"h1": "h9", "h2": "h10"}))...)
	calls = append(calls, traceLines(t, "07a", [2]int{101, 127})...)
	calls = append(calls, parseTrace(t, "end", "h5 aug_close\nh9 aug_close\n")...)
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, func(run *augeasRun) []Provider {
		return []Provider{SshdConfig{run: run}, SshdConfigSubsystem{run: run}}
	}, []catalog.Resource{
		sshdRes("PermitRootLogin", map[string]any{"value": "no"}),
		subsysRes("sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
		sshdRes("AllowUsers", map[string]any{"value": []any{"alice", "bob"}}),
	}, false)
	wantNoErrors(t, got)
	wantEnsureChange(t, got[0], "absent", "present", "sshd_config_created", "created")
	wantEnsureChange(t, got[1], "absent", "present", "sshd_config_subsystem_created", "created")
	wantEnsureChange(t, got[2], "absent", "present", "sshd_config_created", "created")
}

// sshTypesOver is the three augeasproviders_ssh types over one run.
func sshTypesOver(run *augeasRun) []Provider {
	return []Provider{SshConfig{run: run}, SshdConfig{run: run}, SshdConfigSubsystem{run: run}}
}

// sshTypes07 is the 07* catalog: the three types, each with its own handle.
func sshTypes07() []catalog.Resource {
	return []catalog.Resource{
		sshConfigRes("Compression", "no", nil),
		sshConfigRes("Ciphers", []any{"aes256-ctr", "aes128-ctr"}, nil),
		sshdRes("PermitRootLogin", map[string]any{"value": "no"}),
		sshdRes("AllowUsers", map[string]any{"value": []any{"alice", "bob"}}),
		subsysRes("sftp", map[string]any{"command": "/usr/libexec/openssh/sftp-server"}),
	}
}

// M61 (07a), whole: the stock lens only; ssh_config changes and creates,
// sshd_config creates two keys, the subsystem is in sync; each class's handle
// is closed at the run's end in open order.
func TestReplays07a(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "07a"), []string{sshConfigFile, sshdConfigFile}, sshTypesOver, sshTypes07(), false)
	wantNoErrors(t, got)
	wantMessages(t, got,
		[]string{"value changed ['yes'] to ['no']"},
		[]string{"created"}, []string{"created"}, []string{"created"},
		nil)
	wantEnsureChange(t, got[1], "absent", "present", "ssh_config_created", "created")
	wantEnsureChange(t, got[2], "absent", "present", "sshd_config_created", "created")
	wantEnsureChange(t, got[3], "absent", "present", "sshd_config_created", "created")
}

// M62 (07b), whole: SIMP's lens directory is the loadpath. ssh_config fails
// to load its file on every resource; sshd_config and the subsystem, on
// their own handles, are unaffected.
func TestReplays07b(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, loadTrace(t, "07b"), []string{gateLoadpath, sshConfigFile, sshdConfigFile}, sshTypesOver, sshTypes07(), false)
	want := "Augeas didn't load /etc/ssh/ssh_config with Ssh.lns from /tmp/gatelib/augeas/lenses: Get did not match entire input (line:3, character:0)"
	for _, r := range got[:2] {
		if r.err == nil || r.err.Error() != want || len(r.changes) != 0 {
			t.Errorf("%s: err = %v, changes %+v\nwant %s", r.ref, r.err, r.changes, want)
		}
	}
	wantNoErrors(t, got[2:])
	wantEnsureChange(t, got[2], "absent", "present", "sshd_config_created", "created")
	wantEnsureChange(t, got[3], "absent", "present", "sshd_config_created", "created")
	if len(got[4].changes) != 0 {
		t.Errorf("%s: changes %+v", got[4].ref, got[4].changes)
	}
}

func TestSubsystemRefreshAndPrefetch(t *testing.T) {
	s := SshdConfigSubsystem{}
	p, err := s.Prefetch(t.Context(), nil)
	if err != nil || p != Provider(s) {
		t.Errorf("Prefetch = %v, %v", p, err)
	}
	if ok, err := s.Refresh(t.Context(), catalog.Resource{}, false); ok || err != nil {
		t.Errorf("Refresh = %v, %v", ok, err)
	}
}
