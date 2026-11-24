package pkg

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The expectations in these tests are MEASURED, never reasoned. The tables come
// straight out of the oracle's own Ruby -- see the regeneration command in the
// header of each testdata file -- and the hand-written cases below quote the
// output of a recorded Ruby invocation, which is named in each comment so a
// reader can re-run it.
//
// Oracle: puppet-8.10.0 lib/puppet/util/rpm_compare.rb, byte-identical to the
// openvox copy (md5 b82729c2a663 both sides).

// nilField is how the oracle tables spell Ruby's nil, i.e. "this field is
// absent". An empty field means the empty string. compare_values
// (rpm_compare.rb:151-157) gives those two different answers, so the tests must
// keep them apart.
const nilField = "NIL"

// readOracleRows returns the non-comment rows of an oracle table, split on tabs.
// Fields are deliberately NOT trimmed: one epoch input is " 7 ", and trailing
// whitespace is part of what Ruby's Integer() accepts, so trimming here would
// quietly rewrite the ground truth.
func readOracleRows(t *testing.T, path string, cols int) [][]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open oracle table: %v", err)
	}
	defer f.Close()

	var rows [][]string
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSuffix(scanner.Text(), "\r")
		if strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if len(fields) != cols {
			t.Fatalf("%s:%d: %d tab-separated fields, want %d: %q", path, line, len(fields), cols, text)
		}
		rows = append(rows, fields)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read oracle table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no data rows; the capture is empty", path)
	}
	return rows
}

// checkOptional compares an optional field against the table's spelling of it.
func checkOptional(t *testing.T, input, field, want string, got *string) {
	t.Helper()

	switch {
	case want == nilField && got != nil:
		t.Errorf("rpmParseEVR(%q).%s = %q, want absent", input, field, *got)
	case want != nilField && got == nil:
		t.Errorf("rpmParseEVR(%q).%s = absent, want %q", input, field, want)
	case want != nilField && *got != want:
		t.Errorf("rpmParseEVR(%q).%s = %q, want %q", input, field, *got, want)
	}
}

// TestRPMCompareEVRMatchesOracle drives both exported comparisons off the
// measured table. Asserting rpmvercmp alongside rpmCompareEVR matters: several
// rows agree on the EVR answer for opposite reasons, and only the raw column
// shows which branch produced it.
func TestRPMCompareEVRMatchesOracle(t *testing.T) {
	for _, row := range readOracleRows(t, "testdata/rpm-evr.tsv", 4) {
		should, is := row[0], row[1]
		want := [2]int{}
		for i, col := range row[2:4] {
			n, err := strconv.Atoi(col)
			if err != nil {
				t.Fatalf("row %q/%q: result column %q is not an integer: %v", should, is, col, err)
			}
			want[i] = n
		}

		t.Run(fmt.Sprintf("%q_%q", should, is), func(t *testing.T) {
			if got := rpmCompareEVR(should, is); got != want[0] {
				t.Errorf("rpmCompareEVR(%q, %q) = %d, want %d", should, is, got, want[0])
			}
			if got := rpmvercmp(should, is); got != want[1] {
				t.Errorf("rpmvercmp(%q, %q) = %d, want %d", should, is, got, want[1])
			}
		})
	}
}

// TestRPMParseEVRMatchesOracle covers the parse on its own, because two of its
// behaviours are invisible in a comparison result: the architecture excised
// from the release, and the epoch normalised through Ruby's Integer().
func TestRPMParseEVRMatchesOracle(t *testing.T) {
	for _, row := range readOracleRows(t, "testdata/rpm-parse-evr.tsv", 5) {
		input := row[0]

		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			got := rpmParseEVR(input)
			checkOptional(t, input, "epoch", row[1], got.epoch)
			if row[2] == nilField {
				t.Fatalf("table row %q claims an absent version; rpm_parse_evr always sets one", input)
			}
			if got.version != row[2] {
				t.Errorf("rpmParseEVR(%q).version = %q, want %q", input, got.version, row[2])
			}
			checkOptional(t, input, "release", row[3], got.release)
			checkOptional(t, input, "arch", row[4], got.arch)
		})
	}
}

// TestRPMCompareEVRNoDeclaredReleaseIsInSync pins PUP-1244
// (rpm_compare.rb:185-192): when the user did not spell a release, an installed
// package that only differs in release is in sync. Dropping that early return
// does not just "upgrade more eagerly" -- for `ensure => '8.1'` against an
// installed 8.1-3.el10 it reports the declared value as OLDER, which is a
// downgrade on every run.
//
// The rpmvercmp assertions are the control: they show the raw comparison
// disagrees, so a 0 here can only come from the PUP-1244 branch.
func TestRPMCompareEVRNoDeclaredReleaseIsInSync(t *testing.T) {
	for _, tc := range []struct {
		should, is string
		rawWant    int
	}{
		{should: "8.1", is: "8.1-3.el10", rawWant: -1},
		{should: "1.0", is: "1.0-1", rawWant: -1},
		{should: "5.2.15", is: "5.2.15-1.el10.x86_64", rawWant: -1},
	} {
		if got := rpmCompareEVR(tc.should, tc.is); got != 0 {
			t.Errorf("rpmCompareEVR(%q, %q) = %d, want 0 (PUP-1244: no declared release means in sync)", tc.should, tc.is, got)
		}
		if got := rpmvercmp(tc.should, tc.is); got != tc.rawWant {
			t.Errorf("rpmvercmp(%q, %q) = %d, want %d; the raw comparison must disagree or this test proves nothing", tc.should, tc.is, got, tc.rawWant)
		}
	}

	// The converse: a declared release IS compared, so this branch cannot be
	// implemented by never comparing releases at all.
	if got := rpmCompareEVR("1.0-1", "1.0-2"); got != -1 {
		t.Errorf("rpmCompareEVR(%q, %q) = %d, want -1 (a declared release is compared)", "1.0-1", "1.0-2", got)
	}
}

// TestRPMCompareEVREpochComparedOnlyWhenDeclared pins rpm_compare.rb:177-180's
// `unless should_hash[:epoch].nil?`. The installed string usually carries an
// epoch and the declared one usually does not; comparing unconditionally would
// make compare_values(nil, "1") = -1 and report a permanent, unfixable drift.
func TestRPMCompareEVREpochComparedOnlyWhenDeclared(t *testing.T) {
	// Undeclared epoch: the installed 1: is ignored entirely.
	if got := rpmCompareEVR("1.0-1", "1:1.0-1"); got != 0 {
		t.Errorf("rpmCompareEVR(%q, %q) = %d, want 0 (undeclared epoch is not compared)", "1.0-1", "1:1.0-1", got)
	}
	// An epoch that fails Ruby's Integer() is absent, not zero, so it is
	// likewise not compared (rpm_compare.rb:126-131).
	if got := rpmCompareEVR("foo:1.0-1", "1.0-1"); got != 0 {
		t.Errorf("rpmCompareEVR(%q, %q) = %d, want 0 (unparseable epoch is absent)", "foo:1.0-1", "1.0-1", got)
	}
	// Declared epoch: compared, and it outranks a newer version.
	if got := rpmCompareEVR("1:1.0-1", "2.0-1"); got != 1 {
		t.Errorf("rpmCompareEVR(%q, %q) = %d, want 1 (declared epoch wins over version)", "1:1.0-1", "2.0-1", got)
	}
	// Declared epoch 0 against an installed string with no epoch at all is
	// NOT equal: compare_values sees "0" against nil, and non-nil wins.
	if got := rpmCompareEVR("0:1.0-1", "1.0-1"); got != 1 {
		t.Errorf("rpmCompareEVR(%q, %q) = %d, want 1 (declared 0 epoch beats an absent one)", "0:1.0-1", "1.0-1", got)
	}
}

// TestCompareValuesAbsentIsNotEmpty pins rpm_compare.rb:151-157. Absent loses to
// every present value, including the empty string and including "~", which
// rpmvercmp would otherwise sort below everything.
//
// Measured with compare_values over the same argument pairs.
func TestCompareValuesAbsentIsNotEmpty(t *testing.T) {
	empty, one, tilde := "", "1", "~"
	for _, tc := range []struct {
		name   string
		s1, s2 *string
		want   int
	}{
		{name: `absent, absent`, s1: nil, s2: nil, want: 0},
		{name: `"", absent`, s1: &empty, s2: nil, want: 1},
		{name: `absent, ""`, s1: nil, s2: &empty, want: -1},
		{name: `"", ""`, s1: &empty, s2: &empty, want: 0},
		{name: `"", "1"`, s1: &empty, s2: &one, want: -1},
		{name: `"1", ""`, s1: &one, s2: &empty, want: 1},
		{name: `absent, "1"`, s1: nil, s2: &one, want: -1},
		{name: `"1", absent`, s1: &one, s2: nil, want: 1},
		{name: `"~", absent`, s1: &tilde, s2: nil, want: 1},
		{name: `absent, "~"`, s1: nil, s2: &tilde, want: -1},
	} {
		if got := compareValues(tc.s1, tc.s2); got != tc.want {
			t.Errorf("compareValues(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestRPMVercmpTildeSortsBeforeEverything pins the tilde branch
// (rpm_compare.rb:37-47). It is what makes a pre-release older than the release
// it leads to, and it beats the empty string, which no other segment does.
func TestRPMVercmpTildeSortsBeforeEverything(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{a: "1.0~rc1", b: "1.0", want: -1},
		{a: "1.0", b: "1.0~rc1", want: 1},
		{a: "1.0~rc1", b: "1.0~rc2", want: -1},
		{a: "~", b: "", want: -1},
		{a: "", b: "~", want: 1},
		{a: "1~", b: "1", want: -1},
		{a: "1.0~beta", b: "1.0~alpha", want: 1},
	} {
		if got := rpmvercmp(tc.a, tc.b); got != tc.want {
			t.Errorf("rpmvercmp(%q, %q) = %d, want %d (tilde sorts before everything)", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRPMVercmpNumericSegmentsAreNumbers pins rpm_compare.rb:92-99: a numeric
// segment loses its leading zeros and is then ranked by digit count before
// strcmp. Byte comparison alone gets every one of these backwards.
func TestRPMVercmpNumericSegmentsAreNumbers(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{a: "2.0", b: "10.0", want: -1},
		{a: "10.0", b: "2.0", want: 1},
		{a: "1.0", b: "1.00", want: 0},
		{a: "001", b: "1", want: 0},
		{a: "007", b: "7", want: 0},
		{a: "20240101", b: "20231231", want: 1},
	} {
		if got := rpmvercmp(tc.a, tc.b); got != tc.want {
			t.Errorf("rpmvercmp(%q, %q) = %d, want %d (numeric segments compare as numbers)", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRPMVercmpLineAnchorsAreMultiline pins a quirk that only shows up on input
// containing a newline: every anchor in rpm_compare.rb is `^`, and Ruby's `^`
// matches at the start of every LINE, not of the string. So the segment grab at
// :54 can skip past a whole leading line, and the tilde test at :38 can fire on
// a tilde that is not at the front. A port that treats these as \A anchors
// answers differently.
//
// Measured with:
//
//	rpmvercmp("a\n1", "1") ; rpmvercmp("1", "a\n1") ; rpmvercmp("a\n~", "a")
//	rpmvercmp(".a\n.1", "1") ; rpmvercmp("1.0\n-1", "1.0")
func TestRPMVercmpLineAnchorsAreMultiline(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{a: "a\n1", b: "1", want: 0},
		{a: "1", b: "a\n1", want: 0},
		{a: "a\n~", b: "a", want: -1},
		{a: ".a\n.1", b: "1", want: 0},
		{a: "1.0\n-1", b: "1.0", want: 1},
	} {
		if got := rpmvercmp(tc.a, tc.b); got != tc.want {
			t.Errorf("rpmvercmp(%q, %q) = %d, want %d (Ruby's ^ is a line anchor)", tc.a, tc.b, got, tc.want)
		}
	}
}
