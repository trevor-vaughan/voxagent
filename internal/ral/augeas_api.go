package ral

import (
	"errors"
	"fmt"
)

// augHandle is an open libaugeas handle (`augeas *`), opaque to the providers.
// Zero is never a valid handle.
type augHandle uintptr

// augLib is libaugeas 1.15.0's C API (spec §4.1 table), one method per
// function the providers call; nil *string is NULL. The cgo binding implements
// it; unit tests replay Ruby's recorded traces through it
// (augeas_replay_test.go). Results are libaugeas' own: the ruby-augeas mapping
// is rubyAug's job.
type augLib interface {
	Init(root, loadpath *string, flags uint) (augHandle, bool) // false: aug_init returned NULL
	Close(h augHandle)
	Get(h augHandle, path string) (value *string, rc int)
	Set(h augHandle, path string, value *string) int
	Rm(h augHandle, path string) int
	Insert(h augHandle, path, label string, before bool) int
	Mv(h augHandle, src, dst string) int
	Match(h augHandle, path string) (paths []string, rc int) // rc -1: paths nil
	Load(h augHandle) int
	Save(h augHandle) int
	Defvar(h augHandle, name, expr string) int
	Defnode(h augHandle, name, expr string, value *string) int // created is always NULL
	TextStore(h augHandle, lens, node, path string) int
}

// AugeasLib and AugeasHandle name the binding's seam outside the package, so
// that a package routing the augeas types (cmd/voxagent) can implement
// AugeasProbe over a fake library in its tests.
type (
	AugeasLib    = augLib
	AugeasHandle = augHandle
)

// augNoModlAutoload is AUG_NO_MODL_AUTOLOAD (augeas.h:51), the only flag
// ruby-augeas is given here.
const augNoModlAutoload uint = 1 << 6

// rubyAug is ruby-augeas' Augeas class over one handle (RA, RP): the same
// libaugeas calls Ruby makes, with Ruby's mapping of their results.
type rubyAug struct {
	lib    augLib
	h      augHandle
	closed bool
}

// openRubyAug is Augeas.open(nil, loadpath, Augeas::NO_MODL_AUTOLOAD).
func openRubyAug(lib augLib, loadpath *string) (*rubyAug, error) {
	h, ok := lib.Init(nil, loadpath, augNoModlAutoload)
	if !ok {
		return nil, errors.New("unknown error - Failed to initialize Augeas")
	}
	return &rubyAug{lib: lib, h: h}, nil
}

// get is Augeas#get: the value only when exactly one node matches and it has
// a value; nil (false) for none, several, a bad path or a valueless node.
func (a *rubyAug) get(path string) (string, bool) {
	v, rc := a.lib.Get(a.h, path)
	if rc != 1 || v == nil {
		return "", false
	}
	return *v, true
}

// set is Augeas#set with one value. Ruby returns the value array whatever
// aug_set did (RA:135-137), so there is no result to give.
func (a *rubyAug) set(path, value string) {
	a.lib.Set(a.h, path, &value)
}

// clear is Augeas#clear: aug_set with a NULL value, result ignored.
func (a *rubyAug) clear(path string) {
	a.lib.Set(a.h, path, nil)
}

// rm is Augeas#rm: aug_rm's count, or -1.
func (a *rubyAug) rm(path string) int {
	return a.lib.Rm(a.h, path)
}

// insert is Augeas#insert: aug_insert's 0 or -1, never raised.
func (a *rubyAug) insert(path, label string, before bool) int {
	return a.lib.Insert(a.h, path, label, before)
}

// match is Augeas#match, which raises when aug_match fails.
func (a *rubyAug) match(path string) ([]string, error) {
	paths, rc := a.lib.Match(a.h, path)
	if rc < 0 {
		return nil, fmt.Errorf("unknown error - Matching path expression '%s' failed", path)
	}
	return paths, nil
}

// loadBang is Augeas#load!: `raise Augeas::Error unless load` (RA:199-201),
// whose message is the class name.
func (a *rubyAug) loadBang() error {
	if a.lib.Load(a.h) != 0 {
		return errors.New("Augeas::Error")
	}
	return nil
}

// saveBang is Augeas#save! (RA:194-196), as loadBang.
func (a *rubyAug) saveBang() error {
	if a.lib.Save(a.h) != 0 {
		return errors.New("Augeas::Error")
	}
	return nil
}

// defvar is Augeas#defvar; the providers ignore its result.
func (a *rubyAug) defvar(name, expr string) {
	a.lib.Defvar(a.h, name, expr)
}

// defnode is Augeas#defnode(name, expr, nil); the providers ignore its result.
func (a *rubyAug) defnode(name, expr string) {
	a.lib.Defnode(a.h, name, expr, nil)
}

// textStore is Augeas#text_store: true when aug_text_store returned 0.
func (a *rubyAug) textStore(lens, node, path string) bool {
	return a.lib.TextStore(a.h, lens, node, path) == 0
}

// close is Augeas#close. A handle is closed once however often close runs.
func (a *rubyAug) close() {
	if a.closed {
		return
	}
	a.closed = true
	a.lib.Close(a.h)
}
