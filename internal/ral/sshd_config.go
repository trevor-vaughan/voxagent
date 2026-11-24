package ral

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/runlog"
)

// augeasproviders_ssh 7.0.0's sshd_config (SDT: type/sshd_config.rb; SDP:
// provider/sshd_config/augeas.rb) under Puppet's resource harness, spec §6:
// exists?, the value property with array_append, create, destroy and CORE's
// flush.

// sshdConfigLens is SDP:15's lens; the class is Puppet::Type::Sshd_config.
var sshdConfigLens = augLens{"Sshd", "Sshd_config"}

// sshdConfigDefaultTarget is SDP:13's default_file.
const sshdConfigDefaultTarget = "/etc/ssh/sshd_config"

// sshdBasePath is base_path (SDP:25-31) with match_conditions (:159-168): the
// Match block whose conditions are exactly p's, in the condition's order.
func sshdBasePath(p sshdConfigParams) string {
	if len(p.Condition) == 0 {
		return "$target"
	}
	var b strings.Builder
	b.WriteString("$target/Match[count(Condition/*)=" + strconv.Itoa(len(p.Condition)) + "]")
	for _, c := range p.Condition {
		b.WriteString(`[Condition/` + c.Key + `="` + c.Value + `"]`)
	}
	b.WriteString("/Settings")
	return b.String()
}

// Prefetch implements Provider: sshd_config does not prefetch (spec §6).
func (s SshdConfig) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return s, nil }

// Refresh implements Provider: sshd_config has no refresh.
func (SshdConfig) Refresh(context.Context, catalog.Resource, bool) (bool, error) { return false, nil }

// Apply evaluates one Sshd_config as Puppet's harness does, as SshConfig's
// Apply, with SDT:47-83's value property: with array_append, in sync when
// every should is present, synced to is | should, and that union, retrieved
// again, is what the message shows (after the sync, or for noop or a failed
// sync instead of it).
func (s SshdConfig) Apply(ctx context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	p, err := parseSshdConfig(r)
	if err != nil {
		return nil, nil, err
	}
	target := p.Target
	if target == "" {
		target = sshdConfigDefaultTarget
	}
	base := sshdBasePath(p)
	resourcePath := base + "/*[label()=~regexp('" + p.Key + "', 'i')]"
	// ensurable's default: present when the resource manages a property.
	ensure := p.Ensure
	if ensure == "" && p.Value != nil {
		ensure = "present"
	}

	// exists? (CORE:872-874).
	aug, err := s.run.augopen(sshdConfigLens, target, resourcePath)
	if err != nil {
		return nil, nil, err
	}
	found, err := aug.Match("$resource")
	if err != nil {
		return nil, nil, err
	}
	exists := len(found) > 0

	// The value getter (SDP:203-207), for an existing entry only.
	var is []any
	if exists && p.Value != nil {
		if is, err = s.value(target, resourcePath); err != nil {
			return nil, nil, err
		}
	}

	suffix := ""
	if noop {
		suffix = " (noop)"
	}
	var ch Change
	switch {
	case ensure == "present" && !exists:
		ch = Change{Property: "ensure", Was: "absent", Is: "present", Message: "created" + suffix, Name: "sshd_config_created"}
		if !noop {
			ch.Err = s.create(p, target, base, resourcePath)
		}
	case ensure == "absent" && exists:
		ch = Change{Property: "ensure", Was: "present", Is: "absent", Message: "removed" + suffix, Name: "sshd_config_removed"}
		if !noop {
			// destroy (SDP:196-201): the entry, then every emptied Match.
			if aug, err := s.run.augopen(sshdConfigLens, target, resourcePath); err != nil {
				ch.Err = err
			} else {
				aug.Rm("$resource")
				aug.Rm("$target/Match[count(Settings/*)=0]")
			}
		}
	case !exists:
		runlog.Debugf(ctx, sources.SourcesFrom(ctx).Resource, "Nothing to manage: no ensure and the resource doesn't exist")
		return nil, nil, nil
	default:
		should := rubyStrings(p.Value)
		if p.Value == nil || sshdInsync(is, should, p.ArrayAppend) {
			return nil, nil, nil
		}
		was := rubyArrayToS(is)
		ch = Change{Property: "value", Was: is, Is: should, WasDisplay: was, DisplayVerbatim: true}
		if !noop {
			ch.Err = s.sync(p, target, base, resourcePath, should)
		}
		shown := should
		if p.ArrayAppend {
			// should_to_s (SDT:73-83) retrieves again. A raise in
			// change_to_s is the harness's failure, whose message calls
			// should_to_s once more; a raise there escapes the harness,
			// though Ruby's ensure first records a "could not create change
			// error message for value" event.
			now, err := s.value(target, resourcePath)
			if err != nil && ch.Err == nil {
				ch.Err = err
				now, err = s.value(target, resourcePath)
			}
			if err != nil {
				return nil, nil, err
			}
			shown = rubyUnion(now, should)
		}
		ch.IsDisplay = rubyArrayToS(shown)
		ch.Message = "value changed " + was + " to " + ch.IsDisplay + suffix
	}
	if noop || ch.Err != nil {
		return []Change{ch}, nil, nil
	}
	return []Change{ch}, nil, s.run.flush(ctx, sshdConfigLens)
}

// sshdInsync is SDT:47-58's insync?: with array_append every should is in
// is, else the Arrays are equal.
func sshdInsync(is, should []any, arrayAppend bool) bool {
	if !arrayAppend {
		return slices.Equal(is, should)
	}
	for _, v := range should {
		if !slices.Contains(is, v) {
			return false
		}
	}
	return true
}

// rubyUnion is Ruby's Array#|: a's then b's elements, each once, in order.
func rubyUnion(a, b []any) []any {
	var out []any
	for _, v := range slices.Concat(a, b) {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// value is the value getter (SDP:203-207): get_value of $resource.
func (s SshdConfig) value(target, resourcePath string) ([]any, error) {
	aug, err := s.run.augopen(sshdConfigLens, target, resourcePath)
	if err != nil {
		return nil, err
	}
	return getValue(aug, "$resource")
}

// sync is SDT:60-71 then value= (SDP:209-214) on the $resource expression:
// with array_append the value retrieved again, unioned with should.
func (s SshdConfig) sync(p sshdConfigParams, target, base, resourcePath string, should []any) error {
	values := should
	if p.ArrayAppend {
		is, err := s.value(target, resourcePath)
		if err != nil {
			return err
		}
		values = rubyUnion(is, should)
	}
	aug, err := s.run.augopen(sshdConfigLens, target, resourcePath)
	if err != nil {
		return err
	}
	return s.setValue(aug, base, resourcePath, p.Key, values)
}

// create is SDP:175-194 without the comment (native refuses comment): the
// Match block when the condition has none, Port/ListenAddress/AddressFamily
// placed next to each other, then the key under base.
func (s SshdConfig) create(p sshdConfigParams, target, base, resourcePath string) error {
	aug, err := s.run.augopen(sshdConfigLens, target, resourcePath)
	if err != nil {
		return err
	}
	if len(p.Condition) > 0 {
		// match_exists? (SDP:170-173): base without its /Settings step.
		m, err := aug.Match(strings.TrimSuffix(base, "/Settings"))
		if err != nil {
			return err
		}
		if len(m) == 0 {
			aug.Insert("$target/*[last()]", "Match", false)
			for _, c := range p.Condition {
				aug.Set("$target/Match[last()]/Condition/"+c.Key, c.Value)
			}
		}
	}
	// SDP:185-189, each a match only for its own key.
	for _, ins := range []struct {
		key, other, at string
		before         bool
	}{
		{"port", "ListenAddress", "ListenAddress[1]", true},
		{"listenaddress", "AddressFamily", "AddressFamily", false},
		{"addressfamily", "ListenAddress", "ListenAddress", true},
	} {
		if !strings.EqualFold(p.Key, ins.key) {
			continue
		}
		m, err := aug.Match(base + "/" + ins.other)
		if err != nil {
			return err
		}
		if len(m) > 0 {
			aug.Insert(base+"/"+ins.at, p.Key, ins.before)
		}
	}
	return s.setValue(aug, base, base+"/"+p.Key, p.Key, rubyStrings(p.Value))
}

// setValue is set_value (SDP:46-52): an array entry when the stock lens
// parses "<label> FOO" with a numbered child, else one entry per value.
func (s SshdConfig) setValue(aug *augeas.Aug, base, path, label string, values []any) error {
	array, err := s.run.parsedAs(sshdConfigLens.lens(), label)
	if err != nil {
		return err
	}
	if array {
		return setArrayValue(aug, base, path, label, values, true)
	}
	return setSimpleValue(aug, base, path, label, values, ", 'i'", true)
}
