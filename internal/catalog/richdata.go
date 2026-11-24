package catalog

import (
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RichDataError is Puppet's refusal of a whole catalog whose rich data it
// cannot deserialise.
type RichDataError struct{ Detail string }

func (e *RichDataError) Error() string {
	return "Could not deserialize catalog from rich_data_json: Could not intern from rich_data_json: " + e.Detail
}

// RichValue names one rich value left in a resource's parameters after
// Decode: any {"__ptype": ...} map, the top-level kind only.
type RichValue struct {
	Param string
	PType string // the __ptype name; "Object" for a type given as a hash
}

// knownPTypes are the __ptype names voxagent accepts, measured. Ruby
// resolves any name its loaders know, module-defined Object types included
// (decision D-52); a name beginning "Pcore::" is accepted as well.
var knownPTypes = map[string]struct{}{
	"Binary": {}, "Sensitive": {}, "Default": {}, "Hash": {}, "Symbol": {}, "LocalRef": {},
	"Timestamp": {}, "Timespan": {}, "SemVer": {}, "SemVerRange": {}, "Regexp": {}, "URI": {},
	"Error": {}, "Deferred": {},
}

// richPType reports whether m is a rich value and, if so, its kind. A
// __ptype of a JSON type other than a string or a map leaves m a plain hash
// (from_data_converter.rb:159-163).
func richPType(m map[string]any) (string, bool) {
	switch t := m["__ptype"].(type) {
	case string:
		return t, true
	case map[string]any:
		return "Object", true
	default:
		return "", false
	}
}

// ResolveRich returns v with every ASCII-only Binary, at any depth, replaced
// by its string. Other rich values are kept as their maps. It is an error
// for invalid base64 or an unknown __ptype name, anywhere in v.
//
// Recursion is bounded by encoding/json's nesting limit, which every caller's
// input has already passed.
func ResolveRich(v any) (any, error) {
	switch v := v.(type) {
	case []any:
		if v == nil {
			return v, nil
		}
		out := make([]any, len(v))
		for i, e := range v {
			r, err := ResolveRich(e)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		if v == nil {
			return v, nil
		}
		if name, named := v["__ptype"].(string); named {
			_, known := knownPTypes[name]
			if !known && !strings.HasPrefix(name, "Pcore::") {
				return nil, &RichDataError{Detail: "No implementation mapping found for Puppet Type " + name}
			}
			if name == "Binary" {
				return resolveBinary(v)
			}
		}
		// A rich map other than Binary is walked too, so invalid data
		// anywhere in it fails the catalog as Ruby's does.
		out := make(map[string]any, len(v))
		for k, e := range v {
			r, err := ResolveRich(e)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// resolveBinary decodes a Binary as Base64.strict_decode64 does
// (p_binary_type.rb:181-193). An ASCII-only Binary is observably a plain
// String in every channel measured, so it becomes one; any other is
// kept as its map, for pre-flight to refuse on a native resource.
//
// The two "Cannot create a Binary" messages are read, not measured
// (from_data_converter.rb:212-226, decision D-52).
func resolveBinary(m map[string]any) (any, error) {
	s, ok := m["__pvalue"].(string)
	if !ok {
		return nil, &RichDataError{Detail: "Cannot create a Binary from a " + RubyClassName(m["__pvalue"])}
	}
	// Go's strict decoder still skips CR and LF, which strict_decode64
	// refuses.
	if strings.ContainsAny(s, "\r\n") {
		return nil, &RichDataError{Detail: "invalid base64"}
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, &RichDataError{Detail: "invalid base64"}
	}
	for _, c := range b {
		if c >= 0x80 {
			return m, nil
		}
	}
	return string(b), nil
}

// resolveRichData runs ResolveRich over every resource's parameters,
// containers included: Ruby deserialises the whole catalog before it reads any
// of it, so invalid rich data anywhere fails the run.
func resolveRichData(c *Catalog) error {
	for i := range c.Resources {
		params := c.Resources[i].Parameters
		for name, v := range params {
			r, err := ResolveRich(v)
			if err != nil {
				return err
			}
			params[name] = r
		}
	}
	return nil
}

// RichValues lists r's remaining rich values, sorted by Param then PType.
// Arrays and plain maps are searched; a rich map is not, so a Sensitive
// wrapping a Binary is reported as Sensitive. A kind repeated within one
// parameter is listed once.
func RichValues(r Resource) []RichValue {
	var out []RichValue
	for name, v := range r.Parameters {
		seen := map[string]struct{}{}
		collectRich(v, seen)
		for ptype := range seen {
			out = append(out, RichValue{Param: name, PType: ptype})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Param != out[j].Param {
			return out[i].Param < out[j].Param
		}
		return out[i].PType < out[j].PType
	})
	return out
}

func collectRich(v any, seen map[string]struct{}) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			collectRich(e, seen)
		}
	case map[string]any:
		if ptype, rich := richPType(v); rich {
			seen[ptype] = struct{}{}
			return
		}
		for _, e := range v {
			collectRich(e, seen)
		}
	}
}

// RubyClassName is a decoded catalog value's Ruby class name, for Puppet's
// messages that name one: Integer (integral float64), Float, String,
// TrueClass, FalseClass, Array, Hash, NilClass.
func RubyClassName(v any) string {
	switch v := v.(type) {
	case float64:
		if v == math.Trunc(v) {
			return "Integer"
		}
		return "Float"
	case string:
		return "String"
	case bool:
		if v {
			return "TrueClass"
		}
		return "FalseClass"
	case []any:
		return "Array"
	case map[string]any:
		return "Hash"
	case nil:
		return "NilClass"
	default:
		return fmt.Sprintf("%T", v)
	}
}
