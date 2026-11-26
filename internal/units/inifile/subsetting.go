package inifile

import (
	"context"
	"os"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// IniSubsetting is the native provider for the ini_subsetting type. Ruby: pss:
// provider/ini_subsetting/ruby.rb, tss: type/ini_subsetting.rb.
type IniSubsetting struct{}

// Type implements ral.Provider.
func (IniSubsetting) Type() string { return "ini_subsetting" }

// ProviderUsed implements ral.Provider (M8).
func (IniSubsetting) ProviderUsed() *string { p := providerName; return &p }

// Attrs implements ral.Provider: every newparam and newproperty of tss:, name
// and provider, and ensure (tss:7-11, from ensurable).
func (IniSubsetting) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "section", "setting", "subsetting", "subsetting_separator",
		"subsetting_key_val_separator", "path", "show_diff", "key_val_separator", "quote_char", "use_exact_match",
		"value", "insert_type", "insert_value", "delete_if_empty")
}

// Properties implements ral.PropertyDeclaring.
func (IniSubsetting) Properties() ral.Attrs { return ral.NewAttrs("ensure", "value") }

// AutoRequires implements ral.Provider: tss: declares no autorequire (unlike
// ts:151 and tsc:55), in 6.5.0, 6.6.0 and 6.6.1 alike.
func (IniSubsetting) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Prefetch implements ral.Provider: nothing is read ahead of an evaluation.
func (s IniSubsetting) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return s, nil
}

// Refresh implements ral.Provider; Refreshable keeps the engine from calling
// it, as tss: defines no refresh.
func (IniSubsetting) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Refreshable implements ral.Refreshing.
func (IniSubsetting) Refreshable(catalog.Resource) bool { return false }

// subsettingEval is one resource's evaluation: Ruby's provider instance with
// its memoised @ini_file and @setting_value (pss:90-95).
type subsettingEval struct {
	p        subsettingParams
	encoding string
	ini      *IniFile
	value    *SettingValue
}

// iniFile is pss:86-88. Kept from Ruby (pss:87): the IniFile gets only the
// key_val_separator, so the default section syntax and indentation apply
// (spec 7.6).
func (e *subsettingEval) iniFile() (*IniFile, error) {
	if e.ini != nil {
		return e.ini, nil
	}
	f, err := openRubyIniFile(e.p.Path, e.p.KeyValSeparator, "[", "]", " ", nil, e.encoding)
	e.ini = f
	return f, err
}

// settingValue is pss:90-95: the first value of the setting, parsed.
func (e *subsettingEval) settingValue() (*SettingValue, error) {
	if e.value != nil {
		return e.value, nil
	}
	f, err := e.iniFile()
	if err != nil {
		return nil, err
	}
	var first *string
	if values := f.GetValue(e.p.Section, &e.p.Setting); len(values) > 0 {
		first = &values[0]
	}
	v, err := newSettingValue(first, e.p.SubsettingSeparator, e.p.QuoteChar, e.p.SubsettingKeyValSeparator)
	e.value = v
	return v, err
}

// exists is pss:12-14. Kept from Ruby (pss:13): it returns the subsetting's
// value, which is truthy even when it is ” (spec 7.5).
func (e *subsettingEval) exists() (bool, error) {
	v, err := e.settingValue()
	if err != nil {
		return false, err
	}
	return v.GetSubsettingValue(e.p.Subsetting, e.p.UseExactMatch) != nil, nil
}

// store is the tail shared by create, destroy and value= (pss:21-22, 32-34):
// write the setting from the SettingValue, then save.
func (e *subsettingEval) store(v *SettingValue, f *IniFile) error {
	if err := f.SetValue(e.p.Section, &e.p.Setting, e.p.KeyValSeparator, []string{v.GetValue()}); err != nil {
		return err
	}
	return f.Save()
}

// setValue is value= (pss:43-50) and, with the resource's own value, create's
// first half (pss:16-22). The memos stay: the caller decides.
func (e *subsettingEval) setValue(value *string) error {
	v, err := e.settingValue()
	if err != nil {
		return err
	}
	if err := v.AddSubsetting(e.p.Subsetting, value, e.p.UseExactMatch, e.p.InsertType, e.p.InsertValue); err != nil {
		return err
	}
	f, err := e.iniFile()
	if err != nil {
		return err
	}
	return e.store(v, f)
}

// create is pss:16-25.
func (e *subsettingEval) create() error {
	if err := e.setValue(e.p.Value); err != nil {
		return err
	}
	e.ini, e.value = nil, nil
	return nil
}

// destroy is pss:27-37.
func (e *subsettingEval) destroy() error {
	v, err := e.settingValue()
	if err != nil {
		return err
	}
	f, err := e.iniFile()
	if err != nil {
		return err
	}
	v.RemoveSubsetting(e.p.Subsetting, e.p.UseExactMatch)
	// Kept from Ruby (pss:29, tss:125-129): `resource[:delete_if_empty]` is
	// the symbol :false or :true, and :false is truthy, so the setting line is
	// deleted whenever the last subsetting goes, whatever delete_if_empty says
	// (spec 7.11).
	if v.GetValue() == "" {
		err = f.RemoveSetting(e.p.Section, &e.p.Setting)
		if err == nil {
			err = f.Save()
		}
	} else {
		err = e.store(v, f)
	}
	if err != nil {
		return err
	}
	e.ini, e.value = nil, nil
	return nil
}

// Apply implements ral.Provider as Puppet's harness evaluates ini_subsetting:
// ensure first (exists?), then value, as for ini_setting (spec §5).
func (IniSubsetting) Apply(_ context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSubsetting(r)
	if err != nil {
		return nil, nil, err
	}
	e := &subsettingEval{p: p, encoding: rubyvalue.ExternalEncoding(os.Getenv)}
	present, err := e.exists()
	if err != nil {
		return nil, nil, err
	}
	if current := presence(present); current != p.Ensure {
		name, msg, action := "ini_subsetting_created", "created", e.create
		if p.Ensure == "absent" {
			name, msg, action = "ini_subsetting_removed", "removed", e.destroy
		}
		ch := ral.Change{Property: "ensure", Name: name, Was: current, Is: p.Ensure, Message: msg}
		return []ral.Change{settle(ch, noop, func() error { return ensureFailure(r, p.Ensure, action()) })}, nil, nil
	}
	if !present || p.Value == nil {
		return nil, nil, nil
	}

	v, err := e.settingValue()
	if err != nil {
		return nil, nil, err
	}
	current := v.GetSubsettingValue(p.Subsetting, false) // pss:39-41
	if current != nil && *current == *p.Value {
		return nil, nil, nil
	}
	var was any
	if current != nil {
		was = *current
	}
	ch := valueChange(was, *p.Value)
	return []ral.Change{settle(ch, noop, func() error { return e.setValue(p.Value) })}, nil, nil
}
