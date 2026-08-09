package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Durability is the fsync policy for the log. It is an explicit choice rather
// than a default because it is the one knob that trades data loss against
// write throughput, and the honest answer differs per deployment.
type Durability string

const (
	// Full syncs on every commit. An event acknowledged to a caller has
	// reached the disk. Correct default for a log whose entire value is being
	// trustworthy after a crash.
	Full Durability = "full"

	// Relaxed lets the OS schedule the fsync. Survives process kill, can lose
	// the tail on power loss. Measurably faster under high-rate writes.
	Relaxed Durability = "relaxed"
)

// Log is an append-only event store on SQLite in WAL mode.
//
// WAL is what makes readers (dashboard, miner, replay) run concurrently with
// the writer instead of blocking behind it. Writes stay single-threaded behind
// one mutex, which is not a limitation worth removing: sequence numbers must
// be a total order, and SQLite serialises writers anyway.
type Log struct {
	db *sql.DB
	mu sync.Mutex

	subMu sync.RWMutex
	subs  map[int]chan Event
	nextS int
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  seq     INTEGER PRIMARY KEY AUTOINCREMENT,
  at      TEXT    NOT NULL,
  kind    TEXT    NOT NULL,
  source  TEXT    NOT NULL,
  session TEXT    NOT NULL DEFAULT '',
  payload BLOB
);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session, seq);
CREATE INDEX IF NOT EXISTS idx_events_kind    ON events(kind, seq);
CREATE INDEX IF NOT EXISTS idx_events_at      ON events(at);
`

func Open(path string, d Durability) (*Log, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	sync := "FULL"
	if d == Relaxed {
		sync = "NORMAL"
	}
	// busy_timeout matters even single-writer: WAL checkpoints and concurrent
	// readers can still briefly hold locks, and the default is to fail
	// instantly rather than wait.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=" + sync,
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Log{db: db, subs: make(map[int]chan Event)}, nil
}

func (l *Log) Close() error { return l.db.Close() }

// Append writes one event and returns it with Seq assigned. The write is
// committed before subscribers are notified, so nobody can observe an event
// that a crash would then un-happen.
func (l *Log) Append(ctx context.Context, e Event) (Event, error) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}

	l.mu.Lock()
	res, err := l.db.ExecContext(ctx,
		`INSERT INTO events (at, kind, source, session, payload) VALUES (?, ?, ?, ?, ?)`,
		e.At.Format(time.RFC3339Nano), string(e.Kind), e.Source, e.Session, []byte(e.Payload))
	if err != nil {
		l.mu.Unlock()
		return e, fmt.Errorf("append event: %w", err)
	}
	seq, err := res.LastInsertId()
	l.mu.Unlock()
	if err != nil {
		return e, fmt.Errorf("append event seq: %w", err)
	}
	e.Seq = seq

	l.fanout(e)
	return e, nil
}

// Subscribe returns a channel of events appended after this call, plus a
// cancel func. The channel is buffered and a slow subscriber is dropped rather
// than allowed to stall the writer: the log is the durable record, a live
// subscription is a convenience, and losing the latter must never risk the
// former. A dropped subscriber recovers by replaying from its last seq.
func (l *Log) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 128
	}
	ch := make(chan Event, buffer)

	l.subMu.Lock()
	id := l.nextS
	l.nextS++
	l.subs[id] = ch
	l.subMu.Unlock()

	return ch, func() {
		l.subMu.Lock()
		if c, ok := l.subs[id]; ok {
			delete(l.subs, id)
			close(c)
		}
		l.subMu.Unlock()
	}
}

func (l *Log) fanout(e Event) {
	l.subMu.RLock()
	defer l.subMu.RUnlock()
	for _, ch := range l.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop, do not block the writer
		}
	}
}

// Since returns events after seq, oldest first. This is the replay primitive:
// the dashboard uses it to catch up after a reconnect, and the miner uses it
// to walk history.
func (l *Log) Since(ctx context.Context, seq int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT seq, at, kind, source, session, payload FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("query since: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at string
		var payload []byte
		if err := rows.Scan(&e.Seq, &at, &e.Kind, &e.Source, &e.Session, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("parse event time %q: %w", at, err)
		}
		if len(payload) > 0 {
			e.Payload = json.RawMessage(payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Head is the highest sequence written, or 0 on an empty log. Recovery starts
// here: a restarting subsystem asks for Head, then replays what it missed.
func (l *Log) Head(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("head: %w", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

func (l *Log) Count(ctx context.Context) (int64, error) {
	var n int64
	err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}
