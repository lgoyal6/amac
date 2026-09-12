package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A queue answer shaped like the real one, which matters: alongside one key per
// status it carries `seen`, a list of ids rather than a list of entries.
const fakeQueue = `{
  "seen": ["external-a", "external-b"],
  "pending": [{"id":"external-a","title":"AI Builders","url":"https://a.devpost.com","status":"pending","closes":"2026-09-15T23:59:00-07:00"}],
  "ready": [{"id":"external-b","title":"VoltHacks","url":"https://b.devpost.com","status":"ready","closes":"2026-09-13T23:59:00-07:00"}],
  "approved": [],
  "somethingNewTheWorkerAdded": [{"nested":{"not":"an entry"}}]
}`

const fakeRanked = `{
  "rankedAt": "2026-09-11T09:15:00.000Z",
  "ranked": [
    {"id":"external-a","title":"AI Builders","url":"https://a.devpost.com","organizer":"OSC","prize":33900,"going":3130,"closes":"2026-09-15T23:59:00-07:00","daysLeft":4,"fit":84},
    {"id":"external-c","title":"ETHOnline 2026","url":"https://ethglobal.com/events/ethonline2026","organizer":"ETHGlobal","prize":80000,"going":0,"closes":"2026-09-16T23:59:00-07:00","daysLeft":6,"fit":70}
  ]
}`

func hackqueueFixture(t *testing.T, queueBody string, queueCode int) (*httptest.Server, *Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer sekrit" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/board/pass" || r.URL.Path == "/board/retry" {
			w.Header().Set("location", "/board")
			w.WriteHeader(303)
			return
		}
		w.WriteHeader(queueCode)
		_, _ = w.Write([]byte(queueBody))
	}))
	t.Cleanup(upstream.Close)

	envFile := filepath.Join(home, "bot.env")
	body := "# a comment\nDISCORD_BOT_TOKEN=irrelevant\nHACKQUEUE_URL=" + upstream.URL + "\nQUEUE_SECRET=sekrit\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ranked := filepath.Join(home, "ranked.json")
	if err := os.WriteFile(ranked, []byte(fakeRanked), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HACKQUEUE_ENV_FILE", envFile)
	t.Setenv("HACKQUEUE_RANKED", ranked)
	return upstream, testServer(t)
}

func getHackqueue(t *testing.T, srv *Server) hackqueueResponse {
	t.Helper()
	w := httptest.NewRecorder()
	srv.hackqueueGet(w, httptest.NewRequest("GET", "/api/hackqueue", nil))
	if w.Code != 200 {
		t.Fatalf("hackqueue returned %d: %s", w.Code, w.Body)
	}
	var resp hackqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// The bug this test exists for: `seen` is a list of ids, so decoding the whole
// body into one map of entries fails on that key and takes every status with
// it. A tab that shows an empty queue because of one unparsed key is worse than
// one that shows an error.
func TestSeenIdsDoNotSwallowTheWholeQueue(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	resp := getHackqueue(t, srv)

	if len(resp.Queue) != 2 {
		t.Fatalf("want 2 entries past the seen list, got %d: %+v", len(resp.Queue), resp.Queue)
	}
	// And a status this build has never heard of costs its own column, not the
	// whole response.
	for _, e := range resp.Queue {
		if e.Status == "somethingNewTheWorkerAdded" {
			t.Fatal("an unparsable status should be skipped, not carried")
		}
	}
	if resp.Waiting != 1 {
		t.Fatalf("waiting counts pending only, got %d", resp.Waiting)
	}
}

// Soonest first: VoltHacks closes on the 13th, AI Builders on the 15th.
func TestTheQueueIsOrderedByHowSoonItCloses(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	resp := getHackqueue(t, srv)
	if resp.Queue[0].Title != "VoltHacks" {
		t.Fatalf("want the soonest first, got %s", resp.Queue[0].Title)
	}
}

// The board says which of its rows hackqueue has already offered, so it cannot
// invite a decision that was made days ago.
func TestTheBoardMarksWhatWasAlreadyOffered(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	resp := getHackqueue(t, srv)

	byID := map[string]hackqueueBoardRow{}
	for _, row := range resp.Board {
		byID[row.ID] = row
	}
	if !byID["external-a"].Offered || byID["external-a"].Status != "pending" {
		t.Fatalf("offered row not marked: %+v", byID["external-a"])
	}
	if byID["external-c"].Offered {
		t.Fatalf("a row nobody has been offered is not offered: %+v", byID["external-c"])
	}
}

// Half a tab beats none. The board is read off local disk and does not need the
// Worker at all, so an unreachable queue must not blank it.
func TestAnUnreachableQueueStillRendersTheBoard(t *testing.T) {
	_, srv := hackqueueFixture(t, `nonsense`, 500)
	resp := getHackqueue(t, srv)

	if len(resp.Board) != 2 {
		t.Fatalf("the board should survive the queue being down, got %d", len(resp.Board))
	}
	if resp.Warning == "" {
		t.Fatal("a partial answer has to say it is partial")
	}
}

func TestAMissingEnvFileIsReportedRatherThanCrashing(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	t.Setenv("HACKQUEUE_ENV_FILE", filepath.Join(t.TempDir(), "gone.env"))
	resp := getHackqueue(t, srv)
	if !strings.Contains(resp.Warning, "hackqueue env file") {
		t.Fatalf("want a warning naming the env file, got %q", resp.Warning)
	}
	if len(resp.Board) != 2 {
		t.Fatal("the board does not need the env file")
	}
}

func act(t *testing.T, srv *Server, action, id string) int {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/hackqueue/"+action, strings.NewReader(`{"id":"`+id+`"}`))
	r.SetPathValue("action", action)
	w := httptest.NewRecorder()
	srv.hackqueueAct(w, r)
	return w.Code
}

// Discord keeps its buttons. This is deliberately not a second copy of every
// decision, so anything but pass and retry is refused here rather than
// forwarded to a board route that would happily accept it.
func TestOnlyPassAndRetryAreDrivenFromHere(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	if code := act(t, srv, "pass", "external-b"); code != 200 {
		t.Fatalf("pass should forward, got %d", code)
	}
	if code := act(t, srv, "retry", "external-b"); code != 200 {
		t.Fatalf("retry should forward, got %d", code)
	}
	for _, forbidden := range []string{"build", "direction", "result", "submission", "chat"} {
		if code := act(t, srv, forbidden, "external-b"); code != 400 {
			t.Fatalf("%s should be refused here, got %d", forbidden, code)
		}
	}
}

func TestAnActionWithoutAnIDIsRefused(t *testing.T) {
	_, srv := hackqueueFixture(t, fakeQueue, 200)
	r := httptest.NewRequest("POST", "/api/hackqueue/pass", strings.NewReader(`{}`))
	r.SetPathValue("action", "pass")
	w := httptest.NewRecorder()
	srv.hackqueueAct(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 for a missing id, got %d", w.Code)
	}
}
