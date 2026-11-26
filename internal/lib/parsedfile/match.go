package parsedfile

import "regexp"

// Capture is one capture group of a match. OK is false when the group did not
// participate in the match (Ruby's nil, as opposed to the empty string).
type Capture struct {
	S  string
	OK bool
}

// Matcher selects a text line or, for a record type in match mode, splits a
// line into captures (fp:164, fp:189-198). Match returns the captures of
// groups 1..n (not the whole match) and whether the line matched.
type Matcher interface {
	Match(line string) ([]Capture, bool)
}

type re2Matcher struct{ re *regexp.Regexp }

// RE2 adapts a compiled pattern to Matcher. The search is unanchored, as
// Ruby's =~ and Regexp#match are.
func RE2(re *regexp.Regexp) Matcher { return re2Matcher{re} }

func (m re2Matcher) Match(line string) ([]Capture, bool) {
	loc := m.re.FindStringSubmatchIndex(line)
	if loc == nil {
		return nil, false
	}
	caps := make([]Capture, 0, len(loc)/2-1)
	for i := 2; i < len(loc); i += 2 {
		if loc[i] < 0 {
			caps = append(caps, Capture{})
			continue
		}
		caps = append(caps, Capture{S: line[loc[i]:loc[i+1]], OK: true})
	}
	return caps, true
}
