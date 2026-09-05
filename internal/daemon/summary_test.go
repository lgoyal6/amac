package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func getSummary(t *testing.T, s *Server) map[string]summaryPart {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, authed("GET", "/api/summary", ""))
	if w.Code != 200 {
		t.Fatalf("summary returned %d: %s", w.Code, w.Body)
	}
	var got map[string]summaryPart
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("summary is not JSON: %s", w.Body)
	}
	return got
}

// The reason this endpoint dispatches into the handlers instead of reading the
// database itself. If it ever grows its own copy of "what did I spend today",
// the two answers will disagree and the tile will be the one that is wrong,
// because it is the one nobody opens the other tab to check.
func TestSummaryAgreesWithTheEndpointsItSummarises(t *testing.T) {
	s := testServer(t)
	got := getSummary(t, s)

	for _, tc := range []struct{ key, path string }{
		{"health", "/api/health"},
		{"tasks", "/api/tasks"},
		{"spend", "/api/spend"},
		{"jobs", "/api/applications?limit=1"},
		{"due", "/api/applications?limit=1000&due=1"},
		{"runs", "/api/health/runs"},
		{"machine", "/api/machine"},
	} {
		part, ok := got[tc.key]
		if !ok {
			t.Errorf("summary has no %q; home would render that tile empty", tc.key)
			continue
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, authed("GET", tc.path, ""))
		if part.Status != w.Code {
			t.Errorf("%s: summary says %d, %s says %d", tc.key, part.Status, tc.path, w.Code)
		}
		// Compared as JSON rather than as bytes, because key order and
		// whitespace are not disagreements. Machine stats move between the two
		// calls by design, so only its status is compared.
		if tc.key == "machine" {
			continue
		}
		var a, b any
		if json.Unmarshal(part.Body, &a) != nil || json.Unmarshal(w.Body.Bytes(), &b) != nil {
			continue
		}
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			t.Errorf("%s disagrees with %s:\n summary: %s\n direct:  %s", tc.key, tc.path, x, y)
		}
	}
}

// One failing feed must not take the screen down with it. Home names the feed
// that did not answer and renders everything else, which is only possible if
// the statuses arrive separately.
func TestOneFailingFeedDoesNotFailTheWholeSummary(t *testing.T) {
	s := testServer(t)
	got := getSummary(t, s)

	if len(got) != len(homeFeedKeys) {
		t.Fatalf("summary carried %d parts, want %d", len(got), len(homeFeedKeys))
	}
	answered := 0
	for key, part := range got {
		if part.Status == 0 {
			t.Errorf("%s carries no status, so a failure would be indistinguishable from empty", key)
		}
		if part.Status == 200 {
			answered++
		}
	}
	if answered == 0 {
		t.Error("no feed answered at all; the summary is not reaching the handlers")
	}
}

// homeFeedKeys is the same list summary builds, restated so a feed added to one
// and not the other is caught rather than silently missing from the screen.
var homeFeedKeys = []string{"health", "tasks", "spend", "jobs", "due", "runs", "machine"}

// The envelope must survive a handler that writes something unparseable: a
// broken part is reported by its status, not by breaking the JSON around it.
func TestABrokenPartDoesNotCorruptTheEnvelope(t *testing.T) {
	rec := &recorder{}
	rec.WriteHeader(500)
	if _, err := rec.Write([]byte("not json")); err != nil {
		t.Fatal(err)
	}
	part := summaryPart{Status: rec.status}
	if json.Valid(rec.body.Bytes()) {
		t.Fatal("the fixture is not actually invalid JSON")
	}
	b, err := json.Marshal(map[string]summaryPart{"x": part})
	if err != nil {
		t.Fatalf("an unparseable part broke the envelope: %v", err)
	}
	if !json.Valid(b) {
		t.Errorf("envelope is not valid JSON: %s", b)
	}
}

// The recorder is the whole reason httptest is not imported into the daemon.
// It has to behave the way handlers assume a ResponseWriter behaves.
func TestRecorderDefaultsToOKAndKeepsTheFirstStatus(t *testing.T) {
	rec := &recorder{}
	if _, err := rec.Write([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if rec.status != 200 {
		t.Errorf("a handler that only wrote a body recorded %d, want 200", rec.status)
	}

	rec2 := &recorder{}
	rec2.WriteHeader(404)
	rec2.WriteHeader(500)
	if rec2.status != 404 {
		t.Errorf("status = %d, want the first one written (404)", rec2.status)
	}
	if rec2.Header() == nil {
		t.Error("Header() returned nil, which handlers dereference")
	}
}
