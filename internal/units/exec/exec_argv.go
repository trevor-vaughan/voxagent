package exec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// argvOf flattens an argv command and renders each element as Ruby's to_s
// does (util/execution.rb:156-161): strings as is, integral numbers as
// integers, bools, nil as "".
func argvOf(command []any) []string {
	var out []string
	for _, el := range command {
		switch v := el.(type) {
		case []any:
			out = append(out, argvOf(v)...)
		case string:
			out = append(out, v)
		case nil:
			out = append(out, "")
		default:
			out = append(out, rubyvalue.InspectValue(v))
		}
	}
	return out
}

// argvExe is provider/exec.rb extractexe for an argv: its first element,
// whole and unsplit, as Ruby's string interpolation renders it.
// An empty argv's first element is nil, which renders as "".
func argvExe(command []any) string {
	if len(command) == 0 {
		return ""
	}
	switch v := command[0].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return rubyvalue.InspectValue(v)
	}
}

// commandDisplay is how Puppet names a command in its messages: a string as
// written, an argv as Array#inspect of the original value.
func commandDisplay(command any) string {
	if s, ok := command.(string); ok {
		return s
	}
	return rubyvalue.InspectValue(command)
}

// checkExe is provider/exec/posix.rb:30-54 for exe under the resource's
// path (pathList, "" when the resource has none).
//
// An exe File.expand_path leaves unchanged is checked where it is. Any other
// goes to Puppet::Util.which (util.rb:207-243) under the resource's path,
// which accepts a "/"-prefixed exe as it is and otherwise tries
// File.expand_path(File.join(dir, exe)) for each entry. So a "." entry is the
// AGENT's working directory and an empty one is "/" (measured), unlike
// the child's own search (lookPathIn).
//
// redact shows exe as [command redacted] in the errors, quotes included, for
// a marked command (D-78: Ruby leaks it).
func checkExe(exe, pathList string, redact bool) error {
	shown := command.QuoteOrRedact(exe, redact)
	notFound := fmt.Errorf("Could not find command %s", shown)
	if rubyAbsolute(exe) {
		fi, err := os.Stat(exe)
		switch {
		case err != nil:
			return notFound
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is a %s, not a file", shown, rubyFtype(exe))
		case !command.Executable(exe, fi):
			return fmt.Errorf("%s is not executable", shown)
		}
		return nil
	}
	if pathList == "" {
		return notFound
	}
	if strings.HasPrefix(exe, "/") {
		if command.ExecutableFile(exe) {
			return nil
		}
		return notFound
	}
	for _, dir := range rubyvalue.SplitPath(pathList) {
		dest, err := filepath.Abs(dir + "/" + exe)
		if err == nil && command.ExecutableFile(dest) {
			return nil
		}
	}
	return notFound
}

// rubyAbsolute is posix.rb:33's File.expand_path(exe) == exe. expand_path
// keeps a leading "//" (measured: //bin/true runs), so a path that is
// clean below its first slash is absolute too.
func rubyAbsolute(exe string) bool {
	if !filepath.IsAbs(exe) {
		return false
	}
	clean := filepath.Clean(exe)
	return clean == exe || (strings.HasPrefix(exe, "//") && !strings.HasPrefix(exe, "///") && "/"+clean == exe)
}

// rubyFtype is File.ftype, which reads lstat: a link to a directory is a
// "link" (measured).
func rubyFtype(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return "unknown"
	}
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		return "link"
	case m.IsDir():
		return "directory"
	case m&fs.ModeCharDevice != 0:
		return "characterSpecial"
	case m&fs.ModeDevice != 0:
		return "blockSpecial"
	case m&fs.ModeNamedPipe != 0:
		return "fifo"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m.IsRegular():
		return "file"
	default:
		return "unknown"
	}
}

// lookPathIn finds file in the colon-separated pathList as execvp would. An
// empty entry or "." means the working directory; it returns that hit as
// "./"+file so exec.Cmd resolves it against Cmd.Dir. dir is that Cmd.Dir, the
// directory any relative entry is looked up in ("" for the agent's own).
func lookPathIn(file, pathList, dir string) (string, bool) {
	for _, entry := range strings.Split(pathList, ":") {
		candidate := filepath.Join(entry, file)
		if !strings.Contains(candidate, "/") {
			candidate = "./" + candidate
		}
		target := candidate
		if !filepath.IsAbs(target) && dir != "" {
			target = filepath.Join(dir, target)
		}
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() && command.Executable(target, fi) {
			return candidate, true
		}
	}
	return "", false
}
