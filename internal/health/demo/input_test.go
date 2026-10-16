package demo

import (
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/health/core"
)

func TestEvaluateUsesSharedHealthRules(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	raw := `{"automations":[
		{"name":"no-op after stale delivery","cadence_minutes":60,"grace_minutes":15,"last_delivery_age_minutes":90,"outcome":"no-op"},
		{"name":"probe error","cadence_minutes":60,"grace_minutes":15,"last_delivery_age_minutes":0,"outcome":"probe-error"},
		{"name":"failed after stale delivery","cadence_minutes":60,"grace_minutes":15,"last_delivery_age_minutes":90,"outcome":"failed"},
		{"name":"no prior delivery","cadence_minutes":60,"grace_minutes":15,"last_delivery_age_minutes":null,"outcome":"healthy"},
		{"name":"no-op without delivery","cadence_minutes":60,"grace_minutes":15,"last_delivery_age_minutes":null,"outcome":"no-op"}
	]}`
	got, err := Evaluate([]byte(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name  string
		state core.State
	}{
		{name: "failed after stale delivery", state: core.Failing},
		{name: "no-op after stale delivery", state: core.Late},
		{name: "probe error", state: core.Unknown},
		{name: "no prior delivery", state: core.Unknown},
		{name: "no-op without delivery", state: core.Unknown},
	}
	for i := range want {
		if got[i].Name != want[i].name || got[i].State != want[i].state {
			t.Fatalf("report %d = (%q, %s), want (%q, %s)", i, got[i].Name, got[i].State, want[i].name, want[i].state)
		}
	}
	if len(got[1].Notes) == 0 || !strings.Contains(got[1].Notes[0], "no-op") || !strings.Contains(got[1].Notes[0], "retained") {
		t.Fatalf("no-op explanation lost delivery meaning: %#v", got[1])
	}
}

func TestEvaluateRejectsMalformedAndUnboundedInputs(t *testing.T) {
	now := time.Now()
	cases := []string{
		`{`,
		`{"automations":[]}`,
		`{"automations":[{"name":"x","cadence_minutes":0,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":-0,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":525601,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":-1,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":525601,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":-1441,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":525601,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"unknown"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":1e10000,"outcome":"healthy"}]}`,
		`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy","extra":true}]}`,
		`{"automations":[]} {"automations":[]}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := Evaluate([]byte(raw), now); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
	if _, err := Evaluate([]byte(`{"automations":[{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy"}]}`), now); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if _, err := Evaluate([]byte(strings.Repeat("x", maxInputBytes+1)), now); err == nil {
		t.Fatal("oversized input was accepted")
	}
	tooMany := `{"automations":[` + strings.TrimSuffix(strings.Repeat(`{"name":"x","cadence_minutes":1,"grace_minutes":0,"last_delivery_age_minutes":0,"outcome":"healthy"},`, maxAutomations+1), ",") + `]}`
	if _, err := Evaluate([]byte(tooMany), now); err == nil {
		t.Fatal("too many automation rows were accepted")
	}
}
