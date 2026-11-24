package file

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
)

// The cases are those measured against openvox 8.29.0, whose code on this path
// is Puppet 8.10.0's.

// validateFixture is a target directory and the probe scripts the
// measurement used.
type validateFixture struct {
	dir  string // where the managed files live
	rec  string // records argv, the temp file's state and the environment, exits 0
	fail string // prints three lines on stdout and stderr, exits 1
	log  string // rec's record
	mark string // created by fail when it runs
}

func newValidateFixture(t *testing.T) validateFixture {
	t.Helper()
	tools := t.TempDir()
	fx := validateFixture{
		dir:  t.TempDir(),
		rec:  filepath.Join(tools, "rec.sh"),
		fail: filepath.Join(tools, "fail.sh"),
		log:  filepath.Join(tools, "rec.log"),
		mark: filepath.Join(tools, "fail.ran"),
	}
	rec := `#!/bin/sh
{
  printf 'argc=%s\n' "$#"
  i=1
  for a in "$@"; do
    printf 'arg%s=%s\n' "$i" "$a"
    if [ -f "$a" ]; then
      printf 'mode%s=%s\n' "$i" "$(stat -c %a "$a")"
      printf 'content%s=%s\n' "$i" "$(cat "$a")"
      printf 'listing%s=%s\n' "$i" "$(ls -A "$(dirname "$a")" | tr '\n' ' ')"
    fi
    i=$((i+1))
  done
  env | grep -E '^(HOME|USER|LOGNAME|LANG|LANGUAGE|LC_[A-Z]+|VOXAGENT_VALIDATE_KEPT)=' | sort | sed 's/^/env:/'
} >> ` + fx.log + "\nexit 0\n"
	fail := "#!/bin/sh\n: > " + fx.mark + `
echo "stdout line one"
echo "stderr line two" >&2
echo "stdout line three"
exit 1
`
	if err := os.WriteFile(fx.rec, []byte(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.fail, []byte(fail), 0o755); err != nil {
		t.Fatal(err)
	}
	return fx
}

// record reads rec's log as key=value lines; ok is false when rec never ran.
func (fx validateFixture) record(t *testing.T) (map[string]string, bool) {
	t.Helper()
	b, err := os.ReadFile(fx.log)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		out[k] = v
	}
	return out, true
}

func (fx validateFixture) failRan() bool {
	_, err := os.Stat(fx.mark)
	return err == nil
}

// tempPattern matches Puppet's Uniquefile name for base in dir, made by this
// process (file_system/uniquefile.rb:102-116).
func tempPattern(dir, base string) string {
	return regexp.QuoteMeta(filepath.Join(dir, base)) +
		fmt.Sprintf(`\d{8}-%d-[0-9a-z]+`, os.Getpid())
}

func applyValidated(t *testing.T, r catalog.Resource, noop bool) []ral.Change {
	t.Helper()
	changes, _, err := File{}.Apply(context.Background(), r, noop)
	if err != nil {
		t.Fatalf("Apply returned a resource-level error: %v", err)
	}
	return changes
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// Case 1: the validator sees the complete, closed temp file beside the target,
// with the existing file's mode, and the target is replaced afterwards.
func TestFileValidateCmdSuccessReplacesTheFile(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o640)

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{
		"content": "new\n", "validate_cmd": fx.rec + " %"}), false)

	if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err != nil {
		t.Fatalf("changes = %+v, want one successful content change", changes)
	}
	want := fmt.Sprintf("content changed '%s' to '%s'", checksum([]byte("old\n")), checksum([]byte("new\n")))
	if changes[0].Message != want {
		t.Errorf("message = %q, want %q", changes[0].Message, want)
	}
	rec, ran := fx.record(t)
	if !ran {
		t.Fatal("the validator did not run")
	}
	if rec["argc"] != "1" {
		t.Errorf("argc = %s, want 1", rec["argc"])
	}
	if !regexp.MustCompile(`^` + tempPattern(fx.dir, "f") + `$`).MatchString(rec["arg1"]) {
		t.Errorf("arg1 = %q, want Puppet's temp name beside the target", rec["arg1"])
	}
	if rec["mode1"] != "640" || rec["content1"] != "new" {
		t.Errorf("temp during validation: mode %s content %q, want 640 and the new content", rec["mode1"], rec["content1"])
	}
	if got := strings.Fields(rec["listing1"]); len(got) != 2 || !slices.Contains(got, "f") {
		t.Errorf("directory during validation = %v, want the target and the temp file alone", got)
	}
	if got := readString(t, path); got != "new\n" {
		t.Errorf("target = %q, want the new content", got)
	}
	if got := permOf(t, path); got != 0o640 {
		t.Errorf("target mode = %o, want 640 kept", got)
	}
	if got := dirEntries(t, fx.dir); !slices.Equal(got, []string{"f"}) {
		t.Errorf("directory after = %v, want the target alone", got)
	}
}

// Case 2: a failing validator fails content with Puppet's ExecutionFailure,
// leaves the target and removes the temp file; mode still syncs (c02b).
func TestFileValidateCmdFailureLeavesTheTarget(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o644)

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{
		"content": "new\n", "mode": "0600", "validate_cmd": fx.fail + " %"}), false)

	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want a failed content change and a mode change", changes)
	}
	content, mode := changes[0], changes[1]
	if content.Property != "content" || content.Err == nil {
		t.Fatalf("first change = %+v, want a failed content change", content)
	}
	if content.Was != checksum([]byte("old\n")) || content.Is != checksum([]byte("new\n")) {
		t.Errorf("content transition = %v -> %v, want old -> new checksums", content.Was, content.Is)
	}
	wantErr := regexp.MustCompile(`^Execution of '` + regexp.QuoteMeta(fx.fail) + ` ` + tempPattern(fx.dir, "f") +
		`' returned 1: stdout line one\nstderr line two\nstdout line three$`)
	if !wantErr.MatchString(content.Err.Error()) {
		t.Errorf("err = %q, want Puppet's ExecutionFailure with the combined output", content.Err)
	}
	if mode.Property != "mode" || mode.Err != nil || mode.Message != "mode changed '0644' to '0600'" {
		t.Errorf("second change = %+v, want mode to sync after the failure", mode)
	}
	if got := readString(t, path); got != "old\n" {
		t.Errorf("target = %q, want it untouched", got)
	}
	if got := dirEntries(t, fx.dir); !slices.Equal(got, []string{"f"}) {
		t.Errorf("directory after = %v, want the temp file gone", got)
	}
}

// c02c: a new file whose validator fails fails ensure, unwrapped (an
// ExecutionFailure is a Puppet::Error, so property.rb:490 re-raises it), and
// nothing is created.
func TestFileValidateCmdFailureOnANewFileFailsEnsure(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	r := raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": fx.fail + " %"})
	r.File, r.Line = "/etc/site.pp", 3

	changes := applyValidated(t, r, false)

	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one ensure change", changes)
	}
	ch := changes[0]
	if ch.Property != "ensure" || ch.Name != "file_created" || ch.Was != "absent" || ch.Is != "file" || ch.Err == nil {
		t.Fatalf("change = %+v, want a failed file_created from absent to file", ch)
	}
	wantErr := regexp.MustCompile(`^Execution of '` + regexp.QuoteMeta(fx.fail) + ` ` + tempPattern(fx.dir, "f") +
		`' returned 1: stdout line one\nstderr line two\nstdout line three$`)
	if !wantErr.MatchString(ch.Err.Error()) {
		t.Errorf("err = %q, want the bare ExecutionFailure", ch.Err)
	}
	if got := dirEntries(t, fx.dir); len(got) != 0 {
		t.Errorf("directory after = %v, want nothing created", got)
	}
}

// c05a: on a new file the temp file carries the desired mode while it is
// validated.
func TestFileValidateCmdNewFileTempHasTheDesiredMode(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{
		"content": "new\n", "mode": "0750", "validate_cmd": fx.rec + " %"}), false)

	if len(changes) != 1 || changes[0].Name != "file_created" || changes[0].Err != nil {
		t.Fatalf("changes = %+v, want one successful file_created", changes)
	}
	if want := "defined content as '" + checksum([]byte("new\n")) + "'"; changes[0].Message != want {
		t.Errorf("message = %q, want %q", changes[0].Message, want)
	}
	rec, _ := fx.record(t)
	if rec["mode1"] != "750" {
		t.Errorf("temp mode during validation = %s, want 750", rec["mode1"])
	}
	if got := permOf(t, path); got != 0o750 {
		t.Errorf("target mode = %o, want 750", got)
	}
}

// Case 3: without the token the command runs verbatim, with no argument, and
// the file is replaced (c03, c03b).
func TestFileValidateCmdWithoutTheTokenRunsVerbatim(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o644)

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": fx.rec}), false)
	if len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("changes = %+v, want one successful content change", changes)
	}
	if rec, _ := fx.record(t); rec["argc"] != "0" {
		t.Errorf("argc = %s, want 0", rec["argc"])
	}
	if got := readString(t, path); got != "new\n" {
		t.Errorf("target = %q, want it replaced", got)
	}

	seedOld(t, path, 0o644)
	changes = applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": fx.fail}), false)
	want := "Execution of '" + fx.fail + "' returned 1: stdout line one\nstderr line two\nstdout line three"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes = %+v, want content failing with %q", changes, want)
	}
}

// Case 4: validate_replacement is replaced everywhere, inside words too, and
// the default % is then literal (c04, c04b).
func TestFileValidateReplacementReplacesEveryOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, replacement string
		want                   []string // argv, with T for the temp path
	}{
		{"custom", "%s $file %% $file", "$file", []string{"T", "%", "T"}},
		{"inside a word", "%s %% --x=%%", "", []string{"T", "--x=T"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newValidateFixture(t)
			path := filepath.Join(fx.dir, "f")
			seedOld(t, path, 0o644)
			params := map[string]any{"content": "new\n", "validate_cmd": fmt.Sprintf(tc.cmd, fx.rec)}
			if tc.replacement != "" {
				params["validate_replacement"] = tc.replacement
			}
			if changes := applyValidated(t, raltest.FileRes(path, params), false); len(changes) != 1 || changes[0].Err != nil {
				t.Fatalf("changes = %+v, want one successful content change", changes)
			}
			rec, _ := fx.record(t)
			temp := rec["arg1"]
			if !regexp.MustCompile(`^` + tempPattern(fx.dir, "f") + `$`).MatchString(temp) {
				t.Fatalf("arg1 = %q, want the temp path", temp)
			}
			if rec["argc"] != fmt.Sprint(len(tc.want)) {
				t.Errorf("argc = %s, want %d", rec["argc"], len(tc.want))
			}
			for i, w := range tc.want {
				if got, want := rec[fmt.Sprintf("arg%d", i+1)], strings.ReplaceAll(w, "T", temp); got != want {
					t.Errorf("arg%d = %q, want %q", i+1, got, want)
				}
			}
		})
	}
}

// x04d: an empty replacement inserts the path between every character, as
// Ruby's gsub with an empty pattern does; the result names no executable, so the exec fails.
func TestFileValidateEmptyReplacementInsertsThePathEverywhere(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o644)

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{
		"content": "new\n", "validate_cmd": "ab", "validate_replacement": ""}), false)

	if len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("changes = %+v, want a failed content change", changes)
	}
	tp := tempPattern(fx.dir, "f")
	want := regexp.MustCompile(`^Execution of '(` + tp + `)a(` + tp + `)b(` + tp + `)' returned 1: `)
	m := want.FindStringSubmatch(changes[0].Err.Error())
	if m == nil || m[1] != m[2] || m[2] != m[3] {
		t.Errorf("err = %q, want the one temp path around and between a and b", changes[0].Err)
	}
}

// x04c, NOT reproduced (improvements.md): a backslash sequence in the path is
// substituted literally, so the validator gets the real temp file. The
// backslash sends the line to /bin/sh, as it does in Ruby, so the token is
// single-quoted to reach the validator as written.
func TestFileValidateSubstitutesThePathLiterally(t *testing.T) {
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, `x\0y`)

	changes := applyValidated(t, raltest.FileRes(path, map[string]any{
		"content": "new\n", "validate_cmd": fx.rec + " '%'"}), false)

	if len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("changes = %+v, want one successful create", changes)
	}
	rec, _ := fx.record(t)
	if !strings.HasPrefix(rec["arg1"], path) || rec["content1"] != "new" {
		t.Errorf("arg1 = %q content %q, want the real temp file under %q", rec["arg1"], rec["content1"], path)
	}
}

// Case 5 and case 8: the validator runs only when content is written through
// the temp file.
func TestFileValidateCmdDoesNotRunWithoutAWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seed   bool
		params map[string]any
		noop   bool
		want   []string // the properties that change
	}{
		{"ensure absent (c05b)", true, map[string]any{"ensure": "absent"}, false, []string{"ensure"}},
		{"mode only (c05c)", true, map[string]any{"content": "old\n", "mode": "0600"}, false, []string{"mode"}},
		{"in sync (c05h)", true, map[string]any{"content": "old\n"}, false, nil},
		{"replace false (c05i)", true, map[string]any{"content": "new\n", "replace": false}, false, nil},
		{"no content or source (c05f)", false, map[string]any{"ensure": "file"}, false, []string{"ensure"}},
		{"noop (c08)", true, map[string]any{"content": "new\n"}, true, []string{"content"}},
		{"noop create", false, map[string]any{"content": "new\n"}, true, []string{"ensure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newValidateFixture(t)
			path := filepath.Join(fx.dir, "f")
			if tc.seed {
				seedOld(t, path, 0o644)
			}
			params := map[string]any{"validate_cmd": fx.fail + " %"}
			for k, v := range tc.params {
				params[k] = v
			}
			changes := applyValidated(t, raltest.FileRes(path, params), tc.noop)
			var got []string
			for _, ch := range changes {
				if ch.Err != nil {
					t.Errorf("%s failed: %v", ch.Property, ch.Err)
				}
				got = append(got, ch.Property)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("changed %v, want %v", got, tc.want)
			}
			if fx.failRan() {
				t.Error("the validator ran")
			}
		})
	}
}

// c05e and c05d: empty content and a source both take the temp-file path.
func TestFileValidateCmdRunsForEmptyContentAndForASource(t *testing.T) {
	t.Run("empty content", func(t *testing.T) {
		fx := newValidateFixture(t)
		path := filepath.Join(fx.dir, "f")
		seedOld(t, path, 0o644)
		changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "", "validate_cmd": fx.fail + " %"}), false)
		if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err == nil || !fx.failRan() {
			t.Fatalf("changes = %+v (ran %v), want content failing in the validator", changes, fx.failRan())
		}
		if got := readString(t, path); got != "old\n" {
			t.Errorf("target = %q, want it untouched", got)
		}
	})
	t.Run("source", func(t *testing.T) {
		fx := newValidateFixture(t)
		src := filepath.Join(t.TempDir(), "src.txt")
		mustWrite(t, src, "from source\n")
		path := filepath.Join(fx.dir, "f")
		seedOld(t, path, 0o644)
		changes := applyValidated(t, raltest.FileRes(path, map[string]any{"source": src, "validate_cmd": fx.rec + " %"}), false)
		if len(changes) != 1 || changes[0].Property != "content" || changes[0].Err != nil {
			t.Fatalf("changes = %+v, want one successful content change", changes)
		}
		if rec, _ := fx.record(t); rec["content1"] != "from source" {
			t.Errorf("temp held %q during validation, want the source's bytes", rec["content1"])
		}
	})
}

// Case 6: an exec-level failure is status 1; only the prefix is Puppet's
// stable text, the tail is the allowlisted part.
func TestFileValidateCmdExecFailureReturnsOne(t *testing.T) {
	noexec := filepath.Join(t.TempDir(), "noexec.sh")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"/nonexistent/cmd", "voxagent-no-such-validator", noexec} {
		t.Run(filepath.Base(cmd), func(t *testing.T) {
			fx := newValidateFixture(t)
			path := filepath.Join(fx.dir, "f")
			seedOld(t, path, 0o644)
			changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": cmd + " %"}), false)
			if len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("changes = %+v, want a failed content change", changes)
			}
			prefix := regexp.MustCompile(`^Execution of '` + regexp.QuoteMeta(cmd) + ` ` + tempPattern(fx.dir, "f") + `' returned 1: `)
			if !prefix.MatchString(changes[0].Err.Error()) {
				t.Errorf("err = %q, want the exec-level failure prefix", changes[0].Err)
			}
			if got := readString(t, path); got != "old\n" {
				t.Errorf("target = %q, want it untouched", got)
			}
			if got := dirEntries(t, fx.dir); !slices.Equal(got, []string{"f"}) {
				t.Errorf("directory after = %v, want the temp file gone", got)
			}
		})
	}
}

// The child environment: Puppet deletes HOME, USER, LOGNAME and the locale
// variables, sets LANG=C and LC_ALL=C, and keeps the rest.
func TestFileValidateCmdEnvironment(t *testing.T) {
	for k, v := range map[string]string{
		"HOME": "/home/x", "USER": "x", "LOGNAME": "x", "LANG": "en_US.UTF-8", "LC_ALL": "en_US.UTF-8",
		"LC_CTYPE": "en_US.UTF-8", "LC_MESSAGES": "C.UTF-8", "LANGUAGE": "en", "LC_COLLATE": "C",
		"LC_MONETARY": "C", "LC_NUMERIC": "C", "LC_TIME": "C", "LC_PAPER": "C", "VOXAGENT_VALIDATE_KEPT": "yes",
	} {
		t.Setenv(k, v)
	}
	fx := newValidateFixture(t)
	path := filepath.Join(fx.dir, "f")
	applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": fx.rec + " %"}), false)
	rec, _ := fx.record(t)
	var env []string
	for k, v := range rec {
		if name, ok := strings.CutPrefix(k, "env:"); ok {
			env = append(env, name+"="+v)
		}
	}
	slices.Sort(env)
	// LC_PAPER is not in LOCALE_ENV_VARS (util/posix.rb:6-7), so it stays.
	want := []string{"LANG=C", "LC_ALL=C", "LC_PAPER=C", "VOXAGENT_VALIDATE_KEPT=yes"}
	if !slices.Equal(env, want) {
		t.Errorf("child environment = %v, want %v", env, want)
	}
}

// Ruby's execvp searches a PATH entry of "." or "" as the working directory,
// so a validator named without a slash runs from there (I-21, measured with
// Ruby 3.4.9); os/exec refuses that lookup unless exec.ErrDot is cleared.
func TestFileValidateCmdFindsACommandThroughADotPathEntry(t *testing.T) {
	for _, pathEnv := range []string{".:/usr/bin:/bin", ":/usr/bin:/bin", "/usr/bin:/bin:"} {
		t.Run(pathEnv, func(t *testing.T) {
			fx := newValidateFixture(t)
			cwd := t.TempDir()
			b, err := os.ReadFile(fx.rec)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cwd, "vox-dot-validator"), b, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", pathEnv)
			t.Chdir(cwd)
			path := filepath.Join(fx.dir, "f")
			changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": "vox-dot-validator %"}), false)
			if len(changes) != 1 || changes[0].Err != nil {
				t.Fatalf("changes = %+v, want a successful create", changes)
			}
			if _, ran := fx.record(t); !ran {
				t.Error("the validator in the working directory did not run")
			}
		})
	}
}

// Ruby's Kernel.exec(String) runs a script without a #! line through /bin/sh
// when execve says ENOEXEC (process.c proc_exec_cmd), measured with Ruby 3.4.9.
func TestFileValidateCmdRunsAScriptWithoutAShebangThroughSh(t *testing.T) {
	fx := newValidateFixture(t)
	script := filepath.Join(t.TempDir(), "plain.sh")
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(script, []byte(": > "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fx.dir, "f")
	changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": script + " %"}), false)
	if len(changes) != 1 || changes[0].Err != nil {
		t.Fatalf("changes = %+v, want a successful create", changes)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the script did not run: %v", err)
	}
}

// A validator killed by a signal has no exit status: Ruby's exitstatus is nil,
// which interpolates as nothing (util/execution.rb:311-312).
func TestFileValidateCmdKilledBySignalHasNoStatus(t *testing.T) {
	fx := newValidateFixture(t)
	script := filepath.Join(t.TempDir(), "killed.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o644)
	changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": script}), false)
	want := "Execution of '" + script + "' returned : "
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes = %+v, want content failing with %q", changes, want)
	}
}

// The output is String#strip'd, which in Ruby 3.4 takes NULs as well as
// whitespace from both ends (measured: "\0 \t\nab\0 \v\f\r\0".strip == "ab").
func TestFileValidateCmdStripsTheOutput(t *testing.T) {
	fx := newValidateFixture(t)
	script := filepath.Join(t.TempDir(), "noisy.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '\\000 \\t\\na b\\000 \\v\\f\\r\\000'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fx.dir, "f")
	seedOld(t, path, 0o644)
	changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": script}), false)
	want := "Execution of '" + script + "' returned 2: a b"
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Err.Error() != want {
		t.Fatalf("changes = %+v, want content failing with %q", changes, want)
	}
}

// Ruby's choice between /bin/sh -c and a direct exec (process.c
// rb_exec_fillarg), measured with Ruby 3.4.9 under strace: every character in
// `*?{}[]<>()~&|\$;'"` + "`\n#" sends the line to the shell, as do an = in the
// first word before any / and a first word that is a POSIX reserved word or
// special built-in.
func TestRubyExecWords(t *testing.T) {
	for _, tc := range []struct {
		line  string
		words []string // nil: through the shell
	}{
		{"/bin/true a b", []string{"/bin/true", "a", "b"}},
		{"  rec.sh\t%  x ", []string{"rec.sh", "%", "x"}},
		{"echo a=b", []string{"echo", "a=b"}},
		{"/x=b c", []string{"/x=b", "c"}},
		{"x/a=b", []string{"x/a=b"}},
		{"exits 1", []string{"exits", "1"}},
		{"cd /", []string{"cd", "/"}},
		{"a=b true", nil},
		{"ab= true", nil},
		{"exit 3", nil},
		{"  exit 0", nil},
		{"export", nil},
		{"!", nil},
		{". x", nil},
		{"/bin/true a\nb", nil},
		{`printf %s\\n x`, nil},
		{"/bin/false || /bin/true", nil},
		{"", []string{}},
		{"  \t", []string{}},
	} {
		words, shell := rubyExecWords(tc.line)
		if (tc.words == nil) != shell {
			t.Errorf("%q: shell = %v, want %v", tc.line, shell, tc.words == nil)
			continue
		}
		if !shell && !slices.Equal(words, tc.words) {
			t.Errorf("%q: words = %q, want %q", tc.line, words, tc.words)
		}
	}
	for _, c := range "\"#$&'()*;<>?[\\]`{|}~" {
		if _, shell := rubyExecWords("/bin/true a" + string(c) + "b"); !shell {
			t.Errorf("%q does not send the line to the shell", c)
		}
	}
	for _, c := range "!%+,-./:=@^_" {
		if _, shell := rubyExecWords("/bin/true a" + string(c) + "b"); shell {
			t.Errorf("%q sends the line to the shell", c)
		}
	}
}

// file_system/uniquefile.rb:102-116: no separator before the date, the pid,
// base 36 of a random 32-bit number, and -N on a retry.
func TestValidationTempName(t *testing.T) {
	day := time.Date(2026, 10, 7, 23, 0, 0, 0, time.Local)
	if got := validationTempName("sudoers", day, 5906, 4294967295, 0); got != "sudoers20261007-5906-1z141z3" {
		t.Errorf("first try = %q", got)
	}
	if got := validationTempName("f", day, 41, 0, 2); got != "f20261007-41-0-2" {
		t.Errorf("third try = %q", got)
	}
}

// voxagent refuses at Validate what Puppet fails at every sync
// (improvements.md), and validate_cmd on a recursed tree, whose children's
// names would be substituted into a shell command.
func TestFileValidateRefusesValidateParameters(t *testing.T) {
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"validate_cmd": "/bin/true %", "validate_replacement": 42.0},
			"File[/x]: validate_replacement => 42 (float64) is refused: Puppet accepts it and fails every change it validates with \"wrong argument type\""},
		{map[string]any{"validate_cmd": []any{"/bin/true", "%"}},
			"File[/x]: validate_cmd => [/bin/true %] ([]interface {}) is refused: Puppet accepts it and fails every change it validates with \"undefined method 'gsub'\""},
		{map[string]any{"validate_cmd": "/bin/true %", "recurse": true, "source": "/src"},
			"File[/x]: validate_cmd with recurse => true is not implemented: each generated file's name would be substituted into the command"},
	} {
		err := File{}.Validate(raltest.FileRes("/x", tc.params))
		if err == nil || err.Error() != tc.want {
			t.Errorf("Validate(%v) = %v, want %q", tc.params, err, tc.want)
		}
	}
	if err := (File{}).Validate(raltest.FileRes("/x", map[string]any{"validate_cmd": "/bin/true %", "validate_replacement": "@"})); err != nil {
		t.Errorf("Validate refused string parameters: %v", err)
	}
}

// type/file.rb:940: each line of a successful validate_cmd's output is a debug
// line at the resource's path, split as String#split(/\n/) does. The output is
// plain whatever the catalog marks: validate_cmd is a plain parameter. A
// failing command's output goes to the error instead (Ruby raises first).
func TestFileValidateCmdOutputIsLoggedLineByLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"two lines", `/bin/sh -c 'echo one; echo two' %`, []string{"one", "two"}},
		{"one line without a newline", `/bin/sh -c 'printf single' %`, []string{"single"}},
		{"blank lines inside are kept, trailing ones dropped", `/bin/sh -c 'printf "a\n\nb\n\n"' %`, []string{"a", "", "b"}},
		{"no output", `/bin/true %`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "target")
			r := raltest.FileRes(path, map[string]any{"content": "secret\n", "validate_cmd": tc.cmd})
			src := fileSources(path)

			ctx, got := raltest.DebugContext("debug")
			if _, _, err := (File{}).Apply(sources.WithSources(ctx, src), r, false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			if lines := raltest.LinesAt(*got, src.Resource); !slices.Equal(lines, tc.want) {
				t.Errorf("debug lines at %s = %q, want %q", src.Resource, lines, tc.want)
			}
		})
	}

	t.Run("a failing command logs no output lines", func(t *testing.T) {
		fx := newValidateFixture(t)
		path := filepath.Join(fx.dir, "target")
		src := fileSources(path)
		ctx, got := raltest.DebugContext("debug")
		changes, _, err := File{}.Apply(sources.WithSources(ctx, src),
			raltest.FileRes(path, map[string]any{"content": "x\n", "validate_cmd": fx.fail + " %"}), false)
		if err != nil || len(changes) == 0 || changes[0].Err == nil {
			t.Fatalf("Apply = %+v, %v; want a failed ensure", changes, err)
		}
		if lines := raltest.LinesAt(*got, src.Resource); len(lines) != 0 {
			t.Errorf("debug lines = %q, want none", lines)
		}
	})

	t.Run("notice drops them", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "target")
		ctx, got := raltest.DebugContext("notice")
		r := raltest.FileRes(path, map[string]any{"content": "x\n", "validate_cmd": `/bin/sh -c 'echo one' %`})
		if _, _, err := (File{}).Apply(sources.WithSources(ctx, fileSources(path)), r, false); err != nil {
			t.Fatalf("Apply returned error: %v", err)
		}
		if len(*got) != 0 {
			t.Errorf("entries = %+v, want none at notice", *got)
		}
	})
}

// D-78: a validator can echo the content it checks, so a marked content
// prints none of its output, in the failure text or the debug lines.
func TestValidateCmdOutputRedactedWhenContentMarked(t *testing.T) {
	const failCmd = `/bin/sh -c "echo s3cret-token; exit 3" %`
	marks := map[string]func(*catalog.Resource){
		"SensitiveParameters": func(r *catalog.Resource) { r.SensitiveParameters = []string{"content"} },
		"SetSensitivity":      func(r *catalog.Resource) { r.SetSensitivity = map[string]bool{"content": true} },
		// Unmarked content an eval_generate built from marked input.
		"OutputRedacted": func(r *catalog.Resource) { r.OutputRedacted = true },
	}
	for name, mark := range marks {
		t.Run(name+" failure", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "target")
			r := raltest.FileRes(path, map[string]any{"content": "x\n", "validate_cmd": failCmd})
			mark(&r)
			changes := applyValidated(t, r, false)
			if len(changes) == 0 || changes[0].Err == nil {
				t.Fatalf("changes = %+v, want a failed content change", changes)
			}
			wantErr := regexp.MustCompile(`^Execution of '/bin/sh -c "echo s3cret-token; exit 3" ` +
				tempPattern(filepath.Dir(path), "target") + `' returned 3: \[output redacted\]$`)
			if !wantErr.MatchString(changes[0].Err.Error()) {
				t.Errorf("err = %q, want the output redacted", changes[0].Err)
			}
		})
		t.Run(name+" success logs no output", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "target")
			src := fileSources(path)
			r := raltest.FileRes(path, map[string]any{"content": "x\n", "validate_cmd": `/bin/sh -c 'echo s3cret-token' %`})
			mark(&r)
			ctx, got := raltest.DebugContext("debug")
			if _, _, err := (File{}).Apply(sources.WithSources(ctx, src), r, false); err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			if lines := raltest.LinesAt(*got, src.Resource); len(lines) != 0 {
				t.Errorf("debug lines = %q, want none", lines)
			}
		})
	}

	t.Run("unmarked failure keeps the output", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "target")
		changes := applyValidated(t, raltest.FileRes(path, map[string]any{"content": "x\n", "validate_cmd": failCmd}), false)
		if len(changes) == 0 || changes[0].Err == nil || !strings.HasSuffix(changes[0].Err.Error(), "returned 3: s3cret-token") {
			t.Fatalf("changes = %+v, want a failure ending returned 3: s3cret-token", changes)
		}
	})

	t.Run("another marked attribute does not redact", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "target")
		r := raltest.FileRes(path, map[string]any{"content": "x\n", "mode": "0600", "validate_cmd": failCmd})
		r.SensitiveParameters = []string{"mode"}
		changes := applyValidated(t, r, false)
		if len(changes) == 0 || changes[0].Err == nil || !strings.HasSuffix(changes[0].Err.Error(), "returned 3: s3cret-token") {
			t.Fatalf("changes = %+v, want the output kept", changes)
		}
	})
}

func TestValidateCmdTruncationWarns(t *testing.T) {
	raltest.WithOutputCap(t, 4)
	ctx, got := raltest.DebugContext("notice")
	ctx = sources.WithSources(ctx, sources.Sources{Ref: "File[/x]"})
	path := filepath.Join(t.TempDir(), "f")
	r := raltest.FileRes(path, map[string]any{"content": "new\n", "validate_cmd": "printf abcdefgh; true"})
	if _, _, err := (File{}).Apply(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].Level != "warning" || !strings.HasPrefix((*got)[0].Message, "File[/x]: output of ") ||
		!strings.HasSuffix((*got)[0].Message, " truncated at 64 MiB") {
		t.Errorf("entries = %+v, want one truncation warning", *got)
	}
}

func TestFileValidateRequiresAnAbsolutePath(t *testing.T) {
	cases := []struct {
		title string
		want  string // "" means valid
	}{
		{"/tmp/a", ""},
		{"/", ""},
		{"rel/f1", "Parameter path failed on File[rel/f1]: File paths must be fully qualified, not 'rel/f1'"},
		{"./x", "Parameter path failed on File[./x]: File paths must be fully qualified, not './x'"},
		{"", "Parameter path failed on File[]: File paths must be fully qualified, not ''"},
	}
	for _, tc := range cases {
		err := File{}.Validate(catalog.Resource{Type: "File", Title: tc.title})
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tc.title, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("%q: error = %v, want %q", tc.title, err, tc.want)
		}
	}
}
