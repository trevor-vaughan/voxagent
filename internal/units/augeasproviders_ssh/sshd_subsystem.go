package augeasprovidersssh

import (
	"context"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// augeasproviders_ssh 7.0.0's sshd_config_subsystem (SST:
// type/sshd_config_subsystem.rb; SSP: provider/sshd_config_subsystem/augeas.rb)
// under Puppet's resource harness, spec §6: exists?, the command property,
// create, destroy and CORE's flush.

// subsystemLens is SSP:15's lens; the class is
// Puppet::Type::Sshd_config_subsystem, whose handle is not sshd_config's.
var subsystemLens = core.Lens{Name: "Sshd", Type: "Sshd_config_subsystem"}

// Prefetch implements Provider: sshd_config_subsystem does not prefetch (spec
// §6).
func (s SshdConfigSubsystem) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return s, nil
}

// Refresh implements Provider: sshd_config_subsystem has no refresh.
func (SshdConfigSubsystem) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Apply evaluates one Sshd_config_subsystem as SshConfig's Apply does, with
// SST:18-20's command: a plain property, in sync when equal, read and written
// as $resource's value (SSP:53).
func (s SshdConfigSubsystem) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSubsystem(r)
	if err != nil {
		return nil, nil, err
	}
	target := p.Target
	if target == "" {
		target = sshdConfigDefaultTarget
	}
	resourcePath := "$target/*[label()=~regexp('Subsystem', 'i')]/" + p.Name
	// ensurable's default: present when the resource manages a property.
	ensure := p.Ensure
	if ensure == "" && p.HasCommand {
		ensure = "present"
	}

	// exists? (CORE:872-874).
	aug, err := s.run.Augopen(subsystemLens, target, resourcePath)
	if err != nil {
		return nil, nil, err
	}
	found, err := aug.Match("$resource")
	if err != nil {
		return nil, nil, err
	}
	exists := len(found) > 0

	// The command getter, for an existing entry only: nil when $resource has
	// no value or matches several nodes.
	var is any
	if exists && p.HasCommand {
		if aug, err = s.run.Augopen(subsystemLens, target, resourcePath); err != nil {
			return nil, nil, err
		}
		if v, ok := aug.Get("$resource"); ok {
			is = v
		}
	}

	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	var ch ral.Change
	switch {
	case ensure == "present" && !exists:
		ch = ral.Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "sshd_config_subsystem_created"}
		if !noop {
			ch.Err = s.create(p, target, resourcePath)
		}
	case ensure == "absent" && exists:
		ch = ral.Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "sshd_config_subsystem_removed"}
		if !noop {
			// destroy (SSP:48-51): every Subsystem entry holding the name.
			if aug, err := s.run.Augopen(subsystemLens, target, resourcePath); err != nil {
				ch.Err = err
			} else {
				aug.Rm("$target/*[label()=~regexp('Subsystem', 'i') and " + p.Name + "]")
			}
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		if !p.HasCommand || is == p.Command {
			return nil, nil, nil
		}
		// is_to_s/should_to_s: format_value_for_display, nil as undef.
		was := "undef"
		if v, ok := is.(string); ok {
			was = rubyvalue.PuppetQuote(v)
		}
		want := rubyvalue.PuppetQuote(p.Command)
		ch = ral.Change{Property: "command", Was: is, Is: p.Command, Name: "command_changed",
			Message: "command changed " + was + " to " + want + suffix, WasDisplay: was, IsDisplay: want, DisplayVerbatim: true}
		if !noop {
			// command= (SSP:53) on the $resource expression.
			if aug, err := s.run.Augopen(subsystemLens, target, resourcePath); err != nil {
				ch.Err = err
			} else {
				aug.Set("$resource", p.Command)
			}
		}
	}
	if noop || ch.Err != nil {
		return []ral.Change{ch}, nil, nil
	}
	return []ral.Change{ch}, nil, s.run.Flush(ctx, subsystemLens)
}

// create is SSP:36-46 without the comment (native refuses comment): with
// Match blocks, a Subsystem entry inserted before the first and its name
// cleared, then the command set (nil, a valueless set, when unmanaged).
func (s SshdConfigSubsystem) create(p subsystemParams, target, resourcePath string) error {
	aug, err := s.run.Augopen(subsystemLens, target, resourcePath)
	if err != nil {
		return err
	}
	m, err := aug.Match("$target/Match")
	if err != nil {
		return err
	}
	if len(m) > 0 {
		aug.Insert("$target/Match[1]", "Subsystem", true)
		aug.Clear("$target/Subsystem[last()]/" + p.Name)
	}
	path := "$target/Subsystem/" + p.Name
	if p.HasCommand {
		aug.Set(path, p.Command)
	} else {
		aug.Clear(path)
	}
	return nil
}
