package main

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// puppetDurationRe is Puppet's Settings::DurationSetting::FORMAT, anchored to
// the whole string (Ruby's ^ and $ also match at embedded newlines; a value
// holding one is refused here).
var puppetDurationRe = regexp.MustCompile(`\A([0-9]+)(y|d|h|m|s)?\z`)

// puppetDurationUnits is DurationSetting::UNITMAP; a bare number is seconds.
var puppetDurationUnits = map[string]time.Duration{
	"y": 365 * 24 * time.Hour, "d": 24 * time.Hour, "h": time.Hour, "m": time.Minute, "s": time.Second, "": time.Second,
}

// parsePuppetDuration reads a Puppet duration: digits with an optional
// y, d, h, m or s suffix, nothing else. A value too large for a
// time.Duration is an error.
func parsePuppetDuration(s string) (time.Duration, error) {
	m := puppetDurationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("Invalid duration format %q", s)
	}
	n, err := strconv.ParseUint(m[1], 10, 63)
	unit := puppetDurationUnits[m[2]]
	if err != nil || n > uint64(math.MaxInt64/int64(unit)) {
		return 0, fmt.Errorf("Invalid duration %q: too large", s)
	}
	return time.Duration(n) * unit, nil
}

// puppetDurationFlag is a flag.Value taking Puppet's duration syntax.
type puppetDurationFlag time.Duration

func (d *puppetDurationFlag) String() string { return time.Duration(*d).String() }

func (d *puppetDurationFlag) Set(s string) error {
	v, err := parsePuppetDuration(s)
	if err != nil {
		return err
	}
	*d = puppetDurationFlag(v)
	return nil
}
