package mountcore

import (
	"context"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/parsedfile/vectorstest"
)

func testClass() *parsedfile.Class {
	return mountClass(func(string) (parsedfile.FileType, error) { return nil, nil }, time.Now)
}

func TestMountClassIdentity(t *testing.T) {
	c := testClass()
	if c.Name != "parsed" || c.TypeName != "mount" || c.DefaultTarget != "/etc/fstab" {
		t.Errorf("class = %q/%q default %q, want parsed/mount /etc/fstab", c.Name, c.TypeName, c.DefaultTarget)
	}
	if c.Grammar == nil || c.FileType == nil || c.Now == nil || c.PrefetchHook == nil {
		t.Errorf("Grammar, FileType, Now and PrefetchHook must be set: %+v", c)
	}
}

// TestMountGrammarMatchesSharedVectors runs every "mount" case of
// vectors.json, whose wants fileparsing_vectors.rb takes from the real engine
// and the provider's own definitions (mp:34-44, 185-224).
func TestMountGrammarMatchesSharedVectors(t *testing.T) {
	g := testClass().Grammar
	for _, c := range vectorstest.Read(t, "mount") {
		t.Run(c.Name+"/"+c.Op, func(t *testing.T) { c.Check(t, g) })
	}
}

// ---- edges the vectors cannot hold --------------------------------------

// TestIncompleteCountsTokens: the matcher is mp:44's lookahead, true for a line
// with fewer than two whitespace-separated tokens, and \s includes \v.
func TestIncompleteCountsTokens(t *testing.T) {
	m := incompleteMatcher{}
	for line, want := range map[string]bool{
		"": true, "   ": true, "a": true, "  a": true, "a  ": true, "\ta\t": true, "a\v": true,
		"a b": false, "  a b": false, "a\vb": false, "a\tb\tc": false, "a b": true,
	} {
		if _, got := m.Match(line); got != want {
			t.Errorf("incomplete(%q) = %v, want %v", line, got, want)
		}
	}
}

// TestParsedLineRoundTrips: a parsed record is written back as read, an
// optional field it lacks as an empty text (M13: no error, spec 5.4).
func TestParsedLineRoundTrips(t *testing.T) {
	g := testClass().Grammar
	for in, want := range map[string]string{
		"/dev/x\t/mnt/a":                         "/dev/x\t/mnt/a\t\t\t\t",
		"/dev/x /mnt/a ext4":                     "/dev/x\t/mnt/a\text4\t\t\t",
		"/dev/x /mnt/a ext4 ro":                  "/dev/x\t/mnt/a\text4\tro\t\t",
		"/dev/x /mnt/a ext4 ro 1":                "/dev/x\t/mnt/a\text4\tro\t1\t",
		"/dev/x /mnt/a ext4 ro 1 2":              "/dev/x\t/mnt/a\text4\tro\t1\t2",
		"/dev/x /mnt/a ext4 ro 1 2  extra  more": "/dev/x\t/mnt/a\text4\tro\t1\t2\textra\tmore",
		"a\\040b /m\\040n ext4":                  "a\\040b\t/m\\040n\text4\t\t\t",
	} {
		recs, err := g.Parse(in)
		if err != nil || len(recs) != 1 {
			t.Fatalf("Parse(%q) = %v, %v", in, recs, err)
		}
		got, err := g.ToFile(recs)
		if err != nil || got != want+"\n" {
			t.Errorf("ToFile(Parse(%q)) = %q, %v; want %q", in, got, err, want+"\n")
		}
	}
}

// TestLeadingWhitespaceFailsTheJoin: the split's leading empty field leaves
// device absent (spec 5.4, 8 item 13).
func TestLeadingWhitespaceFailsTheJoin(t *testing.T) {
	g := testClass().Grammar
	recs, err := g.Parse("  /dev/x /mnt/a ext4")
	if err != nil || len(recs) != 1 {
		t.Fatalf("Parse = %v, %v", recs, err)
	}
	if got := recs[0].Fields["device"]; got.Kind != parsedfile.Absent {
		t.Errorf("device = %+v, want absent", got)
	}
	if _, err := g.ToFile(recs); err == nil || err.Error() != "Field 'device' is required" {
		t.Errorf("ToFile err = %v, want Field 'device' is required", err)
	}
}

// TestPreGenFstype: a managed record's Nil or empty fstype is refused with the
// name; an Absent one (a parsed record without it) is not.
func TestPreGenFstype(t *testing.T) {
	g := testClass().Grammar
	rec := func(fstype parsedfile.Value) parsedfile.Record {
		return parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{"device": str("/dev/x"), "name": str("/mnt/a"), "fstype": fstype}}
	}
	for name, v := range map[string]parsedfile.Value{"nil": {Kind: parsedfile.Nil}, "empty": str("")} {
		if _, err := g.ToLine(rec(v)); err == nil || err.Error() != "Mount[/mnt/a]: Field 'fstype' is required" {
			t.Errorf("%s fstype: err = %v", name, err)
		}
	}
	if got, err := g.ToLine(rec(parsedfile.Value{Kind: parsedfile.Absent})); err != nil || got != "/dev/x\t/mnt/a\t\tdefaults\t\t" {
		t.Errorf("absent fstype: %q, %v", got, err)
	}
	// The record the caller passed keeps its fields: PreGen works on a copy.
	r := rec(str("ext4"))
	if _, err := g.ToLine(r); err != nil || r.Fields["options"].Kind != parsedfile.Nil {
		t.Errorf("caller's record changed: %+v, %v", r.Fields, err)
	}
}

// TestPreGenErrorsComeFromTheEngine: the fstype refusal is PreGen's, so it
// precedes the join's refusal of a missing device (Ruby's order).
func TestPreGenRefusesBeforeTheJoin(t *testing.T) {
	g := testClass().Grammar
	rec := parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{"name": str("/m")}}
	if _, err := g.ToLine(rec); err == nil || err.Error() != "Mount[/m]: Field 'fstype' is required" {
		t.Errorf("err = %v, want the fstype refusal", err)
	}
}

// TestDecodeAndEncodeOnlyDeviceAndName: \040 is a space in device and name and
// nowhere else.
func TestDecodeAndEncodeOnlyDeviceAndName(t *testing.T) {
	g := testClass().Grammar
	recs, err := g.Parse("a\\040b c\\040d e\\040f g\\040h 0 0")
	if err != nil {
		t.Fatal(err)
	}
	f := recs[0].Fields
	if f["device"].S != "a b" || f["name"].S != "c d" || f["fstype"].S != `e\040f` || f["options"].S != `g\040h` {
		t.Errorf("fields = %+v", f)
	}
	got, _ := g.ToFile(recs)
	if want := "a\\040b\tc\\040d\te\\040f\tg\\040h\t0\t0\n"; got != want {
		t.Errorf("ToFile = %q, want %q", got, want)
	}
}

func TestPrefetchHook(t *testing.T) {
	c := testClass()
	recs, err := c.Grammar.Parse("# c\n\n/dev/x /mnt//a/// ext4 ro 0 0\n/dev/y /\n/dev/z //\nlonely\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		r.Ensure = "present"
	}
	out, err := c.PrefetchHook(context.Background(), "/etc/fstab", recs)
	if err != nil || len(out) != len(recs) {
		t.Fatalf("hook = %v, %v", out, err)
	}
	want := []struct{ typ, name, ensure string }{
		{"comment", "", "present"}, {"blank", "", "present"}, {"parsed", "/mnt//a", "unmounted"},
		{"parsed", "/", "unmounted"}, {"parsed", "/", "unmounted"}, {"incomplete", "", "present"},
	}
	for i, w := range want {
		r := out[i]
		if r.Type != w.typ || r.Fields["name"].S != w.name || r.Ensure != w.ensure {
			t.Errorf("record %d = %s name %q ensure %q, want %+v", i, r.Type, r.Fields["name"].S, r.Ensure, w)
		}
	}
}

// TestEmptyFileGeneratesOneLine: no record gives one empty line (M4).
func TestEmptyFileGeneratesOneLine(t *testing.T) {
	got, err := testClass().Grammar.ToFile(nil)
	if err != nil || got != "\n" {
		t.Errorf("ToFile(nil) = %q, %v", got, err)
	}
}

// TestM13LineBreaksAreWrittenAndReparsed: a name or a field holding a line
// break is written verbatim, and the next prefetch reads the pieces as records
// of their own, which regenerate as results-misc.md M13 measured.
func TestM13LineBreaksAreWrittenAndReparsed(t *testing.T) {
	g := testClass().Grammar
	line, err := g.ToLine(parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{
		"device": str("/dev/x"), "name": str("/mnt/a\n/mnt/b"), "fstype": str("ext4"), "options": str("defaults"), "dump": str("0"), "pass": str("0"),
	}})
	if want := "/dev/x\t/mnt/a\n/mnt/b\text4\tdefaults\t0\t0"; err != nil || line != want {
		t.Fatalf("ToLine = %q, %v; want %q", line, err, want)
	}
	recs, err := g.Parse(line)
	if err != nil || len(recs) != 2 {
		t.Fatalf("Parse = %v, %v", recs, err)
	}
	got, err := g.ToFile(recs)
	if want := "/dev/x\t/mnt/a\t\t\t\t\n/mnt/b\text4\tdefaults\t0\t0\t\n"; err != nil || got != want {
		t.Errorf("ToFile = %q, %v; want %q", got, err, want)
	}
}
