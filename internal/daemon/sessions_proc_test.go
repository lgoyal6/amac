package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/procs"
)

// An ACP adapter runs a claude process of its own, and a claude inside tmux is
// already a tmux card. Either on the board twice would double the count the
// whole feature exists to get right.
func TestProcessSessionsAreNotListedTwice(t *testing.T) {
	now := time.Now()
	list := []procs.Session{
		{PID: 10, Agent: "claude", Kind: "desktop", Model: "claude-opus-5", ResumeID: "acp-owned", Elapsed: time.Minute},
		{PID: 11, Agent: "claude", Kind: "terminal", InTmux: true, Elapsed: time.Minute},
		{PID: 12, Agent: "claude", Kind: "desktop", Model: "claude-opus-5", ResumeID: "desk-1",
			Dir: "/Users/x/hackqueue", Elapsed: 2 * time.Hour, LastActivity: now.Add(-30 * time.Second)},
		{PID: 13, Agent: "claude", Kind: "terminal", Command: "claude --continue", Dir: "/Users/x", Elapsed: time.Hour},
		{PID: 14, Agent: "codex", Kind: "desktop", Elapsed: time.Hour},
	}
	got := procViews(list, map[string]bool{"acp-owned": true}, now)
	if len(got) != 3 {
		t.Fatalf("got %d cards, want 3: %+v", len(got), got)
	}

	desk := got[0]
	if desk.ID != "pid-12" || desk.Kind != "desktop" || desk.PID != 12 || desk.Model != "claude-opus-5" {
		t.Errorf("desktop card = %+v", desk)
	}
	if desk.State != "working" || desk.Since == nil {
		t.Errorf("a transcript written 30s ago should read working with a since: %+v", desk)
	}
	if desk.ContinueCommand != "claude --resume 'desk-1'" {
		t.Errorf("continue command = %q", desk.ContinueCommand)
	}
	if desk.Detail != "desktop app, claude-opus-5" {
		t.Errorf("detail = %q", desk.Detail)
	}
	if !desk.Started.Equal(now.Add(-2 * time.Hour)) {
		t.Errorf("started = %v, want two hours before now", desk.Started)
	}

	term := got[1]
	if term.ID != "pid-13" || term.Kind != "terminal" || term.Detail != "claude --continue" {
		t.Errorf("terminal card = %+v", term)
	}
	if term.State != "unknown" || term.Since != nil || term.ContinueCommand != "" {
		t.Errorf("no transcript means unknown, no since, nothing to resume: %+v", term)
	}
	if got[2].Agent != "codex" || got[2].State != "unknown" {
		t.Errorf("codex card = %+v", got[2])
	}
}

// A card from the process table offers nothing to press, and the API has to
// agree with that. "No such session" would be a lie: the session is right
// there on the board. The refusal has to say what the session is.
func TestActionsOnAProcessSessionAreRefusedAsSuch(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/sessions/pid-4242/prompt", `{"text":"hello"}`},
		{"POST", "/api/sessions/pid-4242/answer", `{"optionId":"allow"}`},
		{"PATCH", "/api/sessions/pid-4242", `{"name":"x"}`},
		{"DELETE", "/api/sessions/pid-4242", ""},
		{"POST", "/api/sessions/pid-4242/keys", `{"key":"Enter"}`},
		{"POST", "/api/sessions/pid-4242/open-on-mac", ""},
	} {
		code, body := post(t, s, tc.method, tc.path, tc.body)
		if code != 400 {
			t.Errorf("%s %s returned %d, want 400 (%v)", tc.method, tc.path, code, body)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "process table") {
			t.Errorf("%s %s: error %q does not say where the session came from", tc.method, tc.path, msg)
		}
	}

	// The guard is for actions. A read still gets whatever answer the handler
	// has, and a task whose id happens to start the same way is not a session.
	if code, body := post(t, s, "GET", "/api/sessions/pid-4242/pane", ""); code == 400 {
		t.Errorf("a read was refused as an action: %v", body)
	}
	if _, body := post(t, s, "DELETE", "/api/tasks/pid-4242", ""); strings.Contains(body["error"].(string), "process table") {
		t.Errorf("a task was mistaken for a process session: %v", body)
	}
}
