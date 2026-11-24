package reportcmp

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/voxagent/voxagent/internal/gatekit/pycompat"
)

// The comparator's regexes, ported from Python's re. Two rules make them say
// what the originals said, and testdata/regex.json, measured against the
// originals, holds both to it:
//
//   - Python's \d is any Unicode decimal digit; RE2's is ASCII. \p{Nd} is
//     Python's.
//   - Python's $, without MULTILINE, also matches just before a final "\n".
//     RE2's matches only at the end, so it is spelled \n?\z, and where the
//     matched text is used the newline is kept outside the capture.
var (
	// The run-summary notice embeds a duration. Same class of variance as a
	// timestamp, but inside a message rather than in a field of its own, so no
	// path-shaped allowlist entry can reach it.
	appliedCatalog = regexp.MustCompile(`^Applied catalog in \p{Nd}+\.\p{Nd}+ seconds\n?\z`)

	// Read from transaction/resource_harness.rb:153: a failed change's message
	// is "change from <is> to <should> failed: " followed by the exception's
	// own to_s.
	failurePrefix = regexp.MustCompile(`^change from .* to .* failed: `)

	// Read from property.rb:493: a non-Puppet::Error raised while setting a
	// property is wrapped in a ResourceError reading "Could not set '<value>'
	// on <property>: <detail>", and error.rb:45 appends the resource's
	// manifest location to it.
	setWrapper = regexp.MustCompile(`^Could not set '[^']*' on [^:]*: `)

	// Read from util/execution.rb:312: a command that exits non-zero raises
	// Puppet::ExecutionFailure reading "Execution of '<command>' returned
	// <status>: <output>". The command and the status are argv and an integer,
	// reproducible between two runs and between the two implementations; the
	// output is the program's own text, which for dnf embeds a wall clock
	// ("Last metadata expiration check: 0:00:03 ago on Tue Sep 22 20:29:48
	// 2026") and so differs between two RUBY runs.
	//
	// It may follow "Could not update: ", which type/package.rb's
	// newvalue(:latest) and newvalue(/./) prepend when re-raising. Without it
	// the Puppet-sourced log of a failed `latest` or versioned install kept
	// dnf's download speeds compared -- measured: 2 divergent leaves between
	// Ruby and voxagent for the same failing `dnf -y downgrade`, differing only
	// in "21 MB/s" against "3.7 MB/s". The prefix stays compared; only the
	// output after it is masked.
	executionWrapper = regexp.MustCompile(`^(?:Could not update: )?Execution of '[^']*' returned \p{Nd}+: `)

	// The command inside the ExecutionFailure wrapper, which is all
	// maskValidateTempNames looks at. Puppet interpolates the command between
	// single quotes without escaping it, so a command that itself holds a
	// single quote does not match: its temp name stays unmasked and the
	// leaf diverges between any two runs. That fails loud, never as a false
	// agreement; G5's validators hold no quote.
	executedCommand = regexp.MustCompile(`Execution of '([^']*)' returned `)

	// Read from file_system/uniquefile.rb:102-116: the temp file a File's
	// validate_cmd is run against is "<base><YYYYMMDD>-<pid>-<base36 rand>",
	// with "-<n>" on a retry, and its path is substituted into the command.
	// Group 1 is the directory and base the name was made from.
	validateTempName = regexp.MustCompile(`(/[^/\s]*?)[0-9]{8}-[0-9]+-[0-9a-z]+(?:-[0-9]+)?\b`)

	// Read from util/errors.rb error_location_with_space, its three non-empty
	// forms. Group 1 is the location without the final newline Python's $
	// would have let it precede.
	errorLocation = regexp.MustCompile(`( \((?:file: [^)]*, line: \p{Nd}+|file: [^)]*|line: \p{Nd}+)\))\n?\z`)
)

// Read from util/logging.rb format_exception: an exception that answers
// `original` has its cause appended under this line.
const wrappedException = "\nWrapped exception:\n"

// with returns a shallow copy of m with key set to value, leaving m untouched,
// as Python's dict(m, key=value) does.
func with(m map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

// normaliseMetrics re-keys each metric group's values from positional
// [name, label, value] triples to names.
//
// Puppet writes every metric group as a list of triples, and voxagent matches
// that encoding. Flattened positionally, a metric's path is its INDEX: an
// allowlist could then only address a value by index, and if the two
// implementations emit different metric SETS, every index after the first
// difference misaligns, so the differential reports a landslide of mismatched
// pairs instead of the one field that actually differs. Re-keyed, a metric's
// path is its name, an absent or extra metric shows up as an absent path, and
// label and value are separately addressable.
//
// A group whose shape is not a list of triples is passed through untouched, so
// a malformed report diverges visibly rather than stopping the comparison. A
// triple whose name is not a string is refused: Python would have keyed the
// group by that value's str(), merging true with 1, which no report needs. A
// name that repeats within a group is refused too: re-keying would silently
// keep the last, hiding a metric emitted twice.
func normaliseMetrics(doc map[string]any) (map[string]any, error) {
	metrics, ok := doc["metrics"].(map[string]any)
	if !ok {
		return doc, nil
	}
	rekeyed := make(map[string]any, len(metrics))
	for group, body := range metrics {
		bodyMap, isMap := body.(map[string]any)
		values, isList := []any(nil), false
		if isMap {
			values, isList = bodyMap["values"].([]any)
		}
		if !isList || !allTriples(values) {
			rekeyed[group] = body
			continue
		}
		named := make(map[string]any, len(values))
		for _, v := range values {
			triple := v.([]any)
			name, ok := triple[0].(string)
			if !ok {
				return nil, fmt.Errorf("metrics.%s holds a metric whose name is %s, not a string", group, pycompat.Shape(triple[0]))
			}
			if _, repeated := named[name]; repeated {
				return nil, fmt.Errorf("metrics.%s holds the metric %s more than once", group, strconv.Quote(name))
			}
			named[name] = map[string]any{"label": triple[1], "value": triple[2]}
		}
		rekeyed[group] = with(bodyMap, "values", named)
	}
	return with(doc, "metrics", rekeyed), nil
}

func allTriples(values []any) bool {
	for _, v := range values {
		triple, ok := v.([]any)
		if !ok || len(triple) != 3 {
			return false
		}
	}
	return true
}

// mapLogs rewrites the message of each log entry for which rewrite returns
// true, leaving every other entry -- including ones that are not objects --
// as it was. A document whose logs are not a list is returned untouched.
func mapLogs(doc map[string]any, rewrite func(entry map[string]any, message string) (string, bool)) map[string]any {
	logs, ok := doc["logs"].([]any)
	if !ok {
		return doc
	}
	masked := make([]any, len(logs))
	for i, e := range logs {
		masked[i] = e
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		message, ok := entry["message"].(string)
		if !ok {
			continue
		}
		if replacement, ok := rewrite(entry, message); ok {
			masked[i] = with(entry, "message", replacement)
		}
	}
	return with(doc, "logs", masked)
}

// normaliseLogs masks the duration in the run-summary log message.
//
// Measured: two runs of the same implementation produce "Applied catalog in
// 0.01 seconds" and "Applied catalog in 0.00 seconds". The number is masked
// rather than the whole entry dropped, so the line's presence, its source and
// the rest of its wording are still asserted.
func normaliseLogs(doc map[string]any) map[string]any {
	return mapLogs(doc, func(_ map[string]any, message string) (string, bool) {
		if appliedCatalog.MatchString(message) {
			return "Applied catalog in <duration> seconds", true
		}
		return "", false
	})
}

// elideFailureDetail masks the underlying error's own text inside a failure
// message.
//
// Measured, 2026-09-19, creating a file under a directory that does not
// exist. Ruby's detail names the temp lockfile its file provider writes,
// carrying a date, a time and a random token, so it differs between two RUBY
// runs. Go's runtime renders the same errno differently again.
//
// Everything around the detail is reproducible and stays compared: the
// "change from X to Y failed: " prefix carries the property and its
// transition, the "Could not set 'V' on P: " wrapper carries the value being
// set, the "Execution of 'C' returned N: " wrapper carries argv and the exit
// status, and the " (file: F, line: N)" suffix carries the manifest location.
// Those ARE the attribution the gate exists to check.
//
// The span is bounded on both sides. A message that begins with none of the
// three is returned untouched, so nothing unrecognised is masked by accident.
func elideFailureDetail(message string) string {
	head, rest := "", message
	prefix := failurePrefix.FindStringIndex(message)
	if prefix != nil {
		head, rest = message[:prefix[1]], message[prefix[1]:]
	}
	wrapper := setWrapper.FindStringIndex(rest)
	if wrapper == nil {
		wrapper = executionWrapper.FindStringIndex(rest)
	}
	switch {
	case wrapper != nil:
		head += rest[:wrapper[1]]
		rest = rest[wrapper[1]:]
	case prefix == nil:
		return message
	}
	location := ""
	if m := errorLocation.FindStringSubmatch(rest); m != nil {
		location = m[1]
	}
	return head + "<detail>" + location
}

// maskValidateTempNames masks the date, pid and random token of a
// validate_cmd temp file's name inside an ExecutionFailure's command.
//
// Measured: a failing validate_cmd's message is
// "Execution of '/tmp/fail.sh /tmp/t/f20261007-625-1c0wrau' returned 1: ...",
// whose name carries the agent's pid and a random number, so it differs
// between two RUBY runs. The directory and base name stay compared, and so does
// the shape: a temp file named any other way is not masked and diverges.
func maskValidateTempNames(message string) string {
	return executedCommand.ReplaceAllStringFunc(message, func(wrapper string) string {
		return validateTempName.ReplaceAllString(wrapper, "${1}<YYYYMMDD-pid-rand>")
	})
}

// normaliseFailureDetails applies elideFailureDetail and maskValidateTempNames
// to failure events and to their logs.
//
// Events are selected by status "failure" and logs by level "err" -- on what
// the report says happened, never on what the message looks like, so a success
// message can never be touched. Read from transaction/event.rb:164, an event
// logs at :err exactly when its status is "failure", so the two selectors are
// one. Both places are normalised because a failed change is recorded twice.
//
// A report whose shape is not the expected one is passed through untouched,
// so it diverges visibly rather than stopping the comparison.
func normaliseFailureDetails(doc map[string]any) map[string]any {
	if statuses, ok := doc["resource_statuses"].(map[string]any); ok {
		rewritten := make(map[string]any, len(statuses))
		for ref, s := range statuses {
			rewritten[ref] = s
			status, ok := s.(map[string]any)
			if !ok {
				continue
			}
			events, ok := status["events"].([]any)
			if !ok {
				continue
			}
			masked := make([]any, len(events))
			for i, e := range events {
				masked[i] = e
				event, ok := e.(map[string]any)
				if !ok {
					continue
				}
				message, isString := event["message"].(string)
				if outcome, _ := event["status"].(string); isString && outcome == "failure" {
					masked[i] = with(event, "message", maskValidateTempNames(elideFailureDetail(message)))
				}
			}
			rewritten[ref] = with(status, "events", masked)
		}
		doc = with(doc, "resource_statuses", rewritten)
	}
	return mapLogs(doc, func(entry map[string]any, message string) (string, bool) {
		if level, _ := entry["level"].(string); level == "err" {
			return maskValidateTempNames(elideFailureDetail(message)), true
		}
		return "", false
	})
}

// dropWrappedExceptionLogs drops Ruby's exception-chain log, which has no Go
// analogue.
//
// Read from transaction/resource_harness.rb:149: the rescue logs the
// ResourceError itself, so util/logging.rb's format_exception appends
// "Wrapped exception:" and the original Errno's message under it. voxagent
// reproduces the OTHER Puppet-sourced log -- property.rb:496's bare wrapper --
// but not this one.
//
// Called on the Ruby document only, deliberately: dropping it from both sides
// would also hide a "Wrapped exception:" log that voxagent emitted and Ruby
// did not. It must run BEFORE the elision, which would consume the line that
// identifies the entry.
func dropWrappedExceptionLogs(doc map[string]any) map[string]any {
	logs, ok := doc["logs"].([]any)
	if !ok {
		return doc
	}
	kept := make([]any, 0, len(logs))
	for _, e := range logs {
		if entry, ok := e.(map[string]any); ok {
			if message, ok := entry["message"].(string); ok && strings.Contains(message, wrappedException) {
				continue
			}
		}
		kept = append(kept, e)
	}
	return with(doc, "logs", kept)
}

// dropCachingCatalogLog drops the line Ruby's agent logs when it writes the
// catalog it received to its cache (configurer.rb:495), which voxagent never
// does (D-67): no catalog cache, no fallback to one.
//
// Exact on every field -- source "Puppet", level info, message "Caching
// catalog for " and the report's own host -- so any other wording still
// diverges. Called on the Ruby document only, like dropWrappedExceptionLogs:
// the same line from voxagent would be a claim it caches, and must surface.
func dropCachingCatalogLog(doc map[string]any) map[string]any {
	logs, ok := doc["logs"].([]any)
	host, named := doc["host"].(string)
	if !ok || !named {
		return doc
	}
	kept := make([]any, 0, len(logs))
	for _, e := range logs {
		if entry, ok := e.(map[string]any); ok && entry["source"] == "Puppet" && entry["level"] == "info" &&
			entry["message"] == "Caching catalog for "+host {
			continue
		}
		kept = append(kept, e)
	}
	return with(doc, "logs", kept)
}
