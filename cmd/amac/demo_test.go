package main

import (
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/health"
)

func TestDemoHealthReportsUseSharedEvaluator(t *testing.T) {
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	reports, err := demoHealthReports(demoHealthPlans(), now)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]health.State, len(reports))
	for _, report := range reports {
		states[report.Name] = report.State
	}
	want := map[string]health.State{
		"link-checker":   health.Failing,
		"cache-warmer":   health.OK,
		"digest-email":   health.OK,
		"invoice-sync":   health.OK,
		"nightly-backup": health.OK,
	}
	if len(states) != len(want) {
		t.Fatalf("got %d reports, want %d", len(states), len(want))
	}
	for name, state := range want {
		if got := states[name]; got != state {
			t.Errorf("%s state = %q, want %q", name, got, state)
		}
	}
}
