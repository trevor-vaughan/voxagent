package exec

import (
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/lib/sources"
	"github.com/voxagent/voxagent/internal/report"
)

// Review Focus 3: the cut is on a character boundary, so logoutput, which
// decodes the output, does not fail on it.
func TestExecLogoutputTruncatedStaysDecodable(t *testing.T) {
	raltest.WithOutputCap(t, 4)
	setLocale(t, "C.UTF-8")
	ctx, got := raltest.DebugContext("notice")
	ctx = sources.WithSources(ctx, sources.Sources{Ref: "Exec[big]"})
	r := raltest.ExecRes("big", map[string]any{"command": `printf 'ab\342\202\254'`, "logoutput": true})
	changes, _, err := Exec{}.Apply(ctx, r, false)
	if err != nil || len(changes) != 1 {
		t.Fatalf("Apply = (%v, %v)", changes, err)
	}
	if changes[0].Err != nil {
		t.Fatalf("change failed: %v", changes[0].Err)
	}
	if want := []string{"ab"}; !slices.Equal(changes[0].PropertyNotices, want) {
		t.Errorf("PropertyNotices = %q, want %q", changes[0].PropertyNotices, want)
	}
	raltest.AssertWarnings(t, *got, []report.LogEntry{{Level: "warning", Source: "voxagent",
		Message: "Exec[big]: output of 'printf 'ab\\342\\202\\254'' truncated at 64 MiB"}})
}

// A Sensitive command stays redacted in the warning.
func TestExecTruncationWarningRedactsASensitiveCommand(t *testing.T) {
	raltest.WithOutputCap(t, 4)
	ctx, got := raltest.DebugContext("notice")
	ctx = sources.WithSources(ctx, sources.Sources{Ref: "Exec[s]"})
	r := markedExec(map[string]any{"command": "printf abcdefgh"}, "command")
	if _, _, err := (Exec{}).Apply(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	raltest.AssertWarnings(t, *got, []report.LogEntry{{Level: "warning", Source: "voxagent",
		Message: "Exec[s]: output of [command redacted] truncated at 64 MiB"}})
}
