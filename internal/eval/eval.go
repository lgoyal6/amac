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
	// Note records why the task is in the set. A suite nobody can read is a
	// suite nobody maintains.
	Note string `json:"note,omitempty"`
}

// Verify is the mechanical check. It is also reused as the router's verifier,
// which keeps the eval honest: the harness measures the same gate production
// uses, rather than a friendlier one.
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
	Arm       string
	Passed    int
	Total     int
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

type Report struct {
	Arms    []ArmSummary
	Results []Result
}

// Table renders the cost/quality frontier. Savings are stated against the
// strong-model arm, because that is the honest baseline: the question is never
// "is cheap cheaper" but "what did choosing cheap cost me".
func (r Report) Table() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-14s %8s %10s %12s %10s\n", "ARM", "QUALITY", "COST", "COST/TASK", "P50 LAT")
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
		fmt.Fprintf(&sb, "%-14s %7.1f%% %10s %12s %10s\n",
			a.Arm, a.Quality()*100, money(a.CostUSD), money(per), a.Latency.Round(time.Millisecond))
	}
	if baseline != nil && baseline.CostUSD > 0 {
		sb.WriteString("\nversus the strong-model baseline:\n")
		for _, a := range r.Arms {
			if a.Arm == "strong" {
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
}

// Run executes every task on every arm. Arms are the tiers that are actually
// configured, plus "routed" for the cascade, so the comparison is between real
// options rather than hypothetical ones.
func (r *Runner) Run(ctx context.Context, tasks []Task) (Report, error) {
	var rep Report
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
			req := model.Request{System: task.System, Prompt: task.Prompt, MaxTokens: 512}
			res := Result{TaskID: task.ID, Arm: arm}
			start := time.Now()

			var answer string
			var costUSD float64
			var err error

			if arm == "routed" {
				resp, dec, rErr := r.Router.Call(ctx, req, func(_, a string) error { return task.Verify(a) })
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
