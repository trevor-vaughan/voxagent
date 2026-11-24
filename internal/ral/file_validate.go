package ral

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// validation is a file's validate_cmd: the command that must accept the new
// content, staged in a temp file beside the target, before it is renamed over
// the target (type/file.rb:935-942, util.rb:658-660).
//
// Only a staged write runs it -- content, empty content included, or a source
// -- and only when that write happens, so an absent, mode-only, in-sync,
// replace => false or noop resource never does.
type validation struct {
	cmd         string
	replacement string
	// outputRedacted is set when the File's content is marked Sensitive, or
	// built from marked input (catalog.Resource.OutputRedacted): a validator
	// can echo the content it checks, so its output is never printed (D-78).
	outputRedacted bool
}

// validationOf returns r's validation, or nil when it declares no
// validate_cmd. outputRedacted says whether r's content is marked or built
// from marked input. validate_replacement defaults to % (type/file.rb:379-384).
// Validate refuses a parameter that is not a string; the error here keeps a
// resource that reached Apply without Validate from writing unvalidated.
func validationOf(r catalog.Resource, outputRedacted bool) (*validation, error) {
	raw, ok := r.Parameters["validate_cmd"]
	if !ok {
		return nil, nil
	}
	cmd, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("validate_cmd is %T, not a string", raw)
	}
	v := &validation{cmd: cmd, replacement: "%", outputRedacted: outputRedacted}
	if raw, ok := r.Parameters["validate_replacement"]; ok {
		if v.replacement, ok = raw.(string); !ok {
			return nil, fmt.Errorf("validate_replacement is %T, not a string", raw)
		}
	}
	return v, nil
}

// validationTempTries bounds the EEXIST retries. Puppet's Uniquefile retries
// without limit; a directory that refuses a hundred random names is not going
// to accept the hundred-and-first.
const validationTempTries = 100

// createTemp creates the temp file for path's validated write in path's
// directory, under Puppet's name, O_EXCL and 0600 as Uniquefile does
// (file_system/uniquefile.rb:30-48). O_EXCL with O_CREATE never follows a
// link at the name, and the fileSystem confines a generated child's write to
// its tree, so the file is new and is where the name says.
func (*validation) createTemp(fsys fileSystem, path string) (*os.File, string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	for retry := range validationTempTries {
		tmp := filepath.Join(dir, validationTempName(base, time.Now(), os.Getpid(), rand.Uint32(), retry))
		f, err := fsys.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return f, tmp, nil
	}
	// uniquefile.rb:136's wording, for the limit Puppet does not have.
	return nil, "", fmt.Errorf("cannot generate temporary name using `%s' under `%s'", base, dir)
}

// run runs the command against tmpPath, the complete, closed temp file, and
// returns Puppet's ExecutionFailure when it exits non-zero.
//
// The replacement is substituted LITERALLY into every occurrence, which an
// empty replacement makes every character boundary, as Ruby's gsub with an empty pattern does.
// Puppet's gsub also reads backslash sequences in the path as back-references
// (measurement x04c); that is not reproduced (improvements.md I-19). The
// command comes from the catalog and the name around the path is
// validationTempName's digits and base-36 letters.
//
// There is no timeout: Puppet passes none (type/file.rb:938).
func (v *validation) run(ctx context.Context, tmpPath string) error {
	line := strings.ReplaceAll(v.cmd, v.replacement, tmpPath)
	status, output, err := runAsRubyExec(ctx, line)
	if err != nil {
		return err
	}
	if status == 0 {
		// type/file.rb:940: each line of the output, in Ruby's split, is a
		// debug line at the resource's path. A marked content prints none
		// (D-78); Exec's one "[output redacted]" line is not copied, as the
		// validator's output is not a command result the user asked to see.
		if l := runlog.From(ctx); !v.outputRedacted && l.Enabled("debug") {
			for _, out := range splitOutput(output) {
				l.Log("debug", SourcesFrom(ctx).Resource, out)
			}
		}
		return nil
	}
	if v.outputRedacted {
		output = "[output redacted]"
	}
	return &executionFailure{command: line, status: status, output: output}
}

// executionFailure is Puppet::ExecutionFailure as util/execution.rb:311-312
// raises it. It is a Puppet::Error, so the ensure setter re-raises it
// unwrapped (property.rb:490), like checksumMismatchError.
type executionFailure struct {
	command string
	// status is the exit status, or -1 for a process killed by a signal,
	// whose Ruby exitstatus is nil and interpolates as nothing.
	status int
	output string
}

func (e *executionFailure) Error() string {
	status := ""
	if e.status >= 0 {
		status = strconv.Itoa(e.status)
	}
	// String#strip: Ruby 3.4 takes NULs as well as whitespace from both ends.
	return fmt.Sprintf("Execution of '%s' returned %s: %s", e.command, status, strings.Trim(e.output, " \t\n\v\f\r\x00"))
}

// rubyEnvDropped are the variables Puppet deletes in the child before every
// command it executes: util/posix.rb:6-7's LOCALE_ENV_VARS, which
// util/execution.rb:372-379 replaces with LANG=C and LC_ALL=C, and :11's
// USER_ENV_VARS (:386), the userEnvVars. Exec and the argv runners drop only
// the userEnvVars; validate_cmd drops both.
var rubyEnvDropped = func() map[string]struct{} {
	dropped := map[string]struct{}{}
	for _, name := range rubyLocaleVars {
		dropped[name] = struct{}{}
	}
	for _, name := range userEnvVars {
		dropped[name] = struct{}{}
	}
	return dropped
}()

// runAsRubyExec runs line as Puppet's execute_posix does
// (util/execution.rb:354-395) with :failonfail and :combine: in a new
// session, stdin on /dev/null, stdout and stderr into one capture in arrival
// order, the agent's cwd and uid, the environment shaped as rubyEnvDropped
// says, and the command run as Ruby's Kernel.exec(String) runs it.
//
// A command that cannot be executed (ENOENT, EACCES, ...) is not an error
// here: Puppet's child logs it and exits 1 (:392-395), so it is status 1. The
// output Ruby's child writes depends on the agent's log destinations
// (measured); this one says what failed, plainly.
func runAsRubyExec(ctx context.Context, line string) (int, string, error) {
	words, shell := rubyExecWords(line)
	var argv0 string
	var cmd *exec.Cmd
	switch {
	case shell:
		argv0 = "/bin/sh"
		cmd = rubyChild(argv0, "sh", "-c", line)
	case len(words) == 0:
		return 1, couldNotExecute(syscall.ENOENT, ""), nil
	default:
		argv0 = words[0]
		cmd = rubyChild(argv0, argv0, words[1:]...)
	}
	status, output, err := runLogged(ctx, cmd, quoteOrRedact(line, false), line)
	if err == nil {
		return status, output, nil
	}
	var lookup *exec.Error
	if errors.As(err, &lookup) {
		// No executable of that name on PATH: Ruby execs the bare name and
		// execve says ENOENT.
		return 1, couldNotExecute(syscall.ENOENT, argv0), nil
	}
	var start *fs.PathError
	var errno syscall.Errno
	if !errors.As(err, &start) || start.Op != "fork/exec" || !errors.As(start.Err, &errno) {
		return 0, "", err
	}
	if errno == syscall.ENOEXEC {
		// A file with no #! line: Ruby retries it as a /bin/sh script
		// (process.c proc_exec_cmd), measured with Ruby 3.4.9.
		retry := rubyChild("/bin/sh", "sh", append([]string{cmd.Path}, cmd.Args[1:]...)...)
		return runCapped(ctx, retry, quoteOrRedact(line, false), (*exec.Cmd).Run)
	}
	return 1, couldNotExecute(errno, argv0), nil
}

// rubyChild is one process as runAsRubyExec starts it, with argv[0] set to
// arg0: "sh" for the shell, as Ruby passes it (measured: $0 is "sh"), and the
// word as written for a direct exec.
func rubyChild(name, arg0 string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	// A PATH entry of "." or "" finds the command relative to the cwd, which
	// os/exec refuses by default and Ruby's execvp does not.
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	cmd.Args[0] = arg0
	environ := os.Environ()
	env := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if _, dropped := rubyEnvDropped[name]; !dropped {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "LANG=C", "LC_ALL=C")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// couldNotExecute is the text of Ruby's SystemCallError for errno on argv0
// ("No such file or directory - nosuchcmd"), under util/execution.rb:393's
// prefix. Go's errno text is strerror's with a lower-case first letter.
func couldNotExecute(errno syscall.Errno, argv0 string) string {
	text := []rune(errno.Error())
	text[0] = unicode.ToUpper(text[0])
	return fmt.Sprintf("Could not execute posix command: %s - %s", string(text), argv0)
}

// validationTempName is the name Puppet's Uniquefile gives the temp file a
// validate_cmd sees (file_system/uniquefile.rb:102-116): the target's base
// name with no separator, the local date, the agent's pid, a random 32-bit
// number in base 36, and -n on the nth retry after EEXIST. It is not cosmetic:
// the path is substituted into the command, so it reaches the report in every
// failure message.
func validationTempName(base string, now time.Time, pid int, random uint32, retry int) string {
	name := base + now.Format("20060102") + "-" + strconv.Itoa(pid) + "-" + strconv.FormatUint(uint64(random), 36)
	if retry > 0 {
		name += "-" + strconv.Itoa(retry)
	}
	return name
}

// rubyShellMeta are the characters that make Ruby's Kernel.exec(String) hand
// the line to /bin/sh (process.c rb_exec_fillarg). Measured with Ruby 3.4.9
// under strace, every printable ASCII character tried in turn.
const rubyShellMeta = "*?{}[]<>()~&|\\$;'`\"\n#"

// rubyShellWords are the POSIX reserved words and special built-ins that, as
// the first word, also send the line to the shell (process.c posix_sh_cmds).
// Measured as above: each of these went to /bin/sh, while echo, test, true,
// cd, umask, wait, alias and command were executed directly.
var rubyShellWords = map[string]struct{}{
	"!": {}, ".": {}, ":": {}, "break": {}, "case": {}, "continue": {}, "do": {}, "done": {},
	"elif": {}, "else": {}, "esac": {}, "eval": {}, "exec": {}, "exit": {}, "export": {},
	"fi": {}, "for": {}, "if": {}, "in": {}, "readonly": {}, "return": {}, "set": {},
	"shift": {}, "then": {}, "times": {}, "trap": {}, "unset": {}, "until": {}, "while": {},
}

// rubyExecWords decides, as Ruby's Kernel.exec(String) does, whether line runs
// through `/bin/sh -c` (shell true) or is split on spaces and tabs and executed
// directly (shell false, with the words). A first word containing = before
// any / is a variable assignment and needs the shell. A line with no words
// runs nothing: Ruby raises ENOENT for it.
func rubyExecWords(line string) (words []string, shell bool) {
	if strings.ContainsAny(line, rubyShellMeta) {
		return nil, true
	}
	words = strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(words) == 0 {
		return []string{}, false
	}
	first := words[0]
	if slash := strings.IndexByte(first, '/'); slash >= 0 {
		first = first[:slash]
	} else if _, builtin := rubyShellWords[first]; builtin {
		return nil, true
	}
	if strings.Contains(first, "=") {
		return nil, true
	}
	return words, false
}

// validateValidateCmd is Validate's check of validate_cmd and
// validate_replacement.
//
// Puppet checks neither (type/file.rb:354-384, 428-459), so a non-string
// passes compilation and fails every change it validates, at sync: an Integer
// replacement with "wrong argument type Integer (expected Regexp)" (measured,
// x03c), a non-string command with NoMethodError on gsub (read). voxagent
// refuses both before anything applies (improvements.md I-20).
//
// validate_cmd with recurse is refused too, as voxagent's own: every generated
// child inherits it (type/file.rb:583-603), so the name of each file found on
// disk would be substituted into a command line Puppet may hand to /bin/sh.
func validateValidateCmd(r catalog.Resource) error {
	cmd, ok := r.Parameters["validate_cmd"]
	if !ok {
		return nil
	}
	if _, isString := cmd.(string); !isString {
		return fmt.Errorf(`%s: validate_cmd => %v (%T) is refused: Puppet accepts it and fails every change it validates with "undefined method 'gsub'"`, r.Ref(), cmd, cmd)
	}
	if v, ok := r.Parameters["validate_replacement"]; ok {
		if _, isString := v.(string); !isString {
			return fmt.Errorf(`%s: validate_replacement => %v (%T) is refused: Puppet accepts it and fails every change it validates with "wrong argument type"`, r.Ref(), v, v)
		}
	}
	if paramString(r, "recurse", "false") == "true" {
		return fmt.Errorf("%s: validate_cmd with recurse => true is not implemented: each generated file's name would be substituted into the command", r.Ref())
	}
	return nil
}
