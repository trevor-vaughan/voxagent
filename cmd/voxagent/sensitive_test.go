package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/units/concat"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
)

// markedResource returns a resource marking the given attributes sensitive.
func markedResource(typ, title string, params map[string]any, marked ...string) map[string]any {
	names := make([]any, len(marked))
	for i, m := range marked {
		names[i] = m
	}
	return map[string]any{
		"type": typ, "title": title, "tags": []any{},
		"parameters": params, "sensitive_parameters": names,
	}
}

// leakCase is one catalog run under every mode, so that a pre-flight refusal in
// one does not hide the others. Marked attributes hold "D8SECRET-<type>-<attr>".
type leakCase struct {
	name string
	// refused cases stop before any report: pre-flight refuses the catalog
	// (exit 2, "could-not-run"). Every other case must apply and report.
	refused bool
	// agentOnly cases need the fake server: they fetch a puppet: source.
	agentOnly bool
	// rootOnly cases change ownership, which needs privilege.
	rootOnly bool
	// fakeShadow cases apply a native User. They run the fakeShadowBinary
	// build, whose shadow-utils are fakes, so no host account is touched, and
	// are skipped where user is not native.
	fakeShadow bool
	// fakeCrontab cases apply a native Cron with a fake crontab first on PATH
	// (fakeCrontab), so no host crontab is read or changed; they are skipped
	// where cron is not native.
	fakeCrontab bool
	// seed prepares pre-existing state under dir before the run.
	seed      func(t *testing.T, dir string)
	resources func(dir string) []map[string]any
	// plain, when set, lists values of marked properties that carry no
	// D8SECRET marker (a mode, an owner), which must not appear either.
	plain []string
	// concat, when set, runs with a libdir holding that puppetlabs-concat
	// version, so Concat_file and Concat_fragment are native. Not in agent
	// mode: pluginsync purges the libdir to the fake server's empty plugins.
	concat concat.ConcatVersion
	// inifile, when set, runs with a libdir holding that puppetlabs-inifile
	// release, so the Ini_* types are native. Not in agent mode, for the
	// reason concat is not.
	inifile string
}

// inifileModuleLib is the testdata lib of one puppetlabs-inifile release.
func inifileModuleLib(t *testing.T, release string) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository")
	}
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(here))), "internal", "units", "inifile", "testdata", release, "lib")
}

func writeSeed(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func leakCases() []leakCase {
	return []leakCase{
		{name: "notify", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Notify", "n1", map[string]any{"message": "D8SECRET-Notify-message"}, "message"),
			}
		}},
		{name: "file-content", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "c.txt"), map[string]any{"ensure": "file", "content": "D8SECRET-File-content"}, "content"),
				// The missing parent directory makes the create fail.
				markedResource("File", filepath.Join(dir, "c-fail/c.txt"), map[string]any{"ensure": "file", "content": "D8SECRET-File-content-fail"}, "content"),
			}
		}},
		{name: "file-content-change", // existing file, different content: the content change message runs
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "cc.txt"), "D8SECRET-File-content-old\n", 0o644)
			},
			plain: []string{"D8SECRET-File-content-old"},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "cc.txt"), map[string]any{"ensure": "file", "content": "D8SECRET-File-content-new\n"}, "content"),
				}
			}},
		{name: "file-content-absent", // marked content with ensure absent on an existing file
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "ca.txt"), "x", 0o644)
			},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "ca.txt"), map[string]any{"ensure": "absent", "content": "D8SECRET-File-content-absent"}, "content"),
				}
			}},
		{name: "file-source", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "s.txt"), map[string]any{"ensure": "file", "source": "/nonexistent/D8SECRET-File-source"}, "source"),
			}
		}},
		{name: "file-source-puppet", agentOnly: true, // a marked source the agent downloads: its debug lines and HTTP lines
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "p.txt"), map[string]any{"ensure": "file", "source": "puppet:///" + secretSourceMount}, "source"),
				}
			}},
		{name: "file-source-recurse", // children are generated from a marked source directory and walked
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "D8SECRET-File-srcdir", "f.txt"), "body\n", 0o644)
				writeSeed(t, filepath.Join(dir, "D8SECRET-File-srcdir", "sub", "g.txt"), "body2\n", 0o644)
			},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "tree"), map[string]any{
						"ensure": "directory", "recurse": true,
						"source": filepath.Join(dir, "D8SECRET-File-srcdir"),
					}, "source"),
				}
			}},
		{name: "file-source-recurse-fail", // the same walk against an unreadable source
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "tree"), map[string]any{
						"ensure": "directory", "recurse": true,
						"source": "/nonexistent/D8SECRET-File-srcdir2",
					}, "source"),
				}
			}},
		{name: "file-owner", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "o.txt"), map[string]any{"ensure": "file", "content": "x", "owner": "D8SECRET-File-owner"}, "owner"),
			}
		}},
		{name: "file-group", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "g.txt"), map[string]any{"ensure": "file", "content": "x", "group": "D8SECRET-File-group"}, "group"),
			}
		}},
		{name: "file-owner-group-change", rootOnly: true, // valid, different owner and group: the change messages run
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "og.txt"), "x", 0o644)
			},
			plain: []string{"daemon"},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "og.txt"), map[string]any{"ensure": "file", "owner": "daemon", "group": "daemon"}, "owner", "group"),
				}
			}},
		{name: "file-mode-invalid", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "m.txt"), map[string]any{"ensure": "file", "content": "x", "mode": "D8SECRET-File-mode"}, "mode"),
			}
		}, refused: true},
		{name: "file-mode-change", // existing file, different mode: the mode change message runs
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "mc.txt"), "x", 0o604)
			},
			plain: []string{`'0751'`, `"0751"`, `'0604'`, `"0604"`}, // quoted: a bare 0751 can match a timing digit run
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "mc.txt"), map[string]any{"ensure": "file", "mode": "0751"}, "mode"),
				}
			}},
		{name: "file-ensure", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "e.txt"), map[string]any{"ensure": "D8SECRET-File-ensure"}, "ensure"),
			}
		}},
		{name: "file-target", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "l"), map[string]any{"ensure": "link", "target": "/nonexistent/D8SECRET-File-target"}, "target"),
				markedResource("File", filepath.Join(dir, "l2"), map[string]any{"ensure": "link", "target": filepath.Join(dir, "l2-target-D8SECRET-File-target2")}, "target"),
			}
		}},
		{name: "exec",
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "fail.sh"), "#!/bin/sh\necho \"$1\"\nexit 1\n", 0o755)
			},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("Exec", "e-ok", map[string]any{
						"command": "/bin/echo D8SECRET-Exec-command", "logoutput": true,
						"unless": "/bin/false D8SECRET-Exec-unless", "onlyif": "/bin/true D8SECRET-Exec-onlyif",
					}, "command", "unless", "onlyif"),
					markedResource("Exec", "e-fail", map[string]any{
						"command": "/bin/false D8SECRET-Exec-failcmd", "logoutput": "on_failure",
					}, "command"),
					// A failing marked command that echoes its own marker: the
					// failure path must log "[output redacted]".
					markedResource("Exec", "e-fail-echo", map[string]any{
						"command": filepath.Join(dir, "fail.sh") + " D8SECRET-Exec-failecho", "logoutput": "on_failure",
					}, "command"),
					markedResource("Exec", "e-missing", map[string]any{
						"command": "/nonexistent/D8SECRET-Exec-missing",
					}, "command"),
					markedResource("Exec", "e-unless-only", map[string]any{
						"command": "/bin/echo plain", "unless": "/bin/false D8SECRET-Exec-unless2", "logoutput": true,
					}, "unless"),
					markedResource("Exec", "e-cwd", map[string]any{
						"command": "/bin/true", "cwd": "/D8SECRET-Exec-cwd",
					}, "cwd"),
					markedResource("Exec", "e-path", map[string]any{
						"command": "true", "path": "/D8SECRET-Exec-path:/bin",
					}, "path"),
				}
			}},
		// Plain parameters the catalog lists as marked (D-78): a creates path in
		// the debug line, a backup bucket in the failure, and a validate_cmd in
		// the failure and the Executing line. tries, try_sleep and returns
		// cannot be driven here: a marker is a string and these hold numbers.
		{name: "exec-creates", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Exec", "e-creates", map[string]any{
					"command": "/bin/true", "creates": "/nonexistent/D8SECRET-Exec-creates",
				}, "creates"),
			}
		}},
		{name: "file-backup",
			seed: func(t *testing.T, dir string) {
				writeSeed(t, filepath.Join(dir, "b.txt"), "old\n", 0o644)
			},
			resources: func(dir string) []map[string]any {
				return []map[string]any{
					markedResource("File", filepath.Join(dir, "b.txt"), map[string]any{"ensure": "file", "content": "new\n", "backup": "D8SECRET-File-backup"}, "backup"),
				}
			}},
		{name: "file-validate_cmd", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("File", filepath.Join(dir, "v.txt"), map[string]any{
					"ensure": "file", "content": "x\n", "validate_cmd": "/bin/false D8SECRET-File-validate_cmd %",
				}, "validate_cmd"),
			}
		}},
		// Custom service commands: like "service", only the refusal is reachable
		// where PID 1 is not systemd.
		{name: "service-custom-commands", refused: pid1IsNotSystemd(), resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Service", "voxagent-no-such-service3", map[string]any{
					"ensure": "running", "status": "/bin/false D8SECRET-Service-status", "start": "/bin/false D8SECRET-Service-start",
				}, "status", "start"),
			}
		}},
		// Package: dnf needs root, so every operation fails; the failure and its
		// message must not carry the marked version.
		{name: "package", resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Package", "voxagent-no-such-package", map[string]any{"ensure": "D8SECRET-Package-ensure"}, "ensure"),
				markedResource("Package", "voxagent-no-such-package2", map[string]any{"ensure": "1.D8SECRET-Package-ensure2"}, "ensure"),
			}
		}},
		// Service: Service.Validate refuses the catalog when PID 1 is not systemd, so
		// the provider's Apply never runs there and this case only proves the
		// refusal text is clean. On a systemd host the catalog applies instead. The
		// provider's own redaction is covered by internal/ral tests.
		{name: "service", refused: pid1IsNotSystemd(), resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Service", "voxagent-no-such-service", map[string]any{"ensure": "running", "enable": true}, "ensure", "enable"),
			}
		}},
		// A Sensitive password on a create the fake useradd accepts and on one
		// it refuses, and an unmarked one on an existing user: password is
		// sensitive whether or not the catalog marks it (type/user.rb:280), so
		// that plain string must not appear either.
		{name: "user-password", fakeShadow: true, resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("User", "voxagent-leak-ok", map[string]any{"password": "D8SECRET-User-password-create", "comment": "c"}, "password"),
				markedResource("User", "voxagent-leak-fail", map[string]any{"password": "D8SECRET-User-password-create-fail", "comment": "c"}, "password"),
				{"type": "User", "title": "nobody", "tags": []any{},
					"parameters": map[string]any{"password": "D8SECRET-User-password-change"}},
			}
		}},
		// A Sensitive comment, which is not password: a create the fake useradd
		// accepts, one it refuses, and a modify the fake usermod refuses. Every
		// create command and the comment's own usermod are redacted
		// (useradd.rb:311-318, nameservice.rb:162 and :270).
		{name: "user-comment", fakeShadow: true, resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("User", "voxagent-leak-ok2", map[string]any{"comment": "D8SECRET-User-comment-create"}, "comment"),
				markedResource("User", "voxagent-leak-fail2", map[string]any{"comment": "D8SECRET-User-comment-create-fail"}, "comment"),
				markedResource("User", "nobody", map[string]any{"comment": "D8SECRET-User-comment-change"}, "comment"),
			}
		}},
		// A Sensitive command: the fake crontab echoes the written crontab, so
		// the D-95 copy of a write's output to stderr carries the marker.
		{name: "cron-command", fakeCrontab: true, resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Cron", "voxagent-leak-cron", map[string]any{"command": "/bin/true D8SECRET-Cron-command", "minute": "5"}, "command"),
			}
		}},
		// A Sensitive key on a change of an existing known_hosts line (whose old
		// key, unmarked, must not show either: plain) and on a create: the change logs
		// "changed [redacted] to [redacted]" and the create shows no value.
		{name: "sshkey-key", seed: func(t *testing.T, dir string) {
			writeSeed(t, filepath.Join(dir, "known_hosts"), "voxagent-leak-sshkey ssh-rsa voxagent-plain-old-sshkey-key\n", 0o644)
		}, plain: []string{"voxagent-plain-old-sshkey-key"}, resources: func(dir string) []map[string]any {
			target := filepath.Join(dir, "known_hosts")
			return []map[string]any{
				markedResource("Sshkey", "voxagent-leak-sshkey@ssh-rsa", map[string]any{"key": "D8SECRET-Sshkey-key", "target": target}, "key"),
				markedResource("Sshkey", "voxagent-leak-sshkey2@ssh-rsa", map[string]any{"key": "D8SECRET-Sshkey-key-create", "target": target}, "key"),
			}
		}},
		// A Sensitive ssh_authorized_key key on a change of an existing line (the
		// old key, unmarked, must not show either) and on a create. The key's
		// user is the agent's own, so the write is in-process (spec §3.4).
		{name: "sak-key", seed: func(t *testing.T, dir string) {
			writeSeed(t, filepath.Join(dir, "authorized_keys"), "ssh-rsa voxagent-plain-old-sak-key voxagent-leak-sak\n", 0o600)
		}, plain: []string{"voxagent-plain-old-sak-key"}, resources: func(dir string) []map[string]any {
			target := filepath.Join(dir, "authorized_keys")
			me := "root"
			if u, err := user.Current(); err == nil {
				me = u.Username
			}
			return []map[string]any{
				markedResource("Ssh_authorized_key", "voxagent-leak-sak", map[string]any{"user": me, "type": "ssh-rsa", "key": "D8SECRET-Ssh_authorized_key-key", "target": target}, "key"),
				markedResource("Ssh_authorized_key", "voxagent-leak-sak2", map[string]any{"user": me, "type": "ssh-rsa", "key": "D8SECRET-Ssh_authorized_key-key-create", "target": target}, "key"),
			}
		}},
		// A Sensitive fragment under each measured concat version, in a file
		// that applies and in one whose validate_cmd fails after printing the
		// file it validated, so the content reaches the validator's output.
		{name: "concat-9.1.0", concat: concat.Concat910, seed: seedCatFail, resources: concatLeakResources},
		{name: "concat-10.0.1", concat: concat.Concat1001, seed: seedCatFail, resources: concatLeakResources},
		// A Sensitive ini_setting value on a change of an existing line (whose old
		// value, unmarked, must not show either: plain), on a create, and on a
		// create whose write fails (the parent directory is missing); and a
		// Sensitive ini_subsetting value on a change and a create.
		{name: "inifile-value", inifile: "6.6.1", seed: func(t *testing.T, dir string) {
			writeSeed(t, filepath.Join(dir, "x.ini"), "[s]\nk = voxagent-plain-old-ini-value\no = a=voxagent-plain-old-sub\n", 0o644)
		}, plain: []string{"voxagent-plain-old-ini-value", "voxagent-plain-old-sub"}, resources: func(dir string) []map[string]any {
			path := filepath.Join(dir, "x.ini")
			return []map[string]any{
				markedResource("Ini_setting", "voxagent-leak-ini", map[string]any{"path": path, "section": "s", "setting": "k", "value": "D8SECRET-Ini_setting-value"}, "value"),
				markedResource("Ini_setting", "voxagent-leak-ini2", map[string]any{"path": path, "section": "s", "setting": "k2", "value": "D8SECRET-Ini_setting-value-create"}, "value"),
				markedResource("Ini_setting", "voxagent-leak-ini-fail", map[string]any{"path": filepath.Join(dir, "missing/x.ini"), "section": "s", "setting": "k", "value": "D8SECRET-Ini_setting-value-fail"}, "value"),
				markedResource("Ini_subsetting", "voxagent-leak-sub", map[string]any{"path": path, "section": "s", "setting": "o", "subsetting": "a", "subsetting_key_val_separator": "=", "value": "D8SECRET-Ini_subsetting-value"}, "value"),
				markedResource("Ini_subsetting", "voxagent-leak-sub2", map[string]any{"path": path, "section": "s", "setting": "o", "subsetting": "b", "subsetting_key_val_separator": "=", "value": "D8SECRET-Ini_subsetting-value-create"}, "value"),
			}
		}},
		// The refusal text of an unimplemented marked value must carry no marker.
		{name: "service-unimplemented-values", refused: true, resources: func(dir string) []map[string]any {
			return []map[string]any{
				markedResource("Service", "voxagent-no-such-service2", map[string]any{"ensure": "D8SECRET-Service-ensure", "enable": "D8SECRET-Service-enable"}, "ensure", "enable"),
			}
		}},
	}
}

// seedCatFail writes cat-fail.sh, a validator that prints the file it is given
// and refuses it.
func seedCatFail(t *testing.T, dir string) {
	writeSeed(t, filepath.Join(dir, "cat-fail.sh"), "#!/bin/sh\ncat \"$1\"\nexit 1\n", 0o755)
}

func concatLeakResources(dir string) []map[string]any {
	ok, fail := filepath.Join(dir, "cs.conf"), filepath.Join(dir, "cs-fail.conf")
	return []map[string]any{
		markedResource("Concat_file", ok, map[string]any{}),
		markedResource("Concat_file", fail, map[string]any{"validate_cmd": filepath.Join(dir, "cat-fail.sh") + " %"}),
		markedResource("Concat_fragment", "cs-secret", map[string]any{"target": ok, "content": "D8SECRET-Concat_fragment-content\n"}, "content"),
		markedResource("Concat_fragment", "cs-fail-secret", map[string]any{"target": fail, "content": "D8SECRET-Concat_fragment-content-fail\n"}, "content"),
	}
}

// pid1IsNotSystemd mirrors Service.Validate's init-system check
// (internal/units/service/service.go). An unreadable /proc/1/comm counts as not systemd.
func pid1IsNotSystemd() bool {
	comm, err := os.ReadFile("/proc/1/comm")
	return err != nil || !strings.Contains(string(comm), "systemd")
}

// fakeCrontab puts a fake crontab first on PATH for the test and returns the
// file holding what it was last given to write. The fake keeps one crontab per
// user (none until written, as the real one says), and prints back whatever it
// is given on stdin, as a crontab writing into a terminal might.
func fakeCrontab(t *testing.T) (written string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	script := `state='` + state + `'
mkdir -p "$state"
user=self
if [ "$1" = -u ]; then user=$2; shift 2; fi
case "$1" in
-l) if [ -f "$state/$user" ]; then cat "$state/$user"; else echo "no crontab for $user" >&2; exit 1; fi ;;
-) tee "$state/$user" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "crontab"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

// fakeShadowBinary builds voxagent with its shadow-utils search path linked
// to a directory of fakes (nameservice.shadowDirs), so a native User never runs the
// host's useradd, usermod or chpasswd, as root or not. The fake useradd
// refuses the names starting voxagent-leak-fail and the fake usermod refuses
// nobody; both accept every other name. No fake prints its arguments or its
// input, as the real commands do not.
func fakeShadowBinary(t *testing.T) string {
	t.Helper()
	fakes := t.TempDir()
	for name, body := range map[string]string{
		"useradd": `for a; do name="$a"; done
case "$name" in voxagent-leak-fail*) echo "useradd: fake refusal of $name" >&2; exit 1;; esac`,
		"usermod": `for a; do name="$a"; done
if [ "$name" = nobody ]; then echo "usermod: fake refusal of $name" >&2; exit 6; fi`,
		"userdel":  ":",
		"chage":    ":",
		"chpasswd": "cat >/dev/null",
	} {
		if err := os.WriteFile(filepath.Join(fakes, name), []byte("#!/bin/sh\n"+body+"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(t.TempDir(), "voxagent")
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/voxagent/voxagent/internal/lib/nameservice.shadowDirs="+fakes, "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// leakAllowed lists markers of plain parameters (never properties) that Ruby
// parity lets an error or command output quote. Each entry names the message.
// None of the cases below surfaced one, so it is empty.
var leakAllowed = []string{}

func assertNoLeak(t *testing.T, where, text string, plain []string) {
	t.Helper()
	rest := text
	for _, a := range leakAllowed {
		rest = strings.ReplaceAll(rest, a, "")
	}
	for i := strings.Index(rest, "D8SECRET-"); i >= 0; i = strings.Index(rest, "D8SECRET-") {
		t.Errorf("%s leaks a marker: %q", where, rest[i:min(len(rest), i+80)])
		rest = rest[i+len("D8SECRET-"):]
	}
	for _, p := range plain {
		if strings.Contains(rest, p) {
			t.Errorf("%s leaks the marked value %q in: %s", where, p, rest)
		}
	}
}

// debugEntry matches a level-debug log entry in a report.
var debugEntry = regexp.MustCompile(`"level":\s*"debug"`)

func TestNoSensitiveMarkerLeaks(t *testing.T) {
	bin := buildBinary(t)
	shadowBin := fakeShadowBinary(t)
	sawWarning := false

	// The -debug modes add --debug, the level that logs the most (graph,
	// transaction, Exec/File/Package/Service lines); the plain agent mode keeps
	// the --verbose this test always used.
	for _, mode := range []string{"apply", "noop", "agent", "apply-debug", "noop-debug", "agent-debug"} {
		debug := strings.HasSuffix(mode, "-debug")
		kind := strings.TrimSuffix(mode, "-debug")
		for _, tc := range leakCases() {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				if tc.agentOnly && kind != "agent" {
					t.Skip("fetches a puppet: source, which only the agent does")
				}
				if (tc.concat != 0 || tc.inifile != "") && kind == "agent" {
					t.Skip("pluginsync purges the libdir the concat and inifile types are read from")
				}
				if tc.rootOnly && os.Geteuid() != 0 {
					t.Skip("changing ownership needs root; verified only when run as root")
				}
				bin := bin
				if tc.fakeShadow {
					probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: "User", Title: "nobody"}}}
					if user, _ := nativeUserGroup(ral.HostPlatform, probe); !user {
						t.Skip("user is not native on this host")
					}
					bin = shadowBin
				}
				var cronState string
				if tc.fakeCrontab {
					cronState = fakeCrontab(t)
					probe := &catalog.Catalog{Resources: []catalog.Resource{{Type: "Cron", Title: "probe"}}}
					if natives, _ := nativeProviders(probe, applyOptions{command: "apply"}, ral.HostPlatform); len(natives) == 0 {
						t.Skip("cron is not native on this host")
					}
				}
				var stdout, stderr string
				var code int
				var reportBytes []byte
				if kind == "agent" {
					base := agentScratch(t)
					if tc.seed != nil {
						tc.seed(t, base)
					}
					f := newFakeServer(t, filepath.Join(base, "managed.txt"))
					cat, err := json.Marshal(map[string]any{
						"catalog_format": 2, "name": agentCertname, "environment": "production", "version": 1,
						"catalog_uuid": "00000000-0000-4000-8000-00000000000a",
						"resources":    tc.resources(base),
						"edges":        []any{}, "tags": []any{}, "classes": []any{},
					})
					if err != nil {
						t.Fatal(err)
					}
					f.catalog = cat
					level := "--verbose"
					if debug {
						level = "--debug"
					}
					stdout, stderr, code = run(t, bin, append(agentArgs(t, base, f), level)...)
					f.mu.Lock()
					reportBytes = append([]byte(nil), f.report...)
					f.mu.Unlock()
				} else {
					dir := t.TempDir()
					if tc.seed != nil {
						tc.seed(t, dir)
					}
					cat := writeCatalog(t, dir, tc.resources(dir))
					reports := filepath.Join(dir, "reports")
					args := []string{"apply", "--catalog", cat, "--report-dir", reports}
					if tc.concat != 0 || tc.inifile != "" {
						lib := inifileModuleLib(t, tc.inifile)
						if tc.concat != 0 {
							lib = concatModuleLib(t, tc.concat)
						}
						libdir := t.TempDir()
						if err := os.CopyFS(libdir, os.DirFS(lib)); err != nil {
							t.Fatal(err)
						}
						args = append(args, "--libdir", libdir, "--no-ruby")
					}
					if kind == "noop" {
						args = append(args, "--noop")
					}
					if debug {
						args = append(args, "--debug")
					}
					stdout, stderr, code = run(t, bin, args...)
					if entries, err := os.ReadDir(reports); err == nil && len(entries) == 1 {
						reportBytes, _ = os.ReadFile(filepath.Join(reports, entries[0].Name()))
					}
				}

				// Plain mode never reports a verdict: 0 for any applied run, 2 for
				// could-not-run. Anything else is a crash.
				wantCode := 0
				if tc.refused {
					wantCode = 2
				}
				if code != wantCode {
					t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, wantCode, stdout, stderr)
				}
				if tc.refused && len(reportBytes) != 0 {
					t.Errorf("a refused catalog produced a report: %s", reportBytes)
				}
				if !tc.refused && len(reportBytes) == 0 {
					t.Fatalf("no report was produced; stderr:\n%s", stderr)
				}
				assertNoLeak(t, "stdout", stdout, tc.plain)
				assertNoLeak(t, "stderr", stderr, tc.plain)
				assertNoLeak(t, "report", string(reportBytes), tc.plain)

				// A --debug that was silently dropped would leave every check above
				// passing on a quieter run, so require the debug output itself.
				if debug && !tc.refused {
					if !strings.Contains(stdout, "Debug: ") {
						t.Errorf("--debug printed no Debug: line on stdout:\n%s", stdout)
					}
					if !debugEntry.Match(reportBytes) {
						t.Errorf("--debug left no level-debug entry in the report")
					}
				}
				if len(tc.plain) > 0 && kind != "noop" && !strings.Contains(string(reportBytes), "[redacted]") {
					t.Errorf("the seeded change never ran: no [redacted] in the report; stderr:\n%s", stderr)
				}
				if tc.name == "user-password" {
					report := string(reportBytes)
					for _, w := range []string{"[redacted]", `"redacted": true`} {
						if !strings.Contains(report, w) {
							t.Errorf("%s missing from the report", w)
						}
					}
					if kind != "noop" && !strings.Contains(stderr+stdout, "Could not create user voxagent-leak-fail: Execution of '[redacted]' returned 1: useradd: fake refusal of voxagent-leak-fail") {
						t.Errorf("the create failure is not redacted; stdout:\n%s\nstderr:\n%s", stdout, stderr)
					}
					if kind != "noop" && !strings.Contains(stdout, "Notice: /User[voxagent-leak-ok]/ensure: created") {
						t.Errorf("the accepted create was not reported; stdout:\n%s\nstderr:\n%s", stdout, stderr)
					}
					if debug && kind != "noop" && !strings.Contains(stdout, "Executing: '[redacted]'") {
						t.Errorf("--debug: no Executing: '[redacted]' line; stdout:\n%s", stdout)
					}
				}
				if tc.fakeCrontab && kind != "noop" {
					// The write ran, so the marker reached crontab's stdin and
					// came back through the copy to stderr, scrubbed.
					written, err := os.ReadDir(cronState)
					if err != nil || len(written) == 0 {
						t.Fatalf("the fake crontab was never written: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
					}
					got, err := os.ReadFile(filepath.Join(cronState, written[0].Name()))
					if err != nil || !strings.Contains(string(got), "D8SECRET-Cron-command") {
						t.Errorf("the crontab written lacks the command: %q, %v", got, err)
					}
					if !strings.Contains(stderr, "# Puppet Name: voxagent-leak-cron") {
						t.Errorf("the write's output was not copied to stderr:\n%s", stderr)
					}
				}
				if tc.name == "user-comment" && kind != "noop" {
					for _, w := range []string{
						"Could not create user voxagent-leak-fail2: Execution of '[redacted]' returned 1: [output redacted]",
						"Could not set comment on user[nobody]: Execution of '[redacted]' returned 6: [output redacted]",
						"Notice: /User[voxagent-leak-ok2]/ensure: created",
					} {
						if !strings.Contains(stderr+stdout, w) {
							t.Errorf("%q missing; stdout:\n%s\nstderr:\n%s", w, stdout, stderr)
						}
					}
				}
				// The validator ran and failed, so the redaction was exercised.
				if tc.concat != 0 && kind != "noop" && !strings.Contains(string(reportBytes), "returned 1: [output redacted]") {
					t.Errorf("no failed validate_cmd with its output redacted in the report; stderr:\n%s", stderr)
				}
				if tc.name == "exec" {
					if strings.Contains(stderr, "Unable to mark 'cwd' as sensitive") {
						sawWarning = true
					}
					if debug {
						wants := []string{"Executing check '[redacted]'"}
						if kind != "noop" {
							wants = append(wants, "Executing '[redacted]'", "Executing: '[redacted]'")
						}
						for _, w := range wants {
							if !strings.Contains(stdout, w) || !strings.Contains(string(reportBytes), w) {
								t.Errorf("--debug: %q missing from stdout or the report; stdout:\n%s", w, stdout)
							}
						}
					}
					if kind != "noop" && !strings.Contains(string(reportBytes), "[output redacted]") {
						t.Errorf("no [output redacted] in the report; stderr:\n%s", stderr)
					}
				}
			})
		}
	}
	if !sawWarning {
		t.Error("the \"Unable to mark 'cwd'\" warning never appeared")
	}
}
