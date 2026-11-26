package inifile

import (
	"errors"
	"strings"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/unit"
	"github.com/voxagent/voxagent/internal/units/group"
)

// Unit is the inifile unit as units.All lists it.
var Unit unit.Unit = unitImpl{}

type unitImpl struct{}

// Manifest implements unit.Unit.
func (unitImpl) Manifest() unit.Manifest { return Manifest }

// regexpSyntax is what a key_val_separator may not contain to stay native
// (spec 2, ruling R6). IniFile interpolates the separator into its regexps
// and into a character class without escaping it, as Ruby does, so RE2 and
// Ruby's Onigmo must read it alike. They do for a literal, and the corpus pins
// "." (a wildcard in both); beyond that they differ (escapes, classes,
// groups, `-` and `&` inside a class), so those go to the host.
const regexpSyntax = `\+*?()|[]{}^$-&`

// typeRule is what Route needs of one type beyond the provider.
type typeRule struct {
	provider ral.Provider
	// parse is the type's munge; its result is handed to hostOnly.
	parse func(catalog.Resource) (any, error)
	// lineBreakAttrs are the Strings the regexes or the section header see
	// (spec 2 condition 4), in the order they are tested.
	lineBreakAttrs []string
	// nameAttr is the attribute that defaults to the resource's name, "" if none.
	nameAttr string
	// hostOnly is the reason a parsed resource must run on the host although
	// it is within the measured shapes, "" if native.
	hostOnly func(r catalog.Resource, parsed any) string
}

// rules is the per-type routing data, keyed by type name.
var rules = map[string]typeRule{
	"ini_setting": {
		provider:       IniSetting{},
		parse:          func(r catalog.Resource) (any, error) { return parseSetting(r) },
		lineBreakAttrs: []string{"section", "setting", "value", "key_val_separator", "section_prefix", "section_suffix"},
		hostOnly: func(r catalog.Resource, parsed any) string {
			p := parsed.(settingParams)
			if why := emptyEnsure(r, p.Ensure); why != "" {
				return why
			}
			return keyValSeparatorSyntax(r, p.KeyValSeparator)
		},
	},
	"ini_subsetting": {
		provider: IniSubsetting{},
		parse:    func(r catalog.Resource) (any, error) { return parseSubsetting(r) },
		lineBreakAttrs: []string{"section", "setting", "subsetting", "value", "key_val_separator", "subsetting_separator",
			"subsetting_key_val_separator", "insert_value"},
		hostOnly: func(r catalog.Resource, parsed any) string {
			p := parsed.(subsettingParams)
			if why := emptyEnsure(r, p.Ensure); why != "" {
				return why
			}
			// Ruling R15: the type gives setting and subsetting no default, so
			// Ruby holds nil, which the parsed strings cannot tell from "".
			for _, attr := range []string{"setting", "subsetting"} {
				if rubyvalue.Param(r, attr) == nil {
					return r.Ref() + ": " + attr + " is unset (Ruby holds nil, unmeasured)"
				}
			}
			if why := keyValSeparatorSyntax(r, p.KeyValSeparator); why != "" {
				return why
			}
			// Ruling R8: the predicates the SettingValue port applies. A separator the
			// port refuses is routed away even for a resource whose setting is absent
			// and so never builds a SettingValue; that costs a host run, not a
			// divergence.
			if !supportedSeparator(p.SubsettingSeparator) {
				return r.Ref() + ": subsetting_separator is not reproduced natively (Ruby regexp semantics)"
			}
			if p.InsertType == "index" && p.InsertValue != nil && !supportedInsertIndex(*p.InsertValue) {
				return r.Ref() + ": insert_value is not reproduced natively (Ruby bignum index)"
			}
			return ""
		},
	},
	"ini_section": {
		provider:       IniSection{},
		parse:          func(r catalog.Resource) (any, error) { return parseSection(r) },
		lineBreakAttrs: []string{"section", "section_prefix", "section_suffix"},
		nameAttr:       "section",
		hostOnly: func(r catalog.Resource, parsed any) string {
			return emptyEnsure(r, parsed.(sectionParams).Ensure)
		},
	},
}

// emptyEnsure is the reason a resource whose ensure parsed to "" (`ensure =>
// []`) runs on the host (ruling R15): Ruby then has no ensure value yet still
// evaluates the resource, which is unmeasured. "" if ensure has a value.
func emptyEnsure(r catalog.Resource, ensure string) string {
	if ensure == "" {
		return r.Ref() + ": ensure is an empty array (Ruby manages no ensure value, unmeasured)"
	}
	return ""
}

// keyValSeparatorSyntax is the reason a resource's key_val_separator, as
// IniFile uses it, has regexp syntax (ruling R6); "" if it has none.
func keyValSeparatorSyntax(r catalog.Resource, keyValSeparator string) string {
	if strings.ContainsAny(regexpKeyValSeparator(keyValSeparator), regexpSyntax) {
		return r.Ref() + ": key_val_separator has regexp syntax (Ruby and RE2 read it differently, unmeasured)"
	}
	return ""
}

// Route implements unit.Unit (spec §2). Each type the catalog holds is routed
// on its own; a catalog with none of the three gets an empty Decision.
func (unitImpl) Route(env *unit.Env) unit.Decision {
	held := map[string][]catalog.Resource{}
	for _, r := range env.Catalog.Resources {
		if typ := strings.ToLower(r.Type); rules[typ].provider != nil {
			held[typ] = append(held[typ], r)
		}
	}
	d := unit.Decision{}
	for _, typ := range Manifest.Types {
		if len(held[typ]) == 0 {
			continue
		}
		if why := routeType(env, typ, held[typ]); why != "" {
			if d.Delegated == nil {
				d.Delegated = map[string]string{}
			}
			d.Delegated[typ] = why
			continue
		}
		d.Providers = append(d.Providers, rules[typ].provider)
	}
	return d
}

// routeType is the reason typ's resources run on the host, "" for native: the
// first of spec §2's four conditions that holds, then the one the rulings
// added. Each condition is tried on every resource before the next.
func routeType(env *unit.Env, typ string, rs []catalog.Resource) string {
	rule := rules[typ]
	if _, why, ok := unit.MatchFiles(env.Tree, Manifest, typ, nil); !ok {
		return why
	}
	dir := "puppet/provider/" + typ
	names, err := env.Tree.List(dir)
	if err != nil {
		return dir + ": " + err.Error()
	}
	for _, n := range names {
		if strings.HasSuffix(n, ".rb") && n != "ruby.rb" {
			return "another " + typ + " provider in libdir"
		}
	}
	// Ruling R11: Puppet refuses a provider the type does not have; native
	// would apply, so a resource naming one goes to the host.
	for _, r := range rs {
		if why := group.ForeignProvider(r, "ruby"); why != "" {
			return why
		}
	}
	// A parse error that is not an UnimplementedValue is Ruby's own refusal,
	// which the native provider reports as Ruby does, so the resource stays
	// native; it has no parsed params, so R6 and R8 skip it, and only the
	// line-break test (on its raw Strings) still applies.
	parsed := make([]any, len(rs))
	var uv *rubyvalue.UnimplementedValue
	for i, r := range rs {
		p, err := rule.parse(r)
		switch {
		case err == nil:
			parsed[i] = p
		case errors.As(err, &uv):
			return err.Error()
		}
	}
	for _, r := range rs {
		if why := lineBreak(r, rule); why != "" {
			return why
		}
	}
	for i, r := range rs {
		if parsed[i] != nil && rule.hostOnly != nil {
			if why := rule.hostOnly(r, parsed[i]); why != "" {
				return why
			}
		}
	}
	return ""
}

// lineBreak is the reason r has a line break in a String the regexes or the
// section header see (spec §2 condition 4), "" if it has none. Ruby's ^ and $
// are line anchors, which the corpora do not measure. The reason names the
// attribute and never its value, which may be Sensitive.
func lineBreak(r catalog.Resource, rule typeRule) string {
	for _, attr := range rule.lineBreakAttrs {
		v := rubyvalue.Param(r, attr)
		if v == nil && attr == rule.nameAttr {
			if v = rubyvalue.Param(r, "name"); v == nil {
				v = r.Title
			}
		}
		elems, ok := v.([]any)
		if !ok {
			elems = []any{v}
		}
		for _, e := range elems {
			if s, ok := e.(string); ok && strings.Contains(s, "\n") {
				return r.Ref() + ": " + attr + " has a line break (Ruby's ^ and $ are line anchors, unmeasured)"
			}
		}
	}
	return ""
}
