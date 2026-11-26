package inifile

import (
	"context"
	"os"
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
)

// IniSection is the native provider for the ini_section type (6.6.x only)
// under Puppet's resource harness, as 6.6.1 with PR #581 applied behaves
// (divergence D-105). Ruby: psc: provider/ini_section/ruby.rb, tsc:
// type/ini_section.rb.
type IniSection struct{}

// Type implements ral.Provider.
func (IniSection) Type() string { return "ini_section" }

// ProviderUsed implements ral.Provider (M8).
func (IniSection) ProviderUsed() *string { p := providerName; return &p }

// Attrs implements ral.Provider: every newparam of tsc:, name and provider, and
// ensure (tsc:3-13, from ensurable). There is no refreshonly: the only code
// that read it is the insync? override PR #581 deletes.
func (IniSection) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "section", "path", "section_prefix", "section_suffix")
}

// Properties implements ral.PropertyDeclaring.
func (IniSection) Properties() ral.Attrs { return ral.NewAttrs("ensure") }

// AutoRequires implements ral.Provider (tsc:55).
func (IniSection) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	return autorequireParent(r, managed)
}

// Prefetch implements ral.Provider: nothing is read ahead of an evaluation.
func (s IniSection) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return s, nil
}

// Refresh implements ral.Provider; Refreshable keeps the engine from calling
// it, as tsc: defines no refresh.
func (IniSection) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Refreshable implements ral.Refreshing.
func (IniSection) Refreshable(catalog.Resource) bool { return false }

// sectionEval is one resource's evaluation: Ruby's provider instance with its
// memoised @ini_file (psc:88-90).
type sectionEval struct {
	p        sectionParams
	encoding string
	ini      *IniFile
}

// file is psc:88-90: the IniFile with the resource's section syntax, indent ' '
// and width 2 (psc:89).
func (e *sectionEval) file() (*IniFile, error) {
	if e.ini != nil {
		return e.ini, nil
	}
	width := 2
	f, err := openRubyIniFile(e.p.Path, "=", e.p.SectionPrefix, e.p.SectionSuffix, " ", &width, e.encoding)
	e.ini = f
	return f, err
}

// exists is psc:35-37.
func (e *sectionEval) exists() (bool, error) {
	f, err := e.file()
	if err != nil {
		return false, err
	}
	return slices.Contains(f.SectionNames(), e.p.Section), nil
}

// create is psc:39-43.
func (e *sectionEval) create() error {
	f, err := e.file()
	if err != nil {
		return err
	}
	f.SetSection(e.p.Section)
	if err := f.Save(); err != nil {
		return err
	}
	e.ini = nil
	return nil
}

// destroy is psc:45-49.
func (e *sectionEval) destroy() error {
	f, err := e.file()
	if err != nil {
		return err
	}
	if err := f.RemoveSection(e.p.Section); err != nil {
		return err
	}
	if err := f.Save(); err != nil {
		return err
	}
	e.ini = nil
	return nil
}

// Apply implements ral.Provider as Puppet's harness evaluates ini_section:
// ensure is retrieved (exists?) and synced when it differs from the desired one.
func (IniSection) Apply(_ context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSection(r)
	if err != nil {
		return nil, nil, err
	}
	e := &sectionEval{p: p, encoding: rubyvalue.ExternalEncoding(os.Getenv)}
	present, err := e.exists()
	if err != nil {
		return nil, nil, err
	}
	// Divergence D-105: Puppet's default insync? (`current == should`), as PR
	// #581 leaves ensure. Shipped 6.6.0 and 6.6.1 override it with a method
	// that reads the undeclared refreshonly parameter and fails every
	// evaluation, in sync or not (NOTES M4).
	current := presence(present)
	if current == p.Ensure {
		return nil, nil, nil
	}
	name, msg, action := "ini_section_created", "created", e.create
	if p.Ensure == "absent" {
		name, msg, action = "ini_section_removed", "removed", e.destroy
	}
	ch := ral.Change{Property: "ensure", Name: name, Was: current, Is: p.Ensure, Message: msg}
	return []ral.Change{settle(ch, noop, func() error { return ensureFailure(r, p.Ensure, action()) })}, nil, nil
}
