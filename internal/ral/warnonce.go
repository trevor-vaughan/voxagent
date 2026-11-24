package ral

import (
	"context"
	"strings"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
	"github.com/voxagent/voxagent/internal/runlog"
)

// uniqueWarnings is Ruby's $unique_warnings (util/logging.rb:184): the keys
// warn_once has logged. Ruby's is process-global, and its agent forks a child
// for each run (agent.rb:69), so it lasts one run. voxagent's agent runs in one
// process, so the set lives on the context Transaction.Run builds for the run.
type uniqueWarnings map[string]struct{}

type uniqueWarningsKey struct{}

// WithUniqueWarnings returns ctx carrying a new, empty warn-once set.
func WithUniqueWarnings(ctx context.Context) context.Context {
	return context.WithValue(ctx, uniqueWarningsKey{}, uniqueWarnings{})
}

// warnOnce is Puppet.warn_once (util/logging.rb:181-201): message at warning
// from Puppet, the location of file and line on a second line, unless key has
// been logged this run or the run has logged 100 keys. A context without a set
// (a provider called outside a run) logs every time.
func warnOnce(ctx context.Context, key, message, file string, line int) {
	if seen, ok := ctx.Value(uniqueWarningsKey{}).(uniqueWarnings); ok {
		if _, dup := seen[key]; dup || len(seen) >= 100 {
			return
		}
		seen[key] = struct{}{}
	}
	location := strings.TrimPrefix(rubyvalue.ErrorLocation(file, line), " ")
	if location == "" {
		location = "(file & line not available)"
	}
	runlog.From(ctx).Log("warning", "Puppet", message+"\n   "+location)
}
