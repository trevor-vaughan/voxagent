package augeasprovidersssh

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	core "github.com/voxagent/voxagent/internal/units/augeasproviders_core"
)

// forEachPermutation calls f with each permutation of 0..n-1 in the order of
// Ruby's Array#permutation (lexicographic by position). f must not keep order.
func forEachPermutation(n int, f func(order []int)) {
	order := make([]int, 0, n)
	used := make([]bool, n)
	var walk func()
	walk = func() {
		if len(order) == n {
			f(order)
			return
		}
		for i := range n {
			if !used[i] {
				used[i] = true
				order = append(order, i)
				walk()
				order = order[:len(order)-1]
				used[i] = false
			}
		}
	}
	walk()
}

// permutationAutoRequires is type/sshd_config.rb's autorequire as written
// (SDT:144-154): the file, then for every permutation of the condition's keys
// in Array#permutation order the bare title and the one followed by
// " in <target>", each where managed. It is the oracle SshdConfig.AutoRequires
// must equal.
func permutationAutoRequires(r catalog.Resource, managed func(string) bool) []string {
	p, err := parseSshdConfig(r)
	if err != nil {
		return nil
	}
	refs := core.TargetRequire(p.Target, managed)
	if len(p.Condition) == 0 {
		return refs
	}
	forEachPermutation(len(p.Condition), func(order []int) {
		name := ""
		for i, at := range order {
			if i > 0 {
				name += " "
			}
			name += p.Condition[at].Key + " " + p.Condition[at].Value
		}
		for _, t := range []string{name, name + " in " + p.Target} {
			if ref := "Sshd_config_match[" + t + "]"; managed(ref) {
				refs = append(refs, ref)
			}
		}
	})
	return refs
}

func TestForEachPermutationIsRubyOrder(t *testing.T) {
	var got []string
	forEachPermutation(3, func(order []int) { got = append(got, fmt.Sprint(order)) })
	want := []string{"[0 1 2]", "[0 2 1]", "[1 0 2]", "[1 2 0]", "[2 0 1]", "[2 1 0]"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// matchCatalog is a catalog of Sshd_config_match resources titled names.
func matchCatalog(names ...string) *catalog.Catalog {
	cat := &catalog.Catalog{}
	for _, n := range names {
		cat.Resources = append(cat.Resources, catalog.Resource{Type: "Sshd_config_match", Title: n})
	}
	return cat
}

// matchCandidates builds a catalog's Sshd_config_match names for pairs and
// target: right permutations bare and suffixed, near misses, and repeats.
// tokens are the condition as written, whose repeated keys' overwritten
// values make near misses too.
func matchCandidates(rng *rand.Rand, pairs []condPair, tokens []string, target string) []string {
	join := func(ps []condPair) string {
		parts := make([]string, 0, 2*len(ps))
		for _, c := range ps {
			parts = append(parts, c.Key, c.Value)
		}
		return strings.Join(parts, " ")
	}
	shuffled := func() []condPair {
		ps := slices.Clone(pairs)
		rng.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		return ps
	}
	var out []string
	for range rng.IntN(12) {
		ps := shuffled()
		name := join(ps)
		switch rng.IntN(16) {
		case 0, 1, 2:
			out = append(out, name)
		case 3, 4, 5:
			out = append(out, name+" in "+target)
		case 6: // an extra token
			out = append(out, name+" z")
		case 7: // a pair missing
			if len(ps) > 0 {
				out = append(out, join(ps[1:]))
			}
		case 8: // a doubled space
			if i := strings.Index(name, " "); i >= 0 {
				out = append(out, name[:i]+" "+name[i:])
			}
		case 9: // a wrong value
			if len(ps) > 0 {
				ps[rng.IntN(len(ps))].Value += "x"
				out = append(out, join(ps))
			}
		case 10: // another target, or the target with none set
			out = append(out, name+" in /etc/other", name+" in /etc/ssh/sshd_config")
		case 11: // a pair repeated
			if len(ps) > 0 {
				out = append(out, join(append(ps, ps[0])))
			}
		case 12: // the suffix alone, or trailing space
			out = append(out, name+" in", name+" ", " "+name)
		case 13: // a name already listed
			if len(out) > 0 {
				out = append(out, out[rng.IntN(len(out))])
			}
		case 14: // the condition as written, overwritten values included
			written := strings.Join(tokens, " ")
			out = append(out, written, written+" in "+target)
			for i := 0; i+1 < len(tokens); i += 2 {
				out = append(out, tokens[i]+" "+tokens[i+1])
			}
		case 15: // a tab for a space
			if i := strings.Index(name, " "); i >= 0 {
				out = append(out, name[:i]+"\t"+name[i+1:])
			}
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestSshdAutoRequiresMatchesPermutations: for random conditions and
// catalogs, the linear match gives the permutation oracle's refs in its order.
func TestSshdAutoRequiresMatchesPermutations(t *testing.T) {
	const iterations = 20000
	rng := rand.New(rand.NewPCG(91, 2026))
	keys := []string{"Host", "User", "Group", "Address", "in", "A", "B", "LocalPort"}
	values := []string{"a", "b", "in", "Host", "*.example.net", "root", "10.0.0.1"}
	targets := []string{"", "/etc/ssh/sshd_config", "/etc/ssh/other"}
	// several counts iterations whose oracle orders two or more match refs,
	// so the comparison is not vacuous.
	several := 0
	for i := range iterations {
		var tokens []string
		for range rng.IntN(9) {
			tokens = append(tokens, keys[rng.IntN(len(keys))], values[rng.IntN(len(values))])
		}
		params := map[string]any{"value": "v"}
		if len(tokens) > 0 {
			params["condition"] = strings.Join(tokens, " ")
		}
		target := targets[rng.IntN(len(targets))]
		if target != "" {
			params["target"] = target
		}
		r := raltest.AugRes("Sshd_config", "PermitRootLogin", params)
		p, err := parseSshdConfig(r)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		names := matchCandidates(rng, p.Condition, tokens, target)
		known := map[string]bool{"File[" + target + "]": rng.IntN(2) == 0}
		for _, n := range names {
			// Mostly the catalog's names; now and then one managed refuses.
			known["Sshd_config_match["+n+"]"] = rng.IntN(8) > 0
		}
		managed := func(ref string) bool { return known[ref] }
		want := permutationAutoRequires(r, managed)
		got := SshdConfig{cat: matchCatalog(names...)}.AutoRequires(r, managed)
		if !slices.Equal(got, want) {
			t.Fatalf("iteration %d: condition %q target %q names %q\n got %q\nwant %q",
				i, params["condition"], target, names, got, want)
		}
		if strings.Count(strings.Join(want, "\n"), "Sshd_config_match[") >= 2 {
			several++
		}
	}
	if several < iterations/10 {
		t.Errorf("only %d of %d iterations ordered two or more matches", several, iterations)
	}
	t.Logf("%d of %d iterations ordered two or more matches", several, iterations)
}

// TestSshdAutoRequiresTwelveKeys: twelve keys (12! permutations) stay native
// and the autorequire asks managed only about the catalog's matching names.
func TestSshdAutoRequiresTwelveKeys(t *testing.T) {
	keys := strings.Fields("A B C D E F G H I J K L")
	var forward, backward []string
	for i, k := range keys {
		forward = append(forward, k, strings.ToLower(k))
		j := len(keys) - 1 - i
		backward = append(backward, keys[j], strings.ToLower(keys[j]))
	}
	cond := strings.Join(forward, " ")
	rev := strings.Join(backward, " ")
	r := raltest.AugRes("Sshd_config", "x", map[string]any{"value": "v", "condition": cond, "target": "/etc/ssh/sshd_config"})
	if err := (SshdConfig{}).Validate(r); err != nil {
		t.Fatalf("twelve keys: %v", err)
	}
	names := []string{rev, cond + " in /etc/ssh/sshd_config", "unrelated", cond}
	var asked int
	managed := func(string) bool { asked++; return true }
	got := SshdConfig{cat: matchCatalog(names...)}.AutoRequires(r, managed)
	want := []string{
		"File[/etc/ssh/sshd_config]",
		"Sshd_config_match[" + cond + "]",
		"Sshd_config_match[" + cond + " in /etc/ssh/sshd_config]",
		"Sshd_config_match[" + rev + "]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if asked != len(want) {
		t.Errorf("managed asked %d time(s), want %d", asked, len(want))
	}
}

// sshdMatchNames: each Sshd_config_match's title, then its alias strings,
// flattened as Puppet flattens them; other types and non-strings are not names.
func TestSshdMatchNames(t *testing.T) {
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Sshd_config_match", Title: "Host a", Parameters: map[string]any{"alias": "User b"}},
		{Type: "Sshd_config", Title: "Host z", Parameters: map[string]any{"alias": "User z"}},
		{Type: "sshd_config_match", Title: "m", Parameters: map[string]any{"alias": []any{"Host c", []any{"Host d"}, float64(1), true}}},
		{Type: "Sshd_config_match", Title: "n"},
	}}
	want := []string{"Host a", "User b", "m", "Host c", "Host d", "n"}
	if got := sshdMatchNames(cat); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
