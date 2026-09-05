// Package holds gives agents mutual exclusion over the files they are editing.
//
// amac already tells an agent whether another session is working in a
// directory. That check is presence, not exclusion: it reads tmux working
// directories, says so out loud in its own output ("this does not prove they
// are touching the same files"), and nothing expires or is enforced. It is a
// courtesy, and courtesies do not survive two agents in a hurry.
//
// The failure it misses is not hypothetical. While amac itself was being
// worked on, a peer session ran `git add -A` and swept another session's
// in-progress files into a commit about something else. The change was captured
// mid-edit and silently lost, and it surfaced later as a failing test rather
// than as a conflict. An hour after that a second session created a branch
// under the first one. Both sessions were in the same directory, both were
// visible to the presence check, and neither was stopped by it.
//
// # Why this is the queue again
//
// A file claim is the same problem as a task claim with a different resource,
// so it is deliberately the same mechanism rather than a new one: a conditional
// UPDATE that takes a path if it is free or its lease has expired, a fencing
// token that only goes up, and a lease so a dead agent does not hold a file
// forever. That machinery is already proved against SIGKILL in package queue,
// and inventing a second, less tested one for the same semantics would be the
// wrong kind of new code.
//
// Two things are different, and both matter.
//
// A path is a tree, not an id. Holding `internal/daemon` has to conflict with
// holding `internal/daemon/server.go` and the other way round, because an agent
// that claims a directory is claiming what is under it.
//
// And a claim is all or nothing. An agent asking for five files and receiving
// four is in the worst possible state: it believes it has permission and it is
// wrong about one file, which is exactly the case that produces a diff nobody
// can explain. Either the whole set is granted or none of it is, and the
// contested paths come back so the agent can say something useful about why it
// stopped.
package holds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// ErrHeld means another live claim overlaps the request. The conflicting holds
// come back with it, because "no" without "by whom" cannot be acted on.
var ErrHeld = errors.New("paths are held by another session")

// Hold is one live claim on one path.
type Hold struct {
	Path    string    `json:"path"`
	Owner   string    `json:"owner"`
	Token   int64     `json:"token"`
	Lease   time.Time `json:"lease"`
	Claimed time.Time `json:"claimed"`
	Note    string    `json:"note,omitempty"`
}

// Expired reports whether this hold's lease has passed.
func (h Hold) Expired(now time.Time) bool { return !h.Lease.After(now) }

type Holds struct {
	db  *sql.DB
	log *event.Log
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS holds (
  path    TEXT PRIMARY KEY,
  owner   TEXT NOT NULL,
  token   INTEGER NOT NULL DEFAULT 0,
  lease   INTEGER NOT NULL,
  claimed INTEGER NOT NULL,
  note    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_holds_lease ON holds(lease);
`

func Open(log *event.Log) (*Holds, error) {
	h := &Holds{db: log.DB(), log: log, now: func() time.Time { return time.Now().UTC() }}
	if _, err := h.db.Exec(schema); err != nil {
		return nil, fmt.Errorf("holds schema: %w", err)
	}
	return h, nil
}

// Clean normalises a path the way every comparison here expects it.
//
// Claims are compared as strings, so `/a/b`, `/a/b/` and `/a/./b` have to
// become one path before they reach the table or the same file is holdable
// three times over.
func Clean(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// overlaps reports whether two cleaned paths contend: the same path, or either
// one inside the other.
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+string(filepath.Separator)) ||
		strings.HasPrefix(b, a+string(filepath.Separator))
}

// Claim takes every path or none of them.
//
// Granted holds share one token, so releasing and renewing act on the set the
// agent actually believes it has rather than on paths one at a time.
func (h *Holds) Claim(ctx context.Context, owner string, paths []string, lease time.Duration, note string) ([]Hold, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("a claim needs an owner")
	}
	want := normalise(paths)
	if len(want) == 0 {
		return nil, errors.New("a claim needs at least one path")
	}

	now := h.now()
	deadline := now.Add(lease)

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Read every live hold once and decide in Go. The alternative is a LIKE
	// per requested path, which is more SQL for a table that holds one row per
	// file currently being edited on one laptop.
	live, err := scanHolds(tx.QueryContext(ctx, `SELECT path, owner, token, lease, claimed, note FROM holds WHERE lease > ?`, now.UnixMilli()))
	if err != nil {
		return nil, err
	}
	var conflicts []Hold
	for _, held := range live {
		if held.Owner == owner {
			continue // re-claiming your own live hold is a renewal, not a conflict
		}
		for _, p := range want {
			if overlaps(p, held.Path) {
				conflicts = append(conflicts, held)
				break
			}
		}
	}
	if len(conflicts) > 0 {
		return conflicts, ErrHeld
	}

	out := make([]Hold, 0, len(want))
	for _, p := range want {
		// token + 1 on conflict, so a path reclaimed after an expiry always
		// issues a number above the one the dead holder is carrying. That is
		// what lets a stale release or renew be rejected on arrival instead of
		// being trusted because it arrived.
		var token int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO holds (path, owner, token, lease, claimed, note)
			VALUES (?, ?, 1, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
			    owner = excluded.owner, token = holds.token + 1,
			    lease = excluded.lease, claimed = excluded.claimed, note = excluded.note
			RETURNING token`,
			p, owner, deadline.UnixMilli(), now.UnixMilli(), note).Scan(&token)
		if err != nil {
			return nil, err
		}
		out = append(out, Hold{Path: p, Owner: owner, Token: token, Lease: deadline, Claimed: now, Note: note})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	h.record(ctx, "hold.claimed", owner, out, note)
	return out, nil
}

// Release drops the caller's holds. A token that no longer matches is refused,
// which is the whole point of the token: an agent that was declared dead and
// replaced must not be able to release the file its replacement now holds.
func (h *Holds) Release(ctx context.Context, owner string, token int64, paths []string) error {
	want := normalise(paths)
	if len(want) == 0 {
		return errors.New("a release needs at least one path")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	released := make([]Hold, 0, len(want))
	for _, p := range want {
		res, err := tx.ExecContext(ctx, `DELETE FROM holds WHERE path = ? AND owner = ? AND token = ?`, p, owner, token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			released = append(released, Hold{Path: p, Owner: owner, Token: token})
		}
	}
	if len(released) == 0 {
		return fmt.Errorf("no hold released: %w", ErrStaleToken)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.record(ctx, "hold.released", owner, released, "")
	return nil
}

// ErrStaleToken means the caller's fencing token is not the one the table holds.
var ErrStaleToken = errors.New("stale fencing token")

// Renew extends a live claim the caller still owns.
func (h *Holds) Renew(ctx context.Context, owner string, token int64, paths []string, lease time.Duration) error {
	want := normalise(paths)
	if len(want) == 0 {
		return errors.New("a renewal needs at least one path")
	}
	deadline := h.now().Add(lease)
	for _, p := range want {
		res, err := h.db.ExecContext(ctx, `UPDATE holds SET lease = ? WHERE path = ? AND owner = ? AND token = ?`,
			deadline.UnixMilli(), p, owner, token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("renew %s: %w", p, ErrStaleToken)
		}
	}
	return nil
}

// Who returns the live holds contending with one path, whoever owns them.
func (h *Holds) Who(ctx context.Context, path string) ([]Hold, error) {
	p := Clean(path)
	if p == "" {
		return nil, errors.New("a lookup needs a path")
	}
	live, err := h.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Hold
	for _, held := range live {
		if overlaps(p, held.Path) {
			out = append(out, held)
		}
	}
	return out, nil
}

// List returns every live hold. Expired rows are left in the table rather than
// swept: the next claim on that path overwrites them and bumps the token, and a
// background sweeper would be a second thing that can be wrong about whether a
// lease has passed.
func (h *Holds) List(ctx context.Context) ([]Hold, error) {
	return scanHolds(h.db.QueryContext(ctx,
		`SELECT path, owner, token, lease, claimed, note FROM holds WHERE lease > ? ORDER BY claimed`,
		h.now().UnixMilli()))
}

// ReleaseAll drops everything one session holds, for when it ends.
func (h *Holds) ReleaseAll(ctx context.Context, owner string) (int, error) {
	res, err := h.db.ExecContext(ctx, `DELETE FROM holds WHERE owner = ?`, owner)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		h.record(ctx, "hold.released", owner, nil, fmt.Sprintf("session ended, %d released", n))
	}
	return int(n), nil
}

func normalise(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		c := Clean(p)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func scanHolds(rows *sql.Rows, err error) ([]Hold, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hold
	for rows.Next() {
		var h Hold
		var lease, claimed int64
		if err := rows.Scan(&h.Path, &h.Owner, &h.Token, &lease, &claimed, &h.Note); err != nil {
			return nil, err
		}
		h.Lease = time.UnixMilli(lease).UTC()
		h.Claimed = time.UnixMilli(claimed).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// record appends to the log after the table has committed, so the journal never
// describes a claim that a rolled-back transaction did not actually make.
func (h *Holds) record(ctx context.Context, kind, owner string, hs []Hold, note string) {
	if h.log == nil {
		return
	}
	paths := make([]string, 0, len(hs))
	for _, x := range hs {
		paths = append(paths, x.Path)
	}
	payload := map[string]any{"owner": owner, "paths": paths}
	if note != "" {
		payload["note"] = note
	}
	if len(hs) > 0 {
		payload["token"] = hs[0].Token
	}
	ev, err := event.New(event.Kind(kind), "holds", owner, payload)
	if err != nil {
		return
	}
	_, _ = h.log.Append(ctx, ev)
}
