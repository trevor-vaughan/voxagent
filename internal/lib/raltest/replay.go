package raltest

import (
	"slices"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
)

// ReplayLib is an augeas.Lib answering each call with the next recorded call's
// results. The first call whose function, handle or arguments differ from the
// recording fails the test (Fatalf) with both lines; after that every call
// returns failure without reporting again, so deferred cleanup during the
// test's unwinding adds no noise. If recorded calls remain when the test ends
// (or at an explicit Done), the test fails: a replay of a prefix slices calls.
type ReplayLib struct {
	t        testing.TB
	calls    []TraceCall
	next     int
	diverged bool
}

// NewScriptLib replays calls, recorded (LoadTrace) or hand-written
// (ParseTrace), and checks at the test's end that every call was made.
func NewScriptLib(t testing.TB, calls []TraceCall) *ReplayLib {
	r := &ReplayLib{t: t, calls: calls}
	t.Cleanup(func() {
		if !t.Failed() {
			r.Done()
		}
	})
	return r
}

// step matches got against the next recorded call; nil after a divergence.
func (r *ReplayLib) step(got TraceCall) *TraceCall {
	r.t.Helper()
	if r.diverged {
		return nil
	}
	if r.next == len(r.calls) {
		r.diverged = true
		r.t.Fatalf("augeas replay: call %d is past the end of the %d recorded calls\n  got:  %s",
			r.next+1, len(r.calls), got.call())
		return nil
	}
	want := &r.calls[r.next]
	if got.Fn == "aug_init" {
		got.Handle = want.Handle // aug_init's handle is its result, not an argument
	}
	if got.call() != want.call() {
		r.diverged = true
		r.t.Fatalf("augeas replay: call %d (%s) differs from Ruby's\n  want: %s\n  got:  %s",
			r.next+1, want.Src, want, got.call())
		return nil
	}
	r.next++
	return want
}

// Done fails the test (Errorf: it also runs as a cleanup) if recorded calls
// remain. It reports once.
func (r *ReplayLib) Done() {
	r.t.Helper()
	if !r.diverged && r.next < len(r.calls) {
		r.diverged = true
		w := r.calls[r.next]
		r.t.Errorf("augeas replay: %d recorded call(s) not made; next is call %d (%s): %s",
			len(r.calls)-r.next, r.next+1, w.Src, w)
	}
}

func (r *ReplayLib) Init(root, loadpath *string, flags uint) (augeas.Handle, bool) {
	r.t.Helper()
	w := r.step(TraceCall{Fn: "aug_init", Args: []*string{root, loadpath}, Flags: flags})
	if w == nil || w.Handle == 0 {
		return 0, false
	}
	return w.Handle, true
}

func (r *ReplayLib) Close(h augeas.Handle) {
	r.t.Helper()
	r.step(TraceCall{Handle: h, Fn: "aug_close"})
}

func (r *ReplayLib) Get(h augeas.Handle, path string) (*string, int) {
	r.t.Helper()
	w := r.step(TraceCall{Handle: h, Fn: "aug_get", Args: []*string{&path}})
	if w == nil {
		return nil, -1
	}
	if w.Value == nil {
		return nil, w.RC
	}
	v := *w.Value
	return &v, w.RC
}

// rc replays a call whose only result is its return code.
func (r *ReplayLib) rc(got TraceCall) int {
	r.t.Helper()
	w := r.step(got)
	if w == nil {
		return -1
	}
	return w.RC
}

func (r *ReplayLib) Set(h augeas.Handle, path string, value *string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_set", Args: []*string{&path, value}})
}

func (r *ReplayLib) Rm(h augeas.Handle, path string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_rm", Args: []*string{&path}})
}

func (r *ReplayLib) Insert(h augeas.Handle, path, label string, before bool) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_insert", Args: []*string{&path, &label}, Before: before})
}

func (r *ReplayLib) Mv(h augeas.Handle, src, dst string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_mv", Args: []*string{&src, &dst}})
}

func (r *ReplayLib) Match(h augeas.Handle, path string) ([]string, int) {
	r.t.Helper()
	w := r.step(TraceCall{Handle: h, Fn: "aug_match", Args: []*string{&path}})
	if w == nil {
		return nil, -1
	}
	if w.RC < 0 {
		return nil, w.RC
	}
	return slices.Clone(w.Matches), w.RC
}

func (r *ReplayLib) Load(h augeas.Handle) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_load"})
}

func (r *ReplayLib) Save(h augeas.Handle) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_save"})
}

func (r *ReplayLib) Defvar(h augeas.Handle, name, expr string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_defvar", Args: []*string{&name, &expr}})
}

func (r *ReplayLib) Defnode(h augeas.Handle, name, expr string, value *string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_defnode", Args: []*string{&name, &expr, value}})
}

func (r *ReplayLib) TextStore(h augeas.Handle, lens, node, path string) int {
	r.t.Helper()
	return r.rc(TraceCall{Handle: h, Fn: "aug_text_store", Args: []*string{&lens, &node, &path}})
}
