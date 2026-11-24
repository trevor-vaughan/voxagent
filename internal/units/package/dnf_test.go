package pkg

import (
	"os"
	"strings"
	"testing"
)

// TestParseInstalledReadsRealRpmOutput parses output captured from the real
// container image rather than a hand-written sample, because the failure this
// guards against is a parser written against an imagined format.
//
// The count assertion is the point of the test. A parser that silently drops
// what it cannot read would still produce a non-empty map and still pass an
// "is tree in there" check, so asserting entries == non-blank lines is what
// makes a dropped line fail instead of shrinking the answer.
func TestParseInstalledReadsRealRpmOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/rpm-qa.txt")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	out := string(raw)

	want := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			want++
		}
	}
	if want == 0 {
		t.Fatalf("testdata/rpm-qa.txt has no non-blank lines; the capture is empty")
	}

	got := parseInstalled(out)
	if len(got) == 0 {
		t.Fatalf("parseInstalled returned no entries for %d non-blank lines", want)
	}
	// The capture is `rpm -qa --qf <rpmQueryFormat> | sort | head -40`, run in
	// localhost/fleet-node-el, so every name in it is
	// distinct and the entry count must equal the line count exactly.
	if len(got) != want {
		t.Errorf("parsed %d entries, want %d (one per non-blank line)", len(got), want)
	}
	for name, version := range got {
		if name == "" {
			t.Errorf("parsed an empty package name (version %q)", version)
		}
		if version == "" {
			t.Errorf("parsed an empty version for package %q", name)
		}
	}

	// rpm.rb:214 prefixes a non-zero epoch to version-release, so a real
	// version can contain a colon.
	if got["dbus"] != "1:1.14.10-6.el10" {
		t.Errorf("dbus version = %q, want %q (epoch must survive parsing)", got["dbus"], "1:1.14.10-6.el10")
	}

	// Every line of the real capture parses, so on its own it cannot tell a
	// parser that SKIPS an unreadable line from one that STOPS at it. Splicing
	// one unreadable line into the middle is what separates them: skipping
	// still yields all 40 real entries, stopping yields the handful before the
	// splice. rpm.rb:231 skips (`next if hash.empty?`), and it matters because
	// one odd line in a real `rpm -qa` must not silently truncate the
	// installed-package map the rest of the provider reasons from.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	spliced := strings.Join(lines[:1], "\n") + "\nunparseable-line-with-no-version\n" + strings.Join(lines[1:], "\n") + "\n"
	if n := len(parseInstalled(spliced)); n != want {
		t.Errorf("with one unparseable line spliced in, parsed %d entries, want %d: it must be skipped, neither stopped at nor accepted", n, want)
	}
}

// TestParseInstalledJoinsMultipleVersions pins the multiversion join. The
// joined string is what lands in a report's previous_value, so the separator is
// part of the output contract and not a display choice -- see rpm.rb:30 and
// rpm.rb:244-246.
func TestParseInstalledJoinsMultipleVersions(t *testing.T) {
	got := parseInstalled("kernel 0 6.12.0 1 x86_64\nkernel 0 6.12.0 2 x86_64\ntree 0 2.1.1 1 x86_64\n")

	if got["kernel"] != "6.12.0-1; 6.12.0-2" {
		t.Errorf("kernel = %q, want %q", got["kernel"], "6.12.0-1; 6.12.0-2")
	}
	// A single-version package must carry no separator at all.
	if got["tree"] != "2.1.1-1" {
		t.Errorf("tree = %q, want %q", got["tree"], "2.1.1-1")
	}
	if len(got) != 2 {
		t.Errorf("parsed %d entries, want 2", len(got))
	}
}

// TestParseInstalledSkipsRepeatedVersions pins rpm.rb:244's `unless
// multiversion_hash[:ensure].include?(hash[:ensure])`. Without it a package
// listed twice at the same version reports "1.0; 1.0", which no Ruby report
// contains.
func TestParseInstalledSkipsRepeatedVersions(t *testing.T) {
	got := parseInstalled("kernel 0 6.12.0 1 x86_64\nkernel 0 6.12.0 1 x86_64\n")

	if got["kernel"] != "6.12.0-1" {
		t.Errorf("kernel = %q, want %q", got["kernel"], "6.12.0-1")
	}
}

// TestParseInstalledIgnoresBlankAndMalformedLines pins rpm.rb:231's `next if
// hash.empty?`: an unparseable line is skipped, and the lines after it are
// still parsed.
func TestParseInstalledIgnoresBlankAndMalformedLines(t *testing.T) {
	got := parseInstalled("tree 0 2.1.1 1 x86_64\n\n   \nnoversion\ntree2 2.1.1-1\n")

	if len(got) != 1 {
		t.Fatalf("parsed %d entries, want 1: %v", len(got), got)
	}
	if got["tree"] != "2.1.1-1" {
		t.Errorf("tree = %q, want %q", got["tree"], "2.1.1-1")
	}
}

// TestParseInstalledDropsAZeroEpoch pins rpm.rb:213-214: version-release, with
// "epoch:" prefixed only when the epoch is not '0'. The query prints 0 both
// for a package with no epoch tag and for one built with an explicit
// `Epoch: 0`, and Ruby reports both as bare version-release. Measured with a
// locally built Epoch: 0 rpm: it reads "epzero 0 1.0 1 noarch" under this
// format, where the earlier `%|EPOCH?{%{EPOCH}:}|` format printed "0:1.0-1".
func TestParseInstalledDropsAZeroEpoch(t *testing.T) {
	got := parseInstalled("epzero 0 1.0 1 noarch\ndbus 1 1.14.10 6.el10 x86_64\n")

	if got["epzero"] != "1.0-1" {
		t.Errorf("epzero = %q, want %q", got["epzero"], "1.0-1")
	}
	if got["dbus"] != "1:1.14.10-6.el10" {
		t.Errorf("dbus = %q, want %q", got["dbus"], "1:1.14.10-6.el10")
	}
}

// TestParseInstalledMatchesNevraRegexExactly pins rpm.rb:28's NEVRA_REGEX,
// /^'?(\S+) (\S+) (\S+) (\S+) (\S+)$/ applied after `line.strip!`: one leading
// quote is dropped, surrounding whitespace is ignored, and fields are
// separated by exactly ONE space, so a doubled space is a non-matching line.
func TestParseInstalledMatchesNevraRegexExactly(t *testing.T) {
	got := parseInstalled("'quoted 0 1.0 1 noarch\n  padded 0 2.0 1 noarch  \ndoubled 0  3.0 1 noarch\n")

	if got["quoted"] != "1.0-1" {
		t.Errorf("quoted = %q, want %q -- the leading quote is outside the name capture", got["quoted"], "1.0-1")
	}
	if got["padded"] != "2.0-1" {
		t.Errorf("padded = %q, want %q -- strip! runs first", got["padded"], "2.0-1")
	}
	if v, ok := got["doubled"]; ok {
		t.Errorf("doubled = %q, want no entry -- the regex allows one space between fields", v)
	}
}
