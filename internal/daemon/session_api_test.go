package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/attention"
	"github.com/lgoyal6/amac/internal/event"
)

// The session control API is what a phone drives: start, rename, set a
// permission mode, prompt, answer, stop. None of it was tested, and the
// interesting cases are the ones where the request is wrong, because a request
// from a phone is routinely stale by the time a thumb reaches it.

func post(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed(method, path, body))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// Every handler that names a session must say so when it does not exist,
// rather than acting on nothing and reporting success.
func TestSessionHandlersRefuseAnUnknownID(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct{ method, path, body string }{
		{"PATCH", "/api/sessions/ghost", `{"name":"x"}`},
		{"POST", "/api/sessions/ghost/prompt", `{"text":"hello"}`},
		{"POST", "/api/sessions/ghost/answer", `{"optionId":"allow"}`},
		{"DELETE", "/api/sessions/ghost", ""},
		{"GET", "/api/sessions/ghost/pane", ""},
	} {
		code, body := post(t, s, tc.method, tc.path, tc.body)
		if code != 404 {
			t.Errorf("%s %s returned %d, want 404 (%v)", tc.method, tc.path, code, body)
		}
	}
}

// A malformed body is the caller's fault and must be said so, not turned into a
// 500 that reads like the daemon broke.
func TestMalformedBodiesAreRejectedAsBadRequests(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct{ path, body string }{
		{"/api/sessions", `{"agent":`},
		{"/api/tasks", `not json at all`},
	} {
		code, _ := post(t, s, "POST", tc.path, tc.body)
		if code != 400 {
			t.Errorf("POST %s with %q returned %d, want 400", tc.path, tc.body, code)
		}
	}
}

// A prompt with no text is not a prompt. Accepting it would start a turn that
// costs money and does nothing.
func TestAnEmptyPromptIsRefused(t *testing.T) {
	s := testServer(t)
	code, body := post(t, s, "POST", "/api/sessions/any/prompt", `{"text":""}`)
	if code == 202 {
		t.Errorf("an empty prompt was accepted: %v", body)
	}
}

// Starting a session with an unknown agent must name what exists. The board
// sends whatever it was rendered from, and the picker can be stale.
func TestStartingAnUnknownAgentSaysWhatIsAvailable(t *testing.T) {
	s := testServer(t)
	code, body := post(t, s, "POST", "/api/sessions", `{"agent":"gemini","dir":"/tmp"}`)
	if code == 200 || code == 201 {
		t.Fatalf("an unknown agent was started: %v", body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "claude") && !strings.Contains(msg, "codex") {
		t.Errorf("error %q does not say which agents exist", msg)
	}
}

// ------------------------------------------------------------- the queue ---

// The claim path, which server_test.go's lifecycle test does not reach: it
// covers filing, idempotency and a stale token on finish, but never takes a
// task. Claiming is the one operation where two callers racing matters.
func TestClaimingOverHTTPFencesTheSecondCaller(t *testing.T) {
	s := testServer(t)

	// A unique title per run, because claiming opens a tmux session named after
	// it and a leftover from an earlier run makes the claim a 409 for the wrong
	// reason. Cleaned up either way.
	title := fmt.Sprintf("queue probe %d", time.Now().UnixNano())
	sessName := "am-" + strings.ReplaceAll(strings.ToLower(title), " ", "-") + "-worker"
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", "="+sessName).Run() })

	code, filed := post(t, s, "POST", "/api/tasks", `{"title":"`+title+`","dir":"/tmp"}`)
	if code != 200 {
		t.Fatalf("filing returned %d: %v", code, filed)
	}

	code, claimed := post(t, s, "POST", "/api/tasks/claim", `{"owner":"worker-1"}`)
	if code != 200 {
		t.Fatalf("claiming returned %d: %v", code, claimed)
	}
	// Claiming over HTTP also opens a tmux session to work in, so the reply
	// nests the task alongside the attach command.
	task, _ := claimed["task"].(map[string]any)
	if task == nil {
		t.Fatalf("no task in the reply: %v", claimed)
	}
	token, ok := task["token"].(float64)
	if !ok || token <= 0 {
		t.Fatalf("a claim must hand back a fencing token: %v", task)
	}
	if task["owner"] != "worker-1" {
		t.Errorf("the claim should name its owner: %v", task)
	}
	if task["state"] != "claimed" {
		t.Errorf("state = %v, want claimed", task["state"])
	}

	// Nothing left is a 409 rather than an error or an empty 200, because "no
	// work" is a normal answer that a worker loops on.
	if code, _ := post(t, s, "POST", "/api/tasks/claim", `{"owner":"worker-2"}`); code != 409 {
		t.Errorf("a second claim returned %d, want 409", code)
	}
}

// Filing is idempotent on title and directory, so an agent that reports the
// same finding twice does not create two tasks for one problem.
func TestFilingTheSameWorkTwiceIsOneTask(t *testing.T) {
	s := testServer(t)
	body := `{"title":"stale docs","dir":"/tmp/x"}`
	_, first := post(t, s, "POST", "/api/tasks", body)
	_, second := post(t, s, "POST", "/api/tasks", body)
	if first["id"] != second["id"] {
		t.Errorf("two ids for one piece of work: %v and %v", first["id"], second["id"])
	}
}

// -------------------------------------------------------------- events ---

// The events endpoint is the non-streaming half, used on load before the
// stream takes over, and its paging is what stops a phone pulling the whole log.
func TestEventsPageFromASequence(t *testing.T) {
	s := testServer(t)
	seqs := appendN(t, s, 5, event.KindObservation)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", fmt.Sprintf("/api/events?since=%d&limit=2", seqs[1]), ""))
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	if len(got) != 2 {
		t.Fatalf("limit was ignored: got %d", len(got))
	}
	for _, e := range got {
		if seq, _ := e["seq"].(float64); int64(seq) <= seqs[1] {
			t.Errorf("returned event %v at or before the since cursor %d", seq, seqs[1])
		}
	}
}

// An empty log returns an empty array rather than null, because a client that
// iterates the response should not have to special-case it.
func TestEventsReturnsAnArrayWhenThereIsNothing(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", "/api/events?since=999999", ""))
	if strings.TrimSpace(w.Body.String()) == "null" {
		t.Error("empty events returned null, which a client cannot iterate")
	}
}

// ------------------------------------------------------- notification loop --

// amac has always known what it sent and never what happened next, which is why
// the obvious engagement label turned out to measure adapter chattiness rather
// than anything the human did. A tokened board open is the missing raw fact.
func TestATokenedOpenIsRecordedAndAnUntokenedOneIsNot(t *testing.T) {
	s := testServer(t)

	// No token: a stranger on the tailnet, not a response to anything.
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req("GET", "/", ""))
	if n := countKind(t, s, event.KindBoardOpened); n != 0 {
		t.Errorf("an untokened open was recorded (%d)", n)
	}

	// With the token in a cookie, which is how an installed PWA arrives.
	w = httptest.NewRecorder()
	r := req("GET", "/?session=am-claude-1152", "")
	r.AddCookie(&http.Cookie{Name: "amac_token", Value: tok})
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("page returned %d", w.Code)
	}

	events := payloadsOf(t, s, event.KindBoardOpened)
	if len(events) != 1 {
		t.Fatalf("recorded %d opens, want 1", len(events))
	}
	// The deep link a notification's button produces names a session, and that
	// is the strongest evidence available that the open answered that alert.
	if events[0]["session"] != "am-claude-1152" {
		t.Errorf("the session was not carried through: %v", events[0])
	}
}

// Every notification needs a stable id, or nothing that happens afterwards can
// be attributed to it. The sequence number is assigned too late: the payload is
// built before the append.
func TestEveryNotificationCarriesAJoinKey(t *testing.T) {
	s := testServer(t)
	seen := map[string]bool{}
	// Handle with a zero coalesce window, so each call records rather than
	// being suppressed as a repeat. Delivery fails without a Discord token and
	// the event is written either way, which is the point: a suppressed or
	// undelivered notification is still a notification that happened.
	for range 3 {
		if _, err := attention.Handle(t.Context(), s.log, attention.Notice{
			Session: "am-test", Agent: "claude", Reason: "turn-complete",
			Message: "done",
		}, 0); err != nil {
			t.Logf("delivery failed as expected without a token: %v", err)
		}
	}
	got := payloadsOf(t, s, event.KindAttention)
	if len(got) != 3 {
		t.Fatalf("recorded %d notifications, want 3", len(got))
	}
	for _, p := range got {
		id, _ := p["id"].(string)
		if id == "" {
			t.Fatal("a notification was recorded with no id to join on")
		}
		if seen[id] {
			t.Errorf("id %q was reused; it cannot identify one notification", id)
		}
		seen[id] = true
	}
}

func countKind(t *testing.T, s *Server, k event.Kind) int {
	t.Helper()
	var n int
	if err := s.log.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`, string(k)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func payloadsOf(t *testing.T, s *Server, k event.Kind) []map[string]any {
	t.Helper()
	rows, err := s.log.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY seq`, string(k))
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
