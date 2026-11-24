package ral

import (
	"context"
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// The four augeasproviders types. Their Apply, Prefetch and Refresh come with
// the run contracts (spec §5, §6): Sysctl's are in sysctl_*.go, SshConfig's in
// ssh_config.go, SshdConfig's in sshd_config.go, SshdConfigSubsystem's in
// sshd_subsystem.go. NativeAugeas routes them and AugeasTypes.Providers
// builds them (augeas_route.go). Their attributes and validation are spec §3.

// sysctlRunner runs the sysctl command and returns its combined output.
type sysctlRunner interface {
	run(ctx context.Context, name string, args ...string) (int, string, error)
}

// Sysctl is the native augeas provider for augeasproviders_sysctl's sysctl.
type Sysctl struct {
	run *augeasRun
	// sysctl is the command's path, resolved when the type is routed.
	sysctl string
	runner sysctlRunner
}

// SshConfig is the native augeas provider for augeasproviders_ssh's ssh_config.
type SshConfig struct{ run *augeasRun }

// SshdConfig is the native augeas provider for sshd_config.
type SshdConfig struct {
	run *augeasRun
	// cat is the run's catalog. AutoRequires reads its Sshd_config_match
	// names (sshdMatchNames) when the graph is built, after the host's
	// load-time generation has merged into it, so a match a purging resources
	// generates is seen as Ruby sees it. (User's CatalogGroups is read before
	// the merge, which is harmless: a purge-generated group sets no gid.)
	cat *catalog.Catalog
}

// SshdConfigSubsystem is the native augeas provider for sshd_config_subsystem.
type SshdConfigSubsystem struct{ run *augeasRun }

// argvSysctl runs sysctl as Puppet's optional_commands do: runArgvWith, so
// Ruby's locale override, combined output and the Executing line.
type argvSysctl struct{}

func (argvSysctl) run(ctx context.Context, name string, args ...string) (int, string, error) {
	return runArgvWith(ctx, argvOpts{}, name, args...)
}

// EndRun implements RunEnding: each type ends the run it shares with the
// others, which closes every handle once however many of them call it.
func (s Sysctl) EndRun()              { s.run.EndRun() }
func (s SshConfig) EndRun()           { s.run.EndRun() }
func (s SshdConfig) EndRun()          { s.run.EndRun() }
func (s SshdConfigSubsystem) EndRun() { s.run.EndRun() }

// augeasProvider is the provider name all four report: the only provider
// directory the routing admits holds augeas.rb.
var augeasProvider = "augeas"

func (Sysctl) Type() string              { return "sysctl" }
func (SshConfig) Type() string           { return "ssh_config" }
func (SshdConfig) Type() string          { return "sshd_config" }
func (SshdConfigSubsystem) Type() string { return "sshd_config_subsystem" }

func (Sysctl) ProviderUsed() *string              { return &augeasProvider }
func (SshConfig) ProviderUsed() *string           { return &augeasProvider }
func (SshdConfig) ProviderUsed() *string          { return &augeasProvider }
func (SshdConfigSubsystem) ProviderUsed() *string { return &augeasProvider }

// Attrs is every attribute the type declares, name and provider included
// (spec §3's allattrs).
func (Sysctl) Attrs() Attrs {
	return NewAttrs("name", "provider", "ensure", "val", "value", "comment", "target", "apply", "persist", "silent")
}

func (SshConfig) Attrs() Attrs {
	return NewAttrs("name", "provider", "ensure", "value", "comment", "key", "target", "host")
}

func (SshdConfig) Attrs() Attrs {
	return NewAttrs("name", "provider", "ensure", "value", "comment", "key", "array_append", "target", "condition")
}

func (SshdConfigSubsystem) Attrs() Attrs {
	return NewAttrs("name", "provider", "ensure", "command", "comment", "target")
}

// Validate implements Validating: the first failure in allattrs order, which
// is Ruby's. A value native does not implement is an *unimplementedValue.
func (Sysctl) Validate(r catalog.Resource) error {
	_, err := parseSysctl(r)
	return err
}

func (SshConfig) Validate(r catalog.Resource) error {
	_, err := parseSshConfig(r)
	return err
}

func (SshdConfig) Validate(r catalog.Resource) error {
	_, err := parseSshdConfig(r)
	return err
}

func (SshdConfigSubsystem) Validate(r catalog.Resource) error {
	_, err := parseSubsystem(r)
	return err
}

// targetRequire is autorequire(:file) { self[:target] }: the File for a target
// the catalog sets, where it is managed. An unset target means no edge, not
// the default file.
func targetRequire(target string, managed func(string) bool) []string {
	if ref := "File[" + target + "]"; target != "" && managed(ref) {
		return []string{ref}
	}
	return nil
}

// Validation refused an unparseable resource before the graph is built, so a
// parse error here means no edge.

func (Sysctl) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSysctl(r)
	if err != nil {
		return nil
	}
	return targetRequire(p.Target, managed)
}

func (SshConfig) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSshConfig(r)
	if err != nil {
		return nil
	}
	return targetRequire(p.Target, managed)
}

func (SshdConfigSubsystem) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSubsystem(r)
	if err != nil {
		return nil
	}
	return targetRequire(p.Target, managed)
}

// AutoRequires is the file, then for a condition every permutation of its keys
// as an Sshd_config_match title, "<k1> <v1> <k2> <v2>", and the same followed
// by " in <target>", which is " in " when the target is unset (SDT:144-154).
//
// Ruby builds all n! permutations; this walks the catalog's names instead and
// keeps each that is one of them, in Ruby's order: by the permutation's
// position in Array#permutation (lexicographic by key index), the bare title
// before the suffixed one.
func (s SshdConfig) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSshdConfig(r)
	if err != nil {
		return nil
	}
	refs := targetRequire(p.Target, managed)
	if len(p.Condition) == 0 {
		return refs
	}
	keyAt := make(map[string]int, len(p.Condition))
	for i, c := range p.Condition {
		keyAt[c.Key] = i
	}
	// order is the name's condition indexes in title order, nil when the name
	// is no permutation: exactly 2n single-space-separated tokens, each key
	// once with its value.
	order := func(name string) []int {
		tokens := strings.Split(name, " ")
		if len(tokens) != 2*len(p.Condition) {
			return nil
		}
		out := make([]int, 0, len(p.Condition))
		used := make([]bool, len(p.Condition))
		for i := 0; i < len(tokens); i += 2 {
			at, ok := keyAt[tokens[i]]
			if !ok || used[at] || p.Condition[at].Value != tokens[i+1] {
				return nil
			}
			used[at] = true
			out = append(out, at)
		}
		return out
	}
	type match struct {
		order    []int
		suffixed bool
		ref      string
	}
	var matches []match
	seen := map[string]bool{}
	for _, name := range sshdMatchNames(s.cat) {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref := "Sshd_config_match[" + name + "]"
		if o := order(name); o != nil && managed(ref) {
			matches = append(matches, match{o, false, ref})
		}
		if bare, ok := strings.CutSuffix(name, " in "+p.Target); ok {
			if o := order(bare); o != nil && managed(ref) {
				matches = append(matches, match{o, true, ref})
			}
		}
	}
	slices.SortFunc(matches, func(a, b match) int {
		if c := slices.Compare(a.order, b.order); c != 0 {
			return c
		}
		switch {
		case a.suffixed == b.suffixed:
			return 0
		case b.suffixed:
			return -1
		}
		return 1
	})
	for _, m := range matches {
		refs = append(refs, m.ref)
	}
	return refs
}
