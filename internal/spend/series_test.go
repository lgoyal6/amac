package spend

import (
	"strings"
	"testing"
	"time"
)

// A snapshot with holes in it: usage on some days, none on others, one charge.
func seriesFixture(t *testing.T, days int) (Snapshot, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 12, 15, 4, 0, 0, time.Local)
	day := func(back int) string { return now.AddDate(0, 0, -back).Format("2006-01-02") }
	charge := int64(2500)
	return Snapshot{
		Events: []Event{
			// 09:00 UTC lands on the same local day west of UTC, and on the
			// next one east of it. Either way it must land on a local day.
			{Date: now.AddDate(0, 0, -1).UTC().Format(time.RFC3339), Kind: "charge", AmountCents: &charge},
			{Date: day(0) + "T09:00:00Z", Kind: "trial_ending"},
		},
		Usage: Usage{Days: days, Tools: []Tool{
			{Tool: "Claude Code", ByDay: map[string]Totals{day(0): {Cents: 100}, day(1): {Cents: 200}, day(3): {Cents: 50}}},
			{Tool: "Codex", ByDay: map[string]Totals{day(0): {Cents: 10}, day(5): {Cents: 5}}},
		}},
	}, now
}

func TestSeriesFillsTheWindowWithNullsNotZeros(t *testing.T) {
	snap, now := seriesFixture(t, 30)
	got := snap.Series(now, 30)

	if got.Days != 30 || len(got.Points) != 30 {
		t.Fatalf("want 30 points, got days=%d len=%d", got.Days, len(got.Points))
	}
	last := got.Points[29]
	if last.Date != "2026-09-12" || last.AgentUSD == nil || *last.AgentUSD != 1.10 {
		t.Errorf("today: %+v, want 2026-09-12 at $1.10 across both tools", last)
	}
	if last.BilledUSD != nil {
		t.Errorf("no charge today, billed must be null, got %v", *last.BilledUSD)
	}
	yesterday := got.Points[28]
	if yesterday.AgentUSD == nil || *yesterday.AgentUSD != 2.00 {
		t.Errorf("yesterday agent: %+v", yesterday)
	}
	if yesterday.BilledUSD == nil || *yesterday.BilledUSD != 25.00 {
		t.Errorf("yesterday billed: %+v, want $25", yesterday)
	}
	if gap := got.Points[27]; gap.AgentUSD != nil || gap.BilledUSD != nil {
		t.Errorf("two days ago has no data and must be null, got %+v", gap)
	}
	if p := got.Points[26]; p.AgentUSD == nil || *p.AgentUSD != 0.50 {
		t.Errorf("three days ago: %+v", p)
	}
	if p := got.Points[24]; p.AgentUSD == nil || *p.AgentUSD != 0.05 {
		t.Errorf("five days ago: %+v", p)
	}
	if got.CoverageDays != 4 {
		t.Errorf("CoverageDays = %d, want 4", got.CoverageDays)
	}
	if !strings.Contains(got.Warning, "4 of 30") {
		t.Errorf("warning must count the gaps: %q", got.Warning)
	}
}

// Asking for more than looseapi scanned must say so, not pad with zeros.
func TestSeriesBeyondTheScanWindowNamesTheCutoff(t *testing.T) {
	snap, now := seriesFixture(t, 30)
	got := snap.Series(now, 90)
	if len(got.Points) != 90 || got.Points[0].Date != "2026-06-15" {
		t.Fatalf("window: len=%d first=%s", len(got.Points), got.Points[0].Date)
	}
	if got.CoverageDays != 4 {
		t.Errorf("CoverageDays = %d, want 4", got.CoverageDays)
	}
	for _, want := range []string{"4 of 90", "last 30 days", "2026-08-13"} {
		if !strings.Contains(got.Warning, want) {
			t.Errorf("warning %q lacks %q", got.Warning, want)
		}
	}
}

func TestSeriesWithFullCoverageHasNoWarning(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 4, 0, 0, time.Local)
	byDay := map[string]Totals{}
	for i := 0; i < 30; i++ {
		byDay[now.AddDate(0, 0, -i).Format("2006-01-02")] = Totals{Cents: 1}
	}
	snap := Snapshot{Usage: Usage{Days: 30, Tools: []Tool{{ByDay: byDay}}}}
	got := snap.Series(now, 30)
	if got.CoverageDays != 30 || got.Warning != "" {
		t.Errorf("coverage=%d warning=%q", got.CoverageDays, got.Warning)
	}
}

// No snapshot at all is a valid answer with the same shape: the chart still
// gets its axis, every point null, and the caller adds the reason.
func TestSeriesOfEmptySnapshotIsAllNull(t *testing.T) {
	got := Snapshot{}.Series(time.Now(), 60)
	if len(got.Points) != 60 || got.CoverageDays != 0 {
		t.Fatalf("len=%d coverage=%d", len(got.Points), got.CoverageDays)
	}
	for _, p := range got.Points {
		if p.AgentUSD != nil || p.BilledUSD != nil {
			t.Fatalf("expected null, got %+v", p)
		}
	}
	if !strings.Contains(got.Warning, "0 of 60") {
		t.Errorf("warning %q", got.Warning)
	}
}
