package event

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Retention, and why most of it is redaction rather than deletion.
//
// An append-only log has an obvious problem and a less obvious one. The obvious
// one is that it grows forever. The less obvious one is that its rows do not
// all age the same way, and a blanket "delete older than N days" throws away
// the cheap rows alongside the expensive ones.
//
// Measured on this installation at 6,839 events and 4.9MB of payload:
//
//	kind               rows    payload
//	attention          2,016    2.9MB      <- 60% of the bytes, 29% of the rows
//	automation.check     714    871KB
//	session.update       664    605KB
//	session.state      2,681    391KB      <- 39% of the rows, 8% of the bytes
//
// attention costs 1.5KB a row because it stores the full text of every message
// sent, while session.state costs 0.15KB. So the expensive thing is not the
// number of decisions, it is one field inside them.
//
// That makes redaction the better tool. Dropping the message body from an old
// attention row reclaims almost all of its size while keeping the timestamp,
// the session, the reason and the outcome, which is everything the board reads
// and everything the analysis in analysis/ is computed from. Deleting the row
// would reclaim the same bytes and destroy the record that a decision happened.
//
// Some kinds are never touched at all. Permission requests and answers, and
// actuations, are the audit trail: they are what "what did it change, and who
// said yes" is answered from, they are tiny, and an audit log with a retention
// policy is not an audit log.
type Rule struct {
	Kind string
	// After is how long a row is kept intact. Zero means forever.
	After time.Duration
	// Redact names payload fields cleared once After has passed. When empty,
	// the row itself is deleted instead.
	Redact []string
	// Why this rule exists, printed by Plan so a policy can be argued with.
	Why string
}

// DefaultRetention is sized from the measurements above rather than by taste.
func DefaultRetention() []Rule {
	return []Rule{
		{Kind: "attention", After: 30 * 24 * time.Hour, Redact: []string{"message"},
			Why: "60% of all payload bytes is the message text; the decision is what matters later"},
		{Kind: "automation.check", After: 14 * 24 * time.Hour, Redact: []string{"reports"},
			Why: "every sweep carries the whole roster, so old ones are the same rows repeated"},
		{Kind: "session.update", After: 30 * 24 * time.Hour,
			Why: "agent chatter, useful while a session is live and rarely after"},
		{Kind: "session.state", After: 90 * 24 * time.Hour,
			Why: "cheap per row, and the history the board draws from"},
		// Deliberately absent, and the absence is the policy: permission.requested,
		// permission.answered, actuation, session.started, session.ended,
		// automation.run, application, route.decided. Audit and delivery history,
		// all small, none of it worth reclaiming.
	}
}

// Change is one rule's effect, counted before anything is written.
type Change struct {
	Rule    Rule
	Rows    int
	Bytes   int64
	Deletes bool
}

func (c Change) String() string {
	what := "redact " + strings.Join(c.Rule.Redact, ", ")
	if c.Deletes {
		what = "delete"
	}
	return fmt.Sprintf("%-18s %s %d rows older than %s, about %s (%s)",
		c.Rule.Kind, what, c.Rows, short(c.Rule.After), size(c.Bytes), c.Rule.Why)
}

// Plan reports what applying these rules would do, and changes nothing.
//
// It exists so the policy can be read before it is trusted. A retention job
// whose first observable effect is missing data is one nobody runs twice.
func (l *Log) Plan(ctx context.Context, rules []Rule, now time.Time) ([]Change, error) {
	var out []Change
	for _, r := range rules {
		if r.After <= 0 {
			continue
		}
		cutoff := now.Add(-r.After).UTC().Format(time.RFC3339Nano)
		var rows int
		var bytes *int64
		err := l.db.QueryRowContext(ctx,
			`SELECT COUNT(*), SUM(length(payload)) FROM events WHERE kind = ? AND at < ?`,
			r.Kind, cutoff).Scan(&rows, &bytes)
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", r.Kind, err)
		}
		if rows == 0 {
			continue
		}
		c := Change{Rule: r, Rows: rows, Deletes: len(r.Redact) == 0}
		if bytes != nil {
			c.Bytes = *bytes
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// Apply carries out the plan.
//
// Redaction rewrites the payload rather than deleting the row, and it is done
// in Go rather than with SQLite's json_remove so that a payload which is not an
// object, or not valid JSON at all, is left exactly as it was instead of being
// replaced by null. A retention pass that corrupts the rows it cannot parse is
// worse than one that skips them.
func (l *Log) Apply(ctx context.Context, rules []Rule, now time.Time) ([]Change, error) {
	planned, err := l.Plan(ctx, rules, now)
	if err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range planned {
		cutoff := now.Add(-c.Rule.After).UTC().Format(time.RFC3339Nano)
		if c.Deletes {
			if _, err := l.db.ExecContext(ctx,
				`DELETE FROM events WHERE kind = ? AND at < ?`, c.Rule.Kind, cutoff); err != nil {
				return nil, fmt.Errorf("delete %s: %w", c.Rule.Kind, err)
			}
			continue
		}
		if err := l.redact(ctx, c.Rule, cutoff); err != nil {
			return nil, err
		}
	}
	return planned, nil
}

func (l *Log) redact(ctx context.Context, r Rule, cutoff string) error {
	rows, err := l.db.QueryContext(ctx,
		`SELECT seq, payload FROM events WHERE kind = ? AND at < ?`, r.Kind, cutoff)
	if err != nil {
		return fmt.Errorf("read %s: %w", r.Kind, err)
	}
	type edit struct {
		seq     int64
		payload []byte
	}
	var edits []edit
	for rows.Next() {
		var seq int64
		var raw []byte
		if err := rows.Scan(&seq, &raw); err != nil {
			rows.Close()
			return err
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue // not an object: leave it alone rather than guess
		}
		var changed bool
		for _, f := range r.Redact {
			if _, present := m[f]; present {
				delete(m, f)
				changed = true
			}
		}
		if !changed {
			continue
		}
		// A marker, so a reader can tell a redacted row from one that never
		// carried the field. Silence would make old rows look like a schema
		// change rather than a retention pass.
		m["redacted"] = json.RawMessage(`true`)
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		edits = append(edits, edit{seq: seq, payload: b})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range edits {
		if _, err := tx.ExecContext(ctx,
			`UPDATE events SET payload = ? WHERE seq = ?`, e.payload, e.seq); err != nil {
			return fmt.Errorf("redact %s: %w", r.Kind, err)
		}
	}
	return tx.Commit()
}

// Vacuum reclaims the file space that redaction and deletion freed.
//
// Kept separate from Apply because it is the expensive half: SQLite rewrites
// the whole database, which needs room for a second copy and takes a lock that
// a running daemon will feel. Apply is safe to run whenever; this is the part
// worth doing when nothing else is.
func (l *Log) Vacuum(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.ExecContext(ctx, "VACUUM")
	return err
}

func size(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

func short(d time.Duration) string {
	if days := int(d.Hours() / 24); days > 0 {
		return fmt.Sprintf("%dd", days)
	}
	return d.String()
}
