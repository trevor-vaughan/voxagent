package augeas

// Session is one run's view of libaugeas: the probe is made at most once and
// its answer, success or refusal, is reused. It holds no package-level state
// and takes no lock: routing is single-goroutine, so build one Session per
// run and do not share it across goroutines.
type Session struct {
	probe  Probe
	probed bool
	lib    Lib
	reason string
	ok     bool

	// Shared is owned by augeasproviders_core: the run its four types share;
	// nothing else reads or writes it. Its type lives in a unit, which this
	// package cannot import.
	Shared any
}

// NewSession returns a Session over p; a nil p means HostProbe.
func NewSession(p Probe) *Session {
	if p == nil {
		p = HostProbe
	}
	return &Session{probe: p}
}

// Probe probes on the first call and returns that answer on every later one.
func (s *Session) Probe() (Lib, string, bool) {
	if !s.probed {
		s.lib, s.reason, s.ok = s.probe.Probe()
		s.probed = true
	}
	return s.lib, s.reason, s.ok
}

// Probed reports whether Probe has been called.
func (s *Session) Probed() bool { return s.probed }
