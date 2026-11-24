package ral

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Shared provider code of augeasproviders_core 4.2.0 (CORE: its
// lib/puppet/provider/augeasprovider/default.rb), spec §4.2-§4.3: the class
// handle, augopen with setvars, flush's save and reload, and parsed_as?.

// augLens names a provider class: Name is the lens module (`lens[/[^.]+/]`,
// CORE:637), Type the Puppet type's class-name segment, which keys the class
// handle and the version memo and names the debug source.
type augLens struct{ Name, Type string }

// lens is the lens CORE passes to libaugeas, e.g. "Sysctl.lns".
func (l augLens) lens() string { return l.Name + ".lns" }

// augeasRun is one run's augeas state, shared by the providers of the run
// (ral.Provider's per-run exception): each class's handle, opened lazily at
// its first use, and every handle the run opened, which EndRun closes.
// Only the walk goroutine uses it.
type augeasRun struct {
	lib    augeas.Lib
	libdir string

	// exists is Ruby's File.exist?: true for any file type, following
	// symlinks.
	exists func(path string) bool

	class  map[string]*augeas.Aug // by augLens.Type
	opened []*augeas.Aug          // every handle opened, in open order

	// sysctl is Sysctl's prefetch table, by title (spec §5).
	sysctl map[string]sysctlEntry
}

func newAugeasRun(lib augeas.Lib, libdir string) *augeasRun {
	return &augeasRun{
		lib:    lib,
		libdir: libdir,
		exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		class: map[string]*augeas.Aug{},
	}
}

// EndRun is post_resource_eval for every class (CORE:597-600): it closes each
// handle the run opened that is still open. A handle is closed once however
// often EndRun runs.
func (r *augeasRun) EndRun() {
	for _, a := range r.opened {
		a.Close()
	}
	clear(r.class)
}

// loadpath is CORE:605-610: $libdir/augeas/lenses when it exists, else nil.
func (r *augeasRun) loadpath() *string {
	p := filepath.Join(r.libdir, "augeas", "lenses")
	if !r.exists(p) {
		return nil
	}
	return &p
}

// open is Augeas.open(nil, loadpath, NO_MODL_AUTOLOAD), registering the handle
// for EndRun before anything else can fail.
func (r *augeasRun) open(loadpath *string) (*augeas.Aug, error) {
	a, err := augeas.Open(r.lib, loadpath)
	if err != nil {
		return nil, err
	}
	r.opened = append(r.opened, a)
	return a, nil
}

// handle is aug_handler (CORE:64-66): the class's handle, opened on first use.
// A failed open is not remembered, as `@aug_handler ||=` does not memoize a
// raise.
func (r *augeasRun) handle(l augLens) (*augeas.Aug, error) {
	if a, ok := r.class[l.Type]; ok {
		return a, nil
	}
	a, err := r.open(r.loadpath())
	if err != nil {
		return nil, err
	}
	r.class[l.Type] = a
	return a, nil
}

// augopen is CORE:633-676 with a block, so followed by setvars (:526-530) for
// target and resourcePath. target is chomped of one trailing "/" as CORE's
// target does (:548).
func (r *augeasRun) augopen(l augLens, target, resourcePath string) (*augeas.Aug, error) {
	aug, err := r.handle(l)
	if err != nil {
		return nil, err
	}
	file := strings.TrimSuffix(target, "/")
	load := "/augeas/load/" + l.Name
	m, err := aug.Match(load)
	if err != nil {
		return nil, err
	}
	if len(m) == 0 {
		// Augeas#transform (RA:177-191) with excl [].
		aug.Set(load+"/lens", l.lens())
		aug.Set(load+"/incl[last()+1]", file)
		if err := aug.LoadBang(); err != nil {
			return nil, err
		}
	} else {
		incl := load + "/incl[.='" + file + "']"
		m, err := aug.Match(incl)
		if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			aug.Set(incl, file)
			if err := aug.LoadBang(); err != nil {
				return nil, err
			}
		}
	}

	if r.exists(file) {
		m, err := aug.Match("/files" + file)
		if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			return nil, r.loadError(aug, l, file)
		}
	}

	aug.Set("/augeas/context", "/files"+file)
	aug.Defnode("target", "/files"+file)
	aug.Defvar("resource", resourcePath)
	return aug, nil
}

// loadError is CORE:653-660's message for a file the lens did not load.
func (r *augeasRun) loadError(aug *augeas.Aug, l augLens, file string) error {
	errPath := "/augeas/files" + file + "/error"
	message, hasMessage := aug.Get(errPath + "/message")
	pos, err := aug.Match(errPath + "/pos")
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		if !hasMessage {
			// `nil += "..."` under openvox's Ruby 3.2.
			return errors.New("undefined method `+' for nil:NilClass")
		}
		line, _ := aug.Get(errPath + "/line")
		char, _ := aug.Get(errPath + "/char")
		message += fmt.Sprintf(" (line:%s, character:%s)", line, char)
	}
	from := ""
	if lp := r.loadpath(); lp != nil {
		from = " from " + *lp
	}
	return fmt.Errorf("Augeas didn't load %s with %s%s: %s", file, l.lens(), from, message)
}

// flush is the default flush, augsave!(aug_handler, true) (CORE:135-153,
// :893-895). Every failure is a *FlushError: the changes were made.
func (r *augeasRun) flush(ctx context.Context, l augLens) error {
	aug, err := r.handle(l)
	if err != nil {
		return &FlushError{Err: err}
	}
	saveErr := r.save(ctx, aug, l)
	// Ruby's ensure clause: a failure here replaces the save error.
	if err := aug.LoadBang(); err != nil {
		return &FlushError{Err: err}
	}
	if err := r.checkVersion(l); err != nil {
		return &FlushError{Err: err}
	}
	if err := aug.LoadBang(); err != nil {
		return &FlushError{Err: err}
	}
	if saveErr != nil {
		return &FlushError{Err: saveErr}
	}
	return nil
}

// save is augsave!'s body and rescue clause (CORE:136-146).
func (r *augeasRun) save(ctx context.Context, aug *augeas.Aug, l augLens) error {
	if aug.SaveBang() == nil {
		return nil
	}
	var lines []string
	nodes, err := aug.Match("/augeas//error")
	if err != nil {
		return err
	}
	for _, n := range nodes {
		subs, err := aug.Match(n + "/*")
		if err != nil {
			return err
		}
		for _, s := range subs {
			v, _ := aug.Get(s)
			lines = append(lines, s+" = "+v)
		}
	}
	runlog.Debugf(ctx, "Puppet::Type::"+l.Type+"::ProviderAugeas", "Save failure details:\n%s", strings.Join(lines, "\n"))
	return errors.New("Failed to save Augeas tree to file. See debug logs for details.")
}

// augVersionChecked is CORE:27-29's `@aug_version ||=` per provider class,
// which lives as long as the process: the set of types whose version was read.
var augVersionChecked struct {
	sync.Mutex
	types map[string]bool
}

// checkVersion is CORE:152's aug_version, whose comparison with 1.13.0 is
// always true here (the probe requires 1.15.0 from the same library), so the
// second load always follows. The first time per type it reads
// /augeas/version on a handle of its own (Augeas.open with a block: open,
// get, close).
func (r *augeasRun) checkVersion(l augLens) error {
	augVersionChecked.Lock()
	defer augVersionChecked.Unlock()
	if augVersionChecked.types[l.Type] {
		return nil
	}
	aug, err := r.open(nil)
	if err != nil {
		return err
	}
	_, ok := aug.Get("/augeas/version")
	aug.Close()
	if !ok {
		// versioncmp(nil, ...) under openvox's Ruby 3.2; nil is not memoized.
		return errors.New("undefined method `scan' for nil:NilClass")
	}
	if augVersionChecked.types == nil {
		augVersionChecked.types = map[string]bool{}
	}
	augVersionChecked.types[l.Type] = true
	return nil
}

// parsedAs is CORE:571-593 as the providers call it, parsed_as?("<label>
// FOO\n", "<label>/1", lens): whether lens parses that line to a node, on a
// fresh handle with a NULL loadpath (stock lenses only), closed before
// returning.
func (r *augeasRun) parsedAs(lens, label string) (bool, error) {
	aug, err := r.open(nil)
	if err != nil {
		return false, err
	}
	defer aug.Close()
	aug.Set("/input", label+" FOO\n")
	if !aug.TextStore(lens, "/input", "/parsed") {
		return false, nil
	}
	m, err := aug.Match("/parsed/" + label + "/1")
	if err != nil {
		return false, err
	}
	return len(m) > 0, nil
}
