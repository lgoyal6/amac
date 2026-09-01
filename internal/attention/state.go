package attention

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// State is what a session is doing, as its own hooks reported it.
type State struct {
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Account string `json:"account,omitempty"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	// At is when the state was recorded. It is read from the event row rather
	// than stored in the payload, because the log already owns the time of
	// every fact in it and a second copy is a second thing to disagree.
	At time.Time `json:"at,omitempty"`
}

// RecordState appends a state change, and only a change.
//
// PostToolUse fires on every tool call a session makes, which on this machine
// is thousands a day across twenty sessions. Recording each one would say
// "working" a thousand times over and turn the log into a heartbeat, so the
// newest recorded state is read first and an unchanged one writes nothing.
// The read costs one indexed lookup; the write it avoids costs a row, an
// fsync, and a broadcast to every live subscriber.
//
// Reported is false when nothing was written.
func RecordState(ctx context.Context, log *event.Log, s State) (reported bool, err error) {
	if s.Session == "" || s.State == "" {
		return false, nil
	}
	if prev, ok := CurrentState(ctx, log, s.Session); ok &&
		prev.State == s.State && prev.Detail == s.Detail && prev.Account == s.Account {
		// An account arriving on a session that had none is a change: the
		// board learns whose session it is, which is the whole point of
		// recording it, and suppressing that write would leave the card
		// untagged until the state happened to move.
		return false, nil
	}
	ev, err := event.New(event.KindSessionState, s.Agent, s.Session, s)
	if err != nil {
		return false, err
	}
	if _, err := log.Append(ctx, ev); err != nil {
		return false, err
	}
	return true, nil
}

// CurrentState returns the newest recorded state for one session.
func CurrentState(ctx context.Context, log *event.Log, session string) (State, bool) {
	row := log.DB().QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = ? AND session = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindSessionState), session)
	var payload []byte
	if err := row.Scan(&payload); err != nil {
		if err != sql.ErrNoRows {
			// An unreadable history must not suppress a state change: the
			// board going stale is worse than one redundant row.
			return State{}, false
		}
		return State{}, false
	}
	var st State
	if json.Unmarshal(payload, &st) != nil {
		return State{}, false
	}
	return st, st.State != ""
}

// States returns the newest state for every session in one query. The board
// refreshes on every event and routinely lists twenty sessions; one query per
// session would be twenty round trips per refresh.
func States(ctx context.Context, log *event.Log) map[string]State {
	rows, err := log.DB().QueryContext(ctx, `
		SELECT session, at, payload FROM events
		 WHERE kind = ? AND seq IN (
		       SELECT MAX(seq) FROM events WHERE kind = ? AND session != '' GROUP BY session)`,
		string(event.KindSessionState), string(event.KindSessionState))
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := map[string]State{}
	for rows.Next() {
		var sess, at string
		var payload []byte
		if rows.Scan(&sess, &at, &payload) != nil {
			continue
		}
		var st State
		if json.Unmarshal(payload, &st) != nil {
			continue
		}
		st.Session = sess
		st.At, _ = time.Parse(time.RFC3339Nano, at)
		out[sess] = st
	}
	return out
}
