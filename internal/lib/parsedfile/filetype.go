package parsedfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/runlog"
)

// uft: Puppet 8.33.0 lib/puppet/util/filetype.rb; cft: cron_core's
// lib/puppet/provider/cron/filetype.rb.

// FileType reads and writes the text a ParsedFile parses.
type FileType interface {
	// Read returns the text after the read wrapper (uft:39-53). Every error
	// it returns except a ReadError (uft:47-48) has already been logged as an
	// err line (Puppet.log_exception); callers must not log it again.
	Read(ctx context.Context) (string, error)
	// Write replaces the text. Every error it returns that wraps a failure
	// ("<class> could not write ...") has already been logged as an err line;
	// callers must not log it again. The crontab's "Cannot write the <t>
	// user's crontab" errors are not logged, as in Ruby (cft:84-86).
	Write(ctx context.Context, text string) error
}

// Backuper is implemented by the filetypes that can back a file up (flat only).
type Backuper interface {
	Backup(ctx context.Context) error
}

// ReadError is Puppet::Util::FileType::FileReadError (uft:13). Unlike the
// plain errors the wrapper raises, Ruby re-raises it untouched (uft:47-48).
type ReadError struct{ Msg string }

func (e ReadError) Error() string { return e.Msg }

const (
	flatClass    = "Puppet::Util::FileType::FileTypeFlat"
	crontabClass = "Puppet::Provider::Cron::FileType::FileTypeCrontab"

	encUTF8  = "UTF-8"
	encASCII = "US-ASCII"
)

var headerLine = regexp.MustCompile(`# HEADER.*\n`) // `.` excludes \n, as in Ruby

// stripHeader is the read wrapper's text handling (uft:42-45): every
// `# HEADER.*\n` is removed, even mid-line. Ruby's gsub raises on text that is
// invalid in Encoding.default_external, which the caller passes as encoding
// ("UTF-8" or "US-ASCII", rubyvalue.ExternalEncoding): under US-ASCII any byte
// above 0x7F is invalid. The raise is wrapped as uft:50 does.
func stripHeader(class, path, encoding, text string) (string, error) {
	if !validIn(encoding, text) {
		return "", fmt.Errorf("%s could not read %s: invalid byte sequence in %s", class, path, encoding)
	}
	return headerLine.ReplaceAllString(text, ""), nil
}

func validIn(encoding, text string) bool {
	if encoding == encASCII {
		return firstNonASCII(text) < 0
	}
	return utf8.ValidString(text)
}

// firstNonASCII is the index of the first byte above 0x7F, or -1.
func firstNonASCII(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return i
		}
	}
	return -1
}

// encodeError is the message Ruby's transcoding of text (UTF-8) to the
// external encoding raises when a write pipe or Tempfile is in encoding
// (uft:125, cft:89), or "" when text is written as it is. Only US-ASCII can
// fail: a byte above 0x7F is an Encoding::UndefinedConversionError
// ("U+00E9 from UTF-8 to US-ASCII") when it starts a valid character, else an
// Encoding::InvalidByteSequenceError in Ruby's forms ("\xFF" on UTF-8,
// "\xE9" followed by "y" on UTF-8, incomplete "\xE9" on UTF-8; measured with
// ruby 3.4.9).
func encodeError(encoding, text string) string {
	if encoding != encASCII {
		return ""
	}
	i := firstNonASCII(text)
	if i < 0 {
		return ""
	}
	if r, size := utf8.DecodeRuneInString(text[i:]); r != utf8.RuneError || size > 1 {
		return fmt.Sprintf("U+%04X from UTF-8 to US-ASCII", r)
	}
	// The invalid sequence is the longest prefix of a well-formed one.
	rest := text[i:]
	n := 1
	for ; n < len(rest) && n < utf8.UTFMax && continues(rest[0], n, rest[n]); n++ {
	}
	switch {
	case n == len(rest) && wellFormedPrefix(rest[0]):
		return fmt.Sprintf("incomplete %s on UTF-8", dumpBytes(rest[:n]))
	case n < len(rest) && n > 0 && wellFormedPrefix(rest[0]):
		return fmt.Sprintf("%s followed by %s on UTF-8", dumpBytes(rest[:n]), dumpBytes(rest[n:n+1]))
	}
	return fmt.Sprintf("%s on UTF-8", dumpBytes(rest[:1]))
}

// wellFormedPrefix says whether b can start a multi-byte UTF-8 character.
func wellFormedPrefix(b byte) bool { return b >= 0xC2 && b <= 0xF4 }

// continues says whether b is a valid byte at offset n of a UTF-8 sequence
// that starts with lead (the ranges of Table 3-7 of the Unicode standard).
func continues(lead byte, n int, b byte) bool {
	lo, hi := byte(0x80), byte(0xBF)
	if n == 1 {
		switch lead {
		case 0xE0:
			lo = 0xA0
		case 0xED:
			hi = 0x9F
		case 0xF0:
			lo = 0x90
		case 0xF4:
			hi = 0x8F
		}
	}
	if !wellFormedPrefix(lead) {
		return false
	}
	want := 2
	switch {
	case lead >= 0xF0:
		want = 4
	case lead >= 0xE0:
		want = 3
	}
	return n < want && b >= lo && b <= hi
}

var namedEscapes = map[byte]string{'\n': `\n`, '\r': `\r`, '\t': `\t`, '\f': `\f`, '\v': `\v`, '\a': `\a`, '\b': `\b`, 0x1b: `\e`}

// dumpBytes is the quoted form Ruby's error messages show bytes in.
func dumpBytes(b string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, c := range []byte(b) {
		switch {
		case c == '"' || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c >= 0x20 && c < 0x7F:
			sb.WriteByte(c)
		case strings.IndexByte("\n\r\t\f\v\a\b\x1b", c) >= 0:
			sb.WriteString(namedEscapes[c])
		default:
			fmt.Fprintf(&sb, "\\x%02X", c)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// logged is Puppet.log_exception for an error the wrapper raised (uft:51,
// :65): an err line from source Puppet, then the error itself.
func logged(ctx context.Context, err error) error {
	runlog.From(ctx).Log("err", "Puppet", err.Error())
	return err
}

type flat struct {
	path        string
	defaultMode fs.FileMode
	backup      func(ctx context.Context, path string) error
	relabel     func(ctx context.Context, path string) error
	encoding    string
}

// NewFlat is the flat filetype (uft:100-133) on path. A new file gets
// defaultMode when it is non-zero, else 0600, less the umask. backup and
// relabel are the consumer's filebucket and SELinux calls (this package may
// not import them); both are required.
func NewFlat(path string, defaultMode fs.FileMode, backup, relabel func(ctx context.Context, path string) error, encoding string) (FileType, error) {
	if backup == nil || relabel == nil {
		return nil, errors.New("NewFlat: backup and relabel hooks are required")
	}
	return &flat{path, defaultMode, backup, relabel, encoding}, nil
}

// exists is Puppet::FileSystem.exist?: it follows symlinks and treats every
// stat failure as absent.
func (f *flat) exists() bool {
	_, err := os.Stat(f.path)
	return err == nil
}

// Backup backs the file up when it exists (uft:102-104).
func (f *flat) Backup(ctx context.Context) error {
	if !f.exists() {
		return nil
	}
	return f.backup(ctx, f.path)
}

// Read is uft:107-115 behind the read wrapper: a missing file reads as "".
func (f *flat) Read(ctx context.Context) (string, error) {
	if !f.exists() {
		return "", nil
	}
	fail := func(err error, fn string) (string, error) {
		return "", logged(ctx, fmt.Errorf("%s could not read %s: %s", flatClass, f.path, rubyvalue.Errno(err, fn, f.path)))
	}
	file, err := os.Open(f.path)
	if err != nil {
		return fail(err, "rb_sysopen")
	}
	defer file.Close()
	b, err := io.ReadAll(file)
	if err != nil {
		return fail(err, "io_fread")
	}
	text, err := stripHeader(flatClass, f.path, f.encoding, string(b))
	if err != nil {
		return "", logged(ctx, err)
	}
	return text, nil
}

// WriteFailure is the step of the flat write that failed and its errno, for a
// failure FlatWriteError can render: "tempfile" (creating, writing or
// chmodding the temp file), "open" (opening the target), "copy" (copying onto
// it), "close" (closing it), or "tmpdir", errno 0, which only the asuser
// helper reports: Ruby's Dir.tmpdir found no temporary directory for the user
// ("could not find a temporary directory", measured in the oracle image).
// Write's errors for its steps carry one, reachable by errors.As, so the
// asuser helper can report a failure as <step> <errno> and its parent can
// render Write's text without any text from the helper (spec §3.4).
type WriteFailure struct {
	Step  string
	Errno syscall.Errno
}

func (f *WriteFailure) Error() string { return f.Step + ": " + f.Errno.Error() }

// writeError is a failed write step: Ruby's text, and the step for errors.As.
type writeError struct {
	msg     string
	failure *WriteFailure
}

func (e *writeError) Error() string { return e.msg }
func (e *writeError) Unwrap() error { return e.failure }

// FlatWriteError is the error Write returns, unlogged, for f on path. It is
// the text Write produces for that step and errno: Write uses the same
// formatter, except for an "open" failure on a directory: a directory target
// fails the prefetch (spec §6.3 item 5), but its user can swap one in after
// that, and then the helper's write opens a copy named after its temp file
// inside it, as Write does. Write's text names that copy; this one names path,
// since the helper reports no name (D-101).
func FlatWriteError(path string, f WriteFailure) error {
	return flatWriteFailure(path, path, f.Step, f.Errno)
}

// flatWriteFailure renders a failed step of the flat write on path, which
// opened dest: the errno as rubyvalue.Errno has it, alone for "tempfile" (and any
// other step), with rb_sysopen and dest for the open, and the syscall for the copy and the close ("No space left on device
// - write", measured with ruby 3.4.9 on /dev/full); "tmpdir" is Dir.tmpdir's
// text alone. The error carries the
// step's WriteFailure, with Errno 0 for an errno-less error, which none of the
// os calls Write makes returns.
func flatWriteFailure(path, dest, step string, err error) error {
	var detail string
	switch step {
	case "open":
		detail = rubyvalue.Errno(err, "rb_sysopen", dest)
	case "copy":
		detail = rubyvalue.Errno(err, "", "") + " - write"
	case "close":
		detail = rubyvalue.Errno(err, "", "") + " - close"
	case "tmpdir":
		detail = "could not find a temporary directory"
	default:
		detail = rubyvalue.Errno(err, "", "")
	}
	var errno syscall.Errno
	errors.As(err, &errno)
	return &writeError{
		msg:     fmt.Sprintf("%s could not write %s: %s", flatClass, path, detail),
		failure: &WriteFailure{Step: step, Errno: errno},
	}
}

// FlatEncodeError is the error Write returns, unlogged, when text cannot be
// written in encoding (encodeError), or nil. Write makes this check itself,
// after creating its temp file; the asuser helper's parent makes it before it
// starts the helper's write, since the helper's closed result has no step for
// it. When the temp file cannot be created either, Ruby reports that failure
// and the parent reports this one (D-101).
func FlatEncodeError(path, encoding, text string) error {
	if detail := encodeError(encoding, text); detail != "" {
		return fmt.Errorf("%s could not write %s: %s", flatClass, path, detail)
	}
	return nil
}

// Write is uft:123-132. The text goes into a new temp file first (mode
// defaultMode when non-zero, else 0600; under US-ASCII the transcoding error
// of encodeError fails here, before the target is touched), which is then
// copied over the target as FileUtils.cp does: the target is opened
// O_WRONLY|O_CREAT|O_TRUNC with the temp file's mode (used, less the umask,
// only when the file is created), following a symlink, so an existing file
// keeps its inode, mode and owner. A target that is a directory receives the
// copy as a new file named after the temp file (puppet<random>; measured with
// ruby 3.4.9). Not atomic. Errors are logged as described on FileType; a
// failed step's error carries a *WriteFailure (FlatWriteError).
func (f *flat) Write(ctx context.Context, text string) error {
	dest := f.path
	fail := func(step string, err error) error {
		return logged(ctx, flatWriteFailure(f.path, dest, step, err))
	}
	tmp, err := os.CreateTemp("", "puppet")
	if err != nil {
		return fail("tempfile", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := FlatEncodeError(f.path, f.encoding, text); err != nil {
		return logged(ctx, err)
	}
	if _, err := io.WriteString(tmp, text); err != nil {
		return fail("tempfile", err)
	}
	mode := f.defaultMode
	if mode == 0 {
		mode = 0o600
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail("tempfile", err)
	}

	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		dest = filepath.Join(dest, filepath.Base(tmp.Name()))
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fail("open", err)
	}
	_, err = tmp.Seek(0, io.SeekStart)
	if err == nil {
		_, err = io.Copy(out, tmp)
	}
	if err != nil {
		_ = out.Close()
		return fail("copy", err)
	}
	if err := out.Close(); err != nil {
		return fail("close", err)
	}
	if err := f.relabel(ctx, f.path); err != nil {
		return logged(ctx, fmt.Errorf("%s could not write %s: %w", flatClass, f.path, err))
	}
	return nil
}

type crontabFile struct {
	user     string
	crontab  string // the resolved executable
	uid      int64  // Ruby's @uid; meaningful when hasUID
	hasUID   bool
	run      command.ShadowRunner
	ns       nameservice.NameService
	stderr   io.Writer
	encoding string
}

// NewCrontab is the Linux crontab filetype (cft:28-103) for user, running the
// executable at path crontab. The user's uid is looked up here, once, as
// cft:35 does for @uid; a lookup error is a ReadError (cft:37). Write copies
// the command's output to stderr, as Ruby's child inherits the agent's
// (D-95).
func NewCrontab(ctx context.Context, user, crontab string, run command.ShadowRunner, ns nameservice.NameService, stderr io.Writer, encoding string) (FileType, error) {
	uid, ok, err := nameservice.PosixUID(ctx, ns, user, false)
	if err != nil {
		return nil, ReadError{fmt.Sprintf("Could not retrieve user %s: %s", user, err)}
	}
	return &crontabFile{user, crontab, uid, ok, run, ns, stderr, encoding}, nil
}

// argv is cft:98-102 ("crontab" or "crontab -u <user>") plus tail. Ruby
// compares @uid, nil for a missing user, with the real uid.
func (c *crontabFile) argv(tail string) []string {
	if c.hasUID && c.uid == int64(os.Getuid()) {
		return []string{"crontab", tail}
	}
	return []string{"crontab", "-u", c.user, tail}
}

// Read is cft:46-65 behind the read wrapper. It looks the user up again, as
// cft:47 does; an NSS error there is the target's ReadError, since only
// (false, nil) means "no such user".
func (c *crontabFile) Read(ctx context.Context) (string, error) {
	_, ok, err := nameservice.PosixUID(ctx, c.ns, c.user, false)
	if err != nil {
		return "", c.readError(err)
	}
	if !ok {
		runlog.Debugf(ctx, "Puppet", "The %s user does not exist. Treating their crontab file as empty in case Puppet creates them in the middle of the run.", c.user)
		return "", nil
	}
	argv := c.argv("-l")
	code, out, err := c.run.Run(ctx, command.ArgvOpts{FailOnTruncate: true, ErrorName: "crontab"}, c.crontab, argv[1:]...)
	if err == nil && code != 0 {
		err = command.ExecFailure(argv, false, false, code, out)
	}
	if err != nil {
		switch detail := err.Error(); {
		case strings.Contains(detail, "no crontab for"):
			return "", nil
		case strings.Contains(detail, "are not allowed to"):
			runlog.Debugf(ctx, "Puppet", "The %s user is not authorized to use cron. Their crontab file is treated as empty in case Puppet authorizes them in the middle of the run (by, for example, modifying the cron.deny or cron.allow files).", c.user)
			return "", nil
		}
		return "", c.readError(err)
	}
	text, err := stripHeader(crontabClass, c.user, c.encoding, out)
	if err != nil {
		return "", logged(ctx, err)
	}
	return text, nil
}

func (c *crontabFile) readError(err error) error {
	return ReadError{fmt.Sprintf("Could not read crontab for %s: %s", c.user, err)}
}

// Write is cft:83-92. The exit status is not checked: cronie's refusals reach
// the user only through the output, which goes to stderr after the command
// exits. Ruby's popen pipe is in default_external, so under US-ASCII a
// non-ASCII character raises before anything is written; Ruby then lets
// crontab install an empty crontab, D-97 does not run it at all.
func (c *crontabFile) Write(ctx context.Context, text string) error {
	fail := func(detail any) error {
		return logged(ctx, fmt.Errorf("%s could not write %s: %v", crontabClass, c.user, detail))
	}
	_, ok, err := nameservice.PosixUID(ctx, c.ns, c.user, false)
	if err != nil {
		return fmt.Errorf("Cannot write the %s user's crontab: %w", c.user, err)
	}
	if !ok {
		return fmt.Errorf("Cannot write the %s user's crontab: The user does not exist", c.user)
	}
	if detail := encodeError(c.encoding, text); detail != "" {
		return fail(detail)
	}
	argv := c.argv("-")
	// cft:86's popen child inherits HOME, so cronie saves its backup of the
	// replaced crontab under it, as it does under Ruby.
	_, out, err := c.run.Run(ctx, command.ArgvOpts{Stdin: []byte(text), KeepUserEnv: true, ErrorName: "crontab"}, c.crontab, argv[1:]...)
	if err != nil {
		return fail(err)
	}
	// Ruby's child writes to the agent's inherited stderr and cannot fail the
	// flush there; crontab has installed the text by now, so neither can this.
	_, _ = io.WriteString(c.stderr, out)
	return nil
}
