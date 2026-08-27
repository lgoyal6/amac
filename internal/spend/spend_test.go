package spend

import (
	"encoding/json"
	"testing"
	"time"
)

const sample = `{
  "generatedAt": "2026-08-26T17:47:33.449Z",
  "monthlyCents": 2500,
  "alerts": [
    {"kind":"unread_money","severity":1,"service":"X","message":"low"},
    {"kind":"credit_burndown","severity":3,"service":"AWS","message":"$10 left, ~1.3 days"}
  ],
  "usage": {"days":30,"tools":[
    {"tool":"Claude Code","total":{"cents":1525091},"byDay":{"2026-08-26":{"cents":102963}}},
    {"tool":"Codex","total":{"cents":390000},"byDay":{"2026-08-26":{"cents":34500}}}
  ]}
}`

func load(t *testing.T) Snapshot {
	t.Helper()
	var s Snapshot
	if err := json.Unmarshal([]byte(sample), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Worst is what reaches a phone banner, so the ordering is the feature. A
// credit balance with a day left must not sit under an unread receipt.
func TestWorstPutsTheUrgentThingFirst(t *testing.T) {
	got := load(t).Worst(1)
	if len(got) != 1 || got[0].Service != "AWS" {
		t.Fatalf("got %+v, want the AWS burndown first", got)
	}
}

func TestTotalsAcrossTools(t *testing.T) {
	s := load(t)
	if got := s.AgentCents(); got != 1915091 {
		t.Errorf("AgentCents = %d, want 1915091", got)
	}
	// Local date, not UTC: this answers "what have I run up today", and a UTC
	// boundary would zero it in the late afternoon here.
	day := time.Date(2026, 8, 26, 15, 4, 0, 0, time.Local)
	if got := s.TodayCents(day); got != 137463 {
		t.Errorf("TodayCents = %d, want 137463", got)
	}
	if got := s.TodayCents(day.AddDate(0, 0, 1)); got != 0 {
		t.Errorf("a day with no usage must be 0, got %d", got)
	}
}

// A cost report that rounds real money to zero is the one thing it must never
// do, and one that prints eight decimals is unreadable on a phone.
func TestUSD(t *testing.T) {
	for _, tc := range []struct {
		cents int64
		want  string
	}{
		{0, "$0"}, {7, "$0.07"}, {99, "$0.99"}, {100, "$1"}, {2500, "$25"}, {1915091, "$19151"},
	} {
		if got := USD(tc.cents); got != tc.want {
			t.Errorf("USD(%d) = %s, want %s", tc.cents, got, tc.want)
		}
	}
}

// A missing snapshot means looseapi has not run, which the caller must say
// plainly. Returning a zeroed struct with no error would render as "you spent
// nothing", which is the worst available answer.
func TestMissingSnapshotIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Read(); err == nil {
		t.Fatal("a missing snapshot must be an error, not an empty report")
	}
}
