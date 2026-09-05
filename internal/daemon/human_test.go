package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// The distinction the whole label rests on. The board polls constantly, and a
// poll is the page doing its job, not a person deciding something. Counting
// reads is how the previous attempt ended up measuring how chatty an adapter
// is instead of anything a human did.
func TestPollingIsNotAHumanAction(t *testing.T) {
	s := testServer(t)
	// Deliberately not /api/health or anything that reaches it. The roster is
	// cached process-wide behind a sync.Once, so the first test to touch it
	// fixes it for every test after, and this one would pin the real machine's
	// roster onto a later test that declares its own. The endpoints below make
	// the same point without the side effect.
	for _, path := range []string{
		"/api/sessions", "/api/tasks", "/api/spend", "/api/machine",
		"/api/panes", "/api/agents",
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, authed("GET", path, ""))
	}
	if n := countKind(t, s, event.KindHumanActed); n != 0 {
		t.Errorf("%d reads were recorded as human actions", n)
	}
}

// Every write through the authenticated API took a deliberate act.
func TestAWriteIsRecordedAsAHumanAction(t *testing.T) {
	s := testServer(t)
	code, _ := post(t, s, "POST", "/api/tasks", `{"title":"look at the flaky test","dir":"/tmp"}`)
	if code != 200 {
		t.Fatalf("filing returned %d", code)
	}
	got := payloadsOf(t, s, event.KindHumanActed)
	if len(got) != 1 {
		t.Fatalf("recorded %d human actions, want 1", len(got))
	}
	if got[0]["action"] != "POST /api/tasks" {
		t.Errorf("action = %v, want the route that was called", got[0]["action"])
	}
	if got[0]["method"] != "POST" {
		t.Errorf("method = %v", got[0]["method"])
	}
}

// A search is the one read that is a decision: somebody typed a question, and
// the board never issues it on its own.
func TestASearchIsAHumanActionButOtherReadsAreNot(t *testing.T) {
	s := testServer(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", "/api/search?q=branch", ""))
	if n := countKind(t, s, event.KindHumanActed); n != 1 {
		t.Errorf("a search recorded %d actions, want 1", n)
	}
}

// A thumb landing on a session that is gone is not a decision about it, and
// counting it would put a positive against a notification nobody acted on.
func TestARefusedRequestIsNotAnAction(t *testing.T) {
	s := testServer(t)
	code, _ := post(t, s, "POST", "/api/sessions/ghost/prompt", `{"text":"hello"}`)
	if code < 400 {
		t.Fatalf("the fixture is wrong: a prompt to a ghost session returned %d", code)
	}
	if n := countKind(t, s, event.KindHumanActed); n != 0 {
		t.Errorf("a refused request was recorded as a human action")
	}
}

// The strongest label available: a signed link out of one specific
// notification, followed by a person. It names the alert it answers, which no
// other signal here can do.
//
// Recorded even when the open that follows fails, which is the deliberate
// exception to the rule above. A session that ended while somebody walked to
// their phone says nothing about whether they answered the alert, and the act
// being recorded is following the link.
func TestFollowingANotificationLinkNamesTheNotification(t *testing.T) {
	s := testServer(t)
	s.recordAct(req("GET", "/handoff?session=am-claude-9&n=abc123", ""), "handoff", "abc123")

	got := payloadsOf(t, s, event.KindHumanActed)
	if len(got) != 1 {
		t.Fatalf("recorded %d actions, want 1", len(got))
	}
	if got[0]["notice"] != "abc123" {
		t.Errorf("notice = %v, so the act cannot be attributed to an alert", got[0]["notice"])
	}
	if got[0]["session"] != "am-claude-9" {
		t.Errorf("session = %v", got[0]["session"])
	}
	if got[0]["action"] != "handoff" {
		t.Errorf("action = %v", got[0]["action"])
	}
}

// A phone and a terminal are different kinds of attention, and the difference
// is worth keeping. It is not a security claim: a header is not evidence.
func TestHowTheRequestArrivedIsRecorded(t *testing.T) {
	s := testServer(t)
	r := authed("POST", "/api/tasks", `{"title":"from a phone","dir":"/tmp"}`)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	got := payloadsOf(t, s, event.KindHumanActed)
	if len(got) != 1 {
		t.Fatalf("recorded %d actions, want 1", len(got))
	}
	if got[0]["via"] != "browser" {
		t.Errorf("via = %v, want browser", got[0]["via"])
	}
}

// The wrapper sits in front of the SSE stream too. Without a Flush that passes
// through, the stream buffers until the connection closes, which is the
// endpoint entirely broken by an observability decorator.
func TestTheWrapperDoesNotBreakStreaming(t *testing.T) {
	s := testServer(t)
	appendN(t, s, 2, event.KindObservation)

	body, closeIt := openStream(t, s, "?since=0", "")
	defer closeIt()
	got := readSSE(t, body, 1, 5*time.Second)
	if len(got) == 0 {
		t.Fatal("nothing arrived on the stream; the wrapper swallowed the flush")
	}
}

// Unauthenticated traffic is a stranger on the tailnet, not a person with a
// decision, and it must not enter the record at all.
func TestUnauthenticatedWritesAreNotRecorded(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/tasks", "application/json",
		strings.NewReader(`{"title":"x","dir":"/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated write returned %d, want 401", resp.StatusCode)
	}
	if n := countKind(t, s, event.KindHumanActed); n != 0 {
		t.Error("an unauthenticated write was recorded as a human action")
	}
}
