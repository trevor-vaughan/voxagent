package ral

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// concatTypeFiles are the files whose digests identify a concat release, in
// the order of concatFingerprints' entries.
var concatTypeFiles = [2]string{"puppet/type/concat_file.rb", "puppet/type/concat_fragment.rb"}

// concatFingerprints are the sha256 digests of each measured release's
// concatTypeFiles (spec §0 row 2).
var concatFingerprints = map[ConcatVersion][2]string{
	Concat910: {"7872760598bf35c8e3b285d89daecc4fa29c727a456e47118626a2c58f9c0d6f",
		"ee003b5d10550f6104fde62ad46f5296ce9361a6a4c1e5ef6d973333f30585ef"},
	Concat1001: {"e38f130ed5ccb6bc0bb3c7a047ead0ace40ebcbb8d2f408acf46f10b8b9c59c6",
		"1a53e85c6a873be95eb4b410153e3632658d01c0c65b5f2e0f2886a6f0cb067d"},
}

// NativeConcat says whether concat_file and concat_fragment are applied
// natively for cat, and as which release; otherwise both go to the extension
// host, which is the oracle for everything not measured (spec §0 rows 1-2).
//
// Both type files in t must match the digests of the same measured release.
// A missing, unreadable or oversized file, an unknown digest or a mixed pair
// delegates. So does a Concat_file or Concat_fragment with a shape the
// native parser does not implement (rubyvalue.UnimplementedValue), and two
// Concat_files sharing a path, which Ruby generates one File for. Ruby's own
// refusals stay native.
func NativeConcat(t PluginTree, cat *catalog.Catalog) (ConcatVersion, bool) {
	var sums [2]string
	for i, rel := range concatTypeFiles {
		b, err := t.Read(rel)
		if err != nil {
			return 0, false
		}
		sum := sha256.Sum256(b)
		sums[i] = hex.EncodeToString(sum[:])
	}
	var v ConcatVersion
	for known, want := range concatFingerprints {
		if sums == want {
			v = known
		}
	}
	if v == 0 {
		return 0, false
	}
	var uv *rubyvalue.UnimplementedValue
	paths := map[string]bool{}
	for _, r := range cat.Resources {
		var err error
		switch strings.ToLower(r.Type) {
		case "concat_file":
			var p concatFileParams
			p, err = parseConcatFile(r)
			if p.Path != "" {
				if paths[p.Path] {
					return 0, false
				}
				paths[p.Path] = true
			}
		case "concat_fragment":
			_, err = parseConcatFragment(r)
		}
		if errors.As(err, &uv) {
			return 0, false
		}
	}
	return v, true
}
