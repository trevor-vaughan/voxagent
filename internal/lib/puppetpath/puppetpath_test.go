package puppetpath

import (
	"testing"

	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

func TestDeclaredModeApply(t *testing.T) {
	for _, tt := range []struct {
		mode    string
		current uint32
		dir     bool
		want    uint32
	}{
		{"u+rwX,g-rwx,o-rwx", 0o750, false, 0o700}, {"u+rwX,g-rwx,o-rwx", 0o644, false, 0o600},
		{"u+rwX,g-rwx,o-rwx", 0o755, true, 0o700}, {"u+rwX,g-rwx,o-rwx", 0o750, true, 0o700},
		{"u+rX,g-rwx,o-rwx", 0o750, false, 0o700}, {"u+rX,g-rwx,o-rwx", 0o644, false, 0o600},
		{"u+rX,g-rwx,o-rwx", 0o755, true, 0o700},
		{"g-w,o-rw", 0o750, false, 0o750}, {"g-w,o-rw", 0o644, false, 0o640},
		{"g-w,o-rw", 0o755, true, 0o751}, {"g-w,o-rw", 0o750, true, 0o750},
		{"u+rwx,g+rwx,o+rwxt", 0o750, false, 0o1777}, {"u+rwx,g+rwx,o+rwxt", 0o755, true, 0o1777},
		{"a=r", 0o644, false, 0o444}, {"u=rw,go=", 0o644, false, 0o600}, {"+t", 0o644, false, 0o1644},
		{"=", 0o644, false, 0o000}, {"+x", 0o644, false, 0o755}, {"-x", 0o644, false, 0o644},
		{"u+", 0o644, false, 0o644}, {"u=,g+r", 0o644, false, 0o044}, {"g=u", 0o644, false, 0o664},
		{"go=u-w", 0o644, false, 0o644}, {"ug+rw,o=", 0o644, false, 0o660},
		{"u+s,g+s", 0o644, false, 0o6644}, {"g+s", 0o644, false, 0o2644}, {"a+s", 0o644, false, 0o7644},
		{"o+s", 0o644, false, 0o1644}, {"o+t", 0o644, false, 0o1644}, {"a+t", 0o644, false, 0o1644},
		{"o-t", 0o644, false, 0o644}, {"u-s", 0o644, false, 0o644}, {"a+X", 0o644, false, 0o644},
		{"=", 0o1755, false, 0o1000}, {"u=rw", 0o2755, false, 0o2655},
		{"u+x,g+X", 0o644, false, 0o744}, {"u+rX,g+X", 0o644, false, 0o644},
		{"a+X", 0o010, false, 0o111}, {"g=u,o=g", 0o100, false, 0o111},
		{"0755", 0o2755, false, 0o755}, {"7755", 0o644, false, 0o7755},
	} {
		m, err := ParseDeclaredMode(tt.mode, false)
		if err != nil {
			t.Fatalf("%q: %v", tt.mode, err)
		}
		got, err := m.Apply(tt.current, tt.dir)
		if err != nil || got != tt.want {
			t.Errorf("%q on %04o (dir %v) = %04o, %v; want %04o", tt.mode, tt.current, tt.dir, got, err, tt.want)
		}
	}
}

func TestDeclaredModeXNeedsPlus(t *testing.T) {
	for mode, want := range map[string]string{
		"u=rwX":       `X only works with the '+' operator in symbolic mode "u=rwX"`,
		"u-X":         `X only works with the '+' operator in symbolic mode "u-X"`,
		"u+r-X":       `X only works with the '+' operator in symbolic mode "u+r-X"`,
		"u+r,g=X,o+r": `X only works with the '+' operator at "g=X" in symbolic mode "u+r,g=X,o+r"`,
	} {
		m, _ := ParseDeclaredMode(mode, false)
		if _, err := m.Apply(0o644, false); err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", mode, err, want)
		}
	}
	if m, _ := ParseDeclaredMode("u-X", false); m.FailureDisplay() != "0u-X" {
		t.Errorf("failureDisplay = %q, want 0u-X", m.FailureDisplay())
	}
}

func TestParseDeclaredMode(t *testing.T) {
	for _, ok := range []struct{ mode, desired string }{
		{"0755", "0755"}, {"755", "0755"}, {"00755", "0755"}, {"7755", "7755"}, {"2775", "2775"},
		{"u+rwX,g-rwx,o-rwx", "u+rwX,g-rwx,o-rwx"},
	} {
		m, err := ParseDeclaredMode(ok.mode, false)
		if err != nil || m.Desired() != ok.desired {
			t.Errorf("%q: desired %q, %v; want %q", ok.mode, m.Desired(), err, ok.desired)
		}
	}
	// "000755" is invalid by util/symbolic_file_mode.rb:19-20 (read, not measured).
	for _, bad := range []string{"u+q", "", " u+x", "u+x ,g+r", "u+rw,,g+r", "u+x,", "rw", "u",
		"12345", "0o755", "789", "000755", "u+x\nj", "j\nu+x", "u+x\n"} {
		_, err := ParseDeclaredMode(bad, false)
		if want := "The file mode specification is invalid: " + rubyvalue.Inspect(bad); err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", bad, err, want)
		}
	}
	if _, err := ParseDeclaredMode(float64(755), false); err == nil ||
		err.Error() != "The file mode specification must be a string, not 'Integer'" {
		t.Errorf("Integer: err = %v", err)
	}
}
