package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/discord"
	"github.com/lgoyal6/amac/internal/event"
)

// Reasons a session wants the human. Codex's own signals are the constraint
// here: its `notify` hook fires only for agent-turn-complete, so a request for
// approval never reaches a program that way and has to arrive as a terminal
// bell, which carries no reason with it.
const (
	TurnComplete = "turn-complete"
	WantsYou     = "wants-attention"
)

type Notice struct {
	Session string // tmux session, e.g. am-mint
	Agent   string // codex, claude
	Reason  string
	Message string // last assistant message, when the signal carried one
}

type Outcome struct {
	Sent bool   `json:"sent"`
	Why  string `json:"why,omitempty"` // why it was held back
}

// DedupeWindow collapses the several signals one event can produce. A finished
// Codex turn rings the terminal bell and calls the notify hook within the same
// second, and those must not be two messages.
const DedupeWindow = 45 * time.Second

// watching is indirected so tests can drive the decision without an actual
// window server. Production always uses the real detector.
var watching = Watching

// Handle decides whether to interrupt, delivers if so, and records either way.
//
// The record is the point: a notification that was correctly suppressed and a
// notification that never fired look identical from the outside, and telling
// them apart is exactly what was impossible with the predecessor.
func Handle(ctx context.Context, log *event.Log, n Notice, coalesce time.Duration) (Outcome, error) {
	// The bell arrives a beat before the notify hook, and the hook carries the
	// assistant's actual message. Waiting lets the better signal win.
	if coalesce > 0 {
		select {
		case <-time.After(coalesce):
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}

	out := decide(ctx, log, n)
	if out.Sent {
		if err := discord.Send(ctx, render(n)); err != nil {
			// Record the attempt and its failure. Swallowing this would let
			// the phone go quiet with the log still claiming delivery.
			out = Outcome{Sent: false, Why: "discord failed: " + err.Error()}
			record(ctx, log, n, out)
			return out, err
		}
	}
	return out, record(ctx, log, n, out)
}

func decide(ctx context.Context, log *event.Log, n Notice) Outcome {
	if os.Getenv("AMAC_QUIET") == "1" {
		return Outcome{Why: "AMAC_QUIET=1"}
	}
	if w, ok := watching(); ok && w == n.Session {
		return Outcome{Why: "you are looking at " + n.Session}
	}
	if when, ok := recentlyNotified(ctx, log, n.Session); ok {
		return Outcome{Why: fmt.Sprintf("already notified %.0fs ago", time.Since(when).Seconds())}
	}
	return Outcome{Sent: true}
}

// recentlyNotified reports the last delivered notification for a session
// inside the dedupe window.
func recentlyNotified(ctx context.Context, log *event.Log, session string) (time.Time, bool) {
	row := log.DB().QueryRowContext(ctx,
		`SELECT at, payload FROM events
		  WHERE kind = ? AND session = ?
		  ORDER BY seq DESC LIMIT 1`,
		string(event.KindAttention), session)
	var at string
	var payload []byte
	if err := row.Scan(&at, &payload); err != nil {
		if err != sql.ErrNoRows {
			// An unreadable history must not silence a real alert.
			return time.Time{}, false
		}
		return time.Time{}, false
	}
	var body struct {
		Outcome Outcome `json:"outcome"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || !body.Outcome.Sent {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, false
	}
	if time.Since(ts) > DedupeWindow {
		return time.Time{}, false
	}
	return ts, true
}

func record(ctx context.Context, log *event.Log, n Notice, out Outcome) error {
	ev, err := event.New(event.KindAttention, n.Agent, n.Session, map[string]any{
		"reason":  n.Reason,
		"message": n.Message,
		"outcome": out,
	})
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, ev)
	return err
}

func render(n Notice) string {
	name := strings.TrimPrefix(n.Session, "am-")
	if name == "" {
		name = n.Agent
	}
	head := "🔴 **" + name + "** needs you"
	if n.Reason == TurnComplete {
		head = "🟡 **" + name + "** finished its turn"
	}
	body := strings.TrimSpace(n.Message)
	if body == "" {
		body = "Waiting on you."
	}
	// Keep the preview short: this is a phone banner, not a transcript.
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	message := head + " · " + n.Agent + "\n" + body
	if link := discord.BoardURL(n.Session); link != "" {
		message += "\n" + link
	}
	return message
}
