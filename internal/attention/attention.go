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
	took, short := shortTurn(ctx, log, n)
	if short {
		return Outcome{Why: fmt.Sprintf("turn took %s, under the %s worth interrupting for",
			took.Round(time.Second), MinTurn)}
	}
	// Last, and only ever to narrow. The rules have already said send; the
	// model can withdraw that and cannot grant it. A bad model should cost
	// notifications you wanted, not deliver ones the rules had ruled out.
	//
	// No model is the normal case and means the rules stand, which is what
	// happens today: the trainer refuses to export one until it can show it
	// beats these rules on data it never saw.
	if score, ok := modelSaysNo(ctx, log, n, took); ok {
		return Outcome{Why: fmt.Sprintf("the recommender scored this %.2f, under its %.2f",
			score, loadModel().Threshold)}
	}
	return Outcome{Sent: true}
}

// modelSaysNo consults the recommender, if one has been exported.
//
// Any failure is a no-opinion rather than a suppression. A model that cannot be
// scored must not be able to silence a notification by being broken, which is
// the one way a serving path can turn a training mistake into missed alerts.
func modelSaysNo(ctx context.Context, log *event.Log, n Notice, turn time.Duration) (float64, bool) {
	m := loadModel()
	if m == nil {
		return 0, false
	}
	since, prior, global := recentRates(ctx, log, n.Session)
	score, err := m.Score(featuresFor(n, turn, since, prior, global, time.Now()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "amac: recommender not consulted: %v\n", err)
		return 0, false
	}
	return score, score < m.Threshold
}

// recentRates reports how long since this session last raised a notification,
// and how many have arrived in the last hour for this session and in total.
// The same three counts the trainer computes over the log, so the model is
// scored on what it was fitted on.
func recentRates(ctx context.Context, log *event.Log, session string) (time.Duration, int, int) {
	const hour = "-1 hour"
	since := 24 * time.Hour
	var last string
	if err := log.DB().QueryRowContext(ctx,
		`SELECT at FROM events WHERE kind = ? AND session = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAttention), session).Scan(&last); err == nil {
		if ts, err := time.Parse(time.RFC3339Nano, last); err == nil {
			since = time.Since(ts)
		}
	}
	count := func(q string, args ...any) int {
		var n int
		if err := log.DB().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return 0
		}
		return n
	}
	prior := count(`SELECT COUNT(*) FROM events WHERE kind = ? AND session = ? AND at >= datetime('now', ?)`,
		string(event.KindAttention), session, hour)
	global := count(`SELECT COUNT(*) FROM events WHERE kind = ? AND at >= datetime('now', ?)`,
		string(event.KindAttention), hour)
	return since, prior, global
}

// MinTurn is how long a turn must have taken before its completion is worth a
// push.
//
// Measured rather than chosen. Over the log's history 81% of everything
// delivered was a turn-complete, 116 a day against 14 a day for a session
// actually blocked on a person, and the median turn behind one of those took
// under a minute. Most of the volume was announcing something instant.
//
// Ten minutes keeps 11% of them, which takes the whole stream from about 130 a
// day to about 27 while still pushing the case that has real value: work left
// running for a while has finished. A blanket cut would be quieter and would
// also throw away the thirty turns in the log that ran over an hour, which are
// the only ones anybody would want interrupting for.
//
// Not applied to a session blocked on a person. Nothing about how long an
// agent worked changes whether it is now stuck waiting for you.
var MinTurn = envDuration("AMAC_MIN_TURN", 10*time.Minute)

func envDuration(name string, fallback time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

// shortTurn reports how long the session had been running before this
// notification, and whether that is too short to be worth one.
//
// The turn is measured as the gap since the session's previous event, which is
// the same statistic the threshold was calibrated on. It fails open: a session
// with no history, or a history that cannot be read, is notified about. A rule
// that goes quiet when it cannot measure is a rule that silences real alerts
// the first time something upstream changes shape.
func shortTurn(ctx context.Context, log *event.Log, n Notice) (time.Duration, bool) {
	if n.Reason != TurnComplete || MinTurn <= 0 {
		return 0, false
	}
	row := log.DB().QueryRowContext(ctx,
		`SELECT at FROM events
		  WHERE session = ? AND kind IN (?, ?, ?)
		  ORDER BY seq DESC LIMIT 1`,
		n.Session, string(event.KindSessionState),
		string(event.KindSessionUpdate), string(event.KindAttention))
	var at string
	if err := row.Scan(&at); err != nil {
		return 0, false
	}
	ts, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return 0, false
	}
	took := time.Since(ts)
	return took, took < MinTurn
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
