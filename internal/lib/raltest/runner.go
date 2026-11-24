package raltest

import (
	"context"
	"strings"
	"testing"
)

// FakeRunner replays scripted responses and records what it was asked to run.
//
// A command with no scripted reply succeeds silently, so a test that cares
// about a failure has to say so -- the reverse would make every unasserted
// command look like a failure and hide which one the provider actually ran.
//
// Sequences answers a command differently each time it runs -- rpm -q before
// and after an install -- and takes precedence over Replies until exhausted.
type FakeRunner struct {
	Replies   map[string]FakeReply
	Sequences map[string][]FakeReply
	Calls     [][]string
}

type FakeReply struct {
	Exit int
	Out  string
	Err  error
}

// run implements commandRunner. Note the result order -- status, output, error
// -- which is the seam's (dnf.go) and Exec.execute's.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (int, string, error) {
	f.Calls = append(f.Calls, append([]string{name}, args...))
	key := strings.Join(append([]string{name}, args...), " ")
	if seq := f.Sequences[key]; len(seq) > 0 {
		f.Sequences[key] = seq[1:]
		return seq[0].Exit, seq[0].Out, seq[0].Err
	}
	r, ok := f.Replies[key]
	if !ok {
		return 0, "", nil
	}
	return r.Exit, r.Out, r.Err
}

// OnlyCall returns the single command the provider ran, failing the test when
// it ran a different number of them.
func (f *FakeRunner) OnlyCall(t *testing.T) []string {
	t.Helper()
	if len(f.Calls) != 1 {
		t.Fatalf("ran %v, want exactly one command", f.Calls)
	}
	return f.Calls[0]
}

// RanExactly fails the test unless the provider ran exactly want, in order.
func (f *FakeRunner) RanExactly(t *testing.T, want ...[]string) {
	t.Helper()
	if len(f.Calls) != len(want) {
		t.Fatalf("ran %v, want %v", f.Calls, want)
	}
	for i := range want {
		if !EqualArgv(f.Calls[i], want[i]) {
			t.Errorf("command %d: ran %v, want %v", i, f.Calls[i], want[i])
		}
	}
}

func EqualArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
