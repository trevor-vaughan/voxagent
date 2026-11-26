package tidy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/units/file"
)

// generatedChildren generates Tidy[path] and returns its Files marked with the Tidy,
// as catalog.MergeGeneration marks them.
func generatedChildren(t *testing.T, path string, params map[string]any) []catalog.Resource {
	t.Helper()
	cat, r := tidyCatalog(path, params)
	g := generate(t, cat, r)
	if g.Failed {
		t.Fatalf("generation failed: %+v", g.Logs)
	}
	for i := range g.Resources {
		g.Resources[i].GeneratedFrom = &r
	}
	return g.Resources
}

// D-104: a parent swapped for a link out of the tree after generation fails
// the child instead of deleting through the link.
func TestTidyChildConfined(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	tree(t, base, "root/", "root/sub/", "root/sub/f", "outside/", "outside/f")
	children := generatedChildren(t, root, map[string]any{"recurse": true})
	if len(children) != 1 || children[0].Title != filepath.Join(root, "sub", "f") {
		t.Fatalf("children = %+v, want only %s/sub/f", children, root)
	}
	sub := filepath.Join(root, "sub")
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (file.File{}).Apply(context.Background(), children[0], false); err == nil ||
		!strings.Contains(err.Error(), "path escapes from parent") {
		t.Errorf("Apply = %v; want the root's escape error", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "f")); err != nil {
		t.Errorf("outside f: %v; want it kept", err)
	}
}

// The child at the Tidy's own path cannot be reached inside a root opened on
// that path, so it is removed by path (spec §6).
func TestTidyChildAtRootUnconfined(t *testing.T) {
	base := t.TempDir()
	tree(t, base, "f", "d/", "d/x")
	for _, c := range []struct {
		path   string
		params map[string]any
	}{
		{filepath.Join(base, "f"), nil},
		{filepath.Join(base, "d"), map[string]any{"rmdirs": true}},
	} {
		children := generatedChildren(t, c.path, c.params)
		if len(children) != 1 || children[0].Title != c.path {
			t.Fatalf("children = %+v, want only %s", children, c.path)
		}
		changes, _, err := (file.File{}).Apply(context.Background(), children[0], false)
		if err != nil || len(changes) != 1 {
			t.Errorf("%s: Apply = %+v, %v; want one change", c.path, changes, err)
		}
		if _, err := os.Lstat(c.path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists or cannot be checked: %v", c.path, err)
		}
	}
}

// A child gone by its turn, alone or with its parent directory, is in sync
// through the root.
func TestTidyChildVanishedIsInSync(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	tree(t, base, "root/", "root/f", "root/sub/", "root/sub/g")
	children := generatedChildren(t, root, map[string]any{"recurse": true})
	if len(children) != 2 {
		t.Fatalf("children = %+v, want two", children)
	}
	if err := os.Remove(filepath.Join(root, "f")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	for _, c := range children {
		if changes, _, err := (file.File{}).Apply(context.Background(), c, false); err != nil || len(changes) != 0 {
			t.Errorf("%s: Apply = %+v, %v; want in sync", c.Title, changes, err)
		}
	}
}

// D-104: with no directory at the Tidy's path by a child's turn, the child,
// which is ensure => absent, is in sync, as in Ruby; a symlink there still
// fails it and deletes nothing through the link. Live and noop alike.
func TestTidyChildRootGone(t *testing.T) {
	for _, noop := range []bool{false, true} {
		for _, c := range []struct {
			name    string
			replace func(t *testing.T, root, outside string)
			wantErr string
		}{
			{"removed", func(t *testing.T, root, _ string) {}, ""},
			{"regular file", func(t *testing.T, root, _ string) {
				if err := os.WriteFile(root, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}, ""},
			{"symlink out", func(t *testing.T, root, outside string) {
				if err := os.Symlink(outside, root); err != nil {
					t.Fatal(err)
				}
			}, "is a symlink; links is not implemented"},
		} {
			t.Run(fmt.Sprintf("%s noop=%t", c.name, noop), func(t *testing.T) {
				base := t.TempDir()
				root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
				tree(t, base, "root/", "root/f", "outside/", "outside/f")
				children := generatedChildren(t, root, map[string]any{"recurse": true})
				if len(children) != 1 || children[0].Title != filepath.Join(root, "f") {
					t.Fatalf("children = %+v, want only %s/f", children, root)
				}
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				c.replace(t, root, outside)
				changes, warnings, err := (file.File{}).Apply(context.Background(), children[0], noop)
				if c.wantErr == "" {
					if err != nil || len(changes) != 0 || len(warnings) != 0 {
						t.Errorf("Apply = %+v, %+v, %v; want in sync", changes, warnings, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("Apply = %+v, %v; want %q", changes, err, c.wantErr)
				}
				if _, err := os.Lstat(filepath.Join(outside, "f")); err != nil {
					t.Errorf("outside f: %v; want it kept", err)
				}
			})
		}
	}
}

// delegateReason's path rules and file's recursionChild agree: a child of a
// Tidy delegateReason keeps native is confined to the Tidy's cleaned path, and
// a child of one it delegates for its path is not confined at all. The path
// cases are TestDelegateReason's, rebased under a temporary directory, plus
// "/". Each child sits below a symlink to an outside directory: a confined
// child fails naming its root (or, at "/", the escape) and the outside file
// survives; an unconfined one is removed through the link, as Ruby removes it.
func TestDelegatedPathsAreUnconfined(t *testing.T) {
	type tidyCase struct {
		name   string
		tidy   func(base string) catalog.Resource
		root   string // the cleaned path below base, or "/" for the filesystem root
		native bool
	}
	res := func(title string, params map[string]any) catalog.Resource {
		return catalog.Resource{Type: "Tidy", Title: title, Parameters: params}
	}
	cases := []tidyCase{
		{"cleaned path parameter", func(b string) catalog.Resource {
			return res("t", map[string]any{"path": b + "//t/../t/"})
		}, "t", true},
		{"root", func(string) catalog.Resource { return res("/", nil) }, "/", true},
		{"title", func(b string) catalog.Resource { return res(b+"/r/", nil) }, "r", true},
		{"undef path falls back to the title", func(b string) catalog.Resource {
			return res(b+"/r", map[string]any{"path": nil})
		}, "r", true},
		{"relative path", func(string) catalog.Resource { return res("t/rel", nil) }, "r", false},
		{"home path", func(string) catalog.Resource { return res("~/x", nil) }, "r", false},
		{"double slash", func(b string) catalog.Resource { return res("/"+b+"/r", nil) }, "r", false},
		{"triple slash", func(b string) catalog.Resource { return res("//"+b+"/r", nil) }, "r", false},
		{"relative path parameter", func(b string) catalog.Resource {
			return res(b+"/r", map[string]any{"path": "t/rel"})
		}, "r", false},
		{"non-String path", func(b string) catalog.Resource {
			return res(b+"/r", map[string]any{"path": float64(5)})
		}, "r", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			tree(t, base, "outside/", "outside/f")
			outside := filepath.Join(base, "outside")
			link := filepath.Join(base, c.root)
			if c.root == "/" {
				link = filepath.Join(base, "r")
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			tidy := c.tidy(base)
			if got := delegateReason(tidy); (got == "") != c.native {
				t.Fatalf("delegateReason = %q, want native %t", got, c.native)
			}
			r := child(filepath.Join(link, "f"), nil)
			r.GeneratedFrom = &tidy
			_, _, err := (file.File{}).Apply(context.Background(), r, false)
			_, kept := os.Lstat(filepath.Join(outside, "f"))
			switch {
			case !c.native:
				if err != nil || !errors.Is(kept, os.ErrNotExist) {
					t.Errorf("Apply = %v, outside f: %v; want it removed through the link", err, kept)
				}
			case c.root == "/":
				if err == nil || !strings.Contains(err.Error(), "path escapes from parent") || kept != nil {
					t.Errorf("Apply = %v, outside f: %v; want the escape from / and f kept", err, kept)
				}
			default:
				want := link + " is a symlink; links is not implemented"
				if err == nil || err.Error() != want || kept != nil {
					t.Errorf("Apply = %v, outside f: %v; want %q and f kept", err, kept, want)
				}
			}
		})
	}
}
