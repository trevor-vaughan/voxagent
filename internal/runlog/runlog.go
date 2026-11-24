// Package runlog carries a per-run logger on the context so any layer can emit
// Puppet-style log lines (chiefly debug) without threading a logger argument.
// A logger never fails a run: a nil *Logger is a valid no-op.
package runlog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/voxagent/voxagent/internal/report"
)

// reportDebugBudget caps the bytes of debug-level messages stored in the
// report. Debug lines past it go to the console only.
const reportDebugBudget = 16 << 20

const truncationWarning = "debug log truncated in the report after 16 MiB; the console has the rest"

// rank orders Puppet's log levels, lowest first. An unknown level ranks as
// debug, so it is dropped only by a logger that is itself above debug.
var rank = map[string]int{
	"debug": 0, "info": 1, "notice": 2, "warning": 3,
	"err": 4, "alert": 5, "emerg": 6, "crit": 7,
}

// Logger filters entries by level and routes them to the report side
// (record) or the console only (print). It is safe for concurrent use.
type Logger struct {
	min   int
	print func(report.LogEntry)

	mu            sync.Mutex
	record        func(report.LogEntry)
	recordedDebug int
	truncated     bool
}

// New returns a Logger that keeps entries at or above level ("notice", "info"
// or "debug"). record stores an entry in the report and, through the report
// observer or console buffer, prints it; print shows an entry on the console
// only. Either may be nil to discard that route.
func New(level string, record, print func(report.LogEntry)) *Logger {
	return &Logger{min: rank[level], record: record, print: print}
}

// Redirect replaces the record sink, keeping the debug budget already spent.
func (l *Logger) Redirect(record func(report.LogEntry)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.record = record
	l.mu.Unlock()
}

// Enabled reports whether entries at level would be kept. A nil Logger keeps
// nothing.
func (l *Logger) Enabled(level string) bool {
	return l != nil && rank[level] >= l.min
}

// Log emits an entry stamped with the current UTC time. See Entry for what is
// dropped and bounded.
func (l *Logger) Log(level, source, message string) {
	l.Entry(report.LogEntry{Level: level, Message: message, Source: source})
}

// Entry emits a built entry, keeping its Time, Source, File and Line; an empty
// Time is stamped with the current UTC time. Entries below the logger's level
// are dropped. A debug entry that would push the recorded debug bytes past
// reportDebugBudget is printed but not recorded, as is every debug entry after
// it; the first such entry is preceded by a recorded warning.
func (l *Logger) Entry(entry report.LogEntry) {
	if !l.Enabled(entry.Level) {
		return
	}
	if entry.Time == "" {
		entry.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if entry.Level == "debug" {
		if l.truncated || l.recordedDebug+len(entry.Message) > reportDebugBudget {
			if !l.truncated {
				l.truncated = true
				l.recordLocked(report.LogEntry{
					Level:   "warning",
					Message: truncationWarning,
					Source:  "voxagent",
					Time:    entry.Time,
				})
			}
			if l.print != nil {
				l.print(entry)
			}
			return
		}
		l.recordedDebug += len(entry.Message)
	}
	l.recordLocked(entry)
}

func (l *Logger) recordLocked(e report.LogEntry) {
	if l.record != nil {
		l.record(e)
	}
}

type ctxKey struct{}

// WithLogger returns ctx carrying l.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the Logger carried by ctx, or nil when there is none.
func From(ctx context.Context) *Logger {
	l, _ := ctx.Value(ctxKey{}).(*Logger)
	return l
}

// Debugf logs a debug entry from ctx's Logger. The message is formatted only
// when debug is enabled, so a disabled call costs no allocation.
func Debugf(ctx context.Context, source, format string, args ...any) {
	l := From(ctx)
	if !l.Enabled("debug") {
		return
	}
	l.Log("debug", source, fmt.Sprintf(format, args...))
}

// AddLogs records entries a host or provider already built, through ctx's
// Logger so the level and the debug budget apply to them as to voxagent's own
// lines; in a run the Logger's record sink is rep. A context with no Logger
// adds every entry to rep unchanged: a transaction used without one keeps what
// it is given.
func AddLogs(ctx context.Context, rep *report.Report, entries ...report.LogEntry) {
	l := From(ctx)
	if l == nil {
		rep.AddLog(entries...)
		return
	}
	for _, e := range entries {
		l.Entry(e)
	}
}
