package transaction

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
)

// markedProvider is a native provider that declares properties, so
// ral.MarkSensitive applies to its resources, and returns fixed changes.
type markedProvider struct {
	recorder
	result []ral.Change
	err    error
}

func (p markedProvider) Attrs() ral.Attrs      { return ral.NewAttrs("content", "mode", "owner") }
func (p markedProvider) Properties() ral.Attrs { return ral.NewAttrs("content", "mode", "owner") }
func (p markedProvider) Apply(context.Context, catalog.Resource, bool) ([]ral.Change, []ral.Warning, error) {
	return p.result, nil, p.err
}

// Prefetch hands back the receiver: the promoted recorder.Prefetch would return
// a bare recorder and lose the overrides.
func (p markedProvider) Prefetch(context.Context, []catalog.Resource) (ral.Provider, error) {
	return p, nil
}

func runMarked(t *testing.T, noop bool, p markedProvider, marked ...string) *report.Report {
	t.Helper()
	p.typ = "file"
	tx := &Transaction{Registry: ral.NewRegistry(p), Noop: noop}
	r := res("File", "/f", map[string]any{"content": "x", "mode": "0600", "owner": 0})
	r.SensitiveParameters = marked
	return runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{r}})
}

func logAt(rep *report.Report, level, source string) *report.LogEntry {
	for i := range rep.Logs {
		if rep.Logs[i].Level == level && rep.Logs[i].Source == source {
			return &rep.Logs[i]
		}
	}
	return nil
}

// A nil previous value stays nil (resource_harness.rb:183-184) while the
// message still redacts both sides.
func TestMarkedPropertyKeepsNilPreviousValue(t *testing.T) {
	rep := runMarked(t, false, markedProvider{result: []ral.Change{
		{Property: "content", Was: nil, Is: "{sha256}b", Message: "content changed 'absent' to '{sha256}b'"},
	}}, "content")
	evs := rep.ResourceStatuses["File[/f]"].Events
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %+v", evs)
	}
	e := evs[0]
	if e.PreviousValue != nil || e.DesiredValue != "[redacted]" || e.Redacted != true {
		t.Errorf("event = %+v, want nil previous, redacted desired, Redacted true", e)
	}
	if want := "changed [redacted] to [redacted]"; e.Message != want {
		t.Errorf("message = %q, want %q", e.Message, want)
	}
}

func TestMarkedPropertyEventIsRedacted(t *testing.T) {
	rep := runMarked(t, false, markedProvider{result: []ral.Change{
		{Property: "content", Was: "{sha256}a", Is: "{sha256}b", Message: "content changed '{sha256}a' to '{sha256}b'"},
		{Property: "mode", Was: "0644", Is: "0600", Message: "mode changed '0644' to '0600'"},
	}}, "content")
	evs := rep.ResourceStatuses["File[/f]"].Events
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %+v", evs)
	}
	m := evs[0]
	if m.PreviousValue != "[redacted]" || m.DesiredValue != "[redacted]" || m.Redacted != true {
		t.Errorf("marked event = %+v, want redacted values and Redacted true", m)
	}
	if want := "changed [redacted] to [redacted]"; m.Message != want {
		t.Errorf("message = %q, want %q", m.Message, want)
	}
	l := logAt(rep, "notice", "/File[/f]/content")
	if l == nil || l.Message != "changed [redacted] to [redacted]" {
		t.Errorf("content log = %+v", l)
	}
	u := evs[1]
	if u.PreviousValue != "0644" || u.DesiredValue != "0600" || u.Redacted != nil ||
		u.Message != "mode changed '0644' to '0600'" {
		t.Errorf("unmarked event changed: %+v", u)
	}
	if l := logAt(rep, "notice", "/File[/f]/mode"); l == nil || l.Message != u.Message {
		t.Errorf("mode log = %+v", l)
	}
}

func TestMarkedPropertyNoopEventIsRedacted(t *testing.T) {
	rep := runMarked(t, true, markedProvider{result: []ral.Change{
		{Property: "content", Was: "a", Is: "b", Message: "current_value 'a', should be 'b' (noop)"},
		{Property: "mode", Was: "0644", Is: "0600", Message: "current_value '0644', should be '0600' (noop)"},
	}}, "content")
	evs := rep.ResourceStatuses["File[/f]"].Events
	want := "current_value [redacted], should be [redacted] (noop)"
	if evs[0].Message != want || evs[0].Status != "noop" || evs[0].Redacted != true {
		t.Errorf("marked noop event = %+v", evs[0])
	}
	if l := logAt(rep, "notice", "/File[/f]/content"); l == nil || l.Message != want {
		t.Errorf("content log = %+v", l)
	}
	if evs[1].Message != "current_value '0644', should be '0600' (noop)" || evs[1].Redacted != nil {
		t.Errorf("unmarked noop event changed: %+v", evs[1])
	}
}

func TestMarkedPropertyFailureIsRedacted(t *testing.T) {
	rep := runMarked(t, false, markedProvider{result: []ral.Change{
		{Property: "content", Was: "a", Is: "b", Err: errors.New("boom")},
		{Property: "mode", Was: "0644", Is: "0600", Err: errors.New("bang")},
	}}, "content")
	evs := rep.ResourceStatuses["File[/f]"].Events
	want := "change from [redacted] to [redacted] failed: boom"
	if evs[0].Message != want || evs[0].Status != "failure" || evs[0].Redacted != true ||
		evs[0].PreviousValue != "[redacted]" || evs[0].DesiredValue != "[redacted]" {
		t.Errorf("marked failure = %+v", evs[0])
	}
	if l := logAt(rep, "err", "/File[/f]/content"); l == nil || l.Message != want {
		t.Errorf("content err log = %+v", l)
	}
	if l := logAt(rep, "err", "Puppet"); l == nil || l.Message != "boom" {
		t.Errorf("Puppet err log = %+v, want boom", l)
	}
	if evs[1].Message != "change from '0644' to '0600' failed: bang" || evs[1].Redacted != nil ||
		evs[1].PreviousValue != "0644" {
		t.Errorf("unmarked failure changed: %+v", evs[1])
	}
}

// Review Focus 2: a non-string value is replaced by the string, not by nil.
func TestMarkedPropertyNonStringValuesBecomeStrings(t *testing.T) {
	rep := runMarked(t, false, markedProvider{result: []ral.Change{
		{Property: "mode", Was: 0644.0, Is: 1000.0, Message: "mode changed 420 to 1000"},
	}}, "mode")
	ev := rep.ResourceStatuses["File[/f]"].Events[0]
	if ev.PreviousValue != "[redacted]" || ev.DesiredValue != "[redacted]" || ev.Redacted != true {
		t.Errorf("event = %+v", ev)
	}
}

// Review Focus 3: a failure with no property is not a property event.
func TestMarkedResourceApplyErrorKeepsItsEvent(t *testing.T) {
	rep := runMarked(t, false, markedProvider{err: errors.New("provider refused")}, "content")
	evs := rep.ResourceStatuses["File[/f]"].Events
	if len(evs) != 1 || evs[0].Redacted != false || evs[0].Message == "" ||
		!strings.Contains(evs[0].Message, "provider refused") {
		t.Errorf("events = %+v", evs)
	}
}

// Notify supplies its own redacted notice; the transaction does not
// touch ch.Notice or the path notices.
func TestMarkedPropertyLeavesNoticesAlone(t *testing.T) {
	rep := runMarked(t, false, markedProvider{result: []ral.Change{
		{Property: "content", Was: "a", Is: "b", Message: "m", Notice: "own notice",
			PathNotices: []string{"path notice"}},
	}}, "content")
	if l := logAt(rep, "notice", "Puppet"); l == nil || l.Message != "own notice" {
		t.Errorf("Puppet notice = %+v", l)
	}
	if l := logAt(rep, "notice", "/File[/f]"); l == nil || l.Message != "path notice" {
		t.Errorf("path notice = %+v", l)
	}
}

// contentMarkRun applies a declared File through the real native provider
// after an eval_generate override carrying mark (design §5.4). existing is
// the file's prior content, or nil for a file that does not exist yet.
func contentMarkRun(t *testing.T, existing *string, mark ral.ContentMark, catalogMarked ...string) []report.Event {
	t.Helper()
	return contentMarkRunWith(t, map[string]any{"ensure": "file", "content": "declared"}, existing, mark, catalogMarked...)
}

// contentMarkRunWith is contentMarkRun for a File declared with params.
func contentMarkRunWith(t *testing.T, params map[string]any, existing *string, mark ral.ContentMark, catalogMarked ...string) []report.Event {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if existing != nil {
		if err := os.WriteFile(path, []byte(*existing), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ref := "File[" + path + "]"
	var calls []string
	stub := evalGenStub{evaluatorStub: evaluatorStub{typ: "vox_evalgen", calls: &calls},
		gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {Overrides: []ral.Override{{
			Ref: ref, Parameters: map[string]any{"content": "public\ns3cret-token\n"}, ContentMark: mark}}}}}
	file := res("File", path, params)
	file.SensitiveParameters = catalogMarked
	tx := &Transaction{Registry: ral.NewRegistry(stub, ral.File{})}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "p", map[string]any{"before": ref}), file}})
	return rep.ResourceStatuses[ref].Events
}

func eventFor(t *testing.T, evs []report.Event, property string) report.Event {
	t.Helper()
	for _, e := range evs {
		if e.Property == property {
			return e
		}
	}
	t.Fatalf("no %q event in %+v", property, evs)
	return report.Event{}
}

// v10-12c: Ruby sets the content flag directly, bypassing set_sensitive_
// parameters, so a new file's ensure event keeps its hash and redacted: null.
func TestContentMarkSensitiveDoesNotCascadeToEnsure(t *testing.T) {
	evs := contentMarkRun(t, nil, ral.ContentMarkSensitive)
	e := eventFor(t, evs, "ensure")
	want := "defined content as '{sha256}ee990bd4314b9f288249b8bf860abd636e5f4aa2aaea57926558ad7eb516fcf3'"
	if e.Message != want || e.Redacted != nil {
		t.Errorf("ensure event = %q redacted %v, want %q and nil", e.Message, e.Redacted, want)
	}

	old := "old\n"
	c := eventFor(t, contentMarkRun(t, &old, ral.ContentMarkSensitive), "content")
	if c.Message != "changed [redacted] to [redacted]" || c.PreviousValue != "[redacted]" ||
		c.DesiredValue != "[redacted]" || c.Redacted != true {
		t.Errorf("content event = %+v, want redacted values and Redacted true", c)
	}
}

// v10-07: a File with no fragments is explicitly not sensitive.
func TestContentMarkNotSensitiveGivesRedactedFalse(t *testing.T) {
	old := "old\n"
	c := eventFor(t, contentMarkRun(t, &old, ral.ContentMarkNotSensitive), "content")
	prefix := "content changed '{sha256}"
	if !strings.HasPrefix(c.Message, prefix) || strings.Contains(c.Message, "[redacted]") || c.Redacted != false {
		t.Errorf("content event = %q redacted %v, want the hash message and false", c.Message, c.Redacted)
	}
	if !strings.HasSuffix(c.Message, "' to '{sha256}ee990bd4314b9f288249b8bf860abd636e5f4aa2aaea57926558ad7eb516fcf3'") {
		t.Errorf("message = %q lacks the new content's hash", c.Message)
	}
}

// A declared File whose catalog marks content: Unset removes that mark, but
// the cascade the catalog already applied to ensure stays.
func TestContentMarkUnsetRemovesCatalogMark(t *testing.T) {
	old := "old\n"
	evs := contentMarkRun(t, &old, ral.ContentMarkUnset, "content")
	c := eventFor(t, evs, "content")
	if !strings.HasPrefix(c.Message, "content changed '{sha256}") || c.Redacted != nil {
		t.Errorf("content event = %q redacted %v, want the hash message and nil", c.Message, c.Redacted)
	}
	if got := contentMarkRun(t, nil, ral.ContentMarkUnset, "content"); eventFor(t, got, "ensure").Redacted != true {
		t.Errorf("ensure event lost its catalog cascade mark: %+v", got)
	}
}

func TestContentMarkKeepChangesNothing(t *testing.T) {
	old := "old\n"
	c := eventFor(t, contentMarkRun(t, &old, ral.ContentMarkKeep, "content"), "content")
	if c.Redacted != true || c.Message != "changed [redacted] to [redacted]" {
		t.Errorf("kept catalog mark: %+v", c)
	}
	c = eventFor(t, contentMarkRun(t, &old, ral.ContentMarkKeep), "content")
	if c.Redacted != nil || !strings.HasPrefix(c.Message, "content changed '{sha256}") {
		t.Errorf("unmarked keep: %+v", c)
	}
}

// Ruby's cascade marks ensure whether or not the catalog gave one
// (type/file.rb:989), so removing the content mark must keep it.
func TestContentMarkRemovalKeepsEnsureMarkWithoutAnEnsureParameter(t *testing.T) {
	for name, mark := range map[string]ral.ContentMark{"unset": ral.ContentMarkUnset, "not sensitive": ral.ContentMarkNotSensitive} {
		t.Run(name, func(t *testing.T) {
			evs := contentMarkRunWith(t, map[string]any{"content": "declared"}, nil, mark, "content")
			if e := eventFor(t, evs, "ensure"); e.Redacted != true {
				t.Errorf("ensure event = %q redacted %v, want the catalog cascade mark kept", e.Message, e.Redacted)
			}
		})
	}
}

// D-78 for a 9.1.0 concat: an override that redacts output without marking
// content leaves the event's hash and redacted null as Ruby reports them, and
// the failing validator's echo of the content out of its message.
func TestOverrideRedactOutputKeepsContentUnmarked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	ref := "File[" + path + "]"
	var calls []string
	stub := evalGenStub{evaluatorStub: evaluatorStub{typ: "vox_evalgen", calls: &calls},
		gens: map[string]ral.EvalGeneration{"Vox_evalgen[p]": {Overrides: []ral.Override{{
			Ref: ref, Parameters: map[string]any{"content": "s3cret-token\n"}, RedactOutput: true}}}}}
	tx := &Transaction{Registry: ral.NewRegistry(stub, ral.File{})}
	rep := runCatalog(t, tx, &catalog.Catalog{Resources: []catalog.Resource{
		res("Vox_evalgen", "p", map[string]any{"before": ref}),
		res("File", path, map[string]any{"ensure": "file", "validate_cmd": `/bin/sh -c 'cat "$0"; exit 3' %`})}})
	e := eventFor(t, rep.ResourceStatuses[ref].Events, "ensure")
	if e.Redacted != nil || !strings.HasSuffix(e.Message, "returned 3: [output redacted]") {
		t.Errorf("ensure event = %q redacted %v, want the output redacted and redacted nil", e.Message, e.Redacted)
	}
}
