package tidy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
)

// tidyRbSHA256 is type/tidy.rb of puppet 8.10.0, the file Attrs mirrors.
const tidyRbSHA256 = "7e5e3ce41de4e2b8681b12aa948110b2f9342a8b75b41e0e28ecdbe0d106a32a"

func TestAttrsCoverTidyRb(t *testing.T) {
	src, err := os.ReadFile("testdata/tidy.rb")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(src); hex.EncodeToString(sum[:]) != tidyRbSHA256 {
		t.Fatalf("testdata/tidy.rb sha256 = %x, want %s", sum, tidyRbSHA256)
	}
	var want []string
	for _, m := range regexp.MustCompile(`(?m)^\s*newparam\(:(\w+)`).FindAllStringSubmatch(string(src), -1) {
		want = append(want, m[1])
	}
	slices.Sort(want)
	if got := (Tidy{}).Attrs().Sorted(); !slices.Equal(got, want) {
		t.Errorf("Attrs = %v, want %v", got, want)
	}
	if len(want) != 9 {
		t.Errorf("parsed %d newparams, want 9", len(want))
	}
	if p := (Tidy{}).Properties().Sorted(); len(p) != 0 {
		t.Errorf("Properties = %v, want none", p)
	}
}

func TestApplyChangesNothing(t *testing.T) {
	var p Tidy
	r := raltest.Res("Tidy", "/var/tmp/a", map[string]any{"age": "1d"})
	for _, noop := range []bool{false, true} {
		ch, w, err := p.Apply(context.Background(), r, noop)
		if len(ch) != 0 || len(w) != 0 || err != nil {
			t.Errorf("Apply(noop=%v) = %v, %v, %v", noop, ch, w, err)
		}
	}
	if pr, err := p.Prefetch(context.Background(), nil); err != nil || pr != (Tidy{}) {
		t.Errorf("Prefetch = %v, %v", pr, err)
	}
	if ref, err := p.Refresh(context.Background(), r, false); ref || err != nil {
		t.Errorf("Refresh = %v, %v", ref, err)
	}
	if p.ProviderUsed() != nil || p.Type() != "tidy" {
		t.Error("Type/ProviderUsed wrong")
	}
}
