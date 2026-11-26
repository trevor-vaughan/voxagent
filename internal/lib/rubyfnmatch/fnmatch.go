// Package rubyfnmatch ports Ruby's File.fnmatch for the one flag set tidy's
// `matches` parameter uses, so patterns select the same files they do under
// openvox.
package rubyfnmatch

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// errInvalidBytes is the ArgumentError Ruby raises from rb_enc_codepoint
// (encoding.c) when it must decode an invalid UTF-8 sequence.
var errInvalidBytes = errors.New("invalid byte sequence in UTF-8")

// The ArgumentErrors for an embedded NUL: StringValueCStr on the pattern and
// FilePathStringValue on the name (dir.c file_s_fnmatch, 3202-3203), in that
// order.
var (
	errPatternNull = errors.New("string contains null byte")
	errNameNull    = errors.New("path name contains null byte")
)

// Match is File.fnmatch(pattern, name, File::FNM_DOTMATCH | File::FNM_PATHNAME)
// in Ruby 3.2 (dir.c). It is a port of dir.c's fnmatch (411-460),
// fnmatch_helper (319-408) and bracket (241-309), with the flags fixed:
// DOTMATCH set (so the leading-period rule is off), PATHNAME set, NOESCAPE and
// CASEFOLD clear. Both strings are treated as UTF-8, as Puppet's are.
//
// Ruby's C code walks NUL-terminated strings; Go strings can hold NUL, but Ruby
// rejects those before matching, and Match does too. at reproduces reading the
// terminator.
//
// The error is non-nil exactly when Ruby raises. Ruby raises on invalid UTF-8
// only where the algorithm decodes a code point (a bracket range, or a name
// character tested against a class); elsewhere an invalid byte is an ordinary
// non-match, and Match reproduces that.
func Match(pattern, name string) (bool, error) {
	if strings.IndexByte(pattern, 0) >= 0 {
		return false, errPatternNull
	}
	if strings.IndexByte(name, 0) >= 0 {
		return false, errNameNull
	}
	p, s := 0, 0
	ptmp, stmp := -1, -1
	for {
		// dir.c 428-434: collapse each leading `**/` and remember the restart point.
		if at(pattern, p) == '*' && at(pattern, p+1) == '*' && at(pattern, p+2) == '/' {
			for at(pattern, p) == '*' && at(pattern, p+1) == '*' && at(pattern, p+2) == '/' {
				p += 3
			}
			ptmp, stmp = p, s
		}
		np, ns, ok, err := helper(pattern, name, p, s)
		if err != nil {
			return false, err
		}
		if ok {
			p, s = np, ns
			// dir.c 434-441: helper stopped at the end of a path element.
			for at(name, s) != 0 && at(name, s) != '/' {
				s += mbclen(name, s)
			}
			if at(pattern, p) != 0 && at(name, s) != 0 {
				p++
				s++
				continue
			}
			if at(pattern, p) == 0 && at(name, s) == 0 {
				return true, nil
			}
		}
		// dir.c 443-452: failed; retry `**/` one directory further along.
		if ptmp >= 0 && stmp >= 0 {
			for at(name, stmp) != 0 && at(name, stmp) != '/' {
				stmp += mbclen(name, stmp)
			}
			if at(name, stmp) != 0 {
				p = ptmp
				stmp++
				s = stmp
				continue
			}
		}
		return false, nil
	}
}

// at is *(str+i): the byte, or 0 at the terminator.
func at(str string, i int) byte {
	if i >= len(str) {
		return 0
	}
	return str[i]
}

// unescape is UNESCAPE (dir.c 316) with escapes on.
func unescape(p string, i int) int {
	if at(p, i) == '\\' {
		return i + 1
	}
	return i
}

// isEnd is ISEND (dir.c 317) with PATHNAME on.
func isEnd(str string, i int) bool {
	c := at(str, i)
	return c == 0 || c == '/'
}

// helper is fnmatch_helper (dir.c 319-408): it matches one path element and, on
// success, returns where it stopped in each string.
func helper(pattern, name string, p, s int) (int, int, bool, error) {
	ptmp, stmp := -1, -1
	for {
		switch at(pattern, p) {
		case '*':
			for p++; at(pattern, p) == '*'; p++ {
			}
			if isEnd(pattern, unescape(pattern, p)) {
				return unescape(pattern, p), s, true, nil
			}
			if isEnd(name, s) {
				return p, s, false, nil
			}
			ptmp, stmp = p, s
			continue
		case '?':
			if isEnd(name, s) {
				return p, s, false, nil
			}
			p++
			s += mbclen(name, s)
			continue
		case '[':
			if isEnd(name, s) {
				return p, s, false, nil
			}
			t, ok, err := bracket(pattern, name, p+1, s)
			if err != nil {
				return 0, 0, false, err
			}
			if ok {
				p = t
				s += mbclen(name, s)
				continue
			}
			goto failed
		}

		// ordinary character (dir.c 366-390)
		p = unescape(pattern, p)
		if isEnd(name, s) {
			return p, s, isEnd(pattern, p), nil
		}
		if isEnd(pattern, p) {
			goto failed
		}
		if r := preciseLen(pattern, p); r > 0 && strings.HasPrefix(name[s:], pattern[p:p+r]) {
			p += r
			s += r
			continue
		}

	failed: // dir.c 392-407: try the next `*` position
		if ptmp >= 0 && stmp >= 0 {
			p = ptmp
			stmp += mbclen(name, stmp)
			s = stmp
			continue
		}
		return p, s, false, nil
	}
}

// bracket is dir.c's bracket (241-309): p indexes the byte after `[`. It returns
// the index after the closing `]` and whether the character at name[s] is in
// the class.
func bracket(pattern, name string, p, s int) (int, bool, error) {
	ok, not := false, false
	if p >= len(pattern) {
		return 0, false, nil
	}
	if c := pattern[p]; c == '!' || c == '^' {
		not = true
		p++
	}
	for at(pattern, p) != ']' {
		t1 := unescape(pattern, p)
		if at(pattern, t1) == 0 {
			return 0, false, nil
		}
		r := mbclen(pattern, t1)
		p = t1 + r
		if p >= len(pattern) {
			return 0, false, nil
		}
		if at(pattern, p) == '-' && at(pattern, p+1) != ']' {
			t2 := unescape(pattern, p+1)
			if at(pattern, t2) == 0 {
				return 0, false, nil
			}
			r2 := mbclen(pattern, t2)
			p = t2 + r2
			if ok {
				continue
			}
			if strings.HasPrefix(name[s:], pattern[t1:t1+r]) || strings.HasPrefix(name[s:], pattern[t2:t2+r2]) {
				ok = true
				continue
			}
			c1, err := codepoint(name, s)
			if err != nil {
				return 0, false, err
			}
			c2, err := codepoint(pattern, t1)
			if err != nil {
				return 0, false, err
			}
			if c1 < c2 {
				continue
			}
			if c2, err = codepoint(pattern, t2); err != nil {
				return 0, false, err
			}
			if c1 > c2 {
				continue
			}
		} else {
			if ok {
				continue
			}
			if strings.HasPrefix(name[s:], pattern[t1:t1+r]) {
				ok = true
			}
			continue
		}
		ok = true
	}
	if ok == not {
		return 0, false, nil
	}
	return p + 1, true, nil
}

// mbclen is rb_enc_mbclen for UTF-8: the length of the character at str[i], or
// one byte for any invalid or truncated sequence (corpus-checked: Ruby does not
// swallow the remainder of a truncated sequence).
func mbclen(str string, i int) int {
	if n := preciseLen(str, i); n > 0 {
		return n
	}
	return 1
}

// preciseLen is the CHARFOUND case of rb_enc_precise_mbclen: the length of a
// valid character at str[i], else 0.
func preciseLen(str string, i int) int {
	r, n := utf8.DecodeRuneInString(str[i:])
	if r == utf8.RuneError && n <= 1 {
		return 0
	}
	return n
}

// codepoint is rb_enc_codepoint: it raises on an invalid sequence.
func codepoint(str string, i int) (rune, error) {
	if preciseLen(str, i) == 0 {
		return 0, errInvalidBytes
	}
	r, _ := utf8.DecodeRuneInString(str[i:])
	return r, nil
}
