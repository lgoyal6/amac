package ledger

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// The cost report had no test, which is the wrong thing to leave unverified:
// its one hard rule is that it must never silently understate spend, and that
// rule is expressed as a *float64 that a careless change turns into 0.0.

func logWith(t *testing.T, evs ...event.Event) *event.Log {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	for _, e := range evs {
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func ev(t *testing.T, kind event.Kind, session string, payload map[string]any, at time.Time) event.Event {
	t.Helper()
	e, err := event.New(kind, "test", session, payload)
	if err != nil {
		t.Fatal(err)
	}
	e.At = at
	return e
}

// An agent that reports tokens but not money must come back as unpriced rather
// than as free. Codex does exactly this, so coercing nil to zero would produce
// a report that reads "this cost nothing" about work that cost something.
func TestASessionWithoutMoneyIsUnpricedNotFree(t *testing.T) {
	now := time.Now().UTC()
	log := logWith(t,
		ev(t, event.KindSessionStarted, "codex-1",
			map[string]any{"agent": "codex", "account": "work"}, now.Add(-time.Hour)),
		ev(t, event.KindSessionUpdate, "codex-1",
			map[string]any{"update": "usage_update", "tokens": 5000}, now.Add(-30*time.Minute)),
	)

	rep, err := Query(context.Background(), log.DB(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(rep.Entries))
	}
	e := rep.Entries[0]
	if e.Cost != nil {
		t.Errorf("cost = %v, want nil for an agent that does not report money", *e.Cost)
	}
	if e.CostString() != "n/a" {
		t.Errorf("CostString() = %q, want n/a rather than a dollar figure", e.CostString())
	}
	if rep.Unpriced != 1 || rep.Priced != 0 {
		t.Errorf("priced=%d unpriced=%d, want 0 and 1", rep.Priced, rep.Unpriced)
	}
	// The total must not be inflated by a session that never reported one.
	if rep.TotalCost != 0 {
		t.Errorf("total cost = %v, want 0 from an unpriced session", rep.TotalCost)
	}
}

// The window is what makes a report answerable. A session older than it must
// not appear, or "what did today cost" quietly answers with last week.
func TestQueryRespectsItsWindow(t *testing.T) {
	now := time.Now().UTC()
	log := logWith(t,
		ev(t, event.KindSessionStarted, "recent",
			map[string]any{"agent": "claude", "account": "a"}, now.Add(-time.Hour)),
		ev(t, event.KindSessionStarted, "ancient",
			map[string]any{"agent": "claude", "account": "a"}, now.Add(-100*time.Hour)),
	)
	rep, err := Query(context.Background(), log.DB(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rep.Entries {
		if e.Session == "ancient" {
			t.Error("a session outside the window was reported")
		}
	}
	if len(rep.Entries) != 1 {
		t.Errorf("got %d entries, want only the recent one", len(rep.Entries))
	}
}

// The account is recorded when a session starts, not worked out when a report
// is read, because two logins of one agent run on separate plans here and
// "codex cost this much" is only half an answer without it.
func TestTheLoginIsCarriedIntoTheReport(t *testing.T) {
	now := time.Now().UTC()
	log := logWith(t,
		ev(t, event.KindSessionStarted, "s1",
			map[string]any{"agent": "codex", "account": "personal"}, now.Add(-time.Hour)),
	)
	rep, err := Query(context.Background(), log.DB(), now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Account != "personal" {
		t.Errorf("account not carried through: %+v", rep.Entries)
	}
}

// An empty window is a real answer, not an error. A report that fails when
// nothing ran is a report nobody can put on a dashboard.
func TestAnEmptyWindowIsAnEmptyReport(t *testing.T) {
	log := logWith(t)
	rep, err := Query(context.Background(), log.DB(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("an empty log is not an error: %v", err)
	}
	if len(rep.Entries) != 0 || rep.TotalCost != 0 {
		t.Errorf("expected nothing, got %+v", rep)
	}
}
