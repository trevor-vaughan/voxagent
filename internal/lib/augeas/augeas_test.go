package augeas_test

import (
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/lib/augeas"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

func TestRubyAugGetNilUnlessExactlyOne(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_get "/a" => 0 NULL
h1 aug_get "/b" => -1 NULL
h1 aug_get "/c" => 1 NULL
h1 aug_get "/d" => 1 "v"
h1 aug_get "/e" => 1 ""
`))
	a, err := augeas.Open(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/a", "/b", "/c"} {
		if v, ok := a.Get(p); ok {
			t.Errorf("get(%s) = %q, want nil", p, v)
		}
	}
	if v, ok := a.Get("/d"); !ok || v != "v" {
		t.Errorf("get(/d) = %q %v", v, ok)
	}
	if v, ok := a.Get("/e"); !ok || v != "" {
		t.Errorf("get(/e) = %q %v", v, ok)
	}
	r.Done()
}

func TestRubyAugMatchError(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", `h1 aug_init NULL "/lenses" 64
h1 aug_match "[[[" => -1
h1 aug_match "/m" => 2 "/m/a" "/m/b"
h1 aug_match "/n" => 0
`))
	lp := "/lenses"
	a, err := augeas.Open(r, &lp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Match("[[["); err == nil || err.Error() != "unknown error - Matching path expression '[[[' failed" {
		t.Fatalf("match error = %v", err)
	}
	if m, err := a.Match("/m"); err != nil || strings.Join(m, ",") != "/m/a,/m/b" {
		t.Fatalf("match = %v %v", m, err)
	}
	if m, err := a.Match("/n"); err != nil || len(m) != 0 {
		t.Fatalf("match = %v %v", m, err)
	}
	r.Done()
}

func TestRubyAugLoadSaveBang(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_load => -1
h1 aug_save => -1
h1 aug_load => 0
h1 aug_save => 0
`))
	a, err := augeas.Open(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.LoadBang(); err == nil || err.Error() != "Augeas::Error" {
		t.Errorf("load! = %v", err)
	}
	if err := a.SaveBang(); err == nil || err.Error() != "Augeas::Error" {
		t.Errorf("save! = %v", err)
	}
	if err := a.LoadBang(); err != nil {
		t.Errorf("load! = %v", err)
	}
	if err := a.SaveBang(); err != nil {
		t.Errorf("save! = %v", err)
	}
	r.Done()
}

func TestOpenRubyAugFailure(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", "h0 aug_init NULL NULL 64\n"))
	a, err := augeas.Open(r, nil)
	if a != nil || err == nil || err.Error() != "unknown error - Failed to initialize Augeas" {
		t.Fatalf("open = %v %v", a, err)
	}
	r.Done()
}

// The remaining wrappers pass Ruby's arguments and return libaugeas' results.
func TestRubyAugPassThrough(t *testing.T) {
	r := raltest.NewScriptLib(t, raltest.ParseTrace(t, "script", `h1 aug_init NULL NULL 64
h1 aug_set "/a" "1" => -1
h1 aug_set "/b" NULL => 0
h1 aug_rm "/c" => 3
h1 aug_insert "/d" "e" 1 => 0
h1 aug_insert "/f" "g" 0 => -1
h1 aug_defvar "v" "/x" => 2
h1 aug_defnode "n" "/y" NULL NULL => 1
h1 aug_text_store "Ssh.lns" "/input" "/parsed" => 0
h1 aug_text_store "Ssh.lns" "/input" "/parsed" => -1
h1 aug_close
`))
	a, err := augeas.Open(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Set("/a", "1")
	a.Clear("/b")
	if n := a.Rm("/c"); n != 3 {
		t.Errorf("rm = %d", n)
	}
	if n := a.Insert("/d", "e", true); n != 0 {
		t.Errorf("insert = %d", n)
	}
	if n := a.Insert("/f", "g", false); n != -1 {
		t.Errorf("insert = %d", n)
	}
	a.Defvar("v", "/x")
	a.Defnode("n", "/y")
	if !a.TextStore("Ssh.lns", "/input", "/parsed") {
		t.Error("text_store 0 = false")
	}
	if a.TextStore("Ssh.lns", "/input", "/parsed") {
		t.Error("text_store -1 = true")
	}
	a.Close()
	a.Close()
	r.Done()
}
