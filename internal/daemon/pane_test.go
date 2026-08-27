package daemon

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/supervisor"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return New(supervisor.New(log), log, nil, "tok")
}

// A pane is mostly blank padding below the prompt. Showing it verbatim puts
// the only interesting line above the fold on a phone.
func TestTailDropsPaddingAndKeepsTheEnd(t *testing.T) {
	if got := tail("a\nb\nc\n\n\n\n", 40); got != "a\nb\nc" {
		t.Errorf("got %q", got)
	}
	if got := tail("1\n2\n3\n4\n5", 2); got != "4\n5" {
		t.Errorf("got %q", got)
	}
	if got := tail("\n\n\n", 40); got != "" {
		t.Errorf("an empty screen must stay empty, got %q", got)
	}
}

func TestKeysToSend(t *testing.T) {
	for _, tc := range []struct {
		key   string
		enter bool
		want  string
	}{
		{"", true, "Enter"},
		{"Escape", false, "Escape"},
		{"Escape", true, "Escape,Enter"},
		// Asking for Enter twice would submit twice, and the second one lands
		// on whatever the agent drew in the meantime.
		{"Enter", true, "Enter"},
	} {
		if got := strings.Join(keysToSend(tc.key, tc.enter), ","); got != tc.want {
			t.Errorf("keysToSend(%q,%v) = %q, want %q", tc.key, tc.enter, got, tc.want)
		}
	}
}

// The target must exist on the live tmux server. An unvalidated name would let
// a stale card type into whatever session holds that name now, and typing into
// the wrong agent is the one failure this feature must not have.
func TestUnknownSessionIsRefused(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{
		"/api/sessions/definitely-not-a-session/pane",
		"/api/sessions/definitely-not-a-session/keys",
	} {
		r := httptest.NewRequest("GET", path, strings.NewReader(`{"text":"x"}`))
		r.SetPathValue("id", "definitely-not-a-session")
		w := httptest.NewRecorder()
		if strings.HasSuffix(path, "keys") {
			s.sendKeys(w, r)
		} else {
			s.pane(w, r)
		}
		if w.Code != 404 {
			t.Errorf("%s: got %d, want 404", path, w.Code)
		}
	}
}

// The artifact path is rebuilt from a slug and a role rather than accepted, so
// there is no input that escapes the run directory. This endpoint reads files
// off disk and is reachable from a phone.
func TestArtifactRejectsAnythingItDidNotProduce(t *testing.T) {
	s := testServer(t)
	for _, q := range []string{
		"slug=../../../etc&role=passwd",
		"slug=x&role=../../etc/passwd",
		"slug=Not-A-Slug&role=planner",
		"slug=ok&role=planner/../../..",
		"slug=&role=planner",
		"slug=ok&role=",
	} {
		w := httptest.NewRecorder()
		s.crewArtifact(w, httptest.NewRequest("GET", "/api/crew/artifact?"+q, nil))
		if w.Code != 400 {
			t.Errorf("%s: got %d, want 400", q, w.Code)
		}
	}
}
