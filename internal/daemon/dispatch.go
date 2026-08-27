package daemon

// Doing something about a red line.
//
// The health tab could say an automation was failing and nothing more, which
// makes it a page you read on your phone and then act on somewhere else. The
// gap between noticing and acting is where things stay broken: job-discovery
// crashed three times in twenty hours and the sweep reported it green
// throughout, and even once that was fixed, knowing about it at 11pm from a
// phone was still not the same as being able to do anything.
//
// So a finding can be handed to the org. The brief is built here rather than in
// the browser, because it is an instruction to an agent and it should read the
// same whoever pressed the button, and because the whole point of the event log
// is that what an agent was told is recoverable afterwards.
//
// Where an automation lives is declared in the registry, never inferred. An
// agent sent to fix a pipeline in the wrong tree is worse than no agent.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
	"github.com/lgoyal6/amac/internal/spend"
)

// lastReport returns the newest recorded verdict for one automation.
//
// Read from the log rather than by running the probe again. A probe is a
// network call or two and the button should feel instant, but more than that:
// the agent should be told what the sweep actually saw, not what a second
// reading taken a moment later happens to say. Those differ exactly when the
// failure is intermittent, which is when the brief matters most.
func (s *Server) lastReport(ctx context.Context, name string) (health.Report, bool) {
	var payload []byte
	err := s.log.DB().QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAutomationCheck)).Scan(&payload)
	if err != nil {
		return health.Report{}, false
	}
	var body struct {
		Reports []health.Report `json:"reports"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return health.Report{}, false
	}
	for _, r := range body.Reports {
		if r.Name == name {
			return r, true
		}
	}
	return health.Report{}, false
}

// brief is what the org is told about a broken automation.
//
// It states the verdict and stops. Telling an agent what is probably wrong
// would be handing it the conclusion, and a wrong conclusion stated with
// authority is harder to recover from than no conclusion: it will spend its
// context confirming the guess instead of reading the logs.
func brief(a health.Automation, r health.Report, ok bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "amac health reports the automation %q as %s.\n\n", a.Name, r.State)
	fmt.Fprintf(&b, "What it is: %s\n", a.What)
	if ok {
		fmt.Fprintf(&b, "Verdict:    %s\n", r.Detail)
		if !r.Last.IsZero() {
			fmt.Fprintf(&b, "Last:       %s\n", r.Last.Format(time.RFC3339))
		}
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "Note:       %s\n", n)
		}
		if r.Err != "" {
			fmt.Fprintf(&b, "Probe err:  %s\n", r.Err)
		}
	}
	b.WriteString("\nFind out why and fix it. Its code and logs are in this directory. " +
		"Read the logs before changing anything, and say plainly if the verdict " +
		"turns out to be wrong rather than working around it.")
	return b.String()
}

func (s *Server) healthTarget(w http.ResponseWriter, r *http.Request) (health.Automation, bool) {
	a, ok := health.Find(r.PathValue("name"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "no such automation"})
		return a, false
	}
	if a.Home == "" {
		// Honest refusal. job-discovery runs on Railway and machine-pressure is
		// a reading, not a program: neither has a tree to open, and opening the
		// wrong one to have something to show would be worse than the button
		// being disabled.
		writeJSON(w, 409, map[string]string{"error": a.Name + " has nothing local to open"})
		return a, false
	}
	return a, true
}

// healthFix hands a finding to the org.
func (s *Server) healthFix(w http.ResponseWriter, r *http.Request) {
	a, ok := s.healthTarget(w, r)
	if !ok {
		return
	}
	rep, found := s.lastReport(r.Context(), a.Name)
	task := "fix " + a.Name

	size, reason := s.orch.Triage(r.Context(), task)
	sessions := s.orch.Attachable(task, a.Home, size)
	var opened *crew.Session
	for i := range sessions {
		if crew.Status(sessions[i]) != "ready" {
			continue
		}
		opened = &sessions[i]
		break
	}
	if opened == nil {
		writeJSON(w, 409, map[string]string{
			"error": "a run for " + a.Name + " is already open; take it over instead"})
		return
	}

	// The role's own brief carries the failure. crew.Brief adds the file
	// contract on top, so the agent gets the verdict, the task and where to
	// leave its answer in one instruction rather than three.
	if err := crew.Open(*opened, crew.Brief(*opened, brief(a, rep, found), task)); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if ev, err := event.New(event.KindSessionStarted, "crew", opened.Name, map[string]any{
		"role": opened.Role, "agent": opened.Agent, "task": task, "dir": opened.Dir,
		"input": opened.Input, "output": opened.Output,
		"size": string(size), "slug": crew.Slug(task), "automation": a.Name,
	}); err == nil {
		_, _ = s.log.Append(r.Context(), ev)
	}

	plan := s.plan(task, a.Home, string(size))
	plan.Reason = reason
	writeJSON(w, 200, plan)
}

// healthShell opens a plain terminal where the automation lives.
//
// Not everything wants an agent. Half of these are a shell script and a log
// file, and the fastest fix is often to read the log yourself. This is the
// cheaper half of the same gesture, and it opens the same kind of tmux session
// as everything else here so it shows up on the board like the rest.
func (s *Server) healthShell(w http.ResponseWriter, r *http.Request) {
	a, ok := s.healthTarget(w, r)
	if !ok {
		return
	}
	name := "am-fix-" + a.Name
	if !crew.Exists(name) {
		if err := exec.CommandContext(r.Context(), "tmux",
			"new-session", "-d", "-s", name, "-c", a.Home).Run(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "tmux: " + err.Error()})
			return
		}
		if ev, err := event.New(event.KindSessionStarted, "dashboard", name, map[string]any{
			"kind": "shell", "dir": a.Home, "automation": a.Name,
		}); err == nil {
			_, _ = s.log.Append(r.Context(), ev)
		}
	}
	writeJSON(w, 200, map[string]string{
		"session": name, "dir": a.Home, "attach": "tmux attach -t " + name,
	})
}

// ------------------------------------------------------------------ spend ---

type spendView struct {
	MonthlyUSD string        `json:"monthlyUsd"`
	AgentUSD   string        `json:"agentUsd"`
	TodayUSD   string        `json:"todayUsd"`
	Days       int           `json:"days"`
	Alerts     []spend.Alert `json:"alerts"`
	ByProject  []spend.Slice `json:"byProject"`
	ByModel    []spend.Slice `json:"byModel"`
	At         time.Time     `json:"at"`
	Stale      bool          `json:"stale"`
	// Caveat travels with the number. looseapi is careful to call the agent
	// figure an equivalent API cost rather than spend, because on a flat
	// subscription those tokens cost nothing marginal. Restating it here keeps
	// the board from quietly upgrading it into money.
	Caveat string `json:"caveat"`
}

func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	snap, err := spend.Read()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"error": "no snapshot yet: run `node ~/looseapi/bin/spend.mjs`",
		})
		return
	}
	writeJSON(w, 200, spendView{
		MonthlyUSD: spend.USD(int64(snap.MonthlyCents)),
		AgentUSD:   spend.USD(snap.AgentCents()),
		TodayUSD:   spend.USD(snap.TodayCents(time.Now())),
		Days:       snap.Usage.Days,
		Alerts:     snap.Worst(6),
		// Five each. The interesting thing about a breakdown on a phone is
		// which two or three lines dominate, and a list long enough to scroll
		// buries that under the ones that do not.
		ByProject: snap.Breakdown("project", 5),
		ByModel:   snap.Breakdown("model", 5),
		At:        snap.GeneratedAt,
		// The scan is daily, so a reading past two days has stopped tracking
		// reality and the page has to say so rather than show it as current.
		Stale:  snap.Age() > 48*time.Hour,
		Caveat: "agent figures are equivalent API cost, not spend: on a flat subscription these tokens cost nothing marginal",
	})
}
