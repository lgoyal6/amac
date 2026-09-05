package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/account"
	"github.com/lgoyal6/amac/internal/crew"
	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
	"github.com/lgoyal6/amac/internal/holds"
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
	hold, err := holds.Open(log)
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
			// Presence and exclusion are different answers and are reported
			// as such. Sessions in the tree is a hint; a hold is a fact about
			// a specific file that somebody took and can lose by expiry.
			var lines []string
			if len(here) > 0 {
				lines = append(lines, fmt.Sprintf("%d other agent session(s) are in %s:", len(here), dir),
					strings.Join(here, "\n"),
					"That is presence, not proof they are touching the same files.")
			}
			held, err := hold.Who(ctx, dir)
			if err == nil {
				var others []holds.Hold
				for _, x := range held {
					if x.Owner != self {
						others = append(others, x)
					}
				}
				if len(others) > 0 {
					lines = append(lines, fmt.Sprintf("\n%d path(s) here are CLAIMED right now:", len(others)))
					for _, x := range others {
						lines = append(lines, fmt.Sprintf("  %s held by %s%s, expires in %s",
							x.Path, x.Owner, note(x.Note), time.Until(x.Lease).Round(time.Second)))
					}
					lines = append(lines, "\nDo not edit those paths. Claim what you need with claim_files "+
						"so the next agent is told the same thing about you.")
				}
			}
			if len(lines) == 0 {
				return "No other agent session is working in " + dir + ", and no path here is claimed. " +
					"Safe to proceed. Call claim_files on what you are about to edit.", nil
			}
			return strings.Join(lines, "\n"), nil
		},
	}, {
		Name: "claim_files",
		Description: "Claim the files you are about to edit, so another agent is refused them " +
			"while you work. Call this before your first edit, with absolute paths. A claim is " +
			"all or nothing: if any path is already held you get none of them and are told who " +
			"holds what, so say what you found rather than editing anyway. Claims expire on " +
			"their own, so an agent that dies does not lock a file forever. Release them when " +
			"you are done.",
		InputSchema: mcp.Schema(map[string]string{
			"paths":   "absolute paths you are about to edit, comma separated",
			"note":    "what you are doing to them, shown to whoever is refused",
			"minutes": "how long you need them, default 30",
		}, "paths"),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			paths := splitPaths(arg(raw, "paths"))
			if len(paths) == 0 {
				return "", fmt.Errorf("paths is required")
			}
			lease := 30 * time.Minute
			if m := arg(raw, "minutes"); m != "" {
				if n, err := strconv.Atoi(m); err == nil && n > 0 && n <= 480 {
					lease = time.Duration(n) * time.Minute
				}
			}
			owner := callerSession()
			if owner == "" {
				owner = "unknown-session"
			}
			got, err := hold.Claim(ctx, owner, paths, lease, arg(raw, "note"))
			if errors.Is(err, holds.ErrHeld) {
				var b strings.Builder
				fmt.Fprintf(&b, "REFUSED. %d path(s) are held by another session, so none of your %d were claimed:\n",
					len(got), len(paths))
				for _, x := range got {
					fmt.Fprintf(&b, "  %s held by %s%s, expires in %s\n",
						x.Path, x.Owner, note(x.Note), time.Until(x.Lease).Round(time.Second))
				}
				b.WriteString("\nDo not edit them anyway. Work on something else, or tell the human who holds what.")
				return b.String(), nil
			}
			if err != nil {
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Claimed %d path(s) as %s for %s, fencing token %d:\n",
				len(got), owner, lease, got[0].Token)
			for _, x := range got {
				fmt.Fprintf(&b, "  %s\n", x.Path)
			}
			b.WriteString("\nRelease them with release_files when you are done. " +
				"They expire on their own if this session dies.")
			return b.String(), nil
		},
	}, {
		Name: "release_files",
		Description: "Release file claims you took with claim_files, so another agent can have " +
			"them. Call this as soon as you stop editing, rather than waiting for the claim to " +
			"expire. Omit paths to release everything this session holds.",
		InputSchema: mcp.Schema(map[string]string{
			"paths": "paths to release, comma separated, or omit for all of them",
			"token": "the fencing token claim_files gave you, required when naming paths",
		}),
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			owner := callerSession()
			if owner == "" {
				owner = "unknown-session"
			}
			paths := splitPaths(arg(raw, "paths"))
			if len(paths) == 0 {
				n, err := hold.ReleaseAll(ctx, owner)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Released all %d path(s) held by %s.", n, owner), nil
			}
			token, _ := strconv.ParseInt(arg(raw, "token"), 10, 64)
			if err := hold.Release(ctx, owner, token, paths); err != nil {
				return fmt.Sprintf("Nothing released: %v. If your claim expired, another session may hold these now.", err), nil
			}
			return fmt.Sprintf("Released %d path(s).", len(paths)), nil
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

// splitPaths accepts the comma-separated list the schema asks for, and also
// tolerates newlines, because an agent filling in a free-text field will use
// whichever separator its own output happened to produce.
func splitPaths(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// note renders a claim's reason inline, or nothing when the agent did not say.
func note(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return " (" + s + ")"
}
