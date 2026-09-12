package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hackboardFixture(t *testing.T) (*httptest.Server, *Server, *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("authorization"))
		if r.Header.Get("authorization") != "Bearer sekrit" {
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			seen = append(seen, "body="+string(body))
			w.Header().Set("location", "/board")
			w.WriteHeader(303)
			return
		}
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>hackqueue</title><form action="/board/pass">`))
	}))
	t.Cleanup(upstream.Close)

	envFile := filepath.Join(home, "bot.env")
	body := "# a comment\nDISCORD_BOT_TOKEN=irrelevant\nHACKQUEUE_URL=" + upstream.URL + "\nQUEUE_SECRET=sekrit\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HACKQUEUE_ENV_FILE", envFile)
	return upstream, testServer(t), &seen
}

func TestTheBoardIsServedWholeWithTheBearerAttached(t *testing.T) {
	_, srv, seen := hackboardFixture(t)
	w := httptest.NewRecorder()
	srv.hackboard(w, httptest.NewRequest("GET", "/board", nil))

	if w.Code != 200 {
		t.Fatalf("want the board, got %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "hackqueue") {
		t.Fatalf("the page did not come through: %s", w.Body)
	}
	if len(*seen) == 0 || !strings.Contains((*seen)[0], "auth=Bearer sekrit") {
		t.Fatalf("the bearer was not attached: %v", *seen)
	}
	// A board rendered from a queue that changes under it must not be cached,
	// or a decision made elsewhere shows as still waiting.
	if w.Header().Get("cache-control") != "no-store" {
		t.Fatalf("want no-store, got %q", w.Header().Get("cache-control"))
	}
}

// The 303 is handed to the browser rather than followed here. Following it
// would fetch the whole page again to throw it away, and the browser would lose
// sight of what its own POST did.
func TestADecisionRedirectsRatherThanBeingFollowed(t *testing.T) {
	_, srv, seen := hackboardFixture(t)
	r := httptest.NewRequest("POST", "/board/pass", strings.NewReader("id=external-a"))
	r.Header.Set("content-type", "application/x-www-form-urlencoded")
	r.SetPathValue("action", "pass")
	w := httptest.NewRecorder()
	srv.hackboard(w, r)

	if w.Code != 303 {
		t.Fatalf("want the redirect passed through, got %d", w.Code)
	}
	// /board is already the right path on this origin, so it needs no rewriting.
	if w.Header().Get("location") != "/board" {
		t.Fatalf("want location /board, got %q", w.Header().Get("location"))
	}
	joined := strings.Join(*seen, " | ")
	if !strings.Contains(joined, "body=id=external-a") {
		t.Fatalf("the form body did not reach the board: %s", joined)
	}
}

func TestEveryBoardActionIsForwarded(t *testing.T) {
	_, srv, _ := hackboardFixture(t)
	for _, action := range []string{"pass", "retry", "direction", "build", "chat", "submission", "result"} {
		r := httptest.NewRequest("POST", "/board/"+action, strings.NewReader("id=x"))
		r.SetPathValue("action", action)
		w := httptest.NewRecorder()
		srv.hackboard(w, r)
		if w.Code != 303 {
			t.Fatalf("%s should forward, got %d", action, w.Code)
		}
	}
}

// This is a proxy to one page, not an open relay. The target is rebuilt from a
// fixed prefix and a known action, so nothing a URL says can reach `/queue`,
// which is the API the dispatcher drives and has no business being reachable
// from a browser tab.
func TestItWillNotProxyAnythingButTheBoard(t *testing.T) {
	_, srv, seen := hackboardFixture(t)
	for _, path := range []string{
		"/board/queue", "/board/../queue", "/board/status", "/board/",
		"/board/pass/extra", "/queue", "/board/pass?x=1#y",
	} {
		r := httptest.NewRequest("GET", "http://amac"+path, nil)
		r.URL.Path = path // keep the raw path, including the traversal attempt
		w := httptest.NewRecorder()
		srv.hackboard(w, r)
		if w.Code != 404 {
			t.Fatalf("%s should be refused, got %d", path, w.Code)
		}
	}
	for _, line := range *seen {
		if strings.Contains(line, "/queue") {
			t.Fatalf("a request reached the queue API: %v", *seen)
		}
	}
}

func TestAMissingEnvFileSaysSoRatherThanCrashing(t *testing.T) {
	_, srv, _ := hackboardFixture(t)
	t.Setenv("HACKQUEUE_ENV_FILE", filepath.Join(t.TempDir(), "gone.env"))
	w := httptest.NewRecorder()
	srv.hackboard(w, httptest.NewRequest("GET", "/board", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "hackqueue env file") {
		t.Fatalf("want the reason named, got %s", w.Body)
	}
}

func TestAnUnreachableBoardIsABadGateway(t *testing.T) {
	upstream, srv, _ := hackboardFixture(t)
	upstream.Close()
	w := httptest.NewRecorder()
	srv.hackboard(w, httptest.NewRequest("GET", "/board", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502 when hackqueue is down, got %d", w.Code)
	}
}
