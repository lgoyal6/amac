// Package queue hands work to agents, once each.
//
// Everything else in amac is a view over the append-only log, and this
// deliberately is not. A log is the wrong structure for mutual exclusion:
// deciding whether a task is free means reading every claim and release ever
// written and hoping nobody appended between the read and the write. Two agents
// asking at the same moment both see it free, both append a claim, and the log
// faithfully records that the work was done twice.
//
// So a claim is a conditional UPDATE inside a transaction, which SQLite
// serialises across processes, and the log keeps the history. That split is the
// point: the log answers "what happened", the table answers "who holds this
// right now", and only the second one needs to be atomic.
//
// # Leases, and the zombie problem
//
// A worker that dies holding a claim must not hold it forever, so a claim
// carries a deadline and is reclaimable once it passes. That introduces the
// failure every lease scheme has: worker A stalls, its lease expires, B takes
// the task, and then A wakes up and reports a result for work B is now doing.
// A lease alone cannot prevent this, because A has no way of knowing it was
// declared dead.
//
// Every claim therefore carries a fencing token, a counter that only goes up.
// A result is accepted only when its token matches the claim the table
// currently holds, so a revived worker's write is rejected on arrival rather
// than trusted because it arrived. This is the one part of the design that
// cannot be bolted on afterwards.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// State is where a task is in its life.
type State string

const (
	Ready    State = "ready"    // nobody holds it
	Claimed  State = "claimed"  // someone holds it, lease live
	Done     State = "done"     // finished, terminal
	Failed   State = "failed"   // gave up, terminal
	Canceled State = "canceled" // withdrawn, terminal
)

// Task is one unit of work.
type Task struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Dir     string    `json:"dir"`
	State   State     `json:"state"`
	Owner   string    `json:"owner,omitempty"`
	Token   int64     `json:"token,omitempty"` // fencing token of the live claim
	Lease   time.Time `json:"lease,omitempty"`
	Attempt int       `json:"attempt"`
	Result  string    `json:"result,omitempty"`
	Filed   time.Time `json:"filed"`
}

var (
	// ErrNotHeld means the caller's claim is no longer the live one: its lease
	// expired and someone else took the task. Returned rather than silently
	// ignored, because a worker that has been fenced needs to stop working, not
	// to retry.
	ErrNotHeld = errors.New("claim superseded: this task is held by someone else now")
	// ErrNoWork means the queue had nothing claimable, which is an ordinary
	// answer and not a failure.
	ErrNoWork = errors.New("no claimable task")
)

type Queue struct {
	db  *sql.DB
	log *event.Log
	// now is indirected so the lease tests can move time without sleeping
	// through a lease.
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
  id       TEXT PRIMARY KEY,
  title    TEXT NOT NULL,
  dir      TEXT NOT NULL DEFAULT '',
  state    TEXT NOT NULL,
  owner    TEXT NOT NULL DEFAULT '',
  token    INTEGER NOT NULL DEFAULT 0,
  lease    INTEGER NOT NULL DEFAULT 0,
  attempt  INTEGER NOT NULL DEFAULT 0,
  result   TEXT NOT NULL DEFAULT '',
  filed    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_claimable ON tasks(state, lease);
`

// Open prepares the queue against the same database the log lives in.
//
// One file rather than two, because a task's history and its claim have to
// commit or fail together: a task marked done in one database and recorded as
// still claimed in another is a task that nobody will ever finish and nobody
// can see is stuck.
func Open(log *event.Log) (*Queue, error) {
	q := &Queue{db: log.DB(), log: log, now: func() time.Time { return time.Now().UTC() }}
	if _, err := q.db.Exec(schema); err != nil {
		return nil, fmt.Errorf("queue schema: %w", err)
	}
	return q, nil
}

// File adds a task. Filing the same id twice is not an error and does not reset
// it: work is filed by whatever noticed it needed doing, and two health sweeps
// noticing the same broken automation must not produce two attempts at it.
func (q *Queue) File(ctx context.Context, t Task) (Task, error) {
	if t.ID == "" || t.Title == "" {
		return t, errors.New("a task needs an id and a title")
	}
	t.Filed = q.now()
	t.State = Ready
	res, err := q.db.ExecContext(ctx, `
		INSERT INTO tasks (id, title, dir, state, filed) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		t.ID, t.Title, t.Dir, string(Ready), t.Filed.UnixMilli())
	if err != nil {
		return t, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		q.record(ctx, "task.filed", t.ID, map[string]any{"title": t.Title, "dir": t.Dir})
	}
	return q.Get(ctx, t.ID)
}

// Claim takes the oldest claimable task for a worker.
//
// The whole correctness of this package is the one UPDATE below. It selects and
// takes in a single statement, so there is no window between deciding a task is
// free and holding it. Two workers racing produce one winner and one ErrNoWork,
// whichever processes they are in, because SQLite serialises the write.
//
// A task is claimable when nobody holds it, or when whoever held it has not
// renewed and the lease has passed. Reclaiming is not a special path: an
// abandoned task simply becomes claimable again, so a worker that was killed
// and a worker that never existed look the same to everyone else.
func (q *Queue) Claim(ctx context.Context, owner string, lease time.Duration) (Task, error) {
	if owner == "" {
		return Task{}, errors.New("a claim needs an owner")
	}
	now := q.now()
	deadline := now.Add(lease)

	res, err := q.db.ExecContext(ctx, `
		UPDATE tasks
		   SET state = ?, owner = ?, lease = ?, token = token + 1, attempt = attempt + 1
		 WHERE id = (
		       SELECT id FROM tasks
		        WHERE state = ? OR (state = ? AND lease <= ?)
		        ORDER BY filed LIMIT 1)`,
		string(Claimed), owner, deadline.UnixMilli(),
		string(Ready), string(Claimed), now.UnixMilli())
	if err != nil {
		return Task{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Task{}, ErrNoWork
	}

	// Which one was taken is read back rather than assumed: the subquery picked
	// it, and re-deriving it here would be a second guess that can differ from
	// the first under a concurrent write.
	var t Task
	row := q.db.QueryRowContext(ctx, `
		SELECT id, title, dir, state, owner, token, lease, attempt, result, filed
		  FROM tasks WHERE owner = ? AND token > 0 ORDER BY lease DESC LIMIT 1`, owner)
	if err := scan(row, &t); err != nil {
		return Task{}, err
	}
	q.record(ctx, "task.claimed", t.ID, map[string]any{
		"owner": owner, "token": t.Token, "attempt": t.Attempt,
		"lease_until": deadline.Format(time.RFC3339),
	})
	return t, nil
}

// Renew extends a lease the caller still holds.
//
// Guarded by the token, not just the owner. A worker restarted under the same
// name is a different worker, and letting it renew a lease it never took would
// keep a task alive under someone who is not doing it.
func (q *Queue) Renew(ctx context.Context, id string, token int64, lease time.Duration) error {
	res, err := q.db.ExecContext(ctx, `
		UPDATE tasks SET lease = ? WHERE id = ? AND token = ? AND state = ?`,
		q.now().Add(lease).UnixMilli(), id, token, string(Claimed))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotHeld
	}
	return nil
}

// Finish closes a task, if the caller still holds it.
//
// The token check is the fence. A worker whose lease expired while it was
// stalled will arrive here with a stale token and be refused, which is the
// entire reason the token exists: by then another worker legitimately holds the
// task, and accepting this result would report someone else's work as finished
// and leave the real attempt to overwrite it later.
func (q *Queue) Finish(ctx context.Context, id string, token int64, state State, result string) error {
	if state != Done && state != Failed && state != Canceled {
		return fmt.Errorf("%q is not a terminal state", state)
	}
	res, err := q.db.ExecContext(ctx, `
		UPDATE tasks SET state = ?, result = ?, owner = '', lease = 0
		 WHERE id = ? AND token = ? AND state = ?`,
		string(state), result, id, token, string(Claimed))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotHeld
	}
	q.record(ctx, "task."+string(state), id, map[string]any{"token": token, "result": result})
	return nil
}

// Release hands a task back without finishing it, for a worker that is giving
// up cleanly. Cheaper than waiting out the lease, and the distinction is worth
// keeping: released means someone decided, expired means nobody knows.
func (q *Queue) Release(ctx context.Context, id string, token int64) error {
	res, err := q.db.ExecContext(ctx, `
		UPDATE tasks SET state = ?, owner = '', lease = 0
		 WHERE id = ? AND token = ? AND state = ?`,
		string(Ready), id, token, string(Claimed))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotHeld
	}
	q.record(ctx, "task.released", id, map[string]any{"token": token})
	return nil
}

// CancelReady withdraws work nobody has claimed. It needs no fencing token
// because there is no worker to fence; the state predicate is the race guard.
func (q *Queue) CancelReady(ctx context.Context, id, result string) error {
	res, err := q.db.ExecContext(ctx, `
		UPDATE tasks SET state = ?, result = ?, owner = '', lease = 0
		 WHERE id = ? AND state = ?`, string(Canceled), result, id, string(Ready))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotHeld
	}
	q.record(ctx, "task.canceled", id, map[string]any{"result": result})
	return nil
}

func (q *Queue) Get(ctx context.Context, id string) (Task, error) {
	var t Task
	row := q.db.QueryRowContext(ctx, `
		SELECT id, title, dir, state, owner, token, lease, attempt, result, filed
		  FROM tasks WHERE id = ?`, id)
	return t, scan(row, &t)
}

// List returns the queue, oldest first, optionally filtered to one state.
func (q *Queue) List(ctx context.Context, state State) ([]Task, error) {
	query := `SELECT id, title, dir, state, owner, token, lease, attempt, result, filed FROM tasks`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, string(state))
	}
	query += ` ORDER BY filed`

	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Task{}
	for rows.Next() {
		var t Task
		if err := scanRows(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scan(s scanner, t *Task) error {
	var lease, filed int64
	err := s.Scan(&t.ID, &t.Title, &t.Dir, &t.State, &t.Owner, &t.Token, &lease, &t.Attempt, &t.Result, &filed)
	if err != nil {
		return err
	}
	if lease > 0 {
		t.Lease = time.UnixMilli(lease).UTC()
	}
	t.Filed = time.UnixMilli(filed).UTC()
	return nil
}

func scanRows(rows *sql.Rows, t *Task) error { return scan(rows, t) }

// record writes the history. A failure here is not allowed to fail the
// operation: the table is the authority on who holds what, and losing an audit
// row is worse than nothing but much better than refusing to hand out work
// because the log is busy.
func (q *Queue) record(ctx context.Context, kind, id string, payload map[string]any) {
	if q.log == nil {
		return
	}
	payload["task"] = id
	ev, err := event.New(event.Kind(kind), "queue", "", payload)
	if err != nil {
		return
	}
	_, _ = q.log.Append(ctx, ev)
}
