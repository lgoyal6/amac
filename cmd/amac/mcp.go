package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/account"
	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
	"github.com/lgoyal6/amac/internal/mcp"
	"github.com/lgoyal6/amac/internal/queue"
	"github.com/lgoyal6/amac/internal/spend"
	"github.com/lgoyal6/amac/internal/tmux"
)

// cmdMCP serves amac to the agents themselves.
//
// The tool descriptions below are the actual interface. An agent decides
// whether to call something by reading them, so they are written as advice
// about when the answer matters rather than as a summary of what the function
// returns. "Returns session states" tells an agent nothing about when to ask.
func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()
	q, err := queue.Open(log)
	if err != nil {
		return err
	}

	arg := func(raw json.RawMessage, key string) string {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return ""
		}
		s, _ := m[key].(string)
		return strings.TrimSpace(s)
	}

	tools := []mcp.Tool{{
		Name: "working_here",
		Description: "Check whether another agent session is already working in a directory, " +
			"before you start editing it. Two agents in one tree produce conflicting edits " +
			"and a diff neither of them can explain. Call this at the start of any task that " +
			"will change files, with the absolute path you are about to work in.",
		InputSchema: mcp.Schema(map[string]string{
			"dir": "absolute path you are about to work in",
		}, "dir"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			dir := arg(raw, "dir")
			if dir == "" {
				return "", fmt.Errorf("dir is required")
			}
			list, err := tmux.List()
			if err != nil {
				return "", err
			}
			states := attentionStates(ctx, log)
			// The asking agent is not another agent. This server runs as a
			// subprocess of the session that called it and inherits its
			// TMUX_PANE, so it can tell. Without this the answer to "is anyone
			// else in here" always includes the one asking, which is both
			// wrong and the kind of wrong that stops an agent from working.
			self := callerSession()

			var here []string
			for _, t := range list {
				if t.Name == self {
					continue
				}
				if t.Dir != dir && !strings.HasPrefix(t.Dir, dir+string(filepath.Separator)) {
					continue
				}
				if t.Agent() == "" {
					continue // a plain shell is not another agent
				}
				state := states[t.Name]
				if state == "" {
					state = "unknown"
				}
				here = append(here, fmt.Sprintf("  %s (%s, %s)", t.Name, t.Agent(), state))
			}
			if len(here) == 0 {
				return "No other agent session is working in " + dir + ". Safe to proceed.", nil
			}
			return fmt.Sprintf("%d other agent session(s) are in %s:\n%s\n\n"+
				"Coordinate before editing, or work somewhere else. This does not prove they "+
				"are touching the same files, only that they are in the same tree.",
				len(here), dir, strings.Join(here, "\n")), nil
		},
	}, {
		Name: "automation_health",
		Description: "Check whether a scheduled automation actually delivered recently, before " +
			"you trust data it produces. A pipeline can exit green and deliver nothing, and " +
			"its output can be days stale while everything looks fine. Call this before " +
			"relying on a file, feed or dataset that something else generates. Omit name to " +
			"see everything.",
		InputSchema: mcp.Schema(map[string]string{
			"name": "one automation, or omit for all of them",
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			roster, err := health.Roster(log)
			if err != nil {
				return "", err
			}
			want := arg(raw, "name")
			reports := health.Sweep(ctx, roster)

			var b strings.Builder
			for _, r := range reports {
				if want != "" && r.Name != want {
					continue
				}
				fmt.Fprintf(&b, "%-24s %-8s %s\n", r.Name, r.State, r.Detail)
				for _, n := range r.Notes {
					fmt.Fprintf(&b, "%-24s          %s\n", "", n)
				}
			}
			if b.Len() == 0 {
				return "No automation named " + want + " is declared.", nil
			}
			b.WriteString("\nunknown means the probe could not establish the truth. " +
				"It never means fine.")
			return b.String(), nil
		},
	}, {
		Name: "file_task",
		Description: "Write down work you found but are not going to do, so it reaches a human " +
			"or another agent instead of being lost when your context ends. Use it for a bug " +
			"you noticed outside your task, a follow-up your change requires, or anything you " +
			"were about to mention only in a final message. Filing the same title twice is " +
			"one task, so it is safe to call when unsure.",
		InputSchema: mcp.Schema(map[string]string{
			"title": "one line, specific enough to act on without you",
			"dir":   "absolute path the work belongs to",
		}, "title"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			title := arg(raw, "title")
			if title == "" {
				return "", fmt.Errorf("title is required")
			}
			dir := arg(raw, "dir")
			if dir == "" {
				dir, _ = os.Getwd()
			}
			t, err := q.File(ctx, queue.Task{ID: crew.Slug(title), Title: title, Dir: dir})
			if err != nil {
				return "", err
			}
			if t.Attempt > 0 || t.State != queue.Ready {
				return fmt.Sprintf("Already filed as %s and currently %s. Nothing added.", t.ID, t.State), nil
			}
			return fmt.Sprintf("Filed as %s in %s. It is on the queue for whoever picks it up.", t.ID, dir), nil
		},
	}, {
		Name: "queue",
		Description: "See what work is queued and what is already being worked on. Call this " +
			"before starting something substantial, to check that another agent has not " +
			"already claimed it.",
		InputSchema: mcp.Schema(map[string]string{}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			list, err := q.List(ctx, "")
			if err != nil {
				return "", err
			}
			if len(list) == 0 {
				return "Nothing queued.", nil
			}
			var b strings.Builder
			for _, t := range list {
				held := ""
				if t.State == queue.Claimed {
					held = " (held by " + t.Owner + ")"
				}
				fmt.Fprintf(&b, "%-9s %s%s\n", t.State, t.Title, held)
			}
			return b.String(), nil
		},
	}, {
		Name: "agent_spend",
		Description: "What the coding agents on this machine have cost recently, by account, " +
			"project and model. Useful when deciding whether a long or repeated run is worth " +
			"it, when choosing a model for a job that will run many times, or when checking " +
			"which login is carrying the load.",
		InputSchema: mcp.Schema(map[string]string{}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			snap, err := spend.Read()
			if err != nil {
				return "", fmt.Errorf("no spend snapshot yet: %w", err)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s over %d days, %s today.\n\n",
				spend.USD(snap.AgentCents()), snap.Usage.Days, spend.USD(snap.TodayCents(time.Now())))
			fmt.Fprintf(&b, "by account:\n")
			for _, a := range snap.Accounts(account.All()) {
				who := a.Email
				if !a.Present {
					who = "not on this machine"
				}
				fmt.Fprintf(&b, "  %-28s %8s  %d%%  %s\n", a.Label, spend.USD(a.Cents), a.Share, who)
			}
			fmt.Fprintf(&b, "by project:\n")
			for _, s := range snap.Breakdown("project", 5) {
				fmt.Fprintf(&b, "  %-28s %8s  %d%%\n", s.Name, spend.USD(s.Cents), s.Share)
			}
			fmt.Fprintf(&b, "by model:\n")
			for _, s := range snap.Breakdown("model", 5) {
				fmt.Fprintf(&b, "  %-28s %8s  %d%%\n", s.Name, spend.USD(s.Cents), s.Share)
			}
			b.WriteString("\nEquivalent API cost, not spend: on a flat subscription these " +
				"tokens cost nothing marginal.")
			return b.String(), nil
		},
	}, {
		Name: "report_done",
		Description: "Tell amac a recurring job you are responsible for has finished, so that " +
			"its absence is noticed if it stops. Only useful for work that runs on a schedule " +
			"and is declared in amac's roster. Say state=failing with a detail if it did not " +
			"work: silence and failure are different, and a job that fails loudly is easier " +
			"to fix than one that goes quiet.",
		InputSchema: mcp.Schema(map[string]string{
			"name":   "the automation's declared name",
			"state":  "ok, or failing",
			"detail": "what happened, when it is worth saying",
		}, "name"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			name := arg(raw, "name")
			if name == "" {
				return "", fmt.Errorf("name is required")
			}
			b := health.Beat{Name: name, State: arg(raw, "state"), Detail: arg(raw, "detail")}
			if err := health.Record(ctx, log, b); err != nil {
				return "", err
			}
			return "Recorded a heartbeat for " + name + ".", nil
		},
	}}

	return mcp.NewServer("amac", tools).Serve(context.Background(), os.Stdin, os.Stdout)
}

// attentionStates is the same join the board does, reduced to a name and a
// state. Duplicated deliberately rather than exported from the daemon: the
// daemon's version carries view fields this has no use for, and an MCP server
// importing an HTTP server to reuse a map would be the wrong direction.
func attentionStates(ctx context.Context, log *event.Log) map[string]string {
	out := map[string]string{}
	rows, err := log.DB().QueryContext(ctx, `
		SELECT session, payload FROM events
		 WHERE kind = ? AND seq IN (
		       SELECT MAX(seq) FROM events WHERE kind = ? AND session != '' GROUP BY session)`,
		string(event.KindSessionState), string(event.KindSessionState))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sess string
		var payload []byte
		if rows.Scan(&sess, &payload) != nil {
			continue
		}
		var st struct {
			State string `json:"state"`
		}
		if json.Unmarshal(payload, &st) == nil {
			out[sess] = st.State
		}
	}
	return out
}
