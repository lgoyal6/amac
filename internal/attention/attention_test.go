package attention

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

func testLog(t *testing.T) *event.Log {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

// stub replaces the real window-server lookup for the duration of one test.
func stub(t *testing.T, session string, ok bool) {
	t.Helper()
	prev := watching
	watching = func() (string, bool) { return session, ok }
	t.Cleanup(func() { watching = prev })
}

// The whole point of the package: do not interrupt him about the pane he is
// already reading, and do interrupt him about every other one.
func TestSuppressedOnlyForTheSessionInFront(t *testing.T) {
	log := testLog(t)
	stub(t, "am-mint", true)

	if got := decide(context.Background(), log, Notice{Session: "am-mint"}); got.Sent {
		t.Fatal("must not interrupt about the session being watched")
	}
	// The predecessor's bug: a dozen clients are attached at any moment, so
	// "attached" suppressed everything. Only the frontmost one may.
	if got := decide(context.Background(), log, Notice{Session: "am-strata"}); !got.Sent {
		t.Fatalf("another session must still get through, got %q", got.Why)
	}
}

// Nothing in front at all (browser focused, screen locked, tmux not running)
// must notify rather than stay silent.
func TestNothingWatchedStillNotifies(t *testing.T) {
	log := testLog(t)
	stub(t, "", false)
	if got := decide(context.Background(), log, Notice{Session: "am-mint"}); !got.Sent {
		t.Fatalf("got %q, want sent", got.Why)
	}
}

func TestQuietSwitch(t *testing.T) {
	log := testLog(t)
	stub(t, "", false)
	t.Setenv("AMAC_QUIET", "1")
	if got := decide(context.Background(), log, Notice{Session: "am-mint"}); got.Sent {
		t.Fatal("AMAC_QUIET=1 must silence everything")
	}
}

// A finished Codex turn rings the bell and calls the notify hook within the
// same second. That must be one message, not two.
func TestDedupeCollapsesTheSameEvent(t *testing.T) {
	ctx := context.Background()
	log := testLog(t)
	stub(t, "", false)

	n := Notice{Session: "am-mint", Agent: "codex", Reason: WantsYou}
	if err := record(ctx, log, n, Outcome{Sent: true}); err != nil {
		t.Fatal(err)
	}
	if got := decide(ctx, log, Notice{Session: "am-mint", Reason: TurnComplete}); got.Sent {
		t.Fatal("a second signal for the same session must be collapsed")
	}
	// Dedupe is per session; a different one is a different problem.
	if got := decide(ctx, log, Notice{Session: "am-strata"}); !got.Sent {
		t.Fatalf("dedupe must not leak across sessions, got %q", got.Why)
	}
}

// Only a DELIVERED notification suppresses the next one. If the last one was
// held back because he was looking at the pane, and he has since looked away,
// the next signal has to reach him.
func TestSuppressedEventDoesNotDedupe(t *testing.T) {
	ctx := context.Background()
	log := testLog(t)
	stub(t, "", false)

	n := Notice{Session: "am-mint", Agent: "codex", Reason: WantsYou}
	if err := record(ctx, log, n, Outcome{Sent: false, Why: "you are looking at am-mint"}); err != nil {
		t.Fatal(err)
	}
	if got := decide(ctx, log, n); !got.Sent {
		t.Fatalf("a held notification must not suppress the next, got %q", got.Why)
	}
}

func TestDedupeExpires(t *testing.T) {
	ctx := context.Background()
	log := testLog(t)
	stub(t, "", false)

	n := Notice{Session: "am-mint", Agent: "codex", Reason: WantsYou}
	ev, err := event.New(event.KindAttention, n.Agent, n.Session, map[string]any{
		"reason": n.Reason, "outcome": Outcome{Sent: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev.At = time.Now().UTC().Add(-DedupeWindow - time.Minute)
	if _, err := log.Append(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if got := decide(ctx, log, n); !got.Sent {
		t.Fatalf("past the window it is a new event, got %q", got.Why)
	}
}

func TestRender(t *testing.T) {
	got := render(Notice{Session: "am-mint", Agent: "codex", Reason: WantsYou})
	if !contains(got, "**mint** needs you") {
		t.Fatalf("the am- prefix is noise on a phone: %q", got)
	}
	got = render(Notice{Session: "am-mint", Agent: "codex", Reason: TurnComplete, Message: "all 47 tests pass"})
	if !contains(got, "**mint** needs you") || contains(got, "all 47 tests pass") {
		t.Fatalf("all session alerts should be the same small call to action: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
