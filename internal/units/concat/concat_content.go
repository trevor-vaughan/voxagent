package concat

import (
	"cmp"
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strings"
)

// concatOrderIncomparable is the message of the ArgumentError Ruby's sort
// raises when two numeric order keys meet an Integer and a String.
const concatOrderIncomparable = "comparison of Array with Array failed"

// concatDigitLine is Ruby's /^\d+$/ on a String: some line is all ASCII digits.
var concatDigitLine = regexp.MustCompile(`(?m)^[0-9]+$`)

// rubyToI is Ruby's String#to_i: skip leading whitespace, take an optional
// sign, then digits where a single "_" may separate two digits. No digits is 0.
func rubyToI(s string) *big.Int {
	i := 0
	for i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0 {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var digits []byte
	for ; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			digits = append(digits, c)
			continue
		}
		if c == '_' && len(digits) > 0 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
			continue
		}
		break
	}
	n, ok := new(big.Int).SetString(string(digits), 10)
	if !ok {
		return new(big.Int)
	}
	if neg {
		n.Neg(n)
	}
	return n
}

// concatKeyPart is one element of a decompounded numeric order key: an Integer
// when num is set, else the String str.
type concatKeyPart struct {
	num *big.Int
	str string
}

func (p concatKeyPart) isInt() bool { return p.num != nil }

// concatDecompound is concat_file.rb's decompound: split the key at the first
// "___" and turn each part holding an all-digit line into its to_i.
func concatDecompound(key string) [2]concatKeyPart {
	var out [2]concatKeyPart
	for i, part := range strings.SplitN(key, "___", 2) {
		if concatDigitLine.MatchString(part) {
			out[i] = concatKeyPart{num: rubyToI(part)}
		} else {
			out[i] = concatKeyPart{str: part}
		}
	}
	return out
}

// concatPartCompare orders two comparable parts; callers have already
// rejected an Integer/String pair.
func concatPartCompare(a, b concatKeyPart) int {
	if a.isInt() {
		return a.num.Cmp(b.num)
	}
	return cmp.Compare(a.str, b.str)
}

// concatPartID names a part's type and value, so equal parts share an ID.
func concatPartID(p concatKeyPart) string {
	if p.isInt() {
		return "i" + p.num.String()
	}
	return "s" + p.str
}

// concatNumericCheck fails when any two keys meet an Integer and a String at
// their first unequal element. No third key can order such a pair, so Ruby's
// sort must compare it and raises whatever the input order.
func concatNumericCheck(keys [][2]concatKeyPart) error {
	var firstInt, firstStr bool
	// secondKinds maps a shared first element to the kinds its second elements take.
	secondKinds := map[string]struct{ i, s bool }{}
	for _, k := range keys {
		firstInt = firstInt || k[0].isInt()
		firstStr = firstStr || !k[0].isInt()
		id := concatPartID(k[0])
		kinds := secondKinds[id]
		kinds.i = kinds.i || k[1].isInt()
		kinds.s = kinds.s || !k[1].isInt()
		secondKinds[id] = kinds
	}
	if firstInt && firstStr {
		return errors.New(concatOrderIncomparable)
	}
	for _, kinds := range secondKinds {
		if kinds.i && kinds.s {
			return errors.New(concatOrderIncomparable)
		}
	}
	return nil
}

// concatSort orders fragments as concat_file.rb's should_content does ("numeric"
// or "alpha"); equal keys keep their input order. It returns a new slice.
func concatSort(frags []concatFragmentParams, order string) ([]concatFragmentParams, error) {
	sorted := slices.Clone(frags)
	keyOf := func(f concatFragmentParams) string { return f.Order + "___" + f.Name }

	if order != "numeric" {
		alphaKey := func(f concatFragmentParams) (string, string) {
			a, b, _ := strings.Cut(keyOf(f), "__")
			return a, b
		}
		slices.SortStableFunc(sorted, func(x, y concatFragmentParams) int {
			xa, xb := alphaKey(x)
			ya, yb := alphaKey(y)
			return cmp.Or(cmp.Compare(xa, ya), cmp.Compare(xb, yb))
		})
		return sorted, nil
	}

	keys := make(map[string][2]concatKeyPart, len(frags))
	list := make([][2]concatKeyPart, 0, len(frags))
	for _, f := range frags {
		k := concatDecompound(keyOf(f))
		keys[keyOf(f)] = k
		list = append(list, k)
	}
	if err := concatNumericCheck(list); err != nil {
		return nil, err
	}
	slices.SortStableFunc(sorted, func(x, y concatFragmentParams) int {
		kx, ky := keys[keyOf(x)], keys[keyOf(y)]
		if c := concatPartCompare(kx[0], ky[0]); c != 0 {
			return c
		}
		return concatPartCompare(kx[1], ky[1])
	})
	return sorted, nil
}

// concatContent joins the fragments' content. With ensureNewline, content not
// ending in "\n" gets one (Ruby's /\n\Z/ matches exactly when the string ends
// in "\n"), so an empty fragment becomes a blank line.
func concatContent(sorted []concatFragmentParams, ensureNewline bool) string {
	var b strings.Builder
	for _, f := range sorted {
		b.WriteString(f.Content)
		if ensureNewline && !strings.HasSuffix(f.Content, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
