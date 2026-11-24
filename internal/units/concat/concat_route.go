package concat

import (
	"errors"
	"fmt"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
)

// concatTypeFiles are the plugin-tree files that identify a concat release.
var concatTypeFiles = []string{"puppet/type/concat_file.rb", "puppet/type/concat_fragment.rb"}

// releaseVersions maps a Manifest release to the ConcatVersion whose behaviour
// the unit reproduces.
var releaseVersions = map[string]ConcatVersion{"9.1.0": Concat910, "10.0.1": Concat1001}

// nativeConcat says whether concat_file and concat_fragment are applied
// natively for cat, and as which release; when not it says why, for Route's
// Delegated map.
//
// Both type files in t must match the digests of the same release of m. A
// missing, unreadable or oversized file, an unknown digest or a mixed pair
// delegates. So does a Concat_file or Concat_fragment with a shape the
// native parser does not implement (rubyvalue.UnimplementedValue), and two
// Concat_files sharing a path, which Ruby generates one File for. Ruby's own
// refusals stay native.
func nativeConcat(t ral.PluginTree, cat *catalog.Catalog, m unit.Manifest) (ConcatVersion, string, bool) {
	rel, reason, ok := unit.MatchFiles(t, m, "concat_file", nil)
	if !ok {
		return 0, reason, false
	}
	v := releaseVersions[rel.Version]
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
					return 0, fmt.Sprintf("two concat_file resources share the path %s", p.Path), false
				}
				paths[p.Path] = true
			}
		case "concat_fragment":
			_, err = parseConcatFragment(r)
		}
		if errors.As(err, &uv) {
			return 0, fmt.Sprintf("%s has a shape the native parser does not implement: %v", r.Ref(), err), false
		}
	}
	return v, "", true
}
