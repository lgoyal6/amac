package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/acp"
	"github.com/lgoyal6/amac/internal/event"
)

// The session lifecycle is the part of amac that actually starts, drives and
// kills agents, and until this file it was the least tested thing in the
// repository: twelve functions at zero coverage, including handlePermission,
// which is the path where an agent asks to do something dangerous and a human
// approves it.
//
// Testing it against a mock of our own client would have proved that the mock
// behaves. So these drive a real child process over a real pipe speaking real
// ACP, the same way the queue's crash test kills real workers. The fake agent
// is this test binary re-executed with an environment variable set, which is
// also how it can be made to misbehave on demand.
//
// Start() resolves its adapter through agent.Adapter.Argv(), which prefers
// $HOME/.amac/adapters/node_modules/.bin/<bin>. Pointing HOME at a temp
// directory containing a script there is what lets the real Start path run
// against a fake agent with no production code changed for testability.

func TestMain(m *testing.M) {
	if os.Getenv("AMAC_FAKE_AGENT") != "" {
		fakeAgent()
		return
	}
	os.Exit(m.Run())
}

// fakeAgent speaks enough ACP to be supervised: it handshakes, opens a
// session, and then does whatever the scenario asks of it.
func fakeAgent() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	scenario := os.Getenv("AMAC_FAKE_SCENARIO")

	send := func(v any) { _ = out.Encode(v) }

	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": acp.ProtocolVersion,
				"agentInfo":       map[string]any{"name": "fake", "version": "9.9.9"},
			}})
		case "session/new":
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"sessionId": "fake-session-1",
			}})
			if scenario == "notify" {
				// One tool call, so consume() has a real update to record.
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "fake-session-1",
					"update": map[string]any{
						"sessionUpdate": string(acp.UpdateToolCall),
						"toolCallId":    "tc-1",
						"title":         "Read main.go",
						"status":        "pending",
					},
				}})
			}
		case "session/prompt":
			if scenario == "fsops" {
				// Drive Handle's real branches and record what came back, so
				// the assertions are on what an agent actually receives.
				results := map[string]any{}
				ask := func(label, method string, params any) {
					id := label
					send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
					for in.Scan() {
						var r struct {
							ID    string `json:"id"`
							Error *struct {
								Code    int    `json:"code"`
								Message string `json:"message"`
							} `json:"error"`
							Result json.RawMessage `json:"result"`
						}
						if json.Unmarshal(in.Bytes(), &r) != nil || r.ID != id {
							continue
						}
						if r.Error != nil {
							results[label] = map[string]any{"code": r.Error.Code, "message": r.Error.Message}
						} else {
							results[label] = map[string]any{"code": 0, "result": string(r.Result)}
						}
						return
					}
				}
				ask("relative", "fs/write_text_file", map[string]any{"path": "notes.md", "content": "x"})
				ask("absolute", "fs/write_text_file", map[string]any{
					"path": os.Getenv("AMAC_FAKE_WRITE"), "content": "hello"})
				ask("unknown", "terminal/create", map[string]any{})
				b, _ := json.Marshal(results)
				_ = os.WriteFile(os.Getenv("AMAC_FAKE_RESULTS"), b, 0o644)
			}
			if scenario == "permission" {
				// Ask before finishing the turn, which is the real ordering:
				// the agent blocks on us, not the other way round.
				send(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "session/request_permission",
					"params": map[string]any{
						"sessionId": "fake-session-1",
						"toolCall":  map[string]any{"toolCallId": "tc-danger", "title": "Run rm -rf /tmp/x"},
						"options": []map[string]any{
							{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
							{"optionId": "allow-always", "name": "Allow always", "kind": "allow_always"},
							{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
						},
					}})
				// Wait for the answer before replying to the prompt.
				for in.Scan() {
					var resp struct {
						ID     json.RawMessage `json:"id"`
						Result json.RawMessage `json:"result"`
					}
					if json.Unmarshal(in.Bytes(), &resp) == nil && len(resp.Result) > 0 {
						break
					}
				}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"stopReason": "end_turn",
			}})
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "fake agent: " + req.Method}})
		}
	}
}

// withFakeAgent points HOME at a temp tree whose adapter path holds a script
// that re-executes this test binary as the fake agent.
func withFakeAgent(t *testing.T, scenario string) *Supervisor {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, ".amac", "adapters", "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nAMAC_FAKE_AGENT=1 AMAC_FAKE_SCENARIO=%s exec %q\n", scenario, self)
	if err := os.WriteFile(filepath.Join(bin, "claude-agent-acp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(log)
}

func kinds(t *testing.T, log *event.Log, kind event.Kind) []map[string]any {
	t.Helper()
	rows, err := log.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY seq`, string(kind))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// Start is the whole entry path: spawn, handshake, open a session, register it,
// and record what it is spending against.
func TestStartHandshakesAndRegistersTheSession(t *testing.T) {
	sup := withFakeAgent(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Shutdown()

	if sess.ACPID != "fake-session-1" {
		t.Errorf("acp session id = %q, want the one the agent returned", sess.ACPID)
	}
	if st, _ := sess.State(); st != StateIdle {
		t.Errorf("state after start = %q, want idle", st)
	}
	if got, ok := sup.Get(sess.ID); !ok || got != sess {
		t.Error("a started session must be retrievable by id")
	}

	started := kinds(t, sup.log, event.KindSessionStarted)
	if len(started) != 1 {
		t.Fatalf("recorded %d session.started events, want 1", len(started))
	}
	// The adapter's own name and version are recorded, not ours. That is the
	// field that says which build of a vendor's adapter produced a session.
	if started[0]["adapter"] != "fake" || started[0]["version"] != "9.9.9" {
		t.Errorf("adapter identity not recorded: %v", started[0])
	}
	if started[0]["acpSessionId"] != "fake-session-1" {
		t.Errorf("acp session id not recorded: %v", started[0])
	}
}

// Ids must be unique across daemon restarts, because in an event-sourced
// system the id is the join key and a reused one is data corruption rather
// than a cosmetic clash.
func TestSessionIdsDoNotCollide(t *testing.T) {
	sup := withFakeAgent(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer sup.Shutdown()

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		s, err := sup.Start(ctx, "claude", t.TempDir())
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if seen[s.ID] {
			t.Fatalf("session id %q reused", s.ID)
		}
		seen[s.ID] = true
		if !strings.HasPrefix(s.ID, "claude-") {
			t.Errorf("id %q should name its agent", s.ID)
		}
	}
}

// The question this whole project exists to answer. An agent asks, the session
// goes blocked with the real title, a human answers, and the answer travels
// back down the protocol rather than being typed at a screen.
func TestPermissionRequestBlocksUntilAnswered(t *testing.T) {
	sup := withFakeAgent(t, "permission")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Shutdown()

	done := make(chan error, 1)
	go func() { _, e := sess.Prompt(ctx, "delete something"); done <- e }()

	// Wait for the agent to ask.
	var p *Pending
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if p = sess.Pending(); p != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p == nil {
		t.Fatal("the agent asked and the session never went pending")
	}
	if st, detail := sess.State(); st != StateBlocked || detail != "Run rm -rf /tmp/x" {
		t.Errorf("state = %q/%q, want blocked with the tool call's own title", st, detail)
	}
	if len(p.Options) != 3 {
		t.Errorf("got %d options, want the 3 the agent offered", len(p.Options))
	}

	if err := sess.Answer("allow-once"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("prompt did not finish after the answer: %v", err)
	}

	if sess.Pending() != nil {
		t.Error("pending must be cleared once answered")
	}
	asked := kinds(t, sup.log, event.KindPermissionRequested)
	answered := kinds(t, sup.log, event.KindPermissionAnswered)
	if len(asked) != 1 || len(answered) != 1 {
		t.Fatalf("recorded %d asks and %d answers, want 1 each", len(asked), len(answered))
	}
	if answered[0]["optionId"] != "allow-once" {
		t.Errorf("recorded the wrong answer: %v", answered[0])
	}
	// Without a waited_ms the log cannot say how long a human took, which is
	// the only measure of whether the notification path is working.
	if _, ok := answered[0]["waited_ms"]; !ok {
		t.Error("an answer must record how long it was blocked")
	}
}

// Answering twice happens for real: the board and the phone can both be
// looking at the same question. The second must be a no-op, not a panic on a
// closed channel.
func TestAnsweringTwiceIsSafe(t *testing.T) {
	sup := withFakeAgent(t, "permission")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()

	go func() { _, _ = sess.Prompt(ctx, "go") }()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if sess.Pending() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sess.Pending() == nil {
		t.Fatal("never went pending")
	}
	if err := sess.Answer("allow-once"); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	// The second answer may find the question already gone, which is fine.
	// What it must not do is panic or block.
	_ = sess.Answer("deny")
}

// An automatic policy must prefer the narrowest affirmative option. Granting
// standing permission changes what every future turn may do without anyone
// deciding that, and it is the failure mode of an unattended approver.
func TestAutoPolicyTakesTheNarrowestAllow(t *testing.T) {
	sup := withFakeAgent(t, "permission")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()

	if err := sess.SetPresentation("", PermissionAuto); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Prompt(ctx, "go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	answered := kinds(t, sup.log, event.KindPermissionAnswered)
	if len(answered) != 1 {
		t.Fatalf("recorded %d answers, want 1", len(answered))
	}
	if answered[0]["optionId"] != "allow-once" {
		t.Errorf("auto mode chose %v, want allow-once over allow-always", answered[0]["optionId"])
	}
}

// consume is the stream a terminal renders and throws away. Keeping it is what
// makes the dashboard and replay possible, so a tool call has to reach the log.
func TestUpdatesReachTheLog(t *testing.T) {
	sup := withFakeAgent(t, "notify")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()

	var found map[string]any
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		for _, u := range kinds(t, sup.log, event.KindSessionUpdate) {
			if u["toolCallId"] == "tc-1" {
				found = u
			}
		}
		if found != nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("a tool call notification never reached the event log")
	}
	if found["title"] != "Read main.go" {
		t.Errorf("title not recorded: %v", found)
	}
	if st, _ := sess.State(); st != StateWorking {
		t.Errorf("a titled tool call should move the session to working, got %q", st)
	}
}

// A crashed agent must show as ended rather than sitting forever as working,
// and it must leave the registry so the board stops offering it.
func TestAgentDeathEndsTheSession(t *testing.T) {
	sup := withFakeAgent(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	sess.Close()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, still := sup.Get(sess.ID); !still {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, still := sup.Get(sess.ID); still {
		t.Error("a dead session must leave the registry")
	}
	if st, _ := sess.State(); st != StateEnded {
		t.Errorf("state = %q, want ended", st)
	}
	sup.Shutdown()
}

// Shutdown waits for the per-session goroutines, because returning early means
// the caller closes the event log while consume and watchExit are still
// writing, and the final events of every run are lost to "database is closed".
func TestShutdownWaitsForSessionGoroutines(t *testing.T) {
	sup := withFakeAgent(t, "notify")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if _, err := sup.Start(ctx, "claude", t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	sup.Shutdown()

	if n := len(sup.List()); n != 0 {
		t.Errorf("%d sessions survived shutdown", n)
	}
	// The log must still be writable afterwards, which is the property the
	// wait exists to guarantee.
	ended := kinds(t, sup.log, event.KindSessionEnded)
	if len(ended) != 2 {
		t.Errorf("recorded %d ended events, want 2", len(ended))
	}
}

// Blocked is the query the product exists to answer, and it has to be a fact
// about live sessions rather than a scan of rendered text.
func TestBlockedListsOnlyWaitingSessions(t *testing.T) {
	sup := withFakeAgent(t, "permission")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()

	if len(sup.Blocked()) != 0 {
		t.Error("nothing is blocked before anything is asked")
	}
	go func() { _, _ = sess.Prompt(ctx, "go") }()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if len(sup.Blocked()) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := sup.Blocked(); len(got) != 1 || got[0].ID != sess.ID {
		t.Errorf("Blocked() = %v, want the one waiting session", got)
	}
	_ = sess.Answer("deny")
}

// Handle's three branches, exercised the way an agent actually reaches them:
// a relative path must be refused rather than resolved against whatever
// directory the daemon happens to be in, an absolute one must land and be
// recorded as an actuation, and an unimplemented method must be refused
// explicitly, because an agent that receives an error can report it while one
// that receives silence waits forever.
func TestHandleAnswersEveryRequestAnAgentMakes(t *testing.T) {
	dir := t.TempDir()
	results := filepath.Join(dir, "results.json")
	target := filepath.Join(dir, "sub", "out.txt")
	t.Setenv("AMAC_FAKE_RESULTS", results)
	t.Setenv("AMAC_FAKE_WRITE", target)

	sup := withFakeAgent(t, "fsops")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	sess, err := sup.Start(ctx, "claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Shutdown()

	if _, err := sess.Prompt(ctx, "do file things"); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	b, err := os.ReadFile(results)
	if err != nil {
		t.Fatalf("the agent recorded no results: %v", err)
	}
	var got map[string]struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("results unreadable: %s", b)
	}
	if got["relative"].Code != -32602 {
		t.Errorf("relative path: code %d, want -32602", got["relative"].Code)
	}
	if got["unknown"].Code != -32601 {
		t.Errorf("unknown method: code %d, want -32601", got["unknown"].Code)
	}
	if got["absolute"].Code != 0 {
		t.Errorf("absolute write refused: %+v", got["absolute"])
	}
	if c, err := os.ReadFile(target); err != nil || string(c) != "hello" {
		t.Errorf("file not written: %v %q", err, c)
	}
	// A write an agent makes is an actuation. Without it, "what did it change"
	// is unanswerable after the fact.
	acts := kinds(t, sup.log, event.KindActuation)
	if len(acts) != 1 || acts[0]["path"] != target {
		t.Errorf("actuation not recorded: %v", acts)
	}
}
