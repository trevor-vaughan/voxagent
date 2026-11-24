package augeasproviderssysctl

import (
	"context"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/command"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/runlog"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// augeasproviders_sysctl 4.1.0's prefetch (SYP: its
// lib/puppet/provider/sysctl/augeas.rb, :58-126 and :188-196), spec §5.

// sysctlLens is SYP:19's lens; the class is Puppet::Type::Sysctl.
var sysctlLens = core.Lens{Name: "Sysctl", Type: "Sysctl"}

// sysctlDefaultTarget is default_file on the measured platform (SYP:11-17).
const sysctlDefaultTarget = "/etc/sysctl.conf"

// sysctlSlice is SYP:122's each_slice(30).
const sysctlSlice = 30

// PrefetchAtTurn implements TurnPrefetching: Puppet prefetches sysctl at the
// first Sysctl the walk traverses (spec §0).
func (s Sysctl) PrefetchAtTurn() {}

// Prefetch is SYP's prefetch with reference resources (instances, :97-126),
// rs being every Sysctl in catalog order (at least one). Every title is
// looked up in the first resource's target; then `sysctl -e` runs over
// cumulative slices of the titles. The result fills s.run's table; a load
// error or a failing command is the prefetch's error.
func (s Sysctl) Prefetch(ctx context.Context, rs []catalog.Resource) (ral.Provider, error) {
	// transaction.rb:379.
	runlog.Debugf(ctx, "Puppet", "Prefetching %s resources for %s", *s.ProviderUsed(), s.Type())
	first, err := parseSysctl(rs[0])
	if err != nil {
		return s, err
	}
	target := first.Target
	if target == "" {
		target = sysctlDefaultTarget
	}
	titles := make([]string, len(rs))
	for i, r := range rs {
		titles[i] = r.Title
	}

	// collect_augeas_resources (:58-95).
	inFile := map[string]bool{}
	aug, err := s.run.Augopen(sysctlLens, target, "$target/"+first.Name)
	if err != nil {
		return s, err
	}
	for _, t := range titles {
		if inFile[t] {
			continue
		}
		m, err := aug.Match("$target/" + t)
		if err != nil {
			return s, err
		}
		if len(m) == 0 {
			continue
		}
		// The value and the comment are read only because Ruby reads them
		// (spec §0: every call, reads included): retrieve reads the value
		// again, and native refuses comment.
		aug.Get(m[0])
		c, err := aug.Match("$target/#comment[following-sibling::*[1][self::" + t + "]]")
		if err != nil {
			return s, err
		}
		if len(c) > 0 {
			aug.Get(c[0])
		}
		inFile[t] = true
	}

	var out strings.Builder
	for _, args := range sysctlArgSlices(titles) {
		code, o, err := s.runner.run(ctx, s.sysctl, args...)
		if err != nil {
			return s, err
		}
		if code != 0 {
			return s, command.ExecFailure(append([]string{s.sysctl}, args...), false, false, code, o)
		}
		out.WriteString(o)
	}
	live := map[string]bool{}
	for _, k := range parseSysctlE(out.String()) {
		live[k] = true
	}

	table := make(map[string]core.SysctlEntry, len(titles))
	for _, t := range titles {
		table[t] = core.SysctlEntry{Valid: inFile[t] || live[t], Persist: inFile[t]}
	}
	s.run.Sysctl = table
	return s, nil
}

// sysctlArgSlices is SYP:118-126's argv per command: "-e" and the titles of
// every slice so far, so slice n names titles 1 … 30·n (M9).
func sysctlArgSlices(titles []string) [][]string {
	var out [][]string
	for end := sysctlSlice; end-sysctlSlice < len(titles); end += sysctlSlice {
		out = append(out, append([]string{"-e"}, titles[:min(end, len(titles))]...))
	}
	return out
}

// parseSysctlE is the key of each output line (SYP:157-165): Ruby's
// each_line lines, each with its bytes >= 0x80 removed (force_encoding
// US-ASCII, then scrub to nothing), the part before the first "=" stripped.
func parseSysctlE(out string) []string {
	var keys []string
	for _, line := range strings.SplitAfter(out, "\n") {
		if line == "" {
			continue
		}
		ascii := strings.Map(func(r rune) rune {
			if r >= 0x80 {
				return -1
			}
			return r
		}, line)
		key, _, _ := strings.Cut(ascii, "=")
		keys = append(keys, rubyvalue.Strip(key))
	}
	return keys
}

// rubyChomp is String#chomp: one trailing "\r\n", "\n" or "\r" removed.
func rubyChomp(s string) string {
	if t, ok := strings.CutSuffix(s, "\r\n"); ok {
		return t
	}
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		return t
	}
	return strings.TrimSuffix(s, "\r")
}

// rubyCollapseSpace is gsub(/\s+/, ' '): each run of Ruby's \s (ASCII space,
// \t, \n, \v, \f, \r only) becomes one space.
func rubyCollapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
		default:
			b.WriteByte(c)
			inSpace = false
		}
	}
	return b.String()
}
