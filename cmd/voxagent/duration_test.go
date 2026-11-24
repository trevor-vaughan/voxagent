package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func TestPuppetDurationAccepts(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "120": 120 * time.Second, "007": 7 * time.Second,
		"30s": 30 * time.Second, "5m": 5 * time.Minute, "2h": 2 * time.Hour,
		"1d": 24 * time.Hour, "1y": 365 * 24 * time.Hour, "0s": 0,
	} {
		got, err := parsePuppetDuration(in)
		if err != nil || got != want {
			t.Errorf("parsePuppetDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// Everything Puppet's DurationSetting::FORMAT, /^(\d+)(y|d|h|m|s)?$/, refuses,
// plus values whose seconds overflow a time.Duration.
func TestPuppetDurationRejects(t *testing.T) {
	for _, in := range []string{
		"", "s", "-5", "+5", "1.5s", "1h30m", "5S", "5M", " 5", "5 ", "5ms", "5w",
		"1_000", "٣", "5\n", "0x10", "300000000000", "9999999999y",
	} {
		if got, err := parsePuppetDuration(in); err == nil {
			t.Errorf("parsePuppetDuration(%q) = %v, want an error", in, got)
		}
	}
}

func TestWaitforcertFlag(t *testing.T) {
	var d puppetDurationFlag
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&d, "waitforcert", "")
	if err := fs.Parse([]string{"--waitforcert", "2m"}); err != nil || time.Duration(d) != 2*time.Minute {
		t.Errorf("--waitforcert 2m = %v, %v", time.Duration(d), err)
	}
	err := fs.Parse([]string{"--waitforcert", "1h30m"})
	if err == nil || !strings.Contains(err.Error(), "Invalid duration format") {
		t.Errorf("--waitforcert 1h30m: err = %v, want Puppet's refusal", err)
	}
}
