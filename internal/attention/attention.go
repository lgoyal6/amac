package attention

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
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
	// Account is which login produced this, as internal/account names them.
	// Two Codex accounts run on this machine under separate homes and separate
	// plans, and a notification that says only "codex" cannot tell you which
	// one is burning its limit.
	Account string
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
// A phone alert is a request for one human response, not a heartbeat. Codex
// can ring the bell repeatedly while sitting at the same prompt, and a
// 45-second transport-only window turned one unanswered request into a new DM
// every few minutes. Ten minutes still permits a genuinely new ask while
// preventing the notification channel from becoming a pager loop.
const DedupeWindow = 10 * time.Minute

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

	// Minted here rather than inside record, because the link has to carry it.
	// A notification whose id is assigned after its link is built is a
	// notification nothing that happens next can be attributed to, which is
	// what made the whole engagement question unanswerable.
	id := noticeID()

	out := decide(ctx, log, n)
	if out.Sent {
		if err := discord.SendHandoff(ctx, render(n), discord.HandoffURL(n.Session, id)); err != nil {
			// Record the attempt and its failure. Swallowing this would let
			// the phone go quiet with the log still claiming delivery.
			out = Outcome{Sent: false, Why: "discord failed: " + err.Error()}
			record(ctx, log, n, id, out)
			return out, err
		}
	}
	return out, record(ctx, log, n, id, out)
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

// noticeID mints the join key a notification is later matched on.
//
// The sequence number would do and arrives too late: it is assigned when the
// event is appended, and the payload has to be built before that. A short
// random id is enough to correlate one notification with what followed it, and
// it survives the retention pass that redacts the message body.
func noticeID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func record(ctx context.Context, log *event.Log, n Notice, id string, out Outcome) error {
	ev, err := event.New(event.KindAttention, n.Agent, n.Session, map[string]any{
		"id":      id,
		"reason":  n.Reason,
		"account": n.Account,
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
	return "🔔 **" + name + "** needs you"
}
