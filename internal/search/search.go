// Package search answers questions about the log that scrolling cannot.
//
// Six thousand events of natural language sit in a database that has a full
// text engine built into it, and until now the only way to find "what was the
// agent doing when it asked to delete that branch" was to page the board until
// it went past. That is not a missing feature so much as an unused one.
//
// Two decisions shape the rest of this file.
//
// The index is append-only, because the log is. There is no trigger and no
// rebuild: whatever has arrived since the last pass gets indexed on the next
// query, which for an append-only table is a single range scan.
//
// The index is contentless, so it stores no copy of the text. That saves the
// obvious thing, but the reason is the other one: with no copy, every hit has
// to be confirmed against the row as it stands right now. Retention redacts
// old rows in place, and an index that kept its own copy would go on answering
// with text the log no longer holds. Here a redacted row simply stops matching
// and drops out. The index may be stale; a result cannot be.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/lgoyal6/amac/internal/event"
)

// fields are the payload keys worth indexing, by name at any depth. A payload
// is mostly identifiers, timestamps and enums; indexing all of it would bury
// the prose that is the only reason to search in the first place.
var fields = map[string]bool{
	"message": true, // what an agent said, and what the notification carried
	"title":   true, // the command a permission request is asking about
	"detail":  true,
	"text":    true,
	"task":    true, // the prompt a session was started with
	"note":    true,
	"reason":  true,
	"why":     true,
	"result":  true,
	"sent":    true,
	"role":    true,
}

// minLen drops "completed", "ok" and the other one-word status strings that
// share a key name with real prose. They match everything and mean nothing.
const minLen = 12

const schema = `
CREATE VIRTUAL TABLE IF NOT EXISTS event_fts USING fts5(text, content='');
CREATE TABLE IF NOT EXISTS event_fts_state (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  indexed_to INTEGER NOT NULL
);
INSERT OR IGNORE INTO event_fts_state (id, indexed_to) VALUES (1, 0);
`

type Hit struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Session string    `json:"session,omitempty"`
	Snippet string    `json:"snippet"`
}

// Text pulls the indexable prose out of a payload, in the order it appears.
// Exported because the searcher and the indexer must agree on what the text of
// an event is: if they can disagree, a row can be indexed under words it will
// never be confirmed to contain, and it would silently never be findable.
func Text(payload []byte) string {
	var v any
	if json.Unmarshal(payload, &v) != nil {
		return ""
	}
	var parts []string
	var walk func(any, string)
	walk = func(n any, key string) {
		switch t := n.(type) {
		case map[string]any:
			for k, child := range t {
				walk(child, k)
			}
		case []any:
			for _, child := range t {
				walk(child, key)
			}
		case string:
			if fields[key] && len(t) >= minLen {
				parts = append(parts, t)
			}
		}
	}
	walk(v, "")
	return strings.Join(parts, "\n")
}

// Update indexes everything appended since the last pass and returns how many
// rows it added.
func Update(ctx context.Context, log *event.Log) (int, error) {
	db := log.DB()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return 0, fmt.Errorf("create index: %w", err)
	}
	var from int64
	if err := db.QueryRowContext(ctx, `SELECT indexed_to FROM event_fts_state WHERE id = 1`).Scan(&from); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT seq, payload FROM events WHERE seq > ? ORDER BY seq`, from)
	if err != nil {
		return 0, err
	}
	type row struct {
		seq  int64
		text string
	}
	var batch []row
	var high int64 = from
	for rows.Next() {
		var seq int64
		var payload []byte
		if err := rows.Scan(&seq, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		high = seq
		if t := Text(payload); t != "" {
			batch = append(batch, row{seq: seq, text: t})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if high == from {
		return 0, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, r := range batch {
		// rowid is the sequence number, which is what makes the join back to
		// the log free and the index idempotent: re-indexing a row replaces it
		// rather than duplicating it.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO event_fts (rowid, text) VALUES (?, ?)`, r.seq, r.text); err != nil {
			return 0, fmt.Errorf("index %d: %w", r.seq, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE event_fts_state SET indexed_to = ? WHERE id = 1`, high); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// Query runs a search, bringing the index up to date first.
//
// The query goes to FTS5 as written, so the operators people expect from a
// search box (quoted phrases, AND, OR, NOT, trailing * for a prefix) work. A
// query FTS5 cannot parse is a bad request rather than an empty result: an
// unbalanced quote returning "nothing found" is indistinguishable from a
// search that genuinely found nothing.
func Query(ctx context.Context, log *event.Log, q string, limit int) ([]Hit, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("empty query")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if _, err := Update(ctx, log); err != nil {
		return nil, err
	}

	// Over-fetch, because confirming against the live payload can drop rows
	// and a page that comes back short would look like the end of the results.
	rows, err := log.DB().QueryContext(ctx, `
		SELECT e.seq, e.at, e.kind, e.session, e.payload
		FROM event_fts f JOIN events e ON e.seq = f.rowid
		WHERE event_fts MATCH ?
		ORDER BY e.seq DESC
		LIMIT ?`, q, limit*3)
	if err != nil {
		return nil, fmt.Errorf("%s", explain(err))
	}
	defer rows.Close()

	terms := termsOf(q)
	out := []Hit{}
	for rows.Next() && len(out) < limit {
		var h Hit
		var at string
		var payload []byte
		if err := rows.Scan(&h.Seq, &at, &h.Kind, &h.Session, &payload); err != nil {
			return nil, err
		}
		// The confirmation the contentless index buys. A row redacted since it
		// was indexed no longer holds the words it was indexed under, and is
		// dropped here rather than returned as a hit with nothing behind it.
		text := Text(payload)
		snip, ok := snippet(text, terms)
		if !ok {
			continue
		}
		h.At, _ = time.Parse(time.RFC3339Nano, at)
		h.Snippet = snip
		out = append(out, h)
	}
	return out, rows.Err()
}

// explain turns the engine's complaint into one a person can act on. The raw
// text is "SQL logic error: unterminated string (1)", which names a language
// the person searching did not write anything in.
func explain(err error) string {
	e := strings.ToLower(err.Error())
	switch {
	case strings.Contains(e, "unterminated string"):
		return "there is an unclosed quote in that search"
	case strings.Contains(e, "syntax error"), strings.Contains(e, "fts5"):
		return "that search could not be read: words, \"quoted phrases\", AND, OR, NOT, " +
			"NEAR, and a trailing * for a prefix"
	}
	return "that search could not be run"
}

// termsOf reduces an FTS5 query to the bare words in it. Confirming a hit does
// not re-implement FTS5 matching, which would be a second search engine with
// its own opinions; it asks the far weaker question of whether any of the words
// searched for are still in the row.
func termsOf(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		switch strings.ToUpper(f) {
		case "AND", "OR", "NOT", "NEAR":
			continue
		}
		if len(f) > 1 {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// snippet returns the text around the first term found, and reports whether
// any term was found at all.
func snippet(text string, terms []string) (string, bool) {
	if text == "" {
		return "", false
	}
	low := strings.ToLower(text)
	at := -1
	for _, t := range terms {
		if i := strings.Index(low, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return "", false
	}
	const before, width = 60, 220
	start := max(at-before, 0)
	end := min(start+width, len(text))
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "..." + s
	}
	if end < len(text) {
		s += "..."
	}
	return s, true
}
