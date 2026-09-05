// Package orchestrator turns one prompt into the right amount of work.
//
// The "C-suite" idea, made concrete and unglamorous: roles are
// prompt+model+tool configurations, not personalities. Giving an agent a job
// title does not make it better at the job; giving it a narrow brief, the
// right model, and a budget does.
//
// The load-bearing part is triage. A team of five specialists convened to
// rename a variable is worse than one agent: slower, five times the cost, and
// more places to go wrong. So every prompt is graded first, and most work
// never convenes a team at all.
package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/model"
	"github.com/lgoyal6/amac/internal/router"
	"github.com/lgoyal6/amac/internal/supervisor"
)

// Size is the outcome of triage.
type Size string

const (
	SizeSolo Size = "solo" // one agent, no ceremony
	SizePair Size = "pair" // implement then review
	SizeTeam Size = "team" // plan, implement, verify, review
)

// Role is a job on the org chart.
type Role struct {
	Name string
	// Brief is prepended to the task. Narrow beats grand: "find defects you
	// can point at in a specific file" produces better output than "you are a
	// world-class senior engineer".
	Brief string
	Agent string // which agent CLI runs it
	// Share is this role's slice of the task budget.
	Share float64
}

var (
	planner = Role{Name: "planner", Agent: "claude", Share: 0.15,
		Brief: "Read the code before proposing anything. Produce a plan naming the exact files to touch, the command that proves it works, and the single most likely way this goes wrong. Write no code."}

	executor = Role{Name: "executor", Agent: "claude", Share: 0.45,
		Brief: "Implement the plan below. Match the style of the surrounding code. Do not commit. Report anything in the plan that turned out to be wrong."}

	verifier = Role{Name: "verifier", Agent: "codex", Share: 0.15,
		Brief: "Run the build and tests and report honestly whether they pass. Do not fix anything, do not judge the code. Paste the failing output if it failed."}

	reviewer = Role{Name: "reviewer", Agent: "codex", Share: 0.25,
		Brief: "Review the uncommitted diff. Report only defects you can point at in a specific file, with the concrete input or state that breaks them. No style opinions, no praise. An empty list is a valid answer."}
)

// Org returns the roles for a size. The verifier deliberately runs on a
// different agent from the executor: an agent checking its own work shares its
// blind spots, and cross-agent verification is the cheapest diversity
// available once you are already vendor-neutral.
func Org(s Size) []Role {
	switch s {
	case SizeSolo:
		return []Role{executor}
	case SizePair:
		return []Role{executor, reviewer}
	default:
		return []Role{planner, executor, verifier, reviewer}
	}
}

// Budget is the CFO's instrument. It is a real ceiling: when it is exhausted,
// remaining roles are skipped and the run reports what it dropped rather than
// quietly overspending.
type Budget struct {
	TotalUSD float64
	spent    float64
}

func (b *Budget) Spend(v float64) { b.spent += v }
func (b *Budget) Spent() float64  { return b.spent }
func (b *Budget) Remaining() float64 {
	r := b.TotalUSD - b.spent
	if r < 0 {
		return 0
	}
	return r
}

type Orchestrator struct {
	sup    *supervisor.Supervisor
	router *router.Router
	log    *event.Log

	// Approve answers permission requests for orchestrated sessions. It must
	// be set: nobody is watching these sessions, so without a policy the first
	// tool call parks forever and the whole run deadlocks.
	Approve func(*supervisor.Pending) (string, bool)
}

func New(sup *supervisor.Supervisor, r *router.Router, log *event.Log) *Orchestrator {
	// Default to the narrowest affirmative option. `amac do` is an explicit
	// instruction to carry out work, so blocking on approvals nobody will see
	// would make it useless; granting standing permission would be worse.
	return &Orchestrator{sup: sup, router: r, log: log, Approve: supervisor.LeastPermissiveAllow}
}

// Triage grades a prompt. It asks the cheap tier through the router, which is
// the CFO doing its own job on itself: the decision about how much to spend is
// the last place that should be expensive.
//
// It falls back to heuristics when no model is reachable, because a triage
// step that can fail is a triage step that blocks all work.
func (o *Orchestrator) Triage(ctx context.Context, task string) (Size, string) {
	if o.router != nil {
		req := model.Request{
			System: "You size software tasks. Answer with exactly one word: solo, pair, or team.",
			Prompt: fmt.Sprintf(`Size this task.

solo = a single mechanical change: rename, typo, add a flag, small edit
pair = a real change worth a second pair of eyes
team = touches several files or systems, or a mistake is expensive

Task: %s

Answer with one word.`, task),
			// Eight is what one word costs and it is not what to ask for. A
			// reasoning model spends its budget thinking before it answers, so
			// a budget sized to the answer buys reasoning and no answer: the
			// call is billed, the text is empty, and triage falls back to the
			// heuristic every time while paying for it. Measured against GMI's
			// cheap tier, which reasons for about twenty tokens before saying
			// one word.
			MaxTokens: 256,
		}
		resp, _, err := o.router.Call(ctx, req, router.OneOfVerifier("solo", "pair", "team"))
		if err == nil {
			word := strings.ToLower(strings.Trim(strings.TrimSpace(resp.Text), ".\"'`"))
			switch Size(word) {
			case SizeSolo, SizePairAlias(word), SizeTeam:
				return Size(word), "graded by " + resp.Model
			}
		}
	}
	return heuristicSize(task), "heuristic (no model available)"
}

// SizePairAlias exists only so the switch above reads as a list of valid
// sizes; Go does not allow a bare constant twice in one case.
func SizePairAlias(s string) Size { return SizePair }

func heuristicSize(task string) Size {
	t := strings.ToLower(task)
	long := len(task) > 400
	hard := strings.ContainsAny(t, "\n") && long
	for _, w := range []string{"refactor", "migrate", "redesign", "architecture", "security", "concurren", "performance"} {
		if strings.Contains(t, w) {
			return SizeTeam
		}
	}
	for _, w := range []string{"rename", "typo", "comment", "bump", "format", "spelling"} {
		if strings.Contains(t, w) {
			return SizeSolo
		}
	}
	if hard {
		return SizeTeam
	}
	if long {
		return SizePair
	}
	return SizePair
}

// -------------------------------------------------------------- attachable ---

// Attachable lays out the same org as Execute, but as sessions a human can
// take over instead of subprocesses only amac can talk to.
//
// The shape of the work is unchanged: still a chain, because the executor
// needs the plan and the reviewer needs the diff. What changes is where the
// handoff lives. Execute passes a role's output to the next in memory; here it
// goes through a file, because the alternative is reading it back off a
// rendered terminal.
//
// This returns the plan rather than starting anything. Deciding what to open
// and opening it are worth keeping apart: the caller can print the chain, and
// a run that is going to fail on the third role should say so before the first
// one has burned any tokens.
func (o *Orchestrator) Attachable(task, dir string, size Size) []crew.Session {
	slug := crew.Slug(task)
	runDir := crew.RunDir(slug)

	roles := Org(size)
	out := make([]crew.Session, 0, len(roles))
	var prev string
	for _, r := range roles {
		s := crew.Session{
			Name:   crew.Name(slug, r.Name),
			Role:   r.Name,
			Agent:  r.Agent,
			Dir:    dir,
			Input:  prev,
			Output: filepath.Join(runDir, r.Name+".md"),
		}
		prev = s.Output
		out = append(out, s)
	}
	return out
}

// BriefFor is the instruction a given role's session opens with.
func BriefFor(s crew.Session, task string) string {
	for _, r := range Org(SizeTeam) {
		if r.Name == s.Role {
			return crew.Brief(s, r.Brief, task)
		}
	}
	return crew.Brief(s, "", task)
}

// ---------------------------------------------------------------- running ---

type RoleResult struct {
	Role    string
	Agent   string
	Session string
	Output  string
	Skipped string
	Elapsed time.Duration
}

type Run struct {
	Task    string
	Size    Size
	Reason  string
	Budget  float64
	Results []RoleResult
	Elapsed time.Duration
}

// Execute runs the org for a task, in order, threading each role's output into
// the next. Sequential rather than parallel on purpose: the executor needs the
// plan, and the reviewer needs the diff, so there is a genuine dependency
// chain. Parallelism belongs across independent tasks, not inside one.
// forced, when non-empty, skips triage. The caller having already decided is
// the one case where grading the prompt is wasted money.
func (o *Orchestrator) Execute(ctx context.Context, task, dir string, budgetUSD float64, forced Size) (Run, error) {
	start := time.Now()

	var size Size
	var reason string
	if forced != "" {
		size, reason = forced, "forced by caller"
	} else {
		size, reason = o.Triage(ctx, task)
	}
	run := Run{Task: task, Size: size, Reason: reason, Budget: budgetUSD}

	o.record(event.KindRouteDecided, map[string]any{
		"stage": "triage", "size": string(size), "reason": reason, "budget": budgetUSD,
	})

	budget := &Budget{TotalUSD: budgetUSD}
	var carry string

	for _, role := range Org(size) {
		if budgetUSD > 0 && budget.Remaining() <= 0 {
			run.Results = append(run.Results, RoleResult{
				Role: role.Name, Skipped: "budget exhausted",
			})
			continue
		}

		prompt := role.Brief + "\n\nTask: " + task
		if carry != "" {
			prompt += "\n\nFrom the previous step:\n" + carry
		}

		roleStart := time.Now()
		sess, err := o.sup.Start(ctx, role.Agent, dir)
		if err != nil {
			run.Results = append(run.Results, RoleResult{
				Role: role.Name, Agent: role.Agent, Skipped: "start failed: " + err.Error(),
			})
			continue
		}
		// Install the policy before prompting. A permission request can arrive
		// on the very first tool call, and a session without a policy has
		// nobody to answer it.
		sess.OnPermission = o.Approve

		_, err = sess.Prompt(ctx, prompt)
		res := RoleResult{
			Role: role.Name, Agent: role.Agent, Session: sess.ID,
			Elapsed: time.Since(roleStart),
		}
		if err != nil {
			res.Skipped = "prompt failed: " + err.Error()
		} else {
			// The agent's own output is the handoff. Reading it back off the
			// event log rather than the wire keeps the log authoritative:
			// anything the next role sees, replay can see too.
			res.Output = o.lastText(ctx, sess.ID)
			carry = truncate(res.Output, 4000)
		}
		sess.Close()

		budget.Spend(o.sessionCost(ctx, sess.ID))
		run.Results = append(run.Results, res)
	}

	run.Elapsed = time.Since(start)
	o.record(event.KindRouteDecided, map[string]any{
		"stage": "org_complete", "size": string(size),
		"roles": len(run.Results), "spent": budget.Spent(), "elapsed_ms": run.Elapsed.Milliseconds(),
	})
	return run, nil
}

// lastText reassembles the agent's final message from the streamed chunks
// already in the log.
func (o *Orchestrator) lastText(ctx context.Context, session string) string {
	if o.log == nil {
		return ""
	}
	rows, err := o.log.DB().QueryContext(ctx, `
		SELECT json_extract(payload,'$.text')
		FROM events
		WHERE session = ? AND json_extract(payload,'$.update') = 'agent_message_chunk'
		ORDER BY seq`, session)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var s *string
		if rows.Scan(&s) == nil && s != nil {
			sb.WriteString(*s)
		}
	}
	return strings.TrimSpace(sb.String())
}

func (o *Orchestrator) sessionCost(ctx context.Context, session string) float64 {
	if o.log == nil {
		return 0
	}
	var c *float64
	err := o.log.DB().QueryRowContext(ctx, `
		SELECT MAX(json_extract(payload,'$.raw.cost.amount'))
		FROM events WHERE session = ?`, session).Scan(&c)
	if err != nil || c == nil {
		return 0
	}
	return *c
}

func (o *Orchestrator) record(kind event.Kind, payload any) {
	if o.log == nil {
		return
	}
	if ev, err := event.New(kind, "orchestrator", "", payload); err == nil {
		_, _ = o.log.Append(context.Background(), ev)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
