package ral

import (
	"context"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

func TestNotifyAlwaysReportsAChange(t *testing.T) {
	// Measured against Puppet 8.10.0: a notify resource logs
	//   Notify[hello]/message: defined 'message' as 'from the catalog'
	// and counts as a change on every run. Notify has no state to be in sync
	// with, so it is never idempotent -- that is Puppet's behaviour, not a
	// defect, and the idempotence gate must therefore never assert zero changes
	// on a catalog containing one.
	r := catalog.Resource{Type: "Notify", Title: "hello",
		Parameters: map[string]any{"message": "from the catalog"}}
	changes, _, err := Notify{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("Apply returned %d changes, want 1", len(changes))
	}
	if changes[0].Property != "message" {
		t.Errorf("Property = %q, want %q", changes[0].Property, "message")
	}
	if got, want := changes[0].Message, "defined 'message' as 'from the catalog'"; got != want {
		t.Errorf("Message = %q, want %q", got, want)
	}
}

// A notify with no message parameter uses its title, which is what Puppet does.
func TestNotifyFallsBackToTitle(t *testing.T) {
	r := catalog.Resource{Type: "Notify", Title: "bare"}
	changes, _, err := Notify{}.Apply(context.Background(), r, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if got, want := changes[0].Is, "bare"; got != want {
		t.Errorf("Is = %v, want %v", got, want)
	}
}

func TestNotifyNoopStillReportsTheChange(t *testing.T) {
	// A noop run must still report what it would have done. The transaction,
	// not the provider, decides that a noop change increments out_of_sync_count
	// rather than change_count.
	r := catalog.Resource{Type: "Notify", Title: "hello",
		Parameters: map[string]any{"message": "x"}}
	changes, _, err := Notify{}.Apply(context.Background(), r, true)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("noop Apply returned %d changes, want 1", len(changes))
	}
}

func TestNotifyRefreshIsANoOp(t *testing.T) {
	refreshed, err := Notify{}.Refresh(context.Background(),
		catalog.Resource{Type: "Notify", Title: "x"}, false)
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if refreshed {
		t.Error("Refresh reported a refresh; notify has nothing to refresh")
	}
}

// A non-string message must render the way Puppet renders it, not as a Go
// struct dump. Catalog parameters arrive as any, so an integer or a bool is
// ordinary.
func TestNotifyRendersNonStringMessages(t *testing.T) {
	for _, tt := range []struct {
		name string
		msg  any
		want string
	}{
		{"integer", float64(8080), "defined 'message' as '8080'"},
		{"bool", true, "defined 'message' as 'true'"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := catalog.Resource{Type: "Notify", Title: "x",
				Parameters: map[string]any{"message": tt.msg}}
			changes, _, err := Notify{}.Apply(context.Background(), r, false)
			if err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			if changes[0].Message != tt.want {
				t.Errorf("Message = %q, want %q", changes[0].Message, tt.want)
			}
		})
	}
}

// Every attribute a provider declares must be one its Apply actually reads.
//
// withpath was declared and never read. Pre-flight would accept a catalog
// setting it, the run would report success, and nothing would have honoured it
// -- a green run that quietly did less than the catalog asked. That is not a
// gap a behavioural test can see, because there is no behaviour to assert on;
// it needs an explicit statement of the set.
func TestNotifyDeclaresOnlyWhatItReads(t *testing.T) {
	got := Notify{}.Attrs().Sorted()
	want := []string{"message"}
	if len(got) != len(want) {
		t.Fatalf("Attrs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Attrs() = %v, want %v", got, want)
		}
	}
}

// And the consequence: a catalog using withpath is refused rather than silently
// under-applied.
func TestNotifyWithpathIsRefusedByPreflight(t *testing.T) {
	c := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "Notify", Title: "hi", Parameters: map[string]any{
			"message": "yo", "withpath": true}},
	}}
	gaps := Preflight(NewRegistry(Notify{}), c)
	if len(gaps) != 1 || gaps[0].Attribute != "withpath" {
		t.Fatalf("Preflight returned %v, want one gap naming withpath", gaps)
	}
}

// A notify's event reports a previous value of "absent", not nothing.
//
// Measured: Ruby's event for Notify[spine] carries previous_value 'absent'.
// property.rb change_to_s branches on `current_value == :absent` to produce
// "defined 'message' as ...", so absent is the value Puppet holds there rather
// than a stand-in for nil. Emitting null instead is a different JSON type for
// every notify event in every report.
func TestNotifyPreviousValueIsAbsent(t *testing.T) {
	changes, _, err := Notify{}.Apply(context.Background(),
		catalog.Resource{Type: "Notify", Title: "n",
			Parameters: map[string]any{"message": "hello"}}, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if changes[0].Was != "absent" {
		t.Errorf("Was = %#v, want \"absent\"", changes[0].Was)
	}
}

// Notify echoes its message into the run log through the global logger, so the
// entry's source is "Puppet" rather than the resource's own path.
//
// Measured: a Ruby run over Notify[spine] logs TWO notices -- one for the
// property change, sourced at the resource path, and one carrying just the
// message, sourced "Puppet". type/notify.rb's message property calls
// Puppet.send(loglevel, message) for exactly this. Without it a notify's whole
// point -- putting the text in the log -- happens only incidentally.
func TestNotifyEchoesItsMessageToTheRunLog(t *testing.T) {
	changes, _, err := Notify{}.Apply(context.Background(),
		catalog.Resource{Type: "Notify", Title: "n",
			Parameters: map[string]any{"message": "constant text"}}, false)
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if changes[0].Notice != "constant text" {
		t.Errorf("Notice = %q, want %q", changes[0].Notice, "constant text")
	}
}

// type/notify.rb defines no refresh, so event_manager.rb:73 queues no event
// for a Notify a changed resource notifies.
func TestNotifyIsNotRefreshable(t *testing.T) {
	if (Notify{}).Refreshable(catalog.Resource{Type: "Notify", Title: "n"}) {
		t.Error("Notify is refreshable; Puppet's notify type has no refresh")
	}
}

// A marked message is logged as Ruby does (type/notify.rb:15): the bare
// notice is "Sensitive [value redacted]". An unmarked one keeps the message.
func TestNotifyMarkedMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marked []string
		want   string
	}{
		{"marked", []string{"message"}, "Sensitive [value redacted]"},
		{"unmarked", nil, "hush-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := catalog.Resource{Type: "Notify", Title: "n",
				Parameters:          map[string]any{"message": "hush-secret"},
				SensitiveParameters: tc.marked}
			changes, _, err := Notify{}.Apply(context.Background(), r, false)
			if err != nil {
				t.Fatalf("Apply returned error: %v", err)
			}
			if got := changes[0].Notice; got != tc.want {
				t.Errorf("Notice = %q, want %q", got, tc.want)
			}
		})
	}
}
