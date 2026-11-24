package augeasprovidersssh

import (
	"context"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// augeasproviders_ssh 7.0.0's ssh_config (SCT: type/ssh_config.rb; SCP:
// provider/ssh_config/augeas.rb) under Puppet's resource harness, spec §6:
// exists?, the value property, create, destroy and CORE's flush.

// sshConfigLens is SCP:15's lens; the class is Puppet::Type::Ssh_config.
var sshConfigLens = core.Lens{Name: "Ssh", Type: "Ssh_config"}

// sshConfigDefaultTarget is SCP:13's default_file.
const sshConfigDefaultTarget = "/etc/ssh/ssh_config"

// Prefetch implements Provider: ssh_config does not prefetch (spec §6).
func (s SshConfig) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) { return s, nil }

// Refresh implements Provider: ssh_config has no refresh.
func (SshConfig) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// Apply evaluates one Ssh_config as Puppet's harness does: ensure is
// retrieved through exists? even when unset, value only for an existing
// entry; an out-of-sync ensure is the only sync, else value is synced on an
// existing entry. A raise while retrieving is the resource's error; one while
// syncing fails the change; a changed resource is flushed unless noop.
func (s SshConfig) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]ral.Change, []ral.Warning, error) {
	p, err := parseSshConfig(r)
	if err != nil {
		return nil, nil, err
	}
	target := p.Target
	if target == "" {
		target = sshConfigDefaultTarget
	}
	base := "$target/Host[.='" + p.Host + "']"
	resourcePath := base + "/*[label()=~regexp('" + p.Key + "', 'i')]"
	// ensurable's default: present when the resource manages a property.
	ensure := p.Ensure
	if ensure == "" && p.Value != nil {
		ensure = "present"
	}

	// exists? (CORE:872-874).
	aug, err := s.run.Augopen(sshConfigLens, target, resourcePath)
	if err != nil {
		return nil, nil, err
	}
	found, err := aug.Match("$resource")
	if err != nil {
		return nil, nil, err
	}
	exists := len(found) > 0

	// The value getter (SCP:126-130), for an existing entry only.
	var is []any
	if exists && p.Value != nil {
		if aug, err = s.run.Augopen(sshConfigLens, target, resourcePath); err != nil {
			return nil, nil, err
		}
		if is, err = getValue(aug, "$resource"); err != nil {
			return nil, nil, err
		}
	}

	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	var ch ral.Change
	switch {
	case ensure == "present" && !exists:
		ch = ral.Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "ssh_config_created"}
		if !noop {
			ch.Err = s.create(p, target, base, resourcePath, rubyStrings(p.Value))
		}
	case ensure == "absent" && exists:
		ch = ral.Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "ssh_config_removed"}
		if !noop {
			// destroy (CORE:880-884).
			if aug, err := s.run.Augopen(sshConfigLens, target, resourcePath); err != nil {
				ch.Err = err
			} else {
				aug.Rm("$resource")
			}
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		should := rubyStrings(p.Value)
		// array_matching :all's insync?: equal in order; nil equals nothing.
		if p.Value == nil || slices.Equal(is, should) {
			return nil, nil, nil
		}
		was, want := rubyArrayToS(is), rubyArrayToS(should)
		ch = ral.Change{Property: "value", Was: is, Is: should, Message: "value changed " + was + " to " + want + suffix,
			WasDisplay: was, IsDisplay: want, DisplayVerbatim: true}
		if !noop {
			// value= (SCP:132-137) on the $resource expression.
			if aug, err := s.run.Augopen(sshConfigLens, target, resourcePath); err != nil {
				ch.Err = err
			} else {
				ch.Err = s.setValue(aug, base, resourcePath, p.Key, should)
			}
		}
	}
	if noop || ch.Err != nil {
		return []ral.Change{ch}, nil, nil
	}
	return []ral.Change{ch}, nil, s.run.Flush(ctx, sshConfigLens)
}

// create is SCP:115-124 without the comment (native refuses comment): the
// Host entry, then the key under it.
func (s SshConfig) create(p sshConfigParams, target, base, resourcePath string, should []any) error {
	aug, err := s.run.Augopen(sshConfigLens, target, resourcePath)
	if err != nil {
		return err
	}
	aug.Set(base, p.Host)
	return s.setValue(aug, base, base+"/"+p.Key, p.Key, should)
}

// setValue is set_value (SCP:64-70): an array entry when the stock lens
// parses "<label> FOO" with a numbered child, else one entry per value.
func (s SshConfig) setValue(aug *augeas.Aug, base, path, label string, values []any) error {
	array, err := s.run.ParsedAs(sshConfigLens.Lens(), label)
	if err != nil {
		return err
	}
	if array {
		return setArrayValue(aug, base, path, label, values, false)
	}
	return setSimpleValue(aug, base, path, label, values, "", false)
}

// rubyStrings is a value property's should as getValue's Ruby Array.
func rubyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, v := range ss {
		out[i] = v
	}
	return out
}

// rubyArrayToS is format_value_for_display of an Array of Strings and nils:
// ['a', 'b'], a nil shown as undef.
func rubyArrayToS(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			parts[i] = rubyvalue.PuppetQuote(s)
		} else {
			parts[i] = "undef"
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
