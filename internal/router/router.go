// Package router decides which model tier answers a given call.
//
// It is a CASCADE, not a predictor, and that distinction is the whole design.
//
// A predictor guesses difficulty up front and sends the work to one model. Its
// errors are quality losses you never find out about, which is exactly the
// failure mode that is unacceptable here: the requirement was "cannot risk
// quality". A cascade instead treats the cheap model as a proposal that must
// survive a check, and escalates whenever the check is not clearly passed.
//
// The asymmetry is deliberate. A wrong escalation costs money. A wrong
// acceptance costs correctness. So every ambiguous case escalates, and the
// savings come from the easy bulk rather than from clever borderline calls.
package router

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
)

// Verifier decides whether a cheap answer is good enough to keep. Returning
// false escalates. It must be cheap and mechanical: if verification needs
// judgement, it needs the strong model, and then routing down saved nothing.
type Verifier func(prompt, answer string) error

type Decision struct {
	Tier      model.Tier
	Reason    string
	Escalated bool
	// Attempts records every model that ran, in order, so the cost of a
	// failed cheap attempt is visible rather than hidden in the savings.
	Attempts []Attempt
}

type Attempt struct {
	Tier    model.Tier
	Model   string
	CostUSD float64
	Latency time.Duration
	Kept    bool
	Err     string
}

func (d Decision) TotalCost() float64 {
	var t float64
	for _, a := range d.Attempts {
		t += a.CostUSD
	}
	return t
}

// Classify exposes the routing decision without spending anything. This is the
// audit path: the answer to "why did that go to the cheap model" has to be
// inspectable, and a rule-based classifier is the reason it can be.
func (r *Router) Classify(req model.Request) (model.Tier, string) {
	t := classify(req)
	if t < r.Floor {
		return r.Floor, "floor raised to " + r.Floor.String()
	}
	return t, reasonFor(req)
}

type Router struct {
	reg *model.Registry
	log *event.Log

	// Floor is the lowest tier the router may choose. Raising it to
	// TierStrong disables routing entirely, which is the kill switch for
	// "something is wrong, stop being clever".
	Floor model.Tier
}

func New(reg *model.Registry, log *event.Log) *Router {
	return &Router{reg: reg, log: log}
}

// Call routes one request. verify may be nil, in which case the cheap tier is
// only used for calls classified as trivially mechanical, since there would be
// nothing to catch a bad answer.
func (r *Router) Call(ctx context.Context, req model.Request, verify Verifier) (model.Response, Decision, error) {
	start := classify(req)
	if start < r.Floor {
		start = r.Floor
	}
	if verify == nil && start < model.TierMid {
		// Unverifiable cheap output is exactly the risk the cascade exists to
		// avoid, so refuse the discount rather than gamble.
		start = model.TierMid
	}

	dec := Decision{Tier: start, Reason: reasonFor(req)}

	// Record the tier that actually RAN, never the one we asked for. Best()
	// degrades to a stronger provider when a tier is unconfigured, and
	// labelling that attempt "cheap" would attribute strong-model spend to the
	// cheap tier. In a router whose whole purpose is measuring cost, that is
	// not a cosmetic bug: every savings number downstream would be a lie.
	tried := map[model.Tier]bool{}

	for want := start; want <= model.TierStrong; want++ {
		p, ok := r.reg.Best(want)
		if !ok {
			continue
		}
		actual := p.Tier()
		if tried[actual] {
			continue // Best() handed back a provider we have already run
		}
		tried[actual] = true

		resp, err := p.Complete(ctx, req)
		att := Attempt{Tier: actual, Model: p.Model(), CostUSD: resp.Usage.CostUSD, Latency: resp.Latency}
		if err != nil {
			att.Err = err.Error()
			dec.Attempts = append(dec.Attempts, att)
			dec.Escalated = true
			continue // a provider that errored is not evidence about difficulty
		}

		// Verifying the strongest tier is pointless: there is nowhere to
		// escalate to, so rejecting its answer would leave us with nothing.
		if verify != nil && actual < model.TierStrong {
			if vErr := verify(req.Prompt, resp.Text); vErr != nil {
				att.Err = "verification failed: " + vErr.Error()
				dec.Attempts = append(dec.Attempts, att)
				dec.Escalated = true
				continue
			}
		}

		att.Kept = true
		dec.Attempts = append(dec.Attempts, att)
		dec.Tier = actual
		r.record(ctx, req, dec, resp)
		return resp, dec, nil
	}

	r.record(ctx, req, dec, model.Response{})
	return model.Response{}, dec, fmt.Errorf("no tier produced an acceptable answer (%d attempts)", len(dec.Attempts))
}

func (r *Router) record(ctx context.Context, req model.Request, d Decision, resp model.Response) {
	if r.log == nil {
		return
	}
	ev, err := event.New(event.KindRouteDecided, "router", "", map[string]any{
		"tier": d.Tier.String(), "reason": d.Reason, "escalated": d.Escalated,
		"attempts": d.Attempts, "cost": d.TotalCost(),
		"promptChars": len(req.Prompt), "model": resp.Model,
	})
	if err == nil {
		_, _ = r.log.Append(ctx, ev)
	}
}

// ---------------------------------------------------------------- classify --

// classify is rule-based, and stays that way until measurement says otherwise.
//
// A learned classifier adds 50-100ms and a training set to maintain, and the
// cascade already catches its mistakes, so the marginal value of being clever
// here is small. Rules are also auditable, which matters when the question is
// "why did this go to the cheap model".
var (
	reStructured = regexp.MustCompile(`(?i)\b(classify|categori[sz]e|extract|parse|label|tag|yes or no|true or false|which of|pick one)\b`)
	reJudgement  = regexp.MustCompile(`(?i)\b(design|architect|refactor|debug|why|trade-?off|review|critique|prove|security|migrate|strategy)\b`)
	reCode       = regexp.MustCompile("(?s)```|\\bfunc \\w+\\(|\\bclass \\w+|\\bdef \\w+\\(")
)

func classify(req model.Request) model.Tier {
	p := req.Prompt

	// Length is the strongest single signal, and it is free. Long prompts
	// carry context that small models drop.
	switch {
	case len(p) > 8000:
		return model.TierStrong
	case reJudgement.MatchString(p):
		return model.TierStrong
	case reCode.MatchString(p) && len(p) > 1500:
		return model.TierStrong
	case reStructured.MatchString(p) && len(p) < 2000:
		return model.TierCheap
	case len(p) < 400:
		return model.TierCheap
	default:
		return model.TierMid
	}
}

func reasonFor(req model.Request) string {
	p := req.Prompt
	switch {
	case len(p) > 8000:
		return "long prompt"
	case reJudgement.MatchString(p):
		return "judgement verb"
	case reCode.MatchString(p) && len(p) > 1500:
		return "substantial code"
	case reStructured.MatchString(p) && len(p) < 2000:
		return "short structured task"
	case len(p) < 400:
		return "short prompt"
	default:
		return "default"
	}
}

// ---------------------------------------------------------------- verifiers -

// JSONVerifier accepts only well-formed JSON containing the required keys.
// This is the workhorse: most cheap-tier work is structured extraction, and
// "did it produce the shape we asked for" is both mechanical and a strong
// proxy for whether the small model understood the task.
func JSONVerifier(required ...string) Verifier {
	return func(_, answer string) error {
		obj, err := ExtractJSON(answer)
		if err != nil {
			return err
		}
		for _, k := range required {
			if _, ok := obj[k]; !ok {
				return fmt.Errorf("missing key %q", k)
			}
		}
		return nil
	}
}

// OneOfVerifier accepts only an answer that is exactly one of the allowed
// labels, ignoring case and surrounding punctuation.
func OneOfVerifier(allowed ...string) Verifier {
	return func(_, answer string) error {
		got := strings.ToLower(strings.Trim(strings.TrimSpace(answer), ".\"'`"))
		for _, a := range allowed {
			if got == strings.ToLower(a) {
				return nil
			}
		}
		return fmt.Errorf("answer %q is not one of %v", truncate(got, 60), allowed)
	}
}

// NonEmptyVerifier is the weakest useful check: it catches a model that
// refused, returned an apology, or produced nothing.
func NonEmptyVerifier(minLen int) Verifier {
	return func(_, answer string) error {
		a := strings.TrimSpace(answer)
		if len(a) < minLen {
			return fmt.Errorf("answer too short (%d < %d)", len(a), minLen)
		}
		if strings.HasPrefix(strings.ToLower(a), "i can't") || strings.HasPrefix(strings.ToLower(a), "i cannot") {
			return fmt.Errorf("model declined")
		}
		return nil
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
