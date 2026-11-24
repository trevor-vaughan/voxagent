package augeas

import (
	"errors"
	"fmt"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// Handle is an open libaugeas handle (`augeas *`), opaque to the providers.
// Zero is never a valid handle.
type Handle uintptr

// Lib is libaugeas 1.15.0's C API (spec §4.1 table), one method per
// function the providers call; nil *string is NULL. The cgo binding implements
// it; unit tests replay Ruby's recorded traces through it
// (augeas_replay_test.go). Results are libaugeas' own: the ruby-augeas mapping
// is Aug's job.
type Lib interface {
	Init(root, loadpath *string, flags uint) (Handle, bool) // false: aug_init returned NULL
	Close(h Handle)
	Get(h Handle, path string) (value *string, rc int)
	Set(h Handle, path string, value *string) int
	Rm(h Handle, path string) int
	Insert(h Handle, path, label string, before bool) int
	Mv(h Handle, src, dst string) int
	Match(h Handle, path string) (paths []string, rc int) // rc -1: paths nil
	Load(h Handle) int
	Save(h Handle) int
	Defvar(h Handle, name, expr string) int
	Defnode(h Handle, name, expr string, value *string) int // created is always NULL
	TextStore(h Handle, lens, node, path string) int
}

// NoModlAutoload is AUG_NO_MODL_AUTOLOAD (augeas.h:51), the only flag
// ruby-augeas is given here.
const NoModlAutoload uint = 1 << 6

// Aug is ruby-augeas' Augeas class over one handle (RA, RP): the same
// libaugeas calls Ruby makes, with Ruby's mapping of their results.
type Aug struct {
	Lib    Lib
	H      Handle
	closed bool
}

// Open is Augeas.open(nil, loadpath, Augeas::NO_MODL_AUTOLOAD).
func Open(lib Lib, loadpath *string) (*Aug, error) {
	h, ok := lib.Init(nil, loadpath, NoModlAutoload)
	if !ok {
		return nil, errors.New("unknown error - Failed to initialize Augeas")
	}
	return &Aug{Lib: lib, H: h}, nil
}

// Get is Augeas#get: the value only when exactly one node matches and it has
// a value; nil (false) for none, several, a bad path or a valueless node.
func (a *Aug) Get(path string) (string, bool) {
	v, rc := a.Lib.Get(a.H, path)
	if rc != 1 || v == nil {
		return "", false
	}
	return *v, true
}

// Set is Augeas#set with one value. Ruby returns the value array whatever
// aug_set did (RA:135-137), so there is no result to give.
func (a *Aug) Set(path, value string) {
	a.Lib.Set(a.H, path, &value)
}

// Clear is Augeas#clear: aug_set with a NULL value, result ignored.
func (a *Aug) Clear(path string) {
	a.Lib.Set(a.H, path, nil)
}

// Rm is Augeas#rm: aug_rm's count, or -1.
func (a *Aug) Rm(path string) int {
	return a.Lib.Rm(a.H, path)
}

// Insert is Augeas#insert: aug_insert's 0 or -1, never raised.
func (a *Aug) Insert(path, label string, before bool) int {
	return a.Lib.Insert(a.H, path, label, before)
}

// Match is Augeas#match, which raises when aug_match fails.
func (a *Aug) Match(path string) ([]string, error) {
	paths, rc := a.Lib.Match(a.H, path)
	if rc < 0 {
		return nil, fmt.Errorf("unknown error - Matching path expression '%s' failed", path)
	}
	return paths, nil
}

// LoadBang is Augeas#load!: `raise Augeas::Error unless load` (RA:199-201),
// whose message is the class name.
func (a *Aug) LoadBang() error {
	if a.Lib.Load(a.H) != 0 {
		return errors.New("Augeas::Error")
	}
	return nil
}

// SaveBang is Augeas#save! (RA:194-196), as loadBang.
func (a *Aug) SaveBang() error {
	if a.Lib.Save(a.H) != 0 {
		return errors.New("Augeas::Error")
	}
	return nil
}

// Defvar is Augeas#defvar; the providers ignore its result.
func (a *Aug) Defvar(name, expr string) {
	a.Lib.Defvar(a.H, name, expr)
}

// Defnode is Augeas#defnode(name, expr, nil); the providers ignore its result.
func (a *Aug) Defnode(name, expr string) {
	a.Lib.Defnode(a.H, name, expr, nil)
}

// TextStore is Augeas#text_store: true when aug_text_store returned 0.
func (a *Aug) TextStore(lens, node, path string) bool {
	return a.Lib.TextStore(a.H, lens, node, path) == 0
}

// Close is Augeas#close. A handle is closed once however often close runs.
func (a *Aug) Close() {
	if a.closed {
		return
	}
	a.closed = true
	a.Lib.Close(a.H)
}

// Unimplemented is the refusal to handle parameter name natively, for
// the value the catalog gives it.
func Unimplemented(r catalog.Resource, name string) error {
	return &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: name, Shown: rubyvalue.ShownValue(r, name, r.Parameters[name])}
}

// Prelude is what every attribute of the four types is subject to: a
// rich value or a Sensitive mark anywhere, a name parameter that is not the
// title, and a provider other than augeas.
func Prelude(r catalog.Resource) error {
	if err := rubyvalue.Rich(r); err != nil {
		return err
	}
	if len(r.SensitiveParameters) > 0 {
		return &rubyvalue.UnimplementedValue{Ref: r.Ref(), Attr: r.SensitiveParameters[0], Shown: "[redacted]"}
	}
	if v := r.Parameters["name"]; v != nil && v != r.Title {
		return Unimplemented(r, "name")
	}
	if v := r.Parameters["provider"]; v != nil && v != "augeas" {
		return Unimplemented(r, "provider")
	}
	return nil
}

// ProviderName is the provider name all four report: the only provider
// directory the routing admits holds augeas.rb.
var ProviderName = "augeas"
