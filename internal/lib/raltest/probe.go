package raltest

import (
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
)

// ProbeFake answers the libaugeas probe and counts its calls. A successful
// probe's library is a script of no calls: routing must not touch it (handles
// open lazily, in Prefetch and Apply).
type ProbeFake struct {
	T      *testing.T
	Reason string // "" probes successfully
	Calls  int
}

// Probe implements augeas.Probe.
func (p *ProbeFake) Probe() (augeas.Lib, string, bool) {
	p.Calls++
	if p.Reason != "" {
		return nil, p.Reason, false
	}
	return NewScriptLib(p.T, nil), "", true
}
