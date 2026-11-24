package ral

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// augeasproviders_sysctl 4.1.0's resource evaluation (SYT: type/sysctl.rb
// :16-55; SYP: provider/sysctl/augeas.rb :198-359), spec §5: exists?, the
// value's retrieve and insync?, change_to_s, the value= setter and flush.

// Apply evaluates one Sysctl as Puppet's harness does: exists? (which fails
// an invalid key unless silent), then the value property, whose sync is a set
// in the class handle's tree and whose flush runs `sysctl -w` before the save.
// A failed -w without silent is a *FlushError after the change, and the edit
// stays unsaved in the shared tree (I-36).
func (s Sysctl) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	p, err := parseSysctl(r)
	if err != nil {
		return nil, nil, err
	}
	src := sources.SourcesFrom(ctx)
	entry := s.run.sysctl[p.Name]

	// exists? (SYP:284-305): ensure is retrieved even when unset (type.rb
	// retrieve), and a valid key is present without touching the handle.
	if !entry.Valid {
		if !p.Silent {
			return nil, nil, errors.New("Error: `" + p.Name + "` is not a valid sysctl key")
		}
		runlog.Debugf(ctx, src.Provider, "augeasproviders_sysctl: `%s` is not a valid sysctl key", p.Name)
	}
	if !p.HasValue {
		return nil, nil, nil
	}

	target := p.Target
	if target == "" {
		target = sysctlDefaultTarget
	}
	resourcePath := "$target/" + p.Name

	// The value's retrieve, attr_aug_accessor's getter: nil when absent.
	aug, err := s.run.augopen(sysctlLens, target, resourcePath)
	if err != nil {
		return nil, nil, err
	}
	var cur *string
	if v, ok := aug.Get("$resource"); ok {
		cur = &v
	}
	var was any
	if cur != nil {
		was = *cur
	}

	// insync? (SYT:17-34).
	if !entry.Valid {
		runlog.Debugf(ctx, src.Resource+"/value", "augeasproviders_sysctl: skipping insync? due to invalid resource `%s`", p.Name)
		return nil, nil, nil
	}
	var live string
	if p.Apply {
		if p.Silent {
			// live_value (SYP:316-322).
			runlog.Debugf(ctx, src.Provider, "augeasproviders_sysctl not setting live value for %s due to :silent mode", p.Name)
			live = p.Value
		} else {
			code, out, err := s.runner.run(ctx, s.sysctl, "-n", p.Name)
			if err == nil && code != 0 {
				err = command.ExecFailure([]string{s.sysctl, "-n", p.Name}, false, code, out)
			}
			if err != nil {
				return []Change{{Property: "value", Was: was, Is: p.Value, Err: err}}, nil, nil
			}
			live = rubyChomp(out)
		}
		if !rubyEqual(&p.Value, &live) {
			return s.sync(ctx, p, entry, target, resourcePath, cur, was, live, noop)
		}
	}
	// persist is always true natively (spec §2), so the file value decides.
	if rubyEqual(&p.Value, cur) {
		return nil, nil, nil
	}
	return s.sync(ctx, p, entry, target, resourcePath, cur, was, live, noop)
}

// sync records the value change and, unless noop, makes it: the value=
// setter, then flush.
func (s Sysctl) sync(ctx context.Context, p sysctlParams, entry sysctlEntry, target, resourcePath string, cur *string, was any, live string, noop bool) ([]Change, []Warning, error) {
	ch := Change{Property: "value", Was: was, Is: p.Value, Message: sysctlChangeToS(cur, live, p.Value, p.Apply, true)}
	if noop {
		ch.Message += " (noop)"
		return []Change{ch}, nil, nil
	}

	// value= (attr_aug_accessor's setter): a no-op for a key not yet in the
	// file, $resource being bound to an empty node set.
	aug, err := s.run.augopen(sysctlLens, target, resourcePath)
	if err != nil {
		ch.Err = err
		return []Change{ch}, nil, nil
	}
	aug.Set("$resource", p.Value)
	return []Change{ch}, nil, s.flush(ctx, p, entry, target, resourcePath)
}

// flush is SYP:334-359 for ensure present: `sysctl -w`, then
// cleanup_duplicates, create for a key the file lacks, and CORE's save.
// Every failure is a *FlushError.
func (s Sysctl) flush(ctx context.Context, p sysctlParams, entry sysctlEntry, target, resourcePath string) error {
	if p.Apply {
		// sysctl_set (SYP:27-37).
		arg := p.Name + "=" + p.Value
		code, out, err := s.runner.run(ctx, s.sysctl, "-w", arg)
		if err == nil && code != 0 {
			err = command.ExecFailure([]string{s.sysctl, "-w", arg}, false, code, out)
		}
		if err != nil {
			if !p.Silent {
				return &FlushError{Err: err}
			}
			runlog.Debugf(ctx, "Puppet::Type::Sysctl::ProviderAugeas", "augeasprovider_sysctl ignoring failed attempt to set %s due to :silent mode", p.Name)
		}
	}
	onDisk, err := s.cleanupDuplicates(p, target, resourcePath)
	if err != nil {
		return &FlushError{Err: err}
	}
	if !entry.Persist && !onDisk {
		if err := s.create(p, target, resourcePath); err != nil {
			return &FlushError{Err: err}
		}
	}
	return s.run.flush(ctx, sysctlLens)
}

// cleanupDuplicates is SYP:225-271 without the comment re-apply (native
// refuses comment): it removes every entry of the key after the first, each
// with the managed comment just before it, then a trailing managed comment,
// sets the remaining entry, and answers whether the key is in the file.
func (s Sysctl) cleanupDuplicates(p sysctlParams, target, resourcePath string) (bool, error) {
	aug, err := s.run.augopen(sysctlLens, target, resourcePath)
	if err != nil {
		return false, err
	}
	entries, err := aug.Match("$target/" + p.Name)
	if err != nil {
		return false, err
	}
	if len(entries) <= 1 {
		return len(entries) > 0, nil
	}
	managed := func(path string) bool {
		if !strings.Contains(path, "#comment") {
			return false
		}
		v, ok := aug.Get(path)
		return ok && rubyLineStartsWith(v, p.Name+":")
	}
	all, err := aug.Match("$target/*")
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 1; i-- {
		e := entries[i]
		if idx := slices.Index(all, e); idx > 0 && managed(all[idx-1]) {
			aug.Rm(all[idx-1])
		}
		aug.Rm(e)
	}
	children, err := aug.Match("$target/*")
	if err != nil {
		return false, err
	}
	if n := len(children); n > 0 && managed(children[n-1]) {
		aug.Rm(children[n-1])
	}
	aug.Set("$target/"+p.Name, p.Value)
	return true, nil
}

// create is SYP:198-212: the entry goes after a commented-out line for the key
// when there is one, then setvars (CORE:526-530) rebinds $resource to it.
func (s Sysctl) create(p sysctlParams, target, resourcePath string) error {
	aug, err := s.run.augopen(sysctlLens, target, resourcePath)
	if err != nil {
		return err
	}
	commented, err := aug.Match("$target/#comment[.=~regexp('" + p.Name + "([^a-z.].*)?')]")
	if err != nil {
		return err
	}
	if len(commented) > 0 {
		aug.Insert(commented[0], p.Name, false)
	}
	aug.Set("$target/"+p.Name, p.Value)
	file := "/files" + strings.TrimSuffix(target, "/")
	aug.Set("/augeas/context", file)
	aug.Defnode("target", file)
	aug.Defvar("resource", resourcePath)
	return nil
}

// Refresh implements Provider: sysctl has no refresh.
func (Sysctl) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// sysctlChangeToS is change_to_s (SYT:37-50). cur is the file value (nil
// prints empty), live the live value as read.
func sysctlChangeToS(cur *string, live, should string, apply, persist bool) string {
	c := ""
	if cur != nil {
		c = *cur
	}
	switch {
	case !apply:
		return "changed configuration value from '" + c + "' to '" + should + "'"
	case rubyEqual(cur, &should):
		return "changed live value from '" + live + "' to '" + should + "'"
	case rubyEqual(&live, &should):
		return "changed configuration value from '" + c + "' to '" + should + "'"
	case persist:
		return "changed configuration value from '" + c + "' to '" + should + "' and live value from '" + live + "' to '" + should + "'"
	default:
		return "changed live value from '" + live + "' to '" + should + "'"
	}
}

// rubyEqual is SYT:52-54's equal: both non-nil and equal once each run of
// Ruby's \s is one space.
func rubyEqual(a, b *string) bool {
	return a != nil && b != nil && rubyCollapseSpace(*a) == rubyCollapseSpace(*b)
}

// rubyLineStartsWith is `v.match?(/^#{Regexp.escape(prefix)}/)`: Ruby's ^
// matches at the start of v and after every "\n".
func rubyLineStartsWith(v, prefix string) bool {
	return strings.HasPrefix(v, prefix) || strings.Contains(v, "\n"+prefix)
}
