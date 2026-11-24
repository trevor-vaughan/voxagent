package pkg

import (
	"math/big"
	"regexp"
	"strings"
)

// This file is a transliteration of util/rpm_compare.rb, the mixin Puppet's yum
// and dnf providers use to decide whether a declared version is newer, older or
// the same as the installed one. yum.rb:250-300 turns that answer into the
// subcommand: `install` for newer, `downgrade` for older, nothing for the same.
// Getting it backwards is not cosmetic -- `dnf -y install <older-version>`
// fails where `downgrade` succeeds.
//
// The Ruby is itself a deliberate transliteration of rpm's lib/rpmvercmp.c, and
// says so: "Some of the things in here look REALLY UGLY and/or arbitrary. Our
// goal is to match how RPM compares versions, quirks and all." The same applies
// here. Nothing below is tidied, simplified or modernised; where a line looks
// wrong it is cited back to the Ruby line it came from, and every expectation in
// rpmvercmp_test.go was measured by running that Ruby rather than reasoned out.
//
// Nothing calls this yet. Wiring `ensure => <version>` into the package
// provider is a separate piece of work.

// rpmArchList is ARCH_LIST (rpm_compare.rb:5-8), in the oracle's order. The
// order is load-bearing, because ARCH_REGEX is an alternation built from it and
// Ruby takes the first alternative that matches at a position rather than the
// longest: ".ppc" is tried before ".ppc64", so a release of "1.ppc64" yields
// architecture "ppc" and, since the match is excised, release "164".
// regexp.MustCompile has those same leftmost-first semantics;
// regexp.MustCompilePOSIX, which is leftmost-longest, would not.
var rpmArchList = []string{
	"noarch", "i386", "i686", "ppc", "ppc64", "armv3l", "armv4b", "armv4l", "armv4tl",
	"armv5tel", "armv5tejl", "armv6l", "armv7l", "m68kmint", "s390", "s390x", "ia64",
	"x86_64", "sh3", "sh4",
}

// rpmArchRegex is ARCH_REGEX (rpm_compare.rb:10), built the same way: each
// architecture prefixed with a literal dot, joined by alternation.
var rpmArchRegex = regexp.MustCompile(`\.` + strings.Join(rpmArchList, `|\.`))

// Every anchor in the Ruby is `^`, and Ruby's `^` matches at the start of every
// LINE, not of the string; `\A` is the string anchor it does not use. On input
// containing a newline that is observable -- the segment grab at
// rpm_compare.rb:54 can skip a whole leading line because it takes the match's
// post_match rather than requiring a prefix match, and the tilde test at :38 can
// fire on a tilde that is not at the front. (?m) is Go's spelling of the same
// semantics, so these patterns behave as the oracle's do rather than as the
// stricter thing a reader might expect. See TestRPMVercmpLineAnchorsAreMultiline.
var (
	rpmFrontStripRe = regexp.MustCompile(`(?m)^[^A-Za-z0-9~]+`)
	rpmTildeRe      = regexp.MustCompile(`(?m)^~`)
	rpmDigitsRe     = regexp.MustCompile(`(?m)^[0-9]+`)
	rpmAlphaRe      = regexp.MustCompile(`(?m)^[A-Za-z]+`)
)

// rpmEVR is the hash rpmParseEVR returns (rpm_compare.rb:145).
//
// Epoch, release and architecture are pointers because the oracle's nil is a
// third state that compare_values acts on: an absent field loses to every
// present one, including the empty string and including "~", which otherwise
// sorts below everything. A plain string would collapse "the user did not say"
// into "the user said nothing", and PUP-1244 turns on exactly that distinction.
// Version is a plain string because the oracle never leaves it nil.
type rpmEVR struct {
	epoch   *string
	version string
	release *string
	arch    *string
}

// rpmvercmp is rpm_compare.rb:27-113, which is rpm's lib/rpmvercmp.c.
//
// Returns 1 if str1 is newer than str2, 0 if they are identical, -1 if str1 is
// older than str2.
func rpmvercmp(str1, str2 string) int {
	if str1 == str2 {
		return 0
	}

	for len(str1) > 0 || len(str2) > 0 {
		// Trim anything that is neither alphanumeric nor '~' off the front.
		str1 = rpmFrontStripRe.ReplaceAllString(str1, "")
		str2 = rpmFrontStripRe.ReplaceAllString(str2, "")

		// "handle the tilde separator, it sorts before everything else"
		// (rpm_compare.rb:37-47). Note that the oracle drops the first
		// character rather than the matched one; with a line anchor those are
		// not always the same character.
		t1, t2 := rpmTildeRe.MatchString(str1), rpmTildeRe.MatchString(str2)
		switch {
		case t1 && t2:
			str1, str2 = str1[1:], str2[1:]
			continue
		case t1:
			return -1
		case t2:
			return 1
		}

		if len(str1) == 0 || len(str2) == 0 {
			break
		}

		// "grab first completely alpha or completely numeric segment". Each
		// grab takes the match and keeps the match's post_match, discarding
		// anything before it (rpm_compare.rb:54-82). A string that does not
		// match yields an empty segment and is left untouched.
		//
		// The oracle tests the numeric condition twice, at :54 and again at
		// :55; the inner test cannot fail, so it is folded away here.
		var segment1, segment2 string
		isnum := false
		if loc := rpmDigitsRe.FindStringIndex(str1); loc != nil {
			segment1, str1 = str1[loc[0]:loc[1]], str1[loc[1]:]
			if loc := rpmDigitsRe.FindStringIndex(str2); loc != nil {
				segment2, str2 = str2[loc[0]:loc[1]], str2[loc[1]:]
			}
			isnum = true
		} else {
			if loc := rpmAlphaRe.FindStringIndex(str1); loc != nil {
				segment1, str1 = str1[loc[0]:loc[1]], str1[loc[1]:]
			}
			if loc := rpmAlphaRe.FindStringIndex(str2); loc != nil {
				segment2, str2 = str2[loc[0]:loc[1]], str2[loc[1]:]
			}
		}

		// The segments are of different types, where alpha includes ''.
		// "numeric segments are always newer than alpha segments". Only
		// segment2 is tested; segment1 being empty is not a case the oracle
		// asks about (rpm_compare.rb:86-90).
		if len(segment2) == 0 {
			if isnum {
				return 1
			}
			return -1
		}

		if isnum {
			// "throw away any leading zeros - it's a number, right?" The
			// oracle uses gsub(/^0+/, ''), but a segment matched by [0-9]+
			// cannot contain a newline, so the line anchor is not reachable
			// here and TrimLeft is exact. Stripping "000" to "" is intended.
			segment1 = strings.TrimLeft(segment1, "0")
			segment2 = strings.TrimLeft(segment2, "0")
			// "whichever number has more digits wins"
			if len(segment1) > len(segment2) {
				return 1
			}
			if len(segment1) < len(segment2) {
				return -1
			}
		}

		// "strcmp will return which one is greater - even if the two segments
		// are alpha or if they are numeric. don't return if they are equal
		// because there might be more segments to compare". Ruby's String <=>
		// and strings.Compare are both bytewise.
		if rc := strings.Compare(segment1, segment2); rc != 0 {
			return rc
		}
	}

	// "whichever version still has characters left over wins". The oracle
	// compares character counts and this compares bytes, which differ on
	// multibyte input -- but the loop can only reach here with at least one of
	// the two strings empty, so the sign is the same either way.
	if len(str1) > len(str2) {
		return 1
	}
	if len(str1) < len(str2) {
		return -1
	}
	return 0
}

// rpmParseEVR is rpm_parse_evr (rpm_compare.rb:118-146), rpm's
// rpmUtils.miscutils.stringToVersion(): epoch before the first ':', release
// after the first '-', architecture scanned back out of the release.
//
// An epoch that is not a number is absent rather than an error, and the
// architecture is only looked for when there is a release to look in.
func rpmParseEVR(full string) rpmEVR {
	var parsed rpmEVR

	if i := strings.Index(full, ":"); i >= 0 {
		if epoch, ok := rubyIntegerString(full[:i]); ok {
			parsed.epoch = &epoch
		}
		full = full[i+1:]
	}

	if i := strings.Index(full, "-"); i >= 0 {
		parsed.version = full[:i]
		release := full[i+1:]
		// scan(ARCH_REGEX)[0] reports the FIRST match, but the gsub that
		// follows removes EVERY match, so "1.noarch.x86_64" reports noarch and
		// leaves release "1" (rpm_compare.rb:136-140).
		if match := rpmArchRegex.FindString(release); match != "" {
			arch := strings.ReplaceAll(match, ".", "")
			parsed.arch = &arch
			release = rpmArchRegex.ReplaceAllString(release, "")
		}
		parsed.release = &release
	} else {
		parsed.version = full
	}

	return parsed
}

// rubyIntegerString is String(Integer(s)) from rpm_compare.rb:127, with ok=false
// standing in for the exception that sends the epoch to nil.
//
// This is not base-10 parsing, and reaching for strconv.Atoi here would disagree
// with the oracle on real input. Ruby's Integer() reads a whole integer literal:
// a bare leading zero means octal, so "010" normalises to "8" and "08" is not a
// number at all; 0x, 0b, 0o and 0d prefixes are honoured; single underscores are
// allowed between digits but not next to a prefix or at either end; surrounding
// ASCII whitespace and a leading sign are accepted; and the result has arbitrary
// precision, hence math/big rather than an int64. Each of those forms is a
// measured row in testdata/rpm-parse-evr.tsv.
func rubyIntegerString(s string) (string, bool) {
	body := strings.Trim(s, " \t\n\v\f\r")

	negative := false
	if len(body) > 0 && (body[0] == '+' || body[0] == '-') {
		negative = body[0] == '-'
		body = body[1:]
	}

	base := 10
	if len(body) >= 2 && body[0] == '0' {
		switch body[1] {
		case 'x', 'X':
			base, body = 16, body[2:]
		case 'b', 'B':
			base, body = 2, body[2:]
		case 'o', 'O':
			base, body = 8, body[2:]
		case 'd', 'D':
			base, body = 10, body[2:]
		default:
			// A bare leading zero is octal and stays a digit rather than
			// becoming a prefix: Ruby accepts "0_1" as 1 but rejects "0x_10",
			// so the underscore in the first is between two digits.
			base = 8
		}
	}
	if body == "" {
		return "", false
	}

	var digits strings.Builder
	// Seeded true so that a leading underscore, which has no digit before it,
	// is rejected; left true by a trailing one for the same reason.
	afterUnderscore := true
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == '_' {
			if afterUnderscore {
				return "", false
			}
			afterUnderscore = true
			continue
		}
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return "", false
		}
		if v >= base {
			return "", false
		}
		afterUnderscore = false
		digits.WriteByte(c)
	}
	if afterUnderscore {
		return "", false
	}

	n, ok := new(big.Int).SetString(digits.String(), base)
	if !ok {
		return "", false
	}
	if negative {
		n.Neg(n)
	}
	return n.String(), true
}

// compareValues is compare_values (rpm_compare.rb:151-157), rpm's
// python/header-py.c. Absent beats nothing and loses to everything present.
func compareValues(s1, s2 *string) int {
	switch {
	case s1 == nil && s2 == nil:
		return 0
	case s2 == nil:
		return 1
	case s1 == nil:
		return -1
	}
	return rpmvercmp(*s1, *s2)
}

// rpmCompareEVR is rpm_compare_evr (rpm_compare.rb:172-195), which combines
// rpmUtils.miscutils.compareEVR() and rpm.labelCompare().
//
// "should" can be v, v-r or e:v-r; "is" will always be at least v-r and can be
// e:v-r. Returns 1 if should is newer than is, 0 if they are the same version,
// -1 if is is newer than should.
func rpmCompareEVR(should, is string) int {
	shouldEVR := rpmParseEVR(should)
	isEVR := rpmParseEVR(is)

	// The epoch is compared only when the declared value carries one
	// (rpm_compare.rb:177). An installed package normally reports an epoch and
	// a manifest normally omits it, so comparing unconditionally would pit
	// absent against present, report drift that no install can fix, and churn
	// on every run.
	if shouldEVR.epoch != nil {
		if rc := compareValues(shouldEVR.epoch, isEVR.epoch); rc != 0 {
			return rc
		}
	}

	// The oracle routes this through compare_values too, but rpm_parse_evr
	// never leaves version nil, so the nil arms are unreachable.
	if rc := rpmvercmp(shouldEVR.version, isEVR.version); rc != 0 {
		return rc
	}

	// PUP-1244 (rpm_compare.rb:185-192): if the user did not specify a release
	// and everything up to here is equal, the values are equal. "We need to
	// evaluate to whatever level of detail the user specified, so we don't end
	// up upgrading or *downgrading* when not intended" -- `ensure => '8.1'` is
	// in sync with an installed 8.1-3.el10, and without this it would compare
	// absent against "3.el10", report the declared value as older, and hand
	// yum.rb a downgrade on every single run.
	//
	// The oracle adds that this "should NOT be triggered if we're trying to
	// ensure latest": `ensure => latest` compares against what the repository
	// offers, which is a full v-r, and must not be routed through this branch.
	if shouldEVR.release == nil {
		return 0
	}

	return compareValues(shouldEVR.release, isEVR.release)
}
