package extension

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a fake extension host. Started with VOXAGENT_FAKE_HOST
// set, the test binary speaks the protocol on its stdin/stdout instead of
// running tests, so the client's lifecycle is tested against a real process
// with real pipes -- crash, hang and garbage included -- without Ruby.
func TestMain(m *testing.M) {
	mode := os.Getenv("VOXAGENT_FAKE_HOST")
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "-fake-host=") {
		mode = strings.TrimPrefix(os.Args[1], "-fake-host=") // for tests that must pass no environment
	}
	if mode != "" {
		os.Exit(fakeHost(mode))
	}
	os.Exit(m.Run())
}

const fakeLoad = `{"load":{"types":{"vox_value":{"class":"tier3","origin":"libdir","refreshable":true,"eval_generates":false}},` +
	`"metaparams":["noop"],"invalid":[],"generated":{"resources":[],"containers":{},"before":[],"overrides":[]},"resources":{"Vox_value[a]":{"provider_used":"file"},"Vox_value[b]":{"provider_used":"file"}},` +
	`"order":["Vox_value[a]","Vox_value[b]"],"logs":[{"level":"warning","source":"Puppet","message":"loaded"}],"generate_logs":[],"failed_to_generate":false}}`

const fakeEvent = `{"audited":false,"property":"value","previous_value":"absent","desired_value":"one","historical_value":null,` +
	`"message":"defined 'value' as 'one'","name":"value_changed","status":"success","time":"2026-10-07T00:00:00Z","redacted":null,"corrective_change":false}`

const emptyGeneration = `"generated":{"resources":[],"containers":{},"before":[],"overrides":[]}`

func generatedNotify(title string) string {
	return `{"type":"Notify","title":"` + title + `","tags":["notify"],"exported":false,"kind":"compilable_type","parameters":{}}`
}

// generation builds a generation: resources, containers, before and overrides as JSON fragments.
func generation(resources, containers, before, overrides string) string {
	return `{"resources":[` + resources + `],"containers":{` + containers + `},"before":[` + before + `],"overrides":[` + overrides + `]}`
}

// retitle adds the JSON fields a and b, when not empty, to the plain
// fakeLoad's Vox_value[a] and Vox_value[b].
func retitle(a, b string) string {
	out := strings.Replace(fakeLoad, `"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file",`+a+`}`, 1)
	if b == "" {
		return out
	}
	return strings.Replace(out, `"Vox_value[b]":{"provider_used":"file"}`, `"Vox_value[b]":{"provider_used":"file",`+b+`}`, 1)
}

// ordered replaces load's order with refs.
func ordered(load string, refs ...string) string {
	order, _ := json.Marshal(refs)
	return strings.Replace(load, `"order":["Vox_value[a]","Vox_value[b]"]`, `"order":`+string(order), 1)
}

// invalidB refuses the plain fakeLoad's Vox_value[b].
func invalidB(load string) string {
	return strings.Replace(load, `"invalid":[]`, `"invalid":[{"ref":"Vox_value[b]","message":"refused"}]`, 1)
}

// fakeLoadFor answers load: the plain fakeLoad, with the generation, the
// aliases, Puppet's own refs or the order the mode calls for.
func fakeLoadFor(mode string) string {
	swap := func(gen string) string { return strings.Replace(fakeLoad, emptyGeneration, `"generated":`+gen, 1) }
	switch mode {
	case "gen-ok":
		return ordered(swap(generation(generatedNotify("made"), `"Notify[made]":"Vox_value[a]"`,
			`{"source":"Vox_value[a]","target":"Notify[made]"}`, "")), "Vox_value[a]", "Notify[made]", "Vox_value[b]")
	case "gen-collides":
		return swap(generation(`{"type":"Vox_value","title":"a","tags":[],"exported":false,"kind":"compilable_type","parameters":{}}`, "", "", ""))
	case "gen-generator-foreign":
		return swap(strings.Replace(generation(generatedNotify("made"), "", "", ""),
			`"before"`, `"generators":{"Notify[made]":"File[/nope]"},"before"`, 1))
	case "gen-generator-not-generated":
		return swap(strings.Replace(generation("", "", "", ""),
			`"before"`, `"generators":{"Vox_value[b]":"Vox_value[a]"},"before"`, 1))
	case "gen-generated-generator":
		return ordered(swap(strings.Replace(generation(generatedNotify("made")+","+generatedNotify("nested"), "", "", ""),
			`"before"`, `"generators":{"Notify[made]":"Vox_value[a]","Notify[nested]":"Notify[made]"},"before"`, 1)),
			"Vox_value[a]", "Notify[made]", "Notify[nested]", "Vox_value[b]")
	case "order-unknown":
		return ordered(fakeLoad, "Vox_value[a]", "Vox_value[b]", "File[/nope]")
	case "order-repeat":
		return ordered(fakeLoad, "Vox_value[a]", "Vox_value[b]", "Vox_value[a]")
	case "order-reordered":
		return ordered(fakeLoad, "Vox_value[b]", "Vox_value[a]")
	case "order-invalid-reordered":
		return ordered(invalidB(fakeLoad), "Vox_value[b]", "Vox_value[a]")
	case "order-missing":
		return ordered(fakeLoad, "Vox_value[a]")
	case "order-invalid-unknown":
		return ordered(invalidB(fakeLoad), "Vox_value[a]", "File[/nope]")
	case "order-invalid-repeat":
		return ordered(invalidB(fakeLoad), "Vox_value[a]", "Vox_value[a]")
	case "order-invalid-missing":
		return ordered(invalidB(fakeLoad), "Vox_value[a]")
	case "gen-container-foreign":
		return swap(generation("", `"File[/nope]":"Vox_value[a]"`, "", ""))
	case "gen-before-foreign":
		return swap(generation("", "", `{"source":"Vox_value[a]","target":"File[/nope]"}`, ""))
	case "gen-override-unknown":
		return swap(generation("", "", "", `{"ref":"File[/nope]","parameters":{},"sensitive":[]}`))
	case "gen-override-sensitive-stray":
		return swap(generation("", "", "", `{"ref":"Vox_value[a]","parameters":{"value":"x"},"sensitive":["content"]}`))
	case "alias-collides":
		return strings.Replace(fakeLoad, `"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file","aliases":["Vox_value[b]"]}`, 1)
	case "alias-wrong-type":
		return strings.Replace(fakeLoad, `"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file","aliases":["File[x]"]}`, 1)
	case "gen-empty-title":
		return swap(generation(strings.Replace(generatedNotify("x"), `"title":"x"`, `"title":""`, 1), "", "", ""))
	case "gen-bad-kind":
		return swap(generation(strings.Replace(generatedNotify("badkind"), "compilable_type", "class", 1), "", "", ""))
	case "gen-exported":
		return swap(generation(strings.Replace(generatedNotify("exp"), `"exported":false`, `"exported":true`, 1), "", "", ""))
	case "gen-repeated":
		return swap(generation(generatedNotify("twice")+","+generatedNotify("twice"), "", "", ""))
	case "gen-container-not-generated":
		return swap(generation("", `"Vox_value[b]":"Vox_value[a]"`, "", ""))
	case "gen-override-added":
		return swap(generation(generatedNotify("fresh"), "", "", `{"ref":"Notify[fresh]","parameters":{},"sensitive":[]}`))
	case "gen-type-brackets":
		return swap(generation(strings.Replace(generatedNotify("t"), `"Notify"`, `"No[tify"`, 1), "", "", ""))
	case "gen-type-lowercase":
		return swap(generation(strings.Replace(generatedNotify("a"), `"Notify"`, `"vox_value"`, 1), "", "", ""))
	case "gen-type-segment-case":
		return swap(generation(strings.Replace(generatedNotify("t"), `"Notify"`, `"Vox::value"`, 1), "", "", ""))
	case "alias-malformed":
		return strings.Replace(fakeLoad, `"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file","aliases":["nonsense"]}`, 1)
	case "alias-twice":
		return strings.NewReplacer(`"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file","aliases":["Vox_value[x]"]}`,
			`"Vox_value[b]":{"provider_used":"file"}`, `"Vox_value[b]":{"provider_used":"file","aliases":["Vox_value[x]"]}`).Replace(fakeLoad)
	case "selfrefresh-tier2":
		return strings.Replace(fakeLoad, `"class":"tier3"`, `"class":"tier2","self_refresh":true`, 1)
	case "title-ok":
		return retitle(`"puppet_ref":"Vox_value[a:x]","puppet_title":"a:x","unresolvable":true`, "")
	case "title-wrong-type":
		return retitle(`"puppet_ref":"File[x]","puppet_title":"x"`, "")
	case "title-collides":
		return retitle(`"puppet_ref":"Vox_value[b]","puppet_title":"b"`, "")
	case "title-twice":
		return retitle(`"puppet_ref":"Vox_value[x]","puppet_title":"x"`, `"puppet_ref":"Vox_value[x]","puppet_title":"x"`)
	case "title-is-alias":
		return retitle(`"puppet_ref":"Vox_value[x]","puppet_title":"x"`, `"aliases":["Vox_value[x]"]`)
	case "title-mismatch":
		return retitle(`"puppet_ref":"Vox_value[x]","puppet_title":"y"`, "")
	case "title-equals-catalog":
		return retitle(`"puppet_ref":"Vox_value[a]","puppet_title":"a"`, "")
	case "title-bare":
		return retitle(`"puppet_title":"x"`, "")
	case "unresolvable-bare":
		return retitle(`"unresolvable":true`, "")
	case "evalgen-alias":
		return fakeLoadFor("alias-ok")
	case "alias-ok":
		return strings.Replace(fakeLoad, `"Vox_value[a]":{"provider_used":"file"}`, `"Vox_value[a]":{"provider_used":"file","aliases":["Vox_value[x]"]}`, 1)
	}
	return fakeLoad
}

// fakeEvalGenerate answers eval_generate.
func fakeEvalGenerate(mode string) string {
	logs := ""
	reply := func(resources, existing string) string {
		containers := ""
		if resources != "" {
			containers = `"Notify[made]":"Vox_value[a]"`
		}
		return `{"eval_generate":` + strings.TrimSuffix(generation(resources, containers, "", ""), "}") +
			`,"existing":[` + existing + `],"failed":false,"logs":[` + logs + `]}}`
	}
	switch mode {
	case "evalgen-bad-log":
		logs = `{"level":"bogus","source":"Puppet","message":"x"}`
		return reply("", "")
	case "evalgen-alias":
		return reply(`{"type":"Vox_value","title":"x","tags":[],"exported":false,"kind":"compilable_type","parameters":{}}`, "")
	case "evalgen-existing-foreign":
		return reply("", `"File[/nope]"`)
	case "evalgen-new-collides":
		return reply(generatedNotify("made")+`,{"type":"Vox_value","title":"b","tags":[],"exported":false,"kind":"compilable_type","parameters":{}}`, "")
	}
	return reply(generatedNotify("made"), `"Vox_value[b]"`)
}

func fakeHost(mode string) int {
	reply := func(s string) {
		if err := WriteFrame(os.Stdout, []byte(s)); err != nil {
			os.Exit(9)
		}
	}
	switch mode {
	case "silent":
		time.Sleep(time.Hour)
		return 0
	case "garbage":
		os.Stdout.WriteString("this is not a frame") // "this" is a 1.9 GB length
		return 0
	case "wrong-protocol":
		reply(`{"hello":{"protocol":2,"puppet_version":"8.10.0","ruby_version":"3.4.9"}}`)
		return 0
	}
	reply(`{"hello":{"protocol":1,"puppet_version":"8.10.0","ruby_version":"3.4.9"}}`)
	if mode == "stderr-split" {
		os.Stderr.WriteString("one SECRET\ntwo SE")
	}
	if mode == "stderr-grandchild" {
		os.Stderr.WriteString("own SECRET\n")
		gc := exec.Command("/usr/bin/sleep", "30") // inherits stderr: holds the pipe open past the host's exit
		gc.Stderr = os.Stderr
		if err := gc.Start(); err != nil {
			os.Exit(8)
		}
	}
	for {
		raw, err := ReadFrame(os.Stdin)
		if err != nil {
			return 0
		}
		var req map[string]json.RawMessage
		_ = json.Unmarshal(raw, &req)
		for op := range req {
			switch {
			case op == "load" && mode != "load-error":
				reply(fakeLoadFor(mode))
			case op == "eval_generate":
				reply(fakeEvalGenerate(mode))
			case op == "evaluate" && mode == "crash-on-evaluate":
				os.Exit(3)
			case op == "evaluate" && mode == "hang-on-evaluate":
				time.Sleep(time.Hour)
			case op == "evaluate" && mode == "grandchild-hang":
				gc := exec.Command("/usr/bin/sleep", "5") // inherits stdout: holds the pipe open past the kill
				gc.Stdout = os.Stdout
				if err := gc.Start(); err != nil {
					os.Exit(8)
				}
				time.Sleep(time.Hour)
			case op == "load" && mode == "load-error":
				reply(`{"error":{"message":"load refused"}}`)
			case op == "finish" && mode == "print-env":
				msg, _ := json.Marshal(strings.Join(os.Environ(), "\n"))
				reply(`{"finish":{"logs":[{"level":"notice","source":"env","message":` + string(msg) + `}]}}`)
			case op == "shutdown" && mode == "shutdown-never-exits":
				reply(`{"shutdown":{"logs":[]}}`)
				time.Sleep(time.Hour)
			case op == "shutdown" && mode == "shutdown-error-stays":
				reply(`{"error":{"message":"shutdown refused"}}`)
				time.Sleep(time.Hour) // ignores stdin EOF: only a kill ends it
			case op == "evaluate" && mode == "unknown-field":
				reply(`{"evaluate":{"events":[],"failed":false,"skipped":false,"invalidate_refreshes":false,"logs":[],"surprise":1}}`)
			case op == "evaluate" && mode == "wrong-op":
				reply(`{"refresh":{"refreshed":true,"error":null,"logs":[]}}`)
			case op == "evaluate" && mode == "bad-status":
				reply(`{"evaluate":{"events":[` + strings.Replace(fakeEvent, `"success"`, `"exploded"`, 1) +
					`],"failed":false,"skipped":false,"invalidate_refreshes":false,"logs":[]}}`)
			case op == "evaluate" && mode == "error-empty":
				reply(`{"error":{}}`)
			case op == "evaluate" && mode == "error-empty-message":
				reply(`{"error":{"message":""}}`)
			case op == "evaluate" && mode == "unsuitable-skipped":
				reply(`{"evaluate":{"events":[],"failed":true,"skipped":true,"unsuitable":true,"invalidate_refreshes":false,"logs":[]}}`)
			case op == "evaluate" && mode == "null-body":
				reply(`{"evaluate":null}`)
			case op == "refresh" && mode == "refresh-silent":
				reply(`{"refresh":{"refreshed":false,"error":null,"logs":[]}}`)
			case op == "refresh" && mode == "refresh-both":
				reply(`{"refresh":{"refreshed":true,"error":"boom","logs":[]}}`)
			case op == "refresh" && mode == "refresh-null":
				reply(`{"refresh":null}`)
			case op == "evaluate" && mode == "call-error":
				reply(`{"error":{"message":"RuntimeError: boom"}}`)
			case op == "evaluate":
				reply(`{"evaluate":{"events":[` + fakeEvent + `],"failed":false,"skipped":false,"invalidate_refreshes":false,` +
					`"logs":[{"level":"notice","source":"/Stage[main]/Main/Vox_value[a]/value","message":"defined 'value' as 'one'","file":"/s.pp","line":3}]}}`)
			case op == "relationships" && mode == "foreign-ref":
				reply(`{"relationships":{"relations":{"Vox_value[a]":{"require":["File[/not/in/catalog]"],"before":[],"subscribe":[],"notify":[]}},"logs":[]}}`)
			case op == "relationships":
				reply(`{"relationships":{"relations":{"Vox_value[a]":{"require":[],"before":[],"subscribe":[],"notify":["Vox_value[b]"]}},"logs":[]}}`)
			case op == "skip":
				reply(`{"skip":{"events":[],"failed":false,"skipped":true,"invalidate_refreshes":false,"logs":[]}}`)
			case op == "overly_deferred" && mode == "overly-deferred-bad-log":
				reply(`{"overly_deferred":{"logs":[{"level":"bogus","source":"Puppet","message":"x"}]}}`)
			case op == "overly_deferred":
				reply(`{"overly_deferred":{"logs":[{"level":"err","source":"/Stage[main]/Main/Vox_value[a]",` +
					`"message":"Provider never is not functional on this host"}]}}`)
			case op == "refresh" && mode == "refresh-stray-exception":
				reply(`{"refresh":{"refreshed":true,"error":null,"logs":[],` +
					`"exception_logs":[{"level":"err","source":"Puppet","message":"no exception"}]}}`)
			case op == "refresh":
				reply(`{"refresh":{"refreshed":false,"error":"refresh refused",` +
					`"logs":[{"level":"notice","source":"Puppet","message":"callback"}],` +
					`"exception_logs":[{"level":"err","source":"Puppet","message":"refresh refused"}]}}`)
			case op == "finish" && mode == "stderr-split":
				os.Stderr.WriteString("CRET\n")
				reply(`{"finish":{"logs":[]}}`)
			case op == "shutdown" && mode == "stderr-split":
				reply(`{"shutdown":{"logs":[]}}`)
				os.Stderr.WriteString("tail SECRET")
				return 0
			case op == "finish":
				reply(`{"finish":{"logs":[{"level":"notice","source":"Puppet","message":"post ran"}]}}`)
			case op == "shutdown":
				reply(`{"shutdown":{"logs":[]}}`)
				return 0
			}
		}
	}
}

var refs = []string{"Vox_value[a]", "Vox_value[b]"}

func startFake(t *testing.T, mode string, timeouts Timeouts) (*Host, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Start([]string{exe, "-test.run=^$"}, []string{"VOXAGENT_FAKE_HOST=" + mode}, "", timeouts, nil)
}

func quick() Timeouts { return Timeouts{Evaluate: 2 * time.Second, Other: 2 * time.Second} }

func loaded(t *testing.T, mode string) *Host { return mustLoad(t, mode, quick()) }

func mustLoad(t *testing.T, mode string, timeouts Timeouts) *Host {
	t.Helper()
	h, err := startFake(t, mode, timeouts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if _, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return h
}

func TestHostHappyPath(t *testing.T) {
	h := loaded(t, "ok")
	if got := h.Hello(); got.Protocol != 1 || got.PuppetVersion != "8.10.0" {
		t.Errorf("Hello = %+v", got)
	}
	rels, err := h.Relationships([]string{"Vox_value[a]"})
	if err != nil || len(rels["Vox_value[a]"].Notify) != 1 {
		t.Fatalf("Relationships = %v, %v", rels, err)
	}
	ev, err := h.Evaluate("Vox_value[a]", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(ev.Events) != 1 || ev.Events[0].Status != "success" || len(ev.Logs) != 1 || ev.Logs[0].Line != 3 {
		t.Errorf("Evaluate = %+v", ev)
	}
	rr, err := h.Refresh("Vox_value[a]")
	if err != nil || rr.Error == nil || *rr.Error != "refresh refused" || len(rr.Logs) != 1 ||
		len(rr.ExceptionLogs) != 1 || rr.ExceptionLogs[0].Message != "refresh refused" {
		t.Errorf("Refresh = %+v, %v", rr, err)
	}
	logs, err := h.Finish()
	if err != nil || len(logs) != 1 {
		t.Errorf("Finish = %v, %v", logs, err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if s := h.Stats(); s.Evaluated != 1 || s.Startup <= 0 || s.Spent <= 0 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestHostHandshakeFailures(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"wrong-protocol", "protocol 2"},
		{"garbage", "frame exceeds"},
		{"silent", "did not answer hello"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			_, err := startFake(t, tt.mode, Timeouts{Evaluate: time.Second, Other: 500 * time.Millisecond})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Start = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestStartRefusesAMissingInterpreter(t *testing.T) {
	_, err := Start([]string{"/nonexistent/ruby", "host.rb"}, nil, "", quick(), nil)
	if err == nil {
		t.Fatal("Start succeeded with no interpreter")
	}
}

// Every way a host can break mid-run kills it: the call fails, and every later
// call fails with the same cause instead of talking to a dead process.
func TestHostDeathIsSticky(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"crash-on-evaluate", "extension host failed"},
		{"hang-on-evaluate", "did not answer evaluate within"},
		{"unknown-field", "unknown field"},
		{"wrong-op", "answered refresh to evaluate"},
		{"bad-status", "status \"exploded\""},
		{"error-empty", "malformed error reply"},
		{"error-empty-message", "malformed error reply"},
		{"null-body", "null reply body"},
		{"unsuitable-skipped", "an unsuitable resource is failed, not skipped, with no events"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h, err := startFake(t, tt.mode, Timeouts{Evaluate: 300 * time.Millisecond, Other: 2 * time.Second})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer h.Close()
			if _, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice"); err != nil {
				t.Fatalf("Load: %v", err)
			}
			_, first := h.Evaluate("Vox_value[a]", false)
			if first == nil || !strings.Contains(first.Error(), tt.want) {
				t.Fatalf("Evaluate = %v, want an error containing %q", first, tt.want)
			}
			_, second := h.Evaluate("Vox_value[b]", false)
			if second == nil || second.Error() != first.Error() {
				t.Errorf("second Evaluate = %v, want the stored %v", second, first)
			}
		})
	}
}

// A refresh reply either refreshed or carries the callback's error; the host
// never sends anything else, so anything else kills it.
func TestHostRefusesInconsistentRefreshReplies(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"refresh-silent", "neither refreshed nor failed"},
		{"refresh-both", "both refreshed and failed"},
		{"refresh-null", "null reply body"},
		{"refresh-stray-exception", "exception lines without an error"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h := loaded(t, tt.mode)
			_, first := h.Refresh("Vox_value[a]")
			if first == nil || !strings.Contains(first.Error(), tt.want) {
				t.Fatalf("Refresh = %v, want an error containing %q", first, tt.want)
			}
			_, second := h.Refresh("Vox_value[b]")
			if second == nil || second.Error() != first.Error() {
				t.Errorf("second Refresh = %v, want the stored %v", second, first)
			}
		})
	}
}

func TestHostOverlyDeferred(t *testing.T) {
	h := loaded(t, "ok")
	logs, err := h.OverlyDeferred("Vox_value[a]")
	if err != nil || len(logs) != 1 || logs[0].Level != "err" ||
		logs[0].Message != "Provider never is not functional on this host" {
		t.Errorf("OverlyDeferred = %+v, %v", logs, err)
	}
	if _, err := h.OverlyDeferred("Nope[x]"); err == nil {
		t.Error("overly_deferred of a ref outside the catalog was sent")
	}
}

// An overly_deferred reply is validated like every other: an invalid log line
// kills the host.
func TestHostRefusesAnInvalidOverlyDeferredReply(t *testing.T) {
	h := loaded(t, "overly-deferred-bad-log")
	_, err := h.OverlyDeferred("Vox_value[a]")
	if err == nil || !strings.Contains(err.Error(), `overly_deferred Vox_value[a]: log level "bogus"`) {
		t.Fatalf("OverlyDeferred = %v, want a protocol error naming the log level", err)
	}
	if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
		t.Errorf("Finish = %v, want the stored %v: the host survived a hostile answer", again, err)
	}
}

func TestHostCallErrorLeavesHostUsable(t *testing.T) {
	h := loaded(t, "call-error")
	_, err := h.Evaluate("Vox_value[a]", false)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Message != "RuntimeError: boom" {
		t.Fatalf("Evaluate = %v, want a CallError", err)
	}
	if _, err := h.Relationships([]string{"Vox_value[a]"}); err != nil {
		t.Errorf("Relationships after a CallError = %v, want the host still usable", err)
	}
}

func TestHostRefusesForeignRefs(t *testing.T) {
	h := loaded(t, "foreign-ref")
	_, err := h.Relationships([]string{"Vox_value[a]"})
	if err == nil || !strings.Contains(err.Error(), "File[/not/in/catalog]") {
		t.Errorf("Relationships = %v, want a protocol error naming the foreign ref", err)
	}
}

// A ref outside the catalog is refused before anything is sent, so the host is
// untouched and still usable.
func TestHostRefusesToAskAboutForeignRefs(t *testing.T) {
	h := loaded(t, "ok")
	_, err := h.Evaluate("Nope[x]", false)
	if err == nil || !strings.Contains(err.Error(), "Nope[x]") {
		t.Fatalf("Evaluate = %v, want an error naming Nope[x]", err)
	}
	if _, err := h.Evaluate("Vox_value[a]", false); err != nil {
		t.Errorf("Evaluate after a refused ref = %v, want the host still usable", err)
	}
}

func TestFailedLoadLeavesNothingKnown(t *testing.T) {
	h, err := startFake(t, "load-error", quick())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var ce *CallError
	if _, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice"); !errors.As(err, &ce) {
		t.Fatalf("Load = %v, want a CallError", err)
	}
	if _, err := h.Evaluate("Vox_value[a]", false); err == nil || !strings.Contains(err.Error(), "not in the catalog") {
		t.Errorf("Evaluate after a failed Load = %v, want the ref refused as unknown", err)
	}
}

func TestStartPassesNoEnvironmentWhenGivenNone(t *testing.T) {
	t.Setenv("RUBYOPT", "-r/evil")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Start([]string{exe, "-fake-host=print-env"}, nil, "", quick(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	logs, err := h.Finish()
	if err != nil || len(logs) != 1 {
		t.Fatalf("Finish = %v, %v", logs, err)
	}
	if logs[0].Message != "" {
		t.Errorf("the host's environment = %q, want it empty", logs[0].Message)
	}
}

// A descendant holding the host's stdout open must not stall a timed-out call.
func TestHostTimeoutDoesNotWaitForDescendants(t *testing.T) {
	h := mustLoad(t, "grandchild-hang", Timeouts{Evaluate: 300 * time.Millisecond, Other: 2 * time.Second})
	started := time.Now()
	_, err := h.Evaluate("Vox_value[a]", false)
	if err == nil || !strings.Contains(err.Error(), "did not answer evaluate within") {
		t.Fatalf("Evaluate = %v, want a timeout", err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("Evaluate took %s, want about the 300ms timeout", took)
	}
}

// Close must not wait for a host that will not leave: a shutdown error kills it
// at once, and a host that answers shutdown but lingers is killed when
// timeouts.Other runs out.
func TestCloseIsBounded(t *testing.T) {
	const other = 3 * time.Second
	for _, tt := range []struct {
		mode, want string
		within     time.Duration
	}{
		{"shutdown-never-exits", "did not exit within", other + time.Second},
		{"shutdown-error-stays", "shutdown refused", time.Second},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h := mustLoad(t, tt.mode, Timeouts{Evaluate: time.Second, Other: other})
			started := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- h.Close() }()
			select {
			case err := <-closed:
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("Close = %v, want an error containing %q", err, tt.want)
				}
				if took := time.Since(started); took > tt.within {
					t.Errorf("Close took %s, want under %s", took, tt.within)
				}
				if h.cmd.ProcessState == nil {
					t.Error("the host process was not reaped")
				}
			case <-time.After(other + 2*time.Second):
				t.Fatal("Close did not return")
			}
		})
	}
}

// A failed handshake closes the host, and Close removes its directory.
func TestFailedHandshakeRemovesTheDirectory(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() + "/host"
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Start([]string{exe, "-fake-host=wrong-protocol"}, nil, dir, quick(), nil); err == nil {
		t.Fatal("Start succeeded against the wrong protocol")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(dir) = %v, want the directory removed", err)
	}
}

func TestLoadRefusesHostileGenerations(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"gen-collides", "Vox_value[a]"},
		{"gen-container-foreign", "File[/nope]"},
		{"gen-before-foreign", "File[/nope]"},
		{"gen-override-unknown", "File[/nope]"},
		{"gen-override-sensitive-stray", "content"},
		{"alias-collides", "Vox_value[b]"},
		{"alias-wrong-type", "File[x]"},
		{"gen-empty-title", "no type or no title"},
		{"gen-bad-kind", "Notify[badkind]"},
		{"gen-exported", "Notify[exp]"},
		{"gen-repeated", "Notify[twice]"},
		{"gen-container-not-generated", "Vox_value[b]"},
		{"gen-override-added", "Notify[fresh]"},
		{"gen-type-brackets", `"No[tify"`},
		{"gen-type-lowercase", `"vox_value"`},
		{"gen-type-segment-case", `"Vox::value"`},
		{"alias-malformed", "nonsense"},
		{"alias-twice", "Vox_value[x]"},
		{"selfrefresh-tier2", "self_refresh"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h, err := startFake(t, tt.mode, quick())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer h.Close()
			_, err = h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want a protocol error naming %s", err, tt.want)
			}
			// Finish names no ref, so only the stored failure can answer it.
			if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
				t.Errorf("Finish = %v, want the stored %v: the host survived a hostile generation", again, err)
			}
		})
	}
}

// Puppet's own ref is a ref of the resource's own type, titled
// by its PuppetTitle, other than the catalog's and every other resource's
// ref; only an override makes a resource unresolvable.
func TestLoadRefusesHostileTitles(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"title-wrong-type", "File[x]"},
		{"title-collides", "Vox_value[b]"},
		{"title-twice", "Vox_value[x]"},
		{"title-is-alias", "Vox_value[a] has the Puppet ref Vox_value[x], which Vox_value[b] has as an alias"},
		{"title-mismatch", "Vox_value[x]"},
		{"title-equals-catalog", "Vox_value[a]"},
		{"title-bare", "Vox_value[a]"},
		{"unresolvable-bare", "Vox_value[a]"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h, err := startFake(t, tt.mode, quick())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer h.Close()
			_, err = h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want a protocol error naming %s", err, tt.want)
			}
			if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
				t.Errorf("Finish = %v, want the stored %v: the host survived a hostile title", again, err)
			}
		})
	}
}

func TestLoadAcceptsPuppetRefs(t *testing.T) {
	h, err := startFake(t, "title-ok", quick())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	load, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := load.Resources["Vox_value[a]"]; got.PuppetRef != "Vox_value[a:x]" || got.PuppetTitle != "a:x" || !got.Unresolvable {
		t.Errorf("Resources[Vox_value[a]] = %+v, want Puppet's ref Vox_value[a:x], unresolvable", got)
	}
}

// A generator is named like a container: known, or added by the same
// generation, and only for a resource that generation added.
func TestLoadRefusesHostileGenerators(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"gen-generator-foreign", "File[/nope]"},
		{"gen-generator-not-generated", "Vox_value[b]"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h, err := startFake(t, tt.mode, quick())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer h.Close()
			if _, err = h.Load([]byte(`{}`), refs, "/lib", "/var", "notice"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want a protocol error naming %s", err, tt.want)
			}
			// Finish names no ref, so only the stored failure can answer it.
			if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
				t.Errorf("Finish = %v, want the stored %v: the host survived a hostile generator", again, err)
			}
		})
	}
}

func TestLoadAcceptsAGeneratedGenerator(t *testing.T) {
	h, err := startFake(t, "gen-generated-generator", quick())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	res, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Generated.Generators["Notify[nested]"]; got != "Notify[made]" {
		t.Errorf("Generators = %v", res.Generated.Generators)
	}
}

func TestLoadAcceptsAGenerationAndKnowsItsRefs(t *testing.T) {
	h, err := startFake(t, "gen-ok", quick())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	res, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(res.Generated.Resources) != 1 || res.Generated.Containers["Notify[made]"] != "Vox_value[a]" ||
		len(res.Generated.Before) != 1 {
		t.Errorf("Generated = %+v", res.Generated)
	}
	if _, err := h.Evaluate("Notify[made]", false); err != nil {
		t.Errorf("a generated ref is unknown to the client: %v", err)
	}
}

func TestLoadAcceptsAliases(t *testing.T) {
	h, err := startFake(t, "alias-ok", quick())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer h.Close()
	res, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Resources["Vox_value[a]"].Aliases; len(got) != 1 || got[0] != "Vox_value[x]" {
		t.Errorf("Aliases = %v", got)
	}
}

// The order names every known and generated ref once and
// nothing else, the catalog's own refs in the catalog's order. A refused load (Invalid not empty) never runs, and Puppet's
// catalog holds no resource it refused, so its order may omit refs but still
// names nothing unknown and nothing twice.
func TestLoadRefusesHostileOrders(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"order-unknown", "File[/nope]"},
		{"order-repeat", "Vox_value[a]"},
		{"order-missing", "Vox_value[b]"},
		{"order-reordered", "the order names Vox_value[a] after Vox_value[b]"},
		{"order-invalid-reordered", "the order names Vox_value[a] after Vox_value[b]"},
		{"order-invalid-unknown", "File[/nope]"},
		{"order-invalid-repeat", "Vox_value[a]"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h, err := startFake(t, tt.mode, quick())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer h.Close()
			if _, err = h.Load([]byte(`{}`), refs, "/lib", "/var", "notice"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load = %v, want a protocol error naming %s", err, tt.want)
			}
			if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
				t.Errorf("Finish = %v, want the stored %v: the host survived a hostile order", again, err)
			}
		})
	}
	t.Run("order-invalid-missing", func(t *testing.T) {
		h, err := startFake(t, "order-invalid-missing", quick())
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer h.Close()
		load, err := h.Load([]byte(`{}`), refs, "/lib", "/var", "notice")
		if err != nil {
			t.Fatalf("Load = %v: a refused load's order may omit the refused resource", err)
		}
		if len(load.Invalid) != 1 || !slices.Equal(load.Order, []string{"Vox_value[a]"}) {
			t.Errorf("Load = invalid %v, order %v", load.Invalid, load.Order)
		}
	})
}

func TestEvalGenerate(t *testing.T) {
	h := loaded(t, "evalgen-ok")
	res, err := h.EvalGenerate("Vox_value[a]")
	if err != nil {
		t.Fatalf("EvalGenerate: %v", err)
	}
	if len(res.Resources) != 1 || res.Containers["Notify[made]"] != "Vox_value[a]" ||
		len(res.Existing) != 1 || res.Existing[0] != "Vox_value[b]" || res.Failed {
		t.Errorf("EvalGenerate = %+v", res)
	}
	if _, err := h.Evaluate("Notify[made]", false); err != nil {
		t.Errorf("a generated ref is unknown to the client: %v", err)
	}
	if _, err := h.EvalGenerate("Nope[x]"); err == nil {
		t.Error("eval_generate of a ref outside the catalog was sent")
	}
}

func TestEvalGenerateRefusesHostileAnswers(t *testing.T) {
	for _, tt := range []struct{ mode, want string }{
		{"evalgen-existing-foreign", "File[/nope]"},
		{"evalgen-new-collides", "Vox_value[b]"},
		{"evalgen-alias", "Vox_value[x]"},
		{"evalgen-bad-log", "bogus"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			h := loaded(t, tt.mode)
			_, err := h.EvalGenerate("Vox_value[a]")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("EvalGenerate = %v, want a protocol error naming %s", err, tt.want)
			}
			// Finish names no ref, so only the stored failure can answer it.
			if _, again := h.Finish(); again == nil || again.Error() != err.Error() {
				t.Errorf("Finish = %v, want the stored %v: the host survived a hostile answer", again, err)
			}
			if _, known := h.known["Notify[made]"]; known {
				t.Error("a refused generation added refs")
			}
		})
	}
}

// Puppet::Resource renders a type with each "::" segment capitalised and the
// rest lower case; anything else would let a host add file[/x] beside File[/x].
func TestCanonicalType(t *testing.T) {
	for typ, want := range map[string]bool{
		"Notify": true, "Concat_fragment": true, "Vox::Value": true, "File_line": true, "Vox2::A_b": true,
		"notify": false, "NOTIFY": false, "Concat_Fragment": false, "Vox::value": false,
		"vox::Value": false, "Vox::": false, "::Vox": false, "Vox::::Value": false,
	} {
		if got := canonicalType(typ); got != want {
			t.Errorf("canonicalType(%q) = %v, want %v", typ, got, want)
		}
	}
}

// captureStderr runs f with os.Stderr a pipe and returns what was written to
// it, restoring os.Stderr however f ends.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(rd)
		rd.Close()
		read <- b
	}()
	saved := os.Stderr
	os.Stderr = wr
	func() {
		defer func() {
			os.Stderr = saved
			wr.Close()
		}()
		f()
	}()
	return string(<-read)
}

// The host's stderr reaches voxagent's line by line through scrub, a line
// split across writes and a form split with it included, and a final line
// without a newline is written at Close as it is. An identity scrub, like none,
// leaves every byte as the host wrote it.
func TestHostStderrGoesThroughScrub(t *testing.T) {
	const raw = "one SECRET\ntwo SECRET\ntail SECRET"
	for _, c := range []struct {
		name  string
		scrub func(string) string
		want  string
	}{
		{"scrubbed", func(s string) string { return strings.ReplaceAll(s, "SECRET", "[x]") }, "one [x]\ntwo [x]\ntail [x]"},
		{"identity", func(s string) string { return s }, raw},
		{"none", nil, raw},
	} {
		t.Run(c.name, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			got := captureStderr(t, func() {
				h, err := Start([]string{exe, "-fake-host=stderr-split"}, nil, "", quick(), c.scrub)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.Finish(); err != nil {
					t.Error(err)
				}
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			})
			if got != c.want {
				t.Errorf("stderr = %q, want %q", got, c.want)
			}
		})
	}
}

// A descendant that keeps the host's stderr open does not hold Close once the
// host itself has exited, and what the host wrote is still scrubbed.
func TestHostCloseDoesNotWaitForStderrDescendants(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var elapsed time.Duration
	got := captureStderr(t, func() {
		h, err := Start([]string{exe, "-fake-host=stderr-grandchild"}, nil, "",
			Timeouts{Evaluate: 10 * time.Second, Other: 10 * time.Second},
			func(s string) string { return strings.ReplaceAll(s, "SECRET", "[x]") })
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if err := h.Close(); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
		elapsed = time.Since(started)
	})
	if elapsed > 5*time.Second {
		t.Errorf("Close took %s, want it to return once the host exits", elapsed)
	}
	if got != "own [x]\n" {
		t.Errorf("stderr = %q, want the host's own line scrubbed", got)
	}
}

// A line that reaches maxPendingStderr bytes without a newline is written,
// scrubbed, rather than held; the rest of it follows as usual.
func TestScrubbedLinesFlushesALongLine(t *testing.T) {
	var out strings.Builder
	l := &scrubbedLines{w: &out, scrub: func(s string) string { return strings.ReplaceAll(s, "SECRET", "[x]") }}
	filler := strings.Repeat("a", maxPendingStderr-len("SECRET")-1)
	if _, err := l.Write([]byte("SECRET" + filler)); err != nil || out.Len() != 0 {
		t.Fatalf("Write = %v; wrote %d bytes below the cap", err, out.Len())
	}
	if _, err := l.Write([]byte("bc")); err != nil {
		t.Fatal(err)
	}
	if want := "[x]" + filler + "bc"; out.String() != want {
		t.Fatalf("wrote %d bytes at the cap, want %d, scrubbed", out.Len(), len(want))
	}
	if _, err := l.Write([]byte("d\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "bcd\n") || len(l.pending) != 0 {
		t.Errorf("the rest of the line was not written after the capped part")
	}
}
