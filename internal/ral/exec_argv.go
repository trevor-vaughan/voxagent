package ral

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
			out = append(out, rubyInspectValue(v))
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
		return rubyInspectValue(v)
	}
}

// commandDisplay is how Puppet names a command in its messages: a string as
// written, an argv as Array#inspect of the original value.
func commandDisplay(command any) string {
	if s, ok := command.(string); ok {
		return s
	}
	return rubyInspectValue(command)
}

// quoteOrRedact is how a command appears in an error: quoted, or
// [command redacted] when a marked parameter carries it.
func quoteOrRedact(s string, redact bool) string {
	if redact {
		return "[command redacted]"
	}
	return "'" + s + "'"
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
	shown := quoteOrRedact(exe, redact)
	notFound := fmt.Errorf("Could not find command %s", shown)
	if rubyAbsolute(exe) {
		fi, err := os.Stat(exe)
		switch {
		case err != nil:
			return notFound
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is a %s, not a file", shown, rubyFtype(exe))
		case !executable(exe, fi):
			return fmt.Errorf("%s is not executable", shown)
		}
		return nil
	}
	if pathList == "" {
		return notFound
	}
	if strings.HasPrefix(exe, "/") {
		if executableFile(exe) {
			return nil
		}
		return notFound
	}
	for _, dir := range rubySplitPath(pathList) {
		dest, err := filepath.Abs(dir + "/" + exe)
		if err == nil && executableFile(dest) {
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

// rubySplitPath is String#split(":") on the joined path, which keeps leading
// and inner empty entries and drops trailing ones.
func rubySplitPath(pathList string) []string {
	entries := strings.Split(pathList, ":")
	for len(entries) > 0 && entries[len(entries)-1] == "" {
		entries = entries[:len(entries)-1]
	}
	return entries
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

// executable is File.executable?, which is eaccess(2): any execute bit for
// an effective uid of 0, else faccessat(2) with X_OK checked against the
// EFFECTIVE uid and gid (AT_EACCESS), not access(2)'s real ones.
func executable(path string, fi fs.FileInfo) bool {
	if os.Geteuid() == 0 {
		return fi.Mode().Perm()&0o111 != 0
	}
	// Linux's values, which package syscall does not export: <fcntl.h>
	// AT_FDCWD and AT_EACCESS, <unistd.h> X_OK. syscall.Faccessat honours the
	// flag through faccessat2, or emulates it from fstatat on older kernels.
	const atFDCWD, atEACCESS, xOK = -100, 0x200, 0x1
	return syscall.Faccessat(atFDCWD, path, xOK, atEACCESS) == nil
}

// executableFile is which's FileTest.file? && FileTest.executable?.
func executableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && executable(path, fi)
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
		if fi, err := os.Stat(target); err == nil && !fi.IsDir() && executable(target, fi) {
			return candidate, true
		}
	}
	return "", false
}
