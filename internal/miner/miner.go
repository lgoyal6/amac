// Package miner finds patterns in the event log and proposes automations.
//
// This is the payoff of the event-sourced design, and it is worth stating why
// it is only possible because of it: nothing here was designed for. The log
// was written to make the dashboard work, and it turns out to also answer
// "what do you do repeatedly", "what do you always approve", and "where does
// your time actually go" - none of which needed new instrumentation.
//
// Every suggestion is a proposal, never an action. A system that watches you
// and then starts doing things unprompted is a different product with a very
// different risk profile.
package miner

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Suggestion struct {
	Kind       string
	Title      string
	Evidence   string
	Confidence float64 // share of occurrences supporting it
	Action     string  // what to do about it, in the user's terms
}

type Report struct {
	Window      time.Duration
	Suggestions []Suggestion
	Stats       map[string]int
}

func Mine(ctx context.Context, db *sql.DB, since time.Time) (Report, error) {
	rep := Report{Window: time.Since(since), Stats: map[string]int{}}

	for _, fn := range []func(context.Context, *sql.DB, time.Time) ([]Suggestion, map[string]int, error){
		alwaysApproved,
		repeatedPrompts,
		expensiveSessions,
		attentionSinks,
		blockedWait,
	} {
		s, stats, err := fn(ctx, db, since)
		if err != nil {
			return rep, err
		}
		rep.Suggestions = append(rep.Suggestions, s...)
		for k, v := range stats {
			rep.Stats[k] += v
		}
	}

	sort.Slice(rep.Suggestions, func(i, j int) bool {
		return rep.Suggestions[i].Confidence > rep.Suggestions[j].Confidence
	})
	return rep, nil
}

// alwaysApproved finds permission prompts you have never once denied. Those
// are interruptions with no decision content, and they are the single highest
// value thing to automate: the approval is not protecting you from anything.
func alwaysApproved(ctx context.Context, db *sql.DB, since time.Time) ([]Suggestion, map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
		  json_extract(r.payload,'$.title')                                     AS title,
		  COUNT(*)                                                              AS asked,
		  SUM(CASE WHEN json_extract(a.payload,'$.outcome')='selected' THEN 1 ELSE 0 END) AS approved
		FROM events r
		JOIN events a
		  ON a.kind='permission.answered'
		 AND a.session = r.session
		 AND json_extract(a.payload,'$.toolCallId') = json_extract(r.payload,'$.toolCallId')
		WHERE r.kind='permission.requested' AND r.at >= ?
		GROUP BY title
		HAVING asked >= 3
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, nil, fmt.Errorf("mine approvals: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	stats := map[string]int{}
	for rows.Next() {
		var title sql.NullString
		var asked, approved int
		if err := rows.Scan(&title, &asked, &approved); err != nil {
			return nil, nil, err
		}
		stats["permission_kinds"]++
		if approved == asked {
			out = append(out, Suggestion{
				Kind:       "auto-approve",
				Title:      fmt.Sprintf("%q is always approved", trim(title.String, 50)),
				Evidence:   fmt.Sprintf("%d of %d approved, never denied", approved, asked),
				Confidence: confidence(asked),
				Action:     "add an allow rule so this stops interrupting you",
			})
		}
	}
	return out, stats, rows.Err()
}

// repeatedPrompts finds work you keep asking for in the same words. Those are
// candidates for a saved command rather than something retyped.
func repeatedPrompts(ctx context.Context, db *sql.DB, since time.Time) ([]Suggestion, map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lower(substr(json_extract(payload,'$.prompt'),1,60)) AS stem, COUNT(*) n
		FROM events
		WHERE json_extract(payload,'$.prompt') IS NOT NULL AND at >= ?
		GROUP BY stem HAVING n >= 3 ORDER BY n DESC LIMIT 5
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, nil, fmt.Errorf("mine prompts: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	stats := map[string]int{}
	for rows.Next() {
		var stem string
		var n int
		if err := rows.Scan(&stem, &n); err != nil {
			return nil, nil, err
		}
		stats["repeated_prompts"]++
		out = append(out, Suggestion{
			Kind:       "saved-command",
			Title:      fmt.Sprintf("asked %d times: %q", n, trim(stem, 45)),
			Evidence:   fmt.Sprintf("%d near-identical prompts", n),
			Confidence: confidence(n),
			Action:     "save this as a named task so it is one word next time",
		})
	}
	return out, stats, rows.Err()
}

// expensiveSessions surfaces the tail that dominates spend. Cost is almost
// always concentrated, and knowing which kind of work is expensive is what
// makes routing decisions concrete instead of theoretical.
func expensiveSessions(ctx context.Context, db *sql.DB, since time.Time) ([]Suggestion, map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT session, MAX(json_extract(payload,'$.raw.cost.amount')) c
		FROM events WHERE at >= ? AND session != ''
		GROUP BY session HAVING c IS NOT NULL ORDER BY c DESC LIMIT 3
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, nil, fmt.Errorf("mine cost: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	stats := map[string]int{}
	for rows.Next() {
		var session string
		var c float64
		if err := rows.Scan(&session, &c); err != nil {
			return nil, nil, err
		}
		stats["priced_sessions"]++
		if c >= 0.20 {
			out = append(out, Suggestion{
				Kind:       "cost",
				Title:      fmt.Sprintf("session %s cost $%.4f", session, c),
				Evidence:   "in the top 3 by spend for this window",
				Confidence: 0.6,
				Action:     "check whether the mechanical parts of this could route cheaper",
			})
		}
	}
	return out, stats, rows.Err()
}

// attentionSinks reports where observed time goes. Only ever covers apps you
// allowlisted, so silence here means the observer is off or denying, not that
// you did nothing.
func attentionSinks(ctx context.Context, db *sql.DB, since time.Time) ([]Suggestion, map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(payload,'$.app') app, SUM(json_extract(payload,'$.seconds')) s
		FROM events WHERE kind='observation' AND at >= ?
		GROUP BY app ORDER BY s DESC LIMIT 3
	`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, nil, fmt.Errorf("mine observations: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	stats := map[string]int{}
	for rows.Next() {
		var app sql.NullString
		var secs sql.NullInt64
		if err := rows.Scan(&app, &secs); err != nil {
			return nil, nil, err
		}
		stats["observed_apps"]++
		if secs.Int64 > 1800 {
			out = append(out, Suggestion{
				Kind:       "attention",
				Title:      fmt.Sprintf("%s took %s", app.String, dur(secs.Int64)),
				Evidence:   "largest observed span in this window",
				Confidence: 0.5,
				Action:     "worth knowing; no action unless it surprises you",
			})
		}
	}
	return out, stats, rows.Err()
}

// blockedWait measures how long agents sat waiting on you. This is the number
// the whole system was built to reduce, so it is the one to watch over time.
func blockedWait(ctx context.Context, db *sql.DB, since time.Time) ([]Suggestion, map[string]int, error) {
	var n sql.NullInt64
	var total sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(json_extract(payload,'$.waited_ms'))
		FROM events WHERE kind='permission.answered' AND at >= ?
	`, since.UTC().Format(time.RFC3339Nano)).Scan(&n, &total)
	if err != nil {
		return nil, nil, fmt.Errorf("mine wait: %w", err)
	}
	stats := map[string]int{"approvals": int(n.Int64)}
	if n.Int64 == 0 {
		return nil, stats, nil
	}
	avg := total.Float64 / float64(n.Int64) / 1000
	if avg < 30 {
		return nil, stats, nil
	}
	return []Suggestion{{
		Kind:       "latency",
		Title:      fmt.Sprintf("agents waited %.0fs on average for you", avg),
		Evidence:   fmt.Sprintf("%d approvals in this window", n.Int64),
		Confidence: 0.7,
		Action:     "auto-approve the always-approved kinds above, or answer from your phone",
	}}, stats, nil
}

func confidence(n int) float64 {
	c := float64(n) / 10
	if c > 0.95 {
		return 0.95
	}
	return c
}

func dur(s int64) string {
	d := time.Duration(s) * time.Second
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

func trim(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
