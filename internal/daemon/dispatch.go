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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/account"
	"github.com/lgoyal6/amac/internal/attention"
	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	macHandoff "github.com/lgoyal6/amac/internal/handoff"
	"github.com/lgoyal6/amac/internal/health"
	"github.com/lgoyal6/amac/internal/spend"
)

const handoffPageHTML = `<!doctype html><meta name="viewport" content="width=device-width"><title>amac handoff</title>
<style>body{font:16px system-ui;background:#111318;color:#e8eaf0;display:grid;place-items:center;min-height:90vh}main{max-width:28rem;padding:2rem}p{color:#aeb4c0}</style>
<main><h1>Opening on your Mac…</h1><p id="status">Sending this session to Terminal.</p></main>
<script>fetch(location.pathname+location.search,{method:'POST'}).then(async r=>{const d=await r.json();document.querySelector('h1').textContent=r.ok?'Opened in Terminal':'Could not open';document.querySelector('#status').textContent=r.ok?'You can put your phone away and continue on the Mac.':(d.error||'The handoff failed.');}).catch(()=>document.querySelector('#status').textContent='The Mac could not be reached.');</script>`

// handoffPage is intentionally inert HTML. Link scanners and Discord previews
// may GET a URL; only a real browser executing the page's script performs the
// signed POST that opens Terminal.
func (s *Server) handoffPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(handoffPageHTML))
}

func (s *Server) signedHandoff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !macHandoff.Valid(s.token, q.Get("session"), q.Get("expires"), q.Get("sig"), time.Now()) {
		writeJSON(w, 401, map[string]string{"error": "handoff link is invalid or expired"})
		return
	}
	s.openSessionOnMac(w, r, q.Get("session"))
}

// openOnMac moves a phone-started workflow to a real local terminal. A fresh
// Terminal window is intentional: it is the only reliable way to put the
// requested tmux client in front without guessing which existing tab belongs
// to which tty.
func (s *Server) openOnMac(w http.ResponseWriter, r *http.Request) {
	s.openSessionOnMac(w, r, r.PathValue("id"))
}

func (s *Server) openSessionOnMac(w http.ResponseWriter, r *http.Request, id string) {
	if runtime.GOOS != "darwin" {
		writeJSON(w, 409, map[string]string{"error": "opening a terminal is only supported on macOS"})
		return
	}
	name, err := s.attachableSession(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	before := attachedClients(name)
	script := terminalHandoffScript(name)
	if err := exec.CommandContext(r.Context(), "osascript", "-e", script).Run(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "open Terminal: " + err.Error()})
		return
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if attachedClients(name) > before {
			_, _ = attention.RecordState(r.Context(), s.log, attention.State{
				Session: id, Agent: "amac", State: "idle", Detail: "continued in Terminal",
			})
			writeJSON(w, 200, map[string]string{"status": "opened", "session": name})
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	writeJSON(w, 500, map[string]string{"error": "Terminal opened but did not attach the session"})
}

func (s *Server) attachableSession(ctx context.Context, id string) (string, error) {
	if crew.Exists(id) {
		return id, nil
	}
	sess, ok := s.sup.Get(id)
	if !ok {
		return "", fmt.Errorf("session no longer exists")
	}
	command := resumeCommand(sess.Agent, sess.ACPID)
	if command == "" {
		return "", fmt.Errorf("%s sessions cannot be resumed in a terminal", sess.Agent)
	}
	name := "am-" + crew.Slug(id)
	if crew.Exists(name) {
		return name, nil
	}
	if err := exec.CommandContext(ctx, "tmux", "new-session", "-d", "-s", name, "-c", sess.Dir).Run(); err != nil {
		return "", fmt.Errorf("create terminal session: %w", err)
	}
	// The tmux destination exists before the managed client is closed, so a
	// failure cannot strand the user with neither control surface available.
	sess.Close()
	if err := exec.CommandContext(ctx, "tmux", "send-keys", "-t", name+":", command, "Enter").Run(); err != nil {
		_ = exec.Command("tmux", "kill-session", "-t", "="+name).Run()
		return "", fmt.Errorf("resume in terminal: %w", err)
	}
	return name, nil
}

func attachedClients(name string) int {
	// display-message takes a pane target, where the = exact-session prefix is
	// not valid (unlike has-session/attach/kill). Name plus ':' identifies its
	// active pane and exposes the owning session's attached-client count.
	out, err := exec.Command("tmux", "display-message", "-p", "-t", name+":", "#{session_attached}").Output()
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func terminalHandoffScript(name string) string {
	// name has already been resolved as an existing tmux session. Quoting it
	// here also keeps AppleScript and the shell boundaries explicit.
	command := "tmux attach -t " + shellSingleQuote("="+name)
	return `tell application "Terminal"` + "\n" +
		`activate` + "\n" +
		`do script ` + strconv.Quote(command) + "\n" +
		`end tell`
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
	a, ok := health.Find(s.log, r.PathValue("name"))
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
	MonthlyUSD          string               `json:"monthlyUsd"`
	AgentUSD            string               `json:"agentUsd"`
	TodayUSD            string               `json:"todayUsd"`
	Days                int                  `json:"days"`
	Source              string               `json:"source"`
	MessageCount        int                  `json:"messageCount"`
	LedgerSize          int                  `json:"ledgerSize"`
	RecoveredFromLedger int                  `json:"recoveredFromLedger"`
	HiddenOutOfScope    int                  `json:"hiddenOutOfScope"`
	Counts              spend.Counts         `json:"counts"`
	Alerts              []spend.Alert        `json:"alerts"`
	Services            []spendServiceView   `json:"services"`
	Events              []spendEventView     `json:"events"`
	Providers           []spendProviderView  `json:"providers"`
	NoAPI               []spend.NoAPI        `json:"noApi"`
	ByProject           []spend.Slice        `json:"byProject"`
	ByModel             []spend.Slice        `json:"byModel"`
	ByAccount           []spend.AccountSlice `json:"byAccount"`
	At                  time.Time            `json:"at"`
	Stale               bool                 `json:"stale"`
	// Caveat travels with the number. looseapi is careful to call the agent
	// figure an equivalent API cost rather than spend, because on a flat
	// subscription those tokens cost nothing marginal. Restating it here keeps
	// the board from quietly upgrading it into money.
	Caveat string `json:"caveat"`
}

type spendServiceView struct {
	spend.Service
	TotalUSD            string `json:"totalUsd"`
	LastAmountUSD       string `json:"lastAmountUsd,omitempty"`
	CreditsRemainingUSD string `json:"creditsRemainingUsd,omitempty"`
}

type spendEventView struct {
	spend.Event
	AmountUSD           string `json:"amountUsd,omitempty"`
	CreditsRemainingUSD string `json:"creditsRemainingUsd,omitempty"`
}

type spendProviderView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Cents      *int64 `json:"cents"`
	USD        string `json:"usd,omitempty"`
	PeriodDays int    `json:"periodDays,omitempty"`
	Note       string `json:"note,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	snap, err := spend.Read()
	if err != nil {
		writeJSON(w, 200, map[string]any{
			"error": "no snapshot yet: run `node ~/looseapi/bin/spend.mjs`",
		})
		return
	}
	services := make([]spendServiceView, 0, len(snap.Services()))
	for _, service := range snap.Services() {
		v := spendServiceView{Service: service, TotalUSD: spend.USD(service.TotalCents)}
		if service.LastAmountCents != nil {
			v.LastAmountUSD = spend.USD(*service.LastAmountCents)
		}
		if service.CreditsRemainingCents != nil {
			v.CreditsRemainingUSD = spend.USD(*service.CreditsRemainingCents)
		}
		services = append(services, v)
	}
	events := make([]spendEventView, 0, len(snap.Events))
	for _, event := range snap.Events {
		v := spendEventView{Event: event}
		if event.AmountCents != nil {
			v.AmountUSD = spend.USD(*event.AmountCents)
		}
		if event.CreditsRemainingCents != nil {
			v.CreditsRemainingUSD = spend.USD(*event.CreditsRemainingCents)
		}
		events = append(events, v)
	}
	providers := make([]spendProviderView, 0, len(snap.Providers))
	for _, provider := range snap.Providers {
		v := spendProviderView{
			ID: provider.ID, Name: provider.Name, Status: provider.Status,
			Cents: provider.Cents, PeriodDays: provider.PeriodDays, Note: provider.Note,
		}
		if provider.Cents != nil {
			v.USD = spend.USD(*provider.Cents)
		}
		if provider.Status == "error" {
			// Provider responses can contain account metadata. LooseAPI's own
			// logs retain the detail; AMAC only needs the actionable state.
			v.Error = "provider check failed; see LooseAPI logs"
		}
		providers = append(providers, v)
	}

	writeJSON(w, 200, spendView{
		MonthlyUSD:          spend.USD(snap.MonthlyCents),
		AgentUSD:            spend.USD(snap.AgentCents()),
		TodayUSD:            spend.USD(snap.TodayCents(time.Now())),
		Days:                snap.Usage.Days,
		Source:              snap.Source,
		MessageCount:        snap.MessageCount,
		LedgerSize:          snap.LedgerSize,
		RecoveredFromLedger: snap.RecoveredFromLedger,
		HiddenOutOfScope:    snap.HiddenOutOfScope,
		Counts:              snap.Counts(),
		Alerts:              snap.Worst(len(snap.Alerts)),
		Services:            services,
		Events:              events,
		Providers:           providers,
		NoAPI:               append([]spend.NoAPI(nil), snap.NoAPI...),
		// Five each. The interesting thing about a breakdown on a phone is
		// which two or three lines dominate, and a list long enough to scroll
		// buries that under the ones that do not.
		ByProject: snap.Breakdown("project", 5),
		ByModel:   snap.Breakdown("model", 5),
		// Not five. There are four logins and one of them was invisible until
		// it was looked for, so the account table shows all of them however
		// small: the row worth seeing is the one nobody remembered.
		ByAccount: snap.Accounts(account.All()),
		At:        snap.GeneratedAt,
		// The scan is daily, so a reading past two days has stopped tracking
		// reality and the page has to say so rather than show it as current.
		Stale:  snap.Age() > 48*time.Hour,
		Caveat: "agent figures are equivalent API cost, not spend: on a flat subscription these tokens cost nothing marginal",
	})
}

// beat takes a heartbeat from a job amac cannot reach.
//
// Every other automation here is watched by reading what it leaves behind,
// which is stronger: an artifact only exists once work landed, and a ping can
// be sent by a job that did nothing. It also limits amac to what it can reach,
// which is this Mac and a GitHub repo. A cron job on a VPS or a pipeline on
// someone else's runner has no artifact within reach and was therefore
// invisible.
//
// A bare POST with no body is a valid beat, because the common case is a job
// adding one line to a script and the shape of that line should be `curl -X
// POST` and nothing else.
func (s *Server) beat(w http.ResponseWriter, r *http.Request) {
	b := health.Beat{Name: r.PathValue("name")}
	// An empty or unparseable body is not an error. A job reporting in is more
	// useful than a job whose reporting line broke on a JSON typo, and the
	// name in the path is the only field that matters.
	_ = json.NewDecoder(r.Body).Decode(&b)
	b.Name = r.PathValue("name")

	if err := health.Record(r.Context(), s.log, b); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"recorded": b.Name})
}
