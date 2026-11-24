package extension

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// rubyVectorScript loads host.rb (defining VoxagentHost without starting the
// server) and prints, for the shared vectors, each match case's redacted text
// as hex and each form case's forms, in file order. A null form entry is a
// case Ruby does not cover.
const rubyVectorScript = `
require 'json'
load ARGV[0]
v = JSON.parse(File.read(ARGV[1]))
unhex = ->(plain, hexed) { hexed ? [hexed].pack('H*') : plain }
match = v['match'].map do |c|
  VoxagentHost.redact(unhex.(c['text'], c['text_hex']), c['forms']).unpack1('H*')
end
forms = v['forms'].map do |c|
  c['name'] == 'integral-float' ? nil : VoxagentHost.forms(c['value'])
end
puts JSON.generate('match' => match, 'forms' => forms)
`

// TestHostRedactMatchesSharedVectors holds host.rb's redactor to the vectors
// the Go scrubber (internal/lib/redact) is tested against.
func TestHostRedactMatchesSharedVectors(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby is not on PATH")
	}
	const vectorsPath = "../lib/redact/testdata/vectors.json"
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Match []struct {
			Name    string `json:"name"`
			Want    string `json:"want"`
			WantHex string `json:"want_hex"`
		} `json:"match"`
		Forms []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
			Forms []string        `json:"forms"`
		} `json:"forms"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(ruby, "-e", rubyVectorScript, "host.rb", vectorsPath)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ruby: %v\n%s", err, stderr.String())
	}
	var got struct {
		Match []string   `json:"match"`
		Forms [][]string `json:"forms"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("ruby output: %v\n%s", err, out)
	}
	if len(got.Match) != len(v.Match) || len(got.Forms) != len(v.Forms) {
		t.Fatalf("ruby returned %d match and %d forms results, want %d and %d",
			len(got.Match), len(got.Forms), len(v.Match), len(v.Forms))
	}

	for i, c := range v.Match {
		t.Run("match/"+c.Name, func(t *testing.T) {
			want := c.Want
			if c.WantHex != "" {
				b, err := hex.DecodeString(c.WantHex)
				if err != nil {
					t.Fatal(err)
				}
				want = string(b)
			}
			// Ruby's redact ends in utf8(): text that is not valid UTF-8
			// is read as US-ASCII, so every byte of 0x80 or more, valid
			// sequences' too, becomes one U+FFFD.
			if !utf8.ValidString(want) {
				var replaced strings.Builder
				for i := range len(want) {
					if want[i] < utf8.RuneSelf {
						replaced.WriteByte(want[i])
					} else {
						replaced.WriteRune(utf8.RuneError)
					}
				}
				want = replaced.String()
			}
			b, err := hex.DecodeString(got.Match[i])
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != want {
				t.Errorf("redact = %q, want %q", b, want)
			}
		})
	}

	for i, c := range v.Forms {
		if got.Forms[i] == nil {
			continue
		}
		t.Run("forms/"+c.Name, func(t *testing.T) {
			// Ruby prints a Hash as {"k" => v}, which Go's formatting does not
			// reproduce, so a Hash case may carry extra forms; no other may.
			hashCase := bytes.ContainsRune(c.Value, '{')
			for _, w := range c.Forms {
				if !slices.Contains(got.Forms[i], w) {
					t.Errorf("forms lack %q (got %q)", w, got.Forms[i])
				}
			}
			for _, f := range got.Forms[i] {
				if slices.Contains(c.Forms, f) {
					continue
				}
				if !hashCase || !strings.HasPrefix(f, "{") {
					t.Errorf("unexpected extra form %q (hash case: %v)", f, hashCase)
				}
			}
		})
	}
}
