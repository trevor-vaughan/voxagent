package inifile

// section is Puppet::Util::IniFile::Section (sec:). Line numbers index
// IniFile.lines; nil start and end lines (a new section) are nil pointers.
//
//   - name is "" for the global section, which always exists with a start line
//     of 0; its end line is nil when the file begins with a real header.
//   - start and end line are nil for a new non-global section.
type section struct {
	name               string
	startLine, endLine *int
	existingSettings   settingMap
	additionalSettings settingMap
	indentation        *int
}

// settingKey is a Ruby Hash key of the settings maps: a setting name, or nil
// (section-only set_value passes setting nil, which lands in
// additional_settings as the key nil).
type settingKey struct {
	name   string
	isNone bool
}

func keyOf(setting *string) settingKey {
	if setting == nil {
		return settingKey{isNone: true}
	}
	return settingKey{name: *setting}
}

// settingMap is a Ruby Hash from setting to its values: it keeps insertion
// order, which decides the order save writes new settings in. A stored slice
// is never nil, as Ruby's [] is never nil.
type settingMap struct {
	keys   []settingKey
	values map[settingKey][]string
}

func (m *settingMap) get(k settingKey) ([]string, bool) {
	v, ok := m.values[k]
	return v, ok
}

// set is Hash#[]=: an existing key keeps its position.
func (m *settingMap) set(k settingKey, v []string) {
	if m.values == nil {
		m.values = map[settingKey][]string{}
	}
	if _, ok := m.values[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.values[k] = v
}

func (m *settingMap) delete(k settingKey) ([]string, bool) {
	v, ok := m.values[k]
	if !ok {
		return nil, false
	}
	delete(m.values, k)
	for i, have := range m.keys {
		if have == k {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
	return v, true
}

// newSection is Section#initialize (sec:17-24).
func newSection(name string, startLine, endLine *int, settings settingMap, indentation *int) *section {
	return &section{name: name, startLine: startLine, endLine: endLine, existingSettings: settings, indentation: indentation}
}

// global is Section#global? (sec:28-30).
func (s *section) global() bool { return s.name == "" }

// isNew is Section#new_section? (sec:32-36): a new section, global or
// named, has no end line.
func (s *section) isNew() bool { return s.endLine == nil }

// getValue is Section#get_value (sec:42-44); nil is Ruby's nil.
func (s *section) getValue(setting settingKey) []string {
	if v, ok := s.existingSettings.get(setting); ok {
		return v
	}
	v, _ := s.additionalSettings.get(setting)
	return v
}

// existingSetting is Section#existing_setting? (sec:46-48).
func (s *section) existingSetting(setting settingKey) bool {
	_, ok := s.existingSettings.get(setting)
	return ok
}

// empty is Section#empty? (sec:52-54): the global section is empty whenever it
// is new, other sections when start and end line are equal (nil == nil too).
func (s *section) empty() bool {
	if s.global() {
		return s.isNew()
	}
	if s.startLine == nil || s.endLine == nil {
		return s.startLine == s.endLine
	}
	return *s.startLine == *s.endLine
}

// length is Section#length (6.6.x sec:56-58), end_line - start_line + 1. A
// nil end line makes Ruby's `nil - Integer` raise NoMethodError.
func (s *section) length() (int, error) {
	if s.endLine == nil {
		return 0, &RubyError{Class: "NoMethodError", Message: "undefined method `-' for nil:NilClass"}
	}
	return *s.endLine - *s.startLine + 1, nil
}

// updateExistingSetting is Section#update_existing_setting (sec:56-62).
func (s *section) updateExistingSetting(setting settingKey, value []string) {
	existing, _ := s.existingSettings.get(setting)
	s.existingSettings.set(setting, value)
	*s.endLine += len(value) - len(existing)
}

// removeExistingSetting is Section#remove_existing_setting (sec:64-68).
func (s *section) removeExistingSetting(setting settingKey) {
	existing, ok := s.existingSettings.delete(setting)
	if ok && s.endLine != nil {
		*s.endLine -= len(existing)
	}
}

// insertInlineSetting is Section#insert_inline_setting (sec:76-79).
func (s *section) insertInlineSetting(setting settingKey, value []string) {
	s.existingSettings.set(setting, value)
	if s.endLine != nil {
		*s.endLine += len(value)
	}
}

// setAdditionalSetting is Section#set_additional_setting (sec:81-83).
func (s *section) setAdditionalSetting(setting settingKey, value []string) {
	s.additionalSettings.set(setting, value)
}

// decrementLineNums is Section#decrement_line_nums (sec:88-91).
func (s *section) decrementLineNums(n int) { s.addLineNums(-n) }

// incrementLineNums is Section#increment_line_nums (sec:96-99).
func (s *section) incrementLineNums(n int) { s.addLineNums(n) }

func (s *section) addLineNums(n int) {
	if s.startLine != nil {
		*s.startLine += n
	}
	if s.endLine != nil {
		*s.endLine += n
	}
}
