package parsedfile

import (
	"regexp"
	"unicode/utf8"
)

// Split is Ruby's String#split(regexp) without a limit: a leading empty field
// is kept, trailing empty fields are dropped, an empty string gives none, and
// a capture group's text is inserted between the fields. A pattern that can
// match the empty string splits between characters, as in Ruby. It follows
// rb_str_split_m's regexp branch (Ruby 3.4).
//
// Each search runs on the text after the previous match, so a pattern must
// not depend on what precedes it: no ^ (it would match after every earlier
// match) and no \b. The patterns this package's grammars use never do.
func Split(s string, sep *regexp.Regexp) []string {
	result := []string{}
	beg, start := 0, 0
	lastNull := false
	for start <= len(s) {
		loc := sep.FindStringSubmatchIndex(s[start:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += start
			}
		}
		if start == loc[0] && loc[0] == loc[1] {
			if len(s) == 0 {
				result = append(result, "")
				break
			}
			if !lastNull {
				if start == len(s) {
					start++
				} else {
					start += runeLen(s, start)
				}
				lastNull = true
				continue
			}
			result = append(result, s[beg:beg+runeLen(s, beg)])
			beg = start
		} else {
			result = append(result, s[beg:loc[0]])
			beg = loc[1]
			start = beg
		}
		lastNull = false
		for i := 2; i < len(loc); i += 2 {
			if loc[i] >= 0 {
				result = append(result, s[loc[i]:loc[i+1]])
			}
		}
	}
	if len(s) > beg {
		result = append(result, s[beg:])
	}
	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}
	return result
}

// runeLen is the byte length of the character at s[i], 1 for an invalid byte
// (Ruby's rb_enc_fast_mbclen on a broken string).
func runeLen(s string, i int) int {
	_, n := utf8.DecodeRuneInString(s[i:])
	return n
}
