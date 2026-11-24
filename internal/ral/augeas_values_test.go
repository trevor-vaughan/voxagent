package ral

import "testing"

const sshdConfigFile = "/etc/ssh/sshd_config"

// sshdValueRun opens 07b's sshd_config handle (trace lines 21-29) and replays
// calls after it, then the handle's close.
func sshdValueRun(t *testing.T, calls []traceCall) *rubyAug {
	t.Helper()
	all := append(traceLines(t, "07b", [2]int{21, 29}), calls...)
	all = append(all, parseTrace(t, "close", "h2 aug_close\n")...)
	r := testAugeasRun(t, all, augeasCaseLibdir, gateLoadpath, sshdConfigFile)
	t.Cleanup(r.EndRun)
	aug, err := r.augopen(sshdConfigLens, sshdConfigFile, "$target/*[label()=~regexp('PermitRootLogin', 'i')]")
	if err != nil {
		t.Fatal(err)
	}
	return aug
}

// 07b: sshd_config's new key goes after its commented-out line, matched
// case-insensitively, and $resource is not rebound.
func TestSetSimpleValueSshdAfterComment(t *testing.T) {
	aug := sshdValueRun(t, traceLines(t, "07b", [2]int{42, 46}))
	if err := setSimpleValue(aug, "$target", "$target/PermitRootLogin", "PermitRootLogin", []any{"no"}, ", 'i'", true); err != nil {
		t.Fatal(err)
	}
}

// Without a commented-out line, sshd_config inserts before the first Match
// when there is one, else after the last node.
func TestSetSimpleValueSshdWithoutComment(t *testing.T) {
	for _, tc := range []struct {
		name, match, insert string
	}{
		{"no Match", `h2 aug_match "$target/Match" => 0`, `h2 aug_insert "$target/*" "X11Forwarding" 0 => -1`},
		{"Match", `h2 aug_match "$target/Match" => 1 "/files/etc/ssh/sshd_config/Match"`, `h2 aug_insert "$target/Match[1]" "X11Forwarding" 1 => 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			aug := sshdValueRun(t, parseTrace(t, "script", `h2 aug_match "$target/X11Forwarding" => 0
h2 aug_match "$target/#comment[.=~regexp('X11Forwarding([^a-z.].*)?', 'i')]" => 0
`+tc.match+"\n"+tc.insert+`
h2 aug_set "$target/X11Forwarding[last()]" "no" => 0
h2 aug_match "$target/X11Forwarding[last()]" => 1 "/files/etc/ssh/sshd_config/X11Forwarding"
`))
			if err := setSimpleValue(aug, "$target", "$target/X11Forwarding", "X11Forwarding", []any{"no"}, ", 'i'", true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Existing entries take the values in order and surplus entries are removed;
// a value beyond them goes after the last one set; ssh_config then rebinds
// $resource to the path.
func TestSetSimpleValueExistingEntries(t *testing.T) {
	aug := sshdValueRun(t, parseTrace(t, "script", `h2 aug_match "$target/Host[.='*']/SendEnv" => 2 "/files/etc/ssh/sshd_config/Host/SendEnv[1]" "/files/etc/ssh/sshd_config/Host/SendEnv[2]"
h2 aug_set "/files/etc/ssh/sshd_config/Host/SendEnv[1]" "A" => 0
h2 aug_set "/files/etc/ssh/sshd_config/Host/SendEnv[2]" "B" => 0
h2 aug_insert "/files/etc/ssh/sshd_config/Host/SendEnv[2]" "SendEnv" 0 => 0
h2 aug_set "$target/Host[.='*']/SendEnv[last()]" "C" => 0
h2 aug_match "$target/Host[.='*']/SendEnv[last()]" => 0
h2 aug_match "$target/Host[.='*']/#comment[.=~regexp('SendEnv([^a-z.].*)?')]" => 1 "/files/etc/ssh/sshd_config/Host/#comment"
h2 aug_insert "/files/etc/ssh/sshd_config/Host/#comment" "SendEnv" 0 => 0
h2 aug_set "$target/Host[.='*']/SendEnv[last()]" "D" => 0
h2 aug_match "$target/Host[.='*']/SendEnv[last()]" => 1 "/files/etc/ssh/sshd_config/Host/SendEnv[3]"
h2 aug_defvar "resource" "$target/Host[.='*']/SendEnv" => 1
h2 aug_match "$target/Host[.='*']/Compression" => 2 "/files/etc/ssh/sshd_config/Host/Compression[1]" "/files/etc/ssh/sshd_config/Host/Compression[2]"
h2 aug_set "/files/etc/ssh/sshd_config/Host/Compression[1]" "no" => 0
h2 aug_rm "/files/etc/ssh/sshd_config/Host/Compression[2]" => 1
h2 aug_defvar "resource" "$target/Host[.='*']/Compression" => 1
`))
	// An empty [last()] match leaves no node to insert after, so the next
	// value goes after a commented-out line, as Ruby's nil lastsp does.
	if err := setSimpleValue(aug, "$target/Host[.='*']", "$target/Host[.='*']/SendEnv", "SendEnv", []any{"A", "B", "C", "D"}, "", false); err != nil {
		t.Fatal(err)
	}
	if err := setSimpleValue(aug, "$target/Host[.='*']", "$target/Host[.='*']/Compression", "Compression", []any{"no"}, "", false); err != nil {
		t.Fatal(err)
	}
}

// 07b: sshd_config's array key is first inserted after the last node (no
// Match), then rewritten.
func TestSetArrayValueSshd(t *testing.T) {
	aug := sshdValueRun(t, traceLines(t, "07b", [2]int{71, 76}))
	if err := setArrayValue(aug, "$target", "$target/AllowUsers", "AllowUsers", []any{"alice", "bob"}, true); err != nil {
		t.Fatal(err)
	}
}

func TestSetArrayValueSshdBeforeMatch(t *testing.T) {
	aug := sshdValueRun(t, parseTrace(t, "script", `h2 aug_match "$target/Match" => 1 "/files/etc/ssh/sshd_config/Match"
h2 aug_insert "$target/Match[1]" "AllowUsers" 1 => 0
h2 aug_rm "$target/AllowUsers/*" => 0
h2 aug_rm "$target/AllowUsers[position() != 1]" => 0
h2 aug_set "$target/AllowUsers/1" "alice" => 0
`))
	if err := setArrayValue(aug, "$target", "$target/AllowUsers", "AllowUsers", []any{"alice"}, true); err != nil {
		t.Fatal(err)
	}
}

// A failing match is ruby-augeas' raise, returned.
func TestSetValueMatchFailure(t *testing.T) {
	aug := sshdValueRun(t, parseTrace(t, "script", `h2 aug_match "$target/Match" => -1
h2 aug_match "$target/X" => -1
h2 aug_match "$target/Y" => -1
`))
	if err := setArrayValue(aug, "$target", "$target/A", "A", []any{"a"}, true); err == nil {
		t.Error("setArrayValue: no error")
	}
	if err := setSimpleValue(aug, "$target", "$target/X", "X", []any{"a"}, "", false); err == nil {
		t.Error("setSimpleValue: no error")
	}
	if _, err := getValue(aug, "$target/Y"); err == nil {
		t.Error("getValue: no error")
	}
}

// A nil value (an array_append union keeping a valueless element) consumes an
// existing entry by removing it, and is written valueless beyond them.
func TestSetValueNil(t *testing.T) {
	aug := sshdValueRun(t, parseTrace(t, "script", `h2 aug_match "$target/Banner" => 2 "/files/etc/ssh/sshd_config/Banner[1]" "/files/etc/ssh/sshd_config/Banner[2]"
h2 aug_rm "/files/etc/ssh/sshd_config/Banner[1]" => 1
h2 aug_set "/files/etc/ssh/sshd_config/Banner[2]" "a" => 0
h2 aug_insert "/files/etc/ssh/sshd_config/Banner[2]" "Banner" 0 => 0
h2 aug_set "$target/Banner[last()]" NULL => 0
h2 aug_match "$target/Banner[last()]" => 1 "/files/etc/ssh/sshd_config/Banner[2]"
h2 aug_match "$target/Match" => 0
h2 aug_insert "$target/*" "AllowUsers" 0 => -1
h2 aug_rm "$target/AllowUsers/*" => 0
h2 aug_rm "$target/AllowUsers[position() != 1]" => 0
h2 aug_set "$target/AllowUsers/1" NULL => 0
h2 aug_set "$target/AllowUsers/2" "bob" => 0
`))
	if err := setSimpleValue(aug, "$target", "$target/Banner", "Banner", []any{nil, "a", nil}, ", 'i'", true); err != nil {
		t.Fatal(err)
	}
	if err := setArrayValue(aug, "$target", "$target/AllowUsers", "AllowUsers", []any{nil, "bob"}, true); err != nil {
		t.Fatal(err)
	}
}
