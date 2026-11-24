package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

type memTree map[string]string

func (t memTree) Read(rel string) ([]byte, error) {
	s, ok := t[rel]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", rel, os.ErrNotExist)
	}
	return []byte(s), nil
}

func (t memTree) List(rel string) ([]string, error) { return nil, os.ErrNotExist }

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func release(version, path, content string) Release {
	return Release{
		Version: version,
		Files:   []FileDigest{{Path: path, SHA256: sum(content)}},
		Types:   map[string][]string{"x": {path}},
	}
}

func TestMatchFilesMatch(t *testing.T) {
	tree := memTree{"puppet/type/x.rb": "new"}
	m := Manifest{Name: "u", Releases: []Release{
		release("10.0.0", "puppet/type/x.rb", "old"),
		release("10.0.1", "puppet/type/x.rb", "new"),
	}}
	rel, reason, ok := MatchFiles(tree, m, "x", nil)
	if !ok || reason != "" || rel.Version != "10.0.1" {
		t.Fatalf("got %q %q %v", rel.Version, reason, ok)
	}
}

func TestMatchFilesMismatchReason(t *testing.T) {
	tree := memTree{"puppet/type/x.rb": "got"}
	m := Manifest{Name: "u", Releases: []Release{release("1", "puppet/type/x.rb", "want")}}
	_, reason, ok := MatchFiles(tree, m, "x", nil)
	want := fmt.Sprintf("puppet/type/x.rb has sha256 %s, not the measured %s", sum("got"), sum("want"))
	if ok || reason != want {
		t.Fatalf("got %v %q, want %q", ok, reason, want)
	}
}

func TestMatchFilesMissingFile(t *testing.T) {
	m := Manifest{Name: "u", Releases: []Release{release("1", "puppet/type/x.rb", "a")}}
	_, reason, ok := MatchFiles(memTree{}, m, "x", nil)
	if ok || !strings.HasPrefix(reason, "puppet/type/x.rb: ") {
		t.Fatalf("got %v %q", ok, reason)
	}
}

func TestMatchFilesReference(t *testing.T) {
	tree := memTree{"puppet/type/x.rb": "a", "puppet/feature/augeas.rb": "f"}
	lib := Manifest{Name: "lib", Releases: []Release{{Version: "1", Files: []FileDigest{{Path: "puppet/feature/augeas.rb", SHA256: sum("f")}}}}}
	m := Manifest{Name: "u", Releases: []Release{{
		Version: "1",
		Files:   []FileDigest{{Path: "puppet/type/x.rb", SHA256: sum("a")}},
		Types:   map[string][]string{"x": {"puppet/type/x.rb", "lib:puppet/feature/augeas.rb"}},
	}}}
	if _, reason, ok := MatchFiles(tree, m, "x", []Manifest{lib}); !ok {
		t.Fatalf("no match: %q", reason)
	}
	tree["puppet/feature/augeas.rb"] = "other"
	_, reason, ok := MatchFiles(tree, m, "x", []Manifest{lib})
	want := fmt.Sprintf("puppet/feature/augeas.rb has sha256 %s, not the measured %s", sum("other"), sum("f"))
	if ok || reason != want {
		t.Fatalf("got %v %q, want %q", ok, reason, want)
	}
}

func TestMatchFilesUnknownReference(t *testing.T) {
	tree := memTree{"puppet/type/x.rb": "a"}
	m := Manifest{Name: "u", Releases: []Release{{
		Version: "1",
		Files:   []FileDigest{{Path: "puppet/type/x.rb", SHA256: sum("a")}},
		Types:   map[string][]string{"x": {"nope:puppet/feature/augeas.rb"}},
	}}}
	_, reason, ok := MatchFiles(tree, m, "x", nil)
	if ok || reason != "puppet/feature/augeas.rb: not listed in the nope manifest" {
		t.Fatalf("got %v %q", ok, reason)
	}
	lib := Manifest{Name: "nope", Releases: []Release{{Version: "1"}}}
	_, reason, ok = MatchFiles(tree, m, "x", []Manifest{lib})
	if ok || reason != "puppet/feature/augeas.rb: not listed in the nope manifest" {
		t.Fatalf("path absent: got %v %q", ok, reason)
	}
}

func TestMatchFilesOwnPathAbsentFromRelease(t *testing.T) {
	tree := memTree{"a.rb": "a"}
	m := Manifest{Name: "u", Releases: []Release{{Version: "1", Types: map[string][]string{"x": {"a.rb"}}}}}
	_, reason, ok := MatchFiles(tree, m, "x", nil)
	if ok || reason != "a.rb: not listed in the u manifest" {
		t.Fatalf("got %v %q", ok, reason)
	}
}

// A tree holding file A of release 1 and file B of release 2 matches neither.
func TestMatchFilesMixedReleasesIsNoMatch(t *testing.T) {
	tree := memTree{"a.rb": "a1", "b.rb": "b2"}
	m := Manifest{Name: "u", Releases: []Release{
		{Version: "1", Files: []FileDigest{{"a.rb", sum("a1")}, {"b.rb", sum("b1")}}, Types: map[string][]string{"x": {"a.rb", "b.rb"}}},
		{Version: "2", Files: []FileDigest{{"a.rb", sum("a2")}, {"b.rb", sum("b2")}}, Types: map[string][]string{"x": {"a.rb", "b.rb"}}},
	}}
	if rel, _, ok := MatchFiles(tree, m, "x", nil); ok {
		t.Fatalf("matched release %q", rel.Version)
	}
}

func TestMatchFilesReasonFromLastRelease(t *testing.T) {
	tree := memTree{"a.rb": "got"}
	m := Manifest{Name: "u", Releases: []Release{
		{Version: "1", Files: []FileDigest{{"a.rb", sum("w1")}}, Types: map[string][]string{"x": {"a.rb"}}},
		{Version: "2", Files: []FileDigest{{"a.rb", sum("w2")}}, Types: map[string][]string{"x": {"a.rb"}}},
	}}
	_, reason, _ := MatchFiles(tree, m, "x", nil)
	want := fmt.Sprintf("a.rb has sha256 %s, not the measured %s", sum("got"), sum("w2"))
	if reason != want {
		t.Fatalf("got %q, want %q", reason, want)
	}
}

func TestMatchFilesTypeAbsent(t *testing.T) {
	m := Manifest{Name: "u", Releases: []Release{release("1", "a.rb", "a")}}
	_, reason, ok := MatchFiles(memTree{}, m, "y", nil)
	if ok || reason != "y: no measured release lists it" {
		t.Fatalf("got %v %q", ok, reason)
	}
}
