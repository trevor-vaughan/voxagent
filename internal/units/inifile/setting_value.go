package inifile

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

// sv: puppetlabs-inifile lib/puppet/util/setting_value.rb (identical in 6.5.0,
// 6.6.0 and 6.6.1).

// errUnsupportedSettingValue marks input whose Ruby behaviour the port does
// not reproduce: Ruby builds a regexp from subsetting_separator, and a
// separator that is a regexp escape or class syntax (or not valid UTF-8), or
// an insert_value index at or below -2^63 (conservatively), would need Ruby's regexp or bignum
// semantics. The caller routes such a resource to Ruby.
var errUnsupportedSettingValue = errors.New("not reproduced natively: ruby regexp or bignum semantics")

// SettingValue is Puppet::Util::SettingValue: the subsettings of one setting's
// value, split by a separator and edited by name.
type SettingValue struct {
	items     []string
	separator string
	quoteChar string
	kvSep     string // subsetting_key_val_separator
}

// newSettingValue is SettingValue#initialize (sv:12-31); a nil value is Ruby's
// nil: no items.
func newSettingValue(value *string, subsettingSeparator, quoteChar, keyValSeparator string) (*SettingValue, error) {
	v := &SettingValue{separator: subsettingSeparator, quoteChar: quoteChar, kvSep: keyValSeparator}
	if value == nil {
		return v, nil
	}
	// sv:24 builds `(?:(?:[^\<sep>]|\.)+)`: the separator's first character
	// escaped, the rest raw, in a negated class.
	if subsettingSeparator == "" {
		return nil, &RubyError{Class: "RegexpError", Message: `premature end of char-class: /(?:(?:[^\]|\.)+)/`}
	}
	if !supportedSeparator(subsettingSeparator) {
		return nil, fmt.Errorf("subsetting_separator %q: %w", subsettingSeparator, errUnsupportedSettingValue)
	}
	unquoted, quote, err := unquoteSettingValue(*value)
	if err != nil {
		return nil, err
	}
	if quote != "" {
		v.quoteChar = quote
	}
	// An item is a run of characters that are not the separator's, or dots
	// (`\.`): a dot in the separator does not separate.
	// Kept from Ruby (sv:24): `\.` makes a "." in subsetting_separator a
	// literal item character, so it never splits.
	for _, item := range strings.FieldsFunc(unquoted, func(r rune) bool {
		return r != '.' && strings.ContainsRune(subsettingSeparator, r)
	}) {
		v.items = append(v.items, rubyvalue.Strip(item))
	}
	return v, nil
}

// supportedSeparator reports whether the sv:24 regexp is the plain negated
// class the port implements: valid UTF-8, a first character that stays literal
// after its backslash (not a letter, digit or `_`, which would escape to a
// class such as \d or \s), and no later character that is class syntax.
func supportedSeparator(sep string) bool {
	if !utf8.ValidString(sep) {
		return false
	}
	first, size := utf8.DecodeRuneInString(sep)
	if first < utf8.RuneSelf && (unicode.IsLetter(first) || unicode.IsDigit(first) || first == '_') {
		return false
	}
	return !strings.ContainsAny(sep[size:], `\[]^-&`)
}

// supportedInsertIndex reports whether insert_value.to_i (sv:111) is an index
// the port reproduces. String#to_i saturates at math.MinInt64 where Ruby
// raises RangeError for a bignum; a positive saturation is clamped to the
// length either way. Route asks the same question (spec 2, ruling R8).
func supportedInsertIndex(insertValue string) bool {
	return rubyvalue.ToI(insertValue) != math.MinInt64
}

// unquoteSettingValue is sv:33-48. Kept from Ruby (sv:35-44): a value that is
// the single character `"` or `'` both starts and ends with the quote, so
// `value[1, -1]` is nil and the constructor raises NoMethodError.
func unquoteSettingValue(value string) (unquoted, quote string, err error) {
	for _, q := range []string{`"`, `'`} {
		if strings.HasPrefix(value, q) && strings.HasSuffix(value, q) {
			if len(value) < 2 {
				return "", "", noMethodOnNil("scan")
			}
			return value[1 : len(value)-1], q, nil
		}
	}
	return value, "", nil
}

// GetValue is get_value (sv:53-56): the items joined, in the quote character.
func (v *SettingValue) GetValue() string {
	return v.quoteChar + strings.Join(v.items, v.separator) + v.quoteChar
}

// GetSubsettingValue is get_subsetting_value (sv:64-73): nil when the item is
// not found; the item's text after the name and key/value separator; for an
// exact match Ruby returns `true`, which here is a non-nil pointer (to ""), as
// the one caller with an exact match (exists?) only tests it for truth.
func (v *SettingValue) GetSubsettingValue(subsetting string, exact bool) *string {
	i := v.find(subsetting, exact)
	if i < 0 {
		return nil
	}
	if exact {
		empty := ""
		return &empty
	}
	rest := v.items[i][len(subsetting)+len(v.kvSep):]
	return &rest
}

// AddSubsetting is add_subsetting (sv:84-120); insertType is the Symbol's name.
// A nil value is Ruby's nil (no value after the name).
func (v *SettingValue) AddSubsetting(subsetting string, value *string, exact bool, insertType string, insertValue *string) error {
	if v.find(subsetting, exact) >= 0 {
		v.update(subsetting, value, exact)
		return nil
	}
	item := v.itemValue(subsetting, value)
	switch insertType {
	case "start":
		v.insertAt(0, item)
	case "before", "after":
		at, err := v.findInsertValue(insertValue, exact)
		if err != nil {
			return err
		}
		switch {
		case at < 0:
			v.items = append(v.items, item)
		case insertType == "before":
			v.insertAt(at, item)
		default:
			v.insertAt(at+1, item)
		}
	case "index":
		return v.insertIndex(item, insertValue)
	default:
		// "end", and Kept from Ruby (sv:115-116): any other insert_type
		// appends too.
		v.items = append(v.items, item)
	}
	return nil
}

// findInsertValue is find_subsetting(insert_value, exact) of sv:98 and sv:105.
// Kept from Ruby (sv:161): a nil insert_value without an exact match
// raises NoMethodError (`nil + key_val_separator`), but only when there is an
// item to test; with an exact match nil equals no item.
func (v *SettingValue) findInsertValue(insertValue *string, exact bool) (int, error) {
	if insertValue != nil {
		return v.find(*insertValue, exact), nil
	}
	if !exact && len(v.items) > 0 {
		return -1, noMethodOnNil("+")
	}
	return -1, nil
}

// insertIndex is sv:111-114: insert_value.to_i, clamped to the length, and
// Array#insert, where a negative index counts from the end.
func (v *SettingValue) insertIndex(item string, insertValue *string) error {
	var n int64
	if insertValue != nil {
		if !supportedInsertIndex(*insertValue) {
			return fmt.Errorf("insert_value %q: %w", *insertValue, errUnsupportedSettingValue)
		}
		n = rubyvalue.ToI(*insertValue)
	}
	length := int64(len(v.items))
	switch {
	case n > length:
		n = length
	case n < 0:
		if n < -(length + 1) {
			return &RubyError{Class: "IndexError", Message: fmt.Sprintf("index %d too small for array; minimum: -%d", n, length+1)}
		}
		n += length + 1
	}
	v.insertAt(int(n), item)
	return nil
}

func (v *SettingValue) insertAt(i int, item string) {
	v.items = append(v.items, "")
	copy(v.items[i+1:], v.items[i:])
	v.items[i] = item
}

// update is update_subsetting (sv:127-136): every matching item is replaced.
func (v *SettingValue) update(subsetting string, value *string, exact bool) {
	item := v.itemValue(subsetting, value)
	for i, existing := range v.items {
		if v.matches(existing, subsetting, exact) {
			v.items[i] = item
		}
	}
}

// RemoveSubsetting is remove_subsetting (sv:170-174): every matching item goes.
func (v *SettingValue) RemoveSubsetting(subsetting string, exact bool) {
	kept := v.items[:0]
	for _, item := range v.items {
		if !v.matches(item, subsetting, exact) {
			kept = append(kept, item)
		}
	}
	v.items = kept
}

// find is find_subsetting (sv:144-148): the first matching item's index, or -1.
func (v *SettingValue) find(subsetting string, exact bool) int {
	for i, item := range v.items {
		if v.matches(item, subsetting, exact) {
			return i
		}
	}
	return -1
}

// matches is match_subsetting? (sv:157-163).
func (v *SettingValue) matches(item, subsetting string, exact bool) bool {
	if exact {
		return item == subsetting
	}
	return strings.HasPrefix(item, subsetting+v.kvSep)
}

// itemValue is item_value (sv:182-184).
func (v *SettingValue) itemValue(subsetting string, value *string) string {
	item := subsetting + v.kvSep
	if value != nil {
		item += *value
	}
	return item
}
