package sshkeyscore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/voxagent/voxagent/internal/catalog"
	"github.com/voxagent/voxagent/internal/lib/asuser"
	"github.com/voxagent/voxagent/internal/lib/nameservice"
	"github.com/voxagent/voxagent/internal/lib/parsedfile"
	"github.com/voxagent/voxagent/internal/lib/raltest"
	"github.com/voxagent/voxagent/internal/ral"
	"github.com/voxagent/voxagent/internal/report"
	"github.com/voxagent/voxagent/internal/runlog"
	"github.com/voxagent/voxagent/internal/transaction"
)

var _ ral.TurnPrefetching = Sak{}

// startCall is one start of the fake helper.
type startCall struct {
	cred  *syscall.Credential
	args  []string
	stdin string
}

// fakeStarter plays the asuser helper. An op is answered from answers, keyed
// "<op> <path>", or failed from errs under the same key; otherwise it is
// performed with asuser's own ops, as the child would (as the test's user).
type fakeStarter struct {
	calls   []startCall
	answers map[string]asuser.Result
	errs    map[string]error
}

func (f *fakeStarter) Start(ctx context.Context, cred *syscall.Credential, args []string, stdin []byte) (asuser.Result, error) {
	f.calls = append(f.calls, startCall{cred, slices.Clone(args), string(stdin)})
	key := args[0] + " " + args[len(args)-1]
	if err := f.errs[key]; err != nil {
		return asuser.Result{}, err
	}
	if r, ok := f.answers[key]; ok {
		return r, nil
	}
	switch args[0] {
	case "mkdir":
		return asuser.Mkdir(args[1]), nil
	case "chmod":
		return asuser.Chmod(args[1]), nil
	case "write":
		return asuser.Write(ctx, args[3], args[2], string(stdin), func(context.Context, string) (string, error) { return "", nil })
	}
	return asuser.Result{}, fmt.Errorf("fake helper: unknown op %q", args)
}

// ops lists the calls as "<op> <path> as <uid>".
func (f *fakeStarter) ops() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, fmt.Sprintf("%s %s as %d", c.args[0], c.args[len(c.args)-1], c.cred.Uid))
	}
	return out
}

// sakHarness is a Sak over a fake NSS whose users live under home, recording
// flat hooks and a fake helper, on fixedNow. euid is the agent's euid.
type sakHarness struct {
	s                 Sak
	ns                *raltest.FakeNSS
	start             *fakeStarter
	euid              int
	home              string
	backups, relabels []string
	resolved          []string
}

const sakContext = "system_u:object_r:ssh_home_t:s0"

func newSakHarness(t *testing.T, euid int, encoding string) *sakHarness {
	t.Helper()
	h := &sakHarness{
		ns:    &raltest.FakeNSS{Users: map[string]nameservice.PasswdEntry{}},
		start: &fakeStarter{answers: map[string]asuser.Result{}, errs: map[string]error{}},
		euid:  euid,
		home:  t.TempDir(),
	}
	h.s = newSak("2.5.0", encoding, h.ns,
		func(_ context.Context, path string) error { h.backups = append(h.backups, path); return nil },
		func(_ context.Context, path string) error { h.relabels = append(h.relabels, path); return nil },
		h.start, func() int { return h.euid })
	h.s.now = fixedNow
	h.s.resolve = func(usr, _ any, _ func(string) bool) (*syscall.Credential, error) {
		name := usr.(string)
		h.resolved = append(h.resolved, name)
		u, ok := h.ns.Users[name]
		if !ok {
			return nil, fmt.Errorf("Invalid user: %s", name)
		}
		return &syscall.Credential{Uid: uint32(u.UID), Gid: uint32(u.GID), Groups: []uint32{uint32(u.GID)}}, nil
	}
	h.s.defaultContext = func(context.Context, string) (string, bool) { return sakContext, true }
	return h
}

// addUser gives the NSS a user and a same-named group, uid as gid, with a
// home directory under h.home, and returns the default target.
func (h *sakHarness) addUser(t *testing.T, name string, uid int64) string {
	t.Helper()
	home := filepath.Join(h.home, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	h.ns.Users[name] = nameservice.PasswdEntry{Name: name, UID: uid, GID: uid, Dir: home}
	h.ns.Groups = append(h.ns.Groups, nameservice.GroupEntry{Name: name, GID: uid})
	return filepath.Join(home, ".ssh", "authorized_keys")
}

func sakAt(title string, params map[string]any) catalog.Resource {
	return catalog.Resource{Type: "Ssh_authorized_key", Title: title, Parameters: params}
}

// seed writes text at path, creating its directory.
func seed(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// apply prefetches rs and applies each, as the walk does, logging into ctx.
func (h *sakHarness) apply(t *testing.T, ctx context.Context, noop bool, rs ...catalog.Resource) []outcome {
	t.Helper()
	primed, err := h.s.Prefetch(ctx, rs)
	if err != nil {
		t.Fatalf("Prefetch: %v", err)
	}
	var out []outcome
	for _, r := range rs {
		changes, warnings, err := primed.Apply(atResource(ctx, r), r, noop)
		if len(warnings) != 0 {
			t.Errorf("%s: warnings %v", r.Ref(), warnings)
		}
		out = append(out, outcome{changes, err})
	}
	return out
}

// runSak runs rs through a transaction whose log, at level, is the report's.
func runSak(t *testing.T, level string, s Sak, rs ...catalog.Resource) *report.Report {
	t.Helper()
	rep := report.New("n", "production", false)
	ctx := runlog.WithLogger(context.Background(), runlog.New(level, func(e report.LogEntry) { rep.AddLog(e) }, nil))
	cat := &catalog.Catalog{Resources: rs}
	tx := &transaction.Transaction{Registry: ral.NewRegistry(s)}
	g, err := transaction.BuildGraph(ctx, cat, tx.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Run(ctx, cat, g, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// allLines is every log entry as "<level> <source>: <message>".
func allLines(entries []report.LogEntry) []string {
	var out []string
	for _, l := range entries {
		out = append(out, l.Level+" "+l.Source+": "+l.Message)
	}
	return out
}

// subsequence reports whether want appears in got in order.
func subsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func alice(params map[string]any) catalog.Resource {
	p := map[string]any{"user": "alice", "type": "ssh-rsa", "key": "AAAA"}
	for k, v := range params {
		p[k] = v
	}
	return sakAt("alice@laptop", p)
}

// TestSakCreateAsUser is M4/M8-create: as root, the three ops run in the
// helper as alice, in order; the new .ssh is announced; nothing is backed up
// (no file yet). A second run finds .ssh and the file: no Creating line, one
// backup in the parent.
func TestSakCreateAsUser(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	dir := filepath.Dir(keys)
	ctx, logs := raltest.DebugContext("debug")
	out := h.apply(t, ctx, false, alice(nil))[0]
	if out.err != nil || len(out.changes) != 1 || out.changes[0].Message != "created" || out.changes[0].Name != "ssh_authorized_key_created" {
		t.Fatalf("changes %+v, err %v", out.changes, out.err)
	}
	want := []string{"mkdir " + dir + " as 1001", "write " + keys + " as 1001", "chmod " + keys + " as 1001"}
	if got := h.start.ops(); !reflect.DeepEqual(got, want) {
		t.Errorf("helper ops = %q, want %q", got, want)
	}
	if args := h.start.calls[1].args; !reflect.DeepEqual(args, []string{"write", "--encoding", "UTF-8", keys}) {
		t.Errorf("write args = %q", args)
	}
	text := header + "ssh-rsa AAAA alice@laptop\n"
	if h.start.calls[1].stdin != text || readTarget(t, keys) != text {
		t.Errorf("stdin %q, file %q", h.start.calls[1].stdin, readTarget(t, keys))
	}
	if h.start.calls[0].stdin != "" || h.start.calls[2].stdin != "" {
		t.Errorf("mkdir/chmod stdin = %q %q", h.start.calls[0].stdin, h.start.calls[2].stdin)
	}
	lines := raltest.LinesAt(*logs, "Puppet")
	if !subsequence(lines, []string{"Prefetching parsed resources for ssh_authorized_key", "Creating " + dir + " as alice", "Flushing ssh_authorized_key provider target " + keys}) {
		t.Errorf("debug lines = %q", lines)
	}
	if len(h.backups) != 0 || !reflect.DeepEqual(h.resolved, []string{"alice"}) {
		t.Errorf("backups %q, resolved %q", h.backups, h.resolved)
	}
	if st, err := os.Stat(keys); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", st, err)
	}

	h.start.calls = nil
	ctx, logs = raltest.DebugContext("debug")
	out = h.apply(t, ctx, false, alice(map[string]any{"key": "BBBB"}))[0]
	if out.err != nil || len(out.changes) != 1 || out.changes[0].Message != "key changed 'AAAA' to 'BBBB'" {
		t.Fatalf("changes %+v, err %v", out.changes, out.err)
	}
	for _, l := range raltest.LinesAt(*logs, "Puppet") {
		if strings.HasPrefix(l, "Creating ") {
			t.Errorf("an existing .ssh was announced: %q", l)
		}
	}
	if !reflect.DeepEqual(h.backups, []string{keys}) {
		t.Errorf("backups = %q", h.backups)
	}
}

// TestSakMessages is M8-existing, in-process: option, type and key messages
// with their events, an unnamed key written with a trailing space, and a
// key_v1 record that reports defined values and is written back unchanged.
func TestSakMessages(t *testing.T) {
	h := newSakHarness(t, 1000, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "# my comment\n\nssh-rsa AAAA alice@laptop\nssh-rsa BBBB\n"+
		"command=\"echo hi\",no-pty ssh-rsa CCCC bob@x\nssh-rsa K1 typed@x\n1024 35 1234 legacy@host\n")
	rs := []catalog.Resource{
		alice(map[string]any{"options": []any{"no-pty", `command="x y"`}}),
		sakAt("bob@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "CCCC", "options": []any{"no-pty"}}),
		sakAt("typed@x", map[string]any{"user": "alice", "type": "ssh-ed25519", "key": "K2"}),
		sakAt("legacy@host", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "K"}),
	}
	out := h.apply(t, context.Background(), false, rs...)
	checkChanges(t, "alice@laptop", out[0].changes, []wantChange{{"options", `options changed ['absent'] to ['no-pty', 'command="x y"']`, "",
		[]string{"absent"}, []string{"no-pty", `command="x y"`}, "['absent']", `['no-pty', 'command="x y"']`}}, "")
	checkChanges(t, "bob@x", out[1].changes, []wantChange{{"options", `options changed ['command="echo hi"', 'no-pty'] to ['no-pty']`, "",
		[]string{`command="echo hi"`, "no-pty"}, []string{"no-pty"}, `['command="echo hi"', 'no-pty']`, "['no-pty']"}}, "")
	checkChanges(t, "typed@x", out[2].changes, []wantChange{
		{"type", "type changed 'ssh-rsa' to 'ssh-ed25519'", "", "ssh-rsa", "ssh-ed25519", "'ssh-rsa'", "'ssh-ed25519'"},
		{"key", "key changed 'K1' to 'K2'", "", "K1", "K2", "'K1'", "'K2'"}}, "")
	checkChanges(t, "legacy@host", out[3].changes, []wantChange{
		{"type", "defined 'type' as 'ssh-rsa'", "", "absent", "ssh-rsa", "absent", "'ssh-rsa'"},
		{"key", "defined 'key' as 'K'", "", "absent", "K", "absent", "'K'"},
		{"options", "defined 'options' as ['absent']", "", "absent", []string{"absent"}, "absent", "['absent']"}}, "")
	for _, o := range out {
		if o.err != nil {
			t.Errorf("err %v", o.err)
		}
	}
	want := header + "# my comment\n\nno-pty,command=\"x y\" ssh-rsa AAAA alice@laptop\nssh-rsa BBBB \n" +
		"no-pty ssh-rsa CCCC bob@x\nssh-ed25519 K2 typed@x\n1024 35 1234 legacy@host\n"
	if got := readTarget(t, keys); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	if len(h.start.calls) != 0 || len(h.resolved) != 0 || !reflect.DeepEqual(h.backups, []string{keys}) {
		t.Errorf("helper %q, resolved %q, backups %q", h.start.ops(), h.resolved, h.backups)
	}

}

// TestSakEventNames pins the report's event names (M8-existing).
func TestSakEventNames(t *testing.T) {
	h := newSakHarness(t, 1000, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "ssh-rsa K1 alice@laptop\n")
	rep := runSak(t, "notice", h.s, alice(map[string]any{"type": "ssh-ed25519", "key": "K2", "options": []any{"no-pty"}}))
	var names []string
	for _, ev := range rep.ResourceStatuses["Ssh_authorized_key[alice@laptop]"].Events {
		names = append(names, *ev.Name)
	}
	if want := []string{"type_changed", "key_changed", "options_changed"}; !reflect.DeepEqual(names, want) {
		t.Errorf("events = %q, want %q", names, want)
	}
}

// TestSakOptionsInSync is property.rb's insync? for an array_matching :all
// property: an empty should is in sync; otherwise is == should, or is ==
// should stringified, so a parsed "absent" option matches the default
// [:absent] but [:absent] does not match a given "absent".
func TestSakOptionsInSync(t *testing.T) {
	absent := parsedfile.Value{Kind: parsedfile.Absent}
	list := func(l ...string) parsedfile.Value { return parsedfile.Value{Kind: parsedfile.List, L: l} }
	tests := []struct {
		name       string
		is, should parsedfile.Value
		want       bool
	}{
		{"absent both", absent, absent, true},
		{"equal lists", list("a", "b"), list("a", "b"), true},
		{"order matters", list("a", "b"), list("b", "a"), false},
		{"empty should", list("a"), list(), true},
		{"parsed absent string matches the default", list("absent"), absent, true},
		{"default does not match a given string", absent, list("absent"), false},
		{"no options field", parsedfile.Value{}, absent, false},
		{"absent vs list", absent, list("a"), false},
	}
	for _, tt := range tests {
		if got := sakChange("options", tt.is, tt.should) == nil; got != tt.want {
			t.Errorf("%s: in sync = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestSakTargetDefault is kt:107-120: the default target is the user's home
// at evaluation time, so a user the run creates after the prefetch counts;
// a missing user logs the debug line and leaves no target; `target => []` has
// none; a user whose name holds a slash keeps the rest as a path under home.
func TestSakTargetDefault(t *testing.T) {
	h := newSakHarness(t, 1000, "UTF-8")
	r := sakAt("k", map[string]any{"user": "dave", "type": "ssh-rsa", "key": "K"})
	ctx, logs := raltest.DebugContext("debug")
	primed, err := h.s.Prefetch(ctx, []catalog.Resource{r})
	if err != nil {
		t.Fatal(err)
	}
	if lines := raltest.LinesAt(*logs, "Puppet"); !slices.Contains(lines, "The required user is not yet present on the system") {
		t.Errorf("debug = %q", lines)
	}
	keys := h.addUser(t, "dave", 1002)
	if _, _, err := primed.Apply(atResource(ctx, r), r, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := readTarget(t, keys); got != header+"ssh-rsa K k\n" {
		t.Errorf("file = %q", got)
	}

	p, err := parseSak(sakAt("k", map[string]any{"user": "dave/sub/..//x"}), "2.5.0", "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if res := h.s.resource(context.Background(), p); !res.hasTarget || res.target != filepath.Join(h.home, "dave", "x", ".ssh", "authorized_keys") {
		t.Errorf("slash user target = %q %v", res.target, res.hasTarget)
	}
	p, _ = parseSak(sakAt("k", map[string]any{"user": "dave", "target": []any{}}), "2.5.0", "UTF-8")
	if res := h.s.resource(context.Background(), p); res.hasTarget {
		t.Errorf("target => [] has target %q", res.target)
	}
	h.ns.Users["rel"] = nameservice.PasswdEntry{Name: "rel", UID: 5, Dir: "home/rel"}
	p, _ = parseSak(sakAt("k", map[string]any{"user": "rel"}), "2.5.0", "UTF-8")
	if res := h.s.resource(context.Background(), p); res.hasTarget {
		t.Errorf("a relative home gave target %q", res.target)
	}
}

// TestSakTargetMove is M8-move (05k-move2), D-100 and spec §8 item 11: a key
// found in another managed user's file moves; the new target is backed up in
// the parent first, then the engine's flush backs up the old one (also in the
// parent, D-100) and writes both, sorted, as the new user. A refused write of
// the old file fails the flush and the new file is never written.
func TestSakTargetMove(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		h := newSakHarness(t, 0, "UTF-8")
		aliceKeys := h.addUser(t, "alice", 1001)
		bobKeys := h.addUser(t, "bob", 1002)
		seed(t, aliceKeys, "ssh-rsa KKK keep@a\nssh-rsa SSS shared@x\n")
		seed(t, bobKeys, "ssh-rsa BBB b@b\n")
		if refuse {
			h.start.answers["write "+aliceKeys] = asuser.Result{Step: asuser.StepOpen, Errno: syscall.EACCES}
		}
		ctx, logs := raltest.DebugContext("debug")
		out := h.apply(t, ctx, false,
			sakAt("keep@a", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "KKK"}),
			sakAt("shared@x", map[string]any{"user": "bob", "type": "ssh-rsa", "key": "SSS"}))
		if len(out[0].changes) != 0 {
			t.Errorf("keep@a changed: %+v", out[0].changes)
		}
		checkChanges(t, "shared@x", out[1].changes, []wantChange{{"target", "target changed '" + aliceKeys + "' to '" + bobKeys + "'", "",
			aliceKeys, bobKeys, "'" + aliceKeys + "'", "'" + bobKeys + "'"}}, "")
		if !reflect.DeepEqual(h.backups, []string{bobKeys, aliceKeys}) {
			t.Errorf("refuse=%v: backups = %q, want the new target then the old", refuse, h.backups)
		}
		if !refuse {
			want := []string{"mkdir " + filepath.Dir(bobKeys) + " as 1002", "write " + aliceKeys + " as 1002", "write " + bobKeys + " as 1002", "chmod " + bobKeys + " as 1002"}
			if got := h.start.ops(); out[1].err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("err %v, ops %q, want %q", out[1].err, got, want)
			}
			if readTarget(t, aliceKeys) != header+"ssh-rsa KKK keep@a\n" || readTarget(t, bobKeys) != header+"ssh-rsa SSS shared@x\nssh-rsa BBB b@b\n" {
				t.Errorf("files %q %q", readTarget(t, aliceKeys), readTarget(t, bobKeys))
			}
			continue
		}
		text := "Puppet::Util::FileType::FileTypeFlat could not write " + aliceKeys + ": Permission denied @ rb_sysopen - " + aliceKeys
		if out[1].err == nil || out[1].err.Error() != text {
			t.Errorf("err = %v, want %q", out[1].err, text)
		}
		if got := h.start.ops(); len(got) != 2 || readTarget(t, bobKeys) != "ssh-rsa BBB b@b\n" {
			t.Errorf("ops %q, bob's file %q", got, readTarget(t, bobKeys))
		}
		if errs := errLines(*logs); !reflect.DeepEqual(errs, []string{"Puppet: " + text}) {
			t.Errorf("err logs = %q", errs)
		}
	}
}

func errLines(entries []report.LogEntry) []string {
	var out []string
	for _, l := range entries {
		if l.Level == "err" {
			out = append(out, l.Source+": "+l.Message)
		}
	}
	return out
}

// TestSakFlushRefusals is spec §6.3 items 1-2: no user, and a user without
// a uid (with or without an explicit target) fail before anything is backed
// up, created or written; a failed lookup is the flush error.
func TestSakFlushRefusals(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "keys")
	seed(t, target, "")
	tests := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"no user", map[string]any{"target": target}, "Cannot write SSH authorized keys without user"},
		{"unknown user", map[string]any{"user": "ghost"}, "User 'ghost' does not exist"},
		{"unknown user with target", map[string]any{"user": "ghost", "target": target}, "User 'ghost' does not exist"},
	}
	for _, tt := range tests {
		h := newSakHarness(t, 0, "UTF-8")
		p := map[string]any{"type": "ssh-rsa", "key": "K"}
		for k, v := range tt.params {
			p[k] = v
		}
		out := h.apply(t, context.Background(), false, sakAt("k", p))[0]
		var fe *ral.FlushError
		if !errors.As(out.err, &fe) || out.err.Error() != tt.want || len(out.changes) != 1 {
			t.Errorf("%s: changes %+v, err %v; want %q", tt.name, out.changes, out.err, tt.want)
		}
		if len(h.start.calls) != 0 || len(h.backups) != 0 || readTarget(t, target) != "" {
			t.Errorf("%s: ops %q, backups %q", tt.name, h.start.ops(), h.backups)
		}
	}

	h := newSakHarness(t, 0, "UTF-8")
	h.addUser(t, "alice", 1001)
	boom := errors.New("getpwnam_r: EIO")
	h.s.ns = failingNSS{h.ns, "alice", boom}
	out := h.apply(t, context.Background(), false, alice(map[string]any{"target": target}))[0]
	if !errors.Is(out.err, boom) || len(h.start.calls) != 0 {
		t.Errorf("lookup error: err %v, ops %q", out.err, h.start.ops())
	}
}

// failingNSS fails the passwd lookup of one name.
type failingNSS struct {
	nameservice.NameService
	name string
	err  error
}

func (f failingNSS) User(name string) (nameservice.PasswdEntry, bool, error) {
	if name == f.name {
		return nameservice.PasswdEntry{}, false, f.err
	}
	return f.NameService.User(name)
}

// TestSakFlushSteps is spec §6.3 item 4 with M8's texts and levels: the
// parent renders each helper result. A refused write is logged by the parent
// (err Puppet) and is the resource's error, after the backup made before the
// drop; a refused mkdir and chmod only the resource's; mkdir-created and
// mkdir <errno> both announce the directory first, ok does not, and a
// refused mkdir writes nothing and logs no Flushing line; a relabel failure
// is a warning naming the parent's default context and the flush succeeds;
// an invalid or mismatched result, or a start failure, is the flush error.
func TestSakFlushSteps(t *testing.T) {
	type tc struct {
		name     string
		answers  func(dir, keys string) map[string]asuser.Result
		errs     func(dir, keys string) map[string]error
		err      func(dir, keys string) string
		logged   bool // the error is also logged err Puppet
		creating bool
		flushing bool
		ops      []string
		warning  func(keys string) string
		written  bool
		seeded   bool // the target exists before the run
	}
	tests := []tc{
		{name: "dir created", creating: true, flushing: true, ops: []string{"mkdir", "write", "chmod"}, written: true},
		{name: "dir exists", answers: func(dir, _ string) map[string]asuser.Result {
			return map[string]asuser.Result{"mkdir " + dir: {Step: asuser.StepOK}}
		}, flushing: true, ops: []string{"mkdir", "write", "chmod"}, seeded: true},
		{name: "mkdir refused", answers: func(dir, _ string) map[string]asuser.Result {
			return map[string]asuser.Result{"mkdir " + dir: {Step: asuser.StepMkdir, Errno: syscall.EACCES}}
		}, err: func(dir, _ string) string { return "Permission denied @ dir_s_mkdir - " + dir }, creating: true, ops: []string{"mkdir"}},
		{name: "home missing", answers: func(dir, _ string) map[string]asuser.Result {
			return map[string]asuser.Result{"mkdir " + dir: {Step: asuser.StepMkdir, Errno: syscall.ENOENT}}
		}, err: func(dir, _ string) string { return "No such file or directory @ dir_s_mkdir - " + dir }, creating: true, ops: []string{"mkdir"}},
		{name: "write refused", answers: func(_, keys string) map[string]asuser.Result {
			return map[string]asuser.Result{"write " + keys: {Step: asuser.StepOpen, Errno: syscall.EACCES}}
		}, err: func(_, keys string) string {
			return "Puppet::Util::FileType::FileTypeFlat could not write " + keys + ": Permission denied @ rb_sysopen - " + keys
		}, logged: true, creating: true, flushing: true, ops: []string{"mkdir", "write"}},
		{name: "copy failed", answers: func(_, keys string) map[string]asuser.Result {
			return map[string]asuser.Result{"write " + keys: {Step: asuser.StepCopy, Errno: syscall.ENOSPC}}
		}, err: func(_, keys string) string {
			return "Puppet::Util::FileType::FileTypeFlat could not write " + keys + ": No space left on device - write"
		}, logged: true, creating: true, flushing: true, ops: []string{"mkdir", "write"}},
		{name: "chmod refused", answers: func(_, keys string) map[string]asuser.Result {
			return map[string]asuser.Result{"chmod " + keys: {Step: asuser.StepChmod, Errno: syscall.EPERM}}
		}, err: func(_, keys string) string { return "Operation not permitted @ apply2files - " + keys }, creating: true, flushing: true,
			ops: []string{"mkdir", "write", "chmod"}, written: true},
		{name: "relabel failed", answers: func(_, keys string) map[string]asuser.Result {
			return map[string]asuser.Result{"write " + keys: {Step: asuser.StepRelabel, Errno: syscall.EACCES}}
		}, flushing: true, ops: []string{"mkdir", "write", "chmod"}, seeded: true,
			warning: func(keys string) string { return "Failed to set SELinux context " + sakContext + " on " + keys }},
		{name: "invalid result", errs: func(_, keys string) map[string]error {
			return map[string]error{"write " + keys: fmt.Errorf("asuser helper for %s returned an %w", keys, asuser.ErrInvalidResult)}
		}, err: func(_, keys string) string { return "asuser helper for " + keys + " returned an invalid result" },
			creating: true, flushing: true, ops: []string{"mkdir", "write"}},
		{name: "result of another op", answers: func(dir, _ string) map[string]asuser.Result {
			return map[string]asuser.Result{"mkdir " + dir: {Step: asuser.StepChmod, Errno: syscall.EPERM}}
		}, err: func(dir, _ string) string { return "asuser helper for " + dir + " returned an invalid result" }, ops: []string{"mkdir"}},
		{name: "write answered as mkdir", answers: func(_, keys string) map[string]asuser.Result {
			return map[string]asuser.Result{"write " + keys: {Step: asuser.StepMkdirCreated}}
		}, err: func(_, keys string) string { return "asuser helper for " + keys + " returned an invalid result" },
			creating: true, flushing: true, ops: []string{"mkdir", "write"}},
		{name: "start failure", errs: func(dir, _ string) map[string]error {
			return map[string]error{"mkdir " + dir: errors.New("Could not start the asuser helper via /proc/self/exe (is /proc mounted?): boom")}
		}, err: func(string, string) string {
			return "Could not start the asuser helper via /proc/self/exe (is /proc mounted?): boom"
		}, ops: []string{"mkdir"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSakHarness(t, 0, "UTF-8")
			keys := h.addUser(t, "alice", 1001)
			dir := filepath.Dir(keys)
			if tt.answers != nil {
				h.start.answers = tt.answers(dir, keys)
			}
			if tt.errs != nil {
				h.start.errs = tt.errs(dir, keys)
			}
			if tt.seeded {
				seed(t, keys, "ssh-rsa OLD old@x\n")
			}
			rep := runSak(t, "debug", h.s, alice(nil))
			lines := allLines(rep.Logs)
			ref := "/Ssh_authorized_key[alice@laptop]"
			var wantErrs []string
			if tt.err != nil {
				text := tt.err(dir, keys)
				if tt.logged {
					wantErrs = append(wantErrs, "Puppet: "+text)
				}
				wantErrs = append(wantErrs, ref+": Could not evaluate: "+text)
				st := rep.ResourceStatuses["Ssh_authorized_key[alice@laptop]"]
				if !st.Failed || len(st.Events) != 2 || st.Events[0].Message != "created" || st.Events[0].Status != "success" ||
					*st.Events[1].Name != "resource_error" || st.Events[1].Message != text || st.Events[1].Status != "failure" {
					t.Errorf("status = %+v", st)
				}
			}
			if got := errLines(rep.Logs); !reflect.DeepEqual(got, wantErrs) {
				t.Errorf("err logs = %q, want %q", got, wantErrs)
			}
			creating := "debug Puppet: Creating " + dir + " as alice"
			if got := slices.Contains(lines, creating); got != tt.creating {
				t.Errorf("Creating logged = %v, want %v; logs %q", got, tt.creating, lines)
			}
			if tt.creating && tt.err != nil && strings.HasPrefix(tt.name, "mkdir") || tt.name == "home missing" {
				i, j := slices.Index(lines, creating), slices.Index(lines, "err "+ref+": Could not evaluate: "+tt.err(dir, keys))
				if i < 0 || j < i {
					t.Errorf("Creating (%d) must precede the error (%d)", i, j)
				}
			}
			flushing := "debug Puppet: Flushing ssh_authorized_key provider target " + keys
			if got := slices.Contains(lines, flushing); got != tt.flushing {
				t.Errorf("Flushing logged = %v, want %v", got, tt.flushing)
			}
			var ops []string
			for _, c := range h.start.calls {
				ops = append(ops, c.args[0])
				if c.cred == nil || c.cred.Uid != 1001 {
					t.Errorf("%q ran as %+v", c.args, c.cred)
				}
			}
			if !reflect.DeepEqual(ops, tt.ops) {
				t.Errorf("ops = %q, want %q", ops, tt.ops)
			}
			var warnings []string
			for _, l := range rep.Logs {
				if l.Level == "warning" {
					warnings = append(warnings, l.Source+": "+l.Message)
				}
			}
			var wantWarnings []string
			if tt.warning != nil {
				wantWarnings = []string{"Puppet: " + tt.warning(keys)}
			}
			if !reflect.DeepEqual(warnings, wantWarnings) {
				t.Errorf("warnings = %q, want %q", warnings, wantWarnings)
			}
			if tt.written && readTarget(t, keys) != header+"ssh-rsa AAAA alice@laptop\n" {
				t.Errorf("file = %q", readTarget(t, keys))
			}
		})
	}
}

// TestSakBackupBeforeRefusedWrite is M5 write-refused: the existing target is
// backed up in the parent before any helper op, so before the refused write.
func TestSakBackupBeforeRefusedWrite(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "ssh-rsa OLD old@x\n")
	h.start.answers["write "+keys] = asuser.Result{Step: asuser.StepOpen, Errno: syscall.EACCES}
	h.s.backup = func(_ context.Context, path string) error {
		if len(h.start.calls) != 0 {
			t.Errorf("backup after %q; want before the drop (kp:76)", h.start.ops())
		}
		h.backups = append(h.backups, path)
		return nil
	}
	out := h.apply(t, context.Background(), false, alice(nil))[0]
	if out.err == nil || !reflect.DeepEqual(h.backups, []string{keys}) || readTarget(t, keys) != "ssh-rsa OLD old@x\n" {
		t.Errorf("err %v, backups %q, file %q", out.err, h.backups, readTarget(t, keys))
	}
}

// TestSakEncodingCheckedBeforeWrite: under US-ASCII the parent refuses text
// the helper could not write, with the flat write's text, logged twice, and
// never starts the write.
func TestSakEncodingCheckedBeforeWrite(t *testing.T) {
	h := newSakHarness(t, 0, "US-ASCII")
	keys := h.addUser(t, "alice", 1001)
	ctx, logs := raltest.DebugContext("debug")
	out := h.apply(t, ctx, false, alice(map[string]any{"key": "Kcafé"}))[0]
	want := parsedfile.FlatEncodeError(keys, "US-ASCII", header+"ssh-rsa Kcafé alice@laptop\n")
	if want == nil || out.err == nil || out.err.Error() != want.Error() {
		t.Fatalf("err = %v, want %v", out.err, want)
	}
	if got := h.start.ops(); len(got) != 1 || !strings.HasPrefix(got[0], "mkdir ") {
		t.Errorf("ops = %q, want the mkdir only", got)
	}
	if errs := errLines(*logs); !reflect.DeepEqual(errs, []string{"Puppet: " + want.Error()}) {
		t.Errorf("err logs = %q", errs)
	}
}

// TestSakRedaction: a marked user is [redacted] in the Creating line and the
// unknown-user error (D-78); paths are rendered raw, as Ruby does, even for a
// marked target (they are on the helper's argv anyway).
func TestSakRedaction(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	h.start.errs["write "+keys] = fmt.Errorf("asuser helper for %s returned an %w", keys, asuser.ErrInvalidResult)
	r := alice(map[string]any{"target": keys})
	r.SensitiveParameters = []string{"target", "user"}
	ctx, logs := raltest.DebugContext("debug")
	out := h.apply(t, ctx, false, r)[0]
	if out.err == nil || out.err.Error() != "asuser helper for "+keys+" returned an invalid result" || !errors.Is(out.err, asuser.ErrInvalidResult) {
		t.Errorf("err = %v", out.err)
	}
	if lines := raltest.LinesAt(*logs, "Puppet"); !slices.Contains(lines, "Creating "+filepath.Dir(keys)+" as [redacted]") {
		t.Errorf("debug = %q", lines)
	}

	h = newSakHarness(t, 0, "UTF-8")
	r = sakAt("k", map[string]any{"user": "ghost", "type": "ssh-rsa", "key": "K", "target": keys})
	r.SensitiveParameters = []string{"user"}
	if out := h.apply(t, context.Background(), false, r)[0]; out.err == nil || out.err.Error() != "User '[redacted]' does not exist" {
		t.Errorf("err = %v", out.err)
	}
}

// TestSakRelabelWithoutDefaultContext: when the parent finds no default
// context Ruby made no relabel attempt, so no warning; the flush succeeds.
func TestSakRelabelWithoutDefaultContext(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "")
	h.s.defaultContext = func(context.Context, string) (string, bool) { return "", false }
	h.start.answers["write "+keys] = asuser.Result{Step: asuser.StepRelabel, Errno: syscall.EACCES}
	ctx, logs := raltest.DebugContext("debug")
	if out := h.apply(t, ctx, false, alice(nil))[0]; out.err != nil {
		t.Fatal(out.err)
	}
	for _, l := range *logs {
		if l.Level == "warning" {
			t.Errorf("warning %q", l.Message)
		}
	}
}

// TestSakEmptyUser: an empty user is a user to Ruby. The default target is
// "~/.ssh/authorized_keys", the agent's HOME, and the flush fails the uid
// lookup ("User '<empty>' does not exist"), and nothing is written.
func TestSakEmptyUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	h := newSakHarness(t, 0, "UTF-8")
	r := sakAt("k", map[string]any{"user": "", "type": "ssh-rsa", "key": "K"})
	p, err := parseSak(r, "2.5.0", "UTF-8")
	if err != nil {
		t.Fatal(err)
	}
	if res := h.s.resource(context.Background(), p); !res.hasTarget || res.target != filepath.Join(home, ".ssh", "authorized_keys") {
		t.Errorf("target = %q %v", res.target, res.hasTarget)
	}
	out := h.apply(t, context.Background(), false, r)[0]
	if out.err == nil || out.err.Error() != "User '' does not exist" || len(h.start.calls) != 0 || exists(filepath.Join(home, ".ssh")) {
		t.Errorf("err %v, ops %q", out.err, h.start.ops())
	}
}

// TestSakDropFalseMoveWalksEveryTarget is the D-102 extension: under
// drop_privileges false a move also writes the old target as root, so every
// own target must pass trusted_path first. An old target behind a symlink in
// a group-writable directory, or in one itself, fails the flush before
// anything is written; a trusted old target is still written.
func TestSakDropFalseMoveWalksEveryTarget(t *testing.T) {
	for _, tc := range []string{"trusted", "group-writable", "symlink"} {
		t.Run(tc, func(t *testing.T) {
			h := newSakHarness(t, os.Geteuid(), "UTF-8")
			h.ns.Users["alice"] = nameservice.PasswdEntry{Name: "alice", UID: int64(os.Getuid()), GID: int64(os.Getgid()), Dir: h.home}
			base := t.TempDir()
			oldDir, newDir := filepath.Join(base, "old"), filepath.Join(base, "new")
			for _, d := range []string{oldDir, newDir} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			oldT, newT := filepath.Join(oldDir, "file"), filepath.Join(newDir, "file")
			const oldText = "ssh-rsa M moved@x\nssh-rsa O other@x\n"
			victim := filepath.Join(base, "victim")
			seed(t, victim, oldText)
			switch tc {
			case "trusted":
				seed(t, oldT, oldText)
			case "group-writable":
				seed(t, oldT, oldText)
				if err := os.Chmod(oldDir, 0o775); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				lnk := filepath.Join(base, "gw")
				if err := os.Mkdir(lnk, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(lnk, 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(oldDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(lnk, oldDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(victim, filepath.Join(lnk, "file")); err != nil {
					t.Fatal(err)
				}
			}
			keep := sakAt("other@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "O", "target": oldT, "drop_privileges": false})
			move := sakAt("moved@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "M", "target": newT, "drop_privileges": false})
			ctx, _ := raltest.DebugContext("debug")
			out := h.apply(t, ctx, false, keep, move)
			if len(out[1].changes) != 1 || out[1].changes[0].Property != "target" {
				t.Fatalf("changes %+v", out[1].changes)
			}
			if tc == "trusted" {
				if out[1].err != nil || readTarget(t, oldT) != header+"ssh-rsa O other@x\n" || readTarget(t, newT) != header+"ssh-rsa M moved@x\n" {
					t.Errorf("err %v, old %q", out[1].err, readTarget(t, oldT))
				}
				return
			}
			if out[1].err == nil || out[1].err.Error() != "drop_privileges is false but the target path is not trusted" {
				t.Errorf("err = %v", out[1].err)
			}
			if exists(newT) || readTarget(t, victim) != oldText {
				t.Errorf("written: new %v, victim %q", exists(newT), readTarget(t, victim))
			}
			if len(h.start.calls) != 0 {
				t.Errorf("ops %q", h.start.ops())
			}
		})
	}
}

// TestSakDirectoryTarget is M8-dropfalse: a directory as target fails the
// prefetch with the flat read's text; every resource is skipped and nothing
// reaches the flush or the helper.
func TestSakDirectoryTarget(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	h.addUser(t, "alice", 1001)
	dir := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, drop := range []bool{true, false} {
		rep := runSak(t, "notice", h.s, alice(map[string]any{"target": dir, "drop_privileges": drop}))
		text := "Puppet::Util::FileType::FileTypeFlat could not read " + dir + ": Is a directory @ io_fread - " + dir
		want := []string{"err Puppet: " + text, "err Puppet: Could not prefetch ssh_authorized_key provider 'parsed': " + text,
			"warning /Ssh_authorized_key[alice@laptop]: Skipping because provider prefetch failed"}
		if got := allLines(rep.Logs); !subsequence(got, want) {
			t.Errorf("drop %v: logs = %q, want %q", drop, got, want)
		}
		if st := rep.ResourceStatuses["Ssh_authorized_key[alice@laptop]"]; !st.Failed || !st.Skipped {
			t.Errorf("status = %+v", st)
		}
	}
	if len(h.start.calls) != 0 || len(h.backups) != 0 {
		t.Errorf("ops %q, backups %q", h.start.ops(), h.backups)
	}
}

// TestSakNoop: noop messages, no flush, no helper, no backup.
func TestSakNoop(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "ssh-rsa K1 alice@laptop\n")
	out := h.apply(t, context.Background(), true, alice(map[string]any{"key": "K2"}), sakAt("new@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "N"}))
	if len(out[0].changes) != 1 || out[0].changes[0].Message != "key changed 'K1' to 'K2' (noop)" ||
		len(out[1].changes) != 1 || out[1].changes[0].Message != "created (noop)" {
		t.Errorf("changes %+v %+v", out[0].changes, out[1].changes)
	}
	if len(h.start.calls) != 0 || len(h.backups) != 0 || readTarget(t, keys) != "ssh-rsa K1 alice@laptop\n" {
		t.Errorf("ops %q, backups %q", h.start.ops(), h.backups)
	}
}

// TestSakEnsureAbsent: a removal flushes as the user; a missing key with
// ensure absent is nothing to manage.
func TestSakEnsureAbsent(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "ssh-rsa K1 alice@laptop\nssh-rsa K2 other@x\n")
	ctx, logs := raltest.DebugContext("debug")
	out := h.apply(t, ctx, false, alice(map[string]any{"ensure": "absent"}), sakAt("gone@x", map[string]any{"user": "alice", "ensure": "absent"}))
	if len(out[0].changes) != 1 || out[0].changes[0].Name != "ssh_authorized_key_removed" || out[0].err != nil || len(out[1].changes) != 0 {
		t.Errorf("out %+v", out)
	}
	if got := readTarget(t, keys); got != header+"ssh-rsa K2 other@x\n" {
		t.Errorf("file = %q", got)
	}
	if !slices.Contains(raltest.LinesAt(*logs, "/Stage[main]/Main/Ssh_authorized_key[gone@x]"), "Nothing to manage: no ensure and the resource doesn't exist") {
		t.Errorf("logs = %q", allLines(*logs))
	}
}

// TestSakDropFalse is spec §6.3 item 5 (M8-dropfalse, 17c): a trusted target
// is written in-process, then chowned to the user and the group named like
// the user (-1 when there is none) and set 0600; untrusted, or trusted with
// debug off, fails before the write. The helper is never started.
func TestSakDropFalse(t *testing.T) {
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	h := newSakHarness(t, os.Geteuid(), "UTF-8")
	h.ns.Users["alice"] = nameservice.PasswdEntry{Name: "alice", UID: uid, GID: 4242, Dir: h.home}
	h.ns.Groups = append(h.ns.Groups, nameservice.GroupEntry{Name: "alice", GID: gid})
	base := t.TempDir()
	target := filepath.Join(base, "a", "b", "file")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	r := alice(map[string]any{"target": target, "drop_privileges": false})

	ctx, logs := raltest.DebugContext("info")
	if out := h.apply(t, ctx, false, r)[0]; out.err == nil || out.err.Error() != "drop_privileges is false but the target path is not trusted" || exists(target) {
		t.Errorf("debug off: err %v, written %v", out.err, exists(target))
	}
	if len(*logs) != 0 {
		t.Errorf("debug off logged %q", allLines(*logs))
	}

	ctx, logs = raltest.DebugContext("debug")
	if out := h.apply(t, ctx, false, r)[0]; out.err != nil {
		t.Fatalf("trusted: %v", out.err)
	}
	st, err := os.Stat(target)
	if err != nil || st.Mode().Perm() != 0o600 || readTarget(t, target) != header+"ssh-rsa AAAA alice@laptop\n" {
		t.Fatalf("file %v %v %q", st, err, readTarget(t, target))
	}
	if sys := st.Sys().(*syscall.Stat_t); int64(sys.Uid) != uid || int64(sys.Gid) != gid {
		t.Errorf("owner %d:%d, want %d:%d", sys.Uid, sys.Gid, uid, gid)
	}
	lines := raltest.LinesAt(*logs, "Puppet")
	if !subsequence(lines, []string{"Path trusted, writing the file as the current user", "Flushing ssh_authorized_key provider target " + target}) {
		t.Errorf("debug = %q", lines)
	}
	if len(h.start.calls) != 0 || len(h.resolved) != 0 {
		t.Errorf("ops %q, resolved %q", h.start.ops(), h.resolved)
	}

	if err := os.Chmod(filepath.Dir(target), 0o775); err != nil {
		t.Fatal(err)
	}
	ctx, logs = raltest.DebugContext("debug")
	if out := h.apply(t, ctx, false, alice(map[string]any{"target": target, "drop_privileges": false, "key": "NEW"}))[0]; out.err == nil ||
		out.err.Error() != "drop_privileges is false but the target path is not trusted" || strings.Contains(readTarget(t, target), "NEW") {
		t.Errorf("group-writable: err %v", out.err)
	}
	if lines := raltest.LinesAt(*logs, "Puppet"); !slices.Contains(lines, "Path untrusted, will attempt to write as the target user") {
		t.Errorf("debug = %q", lines)
	}
}

// TestTrustedPath is trusted_path (kp:53-67).
func TestTrustedPath(t *testing.T) {
	euid := os.Geteuid()
	base := t.TempDir()
	mk := func(rel string, mode os.FileMode) string {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := mk("good/sub", 0o755)
	group := mk("group", 0o775)
	world := mk("world", 0o757)
	sticky := mk("sticky", 0o777|os.ModeSticky)
	if err := os.Symlink(good, filepath.Join(base, "link-good")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(group, filepath.Join(base, "link-group")); err != nil {
		t.Fatal(err)
	}
	const (
		trustedLine   = "Path trusted, writing the file as the current user"
		untrustedLine = "Path untrusted, will attempt to write as the target user"
	)
	tests := []struct {
		name   string
		target string
		euid   int
		want   bool
		line   string
	}{
		{"trusted", filepath.Join(good, "file"), euid, true, trustedLine},
		{"target need not exist", filepath.Join(good, "absent"), euid, true, trustedLine},
		{"missing parent", filepath.Join(base, "nonexistent", "dir", "file"), euid, false, ""},
		{"another owner", filepath.Join(good, "file"), euid + 1, false, untrustedLine},
		{"group-writable parent", filepath.Join(group, "file"), euid, false, untrustedLine},
		{"group-writable ancestor", filepath.Join(mk("group/sub", 0o755), "file"), euid, false, untrustedLine},
		{"world-writable", filepath.Join(world, "file"), euid, false, untrustedLine},
		{"sticky world-writable", filepath.Join(sticky, "file"), euid, false, untrustedLine},
		{"symlink to a trusted dir", filepath.Join(base, "link-good", "file"), euid, true, trustedLine},
		{"symlink to a group-writable dir", filepath.Join(base, "link-group", "file"), euid, false, untrustedLine},
		{"parent's parent is /: no checks", "/tmp/voxagent-trusted-path-file", euid + 1, true, trustedLine},
		{"relative", "file", euid, false, ""},
	}
	if _, err := os.Stat("/tmp"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		ctx, logs := raltest.DebugContext("debug")
		if got := trustedPath(ctx, tt.target, tt.euid); got != tt.want {
			t.Errorf("%s: trustedPath(%q) = %v, want %v", tt.name, tt.target, got, tt.want)
		}
		var want []string
		if tt.line != "" {
			want = []string{tt.line}
		}
		if got := raltest.LinesAt(*logs, "Puppet"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: debug = %q, want %q", tt.name, got, want)
		}
	}
	// With debug off the trusted walk's result is Puppet.debug's nil.
	ctx, _ := raltest.DebugContext("info")
	if trustedPath(ctx, filepath.Join(good, "file"), euid) {
		t.Error("trusted with debug off")
	}
}

func TestRubyDirname(t *testing.T) {
	for in, want := range map[string]string{
		"/a/b/c": "/a/b", "/a/b/c/": "/a/b", "/a//b": "/a", "/a": "/", "/": "/", "//": "/",
		"a": ".", "a/b": "a", "": ".", "/a/./b": "/a/.", "a//": ".",
	} {
		if got := rubyDirname(in); got != want {
			t.Errorf("rubyDirname(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSakLeakedTargetUnknownUser is D-102 (M8 08a-modified-leak): a flush
// that fails before the write leaves its target marked; the next resource's
// flush writes only its own targets, so the leaked one stays unwritten.
func TestSakLeakedTargetUnknownUser(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	leak := filepath.Join(t.TempDir(), "x", "authorized_keys")
	out := h.apply(t, context.Background(), false,
		sakAt("r1", map[string]any{"user": "ghost", "type": "ssh-rsa", "key": "G", "target": leak}),
		alice(nil))
	if out[0].err == nil || out[0].err.Error() != "User 'ghost' does not exist" || out[1].err != nil {
		t.Fatalf("errs %v / %v", out[0].err, out[1].err)
	}
	for _, op := range h.start.ops() {
		if strings.Contains(op, leak) {
			t.Errorf("the leaked target was touched: %q", op)
		}
	}
	if exists(leak) || readTarget(t, keys) != header+"ssh-rsa AAAA alice@laptop\n" {
		t.Errorf("leak written %v, keys %q", exists(leak), readTarget(t, keys))
	}
}

// TestSakLeakedTargetRefusedMkdir is D-102 after a refused mkdir.
func TestSakLeakedTargetRefusedMkdir(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	bobKeys := h.addUser(t, "bob", 1002)
	aliceKeys := h.addUser(t, "alice", 1001)
	h.start.answers["mkdir "+filepath.Dir(bobKeys)] = asuser.Result{Step: asuser.StepMkdir, Errno: syscall.EACCES}
	out := h.apply(t, context.Background(), false,
		sakAt("r1", map[string]any{"user": "bob", "type": "ssh-rsa", "key": "B"}), alice(nil))
	if out[0].err == nil || out[1].err != nil {
		t.Fatalf("errs %v / %v", out[0].err, out[1].err)
	}
	want := []string{"mkdir " + filepath.Dir(bobKeys) + " as 1002", "mkdir " + filepath.Dir(aliceKeys) + " as 1001",
		"write " + aliceKeys + " as 1001", "chmod " + aliceKeys + " as 1001"}
	if got := h.start.ops(); !reflect.DeepEqual(got, want) || exists(bobKeys) {
		t.Errorf("ops = %q, want %q; bob's written %v", got, want, exists(bobKeys))
	}
}

// TestSakLeakedTargetUntrusted is D-102 for drop_privileges false (M8
// 08a-drop-false-walk: Ruby's next flush creates the untrusted file as root).
func TestSakLeakedTargetUntrusted(t *testing.T) {
	h := newSakHarness(t, os.Geteuid(), "UTF-8")
	keys := h.addUser(t, "alice", int64(os.Getuid()))
	untrusted := filepath.Join(t.TempDir(), "o")
	if err := os.Mkdir(untrusted, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(untrusted, 0o777); err != nil {
		t.Fatal(err)
	}
	leak := filepath.Join(untrusted, "file")
	ctx, _ := raltest.DebugContext("debug")
	out := h.apply(t, ctx, false,
		sakAt("w5", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "W5", "target": leak, "drop_privileges": false}),
		alice(nil))
	if out[0].err == nil || out[0].err.Error() != "drop_privileges is false but the target path is not trusted" || out[1].err != nil {
		t.Fatalf("errs %v / %v", out[0].err, out[1].err)
	}
	if exists(leak) || readTarget(t, keys) != header+"ssh-rsa AAAA alice@laptop\n" {
		t.Errorf("leak created %v", exists(leak))
	}
}

// TestSakSharedTargetWrittenWithEveryRecord is D-102's positive case: a later
// resource whose own target is the one a failed flush left marked writes it,
// with every record of the run in it, but not the failed resource's record,
// which never joined the run's records.
func TestSakSharedTargetWrittenWithEveryRecord(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	h.addUser(t, "alice", 1001)
	shared := filepath.Join(t.TempDir(), "shared")
	seed(t, shared, "ssh-rsa OLD old@x\n")
	out := h.apply(t, context.Background(), false,
		sakAt("r1", map[string]any{"user": "ghost", "type": "ssh-rsa", "key": "G", "target": shared}),
		sakAt("r2", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "N", "target": shared}))
	if out[0].err == nil || out[1].err != nil {
		t.Fatalf("errs %v / %v", out[0].err, out[1].err)
	}
	if got := readTarget(t, shared); got != header+"ssh-rsa OLD old@x\nssh-rsa N r2\n" {
		t.Errorf("file = %q", got)
	}
}

// TestSakInProcessWhenEuidIsUser is spec §3.4: change_user returns at once
// when the euid already is the user's uid (suid:121), so the ops run
// in-process; as root that is any user resolving to uid 0, a name other
// than root included.
func TestSakInProcessWhenEuidIsUser(t *testing.T) {
	for _, tc := range []struct {
		euid     int
		user     string
		uid      int64
		resolved []string
	}{
		{0, "toor", 0, []string{"toor"}},
		{1001, "alice", 1001, nil},
	} {
		h := newSakHarness(t, tc.euid, "UTF-8")
		keys := h.addUser(t, tc.user, tc.uid)
		ctx, logs := raltest.DebugContext("debug")
		out := h.apply(t, ctx, false, sakAt("k", map[string]any{"user": tc.user, "type": "ssh-rsa", "key": "K"}))[0]
		if out.err != nil || len(h.start.calls) != 0 || !reflect.DeepEqual(h.resolved, tc.resolved) {
			t.Errorf("%s: err %v, ops %q, resolved %q", tc.user, out.err, h.start.ops(), h.resolved)
		}
		if readTarget(t, keys) != header+"ssh-rsa K k\n" || !slices.Contains(raltest.LinesAt(*logs, "Puppet"), "Creating "+filepath.Dir(keys)+" as "+tc.user) {
			t.Errorf("%s: file %q, debug %q", tc.user, readTarget(t, keys), raltest.LinesAt(*logs, "Puppet"))
		}
		if st, err := os.Stat(keys); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v %v", tc.user, st, err)
		}
	}
}

// TestSakRootKeyNeverStartsHelper: root's own key is written in-process.
func TestSakRootKeyNeverStartsHelper(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "root", 0)
	out := h.apply(t, context.Background(), false, sakAt("k", map[string]any{"user": "root", "type": "ssh-rsa", "key": "K"}))[0]
	if out.err != nil || len(h.start.calls) != 0 || readTarget(t, keys) != header+"ssh-rsa K k\n" {
		t.Errorf("err %v, ops %q", out.err, h.start.ops())
	}
}

// TestSakNonRootAgentInProcess: asuser yields without changing anything when
// the agent is not root (suid:65): no helper, no credential lookup.
func TestSakNonRootAgentInProcess(t *testing.T) {
	h := newSakHarness(t, 1000, "UTF-8")
	keys := h.addUser(t, "bob", 1002)
	h.s.start = nil // a start would panic
	out := h.apply(t, context.Background(), false, sakAt("k", map[string]any{"user": "bob", "type": "ssh-rsa", "key": "K"}))[0]
	if out.err != nil || len(h.start.calls) != 0 || len(h.resolved) != 0 || readTarget(t, keys) != header+"ssh-rsa K k\n" {
		t.Errorf("err %v, ops %q, resolved %q", out.err, h.start.ops(), h.resolved)
	}
}

// TestSakSymlinkErrorNamesLink is M8-write (08a-write-symlink): the refused
// write's text names the target as given, the link, not what it points to;
// through the helper and in-process alike.
func TestSakSymlinkErrorNamesLink(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	foreign := filepath.Join(t.TempDir(), "ak_target")
	seed(t, foreign, "")
	seed(t, filepath.Join(filepath.Dir(keys), "placeholder"), "")
	if err := os.Symlink(foreign, keys); err != nil {
		t.Fatal(err)
	}
	h.start.answers["write "+keys] = asuser.Result{Step: asuser.StepOpen, Errno: syscall.EACCES}
	want := "Puppet::Util::FileType::FileTypeFlat could not write " + keys + ": Permission denied @ rb_sysopen - " + keys
	if out := h.apply(t, context.Background(), false, alice(nil))[0]; out.err == nil || out.err.Error() != want {
		t.Errorf("helper: err = %v, want %q", out.err, want)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only mode the in-process case relies on")
	}
	if err := os.Chmod(foreign, 0o400); err != nil {
		t.Fatal(err)
	}
	h = newSakHarness(t, 1000, "UTF-8")
	keys2 := h.addUser(t, "alice", 1001)
	seed(t, filepath.Join(filepath.Dir(keys2), "placeholder"), "")
	if err := os.Symlink(foreign, keys2); err != nil {
		t.Fatal(err)
	}
	want = "Puppet::Util::FileType::FileTypeFlat could not write " + keys2 + ": Permission denied @ rb_sysopen - " + keys2
	if out := h.apply(t, context.Background(), false, alice(nil))[0]; out.err == nil || out.err.Error() != want {
		t.Errorf("in-process: err = %v, want %q", out.err, want)
	}
}

// TestSakSensitiveKeyOnlyOnStdin is spec §11: a Sensitive key reaches the
// helper on stdin only; it is absent from every argv and from every log
// line, error and event; a change shows [redacted] (M8-sensitive). The
// helper's environment is ProcStarter's PATH and TMPDIR (TestProcCmd).
func TestSakSensitiveKeyOnlyOnStdin(t *testing.T) {
	const marker = "D8SECRET-sak-key"
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	seed(t, keys, "ssh-rsa OLDKEY alice@laptop\n")
	change := alice(map[string]any{"key": marker})
	change.SensitiveParameters = []string{"key"}
	create := sakAt("new@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": marker + "-create"})
	create.SensitiveParameters = []string{"key"}
	h.start.answers["chmod "+keys] = asuser.Result{Step: asuser.StepChmod, Errno: syscall.EPERM}
	rep := runSak(t, "debug", h.s, change, create)

	ev := rep.ResourceStatuses["Ssh_authorized_key[alice@laptop]"].Events
	if len(ev) < 1 || ev[0].Message != "changed [redacted] to [redacted]" || ev[0].PreviousValue != "[redacted]" || ev[0].Redacted != true {
		t.Errorf("events = %+v", ev)
	}
	var stdinSeen bool
	for _, c := range h.start.calls {
		for _, a := range c.args {
			if strings.Contains(a, marker) || strings.Contains(a, "OLDKEY") {
				t.Errorf("argv leaks: %q", c.args)
			}
		}
		stdinSeen = stdinSeen || strings.Contains(c.stdin, marker)
	}
	if !stdinSeen {
		t.Error("the key never reached the helper's stdin")
	}
	for _, l := range allLines(rep.Logs) {
		if strings.Contains(l, marker) || strings.Contains(l, "OLDKEY") {
			t.Errorf("log leaks: %q", l)
		}
	}
	for ref, st := range rep.ResourceStatuses {
		for _, e := range st.Events {
			if strings.Contains(fmt.Sprint(e.Message, e.PreviousValue, e.DesiredValue), marker) {
				t.Errorf("%s event leaks: %+v", ref, e)
			}
		}
	}
	if !strings.Contains(readTarget(t, keys), "ssh-rsa "+marker+" alice@laptop") {
		t.Errorf("file = %q", readTarget(t, keys))
	}
}

// TestSakEnsurableProperties pins what ral.ParsedFileRun.Apply syncs.
func TestSakEnsurableProperties(t *testing.T) {
	s := Sak{}
	if s.Type() != "ssh_authorized_key" || *s.ProviderUsed() != "parsed" {
		t.Errorf("Type %q, ProviderUsed %q", s.Type(), *s.ProviderUsed())
	}
	if !slices.Equal(sakEnsurable.Properties, []string{"type", "key", "target", "options"}) || sakEnsurable.Name != "ssh_authorized_key" {
		t.Errorf("ensurable = %+v", sakEnsurable)
	}
}

// The credential is set for one flush only: a later drop_privileges false
// write in the same run is the agent's, never the previous user's helper.
func TestSakCredentialClearedAfterFlush(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	h.ns.Users["me"] = nameservice.PasswdEntry{Name: "me", UID: int64(os.Getuid()), GID: int64(os.Getgid()), Dir: h.home}
	target := filepath.Join(t.TempDir(), "d", "file")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	r1 := alice(nil)
	r2 := sakAt("me@x", map[string]any{"user": "me", "type": "ssh-rsa", "key": "M", "target": target, "drop_privileges": false})
	ctx, _ := raltest.DebugContext("debug")
	primed, err := h.s.Prefetch(ctx, []catalog.Resource{r1, r2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := primed.Apply(atResource(ctx, r1), r1, false); err != nil {
		t.Fatal(err)
	}
	h.euid = os.Geteuid()
	if _, _, err := primed.Apply(atResource(ctx, r2), r2, false); err != nil {
		t.Fatal(err)
	}
	for _, op := range h.start.ops() {
		if !strings.HasSuffix(op, " as 1001") || strings.Contains(op, target) {
			t.Errorf("op %q", op)
		}
	}
	if len(h.start.calls) != 3 || readTarget(t, target) != header+"ssh-rsa M me@x\n" || readTarget(t, keys) != header+"ssh-rsa AAAA alice@laptop\n" {
		t.Errorf("ops %q, target %q", h.start.ops(), readTarget(t, target))
	}
}

// TestSakRootDropFalseChown is 17c-drop-false-group, as root: the trusted
// write is root's, then the file is chowned to the user and to the group
// named like the user, not the user's primary group (carol); with no such
// group the group is left as root wrote it (dave). Root only: build with
// go test -c and run the binary under sudo.
func TestSakRootDropFalseChown(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: build with go test -c and run the binary under sudo")
	}
	h := newSakHarness(t, 0, "UTF-8")
	h.ns.Users["carol"] = nameservice.PasswdEntry{Name: "carol", UID: 65001, GID: 2001, Dir: h.home}
	h.ns.Users["dave"] = nameservice.PasswdEntry{Name: "dave", UID: 65002, GID: 2001, Dir: h.home}
	h.ns.Groups = append(h.ns.Groups, nameservice.GroupEntry{Name: "staff2", GID: 2001}, nameservice.GroupEntry{Name: "carol", GID: 2002})
	base := t.TempDir()
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	var rs []catalog.Resource
	targets := map[string]string{}
	for _, u := range []string{"carol", "dave"} {
		targets[u] = filepath.Join(base, u, "file")
		if err := os.MkdirAll(filepath.Dir(targets[u]), 0o755); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, sakAt(u+"@x", map[string]any{"user": u, "type": "ssh-rsa", "key": "K", "target": targets[u], "drop_privileges": false}))
	}
	ctx, _ := raltest.DebugContext("debug")
	for _, o := range h.apply(t, ctx, false, rs...) {
		if o.err != nil {
			t.Fatal(o.err)
		}
	}
	for u, want := range map[string][2]uint32{"carol": {65001, 2002}, "dave": {65002, 0}} {
		st, err := os.Stat(targets[u])
		if err != nil {
			t.Fatal(err)
		}
		if sys := st.Sys().(*syscall.Stat_t); sys.Uid != want[0] || sys.Gid != want[1] || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %d:%d %v, want %d:%d 0600", u, sys.Uid, sys.Gid, st.Mode().Perm(), want[0], want[1])
		}
	}
	if len(h.start.calls) != 0 {
		t.Errorf("ops %q", h.start.ops())
	}
}

// TestSakAutorequireDebug is M8-user-res: the graph logs the edge to a
// managed User at the key's path.
func TestSakAutorequireDebug(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	ctx, logs := raltest.DebugContext("debug")
	cat := &catalog.Catalog{Resources: []catalog.Resource{
		{Type: "User", Title: "erin"},
		sakAt("erin@x", map[string]any{"user": "erin", "type": "ssh-rsa", "key": "K"}),
	}}
	if _, err := transaction.BuildGraph(ctx, cat, ral.NewRegistry(h.s)); err != nil {
		t.Fatal(err)
	}
	if got := raltest.LinesAt(*logs, "/Ssh_authorized_key[erin@x]"); !slices.Contains(got, "Adding autorequire relationship with User[erin]") {
		t.Errorf("debug = %q", allLines(*logs))
	}
}

// TestSakKeyV1WithoutName is M8-keyv1 (spec §8 item 19): a key_v1 line with
// no name fails every flush of its target, before any write op; the file is
// untouched.
func TestSakKeyV1WithoutName(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	const text = "1024 35 777\n"
	seed(t, keys, text)
	out := h.apply(t, context.Background(), false, alice(nil), sakAt("b@x", map[string]any{"user": "alice", "type": "ssh-rsa", "key": "B"}))
	for i, o := range out {
		if o.err == nil || o.err.Error() != "Field 'name' is required" {
			t.Errorf("resource %d: err = %v", i, o.err)
		}
	}
	for _, op := range h.start.ops() {
		if strings.HasPrefix(op, "write ") {
			t.Errorf("a write started: %q", h.start.ops())
		}
	}
	if readTarget(t, keys) != text {
		t.Errorf("file = %q", readTarget(t, keys))
	}
}

// The credential is cleared after a failed flush too: a refused mkdir, then
// a drop_privileges false write that starts no helper.
func TestSakCredentialClearedAfterFailedFlush(t *testing.T) {
	h := newSakHarness(t, 0, "UTF-8")
	keys := h.addUser(t, "alice", 1001)
	h.start.answers["mkdir "+filepath.Dir(keys)] = asuser.Result{Step: asuser.StepMkdir, Errno: syscall.EACCES}
	h.ns.Users["me"] = nameservice.PasswdEntry{Name: "me", UID: int64(os.Getuid()), GID: int64(os.Getgid()), Dir: h.home}
	target := filepath.Join(t.TempDir(), "d", "file")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	r1 := alice(nil)
	r2 := sakAt("me@x", map[string]any{"user": "me", "type": "ssh-rsa", "key": "M", "target": target, "drop_privileges": false})
	ctx, _ := raltest.DebugContext("debug")
	primed, err := h.s.Prefetch(ctx, []catalog.Resource{r1, r2})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := primed.Apply(atResource(ctx, r1), r1, false); err == nil {
		t.Fatal("the refused mkdir did not fail the flush")
	}
	h.euid = os.Geteuid()
	if _, _, err := primed.Apply(atResource(ctx, r2), r2, false); err != nil {
		t.Fatal(err)
	}
	if got := h.start.ops(); len(got) != 1 || readTarget(t, target) != header+"ssh-rsa M me@x\n" {
		t.Errorf("ops %q (want only the refused mkdir), target %q", got, readTarget(t, target))
	}
}
