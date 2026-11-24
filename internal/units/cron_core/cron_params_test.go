package croncore

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/rubyvalue"
)

var releases = []string{"1.3.0", "2.1.0"}

func cronRes(params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Cron", Title: "t", Parameters: params}
}

func fakeTarget() (string, error) { return "tgt", nil }

func list(s ...string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.List, L: s} }
func str(s string) parsedfile.Value     { return parsedfile.Value{Kind: parsedfile.Str, S: s} }

var absent = parsedfile.Value{Kind: parsedfile.Absent}

// result is what parseCron must produce for one release: an error text, an
// UnimplementedValue, or the Should entries (target is compared only when the
// want names it).
type result struct {
	err    string
	unimpl bool
	should map[string]parsedfile.Value
}

type should = map[string]parsedfile.Value

func fail(text string) result { return result{err: text} }
func ok(kv should) result     { return result{should: kv} }

var delegated = result{unimpl: true}

type cronCase struct {
	name       string
	params     map[string]any
	v130, v210 result
}

func both(name string, params map[string]any, r result) cronCase {
	return cronCase{name: name, params: params, v130: r, v210: r}
}

func munge(field, shown, tail string) string {
	return fmt.Sprintf("Parameter %s failed on Cron[t]: Munging failed for value %s in class %s: %s is not a valid %s", field, shown, field, tail, field)
}

func checkResult(t *testing.T, got cronParams, err error, want result) {
	t.Helper()
	switch {
	case want.unimpl:
		var u *rubyvalue.UnimplementedValue
		if !errors.As(err, &u) {
			t.Fatalf("err = %v (%T), want UnimplementedValue", err, err)
		}
	case want.err != "":
		if err == nil || err.Error() != want.err {
			t.Fatalf("err = %v, want %q", err, want.err)
		}
	default:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		g := should{}
		for k, v := range got.Should {
			g[k] = v
		}
		if _, named := want.should["target"]; !named {
			delete(g, "target")
		}
		if !reflect.DeepEqual(g, want.should) {
			t.Fatalf("Should = %#v, want %#v", g, want.should)
		}
	}
}

// runCases runs every case under both releases through parseCron and checks
// that Validate agrees with it.
func runCases(t *testing.T, cases []cronCase) {
	t.Helper()
	for _, tt := range cases {
		for _, rel := range releases {
			want := tt.v130
			if rel == "2.1.0" {
				want = tt.v210
			}
			t.Run(tt.name+"/"+rel, func(t *testing.T) {
				got, err := parseCron(cronRes(tt.params), rel, "UTF-8", fakeTarget)
				checkResult(t, got, err, want)
				verr := Cron{release: rel, encoding: "UTF-8"}.Validate(cronRes(tt.params))
				if (verr == nil) != (err == nil) || (err != nil && verr.Error() != err.Error()) {
					t.Errorf("Validate = %v, parseCron = %v", verr, err)
				}
			})
		}
	}
}

// TestCronVectors is spec 4.3's Integer table, run on minute (0-59) and month
// (1-12) under both releases (M12: every row on the oracle).
func TestCronVectors(t *testing.T) {
	rows := []struct {
		s                              string
		min210, mon210, min130, mon130 bool
	}{
		{"0x1f", true, false, false, false},
		{"0b1", true, false, false, false},
		{"0o7", true, false, false, false},
		{"0d9", true, true, false, false},
		{"0D9", true, true, false, false},
		{"1_000", false, false, false, false},
		{"1_2_3", false, false, false, false},
		{" 1", true, true, false, false},
		{"\v1", true, true, false, false},
		{" 0x1f ", true, false, false, false},
		{"-1", false, false, false, false},
		{" -1", false, false, false, false},
		{"+5", true, true, false, false},
		{"+0d1_2", true, true, false, false},
		{"0_1", true, true, false, false},
		{"07", true, true, true, true},
		{"08", true, true, true, true},
		{"1__0", false, false, false, false},
		{"1_", false, false, false, false},
		{"_1", false, false, false, false},
		{"0x", false, false, false, false},
		{"0d", false, false, false, false},
		{"0d_1", false, false, false, false},
		{"+-1", false, false, false, false},
		{"- 1", false, false, false, false},
		{"１", false, false, false, false},
		{"1e3", false, false, false, false},
		{"12abc", false, false, false, false},
		{"0x1_0000_0000_0000_0000", true, false, false, false},
	}
	var cases []cronCase
	for _, row := range rows {
		for _, f := range []struct {
			field      string
			k210, k130 bool
		}{{"minute", row.min210, row.min130}, {"month", row.mon210, row.mon130}} {
			res := func(kept bool) result {
				if kept {
					return ok(should{f.field: list(row.s)})
				}
				return fail(munge(f.field, rubyvalue.Inspect(row.s), row.s))
			}
			cases = append(cases, cronCase{
				name:   fmt.Sprintf("%s %q", f.field, row.s),
				params: map[string]any{f.field: row.s},
				v130:   res(f.k130), v210: res(f.k210),
			})
		}
	}
	runCases(t, cases)
}

func TestCronTimeMunge(t *testing.T) {
	runCases(t, []cronCase{
		both("leading zero is kept", map[string]any{"minute": "05"}, ok(should{"minute": list("05")})),
		both("integer", map[string]any{"hour": float64(7)}, ok(should{"hour": list("7")})),
		both("whole float is an integer", map[string]any{"hour": 7.0}, ok(should{"hour": list("7")})),
		both("integer out of range", map[string]any{"hour": float64(24)}, fail(munge("hour", "24", "24"))),
		both("negative integer", map[string]any{"minute": float64(-1)}, fail(munge("minute", "-1", "-1"))),
		both("weekday name", map[string]any{"weekday": "Sun"}, ok(should{"weekday": list("0")})),
		both("weekday upper full name", map[string]any{"weekday": "TUESDAY"}, ok(should{"weekday": list("2")})),
		both("weekday mixed three letters", map[string]any{"weekday": "tHu"}, ok(should{"weekday": list("4")})),
		both("weekday 7", map[string]any{"weekday": "7"}, ok(should{"weekday": list("7")})),
		both("weekday 8", map[string]any{"weekday": "8"}, fail(munge("weekday", `"8"`, "8"))),
		both("weekday bad name", map[string]any{"weekday": "funday"}, fail(munge("weekday", `"funday"`, "funday"))),
		both("weekday two letters", map[string]any{"weekday": "su"}, fail(munge("weekday", `"su"`, "su"))),
		both("month name", map[string]any{"month": "December"}, ok(should{"month": list("12")})),
		both("month short", map[string]any{"month": "MAY"}, ok(should{"month": list("5")})),
		both("month placeholder", map[string]any{"month": "___"}, ok(should{"month": list("0")})),
		both("month long placeholder", map[string]any{"month": "___placeholder"}, ok(should{"month": list("0")})),
		both("month 0 is out of range", map[string]any{"month": "0"}, fail(munge("month", `"0"`, "0"))),
		both("minute has no names", map[string]any{"minute": "mon"}, fail(munge("minute", `"mon"`, "mon"))),
		both("non-ascii three letters", map[string]any{"weekday": "ｓｕｎ"}, fail(munge("weekday", `"ｓｕｎ"`, "ｓｕｎ"))),
		both("range is not range-checked", map[string]any{"minute": "5-70"}, ok(should{"minute": list("5-70")})),
		both("step is not range-checked", map[string]any{"minute": "*/0"}, ok(should{"minute": list("*/0")})),
		both("range with step", map[string]any{"hour": "1-5/2"}, ok(should{"hour": list("1-5/2")})),
		both("star is absent", map[string]any{"minute": "*"}, ok(should{"minute": absent})),
		both("absent string is absent", map[string]any{"minute": "absent"}, ok(should{"minute": absent})),
		both("absent first, rest ignored", map[string]any{"minute": []any{"*", float64(5)}}, ok(should{"minute": absent})),
		both("absent first, rest still munged", map[string]any{"minute": []any{"*", "70"}}, fail(munge("minute", `"70"`, "70"))),
		both("absent later is a literal", map[string]any{"minute": []any{"5", "*"}}, ok(should{"minute": list("5", "absent")})),
		both("list keeps order", map[string]any{"minute": []any{"2", "1"}}, ok(should{"minute": list("2", "1")})),
		both("list element refused", map[string]any{"minute": []any{"1", "x"}}, fail(munge("minute", `"x"`, "x"))),
		both("empty list is a should", map[string]any{"minute": []any{}}, ok(should{"minute": list()})),
		both("undef is unset", map[string]any{"minute": nil}, ok(should{})),
		{
			name: "non-integral float", params: map[string]any{"minute": 5.5},
			v130: delegated, v210: fail(munge("minute", "5.5", "5.5")),
		},
		{
			name: "true", params: map[string]any{"weekday": true},
			v130: delegated, v210: fail(munge("weekday", "true", "true")),
		},
		{
			name: "true in a list", params: map[string]any{"month": []any{"1", true}},
			v130: delegated, v210: fail(munge("month", "true", "true")),
		},
		{
			name: "nested list", params: map[string]any{"minute": []any{[]any{"5"}}},
			v130: delegated, v210: fail(munge("minute", `["5"]`, `["5"]`)),
		},
		both("hash", map[string]any{"minute": map[string]any{"a": float64(1)}}, delegated),
		{name: "null element", params: map[string]any{"minute": []any{nil}}, v130: delegated, v210: fail(munge("minute", "nil", ""))},
		{name: "null after a valid element", params: map[string]any{"minute": []any{"5", nil}}, v130: delegated, v210: fail(munge("minute", "nil", ""))},
		{name: "integer in a nested list", params: map[string]any{"minute": []any{[]any{float64(5)}}}, v130: delegated, v210: fail(munge("minute", "[5]", "[5]"))},
		{name: "null in a nested list", params: map[string]any{"minute": []any{[]any{nil}}}, v130: delegated, v210: fail(munge("minute", "[nil]", "[nil]"))},
		both("huge float", map[string]any{"minute": 1e20}, delegated),
		{
			name: "non-integral float before a refusal order", params: map[string]any{"minute": []any{"70", 5.5}},
			v130: fail(munge("minute", `"70"`, "70")), v210: fail(munge("minute", `"70"`, "70")),
		},
	})
}

func TestCronLineBreaksKept(t *testing.T) {
	runCases(t, []cronCase{
		both("minute", map[string]any{"minute": "1\n2"}, ok(should{"minute": list("1\n2")})),
		both("command", map[string]any{"command": "true\n* * * * * x"}, ok(should{"command": str("true\n* * * * * x")})),
		both("environment", map[string]any{"environment": "A=1\nB"}, ok(should{"environment": list("A=1\nB")})),
		both("last line decides nothing", map[string]any{"minute": "x\n5"}, ok(should{"minute": list("x\n5")})),
		both("no digit line", map[string]any{"minute": "x\ny"}, fail(munge("minute", `"x\ny"`, "x\ny"))),
	})
}

func TestCronCommand(t *testing.T) {
	runCases(t, []cronCase{
		both("stripped", map[string]any{"command": "  /bin/x \t\n"}, ok(should{"command": str("/bin/x")})),
		both("blank is an empty command", map[string]any{"command": "  "}, ok(should{"command": str("")})),
		both("list keeps first", map[string]any{"command": []any{" a ", "b"}}, ok(should{"command": str("a")})),
		both("empty list is no should", map[string]any{"command": []any{}}, ok(should{})),
		both("true", map[string]any{"command": true}, delegated),
		both("null element is delegated, Ruby's NoMethodError text varies by version", map[string]any{"command": []any{nil}}, delegated),
		both("number", map[string]any{"command": float64(5)}, delegated),
		both("non-string in a list", map[string]any{"command": []any{"a", true}}, delegated),
	})
}

func TestCronSpecial(t *testing.T) {
	var cases []cronCase
	for _, s := range []string{"reboot", "yearly", "annually", "monthly", "weekly", "daily", "midnight", "hourly"} {
		cases = append(cases, both("special "+s, map[string]any{"special": s}, ok(should{"special": str(s)})))
	}
	cases = append(cases,
		both("absent", map[string]any{"special": "absent"}, ok(should{"special": absent})),
		both("case-sensitive", map[string]any{"special": "Daily"}, fail(`Parameter special failed on Cron[t]: Invalid special schedule "Daily"`)),
		both("empty", map[string]any{"special": ""}, fail(`Parameter special failed on Cron[t]: Invalid special schedule ""`)),
		both("number", map[string]any{"special": float64(5)}, fail(`Parameter special failed on Cron[t]: Invalid special schedule 5`)),
		both("list first wins", map[string]any{"special": []any{"daily", "hourly"}}, ok(should{"special": str("daily")})),
		both("list refuses any element", map[string]any{"special": []any{"daily", "x"}}, fail(`Parameter special failed on Cron[t]: Invalid special schedule "x"`)),
		both("null element", map[string]any{"special": []any{nil}}, fail(`Parameter special failed on Cron[t]: Invalid special schedule nil`)),
	)
	runCases(t, cases)
}

func TestCronEnvironment(t *testing.T) {
	invalid := func(shown string) result {
		return fail("Parameter environment failed on Cron[t]: Invalid environment setting " + shown)
	}
	runCases(t, []cronCase{
		both("one", map[string]any{"environment": "A=1"}, ok(should{"environment": list("A=1")})),
		both("spaces around", map[string]any{"environment": "  A = 1 "}, ok(should{"environment": list("  A = 1 ")})),
		both("vertical tab space", map[string]any{"environment": "\vA\v=\v1"}, ok(should{"environment": list("\vA\v=\v1")})),
		both("empty value", map[string]any{"environment": "A="}, ok(should{"environment": list("A=")})),
		both("list", map[string]any{"environment": []any{"B=2", "A=1"}}, ok(should{"environment": list("B=2", "A=1")})),
		both("empty list", map[string]any{"environment": []any{}}, ok(should{"environment": list()})),
		both("absent stays the string", map[string]any{"environment": "absent"}, ok(should{"environment": list("absent")})),
		both("no equals", map[string]any{"environment": "A"}, invalid(`"A"`)),
		both("no name", map[string]any{"environment": "=1"}, invalid(`"=1"`)),
		both("non-word name", map[string]any{"environment": "A-B=1"}, invalid(`"A-B=1"`)),
		both("non-ascii word is not a name", map[string]any{"environment": "é=1"}, invalid(`"é=1"`)),
		both("second element refused", map[string]any{"environment": []any{"A=1", "bad"}}, invalid(`"bad"`)),
		{name: "integer", params: map[string]any{"environment": float64(5)}, v130: delegated, v210: invalid("5")},
		{name: "true in a list", params: map[string]any{"environment": []any{"A=1", true}}, v130: delegated, v210: invalid("true")},
		{name: "nested list", params: map[string]any{"environment": []any{[]any{"A=1"}}}, v130: delegated, v210: invalid(`["A=1"]`)},
		{name: "refusal before delegation", params: map[string]any{"environment": []any{"bad", float64(5)}}, v130: invalid(`"bad"`), v210: invalid(`"bad"`)},
		{name: "null element", params: map[string]any{"environment": []any{nil}}, v130: delegated, v210: invalid("nil")},
		both("hash", map[string]any{"environment": map[string]any{"A": "1"}}, delegated),
	})
}

func TestCronEnsure(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		want   string
		err    string
	}{
		{"explicit present", map[string]any{"ensure": "present"}, "present", ""},
		{"explicit absent", map[string]any{"ensure": "absent"}, "absent", ""},
		{"explicit absent beats default", map[string]any{"ensure": "absent", "command": "x"}, "absent", ""},
		{"defaults to present with command", map[string]any{"command": "x"}, "present", ""},
		{"defaults to present with minute", map[string]any{"minute": "*"}, "present", ""},
		{"defaults to present with empty time list", map[string]any{"minute": []any{}}, "present", ""},
		{"defaults to present with environment", map[string]any{"environment": []any{}}, "present", ""},
		{"explicit user counts, as managed? sees it", map[string]any{"user": "root"}, "present", ""},
		{"explicit target counts", map[string]any{"target": "root"}, "present", ""},
		{"nothing set", map[string]any{}, "", ""},
		{"only the defaulted target", map[string]any{"ensure": nil}, "", ""},
		{"empty command list is no should", map[string]any{"command": []any{}}, "", ""},
		{"empty special list is no should", map[string]any{"special": []any{}}, "", ""},
		{"list first", map[string]any{"ensure": []any{"absent"}}, "absent", ""},
		{"null element", map[string]any{"ensure": []any{nil}}, "", `Parameter ensure failed on Cron[t]: Invalid value nil. Valid values are present, absent.`},
		{"role", map[string]any{"ensure": "role"}, "", `Parameter ensure failed on Cron[t]: Invalid value "role". Valid values are present, absent.`},
		{"case", map[string]any{"ensure": "Present"}, "", `Parameter ensure failed on Cron[t]: Invalid value "Present". Valid values are present, absent.`},
		{"number", map[string]any{"ensure": float64(5)}, "", `Parameter ensure failed on Cron[t]: Invalid value 5. Valid values are present, absent.`},
		{"true", map[string]any{"ensure": true}, "", `Parameter ensure failed on Cron[t]: Invalid value true. Valid values are present, absent.`},
	}
	for _, tt := range tests {
		for _, rel := range releases {
			t.Run(tt.name+"/"+rel, func(t *testing.T) {
				got, err := parseCron(cronRes(tt.params), rel, "UTF-8", fakeTarget)
				if tt.err != "" {
					if err == nil || err.Error() != tt.err {
						t.Fatalf("err = %v, want %q", err, tt.err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.Ensure != tt.want {
					t.Errorf("Ensure = %q, want %q", got.Ensure, tt.want)
				}
			})
		}
	}
}

func TestCronUserAndTarget(t *testing.T) {
	runCases(t, []cronCase{
		both("none: target defaults, user unset", map[string]any{}, ok(should{"target": str("tgt")})),
		both("user sets the target default", map[string]any{"user": "bin"}, ok(should{"user": str("bin"), "target": str("bin")})),
		both("explicit target kept", map[string]any{"user": "u", "target": "tt"}, ok(should{"user": str("u"), "target": str("tt")})),
		both("target alone", map[string]any{"target": "tt"}, ok(should{"target": str("tt")})),
		both("empty user is still a user", map[string]any{"user": ""}, ok(should{"user": str(""), "target": str("")})),
		both("list user first", map[string]any{"user": []any{"a", "b"}}, ok(should{"user": str("a"), "target": str("a")})),
		both("null user element is no user, and no target default replaces a null target", map[string]any{"user": []any{nil}, "target": []any{nil}}, ok(should{})),
		both("null user element leaves the target default", map[string]any{"user": []any{nil}}, ok(should{"target": str("tgt")})),
		both("null after the first user element", map[string]any{"user": []any{"a", nil}}, ok(should{"user": str("a"), "target": str("a")})),
		both("non-string user", map[string]any{"user": float64(5)}, delegated),
		both("non-string target", map[string]any{"target": true}, delegated),
	})
}

func TestCronDefaultTarget(t *testing.T) {
	calls := 0
	def := func() (string, error) { calls++; return "me", nil }
	for _, tt := range []struct {
		name   string
		params map[string]any
		calls  int
	}{{"neither", map[string]any{}, 1}, {"user", map[string]any{"user": "u"}, 0}, {"target", map[string]any{"target": "t"}, 0}} {
		calls = 0
		if _, err := parseCron(cronRes(tt.params), "2.1.0", "UTF-8", def); err != nil {
			t.Fatal(err)
		}
		if calls != tt.calls {
			t.Errorf("%s: defaultTarget called %d times, want %d", tt.name, calls, tt.calls)
		}
	}
	boom := errors.New("nss down")
	if _, err := parseCron(cronRes(nil), "2.1.0", "UTF-8", func() (string, error) { return "", boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the NSS error", err)
	}
	// A refusal comes before the default is consulted.
	if _, err := parseCron(cronRes(map[string]any{"minute": "x"}), "2.1.0", "UTF-8", func() (string, error) { return "", boom }); errors.Is(err, boom) {
		t.Errorf("default consulted before minute was refused")
	}
}

func TestCronSpecialConflict(t *testing.T) {
	conflict := func(field string) result {
		return fail(fmt.Sprintf("Validation of Cron[t] failed: Cron[t] cannot specify both a special schedule and a value for %s", field))
	}
	runCases(t, []cronCase{
		both("minute", map[string]any{"special": "daily", "minute": "5"}, conflict("minute")),
		both("hour", map[string]any{"special": "daily", "hour": "5"}, conflict("hour")),
		both("weekday", map[string]any{"special": "daily", "weekday": "5"}, conflict("weekday")),
		both("monthday", map[string]any{"special": "daily", "monthday": "5"}, conflict("monthday")),
		both("month", map[string]any{"special": "daily", "month": "5"}, conflict("month")),
		both("empty list conflicts", map[string]any{"special": "daily", "minute": []any{}}, conflict("minute")),
		both("check order is minute hour weekday monthday month", map[string]any{"special": "daily", "month": "1", "monthday": "1", "weekday": "1"}, conflict("weekday")),
		both("monthday before month", map[string]any{"special": "daily", "month": "1", "monthday": "1"}, conflict("monthday")),
		both("absent field is fine", map[string]any{"special": "daily", "minute": "*", "hour": "absent"}, ok(should{"special": str("daily"), "minute": absent, "hour": absent})),
		both("special absent allows values", map[string]any{"special": "absent", "minute": "5"}, ok(should{"special": absent, "minute": list("5")})),
		both("no special allows values", map[string]any{"minute": "5"}, ok(should{"minute": list("5")})),
		both("attribute failure comes first", map[string]any{"special": "daily", "minute": "5", "hour": "99"}, fail(munge("hour", `"99"`, "99"))),
	})
}

func TestCronFirstFailureOrder(t *testing.T) {
	// Ruby's allattrs order: ensure, command, special, minute, hour, weekday,
	// month, monthday, environment, user, target (type.rb:57-410).
	steps := []struct {
		attr  string
		value any
		text  string
	}{
		{"ensure", "bad", `Parameter ensure failed on Cron[t]: Invalid value "bad". Valid values are present, absent.`},
		{"special", "bad", `Parameter special failed on Cron[t]: Invalid special schedule "bad"`},
		{"minute", "bad", munge("minute", `"bad"`, "bad")},
		{"hour", "bad", munge("hour", `"bad"`, "bad")},
		{"weekday", "bad", munge("weekday", `"bad"`, "bad")},
		{"month", "bad", munge("month", `"bad"`, "bad")},
		{"monthday", "bad", munge("monthday", `"bad"`, "bad")},
		{"environment", "bad", "Parameter environment failed on Cron[t]: Invalid environment setting \"bad\""},
	}
	for _, rel := range releases {
		for i := range steps {
			params := map[string]any{}
			// Set every attribute from i onward to a bad value; the first one in
			// order is reported whatever the map order.
			for _, s := range steps[i:] {
				params[s.attr] = s.value
			}
			_, err := parseCron(cronRes(params), rel, "UTF-8", fakeTarget)
			if err == nil || err.Error() != steps[i].text {
				t.Errorf("%s from %s: err = %v, want %q", rel, steps[i].attr, err, steps[i].text)
			}
		}
	}
	// A refusal comes before a delegation that follows it in order, and the
	// reverse.
	_, err := parseCron(cronRes(map[string]any{"command": true, "special": "bad"}), "2.1.0", "UTF-8", fakeTarget)
	var u *rubyvalue.UnimplementedValue
	if !errors.As(err, &u) {
		t.Errorf("command true before special bad: err = %v, want UnimplementedValue", err)
	}
	_, err = parseCron(cronRes(map[string]any{"ensure": "bad", "command": true}), "2.1.0", "UTF-8", fakeTarget)
	if err == nil || errors.As(err, &u) {
		t.Errorf("ensure bad before command true: err = %v, want the ensure refusal", err)
	}
}

// TestCronRefusalEncoding: Ruby's inspect escapes what Encoding.default_external
// cannot show (M12: minute => "１" under US-ASCII).
func TestCronRefusalEncoding(t *testing.T) {
	tests := []struct {
		name, attr, value, encoding, want string
	}{
		{"minute utf-8", "minute", "１", "UTF-8", munge("minute", `"１"`, "１")},
		{"minute us-ascii", "minute", "１", "US-ASCII", munge("minute", `"\uFF11"`, "１")},
		{"special us-ascii", "special", "é", "US-ASCII", `Parameter special failed on Cron[t]: Invalid special schedule "\u00E9"`},
		{"special utf-8", "special", "é", "UTF-8", `Parameter special failed on Cron[t]: Invalid special schedule "é"`},
		{"ensure us-ascii", "ensure", "😀", "US-ASCII", `Parameter ensure failed on Cron[t]: Invalid value "\u{1F600}". Valid values are present, absent.`},
		{"environment us-ascii", "environment", "é", "US-ASCII", `Parameter environment failed on Cron[t]: Invalid environment setting "\u00E9"`},
	}
	for _, tt := range tests {
		for _, rel := range releases {
			t.Run(tt.name+"/"+rel, func(t *testing.T) {
				r := cronRes(map[string]any{tt.attr: tt.value})
				_, err := parseCron(r, rel, tt.encoding, fakeTarget)
				if err == nil || err.Error() != tt.want {
					t.Errorf("err = %v, want %q", err, tt.want)
				}
				verr := Cron{release: rel, encoding: tt.encoding}.Validate(r)
				if verr == nil || verr.Error() != tt.want {
					t.Errorf("Validate = %v, want %q", verr, tt.want)
				}
			})
		}
	}
}

func TestCronSensitiveValuesAreRedacted(t *testing.T) {
	r := cronRes(map[string]any{"minute": "bad"})
	r.SensitiveParameters = []string{"minute"}
	_, err := parseCron(r, "2.1.0", "UTF-8", fakeTarget)
	want := "Parameter minute failed on Cron[t]: Munging failed for value [redacted] in class minute: [redacted] is not a valid minute"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestCronTitleAndName(t *testing.T) {
	p, err := parseCron(cronRes(map[string]any{"name": "n"}), "2.1.0", "UTF-8", fakeTarget)
	if err != nil || p.Name != "n" || p.Title != "t" {
		t.Errorf("got %+v, %v; want Name n, Title t", p, err)
	}
	p, err = parseCron(cronRes(nil), "2.1.0", "UTF-8", fakeTarget)
	if err != nil || p.Name != "t" || p.Title != "t" {
		t.Errorf("got %+v, %v; want Name t, Title t", p, err)
	}
}

func TestCronNullTargetElementGetsNoDefault(t *testing.T) {
	p, err := parseCron(cronRes(map[string]any{"target": []any{nil}}), "2.1.0", "UTF-8", fakeTarget)
	if err != nil {
		t.Fatal(err)
	}
	if v, set := p.Should["target"]; set {
		t.Errorf("target = %#v, want no should (Ruby: target=nil)", v)
	}
}
