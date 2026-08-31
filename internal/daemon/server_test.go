package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const tok = "tok"

func req(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func authed(method, path, body string) *http.Request {
	r := req(method, path, body)
	r.Header.Set("X-Amac-Token", tok)
	return r
}

func do(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(w, r)
	return w
}

// The token is the inner gate. The tailnet is the outer one, and anything else
// on the tailnet, a shared node or a compromised device, must still not be able
// to drive agents on this machine.
func TestEveryAPIRouteNeedsTheToken(t *testing.T) {
	routes := []struct{ method, path string }{
		{"GET", "/api/sessions"}, {"POST", "/api/sessions"},
		{"PATCH", "/api/sessions/x"},
		{"GET", "/api/events"}, {"GET", "/api/agents"},
		{"GET", "/api/health"}, {"GET", "/api/spend"},
		{"GET", "/api/health/schedule"},
		{"GET", "/api/tasks"}, {"POST", "/api/tasks"},
		{"POST", "/api/tasks/claim"}, {"GET", "/api/crew"},
		{"POST", "/api/beat/x"}, {"POST", "/api/health/x/fix"},
		{"GET", "/api/sessions/x/pane"}, {"POST", "/api/sessions/x/keys"},
		{"GET", "/api/sessions/x/files"}, {"GET", "/api/sessions/x/diff"},
	}
	for _, rt := range routes {
		if got := do(t, req(rt.method, rt.path, "{}")).Code; got != 401 {
			t.Errorf("%s %s without a token returned %d, want 401", rt.method, rt.path, got)
		}
		if got := do(t, req(rt.method, rt.path+"?token=wrong", "{}")).Code; got != 401 {
			t.Errorf("%s %s with a wrong token returned %d, want 401", rt.method, rt.path, got)
		}
	}
}

func TestResumeCommandsMatchInstalledCLIs(t *testing.T) {
	for _, tc := range []struct{ agent, id, want string }{
		{"claude", "abc", "claude --resume abc"},
		{"codex", "abc", "codex resume abc"},
		{"other", "abc", ""},
	} {
		if got := resumeCommand(tc.agent, tc.id); got != tc.want {
			t.Errorf("resumeCommand(%q, %q) = %q, want %q", tc.agent, tc.id, got, tc.want)
		}
	}
}

func TestDefaultSessionNameSaysAgentAndTime(t *testing.T) {
	at := time.Date(2026, 8, 30, 18, 7, 32, 0, time.Local)
	if got := defaultSessionName("codex", at); got != "codex 18:07:32" {
		t.Fatalf("got %q", got)
	}
}

// The page itself is served without one, because iOS fetches it, the manifest
// and the icon while adding to a home screen, from a context holding none of
// the page's storage. None of those say anything about this machine.
func TestThePageAndItsIconsAreOpen(t *testing.T) {
	for _, path := range []string{"/", "/manifest.webmanifest", "/icon-180.png", "/icon-192.png"} {
		if got := do(t, req("GET", path, "")).Code; got != 200 {
			t.Errorf("%s returned %d, want 200", path, got)
		}
	}
}

// EventSource cannot set headers, so the stream has no way to authenticate
// other than the query string. That is the reason it is accepted, and it must
// not become a second way in for everything else that could use a header.
func TestTheTokenIsAcceptedInAQueryForTheStream(t *testing.T) {
	if got := do(t, req("GET", "/api/sessions?token="+tok, "")).Code; got != 200 {
		t.Errorf("query token rejected: %d", got)
	}
}

// Not "*". These routes start agents and approve their tool calls, so an
// arbitrary website must never be handed a CORS grant even if it somehow
// learned the token.
func TestCORSIsOnlyForExtensions(t *testing.T) {
	for _, origin := range []string{
		"chrome-extension://abc", "moz-extension://abc", "safari-web-extension://abc",
	} {
		r := authed("GET", "/api/agents", "")
		r.Header.Set("Origin", origin)
		if got := do(t, r).Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("%s was not allowed, got %q", origin, got)
		}
	}
	for _, origin := range []string{
		"https://evil.example", "http://localhost:3000", "null",
	} {
		r := authed("GET", "/api/agents", "")
		r.Header.Set("Origin", origin)
		if got := do(t, r).Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s was granted CORS: %q", origin, got)
		}
	}
}

// ------------------------------------------------------------------ tasks ---

func TestTaskLifecycleOverHTTP(t *testing.T) {
	srv := testServer(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := req(method, path, body)
		r.Header.Set("X-Amac-Token", tok)
		srv.Handler().ServeHTTP(w, r)
		return w
	}

	w := call("POST", "/api/tasks", `{"title":"fix the thing","dir":"/tmp"}`)
	if w.Code != 200 {
		t.Fatalf("filing returned %d: %s", w.Code, w.Body)
	}
	var filed struct{ ID, State string }
	json.Unmarshal(w.Body.Bytes(), &filed)
	if filed.State != "ready" {
		t.Fatalf("filed as %q", filed.State)
	}

	// Filing the same title again is one task, so two health sweeps noticing
	// the same broken automation do not produce two attempts at it.
	call("POST", "/api/tasks", `{"title":"fix the thing","dir":"/tmp"}`)
	w = call("GET", "/api/tasks", "")
	var list []struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("filed twice, got %d tasks", len(list))
	}

	// A title is the one thing that cannot be defaulted.
	if got := call("POST", "/api/tasks", `{"dir":"/tmp"}`).Code; got != 400 {
		t.Errorf("a task with no title returned %d, want 400", got)
	}
	// A stale token on finish is refused with a conflict, not a server error:
	// the caller has been fenced and needs to stop, not retry.
	if got := call("POST", "/api/tasks/"+filed.ID+"/finish", `{"token":999,"state":"done"}`).Code; got != 409 {
		t.Errorf("finishing with a stale token returned %d, want 409", got)
	}
}

// A bare POST is a valid heartbeat, because the common case is a job adding one
// line to a script and that line should not carry a payload to get wrong.
func TestHeartbeatAcceptsABarePost(t *testing.T) {
	if got := do(t, authed("POST", "/api/beat/nightly", "")).Code; got != 200 {
		t.Errorf("a bare beat returned %d", got)
	}
	if got := do(t, authed("POST", "/api/beat/nightly", `{"state":"failing","detail":"disk full"}`)).Code; got != 200 {
		t.Errorf("a beat with a state returned %d", got)
	}
	// A state amac cannot interpret is refused rather than stored, because
	// storing it means the probe has to guess about it later.
	if got := do(t, authed("POST", "/api/beat/nightly", `{"state":"probably-fine"}`)).Code; got != 400 {
		t.Errorf("an unknown state returned %d, want 400", got)
	}
	// Garbage in the body does not lose the beat: the name in the path is the
	// only field that matters.
	if got := do(t, authed("POST", "/api/beat/nightly", `not json`)).Code; got != 200 {
		t.Errorf("an unparseable body lost the beat: %d", got)
	}
}

// ----------------------------------------------------------------- health ---

func TestHealthWithNoSweepOnRecord(t *testing.T) {
	w := do(t, authed("GET", "/api/health", ""))
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	// No sweep yet is an empty roster, not a failure. It means the launchd job
	// has not run, and the board should say so rather than show a red screen.
	var body struct {
		Reports []any `json:"reports"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	if len(body.Reports) != 0 {
		t.Errorf("expected no reports, got %d", len(body.Reports))
	}
}

func TestHealthScheduleExplainsIntent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/health.json"
	if err := os.WriteFile(path, []byte(`{"automations":[{
		"name":"pressure","what":"watch swap and disk","every":"30m",
		"grace":"4h","probe":"marker_fields"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMAC_HEALTH_CONFIG", path)
	w := do(t, authed("GET", "/api/health/schedule", ""))
	if w.Code != 200 {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	for _, want := range []string{"pressure", "30m", "completion line", "monitors"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("schedule did not explain %q: %s", want, w.Body)
		}
	}
}

func TestDispatchRefusals(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/health/no-such-automation/fix", 404},
		{"/api/health/no-such-automation/shell", 404},
	} {
		if got := do(t, authed("POST", tc.path, "")).Code; got != tc.want {
			t.Errorf("%s returned %d, want %d", tc.path, got, tc.want)
		}
	}
}

// ------------------------------------------------------------------ spend ---

func TestSpendWithoutASnapshotSaysSo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w := do(t, authed("GET", "/api/spend", ""))
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	// It names the command to run rather than returning zeroes, which would
	// render as "you have spent nothing".
	if !strings.Contains(w.Body.String(), "spend.mjs") {
		t.Errorf("a missing snapshot should say how to make one: %s", w.Body)
	}
}

// ----------------------------------------------------------------- events ---

func TestEventsAreReadable(t *testing.T) {
	w := do(t, authed("GET", "/api/events?limit=5", ""))
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	// Never null: a client that does `for (const e of body)` should not have
	// to special-case an empty log.
	if strings.TrimSpace(w.Body.String()) == "null" {
		t.Error("an empty log must serialise as [], not null")
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	if got := do(t, req("GET", "/nope", "")).Code; got != 404 {
		t.Errorf("got %d", got)
	}
}

// A session amac has never heard from has no timestamp, and the board reads
// whatever it is given as an age. `omitempty` is no help: time.Time is a
// struct, so an unset one is not empty, it is the year 1, and the card said
// "739855d ago" with total confidence. The field is a pointer now so absence
// can be transmitted as absence.
func TestAnUnknownSessionShipsNoTimestamp(t *testing.T) {
	var known = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		v    sessionView
		want bool // is "since" expected in the JSON
	}{
		{"never heard from", sessionView{ID: "am-x", State: "unknown"}, false},
		{"a real observation", sessionView{ID: "am-y", State: "blocked", Since: when(known)}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.v)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), `"since"`); got != c.want {
				t.Fatalf("since present = %v, want %v in %s", got, c.want, b)
			}
		})
	}
}

// The home-screen icon is the credential. iOS gives a standalone web app its
// own storage container, so a manifest whose start_url is "/" opens a board
// that has never seen the token, and Safari evicts localStorage on its own
// schedule anyway. The token goes in start_url - but only back to a request
// that already proved it has one.
func TestTheManifestCarriesTheTokenOnlyToWhoeverAlreadyHasIt(t *testing.T) {
	for _, c := range []struct {
		name, query, want string
	}{
		{"no token", "", `"start_url": "/"`},
		{"wrong token", "?token=nope", `"start_url": "/"`},
		{"the token", "?token=" + tok, `"start_url": "/app/` + tok + `"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, req("GET", "/manifest.webmanifest"+c.query, ""))
			if w.Code != 200 {
				t.Fatalf("code %d, want 200", w.Code)
			}
			if !strings.Contains(w.Body.String(), c.want) {
				t.Fatalf("want %q in body:\n%s", c.want, w.Body.String())
			}
			if strings.Contains(w.Header().Get("Cache-Control"), "max-age") {
				t.Fatalf("a manifest that may carry a credential must not be cached: %q",
					w.Header().Get("Cache-Control"))
			}
		})
	}
}

// The <link rel="manifest"> is in <head>, so Safari fetches it while parsing
// and has start_url cached before any script in the body runs. A JS rewrite is
// too late by construction - the manifest iOS installs is whichever one the
// head fetch returned - so the token has to be in the bytes the server sends.
func TestThePagePointsAtAManifestThatKnowsTheToken(t *testing.T) {
	plain := `<link rel="manifest" href="/manifest.webmanifest">`
	tokened := `<link rel="manifest" href="/manifest.webmanifest?token=` + tok + `">`
	for _, c := range []struct {
		name, path, want, absent string
	}{
		{"a bare visit", "/", plain, tokened},
		{"the link amac url prints", "/?token=" + tok, tokened, plain},
		{"where the icon lands", "/app/" + tok, tokened, plain},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, req("GET", c.path, ""))
			if w.Code != 200 {
				t.Fatalf("code %d, want 200", w.Code)
			}
			if !strings.Contains(w.Body.String(), c.want) {
				t.Fatalf("want %q in the page", c.want)
			}
			if strings.Contains(w.Body.String(), c.absent) {
				t.Fatalf("did not want %q in the page", c.absent)
			}
		})
	}
}

// The icon's launch URL is a credential, so a wrong one is not a page.
func TestTheAppPathRefusesAWrongToken(t *testing.T) {
	if w := do(t, req("GET", "/app/nope", "")); w.Code != 401 {
		t.Fatalf("code %d, want 401", w.Code)
	}
}
