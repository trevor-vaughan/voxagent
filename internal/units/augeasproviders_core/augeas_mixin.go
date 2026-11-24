package augeasproviderscore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
)

// Shared provider code of augeasproviders_core 4.2.0 (CORE: its
// lib/puppet/provider/augeasprovider/default.rb), spec §4.2-§4.3: the class
// handle, augopen with setvars, flush's save and reload, and parsed_as?.

// Lens names a provider class: Name is the lens module (`lens[/[^.]+/]`,
// CORE:637), Type the Puppet type's class-name segment, which keys the class
// handle and the version memo and names the debug source.
type Lens struct{ Name, Type string }

// Lens is the lens CORE passes to libaugeas, e.g. "Sysctl.lns".
func (l Lens) Lens() string { return l.Name + ".lns" }

// Run is one run's augeas state, shared by the providers of the run
// (ral.Provider's per-run exception): each class's handle, opened lazily at
// its first use, and every handle the run opened, which EndRun closes.
// Only the walk goroutine uses it.
type Run struct {
	lib    augeas.Lib
	libdir string

	// Exists is Ruby's File.exist?: true for any file type, following
	// symlinks.
	Exists func(path string) bool

	class  map[string]*augeas.Aug // by Lens.Type
	opened []*augeas.Aug          // every handle opened, in open order

	// Sysctl is Sysctl's prefetch table, by title (spec §5).
	Sysctl map[string]SysctlEntry
}

// NewRun is a run over lib whose File.exist? is the host's.
func NewRun(lib augeas.Lib, libdir string) *Run {
	return &Run{
		lib:    lib,
		libdir: libdir,
		Exists: func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		class: map[string]*augeas.Aug{},
	}
}

// SharedRun is the one run the four augeasproviders types of s's run
// share, made over s's probed library on first use: call it only once s.Probe
// has accepted the library.
func SharedRun(s *augeas.Session, libdir string) *Run {
	if s.Shared == nil {
		lib, _, _ := s.Probe()
		s.Shared = NewRun(lib, libdir)
	}
	return s.Shared.(*Run)
}

// EndRun is post_resource_eval for every class (CORE:597-600): it closes each
// handle the run opened that is still open. A handle is closed once however
// often EndRun runs.
func (r *Run) EndRun() {
	for _, a := range r.opened {
		a.Close()
	}
	clear(r.class)
}

// loadpath is CORE:605-610: $libdir/augeas/lenses when it exists, else nil.
func (r *Run) loadpath() *string {
	p := filepath.Join(r.libdir, "augeas", "lenses")
	if !r.Exists(p) {
		return nil
	}
	return &p
}

// Open is Augeas.open(nil, loadpath, NO_MODL_AUTOLOAD), registering the handle
// for EndRun before anything else can fail.
func (r *Run) Open(loadpath *string) (*augeas.Aug, error) {
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
func (r *Run) handle(l Lens) (*augeas.Aug, error) {
	if a, ok := r.class[l.Type]; ok {
		return a, nil
	}
	a, err := r.Open(r.loadpath())
	if err != nil {
		return nil, err
	}
	r.class[l.Type] = a
	return a, nil
}

// Augopen is CORE:633-676 with a block, so followed by setvars (:526-530) for
// target and resourcePath. target is chomped of one trailing "/" as CORE's
// target does (:548).
func (r *Run) Augopen(l Lens, target, resourcePath string) (*augeas.Aug, error) {
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
		aug.Set(load+"/lens", l.Lens())
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

	if r.Exists(file) {
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
func (r *Run) loadError(aug *augeas.Aug, l Lens, file string) error {
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
	return fmt.Errorf("Augeas didn't load %s with %s%s: %s", file, l.Lens(), from, message)
}

// Flush is the default flush, augsave!(aug_handler, true) (CORE:135-153,
// :893-895). Every failure is a *ral.FlushError: the changes were made.
func (r *Run) Flush(ctx context.Context, l Lens) error {
	aug, err := r.handle(l)
	if err != nil {
		return &ral.FlushError{Err: err}
	}
	saveErr := r.save(ctx, aug, l)
	// Ruby's ensure clause: a failure here replaces the save error.
	if err := aug.LoadBang(); err != nil {
		return &ral.FlushError{Err: err}
	}
	if err := r.checkVersion(l); err != nil {
		return &ral.FlushError{Err: err}
	}
	if err := aug.LoadBang(); err != nil {
		return &ral.FlushError{Err: err}
	}
	if saveErr != nil {
		return &ral.FlushError{Err: saveErr}
	}
	return nil
}

// save is augsave!'s body and rescue clause (CORE:136-146).
func (r *Run) save(ctx context.Context, aug *augeas.Aug, l Lens) error {
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

// VersionChecked is CORE:27-29's `@aug_version ||=` per provider class,
// which lives as long as the process: the set of types whose version was read.
var VersionChecked struct {
	sync.Mutex
	Types map[string]bool
}

// checkVersion is CORE:152's aug_version, whose comparison with 1.13.0 is
// always true here (the probe requires 1.15.0 from the same library), so the
// second load always follows. The first time per type it reads
// /augeas/version on a handle of its own (Augeas.open with a block: open,
// get, close).
func (r *Run) checkVersion(l Lens) error {
	VersionChecked.Lock()
	defer VersionChecked.Unlock()
	if VersionChecked.Types[l.Type] {
		return nil
	}
	aug, err := r.Open(nil)
	if err != nil {
		return err
	}
	_, ok := aug.Get("/augeas/version")
	aug.Close()
	if !ok {
		// versioncmp(nil, ...) under openvox's Ruby 3.2; nil is not memoized.
		return errors.New("undefined method `scan' for nil:NilClass")
	}
	if VersionChecked.Types == nil {
		VersionChecked.Types = map[string]bool{}
	}
	VersionChecked.Types[l.Type] = true
	return nil
}

// ParsedAs is CORE:571-593 as the providers call it, parsed_as?("<label>
// FOO\n", "<label>/1", lens): whether lens parses that line to a node, on a
// fresh handle with a NULL loadpath (stock lenses only), closed before
// returning.
func (r *Run) ParsedAs(lens, label string) (bool, error) {
	aug, err := r.Open(nil)
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
