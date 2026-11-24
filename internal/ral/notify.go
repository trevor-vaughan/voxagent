package ral

import (
	"context"
	"fmt"

	"github.com/voxagent/voxagent/internal/catalog"
)

// Notify is the Tier 1 notify provider.
//
// Notify has no state on the node to compare against, so it reports a change on
// every run. That is Puppet's behaviour, measured, and not a defect: it means a
// catalog containing a notify is never idempotent in the change-count sense,
// which the idempotence gate has to account for rather than work around.
type Notify struct{}

// Type implements Provider.
func (Notify) Type() string { return "notify" }

// Measured: notify is implemented on the type itself and has no provider, so
// Ruby reports provider_used as null rather than as a name.
func (Notify) ProviderUsed() *string { return nil }

// AutoRequires implements Provider. A notify touches nothing on the node, so
// there is nothing for it to be ordered after. Puppet's notify type declares no
// autorelation either.
func (Notify) AutoRequires(catalog.Resource, func(string) bool) []string { return nil }

// Attrs implements Provider.
//
// withpath is deliberately NOT declared. It was, and nothing read it: pre-flight
// would accept a catalog setting it, the run would report success, and the
// attribute would have had no effect -- a green run that quietly did less than
// the catalog asked. Declaring an attribute is a promise to honour it, so the
// set holds only what Apply actually reads.
//
// Refusing is the honest alternative. A catalog using withpath now gets a
// pre-flight refusal naming it, which is visible and actionable, rather than
// silence. If it turns out to be common enough to matter, implement it -- the
// refusal list is precisely the measurement that would say so.
func (Notify) Attrs() Attrs { return NewAttrs("message") }

// Properties implements PropertyDeclaring. type/notify.rb's only property is
// message (newproperty(:message)); name is its namevar parameter.
func (Notify) Properties() Attrs { return NewAttrs("message") }

// Apply implements Provider. It performs no I/O; the transaction turns the
// returned change into a log entry and a report event.
//
// It returns no warnings. type/notify.rb has no condition it warns about, and
// inventing one would put a line in the run log Ruby does not write.
func (n Notify) Apply(_ context.Context, r catalog.Resource, noop bool) ([]Change, []Warning, error) {
	message := r.Title
	if v, ok := r.Parameters["message"]; ok {
		message = fmt.Sprintf("%v", v)
	}
	// A marked message logs as Ruby's bare notice (type/notify.rb:15). The
	// event's own message is redacted by the transaction.
	notice := message
	if isMarked(n, r, "message") {
		notice = "Sensitive [value redacted]"
	}
	suffix := ""
	if noop {
		// resource_harness.rb's noop: change_to_s + " (noop)", and message's
		// sync, which logs the bare notice, never runs.
		notice, suffix = "", " (noop)"
	}
	return []Change{{
		Property: "message",
		// "absent", not nil. property.rb change_to_s branches on
		// `current_value == :absent` to produce the "defined ... as ..." wording
		// this message uses, so absent is the value Puppet holds rather than a
		// stand-in for nothing -- and null would be a different JSON type in
		// every notify event of every report.
		Was:     "absent",
		Is:      message,
		Message: fmt.Sprintf("defined 'message' as '%s'", message) + suffix,
		Notice:  notice,
	}}, nil, nil
}

// Refresh implements Provider. Notify has nothing to refresh.
func (Notify) Refresh(context.Context, catalog.Resource, bool) (bool, error) {
	return false, nil
}

// Refreshable implements Refreshing. type/notify.rb defines no refresh, so
// Puppet queues no event for a notified Notify.
func (Notify) Refreshable(catalog.Resource) bool { return false }

// Prefetch implements Provider. A notify has no state at all.
func (p Notify) Prefetch(context.Context, []catalog.Resource) (Provider, error) { return p, nil }
