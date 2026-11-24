package concat

import (
	"math/big"
	"strings"
	"testing"
)

func TestRubyToI(t *testing.T) {
	huge := "123456789012345678901234567890"
	hugeInt, _ := new(big.Int).SetString(huge, 10)
	for _, c := range []struct {
		in   string
		want string
	}{
		{" 5\n7", "5"},
		{"1_000\n5", "1000"},
		{"-3\n4", "-3"},
		{"+3", "3"},
		{"1__0", "1"},
		{"_1", "0"},
		{"\t\n 9x", "9"},
		{"abc\n12", "0"},
		{"1_", "1"},
		{"-", "0"},
		{"", "0"},
		{"\v\f\r 8", "8"},
		{huge + "\n1", hugeInt.String()},
	} {
		if got := rubyToI(c.in).String(); got != c.want {
			t.Errorf("rubyToI(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

type ordFrag struct{ order, name, content string }

func fragsOf(fs ...ordFrag) []concatFragmentParams {
	out := make([]concatFragmentParams, len(fs))
	for i, f := range fs {
		out[i] = concatFragmentParams{Name: f.name, Order: f.order, Content: f.content}
	}
	return out
}

func contents(fs []concatFragmentParams) string {
	var s []string
	for _, f := range fs {
		s = append(s, f.Content)
	}
	return strings.Join(s, ",")
}

// m3Fragments are the fragments of measurement 03a/03b, in catalog order.
func m3Fragments() []concatFragmentParams {
	return fragsOf(
		ordFrag{"90", "o90", "90"},
		ordFrag{"100", "o100", "100"},
		ordFrag{"1000", "o1000", "1000"},
		ordFrag{"10___n", "on", "10___n"},
		ordFrag{"9", "str9", "str9"},
		ordFrag{"5", "tb", "5-tb"},
		ordFrag{"5", "ta", "5-ta"},
		ordFrag{"7", "2", "7-name2"},
		ordFrag{"7", "10", "7-name10"},
	)
}

func TestConcatNumericOrder(t *testing.T) {
	got, err := concatSort(m3Fragments(), "numeric")
	if err != nil {
		t.Fatal(err)
	}
	want := "5-ta,5-tb,7-name2,7-name10,str9,10___n,90,100,1000"
	// 5-tb/5-ta: names tb, ta sort as strings: ta first.
	if g := contents(got); g != want {
		t.Errorf("got %s\nwant %s", g, want)
	}

	m3d := fragsOf(
		ordFrag{"10", "d", "default10"},
		ordFrag{"009", "z009", "009"},
		ordFrag{"10", "i10", "int10"},
	)
	got, err = concatSort(m3d, "numeric")
	if err != nil {
		t.Fatal(err)
	}
	// [10,"d"] < [10,"i10"]; both after [9,"z009"].
	if g := contents(got); g != "009,default10,int10" {
		t.Errorf("M3d: got %s", g)
	}
}

func TestConcatAlphaOrder(t *testing.T) {
	got, err := concatSort(m3Fragments(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := "10___n,100,1000,5-ta,5-tb,7-name10,7-name2,str9,90"
	if g := contents(got); g != want {
		t.Errorf("got %s\nwant %s", g, want)
	}
}

func TestConcatNumericOrderMixedFails(t *testing.T) {
	const msg = "comparison of Array with Array failed"
	a := ordFrag{"10", "a", "a"}
	five := ordFrag{"10", "5", "5"}
	x := ordFrag{"x", "n", "x"}
	n5 := ordFrag{"5", "n", "5"}
	for name, fs := range map[string][]concatFragmentParams{
		"name a,5":  fragsOf(a, five),
		"name 5,a":  fragsOf(five, a),
		"order x,5": fragsOf(x, n5),
		"order 5,x": fragsOf(n5, x),
		"separated": fragsOf(a, ordFrag{"10", "b", "b"}, five),
	} {
		if _, err := concatSort(fs, "numeric"); err == nil || err.Error() != msg {
			t.Errorf("%s: err = %v, want %q", name, err, msg)
		}
	}
	// The same fragments sort fine under alpha, and a mix with no shared
	// first element is comparable.
	if _, err := concatSort(fragsOf(a, five), "alpha"); err != nil {
		t.Errorf("alpha: %v", err)
	}
	if _, err := concatSort(fragsOf(a, ordFrag{"11", "5", "5"}), "numeric"); err != nil {
		t.Errorf("different orders: %v", err)
	}
}

func TestConcatSortTiesKeepCatalogOrder(t *testing.T) {
	// Numeric: names "7" and "07" are both Integer 7.
	got, err := concatSort(fragsOf(ordFrag{"1", "07", "first"}, ordFrag{"1", "7", "second"}), "numeric")
	if err != nil {
		t.Fatal(err)
	}
	if g := contents(got); g != "first,second" {
		t.Errorf("numeric: %s", g)
	}
	got, _ = concatSort(fragsOf(ordFrag{"1", "7", "first"}, ordFrag{"1", "07", "second"}), "numeric")
	if g := contents(got); g != "first,second" {
		t.Errorf("numeric reversed: %s", g)
	}
	// Alpha: order 1 name "_x" and order "1_" name "x" are both "1____x".
	for _, fs := range [][]concatFragmentParams{
		fragsOf(ordFrag{"1", "_x", "first"}, ordFrag{"1_", "x", "second"}),
		fragsOf(ordFrag{"1_", "x", "first"}, ordFrag{"1", "_x", "second"}),
	} {
		got, err := concatSort(fs, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if g := contents(got); g != "first,second" {
			t.Errorf("alpha: %s", g)
		}
	}
}

func TestConcatSortDoesNotMutateInput(t *testing.T) {
	in := m3Fragments()
	before := contents(in)
	if _, err := concatSort(in, "numeric"); err != nil {
		t.Fatal(err)
	}
	if contents(in) != before {
		t.Error("input reordered")
	}
}

func TestConcatContentEnsureNewline(t *testing.T) {
	fs := fragsOf(
		ordFrag{"1", "a", "no newline"},
		ordFrag{"2", "b", "has newline\n"},
		ordFrag{"3", "c", ""},
		ordFrag{"4", "d", "two newlines\n\n"},
		ordFrag{"5", "e", "inner\nlast"},
	)
	if got, want := concatContent(fs, true), "no newline\nhas newline\n\ntwo newlines\n\ninner\nlast\n"; got != want {
		t.Errorf("true: %q want %q", got, want)
	}
	if got, want := concatContent(fs, false), "no newlinehas newline\ntwo newlines\n\ninner\nlast"; got != want {
		t.Errorf("false: %q want %q", got, want)
	}
	if got := concatContent(nil, true); got != "" {
		t.Errorf("empty: %q", got)
	}
}
