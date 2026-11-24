package sshkeyscore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/parsedfile/vectorstest"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
)

func testSakClass() *parsedfile.Class {
	return sakClass(func(string) (parsedfile.FileType, error) { return nil, nil }, time.Now)
}

func logCtx(lines *[]string) context.Context {
	l := runlog.New("debug", func(e report.LogEntry) {
		*lines = append(*lines, e.Level+" "+e.Source+": "+e.Message)
	}, func(report.LogEntry) {})
	return runlog.WithLogger(context.Background(), l)
}

// prefetched parses text as the engine does for target: records marked on
// disk, then the class's PrefetchHook.
func prefetched(t *testing.T, ctx context.Context, c *parsedfile.Class, target, text string) []*parsedfile.Record {
	t.Helper()
	recs, err := c.Grammar.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		r.OnDisk, r.Target, r.Ensure = true, target, "present"
	}
	recs, err = c.PrefetchHook(ctx, target, recs)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestSakClassIdentity(t *testing.T) {
	c := testSakClass()
	if c.Name != "parsed" || c.TypeName != "ssh_authorized_key" || c.DefaultTarget != "" {
		t.Errorf("class = %q/%q default %q, want parsed/ssh_authorized_key and an empty default target", c.Name, c.TypeName, c.DefaultTarget)
	}
	if c.Grammar == nil || c.FileType == nil || c.Now == nil || c.PrefetchHook == nil {
		t.Errorf("Grammar, FileType, Now and PrefetchHook must be set: %+v", c)
	}
}

// TestSakGrammarMatchesSharedVectors runs every "sak" case of vectors.json,
// whose wants fileparsing_vectors.rb takes from the real engine and the
// provider's own definitions (kp:11-35, kt:169-174).
func TestSakGrammarMatchesSharedVectors(t *testing.T) {
	g := testSakClass().Grammar
	cases := vectorstest.Read(t, "sak")
	if len(cases) < 70 {
		t.Fatalf("only %d sak vectors", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name+"/"+c.Op, func(t *testing.T) { c.Check(t, g) })
	}
}

// ---- edges the vectors cannot hold --------------------------------------

func TestSakUnmatchedLineIsAParseError(t *testing.T) {
	g := testSakClass().Grammar
	for _, text := range []string{"\tssh-rsa K t", "garbage", "ssh-rsa K a\ngarbage"} {
		recs, err := g.Parse(text)
		var pe *parsedfile.ParseError
		if !errors.As(err, &pe) || recs != nil {
			t.Errorf("Parse(%q) = %v, %v; want a *ParseError", text, recs, err)
		}
	}
}

// TestSakPrefetchHookNamesUnnamedKeys: each parsed record with an empty name
// is named <target>:unnamed-<n>, n from 1 per target in file order; key_v1
// lines, whatever their name, are left alone (kp:126-137, M8 08a-existing).
func TestSakPrefetchHookNamesUnnamedKeys(t *testing.T) {
	var lines []string
	ctx := logCtx(&lines)
	c := testSakClass()
	text := "# c\nssh-rsa K1\nssh-rsa K2 named\n1024 35 777\nno-pty ssh-ed25519 K3 \n1024 35 8 legacy\nssh-dss K4"
	recs := prefetched(t, ctx, c, "/home/a/.ssh/authorized_keys", text)

	var names []string
	for _, r := range recs {
		if r.Type != "parsed" && r.Flags["unnamed"] {
			t.Errorf("%s record flagged unnamed", r.Type)
		}
		if r.Type == "parsed" {
			names = append(names, r.Fields["name"].S)
			if want := r.Fields["name"].S != "named"; r.Flags["unnamed"] != want {
				t.Errorf("%q: unnamed = %v, want %v", r.Fields["name"].S, r.Flags["unnamed"], want)
			}
		}
	}
	want := []string{
		"/home/a/.ssh/authorized_keys:unnamed-1", "named",
		"/home/a/.ssh/authorized_keys:unnamed-2", "/home/a/.ssh/authorized_keys:unnamed-3",
	}
	if len(names) != len(want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names = %q, want %q", names, want)
			break
		}
	}
	if recs[3].Type != "key_v1" || recs[3].Fields["name"].Kind != parsedfile.Nil {
		t.Errorf("the name-less key_v1 record changed: %+v", recs[3])
	}
	wantLog := []string{
		"debug Puppet: generating name for on-disk ssh_authorized_key K1: /home/a/.ssh/authorized_keys:unnamed-1",
		"debug Puppet: generating name for on-disk ssh_authorized_key K3: /home/a/.ssh/authorized_keys:unnamed-2",
		"debug Puppet: generating name for on-disk ssh_authorized_key K4: /home/a/.ssh/authorized_keys:unnamed-3",
	}
	if len(lines) != len(wantLog) {
		t.Fatalf("log = %q, want %q", lines, wantLog)
	}
	for i := range wantLog {
		if lines[i] != wantLog[i] {
			t.Errorf("log[%d] = %q, want %q", i, lines[i], wantLog[i])
		}
	}

	// The counter restarts for the next target.
	other := prefetched(t, ctx, c, "/b", "ssh-rsa K5")
	if got := other[0].Fields["name"].S; got != "/b:unnamed-1" {
		t.Errorf("second target's name = %q, want /b:unnamed-1", got)
	}
}

// TestSakUnnamedRecordIsWrittenWithoutItsGeneratedName: pre_gen blanks the
// name of a record the hook named, so the line is written back as read, with
// its trailing space (spec 8 item 8).
func TestSakUnnamedRecordIsWrittenWithoutItsGeneratedName(t *testing.T) {
	c := testSakClass()
	recs := prefetched(t, context.Background(), c, "/t", "ssh-rsa K\nno-pty ssh-ed25519 K2 n")
	got, err := c.Grammar.ToFile(recs)
	if want := "ssh-rsa K \nno-pty ssh-ed25519 K2 n\n"; err != nil || got != want {
		t.Fatalf("ToFile = %q, %v; want %q", got, err, want)
	}
	if recs[0].Fields["name"].S != "/t:unnamed-1" {
		t.Errorf("PreGen changed the caller's name to %q", recs[0].Fields["name"].S)
	}
}

func TestSakKeyV1RoundTripsUnchanged(t *testing.T) {
	c := testSakClass()
	const text = "from=\"x\" 1024 35 12345 legacy@host\n1024 35 1 a b\n"
	recs := prefetched(t, context.Background(), c, "/t", text)
	got, err := c.Grammar.ToFile(recs)
	if err != nil || got != text {
		t.Fatalf("ToFile = %q, %v; want %q", got, err, text)
	}
}

func TestSakNamelessKeyV1RefusesToWrite(t *testing.T) {
	c := testSakClass()
	recs := prefetched(t, context.Background(), c, "/t", "1024 35 777")
	if _, err := c.Grammar.ToFile(recs); err == nil || err.Error() != "Field 'name' is required" {
		t.Fatalf("ToFile error = %v, want Field 'name' is required", err)
	}
}

// TestSakPreGenOptionsShapes: [Absent] and [] write nothing, a list joins with
// ",", and the caller's record keeps its list.
func TestSakPreGenOptionsShapes(t *testing.T) {
	g := testSakClass().Grammar
	for _, tc := range []struct {
		name    string
		options parsedfile.Value
		want    string
	}{
		{"absent", parsedfile.Value{Kind: parsedfile.Absent}, "ssh-rsa K n"},
		{"empty", list(), "ssh-rsa K n"},
		{"two", list("no-pty", `command="x y"`), `no-pty,command="x y" ssh-rsa K n`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := parsedfile.Record{Type: "parsed", Fields: map[string]parsedfile.Value{
				"options": tc.options, "type": str("ssh-rsa"), "key": str("K"), "name": str("n"),
			}}
			got, err := g.ToLine(rec)
			if err != nil || got != tc.want {
				t.Errorf("ToLine = %q, %v; want %q", got, err, tc.want)
			}
			if rec.Fields["options"].Kind != tc.options.Kind {
				t.Errorf("PreGen changed the caller's options to %+v", rec.Fields["options"])
			}
		})
	}
}
