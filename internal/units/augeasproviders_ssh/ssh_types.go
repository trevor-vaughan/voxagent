package augeasprovidersssh

import (
	"slices"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/ral"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// The three augeasproviders_ssh types. Their Apply, Prefetch and Refresh come
// with the run contracts (spec §5, §6): SshConfig's are in ssh_config.go,
// SshdConfig's in sshd_config.go, SshdConfigSubsystem's in sshd_subsystem.go.
// Route routes and builds them (manifest.go). Their attributes and validation
// are spec §3.

// SshConfig is the native augeas provider for augeasproviders_ssh's ssh_config.
type SshConfig struct{ run *core.Run }

// SshdConfig is the native augeas provider for sshd_config.
type SshdConfig struct {
	run *core.Run
	// cat is the run's catalog. AutoRequires reads its Sshd_config_match
	// names (sshdMatchNames) when the graph is built, after the host's
	// load-time generation has merged into it, so a match a purging resources
	// generates is seen as Ruby sees it. (User's CatalogGroups is read before
	// the merge, which is harmless: a purge-generated group sets no gid.)
	cat *catalog.Catalog
}

// SshdConfigSubsystem is the native augeas provider for sshd_config_subsystem.
type SshdConfigSubsystem struct{ run *core.Run }

// EndRun implements RunEnding: each type ends the run it shares with the
// others, which closes every handle once however many of them call it.
func (s SshConfig) EndRun()           { s.run.EndRun() }
func (s SshdConfig) EndRun()          { s.run.EndRun() }
func (s SshdConfigSubsystem) EndRun() { s.run.EndRun() }

func (SshConfig) Type() string           { return "ssh_config" }
func (SshdConfig) Type() string          { return "sshd_config" }
func (SshdConfigSubsystem) Type() string { return "sshd_config_subsystem" }

func (SshConfig) ProviderUsed() *string           { return &augeas.ProviderName }
func (SshdConfig) ProviderUsed() *string          { return &augeas.ProviderName }
func (SshdConfigSubsystem) ProviderUsed() *string { return &augeas.ProviderName }

// Attrs is every attribute the type declares, name and provider included
// (spec §3's allattrs).
func (SshConfig) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "value", "comment", "key", "target", "host")
}

func (SshdConfig) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "value", "comment", "key", "array_append", "target", "condition")
}

func (SshdConfigSubsystem) Attrs() ral.Attrs {
	return ral.NewAttrs("name", "provider", "ensure", "command", "comment", "target")
}

// Validate implements Validating: the first failure in allattrs order, which
// is Ruby's. A value native does not implement is an *rubyvalue.UnimplementedValue.
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

// Validation refused an unparseable resource before the graph is built, so a
// parse error here means no edge.

func (SshConfig) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSshConfig(r)
	if err != nil {
		return nil
	}
	return core.TargetRequire(p.Target, managed)
}

func (SshdConfigSubsystem) AutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSubsystem(r)
	if err != nil {
		return nil
	}
	return core.TargetRequire(p.Target, managed)
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
	refs := core.TargetRequire(p.Target, managed)
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
