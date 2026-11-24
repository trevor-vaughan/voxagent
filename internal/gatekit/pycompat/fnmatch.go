package pycompat

import (
	"fmt"
	"regexp"
	"strings"
)

// CompileFnmatch compiles pattern with the semantics of Python 3.12's
// fnmatch.fnmatch on POSIX, where os.path.normcase is the identity and so the
// call is fnmatchcase: `*` matches any run of characters including `.` and
// newline, `?` matches one character, and `[...]` is a character class with
// `!` for negation. There is no escape character.
//
// It is a port of CPython's fnmatch.translate, not path.Match, because
// path.Match's `*` stops at `/` and its classes differ: the allowlists this
// serves were written for CPython and depend on `*` crossing dots.
//
// Two departures from translate's output text, neither of which changes what
// matches:
//
//   - translate wraps each interior `*`-plus-literal pairing in an atomic group,
//     `(?>.*?fixed)`, which RE2 does not support. Every piece of `fixed` matches
//     exactly one character and the pairing is always followed by another `*`,
//     so the leftmost occurrence of `fixed` can always be extended to a match
//     whenever any occurrence can. The atomic group only stops a backtracking
//     engine from trying the others; RE2 never backtracks, so `.*` suffices.
//   - Inside a class, `[` and `]` are escaped. Python reads `[` inside a class
//     as a literal; RE2 reads `[:` as the start of a POSIX class such as
//     `[:alpha:]`.
//
// An error means CPython's translate would have raised on this pattern, or that
// RE2 rejected the class CPython built for it.
func CompileFnmatch(pattern string) (*regexp.Regexp, error) {
	body, err := translate([]rune(pattern))
	if err != nil {
		return nil, fmt.Errorf("fnmatch pattern %q: %w", pattern, err)
	}
	re, err := regexp.Compile(`^(?s:` + body + `)\z`)
	if err != nil {
		return nil, fmt.Errorf("fnmatch pattern %q: %w", pattern, err)
	}
	return re, nil
}

// translate follows CPython 3.12's fnmatch.translate step for step; the
// variable names are CPython's so the two can be read side by side.
func translate(pat []rune) (string, error) {
	var res strings.Builder
	n := len(pat)
	i := 0
	lastStar := false
	for i < n {
		c := pat[i]
		i++
		if c == '*' {
			if !lastStar {
				res.WriteString(`.*`)
			}
			lastStar = true
			continue
		}
		lastStar = false
		switch c {
		case '?':
			res.WriteString(`.`)
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				res.WriteString(`\[`)
				continue
			}
			class, err := translateClass(pat, i, j)
			if err != nil {
				return "", err
			}
			res.WriteString(class)
			i = j + 1
		default:
			res.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return res.String(), nil
}

// translateClass renders pat[i:j], the text between `[` and its closing `]`,
// as an RE2 class.
func translateClass(pat []rune, i, j int) (string, error) {
	var stuff string
	if !containsRune(pat[i:j], '-') {
		stuff = escapeClassText(pat[i:j], false)
	} else {
		var chunks [][]rune
		k := i + 1
		if pat[i] == '!' {
			k = i + 2
		}
		for {
			k = indexRune(pat, '-', k, j)
			if k < 0 {
				break
			}
			chunks = append(chunks, append([]rune(nil), pat[i:k]...))
			i = k + 1
			k = k + 3
		}
		if chunk := pat[i:j]; len(chunk) > 0 {
			chunks = append(chunks, append([]rune(nil), chunk...))
		} else {
			chunks[len(chunks)-1] = append(chunks[len(chunks)-1], '-')
		}
		// Remove empty ranges -- invalid in RE.
		for k := len(chunks) - 1; k > 0; k-- {
			prev, next := chunks[k-1], chunks[k]
			if len(prev) == 0 || len(next) == 0 {
				return "", fmt.Errorf("CPython's translate raises IndexError on an emptied range")
			}
			if prev[len(prev)-1] > next[0] {
				chunks[k-1] = append(append([]rune(nil), prev[:len(prev)-1]...), next[1:]...)
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
		parts := make([]string, len(chunks))
		for x, chunk := range chunks {
			parts[x] = escapeClassText(chunk, true)
		}
		stuff = strings.Join(parts, "-")
	}
	switch {
	case stuff == "":
		// Empty range: never match. RE2 has no (?!); an empty class says the same.
		return `[^\x00-\x{10FFFF}]`, nil
	case stuff == "!":
		// Negated empty range: match any character.
		return `.`, nil
	case stuff[0] == '!':
		return `[^` + stuff[1:] + `]`, nil
	case stuff[0] == '^':
		return `[\` + stuff + `]`, nil
	default:
		return `[` + stuff + `]`, nil
	}
}

// escapeClassText escapes what CPython escapes inside a class -- the backslash,
// the hyphen when it is not a range separator, and the set operators & ~ | --
// plus the [ and ] RE2 would otherwise misread.
func escapeClassText(text []rune, hyphens bool) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\\', r == '&', r == '~', r == '|', r == '[', r == ']',
			r == '-' && hyphens:
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func containsRune(text []rune, r rune) bool {
	for _, c := range text {
		if c == r {
			return true
		}
	}
	return false
}

// indexRune is Python's str.find(r, start, end) over runes.
func indexRune(text []rune, r rune, start, end int) int {
	for x := start; x < end && x < len(text); x++ {
		if text[x] == r {
			return x
		}
	}
	return -1
}
