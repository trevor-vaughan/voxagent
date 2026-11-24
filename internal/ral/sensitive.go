package ral

import (
	"fmt"
	"slices"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/report"
)

// PropertyDeclaring is implemented by a native provider: the subset of Attrs
// that are Puppet properties of its type.
type PropertyDeclaring interface{ Properties() Attrs }

// MarkSensitive reproduces Puppet's set_sensitive_parameters
// (type.rb:2389-2402) for r: it returns the attributes the catalog's
// sensitive_parameters mark, and the console logs Puppet writes for a name it
// cannot mark. Logs carry no Time; they are never part of a report.
//
// The set holds attributes, not only properties: Exec's override marks command,
// unless and onlyif, which no Exec event carries. A provider that does not
// declare properties (every Evaluator) gets nil, nil: the extension host
// redacts its own resources.
func MarkSensitive(p Provider, r catalog.Resource) (Attrs, []report.LogEntry) {
	pd, ok := p.(PropertyDeclaring)
	if !ok {
		return nil, nil
	}
	props, attrs := pd.Properties(), p.Attrs()
	given := func(name string) bool { _, ok := r.Parameters[name]; return ok }
	marked := make(Attrs)
	var logs []report.LogEntry
	warn := func(level, msg string) {
		logs = append(logs, report.LogEntry{Level: level, Message: msg, Source: "/" + r.Ref()})
	}

	names := slices.Clone(r.SensitiveParameters)
	// handle removes name from names and reports whether it was there.
	handle := func(name string) bool {
		n := len(names)
		names = slices.DeleteFunc(names, func(s string) bool { return s == name })
		return len(names) != n
	}

	switch p.Type() {
	case "file":
		// type/file.rb:982-1005: marked content marks ensure too, and stays
		// listed for the generic rule; marked source stands for content and
		// ensure and is removed.
		if slices.Contains(names, "content") {
			marked["ensure"] = struct{}{}
		}
		if handle("source") {
			marked["content"], marked["ensure"] = struct{}{}, struct{}{}
		}
	case "exec":
		// type/exec.rb:707-728: any of the three marked marks every one given.
		guards := []string{"command", "unless", "onlyif"}
		anyMarked := false
		for _, g := range guards {
			anyMarked = handle(g) || anyMarked // handle every guard: no short-circuit
		}
		if anyMarked {
			for _, g := range guards {
				if given(g) {
					marked[g] = struct{}{}
				}
			}
		}
	case "user":
		// type/user.rb:280: password is a sensitive property whether or not
		// the catalog marks it (type.rb:2404-2410).
		if given("password") {
			marked["password"] = struct{}{}
		}
	}

	// A generator that set the flag directly marks only that attribute: no
	// cascade, since the cascade lives in set_sensitive_parameters (§5.4).
	for name, set := range r.SetSensitivity {
		if set {
			marked[name] = struct{}{}
		}
	}

	for _, name := range names {
		// An override already marked it: Ruby's newattr created the property.
		if _, done := marked[name]; done {
			continue
		}
		_, isProp := props[name]
		_, isAttr := attrs[name]
		switch {
		case isProp && given(name):
			marked[name] = struct{}{}
		case !isAttr && !IsEngineAttr(name):
			warn("err", fmt.Sprintf("Unable to mark '%s' as sensitive: the property itself is not defined on %s.", name, r.Type))
		case !given(name):
			warn("warning", fmt.Sprintf("Unable to mark '%s' as sensitive: the property itself was not assigned a value.", name))
		default:
			warn("warning", fmt.Sprintf("Unable to mark '%s' as sensitive: %s is a parameter and not a property, and cannot be automatically redacted.", name, name))
		}
	}
	return marked, logs
}

// isMarked reports whether MarkSensitive marks attr on r. A provider site
// that interpolates the attribute's value into error text asks this; it
// ignores the console logs, which the agent prints once at validation.
func isMarked(p Provider, r catalog.Resource, attr string) bool {
	marked, _ := MarkSensitive(p, r)
	_, ok := marked[attr]
	return ok
}

// redactIf renders a value as format would, or as [redacted] when marked. The
// replacement takes the format's quotes with it: 'x' becomes [redacted].
func redactIf(marked bool, format string, a ...any) string {
	if marked {
		return "[redacted]"
	}
	return fmt.Sprintf(format, a...)
}
