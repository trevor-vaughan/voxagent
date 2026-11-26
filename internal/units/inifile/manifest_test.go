package inifile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
)

// specDigests is spec §2's table: per release and file, the digest's first
// eight and last characters, written "prefix…suffix".
var specDigests = map[string]map[string]string{
	"6.5.0": {
		"puppet/util/external_iterator.rb":       "f5fe4626…db73",
		"puppet/util/ini_file.rb":                "af5c5c80…5068",
		"puppet/util/ini_file/section.rb":        "af071d70…3e4b",
		"puppet/util/setting_value.rb":           "265a397a…cf94",
		"puppet/type/ini_setting.rb":             "73811669…9185",
		"puppet/provider/ini_setting/ruby.rb":    "8f2ef19a…880b",
		"puppet/type/ini_subsetting.rb":          "38140bb3…a95c",
		"puppet/provider/ini_subsetting/ruby.rb": "071acf48…3bc0",
	},
	"6.6.0": {
		"puppet/util/external_iterator.rb":       "f5fe4626…db73",
		"puppet/util/ini_file.rb":                "939b8d77…3d66",
		"puppet/util/ini_file/section.rb":        "af9ba52e…0bed",
		"puppet/util/setting_value.rb":           "265a397a…cf94",
		"puppet/type/ini_setting.rb":             "73811669…9185",
		"puppet/provider/ini_setting/ruby.rb":    "275c9937…22fd",
		"puppet/type/ini_subsetting.rb":          "38140bb3…a95c",
		"puppet/provider/ini_subsetting/ruby.rb": "ba1765d7…de90b",
		"puppet/type/ini_section.rb":             "52f3feaf…7d36",
		"puppet/provider/ini_section/ruby.rb":    "a844a97f…666f",
	},
	"6.6.1": {
		"puppet/util/external_iterator.rb":       "f5fe4626…db73",
		"puppet/util/ini_file.rb":                "70e6dc6e…7599",
		"puppet/util/ini_file/section.rb":        "af9ba52e…0bed",
		"puppet/util/setting_value.rb":           "265a397a…cf94",
		"puppet/type/ini_setting.rb":             "73811669…9185",
		"puppet/provider/ini_setting/ruby.rb":    "cde0831b…2e64",
		"puppet/type/ini_subsetting.rb":          "38140bb3…a95c",
		"puppet/provider/ini_subsetting/ruby.rb": "ba1765d7…de90b",
		"puppet/type/ini_section.rb":             "52f3feaf…7d36",
		"puppet/provider/ini_section/ruby.rb":    "b0844bc5…dbfdd0",
	},
}

// Each release's testdata copy hashes to its manifest digest, and the digest
// is the one spec §2 lists.
func TestTestdataMatchesManifest(t *testing.T) {
	if len(Manifest.Releases) != len(specDigests) {
		t.Fatalf("manifest has %d releases, spec has %d", len(Manifest.Releases), len(specDigests))
	}
	for _, rel := range Manifest.Releases {
		want := specDigests[rel.Version]
		if len(rel.Files) != len(want) {
			t.Errorf("release %s: %d files, spec lists %d", rel.Version, len(rel.Files), len(want))
		}
		for _, f := range rel.Files {
			b, err := os.ReadFile(filepath.Join("testdata", rel.Version, "lib", f.Path))
			if err != nil {
				t.Fatal(err)
			}
			if got := raltest.SHA256Hex(b); got != f.SHA256 {
				t.Errorf("release %s %s: testdata sha256 %s, manifest %s", rel.Version, f.Path, got, f.SHA256)
			}
			prefix, suffix, _ := strings.Cut(want[f.Path], "…")
			if prefix == "" || !strings.HasPrefix(f.SHA256, prefix) || !strings.HasSuffix(f.SHA256, suffix) {
				t.Errorf("release %s %s: manifest %s, spec %s", rel.Version, f.Path, f.SHA256, want[f.Path])
			}
		}
	}
}

// Every file a type lists has a digest; ini_section is absent from 6.5.0.
func TestManifestTypeFiles(t *testing.T) {
	if want := []string{"ini_setting", "ini_subsetting", "ini_section"}; !slices.Equal(Manifest.Types, want) {
		t.Errorf("Types = %v, want %v", Manifest.Types, want)
	}
	for _, rel := range Manifest.Releases {
		listed := map[string]bool{}
		for _, f := range rel.Files {
			listed[f.Path] = true
		}
		for typ, files := range rel.Types {
			if !slices.Contains(Manifest.Types, typ) {
				t.Errorf("release %s lists unknown type %s", rel.Version, typ)
			}
			for _, f := range files {
				if !listed[f] {
					t.Errorf("release %s %s: %s has no digest", rel.Version, typ, f)
				}
			}
		}
		_, hasSection := rel.Types["ini_section"]
		if hasSection != (rel.Version != "6.5.0") {
			t.Errorf("release %s: ini_section listed = %v", rel.Version, hasSection)
		}
	}
}
