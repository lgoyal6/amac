package daemon

// The third source of sessions: the process table.
//
// ACP covers what amac started and tmux covers what was started in tmux. A
// Claude desktop app session, or a bare `claude` in a terminal tab, is neither,
// and on a Mac with no tmux server the board read "0 sessions" while seven of
// them were running. These are listed so the count is true. They are read
// only: amac can see them, not drive them, and the API says so rather than
// pretending a prompt went somewhere.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/procs"
)

// procIDPrefix marks a session id that names a process rather than anything
// amac or tmux owns. The pid is the only handle the process table offers, and
// the prefix is what lets refuseObserved recognise one without a lookup.
const procIDPrefix = "pid-"

// procSessions lists the agent processes that neither ACP nor tmux accounts
// for. The ACP adapters run a claude process each, so a process whose
// transcript id matches a managed session is that session and is dropped.
func (s *Server) procSessions(ctx context.Context) []sessionView {
	list, err := procs.List()
	if err != nil {
		// Recorded for the same reason tmuxSessions records its failure: an
		// unreadable process table and an empty machine look the same on
		// screen, and only one of them is fine.
		if ev, e := event.New(event.KindDaemon, "daemon", "", map[string]any{
			"op": "ps.list", "error": err.Error(),
		}); e == nil {
			_, _ = s.log.Append(ctx, ev)
		}
		return nil
	}
	known := map[string]bool{}
	for _, sess := range s.sup.List() {
		known[sess.ACPID] = true
	}
	return procViews(list, known, time.Now())
}

// procViews turns process records into cards, dropping the ones another source
// already shows: a managed session by its transcript id, a tmux session by
// ancestry. Pure, so the dedupe can be tested without a supervisor.
func procViews(list []procs.Session, known map[string]bool, now time.Time) []sessionView {
	var out []sessionView
	for _, p := range list {
		if p.InTmux || (p.ResumeID != "" && known[p.ResumeID]) {
			continue
		}
		v := sessionView{
			ID: procIDPrefix + strconv.Itoa(p.PID), Agent: p.Agent, Dir: p.Dir, Kind: p.Kind,
			PID: p.PID, Model: p.Model, State: p.State(now),
			Started: now.Add(-p.Elapsed), Since: when(p.LastActivity),
			ContinueCommand: resumeCommand(p.Agent, p.ResumeID),
			Detail:          procDetail(p),
		}
		// dispatch.mjs builds a hackathon by spawning claude -p in the project
		// directory. On the board that is a session like any other, named for
		// what it is building rather than for its pid.
		if slug := hackqueueSlug(p.Dir); slug != "" {
			v.Name = "hackqueue build · " + slug
			v.Detail = "claude -p, building in " + p.Dir
		}
		out = append(out, v)
	}
	return out
}

// procDetail is the one line under the name. For a terminal it is the command
// as typed, which is what a tmux card shows too; for a desktop app the flags
// are the app's plumbing and the model is the part worth reading.
func procDetail(p procs.Session) string {
	if p.Kind == "desktop" {
		if p.Model != "" {
			return "desktop app, " + p.Model
		}
		return "desktop app"
	}
	if len(p.Command) > 80 {
		return p.Command[:77] + "..."
	}
	return p.Command
}

// refuseObserved turns away any action aimed at a session that only the
// process table knows about.
//
// Without it a stale card's Prompt would fall through to the supervisor, miss,
// and come back "no such session", which is false: the session is right there
// on the board. The true answer is that amac is looking at it through ps and
// has no channel into it. Reads are left alone, so a GET still answers
// whatever it can.
func (s *Server) refuseObserved(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/sessions/") &&
			strings.HasPrefix(r.PathValue("id"), procIDPrefix) {
			writeJSON(w, 400, map[string]string{
				"error": "this session was found in the process table; amac can see it but has no way to drive it",
			})
			return
		}
		next(w, r)
	}
}
