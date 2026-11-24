package extension

import (
	"slices"
	"testing"
)

func TestHostEnvKeepsOnlyTheMinimalSet(t *testing.T) {
	in := []string{"PATH=/usr/bin", "HOME=/root", "LANG=C.UTF-8", "LC_ALL=C", "GEM_HOME=/g",
		"GEM_PATH=/g:/h", "RUBYLIB=/evil", "RUBYOPT=-r/evil", "LD_PRELOAD=/evil.so", "USER=root",
		"PATHOLOGICAL=1", "NOEQUALS"}
	got := HostEnv(in)
	want := []string{"PATH=/usr/bin", "HOME=/root", "LANG=C.UTF-8", "LC_ALL=C", "GEM_HOME=/g", "GEM_PATH=/g:/h"}
	if !slices.Equal(got, want) {
		t.Errorf("HostEnv = %v, want %v", got, want)
	}
}

// exec.Cmd reads a nil Env as "inherit the whole environment", which would
// hand RUBYOPT and LD_PRELOAD to the host.
func TestHostEnvIsNeverNil(t *testing.T) {
	if got := HostEnv([]string{"RUBYOPT=-r/evil"}); got == nil || len(got) != 0 {
		t.Errorf("HostEnv with no allowed key = %#v, want a non-nil empty slice", got)
	}
}
