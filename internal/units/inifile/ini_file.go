package inifile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/lib/rubyregexp"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// if: puppetlabs-inifile lib/puppet/util/ini_file.rb (6.5.0's line numbers,
// or 6.6.1's for remove_section, which 6.5.0 lacks);
// sec: util/ini_file/section.rb; ei: util/external_iterator.rb.

// RubyError is an exception Ruby raises that Puppet reports by its message:
// the port returns it with the class Ruby gave it, so a caller can tell it
// from an I/O failure.
type RubyError struct{ Class, Message string }

func (e *RubyError) Error() string { return e.Message }

// noMethodOnNil is the NoMethodError Ruby 3.2 (the oracle's) raises for a call
// on nil (measured, NOTES M2).
func noMethodOnNil(method string) error {
	return &RubyError{Class: "NoMethodError", Message: "undefined method `" + method + "' for nil:NilClass"}
}

// rubyLines is IniFile's @lines, a Ruby Array of Strings. Its elements can be
// nil: the section line bookkeeping can run off the end of the array, and Ruby
// then reads nil, pads with nil or counts from the end, all of which the
// methods below reproduce.
type rubyLines []*string

// at is Array#[] with an Integer: nil out of range, negative from the end.
func (l rubyLines) at(i int) *string {
	if i < 0 {
		i += len(l)
	}
	if i < 0 || i >= len(l) {
		return nil
	}
	return l[i]
}

// deleteAt is Array#delete_at: out of range deletes nothing.
func (l *rubyLines) deleteAt(i int) {
	if i < 0 {
		i += len(*l)
	}
	if i < 0 || i >= len(*l) {
		return
	}
	*l = slices.Delete(*l, i, i+1)
}

// insert is Array#insert(i, s): a negative index counts from the end and
// inserts after that element, an index past the end pads with nil.
func (l *rubyLines) insert(i int, s string) error {
	n := len(*l)
	switch {
	case i == -1:
		i = n
	case i < 0:
		if i < -n-1 {
			return &RubyError{Class: "IndexError", Message: fmt.Sprintf("index %d too small for array; minimum: %d", i, -n-1)}
		}
		i += n + 1
	}
	for len(*l) < i {
		*l = append(*l, nil)
	}
	*l = slices.Insert(*l, i, &s)
	return nil
}

// bounds resolves the Range first..last (last nil: endless; first nil:
// beginless) as Array#[] and #slice! do: the start and length of the elements
// it selects, or false for nil (a start before the array's first element or
// after its end).
func (l rubyLines) bounds(first, last *int) (start, n int, ok bool) {
	size := len(l)
	if first != nil {
		start = *first
	}
	if start < 0 {
		start += size
		if start < 0 {
			return 0, 0, false
		}
	}
	if start > size {
		return 0, 0, false
	}
	end := size - 1
	if last != nil {
		end = *last
		if end < 0 {
			end += size
		}
	}
	n = max(end+1-start, 0)
	n = min(n, size-start)
	return start, n, true
}

// sliceBang is Array#slice!(first..last).
func (l *rubyLines) sliceBang(first, last *int) {
	if start, n, ok := l.bounds(first, last); ok {
		*l = slices.Delete(*l, start, start+n)
	}
}

// IniFile is Puppet::Util::IniFile: a file's lines, edited in place, with the
// sections found in them and the line numbers each section spans.
type IniFile struct {
	path            string
	keyValSeparator string
	sectionPrefix   string
	sectionSuffix   string
	indentChar      string
	indentWidth     *int

	sectionRegex   *regexp.Regexp
	settingRegex   *regexp.Regexp
	commentedRegex *regexp.Regexp

	sectionNames []string
	sections     map[string]*section
	lines        rubyLines
}

var (
	// spaceSet is the inside of rubyregexp.Space, for use in a character
	// class, where RE2's \s lacks Ruby's \v.
	spaceSet = strings.Trim(rubyregexp.Space, "[]")

	// if:14: `^\s+$`, on an arbitrary string (so line-anchored, as in Ruby).
	allWhitespace = regexp.MustCompile(`(?m)^` + rubyregexp.Space + `+$`)
	// if:165, 6.6.x if:303: `^\s*$` on one line. Ruby's ^ does not match
	// after the line's final "\n"; RE2's (?m)^ does, so the line is anchored
	// as a whole instead, which is the same for a single line.
	blankLine = regexp.MustCompile(`\A` + rubyregexp.Space + `*\z`)
)

// openIniFile is IniFile.new (if:11-30): it reads path as readlines does and
// parses it. indentWidth is already Ruby's to_i of the given value. encoding
// is Encoding.default_external ("UTF-8" or "US-ASCII", as
// rubyvalue.ExternalEncoding), which decides what readlines hands the regexes.
func openIniFile(path, keyValSeparator, sectionPrefix, sectionSuffix, indentChar string, indentWidth *int, encoding string) (*IniFile, error) {
	f := &IniFile{
		path:            path,
		keyValSeparator: keyValSeparator,
		sectionPrefix:   sectionPrefix,
		sectionSuffix:   sectionSuffix,
		indentChar:      indentChar,
		indentWidth:     indentWidth,
		sections:        map[string]*section{},
	}
	kvs := regexpKeyValSeparator(keyValSeparator)
	// Kept from Ruby (if:22): `(.*)` keeps a CRLF file's "\r" in every value,
	// so a value never equals the desired one (spec 7.3).
	// Kept from Ruby (if:22-23): k_v_s is interpolated into the regexps and
	// into a character class without Regexp.escape, so a separator with
	// regexp syntax is regexp syntax.
	var err error
	// (?m) is for $; \A, not ^, anchors the line start because Ruby's ^ does
	// not match after the line's final "\n" and RE2's (?m)^ does (see blankLine).
	f.sectionRegex, err = compile(`(?m)\A` + rubyregexp.Space + `*` + regexp.QuoteMeta(sectionPrefix) + `(.*)` + regexp.QuoteMeta(sectionSuffix) + rubyregexp.Space + `*$`) // if:32-40
	if err != nil {
		return nil, err
	}
	f.settingRegex, err = compile(`(?m)\A(` + rubyregexp.Space + `*)([^#;` + spaceSet + `]|[^#;` + spaceSet + `].*?[^` + spaceSet + kvs + `])(` +
		rubyregexp.Space + `*` + kvs + `[ \t]*)(.*)` + rubyregexp.Space + `*$`) // if:22
	if err != nil {
		return nil, err
	}
	f.commentedRegex, err = compile(`(?m)\A(` + rubyregexp.Space + `*)[#;]+(` + rubyregexp.Space + `*)(.*?[^` + spaceSet + kvs + `])(` +
		rubyregexp.Space + `*` + kvs + `[ \t]*)(.*)` + rubyregexp.Space + `*$`) // if:23
	if err != nil {
		return nil, err
	}
	if err := f.parseFile(encoding); err != nil {
		return nil, err
	}
	return f, nil
}

// regexpKeyValSeparator is k_v_s as if:14 derives it from the key_val_separator
// the resource gave: a space for a whitespace-only one, else the stripped
// text. Route tests the same text for regexp syntax (spec 2, ruling R6).
func regexpKeyValSeparator(keyValSeparator string) string {
	if allWhitespace.MatchString(keyValSeparator) {
		return " "
	}
	return rubyvalue.Strip(keyValSeparator)
}

// compile builds one of the regexps, as Ruby's Regexp literal does. A pattern
// RE2 rejects is Ruby's RegexpError in spirit only: the text is Go's.
func compile(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, &RubyError{Class: "RegexpError", Message: err.Error()}
	}
	return re, nil
}

// SectionNames is attr_reader :section_names (if:42).
func (f *IniFile) SectionNames() []string { return f.sectionNames }

// IsSection is IniFile#section? (if:51-53).
func (f *IniFile) IsSection(name string) bool {
	_, ok := f.sections[name]
	return ok
}

// GetValue is IniFile#get_value (if:55-57): the setting's values, or nil (Ruby
// nil, as opposed to an empty array) when the section or setting is absent.
func (f *IniFile) GetValue(sectionName string, setting *string) []string {
	if sec, ok := f.sections[sectionName]; ok {
		return sec.getValue(keyOf(setting))
	}
	return nil
}

// SetSection is IniFile#set_value with one argument (if:59-62): the section
// only.
func (f *IniFile) SetSection(sectionName string) {
	// The one-argument form cannot fail: with no setting there is no
	// commented line to insert.
	_ = f.SetValue(sectionName, nil, "", nil)
}

// SetValue is IniFile#set_value with four arguments (if:59-117); the
// three-argument form is not reachable from the providers. An error is Ruby's
// exception, raised with the state Ruby leaves behind.
func (f *IniFile) SetValue(sectionName string, setting *string, separator string, value []string) error {
	value = append([]string{}, value...) // if:70: value = [*value]; never nil

	if _, ok := f.sections[sectionName]; !ok { // if:77
		f.addSection(newSection(sectionName, nil, nil, settingMap{}, nil))
	}
	sec := f.sections[sectionName]
	key := keyOf(setting)

	if sec.existingSetting(key) { // if:81
		existing := sec.getValue(key)
		sectionIndex := slices.Index(f.sectionNames, sectionName)

		if err := f.updateLine(sec, *setting, value); err != nil {
			return err
		}
		sec.updateExistingSetting(key, value)

		if len(value) > len(existing) {
			return f.incrementSectionLineNumbers(sectionIndex+1, len(value)-len(existing))
		} else if len(value) < len(existing) {
			return f.decrementSectionLineNumbers(sectionIndex+1, len(existing)-len(value))
		}
		return nil
	}
	// if:93: a commented-out version of a new setting is where it goes.
	if commented := f.findCommentedSetting(sec, setting); commented != nil {
		if err := f.insertInlineSettingLine(commented, sec, *setting, separator, value); err != nil {
			return err
		}
		sec.insertInlineSetting(key, value)
		sectionIndex := slices.Index(f.sectionNames, sectionName)
		return f.incrementSectionLineNumbers(sectionIndex+1, len(value))
	}
	// Kept from Ruby (if:114): the guard `!setting.nil? || !value.nil?` is
	// always true, because value was splatted into an Array at if:70, so a
	// section-only set_value stores the key nil with no values.
	sec.setAdditionalSetting(key, value)
	return nil
}

// RemoveSetting is IniFile#remove_setting (6.6.1 if:119-140) with
// remove_empty_section false, which is what ini_setting and ini_subsetting
// pass; the true branch is not ported, as no provider reaches it. A missing section is Ruby's
// NoMethodError for `existing_setting?` on nil (spec 7.9).
func (f *IniFile) RemoveSetting(sectionName string, setting *string) error {
	sec := f.sections[sectionName] // if:120
	if sec == nil {
		// Kept from Ruby (if:121): remove_setting calls existing_setting? on
		// the nil of a missing section, so destroy and a refresh with
		// ensure => absent fail there instead of finding nothing to remove.
		return noMethodOnNil("existing_setting?")
	}
	key := keyOf(setting)
	if !sec.existingSetting(key) {
		// Kept from Ruby (if:121): a section-only destroy (setting nil) finds
		// nothing to remove and edits nothing, yet the provider's save still
		// rewrites the file (spec 7.2, 7.10).
		return nil
	}
	existing := sec.getValue(key)

	f.removeLine(sec, *setting)
	sec.removeExistingSetting(key)

	sectionIndex := slices.Index(f.sectionNames, sectionName)
	return f.decrementSectionLineNumbers(sectionIndex+1, len(existing))
}

// RemoveSection is IniFile#remove_section (6.6.1 if:142-153). Ruby raises for
// a section created in this run (no end line, so no length); the lines are
// sliced first, as in Ruby.
func (f *IniFile) RemoveSection(sectionName string) error {
	sec := f.sections[sectionName]
	if sec == nil {
		return nil
	}
	f.lines.sliceBang(sec.startLine, sec.endLine)

	sectionIndex := slices.Index(f.sectionNames, sec.name)
	length, err := sec.length()
	if err != nil {
		return err
	}
	if err := f.decrementSectionLineNumbers(sectionIndex+1, length); err != nil {
		return err
	}
	f.sectionNames = slices.Delete(f.sectionNames, sectionIndex, sectionIndex+1)
	delete(f.sections, sec.name)
	return nil
}

// Save is IniFile#save (if:139-199): it truncates the file in place and writes
// every section with puts. A failure while writing leaves the file as written
// so far, as Ruby's block-form File.open does.
func (f *IniFile) Save() error {
	global := f.sections[""] // if:140
	if global == nil {
		return noMethodOnNil("empty?")
	}
	globalEmpty := global.empty() && len(global.additionalSettings.keys) == 0

	// Spec 6: File.open(path, 'w') is O_WRONLY|O_CREAT|O_TRUNC, mode 0666 before
	// the umask, following a symlink.
	fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return errors.New(rubyvalue.Errno(err, "rb_sysopen", f.path))
	}
	var out bytes.Buffer
	buildErr := f.write(&out, globalEmpty)
	_, writeErr := fh.Write(out.Bytes())
	closeErr := fh.Close()
	for _, err := range []error{buildErr, writeErr, closeErr} {
		if err != nil {
			var re *RubyError
			if errors.As(err, &re) {
				return err
			}
			// The text for a failed write or close (ENOSPC, EIO) is unmeasured,
			// so it carries no "@ io_write" suffix.
			return errors.New(rubyvalue.Errno(err, "", f.path))
		}
	}
	return nil
}

// puts is IO#puts for a String: the line, and a newline unless it has one.
func puts(out *bytes.Buffer, line string) {
	out.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		out.WriteByte('\n')
	}
}

// indent is `@indent_char * (@indent_width || section.indentation || 0)`
// (if:180, 320).
func (f *IniFile) indent(sec *section) (string, error) {
	n := 0
	switch {
	case f.indentWidth != nil:
		n = *f.indentWidth
	case sec.indentation != nil:
		n = *sec.indentation
	}
	if n < 0 {
		// Kept from Ruby (if:180): String#* with a negative width raises, in
		// save after the file was truncated.
		return "", &RubyError{Class: "ArgumentError", Message: "negative argument"}
	}
	return strings.Repeat(f.indentChar, n), nil
}

// write is the body of save's File.open block (if:142-197).
func (f *IniFile) write(out *bytes.Buffer, globalEmpty bool) error {
	for index, name := range f.sectionNames {
		sec := f.sections[name]
		if sec == nil {
			return noMethodOnNil("new_section?")
		}

		// A buffer to cache lines that are only whitespace.
		var whitespaceBuffer []string

		if sec.isNew() && !sec.global() {
			// Kept from Ruby (if:151): a blank line precedes a new section
			// only after a non-empty global section or another section.
			if (index == 1 && !globalEmpty) || index > 1 {
				puts(out, "")
			}
			puts(out, f.sectionPrefix+sec.name+f.sectionSuffix)
		}

		if !sec.isNew() {
			// Write all of the pre-existing lines.
			for lineNum := *sec.startLine; lineNum <= *sec.endLine; lineNum++ {
				line := f.lines.at(lineNum)
				if line == nil {
					return noMethodOnNil("match?")
				}
				// Kept from Ruby (if:161-173): trailing blank lines of a
				// section are held back so new settings land before them.
				if blankLine.MatchString(*line) {
					whitespaceBuffer = append(whitespaceBuffer, *line)
					continue
				}
				flushBuffer(&whitespaceBuffer, out)
				puts(out, *line)
			}
		}

		// Write new settings, if there are any.
		for _, key := range sec.additionalSettings.keys {
			for _, v := range sec.additionalSettings.values[key] {
				indent, err := f.indent(sec)
				if err != nil {
					return err
				}
				puts(out, indent+key.name+f.keyValSeparator+v)
			}
		}

		if len(whitespaceBuffer) > 0 {
			flushBuffer(&whitespaceBuffer, out)
		} else if sec.isNew() && len(sec.additionalSettings.keys) > 0 && index < len(f.sectionNames)-1 {
			// Kept from Ruby (if:186-196): a new section with settings, with
			// more sections after it, ends with one blank line.
			puts(out, "")
		}
	}
	return nil
}

// flushBuffer is IniFile#flush_buffer_to_file (if:344-349).
func flushBuffer(buffer *[]string, out *bytes.Buffer) {
	for _, l := range *buffer {
		puts(out, l)
	}
	*buffer = nil
}

// addSection is IniFile#add_section (if:203-206).
//
// Kept from Ruby (if:204-205): a second header of the same name replaces the
// first in the hash but is appended to the names again, so save writes that
// section's lines once per header and the line numbers of the sections after
// the first are adjusted twice. A header with an empty name ("[]") is a second
// global section in the same way.
func (f *IniFile) addSection(sec *section) {
	f.sections[sec.name] = sec
	f.sectionNames = append(f.sectionNames, sec.name)
}

// externalIterator is ExternalIterator (ei:) over the file's lines.
type externalIterator struct {
	lines rubyLines
	cur   int
}

// next is ExternalIterator#next (ei:13-16); the index is -1 at the end.
func (it *externalIterator) next() (string, int) {
	it.cur++
	return it.itemAt(it.cur)
}

// peek is ExternalIterator#peek (ei:18-20).
func (it *externalIterator) peek() (string, int) { return it.itemAt(it.cur + 1) }

func (it *externalIterator) itemAt(index int) (string, int) {
	if len(it.lines) > index {
		return *it.lines[index], index
	}
	return "", -1
}

// parseFile is IniFile#parse_file (if:208-224).
func (f *IniFile) parseFile(encoding string) error {
	if err := f.readLines(encoding); err != nil {
		return err
	}
	iter := &externalIterator{lines: f.lines, cur: -1}

	// We always create a "global" section at the beginning of the file, for
	// anything that appears before the first named section.
	f.addSection(f.readSection("", 0, iter))
	line, lineNum := iter.next()

	for lineNum >= 0 {
		if m := f.sectionRegex.FindStringSubmatch(line); m != nil {
			f.addSection(f.readSection(m[1], lineNum, iter))
		}
		line, lineNum = iter.next()
	}
	return nil
}

// readSection is IniFile#read_section (if:226-249).
func (f *IniFile) readSection(name string, startLine int, iter *externalIterator) *section {
	start := startLine
	var settings settingMap
	// Ruby's start and end line are two Integers: the pointers must not alias
	// (an empty section's end line starts out equal to its start line).
	end := startLine
	endLineNum := &end
	var minIndentation *int
	empty := true
	for {
		line, lineNum := iter.peek()
		if lineNum < 0 || f.sectionRegex.MatchString(line) {
			// The global section always exists, even when it is empty; when it
			// is empty, it must be thought of as new, which is signalled with
			// a nil ending line.
			if name == "" && empty {
				endLineNum = nil
			}
			return newSection(name, &start, endLineNum, settings, minIndentation)
		}
		if m := f.settingRegex.FindStringSubmatch(line); m != nil {
			key := settingKey{name: m[2]}
			values, _ := settings.get(key)
			if values == nil {
				values = []string{}
			}
			settings.set(key, append(values, m[4]))
			indentation := len(m[1])
			if minIndentation != nil {
				indentation = min(indentation, *minIndentation)
			}
			minIndentation = &indentation
		}
		n := lineNum
		endLineNum = &n
		empty = false
		iter.next()
	}
}

// updateLine is IniFile#update_line (if:251-265): walking the section
// backwards, every line of the setting is deleted and the new values take the
// place of the last one found.
func (f *IniFile) updateLine(sec *section, setting string, value []string) error {
	updated := false
	for lineNum := *sec.endLine; lineNum >= *sec.startLine; lineNum-- {
		m := f.matchSetting(f.settingRegex, lineNum)
		if m == nil || m[2] != setting {
			continue
		}
		f.lines.deleteAt(lineNum)
		// Kept from Ruby (if:258): the values go where the first copy found
		// from the end, the last one in the file, was; every other copy is
		// deleted.
		if updated {
			continue
		}
		for i, v := range value {
			if err := f.lines.insert(lineNum+i, m[1]+m[2]+m[3]+v); err != nil {
				return err
			}
		}
		updated = true
	}
	return nil
}

// matchSetting matches regex against line lineNum; nil Ruby lines (out of
// range) match nothing, as Regexp#match(nil) is nil.
func (f *IniFile) matchSetting(re *regexp.Regexp, lineNum int) []string {
	line := f.lines.at(lineNum)
	if line == nil {
		return nil
	}
	return re.FindStringSubmatch(*line)
}

// removeLine is IniFile#remove_line (if:267-273).
func (f *IniFile) removeLine(sec *section, setting string) {
	for lineNum := *sec.endLine; lineNum >= *sec.startLine; lineNum-- {
		if m := f.matchSetting(f.settingRegex, lineNum); m != nil && m[2] == setting {
			f.lines.deleteAt(lineNum)
		}
	}
}

// readLines is IniFile.readlines (if:285-292): File.file? false (missing,
// directory, socket) means no lines. The encoding check stands for parse_file
// matching every line against the section regex, which raises on the first
// line invalid in Encoding.default_external, before anything else happens.
func (f *IniFile) readLines(encoding string) error {
	info, err := os.Stat(f.path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return errors.New(rubyvalue.Errno(err, "rb_sysopen", f.path))
	}
	defer func() { _ = fh.Close() }() // read-only: a close error loses nothing
	data, err := io.ReadAll(fh)
	if err != nil {
		return errors.New(rubyvalue.Errno(err, "io_fillbuf", f.path))
	}
	text := string(data)
	// Kept from Ruby (if:291): under US-ASCII (LANG unset) any byte >= 0x80,
	// valid UTF-8 and comments included, is an invalid byte sequence (spec 7.12).
	if !validIn(encoding, text) {
		return &RubyError{Class: "ArgumentError", Message: "invalid byte sequence in " + encoding}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		if line != "" {
			l := line
			f.lines = append(f.lines, &l)
		}
	}
	return nil
}

// validIn reports whether text is valid in Encoding.default_external.
func validIn(encoding, text string) bool {
	if encoding == "US-ASCII" {
		for i := 0; i < len(text); i++ {
			if text[i] >= utf8.RuneSelf {
				return false
			}
		}
		return true
	}
	return utf8.ValidString(text)
}

// commentedSetting is find_commented_setting's hash: the line holding the
// commented version of a setting, and its match for mimicking the whitespace.
type commentedSetting struct {
	match   []string
	lineNum int
}

// findCommentedSetting is IniFile#find_commented_setting (if:303-311): nil
// when the section is new or no line is a commented version of setting.
func (f *IniFile) findCommentedSetting(sec *section, setting *string) *commentedSetting {
	if sec.isNew() {
		return nil
	}
	for lineNum := *sec.startLine; lineNum <= *sec.endLine; lineNum++ {
		m := f.matchSetting(f.commentedRegex, lineNum)
		if m != nil && setting != nil && m[3] == *setting {
			return &commentedSetting{match: m, lineNum: lineNum}
		}
	}
	return nil
}

// insertInlineSettingLine is IniFile#insert_inline_setting_line (if:316-322).
func (f *IniFile) insertInlineSettingLine(found *commentedSetting, sec *section, setting, separator string, value []string) error {
	for i, v := range value {
		indent, err := f.indent(sec)
		if err != nil {
			return err
		}
		if err := f.lines.insert(found.lineNum+1+i, indent+setting+separator+v); err != nil {
			return err
		}
	}
	return nil
}

// decrementSectionLineNumbers is IniFile#decrement_section_line_numbers
// (if:327-332), for the sections named from index sectionIndex on.
func (f *IniFile) decrementSectionLineNumbers(sectionIndex, n int) error {
	for _, name := range f.sectionNames[sectionIndex:] {
		sec := f.sections[name]
		if sec == nil {
			return noMethodOnNil("decrement_line_nums")
		}
		sec.decrementLineNums(n)
	}
	return nil
}

// incrementSectionLineNumbers is IniFile#increment_section_line_numbers
// (if:337-342).
func (f *IniFile) incrementSectionLineNumbers(sectionIndex, n int) error {
	for _, name := range f.sectionNames[sectionIndex:] {
		sec := f.sections[name]
		if sec == nil {
			return noMethodOnNil("increment_line_nums")
		}
		sec.incrementLineNums(n)
	}
	return nil
}
