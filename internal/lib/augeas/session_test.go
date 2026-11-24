package augeas_test

import (
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

type countingProbe struct {
	t      *testing.T
	reason string // "" probes successfully
	calls  int
}

func (p *countingProbe) Probe() (augeas.Lib, string, bool) {
	p.calls++
	if p.reason != "" {
		return nil, p.reason, false
	}
	return raltest.NewScriptLib(p.t, nil), "", true
}

func TestSessionProbesOnce(t *testing.T) {
	p := &countingProbe{t: t}
	s := augeas.NewSession(p)
	if s.Probed() {
		t.Fatal("Probed before the first Probe call")
	}
	first, why, ok := s.Probe()
	if !ok || why != "" || first == nil {
		t.Fatalf("first probe: %v %q %v", first, why, ok)
	}
	second, _, ok := s.Probe()
	if !ok || second != first || p.calls != 1 || !s.Probed() {
		t.Errorf("second probe returned a different result: calls %d, probed %v", p.calls, s.Probed())
	}
}

func TestSessionMemoizesFailure(t *testing.T) {
	p := &countingProbe{t: t, reason: "libaugeas: dlopen: no such file"}
	s := augeas.NewSession(p)
	for i := range 2 {
		if lib, why, ok := s.Probe(); ok || why != p.reason || lib != nil {
			t.Errorf("probe %d: %v %q %v", i, lib, why, ok)
		}
	}
	if p.calls != 1 {
		t.Errorf("probe called %d times, want 1", p.calls)
	}
}
