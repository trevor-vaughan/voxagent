package ral

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/raltest"
)

var _ Provider = SshdConfig{}

func sshdConfigOver(run *augeasRun) []Provider { return []Provider{SshdConfig{run: run}} }

// sshdRes is a measured Sshd_config: the default target and the given
// parameters.
func sshdRes(title string, params map[string]any) catalog.Resource {
	return raltest.AugRes("Sshd_config", title, params)
}

// M18: three keys created (next to a commented-out line, or at the end) and
// one changed.
func TestSshdConfigReplays03a(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03a"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("PermitRootLogin", map[string]any{"value": "no"}),
		sshdRes("X11Forwarding", map[string]any{"value": "no"}),
		sshdRes("ClientAliveInterval", map[string]any{"value": "300"}),
		sshdRes("UsePAM", map[string]any{"value": "yes"}),
	}, false)
	wantNoErrors(t, got)
	for _, r := range got[:3] {
		wantEnsureChange(t, r, "absent", "present", "sshd_config_created", "created")
	}
	wantValueChange(t, got[3], []any{"no"}, []any{"yes"}, "value changed ['no'] to ['yes']")
}

// M20: array keys new to the stock file are created at its end, though the
// first insert fails (several nodes match "$target/*").
func TestSshdConfigReplays03c(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03c"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("AllowUsers", map[string]any{"value": []any{"alice", "bob"}}),
		sshdRes("Ciphers", map[string]any{"value": []any{"aes256-ctr", "aes128-ctr"}}),
		sshdRes("AcceptEnv", map[string]any{"value": []any{"LANG", "LC_*"}}),
	}, false)
	wantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "sshd_config_created", "created")
	}
}

// M22: existing array keys are rewritten.
func TestSshdConfigReplays03e(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03e"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("AllowUsers", map[string]any{"value": []any{"alice", "bob"}}),
		sshdRes("AuthorizedKeysFile", map[string]any{"value": []any{".ssh/authorized_keys", "/etc/ssh/authorized_keys/%u"}}),
		sshdRes("Ciphers", map[string]any{"value": []any{"aes256-ctr", "aes128-ctr"}}),
	}, false)
	wantNoErrors(t, got)
	wantMessages(t, got,
		[]string{"value changed ['root'] to ['alice', 'bob']"},
		[]string{"value changed ['.ssh/authorized_keys'] to ['.ssh/authorized_keys', '/etc/ssh/authorized_keys/%u']"},
		[]string{"value changed ['aes128-ctr', 'aes192-ctr'] to ['aes256-ctr', 'aes128-ctr']"},
	)
	wantValueChange(t, got[0], []any{"root"}, []any{"alice", "bob"}, "value changed ['root'] to ['alice', 'bob']")
}

func sshd03f() []catalog.Resource {
	return []catalog.Resource{sshdRes("AllowUsers", map[string]any{"value": []any{"alice", "bob"}, "array_append": true})}
}

// M23: array_append sets the union of is and should; the message shows the
// union retrieved after the sync while desired_value stays the should.
func TestSshdConfigReplays03f(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03f"), []string{sshdConfigFile}, sshdConfigOver, sshd03f(), false)
	wantNoErrors(t, got)
	wantValueChange(t, got[0], []any{"root", "alice"}, []any{"alice", "bob"}, "value changed ['root', 'alice'] to ['root', 'alice', 'bob']")
}

// 03f under noop: exists?, the retrieve, and should_to_s's retrieve only.
func TestSshdConfigReplays03fNoop(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	calls := raltest.TraceLines(t, "03f", [2]int{4, 35}, [2]int{72, 72})
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, sshdConfigOver, sshd03f(), true)
	wantNoErrors(t, got)
	wantValueChange(t, got[0], []any{"root", "alice"}, []any{"alice", "bob"}, "value changed ['root', 'alice'] to ['root', 'alice', 'bob'] (noop)")
}

// array_append in sync when every should is already there, nil included.
func TestSshdConfigArrayAppendInSync(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	calls := append(raltest.TraceLines(t, "03f", [2]int{4, 22}), raltest.ParseTrace(t, "script", `h1 aug_get "/files/etc/ssh/sshd_config/AllowUsers/1" => 1 NULL
h1 aug_get "/files/etc/ssh/sshd_config/AllowUsers/2" => 1 "alice"
h1 aug_close
`)...)
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("AllowUsers", map[string]any{"value": []any{"alice"}, "array_append": true}),
	}, false)
	wantNoErrors(t, got)
	if len(got[0].changes) != 0 {
		t.Errorf("changes = %+v", got[0].changes)
	}
}

// array_append's union keeps a valueless element (nil, shown undef), and the
// array setter writes it as a valueless child.
func TestSshdConfigArrayAppendNil(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sshd_config": true})
	retrieve := `h1 aug_match "/augeas/load/Sshd" => 1 "/augeas/load/Sshd"
h1 aug_match "/augeas/load/Sshd/incl[.='/etc/ssh/sshd_config']" => 1 "/augeas/load/Sshd/incl"
h1 aug_match "/files/etc/ssh/sshd_config" => 1 "/files/etc/ssh/sshd_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/sshd_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/sshd_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/*[label()=~regexp('AllowUsers', 'i')]" => 1
h1 aug_match "$resource" => 1 "/files/etc/ssh/sshd_config/AllowUsers"
h1 aug_match "/files/etc/ssh/sshd_config/AllowUsers/*[label()=~regexp('[0-9]*')]" => 2 "/files/etc/ssh/sshd_config/AllowUsers/1" "/files/etc/ssh/sshd_config/AllowUsers/2"
h1 aug_match "/files/etc/ssh/sshd_config/AllowUsers/*" => 2 "/files/etc/ssh/sshd_config/AllowUsers/1" "/files/etc/ssh/sshd_config/AllowUsers/2"
h1 aug_get "/files/etc/ssh/sshd_config/AllowUsers/1" => 1 NULL
h1 aug_get "/files/etc/ssh/sshd_config/AllowUsers/2" => 1 "root"
`
	calls := append(raltest.TraceLines(t, "03f", [2]int{4, 13}), raltest.ParseTrace(t, "script", retrieve+retrieve+`h1 aug_match "/augeas/load/Sshd" => 1 "/augeas/load/Sshd"
h1 aug_match "/augeas/load/Sshd/incl[.='/etc/ssh/sshd_config']" => 1 "/augeas/load/Sshd/incl"
h1 aug_match "/files/etc/ssh/sshd_config" => 1 "/files/etc/ssh/sshd_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/sshd_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/sshd_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/*[label()=~regexp('AllowUsers', 'i')]" => 1
h2 aug_init NULL NULL 64
h2 aug_set "/input" "AllowUsers FOO\n" => 0
h2 aug_text_store "Sshd.lns" "/input" "/parsed" => 0
h2 aug_match "/parsed/AllowUsers/1" => 1 "/parsed/AllowUsers/1"
h2 aug_close
h1 aug_match "$target/Match" => 0
h1 aug_insert "$target/*" "AllowUsers" 0 => -1
h1 aug_rm "$target/*[label()=~regexp('AllowUsers', 'i')]/*" => 2
h1 aug_rm "$target/*[label()=~regexp('AllowUsers', 'i')][position() != 1]" => 0
h1 aug_set "$target/*[label()=~regexp('AllowUsers', 'i')]/1" NULL => 0
h1 aug_set "$target/*[label()=~regexp('AllowUsers', 'i')]/2" "root" => 0
h1 aug_set "$target/*[label()=~regexp('AllowUsers', 'i')]/3" "bob" => 0
`+retrieve+`h1 aug_save => 0
h1 aug_load => 0
h1 aug_load => 0
h1 aug_close
`)...)
	got := runAugeasCase(t, calls, []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("AllowUsers", map[string]any{"value": []any{"root", "bob"}, "array_append": true}),
	}, false)
	wantNoErrors(t, got)
	// The scripted re-retrieve still reads [nil, root], so the union shown is
	// that with the should.
	wantValueChange(t, got[0], []any{nil, "root"}, []any{"root", "bob"}, "value changed [undef, 'root'] to [undef, 'root', 'bob']")
}

// M24: a new Match block per condition, created after the last node with its
// condition entries, the key inside its Settings.
func TestSshdConfigReplays03g(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03g"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("PasswordAuthentication match", map[string]any{"key": "PasswordAuthentication", "value": "no", "condition": "Host example.net User root"}),
		sshdRes("AllowUsers match", map[string]any{"key": "AllowUsers", "value": []any{"alice", "bob"}, "condition": "User carol"}),
	}, false)
	wantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "sshd_config_created", "created")
	}
}

// M25: keys inside existing Match blocks change, and one is created in an
// existing block (no new Match).
func TestSshdConfigReplays03h(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03h"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("PasswordAuthentication match", map[string]any{"key": "PasswordAuthentication", "value": "no", "condition": "Host example.net User root"}),
		sshdRes("X11Forwarding match", map[string]any{"key": "X11Forwarding", "value": "yes", "condition": "User bob"}),
		sshdRes("Banner match", map[string]any{"key": "Banner", "value": "/etc/issue", "condition": "User bob"}),
	}, false)
	wantNoErrors(t, got)
	wantMessages(t, got, []string{"value changed ['yes'] to ['no']"}, []string{"value changed ['no'] to ['yes']"}, []string{"created"})
	wantEnsureChange(t, got[2], "absent", "present", "sshd_config_created", "created")
}

// M26-M28: ensure absent removes keys, top-level and in a Match (and the
// emptied Match); an absent key has nothing to manage.
func TestSshdConfigReplays03k(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03k"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("PermitRootLogin", map[string]any{"ensure": "absent"}),
		sshdRes("Nothere", map[string]any{"ensure": "absent"}),
		sshdRes("X11Forwarding match", map[string]any{"ensure": "absent", "key": "X11Forwarding", "condition": "User bob"}),
		sshdRes("UsePrivilegeSeparation", map[string]any{"ensure": "absent", "value": "sandbox"}),
	}, false)
	wantNoErrors(t, got)
	for _, i := range []int{0, 2, 3} {
		wantEnsureChange(t, got[i], "present", "absent", "sshd_config_removed", "removed")
	}
	if len(got[1].changes) != 0 {
		t.Errorf("Nothere: changes %+v", got[1].changes)
	}
	if d := got[1].debugAt("/Stage[main]/Main/" + got[1].ref); !slices.Equal(d, []string{"Nothing to manage: no ensure and the resource doesn't exist"}) {
		t.Errorf("Nothere debug = %q", d)
	}
}

// M29: no value and no ensure manages nothing; the next key is created.
func TestSshdConfigReplays03l(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{})
	got := runAugeasCase(t, raltest.LoadTrace(t, "03l"), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("AllowUsers", nil),
		sshdRes("PermitRootLogin", map[string]any{"value": "no"}),
	}, false)
	wantNoErrors(t, got)
	if len(got[0].changes) != 0 {
		t.Errorf("AllowUsers: changes %+v", got[0].changes)
	}
	if d := got[0].debugAt("/Stage[main]/Main/" + got[0].ref); !slices.Equal(d, []string{"Nothing to manage: no ensure and the resource doesn't exist"}) {
		t.Errorf("AllowUsers debug = %q", d)
	}
	wantEnsureChange(t, got[1], "absent", "present", "sshd_config_created", "created")
}

func TestSshdBasePath(t *testing.T) {
	for _, c := range []struct {
		cond []condPair
		want string
	}{
		{nil, "$target"},
		{[]condPair{{"User", "carol"}}, `$target/Match[count(Condition/*)=1][Condition/User="carol"]/Settings`},
		{[]condPair{{"Host", "example.net"}, {"User", "root"}}, `$target/Match[count(Condition/*)=2][Condition/Host="example.net"][Condition/User="root"]/Settings`},
	} {
		if got := sshdBasePath(sshdConfigParams{Condition: c.cond}); got != c.want {
			t.Errorf("sshdBasePath(%v) = %s, want %s", c.cond, got, c.want)
		}
	}
	p, err := parseSshdConfig(sshdRes("x", map[string]any{"key": "Banner", "value": "a", "condition": "Host example.net User root"}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sshdBasePath(p), `$target/Match[count(Condition/*)=2][Condition/Host="example.net"][Condition/User="root"]/Settings`; got != want {
		t.Errorf("sshdBasePath = %s, want %s", got, want)
	}
}

// sshdOpen is the augopen of an already loaded handle on the default target,
// binding $resource to key's top-level path.
func sshdOpen(key string) string {
	return `h1 aug_match "/augeas/load/Sshd" => 1 "/augeas/load/Sshd"
h1 aug_match "/augeas/load/Sshd/incl[.='/etc/ssh/sshd_config']" => 1 "/augeas/load/Sshd/incl"
h1 aug_match "/files/etc/ssh/sshd_config" => 1 "/files/etc/ssh/sshd_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/sshd_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/sshd_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/*[label()=~regexp('` + key + `', 'i')]" => 0
`
}

// sshdCreatePlain is exists? (absent) and create's set_value for a plain key
// whose node the earlier insert placed, so set_simple_value sets it in place,
// then the save.
func sshdCreatePlain(key, value, preamble string) string {
	return sshdOpen(key) + `h1 aug_match "$resource" => 0
` + sshdOpen(key) + preamble + `h2 aug_init NULL NULL 64
h2 aug_set "/input" "` + key + ` FOO\n" => 0
h2 aug_text_store "Sshd.lns" "/input" "/parsed" => 0
h2 aug_match "/parsed/` + key + `/1" => 0
h2 aug_close
h1 aug_match "$target/` + key + `" => 1 "/files/etc/ssh/sshd_config/` + key + `"
h1 aug_set "/files/etc/ssh/sshd_config/` + key + `" "` + value + `" => 0
h1 aug_save => 0
h1 aug_load => 0
h1 aug_load => 0
`
}

// SDP:185-189: Port goes before the first ListenAddress, ListenAddress after
// AddressFamily, AddressFamily before ListenAddress, each only when the other
// exists, the key compared case-insensitively.
func TestSshdPortListenAddressInserts(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sshd_config": true})
	script := strings.Join([]string{
		`h1 aug_init NULL NULL 64
h1 aug_match "/augeas/load/Sshd" => 0
h1 aug_set "/augeas/load/Sshd/lens" "Sshd.lns" => 0
h1 aug_set "/augeas/load/Sshd/incl[last()+1]" "/etc/ssh/sshd_config" => 0
h1 aug_load => 0
h1 aug_match "/files/etc/ssh/sshd_config" => 1 "/files/etc/ssh/sshd_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/sshd_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/sshd_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/*[label()=~regexp('port', 'i')]" => 0
h1 aug_match "$resource" => 0
`,
		sshdOpen("port"),
		`h1 aug_match "$target/ListenAddress" => 1 "/files/etc/ssh/sshd_config/ListenAddress"
h1 aug_insert "$target/ListenAddress[1]" "port" 1 => 0
h2 aug_init NULL NULL 64
h2 aug_set "/input" "port FOO\n" => 0
h2 aug_text_store "Sshd.lns" "/input" "/parsed" => 0
h2 aug_match "/parsed/port/1" => 0
h2 aug_close
h1 aug_match "$target/port" => 1 "/files/etc/ssh/sshd_config/port"
h1 aug_set "/files/etc/ssh/sshd_config/port" "2222" => 0
h1 aug_save => 0
h1 aug_load => 0
h1 aug_load => 0
`,
		sshdCreatePlain("LISTENADDRESS", "0.0.0.0", `h1 aug_match "$target/AddressFamily" => 1 "/files/etc/ssh/sshd_config/AddressFamily"
h1 aug_insert "$target/AddressFamily" "LISTENADDRESS" 0 => 0
`),
		sshdCreatePlain("AddressFamily", "inet", `h1 aug_match "$target/ListenAddress" => 1 "/files/etc/ssh/sshd_config/ListenAddress"
h1 aug_insert "$target/ListenAddress" "AddressFamily" 1 => 0
`),
		"h1 aug_close\n",
	}, "")
	got := runAugeasCase(t, raltest.ParseTrace(t, "script", script), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("port", map[string]any{"value": "2222"}),
		sshdRes("LISTENADDRESS", map[string]any{"value": "0.0.0.0"}),
		sshdRes("AddressFamily", map[string]any{"value": "inet"}),
	}, false)
	wantNoErrors(t, got)
	for _, r := range got {
		wantEnsureChange(t, r, "absent", "present", "sshd_config_created", "created")
	}
}

// Without the other key, Port gets no insert of its own and is placed as any
// new key.
func TestSshdPortWithoutListenAddress(t *testing.T) {
	withAugVersionMemo(t, map[string]bool{"Sshd_config": true})
	script := `h1 aug_init NULL NULL 64
h1 aug_match "/augeas/load/Sshd" => 0
h1 aug_set "/augeas/load/Sshd/lens" "Sshd.lns" => 0
h1 aug_set "/augeas/load/Sshd/incl[last()+1]" "/etc/ssh/sshd_config" => 0
h1 aug_load => 0
h1 aug_match "/files/etc/ssh/sshd_config" => 1 "/files/etc/ssh/sshd_config"
h1 aug_set "/augeas/context" "/files/etc/ssh/sshd_config" => 0
h1 aug_defnode "target" "/files/etc/ssh/sshd_config" NULL NULL => 1
h1 aug_defvar "resource" "$target/*[label()=~regexp('Port', 'i')]" => 0
h1 aug_match "$resource" => 0
` + sshdOpen("Port") + `h1 aug_match "$target/ListenAddress" => 0
h2 aug_init NULL NULL 64
h2 aug_set "/input" "Port FOO\n" => 0
h2 aug_text_store "Sshd.lns" "/input" "/parsed" => 0
h2 aug_match "/parsed/Port/1" => 0
h2 aug_close
h1 aug_match "$target/Port" => 0
h1 aug_match "$target/#comment[.=~regexp('Port([^a-z.].*)?', 'i')]" => 1 "/files/etc/ssh/sshd_config/#comment[2]"
h1 aug_insert "/files/etc/ssh/sshd_config/#comment[2]" "Port" 0 => 0
h1 aug_set "$target/Port[last()]" "2222" => 0
h1 aug_match "$target/Port[last()]" => 1 "/files/etc/ssh/sshd_config/Port"
h1 aug_save => 0
h1 aug_load => 0
h1 aug_load => 0
h1 aug_close
`
	got := runAugeasCase(t, raltest.ParseTrace(t, "script", script), []string{sshdConfigFile}, sshdConfigOver, []catalog.Resource{
		sshdRes("Port", map[string]any{"value": "2222"}),
	}, false)
	wantNoErrors(t, got)
	wantEnsureChange(t, got[0], "absent", "present", "sshd_config_created", "created")
}

func TestSshdConfigRefreshAndPrefetch(t *testing.T) {
	s := SshdConfig{}
	if ok, err := s.Refresh(t.Context(), catalog.Resource{}, false); ok || err != nil {
		t.Errorf("Refresh = %v, %v", ok, err)
	}
	p, err := s.Prefetch(t.Context(), nil)
	if err != nil || !reflect.DeepEqual(p, Provider(s)) {
		t.Errorf("Prefetch = %v, %v", p, err)
	}
}
