// Package ledger answers "what did this cost" by querying the event log.
//
// Nothing here is instrumented separately. Agents report token usage and, when
// they can, money, through ACP usage_update notifications that the supervisor
// already records. The ledger is a view, which is the property that makes the
// event-log-as-spine decision pay off: a question nobody designed for turns
// out to be answerable from data already on disk.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Entry is one session's resource use.
//
// Cost is deliberately a *float64. Codex reports tokens but no money, and a
// nil is the honest representation of "this agent does not tell us". Coercing
// it to 0.0 would make a report that silently understates spend, which is the
// one thing a cost report must never do.
type Entry struct {
	Session string
	Agent   string
	// Account is the login the session ran as, as recorded when it started.
	// Two accounts of one agent run here on separate plans, so "codex cost
	// this much" is only half an answer.
	Account   string
	Started   time.Time
	Cost      *float64
	Tokens    int64
	Window    int64
	Turns     int
	Approvals int
}

func (e Entry) CostString() string {
	if e.Cost == nil {
		return "n/a"
	}
	return fmt.Sprintf("$%.4f", *e.Cost)
}

type Report struct {
	Entries    []Entry
	TotalCost  float64
	Priced     int // sessions that reported money
	Unpriced   int // sessions that did not
	TotalToken int64
}

// Query aggregates sessions started within the window.
//
// Two facts about the upstream data shape this query, both discovered by
// reading real events rather than assuming:
//
//   - usage_update carries a running total, not a delta, so the aggregate is
//     MAX per session and not SUM. Summing would multiply the bill by the
//     number of notifications, which for one short turn was already dozens.
//   - `used`/`size` is context-window occupancy, not cumulative tokens billed.
//     It is reported as a window high-water mark and labelled as such rather
//     than passed off as token spend.
func Query(ctx context.Context, db *sql.DB, since time.Time) (Report, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
		  e.session,
		  MIN(e.at)                                                        AS started,
		  MAX(json_extract(e.payload,'$.raw.cost.amount'))                 AS cost,
		  MAX(json_extract(e.payload,'$.raw.used'))                        AS used,
		  MAX(json_extract(e.payload,'$.raw.size'))                        AS size
		FROM events e
		WHERE e.session != '' AND e.at >= ?
		GROUP BY e.session
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Report{}, fmt.Errorf("ledger query: %w", err)
	}
	defer rows.Close()

	byID := map[string]*Entry{}
	for rows.Next() {
		var e Entry
		var started string
		var cost sql.NullFloat64
		var used, size sql.NullInt64
		if err := rows.Scan(&e.Session, &started, &cost, &used, &size); err != nil {
			return Report{}, err
		}
		e.Started, _ = time.Parse(time.RFC3339Nano, started)
		if cost.Valid {
			c := cost.Float64
			e.Cost = &c
		}
		e.Tokens, e.Window = used.Int64, size.Int64
		byID[e.Session] = &e
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}

	if err := annotate(ctx, db, since, byID); err != nil {
		return Report{}, err
	}

	rep := Report{}
	for _, e := range byID {
		if e.Cost != nil {
			rep.TotalCost += *e.Cost
			rep.Priced++
		} else {
			rep.Unpriced++
		}
		rep.TotalToken += e.Tokens
		rep.Entries = append(rep.Entries, *e)
	}
	sort.Slice(rep.Entries, func(i, j int) bool { return rep.Entries[i].Started.After(rep.Entries[j].Started) })
	return rep, nil
}

// annotate fills in the facts that live in other event kinds: which agent ran,
// which login it ran as, how many turns, and how often it had to stop and ask.
//
// Two sources for the agent and the account, and the stronger one wins. Only a
// session amac started has a session.started event; the sessions Laksh started
// himself in tmux — which is most of them — are known only through the state
// their own hooks recorded. Reading just the first left every one of those rows
// blank, so the report named the agent for the handful of sessions amac owned
// and said nothing about the twenty it watches.
func annotate(ctx context.Context, db *sql.DB, since time.Time, byID map[string]*Entry) error {
	rows, err := db.QueryContext(ctx, `
		SELECT
		  session,
		  MAX(CASE WHEN kind='session.started' THEN json_extract(payload,'$.agent') END)   AS agent,
		  MAX(CASE WHEN kind='session.started' THEN json_extract(payload,'$.account') END) AS account,
		  MAX(CASE WHEN kind='session.state'   THEN json_extract(payload,'$.agent') END)   AS hookAgent,
		  MAX(CASE WHEN kind='session.state'   THEN json_extract(payload,'$.account') END) AS hookAccount,
		  SUM(CASE WHEN json_extract(payload,'$.prompt') IS NOT NULL THEN 1 ELSE 0 END)  AS turns,
		  SUM(CASE WHEN kind='permission.requested' THEN 1 ELSE 0 END)                   AS approvals
		FROM events
		WHERE session != '' AND at >= ?
		GROUP BY session
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("ledger annotate: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var agent, acct, hookAgent, hookAcct sql.NullString
		var turns, approvals sql.NullInt64
		if err := rows.Scan(&id, &agent, &acct, &hookAgent, &hookAcct, &turns, &approvals); err != nil {
			return err
		}
		e, ok := byID[id]
		if !ok {
			continue
		}
		e.Agent, e.Account = first(agent, hookAgent), first(acct, hookAcct)
		e.Turns = int(turns.Int64)
		e.Approvals = int(approvals.Int64)
	}
	return rows.Err()
}

// first returns the leftmost value that is actually there.
func first(vals ...sql.NullString) string {
	for _, v := range vals {
		if v.Valid && v.String != "" {
			return v.String
		}
	}
	return ""
}
