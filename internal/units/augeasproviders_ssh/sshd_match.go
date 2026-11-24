package augeasprovidersssh

import (
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
)

// sshdMatchNames lists, in catalog order, every name a Sshd_config_match of
// cat is known by (none for a nil cat): its title, and each string of its alias metaparameter,
// which Puppet's catalog adds as an alias (catalog.rb:165-170). Its uniqueness
// key, [name, condition, target], is no single-key alias (host.rb aliases_of).
// A non-string alias never names a condition: a number or boolean holds no
// space, and a hash's text holds quotes, which no condition value does.
func sshdMatchNames(cat *catalog.Catalog) []string {
	if cat == nil {
		return nil
	}
	var out []string
	var aliases func(v any)
	aliases = func(v any) {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case []any:
			for _, e := range v {
				aliases(e)
			}
		}
	}
	for _, r := range cat.Resources {
		if strings.EqualFold(r.Type, "sshd_config_match") {
			out = append(out, r.Title)
			aliases(r.Parameters["alias"])
		}
	}
	return out
}
