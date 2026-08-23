package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lgoyal6/amac/internal/event"
	"github.com/lgoyal6/amac/internal/health"
)

// cmdHealth sweeps the declared automations, records the result, and decides
// whether it is worth interrupting him.
//
// Every sweep appends an event whether or not anything is wrong. The alert is
// derived from the difference between this sweep and the last one, which is
// why the log has to hold the healthy sweeps too: without them there is no
// baseline to diff against, and every restart would re-announce every existing
// failure.
func cmdHealth(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	digest := fs.Bool("digest", false, "DM the full roster, healthy or not")
	alert := fs.Bool("alert", false, "DM only what changed since the last sweep")
	quiet := fs.Bool("quiet", false, "no stdout, for launchd")
	dry := fs.Bool("dry-run", false, "print the DM instead of sending it")
	dbPath := fs.String("db", defaultLogPath(), "event log path")
	timeout := fs.Duration("timeout", 90*time.Second, "overall probe timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log, err := event.Open(*dbPath, event.Full)
	if err != nil {
		return err
	}
	defer log.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	prev, err := lastStates(ctx, log)
	if err != nil {
		return err
	}

	reports := health.Run(ctx, health.All())

	if !*quiet {
		for _, r := range reports {
			fmt.Printf("%s %-22s %-8s %s\n", r.State.Icon(), r.Name, r.State, r.Detail)
			for _, n := range r.Notes {
				fmt.Printf("  %-22s   · %s\n", "", n)
			}
			if r.Err != "" {
				fmt.Printf("  %-22s   · %s\n", "", r.Err)
			}
		}
	}

	// Record before notifying. If Discord is the thing that is down, the sweep
	// still has to survive in the log.
	ev, err := event.New(event.KindAutomationCheck, "health", "", map[string]any{
		"reports": reports,
	})
	if err != nil {
		return err
	}
	stored, err := log.Append(ctx, ev)
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Printf("\nevent seq=%d\n", stored.Seq)
	}

	// deliver prints or sends, so the wording of a DM can be iterated on
	// without putting a run of drafts on his phone.
	deliver := func(kind, msg string) error {
		if *dry {
			fmt.Printf("--- %s (dry run, %d chars) ---\n%s\n", kind, len(msg), msg)
			return nil
		}
		if err := health.Send(ctx, msg); err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if !*quiet {
			fmt.Println(kind + " sent")
		}
		return nil
	}

	switch {
	case *digest:
		return deliver("digest", health.Digest(reports))
	case *alert:
		msg, changed := health.Alert(reports, prev)
		if !changed {
			if !*quiet {
				fmt.Println("no change since last sweep, staying quiet")
			}
			return nil
		}
		return deliver("alert", msg)
	}

	// A failing automation is not a failing command. Exiting non-zero here
	// would make launchd treat a known-bad pipeline as a broken monitor and
	// start backing the monitor off, which is exactly backwards.
	return nil
}

// lastStates reads the previous sweep's verdicts, so an alert can be a diff.
// A missing or unreadable previous sweep yields an empty map, which makes the
// first run treat every current failure as new. That is the correct behaviour
// on a fresh install: he has not been told about them yet.
func lastStates(ctx context.Context, log *event.Log) (map[string]health.State, error) {
	row := log.DB().QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = ? ORDER BY seq DESC LIMIT 1`,
		string(event.KindAutomationCheck))
	var payload []byte
	switch err := row.Scan(&payload); {
	case err == sql.ErrNoRows:
		return map[string]health.State{}, nil
	case err != nil:
		return nil, err
	}
	var body struct {
		Reports []health.Report `json:"reports"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		fmt.Fprintf(os.Stderr, "amac: previous sweep unreadable (%v), treating as first run\n", err)
		return map[string]health.State{}, nil
	}
	out := make(map[string]health.State, len(body.Reports))
	for _, r := range body.Reports {
		out[r.Name] = r.State
	}
	return out, nil
}
