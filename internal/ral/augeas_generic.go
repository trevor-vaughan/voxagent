package ral

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"syscall"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/runlog"
)

// augeas_core 2.1.0's generic augeas type (T: type/augeas.rb; P:
// provider/augeas/augeas.rb) under Puppet's resource harness, spec §4-§6:
// retrieve is need_to_run? (P:427-485), sync is execute_changes (P:487-506).
// Only the shapes parseAugeas admits reach Apply (spec §0 row 6), so onlyif,
// force, root, load_path and type_check are at their defaults here.

// Augeas is the native provider for the augeas type. lensPath is
// get_load_path's result for the run's command (spec §0 row 4); unlink is
// File.delete's system call, a seam for the replay tests, whose recorded
// .augnew files are not on this host.
type Augeas struct {
	lib      augLib
	lensPath string
	unlink   func(path string) error
}

func newAugeas(lib augLib, lensPath string) Augeas {
	return Augeas{lib: lib, lensPath: lensPath, unlink: syscall.Unlink}
}

func (Augeas) Type() string                                              { return "augeas" }
func (Augeas) ProviderUsed() *string                                     { return &augeasProvider }
func (Augeas) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs is every attribute T declares, name and provider included (spec §1
// acceptance 1).
func (Augeas) Attrs() Attrs {
	return NewAttrs("name", "provider", "context", "onlyif", "changes", "root", "load_path", "force",
		"type_check", "lens", "incl", "show_diff", "returns")
}

// Prefetch implements Provider: augeas does not prefetch.
func (a Augeas) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return a, nil }

// Refresh implements Provider; Refreshable keeps the engine from calling it.
func (Augeas) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// Refreshable implements Refreshing: T defines no refresh, so Ruby's event
// manager skips a notified Augeas (spec §0 row 11).
func (Augeas) Refreshable(catalog.Resource) bool { return false }

// augNoLoad is AUG_NO_LOAD (augeas.h), P:167's flag without incl; with incl
// P:165 uses augNoModlAutoload. type_check is always false here.
const augNoLoad uint = 1 << 5

// Ruby's regexps at P:180 and P:189. Contexts reaching here hold no newline
// (spec §0 row 6), so Ruby's line anchors are Go's text anchors.
var (
	augOptCtx  = regexp.MustCompile(`^/files/[^'"\[\]]+$`)
	augCtxPath = regexp.MustCompile(`^/files(.*?)/?$`)
	// augFilesPrefix is P:464's sub(%r{^/files}, root): the first /files at
	// the start of a line.
	augFilesPrefix = regexp.MustCompile(`(?m)^/files`)
)

// augeasOpen is one resource's handle and what its provider logs through.
type augeasOpen struct {
	*rubyAug
	ctx context.Context
	src string
	p   augeasParams
}

func (o *augeasOpen) debug(format string, args ...any) { runlog.Debugf(o.ctx, o.src, format, args...) }

// Apply is the returns property's retrieve then, when out of sync and not
// noop, its sync. The handle is closed when Apply returns, which is where
// Ruby's ensures close it (spec §0 row 8): need_to_run? closes it unless
// sync will run, and sync always closes it.
func (a Augeas) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	p, err := parseAugeas(r)
	if err != nil {
		return nil, nil, err
	}
	o := &augeasOpen{ctx: ctx, src: SourcesFrom(ctx).Provider, p: p}

	// open_augeas (P:159-204).
	flags := augNoLoad
	if p.HasIncl {
		flags = augNoModlAutoload
	}
	o.debug("Opening augeas with root /, lens path %s, flags %d", a.lensPath, flags)
	root, lensPath := "/", a.lensPath
	h, ok := a.lib.Init(&root, &lensPath, flags)
	if !ok {
		return nil, nil, errors.New("unknown error - Failed to initialize Augeas")
	}
	o.rubyAug = &rubyAug{lib: a.lib, h: h}
	defer func() {
		o.close()
		o.debug("Closed the augeas connection")
	}()
	warnings, err := o.open()
	if err != nil {
		return nil, warnings, err
	}

	run, err := o.dryRun(a.unlink)
	if err != nil || !run {
		return nil, warnings, err
	}
	ch := Change{Property: "returns", Was: "need_to_run", Is: 0, Message: "executed successfully"}
	if noop {
		ch.Message += " (noop)"
		return []Change{ch}, warnings, nil
	}
	ch.Err = o.sync()
	return []Change{ch}, warnings, nil
}

// open is open_augeas after Augeas.open: the version lines, the load
// restriction, the load and its errors. The warning, if any, is
// print_load_errors'.
func (o *augeasOpen) open() ([]Warning, error) {
	o.version() // P:175's versioncmp, which 1.15.0 always passes (spec §4 step 2).
	o.debug("Augeas version %s is installed", o.version())
	glob, err := o.match("/augeas/version/pathx/functions/glob")
	if err != nil {
		return nil, err
	}
	var restricted string
	hasRestricted := false
	switch {
	case o.p.HasIncl:
		o.set("/augeas/load/Xfm/lens", o.p.Lens)
		o.set("/augeas/load/Xfm/incl", o.p.Incl)
		restricted, hasRestricted = "/augeas//error", true
	case len(glob) > 0 && augOptCtx.MatchString(o.p.Context):
		ctxPath := augCtxPath.ReplaceAllString(o.p.Context, "$1/")
		loadPath := fmt.Sprintf("/augeas/load/*['%s' !~ glob(incl) + regexp('/.*')]", ctxPath)
		some, err := o.match(loadPath)
		if err != nil {
			return nil, err
		}
		all, err := o.match("/augeas/load/*")
		if err != nil {
			return nil, err
		}
		if len(some) < len(all) {
			o.rm(loadPath)
			restricted, hasRestricted = "/augeas/files"+ctxPath+"/error", true
		} else {
			o.debug("Unable to optimize files loaded by context path, no glob matches")
		}
	}
	o.lib.Load(o.h) // P:200: the result is ignored.

	// print_load_errors (P:397-407).
	errs, err := o.match("/augeas//error")
	if err != nil {
		return nil, err
	}
	var warnings []Warning
	if len(errs) > 0 {
		warn := false
		if hasRestricted {
			m, err := o.match(restricted)
			if err != nil {
				return nil, err
			}
			warn = len(m) > 0
		}
		if warn {
			warnings = append(warnings, Warning{Source: o.src,
				Message: "Loading failed for one or more files, see debug for /augeas//error output"})
		} else {
			o.debug("Loading failed for one or more files, output from /augeas//error:")
		}
	}
	return warnings, o.printErrors(errs)
}

// version is get_augeas_version (P:389-391).
func (o *augeasOpen) version() string {
	v, _ := o.get("/augeas/version")
	return v
}

// printErrors is print_errors (P:415-424): a valueless error node is not
// printed, a valueless child is printed empty.
func (o *augeasOpen) printErrors(errs []string) error {
	for _, node := range errs {
		if v, ok := o.get(node); ok {
			o.debug("%s = %s", node, v)
		}
		children, err := o.match(node + "/*")
		if err != nil {
			return err
		}
		for _, child := range children {
			v, _ := o.get(child)
			o.debug("%s = %s", child, v)
		}
	}
	return nil
}

// save is aug.save, raising as P:455-459 and P:498-501 do when it fails.
func (o *augeasOpen) save() error {
	if o.lib.Save(o.h) == 0 {
		return nil
	}
	// print_put_errors (P:409-413).
	errs, err := o.match("/augeas//error[. = 'put_failed']")
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		o.debug("Put failed on one or more files, output from /augeas//error:")
	}
	if err := o.printErrors(errs); err != nil {
		return err
	}
	return errors.New("Save failed, see debug output for details")
}

// dryRun is need_to_run?'s save in newfile mode (P:447-477): whether any
// file changed, after deleting each .augnew.
func (o *augeasOpen) dryRun(unlink func(string) error) (bool, error) {
	o.version() // P:450's versioncmp.
	o.debug("Will attempt to save and only run if files changed")
	o.set("/augeas/save", "newfile")
	if err := o.execute(); err != nil {
		return false, err
	}
	if err := o.save(); err != nil {
		return false, err
	}
	saved, err := o.match("/augeas/events/saved")
	if err != nil {
		return false, err
	}
	if len(saved) == 0 {
		o.debug("Skipping because no files were changed")
		return false, nil
	}
	// root is "/", so P:463's root is "". A nil get is Ruby's NoMethodError.
	files := make([]string, 0, len(saved))
	for _, key := range saved {
		v, ok := o.get(key)
		if !ok {
			return false, errors.New("undefined method `sub' for nil:NilClass")
		}
		if loc := augFilesPrefix.FindStringIndex(v); loc != nil {
			v = v[:loc[0]] + v[loc[1]:]
		}
		files = append(files, v)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		// File.delete (P:469); no diff is shown (spec §0 row 5).
		path := f + ".augnew"
		if err := unlink(path); err != nil {
			return false, rubyErrno(err, path)
		}
	}
	o.debug("Files changed, should execute")
	return true, nil
}

// rubyErrno is the message of the Errno Ruby's File.delete raises:
// "<strerror> @ apply2files - <path>" (spec §0 row 13).
func rubyErrno(err error, path string) error {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return err
	}
	return fmt.Errorf("%s @ apply2files - %s", libcStrerror(e), path)
}

// sync is execute_changes (P:487-506) without its close, which Apply makes.
func (o *augeasOpen) sync() error {
	saved, err := o.match("/augeas/events/saved")
	if err != nil {
		return err
	}
	for _, key := range saved {
		v, _ := o.get(key) // nil interpolates as "".
		o.rm("/augeas" + v + "/mtime")
	}
	o.version() // P:495's versioncmp.
	o.set("/augeas/save", "overwrite")
	o.lib.Load(o.h) // P:496: the result is ignored.
	if err := o.execute(); err != nil {
		return err
	}
	return o.save()
}

// execute is do_execute_changes (P:510-587) over the parsed commands.
func (o *augeasOpen) execute() error {
	for _, c := range o.p.Commands {
		if err := o.command(c); err != nil {
			// P:583-584's rescue wraps every failure, its own included.
			return fmt.Errorf("Error sending command '%s' with params %s/%s", c.Name, rubyInspectStrings(c.Args), err)
		}
	}
	return nil
}

// command runs one command. A failed result raises the "Error sending
// command" text itself, which execute then wraps again (spec §5).
func (o *augeasOpen) command(c augeasCommand) error {
	args := c.Args
	params := rubyInspectStrings(args)
	failed := fmt.Errorf("Error sending command '%s' with params %s", c.Name, params)
	switch c.Name {
	case "set":
		o.debug("sending command '%s' with params %s", c.Name, params)
		o.set(args[0], args[1]) // ruby-augeas returns the value array: never false.
	case "rm", "remove":
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.rm(args[0]) == -1 {
			return failed
		}
	case "clear":
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.lib.Set(o.h, args[0], nil) != 0 {
			return failed
		}
	case "touch":
		o.debug("sending command '%s' (match, set) with params %s", c.Name, params)
		m, err := o.match(args[0])
		if err != nil {
			return err
		}
		if len(m) == 0 && o.lib.Set(o.h, args[0], nil) != 0 {
			return failed
		}
	case "insert", "ins":
		label, where, path := args[0], args[1], args[2]
		var before bool
		switch where {
		case "before":
			before = true
		case "after":
			before = false
		default:
			return fmt.Errorf("Invalid value '%s' for where param", where)
		}
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.insert(path, label, before) == -1 {
			return failed
		}
	case "defvar":
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.lib.Defvar(o.h, args[0], args[1]) < 0 {
			return failed
		}
	case "defnode":
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.lib.Defnode(o.h, args[0], args[1], &args[2]) < 0 {
			return failed
		}
	case "mv", "move":
		o.debug("sending command '%s' with params %s", c.Name, params)
		if o.lib.Mv(o.h, args[0], args[1]) == -1 {
			return failed
		}
	default:
		// P:581; parseAugeas admits none (spec §0 row 6).
		return fmt.Errorf("Command '%s' is not supported", c.Name)
	}
	return nil
}
