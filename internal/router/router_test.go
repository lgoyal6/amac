package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lgoyal6/amac/internal/model"
)

// fake is a deterministic provider so routing behaviour can be tested without
// a network, a key, or a bill.
type fake struct {
	tier   model.Tier
	answer string
	err    error
	calls  *[]model.Tier
}

func (f *fake) Name() string     { return "fake-" + f.tier.String() }
func (f *fake) Model() string    { return "fake-" + f.tier.String() }
func (f *fake) Tier() model.Tier { return f.tier }
func (f *fake) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	*f.calls = append(*f.calls, f.tier)
	if f.err != nil {
		return model.Response{}, f.err
	}
	return model.Response{
		Text: f.answer, Model: f.Model(), Tier: f.tier, Latency: time.Millisecond,
		Usage: model.Usage{CostUSD: costFor(f.tier)},
	}, nil
}

func costFor(t model.Tier) float64 {
	switch t {
	case model.TierCheap:
		return 0.0001
	case model.TierMid:
		return 0.001
	default:
		return 0.01
	}
}

func rig(t *testing.T, answers map[model.Tier]string) (*Router, *[]model.Tier) {
	t.Helper()
	calls := &[]model.Tier{}
	reg := model.NewRegistry()
	for tier, ans := range answers {
		reg.Set(&fake{tier: tier, answer: ans, calls: calls})
	}
	return New(reg, nil), calls
}

func TestClassifierPicksTierByShape(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   model.Tier
	}{
		{"short structured", "Classify this as bug or feature: the retry loop spins", model.TierCheap},
		{"short anything", "what is 2+2", model.TierCheap},
		{"judgement verb", "Explain the trade-off between fsync on every commit and letting the OS schedule it, and say which you would pick", model.TierStrong},
		{"very long", strings.Repeat("context ", 1200), model.TierStrong},
		{"medium prose", strings.Repeat("some ordinary sentence about the system. ", 20), model.TierMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(model.Request{Prompt: c.prompt}); got != c.want {
				t.Fatalf("classify() = %s, want %s (reason %q)", got, c.want, reasonFor(model.Request{Prompt: c.prompt}))
			}
		})
	}
}

// The core cascade property: a cheap answer that fails verification must be
// replaced by a stronger one, not returned.
func TestCascadeEscalatesOnFailedVerification(t *testing.T) {
	r, calls := rig(t, map[model.Tier]string{
		model.TierCheap:  "not json at all",
		model.TierMid:    "still not json",
		model.TierStrong: `{"company":"Vercel","role":"Backend"}`,
	})

	resp, dec, err := r.Call(context.Background(),
		model.Request{Prompt: "Extract company and role as JSON"},
		JSONVerifier("company", "role"))
	if err != nil {
		t.Fatalf("cascade should have reached the strong tier: %v", err)
	}
	if dec.Tier != model.TierStrong {
		t.Fatalf("settled on %s, want strong", dec.Tier)
	}
	if !dec.Escalated {
		t.Fatal("escalated flag not set after two rejected attempts")
	}
	if !strings.Contains(resp.Text, "Vercel") {
		t.Fatalf("returned the wrong answer: %q", resp.Text)
	}
	if len(*calls) != 3 {
		t.Fatalf("called %v, want all three tiers in order", *calls)
	}
	// The failed cheap attempts must still be billed, not hidden.
	if got := dec.TotalCost(); got <= costFor(model.TierStrong) {
		t.Fatalf("total cost %v ignores the failed attempts", got)
	}
}

func TestCascadeStopsAtFirstAcceptableTier(t *testing.T) {
	r, calls := rig(t, map[model.Tier]string{
		model.TierCheap:  `{"company":"Vercel","role":"Backend"}`,
		model.TierStrong: `{"company":"WRONG","role":"WRONG"}`,
	})

	_, dec, err := r.Call(context.Background(),
		model.Request{Prompt: "Extract company and role as JSON"},
		JSONVerifier("company", "role"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != model.TierCheap {
		t.Fatalf("settled on %s, want cheap: a passing cheap answer must be kept", dec.Tier)
	}
	if len(*calls) != 1 {
		t.Fatalf("called %v, want exactly one call", *calls)
	}
}

// Without a verifier there is nothing to catch a bad cheap answer, so the
// router must refuse the discount rather than gamble.
func TestUnverifiableWorkNeverStartsCheap(t *testing.T) {
	r, calls := rig(t, map[model.Tier]string{
		model.TierCheap:  "cheap",
		model.TierMid:    "mid",
		model.TierStrong: "strong",
	})

	_, dec, err := r.Call(context.Background(), model.Request{Prompt: "what is 2+2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier == model.TierCheap {
		t.Fatal("routed to cheap with no verifier: an unverifiable cheap answer is exactly the risk to avoid")
	}
	if (*calls)[0] == model.TierCheap {
		t.Fatalf("first call was cheap: %v", *calls)
	}
}

// A provider error is an infrastructure fact, not evidence about difficulty,
// so it must escalate rather than fail the call.
func TestProviderErrorEscalates(t *testing.T) {
	calls := &[]model.Tier{}
	reg := model.NewRegistry()
	reg.Set(&fake{tier: model.TierCheap, err: errors.New("502 bad gateway"), calls: calls})
	reg.Set(&fake{tier: model.TierStrong, answer: "recovered", calls: calls})
	r := New(reg, nil)

	resp, dec, err := r.Call(context.Background(),
		model.Request{Prompt: "classify this as a or b"}, NonEmptyVerifier(1))
	if err != nil {
		t.Fatalf("a dead cheap provider must not fail the call: %v", err)
	}
	if resp.Text != "recovered" || dec.Tier != model.TierStrong {
		t.Fatalf("got %q at %s", resp.Text, dec.Tier)
	}
	if dec.Attempts[0].Err == "" {
		t.Fatal("the failed attempt was not recorded")
	}
}

// Floor is the kill switch: raise it and routing stops being clever.
func TestFloorDisablesRouting(t *testing.T) {
	r, calls := rig(t, map[model.Tier]string{
		model.TierCheap:  `{"a":1}`,
		model.TierStrong: `{"a":1}`,
	})
	r.Floor = model.TierStrong

	_, dec, err := r.Call(context.Background(),
		model.Request{Prompt: "classify this"}, JSONVerifier("a"))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Tier != model.TierStrong {
		t.Fatalf("floor ignored: settled on %s", dec.Tier)
	}
	if len(*calls) != 1 || (*calls)[0] != model.TierStrong {
		t.Fatalf("calls %v, want a single strong call", *calls)
	}
}

// A missing tier must degrade to a stronger one. Doing the work expensively
// beats not doing it.
func TestMissingTierDegradesUpward(t *testing.T) {
	calls := &[]model.Tier{}
	reg := model.NewRegistry()
	reg.Set(&fake{tier: model.TierStrong, answer: "ok", calls: calls})
	r := New(reg, nil)

	_, dec, err := r.Call(context.Background(),
		model.Request{Prompt: "classify this as a or b"}, NonEmptyVerifier(1))
	if err != nil {
		t.Fatalf("should have degraded to strong: %v", err)
	}
	if dec.Tier != model.TierStrong {
		t.Fatalf("got %s", dec.Tier)
	}
}

func TestExtractJSONSurvivesModelFormatting(t *testing.T) {
	want := "Vercel"
	cases := []string{
		`{"company":"Vercel"}`,
		"```json\n{\"company\":\"Vercel\"}\n```",
		"```\n{\"company\":\"Vercel\"}\n```",
		"Sure! Here is the JSON:\n\n{\"company\":\"Vercel\"}\n\nLet me know if you need anything else.",
		"Here you go: {\"company\":\"Vercel\", \"note\":\"has } inside\"}",
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			obj, err := ExtractJSON(c)
			if err != nil {
				t.Fatalf("failed on %q: %v", c, err)
			}
			if obj["company"] != want {
				t.Fatalf("got %v", obj)
			}
		})
	}
}

func TestExtractJSONRejectsProse(t *testing.T) {
	if _, err := ExtractJSON("I think the company is Vercel."); err == nil {
		t.Fatal("prose accepted as JSON")
	}
}
