package health

// Automations that tell amac, rather than being read.
//
// Every other probe here pulls: it reads the artifact a job commits, the marker
// it appends, the file it writes. Pulling is the stronger design and it is why
// this package counts deliveries rather than runs, because an artifact only
// exists once work landed and a ping can be sent by a job that did nothing.
//
// It is also the reason amac can only watch things it can reach, which is this
// machine and a GitHub repo. A cron job on a VPS, a pipeline on someone else's
// runner, a script on a Linux box: all invisible, not because watching them is
// hard but because there is no artifact within reach.
//
// So a job can post instead. The cadence, the grace and the lateness test are
// the same ones every other automation gets, and that is the point: a heartbeat
// is a different way of learning the same fact, not a different kind of
// automation with weaker rules.
//
// The weakness is stated rather than designed away. A push says a job ran; it
// cannot say the job delivered, and a job that succeeds while doing nothing
// pings exactly like one that worked. Where an artifact is reachable, read the
// artifact. This is for where one is not.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lgoyal6/amac/internal/event"
)

// Beat is one report from a job, as posted.
type Beat struct {
	Name string `json:"name"`
	// State lets a job say it failed rather than only that it lived. Empty
	// means it delivered, because the overwhelmingly common case is a job that
	// finished and has nothing to add.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Count is whatever the job counts: rows written, files sent, errors hit.
	// Reported back verbatim, since amac has no way to know what it means.
	Count *int `json:"count,omitempty"`
}

// Record files a heartbeat in the log.
//
// In the log rather than a table, unlike the queue: nothing here needs mutual
// exclusion, the newest beat is the whole state, and history is worth keeping
// for the same reason every other verdict here is.
func Record(ctx context.Context, log *event.Log, b Beat) error {
	if b.Name == "" {
		return fmt.Errorf("a heartbeat needs a name")
	}
	switch b.State {
	case "", "ok", "failing":
	default:
		return fmt.Errorf("%q is not a state a heartbeat may report", b.State)
	}
	ev, err := event.New(event.KindHeartbeat, "heartbeat", b.Name, b)
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, ev)
	return err
}

// newHeartbeat builds the probe that reads them back.
//
// It takes the log at construction because a probe has no other way to reach
// it, which is the one thing about this kind that differs from the others: the
// rest read files and APIs, and this one reads amac's own record.
func newHeartbeat(log *event.Log) probeMaker {
	return func(d Declaration) (func(context.Context) (Report, error), error) {
		p := paramsOf(d)
		// The name posted by the job, when it differs from the declared one.
		// Usually it does not, and defaulting saves a line in every roster.
		key := p.str("key", false)
		if key == "" {
			key = d.Name
		}
		if err := p.err(); err != nil {
			return nil, err
		}
		return func(ctx context.Context) (Report, error) {
			return heartbeat(ctx, log, key)
		}, nil
	}
}

func heartbeat(ctx context.Context, log *event.Log, key string) (Report, error) {
	r := Report{State: OK}
	if log == nil {
		r.State = Unknown
		r.Detail = "no event log to read heartbeats from"
		return r, nil
	}

	var at string
	var payload []byte
	err := log.DB().QueryRowContext(ctx, `
		SELECT at, payload FROM events
		 WHERE kind = ? AND session = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindHeartbeat), key).Scan(&at, &payload)
	if err == sql.ErrNoRows {
		// Never heard from. Unknown rather than late: a job that has not been
		// wired up yet and a job that has stopped are different problems, and
		// the lateness test has nothing to measure from anyway.
		r.State = Unknown
		r.Detail = "declared, but nothing has ever posted to " + key
		return r, nil
	}
	if err != nil {
		return r, err
	}

	var b Beat
	if err := json.Unmarshal(payload, &b); err != nil {
		return r, fmt.Errorf("heartbeat payload: %w", err)
	}
	r.Last, _ = time.Parse(time.RFC3339Nano, at)

	// Last stays set even when the job reported failure, so the lateness test
	// upstream still measures from the last time it was heard from. A job that
	// fails and then goes silent is worse than one that fails and keeps saying
	// so, and collapsing both into failing would hide the difference.
	switch b.State {
	case "failing":
		r.State = Failing
		r.Detail = b.Detail
		if r.Detail == "" {
			r.Detail = "reported failing " + short(time.Since(r.Last)) + " ago"
		}
	default:
		r.Detail = "last posted " + short(time.Since(r.Last)) + " ago"
		if b.Detail != "" {
			r.Detail += ": " + b.Detail
		}
	}
	if b.Count != nil {
		r.Notes = append(r.Notes, fmt.Sprintf("reported count %d", *b.Count))
	}
	return r, nil
}
