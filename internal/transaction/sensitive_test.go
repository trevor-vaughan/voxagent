package transaction

import (
	"context"
	"errors"
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
