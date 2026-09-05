package event

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retentionLog(t *testing.T) *Log {
	t.Helper()
	log, err := Open(filepath.Join(t.TempDir(), "events.db"), Relaxed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

func put(t *testing.T, l *Log, kind Kind, at time.Time, payload map[string]any) {
	t.Helper()
	e, err := New(kind, "test", "s1", payload)
	if err != nil {
		t.Fatal(err)
	}
	e.At = at
	if _, err := l.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}
}

func payloadOf(t *testing.T, l *Log, kind Kind) []map[string]any {
	t.Helper()
	rows, err := l.DB().Query(`SELECT payload FROM events WHERE kind = ? ORDER BY seq`, string(kind))
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

// The core of the design. attention rows are 60% of this log's bytes because
// each carries the full message text, so dropping that one field reclaims
// nearly all of the size while keeping the record that a decision happened,
// which is what the board reads and what the analysis is computed from.
func TestOldAttentionKeepsItsDecisionAndLosesItsBody(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	body := strings.Repeat("a very long notification body ", 60)

	put(t, l, "attention", now.Add(-60*24*time.Hour),
		map[string]any{"message": body, "reason": "turn-complete",
			"outcome": map[string]any{"sent": true}})
	put(t, l, "attention", now.Add(-time.Hour),
		map[string]any{"message": body, "reason": "wants-attention",
			"outcome": map[string]any{"sent": true}})

	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}

	got := payloadOf(t, l, "attention")
	if len(got) != 2 {
		t.Fatalf("retention deleted an attention row; it should redact: %v", got)
	}
	old, recent := got[0], got[1]

	if _, still := old["message"]; still {
		t.Error("the old message body was not reclaimed")
	}
	if old["reason"] != "turn-complete" || old["outcome"] == nil {
		t.Errorf("redaction took more than the body: %v", old)
	}
	// A marker, or an old row looks like a schema change rather than a
	// retention pass.
	if old["redacted"] != true {
		t.Errorf("a redacted row must say so: %v", old)
	}
	if recent["message"] == nil {
		t.Error("a recent row was redacted; the window did not hold")
	}
	if _, marked := recent["redacted"]; marked {
		t.Error("an untouched row was marked redacted")
	}
}

// An audit log with a retention policy is not an audit log. These are what
// "what did it change, and who said yes" is answered from, and they are tiny.
func TestTheAuditTrailIsNeverTouched(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	ancient := now.Add(-5 * 365 * 24 * time.Hour)

	for _, k := range []Kind{KindPermissionRequested, KindPermissionAnswered,
		KindActuation, KindSessionStarted, KindSessionEnded, KindAutomationRun} {
		put(t, l, k, ancient, map[string]any{"detail": "five years old", "path": "/etc/x"})
	}

	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	for _, k := range []Kind{KindPermissionRequested, KindPermissionAnswered,
		KindActuation, KindSessionStarted, KindSessionEnded, KindAutomationRun} {
		got := payloadOf(t, l, k)
		if len(got) != 1 {
			t.Errorf("%s was pruned; it is audit or delivery history", k)
			continue
		}
		if got[0]["detail"] != "five years old" || got[0]["path"] != "/etc/x" {
			t.Errorf("%s was redacted: %v", k, got[0])
		}
	}
}

// Plan changes nothing, which is what makes the policy arguable before it is
// trusted. A retention job whose first observable effect is missing data is one
// nobody runs twice.
func TestPlanIsReadOnlyAndExplainsItself(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	for i := range 5 {
		put(t, l, "attention", now.Add(-time.Duration(60+i)*24*time.Hour),
			map[string]any{"message": strings.Repeat("x", 2000), "reason": "turn-complete"})
	}

	plan, err := l.Plan(context.Background(), DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Rows != 5 {
		t.Fatalf("plan = %+v, want one change covering 5 rows", plan)
	}
	if plan[0].Bytes < 5*2000 {
		t.Errorf("plan should count the bytes it would reclaim, got %d", plan[0].Bytes)
	}
	if plan[0].Deletes {
		t.Error("attention should be redacted, not deleted")
	}
	if !strings.Contains(plan[0].String(), "message") || plan[0].Rule.Why == "" {
		t.Errorf("a plan line must say what it does and why: %q", plan[0].String())
	}

	// Nothing may have changed.
	for _, p := range payloadOf(t, l, "attention") {
		if p["message"] == nil {
			t.Fatal("Plan modified the log")
		}
	}
}

// A payload that is not a JSON object is left exactly as it was. Replacing what
// it cannot parse would make a retention pass a corruption pass.
func TestUnparseablePayloadsAreSkippedNotClobbered(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	old := now.Add(-60 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := l.DB().Exec(
		`INSERT INTO events (at, kind, source, session, payload) VALUES (?,?,?,?,?)`,
		old, "attention", "test", "s1", []byte(`"just a string"`)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := l.DB().QueryRow(`SELECT payload FROM events WHERE kind='attention'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"just a string"` {
		t.Errorf("payload was rewritten to %q", raw)
	}
}

// Applying twice must be a no-op, because this runs on a schedule.
func TestApplyingTwiceChangesNothingFurther(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	put(t, l, "attention", now.Add(-60*24*time.Hour),
		map[string]any{"message": "body", "reason": "turn-complete"})

	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	first := payloadOf(t, l, "attention")
	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	second := payloadOf(t, l, "attention")
	if len(first) != len(second) || len(second) != 1 {
		t.Fatalf("row count moved on the second pass: %v then %v", first, second)
	}
	if first[0]["reason"] != second[0]["reason"] {
		t.Errorf("second pass changed a redacted row: %v vs %v", first, second)
	}
}

// The sequence is the join key for everything, so retention must not renumber
// or reuse it. Appending after a pass has to continue past the highest seq that
// ever existed, not fill a gap left by a delete.
func TestRetentionDoesNotDisturbTheSequence(t *testing.T) {
	l := retentionLog(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 4 {
		put(t, l, KindSessionUpdate, now.Add(-time.Duration(60+i)*24*time.Hour),
			map[string]any{"i": i})
	}
	head, _ := l.Head(ctx)

	if _, err := l.Apply(ctx, DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	e, err := New(KindSessionUpdate, "test", "s1", map[string]any{"after": true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Append(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq <= head {
		t.Errorf("new event took seq %d, at or below the pre-prune head %d", got.Seq, head)
	}
}

// A rule with no Redact fields deletes, and session.update is the one kind
// where that is right: it is agent chatter, useful while a session is live and
// rarely after.
func TestARuleWithoutRedactFieldsDeletes(t *testing.T) {
	l := retentionLog(t)
	now := time.Now().UTC()
	put(t, l, KindSessionUpdate, now.Add(-60*24*time.Hour), map[string]any{"text": "old chatter"})
	put(t, l, KindSessionUpdate, now.Add(-time.Hour), map[string]any{"text": "live chatter"})

	if _, err := l.Apply(context.Background(), DefaultRetention(), now); err != nil {
		t.Fatal(err)
	}
	got := payloadOf(t, l, KindSessionUpdate)
	if len(got) != 1 || got[0]["text"] != "live chatter" {
		t.Errorf("expected only the recent update to survive, got %v", got)
	}
}
