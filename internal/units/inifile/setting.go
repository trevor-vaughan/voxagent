package inifile

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// The ini_setting and ini_subsetting providers (spec §5) under Puppet's
// resource harness: retrieve is `exists?` and `value`, sync is `create`,
// `destroy` and `value=`. Ruby: ps: provider/ini_setting/ruby.rb,
// ts: type/ini_setting.rb.

// redactedDisplay is should_to_s and is_to_s of ini_setting's and
// ini_subsetting's value (ts:96-109, tss:91-103). Kept from Ruby
// (ts:96-109, tss:91-103): with the global show_diff off, which voxagent
// assumes (spec §3), every value is redacted in a message, a Sensitive one
// through the transaction's own texts (spec 7.13).
const redactedDisplay = "[redacted sensitive information]"

// providerName is NOTES M8: provider_used is "ruby" for every inifile type.
const providerName = "ruby"

// IniSetting is the native provider for the ini_setting type. A provider from
// Prefetch also keeps what Ruby keeps on the resource between its evaluation
// and its refresh (see settingState); the zero value works without it.
type IniSetting struct{ state *settingState }

// settingState is the ensure each resource ended its evaluation with, by ref.
// Kept from Ruby (ps:47): exists? sets the resource's ensure to absent when the
// section is missing and force_new_section_creation is false, and the
// resource's own refresh reads that ensure (ts:145). Only the walk goroutine
// uses it.
type settingState struct{ ensure map[string]string }

// Type implements ral.Provider.
func (IniSetting) Type() string { return "ini_setting" }

// ProviderUsed implements ral.Provider (M8).
func (IniSetting) ProviderUsed() *string { p := providerName; return &p }

// Attrs implements ral.Provider: every newparam and newproperty of ts:, name
// and provider (which Puppet adds to a type with providers) and ensure
// (ts:8-23, from ensurable).
func (IniSetting) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "section", "setting", "force_new_section_creation", "path",
		"show_diff", "key_val_separator", "value", "section_prefix", "section_suffix", "indent_char", "indent_width",
		"refreshonly")
}

// Properties implements ral.PropertyDeclaring: the attributes an event carries
// and a Sensitive mark can redact.
func (IniSetting) Properties() ral.Attrs { return ral.NewAttrs("ensure", "value") }

// AutoRequires implements ral.Provider (ts:151-153).
func (IniSetting) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	return autorequireParent(r, managed)
}

// Prefetch implements ral.Provider: ini_setting reads its file per evaluation
// (spec §5), so there is nothing to prefetch; the returned provider holds the
// per-run state.
func (IniSetting) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return IniSetting{state: &settingState{ensure: map[string]string{}}}, nil
}

// Refreshable implements ral.Refreshing: ts:144 defines refresh.
func (IniSetting) Refreshable(catalog.Resource) bool { return true }

// settingEval is one resource's evaluation: Ruby's provider instance, with its
// memoised @ini_file.
type settingEval struct {
	p        settingParams
	ensure   string // the resource's ensure, which exists? can change (ps:47)
	encoding string
	ini      *IniFile
}

func newSettingEval(p settingParams) *settingEval {
	return &settingEval{p: p, ensure: p.Ensure, encoding: rubyvalue.ExternalEncoding(os.Getenv)}
}

// file is ps:158-160: the IniFile, read on first use.
func (e *settingEval) file() (*IniFile, error) {
	if e.ini != nil {
		return e.ini, nil
	}
	f, err := openRubyIniFile(e.p.Path, e.p.KeyValSeparator, e.p.SectionPrefix, e.p.SectionSuffix, e.p.IndentChar, e.p.IndentWidth, e.encoding)
	e.ini = f
	return f, err
}

// openRubyIniFile is IniFile.new for a provider whose path may be unset.
func openRubyIniFile(path, kvs, prefix, suffix, indentChar string, indentWidth *int, encoding string) (*IniFile, error) {
	if path == "" {
		// Kept from Ruby (if: readlines): File.file?(nil) raises. Ruby gives
		// the resource no path when the manifest omits it; the text is Ruby's.
		return nil, &RubyError{Class: "TypeError", Message: "no implicit conversion of nil into String"}
	}
	return openIniFile(path, kvs, prefix, suffix, indentChar, indentWidth, encoding)
}

// exists is ps:35-54.
func (e *settingEval) exists() (bool, error) {
	f, err := e.file()
	if err != nil {
		return false, err
	}
	switch {
	case e.p.Setting == nil:
		return slices.Contains(f.SectionNames(), e.p.Section), nil
	case f.IsSection(e.p.Section):
		return f.GetValue(e.p.Section, e.p.Setting) != nil, nil
	case !e.p.ForceNewSectionCreation:
		// Kept from Ruby (ps:47): the resource's ensure becomes absent, so
		// exists? returning false is "in sync" (spec 7.4, M7).
		e.ensure = "absent"
		return false, nil
	default:
		return false, nil
	}
}

// setValue is the shared body of create and value= (ps:57-62, ps:85-90). The
// IniFile stays memoised: the caller decides whether Ruby drops it.
func (e *settingEval) setValue() error {
	f, err := e.file()
	if err != nil {
		return err
	}
	if e.p.Setting == nil && e.p.Value == nil {
		f.SetSection(e.p.Section)
	} else if err := f.SetValue(e.p.Section, e.p.Setting, e.p.KeyValSeparator, e.p.Value); err != nil {
		return err
	}
	return f.Save()
}

// create is ps:56-64.
func (e *settingEval) create() error {
	if err := e.setValue(); err != nil {
		return err
	}
	e.ini = nil
	return nil
}

// destroy is ps:66-70.
func (e *settingEval) destroy() error {
	f, err := e.file()
	if err != nil {
		return err
	}
	if err := f.RemoveSetting(e.p.Section, e.p.Setting); err != nil {
		return err
	}
	if err := f.Save(); err != nil {
		return err
	}
	e.ini = nil
	return nil
}

// Apply implements ral.Provider as Puppet's harness evaluates ini_setting: it
// retrieves ensure (exists?), syncs ensure when it is out of sync and then
// stops, and otherwise syncs value (spec §5, "Properties and events").
func (s IniSetting) Apply(_ context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSetting(r)
	if err != nil {
		return nil, nil, err
	}
	e := newSettingEval(p)
	present, err := e.exists()
	if err != nil {
		return nil, nil, err
	}
	if s.state != nil {
		s.state.ensure[r.Ref()] = e.ensure
	}

	// ts:8-23: with refreshonly both properties report in sync (ts:16-22,
	// ts:111-117).
	if p.Refreshonly {
		return nil, nil, nil
	}
	// e.ensure is read after exists?, which can change it
	if was := presence(present); was != e.ensure {
		name, msg, action := "ini_setting_created", "created", e.create
		if e.ensure == "absent" {
			name, msg, action = "ini_setting_removed", "removed", e.destroy
		}
		ch := ral.Change{Property: "ensure", Name: name, Was: was, Is: e.ensure, Message: msg}
		return []ral.Change{settle(ch, noop, func() error { return ensureFailure(r, e.ensure, action()) })}, nil, nil
	}
	if !present || p.Value == nil {
		return nil, nil, nil
	}

	f, err := e.file()
	if err != nil {
		return nil, nil, err
	}
	current := f.GetValue(p.Section, p.Setting) // ps:72-82: the array, array_matching :all
	// Ruby's `current == should`: nil is not an empty array.
	if current != nil && slices.Equal(current, p.Value) {
		return nil, nil, nil
	}
	var was any
	if current != nil {
		was = current
	}
	ch := valueChange(was, p.Value)
	return []ral.Change{settle(ch, noop, e.setValue)}, nil, nil
}

// Refresh implements ral.Provider as ts:144-149: with refreshonly, destroy
// when the resource's ensure is absent, else write the value.
func (s IniSetting) Refresh(_ context.Context, r catalog.Resource, noop bool) (bool, error) {
	if noop {
		// Ruby's process_callback? is false under noop: the walk logs "Would
		// have triggered" and never calls ts:144's refresh (G5, M2).
		return false, nil
	}
	p, err := parseSetting(r)
	if err != nil {
		return false, err
	}
	e := newSettingEval(p)
	if ensure, ok := s.state.ensureOf(r); ok {
		e.ensure = ensure
	}
	switch {
	case e.ensure == "absent" && p.Refreshonly:
		// Kept from Ruby (ts:145): destroy runs unconditionally, so a missing
		// section or file fails the refresh with NoMethodError (spec 7.9, M2).
		err = e.destroy()
	case p.Refreshonly:
		err = e.setValue()
	}
	return err == nil, err
}

func (s *settingState) ensureOf(r catalog.Resource) (string, bool) {
	if s == nil {
		return "", false
	}
	ensure, ok := s.ensure[r.Ref()]
	return ensure, ok
}

// valueChange is a value event: the message and displays are redacted whatever
// the value (M1, M3), and the transaction redacts a Sensitive one further.
func valueChange(was, is any) ral.Change {
	return ral.Change{Property: "value", Was: was, Is: is, Message: "value changed " + redactedDisplay + " to " + redactedDisplay,
		WasDisplay: redactedDisplay, IsDisplay: redactedDisplay, DisplayVerbatim: true}
}

// presence is the ensure property's retrieve (exists? ? :present : :absent).
func presence(present bool) string {
	if present {
		return "present"
	}
	return "absent"
}

// settle completes a change: under noop it is only reported, with the
// harness's " (noop)" (M1); otherwise sync runs and a failure rides on Err.
func settle(ch ral.Change, noop bool, sync func() error) ral.Change {
	if noop {
		ch.Message += " (noop)"
		return ch
	}
	ch.Err = sync()
	return ch
}

// ensureFailure is property.rb:483-493's wrapper for a StandardError raised
// while setting ensure (M3). A value= failure has none.
func ensureFailure(r catalog.Resource, value string, err error) error {
	if err == nil {
		return nil
	}
	//nolint:staticcheck // ST1005: Puppet's own text (the harness's ensure failure), which the report must carry verbatim.
	return fmt.Errorf("Could not set '%s' on ensure: %s%s", value, err, rubyvalue.ErrorLocation(r.File, r.Line))
}

// autorequireParent is `autorequire(:file) { Pathname.new(self[:path]).parent.to_s }`
// (ts:151-153; ini_subsetting has none), soft: only a managed File becomes an edge.
func autorequireParent(r catalog.Resource, managed func(string) bool) []string {
	path, ok := r.Parameters["path"].(string)
	if !ok {
		return nil
	}
	if ref := "File[" + pathnameParent(path) + "]"; managed(ref) {
		return []string{ref}
	}
	return nil
}

// pathnameParent is Pathname#parent (`self + '..'`) for an absolute path, as
// measured in M5: "/x" gives "/", "/a/b/" gives "/a", "//x" gives "//".
func pathnameParent(path string) string {
	lead := len(path) - len(strings.TrimLeft(path, "/"))
	// comps are the path's components with the offset each one ends at;
	// trailing slashes belong to no component.
	type comp struct {
		name string
		end  int
	}
	var comps []comp
	for start := lead; start < len(path); {
		next := strings.IndexByte(path[start:], '/')
		if next < 0 {
			next = len(path) - start
		}
		if next > 0 {
			comps = append(comps, comp{path[start : start+next], start + next})
		}
		start += next + 1
	}
	i := len(comps) - 1
	for ; i >= 0 && comps[i].name == "."; i-- {
	}
	if i >= 0 && comps[i].name == ".." {
		// Pathname#+ cannot cancel a ".." with another: it appends one.
		return path[:comps[i].end] + "/.."
	}
	for i--; i >= 0; i-- { // past the component the ".." cancels
		if comps[i].name != "." {
			return path[:comps[i].end]
		}
	}
	return path[:lead]
}
