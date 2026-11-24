package ral

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
)

// markedFile is a File resource that marks the named parameters sensitive.
func markedFile(path string, params map[string]any, marked ...string) catalog.Resource {
	r := fileRes(path, params)
	r.SensitiveParameters = marked
	return r
}

// leakFree fails when text contains secret or lacks [redacted]: the redacted
// forms replace a value, quotes included, where Ruby leaks it (D-78).
func leakFree(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Errorf("%q leaks %q", text, secret)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Errorf("%q lacks [redacted]", text)
	}
}

func TestFileMarkedOwnerGroupLookup(t *testing.T) {
	for _, attr := range []string{"owner", "group"} {
		kind := map[string]string{"owner": "user", "group": "group"}[attr]
		name := "nosuch" + attr + "-secret"
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, marked := range []bool{true, false} {
			var sens []string
			if marked {
				sens = []string{attr}
			}
			changes, _, err := File{}.Apply(context.Background(),
				markedFile(path, map[string]any{"ensure": "file", attr: name}, sens...), false)
			if err != nil || len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("%s marked=%v: %+v, %v; want one failed change", attr, marked, changes, err)
			}
			got := changes[0].Err.Error()
			if marked {
				leakFree(t, got, name)
			} else if want := "Could not find " + kind + " " + name; got != want {
				t.Errorf("%s unmarked Err = %q, want %q", attr, got, want)
			}
		}
	}
}

// chownFailFS makes every chown fail, so the "Failed to set" text is reached.
type chownFailFS struct{ osFS }

func (chownFailFS) Lchown(string, int, int) error { return errors.New("injected chown failure") }
func (chownFailFS) Chown(string, int, int) error  { return errors.New("injected chown failure") }

func TestFileMarkedOwnerGroupSetFailure(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("an unprivileged run warns instead of chowning; this case runs under task dev:test-privileged")
	}
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, attr := range []string{"owner", "group"} {
		for _, marked := range []bool{true, false} {
			var sens []string
			if marked {
				sens = []string{attr}
			}
			changes, _, err := applyOwnership(chownFailFS{}, path, markedFile(path, map[string]any{attr: "54321"}, sens...), false, nil)
			if err != nil || len(changes) != 1 || changes[0].Err == nil {
				t.Fatalf("%s marked=%v: %+v, %v; want one failed change", attr, marked, changes, err)
			}
			got := changes[0].Err.Error()
			if marked {
				leakFree(t, got, "54321")
			} else if !strings.HasPrefix(got, "Failed to set "+attr+" to '54321': ") {
				t.Errorf("%s unmarked Err = %q", attr, got)
			}
		}
	}
}

func TestFileMarkedModeInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// "bogus-secret" fails to parse; "u-X" parses and fails when computed.
	for _, mode := range []string{"bogus-secret", "u-X"} {
		for _, marked := range []bool{true, false} {
			var sens []string
			if marked {
				sens = []string{"mode"}
			}
			r := markedFile(path, map[string]any{"ensure": "file", "mode": mode}, sens...)
			errs := map[string]error{"Validate": File{}.Validate(r)}
			changes, _, applyErr := File{}.Apply(context.Background(), r, false)
			errs["Apply"] = applyErr
			for _, c := range changes {
				if c.Err != nil {
					errs["Apply "+c.Property] = c.Err
				}
			}
			failed := false
			for _, err := range errs {
				failed = failed || err != nil
			}
			if !failed {
				t.Fatalf("mode %q: neither Validate nor Apply failed", mode)
			}
			for site, err := range errs {
				if err == nil {
					continue // a mode valid at one site fails at the other
				}
				if marked {
					leakFree(t, err.Error(), mode)
				} else if !strings.Contains(err.Error(), mode) {
					t.Errorf("%s unmarked %q: %v, want the mode named", site, mode, err)
				}
			}
		}
	}
}

func TestFileMarkedValidateValues(t *testing.T) {
	for _, tc := range []struct {
		attr  string
		value any
	}{
		{"ensure", []any{"ensure-secret"}},
		{"target", []any{"target-secret"}},
		{"seluser", []any{"seluser-secret"}},
	} {
		path := filepath.Join(t.TempDir(), "f")
		marked := File{}.Validate(markedFile(path, map[string]any{tc.attr: tc.value}, tc.attr))
		if marked == nil {
			t.Fatalf("%s: marked invalid value accepted", tc.attr)
		}
		leakFree(t, marked.Error(), "secret")
		unmarked := File{}.Validate(markedFile(path, map[string]any{tc.attr: tc.value}))
		if unmarked == nil || !strings.Contains(unmarked.Error(), tc.attr+"-secret") {
			t.Errorf("%s unmarked: %v, want the value named", tc.attr, unmarked)
		}
	}
}

func TestFileMarkedSourceValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	for _, source := range []string{"relative-secret", "opaque-secret:x", "file://host-secret/etc/x"} {
		marked := File{}.Validate(markedFile(path, map[string]any{"source": source}, "source"))
		if marked == nil {
			t.Fatalf("source %q: marked invalid source accepted", source)
		}
		leakFree(t, marked.Error(), "secret")
		unmarked := File{}.Validate(markedFile(path, map[string]any{"source": source}))
		if unmarked == nil || !strings.Contains(unmarked.Error(), "secret") {
			t.Errorf("source %q unmarked: %v, want the value named", source, unmarked)
		}
	}
}

func TestFileMarkedSourceMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	params := map[string]any{"ensure": "file", "source": "/no/such-secret"}
	_, _, err := File{Environment: "production"}.Apply(context.Background(), markedFile(path, params, "source"), false)
	if err == nil {
		t.Fatal("marked missing source: no error")
	}
	leakFree(t, err.Error(), "such-secret")
	_, _, err = File{Environment: "production"}.Apply(context.Background(), markedFile(path, params), false)
	if err == nil || !strings.Contains(err.Error(), "file:///no/such-secret") {
		t.Errorf("unmarked missing source: %v, want the source named", err)
	}
}

// A child generated from a marked source carries no marks of its own (it is
// not redacted in events), but its error text must not name the source.
func TestFileMarkedChildSourceError(t *testing.T) {
	parent := markedFile("/p/par", map[string]any{"source": "/srv/other-secret", "recurse": true}, "source")
	child := fileRes("/p/par/a", map[string]any{"source": "/nowhere-secret/zzz"})
	child.GeneratedFrom = &parent
	_, _, err := childSourceFS(child, nil)
	if err == nil {
		t.Fatal("child with a source outside the walk: no error")
	}
	leakFree(t, err.Error(), "secret")
	parent.SensitiveParameters = nil
	if _, _, err = childSourceFS(child, nil); err == nil || !strings.Contains(err.Error(), "/nowhere-secret/zzz") {
		t.Errorf("unmarked child: %v, want the source named", err)
	}
}

func TestFileMarkedEnsureWrap(t *testing.T) {
	cause := errors.New("boom")
	if got, want := wrapEnsureError("file", markedFile("/x", map[string]any{"ensure": "file"}, "ensure"), cause).Error(), "Could not set [redacted] on ensure: boom"; got != want {
		t.Errorf("marked = %q, want %q", got, want)
	}
	if got, want := wrapEnsureError("file", markedFile("/x", nil), cause).Error(), "Could not set 'file' on ensure: boom"; got != want {
		t.Errorf("unmarked = %q, want %q", got, want)
	}
}

// The parent's walk of a marked source names it in neither its failure nor
// its soft-limit warning (D-78).
func TestFileMarkedSourceWalk(t *testing.T) {
	gen := func(src string, marked bool) ([]Warning, error) {
		parent := fileRes(t.TempDir(), map[string]any{"recurse": true, "source": src})
		if marked {
			parent.SensitiveParameters = []string{"source"}
		}
		_, warnings, err := File{}.Generate(context.Background(), parent, func(string) bool { return false })
		return warnings, err
	}

	t.Run("unreadable directory", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "src-secret")
		if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(src, "sub"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "sub"), 0o755) })
		if os.Getuid() == 0 {
			t.Skip("root reads a mode 000 directory")
		}
		_, err := gen(src, true)
		if err == nil {
			t.Fatal("marked unreadable source directory: no error")
		}
		if strings.Contains(err.Error(), "src-secret") || !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("marked Err = %q, want [redacted] and no source", err)
		}
		if _, err = gen(src, false); err == nil || !strings.Contains(err.Error(), "src-secret") {
			t.Errorf("unmarked Err = %v, want the source named", err)
		}
	})

	t.Run("soft limit", func(t *testing.T) {
		src := filepath.Join(t.TempDir(), "src-secret")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= softMaxFiles; i++ {
			if err := os.WriteFile(filepath.Join(src, strconv.Itoa(i)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		warnings, err := gen(src, true)
		if err != nil || len(warnings) != 1 {
			t.Fatalf("marked: %v, %v; want one warning", warnings, err)
		}
		if got := warnings[0].Message; strings.Contains(got, "src-secret") || !strings.HasPrefix(got, "The directory [redacted] contains ") {
			t.Errorf("marked warning = %q", got)
		}
		if warnings, _ = gen(src, false); len(warnings) != 1 || !strings.Contains(warnings[0].Message, "'"+src+"'") {
			t.Errorf("unmarked warnings = %v, want the source named", warnings)
		}
	})
}

// A failed symlink's os.LinkError names the target; a marked one is replaced.
func TestFileMarkedLinkTargetError(t *testing.T) {
	for _, attr := range []string{"target", "ensure"} {
		params := map[string]any{"ensure": "link", "target": "/link-target-secret"}
		if attr == "ensure" {
			params = map[string]any{"ensure": "/link-target-secret"}
		}
		path := filepath.Join(t.TempDir(), "no-such-dir", "link")
		changes, _, err := File{}.Apply(context.Background(), markedFile(path, params, attr), false)
		if err != nil || len(changes) == 0 || changes[0].Err == nil {
			t.Fatalf("%s marked: %+v, %v; want a failed change", attr, changes, err)
		}
		if got := changes[0].Err.Error(); strings.Contains(got, "link-target-secret") || !strings.Contains(got, "[redacted]") {
			t.Errorf("%s marked Err = %q, want [redacted] and no target", attr, got)
		}
		changes, _, err = File{}.Apply(context.Background(), markedFile(path, params), false)
		if err != nil || len(changes) == 0 || changes[0].Err == nil || !strings.Contains(changes[0].Err.Error(), "/link-target-secret") {
			t.Errorf("%s unmarked: %+v, %v; want the target named", attr, changes, err)
		}
	}
}
