package miner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// The miner turns the log into suggestions, which means every bug in it becomes
// advice. It had no test at all, so nothing checked the property that matters
// most: it must not suggest automating a decision you have ever actually made.

func mineLog(t *testing.T, evs ...event.Event) Report {
	t.Helper()
	log, err := event.Open(filepath.Join(t.TempDir(), "events.db"), event.Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for _, e := range evs {
		if _, err := log.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Mine(context.Background(), log.DB(), time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func ev(t *testing.T, kind event.Kind, session string, payload map[string]any) event.Event {
	t.Helper()
	e, err := event.New(kind, "test", session, payload)
	if err != nil {
		t.Fatal(err)
	}
	e.At = time.Now().UTC().Add(-time.Hour)
	return e
}

func permission(t *testing.T, session, tool, title, outcome string) []event.Event {
	return []event.Event{
		ev(t, event.KindPermissionRequested, session,
			map[string]any{"toolCallId": tool, "title": title}),
		ev(t, event.KindPermissionAnswered, session,
			map[string]any{"toolCallId": tool, "outcome": outcome}),
	}
}

func find(rep Report, kind string) []Suggestion {
	var out []Suggestion
	for _, s := range rep.Suggestions {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// A prompt approved every time carries no decision, so automating it removes an
// interruption rather than a safeguard. That is the whole premise of the
// suggestion, and it needs enough repetitions to mean something.
func TestAlwaysApprovedIsSuggestedOnlyAfterEnoughEvidence(t *testing.T) {
	var evs []event.Event
	for i := range 4 {
		evs = append(evs, permission(t, "s1", "tc-"+string(rune('a'+i)), "Read a file", "selected")...)
	}
	got := find(mineLog(t, evs...), "auto-approve")
	if len(got) != 1 {
		t.Fatalf("got %d auto-approve suggestions, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Evidence, "4 of 4") {
		t.Errorf("evidence should say what it counted: %q", got[0].Evidence)
	}
	if got[0].Action == "" {
		t.Error("a suggestion without an action is an observation")
	}

	// Twice is not a pattern. Suggesting from two samples is how a miner
	// becomes noise nobody reads.
	twice := permission(t, "s1", "x1", "Read a file", "selected")
	twice = append(twice, permission(t, "s1", "x2", "Read a file", "selected")...)
	if got := find(mineLog(t, twice...), "auto-approve"); len(got) != 0 {
		t.Errorf("two approvals should not be a pattern: %+v", got)
	}
}

// The one thing this must never do. A prompt you denied even once is a decision
// you make, and suggesting an allow rule for it would automate away the exact
// judgement the prompt exists to collect.
func TestNeverSuggestAutomatingSomethingYouHaveDenied(t *testing.T) {
	var evs []event.Event
	for i := range 5 {
		evs = append(evs, permission(t, "s1", "ok-"+string(rune('a'+i)), "Delete a branch", "selected")...)
	}
	evs = append(evs, permission(t, "s1", "denied-once", "Delete a branch", "cancelled")...)

	if got := find(mineLog(t, evs...), "auto-approve"); len(got) != 0 {
		t.Errorf("a single denial must veto the suggestion, got %+v", got)
	}
}

// Work asked for in the same words repeatedly is a saved command waiting to
// happen, and the stem is what groups it.
func TestRepeatedPromptsAreFoundAndRareOnesAreNot(t *testing.T) {
	var evs []event.Event
	for range 3 {
		evs = append(evs, ev(t, event.KindSessionUpdate, "s1",
			map[string]any{"prompt": "run the tests and fix whatever breaks"}))
	}
	evs = append(evs, ev(t, event.KindSessionUpdate, "s1",
		map[string]any{"prompt": "something asked once and never again"}))

	got := find(mineLog(t, evs...), "saved-command")
	if len(got) == 0 {
		// The kind name is an implementation detail; assert on the behaviour
		// rather than failing on a rename.
		if len(mineLog(t, evs...).Suggestions) == 0 {
			t.Fatal("three identical prompts produced no suggestion at all")
		}
	}
	for _, s := range mineLog(t, evs...).Suggestions {
		if strings.Contains(s.Title, "once and never again") {
			t.Error("a prompt seen once was suggested as a pattern")
		}
	}
}

// Confidence orders the list, so it has to rise with evidence or the most
// speculative suggestion can lead.
func TestMoreEvidenceMeansMoreConfidenceAndSortsFirst(t *testing.T) {
	if confidence(3) >= confidence(30) {
		t.Errorf("confidence(3)=%v is not below confidence(30)=%v", confidence(3), confidence(30))
	}
	if confidence(3) <= 0 || confidence(1000) > 1 {
		t.Errorf("confidence must stay a share: %v .. %v", confidence(3), confidence(1000))
	}

	var evs []event.Event
	for i := range 3 {
		evs = append(evs, permission(t, "s1", "few-"+string(rune('a'+i)), "Rare thing", "selected")...)
	}
	for i := range 12 {
		evs = append(evs, permission(t, "s2", "many-"+string(rune('a'+i)), "Common thing", "selected")...)
	}
	rep := mineLog(t, evs...)
	if len(rep.Suggestions) < 2 {
		t.Fatalf("expected both, got %+v", rep.Suggestions)
	}
	if !strings.Contains(rep.Suggestions[0].Title, "Common thing") {
		t.Errorf("the better-evidenced suggestion should lead: %+v", rep.Suggestions)
	}
}

// An empty log is a real answer. A miner that errors on a quiet week is one
// nobody can put on a schedule.
func TestAnEmptyLogMinesNothingWithoutFailing(t *testing.T) {
	rep := mineLog(t)
	if len(rep.Suggestions) != 0 {
		t.Errorf("invented %d suggestions from nothing", len(rep.Suggestions))
	}
	if rep.Stats == nil {
		t.Error("stats must be usable rather than nil")
	}
	if rep.Window <= 0 {
		t.Error("the report should say what window it covered")
	}
}
