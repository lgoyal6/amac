// Package eval measures what routing actually costs in quality.
//
// This exists before the router is trusted, not after. "I built a router" is
// an assertion; "on 200 tasks from my own workload, routing cut spend 61% and
// retained 96% of strong-model quality, here is the curve" is a result. The
// harness is what turns one into the other, so it is deliberately the boring,
// reproducible part of the system.
//
// Every task carries its own mechanical check. A judge model grading a judge
// model is how evaluation harnesses quietly stop measuring anything.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/router"
)

type CheckKind string

const (
	CheckContains CheckKind = "contains"  // answer contains all of Values
	CheckOneOf    CheckKind = "one_of"    // answer is exactly one of Values
	CheckRegex    CheckKind = "regex"     // answer matches Values[0]
	CheckJSONKeys CheckKind = "json_keys" // answer is JSON with all Values as keys
)

type Task struct {
	ID     string    `json:"id"`
	Prompt string    `json:"prompt"`
	System string    `json:"system,omitempty"`
	Check  CheckKind `json:"check"`
	Values []string  `json:"values"`
	// Labels is the option set production knows before the call, for one_of
	// tasks. It exists because Values is the answer key, and a gate built from
	// the answer key is not a gate: see Gate.
	Labels []string `json:"labels,omitempty"`
	// Note records why the task is in the set. A suite nobody can read is a
	// suite nobody maintains.
	Note string `json:"note,omitempty"`
}

// Verify is the mechanical check that GRADES an answer. It is the answer key,
// so it is the one thing the cascade must never be given: see Gate.
//
// It stays mechanical for the reason the package doc gives. A judge model
// grading a judge model is how evaluation harnesses quietly stop measuring
// anything.
func (t Task) Verify(answer string) error {
	a := strings.TrimSpace(answer)
	switch t.Check {
	case CheckContains:
		for _, v := range t.Values {
			if !strings.Contains(strings.ToLower(a), strings.ToLower(v)) {
				return fmt.Errorf("missing %q", v)
			}
		}
		return nil
	case CheckOneOf:
		// Values, not Labels: this is the grader, and the correct answer is
		// the whole point of it.
		return router.OneOfVerifier(t.Values...)("", a)
	case CheckRegex:
		if len(t.Values) == 0 {
			return fmt.Errorf("regex check with no pattern")
		}
		re, err := regexp.Compile(t.Values[0])
		if err != nil {
			return err
		}
		if !re.MatchString(a) {
			return fmt.Errorf("does not match /%s/", t.Values[0])
		}
		return nil
	case CheckJSONKeys:
		return router.JSONVerifier(t.Values...)("", a)
	}
	return fmt.Errorf("unknown check %q", t.Check)
}

// Gate is the verifier the CASCADE runs with, which is deliberately not the
// same thing as the check that grades the answer.
//
// contains and regex carry the expected answer inside the check. Handing one to
// the router lets the cascade consult the answer key before deciding whether to
// keep a cheap model's reply, and production has no answer key. Measured
// routed-arm quality would then be a property of the harness rather than of the
// router, which is the one thing this package exists not to do.
//
// one_of and json_keys are different in kind: production really does know the
// label set and the required keys before the call, so those gates pass through
// unchanged. Everything else falls back to the weakest check production could
// actually run, which is "did the model answer at all".
func (t Task) Gate() router.Verifier {
	switch t.Check {
	case CheckOneOf:
		// Only when the task says what production would have known. Without
		// Labels the only set available is Values, which is the correct answer,
		// and gating on it makes the cascade retry until a model says the right
		// word. The routed arm would then be measured with the answer key in
		// its hand, which is the one thing this package exists not to do.
		if len(t.Labels) > len(t.Values) {
			return router.OneOfVerifier(t.Labels...)
		}
		return router.NonEmptyVerifier(1)
	case CheckJSONKeys:
		// No leak here: the required keys are the shape of the reply, not its
		// content, and production really does know them before it calls.
		return router.JSONVerifier(t.Values...)
	default:
		return router.NonEmptyVerifier(1)
	}
}

// RealGate reports whether Gate is the same check production would run. It is
// reported per run because it bounds how much the routed number is worth: a
// suite of tasks nothing can mechanically gate measures a cascade that is
// mostly running on hope.
func (t Task) RealGate() bool {
	switch t.Check {
	case CheckOneOf:
		// A label set that is exactly the answer is not a label set. It was
		// counted as a real gate before, so the tasks that leaked the answer
		// were the ones the report held up as trustworthy.
		return len(t.Labels) > len(t.Values)
	case CheckJSONKeys:
		return true
	}
	return false
}

func LoadTasks(path string) ([]Task, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tasks []Task
	if err := json.Unmarshal(b, &tasks); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, t := range tasks {
		if t.ID == "" || t.Prompt == "" || t.Check == "" {
			return nil, fmt.Errorf("task %d: id, prompt and check are required", i)
		}
		// A label set that does not contain the answer would make the gate
		// reject every correct reply, so the cascade would escalate to the top
		// on a task the cheap model got right.
		for _, v := range t.Values {
			if len(t.Labels) == 0 {
				break
			}
			if !slices.ContainsFunc(t.Labels, func(l string) bool {
				return strings.EqualFold(l, v)
			}) {
				return nil, fmt.Errorf("task %s: answer %q is not among its labels %v", t.ID, v, t.Labels)
			}
		}
	}
	return tasks, nil
}

// ---------------------------------------------------------------- results ---

type Result struct {
	TaskID  string
	Arm     string
	Passed  bool
	CostUSD float64
	Latency time.Duration
	Detail  string
}

type ArmSummary struct {
	Arm    string
	Passed int
	Total  int
	// Errored counts calls that never produced an answer at all, which is a
	// different fact from an answer that was wrong and must not be folded into
	// the same percentage. An arm whose model id is misspelled scores zero on
	// quality and zero on cost, and reads exactly like a free, useless model.
	Errored   int
	ErrDetail string // one example, because a 404 is actionable and a timeout is not
	CostUSD   float64
	Latency   time.Duration
	Escalated int
}

func (a ArmSummary) Quality() float64 {
	if a.Total == 0 {
		return 0
	}
	return float64(a.Passed) / float64(a.Total)
}

// Measured reports whether the arm produced enough answers to have a quality
// worth reading. An arm that never answered has no score, as opposed to a
// score of zero.
func (a ArmSummary) Measured() bool { return a.Total > 0 && a.Errored < a.Total }

type Report struct {
	Arms    []ArmSummary
	Results []Result

	// RealGates and WeakGates split the task set by whether the cascade could
	// be gated the way production would gate it. Reported alongside the curve
	// because the routed arm's quality means less the larger WeakGates is.
	RealGates int
	WeakGates int
}

// Table renders the cost/quality frontier. Savings are stated against the
// strong-model arm, because that is the honest baseline: the question is never
// "is cheap cheaper" but "what did choosing cheap cost me".
func (r Report) Table() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-14s %8s %10s %12s %10s %7s\n",
		"ARM", "QUALITY", "COST", "COST/TASK", "P50 LAT", "ERRORS")
	var baseline *ArmSummary
	for i := range r.Arms {
		if r.Arms[i].Arm == "strong" {
			baseline = &r.Arms[i]
		}
	}
	for _, a := range r.Arms {
		per := 0.0
		if a.Total > 0 {
			per = a.CostUSD / float64(a.Total)
		}
		quality := fmt.Sprintf("%6.1f%%", a.Quality()*100)
		if !a.Measured() {
			quality = "      -"
		}
		errs := ""
		if a.Errored > 0 {
			errs = fmt.Sprintf("%d", a.Errored)
		}
		fmt.Fprintf(&sb, "%-14s %8s %10s %12s %10s %7s\n",
			a.Arm, quality, money(a.CostUSD), money(per), a.Latency.Round(time.Millisecond), errs)
	}
	// An arm that failed to answer is a configuration report, not a result.
	// Saying so here is the difference between "the strong model is terrible"
	// and "the strong model's id is misspelled", which is how this arrived.
	for _, a := range r.Arms {
		if a.Errored == 0 {
			continue
		}
		fmt.Fprintf(&sb, "\n%s: %d of %d calls returned no answer", a.Arm, a.Errored, a.Total)
		if !a.Measured() {
			sb.WriteString(", so it has no quality score")
		}
		if a.ErrDetail != "" {
			fmt.Fprintf(&sb, "\n  %s", a.ErrDetail)
		}
		sb.WriteString("\n")
	}
	// Savings are quoted against the strong arm, so a strong arm that never
	// ran makes every one of them meaningless: an arm that spent nothing
	// because it errored would show as a 100% saving.
	if baseline != nil && !baseline.Measured() {
		sb.WriteString("\nno comparison: the strong baseline never answered, " +
			"so any saving against it would be a saving against a broken arm.\n")
	} else if baseline != nil && baseline.CostUSD > 0 {
		sb.WriteString("\nversus the strong-model baseline:\n")
		for _, a := range r.Arms {
			if a.Arm == "strong" || !a.Measured() {
				continue
			}
			saving := (1 - a.CostUSD/baseline.CostUSD) * 100
			quality := (a.Quality() - baseline.Quality()) * 100
			fmt.Fprintf(&sb, "  %-12s %+.0f%% cost, %+.1f pts quality", a.Arm, -saving, quality)
			if a.Escalated > 0 {
				fmt.Fprintf(&sb, " (%d escalated)", a.Escalated)
			}
			sb.WriteString("\n")
		}
	}
	// State how much of the routed number is load-bearing. A cascade gated on
	// "the model said something" is not the cascade production runs on the
	// tasks that matter, and a curve that does not say so invites being quoted
	// as though every task were gated properly.
	if len(r.Arms) > 0 && r.WeakGates > 0 {
		fmt.Fprintf(&sb, "\nrouted gating: %d of %d tasks gated as production would; %d carry their\n"+
			"answer in the check and fell back to non-empty output.\n",
			r.RealGates, r.RealGates+r.WeakGates, r.WeakGates)
	}
	return sb.String()
}

func money(f float64) string {
	if f == 0 {
		return "$0"
	}
	if f < 0.01 {
		return fmt.Sprintf("$%.5f", f)
	}
	return fmt.Sprintf("$%.4f", f)
}

// ---------------------------------------------------------------- runner ----

type Runner struct {
	Reg    *model.Registry
	Router *router.Router
	// MaxTokens is the budget every arm gets. It is one number on purpose:
	// a reasoning model spends most of a small budget thinking, so a budget
	// that fits one tier's answer and not another's measures the budget rather
	// than the models. At 512 the cheap tier ran out mid-thought on two of
	// eight tasks and scored them as failures.
	MaxTokens int
}

// DefaultMaxTokens is generous enough that no tier here has been observed to
// run out, which is the only property that matters: an arm that dies on length
// is not a worse model, it is an unmeasured one.
const DefaultMaxTokens = 2048

// Run executes every task on every arm. Arms are the tiers that are actually
// configured, plus "routed" for the cascade, so the comparison is between real
// options rather than hypothetical ones.
func (r *Runner) Run(ctx context.Context, tasks []Task) (Report, error) {
	var rep Report
	for _, t := range tasks {
		if t.RealGate() {
			rep.RealGates++
		} else {
			rep.WeakGates++
		}
	}
	arms := []string{}
	for _, t := range r.Reg.Tiers() {
		arms = append(arms, t.String())
	}
	if r.Router != nil {
		arms = append(arms, "routed")
	}

	for _, arm := range arms {
		sum := ArmSummary{Arm: arm}
		var lats []time.Duration

		for _, task := range tasks {
			budget := r.MaxTokens
			if budget <= 0 {
				budget = DefaultMaxTokens
			}
			req := model.Request{System: task.System, Prompt: task.Prompt, MaxTokens: budget}
			res := Result{TaskID: task.ID, Arm: arm}
			start := time.Now()

			var answer string
			var costUSD float64
			var err error

			if arm == "routed" {
				// Gate(), never Verify(): the cascade must decide without the
				// answer key, exactly as it does in production.
				resp, dec, rErr := r.Router.Call(ctx, req, task.Gate())
				answer, costUSD, err = resp.Text, dec.TotalCost(), rErr
				if dec.Escalated {
					sum.Escalated++
				}
			} else {
				tier, tErr := model.ParseTier(arm)
				if tErr != nil {
					return rep, tErr
				}
				p, ok := r.Reg.Get(tier)
				if !ok {
					continue
				}
				var resp model.Response
				resp, err = p.Complete(ctx, req)
				answer, costUSD = resp.Text, resp.Usage.CostUSD
			}

			res.Latency = time.Since(start)
			res.CostUSD = costUSD
			lats = append(lats, res.Latency)

			switch {
			case err != nil:
				res.Detail = "error: " + err.Error()
				sum.Errored++
				if sum.ErrDetail == "" {
					sum.ErrDetail = err.Error()
				}
			default:
				if vErr := task.Verify(answer); vErr != nil {
					res.Detail = vErr.Error()
				} else {
					res.Passed = true
				}
			}

			sum.Total++
			sum.CostUSD += res.CostUSD
			if res.Passed {
				sum.Passed++
			}
			rep.Results = append(rep.Results, res)
		}

		sum.Latency = p50(lats)
		if sum.Total > 0 {
			rep.Arms = append(rep.Arms, sum)
		}
	}
	return rep, nil
}

func p50(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}
